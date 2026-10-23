package runtime

import (
	"bufio"
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"slices"
	"strconv"
	"strings"

	"github.com/ipfs/go-cid"
)

// Forecast fits candidate models to a time series, backtests each on the
// series' own recent past, and forecasts ahead with the median of the best
// candidate of each of the best-scoring kinds of model.
// The candidates are shared out across the pool, a few to each task, so
// many models and settings are tried side by side.
//
// It is plain arithmetic in the daemon, with no image and no randomness,
// so any worker can run it, and two workers given the same task return
// the same bytes: a job is checked by comparing them. Go may fuse a
// multiplication and an addition into one instruction on some processors,
// rounding once where others round twice; every product here is
// converted to float64 explicitly, which the language says rounds it, so
// the answer is the same on every processor the daemon is built for.
type Forecast struct {
	// Containers says that this node runs container work, and so may be
	// given the Chronos-Bolt candidates, which run in a pinned image.
	// Without it, a job that asks for them is refused.
	Containers bool
	// Engine runs the container engine, as Container's does. Nil means
	// Docker.
	Engine func(ctx context.Context, args []string, stdout, stderr io.Writer) (status int, err error)
	// Confined holds the Chronos containers in as Container's does.
	Confined Confinement
}

// ForecastParams are the job parameters.
type ForecastParams struct {
	// Input is the CID of the series: a CSV file with a value on each
	// line, in the last column, oldest first. A first line whose value is
	// not a number is taken as a header.
	Input string `json:"input"`
	// Horizon is how many steps ahead to forecast; 12 if not given.
	Horizon int `json:"horizon,omitempty"`
	// Season is the length of the series' cycle in steps, 12 for months
	// in a year, or 0 for none. Seasonal models are tried only with one.
	Season int `json:"season,omitempty"`
	// Folds is how many windows of Horizon steps at the end of the series
	// each candidate is backtested on. If not given, as many as leave a
	// horizon's worth of points before them, up to autoFolds.
	Folds int `json:"folds,omitempty"`
	// Models is which kinds of model to try: "all", the default, or one
	// kind, or several separated by commas.
	Models string `json:"models,omitempty"`
	// Tasks caps how many tasks the candidates are shared out among; 0
	// leaves it to the pool.
	Tasks int `json:"tasks,omitempty"`
}

// forecastKinds are the kinds of model, in the order their candidates are
// tried.
var forecastKinds = []string{"naive", "mean", "drift", "snaive", "ses", "holt", "holtwinters", "ar", "theta"}

const (
	maxForecastBytes   = 16 << 20
	maxForecastPoints  = 200_000
	maxForecastHorizon = 1000
	maxForecastSeason  = 1000
	maxForecastFolds   = 50
	// maxForecastOrder bounds an autoregression, whose fitting grows with
	// the square of its order.
	maxForecastOrder = 24
	// maxForecastResiduals bounds how many backtest residuals a candidate
	// sends back, so that a task's output stays small; past it, the
	// forecast is the best candidate's alone.
	maxForecastResiduals = 2048
	// combineKinds is how many kinds of model the forecast combines: the
	// best candidate of each of the best-scoring kinds.
	combineKinds = 4
	// autoFolds is the most backtest windows a job that does not say how
	// many is given. On the M4 competition's series, choosing on 8 windows
	// forecast better than on 2 or 4, and as well as on 12.
	autoFolds = 8
)

// forecastModel is one candidate: a kind of model and its settings.
type forecastModel struct {
	Kind   string  `json:"kind"`
	Alpha  float64 `json:"alpha,omitempty"`
	Beta   float64 `json:"beta,omitempty"`
	Gamma  float64 `json:"gamma,omitempty"`
	Order  int     `json:"order,omitempty"`
	Season int     `json:"season,omitempty"`
	// Variant is which Chronos-Bolt model a chronos candidate is.
	Variant string `json:"variant,omitempty"`
}

// Name says what the candidate is, settings and all.
func (m forecastModel) Name() string {
	var b strings.Builder
	b.WriteString(m.Kind)
	setting := func(key string, v float64) {
		if v != 0 {
			fmt.Fprintf(&b, " %s=%s", key, strconv.FormatFloat(v, 'g', -1, 64))
		}
	}
	setting("alpha", m.Alpha)
	setting("beta", m.Beta)
	setting("gamma", m.Gamma)
	setting("order", float64(m.Order))
	setting("season", float64(m.Season))
	if m.Variant != "" {
		b.WriteString(" variant=" + m.Variant)
	}
	return b.String()
}

// fewest is how many points the model needs to be fitted to.
func (m forecastModel) fewest() int {
	switch m.Kind {
	case "snaive":
		return m.Season
	case "holtwinters":
		return 2 * m.Season
	case "drift", "holt", "theta":
		return 2
	case "ar":
		return 3*m.Order + 2
	case "chronos":
		return chronosFewest
	}
	return 1
}

// invalid says what is wrong with a candidate's settings, or "" if
// nothing is. Split makes only valid ones; a task is checked all the same.
func (m forecastModel) invalid() string {
	rate := func(v float64) bool { return v > 0 && v <= 1 }
	season := m.Season >= 2 && m.Season <= maxForecastSeason
	ok := false
	switch m.Kind {
	case "naive", "mean", "drift":
		ok = true
	case "snaive":
		ok = season
	case "ses":
		ok = rate(m.Alpha)
	case "theta":
		ok = rate(m.Alpha) && (m.Season == 0 || season)
	case "holt":
		ok = rate(m.Alpha) && rate(m.Beta)
	case "holtwinters":
		ok = season && rate(m.Alpha) && rate(m.Beta) && rate(m.Gamma)
	case "ar":
		ok = m.Order >= 1 && m.Order <= maxForecastOrder
	case "chronos":
		_, ok = chronosVariants[m.Variant]
		ok = ok && m.Alpha == 0 && m.Beta == 0 && m.Gamma == 0 && m.Order == 0 && m.Season == 0
	default:
		return fmt.Sprintf("no such kind of model as %q", m.Kind)
	}
	if !ok || (m.Kind != "chronos" && m.Variant != "") {
		return fmt.Sprintf("%s is not a model with settings it can take", m.Name())
	}
	return ""
}

// forecastTask is a task payload: backtest and forecast these candidates.
type forecastTask struct {
	Input   string          `json:"input"`
	Horizon int             `json:"horizon"`
	Folds   int             `json:"folds"`
	Models  []forecastModel `json:"models"`
}

// forecastPart is a task output.
type forecastPart struct {
	Points     int               `json:"points"`
	Horizon    int               `json:"horizon"`
	Folds      int               `json:"folds"`
	Last       string            `json:"last"`
	Candidates []forecastOutcome `json:"candidates"`
}

// forecastOutcome is how one candidate did: its errors over the backtest,
// the root mean square error at each step ahead, and its forecast from the
// whole series; or why it could not be fitted.
type forecastOutcome struct {
	Model    forecastModel `json:"model"`
	MAE      float64       `json:"mae"`
	RMSE     float64       `json:"rmse"`
	StepRMSE []float64     `json:"step_rmse,omitempty"`
	Forecast []float64     `json:"forecast,omitempty"`
	// Residuals are what each backtest window's values were less what
	// was forecast for them, when there are few enough of them to send.
	Residuals [][]float64 `json:"residuals,omitempty"`
	Error     string      `json:"error,omitempty"`
}

// ForecastScore is one candidate's backtest, as the result gives it.
type ForecastScore struct {
	Model string  `json:"model"`
	MAE   float64 `json:"mae,omitempty"`
	RMSE  float64 `json:"rmse,omitempty"`
	Error string  `json:"error,omitempty"`
}

// ForecastPoint is one step of the forecast, with the ranges the value
// fell in 80 and 95 times in a hundred in the backtest, taking its errors
// at that step as normal.
type ForecastPoint struct {
	Step  int     `json:"step"`
	Value float64 `json:"value"`
	Lo80  float64 `json:"lo80"`
	Hi80  float64 `json:"hi80"`
	Lo95  float64 `json:"lo95"`
	Hi95  float64 `json:"hi95"`
}

// ForecastResult is the job result.
type ForecastResult struct {
	// Points is how long the series is, and Last its last time, as its
	// first column gave it, or its last line's number if it had one
	// column only. The forecast goes on from there.
	Points  int    `json:"points"`
	Last    string `json:"last"`
	Horizon int    `json:"horizon"`
	Folds   int    `json:"folds"`
	// Best is the candidate with the least root mean square error over
	// the backtest, and Forecast what it forecasts.
	Best ForecastScore `json:"best"`
	// Chosen is what Forecast is: the median, step by step, of the best
	// candidate of each of the best-scoring kinds of model, or Best when
	// only one kind could be fitted. On the M4 competition's series the
	// median forecasts better than the best candidate alone, even where
	// the backtest says otherwise; see docs/forecast-benchmark.md.
	Chosen   ForecastScore   `json:"chosen"`
	Forecast []ForecastPoint `json:"forecast"`
	// Candidates are all of them, best first; those that could not be
	// fitted come last, saying why.
	Candidates []ForecastScore `json:"candidates"`
	// ForecastTable and ScoresTable are the CIDs of CSV files of the
	// forecast and of the candidates' scores.
	ForecastTable string `json:"forecast_table"`
	ScoresTable   string `json:"scores_table"`
	Tasks         int    `json:"tasks"`
}

func (Forecast) Name() string { return "forecast" }

func (Forecast) Describe() string {
	return `Forecasts a time series: fits naive, mean, drift, seasonal naive, exponential smoothing (simple, Holt's trend, Holt-Winters seasonal), autoregressive and theta models with a range of settings, backtests each on the last windows of the series, a few candidates on each worker, and forecasts with the median, step by step, of the best candidate of each of the four best kinds. Parameters: {"input": "<CID of a CSV file with a value on each line, in the last column, oldest first>", "horizon": <steps ahead, default 12>, "season": <steps in the series' cycle, 0 for none>, "folds": <backtest windows, by default as many as fit, up to 8>, "models": "all" or one or more of naive, mean, drift, snaive, ses, holt, holtwinters, ar, theta, separated by commas, and on a pool whose workers run containers chronos-tiny, chronos-mini, chronos-small or chronos-base, the pretrained Chronos-Bolt models, which "all" does not include}. Result: {"best": {"model", "mae", "rmse"}, "chosen": {"model": "median of …", "mae", "rmse"}, "forecast": [{"step", "value", "lo80", "hi80", "lo95", "hi95"}], "candidates": [every candidate's scores, best first], "forecast_table": "<CID of the forecast as CSV>", "scores_table": "<CID of the scores as CSV>"}.`
}

func (f Forecast) Form() []Field {
	return []Field{
		{Name: "input", Kind: KindFile, Label: "Series", Help: "A CSV file with a value on each line, in its last column, oldest first; a header line is skipped.", Required: true},
		{Name: "horizon", Kind: KindInteger, Label: "Steps ahead", Help: "How many steps of the series to forecast.", Default: 12, Minimum: bound(1), Maximum: bound(maxForecastHorizon)},
		{Name: "season", Kind: KindInteger, Label: "Season length", Help: "How many steps the series' cycle takes, such as 12 for months or 7 for days of the week, or 0 if it has none.", Default: 0, Minimum: bound(0), Maximum: bound(maxForecastSeason)},
		{Name: "folds", Kind: KindInteger, Label: "Backtest windows", Help: "How many windows at the end of the series each model is tested on before one is chosen. Leave it empty to use as many as the series has room for, up to 8.", Minimum: bound(1), Maximum: bound(maxForecastFolds)},
		{Name: "models", Kind: KindChoice, Label: "Models", Help: f.modelsHelp(), Default: "all", Choices: f.modelChoices()},
	}
}

// settings checks the parameters and fills in their defaults.
func (p *ForecastParams) settings() error {
	p.Horizon = cmp.Or(p.Horizon, 12)
	switch {
	case p.Horizon < 1 || p.Horizon > maxForecastHorizon:
		return fmt.Errorf("horizon %d is not between 1 and %d", p.Horizon, maxForecastHorizon)
	case p.Season == 1 || p.Season < 0 || p.Season > maxForecastSeason:
		return fmt.Errorf("season %d is neither 0, for none, nor between 2 and %d", p.Season, maxForecastSeason)
	case p.Folds < 0 || p.Folds > maxForecastFolds:
		return fmt.Errorf("folds %d is neither 0, for as many as fit, nor between 1 and %d", p.Folds, maxForecastFolds)
	case p.Tasks < 0 || p.Tasks > MaxTasks:
		return fmt.Errorf("tasks %d is not between 0 and %d", p.Tasks, MaxTasks)
	}
	return nil
}

// kinds returns the kinds of model the parameters ask for.
func (p ForecastParams) kinds() ([]string, error) {
	if p.Models == "" || p.Models == "all" {
		return forecastKinds, nil
	}
	var kinds []string
	for _, kind := range strings.Split(p.Models, ",") {
		kind = strings.TrimSpace(kind)
		if !slices.Contains(forecastKinds, kind) && !slices.Contains(chronosKinds, kind) {
			return nil, fmt.Errorf("models: %q is none of %s", kind, strings.Join(slices.Concat(forecastKinds, chronosKinds), ", "))
		}
		if !slices.Contains(kinds, kind) {
			kinds = append(kinds, kind)
		}
	}
	return kinds, nil
}

// forecastCandidates returns the candidates of these kinds that can be
// fitted to every backtest window of a series of the given length.
func forecastCandidates(kinds []string, season, shortest int) []forecastModel {
	var models []forecastModel
	for _, kind := range forecastKinds {
		if !slices.Contains(kinds, kind) {
			continue
		}
		switch kind {
		case "snaive":
			if season > 0 {
				models = append(models, forecastModel{Kind: kind, Season: season})
			}
		case "ses":
			for _, a := range []float64{0.1, 0.2, 0.3, 0.5, 0.7, 0.9} {
				models = append(models, forecastModel{Kind: kind, Alpha: a})
			}
		case "holt":
			for _, a := range []float64{0.2, 0.5, 0.8} {
				for _, b := range []float64{0.05, 0.2} {
					models = append(models, forecastModel{Kind: kind, Alpha: a, Beta: b})
				}
			}
		case "holtwinters":
			if season == 0 {
				continue
			}
			for _, a := range []float64{0.2, 0.5} {
				for _, b := range []float64{0.05, 0.2} {
					for _, g := range []float64{0.1, 0.3} {
						models = append(models, forecastModel{Kind: kind, Alpha: a, Beta: b, Gamma: g, Season: season})
					}
				}
			}
		case "ar":
			orders := []int{1, 2, 3, 4, 6, 8, 12}
			if season > 0 && season <= maxForecastOrder && !slices.Contains(orders, season) {
				orders = append(orders, season)
			}
			for _, p := range orders {
				models = append(models, forecastModel{Kind: kind, Order: p})
			}
		case "theta":
			for _, a := range []float64{0.1, 0.2, 0.3, 0.5, 0.7, 0.9} {
				models = append(models, forecastModel{Kind: kind, Alpha: a, Season: season})
			}
		default:
			models = append(models, forecastModel{Kind: kind})
		}
	}
	for _, kind := range chronosKinds {
		if slices.Contains(kinds, kind) {
			models = append(models, forecastModel{Kind: "chronos", Variant: strings.TrimPrefix(kind, "chronos-")})
		}
	}
	return slices.DeleteFunc(models, func(m forecastModel) bool { return m.fewest() > shortest })
}

func (f Forecast) Split(ctx context.Context, blobs Blobs, params []byte, parts int) ([][]byte, error) {
	var p ForecastParams
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, fmt.Errorf("forecast params: %w", err)
	}
	if err := p.settings(); err != nil {
		return nil, fmt.Errorf("forecast params: %w", err)
	}
	kinds, err := p.kinds()
	if err != nil {
		return nil, fmt.Errorf("forecast params: %w", err)
	}
	if wantsChronos(kinds) && !f.Containers {
		return nil, errors.New("forecast params: the Chronos-Bolt models run in containers, and this node does not run container work")
	}
	values, _, err := readSeries(ctx, blobs, p.Input)
	if err != nil {
		return nil, err
	}
	if p.Folds == 0 {
		p.Folds = min(autoFolds, max(1, (len(values)-p.Horizon)/p.Horizon))
	}
	shortest := len(values) - p.Folds*p.Horizon
	if shortest < 1 {
		return nil, fmt.Errorf("forecast: a series of %d points is too short to backtest %d windows of %d steps", len(values), p.Folds, p.Horizon)
	}
	models := forecastCandidates(kinds, p.Season, shortest)
	if len(models) == 0 {
		return nil, fmt.Errorf("forecast: no model of the kinds asked for (%s) can be fitted to %d points with season %d", strings.Join(kinds, ", "), shortest, p.Season)
	}
	n := min(max(parts, 1), len(models))
	if p.Tasks > 0 {
		n = min(n, p.Tasks)
	}
	var payloads [][]byte
	for i := range n {
		share := models[len(models)*i/n : len(models)*(i+1)/n]
		payloads = append(payloads, mustJSON(forecastTask{Input: p.Input, Horizon: p.Horizon, Folds: p.Folds, Models: share}))
	}
	return payloads, nil
}

func (f Forecast) Execute(ctx context.Context, blobs Blobs, payload []byte) ([]byte, error) {
	var task forecastTask
	if err := json.Unmarshal(payload, &task); err != nil {
		return nil, fmt.Errorf("forecast payload: %w", err)
	}
	if task.Horizon < 1 || task.Horizon > maxForecastHorizon || task.Folds < 1 || task.Folds > maxForecastFolds {
		return nil, fmt.Errorf("forecast payload: horizon %d or folds %d out of range", task.Horizon, task.Folds)
	}
	values, last, err := readSeries(ctx, blobs, task.Input)
	if err != nil {
		return nil, err
	}
	report := Report(ctx)
	report.Log(fmt.Sprintf("backtesting %d candidates on %d points", len(task.Models), len(values)))
	part := forecastPart{Points: len(values), Horizon: task.Horizon, Folds: task.Folds, Last: last}
	runners, err := f.runners(task.Models)
	if err != nil {
		return nil, err
	}
	outcomes, err := backtestAll(ctx, runners, [][]float64{values}, [][]forecastModel{task.Models}, task.Horizon, []int{task.Folds}, report.Progress)
	if err != nil {
		return nil, err
	}
	part.Candidates = outcomes[0]
	return mustJSON(part), nil
}

// backtest fits the model to the whole series and forecasts from there,
// then fits it to each window's past and scores its forecast of the
// window. backtestAll does the same for many candidates and series at once.
func backtest(m forecastModel, values []float64, horizon, folds int) forecastOutcome {
	outcomes, _ := backtestAll(context.Background(), inProcess, [][]float64{values}, [][]forecastModel{{m}}, horizon, []int{folds}, nil)
	return outcomes[0][0]
}

// fitForecast fits the model to y and forecasts h steps beyond it. y is at
// least as long as the model needs.
func fitForecast(m forecastModel, y []float64, h int) ([]float64, error) {
	n := len(y)
	f := make([]float64, h)
	switch m.Kind {
	case "naive":
		for k := range f {
			f[k] = y[n-1]
		}
	case "mean":
		var sum float64
		for _, v := range y {
			sum += v
		}
		for k := range f {
			f[k] = sum / float64(n)
		}
	case "drift":
		slope := (y[n-1] - y[0]) / float64(n-1)
		for k := range f {
			f[k] = y[n-1] + float64(float64(k+1)*slope)
		}
	case "snaive":
		for k := range f {
			f[k] = y[n-m.Season+k%m.Season]
		}
	case "ses":
		level := y[0]
		for _, v := range y[1:] {
			level += float64(m.Alpha * (v - level))
		}
		for k := range f {
			f[k] = level
		}
	case "holt":
		level, trend := y[0], y[1]-y[0]
		for _, v := range y[1:] {
			before := level
			ahead := level + trend
			level = ahead + float64(m.Alpha*(v-ahead))
			trend += float64(m.Beta * ((level - before) - trend))
		}
		for k := range f {
			f[k] = level + float64(float64(k+1)*trend)
		}
	case "holtwinters":
		s := m.Season
		var first, second float64
		for i := range s {
			first += y[i]
			second += y[s+i]
		}
		first /= float64(s)
		second /= float64(s)
		level, trend := first, (second-first)/float64(s)
		seasonal := make([]float64, s)
		for i := range s {
			seasonal[i] = y[i] - first
		}
		for t := s; t < n; t++ {
			i := t % s
			before, slope := level, trend
			ahead := level + trend
			level = ahead + float64(m.Alpha*((y[t]-seasonal[i])-ahead))
			trend += float64(m.Beta * ((level - before) - trend))
			seasonal[i] += float64(m.Gamma * ((y[t] - before - slope) - seasonal[i]))
		}
		for k := range f {
			f[k] = level + float64(float64(k+1)*trend) + seasonal[(n+k)%s]
		}
	case "theta":
		// The cycle is taken out first if the series has one, by M4's
		// test and classical decomposition, and put back in the forecast.
		x, index := y, []float64{1}
		if m.Season > 1 && cyclic(y, m.Season) {
			index = seasonIndices(y, m.Season)
			x = make([]float64, n)
			for t, v := range y {
				x[t] = v / index[t%m.Season]
			}
		}
		// Simple exponential smoothing with a drift of half the slope of
		// the least-squares line through the series: the theta method as
		// Hyndman and Billah wrote it.
		var tMean, xMean float64
		for t, v := range x {
			tMean += float64(t)
			xMean += v
		}
		tMean /= float64(n)
		xMean /= float64(n)
		var sxy, sxx float64
		for t, v := range x {
			dt := float64(t) - tMean
			sxy += float64(dt * (v - xMean))
			sxx += float64(dt * dt)
		}
		slope := sxy / sxx
		level, decay := x[0], 1-m.Alpha
		for _, v := range x[1:] {
			level += float64(m.Alpha * (v - level))
			decay = float64(decay * (1 - m.Alpha))
		}
		for k := range f {
			drift := float64(slope / 2 * (float64(k) + (1-decay)/m.Alpha))
			f[k] = float64((level + drift) * index[(n+k)%len(index)])
		}
	case "ar":
		coef, err := fitAR(y, m.Order)
		if err != nil {
			return nil, err
		}
		hist := append(slices.Clone(y), f...)
		for k := range f {
			t := n + k
			v := coef[0]
			for i := 1; i <= m.Order; i++ {
				v += float64(coef[i] * hist[t-i])
			}
			hist[t] = v
			f[k] = v
		}
	}
	return f, nil
}

// fitAR fits y[t] = c + a1·y[t-1] + … + ap·y[t-p] by least squares,
// solving the normal equations by Gaussian elimination with partial
// pivoting, and returns c, a1, …, ap.
func fitAR(y []float64, p int) ([]float64, error) {
	size := p + 1
	a := make([][]float64, size)
	for i := range a {
		a[i] = make([]float64, size+1)
	}
	row := make([]float64, size)
	for t := p; t < len(y); t++ {
		row[0] = 1
		for i := 1; i <= p; i++ {
			row[i] = y[t-i]
		}
		for i := range size {
			for j := range size {
				a[i][j] += float64(row[i] * row[j])
			}
			a[i][size] += float64(row[i] * y[t])
		}
	}
	scale := 0.0
	for i := range size {
		scale = max(scale, math.Abs(a[i][i]))
	}
	for c := range size {
		pivot := c
		for r := c + 1; r < size; r++ {
			if math.Abs(a[r][c]) > math.Abs(a[pivot][c]) {
				pivot = r
			}
		}
		if math.Abs(a[pivot][c]) <= float64(1e-12*scale) {
			return nil, errors.New("the series is too regular to fit an autoregression of this order to")
		}
		a[c], a[pivot] = a[pivot], a[c]
		for r := c + 1; r < size; r++ {
			factor := a[r][c] / a[c][c]
			for j := c; j <= size; j++ {
				a[r][j] -= float64(factor * a[c][j])
			}
		}
	}
	coef := make([]float64, size)
	for i := size - 1; i >= 0; i-- {
		v := a[i][size]
		for j := i + 1; j < size; j++ {
			v -= float64(a[i][j] * coef[j])
		}
		coef[i] = v / a[i][i]
	}
	return coef, nil
}

func (Forecast) Aggregate(ctx context.Context, blobs Blobs, outputs [][]byte) ([]byte, error) {
	var result ForecastResult
	var outcomes []forecastOutcome
	for i, output := range outputs {
		var part forecastPart
		if err := json.Unmarshal(output, &part); err != nil {
			return nil, fmt.Errorf("task %d output: %w", i, err)
		}
		if part.Folds < 1 || part.Folds > maxForecastFolds {
			return nil, fmt.Errorf("task %d output: %d backtest windows, not between 1 and %d", i, part.Folds, maxForecastFolds)
		}
		if i == 0 {
			result.Points, result.Last, result.Horizon, result.Folds = part.Points, part.Last, part.Horizon, part.Folds
		} else if part.Points != result.Points || part.Last != result.Last || part.Horizon != result.Horizon || part.Folds != result.Folds {
			return nil, fmt.Errorf("task %d output: forecast %d steps of a series of %d points ending at %q with %d windows, where task 0 did %d of %d ending at %q with %d", i, part.Horizon, part.Points, part.Last, part.Folds, result.Horizon, result.Points, result.Last, result.Folds)
		}
		for _, o := range part.Candidates {
			if o.Error == "" && !wellFormed(o, result.Horizon, result.Folds) {
				return nil, fmt.Errorf("task %d output: candidate %s has scores, a forecast or residuals that are not finite numbers for %d steps and %d windows", i, o.Model.Name(), result.Horizon, result.Folds)
			}
			outcomes = append(outcomes, o)
		}
	}
	d, err := decide(outcomes, result.Folds)
	if err != nil {
		return nil, err
	}
	result.Candidates, result.Best, result.Chosen, result.Forecast = d.scores, d.scores[0], d.chosen, d.points
	var scores bytes.Buffer
	scores.WriteString("model,mae,rmse,error\n")
	for _, score := range d.scores {
		if score.Error != "" {
			fmt.Fprintf(&scores, "%s,,,%s\n", score.Model, csvField(score.Error))
		} else {
			fmt.Fprintf(&scores, "%s,%s,%s,\n", score.Model, plain(score.MAE), plain(score.RMSE))
		}
	}
	var table bytes.Buffer
	table.WriteString("step,value,lo80,hi80,lo95,hi95\n")
	for _, point := range d.points {
		fmt.Fprintf(&table, "%d,%s,%s,%s,%s,%s\n", point.Step, plain(point.Value), plain(point.Lo80), plain(point.Hi80), plain(point.Lo95), plain(point.Hi95))
	}
	for _, t := range []struct {
		into *string
		data *bytes.Buffer
		what string
	}{{&result.ForecastTable, &table, "forecast"}, {&result.ScoresTable, &scores, "scores"}} {
		stored, err := blobs.Put(ctx, t.data)
		if err != nil {
			return nil, fmt.Errorf("store the %s table: %w", t.what, err)
		}
		*t.into = stored.String()
	}
	result.Tasks = len(outputs)
	return mustJSON(result), nil
}

// decision is what a series' backtest comes to: every candidate's scores,
// best first, the forecast chosen, and its steps with their ranges.
type decision struct {
	scores []ForecastScore
	chosen ForecastScore
	points []ForecastPoint
}

// decide ranks a series' candidates by their backtest, those that could
// not be fitted last, and forecasts with the median of the best of each
// of the best kinds, or the best alone, with ranges from Student's t for
// as many degrees of freedom as there were windows. folds is between 1 and
// maxForecastFolds.
func decide(outcomes []forecastOutcome, folds int) (decision, error) {
	slices.SortStableFunc(outcomes, func(a, b forecastOutcome) int {
		if (a.Error == "") != (b.Error == "") {
			if a.Error == "" {
				return -1
			}
			return 1
		}
		return cmp.Or(cmp.Compare(a.MAE, b.MAE), cmp.Compare(a.RMSE, b.RMSE), strings.Compare(a.Model.Name(), b.Model.Name()))
	})
	if len(outcomes) == 0 || outcomes[0].Error != "" {
		why := "there were none"
		if len(outcomes) > 0 {
			why = outcomes[0].Model.Name() + ": " + outcomes[0].Error
		}
		return decision{}, fmt.Errorf("forecast: no candidate model could be fitted (%s)", why)
	}
	var d decision
	for _, o := range outcomes {
		d.scores = append(d.scores, ForecastScore{Model: o.Model.Name(), MAE: o.MAE, RMSE: o.RMSE, Error: o.Error})
	}
	best := outcomes[0]
	d.chosen = d.scores[0]
	if c, name, ok := combine(outcomes, folds); ok {
		best = c
		d.chosen = ForecastScore{Model: name, MAE: c.MAE, RMSE: c.RMSE}
	}
	q80, q95 := tQuantile80[folds-1], tQuantile95[folds-1]
	var spread float64
	for j, v := range best.Forecast {
		spread = max(spread, best.StepRMSE[j], float64(best.RMSE*math.Sqrt(float64(j+1)/(float64(len(best.Forecast)+1)/2))))
		w80 := float64(q80 * spread)
		w95 := float64(q95 * spread)
		d.points = append(d.points, ForecastPoint{Step: j + 1, Value: v, Lo80: v - w80, Hi80: v + w80, Lo95: v - w95, Hi95: v + w95})
	}
	return d, nil
}

// wellFormed reports whether a fitted candidate's numbers are all there and
// finite: a forecast and an error for each step, and residuals, if any,
// for each step of each backtest window.
func wellFormed(o forecastOutcome, horizon, folds int) bool {
	if len(o.Forecast) != horizon || len(o.StepRMSE) != horizon {
		return false
	}
	if o.Residuals != nil && len(o.Residuals) != folds {
		return false
	}
	numbers := slices.Concat([]float64{o.MAE, o.RMSE}, o.StepRMSE, o.Forecast)
	for _, window := range o.Residuals {
		if len(window) != horizon {
			return false
		}
		numbers = append(numbers, window...)
	}
	for _, v := range numbers {
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return false
		}
	}
	return true
}

// plain writes a number as a CSV table wants it.
func plain(v float64) string { return strconv.FormatFloat(v, 'g', -1, 64) }

// csvField quotes a field that has a comma, a quote or a line break in it.
func csvField(s string) string {
	if !strings.ContainsAny(s, ",\"\n\r") {
		return s
	}
	return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
}

// readSeries reads a series: a CSV file with a value on each line, in the
// last column, and the line's time in the first. A first line whose value
// is not a number is a header. It returns the values and the last time, or
// the last line's number if the file has one column.
func readSeries(ctx context.Context, blobs Blobs, input string) ([]float64, string, error) {
	c, err := cid.Decode(input)
	if err != nil {
		return nil, "", fmt.Errorf("forecast: input is not a CID: %w", err)
	}
	blob, err := blobs.Open(ctx, c)
	if err != nil {
		return nil, "", fmt.Errorf("forecast input %s: %w", c, err)
	}
	defer blob.Close()
	if blob.Size() > maxForecastBytes {
		return nil, "", fmt.Errorf("forecast input %s: %d bytes, more than %d", c, blob.Size(), maxForecastBytes)
	}
	var values []float64
	var last string
	lines := bufio.NewScanner(blob)
	lines.Buffer(make([]byte, 0, 4096), 64<<10)
	header := true
	for number := 1; lines.Scan(); number++ {
		line := strings.TrimSpace(lines.Text())
		if line == "" {
			continue
		}
		fields := strings.FieldsFunc(line, func(r rune) bool { return r == ',' || r == '\t' || r == ';' })
		value := ""
		if len(fields) > 0 {
			value = strings.TrimSpace(fields[len(fields)-1])
		}
		v, err := strconv.ParseFloat(value, 64)
		if err != nil && header {
			header = false
			continue
		}
		header = false
		if err != nil || math.IsNaN(v) || math.IsInf(v, 0) {
			return nil, "", fmt.Errorf("forecast input %s: line %d: %q is not a number", c, number, value)
		}
		if len(values) == maxForecastPoints {
			return nil, "", fmt.Errorf("forecast input %s: more than %d points", c, maxForecastPoints)
		}
		values = append(values, v)
		last = strconv.Itoa(number)
		if len(fields) > 1 {
			last = strings.TrimSpace(fields[0])
		}
	}
	if err := lines.Err(); err != nil {
		return nil, "", fmt.Errorf("forecast input %s: %w", c, err)
	}
	if len(values) < 2 {
		return nil, "", fmt.Errorf("forecast input %s: %d points, too few to forecast from", c, len(values))
	}
	return values, last, nil
}

// combine returns the median, step by step, of the forecasts of the best
// candidate of each of the combineKinds best-scoring kinds among fitted,
// which are sorted best first, and its backtest errors: the median of
// theirs, since the median of forecasts misses by the median of their
// misses. It returns false when fewer than two kinds were fitted, or a
// candidate sent no residuals.
func combine(sorted []forecastOutcome, folds int) (forecastOutcome, string, bool) {
	var picks []forecastOutcome
	var names []string
	seen := map[string]bool{}
	for _, o := range sorted {
		if o.Error != "" || len(picks) == combineKinds {
			break
		}
		if seen[o.Model.Kind] {
			continue
		}
		if len(o.Residuals) != folds {
			return forecastOutcome{}, "", false
		}
		seen[o.Model.Kind] = true
		picks = append(picks, o)
		names = append(names, o.Model.Name())
	}
	if len(picks) < 2 {
		return forecastOutcome{}, "", false
	}
	column := make([]float64, len(picks))
	median := func(at func(o forecastOutcome) float64) float64 {
		for i, o := range picks {
			column[i] = at(o)
		}
		slices.Sort(column)
		mid := len(column) / 2
		if len(column)%2 == 1 {
			return column[mid]
		}
		return (column[mid-1] + column[mid]) / 2
	}
	horizon := len(picks[0].Forecast)
	out := forecastOutcome{Forecast: make([]float64, horizon), StepRMSE: make([]float64, horizon)}
	var absSum, sqSum float64
	for j := range horizon {
		out.Forecast[j] = median(func(o forecastOutcome) float64 { return o.Forecast[j] })
		var stepSq float64
		for k := range folds {
			e := median(func(o forecastOutcome) float64 { return o.Residuals[k][j] })
			sq := float64(e * e)
			absSum += math.Abs(e)
			sqSum += sq
			stepSq += sq
		}
		out.StepRMSE[j] = math.Sqrt(stepSq / float64(folds))
	}
	count := float64(folds * horizon)
	out.MAE = absSum / count
	out.RMSE = math.Sqrt(sqSum / count)
	return out, "median of " + strings.Join(names, "; "), true
}

// cyclic is the M4 competition's test of whether a series has a cycle of
// the given length: whether its autocorrelation at that lag is beyond
// what 90% of series without one would show. A series of fewer than
// three cycles is taken to have none.
func cyclic(y []float64, season int) bool {
	n := len(y)
	if n < 3*season {
		return false
	}
	var mean float64
	for _, v := range y {
		mean += v
	}
	mean /= float64(n)
	var c0 float64
	for _, v := range y {
		c0 += float64((v - mean) * (v - mean))
	}
	acf := func(k int) float64 {
		var s float64
		for t := k; t < n; t++ {
			s += float64((y[t] - mean) * (y[t-k] - mean))
		}
		return s / c0
	}
	sum := 1.0
	for k := 1; k < season; k++ {
		a := acf(k)
		sum += float64(2 * float64(a*a))
	}
	return math.Abs(acf(season)) > float64(1.645/math.Sqrt(float64(n))*math.Sqrt(sum))
}

// seasonIndices are a series' multiplicative seasonal indices, one for
// each place in its cycle, averaging one: each value's ratio to the
// centred moving average around it, averaged over its place in the cycle.
func seasonIndices(y []float64, season int) []float64 {
	half := season / 2
	sums := make([]float64, season)
	counts := make([]float64, season)
	for t := half; t < len(y)-half; t++ {
		var trend float64
		if season%2 == 0 {
			trend = (y[t-half] + y[t+half]) / 2
			for j := t - half + 1; j < t+half; j++ {
				trend += y[j]
			}
		} else {
			for j := t - half; j <= t+half; j++ {
				trend += y[j]
			}
		}
		trend /= float64(season)
		sums[t%season] += y[t] / trend
		counts[t%season]++
	}
	var mean float64
	for i := range sums {
		sums[i] /= counts[i]
		mean += sums[i]
	}
	mean /= float64(season)
	for i := range sums {
		sums[i] /= mean
	}
	return sums
}

// tQuantile80 and tQuantile95 are the 90th and 97.5th percentiles of
// Student's t distribution with 1 to 50 degrees of freedom, which bound
// the middle 80 and 95 of every hundred values. A forecast's ranges are
// these times the spread of its backtest errors, with as many degrees of
// freedom as there were backtest windows: the fewer the windows, the less
// the spread is known, and the wider the ranges. On the M4 competition's
// series this makes the 80% and 95% ranges hold 83% and 95% of what
// followed, where the normal distribution's held 67% and 81%.
var tQuantile80 = [maxForecastFolds]float64{
	3.0777, 1.8856, 1.6377, 1.5332, 1.4759, 1.4398, 1.4149, 1.3968, 1.3830, 1.3722,
	1.3634, 1.3562, 1.3502, 1.3450, 1.3406, 1.3368, 1.3334, 1.3304, 1.3277, 1.3253,
	1.3232, 1.3212, 1.3195, 1.3178, 1.3163, 1.3150, 1.3137, 1.3125, 1.3114, 1.3104,
	1.3095, 1.3086, 1.3077, 1.3070, 1.3062, 1.3055, 1.3049, 1.3042, 1.3036, 1.3031,
	1.3025, 1.3020, 1.3016, 1.3011, 1.3006, 1.3002, 1.2998, 1.2994, 1.2991, 1.2987,
}

var tQuantile95 = [maxForecastFolds]float64{
	12.7062, 4.3027, 3.1824, 2.7764, 2.5706, 2.4469, 2.3646, 2.3060, 2.2622, 2.2281,
	2.2010, 2.1788, 2.1604, 2.1448, 2.1314, 2.1199, 2.1098, 2.1009, 2.0930, 2.0860,
	2.0796, 2.0739, 2.0687, 2.0639, 2.0595, 2.0555, 2.0518, 2.0484, 2.0452, 2.0423,
	2.0395, 2.0369, 2.0345, 2.0322, 2.0301, 2.0281, 2.0262, 2.0244, 2.0227, 2.0211,
	2.0195, 2.0181, 2.0167, 2.0154, 2.0141, 2.0129, 2.0117, 2.0106, 2.0096, 2.0086,
}

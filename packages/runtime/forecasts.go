package runtime

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"

	"github.com/ipfs/go-cid"
)

// Forecasts forecasts many series in one job: a store's sales of each of
// its products, say, or each meter's readings. Each series is forecast as
// the forecast workload forecasts one, every candidate backtested and the
// median of the best kinds chosen, and the series are shared out across
// the pool, a run of them to each task.
//
// Like forecast it is plain arithmetic with no randomness, so two workers
// given the same task return the same bytes.
type Forecasts struct{}

// ForecastsParams are the job parameters.
type ForecastsParams struct {
	// Input is the CID of a CSV file with a line for each value: the
	// series' name in the first column, the value in the last, and its
	// time, if given, in a column between. A series' values are oldest
	// first; the series need not be kept together. A first line whose
	// value is not a number is taken as a header.
	Input string `json:"input"`
	// Horizon, Season, Folds and Models are as for forecast, and apply to
	// every series; Folds 0 gives each series as many backtest windows as
	// fit, up to eight.
	Horizon int    `json:"horizon,omitempty"`
	Season  int    `json:"season,omitempty"`
	Folds   int    `json:"folds,omitempty"`
	Models  string `json:"models,omitempty"`
}

// The bounds on a forecasts job are variables only so that the tests can
// reach them with small files.
var (
	maxForecastsBytes  uint64 = 256 << 20
	maxForecastsSeries        = 200_000
	maxForecastsValues        = 20_000_000
	// maxForecastsStepsPerTask bounds how many steps of forecasts one task
	// returns, so that a task's output stays small: a long horizon gives
	// fewer series to each task.
	maxForecastsStepsPerTask = 20_000
)

type forecastsTask struct {
	Input   string   `json:"input"`
	From    int      `json:"from"`
	To      int      `json:"to"`
	Horizon int      `json:"horizon"`
	Season  int      `json:"season"`
	Folds   int      `json:"folds"`
	Kinds   []string `json:"kinds"`
}

// SeriesForecast is how one series was forecast, or why it was not.
type SeriesForecast struct {
	ID       string          `json:"id"`
	Points   int             `json:"points"`
	Last     string          `json:"last,omitempty"`
	Model    string          `json:"model,omitempty"`
	MAE      float64         `json:"mae,omitempty"`
	RMSE     float64         `json:"rmse,omitempty"`
	Forecast []ForecastPoint `json:"forecast,omitempty"`
	Error    string          `json:"error,omitempty"`
}

type forecastsPart struct {
	From    int              `json:"from"`
	Horizon int              `json:"horizon"`
	Series  []SeriesForecast `json:"series"`
}

// ForecastsResult is the job result.
type ForecastsResult struct {
	Series   int `json:"series"`
	Forecast int `json:"forecast"`
	Failed   int `json:"failed"`
	Horizon  int `json:"horizon"`
	// ForecastTable is the CID of a CSV file of every series' forecast,
	// a line for each step: id,step,value,lo80,hi80,lo95,hi95.
	ForecastTable string `json:"forecast_table"`
	// ModelsTable is the CID of a CSV file of how each series was
	// forecast: id,points,last,model,mae,rmse,error.
	ModelsTable string `json:"models_table"`
	Tasks       int    `json:"tasks"`
}

func (Forecasts) Name() string { return "forecasts" }

func (Forecasts) Describe() string {
	return `Forecasts many time series at once, each as forecast forecasts one: every candidate model backtested on the series' own past and the median of the best kinds chosen, with ranges. The series are shared out across the pool, a run of them to each task. Parameters: {"input": "<CID of a CSV file with a line for each value: the series' name first, the value last, and its time between if given>", "horizon": <steps ahead, default 12>, "season": <steps in the series' cycle, 0 for none>, "folds": <backtest windows, by default as many as fit, up to 8>, "models": "all" or one or more of naive, mean, drift, snaive, ses, holt, holtwinters, ar, theta}. Result: {"series", "forecast", "failed", "forecast_table": "<CID of every forecast as CSV: id,step,value,lo80,hi80,lo95,hi95>", "models_table": "<CID of how each series was forecast as CSV>"}.`
}

func (Forecasts) Form() []Field {
	return []Field{
		{Name: "input", Kind: KindFile, Label: "Series", Help: "A CSV file with a line for each value: the series' name in the first column, the value in the last, oldest first; a header line is skipped.", Required: true},
		{Name: "horizon", Kind: KindInteger, Label: "Steps ahead", Help: "How many steps of each series to forecast.", Default: 12, Minimum: bound(1), Maximum: bound(maxForecastHorizon)},
		{Name: "season", Kind: KindInteger, Label: "Season length", Help: "How many steps the series' cycle takes, such as 12 for months or 7 for days of the week, or 0 if they have none.", Default: 0, Minimum: bound(0), Maximum: bound(maxForecastSeason)},
		{Name: "folds", Kind: KindInteger, Label: "Backtest windows", Help: "How many windows at the end of each series its models are tested on. Leave it empty to use as many as each series has room for, up to 8.", Minimum: bound(1), Maximum: bound(maxForecastFolds)},
		{Name: "models", Kind: KindChoice, Label: "Models", Help: "Which kind of model to try, or all of them.", Default: "all", Choices: append([]string{"all"}, forecastKinds...)},
	}
}

func (Forecasts) Split(ctx context.Context, blobs Blobs, params []byte, parts int) ([][]byte, error) {
	var p ForecastsParams
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, fmt.Errorf("forecasts params: %w", err)
	}
	one := ForecastParams{Input: p.Input, Horizon: p.Horizon, Season: p.Season, Folds: p.Folds, Models: p.Models}
	if err := one.settings(); err != nil {
		return nil, fmt.Errorf("forecasts params: %w", err)
	}
	kinds, err := one.kinds()
	if err != nil {
		return nil, fmt.Errorf("forecasts params: %w", err)
	}
	panel, err := readPanel(ctx, blobs, p.Input)
	if err != nil {
		return nil, err
	}
	// As many tasks as the pool asks for, and at least as many as keep each
	// task's forecasts within bounds; no more than there are series.
	perTask := max(1, maxForecastsStepsPerTask/one.Horizon)
	n := min(len(panel), max(parts, 1, (len(panel)+perTask-1)/perTask))
	n = min(n, MaxTasks)
	var payloads [][]byte
	for i := range n {
		payloads = append(payloads, mustJSON(forecastsTask{Input: p.Input, From: len(panel) * i / n, To: len(panel) * (i + 1) / n, Horizon: one.Horizon, Season: one.Season, Folds: one.Folds, Kinds: kinds}))
	}
	return payloads, nil
}

func (Forecasts) Execute(ctx context.Context, blobs Blobs, payload []byte) ([]byte, error) {
	var task forecastsTask
	if err := json.Unmarshal(payload, &task); err != nil {
		return nil, fmt.Errorf("forecasts payload: %w", err)
	}
	check := ForecastParams{Horizon: task.Horizon, Season: task.Season, Folds: task.Folds}
	if err := check.settings(); err != nil || task.Horizon == 0 {
		return nil, fmt.Errorf("forecasts payload: horizon %d, season %d or folds %d out of range", task.Horizon, task.Season, task.Folds)
	}
	for _, kind := range task.Kinds {
		if !slices.Contains(forecastKinds, kind) {
			return nil, fmt.Errorf("forecasts payload: no such kind of model as %q", kind)
		}
	}
	panel, err := readPanel(ctx, blobs, task.Input)
	if err != nil {
		return nil, err
	}
	if task.From < 0 || task.To > len(panel) || task.From >= task.To {
		return nil, fmt.Errorf("forecasts payload: series %d to %d of %d", task.From, task.To, len(panel))
	}
	report := Report(ctx)
	report.Log(fmt.Sprintf("forecasting series %d to %d of %d", task.From+1, task.To, len(panel)))
	part := forecastsPart{From: task.From, Horizon: task.Horizon}
	for i, s := range panel[task.From:task.To] {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		part.Series = append(part.Series, forecastOne(s, task))
		report.Progress(float64(i+1) / float64(task.To-task.From))
	}
	return mustJSON(part), nil
}

// forecastOne backtests every candidate on one series and decides its
// forecast, as a forecast job of that series alone would.
func forecastOne(s panelSeries, task forecastsTask) SeriesForecast {
	out := SeriesForecast{ID: s.id, Points: len(s.values), Last: s.last}
	folds := task.Folds
	if folds == 0 {
		folds = min(autoFolds, max(1, (len(s.values)-task.Horizon)/task.Horizon))
	}
	shortest := len(s.values) - folds*task.Horizon
	if shortest < 1 {
		out.Error = fmt.Sprintf("%d points are too few to backtest %d windows of %d steps", len(s.values), folds, task.Horizon)
		return out
	}
	var outcomes []forecastOutcome
	for _, m := range forecastCandidates(task.Kinds, task.Season, shortest) {
		outcomes = append(outcomes, backtest(m, s.values, task.Horizon, folds))
	}
	d, err := decide(outcomes, folds)
	if err != nil {
		out.Error = strings.TrimPrefix(err.Error(), "forecast: ")
		return out
	}
	out.Model, out.MAE, out.RMSE, out.Forecast = d.chosen.Model, d.chosen.MAE, d.chosen.RMSE, d.points
	return out
}

func (Forecasts) Aggregate(ctx context.Context, blobs Blobs, outputs [][]byte) ([]byte, error) {
	var result ForecastsResult
	var forecasts, models bytes.Buffer
	forecasts.WriteString("id,step,value,lo80,hi80,lo95,hi95\n")
	models.WriteString("id,points,last,model,mae,rmse,error\n")
	for i, output := range outputs {
		var part forecastsPart
		if err := json.Unmarshal(output, &part); err != nil {
			return nil, fmt.Errorf("task %d output: %w", i, err)
		}
		if part.From != result.Series || len(part.Series) == 0 || (i > 0 && part.Horizon != result.Horizon) || part.Horizon < 1 || part.Horizon > maxForecastHorizon {
			return nil, fmt.Errorf("task %d output: %d series from %d, %d steps ahead, where series %d was next and %d steps were asked for", i, len(part.Series), part.From, part.Horizon, result.Series, result.Horizon)
		}
		result.Horizon = part.Horizon
		for _, s := range part.Series {
			if why := seriesProblem(s, result.Horizon); why != "" {
				return nil, fmt.Errorf("task %d output: series %q: %s", i, s.ID, why)
			}
			result.Series++
			if s.Error != "" {
				result.Failed++
				fmt.Fprintf(&models, "%s,%d,%s,,,,%s\n", csvField(s.ID), s.Points, csvField(s.Last), csvField(s.Error))
				continue
			}
			result.Forecast++
			fmt.Fprintf(&models, "%s,%d,%s,%s,%s,%s,\n", csvField(s.ID), s.Points, csvField(s.Last), csvField(s.Model), plain(s.MAE), plain(s.RMSE))
			for _, p := range s.Forecast {
				fmt.Fprintf(&forecasts, "%s,%d,%s,%s,%s,%s,%s\n", csvField(s.ID), p.Step, plain(p.Value), plain(p.Lo80), plain(p.Hi80), plain(p.Lo95), plain(p.Hi95))
			}
		}
	}
	if result.Series == 0 {
		return nil, errors.New("forecasts: no task returned a series")
	}
	for _, t := range []struct {
		into *string
		data *bytes.Buffer
		what string
	}{{&result.ForecastTable, &forecasts, "forecast"}, {&result.ModelsTable, &models, "models"}} {
		stored, err := blobs.Put(ctx, t.data)
		if err != nil {
			return nil, fmt.Errorf("store the %s table: %w", t.what, err)
		}
		*t.into = stored.String()
	}
	result.Tasks = len(outputs)
	return mustJSON(result), nil
}

// seriesProblem says what is wrong with a series' forecast as a task
// returned it, or "" if nothing is: a name, and either a reason it was not
// forecast or a model and a forecast of every step in order, each within
// its ranges. Its numbers came as JSON, which has no infinities and no
// NaN, so they are finite.
func seriesProblem(s SeriesForecast, horizon int) string {
	if s.ID == "" || s.Points < 1 {
		return "no name, or no points"
	}
	if s.Error != "" {
		if s.Model != "" || len(s.Forecast) > 0 {
			return "both forecast and not"
		}
		return ""
	}
	if s.Model == "" || len(s.Forecast) != horizon {
		return fmt.Sprintf("no model, or not %d steps", horizon)
	}
	for j, p := range s.Forecast {
		if p.Step != j+1 || !(p.Lo95 <= p.Lo80 && p.Lo80 <= p.Value && p.Value <= p.Hi80 && p.Hi80 <= p.Hi95) {
			return fmt.Sprintf("step %d is not a forecast within its ranges", j+1)
		}
	}
	return ""
}

// panelSeries is one series of a forecasts job's file.
type panelSeries struct {
	id     string
	last   string
	values []float64
}

// readPanel reads a forecasts job's file: a line for each value, the
// series' name first, the value last and its time, if given, between.
// Series come in the order their first lines do.
func readPanel(ctx context.Context, blobs Blobs, input string) ([]panelSeries, error) {
	c, err := cid.Decode(input)
	if err != nil {
		return nil, fmt.Errorf("forecasts: input is not a CID: %w", err)
	}
	blob, err := blobs.Open(ctx, c)
	if err != nil {
		return nil, fmt.Errorf("forecasts input %s: %w", c, err)
	}
	defer blob.Close()
	if blob.Size() > maxForecastsBytes {
		return nil, fmt.Errorf("forecasts input %s: %d bytes, more than %d", c, blob.Size(), maxForecastsBytes)
	}
	var panel []panelSeries
	index := map[string]int{}
	lines := bufio.NewScanner(blob)
	lines.Buffer(make([]byte, 0, 4096), 64<<10)
	header, values := true, 0
	for number := 1; lines.Scan(); number++ {
		line := strings.TrimSpace(lines.Text())
		if line == "" {
			continue
		}
		fields := strings.FieldsFunc(line, func(r rune) bool { return r == ',' || r == '\t' || r == ';' })
		if len(fields) < 2 {
			return nil, fmt.Errorf("forecasts input %s: line %d has no series name and value", c, number)
		}
		id, value := strings.TrimSpace(fields[0]), strings.TrimSpace(fields[len(fields)-1])
		v, err := strconv.ParseFloat(value, 64)
		if err != nil && header {
			header = false
			continue
		}
		header = false
		if err != nil || math.IsNaN(v) || math.IsInf(v, 0) {
			return nil, fmt.Errorf("forecasts input %s: line %d: %q is not a number", c, number, value)
		}
		// The line is trimmed and split on its separators, which drops
		// empty fields, so the first field, the name, is never empty.
		i, seen := index[id]
		if !seen {
			if len(panel) == maxForecastsSeries {
				return nil, fmt.Errorf("forecasts input %s: more than %d series", c, maxForecastsSeries)
			}
			i = len(panel)
			index[id] = i
			panel = append(panel, panelSeries{id: id})
		}
		if values++; values > maxForecastsValues || len(panel[i].values) == maxForecastPoints {
			return nil, fmt.Errorf("forecasts input %s: more than %d values, or more than %d in series %q", c, maxForecastsValues, maxForecastPoints, id)
		}
		panel[i].values = append(panel[i].values, v)
		if len(fields) > 2 {
			panel[i].last = strings.TrimSpace(fields[1])
		}
	}
	if err := lines.Err(); err != nil {
		return nil, fmt.Errorf("forecasts input %s: %w", c, err)
	}
	if len(panel) == 0 {
		return nil, fmt.Errorf("forecasts input %s: no series", c)
	}
	return panel, nil
}

package runtime

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"slices"
	"strings"
	"testing"

	"github.com/sisyphus-network/Sisyphus/packages/storage"
)

// seasonalValues are seasonalSeries' values, as a forecast reads them.
func seasonalValues(t *testing.T, points int) []float64 {
	t.Helper()
	store := storage.NewMemory()
	input, _ := store.Put(ctx, strings.NewReader(seasonalSeries(points)))
	values, _, err := readSeries(ctx, store, input.String())
	if err != nil {
		t.Fatal(err)
	}
	return values
}

// A damped trend forecasts what was worked out by hand: the trend is
// multiplied by phi at each step, in the fit and in the forecast.
func TestADampedTrendForecastsWhatItShould(t *testing.T) {
	cases := []struct {
		model forecastModel
		y     []float64
		want  []float64
	}{
		// A straight line, followed exactly: level 4, trend 1, and the
		// forecast adds 0.5, 0.75 and 0.875 of the trend.
		{forecastModel{Kind: "damped", Alpha: 1, Beta: 1, Phi: 0.5}, []float64{1, 2, 3, 4}, []float64{4.5, 4.75, 4.875}},
		// Level 10 and trend 2 to start; after 12 the level is 11.8 and the
		// trend 1.7, after 13 they are 13.08 and 1.32, and the forecast
		// adds 0.8 and 0.8 + 0.64 of the trend.
		{forecastModel{Kind: "damped", Alpha: 0.5, Beta: 0.5, Phi: 0.8}, []float64{10, 12, 13}, []float64{14.136, 14.9808}},
		// No trend, nothing to damp.
		{forecastModel{Kind: "damped", Alpha: 0.5, Beta: 0.1, Phi: 0.9}, []float64{7, 7, 7, 7}, []float64{7, 7, 7}},
	}
	for _, tt := range cases {
		got, err := fitForecast(tt.model, tt.y, len(tt.want))
		if err != nil {
			t.Errorf("%s: %v", tt.model.Name(), err)
			continue
		}
		for i := range got {
			if math.Abs(got[i]-tt.want[i]) > 1e-9 {
				t.Errorf("%s forecasts %v from %v, want %v", tt.model.Name(), got, tt.y, tt.want)
				break
			}
		}
	}
	if name := (forecastModel{Kind: "damped", Alpha: 0.5, Beta: 0.1, Phi: 0.98}).Name(); name != "damped alpha=0.5 beta=0.1 phi=0.98" {
		t.Errorf("a damped candidate is named %q", name)
	}
}

// With phi 1 nothing is damped, and the forecast is Holt's to the bit; with
// phi a hair under 1 it is all but Holt's.
func TestADampedTrendWithPhiOneIsHolts(t *testing.T) {
	values := seasonalValues(t, 60)
	for _, a := range []float64{0.2, 0.5, 0.8} {
		for _, b := range []float64{0.05, 0.1, 0.2} {
			holt, _ := fitForecast(forecastModel{Kind: "holt", Alpha: a, Beta: b}, values, 18)
			same, _ := fitForecast(forecastModel{Kind: "damped", Alpha: a, Beta: b, Phi: 1}, values, 18)
			near, _ := fitForecast(forecastModel{Kind: "damped", Alpha: a, Beta: b, Phi: 1 - 1e-9}, values, 18)
			if !slices.Equal(holt, same) {
				t.Errorf("alpha %g beta %g: phi 1 forecasts %v, Holt's %v", a, b, same, holt)
			}
			for i := range holt {
				if math.Abs(near[i]-holt[i]) > 1e-5 {
					t.Errorf("alpha %g beta %g: phi 1-1e-9 forecasts %v, far from Holt's %v", a, b, near, holt)
					break
				}
			}
		}
	}
}

// Far ahead, a damped trend levels off at level + trend·phi/(1-phi), each
// step adding phi times what the one before it added.
func TestADampedTrendLevelsOff(t *testing.T) {
	values := seasonalValues(t, 60)
	for _, phi := range []float64{0.8, 0.9, 0.98} {
		f, err := fitForecast(forecastModel{Kind: "damped", Alpha: 0.5, Beta: 0.1, Phi: phi}, values, 1000)
		if err != nil {
			t.Fatal(err)
		}
		// f[0] = level + phi·trend and f[1] = level + (phi + phi²)·trend.
		trend := (f[1] - f[0]) / (phi * phi)
		level := f[0] - phi*trend
		if trend == 0 {
			t.Fatalf("phi %g: no trend to damp", phi)
		}
		limit := level + trend*phi/(1-phi)
		if last := f[len(f)-1]; math.Abs(last-limit) > 1e-6*math.Abs(limit) {
			t.Errorf("phi %g: levels off at %g, want %g", phi, last, limit)
		}
		for k := 1; k+1 < 40; k++ {
			step, next := f[k]-f[k-1], f[k+1]-f[k]
			if math.Abs(next-phi*step) > 1e-9*math.Abs(limit) {
				t.Errorf("phi %g: step %d adds %g after %g, not phi times it", phi, k+1, next, step)
				break
			}
		}
	}
}

// A damped trend needs two points, as Holt's does, and settings it can
// take; no other kind takes a phi.
func TestADampedTrendNeedsTwoPointsAndItsSettings(t *testing.T) {
	flat := []float64{5, 5, 5, 5, 5, 5, 5, 5, 5, 5}
	for _, tt := range []struct {
		model forecastModel
		y     []float64
		why   string
	}{
		{forecastModel{Kind: "damped", Alpha: 0.5, Beta: 0.1, Phi: 0.9}, []float64{1, 2, 3}, "needs at least 4 points, 2 to fit"},
		{forecastModel{Kind: "damped", Alpha: 0.5, Beta: 0.1}, flat, "settings it can take"},
		{forecastModel{Kind: "damped", Alpha: 0.5, Beta: 0.1, Phi: 1.5}, flat, "settings it can take"},
		{forecastModel{Kind: "damped", Alpha: 0.5, Phi: 0.9}, flat, "settings it can take"},
		{forecastModel{Kind: "damped", Beta: 0.1, Phi: 0.9}, flat, "settings it can take"},
		{forecastModel{Kind: "holt", Alpha: 0.5, Beta: 0.1, Phi: 0.9}, flat, "settings it can take"},
		{forecastModel{Kind: "chronos", Variant: "tiny", Phi: 0.9}, flat, "settings it can take"},
	} {
		out := backtest(tt.model, tt.y, 2, 1)
		if !strings.Contains(out.Error, tt.why) || out.Forecast != nil {
			t.Errorf("%s: %+v, want an error saying %q", tt.model.Name(), out, tt.why)
		}
	}
	if m := (forecastModel{Kind: "damped", Alpha: 0.5, Beta: 0.1, Phi: 0.9}); m.fewest() != 2 || m.invalid() != "" {
		t.Errorf("%s needs %d points and says %q", m.Name(), m.fewest(), m.invalid())
	}
	if got := forecastCandidates([]string{"damped"}, 0, 1); len(got) != 0 {
		t.Errorf("a point to fit to gave %+v", got)
	}
}

// Damped is tried when a job names it, and not when the job asks for
// all or says nothing; a name that is no kind is still refused, and the
// form offers damped beside the others.
func TestADampedTrendIsTriedOnlyWhenAskedFor(t *testing.T) {
	for _, models := range []string{"", "all"} {
		kinds, err := ForecastParams{Models: models}.kinds()
		if err != nil || slices.Contains(kinds, "damped") || !slices.Equal(kinds, forecastKinds) {
			t.Errorf("models %q asks for %v (%v)", models, kinds, err)
		}
	}
	for models, want := range map[string][]string{
		"damped":                {"damped"},
		" holt, damped ,damped": {"holt", "damped"},
		"damped,chronos-tiny":   {"damped", "chronos-tiny"},
	} {
		if kinds, err := (ForecastParams{Models: models}).kinds(); err != nil || !slices.Equal(kinds, want) {
			t.Errorf("models %q asks for %v (%v), want %v", models, kinds, err, want)
		}
	}
	for _, models := range []string{"dampened", "damped,", "all,damped", "Damped"} {
		if _, err := (ForecastParams{Models: models}).kinds(); err == nil || !strings.Contains(err.Error(), "is none of") || !strings.Contains(err.Error(), "theta, damped") {
			t.Errorf("models %q gave %v, want it refused", models, err)
		}
	}
	if choices := (Forecast{}).modelChoices(); !slices.Contains(choices, "damped") || choices[0] != "all" {
		t.Errorf("the form offers %v", choices)
	}
	if help := (Forecast{}).modelsHelp(); !strings.Contains(help, "only when asked for") {
		t.Errorf("the form says %q", help)
	}
	for _, describe := range []string{Forecast{}.Describe(), Forecasts{}.Describe()} {
		if !strings.Contains(describe, "damped") {
			t.Errorf("the workload does not say it can be asked for damped: %s", describe)
		}
	}

	store := storage.NewMemory()
	input, _ := store.Put(ctx, strings.NewReader(seasonalSeries(60)))
	tried := func(models string) []forecastModel {
		t.Helper()
		payloads, err := Forecast{}.Split(ctx, store, mustJSON(ForecastParams{Input: input.String(), Season: 12, Models: models}), 1)
		if err != nil {
			t.Fatal(err)
		}
		var task forecastTask
		if err := json.Unmarshal(payloads[0], &task); err != nil {
			t.Fatal(err)
		}
		return task.Models
	}
	if slices.ContainsFunc(tried(""), func(m forecastModel) bool { return m.Kind == "damped" || m.Phi != 0 }) {
		t.Error("a job that names no models tried a damped trend")
	}
	// Few settings: three of alpha, one of beta, three of phi.
	damped := tried("damped")
	if len(damped) != 9 {
		t.Errorf("damped tried %d candidates: %+v", len(damped), damped)
	}
	for _, m := range damped {
		if m.Kind != "damped" || m.Beta != 0.1 || !slices.Contains([]float64{0.8, 0.9, 0.98}, m.Phi) || m.invalid() != "" {
			t.Errorf("damped tried %+v", m)
		}
	}
	// Beside the others, damped's candidates come after theta's.
	both := tried("damped,theta,naive")
	if both[0].Kind != "naive" || both[len(both)-1].Kind != "damped" || both[len(both)-10].Kind != "theta" {
		t.Errorf("damped, theta and naive tried %+v", both)
	}

	// A whole job asked for damped alone forecasts with its best candidate.
	result := runForecast(t, store, ForecastParams{Input: input.String(), Season: 12, Models: "damped"}, 3)
	if !strings.HasPrefix(result.Best.Model, "damped alpha=") || result.Chosen != result.Best || len(result.Candidates) != 9 {
		t.Errorf("damped alone forecast with %+v of %+v", result.Chosen, result.Candidates)
	}
}

// forecasts takes damped too, and forecasts each series as forecast would
// with the same models.
func TestManySeriesCanBeForecastWithADampedTrend(t *testing.T) {
	store := storage.NewMemory()
	input, _ := store.Put(ctx, strings.NewReader(panelOf(map[string]string{"north": seasonalSeries(72), "south": seasonalSeries(48), "tiny": seasonalSeries(5)})))
	const models = "naive,holt,theta,damped"
	params := ForecastsParams{Input: input.String(), Horizon: 6, Season: 12, Models: models}
	result := runForecasts(t, store, params, 2)
	if result.Series != 3 || result.Forecast != 2 || result.Failed != 1 {
		t.Errorf("result %+v", result)
	}
	if again := runForecasts(t, store, params, 3); again.ForecastTable != result.ForecastTable || again.ModelsTable != result.ModelsTable {
		t.Error("split differently, the result differs")
	}
	payloads, err := Forecasts{}.Split(ctx, store, mustJSON(params), 1)
	if err != nil {
		t.Fatal(err)
	}
	var task forecastsTask
	if err := json.Unmarshal(payloads[0], &task); err != nil || !slices.Equal(task.Kinds, []string{"naive", "holt", "theta", "damped"}) {
		t.Errorf("the task asks for %v (%v)", task.Kinds, err)
	}
	table := strings.Split(strings.TrimSpace(readStored(t, store, result.ForecastTable)), "\n")
	modelsTable := readStored(t, store, result.ModelsTable)
	for _, s := range []struct {
		id     string
		points int
	}{{"north", 72}, {"south", 48}} {
		one, _ := store.Put(ctx, strings.NewReader(seasonalSeries(s.points)))
		alone := runForecast(t, store, ForecastParams{Input: one.String(), Horizon: 6, Season: 12, Models: models}, 1)
		if !slices.ContainsFunc(alone.Candidates, func(c ForecastScore) bool { return strings.HasPrefix(c.Model, "damped ") }) {
			t.Errorf("%s alone tried no damped trend", s.id)
		}
		if !strings.Contains(modelsTable, s.id+","+fmt.Sprint(s.points)+",2016-"+fmt.Sprintf("%03d", s.points-1)+","+alone.Chosen.Model+",") {
			t.Errorf("%s was not forecast with %q, as alone:\n%s", s.id, alone.Chosen.Model, modelsTable)
		}
		for _, p := range alone.Forecast {
			want := fmt.Sprintf("%s,%d,%s,%s,%s,%s,%s", s.id, p.Step, plain(p.Value), plain(p.Lo80), plain(p.Hi80), plain(p.Lo95), plain(p.Hi95))
			if !slices.Contains(table, want) {
				t.Errorf("%s step %d: no line %q in the forecast table", s.id, p.Step, want)
			}
		}
	}
	// A task naming a kind there is none of is refused, damped or not.
	task.Kinds = []string{"damped", "dampened"}
	if _, err := (Forecasts{}).Execute(ctx, store, mustJSON(task)); err == nil || !strings.Contains(err.Error(), `"dampened"`) {
		t.Errorf("a task asking for dampened gave %v", err)
	}
}

// As TestAForecastGivesTheKnownAnswer, for a job that asks for damped
// beside the others: the outputs are known to the last bit, so that every
// processor CI runs this on must come to the same bytes.
func TestADampedForecastGivesTheKnownAnswer(t *testing.T) {
	store := storage.NewMemory()
	input, _ := store.Put(ctx, strings.NewReader(seasonalSeries(72)))
	payloads, err := Forecast{}.Split(ctx, store, mustJSON(ForecastParams{Input: input.String(), Horizon: 6, Season: 12, Folds: 2, Models: "holt,theta,damped"}), 3)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.New()
	for _, payload := range payloads {
		output, err := Forecast{}.Execute(ctx, store, payload)
		if err != nil {
			t.Fatal(err)
		}
		digest.Write(output)
	}
	const want = "516b4ca03d3e767bc3a4d78465b2ed6764392fd1328eaeec91d7886e6125304b"
	if got := hex.EncodeToString(digest.Sum(nil)); got != want {
		t.Errorf("the tasks' outputs hash to %s, want %s", got, want)
	}
}

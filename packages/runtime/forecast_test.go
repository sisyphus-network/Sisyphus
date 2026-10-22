package runtime

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"slices"
	"strings"
	"testing"

	"github.com/ipfs/go-cid"

	"github.com/sisyphus-network/Sisyphus/packages/storage"
)

// seasonalSeries is monthly data with a trend, a yearly cycle and a
// little irregularity, written with three decimals as a file would have it.
func seasonalSeries(points int) string {
	var b strings.Builder
	b.WriteString("month,sales\n")
	for i := range points {
		v := 100 + 0.5*float64(i) + 10*math.Sin(2*math.Pi*float64(i)/12) + float64((i*7)%5)
		fmt.Fprintf(&b, "2016-%03d,%.3f\n", i, v)
	}
	return b.String()
}

// runForecast runs a whole forecast job split into parts, the way the
// coordinator and its workers would, and returns its result.
func runForecast(t *testing.T, blobs Blobs, params ForecastParams, parts int) ForecastResult {
	t.Helper()
	payloads, err := Forecast{}.Split(ctx, blobs, mustJSON(params), parts)
	if err != nil {
		t.Fatal(err)
	}
	var outputs [][]byte
	for _, payload := range payloads {
		output, err := Forecast{}.Execute(ctx, blobs, payload)
		if err != nil {
			t.Fatal(err)
		}
		outputs = append(outputs, output)
	}
	raw, err := Forecast{}.Aggregate(ctx, blobs, outputs)
	if err != nil {
		t.Fatal(err)
	}
	var result ForecastResult
	if err := json.Unmarshal(raw, &result); err != nil {
		t.Fatal(err)
	}
	return result
}

func readStored(t *testing.T, store *storage.Store, c string) string {
	t.Helper()
	id, err := cid.Decode(c)
	if err != nil {
		t.Fatal(err)
	}
	blob, err := store.Open(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	defer blob.Close()
	data, err := io.ReadAll(blob)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// A seasonal series is forecast a year ahead with the median of the best
// candidate of each of the best kinds of model, every candidate's scores
// are there best first, and the forecast and the scores can be saved as
// CSV.
func TestASeasonalSeriesIsForecast(t *testing.T) {
	if (Forecast{}).Name() != "forecast" || !strings.Contains(Forecast{}.Describe(), `"horizon"`) {
		t.Errorf("forecast calls itself %q and says %q", Forecast{}.Name(), Forecast{}.Describe())
	}
	store := storage.NewMemory()
	input, _ := store.Put(ctx, strings.NewReader(seasonalSeries(120)))
	result := runForecast(t, store, ForecastParams{Input: input.String(), Season: 12, Folds: 4}, 4)

	if result.Points != 120 || result.Last != "2016-119" || result.Horizon != 12 || result.Folds != 4 || result.Tasks != 4 {
		t.Errorf("result says %d points ending %q, %d steps, %d windows, %d tasks", result.Points, result.Last, result.Horizon, result.Folds, result.Tasks)
	}
	if result.Best != result.Candidates[0] || !strings.HasPrefix(result.Best.Model, "ar ") {
		t.Errorf("best is %+v, of %+v", result.Best, result.Candidates[:3])
	}
	if !strings.HasPrefix(result.Chosen.Model, "median of "+result.Best.Model+"; ") || strings.Count(result.Chosen.Model, ";") != combineKinds-1 || result.Chosen.MAE <= 0 {
		t.Errorf("the forecast is %+v", result.Chosen)
	}
	fitted := slices.DeleteFunc(slices.Clone(result.Candidates), func(c ForecastScore) bool { return c.Error != "" })
	if !slices.IsSortedFunc(fitted, func(a, b ForecastScore) int { return cmpFloat(a.MAE, b.MAE) }) || len(fitted) == 0 {
		t.Error("the candidates are not best first")
	}
	kinds := map[string]bool{}
	for _, c := range result.Candidates {
		kinds[strings.Fields(c.Model)[0]] = true
	}
	if len(kinds) != len(forecastKinds) {
		t.Errorf("tried %v, not every kind", kinds)
	}
	if len(result.Forecast) != 12 {
		t.Fatalf("%d steps forecast", len(result.Forecast))
	}
	truth := func(i int) float64 {
		return 100 + 0.5*float64(120+i) + 10*math.Sin(2*math.Pi*float64(120+i)/12) + float64(((120+i)*7)%5)
	}
	// The series has a jiggle of up to 4 that only the autoregression
	// follows; the median of the four kinds keeps to the shape.
	for i, p := range result.Forecast {
		if p.Step != i+1 || math.Abs(p.Value-truth(i)) > 8 || !(p.Lo95 <= p.Lo80 && p.Lo80 <= p.Value && p.Value <= p.Hi80 && p.Hi80 <= p.Hi95) {
			t.Errorf("step %d is %+v, where the series goes on to %.3f", i+1, p, truth(i))
		}
	}
	table := strings.Split(strings.TrimSpace(readStored(t, store, result.ForecastTable)), "\n")
	if table[0] != "step,value,lo80,hi80,lo95,hi95" || len(table) != 13 || !strings.HasPrefix(table[1], "1,") {
		t.Errorf("forecast table:\n%s", strings.Join(table, "\n"))
	}
	scores := strings.Split(strings.TrimSpace(readStored(t, store, result.ScoresTable)), "\n")
	if scores[0] != "model,mae,rmse,error" || len(scores) != len(result.Candidates)+1 || !strings.HasPrefix(scores[1], result.Best.Model+",") {
		t.Errorf("scores table:\n%s", strings.Join(scores, "\n"))
	}

	// With one kind of model there is nothing to combine: the forecast is
	// the best candidate's, which here is the series itself.
	result = runForecast(t, store, ForecastParams{Input: input.String(), Season: 12, Folds: 4, Models: "ar"}, 2)
	if result.Chosen != result.Best {
		t.Errorf("one kind forecast with %+v, not the best, %+v", result.Chosen, result.Best)
	}
	for i, p := range result.Forecast {
		if math.Abs(p.Value-truth(i)) > 0.01 {
			t.Errorf("step %d is %+v, where the series goes on to %.3f", i+1, p, truth(i))
		}
	}
}

func cmpFloat(a, b float64) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

// However the candidates are shared out, the job comes to the same
// answer, and a task run twice returns the same bytes, which is how a job
// is verified.
func TestAForecastIsTheSameHoweverTheJobIsSplit(t *testing.T) {
	store := storage.NewMemory()
	input, _ := store.Put(ctx, strings.NewReader(seasonalSeries(96)))
	params := ForecastParams{Input: input.String(), Horizon: 6, Season: 12, Folds: 3}
	whole := runForecast(t, store, params, 1)
	for _, parts := range []int{2, 5, 100} {
		got := runForecast(t, store, params, parts)
		got.Tasks = whole.Tasks
		if string(mustJSON(got)) != string(mustJSON(whole)) {
			t.Errorf("split into %d, the result differs from one task's", parts)
		}
	}
	payloads, _ := Forecast{}.Split(ctx, store, mustJSON(params), 3)
	first, _ := Forecast{}.Execute(ctx, store, payloads[1])
	second, _ := Forecast{}.Execute(ctx, store, payloads[1])
	if string(first) != string(second) {
		t.Error("a task run twice returned different bytes")
	}
}

// The answer is known to the last bit, and CI runs this on every processor
// the daemon is built for: a forecast checked by comparing two workers'
// bytes must come out the same on all of them.
func TestAForecastGivesTheKnownAnswer(t *testing.T) {
	store := storage.NewMemory()
	input, _ := store.Put(ctx, strings.NewReader(seasonalSeries(72)))
	payloads, err := Forecast{}.Split(ctx, store, mustJSON(ForecastParams{Input: input.String(), Horizon: 6, Season: 12, Folds: 2}), 3)
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
	const want = "ff3882b5c2092e2eb24ef9968e957d4dac906925a59e2b4a967b0045b897c029"
	if got := hex.EncodeToString(digest.Sum(nil)); got != want {
		t.Errorf("the tasks' outputs hash to %s, want %s", got, want)
	}
}

// Each kind of model forecasts what it should from a series it fits
// exactly.
func TestEachForecastModelForecastsWhatItShould(t *testing.T) {
	cases := []struct {
		model forecastModel
		y     []float64
		want  []float64
	}{
		{forecastModel{Kind: "naive"}, []float64{1, 2, 3}, []float64{3, 3}},
		{forecastModel{Kind: "mean"}, []float64{1, 2, 3}, []float64{2, 2}},
		{forecastModel{Kind: "drift"}, []float64{1, 2, 3}, []float64{4, 5}},
		{forecastModel{Kind: "snaive", Season: 2}, []float64{1, 5, 2, 6}, []float64{2, 6, 2}},
		{forecastModel{Kind: "ses", Alpha: 1}, []float64{4, 7, 9}, []float64{9, 9}},
		{forecastModel{Kind: "holt", Alpha: 1, Beta: 1}, []float64{1, 2, 3, 4}, []float64{5, 6}},
		{forecastModel{Kind: "holtwinters", Alpha: 0.5, Beta: 0.5, Gamma: 0.5, Season: 2}, []float64{1, 3, 1, 3, 1, 3, 1, 3}, []float64{1, 3, 1}},
		{forecastModel{Kind: "ar", Order: 1}, []float64{1, 2, 4, 8, 16, 32}, []float64{64, 128}},
		// The last value, with half the series' slope added from half a
		// step ahead.
		{forecastModel{Kind: "theta", Alpha: 1}, []float64{1, 2, 3, 4}, []float64{4.5, 5}},
		// Too short to test for its cycle of 4: forecast as it is.
		{forecastModel{Kind: "theta", Alpha: 1, Season: 4}, []float64{1, 2, 3, 4, 5}, []float64{5.5, 6}},
		// Long enough to test, but with no cycle of 2: as it is.
		{forecastModel{Kind: "theta", Alpha: 1, Season: 2}, []float64{1, 2, 3, 4, 5, 6, 7, 8}, []float64{8.5, 9}},
		// A level times a cycle: the cycle is taken out, leaving the
		// level, and put back.
		{forecastModel{Kind: "theta", Alpha: 0.5, Season: 2}, slices.Repeat([]float64{1, 3}, 12), []float64{1, 3, 1}},
		{forecastModel{Kind: "theta", Alpha: 0.5, Season: 3}, slices.Repeat([]float64{2, 4, 6}, 6), []float64{2, 4, 6}},
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
}

// A candidate that cannot be fitted is scored as such, saying why, and
// the job goes on with the others.
func TestACandidateThatCannotBeFittedSaysWhy(t *testing.T) {
	flat := []float64{5, 5, 5, 5, 5, 5, 5, 5, 5, 5}
	for _, tt := range []struct {
		model forecastModel
		y     []float64
		why   string
	}{
		{forecastModel{Kind: "ar", Order: 1}, flat, "too regular"},
		{forecastModel{Kind: "ar", Order: 2}, []float64{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14}, "too regular"},
		{forecastModel{Kind: "crystal ball"}, flat, "no such kind"},
		{forecastModel{Kind: "ses"}, flat, "settings it can take"},
		{forecastModel{Kind: "holt", Alpha: 0.5, Beta: 2}, flat, "settings it can take"},
		{forecastModel{Kind: "snaive", Season: 1}, flat, "settings it can take"},
		{forecastModel{Kind: "holtwinters", Alpha: 0.5, Beta: 0.5, Gamma: 0.5, Season: 2000}, flat, "settings it can take"},
		{forecastModel{Kind: "ar", Order: -1}, flat, "settings it can take"},
		{forecastModel{Kind: "theta", Alpha: 0.5, Season: 1}, flat, "settings it can take"},
		{forecastModel{Kind: "holtwinters", Alpha: 0.5, Beta: 0.5, Gamma: 0.5, Season: 4}, flat, "needs at least 12 points"},
	} {
		out := backtest(tt.model, tt.y, 2, 2)
		if !strings.Contains(out.Error, tt.why) || out.Forecast != nil {
			t.Errorf("%s: %+v, want an error saying %q", tt.model.Name(), out, tt.why)
		}
	}
	// A series that can be fitted whole, but is too regular in a backtest
	// window's past, fails when that window is.
	out := backtest(forecastModel{Kind: "ar", Order: 1}, []float64{3, 3, 3, 3, 3, 3, 3, 3, 3, 3, 1, 2}, 1, 1)
	if !strings.Contains(out.Error, "too regular") {
		t.Errorf("got %+v", out)
	}
}

func TestForecastSplitChecksItsParameters(t *testing.T) {
	store := storage.NewMemory()
	series, _ := store.Put(ctx, strings.NewReader(seasonalSeries(60)))
	short, _ := store.Put(ctx, strings.NewReader("1\n2\n3\n"))
	missing, _ := storage.NewMemory().Put(ctx, strings.NewReader("kept somewhere else"))
	in := series.String()
	for _, tt := range []struct {
		params string
		why    string
	}{
		{`{`, "forecast params"},
		{`{"input":"` + in + `","horizon":1001}`, "horizon"},
		{`{"input":"` + in + `","horizon":-1}`, "horizon"},
		{`{"input":"` + in + `","season":1}`, "season"},
		{`{"input":"` + in + `","season":-2}`, "season"},
		{`{"input":"` + in + `","season":1001}`, "season"},
		{`{"input":"` + in + `","folds":51}`, "folds"},
		{`{"input":"` + in + `","folds":-1}`, "folds"},
		{`{"input":"` + in + `","tasks":-1}`, "tasks"},
		{`{"input":"` + in + `","tasks":10001}`, "tasks"},
		{`{"input":"` + in + `","models":"naive,tea leaves"}`, "tea leaves"},
		{`{"input":"not a cid"}`, "not a CID"},
		{`{"input":"` + missing.String() + `"}`, "forecast input"},
		{`{"input":"` + short.String() + `"}`, "too short"},
		{`{"input":"` + in + `","models":"snaive"}`, "no model"},
	} {
		if _, err := (Forecast{}).Split(ctx, store, []byte(tt.params), 3); err == nil || !strings.Contains(err.Error(), tt.why) {
			t.Errorf("%s: error %v, want one about %q", tt.params, err, tt.why)
		}
	}

	split := func(params ForecastParams, parts int) []forecastTask {
		t.Helper()
		payloads, err := Forecast{}.Split(ctx, store, mustJSON(params), parts)
		if err != nil {
			t.Fatal(err)
		}
		var tasks []forecastTask
		for _, p := range payloads {
			var task forecastTask
			json.Unmarshal(p, &task)
			tasks = append(tasks, task)
		}
		return tasks
	}
	if tasks := split(ForecastParams{Input: in, Tasks: 2}, 10); len(tasks) != 2 {
		t.Errorf("tasks 2 split into %d", len(tasks))
	}
	tasks := split(ForecastParams{Input: in, Models: " ses, naive,ses"}, 0)
	if len(tasks) != 1 || len(tasks[0].Models) != 7 || tasks[0].Models[0].Kind != "naive" || tasks[0].Horizon != 12 || tasks[0].Folds != 4 {
		t.Errorf("ses and naive came to %+v", tasks)
	}
	// A week's season adds an autoregression of order 7 to those tried.
	if task := split(ForecastParams{Input: in, Season: 7, Horizon: 1, Folds: 1, Models: "ar"}, 1)[0]; !slices.ContainsFunc(task.Models, func(m forecastModel) bool { return m.Order == 7 }) {
		t.Errorf("season 7 tried %+v", task.Models)
	}
	// Without a season, the seasonal models are not tried; with a long
	// one, an autoregression of its order is not either.
	for _, task := range split(ForecastParams{Input: in, Season: 30, Horizon: 1, Folds: 1, Models: "ar,snaive"}, 1) {
		for _, m := range task.Models {
			if m.Order == 30 {
				t.Errorf("an autoregression of order %d was tried", m.Order)
			}
		}
	}
}

func TestASeriesIsReadAsItShouldBe(t *testing.T) {
	store := storage.NewMemory()
	read := func(content string) ([]float64, string, error) {
		input, _ := store.Put(ctx, strings.NewReader(content))
		return readSeries(ctx, store, input.String())
	}
	values, last, err := read("\n\nvalue\n1.5\n\n2\n-3e2\n")
	if err != nil || !slices.Equal(values, []float64{1.5, 2, -300}) || last != "7" {
		t.Errorf("one column: %v, last %q, %v", values, last, err)
	}
	values, last, err = read("2024-01\t1;x;4\n2024-02\t5\n")
	if err != nil || !slices.Equal(values, []float64{4, 5}) || last != "2024-02" {
		t.Errorf("tabs and semicolons: %v, last %q, %v", values, last, err)
	}
	values, _, err = read(",,,\n1\n2\n")
	if err != nil || !slices.Equal(values, []float64{1, 2}) {
		t.Errorf("an empty header: %v, %v", values, err)
	}
	for _, tt := range []struct{ content, why string }{
		{"t,v\n1,2\nx,three\n", `line 3: "three"`},
		{"1\n,,\n", `line 2: ""`},
		{"1\nNaN\n", "NaN"},
		{"1\n+Inf\n", "Inf"},
		{"1\n", "too few"},
		{strings.Repeat("9", 70<<10) + "\n", "too long"},
		{strings.Repeat("1\n", maxForecastPoints+1), "more than"},
		{strings.Repeat(" ", maxForecastBytes+1), "bytes"},
	} {
		if _, _, err := read(tt.content); err == nil || !strings.Contains(err.Error(), tt.why) {
			content := tt.content
			if len(content) > 40 {
				content = content[:40] + "…"
			}
			t.Errorf("%q: error %v, want one saying %q", content, err, tt.why)
		}
	}
}

func TestForecastExecuteChecksItsTask(t *testing.T) {
	store := storage.NewMemory()
	input, _ := store.Put(ctx, strings.NewReader(seasonalSeries(30)))
	in := input.String()
	for _, tt := range []struct{ payload, why string }{
		{`[`, "forecast payload"},
		{`{"input":"` + in + `","horizon":0,"folds":1}`, "out of range"},
		{`{"input":"` + in + `","horizon":1,"folds":51}`, "out of range"},
		{`{"input":"` + in + `","horizon":1001,"folds":1}`, "out of range"},
		{`{"input":"` + in + `","horizon":1,"folds":0}`, "out of range"},
		{`{"input":"x","horizon":1,"folds":1}`, "not a CID"},
	} {
		if _, err := (Forecast{}).Execute(ctx, store, []byte(tt.payload)); err == nil || !strings.Contains(err.Error(), tt.why) {
			t.Errorf("%s: error %v, want one about %q", tt.payload, err, tt.why)
		}
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	payload := mustJSON(forecastTask{Input: in, Horizon: 1, Folds: 1, Models: []forecastModel{{Kind: "naive"}}})
	if _, err := (Forecast{}).Execute(cancelled, store, payload); err == nil {
		t.Error("a cancelled task ran on")
	}
}

func TestForecastAggregateChecksWhatTheTasksReturned(t *testing.T) {
	store := storage.NewMemory()
	good := forecastOutcome{Model: forecastModel{Kind: "naive"}, MAE: 1, RMSE: 1, StepRMSE: []float64{1}, Forecast: []float64{2}}
	part := func(points int, outcomes ...forecastOutcome) []byte {
		return mustJSON(forecastPart{Points: points, Horizon: 1, Folds: 1, Last: "9", Candidates: outcomes})
	}
	failed := forecastOutcome{Model: forecastModel{Kind: "ar", Order: 1}, Error: "too regular, it said"}
	short := good
	short.Forecast = nil
	for _, tt := range []struct {
		outputs [][]byte
		why     string
	}{
		{[][]byte{[]byte("{")}, "task 0 output"},
		{[][]byte{part(10, good), part(11, good)}, "task 1 output"},
		{[][]byte{mustJSON(forecastPart{Points: 10, Horizon: 1, Last: "9", Candidates: []forecastOutcome{good}})}, "0 backtest windows"},
		{[][]byte{mustJSON(forecastPart{Points: 10, Horizon: 1, Folds: 51, Last: "9", Candidates: []forecastOutcome{good}})}, "51 backtest windows"},
		{[][]byte{part(10, short)}, "not finite numbers for 1 steps and 1 windows"},
		{nil, "there were none"},
		{[][]byte{part(10, failed)}, "ar order=1: too regular"},
	} {
		if _, err := (Forecast{}).Aggregate(ctx, store, tt.outputs); err == nil || !strings.Contains(err.Error(), tt.why) {
			t.Errorf("error %v, want one saying %q", err, tt.why)
		}
	}

	// One that failed is listed last, its reason quoted in the table.
	raw, err := Forecast{}.Aggregate(ctx, store, [][]byte{part(10, failed), part(10, good)})
	if err != nil {
		t.Fatal(err)
	}
	var result ForecastResult
	json.Unmarshal(raw, &result)
	if result.Best.Model != "naive" || result.Candidates[1].Error == "" {
		t.Errorf("got %+v", result.Candidates)
	}
	if scores := readStored(t, store, result.ScoresTable); !strings.Contains(scores, `ar order=1,,,"too regular, it said"`) {
		t.Errorf("scores table:\n%s", scores)
	}

	for _, fail := range []int{1, 2} {
		puts := 0
		blobs := countingBlobs{Store: store, fail: fail, puts: &puts}
		if _, err := (Forecast{}).Aggregate(ctx, blobs, [][]byte{part(10, good)}); err == nil || !strings.Contains(err.Error(), "store the") {
			t.Errorf("with store %d failing: error %v", fail, err)
		}
	}
}

// countingBlobs fails its fail-th Put.
type countingBlobs struct {
	*storage.Store
	fail int
	puts *int
}

func (c countingBlobs) Put(ctx context.Context, r io.Reader) (cid.Cid, error) {
	*c.puts++
	if *c.puts == c.fail {
		return cid.Undef, errBlobs
	}
	return c.Store.Put(ctx, r)
}

func TestOnlyFiniteNumbersAreWellFormed(t *testing.T) {
	good := forecastOutcome{MAE: 1, RMSE: 1, StepRMSE: []float64{1}, Forecast: []float64{1}, Residuals: [][]float64{{1}, {2}}}
	if !wellFormed(good, 1, 2) {
		t.Error("finite numbers are not taken")
	}
	for _, bad := range []float64{math.NaN(), math.Inf(1), math.Inf(-1)} {
		o := good
		o.Forecast = []float64{bad}
		if wellFormed(o, 1, 2) {
			t.Errorf("%v is taken", bad)
		}
		o = good
		o.Residuals = [][]float64{{1}, {bad}}
		if wellFormed(o, 1, 2) {
			t.Errorf("a residual of %v is taken", bad)
		}
	}
	// Residuals are for each step of each window, or not sent at all.
	for _, residuals := range [][][]float64{{{1}}, {{1}, {1, 2}}, {}} {
		o := good
		o.Residuals = residuals
		if wellFormed(o, 1, 2) {
			t.Errorf("residuals %v are taken for 2 windows of 1 step", residuals)
		}
	}
	good.Residuals = nil
	if !wellFormed(good, 1, 2) {
		t.Error("a candidate that sent no residuals is not taken")
	}
	if csvField("plain") != "plain" || csvField(`a "b", c`) != `"a ""b"", c"` {
		t.Error("CSV fields are not quoted as they should be")
	}
}

// The forecast is the median of the best candidate of each of the best
// kinds, and its backtest errors the median of theirs; it is not made
// when fewer than two kinds were fitted or a candidate sent no residuals.
func TestTheForecastIsTheMedianOfTheBestKinds(t *testing.T) {
	outcome := func(kind string, alpha float64, forecast, residuals []float64) forecastOutcome {
		return forecastOutcome{Model: forecastModel{Kind: kind, Alpha: alpha}, Forecast: forecast, Residuals: [][]float64{residuals}}
	}
	sorted := []forecastOutcome{
		outcome("ses", 0.5, []float64{10, 20}, []float64{1, -4}),
		outcome("ses", 0.2, []float64{99, 99}, []float64{0, 0}),
		outcome("naive", 0, []float64{12, 26}, []float64{-1, 2}),
		outcome("drift", 0, []float64{11, 22}, []float64{3, 0}),
		outcome("mean", 0, []float64{14, 18}, []float64{-2, -2}),
		outcome("holt", 0.2, []float64{0, 0}, []float64{50, 50}),
		{Model: forecastModel{Kind: "ar", Order: 1}, Error: "too regular"},
	}
	// The first four kinds, ses at its best: the median of four is the
	// mean of the middle two.
	got, name, ok := combine(sorted, 1)
	if !ok || name != "median of ses alpha=0.5; naive; drift; mean" {
		t.Fatalf("combined %q (%v)", name, ok)
	}
	if !slices.Equal(got.Forecast, []float64{11.5, 21}) {
		t.Errorf("forecast %v, not [11.5 21]", got.Forecast)
	}
	// Residuals {1, -1, 3, -2} and {-4, 2, 0, -2} have medians 0 and -1.
	if got.MAE != 0.5 || got.RMSE != math.Sqrt(0.5) || !slices.Equal(got.StepRMSE, []float64{0, 1}) {
		t.Errorf("backtest %+v", got)
	}
	// Three kinds, before one that failed: the median of three is the
	// middle one.
	got, _, ok = combine([]forecastOutcome{sorted[0], sorted[2], sorted[3], sorted[6], sorted[4]}, 1)
	if !ok || !slices.Equal(got.Forecast, []float64{11, 22}) {
		t.Errorf("of three kinds, forecast %v (%v)", got.Forecast, ok)
	}

	for _, c := range [][]forecastOutcome{
		sorted[:2],             // one kind
		{sorted[0], sorted[6]}, // one kind fitted
		{sorted[0], {Model: forecastModel{Kind: "mean"}, Forecast: []float64{1, 1}}}, // no residuals
	} {
		if _, _, ok := combine(c, 1); ok {
			t.Errorf("combined %+v", c)
		}
	}
}

// A job that does not say how many windows to backtest on gets as many as
// leave a horizon's worth of points before them, up to eight.
func TestABacktestHasAsManyWindowsAsFit(t *testing.T) {
	store := storage.NewMemory()
	for _, c := range []struct{ points, horizon, want int }{{120, 12, 8}, {40, 12, 2}, {20, 12, 1}} {
		input, _ := store.Put(ctx, strings.NewReader(seasonalSeries(c.points)))
		payloads, err := Forecast{}.Split(ctx, store, mustJSON(ForecastParams{Input: input.String(), Horizon: c.horizon}), 1)
		if err != nil {
			t.Fatal(err)
		}
		var task forecastTask
		json.Unmarshal(payloads[0], &task)
		if task.Folds != c.want {
			t.Errorf("%d points, %d steps ahead: %d windows, not %d", c.points, c.horizon, task.Folds, c.want)
		}
	}
}

// A forecast's ranges are as wide as the spread of its backtest errors
// times Student's t for as many degrees of freedom as there were windows,
// and never narrower further ahead than nearer.
func TestTheRangesWidenWithFewerWindowsAndFurtherAhead(t *testing.T) {
	store := storage.NewMemory()
	outcome := forecastOutcome{Model: forecastModel{Kind: "naive"}, MAE: 1, RMSE: 2, StepRMSE: []float64{3, 1, 1}, Forecast: []float64{10, 10, 10}}
	ranges := func(folds int) []ForecastPoint {
		raw, err := Forecast{}.Aggregate(ctx, store, [][]byte{mustJSON(forecastPart{Points: 30, Horizon: 3, Folds: folds, Last: "29", Candidates: []forecastOutcome{outcome}})})
		if err != nil {
			t.Fatal(err)
		}
		var result ForecastResult
		json.Unmarshal(raw, &result)
		return result.Forecast
	}
	one, eight := ranges(1), ranges(8)
	// Step 1's spread is its own error, 3; step 2's is no less; step 3's
	// is the overall error grown as the square root of the steps ahead,
	// 2·sqrt(3/2), which is less than 3.
	for j, p := range eight {
		if half := p.Hi95 - p.Value; math.Abs(half-2.3060*3) > 1e-3 || math.Abs((p.Hi80-p.Value)-1.3968*3) > 1e-3 {
			t.Errorf("step %d with 8 windows: %+v", j+1, p)
		}
	}
	if half := one[0].Hi95 - one[0].Value; math.Abs(half-12.7062*3) > 1e-3 {
		t.Errorf("with one window the 95%% range reaches %v either side, not %v", half, 12.7062*3)
	}
	// A larger overall error widens the later steps.
	outcome.RMSE = 4
	if p := ranges(8)[2]; math.Abs((p.Hi95-p.Value)-2.3060*4*math.Sqrt(1.5)) > 1e-3 {
		t.Errorf("step 3 with an overall error of 4: %+v", p)
	}
}

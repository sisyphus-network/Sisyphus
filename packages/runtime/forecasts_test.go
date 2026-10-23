package runtime

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/sisyphus-network/Sisyphus/packages/storage"
)

// panelOf writes series as a forecasts job's file: a header, then each
// series' lines, its name, its time and its value, in the order of the
// names. Each series is given as forecast's tests write one, a header line
// then month,value lines.
func panelOf(series map[string]string) string {
	var b strings.Builder
	b.WriteString("store,month,sales\n")
	names := make([]string, 0, len(series))
	for name := range series {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		for _, line := range strings.Split(strings.TrimSpace(series[name]), "\n")[1:] {
			fmt.Fprintf(&b, "%s,%s\n", name, line)
		}
	}
	return b.String()
}

// runForecasts runs a forecasts job split into parts and returns its result.
func runForecasts(t *testing.T, blobs Blobs, params ForecastsParams, parts int) ForecastsResult {
	t.Helper()
	payloads, err := Forecasts{}.Split(ctx, blobs, mustJSON(params), parts)
	if err != nil {
		t.Fatal(err)
	}
	var outputs [][]byte
	for _, p := range payloads {
		out, err := Forecasts{}.Execute(ctx, blobs, p)
		if err != nil {
			t.Fatal(err)
		}
		outputs = append(outputs, out)
	}
	raw, err := Forecasts{}.Aggregate(ctx, blobs, outputs)
	if err != nil {
		t.Fatal(err)
	}
	var result ForecastsResult
	if err := json.Unmarshal(raw, &result); err != nil {
		t.Fatal(err)
	}
	return result
}

// Each series is forecast as a forecast job of it alone would forecast
// it, a series too short to backtest is listed with why, and the tables
// hold every step of every forecast and how each series was forecast.
func TestManySeriesAreForecastEachAsOneWouldBe(t *testing.T) {
	if (Forecasts{}).Name() != "forecasts" || !strings.Contains(Forecasts{}.Describe(), `"models_table"`) {
		t.Errorf("forecasts calls itself %q and says %q", Forecasts{}.Name(), Forecasts{}.Describe())
	}
	store := storage.NewMemory()
	// Blank lines are passed over.
	input, _ := store.Put(ctx, strings.NewReader(panelOf(map[string]string{"north": seasonalSeries(72), "south": seasonalSeries(60), "tiny": seasonalSeries(5)})+"\n  \n"))
	result := runForecasts(t, store, ForecastsParams{Input: input.String(), Horizon: 6, Season: 12}, 2)
	if result.Series != 3 || result.Forecast != 2 || result.Failed != 1 || result.Horizon != 6 || result.Tasks != 2 {
		t.Errorf("result %+v", result)
	}

	models := strings.Split(strings.TrimSpace(readStored(t, store, result.ModelsTable)), "\n")
	if len(models) != 4 || models[0] != "id,points,last,model,mae,rmse,error" || !strings.HasPrefix(models[1], "north,72,2016-071,median of ") || !strings.HasPrefix(models[3], "tiny,5,2016-004,,,,5 points are too few") {
		t.Errorf("models table:\n%s", strings.Join(models, "\n"))
	}
	table := strings.Split(strings.TrimSpace(readStored(t, store, result.ForecastTable)), "\n")
	if len(table) != 1+2*6 || table[0] != "id,step,value,lo80,hi80,lo95,hi95" {
		t.Fatalf("forecast table:\n%s", strings.Join(table, "\n"))
	}
	// The same as a forecast job of north alone.
	one, _ := store.Put(ctx, strings.NewReader(seasonalSeries(72)))
	alone := runForecast(t, store, ForecastParams{Input: one.String(), Horizon: 6, Season: 12}, 1)
	for j, p := range alone.Forecast {
		want := fmt.Sprintf("north,%d,%s,%s,%s,%s,%s", p.Step, plain(p.Value), plain(p.Lo80), plain(p.Hi80), plain(p.Lo95), plain(p.Hi95))
		if table[1+j] != want {
			t.Errorf("north step %d is %q, where forecast alone gives %q", j+1, table[1+j], want)
		}
	}
	if !strings.Contains(models[1], ","+alone.Chosen.Model+",") {
		t.Errorf("north was forecast with %q, where forecast alone chose %q", models[1], alone.Chosen.Model)
	}
}

// However the series are shared out, the job comes to the same answer, and
// a task run twice returns the same bytes.
func TestForecastsAreTheSameHoweverTheJobIsSplit(t *testing.T) {
	store := storage.NewMemory()
	series := map[string]string{}
	for i := range 7 {
		series[fmt.Sprintf("s%d", i)] = seasonalSeries(40 + 4*i)
	}
	input, _ := store.Put(ctx, strings.NewReader(panelOf(series)))
	params := ForecastsParams{Input: input.String(), Horizon: 4, Season: 12, Folds: 2}
	whole := runForecasts(t, store, params, 1)
	for _, parts := range []int{3, 7, 100} {
		got := runForecasts(t, store, params, parts)
		if got.ForecastTable != whole.ForecastTable || got.ModelsTable != whole.ModelsTable || got.Tasks != min(parts, 7) {
			t.Errorf("split into %d, the result is %+v, not %+v", parts, got, whole)
		}
	}
	payloads, _ := Forecasts{}.Split(ctx, store, mustJSON(params), 3)
	first, _ := Forecasts{}.Execute(ctx, store, payloads[1])
	second, _ := Forecasts{}.Execute(ctx, store, payloads[1])
	if string(first) != string(second) {
		t.Error("a task run twice returned different bytes")
	}
	// A long horizon gives each task fewer series, so that its output
	// stays small, whatever the pool asks for.
	defer func(was int) { maxForecastsStepsPerTask = was }(maxForecastsStepsPerTask)
	maxForecastsStepsPerTask = 8
	if payloads, _ := (Forecasts{}).Split(ctx, store, mustJSON(params), 1); len(payloads) != 4 {
		t.Errorf("with room for two series a task, 7 series were split into %d tasks", len(payloads))
	}
}

func TestForecastsSplitChecksItsParametersAndFile(t *testing.T) {
	store := storage.NewMemory()
	put := func(text string) string { c, _ := store.Put(ctx, strings.NewReader(text)); return c.String() }
	good := put(panelOf(map[string]string{"a": seasonalSeries(30)}))
	for _, c := range []struct{ params, why string }{
		{`{`, "forecasts params"},
		{`{"input":"` + good + `","horizon":1001}`, "horizon"},
		{`{"input":"` + good + `","models":"tea leaves"}`, "tea leaves"},
		{`{"input":"x"}`, "not a CID"},
		{`{"input":"` + put("") + `"}`, "no series"},
		{`{"input":"` + put("a,1\n2\n") + `"}`, "line 2 has no series name and value"},
		{`{"input":"` + put("a,1\na,x\n") + `"}`, `line 2: "x" is not a number`},
		{`{"input":"` + put(`a,"1`+"\n") + `"}`, "invalid CSV"},
		{`{"input":"` + put(",1\n") + `"}`, "line 1 has no series name and value"},
		{`{"input":"` + put("a,1\na,NaN\n") + `"}`, `"NaN" is not a number`},
		{`{"input":"` + put("a,"+strings.Repeat("1", 70<<10)+"\n") + `"}`, "token too long"},
	} {
		if _, err := (Forecasts{}).Split(ctx, store, []byte(c.params), 2); err == nil || !strings.Contains(err.Error(), c.why) {
			t.Errorf("%s: error %v, want one saying %q", c.params, err, c.why)
		}
	}
	// The file's bounds: its size, its series, its values, and the values
	// of any one series.
	missing := `{"input":"bafkreigh2akiscaildcqabsyg3dfr6chu3fgpregiymsck7e7aqa4s52zy"}`
	if _, err := (Forecasts{}).Split(ctx, store, []byte(missing), 1); err == nil || !strings.Contains(err.Error(), "forecasts input") {
		t.Errorf("a file that is not stored gave %v", err)
	}
	limits := []struct {
		set  func()
		text string
		why  string
	}{
		{func() { maxForecastsBytes = 4 }, "a,1\nb,2\n", "more than 4"},
		{func() { maxForecastsSeries = 1 }, "a,1\nb,2\n", "more than 1 series"},
		{func() { maxForecastsValues = 2 }, "a,1\na,2\na,3\n", "more than 2 values"},
	}
	defer func(bytes uint64, series, values int) {
		maxForecastsBytes, maxForecastsSeries, maxForecastsValues = bytes, series, values
	}(maxForecastsBytes, maxForecastsSeries, maxForecastsValues)
	for _, l := range limits {
		maxForecastsBytes, maxForecastsSeries, maxForecastsValues = 256<<20, 200_000, 20_000_000
		l.set()
		if _, err := (Forecasts{}).Split(ctx, store, []byte(`{"input":"`+put(l.text)+`"}`), 1); err == nil || !strings.Contains(err.Error(), l.why) {
			t.Errorf("error %v, want one saying %q", err, l.why)
		}
	}
}

func TestReadPanelKeepsQuotedNamesAndDetectsSeparators(t *testing.T) {
	if got := panelDelimiter(`a,"1"`); got != ',' {
		t.Errorf("panelDelimiter returned %q for a comma-delimited row with a quoted last field", got)
	}
	if got := panelDelimiter(`"a""b",1`); got != ',' {
		t.Errorf("panelDelimiter returned %q for a comma-delimited row with an escaped quote", got)
	}
	store := storage.NewMemory()
	for _, test := range []struct {
		name      string
		delimiter string
		series    string
		want      string
	}{
		{name: "comma in a comma-delimited name", delimiter: ",", series: `"North, East"`},
		{name: "escaped quote and comma in a name", delimiter: ",", series: `"North ""East"", West"`, want: `North "East", West`},
		{name: "semicolon in a semicolon-delimited name", delimiter: ";", series: `"North; East"`},
		{name: "tab in a tab-delimited name", delimiter: "\t", series: "\"North\tEast\""},
	} {
		t.Run(test.name, func(t *testing.T) {
			input := "series" + test.delimiter + "time" + test.delimiter + "value\n" +
				test.series + test.delimiter + "Jan" + test.delimiter + "1\n" +
				test.series + test.delimiter + "Feb" + test.delimiter + "2\n"
			cid, err := store.Put(ctx, strings.NewReader(input))
			if err != nil {
				t.Fatal(err)
			}
			panel, err := readPanel(ctx, store, cid.String())
			if err != nil {
				t.Fatal(err)
			}
			want := test.want
			if want == "" {
				want = strings.Trim(test.series, `"`)
			}
			if len(panel) != 1 || panel[0].id != want || panel[0].last != "Feb" || !slices.Equal(panel[0].values, []float64{1, 2}) {
				t.Errorf("readPanel returned %+v, want one %q series with the last time and both values", panel, want)
			}
		})
	}
}

func TestForecastsExecuteChecksItsTask(t *testing.T) {
	store := storage.NewMemory()
	in, _ := store.Put(ctx, strings.NewReader(panelOf(map[string]string{"a": seasonalSeries(30)})))
	task := func(change func(*forecastsTask)) []byte {
		ft := forecastsTask{Input: in.String(), From: 0, To: 1, Horizon: 3, Kinds: []string{"naive"}}
		change(&ft)
		return mustJSON(ft)
	}
	for _, c := range []struct {
		payload []byte
		why     string
	}{
		{[]byte("{"), "forecasts payload"},
		{task(func(ft *forecastsTask) { ft.Horizon = 0 }), "out of range"},
		{task(func(ft *forecastsTask) { ft.Season = 1 }), "out of range"},
		{task(func(ft *forecastsTask) { ft.Kinds = []string{"tea leaves"} }), "no such kind"},
		{task(func(ft *forecastsTask) { ft.Input = "x" }), "not a CID"},
		{task(func(ft *forecastsTask) { ft.To = 2 }), "series 0 to 2 of 1"},
		{task(func(ft *forecastsTask) { ft.From = 1 }), "series 1 to 1 of 1"},
	} {
		if _, err := (Forecasts{}).Execute(ctx, store, c.payload); err == nil || !strings.Contains(err.Error(), c.why) {
			t.Errorf("error %v, want one saying %q", err, c.why)
		}
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := (Forecasts{}).Execute(cancelled, store, task(func(*forecastsTask) {})); err == nil {
		t.Error("a cancelled task ran")
	}
	// A series no candidate can be fitted to is listed with why.
	flat, _ := store.Put(ctx, strings.NewReader("a,5\na,5\na,5\na,5\na,5\na,5\na,5\na,5\na,5\na,5\n"))
	out, err := Forecasts{}.Execute(ctx, store, mustJSON(forecastsTask{Input: flat.String(), To: 1, Horizon: 2, Kinds: []string{"ar"}}))
	var part forecastsPart
	if err != nil || json.Unmarshal(out, &part) != nil || !strings.HasPrefix(part.Series[0].Error, "no candidate model could be fitted") {
		t.Errorf("a series no model fits gave %+v (%v)", part, err)
	}
}

func TestForecastsAggregateChecksWhatTheTasksReturned(t *testing.T) {
	store := storage.NewMemory()
	point := ForecastPoint{Step: 1, Value: 2, Lo80: 1, Hi80: 3, Lo95: 0, Hi95: 4}
	good := SeriesForecast{ID: "a", Points: 9, Model: "naive", MAE: 1, RMSE: 1, Forecast: []ForecastPoint{point}}
	part := func(from, horizon int, series ...SeriesForecast) []byte {
		return mustJSON(forecastsPart{From: from, Horizon: horizon, Series: series})
	}
	with := func(change func(*SeriesForecast)) SeriesForecast {
		s := good
		s.Forecast = slices.Clone(good.Forecast)
		change(&s)
		return s
	}
	for _, c := range []struct {
		outputs [][]byte
		why     string
	}{
		{[][]byte{[]byte("{")}, "task 0 output"},
		{nil, "no task returned a series"},
		{[][]byte{part(1, 1, good)}, "where series 0 was next"},
		{[][]byte{part(0, 1)}, "0 series from 0"},
		{[][]byte{part(0, 1, good), part(1, 2, good)}, "task 1 output"},
		{[][]byte{part(0, 0, good)}, "0 steps ahead"},
		{[][]byte{part(0, 1, with(func(s *SeriesForecast) { s.ID = "" }))}, "no name"},
		{[][]byte{part(0, 1, with(func(s *SeriesForecast) { s.Error = "too short" }))}, "both forecast and not"},
		{[][]byte{part(0, 1, with(func(s *SeriesForecast) { s.Model = "" }))}, "no model"},
		{[][]byte{part(0, 1, with(func(s *SeriesForecast) { s.Forecast[0].Step = 2 }))}, "step 1 is not"},
		{[][]byte{part(0, 1, with(func(s *SeriesForecast) { s.Forecast[0].Lo95 = 1.5 }))}, "step 1 is not"},
		{[][]byte{part(0, 1, with(func(s *SeriesForecast) { s.Forecast[0].Value = 5 }))}, "within its ranges"},
	} {
		if _, err := (Forecasts{}).Aggregate(ctx, store, c.outputs); err == nil || !strings.Contains(err.Error(), c.why) {
			t.Errorf("error %v, want one saying %q", err, c.why)
		}
	}
	// A name with a comma in it is quoted in both tables.
	failed := SeriesForecast{ID: "b", Points: 2, Error: "too short, it said"}
	raw, err := Forecasts{}.Aggregate(ctx, store, [][]byte{part(0, 1, with(func(s *SeriesForecast) { s.ID = "north, east" }), failed)})
	var result ForecastsResult
	if err != nil || json.Unmarshal(raw, &result) != nil || result.Failed != 1 || !strings.Contains(readStored(t, store, result.ModelsTable), `b,2,,,,,"too short, it said"`) || !strings.Contains(readStored(t, store, result.ForecastTable), `"north, east",1,2,1,3,0,4`) {
		t.Errorf("result %+v (%v)", result, err)
	}
	for _, fail := range []int{1, 2} {
		puts := 0
		blobs := countingBlobs{Store: store, fail: fail, puts: &puts}
		if _, err := (Forecasts{}).Aggregate(ctx, blobs, [][]byte{part(0, 1, good)}); err == nil || !strings.Contains(err.Error(), "store the") {
			t.Errorf("with store %d failing: error %v", fail, err)
		}
	}
}

// The answer is known to the last bit, and CI runs this on every processor
// the daemon is built for: a task chooses its series' forecasts and their
// ranges itself, so two workers' bytes must agree on all of them.
func TestForecastsGiveTheKnownAnswer(t *testing.T) {
	store := storage.NewMemory()
	input, _ := store.Put(ctx, strings.NewReader(panelOf(map[string]string{"a": seasonalSeries(72), "b": seasonalSeries(50), "c": seasonalSeries(4)})))
	payloads, err := Forecasts{}.Split(ctx, store, mustJSON(ForecastsParams{Input: input.String(), Horizon: 6, Season: 12}), 2)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.New()
	for _, payload := range payloads {
		output, err := Forecasts{}.Execute(ctx, store, payload)
		if err != nil {
			t.Fatal(err)
		}
		digest.Write(output)
	}
	const want = "e43bbdce2adcc2cd4597575ecef28d56dc1a2101319ca1298b30b01f29d7463a"
	if got := hex.EncodeToString(digest.Sum(nil)); got != want {
		t.Errorf("the tasks' outputs hash to %s, want %s", got, want)
	}
}

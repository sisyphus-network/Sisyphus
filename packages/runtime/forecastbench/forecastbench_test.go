package forecastbench

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	goruntime "runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
	"text/tabwriter"
	"time"

	"github.com/ipfs/go-cid"
	"github.com/sisyphus-network/Sisyphus/packages/runtime"
	"github.com/sisyphus-network/Sisyphus/packages/storage"
)

// TestM4 scores the forecast workload on the M4 competition's 100,000
// series, or on every SISYPHUS_M4_EVERY'th of them, and prints how it did
// beside Naive2, frequency by frequency and overall. It runs only when
// SISYPHUS_M4 names a directory holding the competition's training and
// test files (Yearly-train.csv, Yearly-test.csv, and so on), which are
// published in the Dataset directory of
// https://github.com/Mcompetitions/M4-methods. See
// docs/forecast-benchmark.md.
func TestM4(t *testing.T) {
	dir := os.Getenv("SISYPHUS_M4")
	if dir == "" {
		t.Skip("SISYPHUS_M4 is not set to a directory of the M4 data")
	}
	every := 1
	if s := os.Getenv("SISYPHUS_M4_EVERY"); s != "" {
		var err error
		if every, err = strconv.Atoi(s); err != nil || every < 1 {
			t.Fatalf("SISYPHUS_M4_EVERY=%q is not a whole number above zero", s)
		}
	}
	ctx := context.Background()
	table := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', tabwriter.AlignRight)
	fmt.Fprintln(table, "frequency\tseries\tsMAPE\tMASE\tOWA\tNaive2 sMAPE\tNaive2 MASE\tin 80%\tin 95%\tMSIS\tfell back\ttime\t")
	row := func(name string, s Score, took time.Duration) {
		fmt.Fprintf(table, "%s\t%d\t%.3f\t%.3f\t%.3f\t%.3f\t%.3f\t%.3f\t%.3f\t%.3f\t%d\t%s\t\n", name, s.Series, s.SMAPE, s.MASE, s.OWA(), s.Naive2SMAPE, s.Naive2MASE, s.Coverage80, s.Coverage95, s.MSIS, s.Fallbacks, took.Round(time.Second))
	}
	var overall Score
	began := time.Now()
	for _, freq := range Frequencies {
		series := readFrequency(t, dir, freq.Name)
		var sample []Series
		for i := 0; i < len(series); i += every {
			sample = append(sample, series[i])
		}
		start := time.Now()
		score, err := Run(ctx, sample, freq, goruntime.NumCPU(), Workload)
		if err != nil {
			t.Fatal(err)
		}
		row(freq.Name, score, time.Since(start))
		if overall.Series == 0 {
			overall = score
		} else {
			overall = overall.Add(score)
		}
	}
	row("Overall", overall, time.Since(began))
	table.Flush()
}

func readFrequency(t *testing.T, dir, name string) []Series {
	t.Helper()
	train, err := os.Open(filepath.Join(dir, name+"-train.csv"))
	if err != nil {
		t.Fatal(err)
	}
	defer train.Close()
	test, err := os.Open(filepath.Join(dir, name+"-test.csv"))
	if err != nil {
		t.Fatal(err)
	}
	defer test.Close()
	series, err := ReadM4(train, test)
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return series
}

func near(a, b float64) bool { return math.Abs(a-b) <= 1e-9*math.Max(1, math.Abs(b)) }

func TestTheErrorsAreScoredAsM4ScoredThem(t *testing.T) {
	// |10-8|·200/18 and |20-25|·200/45, averaged.
	if got, want := SMAPE([]float64{10, 20}, []float64{8, 25}), (400.0/18+1000.0/45)/2; !near(got, want) {
		t.Errorf("sMAPE is %v, not %v", got, want)
	}
	// Forecasting each value as the one before it errs by 2, 4 and 6 in
	// the sample, 4 on average; the forecast errs by 1 and 3, 2 on
	// average, half as much.
	if got := MASE([]float64{1, 3, 7, 13}, []float64{20, 30}, []float64{21, 27}, 1); !near(got, 0.5) {
		t.Errorf("MASE is %v, not 0.5", got)
	}
	// Scaled by the change over a season of two: 5 and 7, 6 on average.
	if got := MASE([]float64{1, 2, 6, 9}, []float64{10}, []float64{13}, 2); !near(got, 0.5) {
		t.Errorf("seasonal MASE is %v, not 0.5", got)
	}
	// A range of width 2 that holds the first value, and one of width 4
	// that misses the second by 1: (2 + 4 + 40·1) / 2 steps, scaled by 4.
	forecast := []runtime.ForecastPoint{{Value: 21, Lo80: 20.5, Hi80: 21.5, Lo95: 20, Hi95: 22}, {Value: 27, Lo80: 26, Hi80: 28, Lo95: 25, Hi95: 29}}
	if got := MSIS([]float64{1, 3, 7, 13}, []float64{20, 30}, forecast, 1); !near(got, 46.0/2/4) {
		t.Errorf("MSIS is %v, not %v", got, 46.0/2/4)
	}
	if in80, in95 := coverage([]float64{20, 30}, forecast); in80 != 0 || in95 != 0.5 {
		t.Errorf("%v fell in the 80%% ranges and %v in the 95%%", in80, in95)
	}
	if in80, _ := coverage([]float64{21, 27}, forecast); in80 != 1 {
		t.Errorf("values on their forecasts are not in their ranges")
	}
	if got := MSIS([]float64{1, 3}, []float64{0}, forecast[1:], 1); !near(got, (4+40*25)/2.0) {
		t.Errorf("a value below its range scored %v", got)
	}
	s := Score{Series: 2, SMAPE: 10, MASE: 2, Naive2SMAPE: 20, Naive2MASE: 2, Fallbacks: 1}
	if got := s.OWA(); !near(got, 0.75) {
		t.Errorf("OWA is %v, not 0.75", got)
	}
	s.Coverage80, s.Coverage95, s.MSIS = 0.5, 1, 10
	pooled := s.Add(Score{Series: 6, SMAPE: 2, MASE: 1, Naive2SMAPE: 4, Naive2MASE: 1, Coverage80: 0.75, Coverage95: 0.9, MSIS: 2, Fallbacks: 2})
	if want := (Score{Series: 8, SMAPE: 4, MASE: 1.25, Naive2SMAPE: 8, Naive2MASE: 1.25, Coverage80: 0.6875, Coverage95: 0.925, MSIS: 4, Fallbacks: 3}); pooled != want {
		t.Errorf("pooled %+v and %+v into %+v, not %+v", s, Score{}, pooled, want)
	}
}

// cycle returns n values of a level that rises by rise each step, times
// a cycle of these factors.
func cycle(n int, level, rise float64, factors ...float64) []float64 {
	y := make([]float64, n)
	for t := range y {
		y[t] = (level + rise*float64(t)) * factors[t%len(factors)]
	}
	return y
}

func TestOnlyASeriesWithACycleIsTakenToHaveOne(t *testing.T) {
	seasonal := cycle(48, 100, 0, 0.8, 1.2, 1.1, 0.9)
	if !Seasonal(seasonal, 4) {
		t.Error("a series repeating every 4 steps is not taken to have a cycle of 4")
	}
	if Seasonal(seasonal, 1) || Seasonal(seasonal[:11], 4) {
		t.Error("a cycle of one step, or a series of under three cycles, is taken to have a cycle")
	}
	// Scattered values, like noise: no cycle of 3.
	noise := make([]float64, 60)
	for t := range noise {
		noise[t] = float64(t * 7919 % 101)
	}
	if Seasonal(noise, 3) {
		t.Error("a series with no cycle of 3 is taken to have one")
	}
	if Seasonal(cycle(12, 5, 0, 1), 3) {
		t.Error("a constant series is taken to have a cycle")
	}
}

func TestNaive2CarriesTheLastValueForwardInItsPlaceInTheCycle(t *testing.T) {
	if got := Naive2([]float64{4, 9, 2, 7}, 3, 1); !slices.Equal(got, []float64{7, 7, 7}) {
		t.Errorf("without a cycle, Naive2 forecasts %v", got)
	}
	// A level times a cycle averaging one: the cycle is found exactly,
	// and the forecast goes on with it from where the series ends. The
	// cycles are odd and even in length, which are averaged differently.
	for _, factors := range [][]float64{{0.5, 1.5, 1}, {0.8, 1.2, 1.1, 0.9}} {
		m := len(factors)
		y := cycle(5*m+1, 40, 0, factors...)
		got := Naive2(y, 2*m, m)
		want := cycle(7*m+1, 40, 0, factors...)[5*m+1:]
		for k := range want {
			if !near(got[k], want[k]) {
				t.Fatalf("with a cycle of %d, Naive2 forecasts %v, not %v", m, got, want)
			}
		}
	}
	// A series too short to test for its cycle is carried forward as it is.
	if got := Naive2([]float64{1, 2, 3}, 2, 4); !slices.Equal(got, []float64{3, 3}) {
		t.Errorf("Naive2 forecasts %v from a series too short for its cycle", got)
	}
}

const m4Train = `"V1","V2","V3","V4","V5","V6"
"Y1","1","2","3","4","5"
"Y2","10","20","30","",""
`

const m4Test = `"V1","V2","V3"
"Y1","6","7"
"Y2","40","50"
`

func TestM4FilesAreReadAsTheCompetitionPublishedThem(t *testing.T) {
	series, err := ReadM4(strings.NewReader(m4Train), strings.NewReader(m4Test))
	if err != nil {
		t.Fatal(err)
	}
	want := []Series{{"Y1", []float64{1, 2, 3, 4, 5}, []float64{6, 7}}, {"Y2", []float64{10, 20, 30}, []float64{40, 50}}}
	if fmt.Sprint(series) != fmt.Sprint(want) {
		t.Errorf("read %v, not %v", series, want)
	}
	for _, c := range []struct{ train, test, why string }{
		{"", m4Test, "training file: header"},
		{m4Train, "", "test file: header"},
		{m4Train, "\"V1\"\n\"Y1\",\"6\",\"7\"\n", "has 2 series and the test file 1"},
		{m4Train, strings.Replace(m4Test, "Y2", "Y3", 1), "series 2 is Y2 in the training file and Y3"},
		{m4Train + "\"Y3\",\"x\"\n", m4Test, `series Y3: "x" is not a number`},
		{m4Train + "\"Y3\",\"NaN\",\"1\"\n", m4Test, `series Y3: "NaN" is not a number`},
		{m4Train + "\"Y3\",\"1\"\n", m4Test, "series Y3 has 1 values"},
		{m4Train + "\"Y3,\"1\"\n", m4Test, "training file: parse error"},
	} {
		if _, err := ReadM4(strings.NewReader(c.train), strings.NewReader(c.test)); err == nil || !strings.Contains(err.Error(), c.why) {
			t.Errorf("reading %q and %q gave %v, not an error saying %q", c.train, c.test, err, c.why)
		}
	}
}

func TestTheWorkloadForecastsAsTheForecastJobDoes(t *testing.T) {
	ctx := context.Background()
	y := cycle(40, 100, 1, 0.9, 1.1, 1.2, 0.8)
	got, err := Workload(ctx, y, 4, 4)
	if err != nil {
		t.Fatal(err)
	}
	// The same series as a job, with the backtest the workload chooses
	// for a job that does not say: 8 windows, the most it takes.
	blobs := storage.NewMemory()
	var data strings.Builder
	for _, v := range y {
		data.WriteString(strconv.FormatFloat(v, 'g', -1, 64) + "\n")
	}
	input, _ := blobs.Put(ctx, strings.NewReader(data.String()))
	w := runtime.Forecast{}
	payloads, err := w.Split(ctx, blobs, []byte(`{"input":"`+input.String()+`","horizon":4,"season":4,"folds":8}`), 3)
	if err != nil {
		t.Fatal(err)
	}
	var outputs [][]byte
	for _, p := range payloads {
		out, err := w.Execute(ctx, blobs, p)
		if err != nil {
			t.Fatal(err)
		}
		outputs = append(outputs, out)
	}
	raw, err := w.Aggregate(ctx, blobs, outputs)
	if err != nil {
		t.Fatal(err)
	}
	var result runtime.ForecastResult
	if err := json.Unmarshal(raw, &result); err != nil {
		t.Fatal(err)
	}
	if want := result.Forecast; !slices.Equal(got, want) {
		t.Errorf("Workload forecast %v, the job %v", got, want)
	}

	// A series no longer than the horizon leaves nothing to fit to.
	if _, err := Workload(ctx, []float64{1, 2, 3}, 3, 1); err == nil {
		t.Error("a series no longer than the horizon was forecast")
	}
}

// failing is a store whose puts and opens fail from a given one on.
type failing struct {
	*storage.Store
	puts, opens int
}

var errFailed = errors.New("the store failed")

func (f *failing) Put(ctx context.Context, r io.Reader) (cid.Cid, error) {
	if f.puts--; f.puts < 0 {
		return cid.Undef, errFailed
	}
	return f.Store.Put(ctx, r)
}

func (f *failing) Open(ctx context.Context, c cid.Cid) (storage.Blob, error) {
	if f.opens--; f.opens < 0 {
		return nil, errFailed
	}
	return f.Store.Open(ctx, c)
}

func TestAStoreThatFailsFailsTheForecast(t *testing.T) {
	y := cycle(30, 10, 1, 1)
	// The series is put, opened to split and to backtest, and the two
	// tables are put; each failing in turn fails the forecast.
	for _, c := range []struct{ puts, opens int }{{0, 9}, {9, 0}, {9, 1}, {1, 9}} {
		if _, err := forecastIn(context.Background(), &failing{storage.NewMemory(), c.puts, c.opens}, y, 3, 1); !errors.Is(err, errFailed) {
			t.Errorf("with %d puts and %d opens before failing, the forecast gave %v", c.puts, c.opens, err)
		}
	}
}

func TestRunScoresEachSeriesAndFallsBackToNaive2(t *testing.T) {
	ctx := context.Background()
	freq := Frequency{"Quarterly", 4, 4}
	var series []Series
	for i := range 9 {
		y := cycle(48, 50+float64(i), 0.5, 0.8, 1.2, 1.1, 0.9)
		series = append(series, Series{ID: fmt.Sprint("Q", i), Train: y[:44], Test: y[44:]})
	}
	// Naive2 forecasting for itself scores exactly as Naive2.
	naive2 := func(_ context.Context, y []float64, h, m int) ([]runtime.ForecastPoint, error) {
		return Points(Naive2(y, h, m)), nil
	}
	s, err := Run(ctx, series, freq, 3, naive2)
	if err != nil {
		t.Fatal(err)
	}
	if s.Series != 9 || s.Fallbacks != 0 || s.SMAPE != s.Naive2SMAPE || s.MASE != s.Naive2MASE || !near(s.OWA(), 1) {
		t.Errorf("Naive2 scored %+v against itself", s)
	}
	// The workload is scored the same however many work at it.
	one, err := Run(ctx, series, freq, 1, Workload)
	if err != nil {
		t.Fatal(err)
	}
	many, err := Run(ctx, series, freq, 4, Workload)
	if err != nil || one != many || one.Fallbacks != 0 {
		t.Errorf("one worker scored %+v and four %+v (%v)", one, many, err)
	}
	// A forecaster that fails, or forecasts the wrong number of steps,
	// is scored as Naive2.
	calls := 0
	odd := func(_ context.Context, y []float64, h, m int) ([]runtime.ForecastPoint, error) {
		if calls++; calls%2 == 0 {
			return nil, errFailed
		}
		return make([]runtime.ForecastPoint, h-1), nil
	}
	s, err = Run(ctx, series, freq, 1, odd)
	if err != nil || s.Fallbacks != 9 || s.SMAPE != s.Naive2SMAPE {
		t.Errorf("a forecaster that fails was scored %+v (%v)", s, err)
	}

	if _, err := Run(ctx, nil, freq, 2, naive2); err == nil {
		t.Error("no series were scored")
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := Run(cancelled, series, freq, 0, naive2); !errors.Is(err, context.Canceled) {
		t.Errorf("a cancelled run gave %v", err)
	}
}

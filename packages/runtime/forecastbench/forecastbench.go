// Package forecastbench measures the forecast workload on the M4
// competition's series, the way the competition scored its entries: the
// symmetric mean absolute percentage error (sMAPE), the mean absolute
// scaled error (MASE), and their overall weighted average (OWA) relative
// to the competition's Naive2 benchmark, so that 1 is Naive2 and less is
// better. The data is not part of the repository; see TestM4.
package forecastbench

import (
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"strconv"
	"sync"

	"github.com/ipfs/go-cid"
	"github.com/sisyphus-network/Sisyphus/packages/runtime"
	"github.com/sisyphus-network/Sisyphus/packages/storage"
)

// Frequency is one of M4's six sets of series: how far ahead each is
// forecast, and the length of its seasonal cycle, 1 for none, which
// Naive2 and MASE use.
type Frequency struct {
	Name    string
	Horizon int
	Season  int
}

// Frequencies are M4's, with the horizons and cycles the competition set.
var Frequencies = []Frequency{
	{"Yearly", 6, 1},
	{"Quarterly", 8, 4},
	{"Monthly", 18, 12},
	{"Weekly", 13, 1},
	{"Daily", 14, 1},
	{"Hourly", 48, 24},
}

// Series is one M4 series: the past a forecast is made from and the
// values that followed, which it is scored against.
type Series struct {
	ID    string
	Train []float64
	Test  []float64
}

// ReadM4 reads a frequency's training and test files, as the competition
// published them: a header line, then a line for each series, its ID and
// its values, quoted, with empty fields after a series shorter than the
// longest. The two files must list the same series in the same order.
func ReadM4(train, test io.Reader) ([]Series, error) {
	pasts, err := readRows(train)
	if err != nil {
		return nil, fmt.Errorf("training file: %w", err)
	}
	futures, err := readRows(test)
	if err != nil {
		return nil, fmt.Errorf("test file: %w", err)
	}
	if len(pasts) != len(futures) {
		return nil, fmt.Errorf("the training file has %d series and the test file %d", len(pasts), len(futures))
	}
	series := make([]Series, len(pasts))
	for i, past := range pasts {
		if past.id != futures[i].id {
			return nil, fmt.Errorf("series %d is %s in the training file and %s in the test file", i+1, past.id, futures[i].id)
		}
		series[i] = Series{ID: past.id, Train: past.values, Test: futures[i].values}
	}
	return series, nil
}

type row struct {
	id     string
	values []float64
}

func readRows(r io.Reader) ([]row, error) {
	lines := csv.NewReader(r)
	lines.FieldsPerRecord = -1
	if _, err := lines.Read(); err != nil {
		return nil, fmt.Errorf("header: %w", err)
	}
	var rows []row
	for {
		fields, err := lines.Read()
		if err == io.EOF {
			return rows, nil
		}
		if err != nil {
			return nil, err
		}
		got := row{id: fields[0]}
		for _, field := range fields[1:] {
			if field == "" {
				break
			}
			v, err := strconv.ParseFloat(field, 64)
			if err != nil || math.IsNaN(v) || math.IsInf(v, 0) {
				return nil, fmt.Errorf("series %s: %q is not a number", got.id, field)
			}
			got.values = append(got.values, v)
		}
		if len(got.values) < 2 {
			return nil, fmt.Errorf("series %s has %d values, too few to forecast or score", got.id, len(got.values))
		}
		rows = append(rows, got)
	}
}

// SMAPE is the symmetric mean absolute percentage error of a forecast, in
// percent, as M4 scored it.
func SMAPE(actual, forecast []float64) float64 {
	var sum float64
	for i, a := range actual {
		sum += 200 * math.Abs(a-forecast[i]) / (math.Abs(a) + math.Abs(forecast[i]))
	}
	return sum / float64(len(actual))
}

// MASE is the mean absolute error of a forecast, scaled by the in-sample
// mean absolute error of forecasting each value as the one a season
// before it, as M4 scored it.
func MASE(insample, actual, forecast []float64, season int) float64 {
	var scale float64
	for t := season; t < len(insample); t++ {
		scale += math.Abs(insample[t] - insample[t-season])
	}
	scale /= float64(len(insample) - season)
	var sum float64
	for i, a := range actual {
		sum += math.Abs(a - forecast[i])
	}
	return sum / float64(len(actual)) / scale
}

// Seasonal is M4's test of whether a series has a cycle of the given
// length: whether its autocorrelation at that lag is beyond what 90% of
// series without one would show. A series shorter than three cycles is
// taken to have none.
func Seasonal(y []float64, season int) bool {
	n := len(y)
	if season < 2 || n < 3*season {
		return false
	}
	var mean float64
	for _, v := range y {
		mean += v
	}
	mean /= float64(n)
	var c0 float64
	for _, v := range y {
		c0 += (v - mean) * (v - mean)
	}
	acf := func(k int) float64 {
		var s float64
		for t := k; t < n; t++ {
			s += (y[t] - mean) * (y[t-k] - mean)
		}
		return s / c0
	}
	sum := 1.0
	for k := 1; k < season; k++ {
		sum += 2 * acf(k) * acf(k)
	}
	return math.Abs(acf(season)) > 1.645/math.Sqrt(float64(n))*math.Sqrt(sum)
}

// indices are a series' multiplicative seasonal indices, one for each
// place in its cycle, averaging one: a classical decomposition, the ratio
// of each value to the centred moving average around it, averaged over
// each place in the cycle.
func indices(y []float64, season int) []float64 {
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

// Naive2 is M4's benchmark: the last value carried forward, after taking
// out the series' cycle if it has one, which is put back in the forecast.
func Naive2(insample []float64, horizon, season int) []float64 {
	n := len(insample)
	last, index := insample[n-1], []float64{1}
	if Seasonal(insample, season) {
		index = indices(insample, season)
		last /= index[(n-1)%season]
	}
	f := make([]float64, horizon)
	for k := range f {
		f[k] = last * index[(n+k)%len(index)]
	}
	return f
}

// Forecaster forecasts horizon steps of a series whose cycle is season
// steps long, 1 for none, with the ranges it expects each value to fall in
// 80 and 95 times in a hundred.
type Forecaster func(ctx context.Context, y []float64, horizon, season int) ([]runtime.ForecastPoint, error)

// Points makes a forecast with no ranges, each value its own range.
func Points(values []float64) []runtime.ForecastPoint {
	points := make([]runtime.ForecastPoint, len(values))
	for i, v := range values {
		points[i] = runtime.ForecastPoint{Step: i + 1, Value: v, Lo80: v, Hi80: v, Lo95: v, Hi95: v}
	}
	return points
}

// Workload forecasts with the forecast workload, run in this process as
// one task: what a pool would forecast, since a pool's answer does not
// depend on how the job is split. It leaves the number of backtest windows
// to the workload, as a job that does not say does.
func Workload(ctx context.Context, y []float64, horizon, season int) ([]runtime.ForecastPoint, error) {
	return forecastIn(ctx, storage.NewMemory(), y, horizon, season)
}

// forecastIn is Workload with the store the series and tables are kept in.
func forecastIn(ctx context.Context, blobs runtime.Blobs, y []float64, horizon, season int) ([]runtime.ForecastPoint, error) {
	var data bytes.Buffer
	for _, v := range y {
		data.WriteString(strconv.FormatFloat(v, 'g', -1, 64) + "\n")
	}
	input, err := blobs.Put(ctx, &data)
	if err != nil {
		return nil, err
	}
	params := runtime.ForecastParams{Input: input.String(), Horizon: horizon}
	if season > 1 {
		params.Season = season
	}
	encoded, _ := json.Marshal(params)
	w := runtime.Forecast{}
	payloads, err := w.Split(ctx, blobs, encoded, 1)
	if err != nil {
		return nil, err
	}
	output, err := w.Execute(ctx, blobs, payloads[0])
	if err != nil {
		return nil, err
	}
	raw, err := w.Aggregate(ctx, blobs, [][]byte{output})
	if err != nil {
		return nil, err
	}
	// The result is the workload's own JSON, so it decodes.
	var result runtime.ForecastResult
	_ = json.Unmarshal(raw, &result)
	return result.Forecast, nil
}

// Batch forecasts the series all at once with the forecasts workload, run
// in this process as tasks tasks, workers at a time: what a pool would
// forecast of them as one job. Models is the job's models, "" for the
// workload's default; with w.Containers it may name the Chronos-Bolt
// kinds, which run in their image. It returns each series' forecast, in
// the series' order, or none for a series the workload could not forecast.
func Batch(ctx context.Context, w runtime.Forecasts, series []Series, freq Frequency, models string, tasks, workers int) ([][]runtime.ForecastPoint, error) {
	return batchIn(ctx, storage.NewMemory(), w, series, freq, models, tasks, workers)
}

// batchIn is Batch with the store the series and tables are kept in.
func batchIn(ctx context.Context, blobs runtime.Blobs, w runtime.Forecasts, series []Series, freq Frequency, models string, tasks, workers int) ([][]runtime.ForecastPoint, error) {
	// The job knows a series by its ID, so each must be another's.
	index := make(map[string]int, len(series))
	for i, s := range series {
		if _, seen := index[s.ID]; seen {
			return nil, fmt.Errorf("series %q is given twice", s.ID)
		}
		index[s.ID] = i
	}
	var data bytes.Buffer
	lines := csv.NewWriter(&data)
	_ = lines.Write([]string{"id", "value"}) // to memory, which does not fail
	for _, s := range series {
		for _, v := range s.Train {
			_ = lines.Write([]string{s.ID, strconv.FormatFloat(v, 'g', -1, 64)})
		}
	}
	lines.Flush()
	input, err := blobs.Put(ctx, &data)
	if err != nil {
		return nil, err
	}
	params := runtime.ForecastsParams{Input: input.String(), Horizon: freq.Horizon, Models: models}
	if freq.Season > 1 {
		params.Season = freq.Season
	}
	encoded, _ := json.Marshal(params)
	payloads, err := w.Split(ctx, blobs, encoded, tasks)
	if err != nil {
		return nil, err
	}
	outputs := make([][]byte, len(payloads))
	failures := make([]error, len(payloads))
	next := make(chan int)
	var wg sync.WaitGroup
	for range max(workers, 1) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range next {
				outputs[i], failures[i] = w.Execute(ctx, blobs, payloads[i])
			}
		}()
	}
	for i := range payloads {
		next <- i
	}
	close(next)
	wg.Wait()
	for i, err := range failures {
		if err != nil {
			return nil, fmt.Errorf("task %d of %d: %w", i+1, len(payloads), err)
		}
	}
	raw, err := w.Aggregate(ctx, blobs, outputs)
	if err != nil {
		return nil, err
	}
	// The result is the workload's own JSON, naming a table it stored.
	var result runtime.ForecastsResult
	_ = json.Unmarshal(raw, &result)
	table, _ := cid.Decode(result.ForecastTable)
	stored, err := blobs.Open(ctx, table)
	if err != nil {
		return nil, err
	}
	defer stored.Close()
	return forecastsByID(stored, index)
}

// forecastsByID reads a forecasts job's table, a line for each step of
// each series' forecast (id,step,value,lo80,hi80,lo95,hi95), into the
// forecasts of the series, in the order index gives their IDs.
func forecastsByID(r io.Reader, index map[string]int) ([][]runtime.ForecastPoint, error) {
	rows, err := csv.NewReader(r).ReadAll()
	if err != nil {
		return nil, fmt.Errorf("forecast table: %w", err)
	}
	forecasts := make([][]runtime.ForecastPoint, len(index))
	for n, row := range rows[min(1, len(rows)):] {
		if len(row) != 7 {
			return nil, fmt.Errorf("forecast table line %d has %d fields, not 7", n+2, len(row))
		}
		i, ok := index[row[0]]
		if !ok {
			return nil, fmt.Errorf("forecast table line %d forecasts %q, which is none of the series", n+2, row[0])
		}
		var numbers [6]float64
		for j, field := range row[1:] {
			if numbers[j], err = strconv.ParseFloat(field, 64); err != nil {
				return nil, fmt.Errorf("forecast table line %d: %w", n+2, err)
			}
		}
		forecasts[i] = append(forecasts[i], runtime.ForecastPoint{Step: int(numbers[0]), Value: numbers[1], Lo80: numbers[2], Hi80: numbers[3], Lo95: numbers[4], Hi95: numbers[5]})
	}
	return forecasts, nil
}

// MSIS is M4's mean scaled interval score of a 95% range: its width, plus
// forty times how far each value fell outside it, averaged over the steps
// and scaled as MASE is. A range is better the narrower it is and the
// fewer values it misses.
func MSIS(insample, actual []float64, forecast []runtime.ForecastPoint, season int) float64 {
	const a = 0.05
	var scale float64
	for t := season; t < len(insample); t++ {
		scale += math.Abs(insample[t] - insample[t-season])
	}
	scale /= float64(len(insample) - season)
	var sum float64
	for i, y := range actual {
		lo, hi := forecast[i].Lo95, forecast[i].Hi95
		sum += hi - lo
		if y < lo {
			sum += 2 / a * (lo - y)
		}
		if y > hi {
			sum += 2 / a * (y - hi)
		}
	}
	return sum / float64(len(actual)) / scale
}

// coverage is the share of the values that fell in their 80% and in their
// 95% ranges.
func coverage(actual []float64, forecast []runtime.ForecastPoint) (in80, in95 float64) {
	for i, y := range actual {
		p := forecast[i]
		if p.Lo80 <= y && y <= p.Hi80 {
			in80++
		}
		if p.Lo95 <= y && y <= p.Hi95 {
			in95++
		}
	}
	n := float64(len(actual))
	return in80 / n, in95 / n
}

// Score is how a forecaster did on a set of series, beside Naive2.
type Score struct {
	Series      int
	SMAPE       float64
	MASE        float64
	Naive2SMAPE float64
	Naive2MASE  float64
	// Coverage80 and Coverage95 are the shares of the values that fell in
	// the forecaster's 80% and 95% ranges, which should be near 0.8 and
	// 0.95, averaged over the series; MSIS is the mean scaled interval
	// score of its 95% ranges.
	Coverage80 float64
	Coverage95 float64
	MSIS       float64
	// Fallbacks is how many series the forecaster could not forecast,
	// for which Naive2's forecast was scored in its place.
	Fallbacks int
}

// OWA is M4's overall weighted average: the forecaster's sMAPE and MASE
// each as a share of Naive2's, averaged. Below 1 is better than Naive2.
func (s Score) OWA() float64 {
	return (s.SMAPE/s.Naive2SMAPE + s.MASE/s.Naive2MASE) / 2
}

// Add pools two scores, each weighted by its number of series, as M4
// pooled its frequencies into an overall score.
func (s Score) Add(o Score) Score {
	n := float64(s.Series + o.Series)
	mean := func(a, b float64) float64 { return (a*float64(s.Series) + b*float64(o.Series)) / n }
	return Score{
		Series:      s.Series + o.Series,
		SMAPE:       mean(s.SMAPE, o.SMAPE),
		MASE:        mean(s.MASE, o.MASE),
		Naive2SMAPE: mean(s.Naive2SMAPE, o.Naive2SMAPE),
		Naive2MASE:  mean(s.Naive2MASE, o.Naive2MASE),
		Coverage80:  mean(s.Coverage80, o.Coverage80),
		Coverage95:  mean(s.Coverage95, o.Coverage95),
		MSIS:        mean(s.MSIS, o.MSIS),
		Fallbacks:   s.Fallbacks + o.Fallbacks,
	}
}

// Run forecasts each series with the forecaster, workers at a time, and
// scores the forecasts and Naive2's against what followed. The score does
// not depend on how many workers there are: each series is scored on its
// own and the scores are summed in order.
func Run(ctx context.Context, series []Series, freq Frequency, workers int, forecast Forecaster) (Score, error) {
	if len(series) == 0 {
		return Score{}, fmt.Errorf("no %s series to score", freq.Name)
	}
	forecasts := make([][]runtime.ForecastPoint, len(series))
	next := make(chan int)
	var wg sync.WaitGroup
	for range max(workers, 1) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range next {
				// A forecast that fails is scored as none, and Naive2's in its place.
				forecasts[i], _ = forecast(ctx, series[i].Train, freq.Horizon, freq.Season)
			}
		}()
	}
	for i := range series {
		if ctx.Err() != nil {
			break
		}
		next <- i
	}
	close(next)
	wg.Wait()
	if err := ctx.Err(); err != nil {
		return Score{}, err
	}
	return Scored(series, freq, forecasts)
}

// Scored scores the forecasts made of the series, the forecasts in the
// series' order, and Naive2's, against what followed. A series with no
// forecast, or one of the wrong length, is scored by Naive2's forecast in
// its place and counted as a fallback.
func Scored(series []Series, freq Frequency, forecasts [][]runtime.ForecastPoint) (Score, error) {
	if len(series) == 0 {
		return Score{}, fmt.Errorf("no %s series to score", freq.Name)
	}
	if len(forecasts) != len(series) {
		return Score{}, fmt.Errorf("%d forecasts of %d %s series", len(forecasts), len(series), freq.Name)
	}
	total := Score{Series: len(series)}
	for i, s := range series {
		naive2 := Naive2(s.Train, freq.Horizon, freq.Season)
		f := forecasts[i]
		if len(f) != freq.Horizon {
			f = Points(naive2)
			total.Fallbacks++
		}
		values := make([]float64, len(f))
		for j, p := range f {
			values[j] = p.Value
		}
		in80, in95 := coverage(s.Test, f)
		total.SMAPE += SMAPE(s.Test, values)
		total.MASE += MASE(s.Train, s.Test, values, freq.Season)
		total.Naive2SMAPE += SMAPE(s.Test, naive2)
		total.Naive2MASE += MASE(s.Train, s.Test, naive2, freq.Season)
		total.Coverage80 += in80
		total.Coverage95 += in95
		total.MSIS += MSIS(s.Train, s.Test, f, freq.Season)
	}
	n := float64(len(series))
	total.SMAPE /= n
	total.MASE /= n
	total.Naive2SMAPE /= n
	total.Naive2MASE /= n
	total.Coverage80 /= n
	total.Coverage95 /= n
	total.MSIS /= n
	return total, nil
}

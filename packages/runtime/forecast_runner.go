package runtime

import (
	"context"
	"fmt"
	"math"
)

// A candidate's backtest needs forecasts from several points in its
// series: from the whole series, and from the past of each backtest window.
// A runner makes them. The models written in Go are fitted here, in the
// daemon; a runner can as well hand its fits to something else, such as a
// pinned container, which is why it is given every fit a task needs at
// once. Choosing the windows, scoring the forecasts and deciding between
// candidates stay here whatever made the forecasts.

// fitRequest asks for a candidate's forecasts, Horizon steps ahead, from
// each of several prefixes of a series: the whole series first, then the
// past of each backtest window, oldest window first.
type fitRequest struct {
	Model    forecastModel
	Prefixes [][]float64
	Horizon  int
}

// fitAnswer is a fitRequest's forecasts, one for each prefix in order, or
// why the candidate could not be fitted.
type fitAnswer struct {
	Forecasts [][]float64
	Err       string
}

// forecastRunner fits candidates. It answers every request, in order, and
// calls done as requests are answered with how many were. An error means
// none could be, as when the task is cancelled. Its name tells runners
// apart, so that each is asked once for all of its fits.
type forecastRunner interface {
	name() string
	fit(ctx context.Context, requests []fitRequest, done func(n int)) ([]fitAnswer, error)
}

// goRunner fits the models written in Go, in process.
type goRunner struct{}

func (goRunner) name() string { return "go" }

func (goRunner) fit(ctx context.Context, requests []fitRequest, done func(int)) ([]fitAnswer, error) {
	answers := make([]fitAnswer, len(requests))
	for i, r := range requests {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		for _, y := range r.Prefixes {
			f, err := fitForecast(r.Model, y, r.Horizon)
			if err != nil {
				answers[i] = fitAnswer{Err: err.Error()}
				break
			}
			answers[i].Forecasts = append(answers[i].Forecasts, f)
		}
		done(1)
	}
	return answers, nil
}

// runnerFor says which runner fits a model. A workload passes its own to
// backtestAll; one whose every kind is written in Go passes inProcess.
type runnerFor func(forecastModel) forecastRunner

// inProcess fits every model with goRunner.
func inProcess(forecastModel) forecastRunner { return goRunner{} }

// backtestAll backtests models[i] on series[i] with folds[i] windows of
// horizon steps, asking each of runners' runners for all of its fits at once, and
// reports the share of fits made to progress, which may be nil. A
// candidate that cannot be fitted has an outcome saying why; an error
// means the backtests could not be finished at all.
func backtestAll(ctx context.Context, runners runnerFor, series [][]float64, models [][]forecastModel, horizon int, folds []int, progress func(float64)) ([][]forecastOutcome, error) {
	type slot struct{ series, candidate int }
	outcomes := make([][]forecastOutcome, len(series))
	slots := map[string][]slot{}
	requests := map[string][]fitRequest{}
	var asked []forecastRunner
	total := 0
	for s, values := range series {
		if len(models[s]) > 0 {
			outcomes[s] = make([]forecastOutcome, len(models[s]))
		}
		for c, m := range models[s] {
			r, why := backtestRequest(m, values, horizon, folds[s])
			if why != "" {
				outcomes[s][c] = forecastOutcome{Model: m, Error: why}
				continue
			}
			run := runners(m)
			key := run.name()
			if _, ok := slots[key]; !ok {
				asked = append(asked, run)
			}
			slots[key] = append(slots[key], slot{s, c})
			requests[key] = append(requests[key], r)
			total++
		}
	}
	made := 0
	done := func(n int) {
		made += n
		if progress != nil {
			progress(float64(made) / float64(total))
		}
	}
	for _, run := range asked {
		key := run.name()
		answers, err := run.fit(ctx, requests[key], done)
		if err != nil {
			return nil, err
		}
		if len(answers) != len(requests[key]) {
			return nil, fmt.Errorf("forecast: the %s runner answered %d of %d fits", key, len(answers), len(requests[key]))
		}
		for i, at := range slots[key] {
			outcomes[at.series][at.candidate] = scoreBacktest(models[at.series][at.candidate], series[at.series], horizon, folds[at.series], answers[i])
		}
	}
	return outcomes, nil
}

// backtestRequest is the fits a candidate's backtest needs, or why it
// cannot have one.
func backtestRequest(m forecastModel, values []float64, horizon, folds int) (fitRequest, string) {
	if why := m.invalid(); why != "" {
		return fitRequest{}, why
	}
	if need := m.fewest() + folds*horizon; len(values) < need {
		return fitRequest{}, fmt.Sprintf("needs at least %d points, %d to fit and %d to test", need, m.fewest(), folds*horizon)
	}
	r := fitRequest{Model: m, Horizon: horizon, Prefixes: [][]float64{values}}
	for k := range folds {
		r.Prefixes = append(r.Prefixes, values[:len(values)-(folds-k)*horizon])
	}
	return r, ""
}

// scoreBacktest scores a candidate's forecast of each window against what
// the series did, and keeps its forecast from the whole series. A runner's
// answer of the wrong shape is the candidate's error, not a score.
func scoreBacktest(m forecastModel, values []float64, horizon, folds int, a fitAnswer) forecastOutcome {
	out := forecastOutcome{Model: m}
	if a.Err != "" {
		out.Error = a.Err
		return out
	}
	if len(a.Forecasts) != folds+1 {
		out.Error = fmt.Sprintf("its runner returned %d forecasts where %d were asked for", len(a.Forecasts), folds+1)
		return out
	}
	for _, f := range a.Forecasts {
		if len(f) != horizon {
			out.Error = fmt.Sprintf("its runner returned a forecast of %d steps where %d were asked for", len(f), horizon)
			return out
		}
	}
	var absSum, sqSum float64
	stepSq := make([]float64, horizon)
	keep := folds*horizon <= maxForecastResiduals
	for k := range folds {
		end := len(values) - (folds-k)*horizon
		past := a.Forecasts[k+1]
		if keep {
			out.Residuals = append(out.Residuals, make([]float64, horizon))
		}
		for j, v := range values[end : end+horizon] {
			e := v - past[j]
			if keep {
				out.Residuals[k][j] = e
			}
			sq := float64(e * e)
			absSum += math.Abs(e)
			sqSum += sq
			stepSq[j] += sq
		}
	}
	count := float64(folds * horizon)
	out.MAE = absSum / count
	out.RMSE = math.Sqrt(sqSum / count)
	out.StepRMSE = make([]float64, horizon)
	for j, sq := range stepSq {
		out.StepRMSE[j] = math.Sqrt(sq / float64(folds))
	}
	out.Forecast = a.Forecasts[0]
	return out
}

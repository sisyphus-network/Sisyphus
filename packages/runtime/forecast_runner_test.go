package runtime

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
)

// recordingRunner answers every fit with a flat forecast of the prefix's
// length, unless told to answer otherwise, and keeps what it was asked.
type recordingRunner struct {
	asked  *[]fitRequest
	answer func(fitRequest) fitAnswer
	err    error
}

func (recordingRunner) name() string { return "recording" }

func (r recordingRunner) fit(ctx context.Context, requests []fitRequest, done func(int)) ([]fitAnswer, error) {
	if r.err != nil {
		return nil, r.err
	}
	*r.asked = append(*r.asked, requests...)
	var answers []fitAnswer
	for _, q := range requests {
		if r.answer != nil {
			answers = append(answers, r.answer(q))
		} else {
			var a fitAnswer
			for _, y := range q.Prefixes {
				a.Forecasts = append(a.Forecasts, slices.Repeat([]float64{float64(len(y))}, q.Horizon))
			}
			answers = append(answers, a)
		}
		done(1)
	}
	return answers, nil
}

// every fits every model with run.
func every(run forecastRunner) runnerFor {
	return func(forecastModel) forecastRunner { return run }
}

func TestABacktestAsksItsRunnerForEveryFitAtOnce(t *testing.T) {
	var asked []fitRequest
	run := recordingRunner{asked: &asked}
	series := [][]float64{
		{1, 2, 3, 4, 5, 6, 7, 8, 9, 10},
		{5, 5, 5, 5, 5, 5, 5},
	}
	models := [][]forecastModel{
		{{Kind: "naive"}, {Kind: "mean"}},
		{{Kind: "naive"}, {Kind: "crystal ball"}},
	}
	var progress []float64
	outcomes, err := backtestAll(ctx, every(run), series, models, 2, []int{3, 2}, func(f float64) { progress = append(progress, f) })
	if err != nil {
		t.Fatal(err)
	}
	// Three valid candidates, asked for together: each with the whole
	// series first, then each window's past, oldest first.
	if len(asked) != 3 {
		t.Fatalf("asked for %d fits, want 3: %+v", len(asked), asked)
	}
	for i, want := range [][]int{{10, 4, 6, 8}, {10, 4, 6, 8}, {7, 3, 5}} {
		var got []int
		for _, p := range asked[i].Prefixes {
			got = append(got, len(p))
		}
		if !slices.Equal(got, want) || asked[i].Horizon != 2 {
			t.Errorf("fit %d asked for prefixes of %v, %d steps ahead; want %v, 2", i, got, asked[i].Horizon, want)
		}
	}
	if !slices.Equal(progress, []float64{1.0 / 3, 2.0 / 3, 1}) {
		t.Errorf("progress %v", progress)
	}
	// The invalid candidate was never asked for, and says why.
	if !strings.Contains(outcomes[1][1].Error, "no such kind") {
		t.Errorf("invalid candidate: %+v", outcomes[1][1])
	}
	// The runner's forecasts are scored against the series: the flat
	// forecast from series 0's first window's past is 4, and the window
	// held 5 and 6.
	got := outcomes[0][0]
	if got.Error != "" || !slices.Equal(got.Forecast, []float64{10, 10}) || !slices.Equal(got.Residuals[0], []float64{1, 2}) {
		t.Errorf("scored %+v", got)
	}
}

func TestARunnersAnswerOfTheWrongShapeIsTheCandidatesError(t *testing.T) {
	values := []float64{1, 2, 3, 4, 5, 6, 7, 8}
	for _, tt := range []struct {
		answer fitAnswer
		why    string
	}{
		{fitAnswer{Err: "out of memory"}, "out of memory"},
		{fitAnswer{Forecasts: [][]float64{{1, 1}}}, "1 forecasts where 3 were asked for"},
		{fitAnswer{Forecasts: [][]float64{{1, 1}, {1}, {1, 1}}}, "a forecast of 1 steps where 2 were asked for"},
	} {
		var asked []fitRequest
		run := recordingRunner{asked: &asked, answer: func(fitRequest) fitAnswer { return tt.answer }}
		outcomes, err := backtestAll(ctx, every(run), [][]float64{values}, [][]forecastModel{{{Kind: "naive"}}}, 2, []int{2}, nil)
		if err != nil {
			t.Fatal(err)
		}
		if o := outcomes[0][0]; !strings.Contains(o.Error, tt.why) || o.Forecast != nil || o.MAE != 0 {
			t.Errorf("answer %+v gave %+v, want an error saying %q", tt.answer, o, tt.why)
		}
	}
}

func TestABacktestFailsWhenItsRunnerCannotAnswer(t *testing.T) {
	run := recordingRunner{err: errors.New("the container would not start")}
	_, err := backtestAll(ctx, every(run), [][]float64{{1, 2, 3, 4}}, [][]forecastModel{{{Kind: "naive"}}}, 1, []int{1}, nil)
	if err == nil || !strings.Contains(err.Error(), "would not start") {
		t.Errorf("got %v", err)
	}
	// So does one that answers too few fits.
	_, err = backtestAll(ctx, every(shortRunner{}), [][]float64{{1, 2, 3, 4}}, [][]forecastModel{{{Kind: "naive"}, {Kind: "mean"}}}, 1, []int{1}, nil)
	if err == nil || !strings.Contains(err.Error(), "short runner answered 1 of 2 fits") {
		t.Errorf("got %v", err)
	}
}

// shortRunner answers only the first fit it is asked for.
type shortRunner struct{}

func (shortRunner) name() string { return "short" }

func (shortRunner) fit(ctx context.Context, requests []fitRequest, done func(int)) ([]fitAnswer, error) {
	return goRunner{}.fit(ctx, requests[:1], done)
}

func TestTheGoRunnerStopsWhenTheTaskIsCancelled(t *testing.T) {
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	_, err := goRunner{}.fit(cancelled, []fitRequest{{Model: forecastModel{Kind: "naive"}, Prefixes: [][]float64{{1}}, Horizon: 1}}, func(int) {})
	if !errors.Is(err, context.Canceled) {
		t.Errorf("got %v", err)
	}
}

package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"slices"
	"strings"
)

// A forecast family is a family of pretrained forecasting models that runs
// in an image of its own, as Chronos-Bolt does. Its models are candidates
// like the ones written in Go: backtested on the same windows, scored the
// same way, and joining the forecast only if they score among the best
// kinds. What the families have in common is here: the kinds a job names
// them by, which workers take them, which runner fits them, and how two
// workers' runs of them are compared. Each family's own file has the rest:
// its pins, how its image is made, what the image is given and leaves, and
// how what it leaves is checked.
type forecastFamily struct {
	// kind is the candidates' kind, as a task names them.
	kind string
	// name is what errors and the form call the family.
	name string
	// variants are its models, smallest first. A job asks for one as the
	// kind, a hyphen and the variant: "chronos-base".
	variants []string
	// fewest is how many points a model is given at least to forecast
	// from.
	fewest int
	// memoryMB is the most memory its image is given.
	memoryMB int
	// tolerance is how far apart, relative to the size of what is
	// forecast, two workers' numbers for one of its candidates may be and
	// still agree.
	tolerance float64
	// label marks a worker that runs the family's image. A job that asks
	// for the family needs it.
	label string
	// runner fits the family's candidates for a node.
	runner func(Forecast) forecastRunner
}

// forecastFamilies are the families a job may ask for, in the order the
// form offers them.
var forecastFamilies = []forecastFamily{chronosFamily, timesfmFamily}

// kinds are the kinds a job names the family's models by, smallest first.
// "all" does not include them: they are asked for.
func (fam forecastFamily) kinds() []string {
	var kinds []string
	for _, v := range fam.variants {
		kinds = append(kinds, fam.kind+"-"+v)
	}
	return kinds
}

// familyKinds are the kinds of every family.
func familyKinds() []string {
	var kinds []string
	for _, fam := range forecastFamilies {
		kinds = append(kinds, fam.kinds()...)
	}
	return kinds
}

// familyOf is the family whose candidates are of the kind, if any.
func familyOf(kind string) (forecastFamily, bool) {
	for _, fam := range forecastFamilies {
		if fam.kind == kind {
			return fam, true
		}
	}
	return forecastFamily{}, false
}

// wantedFamilies are the families any of the kinds asks for.
func wantedFamilies(kinds []string) []forecastFamily {
	var wanted []forecastFamily
	for _, fam := range forecastFamilies {
		if slices.ContainsFunc(kinds, func(k string) bool { return slices.Contains(fam.kinds(), k) }) {
			wanted = append(wanted, fam)
		}
	}
	return wanted
}

// familyCandidates are the candidates the kinds ask for of each family.
func familyCandidates(kinds []string) []forecastModel {
	var models []forecastModel
	for _, fam := range forecastFamilies {
		for _, v := range fam.variants {
			if slices.Contains(kinds, fam.kind+"-"+v) {
				models = append(models, forecastModel{Kind: fam.kind, Variant: v})
			}
		}
	}
	return models
}

// varies reports whether the candidate is of a family, whose numbers two
// honest workers may disagree on in their last digits.
func (m forecastModel) varies() bool {
	_, ok := familyOf(m.Kind)
	return ok
}

// familyNames names the families, for the form and errors.
func familyNames(families []forecastFamily) string {
	var names []string
	for _, fam := range families {
		names = append(names, fam.name)
	}
	if len(names) < 3 {
		return strings.Join(names, " and ")
	}
	return strings.Join(names[:len(names)-1], ", ") + ", and " + names[len(names)-1]
}

// cannotRun says why this node cannot run the family's image, or nothing
// if it can: it must run container work, and give a container the memory
// the image asks for, if its owner holds containers to less.
func (f Forecast) cannotRun(fam forecastFamily) string {
	switch {
	case !f.Containers:
		return "this node does not run container work"
	case f.Confined.MemoryMB > 0 && f.Confined.MemoryMB < fam.memoryMB:
		return fmt.Sprintf("this node holds a container to %d MB, and %s needs %d", f.Confined.MemoryMB, fam.name, fam.memoryMB)
	}
	return ""
}

// runnable are the families this node can run.
func (f Forecast) runnable() []forecastFamily {
	return slices.DeleteFunc(slices.Clone(forecastFamilies), func(fam forecastFamily) bool { return f.cannotRun(fam) != "" })
}

// refuseFamilies says why a job of the kinds cannot be split on this node,
// if it asks for a family the node cannot run.
func (f Forecast) refuseFamilies(what string, kinds []string) error {
	for _, fam := range wantedFamilies(kinds) {
		if why := f.cannotRun(fam); why != "" {
			return errors.New(what + ": the " + fam.name + " models run in containers, and " + why)
		}
	}
	return nil
}

// runners is how a forecast task fits its candidates: each family's in its
// image, everything else in process.
func (f Forecast) runners(models []forecastModel) (runnerFor, error) {
	byKind := map[string]forecastRunner{}
	for _, fam := range forecastFamilies {
		if !slices.ContainsFunc(models, func(m forecastModel) bool { return m.Kind == fam.kind }) {
			continue
		}
		if why := f.cannotRun(fam); why != "" {
			return nil, errors.New("forecast payload: the task has " + fam.name + " candidates, and " + why)
		}
		byKind[fam.kind] = fam.runner(f)
	}
	if len(byKind) == 0 {
		return inProcess, nil
	}
	return func(m forecastModel) forecastRunner {
		if run, ok := byKind[m.Kind]; ok {
			return run
		}
		return goRunner{}
	}, nil
}

// Needs says that a job that asks for a family needs a worker that runs
// the family's image, the coordinator's.
func (Forecast) Needs(params []byte) []string {
	var p ForecastParams
	if json.Unmarshal(params, &p) != nil {
		return nil
	}
	kinds, err := p.kinds()
	if err != nil {
		return nil
	}
	var labels []string
	for _, fam := range wantedFamilies(kinds) {
		labels = append(labels, fam.label)
	}
	return labels
}

// Offers is the families this node can run.
func (f Forecast) Offers(context.Context) []string {
	var labels []string
	for _, fam := range f.runnable() {
		labels = append(labels, fam.label)
	}
	return labels
}

// modelChoices are the kinds a form offers: a family's only where the
// node can run it.
func (f Forecast) modelChoices() []string {
	choices := append([]string{"all"}, goKinds()...)
	for _, fam := range f.runnable() {
		choices = append(choices, fam.kinds()...)
	}
	return choices
}

func (f Forecast) modelsHelp() string {
	help := "Which kind of model to try, or all of them. Damped, Holt's smoothing with a damped trend, is tried only when asked for."
	families := f.runnable()
	if len(families) == 0 {
		return help
	}
	return help + " The " + familyNames(families) + " models are pretrained, run in an image on workers that run containers, and are tried only when asked for too."
}

// Varies says that, with a family's candidate, two honest workers' outputs
// can differ in their last digits. Compare says when they still agree.
func (Forecast) Varies() bool { return true }

// Compare says whether two outputs of one forecast task are the same
// result. Outputs without a family's candidate are the same only if their
// bytes are. With one, every other candidate must still be the same to the
// bit, and each family candidate's forecasts, residuals and scores must
// agree to within its family's tolerance of the size of the series or of
// what it forecast, whichever is larger. The series' size is given by both
// outputs and must be the same, since it is read from the task's input.
// The choice of forecast is not compared: Aggregate makes it once, from
// the outputs taken.
func (Forecast) Compare(a, b []byte) (bool, string) {
	if bytes.Equal(a, b) {
		return true, ""
	}
	if bytes.Compare(a, b) > 0 {
		a, b = b, a
	}
	var x, y forecastPart
	if json.Unmarshal(a, &x) != nil || json.Unmarshal(b, &y) != nil {
		return false, "an output is not a forecast task's"
	}
	if x.Points != y.Points || x.Horizon != y.Horizon || x.Folds != y.Folds || x.Last != y.Last || x.Scale != y.Scale || len(x.Candidates) != len(y.Candidates) {
		return false, fmt.Sprintf("one backtested %d candidates %d steps ahead on %d windows of %d points of size %g ending at %q, the other %d %d steps ahead on %d windows of %d points of size %g ending at %q", len(x.Candidates), x.Horizon, x.Folds, x.Points, x.Scale, x.Last, len(y.Candidates), y.Horizon, y.Folds, y.Points, y.Scale, y.Last)
	}
	varied, why := compareCandidates(x.Candidates, y.Candidates, x.Scale)
	if why != "" {
		return false, why
	}
	if !varied {
		return false, "the outputs differ, and neither has a candidate that may"
	}
	return true, ""
}

// compareCandidates says how two runs' backtests of the same candidates,
// in the same order, differ by more than they may, or nothing where they
// agree, and whether there was a family's candidate among them, the only
// kind that may differ at all.
func compareCandidates(xs, ys []forecastOutcome, seriesSize float64) (varied bool, why string) {
	for i := range xs {
		p, q := xs[i], ys[i]
		fam, ok := familyOf(p.Model.Kind)
		switch {
		case p.Model != q.Model:
			return false, fmt.Sprintf("candidate %d is %s in one and %s in the other", i+1, p.Model.Name(), q.Model.Name())
		case !ok:
			if !bytes.Equal(mustJSON(p), mustJSON(q)) {
				return false, fmt.Sprintf("candidate %s, which is the same on every processor, differs", p.Model.Name())
			}
		default:
			varied = true
			if why := compareWithin(p, q, seriesSize, fam.tolerance); why != "" {
				return false, fmt.Sprintf("candidate %s: %s", p.Model.Name(), why)
			}
		}
	}
	return varied, ""
}

// Varies says that, with a family's candidate, two honest workers' outputs
// can differ in their last digits, as forecast's can.
func (Forecasts) Varies() bool { return true }

// Compare says whether two outputs of one forecasts task are the same
// result, as forecast's Compare does for one series: each series' must be
// the same but for its family candidates, which must agree within their
// family's tolerance. Which forecast those candidates lead to is chosen
// once, by Aggregate, from the output taken.
func (Forecasts) Compare(a, b []byte) (bool, string) {
	if bytes.Equal(a, b) {
		return true, ""
	}
	if bytes.Compare(a, b) > 0 {
		a, b = b, a
	}
	var x, y forecastsPart
	if json.Unmarshal(a, &x) != nil || json.Unmarshal(b, &y) != nil {
		return false, "an output is not a forecasts task's"
	}
	if x.From != y.From || x.Horizon != y.Horizon || len(x.Series) != len(y.Series) {
		return false, fmt.Sprintf("one forecast %d series from %d, %d steps ahead, the other %d from %d, %d steps ahead", len(x.Series), x.From, x.Horizon, len(y.Series), y.From, y.Horizon)
	}
	varied := false
	for i := range x.Series {
		p, q := x.Series[i], y.Series[i]
		pc, qc := p.Candidates, q.Candidates
		p.Candidates, q.Candidates = nil, nil
		if !bytes.Equal(mustJSON(p), mustJSON(q)) || len(pc) != len(qc) {
			return false, fmt.Sprintf("series %q differs otherwise than in its %s candidates", p.ID, familyNames(forecastFamilies))
		}
		some, why := compareCandidates(pc, qc, p.Scale)
		if why != "" {
			return false, fmt.Sprintf("series %q: %s", p.ID, why)
		}
		varied = varied || some
	}
	if !varied {
		return false, "the outputs differ, and neither has a candidate that may"
	}
	return true, ""
}

// Needs is as forecast's: a job that asks for a family needs a worker that
// runs the family's image, the coordinator's.
func (Forecasts) Needs(params []byte) []string {
	var p ForecastsParams
	if json.Unmarshal(params, &p) != nil {
		return nil
	}
	return Forecast{}.Needs(mustJSON(ForecastParams{Models: p.Models}))
}

// Offers is as forecast's.
func (f Forecasts) Offers(ctx context.Context) []string { return f.forecast().Offers(ctx) }

// compareWithin says how two runs of one family candidate differ by more
// than the tolerance of the size of what was forecast, or nothing where
// they agree. The size is the series' own, its largest value by magnitude,
// or the largest forecast either made if that is larger. A residual is
// what the series did less a forecast of it from a window's past, so it
// moves as much as that forecast does; and a window's forecast is the size
// of the series, though the forecast from its end may be near zero, as for
// a series that swings about it.
func compareWithin(p, q forecastOutcome, seriesSize, tolerance float64) string {
	switch {
	case p.Error != q.Error:
		return fmt.Sprintf("one failed with %q and the other with %q", p.Error, q.Error)
	case p.Error != "":
		return ""
	case len(p.Forecast) != len(q.Forecast) || len(p.StepRMSE) != len(q.StepRMSE) || len(p.Residuals) != len(q.Residuals):
		return "the two forecast, scored or kept residuals for different numbers of steps"
	}
	scale := seriesSize
	for _, v := range slices.Concat(p.Forecast, q.Forecast) {
		scale = max(scale, math.Abs(v))
	}
	limit := tolerance * max(scale, 1e-12)
	within := func(what string, xs, ys []float64) string {
		for i := range xs {
			if gap := math.Abs(xs[i] - ys[i]); !(gap <= limit) {
				return fmt.Sprintf("its %s %d is %g in one and %g in the other, %.3g apart, more than %g of the size %g", what, i+1, xs[i], ys[i], gap, tolerance, scale)
			}
		}
		return ""
	}
	if why := within("forecast step", p.Forecast, q.Forecast); why != "" {
		return why
	}
	if why := within("score", []float64{p.MAE, p.RMSE}, []float64{q.MAE, q.RMSE}); why != "" {
		return why
	}
	if why := within("error at step", p.StepRMSE, q.StepRMSE); why != "" {
		return why
	}
	for k := range p.Residuals {
		if len(p.Residuals[k]) != len(q.Residuals[k]) {
			return fmt.Sprintf("window %d kept residuals for different numbers of steps", k+1)
		}
		if why := within(fmt.Sprintf("window %d residual", k+1), p.Residuals[k], q.Residuals[k]); why != "" {
			return why
		}
	}
	return ""
}

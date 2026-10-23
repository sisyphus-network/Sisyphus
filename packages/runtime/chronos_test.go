package runtime

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/sisyphus-network/Sisyphus/packages/storage"
)

// pretendChronos is an image that forecasts each prefix with its last
// value, as the naive model does, so that what it is asked and what is
// made of its answers can be checked against the models written in Go.
func pretendChronos(t *testing.T, asked *chronosAsked, answer func([]map[string]any) any) *pretend {
	t.Helper()
	return &pretend{do: func(args []string, stdout, stderr io.Writer) (int, error) {
		if args[0] == "image" {
			return 0, nil
		}
		raw, err := os.ReadFile(filepath.Join(volume(args, "/input"), "data"))
		if err != nil {
			return 1, err
		}
		if err := json.Unmarshal(raw, asked); err != nil {
			return 1, err
		}
		var answers []map[string]any
		for _, r := range asked.Requests {
			var forecasts [][]float64
			for _, end := range r.Ends {
				forecasts = append(forecasts, slices.Repeat([]float64{r.Series[end-1]}, r.Horizon))
			}
			answers = append(answers, map[string]any{"forecasts": forecasts})
		}
		var left any = answers
		if answer != nil {
			left = answer(answers)
		}
		if left != nil {
			os.WriteFile(filepath.Join(volume(args, "/output"), "answers.json"), mustJSON(left), 0o644)
		}
		return 0, nil
	}}
}

// forecastJob splits and runs a forecast job on the workload given and puts
// it together.
func forecastJob(t *testing.T, f Forecast, store *storage.Store, params string) (ForecastResult, error) {
	t.Helper()
	payloads, err := f.Split(ctx, store, []byte(params), 2)
	if err != nil {
		return ForecastResult{}, err
	}
	var outputs [][]byte
	for _, p := range payloads {
		out, err := f.Execute(ctx, store, p)
		if err != nil {
			return ForecastResult{}, err
		}
		outputs = append(outputs, out)
	}
	raw, err := f.Aggregate(ctx, store, outputs)
	if err != nil {
		return ForecastResult{}, err
	}
	var result ForecastResult
	json.Unmarshal(raw, &result)
	return result, nil
}

func TestChronosCandidatesAreFittedInTheirImageAndScoredLikeAnyOther(t *testing.T) {
	store := storage.NewMemory()
	series, _ := store.Put(ctx, strings.NewReader(seasonalSeries(48)))
	var asked chronosAsked
	engine := pretendChronos(t, &asked, nil)
	f := Forecast{Containers: true, Engine: engine.engine, Confined: Confinement{ReadOnly: true}}
	result, err := forecastJob(t, f, store, `{"input":"`+series.String()+`","horizon":4,"folds":3,"models":"naive,chronos-base"}`)
	if err != nil {
		t.Fatal(err)
	}
	// The image was asked for the whole series and each window's past,
	// in batches of the size the task fixes.
	if asked.Batch != chronosBatch || len(asked.Requests) != 1 {
		t.Fatalf("asked %+v", asked)
	}
	if r := asked.Requests[0]; r.Variant != "base" || r.Horizon != 4 || len(r.Series) != 48 || !slices.Equal(r.Ends, []int{48, 36, 40, 44}) {
		t.Errorf("asked for %s %d steps ahead from %d points ending at %v", r.Variant, r.Horizon, len(r.Series), r.Ends)
	}
	// It forecast as naive does, so it scored as naive did.
	scores := map[string]ForecastScore{}
	for _, s := range result.Candidates {
		scores[s.Model] = s
	}
	naive, chronos := scores["naive"], scores["chronos variant=base"]
	if naive.MAE == 0 || chronos.MAE != naive.MAE || chronos.RMSE != naive.RMSE {
		t.Errorf("naive scored %+v and the image %+v", naive, chronos)
	}
	// It ran with no network, one processor and bounded memory, in its own
	// image, held in as the worker's owner asked.
	var run string
	for _, a := range engine.asked {
		if a[0] == "run" {
			run = strings.Join(a, " ")
		}
	}
	for _, want := range []string{"--network none", "--memory 4096m", "--cpus 1", "--read-only", ChronosImage + " python -I -c"} {
		if !strings.Contains(run, want) {
			t.Errorf("the image was run as %q, without %q", run, want)
		}
	}
}

func TestOnlyANodeThatRunsContainersTakesChronos(t *testing.T) {
	store := storage.NewMemory()
	series, _ := store.Put(ctx, strings.NewReader(seasonalSeries(48)))
	params := `{"input":"` + series.String() + `","horizon":4,"models":"chronos-tiny"}`
	if _, err := (Forecast{}).Split(ctx, store, []byte(params), 1); err == nil || !strings.Contains(err.Error(), "does not run container work") {
		t.Errorf("a node without containers split a Chronos job: %v", err)
	}
	payload := mustJSON(forecastTask{Input: series.String(), Horizon: 4, Folds: 2, Models: []forecastModel{{Kind: "chronos", Variant: "tiny"}}})
	if _, err := (Forecast{}).Execute(ctx, store, payload); err == nil || !strings.Contains(err.Error(), "does not run container work") {
		t.Errorf("a worker without containers ran a Chronos task: %v", err)
	}
	if _, err := (Forecasts{}).Split(ctx, store, []byte(params), 1); err == nil || !strings.Contains(err.Error(), "not yet offered for many series") {
		t.Errorf("forecasts took Chronos: %v", err)
	}
	// "all" is the models written in Go, as it was.
	kinds, _ := ForecastParams{}.kinds()
	if wantsChronos(kinds) {
		t.Errorf("all is %v", kinds)
	}
	if _, err := (ForecastParams{Models: "chronos-huge"}).kinds(); err == nil || !strings.Contains(err.Error(), "chronos-base") {
		t.Errorf("an unknown model: %v", err)
	}
}

func TestAChronosJobNeedsAWorkerWithTheSameImage(t *testing.T) {
	if !regexp.MustCompile(`^forecast-chronos:[0-9a-f]{12}$`).MatchString(ChronosLabel) || !strings.HasSuffix(ChronosImage, strings.TrimPrefix(ChronosLabel, "forecast-chronos")) {
		t.Errorf("the label %q does not name the image %q", ChronosLabel, ChronosImage)
	}
	for params, want := range map[string][]string{
		`{"models":"naive,chronos-small"}`: {ChronosLabel},
		`{"models":"chronos-tiny"}`:        {ChronosLabel},
		`{"models":"all"}`:                 nil,
		`{}`:                               nil,
		`{"models":"crystal-ball"}`:        nil,
		`not json`:                         nil,
	} {
		if got := (Forecast{}).Needs([]byte(params)); !slices.Equal(got, want) {
			t.Errorf("%s needs %v, want %v", params, got, want)
		}
	}
	if got := (Forecast{Containers: true}).Offers(ctx); !slices.Equal(got, []string{ChronosLabel}) {
		t.Errorf("a node with containers offers %v", got)
	}
	if got := (Forecast{}).Offers(ctx); got != nil {
		t.Errorf("a node without offers %v", got)
	}
	// Its form offers Chronos only where it can be run.
	choices := func(f Forecast) []string {
		for _, field := range f.Form() {
			if field.Name == "models" {
				return field.Choices
			}
		}
		return nil
	}
	if c := choices(Forecast{}); slices.Contains(c, "chronos-base") || !slices.Contains(c, "theta") {
		t.Errorf("without containers the form offers %v", c)
	}
	if c := choices(Forecast{Containers: true}); !slices.Contains(c, "chronos-tiny") || !slices.Contains(c, "chronos-base") {
		t.Errorf("with containers the form offers %v", c)
	}
}

func TestAChronosCandidateIsAModelWithOnlyAVariant(t *testing.T) {
	for _, tt := range []struct {
		model forecastModel
		why   string
	}{
		{forecastModel{Kind: "chronos", Variant: "base"}, ""},
		{forecastModel{Kind: "chronos", Variant: "huge"}, "settings it can take"},
		{forecastModel{Kind: "chronos"}, "settings it can take"},
		{forecastModel{Kind: "chronos", Variant: "tiny", Alpha: 0.5}, "settings it can take"},
		{forecastModel{Kind: "naive", Variant: "tiny"}, "settings it can take"},
	} {
		if got := tt.model.invalid(); (tt.why == "") != (got == "") || !strings.Contains(got, tt.why) {
			t.Errorf("%s: %q, want %q", tt.model.Name(), got, tt.why)
		}
	}
	if name := (forecastModel{Kind: "chronos", Variant: "mini"}).Name(); name != "chronos variant=mini" {
		t.Errorf("named %q", name)
	}
	// The pins are what they say they are.
	hex40, hex64 := regexp.MustCompile(`^[0-9a-f]{40}$`), regexp.MustCompile(`^[0-9a-f]{64}$`)
	for name, p := range chronosVariants {
		if !slices.Contains(chronosKinds, "chronos-"+name) || p.repo != "amazon/chronos-bolt-"+name || !hex40.MatchString(p.revision) || !hex64.MatchString(p.config) || !hex64.MatchString(p.weights) {
			t.Errorf("%s is pinned as %+v", name, p)
		}
		if !strings.Contains(chronosFetch, p.revision) || !strings.Contains(chronosFetch, p.weights) {
			t.Errorf("%s's pin is not what the image is made from", name)
		}
	}
	// The image is made only of hashed packages, from a pinned base.
	for _, line := range strings.Split(strings.TrimSpace(chronosRequirements), "\n") {
		if !strings.Contains(line, "==") || !strings.Contains(line, " --hash=sha256:") {
			t.Errorf("an unpinned requirement: %q", line)
		}
	}
	for _, want := range []string{"FROM python:3.12-slim@sha256:", "--require-hashes", "--no-deps", "HF_HUB_OFFLINE=1"} {
		if !strings.Contains(chronosDockerfile, want) {
			t.Errorf("the Dockerfile has no %q", want)
		}
	}
}

func TestChronosImageAnswersOfTheWrongShapeFailTheTask(t *testing.T) {
	store := storage.NewMemory()
	series, _ := store.Put(ctx, strings.NewReader(seasonalSeries(48)))
	payload := mustJSON(forecastTask{Input: series.String(), Horizon: 4, Folds: 2, Models: []forecastModel{{Kind: "naive"}, {Kind: "chronos", Variant: "tiny"}}})
	for _, tt := range []struct {
		name   string
		answer func([]map[string]any) any
		why    string
	}{
		{"none left", func([]map[string]any) any { return nil }, "left no answers"},
		{"not JSON", func([]map[string]any) any { return json.RawMessage(`"x"`) }, "the answers"},
		{"too few", func([]map[string]any) any { return []any{} }, "answered 0 of 1"},
	} {
		var asked chronosAsked
		engine := pretendChronos(t, &asked, tt.answer)
		_, err := (Forecast{Containers: true, Engine: engine.engine}).Execute(ctx, store, payload)
		if err == nil || !strings.Contains(err.Error(), tt.why) {
			t.Errorf("%s: %v, want %q", tt.name, err, tt.why)
		}
	}
	// A forecast of the wrong length is the candidate's error, not the
	// task's, as from any runner.
	var asked chronosAsked
	short := pretendChronos(t, &asked, func(a []map[string]any) any {
		a[0]["forecasts"] = [][]float64{{1}, {1}, {1}}
		return a
	})
	raw, err := (Forecast{Containers: true, Engine: short.engine}).Execute(ctx, store, payload)
	var part forecastPart
	json.Unmarshal(raw, &part)
	if err != nil || len(part.Candidates) != 2 || !strings.Contains(part.Candidates[1].Error, "1 steps where 4") || part.Candidates[0].Error != "" {
		t.Errorf("a short forecast: %v %+v", err, part.Candidates)
	}
	// An image that fails fails the task.
	broken := &pretend{do: func(args []string, stdout, stderr io.Writer) (int, error) {
		if args[0] == "image" {
			return 0, nil
		}
		io.WriteString(stderr, "Killed\n")
		return 137, nil
	}}
	if _, err := (Forecast{Containers: true, Engine: broken.engine}).Execute(ctx, store, payload); err == nil || !strings.HasPrefix(err.Error(), "chronos: ") {
		t.Errorf("an image that fails: %v", err)
	}
	// A request is made only of prefixes of its series.
	values := []float64{1, 2, 3, 4}
	other := []float64{1, 2}
	if _, err := (chronosRunner{Forecast{Containers: true, Engine: broken.engine}}).fit(ctx, []fitRequest{{Model: forecastModel{Kind: "chronos", Variant: "tiny"}, Horizon: 1, Prefixes: [][]float64{values, other}}}, func(int) {}); err == nil || !strings.Contains(err.Error(), "not the start of its series") {
		t.Errorf("a prefix of another series: %v", err)
	}
	// An image that cannot be built fails the task.
	failing := &pretend{do: func(args []string, stdout, stderr io.Writer) (int, error) {
		io.WriteString(stderr, "no space left on device\n")
		return 1, nil
	}}
	if _, err := (Forecast{Containers: true, Engine: failing.engine}).Execute(ctx, store, payload); err == nil || !strings.Contains(err.Error(), "no space left") {
		t.Errorf("an image that cannot be built: %v", err)
	}
}

func TestForecastOutputsAreComparedExactlyExceptChronos(t *testing.T) {
	base := func() forecastPart {
		return forecastPart{Points: 48, Horizon: 2, Folds: 2, Last: "48", Candidates: []forecastOutcome{
			{Model: forecastModel{Kind: "naive"}, MAE: 1.5, RMSE: 2, StepRMSE: []float64{1, 2}, Forecast: []float64{100, 100}, Residuals: [][]float64{{1, 2}, {-1, 3}}},
			{Model: forecastModel{Kind: "chronos", Variant: "base"}, MAE: 1.25, RMSE: 1.75, StepRMSE: []float64{1.5, 2}, Forecast: []float64{101, 103}, Residuals: [][]float64{{0.5, 1}, {-0.25, 2}}},
		}}
	}
	changed := func(change func(*forecastPart)) []byte {
		p := base()
		change(&p)
		return mustJSON(p)
	}
	// The forecast's size is 103, so the tolerance is 0.103.
	original := mustJSON(base())
	for _, tt := range []struct {
		name  string
		other []byte
		want  string
	}{
		{"the same bytes", original, ""},
		{"Chronos a hundredth of the tolerance apart", changed(func(p *forecastPart) { p.Candidates[1].Forecast[1] += 0.001 }), ""},
		{"a Chronos residual just inside", changed(func(p *forecastPart) { p.Candidates[1].Residuals[1][0] += 0.1 }), ""},
		{"a Chronos residual just outside", changed(func(p *forecastPart) { p.Candidates[1].Residuals[1][0] += 0.11 }), "window 2 residual 1 is"},
		{"a Chronos forecast just outside", changed(func(p *forecastPart) { p.Candidates[1].Forecast[0] += 0.2 }), "forecast step 1 is"},
		{"a Chronos score outside", changed(func(p *forecastPart) { p.Candidates[1].MAE += 1 }), "score 1 is"},
		{"a Chronos step error outside", changed(func(p *forecastPart) { p.Candidates[1].StepRMSE[1] += 1 }), "error at step 2 is"},
		{"Chronos failed in one", changed(func(p *forecastPart) {
			p.Candidates[1] = forecastOutcome{Model: p.Candidates[1].Model, Error: "out of memory"}
		}), `"out of memory"`},
		{"Chronos failed the same way in both", changed(func(p *forecastPart) {
			p.Candidates[1] = forecastOutcome{Model: p.Candidates[1].Model, Error: "out of memory"}
		}), "failed with"},
		{"Chronos kept fewer residuals", changed(func(p *forecastPart) { p.Candidates[1].Residuals = p.Candidates[1].Residuals[:1] }), "different numbers of steps"},
		{"Chronos kept a short window", changed(func(p *forecastPart) { p.Candidates[1].Residuals[0] = p.Candidates[1].Residuals[0][:1] }), "window 1 kept residuals"},
		{"the Go candidate a hundredth apart", changed(func(p *forecastPart) { p.Candidates[0].Forecast[0] += 0.001 }), "candidate naive, which is the same on every processor, differs"},
		{"a candidate missing", changed(func(p *forecastPart) { p.Candidates = p.Candidates[:1] }), "one backtested"},
		{"the candidates in another order", changed(func(p *forecastPart) { p.Candidates[0], p.Candidates[1] = p.Candidates[1], p.Candidates[0] }), "candidate 1 is"},
		{"another series", changed(func(p *forecastPart) { p.Last = "49" }), "ending at"},
		{"not an output", []byte("not json"), "not a forecast task's"},
	} {
		same, why := Forecast{}.Compare(original, tt.other)
		back, whyBack := Forecast{}.Compare(tt.other, original)
		if same != back || why != whyBack {
			t.Errorf("%s: compared one way %t %q, the other %t %q", tt.name, same, why, back, whyBack)
		}
		if same != (tt.want == "") || !strings.Contains(why, tt.want) {
			t.Errorf("%s: %t, %q, want %q", tt.name, same, why, tt.want)
		}
	}
	// Without a Chronos candidate, only the same bytes are the same.
	goOnly := forecastPart{Points: 48, Horizon: 2, Folds: 2, Last: "48", Candidates: base().Candidates[:1]}
	nudged := goOnly
	nudged.Candidates = []forecastOutcome{goOnly.Candidates[0]}
	nudged.Candidates[0].MAE += 1e-12
	if same, why := (Forecast{}).Compare(mustJSON(goOnly), mustJSON(nudged)); same || why == "" {
		t.Errorf("Go-only outputs a hair apart: %t %q", same, why)
	}
	// Nor are the same numbers written otherwise, where nothing may vary.
	if same, why := (Forecast{}).Compare(mustJSON(goOnly), append(mustJSON(goOnly), ' ')); same || !strings.Contains(why, "neither has a candidate that may") {
		t.Errorf("Go-only outputs written otherwise: %t %q", same, why)
	}
	// Two runs where Chronos failed alike agree.
	failed := base()
	failed.Candidates[1] = forecastOutcome{Model: failed.Candidates[1].Model, Error: "out of memory"}
	if same, why := (Forecast{}).Compare(mustJSON(failed), append(mustJSON(failed), ' ')); !same {
		t.Errorf("Chronos failing alike: %q", why)
	}
	if !Checkable(Forecast{}) {
		t.Error("forecast's tasks cannot be checked")
	}
}

// Chronos-Bolt forecasts a real series in its real image, through Docker,
// and a task run twice on one machine gives the same bytes.
func TestChronosForecastsARealSeriesInItsImage(t *testing.T) {
	requireRealImages(t)
	store := storage.NewMemory()
	series, _ := store.Put(ctx, strings.NewReader(seasonalSeries(60)))
	f := Forecast{Containers: true}
	payloads, err := f.Split(ctx, store, []byte(`{"input":"`+series.String()+`","horizon":6,"folds":2,"models":"naive,chronos-tiny"}`), 1)
	if err != nil {
		t.Fatal(err)
	}
	first, err := f.Execute(ctx, store, payloads[0])
	if err != nil {
		t.Fatal(err)
	}
	again, err := f.Execute(ctx, store, payloads[0])
	if err != nil {
		t.Fatal(err)
	}
	if string(first) != string(again) {
		t.Errorf("a task run twice on one machine gave different bytes")
	}
	if same, why := f.Compare(first, again); !same {
		t.Errorf("a task compared with itself: %s", why)
	}
	var part forecastPart
	json.Unmarshal(first, &part)
	var chronos *forecastOutcome
	for i, o := range part.Candidates {
		if o.Model.Kind == "chronos" {
			chronos = &part.Candidates[i]
		}
	}
	if chronos == nil || chronos.Error != "" || !wellFormed(*chronos, 6, 2) {
		t.Fatalf("Chronos-Bolt gave %+v", chronos)
	}
	// The series is a year of months with a season of 12 and a trend, which
	// Chronos-Bolt forecasts closer than within a tenth of its size.
	if chronos.MAE <= 0 || chronos.MAE > 0.1*slices.Max(chronos.Forecast) {
		t.Errorf("Chronos-Bolt's backtest error is %g against a forecast of %v", chronos.MAE, chronos.Forecast)
	}
	raw, err := f.Aggregate(ctx, store, [][]byte{first})
	if err != nil {
		t.Fatal(err)
	}
	var result ForecastResult
	json.Unmarshal(raw, &result)
	if len(result.Forecast) != 6 {
		t.Errorf("the job's result: %s", raw)
	}
}

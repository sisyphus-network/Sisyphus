package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
	if _, err := (Forecasts{}).Split(ctx, store, []byte(params), 1); err == nil || !strings.Contains(err.Error(), "does not run container work") {
		t.Errorf("a node without containers split a Chronos job of many series: %v", err)
	}
	panel, _ := store.Put(ctx, strings.NewReader(panelOf(map[string]string{"north": seasonalSeries(48)})))
	task := mustJSON(forecastsTask{Input: panel.String(), To: 1, Horizon: 4, Kinds: []string{"chronos-tiny"}})
	if _, err := (Forecasts{}).Execute(ctx, store, task); err == nil || !strings.Contains(err.Error(), "does not run container work") {
		t.Errorf("a worker without containers ran a Chronos task of many series: %v", err)
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

// Whatever goes wrong in the image, as when it runs out of memory or
// answers in the wrong shape, is its candidates' error: the candidates
// written in Go are still scored, and the task fails only if it was given
// up as a whole.
func TestChronosImageFailuresAreItsCandidatesErrors(t *testing.T) {
	store := storage.NewMemory()
	series, _ := store.Put(ctx, strings.NewReader(seasonalSeries(48)))
	payload := mustJSON(forecastTask{Input: series.String(), Horizon: 4, Folds: 2, Models: []forecastModel{{Kind: "naive"}, {Kind: "chronos", Variant: "tiny"}}})
	outputs := map[string][]byte{}
	answered := func(name string, engine func(context.Context, []string, io.Writer, io.Writer) (int, error)) ([]forecastOutcome, []string, error) {
		var log heard
		raw, err := (Forecast{Containers: true, Engine: engine}).Execute(WithReporter(ctx, &log), store, payload)
		outputs[name] = raw
		var part forecastPart
		json.Unmarshal(raw, &part)
		return part.Candidates, log.lines, err
	}
	broken := &pretend{do: func(args []string, stdout, stderr io.Writer) (int, error) {
		if args[0] == "image" {
			return 0, nil
		}
		io.WriteString(stderr, "Killed\n")
		return 137, nil
	}}
	failing := &pretend{do: func(args []string, stdout, stderr io.Writer) (int, error) {
		io.WriteString(stderr, "no space left on device\n")
		return 1, nil
	}}
	var asked chronosAsked
	for _, tt := range []struct {
		name   string
		engine func(context.Context, []string, io.Writer, io.Writer) (int, error)
		why    string
	}{
		{"none left", pretendChronos(t, &asked, func([]map[string]any) any { return nil }).engine, "left no answers"},
		{"not JSON", pretendChronos(t, &asked, func([]map[string]any) any { return json.RawMessage(`"x"`) }).engine, "the answers"},
		{"too few", pretendChronos(t, &asked, func([]map[string]any) any { return []any{} }).engine, "answered 0 of 1"},
		{"short of memory", broken.engine, "exited with status 137: Killed"},
		{"not built", failing.engine, "no space left"},
		{"a forecast of the wrong length", pretendChronos(t, &asked, func(a []map[string]any) any {
			a[0]["forecasts"] = [][]float64{{1}, {1}, {1}}
			return a
		}).engine, "1 steps where 4"},
	} {
		candidates, lines, err := answered(tt.name, tt.engine)
		if err != nil || len(candidates) != 2 || candidates[0].Error != "" || candidates[0].Forecast == nil {
			t.Errorf("%s: %v %+v, want naive scored", tt.name, err, candidates)
			continue
		}
		// What went wrong in the image is in the task's log, and the
		// candidate's error says only that it failed, in words every
		// worker uses alike; a forecast of the wrong length is scored
		// as any runner's is.
		logged := strings.Contains(strings.Join(lines, "\n"), tt.why)
		if tt.name == "a forecast of the wrong length" {
			if !strings.Contains(candidates[1].Error, tt.why) {
				t.Errorf("%s: %q, want %q", tt.name, candidates[1].Error, tt.why)
			}
		} else if candidates[1].Error != chronosFailed || !logged {
			t.Errorf("%s: candidate error %q, logged %t, want %q with %q logged", tt.name, candidates[1].Error, logged, chronosFailed, tt.why)
		}
	}
	// So two honest workers whose images failed in their own ways agree.
	if same, why := (Forecast{}).Compare(outputs["short of memory"], outputs["not built"]); !same {
		t.Errorf("two images failing in their own ways: %q", why)
	}
	// A task given up as a whole fails.
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := (chronosRunner{Forecast{Containers: true, Engine: broken.engine}}).fit(cancelled, []fitRequest{{Model: forecastModel{Kind: "chronos", Variant: "tiny"}, Horizon: 1, Prefixes: [][]float64{{1, 2}}}}, func(int) {}); !errors.Is(err, context.Canceled) {
		t.Errorf("a cancelled task: %v", err)
	}
	// A request is made only of prefixes of its series.
	values := []float64{1, 2, 3, 4}
	other := []float64{1, 2}
	if _, err := (chronosRunner{Forecast{Containers: true, Engine: broken.engine}}).fit(ctx, []fitRequest{{Model: forecastModel{Kind: "chronos", Variant: "tiny"}, Horizon: 1, Prefixes: [][]float64{values, other}}}, func(int) {}); err == nil || !strings.Contains(err.Error(), "not the start of its series") {
		t.Errorf("a prefix of another series: %v", err)
	}
}

// A node whose owner holds containers to less memory than Chronos-Bolt
// needs neither offers it nor runs it.
func TestANodeThatCannotGiveChronosItsMemoryDoesNotOfferIt(t *testing.T) {
	for _, tt := range []struct {
		memory int
		offers bool
	}{{0, true}, {chronosMemoryMB, true}, {2 * chronosMemoryMB, true}, {chronosMemoryMB - 1, false}, {512, false}} {
		f := Forecast{Containers: true, Confined: Confinement{MemoryMB: tt.memory}}
		offered := slices.Equal(f.Offers(ctx), []string{ChronosLabel})
		choices := slices.Contains(f.modelChoices(), "chronos-tiny")
		if offered != tt.offers || choices != tt.offers || strings.Contains(f.modelsHelp(), "Chronos-Bolt") != tt.offers {
			t.Errorf("held to %d MB: offers %t, lists %t, want %t", tt.memory, offered, choices, tt.offers)
		}
	}
	store := storage.NewMemory()
	series, _ := store.Put(ctx, strings.NewReader(seasonalSeries(48)))
	small := Forecast{Containers: true, Confined: Confinement{MemoryMB: 1024}}
	if _, err := small.Split(ctx, store, []byte(`{"input":"`+series.String()+`","horizon":4,"models":"chronos-tiny"}`), 1); err == nil || !strings.Contains(err.Error(), "holds a container to 1024 MB, and Chronos-Bolt needs 4096") {
		t.Errorf("a small node split a Chronos job: %v", err)
	}
	payload := mustJSON(forecastTask{Input: series.String(), Horizon: 4, Folds: 2, Models: []forecastModel{{Kind: "chronos", Variant: "tiny"}}})
	if _, err := small.Execute(ctx, store, payload); err == nil || !strings.Contains(err.Error(), "holds a container to 1024 MB") {
		t.Errorf("a small worker ran a Chronos task: %v", err)
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

// A series that swings about zero can be forecast near zero from its end
// though its windows' forecasts, and so its residuals, are the size of the
// series. Two runs are compared against the series' size, which both give
// and which must be the same.
func TestChronosIsComparedAgainstTheSizeOfItsSeries(t *testing.T) {
	swinging := func(residual float64) forecastPart {
		return forecastPart{Points: 48, Horizon: 2, Folds: 1, Last: "48", Scale: 100, Candidates: []forecastOutcome{
			{Model: forecastModel{Kind: "chronos", Variant: "base"}, MAE: 50, RMSE: 50, StepRMSE: []float64{50, 50}, Forecast: []float64{0.01, -0.01}, Residuals: [][]float64{{residual, -50}}},
		}}
	}
	// The tolerance is a thousandth of 100, not of 0.01.
	if same, why := (Forecast{}).Compare(mustJSON(swinging(50)), mustJSON(swinging(50.05))); !same {
		t.Errorf("a residual 0.05 apart on a series of size 100: %q", why)
	}
	if same, why := (Forecast{}).Compare(mustJSON(swinging(50)), mustJSON(swinging(50.2))); same || !strings.Contains(why, "of the size 100") {
		t.Errorf("a residual 0.2 apart on a series of size 100: %t %q", same, why)
	}
	// A worker cannot widen it by giving a larger size.
	wider := swinging(50.2)
	wider.Scale = 1000
	if same, why := (Forecast{}).Compare(mustJSON(swinging(50)), mustJSON(wider)); same || !strings.Contains(why, "of size 100") {
		t.Errorf("a size given larger: %t %q", same, why)
	}
	// A task gives its series' size only when it has a Chronos-Bolt
	// candidate, so outputs without one are as they were.
	store := storage.NewMemory()
	series, _ := store.Put(ctx, strings.NewReader("value\n3\n-7\n5\n-2\n4\n-6\n1\n2\n"))
	var asked chronosAsked
	f := Forecast{Containers: true, Engine: pretendChronos(t, &asked, nil).engine}
	for models, want := range map[string]float64{
		`[{"kind":"naive"}]`: 0,
		`[{"kind":"naive"},{"kind":"chronos","variant":"tiny"}]`: 7,
	} {
		raw, err := f.Execute(ctx, store, []byte(`{"input":"`+series.String()+`","horizon":1,"folds":1,"models":`+models+`}`))
		var part forecastPart
		json.Unmarshal(raw, &part)
		if err != nil || part.Scale != want {
			t.Errorf("%s: size %g, %v, want %g", models, part.Scale, err, want)
		}
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

// forecastsJob splits and runs a forecasts job on the workload given,
// returning its tasks' outputs and what Aggregate made of them.
func forecastsJob(t *testing.T, f Forecasts, store *storage.Store, params ForecastsParams, parts int) ([][]byte, ForecastsResult) {
	t.Helper()
	payloads, err := f.Split(ctx, store, mustJSON(params), parts)
	if err != nil {
		t.Fatal(err)
	}
	var outputs [][]byte
	for _, p := range payloads {
		out, err := f.Execute(ctx, store, p)
		if err != nil {
			t.Fatal(err)
		}
		outputs = append(outputs, out)
	}
	raw, err := f.Aggregate(ctx, store, outputs)
	if err != nil {
		t.Fatal(err)
	}
	var result ForecastsResult
	json.Unmarshal(raw, &result)
	return outputs, result
}

// With Chronos-Bolt, each series of a forecasts task carries the backtests
// its forecast is chosen from, and the coordinator chooses it: the same
// forecast a forecast job of that series alone comes to.
func TestManySeriesWithChronosAreEachForecastAsOneWouldBe(t *testing.T) {
	store := storage.NewMemory()
	input, _ := store.Put(ctx, strings.NewReader(panelOf(map[string]string{"north": seasonalSeries(72), "south": seasonalSeries(60), "tiny": seasonalSeries(5)})))
	var asked chronosAsked
	engine := pretendChronos(t, &asked, func(answers []map[string]any) any {
		// Chronos forecasts a little above the last value, so that it
		// scores otherwise than naive.
		for _, a := range answers {
			for _, f := range a["forecasts"].([][]float64) {
				for j := range f {
					f[j] += 0.5
				}
			}
		}
		return answers
	})
	f := Forecasts{Containers: true, Engine: engine.engine}
	models := "naive,ses,holtwinters,chronos-base,chronos-tiny"
	outputs, result := forecastsJob(t, f, store, ForecastsParams{Input: input.String(), Horizon: 6, Season: 12, Models: models}, 2)
	if result.Series != 3 || result.Forecast != 2 || result.Failed != 1 {
		t.Errorf("result %+v", result)
	}
	// What a task returns: for each series that could be backtested, the
	// best of each kind written in Go and every Chronos-Bolt candidate.
	var part forecastsPart
	json.Unmarshal(outputs[0], &part)
	north := part.Series[0]
	var names []string
	for _, o := range north.Candidates {
		names = append(names, o.Model.Kind)
	}
	if north.ID != "north" || north.Model != "" || north.Forecast != nil || north.Folds != 8 || strings.Join(names, " ") != "naive ses holtwinters chronos chronos" {
		t.Errorf("north as its task returned it: %s", mustJSON(north))
	}
	// What Aggregate makes of it is what forecast makes of north alone.
	one, _ := store.Put(ctx, strings.NewReader(seasonalSeries(72)))
	alone, err := forecastJob(t, f.forecast(), store, `{"input":"`+one.String()+`","horizon":6,"season":12,"models":"`+models+`"}`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(alone.Chosen.Model, "chronos") {
		t.Errorf("Chronos-Bolt did not play a part in forecasting north: %s", alone.Chosen.Model)
	}
	table := strings.Split(strings.TrimSpace(readStored(t, store, result.ForecastTable)), "\n")
	for j, p := range alone.Forecast {
		want := fmt.Sprintf("north,%d,%s,%s,%s,%s,%s", p.Step, plain(p.Value), plain(p.Lo80), plain(p.Hi80), plain(p.Lo95), plain(p.Hi95))
		if table[1+j] != want {
			t.Errorf("north step %d is %q, where forecast alone gives %q", j+1, table[1+j], want)
		}
	}
	rows := strings.Split(strings.TrimSpace(readStored(t, store, result.ModelsTable)), "\n")
	if !strings.Contains(rows[1], ","+alone.Chosen.Model+",") {
		t.Errorf("north was forecast with %q, where forecast alone chose %q", rows[1], alone.Chosen.Model)
	}
	// It needs a worker with the image, and a node that runs containers
	// offers it once, for both workloads.
	if got := (Forecasts{}).Needs(mustJSON(ForecastsParams{Models: models})); !slices.Equal(got, []string{ChronosLabel}) {
		t.Errorf("a forecasts job with Chronos-Bolt needs %v", got)
	}
	if (Forecasts{}).Needs(mustJSON(ForecastsParams{Models: "naive"})) != nil || (Forecasts{}).Needs([]byte("{")) != nil {
		t.Error("a forecasts job without Chronos-Bolt needs something")
	}
	if !slices.Contains((Forecasts{Containers: true}).Form()[4].Choices, "chronos-base") || slices.Contains((Forecasts{}).Form()[4].Choices, "chronos-base") {
		t.Error("the form offers Chronos-Bolt where it should not, or not where it should")
	}
}

// Of a series' candidates, the best of each kind written in Go is all
// forecast's choice needs, and every Chronos-Bolt candidate is kept.
func TestTheCandidatesKeptChooseAsAllOfThemWould(t *testing.T) {
	o := func(kind, variant string, mae float64, why string) forecastOutcome {
		return forecastOutcome{Model: forecastModel{Kind: kind, Variant: variant, Alpha: mae / 10}, MAE: mae, RMSE: mae, Error: why}
	}
	all := []forecastOutcome{o("naive", "", 3, ""), o("ses", "", 2, ""), o("ses", "", 1, ""), o("ses", "", 0, "no"), o("ar", "", 0, "no"), o("ar", "", 0, "worse"), o("chronos", "tiny", 4, ""), o("chronos", "base", 5, "")}
	var got []string
	for _, k := range keptCandidates(all) {
		got = append(got, k.Model.Name()+" "+k.Error)
	}
	want := []string{all[0].Model.Name() + " ", all[2].Model.Name() + " ", all[4].Model.Name() + " no", all[6].Model.Name() + " ", all[7].Model.Name() + " "}
	if !slices.Equal(got, want) {
		t.Errorf("kept %q, want %q", got, want)
	}
}

func TestForecastsOutputsAreComparedExactlyExceptChronos(t *testing.T) {
	base := func() forecastsPart {
		return forecastsPart{From: 0, Horizon: 2, Series: []SeriesForecast{
			{ID: "north", Points: 48, Last: "48", Folds: 2, Candidates: []forecastOutcome{
				{Model: forecastModel{Kind: "naive"}, MAE: 1.5, RMSE: 2, StepRMSE: []float64{1, 2}, Forecast: []float64{100, 100}},
				{Model: forecastModel{Kind: "chronos", Variant: "base"}, MAE: 1.25, RMSE: 1.75, StepRMSE: []float64{1.5, 2}, Forecast: []float64{101, 103}},
			}},
			{ID: "tiny", Points: 3, Error: "3 points are too few"},
		}}
	}
	changed := func(change func(*forecastsPart)) []byte {
		p := base()
		change(&p)
		return mustJSON(p)
	}
	plainly := mustJSON(forecastsPart{Horizon: 2, Series: []SeriesForecast{{ID: "tiny", Points: 3, Error: "too few"}}})
	original := mustJSON(base())
	for _, tt := range []struct {
		name  string
		other []byte
		want  string
	}{
		{"the same bytes", original, ""},
		{"Chronos-Bolt within the tolerance", changed(func(p *forecastsPart) { p.Series[0].Candidates[1].Forecast[1] = 103.1 }), ""},
		{"Chronos-Bolt beyond it", changed(func(p *forecastsPart) { p.Series[0].Candidates[1].Forecast[1] = 104 }), `series "north": candidate chronos variant=base: its forecast step 2`},
		{"a candidate in Go differs", changed(func(p *forecastsPart) { p.Series[0].Candidates[0].MAE = 1.5000001 }), "candidate naive, which is the same on every processor, differs"},
		{"a candidate fewer", changed(func(p *forecastsPart) { p.Series[0].Candidates = p.Series[0].Candidates[:1] }), `series "north" differs otherwise`},
		{"another error", changed(func(p *forecastsPart) { p.Series[1].Error = "too few" }), `series "tiny" differs otherwise`},
		{"a series fewer", changed(func(p *forecastsPart) { p.Series = p.Series[:1] }), "series from 0, 2 steps ahead"},
		{"not an output", []byte("{"), "an output is not a forecasts task's"},
	} {
		same, why := Forecasts{}.Compare(original, tt.other)
		back, whyBack := Forecasts{}.Compare(tt.other, original)
		if same != back || why != whyBack {
			t.Errorf("%s: compared one way %t %q, the other %t %q", tt.name, same, why, back, whyBack)
		}
		if same != (tt.want == "") || !strings.Contains(why, tt.want) {
			t.Errorf("%s: %t, %q, want %q", tt.name, same, why, tt.want)
		}
	}
	// Outputs without Chronos-Bolt are the same only if their bytes are.
	other := mustJSON(forecastsPart{Horizon: 2, Series: []SeriesForecast{{ID: "tiny", Points: 3, Error: "too few!"}}})
	if same, why := (Forecasts{}).Compare(plainly, other); same || !strings.Contains(why, "differs otherwise") {
		t.Errorf("two outputs without Chronos-Bolt: %t %q", same, why)
	}
	if same, why := (Forecasts{}).Compare(plainly, append(slices.Clone(plainly), ' ')); same || !strings.Contains(why, "neither has a candidate that may") {
		t.Errorf("two outputs that differ only in their bytes: %t %q", same, why)
	}
	if !Checkable(Forecasts{}) {
		t.Error("forecasts' tasks cannot be checked")
	}
}

// Each series' Chronos-Bolt candidates are compared against that series'
// size, which a task gives with its candidates and which both outputs must
// give alike; and a node that cannot give Chronos-Bolt its memory takes no
// forecasts job that asks for it.
func TestManySeriesAreComparedEachAgainstItsOwnSize(t *testing.T) {
	part := func(residual, size float64) forecastsPart {
		return forecastsPart{Horizon: 1, Series: []SeriesForecast{{ID: "swing", Points: 9, Folds: 1, Scale: size, Candidates: []forecastOutcome{
			{Model: forecastModel{Kind: "chronos", Variant: "base"}, MAE: 50, RMSE: 50, StepRMSE: []float64{50}, Forecast: []float64{0.01}, Residuals: [][]float64{{residual}}},
		}}}}
	}
	if same, why := (Forecasts{}).Compare(mustJSON(part(50, 100)), mustJSON(part(50.05, 100))); !same {
		t.Errorf("a residual 0.05 apart on a series of size 100: %q", why)
	}
	if same, why := (Forecasts{}).Compare(mustJSON(part(50, 100)), mustJSON(part(50.2, 100))); same || !strings.Contains(why, `series "swing"`) || !strings.Contains(why, "of the size 100") {
		t.Errorf("a residual 0.2 apart on a series of size 100: %t %q", same, why)
	}
	if same, why := (Forecasts{}).Compare(mustJSON(part(50, 100)), mustJSON(part(50.2, 1000))); same || !strings.Contains(why, "differs otherwise") {
		t.Errorf("a size given larger: %t %q", same, why)
	}
	// A size without candidates is not a series' output.
	if _, err := (Forecasts{}).Aggregate(ctx, storage.NewMemory(), [][]byte{mustJSON(forecastsPart{Horizon: 1, Series: []SeriesForecast{{ID: "a", Points: 9, Scale: 3}}})}); err == nil {
		t.Error("a size without candidates was taken")
	}
	// A task gives each series' size with its candidates.
	store := storage.NewMemory()
	input, _ := store.Put(ctx, strings.NewReader(panelOf(map[string]string{"north": seasonalSeries(48)})))
	var asked chronosAsked
	f := Forecasts{Containers: true, Engine: pretendChronos(t, &asked, nil).engine}
	outputs, _ := forecastsJob(t, f, store, ForecastsParams{Input: input.String(), Horizon: 4, Models: "naive,chronos-tiny"}, 1)
	var got forecastsPart
	json.Unmarshal(outputs[0], &got)
	values, _, _ := readSeries(ctx, store, mustPut(t, store, seasonalSeries(48)))
	if got.Series[0].Scale != seriesScale(values) || got.Series[0].Scale == 0 {
		t.Errorf("north's size: %g, want %g", got.Series[0].Scale, seriesScale(values))
	}
	// A node held to less memory than Chronos-Bolt needs offers it for
	// neither workload, and takes no such job.
	small := Forecasts{Containers: true, Confined: Confinement{MemoryMB: 2048}}
	if small.Offers(ctx) != nil || slices.Contains(small.Form()[4].Choices, "chronos-tiny") {
		t.Error("a node held to 2048 MB offers Chronos-Bolt for many series")
	}
	if _, err := small.Split(ctx, store, mustJSON(ForecastsParams{Input: input.String(), Horizon: 4, Models: "chronos-tiny"}), 1); err == nil || !strings.Contains(err.Error(), "holds a container to 2048 MB") {
		t.Errorf("a node held to 2048 MB split a Chronos-Bolt job of many series: %v", err)
	}
}

// mustPut stores text and returns its CID as a string.
func mustPut(t *testing.T, store *storage.Store, text string) string {
	t.Helper()
	c, err := store.Put(ctx, strings.NewReader(text))
	if err != nil {
		t.Fatal(err)
	}
	return c.String()
}

// A task's candidates to choose from are checked as forecast checks a
// task's: a series with them has no forecast of its own, and every
// candidate fitted is finite and of the right shape.
func TestForecastsCandidatesOfTheWrongShapeAreRefused(t *testing.T) {
	store := storage.NewMemory()
	good := forecastOutcome{Model: forecastModel{Kind: "naive"}, MAE: 1, RMSE: 1, StepRMSE: []float64{1, 1}, Forecast: []float64{1, 1}}
	for name, s := range map[string]SeriesForecast{
		"candidates and a model":     {ID: "a", Points: 9, Folds: 2, Model: "naive", Candidates: []forecastOutcome{good}},
		"candidates and an error":    {ID: "a", Points: 9, Folds: 2, Error: "no", Candidates: []forecastOutcome{good}},
		"windows and no candidates":  {ID: "a", Points: 9, Folds: 2},
		"candidates and no windows":  {ID: "a", Points: 9, Candidates: []forecastOutcome{good}},
		"a candidate of three steps": {ID: "a", Points: 9, Folds: 2, Candidates: []forecastOutcome{{Model: good.Model, StepRMSE: []float64{1, 1, 1}, Forecast: []float64{1, 1, 1}}}},
	} {
		output := mustJSON(forecastsPart{Horizon: 2, Series: []SeriesForecast{s}})
		if _, err := (Forecasts{}).Aggregate(ctx, store, [][]byte{output}); err == nil {
			t.Errorf("%s: taken", name)
		}
	}
	// One whose candidates were all failures is a series that failed.
	output := mustJSON(forecastsPart{Horizon: 2, Series: []SeriesForecast{{ID: "a", Points: 9, Folds: 2, Candidates: []forecastOutcome{{Model: good.Model, Error: "no"}}}}})
	raw, err := (Forecasts{}).Aggregate(ctx, store, [][]byte{output})
	var result ForecastsResult
	json.Unmarshal(raw, &result)
	if err != nil || result.Failed != 1 {
		t.Errorf("a series none of whose candidates were fitted: %+v, %v", result, err)
	}
}

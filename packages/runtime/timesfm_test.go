package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/sisyphus-network/Sisyphus/packages/storage"
)

// pretendTimesFM is an image that forecasts each prefix with its last
// value, as the naive model does, so that what it is asked and what is made
// of its answers can be checked against the models written in Go.
func pretendTimesFM(t *testing.T, asked *timesfmAsked, answer func([]map[string]any) any) *pretend {
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

func TestTimesFMCandidatesAreFittedInTheirImageAndScoredLikeAnyOther(t *testing.T) {
	store := storage.NewMemory()
	series, _ := store.Put(ctx, strings.NewReader(seasonalSeries(48)))
	var asked timesfmAsked
	engine := pretendTimesFM(t, &asked, nil)
	f := Forecast{Containers: true, Engine: engine.engine, Confined: Confinement{ReadOnly: true}}
	result, err := forecastJob(t, f, store, `{"input":"`+series.String()+`","horizon":4,"folds":3,"models":"naive,timesfm-200m"}`)
	if err != nil {
		t.Fatal(err)
	}
	// The image was asked for the whole series and each window's past, in
	// batches of the size the task fixes.
	if asked.Batch != timesfmBatch || len(asked.Requests) != 1 {
		t.Fatalf("asked %+v", asked)
	}
	if r := asked.Requests[0]; r.Horizon != 4 || len(r.Series) != 48 || !slices.Equal(r.Ends, []int{48, 36, 40, 44}) {
		t.Errorf("asked for %d steps ahead from %d points ending at %v", r.Horizon, len(r.Series), r.Ends)
	}
	// It forecast as naive does, so it scored as naive did.
	scores := map[string]ForecastScore{}
	for _, s := range result.Candidates {
		scores[s.Model] = s
	}
	naive, timesfm := scores["naive"], scores["timesfm variant=200m"]
	if naive.MAE == 0 || timesfm.MAE != naive.MAE || timesfm.RMSE != naive.RMSE {
		t.Errorf("naive scored %+v and the image %+v", naive, timesfm)
	}
	// It ran with no network, one processor and bounded memory, in its own
	// image, held in as the worker's owner asked.
	var run string
	for _, a := range engine.asked {
		if a[0] == "run" {
			run = strings.Join(a, " ")
		}
	}
	for _, want := range []string{"--network none", "--memory 3072m", "--cpus 1", "--read-only", TimesFMImage + " python -I -c"} {
		if !strings.Contains(run, want) {
			t.Errorf("the image was run as %q, without %q", run, want)
		}
	}
}

func TestATimesFMJobNeedsAWorkerWithTheSameImage(t *testing.T) {
	if !regexp.MustCompile(`^forecast-timesfm:[0-9a-f]{12}$`).MatchString(TimesFMLabel) || !strings.HasSuffix(TimesFMImage, strings.TrimPrefix(TimesFMLabel, "forecast-timesfm")) {
		t.Errorf("the label %q does not name the image %q", TimesFMLabel, TimesFMImage)
	}
	for params, want := range map[string][]string{
		`{"models":"naive,timesfm-200m"}`:              {TimesFMLabel},
		`{"models":"chronos-tiny,timesfm-200m"}`:       {ChronosLabel, TimesFMLabel},
		`{"models":"all"}`:                             nil,
		`{"models":"timesfm-500m"}`:                    nil,
		`{"models":"naive,mean,drift,ses,holt,theta"}`: nil,
	} {
		if got := (Forecast{}).Needs([]byte(params)); !slices.Equal(got, want) {
			t.Errorf("%s needs %v, want %v", params, got, want)
		}
	}
	if _, err := (ForecastParams{Models: "timesfm-500m"}).kinds(); err == nil || !strings.Contains(err.Error(), "timesfm-200m") {
		t.Errorf("an unknown variant: %v", err)
	}
	// "all" is the models written in Go, not the pretrained ones.
	if kinds, err := (ForecastParams{Models: "all"}).kinds(); err != nil || slices.Contains(kinds, "timesfm-200m") {
		t.Errorf("all is %v, %v", kinds, err)
	}
}

// A node whose owner holds containers to less memory than TimesFM needs
// neither offers it nor lets a job ask for it.
func TestANodeThatCannotGiveTimesFMItsMemoryDoesNotOfferIt(t *testing.T) {
	for _, tt := range []struct {
		memory int
		offers bool
	}{{0, true}, {timesfmMemoryMB, true}, {2 * timesfmMemoryMB, true}, {timesfmMemoryMB - 1, false}, {512, false}} {
		f := Forecast{Containers: true, Confined: Confinement{MemoryMB: tt.memory}}
		offered := slices.Contains(f.Offers(ctx), TimesFMLabel)
		choices := slices.Contains(f.modelChoices(), "timesfm-200m")
		if offered != tt.offers || choices != tt.offers || strings.Contains(f.modelsHelp(), "TimesFM") != tt.offers {
			t.Errorf("held to %d MB: offers %t, lists %t, want %t", tt.memory, offered, choices, tt.offers)
		}
	}
	if got := (Forecast{}).Offers(ctx); slices.Contains(got, TimesFMLabel) {
		t.Errorf("a node that runs no containers offers %v", got)
	}
	store := storage.NewMemory()
	series, _ := store.Put(ctx, strings.NewReader(seasonalSeries(48)))
	small := Forecast{Containers: true, Confined: Confinement{MemoryMB: 1024}}
	if _, err := small.Split(ctx, store, []byte(`{"input":"`+series.String()+`","horizon":4,"models":"timesfm-200m"}`), 1); err == nil || !strings.Contains(err.Error(), "holds a container to 1024 MB, and TimesFM needs 3072") {
		t.Errorf("a job asking a small node for TimesFM: %v", err)
	}
}

func TestTimesFMImageFailuresAreItsCandidatesErrors(t *testing.T) {
	store := storage.NewMemory()
	series, _ := store.Put(ctx, strings.NewReader(seasonalSeries(48)))
	payload := mustJSON(forecastTask{Input: series.String(), Horizon: 4, Folds: 2, Models: []forecastModel{{Kind: "naive"}, {Kind: "timesfm", Variant: "200m"}}})
	killed := &pretend{do: func(args []string, stdout, stderr io.Writer) (int, error) {
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
	var asked timesfmAsked
	outputs := map[string][]byte{}
	for _, tt := range []struct {
		name   string
		engine *pretend
		why    string
	}{
		{"none left", pretendTimesFM(t, &asked, func([]map[string]any) any { return nil }), "left no answers"},
		{"not JSON", pretendTimesFM(t, &asked, func([]map[string]any) any { return json.RawMessage(`"x"`) }), "the answers"},
		{"too few", pretendTimesFM(t, &asked, func([]map[string]any) any { return []any{} }), "answered 0 of 1"},
		{"short of memory", killed, "exited with status 137: Killed"},
		{"not built", failing, "no space left"},
	} {
		var log heard
		raw, err := (Forecast{Containers: true, Engine: tt.engine.engine}).Execute(WithReporter(ctx, &log), store, payload)
		outputs[tt.name] = raw
		var part forecastPart
		json.Unmarshal(raw, &part)
		c := part.Candidates
		if err != nil || len(c) != 2 || c[0].Error != "" || c[0].Forecast == nil {
			t.Errorf("%s: %v %+v, want naive scored", tt.name, err, c)
			continue
		}
		// What went wrong is in the task's log, and the candidate's error
		// says only that it failed, in words every worker uses alike.
		if c[1].Error != timesfmFailed || !strings.Contains(strings.Join(log.lines, "\n"), tt.why) {
			t.Errorf("%s: candidate error %q, log %q, want %q with %q logged", tt.name, c[1].Error, log.lines, timesfmFailed, tt.why)
		}
	}
	// So two honest workers whose images failed in their own ways agree.
	if same, why := (Forecast{}).Compare(outputs["short of memory"], outputs["none left"]); !same {
		t.Errorf("two images failing in their own ways: %q", why)
	}
	// A task given up as a whole fails.
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := (timesfmRunner{Forecast{Containers: true, Engine: killed.engine}}).fit(cancelled, []fitRequest{{Model: forecastModel{Kind: "timesfm", Variant: "200m"}, Horizon: 1, Prefixes: [][]float64{{1, 2}}}}, func(int) {}); !errors.Is(err, context.Canceled) {
		t.Errorf("a cancelled task: %v", err)
	}
	// A request is made only of prefixes of its series.
	values := []float64{1, 2, 3, 4}
	other := []float64{1, 2}
	if _, err := (timesfmRunner{Forecast{Containers: true, Engine: killed.engine}}).fit(ctx, []fitRequest{{Model: forecastModel{Kind: "timesfm", Variant: "200m"}, Horizon: 1, Prefixes: [][]float64{values, other}}}, func(int) {}); err == nil || !strings.Contains(err.Error(), "not the start of its series") {
		t.Errorf("a prefix of another series: %v", err)
	}
}

// The image is made only of parts held to hashes: a base image by digest,
// Python packages by their hashes, and the model at a revision whose files
// are checked against theirs.
func TestTheTimesFMImageIsBuiltFromPinnedParts(t *testing.T) {
	if !regexp.MustCompile(`(?m)^FROM [^ ]+@sha256:[0-9a-f]{64}$`).MatchString(timesfmDockerfile) {
		t.Errorf("the base image is not pinned by digest:\n%s", timesfmDockerfile)
	}
	if !strings.Contains(timesfmDockerfile, "--require-hashes") || !strings.Contains(timesfmDockerfile, "HF_HUB_OFFLINE=1") {
		t.Errorf("the image does not require hashes or keep the model offline:\n%s", timesfmDockerfile)
	}
	hashed := regexp.MustCompile(`^[a-z0-9-]+==[^ ]+( --hash=sha256:[0-9a-f]{64})+$`)
	var names []string
	for _, line := range timesfmRequirements {
		if !hashed.MatchString(line) {
			t.Errorf("a requirement is not held to its hash: %q", line)
		}
		name, _, _ := strings.Cut(line, "==")
		names = append(names, name)
	}
	// Every package TimesFM shares with Chronos-Bolt is there, at
	// Chronos-Bolt's pins.
	for _, need := range timesfmNeeds {
		if !slices.Contains(names, need) {
			t.Errorf("%s is needed but not in the requirements", need)
		}
	}
	if !slices.Contains(names, "timesfm") {
		t.Errorf("timesfm itself is not in the requirements %v", names)
	}
	for _, want := range []string{timesfmPin.repo, timesfmPin.revision, timesfmPin.config, timesfmPin.weights} {
		if !strings.Contains(timesfmFetch, want) {
			t.Errorf("the model fetch does not hold to %q", want)
		}
	}
	if !regexp.MustCompile(`^[0-9a-f]{40}$`).MatchString(timesfmPin.revision) {
		t.Errorf("the model revision %q is not a commit", timesfmPin.revision)
	}
}

func TestTimesFMForecastsARealSeriesInItsImage(t *testing.T) {
	requireRealImages(t)
	store := storage.NewMemory()
	series, _ := store.Put(ctx, strings.NewReader(seasonalSeries(60)))
	f := Forecast{Containers: true}
	payloads, err := f.Split(ctx, store, []byte(`{"input":"`+series.String()+`","horizon":6,"folds":2,"models":"naive,timesfm-200m"}`), 1)
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
	var timesfm *forecastOutcome
	for i, o := range part.Candidates {
		if o.Model.Kind == "timesfm" {
			timesfm = &part.Candidates[i]
		}
	}
	if timesfm == nil || timesfm.Error != "" || !wellFormed(*timesfm, 6, 2) {
		t.Fatalf("TimesFM gave %+v", timesfm)
	}
	// The series is a year of months with a season of 12 and a trend, which
	// TimesFM forecasts closer than within a tenth of its size.
	if timesfm.MAE <= 0 || timesfm.MAE > 0.1*slices.Max(timesfm.Forecast) {
		t.Errorf("TimesFM's backtest error is %g against a forecast of %v", timesfm.MAE, timesfm.Forecast)
	}
}

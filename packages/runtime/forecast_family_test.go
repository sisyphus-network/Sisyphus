package runtime

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/sisyphus-network/Sisyphus/packages/storage"
)

// Every family is named apart from the models written in Go and from every
// other family, and says what its candidates need.
func TestTheFamiliesAreNamedApartAndSayWhatTheyNeed(t *testing.T) {
	seen := map[string]bool{}
	for _, kind := range forecastKinds {
		seen[kind] = true
	}
	for _, fam := range forecastFamilies {
		if fam.kind == "" || seen[fam.kind] || fam.name == "" || fam.label == "" || len(fam.variants) == 0 {
			t.Errorf("family %q is not named apart or is missing a part: %+v", fam.kind, fam)
		}
		seen[fam.kind] = true
		for _, kind := range fam.kinds() {
			if seen[kind] {
				t.Errorf("%s is named twice", kind)
			}
			seen[kind] = true
		}
		if fam.fewest < 1 || fam.memoryMB < 1 || !(fam.tolerance > 0 && fam.tolerance < 1) {
			t.Errorf("family %s: fewest %d, memory %d MB, tolerance %g", fam.kind, fam.fewest, fam.memoryMB, fam.tolerance)
		}
		if run := fam.runner(Forecast{}); run == nil || run.name() == (goRunner{}).name() {
			t.Errorf("family %s is fitted by %v", fam.kind, run)
		}
	}
	// Chronos-Bolt is the one family, named as it was before there were
	// families.
	if len(forecastFamilies) != 1 || forecastFamilies[0].kind != "chronos" {
		t.Fatalf("the families are %+v", forecastFamilies)
	}
	if want := []string{"chronos-tiny", "chronos-mini", "chronos-small", "chronos-base"}; !slices.Equal(familyKinds(), want) {
		t.Errorf("the family kinds are %v, want %v", familyKinds(), want)
	}
	if chronosFamily.label != ChronosLabel || chronosFamily.memoryMB != chronosMemoryMB || chronosFamily.tolerance != chronosTolerance {
		t.Errorf("Chronos-Bolt is described as %+v", chronosFamily)
	}
}

// withFamily adds a pretend family, fitted by run, for the length of the
// test: what forecast and forecasts do with it is all the shared code's,
// since none of it is Chronos-Bolt's.
func withFamily(t *testing.T, run forecastRunner) forecastFamily {
	t.Helper()
	fam := forecastFamily{
		kind:      "pretend",
		name:      "Pretend",
		variants:  []string{"small", "large"},
		fewest:    3,
		memoryMB:  1024,
		tolerance: 1e-2,
		label:     "forecast-pretend:0",
		runner:    func(Forecast) forecastRunner { return run },
	}
	saved := forecastFamilies
	forecastFamilies = append(slices.Clone(saved), fam)
	t.Cleanup(func() { forecastFamilies = saved })
	return fam
}

// A second family is asked for, offered, refused, fitted and compared as
// Chronos-Bolt is, by the same code.
func TestASecondFamilyTakesTheSamePathAsChronos(t *testing.T) {
	var asked []fitRequest
	fam := withFamily(t, recordingRunner{asked: &asked})
	// A job names it by kind and variant, and an unknown variant is none
	// of the kinds.
	if kinds, err := (ForecastParams{Models: "naive,pretend-large"}).kinds(); err != nil || !slices.Equal(kinds, []string{"naive", "pretend-large"}) {
		t.Errorf("kinds %v, %v", kinds, err)
	}
	if _, err := (ForecastParams{Models: "pretend-huge"}).kinds(); err == nil || !strings.Contains(err.Error(), "pretend-small, pretend-large") {
		t.Errorf("an unknown variant: %v", err)
	}
	// Its candidates are models with only a variant, and need its fewest
	// points.
	for _, tt := range []struct {
		m    forecastModel
		want string
	}{
		{forecastModel{Kind: "pretend", Variant: "small"}, ""},
		{forecastModel{Kind: "pretend", Variant: "huge"}, "settings it can take"},
		{forecastModel{Kind: "pretend", Variant: "small", Order: 2}, "settings it can take"},
	} {
		if got := tt.m.invalid(); (tt.want == "") != (got == "") || !strings.Contains(got, tt.want) {
			t.Errorf("%s: %q, want %q", tt.m.Name(), got, tt.want)
		}
	}
	if got := (forecastModel{Kind: "pretend", Variant: "small"}).fewest(); got != 3 {
		t.Errorf("fewest %d", got)
	}
	// A job that asks for it needs its label; a node offers it only where
	// it can give it its memory, and the form offers it only there.
	if got := (Forecast{}).Needs([]byte(`{"models":"naive,pretend-small"}`)); !slices.Equal(got, []string{fam.label}) {
		t.Errorf("needs %v", got)
	}
	if got := (Forecast{}).Needs([]byte(`{"models":"chronos-tiny,pretend-small"}`)); !slices.Equal(got, []string{ChronosLabel, fam.label}) {
		t.Errorf("a job of both families needs %v", got)
	}
	small := Forecast{Containers: true, Confined: Confinement{MemoryMB: 2048}}
	if got := small.Offers(ctx); !slices.Equal(got, []string{fam.label}) {
		t.Errorf("a node that holds containers to 2048 MB offers %v", got)
	}
	if got := (Forecast{Containers: true}).Offers(ctx); !slices.Equal(got, []string{ChronosLabel, fam.label}) {
		t.Errorf("a node that runs containers offers %v", got)
	}
	if choices := small.modelChoices(); !slices.Contains(choices, "pretend-large") || slices.Contains(choices, "chronos-tiny") {
		t.Errorf("choices %v", choices)
	}
	if help := (Forecast{Containers: true}).modelsHelp(); !strings.Contains(help, "The Chronos-Bolt and Pretend models are pretrained") {
		t.Errorf("help %q", help)
	}
	store := storage.NewMemory()
	series, _ := store.Put(ctx, strings.NewReader(seasonalSeries(40)))
	params := []byte(`{"input":"` + series.String() + `","horizon":4,"folds":2,"models":"naive,pretend-small"}`)
	tight := Forecast{Containers: true, Confined: Confinement{MemoryMB: 512}}
	if _, err := tight.Split(ctx, store, params, 1); err == nil || err.Error() != "forecast params: the Pretend models run in containers, and this node holds a container to 512 MB, and Pretend needs 1024" {
		t.Errorf("a node that cannot give it its memory split it: %v", err)
	}
	// Its candidates go to its runner, and the rest are fitted in process.
	f := Forecast{Containers: true}
	payloads, err := f.Split(ctx, store, params, 1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tight.Execute(ctx, store, payloads[0]); err == nil || !strings.Contains(err.Error(), "the task has Pretend candidates") {
		t.Errorf("a node that cannot run it ran it: %v", err)
	}
	out, err := f.Execute(ctx, store, payloads[0])
	if err != nil {
		t.Fatal(err)
	}
	if len(asked) != 1 || asked[0].Model != (forecastModel{Kind: "pretend", Variant: "small"}) {
		t.Fatalf("its runner was asked for %+v", asked)
	}
	var part forecastPart
	if err := json.Unmarshal(out, &part); err != nil {
		t.Fatal(err)
	}
	if part.Scale == 0 || len(part.Candidates) != 2 || part.Candidates[1].Model.Kind != "pretend" || part.Candidates[1].Error != "" {
		t.Fatalf("the task returned %s", out)
	}
	// Two workers' runs of it agree within its tolerance, and not beyond.
	nudged := func(by float64) []byte {
		var p forecastPart
		json.Unmarshal(out, &p)
		p.Candidates[1].Forecast[0] += by * part.Scale
		return mustJSON(p)
	}
	if same, why := f.Compare(out, nudged(5e-3)); !same {
		t.Errorf("within its tolerance: %s", why)
	}
	if same, why := f.Compare(out, nudged(5e-2)); same || !strings.Contains(why, "candidate pretend variant=small: its forecast step 1") || !strings.Contains(why, "more than 0.01 of the size") {
		t.Errorf("beyond its tolerance: %v, %s", same, why)
	}
	// Of many series, every one of its candidates is kept, as Chronos-Bolt's
	// are.
	kept := keptCandidates([]forecastOutcome{
		{Model: forecastModel{Kind: "naive"}, MAE: 2},
		{Model: forecastModel{Kind: "pretend", Variant: "small"}, MAE: 3},
		{Model: forecastModel{Kind: "pretend", Variant: "large"}, MAE: 1},
	})
	if len(kept) != 3 {
		t.Errorf("kept %+v", kept)
	}
}

// Without a family's candidate, a task runs wholly in process, whatever
// the node is.
func TestATaskWithoutAFamilyRunsInProcess(t *testing.T) {
	var asked []fitRequest
	withFamily(t, recordingRunner{asked: &asked})
	run, err := (Forecast{}).runners([]forecastModel{{Kind: "naive"}, {Kind: "ses", Alpha: 0.5}})
	if err != nil {
		t.Fatal(err)
	}
	if got := run(forecastModel{Kind: "naive"}).name(); got != "go" {
		t.Errorf("naive is fitted by %s", got)
	}
	if got := (Forecast{}).Offers(ctx); got != nil {
		t.Errorf("a node without containers offers %v", got)
	}
}

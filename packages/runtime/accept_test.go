package runtime

import (
	"slices"
	"strings"
	"testing"
)

// fakeMining stands in for an opt-in workload, as mining is to be (#776):
// in a group of its own, and taken only when its owner names it.
type fakeMining struct{ Primes }

func (fakeMining) Name() string     { return "fake-mining" }
func (fakeMining) OptIn() bool      { return true }
func (fakeMining) Groups() []string { return []string{"fake-miners"} }

// fakeHeavy is in a group of its own, but not opt-in.
type fakeHeavy struct{ Primes }

func (fakeHeavy) Name() string     { return "fake-heavy" }
func (fakeHeavy) OptIn() bool      { return false }
func (fakeHeavy) Groups() []string { return []string{"fake-miners"} }

func TestTheGroupsAWorkloadIsIn(t *testing.T) {
	for _, tc := range []struct {
		w    Workload
		want []string
	}{
		{Primes{}, []string{GroupDefault}},
		{Container{}, []string{GroupDefault, GroupContainers}},
		{Images{}, []string{GroupDefault, GroupContainers}},
		// Forecasts run images only on a node that runs containers.
		{Forecast{}, []string{GroupDefault}},
		{Forecasts{}, []string{GroupDefault}},
		{Forecast{Containers: true}, []string{GroupDefault, GroupContainers}},
		{Forecasts{Containers: true}, []string{GroupDefault, GroupContainers}},
		{Chat{}, []string{GroupDefault, GroupModels}},
		{Prompts{}, []string{GroupDefault, GroupModels}},
		{Embed{}, []string{GroupDefault, GroupModels}},
		{fakeMining{}, []string{"fake-miners"}},
		{fakeHeavy{}, []string{GroupDefault, "fake-miners"}},
	} {
		if got := GroupsOf(tc.w); !slices.Equal(got, tc.want) {
			t.Errorf("GroupsOf(%s) = %v, want %v", tc.w.Name(), got, tc.want)
		}
	}
}

func TestAnOwnerNamesOnlyWorkloadsAndGroupsThatExist(t *testing.T) {
	known := WithContainers().With(fakeMining{})
	for _, tc := range []struct {
		allow, deny []string
		bad         string
	}{
		{},
		{allow: []string{"primes", "transcode", "chat"}},
		{allow: []string{GroupDefault, "fake-mining"}, deny: []string{GroupContainers, GroupModels}},
		{allow: []string{"fake-miners"}},
		// A group with nothing known in it can still be named.
		{allow: []string{GroupModels}},
		{allow: []string{"primse"}, bad: "primse"},
		{deny: []string{"container", "gpu"}, bad: "gpu"},
		{deny: []string{"minnig"}, bad: "minnig"},
		{allow: []string{"Primes"}, bad: "Primes"},
	} {
		_, err := Accept(known, tc.allow, tc.deny)
		switch {
		case tc.bad == "" && err != nil:
			t.Errorf("allow %v, deny %v: %v", tc.allow, tc.deny, err)
		case tc.bad != "" && (err == nil || !strings.Contains(err.Error(), `"`+tc.bad+`" is neither a workload nor a group`) || !strings.Contains(err.Error(), "containers, default, embed")):
			t.Errorf("allow %v, deny %v: error %v, want %q refused and what is known listed", tc.allow, tc.deny, err, tc.bad)
		}
	}
	// Groups always known, though nothing known is in them.
	if _, err := Accept(NewRegistry(), []string{GroupContainers, GroupModels}, []string{GroupDefault}); err != nil {
		t.Errorf("the fixed groups with nothing known: %v", err)
	}
}

func TestWhatAnOwnerAllowsAndDenies(t *testing.T) {
	known := WithContainers().With(fakeMining{}, fakeHeavy{})
	all := known.Names()
	without := func(names ...string) []string {
		return slices.DeleteFunc(slices.Clone(all), func(n string) bool { return slices.Contains(names, n) })
	}
	for _, tc := range []struct {
		name        string
		allow, deny []string
		want        []string
	}{
		{name: "saying nothing takes all but the opt-in", want: without("fake-mining", "mining")},
		{name: "the default group is the same", allow: []string{GroupDefault}, want: without("fake-mining", "mining")},
		{name: "names alone", allow: []string{"primes", "wordcount"}, want: []string{"primes", "wordcount"}},
		{name: "a group", allow: []string{GroupModels}, want: []string{"chat", "embed", "prompts"}},
		{name: "a name and a group", allow: []string{"primes", GroupContainers}, want: []string{"container", "forecast", "forecasts", "images", "md", "ocr", "prep", "primes", "qchem", "render", "traj", "transcode", "transcribe"}},
		{name: "deny a name", deny: []string{"chat"}, want: without("fake-mining", "mining", "chat")},
		{name: "deny a group", deny: []string{GroupContainers, GroupModels}, want: []string{"fake-heavy", "primes", "vanity", "wordcount"}},
		{name: "deny wins over allowing the same name", allow: []string{"primes", "wordcount"}, deny: []string{"primes"}, want: []string{"wordcount"}},
		{name: "deny of a group wins over allowing a name in it", allow: []string{"transcode", "primes"}, deny: []string{GroupContainers}, want: []string{"primes"}},
		{name: "deny of a name wins over allowing its group", allow: []string{GroupModels}, deny: []string{"embed"}, want: []string{"chat", "prompts"}},
		{name: "deny everything", deny: []string{GroupDefault}, want: nil},
		{name: "an opt-in workload by name", allow: []string{GroupDefault, "fake-mining"}, want: without("mining")},
		// Allowing a group an opt-in workload is in, but not one of its
		// own, does not allow it: mining runs in containers, and allowing
		// containers does not allow mining.
		{name: "an opt-in workload by a group not its own", allow: []string{GroupContainers}, want: []string{"container", "forecast", "forecasts", "images", "md", "ocr", "prep", "qchem", "render", "traj", "transcode", "transcribe"}},
		{name: "an opt-in workload in containers by its own group", allow: []string{GroupMining}, want: []string{"mining"}},
		{name: "an opt-in workload by its group", allow: []string{"fake-miners"}, want: []string{"fake-heavy", "fake-mining"}},
		{name: "an opt-in workload alone", allow: []string{"fake-mining"}, want: []string{"fake-mining"}},
		{name: "an opt-in workload allowed and denied", allow: []string{"fake-mining"}, deny: []string{"fake-miners"}, want: nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, err := Accept(known, tc.allow, tc.deny)
			if err != nil {
				t.Fatal(err)
			}
			if got := known.Accepted(a).Names(); !slices.Equal(got, tc.want) {
				t.Errorf("accepted %v, want %v", got, tc.want)
			}
			for _, name := range all {
				w, _ := known.Get(name)
				if got := a.Accepts(w); got != slices.Contains(tc.want, name) {
					t.Errorf("Accepts(%s) = %v", name, got)
				}
			}
		})
	}
}

// The zero Acceptance is what a worker whose owner said nothing has: it
// changes nothing for the workloads there are, and takes no opt-in one.
func TestTheZeroAcceptanceTakesAllButOptIn(t *testing.T) {
	known := WithContainers().With(fakeMining{})
	want := slices.DeleteFunc(WithContainers().Names(), func(name string) bool { return name == "mining" })
	if got := known.Accepted(Acceptance{}).Names(); !slices.Equal(got, want) {
		t.Errorf("accepted %v, want %v", got, want)
	}
}

// What Accept keeps is its own: the lists it was given may change after.
func TestAnAcceptanceKeepsItsOwnLists(t *testing.T) {
	allow, deny := []string{"primes"}, []string{"wordcount"}
	a, err := Accept(Builtin(), allow, deny)
	if err != nil {
		t.Fatal(err)
	}
	allow[0], deny[0] = "wordcount", "primes"
	if !a.Accepts(Primes{}) || a.Accepts(WordCount{}) {
		t.Error("the acceptance changed with the lists it was made of")
	}
}

// A containers denial keeps every container off a node that runs them,
// forecasts' images too; on one that does not, forecasts run in the daemon
// and are taken.
func TestDenyingContainersKeepsForecastsImagesOff(t *testing.T) {
	a, err := Accept(WithContainers(), nil, []string{GroupContainers})
	if err != nil {
		t.Fatal(err)
	}
	if got := ContainersConfined(Confinement{}).Accepted(a).Names(); !slices.Equal(got, []string{"chat", "embed", "primes", "prompts", "vanity", "wordcount"}) {
		t.Errorf("a node that runs containers takes %v", got)
	}
	if got := Builtin().Accepted(a).Names(); !slices.Equal(got, []string{"forecast", "forecasts", "primes", "vanity", "wordcount"}) {
		t.Errorf("a node that does not takes %v", got)
	}
}

func TestWhatAnOwnerAllowsThatANodeCannotRun(t *testing.T) {
	known := WithContainers().With(fakeMining{})
	for _, tc := range []struct {
		allow []string
		want  []string
	}{
		{nil, nil},
		{[]string{GroupDefault, "primes"}, nil},
		{[]string{"primes", "transcode", GroupContainers, GroupModels, "fake-mining", "forecast"}, []string{"transcode", GroupContainers, GroupModels, "fake-mining"}},
	} {
		a, err := Accept(known, tc.allow, nil)
		if err != nil {
			t.Fatal(err)
		}
		if got := a.Unmatched(Builtin()); !slices.Equal(got, tc.want) {
			t.Errorf("allowing %v on a plain node: unmatched %v, want %v", tc.allow, got, tc.want)
		}
	}
}

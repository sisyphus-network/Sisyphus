package runtime

import (
	"cmp"
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/sisyphus-network/Sisyphus/packages/storage"
)

// The form a workload gives is what its Split accepts: a job made of the
// form's defaults is taken, and one with a value just past a bound, off a
// step, outside the choices or missing where the form says it is needed,
// is refused. Were the two to drift, a form would offer jobs that fail.
func TestEachFormAsksForWhatItsWorkloadAccepts(t *testing.T) {
	ctx := context.Background()
	store := storage.NewMemory()
	stored, _ := store.Put(ctx, strings.NewReader("the boulder and the hill"))
	registry := WithContainers().With(Graph{})
	// Some workloads read a file when a job is split: qchem counts the
	// structures in it, and refuses what is not a file of them; ocr tells
	// a PDF or a picture from its first bytes, and refuses anything else;
	// traj tells a topology and a trajectory from theirs, so each of its
	// fields is given a file of its own.
	water, _ := store.Put(ctx, strings.NewReader("3\nwater\nO 0 0 0.12\nH 0 0.76 -0.47\nH 0 -0.76 -0.47\n"))
	scan, _ := store.Put(ctx, strings.NewReader("%PDF-1.7\n"))
	topology, _ := store.Put(ctx, strings.NewReader("%VERSION  VERSION_STAMP = V0001.000\n"))
	trajectory, _ := store.Put(ctx, strings.NewReader("T\x00\x00\x00CORD"))
	files := map[string]string{"qchem": water.String(), "ocr": scan.String(), "traj.topology": topology.String(), "traj.trajectory": trajectory.String()}

	// Some file fields stand in place of another, which a form's fields
	// cannot say, and their help does: md's Amber system, its topology and
	// coordinates given together, is given in place of its molecule. They
	// are left out of the defaults, and given together in place of the
	// other below.
	instead := map[string]map[string][]string{"md": {"input": {"prmtop", "inpcrd"}}}

	var formed []string
	for _, name := range registry.Names() {
		form := registry.Form(name)
		if form == nil {
			continue
		}
		formed = append(formed, name)
		w, _ := registry.Get(name)
		defaults := map[string]any{}
		alternatives := map[string]bool{}
		for _, fields := range instead[name] {
			for _, field := range fields {
				alternatives[field] = true
			}
		}
		for _, f := range form {
			checkField(t, name, f)
			switch {
			case alternatives[f.Name]:
			case f.Kind == KindFile:
				defaults[f.Name] = cmp.Or(files[name+"."+f.Name], files[name], stored.String())
			case f.Default != nil:
				defaults[f.Name] = f.Default
			}
		}
		split := func(params map[string]any) error {
			_, err := w.Split(ctx, store, mustJSON(params), 2)
			return err
		}
		if err := split(defaults); err != nil {
			t.Errorf("%s refuses its form's defaults %v: %v", name, defaults, err)
		}

		with := func(field string, value any) map[string]any {
			params := map[string]any{}
			for k, v := range defaults {
				params[k] = v
			}
			if value == nil {
				delete(params, field)
			} else {
				params[field] = value
			}
			return params
		}
		refused := func(field string, value any, why string) {
			t.Helper()
			if err := split(with(field, value)); err == nil {
				t.Errorf("%s takes %s %v, %s", name, field, value, why)
			}
		}
		for _, f := range form {
			step := max(f.Step, 1)
			// Zero is how these parameters say "not given", and is taken
			// as the workload's own default, so it is no test of a bound.
			if f.Minimum != nil && *f.Minimum-step != 0 {
				refused(f.Name, *f.Minimum-step, "below its minimum")
			}
			if f.Maximum != nil {
				refused(f.Name, *f.Maximum+step, "above its maximum")
			}
			if f.Step > 1 {
				refused(f.Name, f.Step+1, "off its step")
			}
			if f.Kind == KindChoice {
				refused(f.Name, "none of the choices", "which is not one of its choices")
			}
			if f.Required {
				refused(f.Name, nil, "left out, though the form says it is needed")
			}
		}
		for field, together := range instead[name] {
			found := 0
			for _, f := range form {
				if slices.Contains(together, f.Name) {
					found++
					if f.Kind != KindFile || f.Required {
						t.Errorf("%s.%s, given in place of %s, is not an optional file", name, f.Name, field)
					}
				}
			}
			if found != len(together) {
				t.Errorf("%s's form has %d of %v", name, found, together)
			}
			given := with(field, nil)
			for _, other := range together {
				given[other] = stored.String()
			}
			if err := split(given); err != nil {
				t.Errorf("%s refuses %v in place of %s: %v", name, together, field, err)
			}
			for _, other := range together {
				alone := with(field, nil)
				alone[other] = stored.String()
				if split(alone) == nil {
					t.Errorf("%s takes %s without the rest of %v", name, other, together)
				}
				if split(with(other, stored.String())) == nil {
					t.Errorf("%s takes %s with %s", name, other, field)
				}
			}
		}
	}
	if !slices.Equal(formed, []string{"md", "ocr", "primes", "qchem", "render", "traj", "transcode", "transcribe", "wordcount"}) {
		t.Errorf("the workloads with forms are %v", formed)
	}
	if Builtin().Form("no-such-workload") != nil || registry.Form("graph") != nil {
		t.Error("a workload that is not there, or has no form, has one")
	}
}

// checkField fails the test for a field a form could not be built from.
func checkField(t *testing.T, workload string, f Field) {
	t.Helper()
	if f.Name == "" || f.Label == "" || !strings.HasSuffix(f.Help, ".") {
		t.Errorf("%s has a field that is not named, labelled and explained: %+v", workload, f)
	}
	if !slices.Contains([]Kind{KindFile, KindInteger, KindNumber, KindText, KindChoice, KindToggle}, f.Kind) {
		t.Errorf("%s.%s is of no kind a form knows: %q", workload, f.Name, f.Kind)
	}
	chosen, _ := f.Default.(string)
	if (f.Kind == KindChoice) != (len(f.Choices) > 0) || f.Kind == KindChoice && !slices.Contains(f.Choices, chosen) {
		t.Errorf("%s.%s has choices %v and default %v", workload, f.Name, f.Choices, f.Default)
	}
	if f.Default == nil {
		return
	}
	encoded, _ := json.Marshal(f.Default)
	var number float64
	if json.Unmarshal(encoded, &number) == nil && (f.Minimum != nil && number < *f.Minimum || f.Maximum != nil && number > *f.Maximum) {
		t.Errorf("%s.%s starts at %v, out of its bounds", workload, f.Name, f.Default)
	}
}

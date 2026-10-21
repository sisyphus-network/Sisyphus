package runtime

import (
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

	var formed []string
	for _, name := range registry.Names() {
		form := registry.Form(name)
		if form == nil {
			continue
		}
		formed = append(formed, name)
		w, _ := registry.Get(name)
		defaults := map[string]any{}
		for _, f := range form {
			checkField(t, name, f)
			switch {
			case f.Kind == KindFile:
				defaults[f.Name] = stored.String()
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
	}
	if !slices.Equal(formed, []string{"primes", "transcode", "wordcount"}) {
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

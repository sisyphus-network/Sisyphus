package runtime

import (
	"context"
	"testing"
)

// heard collects what a task reports.
type heard struct {
	progress []float64
	lines    []string
}

func (h *heard) Progress(done float64) { h.progress = append(h.progress, done) }
func (h *heard) Log(line string)       { h.lines = append(h.lines, line) }

func TestWorkloadsReportOnThemselves(t *testing.T) {
	// With nobody listening, reporting is free and harmless.
	Report(context.Background()).Progress(0.5)
	Report(context.Background()).Log("into the void")

	said := new(heard)
	ctx := WithReporter(context.Background(), said)
	if _, err := (Primes{}).Execute(ctx, nil, []byte(`{"from":0,"to":2000000}`)); err != nil {
		t.Fatal(err)
	}
	if len(said.lines) != 1 || said.lines[0] != "counting primes from 2 to 2000000" {
		t.Errorf("primes logged %q", said.lines)
	}
	if len(said.progress) < 2 || said.progress[0] != 0 || said.progress[len(said.progress)-1] >= 1 {
		t.Errorf("primes reported progress %v", said.progress)
	}
	for i := 1; i < len(said.progress); i++ {
		if said.progress[i] < said.progress[i-1] {
			t.Fatalf("primes' progress went backwards: %v", said.progress)
		}
	}
}

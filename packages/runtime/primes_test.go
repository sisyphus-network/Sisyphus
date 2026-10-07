package runtime

import (
	"context"
	"encoding/json"
	"testing"
)

var ctx = context.Background()

func TestCountPrimes(t *testing.T) {
	tests := []struct {
		from, to, want uint64
	}{
		{0, 0, 0},
		{0, 2, 0},
		{0, 3, 1},
		{2, 3, 1},
		{0, 100, 25},
		{97, 98, 1},
		{90, 97, 0},
		{0, 1_000_000, 78_498},
		{1_000_000, 2_000_000, 70_435},
		// Spans several sieve blocks without starting at a block boundary.
		{999_983, 2_000_004, 70_437},
	}
	for _, tt := range tests {
		got, err := countPrimes(context.Background(), tt.from, tt.to)
		if err != nil {
			t.Fatalf("countPrimes(%d, %d): %v", tt.from, tt.to, err)
		}
		if got != tt.want {
			t.Errorf("countPrimes(%d, %d) = %d, want %d", tt.from, tt.to, got, tt.want)
		}
	}
}

func TestCountPrimesStopsWhenCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := countPrimes(ctx, 0, 1_000_000); err == nil {
		t.Fatal("expected an error from a cancelled context")
	}
}

func TestPrimesSplitCoversRangeExactly(t *testing.T) {
	for _, tt := range []struct {
		from, to  uint64
		parts     int
		wantParts int
	}{
		{0, 1000, 7, 7},
		{10, 13, 8, 3},
		{5, 5, 4, 1},
		{0, 1000, 0, 1},
		{0, 1000, 1, 1},
	} {
		params, _ := json.Marshal(PrimesRange{From: tt.from, To: tt.to})
		payloads, err := Primes{}.Split(ctx, nil, params, tt.parts)
		if err != nil {
			t.Fatal(err)
		}
		if len(payloads) != tt.wantParts {
			t.Errorf("Split(%d..%d, %d) gave %d parts, want %d", tt.from, tt.to, tt.parts, len(payloads), tt.wantParts)
		}
		next := tt.from
		for _, payload := range payloads {
			var r PrimesRange
			if err := json.Unmarshal(payload, &r); err != nil {
				t.Fatal(err)
			}
			if r.From != next || r.To < r.From {
				t.Fatalf("Split(%d..%d, %d): part %+v does not continue from %d", tt.from, tt.to, tt.parts, r, next)
			}
			next = r.To
		}
		if next != tt.to {
			t.Errorf("Split(%d..%d, %d) ends at %d", tt.from, tt.to, tt.parts, next)
		}
	}
}

func TestPrimesSplitExecuteAggregate(t *testing.T) {
	w := Primes{}
	params, _ := json.Marshal(PrimesRange{From: 0, To: 1_000_000})
	payloads, err := w.Split(ctx, nil, params, 9)
	if err != nil {
		t.Fatal(err)
	}
	var outputs [][]byte
	for _, payload := range payloads {
		output, err := w.Execute(ctx, nil, payload)
		if err != nil {
			t.Fatal(err)
		}
		outputs = append(outputs, output)
	}
	result, err := w.Aggregate(ctx, nil, outputs)
	if err != nil {
		t.Fatal(err)
	}
	var got PrimesCount
	if err := json.Unmarshal(result, &got); err != nil {
		t.Fatal(err)
	}
	if got.Count != 78_498 {
		t.Errorf("count = %d, want 78498", got.Count)
	}
}

func TestPrimesRejectsBadRanges(t *testing.T) {
	for _, params := range []string{`{"from":10,"to":5}`, `{"from":0,"to":2199023255552}`, `not json`} {
		if _, err := (Primes{}).Split(ctx, nil, []byte(params), 2); err == nil {
			t.Errorf("Split(%s) succeeded, want an error", params)
		}
	}
}

func contextWithCancel() (context.Context, context.CancelFunc) {
	return context.WithCancel(ctx)
}

func TestPrimesReportsBadTaskData(t *testing.T) {
	if _, err := (Primes{}).Execute(ctx, nil, []byte("not json")); err == nil {
		t.Error("Execute accepted a payload that is not JSON")
	}
	if _, err := (Primes{}).Aggregate(ctx, nil, [][]byte{[]byte(`{"count":1}`), []byte("not json")}); err == nil {
		t.Error("Aggregate accepted an output that is not JSON")
	}
	cancelled, cancel := contextWithCancel()
	cancel()
	if _, err := (Primes{}).Execute(cancelled, nil, []byte(`{"from":0,"to":1000}`)); err == nil {
		t.Error("Execute ignored a cancelled context")
	}
}

func TestIsqrtIsExactAcrossThePrimesRange(t *testing.T) {
	for k := uint64(1); k*k <= maxPrimesTo; k += 997 {
		for _, tt := range []struct{ n, want uint64 }{{k*k - 1, k - 1}, {k * k, k}, {k*k + 1, k}} {
			if got := isqrt(tt.n); got != tt.want {
				t.Fatalf("isqrt(%d) = %d, want %d", tt.n, got, tt.want)
			}
		}
	}
	if got := isqrt(maxPrimesTo); got != 1<<20 {
		t.Errorf("isqrt(maxPrimesTo) = %d, want %d", got, 1<<20)
	}
}

func TestRegistry(t *testing.T) {
	r := Builtin()
	if got := r.Names(); len(got) != 2 || got[0] != "primes" || got[1] != "wordcount" {
		t.Errorf("built-in workloads: %v", got)
	}
	if _, err := r.Get("nope"); err == nil {
		t.Error("Get of an unknown workload succeeded")
	}
	if w, err := r.Get("primes"); err != nil || w.Name() != "primes" {
		t.Errorf("Get(primes) = %v, %v", w, err)
	}
}

func TestMustJSONPanicsOnAValueThatCannotBeEncoded(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("mustJSON returned for a channel")
		}
	}()
	mustJSON(make(chan int))
}

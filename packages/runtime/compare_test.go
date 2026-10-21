package runtime

import "testing"

// within is a workload whose outputs are the same result if they are no
// more than a character apart in length: a comparison that is not
// transitive, as no tolerance is.
type within struct{ Primes }

func (within) Varies() bool { return true }

func (within) Compare(a, b []byte) (bool, string) {
	if d := len(a) - len(b); d < -1 || d > 1 {
		return false, "their lengths differ by more than one"
	}
	return true, ""
}

// varies is a workload whose outputs vary and which does not compare them.
type varies struct{ Primes }

func (varies) Varies() bool { return true }

// steady is a workload that says it does not vary.
type steady struct{ Primes }

func (steady) Varies() bool { return false }

func TestOutputsAreComparedByTheirWorkloadOrElseByTheirBytes(t *testing.T) {
	for _, tt := range []struct {
		name     string
		workload Workload
		a, b     string
		same     bool
		why      string
	}{
		{"equal bytes", Primes{}, "7", "7", true, ""},
		{"different bytes", Primes{}, "7", "8", false, ""},
		{"equal bytes, whatever the workload says", within{}, "seven", "seven", true, ""},
		{"the workload says they are the same", within{}, "seven", "eight", true, ""},
		{"the workload says they differ", within{}, "7", "seven", false, "their lengths differ by more than one"},
		{"no workload at all", nil, "7", "8", false, ""},
	} {
		same, why := Compare(tt.workload, []byte(tt.a), []byte(tt.b))
		if same != tt.same || why != tt.why {
			t.Errorf("%s: %v, %q", tt.name, same, why)
		}
	}
	// The fake comparison is as the coordinator must take any to be: a
	// is the same as b and b as c, but a is not the same as c.
	ab, _ := Compare(within{}, []byte("a"), []byte("ab"))
	bc, _ := Compare(within{}, []byte("ab"), []byte("abc"))
	ac, _ := Compare(within{}, []byte("a"), []byte("abc"))
	if !ab || !bc || ac {
		t.Errorf("the comparison: %v %v %v", ab, bc, ac)
	}
}

func TestOnlyWorkWhoseResultsCanBeComparedCanBeChecked(t *testing.T) {
	for _, tt := range []struct {
		workload  Workload
		checkable bool
	}{
		{Primes{}, true},
		{steady{}, true},
		{within{}, true},
		{varies{}, false},
		{Chat{}, false},
		{MD{}, false},
	} {
		if got := Checkable(tt.workload); got != tt.checkable {
			t.Errorf("%T checkable: %v", tt.workload, got)
		}
	}
}

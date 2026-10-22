package jobmodel

import (
	"fmt"
	"reflect"
	"strconv"
	"testing"
)

// withinOne compares outputs that are whole numbers, as the same if they
// are no more than one apart. Like any tolerance it is not transitive: 0
// is the same as 1 and 1 as 2, but 0 is not the same as 2.
func withinOne(a, b []byte) (bool, string) {
	x, errA := strconv.Atoi(string(a))
	y, errB := strconv.Atoi(string(b))
	switch {
	case errA != nil || errB != nil:
		return false, "not a number"
	case x-y > 1 || y-x > 1:
		return false, fmt.Sprintf("%d and %d are more than one apart", x, y)
	}
	return true, ""
}

// returning has workers return these outputs for a task, in turn, and
// reports whether the last made the job ready to be combined.
func returning(j *Job, task *Task, outputs ...string) (allDone bool) {
	for n, output := range outputs {
		node := fmt.Sprintf("node-%d", n)
		allDone = j.ReturnCopy(task, j.StartCopy(task, node, node, now), []byte(output), nil)
	}
	return allDone
}

// outputs lists the outputs of each side, in order.
func outputs(sides [][]Result) (all [][]string) {
	for _, side := range sides {
		var these []string
		for _, r := range side {
			these = append(these, string(r.Output))
		}
		all = append(all, these)
	}
	return all
}

func TestResultsComparedByTheirWorkloadAreDrawnIntoSidesAroundOneResult(t *testing.T) {
	for _, tt := range []struct {
		name     string
		verify   int
		returned []string
		sides    [][]string
		output   string // the task's, if it succeeded
	}{
		// Two results that differ within the tolerance settle the task,
		// and it has the first.
		{"within", 2, []string{"10", "11"}, [][]string{{"10", "11"}}, "10"},
		// One beyond it is outvoted.
		{"beyond", 2, []string{"10", "20", "11"}, [][]string{{"10", "11"}, {"20"}}, "10"},
		// 0 and 2 are not the same, but 1 is the same as each, so the side
		// drawn around it has all three. The task has 1, which agrees
		// with both.
		{"around the middle", 2, []string{"0", "2", "1"}, [][]string{{"1", "0", "2"}}, "1"},
		// Of sides as large, the one drawn around the first heard from.
		{"ties", 3, []string{"3", "0", "4", "1"}, [][]string{{"3", "4"}, {"0", "1"}}, ""},
		// What is the same as most others settles a task, wherever it
		// comes in the order.
		{"most", 3, []string{"4", "6", "5"}, [][]string{{"5", "4", "6"}}, "5"},
		// Once a side is drawn, the next is drawn from what is left.
		{"apart", 3, []string{"0", "10", "11", "1", "x"}, [][]string{{"0", "1"}, {"10", "11"}, {"x"}}, ""},
	} {
		j := verified(1, tt.verify)
		j.CompareBy(withinOne)
		task := j.Tasks[0]
		done := returning(j, task, tt.returned...)
		if got := outputs(task.Sides()); !reflect.DeepEqual(got, tt.sides) {
			t.Errorf("%s: sides %q, want %q", tt.name, got, tt.sides)
		}
		if succeeded := tt.output != ""; done != succeeded || (task.State == Succeeded) != succeeded || string(task.Output) != tt.output {
			t.Errorf("%s: %v %v %q", tt.name, done, task.State, task.Output)
		}
		if tt.output != "" && string(task.Agreed()[0].Output) != tt.output {
			t.Errorf("%s: agreed %v", tt.name, task.Agreed())
		}
	}
}

func TestWhatMakesTwoResultsTheSame(t *testing.T) {
	j := verified(1, 2)
	task := j.Tasks[0]
	one := Result{Output: []byte("1"), Blobs: []string{"cid-1"}}
	two := Result{Output: []byte("2"), Blobs: []string{"cid-1"}}
	five := Result{Output: []byte("5"), Blobs: []string{"cid-1"}}
	elsewhere := Result{Output: []byte("1"), Blobs: []string{"cid-2"}}

	// Byte for byte, by default and when told to go back to it.
	for _, compare := range []Comparer{nil, withinOne} {
		if compare != nil {
			j.CompareBy(compare)
			j.CompareBy(nil)
		}
		if same, why := task.Same(one, one); !same || why != "" {
			t.Errorf("a result and itself: %v %q", same, why)
		}
		if same, why := task.Same(one, two); same || why != "" {
			t.Errorf("results of different bytes: %v %q", same, why)
		}
		if same, why := task.Same(one, elsewhere); same || why != "" {
			t.Errorf("results with different blobs: %v %q", same, why)
		}
	}

	// By the workload, which also says why.
	j.CompareBy(withinOne)
	if same, why := task.Same(one, two); !same || why != "" {
		t.Errorf("results within the tolerance: %v %q", same, why)
	}
	if same, why := task.Same(one, five); same || why != "1 and 5 are more than one apart" {
		t.Errorf("results beyond it: %v %q", same, why)
	}
	// The blobs a result stored are not the workload's to compare.
	if same, why := task.Same(one, elsewhere); same || why != "they stored different blobs" {
		t.Errorf("results with different blobs: %v %q", same, why)
	}
}

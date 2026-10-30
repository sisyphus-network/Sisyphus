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
func withinOne(a, b Result) (bool, string, string) {
	x, errA := strconv.Atoi(string(a.Output))
	y, errB := strconv.Atoi(string(b.Output))
	switch {
	case errA != nil || errB != nil:
		return false, "", "not a number"
	case x-y > 1 || y-x > 1:
		return false, "", fmt.Sprintf("%d and %d are more than one apart", x, y)
	}
	return true, "", ""
}

// tiered compares as withinOne does for results whose processors are
// known and alike, in the tier "tight", and otherwise within ten, in the
// tier "loose". It reads only each result's CPUModel, never its output, to
// pick the tier.
func tiered(a, b Result) (bool, string, string) {
	tier, within := "loose", 10
	if a.CPUModel != "" && a.CPUModel == b.CPUModel {
		tier, within = "tight", 1
	}
	x, errA := strconv.Atoi(string(a.Output))
	y, errB := strconv.Atoi(string(b.Output))
	switch {
	case errA != nil || errB != nil:
		return false, tier, "not a number"
	case x-y > within || y-x > within:
		return false, tier, fmt.Sprintf("%d and %d are more than %d apart", x, y, within)
	}
	return true, tier, ""
}

// returningOn has workers on these processors return these outputs for a
// task, in turn, and reports whether the last made the job ready to be
// combined.
func returningOn(j *Job, task *Task, cpus []string, outputs ...string) (allDone bool) {
	for n, output := range outputs {
		node := fmt.Sprintf("node-%d", n)
		allDone = j.ReturnCopyOn(task, j.StartCopy(task, node, node, now), cpus[n], []byte(output), nil)
	}
	return allDone
}

// A workload whose tolerance depends on where its results were computed is
// told each result's processor, as the coordinator knew it, and the task
// records the tier that settled it.
func TestTheTierThatSettledATaskIsRecorded(t *testing.T) {
	for _, tt := range []struct {
		name     string
		verify   int
		cpus     []string
		returned []string
		output   string // the task's, if it succeeded
		tier     string
	}{
		{"the same processor", 2, []string{"xeon", "xeon"}, []string{"10", "11"}, "10", "tight"},
		{"the same processor, too far apart for it", 2, []string{"xeon", "xeon", "xeon"}, []string{"10", "15", "11"}, "10", "tight"},
		{"different processors", 2, []string{"xeon", "epyc"}, []string{"10", "15"}, "10", "loose"},
		{"one processor unknown", 2, []string{"xeon", ""}, []string{"10", "15"}, "10", "loose"},
		{"both unknown", 2, []string{"", ""}, []string{"10", "15"}, "10", "loose"},
		{"both tiers among three", 3, []string{"xeon", "xeon", "epyc"}, []string{"10", "10", "12"}, "10", "loose+tight"},
		{"not settled", 2, []string{"xeon", "xeon"}, []string{"10", "15"}, "", ""},
	} {
		j := verified(1, tt.verify)
		j.CompareBy(tiered)
		task := j.Tasks[0]
		returningOn(j, task, tt.cpus, tt.returned...)
		if string(task.Output) != tt.output || task.Tier != tt.tier {
			t.Errorf("%s: output %q in tier %q, want %q in %q", tt.name, task.Output, task.Tier, tt.output, tt.tier)
		}
		for i, r := range task.Results {
			if r.CPUModel != tt.cpus[i] {
				t.Errorf("%s: result %d from processor %q, want %q", tt.name, i, r.CPUModel, tt.cpus[i])
			}
		}
	}

	// Results compared byte for byte, or by a workload with one tolerance,
	// are settled in no tier.
	for _, compare := range []Comparer{nil, withinOne} {
		j := verified(1, 2)
		j.CompareBy(compare)
		returningOn(j, j.Tasks[0], []string{"xeon", "xeon"}, "10", "10")
		if task := j.Tasks[0]; task.State != Succeeded || task.Tier != "" {
			t.Errorf("a task settled in no tier: %v %q", task.State, task.Tier)
		}
	}

	// A task settled and then verified after all no longer has a tier.
	j := verified(1, 2)
	j.Tasks[0].Verify = 1
	j.CompareBy(tiered)
	returningOn(j, j.Tasks[0], []string{"xeon"}, "10")
	j.NoteTier(j.Tasks[0], "tight")
	if again := j.Recheck("node-0"); len(again) != 1 || j.Tasks[0].Tier != "" || j.Tasks[0].Output != nil {
		t.Errorf("a task verified after all: %v %q %q", again, j.Tasks[0].Tier, j.Tasks[0].Output)
	}
	if len(j.Unsaved().Tasks) == 0 {
		t.Error("the task is not to be saved")
	}
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

package accounting

import (
	"reflect"
	"testing"
	"time"

	jobmodel "github.com/sisyphus-network/Sisyphus/packages/job-model"
)

var noon = time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)

// attempt is an attempt at a task by a machine, ending as state.
func attempt(n int, node string, state jobmodel.State) jobmodel.Attempt {
	return jobmodel.Attempt{Number: n, NodeID: node + "-id", NodeName: node, State: state}
}

func result(n int, node, output string) jobmodel.Result {
	return jobmodel.Result{Attempt: n, NodeID: node + "-id", NodeName: node, Output: []byte(output)}
}

// settled is a task that succeeded, settled by the machine of its last
// attempt.
func settled(index int, attempts ...jobmodel.Attempt) *jobmodel.Task {
	last := attempts[len(attempts)-1]
	return &jobmodel.Task{Index: index, State: jobmodel.Succeeded, NodeID: last.NodeID, NodeName: last.NodeName, History: attempts}
}

// fourWays is a job with a task accepted on each basis: one two machines
// agreed on, with a third outvoted; one an audit matched; one taken
// unchecked after a failed attempt; and one an audit caught.
func fourWays(id string, finished time.Time) *jobmodel.Job {
	agreed := settled(0, attempt(1, "r620", jobmodel.Succeeded), attempt(2, "laptop", jobmodel.Succeeded), attempt(3, "rig", jobmodel.Succeeded))
	agreed.Results = []jobmodel.Result{result(1, "r620", "42"), result(2, "rig", "41"), result(3, "laptop", "42")}
	agreed.NodeID, agreed.NodeName = "r620-id", "r620"
	audited := settled(1, attempt(1, "laptop", jobmodel.Succeeded))
	audited.Audit = jobmodel.AuditMatched
	unchecked := settled(2, attempt(1, "rig", jobmodel.Failed), attempt(2, "r620", jobmodel.Succeeded))
	caught := settled(3, attempt(1, "rig", jobmodel.Succeeded))
	caught.Audit = jobmodel.AuditCaught
	return &jobmodel.Job{ID: id, Workload: "primes", State: jobmodel.Succeeded, CreatedAt: finished.Add(-time.Minute), FinishedAt: finished,
		Tasks: []*jobmodel.Task{agreed, audited, unchecked, caught}}
}

func TestEachAcceptedTaskSaysOnWhatBasisAndWhose(t *testing.T) {
	got := Events(fourWays("j1", noon))
	want := []Accepted{
		{Job: "j1", Workload: "primes", Task: 0, Basis: Agreement, Workers: []Worker{{"r620-id", "r620"}, {"laptop-id", "laptop"}}, At: noon},
		{Job: "j1", Workload: "primes", Task: 1, Basis: Audit, Workers: []Worker{{"laptop-id", "laptop"}}, At: noon},
		{Job: "j1", Workload: "primes", Task: 2, Basis: Unchecked, Workers: []Worker{{"r620-id", "r620"}}, At: noon},
		{Job: "j1", Workload: "primes", Task: 3, Basis: Caught, At: noon},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Events =\n%+v\nwant\n%+v", got, want)
	}
}

func TestAJobNotOverAndATaskThatFailedAcceptNothing(t *testing.T) {
	running := fourWays("j1", noon)
	running.State = jobmodel.Running
	if got := Events(running); got != nil {
		t.Errorf("a running job has events %+v", got)
	}
	failed := &jobmodel.Job{ID: "j2", State: jobmodel.Failed, Tasks: []*jobmodel.Task{{State: jobmodel.Failed, History: []jobmodel.Attempt{attempt(1, "rig", jobmodel.Failed)}}}}
	if got := Events(failed); got != nil {
		t.Errorf("a failed task was accepted: %+v", got)
	}
}

func TestARangeIncludesItsStartAndNotItsEnd(t *testing.T) {
	r := Range{From: noon, To: noon.Add(time.Hour)}
	for at, want := range map[time.Time]bool{
		noon.Add(-time.Nanosecond): false, noon: true, noon.Add(30 * time.Minute): true, noon.Add(time.Hour): false,
	} {
		if got := r.Contains(at); got != want {
			t.Errorf("Contains(%v) = %v, want %v", at, got, want)
		}
	}
	if !(Range{}).Contains(time.Time{}) || !(Range{}).Contains(noon) {
		t.Error("a range with no bounds leaves something out")
	}
}

func TestATallyCountsEachMachineAndWorkload(t *testing.T) {
	// A second, cancelled job of another workload, which the rig was
	// handed a task of and the laptop then succeeded at, and a job that
	// failed. A third machine renames itself between jobs.
	cancelled := &jobmodel.Job{ID: "j2", Workload: "render", State: jobmodel.Cancelled, CreatedAt: noon, FinishedAt: noon.Add(2 * time.Minute), Tasks: []*jobmodel.Task{
		settled(0, attempt(1, "rig", jobmodel.Failed), attempt(2, "laptop", jobmodel.Succeeded)),
		{Index: 1, State: jobmodel.Cancelled, History: []jobmodel.Attempt{attempt(1, "rig", jobmodel.Cancelled)}},
	}}
	failed := &jobmodel.Job{ID: "j3", Workload: "render", State: jobmodel.Failed, CreatedAt: noon, FinishedAt: noon.Add(3 * time.Minute), Tasks: []*jobmodel.Task{
		{Index: 0, State: jobmodel.Failed, History: []jobmodel.Attempt{{Number: 1}, attempt(2, "rig", jobmodel.Failed)}},
	}}
	got := Summarize(Input{Jobs: []*jobmodel.Job{failed, cancelled, fourWays("j1", noon.Add(time.Minute))}})
	want := Summary{
		Jobs: 3,
		Machines: []Machine{
			{Worker: Worker{"laptop-id", "laptop"}, Jobs: 2, Attempts: 3, Counts: Counts{Agreement: 1, Audit: 1, Unchecked: 1}},
			{Worker: Worker{"r620-id", "r620"}, Jobs: 1, Attempts: 2, Counts: Counts{Agreement: 1, Unchecked: 1}},
			{Worker: Worker{"rig-id", "rig"}, Jobs: 3, Attempts: 6, FailedAttempts: 3, Counts: Counts{Caught: 1}, Outvoted: 1},
		},
		Workloads: []Workload{
			{Name: "primes", Jobs: 1, Succeeded: 1, Tasks: 4, Attempts: 7, FailedAttempts: 1, Counts: Counts{Agreement: 1, Audit: 1, Unchecked: 1, Caught: 1}, Machines: 3, WallTime: time.Minute},
			{Name: "render", Jobs: 2, Failed: 1, Cancelled: 1, Tasks: 3, Attempts: 4, FailedAttempts: 2, Counts: Counts{Unchecked: 1}, Machines: 2, WallTime: 5 * time.Minute},
		},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Summarize =\n%+v\nwant\n%+v", got, want)
	}
	if got.Machines[0].Accepted() != 3 || got.Workloads[0].Accepted() != 3 {
		t.Errorf("accepted: laptop %d, primes %d; want 3 and 3", got.Machines[0].Accepted(), got.Workloads[0].Accepted())
	}
}

func TestATallyKeepsToItsRangeAndToJobsThatAreOver(t *testing.T) {
	early, late := fourWays("early", noon), fourWays("late", noon.Add(48*time.Hour))
	running := fourWays("running", time.Time{})
	running.State = jobmodel.Running
	got := Summarize(Input{Jobs: []*jobmodel.Job{early, late, running}, Range: Range{From: noon.Add(time.Hour)}})
	if got.Jobs != 1 || len(got.Workloads) != 1 || got.Workloads[0].Jobs != 1 || got.Range.From != noon.Add(time.Hour) {
		t.Errorf("Summarize from an hour after noon = %+v; want only the late job", got)
	}
	if empty := Summarize(Input{Jobs: []*jobmodel.Job{early}, Range: Range{To: noon}}); empty.Jobs != 0 || empty.Machines != nil || empty.Workloads != nil {
		t.Errorf("Summarize before noon = %+v; want nothing", empty)
	}
}

func TestAMachineGoesByTheNameItGaveLast(t *testing.T) {
	first := fourWays("j1", noon)
	renamed := fourWays("j2", noon.Add(time.Hour))
	for _, task := range renamed.Tasks {
		if task.NodeID == "rig-id" {
			task.NodeName = "big-rig"
		}
		for i := range task.History {
			if task.History[i].NodeID == "rig-id" {
				task.History[i].NodeName = "big-rig"
			}
		}
		for i := range task.Results {
			if task.Results[i].NodeID == "rig-id" {
				task.Results[i].NodeName = "big-rig"
			}
		}
	}
	// Given in the opposite order: it is when they ended that counts.
	got := Summarize(Input{Jobs: []*jobmodel.Job{renamed, first}})
	for _, m := range got.Machines {
		if m.ID == "rig-id" && m.Name != "big-rig" {
			t.Errorf("the rig is called %q, want big-rig", m.Name)
		}
	}
}

func TestACompositeJobCountsAsAJobAndNotItsTasks(t *testing.T) {
	parent := &jobmodel.Job{ID: "graph", Workload: "pipeline", State: jobmodel.Succeeded, CreatedAt: noon, FinishedAt: noon.Add(time.Hour),
		Tasks: []*jobmodel.Task{settled(0, attempt(1, "coordinator", jobmodel.Succeeded))}}
	got := Summarize(Input{Jobs: []*jobmodel.Job{parent, fourWays("step", noon)}, Composite: func(j *jobmodel.Job) bool { return j.ID == "graph" }})
	want := Workload{Name: "pipeline", Jobs: 1, Succeeded: 1, WallTime: time.Hour}
	if got.Workloads[0] != want {
		t.Errorf("the composite job's workload = %+v, want %+v", got.Workloads[0], want)
	}
	for _, m := range got.Machines {
		if m.Name == "coordinator" {
			t.Errorf("the coordinator is credited as a machine for carrying out steps: %+v", m)
		}
	}
}

func TestAJobThatEndedBeforeItWasSubmittedTakesNoTime(t *testing.T) {
	// A clock set back between the two.
	job := fourWays("j1", noon)
	job.CreatedAt = noon.Add(time.Hour)
	if got := Summarize(Input{Jobs: []*jobmodel.Job{job}}).Workloads[0].WallTime; got != 0 {
		t.Errorf("wall time %v, want 0", got)
	}
}

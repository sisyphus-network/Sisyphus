package jobmodel

import (
	"reflect"
	"testing"
	"time"
)

func indexes(tasks []*Task) []int {
	out := make([]int, 0, len(tasks))
	for _, t := range tasks {
		out = append(out, t.Index)
	}
	return out
}

func TestAJobKnowsWhatHasChangedSinceItWasSaved(t *testing.T) {
	j := New("j", "primes", nil, Distributed, 3, [][]byte{nil, nil, nil}, time.Now())
	j.NoteRead("in")

	// Never saved: all of it is new.
	if u := j.Unsaved(); !u.New || !reflect.DeepEqual(indexes(u.Tasks), []int{0, 1, 2}) || !reflect.DeepEqual(u.Read, []string{"in"}) {
		t.Fatalf("a new job's unsaved changes: %+v", u)
	}
	// Asking does not clear it; saving does.
	if u := j.Unsaved(); len(u.Tasks) != 3 {
		t.Fatalf("asking twice gave %+v the second time", u)
	}
	j.MarkSaved()
	if u := j.Unsaved(); u.New || len(u.Tasks) != 0 || len(u.Read) != 0 {
		t.Fatalf("a job just saved has unsaved changes: %+v", u)
	}

	// Only what was touched since, each once and in order.
	j.Start(j.Tasks[2], "n", "node", time.Now())
	j.Start(j.Tasks[0], "n", "node", time.Now())
	j.Succeed(j.Tasks[2], []byte("out"))
	j.NoteRead("in", "more")
	j.NoteTaskOutput("mid", "mid")
	j.NoteResult("res")
	u := j.Unsaved()
	if !reflect.DeepEqual(indexes(u.Tasks), []int{0, 2}) {
		t.Errorf("changed tasks: %v, want 0 and 2", indexes(u.Tasks))
	}
	if !reflect.DeepEqual(u.Read, []string{"more"}) || !reflect.DeepEqual(u.TaskOutputs, []string{"mid"}) || !reflect.DeepEqual(u.Results, []string{"res"}) {
		t.Errorf("blobs noted since the last save: %+v", u)
	}
}

func TestRequeueDoesNotCountAgainstATask(t *testing.T) {
	const maxAttempts = 2
	j := New("j", "primes", nil, Distributed, 1, [][]byte{nil}, time.Now())
	task := j.Tasks[0]
	// Lost more often than a task may fail.
	for range 5 {
		j.Start(task, "n", "node", time.Now())
		j.Requeue(task, "the coordinator stopped")
		if task.State != Pending || task.Err != "the coordinator stopped" {
			t.Fatalf("a requeued task is %v with error %q", task.State, task.Err)
		}
	}
	if task.Attempt != 5 || task.Failures != 0 || j.Terminal() {
		t.Fatalf("after five lost attempts: attempt %d, failures %d, job %v", task.Attempt, task.Failures, j.State)
	}
	// Real failures still add up to the limit.
	j.Start(task, "n", "node", time.Now())
	j.Fail(task, "boom", maxAttempts, time.Now())
	if task.State != Pending || j.Terminal() {
		t.Fatalf("after one failure of two allowed the task is %v", task.State)
	}
	j.Start(task, "n", "node", time.Now())
	j.Fail(task, "boom", maxAttempts, time.Now())
	if task.State != Failed || j.State != Failed || j.Err != "task 0 failed after 2 attempts: boom" {
		t.Errorf("after two failures: task %v, job %v: %s", task.State, j.State, j.Err)
	}
}

func TestRestoreRebuildsAJobWithNothingUnsaved(t *testing.T) {
	saved := Job{ID: "j", Workload: "wordcount", State: Running, Tasks: []*Task{{ID: TaskID("j", 0), State: Succeeded}}}
	j := Restore(saved, []string{"in", "mid"}, []string{"mid", "kept"}, []string{"kept"})

	if u := j.Unsaved(); u.New || len(u.Tasks) != 0 || len(u.Read)+len(u.TaskOutputs)+len(u.Results) != 0 {
		t.Errorf("a restored job has unsaved changes: %+v", u)
	}
	if got := j.InputBlobs(); !reflect.DeepEqual(got, []string{"in"}) {
		t.Errorf("inputs %v", got)
	}
	if got := j.IntermediateBlobs(); !reflect.DeepEqual(got, []string{"mid"}) {
		t.Errorf("intermediate blobs %v", got)
	}
	if got := j.OutputBlobs(); !reflect.DeepEqual(got, []string{"kept"}) {
		t.Errorf("outputs %v", got)
	}
	// It goes on from there like any other job.
	j.Requeue(j.Tasks[0], "again")
	if u := j.Unsaved(); len(u.Tasks) != 1 {
		t.Errorf("a change to a restored job is not tracked: %+v", u)
	}
	if TaskID("j", 7) != "j/7" {
		t.Errorf("TaskID = %q", TaskID("j", 7))
	}
}

func TestATaskKeepsEveryAttemptAtItAsItStoodWhenTheTaskMovedOn(t *testing.T) {
	j := New("j", "primes", nil, Distributed, 2, [][]byte{nil, nil}, time.Now())
	first, second := j.Tasks[0], j.Tasks[1]
	if len(first.History) != 0 {
		t.Fatalf("a task never handed out has a history: %+v", first.History)
	}
	j.Start(first, "node-a", "alpha", time.Now())
	j.Fail(first, "disk full", 3, time.Now())
	j.Start(first, "node-b", "beta", time.Now())
	j.Requeue(first, "the coordinator stopped")
	j.Start(first, "node-b", "beta", time.Now())
	j.Succeed(first, []byte("done"))
	j.Start(second, "node-a", "alpha", time.Now())
	j.Cancel(time.Now())

	want := []Attempt{
		{TaskIndex: 0, Number: 1, NodeID: "node-a", NodeName: "alpha", State: Pending, Err: "disk full"},
		{TaskIndex: 0, Number: 2, NodeID: "node-b", NodeName: "beta", State: Pending, Err: "the coordinator stopped"},
		{TaskIndex: 0, Number: 3, NodeID: "node-b", NodeName: "beta", State: Succeeded},
	}
	if !reflect.DeepEqual(first.History, want) {
		t.Errorf("the first task's history:\n%+v\nwant:\n%+v", first.History, want)
	}
	if want := []Attempt{{TaskIndex: 1, Number: 1, NodeID: "node-a", NodeName: "alpha", State: Cancelled}}; !reflect.DeepEqual(second.History, want) {
		t.Errorf("the history of a task its job ended without: %+v", second.History)
	}

	// The attempt that fails a task for good is kept as failed.
	k := New("k", "primes", nil, Distributed, 1, [][]byte{nil}, time.Now())
	k.Start(k.Tasks[0], "node-a", "alpha", time.Now())
	k.Fail(k.Tasks[0], "boom", 1, time.Now())
	if h := k.Tasks[0].History; len(h) != 1 || h[0].State != Failed || h[0].Err != "boom" {
		t.Errorf("the history of a task that failed for good: %+v", h)
	}

	// A task restored at its third attempt with only one on record has
	// the gap filled, so that each attempt sits at its own number.
	restored := Restore(Job{ID: "r", State: Running, Tasks: []*Task{{Index: 0, Attempt: 3, State: Running, NodeID: "node-c", History: []Attempt{{Number: 1}}}}}, nil, nil, nil)
	restored.Requeue(restored.Tasks[0], "lost")
	if h := restored.Tasks[0].History; len(h) != 3 || h[1].Number != 2 || h[2].NodeID != "node-c" || h[2].Err != "lost" {
		t.Errorf("the history of a task restored with attempts missing: %+v", h)
	}
}

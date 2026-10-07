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

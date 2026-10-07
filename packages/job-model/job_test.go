package jobmodel

import (
	"testing"
	"time"

	pb "github.com/sisyphus-network/Sisyphus/packages/protocol/sisyphus/v1"
)

var now = time.Unix(1_700_000_000, 0)

func newJob(tasks int) *Job {
	return New("job", "primes", []byte("{}"), Distributed, tasks, make([][]byte, tasks), now)
}

func TestJobSucceedsWhenEveryTaskSucceeds(t *testing.T) {
	j := newJob(2)
	if j.State != Pending || len(j.Assignable()) != 2 {
		t.Fatalf("new job: state %v, %d assignable", j.State, len(j.Assignable()))
	}

	j.Start(j.Tasks[0], "a", "node a")
	j.Start(j.Tasks[1], "b", "node b")
	if j.State != Running || len(j.Assignable()) != 0 {
		t.Fatalf("after start: state %v, %d assignable", j.State, len(j.Assignable()))
	}
	if j.Succeed(j.Tasks[1], []byte("two")) {
		t.Fatal("job reported done with a task still running")
	}
	if !j.Succeed(j.Tasks[0], []byte("one")) {
		t.Fatal("job not reported done after its last task")
	}

	outputs := j.Outputs()
	if string(outputs[0]) != "one" || string(outputs[1]) != "two" {
		t.Errorf("outputs out of task order: %q", outputs)
	}
	j.Finish([]byte("result"), nil, now)
	if j.State != Succeeded || !j.Terminal() || string(j.Result) != "result" {
		t.Errorf("finished job: state %v, result %q", j.State, j.Result)
	}
}

func TestFailedTaskRetriesThenFailsJob(t *testing.T) {
	j := newJob(2)
	task := j.Tasks[0]

	j.Start(task, "a", "node a")
	j.Fail(task, "boom", 2, now)
	if task.State != Pending || task.Attempt != 1 || j.Terminal() {
		t.Fatalf("after first failure: task %v attempt %d, job %v", task.State, task.Attempt, j.State)
	}

	j.Start(task, "b", "node b")
	j.Fail(task, "boom again", 2, now)
	if task.State != Failed || j.State != Failed {
		t.Fatalf("after final failure: task %v, job %v", task.State, j.State)
	}
	if j.Err == "" || j.FinishedAt.IsZero() {
		t.Errorf("failed job is missing its error or finish time: %+v", j)
	}
	if len(j.Assignable()) != 0 {
		t.Error("a failed job still offers tasks for assignment")
	}
}

func TestSucceedClearsEarlierError(t *testing.T) {
	j := newJob(1)
	j.Start(j.Tasks[0], "a", "node a")
	j.Fail(j.Tasks[0], "boom", 3, now)
	j.Start(j.Tasks[0], "b", "node b")
	j.Succeed(j.Tasks[0], nil)
	if j.Tasks[0].Err != "" {
		t.Errorf("succeeded task kept error %q", j.Tasks[0].Err)
	}
}

func TestToProtoMapsStatesAndMode(t *testing.T) {
	j := New("job", "primes", nil, FullWorker, 0, [][]byte{nil}, now)
	j.Start(j.Tasks[0], "a", "node a")
	p := j.ToProto()
	if p.State != pb.JobState_JOB_STATE_RUNNING || p.Tasks[0].State != pb.TaskState_TASK_STATE_RUNNING {
		t.Errorf("states: job %v, task %v", p.State, p.Tasks[0].State)
	}
	if p.Spec.Mode != pb.ScheduleMode_SCHEDULE_MODE_FULL_WORKER {
		t.Errorf("mode = %v", p.Spec.Mode)
	}
	if p.FinishedAt != nil {
		t.Error("unfinished job has a finish time")
	}

	j.Succeed(j.Tasks[0], nil)
	j.Finish(nil, nil, now)
	if p := j.ToProto(); p.State != pb.JobState_JOB_STATE_SUCCEEDED || p.FinishedAt == nil {
		t.Errorf("finished job: state %v, finished at %v", p.State, p.FinishedAt)
	}
}

func TestStateNames(t *testing.T) {
	for state, want := range map[State]string{
		Pending: "pending", Running: "running", Succeeded: "succeeded", Failed: "failed", State(99): "State(99)",
	} {
		if got := state.String(); got != want {
			t.Errorf("State(%d).String() = %q, want %q", int(state), got, want)
		}
	}
}

func TestJobSortsTheBlobsItTouched(t *testing.T) {
	j := newJob(2)
	j.NoteRead("input", "dataset") // opened by the split
	j.NoteRead("input")            // and again by a task
	j.NoteTaskOutput("part-0", "part-1")
	j.NoteRead("part-0", "part-1")     // the aggregation opens what tasks stored
	j.NoteTaskOutput("passed-through") // a task stored something the aggregation re-stores as the result
	j.NoteResult("table", "passed-through")

	if got := j.InputBlobs(); !equal(got, "dataset", "input") {
		t.Errorf("inputs %v", got)
	}
	if got := j.OutputBlobs(); !equal(got, "passed-through", "table") {
		t.Errorf("outputs %v", got)
	}
	if got := j.IntermediateBlobs(); !equal(got, "part-0", "part-1") {
		t.Errorf("intermediates %v", got)
	}

	p := j.ToProto()
	if !equal(p.GetInputBlobs(), "dataset", "input") || !equal(p.GetOutputBlobs(), "passed-through", "table") {
		t.Errorf("snapshot blobs: inputs %v, outputs %v", p.GetInputBlobs(), p.GetOutputBlobs())
	}
}

func equal(got []string, want ...string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

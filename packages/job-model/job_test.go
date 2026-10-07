package jobmodel

import (
	"errors"
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

	j.Start(j.Tasks[0], "a", "node a", time.Now())
	j.Start(j.Tasks[1], "b", "node b", time.Now())
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

	j.Start(task, "a", "node a", time.Now())
	j.Fail(task, "boom", 2, now)
	if task.State != Pending || task.Attempt != 1 || j.Terminal() {
		t.Fatalf("after first failure: task %v attempt %d, job %v", task.State, task.Attempt, j.State)
	}

	j.Start(task, "b", "node b", time.Now())
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
	j.Start(j.Tasks[0], "a", "node a", time.Now())
	j.Fail(j.Tasks[0], "boom", 3, now)
	j.Start(j.Tasks[0], "b", "node b", time.Now())
	j.Succeed(j.Tasks[0], nil)
	if j.Tasks[0].Err != "" {
		t.Errorf("succeeded task kept error %q", j.Tasks[0].Err)
	}
}

func TestToProtoMapsStatesAndMode(t *testing.T) {
	j := New("job", "primes", nil, FullWorker, 0, [][]byte{nil}, now)
	j.Start(j.Tasks[0], "a", "node a", time.Now())
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

func TestCancellingAJobGivesUpWhatIsNotDone(t *testing.T) {
	now := time.Now()
	j := New("j", "primes", nil, Distributed, 3, [][]byte{nil, nil, nil}, now)
	j.Start(j.Tasks[0], "n", "node", now)
	j.Succeed(j.Tasks[0], []byte("done"))
	j.Start(j.Tasks[1], "n", "node", now)
	j.MarkSaved()

	if !j.Cancel(now.Add(time.Minute)) {
		t.Fatal("a running job could not be cancelled")
	}
	if j.State != Cancelled || !j.Terminal() || j.Err != "cancelled" || !j.FinishedAt.Equal(now.Add(time.Minute)) {
		t.Errorf("the cancelled job: %v, %q, finished %v", j.State, j.Err, j.FinishedAt)
	}
	// What was done stays done; what was running or waiting is given up.
	if j.Tasks[0].State != Succeeded || j.Tasks[1].State != Cancelled || j.Tasks[2].State != Cancelled {
		t.Errorf("its tasks: %v %v %v", j.Tasks[0].State, j.Tasks[1].State, j.Tasks[2].State)
	}
	if u := j.Unsaved(); len(u.Tasks) != 2 {
		t.Errorf("%d tasks to save after cancelling, want the two given up", len(u.Tasks))
	}
	if len(j.Assignable()) != 0 {
		t.Error("a cancelled job has tasks to hand out")
	}
	// Once over, a job cannot be cancelled.
	if j.Cancel(now) {
		t.Error("a job was cancelled twice")
	}
	if Cancelled.String() != "cancelled" {
		t.Errorf("Cancelled is written %q", Cancelled)
	}
}

func TestProgressAndEventsAsTheProtocolGivesThem(t *testing.T) {
	now := time.Now()
	j := New("j", "primes", nil, Distributed, 2, [][]byte{nil, nil}, now)
	j.TaskTimeout = 90 * time.Second
	j.Start(j.Tasks[0], "n", "node", now)
	if j.Tasks[0].StartedAt != now {
		t.Errorf("the task started at %v", j.Tasks[0].StartedAt)
	}
	j.Tasks[0].Progress = 0.5
	out := j.ToProto()
	if out.GetProgress() != 0.25 || out.GetTasks()[0].GetProgress() != 0.5 || out.GetSpec().GetTaskTimeoutSeconds() != 90 {
		t.Errorf("progress %v, the first task's %v, limit %ds", out.GetProgress(), out.GetTasks()[0].GetProgress(), out.GetSpec().GetTaskTimeoutSeconds())
	}
	// A new attempt starts from nothing, and a task that succeeds is done.
	j.Fail(j.Tasks[0], "boom", 3, now)
	j.Start(j.Tasks[0], "n", "node", now)
	if j.Tasks[0].Progress != 0 {
		t.Errorf("a second attempt starts at %v", j.Tasks[0].Progress)
	}
	j.Succeed(j.Tasks[0], nil)
	if j.Tasks[0].Progress != 1 {
		t.Errorf("a succeeded task is at %v", j.Tasks[0].Progress)
	}
	// A job that fails gives up the tasks it leaves behind.
	j.Start(j.Tasks[1], "n", "node", now)
	j.Finish(nil, errors.New("no good"), now)
	if j.Tasks[1].State != Cancelled {
		t.Errorf("the task a failed job left behind is %v", j.Tasks[1].State)
	}

	e := Event{Seq: 3, At: now, Kind: "log", Task: 1, Node: "rig", Text: "counting"}.ToProto()
	if e.GetSeq() != 3 || !e.GetAt().AsTime().Equal(now) || e.GetKind() != "log" || e.GetTaskIndex() != 1 || e.GetNodeName() != "rig" || e.GetText() != "counting" {
		t.Errorf("the event as the protocol gives it: %v", e)
	}
}

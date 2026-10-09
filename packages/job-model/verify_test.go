package jobmodel

import (
	"reflect"
	"strings"
	"testing"
	"time"
)

// verified returns a job of so many tasks, each to be returned the same by
// so many workers.
func verified(tasks, by int) *Job {
	j := newJob(tasks)
	j.Check(by, 0, order(tasks))
	return j
}

// order lists the indexes of so many tasks, first to last.
func order(tasks int) (all []int) {
	for i := range tasks {
		all = append(all, i)
	}
	return all
}

// states lists the states of every attempt at a task, in order.
func states(t *Task) (all []State) {
	for _, a := range t.History {
		all = append(all, a.State)
	}
	return all
}

func TestAVerifiedTaskSucceedsOnceEnoughWorkersReturnTheSameResult(t *testing.T) {
	j := verified(2, 2)
	first, second := j.Tasks[0], j.Tasks[1]
	if j.Wanted(first) != 2 || first.MaxResults() != 3 || first.Asked("node-a") || len(first.Sides()) != 0 || first.Agreed() != nil {
		t.Fatalf("a task nobody has yet: wants %d workers, takes %d results, sides %v", j.Wanted(first), first.MaxResults(), first.Sides())
	}

	a := j.StartCopy(first, "node-a", "alpha", now)
	b := j.StartCopy(first, "node-b", "beta", now)
	if a != 1 || b != 2 || j.State != Running || first.State != Running || j.Wanted(first) != 0 {
		t.Fatalf("attempts %d and %d, job %v, task %v wanting %d more", a, b, j.State, first.State, j.Wanted(first))
	}
	if !first.Asked("node-a") || !first.Asked("node-b") || first.Asked("node-c") {
		t.Error("the workers that have a copy are the ones that have been asked")
	}

	// A copy counts for as much as one of the results that must agree, and
	// a task never goes back.
	j.ReportCopy(first, 0.5)
	j.ReportCopy(first, 0.2)
	if first.Progress != 0.25 {
		t.Errorf("with one of two copies half done the task is %v along", first.Progress)
	}

	// The second to be handed out answers first. One result settles nothing.
	if j.ReturnCopy(first, b, []byte("42"), []string{"cid-1"}) {
		t.Error("the job was said to be done after one result")
	}
	if first.State != Running || first.Progress != 0.5 || len(first.Output) != 0 || j.Wanted(first) != 0 || !first.Asked("node-b") {
		t.Errorf("after one result the task is %v, %v along, with output %q", first.State, first.Progress, first.Output)
	}
	j.ReportCopy(first, 0.5)
	if first.Progress != 0.75 {
		t.Errorf("with one result in and the other copy half done the task is %v along", first.Progress)
	}
	if j.ReturnCopy(first, a, []byte("42"), []string{"cid-1"}) {
		t.Error("the job was said to be done with a task not begun")
	}
	if first.State != Succeeded || first.Progress != 1 || string(first.Output) != "42" || first.NodeName != "alpha" || j.Wanted(first) != 0 {
		t.Errorf("after two results the same the task is %v with output %q, by %s", first.State, first.Output, first.NodeName)
	}
	// Results are kept in the order of their attempts, whichever came first.
	if got := first.Agreed(); len(got) != 2 || got[0].NodeID != "node-a" || got[1].NodeID != "node-b" || got[0].Attempt != 1 {
		t.Errorf("the results that settled it: %+v", got)
	}
	if !reflect.DeepEqual(states(first), []State{Succeeded, Succeeded}) {
		t.Errorf("its attempts: %v", states(first))
	}

	j.StartCopy(second, "node-a", "alpha", now)
	j.StartCopy(second, "node-b", "beta", now)
	j.ReturnCopy(second, 1, nil, nil)
	if !j.ReturnCopy(second, 2, nil, nil) {
		t.Error("the job was not said to be done when its last task was settled")
	}
	if got := j.ToProto(); got.GetSpec().GetVerify() != 2 || got.GetProgress() != 1 {
		t.Errorf("as the protocol gives it: verified by %d, %v along", got.GetSpec().GetVerify(), got.GetProgress())
	}
}

func TestResultsAreTheSameOnlyIfTheirOutputAndTheirBlobsAre(t *testing.T) {
	base := Result{Output: []byte("out"), Blobs: []string{"a", "b"}}
	for name, other := range map[string]Result{
		"another output":     {Output: []byte("other"), Blobs: []string{"a", "b"}},
		"another blob":       {Output: []byte("out"), Blobs: []string{"a", "c"}},
		"a blob fewer":       {Output: []byte("out"), Blobs: []string{"a"}},
		"the output a blob":  {Output: nil, Blobs: []string{"out", "a", "b"}},
		"blobs run together": {Output: []byte("out"), Blobs: []string{"ab"}},
	} {
		if base.same(other) || base.Digest() == other.Digest() {
			t.Errorf("%s: taken for the same result", name)
		}
	}
	again := Result{Attempt: 7, NodeID: "elsewhere", Output: []byte("out"), Blobs: []string{"a", "b"}}
	if !base.same(again) || base.Digest() != again.Digest() || len(base.Digest()) != 64 {
		t.Errorf("the same result from another worker: digests %s and %s", base.Digest(), again.Digest())
	}
}

func TestResultsThatDifferAreSettledByAskingMoreWorkers(t *testing.T) {
	j := verified(1, 2)
	task := j.Tasks[0]
	j.StartCopy(task, "node-a", "alpha", now)
	j.StartCopy(task, "node-b", "beta", now)
	j.ReturnCopy(task, 1, []byte("right"), nil)
	j.ReturnCopy(task, 2, []byte("wrong"), nil)

	// Two sides of one, the first heard from first; and one more worker to
	// ask, who cannot be either of those.
	sides := task.Sides()
	if len(sides) != 2 || string(sides[0][0].Output) != "right" || task.State != Pending || j.Wanted(task) != 1 || task.Agreed() != nil {
		t.Fatalf("after two results that differ: sides %+v, task %v wanting %d", sides, task.State, j.Wanted(task))
	}
	if task.Progress != 0.5 || !task.Asked("node-a") || !task.Asked("node-b") {
		t.Errorf("progress %v", task.Progress)
	}
	third := j.StartCopy(task, "node-c", "gamma", now)
	if !j.ReturnCopy(task, third, []byte("right"), nil) {
		t.Fatal("the third result did not settle the task")
	}
	sides = task.Sides()
	if task.State != Succeeded || string(task.Output) != "right" || len(sides) != 2 || len(sides[0]) != 2 || sides[1][0].NodeName != "beta" {
		t.Errorf("settled as %v with output %q, sides %+v", task.State, task.Output, sides)
	}

	// The larger side comes first even if it was heard from last.
	late := verified(1, 3)
	task = late.Tasks[0]
	for i, output := range []string{"wrong", "right", "right"} {
		late.ReturnCopy(task, late.StartCopy(task, string(rune('a'+i)), "", now), []byte(output), nil)
	}
	if sides := task.Sides(); len(sides[0]) != 2 || string(sides[0][0].Output) != "right" || late.Wanted(task) != 1 {
		t.Errorf("sides %+v, wanting %d", sides, late.Wanted(task))
	}
}

func TestAFailedCopyIsNotAResultAndIsTriedAgain(t *testing.T) {
	j := verified(2, 2)
	task := j.Tasks[0]
	j.StartCopy(task, "node-a", "alpha", now)
	j.StartCopy(task, "node-b", "beta", now)
	j.StartCopy(j.Tasks[1], "node-c", "gamma", now)

	j.FailCopy(task, 1, "disk full", 3, now)
	if task.State != Running || task.Failures != 1 || task.Err != "disk full" || len(task.Results) != 0 || j.Wanted(task) != 1 || task.Asked("node-a") {
		t.Fatalf("after one copy failed: %v, %d failures, %d results, wanting %d", task.State, task.Failures, len(task.Results), j.Wanted(task))
	}
	// Lost through nobody's fault: taken back, and not counted.
	j.RequeueCopy(task, 2, "the coordinator stopped")
	if task.State != Pending || task.Failures != 1 || task.Err != "the coordinator stopped" || j.Wanted(task) != 2 {
		t.Errorf("after the other was taken back: %v, %d failures, wanting %d", task.State, task.Failures, j.Wanted(task))
	}
	if got := task.History; got[0].State != Pending || got[0].Err != "disk full" || got[1].State != Pending || got[1].Err != "the coordinator stopped" {
		t.Errorf("its attempts: %+v", got)
	}

	// The third failure is the task's last, and the job's: what is still
	// out, of this task and the other, is given up.
	j.StartCopy(task, "node-a", "alpha", now)
	j.StartCopy(task, "node-b", "beta", now)
	j.FailCopy(task, 3, "disk full", 3, now)
	j.StartCopy(task, "node-a", "alpha", now)
	j.FailCopy(task, 5, "still full", 3, now)
	if j.State != Failed || task.State != Failed || j.Err != "task 0 failed after 3 attempts: still full" || j.Wanted(task) != 0 {
		t.Errorf("job %v, task %v: %s", j.State, task.State, j.Err)
	}
	if !reflect.DeepEqual(states(task), []State{Pending, Pending, Pending, Cancelled, Failed}) || !reflect.DeepEqual(states(j.Tasks[1]), []State{Cancelled}) {
		t.Errorf("attempts at the task that failed: %v, and at the other: %v", states(task), states(j.Tasks[1]))
	}
	if j.Tasks[1].State != Cancelled {
		t.Errorf("the other task is %v", j.Tasks[1].State)
	}
}

func TestATaskItsWorkersCannotAgreeOnFailsItsJob(t *testing.T) {
	j := verified(1, 2)
	task := j.Tasks[0]
	j.StartCopy(task, "node-a", "alpha", now)
	j.StartCopy(task, "node-b", "beta", now)
	j.ReturnCopy(task, 1, []byte("one"), nil)
	j.ReturnCopy(task, 2, []byte("two"), nil)
	j.StartCopy(task, "node-c", "gamma", now)
	j.Dispute(task, "task 0 could not be verified", now.Add(time.Second))
	if j.State != Failed || task.State != Failed || j.Err != "task 0 could not be verified" || task.Err != j.Err || !j.FinishedAt.Equal(now.Add(time.Second)) {
		t.Errorf("job %v, task %v: %s", j.State, task.State, j.Err)
	}
	// Both results are kept, and the copy still out is given up.
	if len(task.Results) != 2 || !reflect.DeepEqual(states(task), []State{Succeeded, Succeeded, Cancelled}) || j.Wanted(task) != 0 || task.Agreed() != nil {
		t.Errorf("results %+v, attempts %v", task.Results, states(task))
	}
}

func TestCancellingAVerifiedJobGivesUpEveryCopyThatIsOut(t *testing.T) {
	j := verified(2, 2)
	task := j.Tasks[0]
	j.StartCopy(task, "node-a", "alpha", now)
	j.StartCopy(task, "node-b", "beta", now)
	j.ReturnCopy(task, 1, []byte("one"), nil)
	j.MarkSaved()
	if !j.Cancel(now) {
		t.Fatal("a running job could not be cancelled")
	}
	if !reflect.DeepEqual(states(task), []State{Succeeded, Cancelled}) || task.State != Cancelled || j.Tasks[1].State != Cancelled || len(j.Tasks[1].History) != 0 {
		t.Errorf("attempts %v, task %v, the task never begun %v", states(task), task.State, j.Tasks[1].State)
	}
	if changed := indexes(j.Unsaved().Tasks); !reflect.DeepEqual(changed, []int{0, 1}) {
		t.Errorf("tasks to be saved after cancelling: %v", changed)
	}
	if got := strings.Join([]string{task.Results[0].NodeName, task.Results[0].Digest()[:4]}, " "); !strings.HasPrefix(got, "alpha ") {
		t.Errorf("the result it had is kept: %s", got)
	}
}

// checks lists how many workers must agree on each of a job's tasks.
func checks(j *Job) (all []int) {
	for _, t := range j.Tasks {
		all = append(all, t.Verify)
	}
	return all
}

func TestAJobCanHaveOnlyAShareOfItsTasksVerified(t *testing.T) {
	for _, tt := range []struct {
		share float64
		want  []int
		kept  float64
	}{
		// Two fifths of five is two, the first two of the order given.
		{0.4, []int{1, 1, 3, 1, 3}, 0.4},
		// A share is rounded up to a whole task, so some task always is.
		{0.01, []int{1, 1, 1, 1, 3}, 0.01},
		{0.41, []int{3, 1, 3, 1, 3}, 0.41},
		// No share, and all of them, are every task.
		{0, []int{3, 3, 3, 3, 3}, 0},
		{1, []int{3, 3, 3, 3, 3}, 0},
	} {
		j := newJob(5)
		j.Check(3, tt.share, []int{4, 2, 0, 1, 3})
		if got := checks(j); !reflect.DeepEqual(got, tt.want) || j.Verify != 3 || j.VerifyShare != tt.kept {
			t.Errorf("a share of %v: tasks held to %v, want %v; the job to %d, a share of %v", tt.share, got, tt.want, j.Verify, j.VerifyShare)
		}
		if spec := j.ToProto(); spec.GetSpec().GetVerifyShare() != tt.kept || spec.GetTasks()[4].GetVerify() != 3 || spec.GetTasks()[1].GetVerify() != uint32(tt.want[1]) {
			t.Errorf("a share of %v as others are told of it: %v", tt.share, spec)
		}
	}

	// A task that was passed over is run once, and what comes back is it.
	j := newJob(2)
	j.Check(2, 0.5, []int{1, 0})
	spared := j.Tasks[0]
	if j.Wanted(spared) != 1 || spared.MaxResults() != 1 || j.Wanted(j.Tasks[1]) != 2 {
		t.Fatalf("a task passed over wants %d workers and takes %d results; the other wants %d", j.Wanted(spared), spared.MaxResults(), j.Wanted(j.Tasks[1]))
	}
	only := j.StartCopy(spared, "node-a", "alpha", now)
	j.ReportCopy(spared, 0.5)
	if spared.Progress != 0.5 || j.Wanted(spared) != 0 {
		t.Errorf("with its one copy half done it is %v along, wanting %d more", spared.Progress, j.Wanted(spared))
	}
	if j.ReturnCopy(spared, only, []byte("42"), nil) || spared.State != Succeeded || string(spared.Output) != "42" || len(spared.Agreed()) != 1 {
		t.Errorf("after its one result the task is %v with %q", spared.State, spared.Output)
	}
}

func TestAWorkerThatIsOutvotedHasWhatItRanUnverifiedVerifiedAfterAll(t *testing.T) {
	// Four tasks, the first of them verified. One worker has returned a
	// result for the second on its own, has the third, and has had nothing
	// to do with the fourth.
	j := newJob(4)
	j.Check(2, 0.25, order(4))
	checked, done, out, apart := j.Tasks[0], j.Tasks[1], j.Tasks[2], j.Tasks[3]
	j.ReturnCopy(done, j.StartCopy(done, "node-a", "alpha", now), []byte("wrong"), nil)
	j.StartCopy(out, "node-a", "alpha", now)
	j.ReportCopy(out, 0.5)
	j.ReturnCopy(apart, j.StartCopy(apart, "node-b", "beta", now), []byte("right"), nil)
	if done.State != Succeeded || j.Outvoted("node-a") {
		t.Fatalf("before anything is known against it: its task is %v, outvoted %v", done.State, j.Outvoted("node-a"))
	}

	// It is outvoted on the first, and from then on is known to have been.
	a := j.StartCopy(checked, "node-a", "alpha", now)
	b := j.StartCopy(checked, "node-b", "beta", now)
	j.ReturnCopy(checked, a, []byte("wrong"), nil)
	j.ReturnCopy(checked, b, []byte("right"), nil)
	if j.Outvoted("node-a") || j.Outvoted("node-b") {
		t.Error("a worker was held to be outvoted by a task that is not settled")
	}
	j.ReturnCopy(checked, j.StartCopy(checked, "node-c", "gamma", now), []byte("right"), nil)
	if !j.Outvoted("node-a") || j.Outvoted("node-b") || j.Outvoted("node-c") {
		t.Fatalf("after the task was settled against it: %v, %v, %v", j.Outvoted("node-a"), j.Outvoted("node-b"), j.Outvoted("node-c"))
	}
	j.MarkSaved()

	// What it returned on its own is one result of two now, and the task
	// it has needs another worker besides. The others are as they were.
	again := j.Recheck("node-a")
	if len(again) != 2 || again[0] != done || again[1] != out || !reflect.DeepEqual(checks(j), []int{2, 2, 2, 1}) {
		t.Fatalf("checked again: %d tasks, held to %v", len(again), checks(j))
	}
	if done.State != Pending || done.Output != nil || done.Progress != 0.5 || j.Wanted(done) != 1 || !done.Asked("node-a") || len(done.Results) != 1 {
		t.Errorf("the task it had settled alone: %v, %v along, wanting %d more, output %q", done.State, done.Progress, j.Wanted(done), done.Output)
	}
	if out.State != Running || out.Progress != 0 || j.Wanted(out) != 1 {
		t.Errorf("the task it has: %v, %v along, wanting %d more", out.State, out.Progress, j.Wanted(out))
	}
	if apart.State != Succeeded || string(apart.Output) != "right" {
		t.Errorf("a task it had no hand in: %v with %q", apart.State, apart.Output)
	}
	if changed := j.Unsaved().Tasks; len(changed) != 2 || changed[0] != done || changed[1] != out {
		t.Errorf("%d tasks are to be saved again", len(changed))
	}
	// There is nothing more to check of it, and nothing at all of a worker
	// that ran nothing unverified.
	if len(j.Recheck("node-a")) != 0 || len(j.Recheck("node-c")) != 0 {
		t.Error("tasks were checked again that had been or had no need")
	}

	// The task goes on as any verified task does, and a worker outvoted on
	// it joins those known to have been.
	if j.ReturnCopy(done, j.StartCopy(done, "node-b", "beta", now), []byte("right"), nil) || done.State != Pending || j.Wanted(done) != 1 {
		t.Errorf("with two results that differ the task is %v, wanting %d more", done.State, j.Wanted(done))
	}
	if j.ReturnCopy(done, j.StartCopy(done, "node-c", "gamma", now), []byte("right"), nil) || done.State != Succeeded || string(done.Output) != "right" || done.NodeName != "beta" {
		t.Errorf("settled, the task is %v with %q, as %s's", done.State, done.Output, done.NodeName)
	}
	last := j.StartCopy(out, "node-b", "beta", now)
	j.ReturnCopy(out, 1, []byte("right"), nil)
	if !j.ReturnCopy(out, last, []byte("right"), nil) {
		t.Error("the job was not said to be done when its last task was settled")
	}

	// A job that is over is left as it ended.
	j.Finish([]byte("sum"), nil, now)
	if len(j.Recheck("node-b")) != 0 || apart.Verify != 1 {
		t.Errorf("a finished job had tasks checked again: held to %v", checks(j))
	}

	// A job loaded again knows who was outvoted from what its tasks hold.
	loaded := Restore(*j, nil, nil, nil)
	if !loaded.Outvoted("node-a") || loaded.Outvoted("node-b") {
		t.Errorf("as loaded: %v, %v", loaded.Outvoted("node-a"), loaded.Outvoted("node-b"))
	}
	// And a job that is not verified has nobody who was.
	plain := newJob(1)
	plain.Start(plain.Tasks[0], "node-a", "alpha", now)
	plain.Succeed(plain.Tasks[0], nil)
	if plain.Outvoted("node-a") || len(plain.Recheck("node-a")) != 0 {
		t.Error("a job that is not verified has a worker that was outvoted")
	}
}

func TestAWorkerThatIsOutvotedIsOnProbationUntilEnoughOfItsResultsAgree(t *testing.T) {
	s := Standing{NodeID: "node-a"}
	// Agreeing owes nothing to begin with, and leaves nothing owed.
	s.Agree()
	if want := (Standing{NodeID: "node-a", Agreed: 1}); s != want {
		t.Errorf("after one result that agreed: %+v", s)
	}
	s.Outvote()
	for range 3 {
		s.Agree()
	}
	if want := (Standing{NodeID: "node-a", Agreed: 4, Outvoted: 1, Probation: Probation - 3}); s != want {
		t.Errorf("outvoted once and agreed three times since: %+v", s)
	}
	// Outvoted again, it starts over.
	s.Outvote()
	if s.Outvoted != 2 || s.Probation != Probation {
		t.Errorf("outvoted again: %+v", s)
	}
	for range Probation + 2 {
		s.Agree()
	}
	if want := (Standing{NodeID: "node-a", Agreed: 4 + Probation + 2, Outvoted: 2}); s != want {
		t.Errorf("once it has agreed as often as asked, and twice more: %+v", s)
	}
}

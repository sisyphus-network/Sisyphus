package main

import (
	"testing"

	pb "github.com/sisyphus-network/Sisyphus/packages/protocol/sisyphus/v1"
	"github.com/sisyphus-network/Sisyphus/packages/runtime"
)

// Whatever made a task fail on a worker may still be wrong there: a model
// server that has stopped, a full disk. So its next attempt goes to another
// worker, even one with fewer slots free, and back to the first only when
// every worker that could run it has had it go wrong.
func TestATaskThatFailedOnAWorkerIsTriedOnAnotherNext(t *testing.T) {
	p := startPool(t, runtime.Builtin())
	crew := p.crew("roomy", "small")
	roomy, small := crew[0], crew[1]
	id := p.submit(primesJob(pb.ScheduleMode_SCHEDULE_MODE_DISTRIBUTED, 1)).GetJobId()

	first := roomy.assignment()
	roomy.fails(first, "the model server is not answering")
	// The roomy worker still has more slots free, and is passed over.
	second := small.assignment()
	if second.GetTaskId() != first.GetTaskId() || second.GetAttempt() != 2 {
		t.Fatalf("the worker it had not failed on was handed %v", second)
	}
	small.fails(second, "out of memory")
	// Now it has gone wrong everywhere, and anywhere will do.
	third := roomy.assignment()
	if third.GetAttempt() != 3 {
		t.Fatalf("with nowhere new to go, the task was handed out as %v", third)
	}
	roomy.returns(third, `{"count":148933}`)
	if done := p.wait(id); done.GetState() != pb.JobState_JOB_STATE_SUCCEEDED {
		t.Errorf("the job: %v %s", done.GetState(), done.GetError())
	}
}

// Another worker that is busy is still another worker: the task waits for
// it rather than go straight back to where it failed.
func TestATaskThatFailedWaitsForAnotherWorkerThatIsBusy(t *testing.T) {
	p := startPool(t, runtime.Builtin())
	busy := p.connectRaw(hello("busy", 1, "primes"))
	busy.welcome()
	other := p.submit(primesJob(pb.ScheduleMode_SCHEDULE_MODE_DISTRIBUTED, 1)).GetJobId()
	holding := busy.assignment()

	failing := p.connectRaw(hello("failing", 1, "primes"))
	failing.welcome()
	id := p.submit(primesJob(pb.ScheduleMode_SCHEDULE_MODE_DISTRIBUTED, 1)).GetJobId()
	first := failing.assignment()
	failing.fails(first, "the model server is not answering")

	// The busy worker finishes what it had, and the waiting task is next.
	busy.returns(holding, `{"count":148933}`)
	second := busy.assignment()
	if second.GetTaskId() != first.GetTaskId() || second.GetAttempt() != 2 {
		t.Fatalf("the worker that came free was handed %v", second)
	}
	busy.returns(second, `{"count":148933}`)
	for _, job := range []string{other, id} {
		if done := p.wait(job); done.GetState() != pb.JobState_JOB_STATE_SUCCEEDED {
			t.Errorf("job %s: %v %s", job, done.GetState(), done.GetError())
		}
	}
	if got := p.job(id).GetTasks()[0]; got.GetNodeName() != "busy" {
		t.Errorf("the task ended on %q", got.GetNodeName())
	}
}

// A task that waits for another worker holds up only itself: the worker it
// went wrong on still gets the job's other tasks, rather than sit idle with
// free slots until every task it lost has gone elsewhere (#541). That is
// what a worker that rejoins a pool after losing its tasks runs into.
func TestATaskThatWaitsForAnotherWorkerDoesNotHoldUpTheRestOfItsJob(t *testing.T) {
	p := startPool(t, runtime.Builtin())
	busy := p.connectRaw(hello("busy", 1, "primes"))
	busy.welcome()
	other := p.submit(primesJob(pb.ScheduleMode_SCHEDULE_MODE_DISTRIBUTED, 1)).GetJobId()
	holding := busy.assignment()

	failing := p.connectRaw(hello("failing", 2, "primes"))
	failing.welcome()
	id := p.submit(primesJob(pb.ScheduleMode_SCHEDULE_MODE_DISTRIBUTED, 3)).GetJobId()
	first, second := failing.assignment(), failing.assignment()
	failing.fails(first, "the model server is not answering")

	// The failed task waits for the busy worker; the job's third task does
	// not wait with it.
	third := failing.assignment()
	if third.GetTaskId() == first.GetTaskId() || third.GetTaskId() == second.GetTaskId() || third.GetAttempt() != 1 {
		t.Fatalf("the worker with a slot free was handed %v, want the job's untouched task", third)
	}
	failing.returns(second, `{"count":1}`)
	failing.returns(third, `{"count":1}`)
	busy.returns(holding, `{"count":148933}`)
	again := busy.assignment()
	if again.GetTaskId() != first.GetTaskId() || again.GetAttempt() != 2 {
		t.Fatalf("the worker that came free was handed %v, want the task that failed", again)
	}
	busy.returns(again, `{"count":1}`)
	for _, job := range []string{other, id} {
		if done := p.wait(job); done.GetState() != pb.JobState_JOB_STATE_SUCCEEDED {
			t.Errorf("job %s: %v %s", job, done.GetState(), done.GetError())
		}
	}
}

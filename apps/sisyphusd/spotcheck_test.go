package main

import (
	"math"
	"slices"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pb "github.com/sisyphus-network/Sisyphus/packages/protocol/sisyphus/v1"
	"github.com/sisyphus-network/Sisyphus/packages/runtime"
)

// These tests are about spot checks: a verified job that asks for only a
// share of its tasks to be verified. Which tasks those are is drawn at
// random, so each test finds out from the job and goes on from there.

// spotChecked is a primes job split into so many tasks, a share of them to
// be returned the same by two workers.
func spotChecked(tasks uint32, share float64) *pb.JobSpec {
	spec := verifiedPrimes(tasks, 2)
	spec.VerifyShare = share
	return spec
}

// checkedTask is the index of the one task of a job that is verified, and
// spared that of the one task that is not, in a job of two.
func checkedTask(t *testing.T, job *pb.Job) (checked, spared int) {
	t.Helper()
	held := []uint32{job.GetTasks()[0].GetVerify(), job.GetTasks()[1].GetVerify()}
	if !slices.Equal(held, []uint32{2, 1}) && !slices.Equal(held, []uint32{1, 2}) {
		t.Fatalf("the job's two tasks are held to %v", held)
	}
	return slices.Index(held, 2), slices.Index(held, 1)
}

func TestAWorkerCaughtByASpotCheckHasWhatItReturnedUnverifiedVerified(t *testing.T) {
	p := startPool(t, runtime.Builtin())
	// The first has room for both tasks, and is given both: whichever is
	// verified goes to the second as well.
	liar := p.connectRaw(hello("liar", 5, "primes"))
	beta := p.connectRaw(hello("beta", 2, "primes"))
	gamma := p.connectRaw(hello("gamma", 1, "primes"))
	for _, w := range []*rawWorker{liar, beta, gamma} {
		w.welcome()
	}
	job := p.submit(spotChecked(2, 0.5))
	id := job.GetJobId()
	checked, spared := checkedTask(t, job)
	if job.GetSpec().GetVerify() != 2 || job.GetSpec().GetVerifyShare() != 0.5 {
		t.Errorf("the job as submitted: %v", job.GetSpec())
	}

	mine := map[string]*pb.TaskAssignment{}
	for range 2 {
		a := liar.assignment()
		mine[a.GetTaskId()] = a
	}
	second := beta.assignment()
	alone := mine[job.GetTasks()[spared].GetTaskId()]
	first := mine[job.GetTasks()[checked].GetTaskId()]
	if second.GetTaskId() != job.GetTasks()[checked].GetTaskId() || alone == nil || first == nil {
		t.Fatalf("task %d is the one verified; the second worker was handed %v and the first %v", checked, second, mine)
	}

	// What is returned for the task that was passed over is taken as it is.
	liar.returns(alone, `{"count":1}`)
	waitFor(t, func() bool { return p.job(id).GetTasks()[spared].GetState() == pb.TaskState_TASK_STATE_SUCCEEDED })

	// On the other it is outvoted.
	liar.returns(first, `{"count":1}`)
	beta.returns(second, `{"count":7}`)
	third := gamma.assignment()
	if third.GetTaskId() != first.GetTaskId() {
		t.Fatalf("the copy handed out to settle the verified task: %v", third)
	}
	gamma.returns(third, `{"count":7}`)

	// So the task that had succeeded on its word has not: it goes to
	// another worker, with what the first returned standing as one result.
	recheck := beta.assignment()
	if recheck.GetTaskId() != alone.GetTaskId() || recheck.GetAttempt() != 2 {
		t.Fatalf("the copy handed out to check the other task: %v", recheck)
	}
	now := p.job(id)
	if task := now.GetTasks()[spared]; now.GetState() != pb.JobState_JOB_STATE_RUNNING || task.GetState() != pb.TaskState_TASK_STATE_RUNNING || task.GetVerify() != 2 || task.GetProgress() != 0.5 {
		t.Errorf("the job is %v and the task being checked again: %v", now.GetState(), task)
	}
	if task := now.GetTasks()[checked]; task.GetState() != pb.TaskState_TASK_STATE_SUCCEEDED || task.GetNodeName() != "beta" {
		t.Errorf("the task that was verified from the start: %v", task)
	}
	beta.returns(recheck, `{"count":7}`)
	last := gamma.assignment()
	if last.GetTaskId() != alone.GetTaskId() {
		t.Fatalf("the copy handed out to settle the other task: %v", last)
	}
	gamma.returns(last, `{"count":7}`)

	done := p.wait(id)
	if done.GetState() != pb.JobState_JOB_STATE_SUCCEEDED || string(done.GetResult()) != `{"count":14}` {
		t.Fatalf("the job %v: %s, %s", done.GetState(), done.GetResult(), done.GetError())
	}
	for _, task := range done.GetTasks() {
		if task.GetNodeName() != "beta" || task.GetVerify() != 2 {
			t.Errorf("a settled task: %v", task)
		}
	}

	events := p.events(id, 0)
	told := story(events)
	if count(events, "task-rechecked") != 1 || count(events, "task-disagreed") != 2 || count(events, "task-succeeded") != 3 || count(events, "task-result") != 6 {
		t.Errorf("the job's events:\n%s", told)
	}
	for _, want := range []string{
		"submitted: primes, in 2 tasks, a share of 0.5 of them, picked at random, to be verified by 2 workers",
		"task-succeeded: liar returned the same result\n",
		"task-succeeded: beta, gamma returned the same result; liar returned another\n",
		"task-rechecked liar: was outvoted elsewhere in the job, so this task is now to be verified by 2 workers",
	} {
		if !strings.Contains(told, want) {
			t.Errorf("the job's events lack %q:\n%s", want, told)
		}
	}
	for _, e := range events {
		if e.GetKind() == "task-rechecked" && int(e.GetTaskIndex()) != spared {
			t.Errorf("task %d was checked again, and task %d was the one passed over", e.GetTaskIndex(), spared)
		}
	}

	// The record says what share was asked, what each task was held to in
	// the end, and who was on which side.
	record := p.recordText(id)
	if !strings.Contains(record, `"verify_share":0.5`) || strings.Count(record, `"verify":2`) != 3 || strings.Count(record, `"agreed":false`) != 2 || strings.Count(record, `"agreed":true`) != 4 {
		t.Errorf("the job's record: %s", record)
	}
}

func TestAWorkerCaughtByASpotCheckIsNotTakenAtItsWordAgainInTheJob(t *testing.T) {
	p := startPool(t, runtime.Builtin())
	// Four workers with a slot each. Of those free, a task goes to the one
	// whose ID sorts first.
	var crew []*rawWorker
	for _, name := range []string{"north", "east", "south", "west"} {
		w := p.connectRaw(hello(name, 1, "primes"))
		w.welcome()
		crew = append(crew, w)
	}
	slices.SortFunc(crew, func(a, b *rawWorker) int { return strings.Compare(a.id, b.id) })
	names := map[*rawWorker]string{}
	for _, node := range p.coord.Nodes() {
		for _, w := range crew {
			if w.id == node.GetNodeId() {
				names[w] = node.GetName()
			}
		}
	}

	job := p.submit(spotChecked(2, 0.5))
	id := job.GetJobId()
	checked, spared := checkedTask(t, job)
	// The tasks are handed out in order: the first to one worker or two,
	// and the second to those after them.
	liar, honest, holder, spare := crew[0], crew[1], crew[2], crew[3]
	if checked == 1 {
		holder, liar, honest = crew[0], crew[1], crew[2]
	}
	wrong, right, held := liar.assignment(), honest.assignment(), holder.assignment()
	if wrong.GetTaskId() != right.GetTaskId() || held.GetTaskId() != job.GetTasks()[spared].GetTaskId() {
		t.Fatalf("task %d is the one verified; handed out were %v, %v and %v", checked, wrong, right, held)
	}

	// One of the two on the verified task is outvoted, while a third
	// worker still has the task that was passed over.
	liar.returns(wrong, `{"count":1}`)
	honest.returns(right, `{"count":7}`)
	settle := spare.assignment()
	spare.returns(settle, `{"count":7}`)
	waitFor(t, func() bool { return p.job(id).GetTasks()[checked].GetState() == pb.TaskState_TASK_STATE_SUCCEEDED })
	if task := p.job(id).GetTasks()[spared]; task.GetVerify() != 1 || count(p.eventsSoFar(id), "task-rechecked") != 0 {
		t.Errorf("a task the outvoted worker had no hand in: %v", task)
	}

	// That worker is lost, and its task goes to the one that was outvoted.
	// It is no longer taken at its word, so the task goes to another too.
	holder.cancel()
	again, with := liar.assignment(), honest.assignment()
	if again.GetTaskId() != held.GetTaskId() || with.GetTaskId() != held.GetTaskId() || again.GetAttempt() != 2 || with.GetAttempt() != 3 {
		t.Fatalf("the task that was passed over was handed out as %v and %v", again, with)
	}
	if task := p.job(id).GetTasks()[spared]; task.GetVerify() != 2 {
		t.Errorf("the task, now with a worker that was outvoted: %v", task)
	}
	liar.returns(again, `{"count":1}`)
	honest.returns(with, `{"count":7}`)
	last := spare.assignment()
	spare.returns(last, `{"count":7}`)

	done := p.wait(id)
	if done.GetState() != pb.JobState_JOB_STATE_SUCCEEDED || string(done.GetResult()) != `{"count":14}` {
		t.Fatalf("the job %v: %s, %s", done.GetState(), done.GetResult(), done.GetError())
	}
	events := p.events(id, 0)
	told := story(events)
	if count(events, "task-rechecked") != 1 || count(events, "task-lost") != 1 || count(events, "task-disagreed") != 2 ||
		!strings.Contains(told, "task-rechecked "+names[liar]+": was outvoted elsewhere in the job") {
		t.Errorf("the job's events, with %s the worker outvoted:\n%s", names[liar], told)
	}
}

func TestAShareOfAJobToVerifyMustMakeSense(t *testing.T) {
	p := startPool(t, runtime.Builtin().With(runtime.Graph{}))
	p.crew("alpha", "beta")
	unverified := primesJob(pb.ScheduleMode_SCHEDULE_MODE_DISTRIBUTED, 2)
	unverified.VerifyShare = 0.5
	for name, tt := range map[string]struct {
		spec *pb.JobSpec
		want string
	}{
		"more than all of it":           {spotChecked(2, 1.5), "verify_share is the share of a job's tasks to verify, from 0 to 1"},
		"less than none":                {spotChecked(2, -0.1), "verify_share is the share of a job's tasks to verify, from 0 to 1"},
		"not a number":                  {spotChecked(2, math.NaN()), "verify_share is the share of a job's tasks to verify, from 0 to 1"},
		"of a job that is not verified": {unverified, "verify_share says how many of a job's tasks to verify, and needs verify to say by how many workers"},
	} {
		if _, err := p.client.SubmitJob(p.ctx, &pb.SubmitJobRequest{Spec: tt.spec}); status.Code(err) != codes.InvalidArgument || !strings.Contains(status.Convert(err).Message(), tt.want) {
			t.Errorf("%s: %v, want it refused with %q", name, err, tt.want)
		}
	}
	if len(p.coord.Jobs()) != 0 {
		t.Errorf("%d jobs were taken in", len(p.coord.Jobs()))
	}

	// All of it is every task, as if no share had been named.
	whole := p.submit(spotChecked(2, 1))
	if whole.GetSpec().GetVerifyShare() != 0 || whole.GetTasks()[0].GetVerify() != 2 || whole.GetTasks()[1].GetVerify() != 2 {
		t.Errorf("a job all of which is to be verified: %v", whole)
	}
	if _, err := p.client.CancelJob(p.ctx, &pb.CancelJobRequest{JobId: whole.GetJobId()}); err != nil {
		t.Fatal(err)
	}
}

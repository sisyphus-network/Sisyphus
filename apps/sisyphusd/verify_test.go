package main

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/sisyphus-network/Sisyphus/apps/sisyphusd/access"
	pb "github.com/sisyphus-network/Sisyphus/packages/protocol/sisyphus/v1"
	"github.com/sisyphus-network/Sisyphus/packages/runtime"
)

// These tests are about verification by replication: a job that asks for
// it has each task run by several workers and takes only a result enough
// of them agree on. Most drive the workers by hand, to decide what each
// returns and when.

// verifiedPrimes is a primes job split into so many tasks, each to be
// returned the same by so many workers.
func verifiedPrimes(tasks, by uint32) *pb.JobSpec {
	spec := primesJob(pb.ScheduleMode_SCHEDULE_MODE_DISTRIBUTED, tasks)
	spec.Verify = by
	return spec
}

// crew connects hand-driven workers with the given names. Each has a slot
// fewer than the one before, and the coordinator gives work to whoever has
// most slots free, so a task that several of them could be given goes to
// them in this order.
func (p *pool) crew(names ...string) []*rawWorker {
	p.t.Helper()
	var crew []*rawWorker
	for i, name := range names {
		w := p.connectRaw(hello(name, uint32(len(names)-i), "primes"))
		w.welcome()
		crew = append(crew, w)
	}
	return crew
}

// returns answers a task with an output, and the blobs it says it stored.
func (w *rawWorker) returns(a *pb.TaskAssignment, output string, stored ...string) {
	w.t.Helper()
	w.reply(&pb.TaskResult{TaskId: a.GetTaskId(), Attempt: a.GetAttempt(), Outcome: &pb.TaskResult_Output{Output: []byte(output)}, WrittenBlobs: stored})
}

// fails answers a task with an error.
func (w *rawWorker) fails(a *pb.TaskAssignment, why string) {
	w.t.Helper()
	w.reply(&pb.TaskResult{TaskId: a.GetTaskId(), Attempt: a.GetAttempt(), Outcome: &pb.TaskResult_Error{Error: why}})
}

// toldToStop reads the coordinator's next message to this worker, which
// must tell it to stop a task.
func (w *rawWorker) toldToStop() *pb.TaskCancel {
	w.t.Helper()
	msg, err := w.stream.Recv()
	if err != nil || msg.GetCancel() == nil {
		w.t.Fatalf("expected to be told to stop a task, got %v, %v", msg, err)
	}
	return msg.GetCancel()
}

// story is a job's events as lines, each with its kind, the worker it is
// about and what it says.
func story(events []*pb.JobEvent) string {
	var lines []string
	for _, e := range events {
		lines = append(lines, strings.TrimSpace(e.GetKind()+" "+e.GetNodeName())+": "+e.GetText())
	}
	return strings.Join(lines, "\n")
}

// recordText is a finished job's record as JSON, every node of it, having
// checked that the job as the coordinator holds it still gives that record.
func (p *pool) recordText(jobID string) string {
	p.t.Helper()
	record := p.recordOf(jobID)
	if !record.GetMatches() {
		p.t.Errorf("job %s does not give its record again", jobID)
	}
	var text strings.Builder
	for _, node := range record.GetNodes() {
		text.WriteString(node.GetJson())
	}
	return text.String()
}

func TestAVerifiedTaskIsRunByDifferentWorkersAndSucceedsWhenTheyAgree(t *testing.T) {
	p := startPool(t, runtime.Builtin())
	crew := p.crew("alpha", "beta", "gamma")
	alpha, beta := crew[0], crew[1]
	job := p.submit(verifiedPrimes(1, 2))
	id := job.GetJobId()
	if job.GetSpec().GetVerify() != 2 {
		t.Errorf("the job says it is verified by %d", job.GetSpec().GetVerify())
	}

	// The one task goes to two workers at once, each a different attempt.
	first, second := alpha.assignment(), beta.assignment()
	if first.GetTaskId() != second.GetTaskId() || first.GetAttempt() != 1 || second.GetAttempt() != 2 || !bytes.Equal(first.GetPayload(), second.GetPayload()) {
		t.Fatalf("the two copies: %v and %v", first, second)
	}

	// What one copy says of itself is not taken for the other's: news of
	// an attempt a worker does not have is ignored, and its own counts for
	// its share of the task.
	alpha.send(&pb.WorkerMessage{Kind: &pb.WorkerMessage_TaskUpdate{TaskUpdate: &pb.TaskUpdate{TaskId: first.GetTaskId(), Attempt: second.GetAttempt(), Progress: 1}}})
	alpha.send(&pb.WorkerMessage{Kind: &pb.WorkerMessage_TaskUpdate{TaskUpdate: &pb.TaskUpdate{TaskId: first.GetTaskId(), Attempt: first.GetAttempt(), Progress: 0.5, Log: []string{"halfway"}}}})
	waitFor(t, func() bool { return strings.Contains(story(p.eventsSoFar(id)), "log alpha: halfway") })
	if task := p.job(id).GetTasks()[0]; task.GetProgress() != 0.25 {
		t.Errorf("with one of two copies half done the task is %v along", task.GetProgress())
	}

	// One result settles nothing.
	blobA, blobB := p.upload("blob-a"), p.upload("blob-b")
	alpha.returns(first, `{"count":7}`, blobB, blobA)
	waitFor(t, func() bool { return p.job(id).GetTasks()[0].GetProgress() == 0.5 })
	if got := p.job(id); got.GetState() != pb.JobState_JOB_STATE_RUNNING || got.GetTasks()[0].GetState() != pb.TaskState_TASK_STATE_RUNNING {
		t.Errorf("after one result the job is %v and its task %v", got.GetState(), got.GetTasks()[0].GetState())
	}
	// The same output and the same blobs, in whatever order they are named.
	beta.returns(second, `{"count":7}`, blobA, blobB, blobA)
	done := p.wait(id)
	if done.GetState() != pb.JobState_JOB_STATE_SUCCEEDED || string(done.GetResult()) != `{"count":7}` {
		t.Fatalf("the job %v: %s, %s", done.GetState(), done.GetResult(), done.GetError())
	}
	if task := done.GetTasks()[0]; task.GetNodeName() != "alpha" || task.GetAttempt() != 2 || task.GetProgress() != 1 {
		t.Errorf("the settled task: %v", task)
	}

	// The third worker was never asked, and the events say who agreed.
	events := p.events(id, 0)
	told := story(events)
	if count(events, "task-started") != 2 || count(events, "task-result") != 2 || count(events, "task-succeeded") != 1 || count(events, "task-disagreed") != 0 {
		t.Errorf("the job's events:\n%s", told)
	}
	for _, want := range []string{
		"submitted: primes, in 1 tasks, each to be verified by 2 workers",
		"task-started alpha: attempt 1", "task-started beta: attempt 2",
		"task-result alpha: result ", "task-result beta: result ",
		"task-succeeded: alpha, beta returned the same result\n",
	} {
		if !strings.Contains(told, want) {
			t.Errorf("the job's events lack %q:\n%s", want, told)
		}
	}

	// Its record says what was asked and who agreed, and is still the
	// record the job gives.
	record := p.recordText(id)
	if !strings.Contains(record, `"verify":2`) || strings.Count(record, `"agreed":true`) != 2 || strings.Contains(record, `"agreed":false`) {
		t.Errorf("the job's record: %s", record)
	}
	if !strings.Contains(record, `"node":"`+alpha.id+`"`) || !strings.Contains(record, `"node":"`+beta.id+`"`) {
		t.Errorf("the record does not name the workers that agreed: %s", record)
	}
}

func TestAWorkerThatReturnsAWrongResultIsOutvotedAndNamed(t *testing.T) {
	p := startPool(t, runtime.Builtin())
	crew := p.crew("liar", "beta", "gamma")
	liar, beta, gamma := crew[0], crew[1], crew[2]
	id := p.submit(verifiedPrimes(1, 2)).GetJobId()
	first, second := liar.assignment(), beta.assignment()
	liar.returns(first, `{"count":1}`)
	beta.returns(second, `{"count":7}`)

	// They differ, so one more worker is asked, and it cannot be either.
	third := gamma.assignment()
	if third.GetAttempt() != 3 || third.GetTaskId() != first.GetTaskId() {
		t.Fatalf("the copy handed out to settle it: %v", third)
	}
	if got := p.job(id); got.GetState() != pb.JobState_JOB_STATE_RUNNING {
		t.Errorf("while its workers disagree the job is %v", got.GetState())
	}
	gamma.returns(third, `{"count":7}`)
	done := p.wait(id)
	if done.GetState() != pb.JobState_JOB_STATE_SUCCEEDED || string(done.GetResult()) != `{"count":7}` {
		t.Fatalf("the job %v: %s, %s", done.GetState(), done.GetResult(), done.GetError())
	}
	// The task is the first worker's that returned what was agreed on.
	if task := done.GetTasks()[0]; task.GetNodeName() != "beta" || task.GetAttempt() != 3 {
		t.Errorf("the settled task: %v", task)
	}

	events := p.events(id, 0)
	told := story(events)
	if count(events, "task-disagreed") != 1 || count(events, "task-succeeded") != 1 || count(events, "task-failed") != 0 {
		t.Errorf("the job's events:\n%s", told)
	}
	var disagreed string
	for _, e := range events {
		if e.GetKind() == "task-disagreed" {
			disagreed = e.GetText()
		}
	}
	// Each side by its workers and by a digest that tells the results apart.
	sides := strings.Split(disagreed, "; ")
	if len(sides) != 2 || !strings.HasPrefix(sides[0], "liar returned ") || !strings.HasPrefix(sides[1], "beta returned ") ||
		strings.TrimPrefix(sides[0], "liar returned ") == strings.TrimPrefix(sides[1], "beta returned ") {
		t.Errorf("the disagreement as told: %q", disagreed)
	}
	if !strings.Contains(told, "task-succeeded: beta, gamma returned the same result; liar returned another") {
		t.Errorf("the job's events:\n%s", told)
	}
	if !strings.Contains(p.logs.String(), "workers returned different results for a task") {
		t.Error("the coordinator logged nothing of the disagreement")
	}

	record := p.recordText(id)
	if strings.Count(record, `"agreed":true`) != 2 || strings.Count(record, `"agreed":false`) != 1 {
		t.Errorf("the job's record: %s", record)
	}
}

func TestAVerifiedJobFailsWhenItsWorkersCannotAgree(t *testing.T) {
	const explained = "Verification is for work that gives the same result every time it is run: either this workload does not, or a worker returned a wrong result"

	// Three workers and three answers: the same output is not the same
	// result if the blobs stored with it differ.
	p := startPool(t, runtime.Builtin())
	crew := p.crew("alpha", "beta", "gamma")
	id := p.submit(verifiedPrimes(1, 2)).GetJobId()
	first, second := crew[0].assignment(), crew[1].assignment()
	crew[0].returns(first, `{"count":7}`, p.upload("blob-a"))
	crew[1].returns(second, `{"count":7}`, p.upload("blob-b"))
	crew[2].returns(crew[2].assignment(), `{"count":7}`)
	failed := p.wait(id)
	if failed.GetState() != pb.JobState_JOB_STATE_FAILED || failed.GetTasks()[0].GetState() != pb.TaskState_TASK_STATE_FAILED {
		t.Fatalf("the job %v, its task %v", failed.GetState(), failed.GetTasks()[0].GetState())
	}
	for _, want := range []string{
		"task 0 could not be verified: its workers returned different results, no 2 of them the same, after 3 results (",
		"alpha returned ", "; beta returned ", "; gamma returned ", explained,
	} {
		if !strings.Contains(failed.GetError(), want) {
			t.Errorf("the job's error lacks %q: %s", want, failed.GetError())
		}
	}
	events := p.events(id, 0)
	if count(events, "task-disagreed") != 2 || count(events, "task-succeeded") != 0 || events[len(events)-1].GetKind() != "failed" || events[len(events)-1].GetText() != failed.GetError() {
		t.Errorf("the job's events:\n%s", story(events))
	}
	if record := p.recordText(id); strings.Count(record, `"agreed":false`) != 3 || strings.Contains(record, `"agreed":true`) {
		t.Errorf("the job's record: %s", record)
	}

	// Two workers that differ, and nobody else to ask.
	p = startPool(t, runtime.Builtin())
	crew = p.crew("alpha", "beta")
	id = p.submit(verifiedPrimes(1, 2)).GetJobId()
	first, second = crew[0].assignment(), crew[1].assignment()
	crew[0].returns(first, `{"count":7}`)
	crew[1].returns(second, `{"count":8}`)
	failed = p.wait(id)
	if failed.GetState() != pb.JobState_JOB_STATE_FAILED || !strings.Contains(failed.GetError(), explained) ||
		!strings.Contains(failed.GetError(), "no 2 of them the same, and no other connected worker can be asked (alpha returned ") {
		t.Errorf("the job %v: %s", failed.GetState(), failed.GetError())
	}

	// Three must agree, three differ, and the one worker left is already
	// at work on it: it is told to stop, since it could not settle it.
	p = startPool(t, runtime.Builtin())
	crew = p.crew("alpha", "beta", "gamma", "delta")
	id = p.submit(verifiedPrimes(1, 3)).GetJobId()
	copies := []*pb.TaskAssignment{crew[0].assignment(), crew[1].assignment(), crew[2].assignment()}
	crew[0].returns(copies[0], `{"count":1}`)
	crew[1].returns(copies[1], `{"count":2}`)
	fourth := crew[3].assignment()
	crew[2].returns(copies[2], `{"count":3}`)
	failed = p.wait(id)
	if failed.GetState() != pb.JobState_JOB_STATE_FAILED || !strings.Contains(failed.GetError(), "no 3 of them the same, and no other connected worker can be asked") {
		t.Errorf("the job %v: %s", failed.GetState(), failed.GetError())
	}
	if stop := crew[3].toldToStop(); stop.GetTaskId() != fourth.GetTaskId() || stop.GetAttempt() != 4 {
		t.Errorf("the worker still at it was told to stop %v", stop)
	}
}

func TestAnAttemptThatFailsOrIsLostIsNotADisagreement(t *testing.T) {
	p := startPool(t, runtime.Builtin())
	crew := p.crew("alpha", "beta")
	alpha, beta := crew[0], crew[1]
	id := p.submit(verifiedPrimes(1, 2)).GetJobId()
	first := alpha.assignment()
	beta.assignment()

	// A worker whose attempt failed has returned no result, and may be
	// given the task again.
	alpha.fails(first, "out of memory")
	again := alpha.assignment()
	if again.GetAttempt() != 3 || again.GetTaskId() != first.GetTaskId() {
		t.Fatalf("the copy handed out after one failed: %v", again)
	}
	// One whose worker is lost goes to another, when there is one. Until
	// then the task waits, with the result it has.
	beta.cancel()
	waitFor(t, func() bool {
		return strings.Contains(story(p.eventsSoFar(id)), "task-lost beta: its worker disconnected")
	})
	alpha.returns(again, `{"count":7}`)
	waitFor(t, func() bool { return p.job(id).GetTasks()[0].GetState() == pb.TaskState_TASK_STATE_PENDING })
	if got := p.job(id); got.GetState() != pb.JobState_JOB_STATE_RUNNING || got.GetTasks()[0].GetProgress() != 0.5 {
		t.Errorf("with one result in and nobody else to ask the job is %v, its task %v along", got.GetState(), got.GetTasks()[0].GetProgress())
	}
	gamma := p.connectRaw(hello("gamma", 1, "primes"))
	gamma.welcome()
	last := gamma.assignment()
	if last.GetAttempt() != 4 {
		t.Errorf("the copy handed to the worker that joined is attempt %d", last.GetAttempt())
	}
	gamma.returns(last, `{"count":7}`)
	done := p.wait(id)
	if done.GetState() != pb.JobState_JOB_STATE_SUCCEEDED || string(done.GetResult()) != `{"count":7}` {
		t.Fatalf("the job %v: %s, %s", done.GetState(), done.GetResult(), done.GetError())
	}
	events := p.events(id, 0)
	if count(events, "task-disagreed") != 0 || count(events, "task-failed") != 1 || count(events, "task-lost") != 1 || count(events, "task-result") != 2 {
		t.Errorf("the job's events:\n%s", story(events))
	}

	// Failed attempts count against the task as they always have: the
	// third fails it and the job, and the copy still out is stopped.
	id = p.submit(verifiedPrimes(1, 2)).GetJobId()
	held := gamma.assignment()
	for attempt := range 3 {
		got := alpha.assignment()
		if attempt > 0 && got.GetAttempt() != uint32(attempt+2) {
			t.Errorf("after %d failures the copy handed out is attempt %d", attempt, got.GetAttempt())
		}
		alpha.fails(got, "out of memory")
	}
	failed := p.wait(id)
	if failed.GetState() != pb.JobState_JOB_STATE_FAILED || failed.GetError() != "task 0 failed after 3 attempts: out of memory" {
		t.Errorf("the job %v: %s", failed.GetState(), failed.GetError())
	}
	if stop := gamma.toldToStop(); stop.GetTaskId() != held.GetTaskId() || stop.GetAttempt() != held.GetAttempt() {
		t.Errorf("the worker still at it was told to stop %v, and had %v", stop, held)
	}
}

func TestCancellingAVerifiedJobStopsEveryCopy(t *testing.T) {
	p := startPool(t, runtime.Builtin())
	crew := p.crew("alpha", "beta")
	id := p.submit(verifiedPrimes(1, 2)).GetJobId()
	first, second := crew[0].assignment(), crew[1].assignment()
	if _, err := p.client.CancelJob(p.ctx, &pb.CancelJobRequest{JobId: id}); err != nil {
		t.Fatal(err)
	}
	// Each worker is told to stop the attempt it was given.
	for i, a := range []*pb.TaskAssignment{first, second} {
		if stop := crew[i].toldToStop(); stop.GetTaskId() != a.GetTaskId() || stop.GetAttempt() != a.GetAttempt() {
			t.Errorf("worker %d was told to stop %v, and had %v", i, stop, a)
		}
	}
	// What a copy returns after that changes nothing.
	crew[0].returns(first, `{"count":7}`)
	waitFor(t, func() bool {
		return strings.Contains(p.logs.String(), "dropping result for a task this worker is not running")
	})
	cancelled := p.wait(id)
	if cancelled.GetState() != pb.JobState_JOB_STATE_CANCELLED || cancelled.GetTasks()[0].GetState() != pb.TaskState_TASK_STATE_CANCELLED {
		t.Errorf("the job %v, its task %v", cancelled.GetState(), cancelled.GetTasks()[0].GetState())
	}
	if record := p.recordText(id); strings.Count(record, `"state":"cancelled"`) != 4 || strings.Contains(record, `"results"`) {
		t.Errorf("the cancelled job's record: %s", record)
	}
}

func TestACopyThatRunsTooLongIsStoppedAndTheResultsInAreKept(t *testing.T) {
	p := startPool(t, runtime.Builtin())
	crew := p.crew("alpha", "beta", "gamma")
	alpha, beta := crew[0], crew[1]
	spec := verifiedPrimes(1, 2)
	spec.TaskTimeoutSeconds = 3600
	id := p.submit(spec).GetJobId()
	first, second := alpha.assignment(), beta.assignment()
	alpha.returns(first, `{"count":7}`)
	waitFor(t, func() bool { return p.job(id).GetTasks()[0].GetProgress() == 0.5 })

	// Within the hour nothing happens; past it the copy still out is
	// stopped and handed out again, to a worker that has not answered.
	p.coord.Expire(time.Now().Add(30 * time.Minute))
	p.coord.Expire(time.Now().Add(2 * time.Hour))
	if stop := beta.toldToStop(); stop.GetTaskId() != second.GetTaskId() || stop.GetAttempt() != 2 {
		t.Errorf("the worker that ran too long was told to stop %v", stop)
	}
	again := beta.assignment()
	if again.GetAttempt() != 3 {
		t.Errorf("the copy handed out after one ran too long is attempt %d", again.GetAttempt())
	}
	beta.returns(again, `{"count":7}`)
	done := p.wait(id)
	if done.GetState() != pb.JobState_JOB_STATE_SUCCEEDED || string(done.GetResult()) != `{"count":7}` {
		t.Fatalf("the job %v: %s, %s", done.GetState(), done.GetResult(), done.GetError())
	}
	events := p.events(id, 0)
	if count(events, "task-timed-out") != 1 || count(events, "task-disagreed") != 0 || !strings.Contains(story(events), "task-timed-out beta: timed out after 1h0m0s") {
		t.Errorf("the job's events:\n%s", story(events))
	}
}

func TestAVerifiedJobOnWorkersThatDoTheWorkGivesWhatTheyAgreeOn(t *testing.T) {
	p := startPool(t, runtime.Builtin())
	p.startWorker("one", 2)
	p.startWorker("two", 2)
	p.waitForWorkers(2)

	// With no task count given a job is split so that its workers can run
	// every task as often as asked: four slots, each task twice, two tasks.
	spec := verifiedPrimes(0, 2)
	done := p.wait(p.submit(spec).GetJobId())
	if done.GetState() != pb.JobState_JOB_STATE_SUCCEEDED || string(done.GetResult()) != primesBelowTwoMillion || len(done.GetTasks()) != 2 {
		t.Fatalf("the job %v in %d tasks: %s, %s", done.GetState(), len(done.GetTasks()), done.GetResult(), done.GetError())
	}
	// Both workers ran each task, and what each logged is told as its own.
	events := p.events(done.GetJobId(), 0)
	ran, logged := make(map[string]bool), make(map[string]bool)
	for _, e := range events {
		switch e.GetKind() {
		case "task-started":
			ran[e.GetNodeName()+" "+string(rune('0'+e.GetTaskIndex()))] = true
		case "log":
			logged[e.GetNodeName()+" "+string(rune('0'+e.GetTaskIndex()))] = true
		}
	}
	if len(ran) != 4 || len(logged) != 4 || count(events, "task-started") != 4 || count(events, "task-succeeded") != 2 || count(events, "task-disagreed") != 0 {
		t.Errorf("who ran what: %v, who logged what: %v; the job's events:\n%s", ran, logged, story(events))
	}

	// A job that is one task for one worker is one task for two.
	whole := primesJob(pb.ScheduleMode_SCHEDULE_MODE_FULL_WORKER, 0)
	whole.Verify = 2
	done = p.wait(p.submit(whole).GetJobId())
	if done.GetState() != pb.JobState_JOB_STATE_SUCCEEDED || string(done.GetResult()) != primesBelowTwoMillion || len(done.GetTasks()) != 1 ||
		done.GetSpec().GetMode() != pb.ScheduleMode_SCHEDULE_MODE_FULL_WORKER {
		t.Errorf("the whole job on two workers: %v in %d tasks: %s, %s", done.GetState(), len(done.GetTasks()), done.GetResult(), done.GetError())
	}
	if record := p.recordText(done.GetJobId()); strings.Count(record, `"agreed":true`) != 2 {
		t.Errorf("its record: %s", record)
	}

	// What tasks store is compared by its CIDs: two workers that count the
	// same words store the same blobs.
	text := sampleText()
	counted := p.wait(p.submit(&pb.JobSpec{Workload: "wordcount", Params: []byte(`{"input":"` + p.upload(text) + `"}`), MaxTasks: 3, Verify: 2}).GetJobId())
	if want, _ := wordCountLocally(t, text); counted.GetState() != pb.JobState_JOB_STATE_SUCCEEDED || string(counted.GetResult()) != want {
		t.Errorf("the word count %v: %s, %s", counted.GetState(), counted.GetResult(), counted.GetError())
	}
	if events := p.events(counted.GetJobId(), 0); count(events, "task-disagreed") != 0 || count(events, "task-result") != 2*len(counted.GetTasks()) {
		t.Errorf("the word count's events:\n%s", story(events))
	}

	// Asking for one worker is asking for nothing.
	plain := p.wait(p.submit(verifiedPrimes(2, 1)).GetJobId())
	if events := p.events(plain.GetJobId(), 0); plain.GetState() != pb.JobState_JOB_STATE_SUCCEEDED || count(events, "task-started") != 2 || count(events, "task-result") != 0 {
		t.Errorf("a job verified by one worker: %v\n%s", plain.GetState(), story(events))
	}
}

func TestAJobThatCannotBeVerifiedIsRefused(t *testing.T) {
	p := startPool(t, runtime.Builtin().With(runtime.Graph{}))
	for name, tt := range map[string]struct {
		spec *pb.JobSpec
		code codes.Code
		want string
	}{
		"by too many":    {verifiedPrimes(1, 11), codes.InvalidArgument, "verify exceeds 10"},
		"with no worker": {verifiedPrimes(1, 2), codes.FailedPrecondition, "verify asks for 2 different workers to run each task, and 0 connected now could take this job"},
	} {
		if _, err := p.client.SubmitJob(p.ctx, &pb.SubmitJobRequest{Spec: tt.spec}); status.Code(err) != tt.code || !strings.Contains(status.Convert(err).Message(), tt.want) {
			t.Errorf("%s: %v, want %v with %q", name, err, tt.code, tt.want)
		}
	}

	// Only workers that could take the job count: not one that runs
	// something else, nor one with no slot to run anything in.
	p.crew("able")
	other := p.connectRaw(hello("other", 4, "alchemy"))
	other.welcome()
	full := p.connectRaw(hello("full", 0, "primes"))
	full.welcome()
	if _, err := p.client.SubmitJob(p.ctx, &pb.SubmitJobRequest{Spec: verifiedPrimes(1, 2)}); status.Code(err) != codes.FailedPrecondition || !strings.Contains(err.Error(), "and 1 connected now could take this job") {
		t.Errorf("with one worker that could take it: %v", err)
	}
	if len(p.coord.Jobs()) != 0 {
		t.Errorf("%d jobs were taken in", len(p.coord.Jobs()))
	}
}

func TestAVerifiedGraphHasEachOfItsStepsVerified(t *testing.T) {
	p := startPool(t, runtime.Builtin().With(runtime.Graph{}))
	steps := `{"name":"low","workload":"primes","params":{"from":0,"to":1000},"tasks":1},
		{"name":"span","workload":"primes","params":{"from":0,"to":"${low.result.count}"},"tasks":1}`
	verified := func() *pb.JobSpec {
		spec := graphOf(steps)
		// Half of a step that is one task is that task.
		spec.Verify, spec.VerifyShare = 2, 0.5
		return spec
	}

	// A graph is taken in whoever is connected: no worker runs it. Its
	// steps are held to what it asks as each begins, and with one worker
	// the first cannot be verified.
	p.startWorker("one", 2)
	p.waitForWorkers(1)
	failed := p.wait(p.submit(verified()).GetJobId())
	if failed.GetState() != pb.JobState_JOB_STATE_FAILED || !strings.Contains(failed.GetError(), "step low: verify asks for 2 different workers to run each task, and 1 connected now could take this job") {
		t.Errorf("the graph with one worker: %v, %s", failed.GetState(), failed.GetError())
	}

	p.startWorker("two", 2)
	p.waitForWorkers(2)
	done := p.wait(p.submit(verified()).GetJobId())
	// 168 primes below 1000, and 39 below 168.
	if done.GetState() != pb.JobState_JOB_STATE_SUCCEEDED || !strings.Contains(string(done.GetResult()), `"result":{"count":39}`) || done.GetSpec().GetVerify() != 2 {
		t.Fatalf("the graph %v: %s, %s", done.GetState(), done.GetResult(), done.GetError())
	}
	for name, jobs := range p.stepsOf(done.GetJobId()) {
		if len(jobs) != 1 || jobs[0].GetSpec().GetVerify() != 2 || jobs[0].GetSpec().GetVerifyShare() != 0.5 || jobs[0].GetState() != pb.JobState_JOB_STATE_SUCCEEDED {
			t.Fatalf("step %s was carried out by %v", name, jobs)
		}
		if events := p.events(jobs[0].GetJobId(), 0); count(events, "task-result") != 2 || count(events, "task-succeeded") != 1 {
			t.Errorf("the events of step %s:\n%s", name, story(events))
		}
	}
	if told := story(p.events(done.GetJobId(), 0)); !strings.Contains(told, "a share of 0.5 of the tasks of each to be verified by 2 workers") {
		t.Errorf("the graph's events:\n%s", told)
	}
	// The graph's own tasks are its steps, each carried out once, and
	// held to nothing themselves.
	for _, task := range done.GetTasks() {
		if task.GetNodeName() != "coordinator" || task.GetAttempt() != 1 || task.GetVerify() != 0 {
			t.Errorf("the graph's task %d: %v", task.GetIndex(), task)
		}
	}
	if record := p.recordText(done.GetJobId()); !strings.Contains(record, `"verify":2`) || strings.Contains(record, `"results"`) {
		t.Errorf("the graph's record: %s", record)
	}
}

func TestAVerifiedJobIsTakenUpWhereItWasAfterARestart(t *testing.T) {
	file := filepath.Join(t.TempDir(), "node.db")
	workloads := runtime.NewRegistry(runtime.Primes{})
	db := journalIn(t, file)
	before := startPoolWith(t, workloads, sameStore, db)
	crew := before.crew("alpha", "beta")
	alpha := crew[0]
	id := before.submit(verifiedPrimes(1, 2)).GetJobId()
	first := alpha.assignment()
	crew[1].assignment()
	alpha.returns(first, `{"count":7}`)
	waitFor(t, func() bool { return before.job(id).GetTasks()[0].GetProgress() == 0.5 })

	// A crash saves nothing on the way out: the database is as it was with
	// one result in and the other copy out.
	after := startPoolWith(t, workloads, sameStore, journalIn(t, file))
	task := after.job(id).GetTasks()[0]
	if task.GetState() != pb.TaskState_TASK_STATE_PENDING || task.GetError() != "the coordinator was restarted" || task.GetAttempt() != 2 {
		t.Errorf("the task after a crash: %v", task)
	}

	// The worker that had answered comes back and is not asked again: its
	// result was kept. One that had not is, and settles the task.
	ident := before.rawIdents[alpha]
	if err := after.access.Admit(ident.ID(), access.Worker, time.Now()); err != nil {
		t.Fatal(err)
	}
	back := after.connectRawAs(after.credentialsFor(ident), hello("alpha", 3, "primes"))
	back.welcome()
	gamma := after.crew("gamma")[0]
	last := gamma.assignment()
	if last.GetAttempt() != 3 {
		t.Errorf("the copy handed out after the restart is attempt %d", last.GetAttempt())
	}
	gamma.returns(last, `{"count":7}`)
	done := after.wait(id)
	if done.GetState() != pb.JobState_JOB_STATE_SUCCEEDED || string(done.GetResult()) != `{"count":7}` {
		t.Fatalf("the job after the restart %v: %s, %s", done.GetState(), done.GetResult(), done.GetError())
	}
	events := after.events(id, 0)
	told := story(events)
	if count(events, "task-started") != 3 || count(events, "resumed") != 1 || !strings.Contains(told, "task-succeeded: alpha, gamma returned the same result") {
		t.Errorf("the job's events:\n%s", told)
	}
	if record := after.recordText(id); strings.Count(record, `"agreed":true`) != 2 || !strings.Contains(record, `"error":"the coordinator was restarted"`) {
		t.Errorf("the job's record: %s", record)
	}
}

func TestAStoppingCoordinatorTakesBackTheCopiesOfAVerifiedTask(t *testing.T) {
	file := filepath.Join(t.TempDir(), "node.db")
	workloads := runtime.NewRegistry(runtime.Primes{})
	db := journalIn(t, file)
	before := startPoolWith(t, workloads, sameStore, db)
	crew := before.crew("alpha", "beta")
	id := before.submit(verifiedPrimes(1, 2)).GetJobId()
	crew[0].assignment()
	crew[1].assignment()
	before.stopAndForget()
	db.Close()

	// Neither copy is held against the task, however often that happens.
	db = journalIn(t, file)
	after := startPoolWith(t, workloads, sameStore, db)
	task := after.job(id).GetTasks()[0]
	if task.GetState() != pb.TaskState_TASK_STATE_PENDING || task.GetError() != "the coordinator stopped" {
		t.Errorf("the task after the coordinator was stopped: %v", task)
	}
	attempts, err := db.Attempts(id)
	if err != nil || len(attempts) != 2 || attempts[0].Err != "the coordinator stopped" || attempts[1].Err != "the coordinator stopped" {
		t.Errorf("the attempts on record: %+v, %v", attempts, err)
	}
	after.startWorker("one", 1)
	after.startWorker("two", 1)
	if done := after.wait(id); done.GetState() != pb.JobState_JOB_STATE_SUCCEEDED || string(done.GetResult()) != primesBelowTwoMillion {
		t.Errorf("the job after the restart %v: %s, %s", done.GetState(), done.GetResult(), done.GetError())
	}
}

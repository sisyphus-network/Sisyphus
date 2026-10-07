package main

import (
	"context"
	"errors"
	"io"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/sisyphus-network/Sisyphus/apps/sisyphusd/coordinator"
	jobmodel "github.com/sisyphus-network/Sisyphus/packages/job-model"
	pb "github.com/sisyphus-network/Sisyphus/packages/protocol/sisyphus/v1"
	"github.com/sisyphus-network/Sisyphus/packages/runtime"
	"github.com/sisyphus-network/Sisyphus/packages/storage"
)

// events returns everything that has happened to a job, waiting for the
// job to be over if it is not.
func (p *pool) events(jobID string, after uint64) []*pb.JobEvent {
	p.t.Helper()
	stream, err := p.client.WatchJobEvents(p.ctx, &pb.WatchJobEventsRequest{JobId: jobID, AfterSeq: after})
	if err != nil {
		p.t.Fatal(err)
	}
	var got []*pb.JobEvent
	for {
		e, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return got
		}
		if err != nil {
			p.t.Fatal(err)
		}
		got = append(got, e)
	}
}

// kinds lists what kinds of thing happened, in order.
func kinds(events []*pb.JobEvent) string {
	var out []string
	for _, e := range events {
		out = append(out, e.GetKind())
	}
	return strings.Join(out, " ")
}

func count(events []*pb.JobEvent, kind string) (n int) {
	for _, e := range events {
		if e.GetKind() == kind {
			n++
		}
	}
	return n
}

func TestAJobsEventsTellItsStory(t *testing.T) {
	p := startPool(t, runtime.Builtin())
	p.startWorker("rig", 2)
	p.waitForWorkers(1)
	job := p.wait(p.submit(primesJob(pb.ScheduleMode_SCHEDULE_MODE_DISTRIBUTED, 2)).GetJobId())

	all := p.events(job.GetJobId(), 0)
	for i, e := range all {
		if e.GetSeq() != uint64(i+1) || e.GetAt() == nil {
			t.Fatalf("event %d is numbered %d, at %v", i, e.GetSeq(), e.GetAt())
		}
	}
	if all[0].GetKind() != "submitted" || all[0].GetTaskIndex() != -1 || !strings.Contains(all[0].GetText(), "primes, in 2 tasks") {
		t.Errorf("the first event: %v", all[0])
	}
	if last := all[len(all)-1]; last.GetKind() != "succeeded" || last.GetTaskIndex() != -1 {
		t.Errorf("the last event: %v", last)
	}
	if count(all, "task-started") != 2 || count(all, "task-succeeded") != 2 {
		t.Errorf("the job's events: %s", kinds(all))
	}
	// What the tasks said of themselves is there too, with who said it.
	logged := 0
	for _, e := range all {
		if e.GetKind() == "log" && strings.HasPrefix(e.GetText(), "counting primes from") && e.GetNodeName() == "rig" && e.GetTaskIndex() >= 0 {
			logged++
		}
	}
	if logged != 2 {
		t.Errorf("%d tasks' log lines among the events: %s", logged, kinds(all))
	}
	// From a given point on, only what came after.
	if later := p.events(job.GetJobId(), all[len(all)-2].GetSeq()); len(later) != 1 || later[0].GetKind() != "succeeded" {
		t.Errorf("events after the last but one: %s", kinds(later))
	}
	// And the finished job says it is all done.
	if job.GetProgress() != 1 || job.GetTasks()[0].GetProgress() != 1 {
		t.Errorf("a finished job's progress is %v, its first task's %v", job.GetProgress(), job.GetTasks()[0].GetProgress())
	}

	stream, err := p.client.WatchJobEvents(p.ctx, &pb.WatchJobEventsRequest{JobId: "no-such-job"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := stream.Recv(); status.Code(err) != codes.NotFound {
		t.Errorf("the events of a job that is not there: %v", err)
	}
}

// talkative is a workload whose task reports on itself and then waits to
// be released.
type talkative struct {
	broken
	reported chan struct{}
	release  chan struct{}
	progress float64
}

func (talkative) Name() string { return "talkative" }

func (w talkative) Execute(ctx context.Context, _ runtime.Blobs, _ []byte) ([]byte, error) {
	report := runtime.Report(ctx)
	report.Progress(w.progress)
	report.Log("halfway there")
	report.Log(strings.Repeat("x", 5000))
	w.reported <- struct{}{}
	select {
	case <-w.release:
		return []byte("ok"), nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (talkative) Aggregate(context.Context, runtime.Blobs, [][]byte) ([]byte, error) {
	return []byte("done"), nil
}

func TestATaskReportsItsProgressAndWhatItIsDoingWhileItRuns(t *testing.T) {
	for name, tt := range map[string]struct{ said, want float64 }{
		"halfway":           {0.5, 0.5},
		"more than all":     {7, 1},
		"less than nothing": {-3, 0},
	} {
		w := talkative{reported: make(chan struct{}, 4), release: make(chan struct{}), progress: tt.said}
		p := startPool(t, runtime.NewRegistry(w))
		p.startWorker("rig", 1)
		p.waitForWorkers(1)
		id := p.submit(&pb.JobSpec{Workload: "talkative", MaxTasks: 1}).GetJobId()
		<-w.reported

		// While it is still running, the job shows how far along it is.
		waitFor(t, func() bool {
			return strings.Contains(kinds(p.eventsSoFar(id)), "log log")
		})
		job := p.job(id)
		if job.GetState() != pb.JobState_JOB_STATE_RUNNING || job.GetTasks()[0].GetProgress() != tt.want || job.GetProgress() != tt.want {
			t.Errorf("%s: the running job's progress is %v, its task's %v, want %v", name, job.GetProgress(), job.GetTasks()[0].GetProgress(), tt.want)
		}
		var lines []string
		for _, e := range p.eventsSoFar(id) {
			if e.GetKind() == "log" {
				lines = append(lines, e.GetText())
			}
		}
		// A line too long to be worth passing on whole is cut.
		if len(lines) != 2 || lines[0] != "halfway there" || len(lines[1]) > 4200 || !strings.HasSuffix(lines[1], "[cut short]") {
			t.Errorf("%s: logged %d lines, the second %d bytes", name, len(lines), len(lines[len(lines)-1]))
		}
		close(w.release)
		if done := p.wait(id); done.GetProgress() != 1 {
			t.Errorf("%s: the finished job's progress is %v", name, done.GetProgress())
		}
	}
}

// eventsSoFar returns what has happened to a job up to now, without waiting
// for it to end.
func (p *pool) eventsSoFar(jobID string) (got []*pb.JobEvent) {
	p.t.Helper()
	ctx, cancel := context.WithCancel(p.ctx)
	defer cancel()
	seen := make(chan *pb.JobEvent, 64)
	go p.coord.WatchEvents(ctx, jobID, 0, func(e *pb.JobEvent) error {
		seen <- e
		return nil
	})
	for {
		select {
		case e := <-seen:
			got = append(got, e)
		case <-time.After(50 * time.Millisecond):
			return got
		}
	}
}

func TestFollowingAJobsEventsEndsWhenTheFollowerDoes(t *testing.T) {
	g := gated{started: make(chan struct{}, 16), release: make(chan struct{})}
	p := startPool(t, runtime.NewRegistry(g))
	id := p.submit(&pb.JobSpec{Workload: "gated", MaxTasks: 1}).GetJobId()
	defer close(g.release)

	// A follower that gives up while the job is still going.
	ctx, cancel := context.WithCancel(p.ctx)
	done := make(chan error, 1)
	go func() {
		done <- p.coord.WatchEvents(ctx, id, 0, func(*pb.JobEvent) error { return nil })
	}()
	time.Sleep(20 * time.Millisecond)
	cancel()
	if err := <-done; status.Code(err) != codes.Canceled {
		t.Errorf("a follower that gave up: %v", err)
	}
	// One that cannot be delivered to.
	undeliverable := errors.New("the follower has gone")
	if err := p.coord.WatchEvents(p.ctx, id, 0, func(*pb.JobEvent) error { return undeliverable }); !errors.Is(err, undeliverable) {
		t.Errorf("a follower that could not be delivered to: %v", err)
	}
}

func TestNewsOfATaskTheWorkerIsNotRunningIsIgnored(t *testing.T) {
	p := startPool(t, runtime.Builtin())
	w := p.connectRaw(hello("confused", 1, "primes"))
	w.welcome()
	id := p.submit(primesJob(pb.ScheduleMode_SCHEDULE_MODE_FULL_WORKER, 0)).GetJobId()
	assigned := w.assignment()
	for _, update := range []*pb.TaskUpdate{
		{TaskId: "some-other-task", Attempt: 1, Progress: 0.9, Log: []string{"not mine"}},
		{TaskId: assigned.GetTaskId(), Attempt: assigned.GetAttempt() + 1, Progress: 0.9, Log: []string{"from the future"}},
	} {
		w.send(&pb.WorkerMessage{Kind: &pb.WorkerMessage_TaskUpdate{TaskUpdate: update}})
	}
	// One that is its own is taken in, which shows the others were read.
	w.send(&pb.WorkerMessage{Kind: &pb.WorkerMessage_TaskUpdate{TaskUpdate: &pb.TaskUpdate{TaskId: assigned.GetTaskId(), Attempt: assigned.GetAttempt(), Progress: 0.25}}})
	waitFor(t, func() bool { return p.job(id).GetTasks()[0].GetProgress() == 0.25 })
	if got := kinds(p.eventsSoFar(id)); strings.Contains(got, "log") {
		t.Errorf("lines from tasks the worker was not running were recorded: %s", got)
	}
}

func TestAJobCanBeCancelled(t *testing.T) {
	g := gated{started: make(chan struct{}, 16), release: make(chan struct{})}
	p := startPool(t, runtime.NewRegistry(g, runtime.Primes{}))
	p.startWorker("rig", 1)
	p.waitForWorkers(1)
	// One task running, one waiting for the slot.
	id := p.submit(&pb.JobSpec{Workload: "gated", MaxTasks: 2}).GetJobId()
	<-g.started

	cancelled, err := p.client.CancelJob(p.ctx, &pb.CancelJobRequest{JobId: id})
	if err != nil {
		t.Fatal(err)
	}
	job := cancelled.GetJob()
	if job.GetState() != pb.JobState_JOB_STATE_CANCELLED || job.GetFinishedAt() == nil || job.GetError() != "cancelled" {
		t.Errorf("the cancelled job: %v", job)
	}
	for _, task := range job.GetTasks() {
		if task.GetState() != pb.TaskState_TASK_STATE_CANCELLED {
			t.Errorf("task %d of a cancelled job is %v", task.GetIndex(), task.GetState())
		}
	}
	// A watcher is told it is over, and its events end with the cancelling.
	if got := p.wait(id); got.GetState() != pb.JobState_JOB_STATE_CANCELLED {
		t.Errorf("a watcher saw %v", got.GetState())
	}
	if all := p.events(id, 0); all[len(all)-1].GetKind() != "cancelled" {
		t.Errorf("the cancelled job's events: %s", kinds(all))
	}
	// The worker was told to stop: its one slot is free for the next job,
	// though the cancelled task was never released.
	next := p.wait(p.submit(primesJob(pb.ScheduleMode_SCHEDULE_MODE_FULL_WORKER, 0)).GetJobId())
	if string(next.GetResult()) != primesBelowTwoMillion {
		t.Errorf("the job after the cancelled one: %v: %s", next.GetState(), next.GetError())
	}

	// What is over cannot be cancelled, nor what is not there.
	for jobID, want := range map[string]codes.Code{id: codes.FailedPrecondition, next.GetJobId(): codes.FailedPrecondition, "no-such-job": codes.NotFound} {
		if _, err := p.client.CancelJob(p.ctx, &pb.CancelJobRequest{JobId: jobID}); status.Code(err) != want {
			t.Errorf("cancelling %s: %v, want %v", jobID, err, want)
		}
	}
}

func TestCancellingAJobWhileItsOutputsAreBeingCombined(t *testing.T) {
	slow := slowAggregate{aggregating: make(chan struct{}), release: make(chan struct{})}
	p := startPool(t, runtime.NewRegistry(slow))
	p.startWorker("rig", 2)
	p.waitForWorkers(1)
	id := p.submit(&pb.JobSpec{Workload: "slow-aggregate", MaxTasks: 2}).GetJobId()
	<-slow.aggregating
	if _, err := p.client.CancelJob(p.ctx, &pb.CancelJobRequest{JobId: id}); err != nil {
		t.Fatal(err)
	}
	// The combining comes to its end and changes nothing.
	close(slow.release)
	time.Sleep(100 * time.Millisecond)
	if job := p.job(id); job.GetState() != pb.JobState_JOB_STATE_CANCELLED || len(job.GetResult()) != 0 {
		t.Errorf("the job after its combining finished: %v, result %q", job.GetState(), job.GetResult())
	}
}

func TestAnAttemptThatRunsTooLongIsStoppedAndTriedAgain(t *testing.T) {
	g := gated{started: make(chan struct{}, 16), release: make(chan struct{})}
	p := startPool(t, runtime.NewRegistry(g, runtime.Primes{}))
	p.startWorker("rig", 2)
	p.waitForWorkers(1)
	// An hour for each attempt, and a job with no limit beside it.
	limited := p.submit(&pb.JobSpec{Workload: "gated", MaxTasks: 1, TaskTimeoutSeconds: 3600})
	unlimited := p.submit(&pb.JobSpec{Workload: "gated", MaxTasks: 1})
	<-g.started
	<-g.started
	if limited.GetSpec().GetTaskTimeoutSeconds() != 3600 {
		t.Errorf("the job's limit is %d seconds", limited.GetSpec().GetTaskTimeoutSeconds())
	}

	// Within the hour nothing happens.
	p.coord.Expire(time.Now().Add(30 * time.Minute))
	if task := p.job(limited.GetJobId()).GetTasks()[0]; task.GetAttempt() != 1 || task.GetState() != pb.TaskState_TASK_STATE_RUNNING {
		t.Fatalf("half an hour in, the task is %v on attempt %d", task.GetState(), task.GetAttempt())
	}
	// Past it, the attempt is stopped and the task handed out again; three
	// times, and the job has failed.
	for attempt := 1; attempt <= 3; attempt++ {
		p.coord.Expire(time.Now().Add(2 * time.Hour))
		if attempt < 3 {
			<-g.started
			waitFor(t, func() bool { return p.job(limited.GetJobId()).GetTasks()[0].GetAttempt() == uint32(attempt+1) })
		}
	}
	failed := p.wait(limited.GetJobId())
	if failed.GetState() != pb.JobState_JOB_STATE_FAILED || !strings.Contains(failed.GetError(), "timed out after 1h0m0s") {
		t.Errorf("the job whose attempts all ran too long: %v: %s", failed.GetState(), failed.GetError())
	}
	if all := p.events(limited.GetJobId(), 0); count(all, "task-timed-out") != 3 {
		t.Errorf("its events: %s", kinds(all))
	}
	// The one with no limit was left alone throughout.
	if job := p.job(unlimited.GetJobId()); job.GetState() != pb.JobState_JOB_STATE_RUNNING || job.GetTasks()[0].GetAttempt() != 1 {
		t.Errorf("the job with no limit: %v, attempt %d", job.GetState(), job.GetTasks()[0].GetAttempt())
	}
	close(g.release)
	p.wait(unlimited.GetJobId())
}

func TestAJobThatFailsStopsItsOtherTasks(t *testing.T) {
	p, w, _, failed := startPartlyBrokenJob(t)
	// The task that was still running when the job failed is given up, and
	// was told to stop rather than left to finish.
	for _, task := range failed.GetTasks() {
		if task.GetState() == pb.TaskState_TASK_STATE_RUNNING || task.GetState() == pb.TaskState_TASK_STATE_PENDING {
			t.Errorf("task %d of a failed job is still %v", task.GetIndex(), task.GetState())
		}
	}
	_ = w
	if all := p.events(failed.GetJobId(), 0); all[len(all)-1].GetKind() != "failed" || count(all, "task-failed") != 3 {
		t.Errorf("the failed job's events: %s", kinds(all))
	}
}

func TestAJobsEventsSurviveARestart(t *testing.T) {
	file := filepath.Join(t.TempDir(), "node.db")
	g := gated{started: make(chan struct{}, 16), release: make(chan struct{})}
	workloads := runtime.NewRegistry(g, runtime.Primes{})
	db := journalIn(t, file)
	before := startPoolWith(t, workloads, sameStore, db)
	stopWorker := before.startWorker("rig", 1)
	before.waitForWorkers(1)
	finished := before.wait(before.submit(primesJob(pb.ScheduleMode_SCHEDULE_MODE_FULL_WORKER, 0)).GetJobId())
	told := before.events(finished.GetJobId(), 0)
	unfinished := before.submit(&pb.JobSpec{Workload: "gated", MaxTasks: 1, TaskTimeoutSeconds: 90}).GetJobId()
	<-g.started
	before.stopAndForget()
	stopWorker()
	db.Close()

	db = journalIn(t, file)
	after := startPoolWith(t, workloads, sameStore, db)
	// The finished job's story is as it was.
	if again := after.events(finished.GetJobId(), 0); kinds(again) != kinds(told) || again[0].GetSeq() != 1 {
		t.Errorf("after a restart the finished job's events are %q, and were %q", kinds(again), kinds(told))
	}
	// The unfinished one's carries on where it left off, numbered on from
	// there, and keeps its limit.
	sofar := after.eventsSoFar(unfinished)
	if last := sofar[len(sofar)-1]; last.GetKind() != "resumed" || last.GetSeq() != uint64(len(sofar)) {
		t.Errorf("the unfinished job's events after a restart: %s", kinds(sofar))
	}
	if job := after.job(unfinished); job.GetSpec().GetTaskTimeoutSeconds() != 90 {
		t.Errorf("after a restart the job's limit is %d seconds", job.GetSpec().GetTaskTimeoutSeconds())
	}
	close(g.release)
}

// forgetful is a journal that keeps jobs and cannot keep or recall events.
type forgetful struct{ coordinator.Journal }

var errForgot = errors.New("the events table is gone")

func (forgetful) SaveEvent(string, jobmodel.Event) error      { return errForgot }
func (forgetful) LoadEvents(string) ([]jobmodel.Event, error) { return nil, errForgot }

func TestAJobCarriesOnWhenItsEventsCannotBeKept(t *testing.T) {
	file := filepath.Join(t.TempDir(), "node.db")
	journal := forgetful{journalIn(t, file)}
	p := startPoolWith(t, runtime.Builtin(), sameStore, journal)
	p.startWorker("rig", 1)
	p.waitForWorkers(1)
	job := p.wait(p.submit(primesJob(pb.ScheduleMode_SCHEDULE_MODE_FULL_WORKER, 0)).GetJobId())
	if job.GetState() != pb.JobState_JOB_STATE_SUCCEEDED {
		t.Fatalf("job %v: %s", job.GetState(), job.GetError())
	}
	if !strings.Contains(p.logs.String(), "could not save a job's event") {
		t.Error("nothing logged about the events that could not be saved")
	}
	// They are still there to be followed while the coordinator runs.
	if all := p.events(job.GetJobId(), 0); len(all) == 0 {
		t.Error("the job has no events at all")
	}
	// And a coordinator that cannot read a job's events back still has the job.
	coord := coordinator.New(coordinator.Config{Workloads: runtime.Builtin(), Store: storage.NewMemory(), Journal: journal, Log: quiet})
	defer coord.Close()
	if _, err := coord.Recover(); err != nil {
		t.Fatal(err)
	}
	if got, err := coord.Get(job.GetJobId()); err != nil || got.GetState() != pb.JobState_JOB_STATE_SUCCEEDED {
		t.Errorf("the job after a restart without its events: %v, %v", got, err)
	}
}

func TestCLIFollowsAndCancelsJobs(t *testing.T) {
	addr, _ := startNode(t)
	out := mustCLI(t, "job", "submit", "--addr", addr, "--detach", "--timeout", "90s", "--params", `{"from":0,"to":100}`)
	id := strings.TrimSpace(out)
	// The log follows the job to its end and then returns.
	logs := mustCLI(t, "job", "logs", "--addr", addr, id)
	lines := strings.Split(strings.TrimSpace(logs), "\n")
	if !strings.HasSuffix(lines[0], "submitted: primes, in 1 tasks") || !strings.HasSuffix(lines[len(lines)-1], "succeeded") {
		t.Errorf("job logs printed:\n%s", logs)
	}
	if !strings.Contains(logs, "task 0 on ") || !strings.Contains(logs, " log: counting primes from 2 to 100") || !strings.Contains(logs, "task-started: attempt 1") {
		t.Errorf("job logs printed:\n%s", logs)
	}
	// A finished job cannot be cancelled.
	if _, err := cli(t, "job", "cancel", "--addr", addr, id); err == nil || !strings.Contains(err.Error(), "already over: it succeeded") {
		t.Errorf("cancelling a finished job: %v", err)
	}

	// One that would run for a long while can.
	long := strings.TrimSpace(mustCLI(t, "job", "submit", "--addr", addr, "--detach", "--params", `{"from":0,"to":900000000000}`))
	if out := mustCLI(t, "job", "cancel", "--addr", addr, long); !strings.Contains(out, "job "+long+" cancelled") {
		t.Errorf("job cancel printed %q", out)
	}
	if _, err := cli(t, "job", "get", "--addr", addr, long); err == nil || !strings.Contains(err.Error(), "was cancelled after") {
		t.Errorf("job get on a cancelled job: %v", err)
	}
	if logs := mustCLI(t, "job", "logs", "--addr", addr, long); !strings.HasSuffix(strings.TrimSpace(logs), "cancelled: cancelled") {
		t.Errorf("a cancelled job's log:\n%s", logs)
	}

	// A caller that has already given up gets nowhere.
	gone, cancel := context.WithCancel(context.Background())
	cancel()
	if err := jobLogs(gone, []string{"--addr", addr, "--data-dir", dataDirOfNode(addr), id}); err == nil {
		t.Error("following a job's log with nobody waiting succeeded")
	}
	for _, bad := range [][]string{
		{"job", "logs", "--addr", addr},
		{"job", "logs", "--addr", addr, "no-such-job"},
		{"job", "logs", "--no-such-flag"},
		{"job", "logs", "--data-dir", "\x00", id},
		{"job", "cancel", "--addr", addr},
		{"job", "cancel", "--addr", addr, "no-such-job"},
		{"job", "cancel", "--no-such-flag"},
		{"job", "cancel", "--data-dir", "\x00", id},
	} {
		if _, err := cli(t, bad...); err == nil {
			t.Errorf("%v succeeded", bad)
		}
	}
}

// dataDirOfNode is the data directory of the test node listening at addr.
func dataDirOfNode(addr string) string {
	dir, _ := dataDirs.Load(addr)
	return dir.(string)
}

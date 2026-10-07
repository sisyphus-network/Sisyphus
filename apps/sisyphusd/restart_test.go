package main

import (
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/excho0/Sisyphus/apps/sisyphusd/coordinator"
	jobmodel "github.com/excho0/Sisyphus/packages/job-model"
	"github.com/excho0/Sisyphus/packages/nodedb"
	pb "github.com/excho0/Sisyphus/packages/protocol/sisyphus/v1"
	"github.com/excho0/Sisyphus/packages/runtime"
	"github.com/excho0/Sisyphus/packages/storage"
)

func sameStore(s *storage.Store) coordinator.Store { return s }

// journalIn opens the node database in file for the length of the test.
func journalIn(t *testing.T, file string) *nodedb.DB {
	t.Helper()
	db, err := nodedb.Open(file)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// stopAndForget stops a pool's coordinator the way a node being stopped
// does, and waits until it has let go of every worker.
func (p *pool) stopAndForget() {
	p.t.Helper()
	p.stop()
	waitFor(p.t, func() bool { return len(p.coord.Nodes()) == 0 })
}

func (p *pool) job(id string) *pb.Job {
	p.t.Helper()
	got, err := p.client.GetJob(p.ctx, &pb.GetJobRequest{JobId: id})
	if err != nil {
		p.t.Fatal(err)
	}
	return got.GetJob()
}

func TestJobsSurviveACoordinatorRestart(t *testing.T) {
	file := filepath.Join(t.TempDir(), "node.db")
	g := gated{started: make(chan struct{}, 16), release: make(chan struct{})}
	workloads := runtime.NewRegistry(g, runtime.Primes{})

	db := journalIn(t, file)
	before := startPoolWith(t, workloads, sameStore, db)
	stopFirst := before.startWorker("first", 2)
	before.waitForWorkers(1)
	finished := before.wait(before.submit(primesJob(pb.ScheduleMode_SCHEDULE_MODE_DISTRIBUTED, 2)).GetJobId())
	// Three tasks for a worker with room for two: two start, one waits.
	interrupted := before.submit(&pb.JobSpec{Workload: "gated", MaxTasks: 3})
	<-g.started
	<-g.started

	before.stopAndForget()
	stopFirst()
	db.Close()

	db = journalIn(t, file)
	after := startPoolWith(t, workloads, sameStore, db)

	// A job that had finished is still there to be asked after.
	if got := after.job(finished.GetJobId()); got.GetState() != pb.JobState_JOB_STATE_SUCCEEDED || string(got.GetResult()) != primesBelowTwoMillion ||
		!got.GetFinishedAt().AsTime().Equal(finished.GetFinishedAt().AsTime()) {
		t.Errorf("the finished job after a restart: %v", got)
	}
	// The one cut short has every task waiting again, and nothing held
	// against the two that were running.
	got := after.job(interrupted.GetJobId())
	if got.GetState() != pb.JobState_JOB_STATE_RUNNING || len(got.GetTasks()) != 3 {
		t.Fatalf("the interrupted job after a restart: %v", got)
	}
	for _, task := range got.GetTasks() {
		if task.GetState() != pb.TaskState_TASK_STATE_PENDING {
			t.Errorf("task %d is %v after a restart, want pending", task.GetIndex(), task.GetState())
		}
	}

	after.startWorker("second", 3)
	for range 3 {
		<-g.started
	}
	close(g.release)
	done := after.wait(interrupted.GetJobId())
	if done.GetState() != pb.JobState_JOB_STATE_SUCCEEDED || string(done.GetResult()) != "3" {
		t.Fatalf("the interrupted job, resumed: %v, result %q, error %s", done.GetState(), done.GetResult(), done.GetError())
	}

	attempts, err := db.Attempts(interrupted.GetJobId())
	if err != nil {
		t.Fatal(err)
	}
	var lost, succeeded int
	for _, attempt := range attempts {
		switch {
		case attempt.NodeName == "first" && attempt.State == jobmodel.Pending && attempt.Err == "the coordinator stopped":
			lost++
		case attempt.NodeName == "second" && attempt.State == jobmodel.Succeeded:
			succeeded++
		}
	}
	if len(attempts) != 5 || lost != 2 || succeeded != 3 {
		t.Errorf("attempts on record: %+v, want two lost on the first worker and three succeeded on the second", attempts)
	}
}

func TestAJobIsNotFailedByHoweverManyRestarts(t *testing.T) {
	file := filepath.Join(t.TempDir(), "node.db")
	workloads := runtime.NewRegistry(runtime.Primes{})
	var jobID string
	// More restarts than a task has attempts, each with the task handed out.
	for round := range 5 {
		db := journalIn(t, file)
		p := startPoolWith(t, workloads, sameStore, db)
		w := p.connectRaw(hello("holds-on", 1, "primes"))
		w.welcome()
		if round == 0 {
			jobID = p.submit(primesJob(pb.ScheduleMode_SCHEDULE_MODE_FULL_WORKER, 0)).GetJobId()
		}
		if got := w.assignment(); got.GetAttempt() != uint32(round+1) {
			t.Fatalf("round %d: the task is on attempt %d", round, got.GetAttempt())
		}
		p.stopAndForget()
		db.Close()
	}

	p := startPoolWith(t, workloads, sameStore, journalIn(t, file))
	p.startWorker("finisher", 1)
	if job := p.wait(jobID); job.GetState() != pb.JobState_JOB_STATE_SUCCEEDED {
		t.Errorf("after five restarts the job %v: %s", job.GetState(), job.GetError())
	}
}

func TestACrashedCoordinatorsRunningTasksGoBackToWaiting(t *testing.T) {
	file := filepath.Join(t.TempDir(), "node.db")
	workloads := runtime.NewRegistry(runtime.Primes{})
	db := journalIn(t, file)
	p := startPoolWith(t, workloads, sameStore, db)
	w := p.connectRaw(hello("holds-on", 1, "primes"))
	w.welcome()
	jobID := p.submit(primesJob(pb.ScheduleMode_SCHEDULE_MODE_FULL_WORKER, 0)).GetJobId()
	w.assignment()

	// A crash saves nothing on the way out: the database is as it was while
	// the task was running.
	crashed := journalIn(t, file)
	after := startPoolWith(t, workloads, sameStore, crashed)
	task := after.job(jobID).GetTasks()[0]
	if task.GetState() != pb.TaskState_TASK_STATE_PENDING || task.GetError() != "the coordinator was restarted" {
		t.Errorf("the task after a crash: %v", task)
	}
	after.startWorker("finisher", 1)
	if job := after.wait(jobID); job.GetState() != pb.JobState_JOB_STATE_SUCCEEDED || job.GetTasks()[0].GetAttempt() != 2 {
		t.Errorf("the job after a crash: %v", job)
	}
}

func TestAStoppingCoordinatorLeavesCombiningForAfterTheRestart(t *testing.T) {
	file := filepath.Join(t.TempDir(), "node.db")
	slow := slowAggregate{aggregating: make(chan struct{}), release: make(chan struct{})}
	db := journalIn(t, file)
	before := startPoolWith(t, runtime.NewRegistry(slow), sameStore, db)
	stopWorker := before.startWorker("a", 2)
	before.waitForWorkers(1)
	jobID := before.submit(&pb.JobSpec{Workload: "slow-aggregate", MaxTasks: 2}).GetJobId()
	<-slow.aggregating

	// Stopping cuts the combining short. That is not the job's failure.
	before.stopAndForget()
	stopWorker()
	if got, err := before.coord.Get(jobID); err != nil || got.GetState() != pb.JobState_JOB_STATE_RUNNING {
		t.Fatalf("the job as the coordinator stopped: %v, %v", got, err)
	}
	db.Close()

	// With no worker at all, the restarted coordinator finishes the job
	// from the outputs it already had.
	ready := slowAggregate{aggregating: make(chan struct{}), release: make(chan struct{})}
	close(ready.release)
	after := startPoolWith(t, runtime.NewRegistry(ready), sameStore, journalIn(t, file))
	if job := after.wait(jobID); job.GetState() != pb.JobState_JOB_STATE_SUCCEEDED || string(job.GetResult()) != "done" {
		t.Errorf("the job after the restart: %v, result %q, error %s", job.GetState(), job.GetResult(), job.GetError())
	}
}

func TestAStoppingCoordinatorTakesInNoMoreResults(t *testing.T) {
	file := filepath.Join(t.TempDir(), "node.db")
	workloads := runtime.NewRegistry(runtime.Primes{})
	db := journalIn(t, file)
	p := startPoolWith(t, workloads, sameStore, db)
	w := p.connectRaw(hello("late", 1, "primes"))
	w.welcome()
	jobID := p.submit(primesJob(pb.ScheduleMode_SCHEDULE_MODE_FULL_WORKER, 0)).GetJobId()
	assigned := w.assignment()

	p.coord.Close()
	w.reply(&pb.TaskResult{TaskId: assigned.GetTaskId(), Attempt: assigned.GetAttempt(), Outcome: &pb.TaskResult_Output{Output: []byte(`{"count":1}`)}})
	waitFor(t, func() bool { return strings.Contains(p.logs.String(), "dropping a result that arrived while stopping") })
	p.stopAndForget()
	db.Close()

	after := startPoolWith(t, workloads, sameStore, journalIn(t, file))
	if task := after.job(jobID).GetTasks()[0]; task.GetState() != pb.TaskState_TASK_STATE_PENDING {
		t.Errorf("the task whose result came too late is %v after the restart, want pending", task.GetState())
	}
	after.startWorker("finisher", 1)
	if job := after.wait(jobID); string(job.GetResult()) != primesBelowTwoMillion {
		t.Errorf("the job, run again: %s (%s)", job.GetResult(), job.GetError())
	}
}

func TestAJobWhoseWorkloadIsGoneAfterARestartFails(t *testing.T) {
	file := filepath.Join(t.TempDir(), "node.db")
	g := gated{started: make(chan struct{}, 16), release: make(chan struct{})}
	db := journalIn(t, file)
	before := startPoolWith(t, runtime.NewRegistry(g, runtime.Primes{}), sameStore, db)
	jobID := before.submit(&pb.JobSpec{Workload: "gated", MaxTasks: 2}).GetJobId()
	before.stopAndForget()
	db.Close()

	db = journalIn(t, file)
	after := startPoolWith(t, runtime.NewRegistry(runtime.Primes{}), sameStore, db)
	job := after.job(jobID)
	if job.GetState() != pb.JobState_JOB_STATE_FAILED || !strings.Contains(job.GetError(), "restarted without this job's workload") {
		t.Errorf("the job after a restart without its workload: %v: %s", job.GetState(), job.GetError())
	}
	// And that is on record, not decided afresh each time.
	saved, err := db.LoadJobs()
	if err != nil || len(saved) != 1 || saved[0].State != jobmodel.Failed {
		t.Errorf("the job as saved: %+v, %v", saved, err)
	}
}

func TestOnlyUnfinishedJobsHoldPinsOpen(t *testing.T) {
	g := gated{started: make(chan struct{}, 16), release: make(chan struct{})}
	p := startPool(t, runtime.NewRegistry(g, runtime.Primes{}))
	p.startWorker("a", 2)
	p.waitForWorkers(1)
	finished := p.wait(p.submit(primesJob(pb.ScheduleMode_SCHEDULE_MODE_DISTRIBUTED, 2)).GetJobId())
	running := p.submit(&pb.JobSpec{Workload: "gated", MaxTasks: 1})
	defer close(g.release)

	for owner, want := range map[string]bool{
		"job:" + running.GetJobId():  true,
		"job:" + finished.GetJobId(): false,
		"job:never-heard-of-it":      false,
		running.GetJobId():           false, // a job's ID is not the name its pins are under
		"user":                       false,
	} {
		if got := p.coord.Holds(owner); got != want {
			t.Errorf("Holds(%q) = %v, want %v", owner, got, want)
		}
	}
	if !coordinator.OwnsPin("job:anything") || coordinator.OwnsPin("user") {
		t.Error("OwnsPin does not tell a job's pins from a user's")
	}
}

// faultyJournal is a journal that can be made to fail.
type faultyJournal struct {
	coordinator.Journal
	failing atomic.Bool
}

var errJournal = errors.New("disk on fire")

func (j *faultyJournal) SaveJob(job *jobmodel.Job) error {
	if j.failing.Load() {
		return errJournal
	}
	return j.Journal.SaveJob(job)
}

func (j *faultyJournal) LoadJobs() ([]*jobmodel.Job, error) {
	if j.failing.Load() {
		return nil, errJournal
	}
	return j.Journal.LoadJobs()
}

func TestAJobThatCannotBeRecordedIsNotAccepted(t *testing.T) {
	file := filepath.Join(t.TempDir(), "node.db")
	journal := &faultyJournal{Journal: journalIn(t, file)}
	p := startPoolWith(t, runtime.NewRegistry(runtime.Primes{}), sameStore, journal)
	p.startWorker("a", 1)
	p.waitForWorkers(1)

	journal.failing.Store(true)
	_, err := p.client.SubmitJob(p.ctx, &pb.SubmitJobRequest{Spec: primesJob(pb.ScheduleMode_SCHEDULE_MODE_FULL_WORKER, 0)})
	if status.Code(err) != codes.Internal || !strings.Contains(err.Error(), "could not record the job") {
		t.Fatalf("submitting to a coordinator that cannot record jobs: %v", err)
	}
	journal.failing.Store(false)
	if saved, _ := journal.LoadJobs(); len(saved) != 0 {
		t.Errorf("a refused job was recorded all the same: %+v", saved)
	}
}

func TestAJobCarriesOnWhenAChangeCannotBeRecorded(t *testing.T) {
	file := filepath.Join(t.TempDir(), "node.db")
	journal := &faultyJournal{Journal: journalIn(t, file)}
	p := startPoolWith(t, runtime.NewRegistry(runtime.Primes{}), sameStore, journal)
	w := p.connectRaw(hello("a", 1, "primes"))
	w.welcome()
	jobID := p.submit(primesJob(pb.ScheduleMode_SCHEDULE_MODE_FULL_WORKER, 0)).GetJobId()
	assigned := w.assignment()

	journal.failing.Store(true)
	w.reply(&pb.TaskResult{TaskId: assigned.GetTaskId(), Attempt: assigned.GetAttempt(), Outcome: &pb.TaskResult_Error{Error: "first try failed"}})
	second := w.assignment()
	if !strings.Contains(p.logs.String(), "could not save a job") {
		t.Errorf("nothing logged about the change that could not be saved:\n%s", p.logs)
	}

	// Once the journal works again it catches up, missing nothing.
	journal.failing.Store(false)
	w.reply(&pb.TaskResult{TaskId: second.GetTaskId(), Attempt: second.GetAttempt(), Outcome: &pb.TaskResult_Output{Output: []byte(`{"count":3}`)}})
	if job := p.wait(jobID); job.GetState() != pb.JobState_JOB_STATE_SUCCEEDED {
		t.Fatalf("job %v: %s", job.GetState(), job.GetError())
	}
	saved, err := journal.LoadJobs()
	if err != nil || len(saved) != 1 || saved[0].State != jobmodel.Succeeded || saved[0].Tasks[0].Attempt != 2 || saved[0].Tasks[0].Failures != 1 {
		t.Errorf("the job as saved: %+v, %v", saved, err)
	}
}

func TestACoordinatorThatCannotReadItsJobsSaysSo(t *testing.T) {
	journal := &faultyJournal{Journal: journalIn(t, filepath.Join(t.TempDir(), "node.db"))}
	journal.failing.Store(true)
	coord := coordinator.New(coordinator.Config{Workloads: runtime.Builtin(), Store: storage.NewMemory(), Journal: journal, Log: quiet})
	defer coord.Close()
	if _, err := coord.Recover(); !errors.Is(err, errJournal) {
		t.Errorf("Recover: %v, want the journal's error", err)
	}
}

func TestANodePicksUpItsJobsWhenRestarted(t *testing.T) {
	dataDir := t.TempDir()
	input := writeFile(t, sampleText())

	// A coordinator with nobody to do the work takes a job and is stopped.
	addr := freeAddr(t)
	stop := startDaemon(t, "--data-dir", dataDir, "--role", "coordinator", "--listen", addr, "--retain", "2h", "--gc-interval", "0")
	id := strings.TrimSpace(waitForOutput(t, "bafy", "blob", "put", "--addr", addr, "--no-pin", input))
	jobID := strings.TrimSpace(mustCLI(t, "job", "submit", "--addr", addr, "--detach", "--workload", "wordcount", "--params", `{"input":"`+id+`"}`))
	stop()

	// Started again, it still has the job, and still holds the job's input
	// for as long as the job takes rather than for the two hours left to a
	// job that is gone.
	addr = freeAddr(t)
	stop = startDaemon(t, "--data-dir", dataDir, "--role", "coordinator", "--listen", addr, "--retain", "2h", "--gc-interval", "0")
	if got := waitForOutput(t, jobID, "job", "get", "--addr", addr, jobID); !strings.Contains(got, "pending") {
		t.Errorf("the job after a restart:\n%s", got)
	}
	if lines := pinLines(t, addr, id); !has(lines, "job:"+jobID+" released") {
		t.Errorf("the unfinished job's input is pinned as %v, want held until the job releases it", lines)
	}
	stop()

	// A third start, now with a worker, and the job runs.
	addr = freeAddr(t)
	startDaemon(t, "--data-dir", dataDir, "--listen", addr, "--slots", "2")
	if got := waitForOutput(t, "succeeded in", "job", "get", "--addr", addr, jobID); !strings.Contains(got, "workload wordcount") {
		t.Errorf("the job once a worker came:\n%s", got)
	}
}

func TestANodeWillNotStartOnADatabaseItCannotUse(t *testing.T) {
	// Something that is not a database where the database goes.
	blocked := t.TempDir()
	if err := os.Mkdir(filepath.Join(blocked, "node.db"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := cli(t, "run", "--data-dir", blocked, "--listen", freeAddr(t)); err == nil || !strings.Contains(err.Error(), "open node database") {
		t.Errorf("error %v, want the database not opened", err)
	}

	// A database with a table gone.
	damaged := t.TempDir()
	file := filepath.Join(damaged, "node.db")
	journalIn(t, file).Close()
	raw, err := sql.Open("sqlite", file)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`DROP TABLE job_blobs`); err != nil {
		t.Fatal(err)
	}
	raw.Close()
	if _, err := cli(t, "run", "--data-dir", damaged, "--listen", freeAddr(t)); err == nil || !strings.Contains(err.Error(), "load job blobs") {
		t.Errorf("error %v, want the jobs not loaded", err)
	}
}

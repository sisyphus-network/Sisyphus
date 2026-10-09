package main

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sisyphus-network/Sisyphus/apps/sisyphusd/access"
	"github.com/sisyphus-network/Sisyphus/apps/sisyphusd/coordinator"
	jobmodel "github.com/sisyphus-network/Sisyphus/packages/job-model"
	pb "github.com/sisyphus-network/Sisyphus/packages/protocol/sisyphus/v1"
	"github.com/sisyphus-network/Sisyphus/packages/runtime"
	"github.com/sisyphus-network/Sisyphus/packages/storage"
)

// These tests are about a worker's standing: what verified tasks have shown
// of it, which is kept from one job to the next, and what being outvoted
// costs it in the jobs that follow.

// standing is how the coordinator lists a connected worker's standing: the
// results that agreed, those that were outvoted, and what it still owes.
func (p *pool) standing(w *rawWorker) string {
	p.t.Helper()
	for _, node := range p.coord.Nodes() {
		if node.GetNodeId() == w.id {
			return fmt.Sprintf("%d agreed, %d outvoted, %d owed", node.GetVerifiedAgreed(), node.GetVerifiedOutvoted(), node.GetProbation())
		}
	}
	p.t.Fatalf("worker %s is not connected", w.id)
	return ""
}

// outvote runs a job of one task that two must agree on, for which the
// first of three workers returns one thing and the other two another. The
// first must be the one with most slots free, and the second the next.
func (p *pool) outvote(liar, second, third *rawWorker) *pb.Job {
	p.t.Helper()
	id := p.submit(verifiedPrimes(1, 2)).GetJobId()
	wrong, right := liar.assignment(), second.assignment()
	liar.returns(wrong, `{"count":1}`)
	second.returns(right, `{"count":7}`)
	third.returns(third.assignment(), `{"count":7}`)
	done := p.wait(id)
	if done.GetState() != pb.JobState_JOB_STATE_SUCCEEDED || string(done.GetResult()) != `{"count":7}` {
		p.t.Fatalf("the job in which a worker is outvoted %v: %s, %s", done.GetState(), done.GetResult(), done.GetError())
	}
	return done
}

// bothTasks has two workers each take the two tasks of a job and return
// the same for both.
func bothTasks(first, second *rawWorker) {
	first.t.Helper()
	for _, w := range []*rawWorker{first, second} {
		for range 2 {
			w.returns(w.assignment(), `{"count":7}`)
		}
	}
}

func TestWhatAWorkerReturnsForVerifiedTasksIsCountedWhenTheyAreSettled(t *testing.T) {
	p := startPool(t, runtime.Builtin())
	crew := p.crew("liar", "beta", "gamma")
	liar, beta, gamma := crew[0], crew[1], crew[2]
	if got := p.standing(liar); got != "0 agreed, 0 outvoted, 0 owed" {
		t.Errorf("a worker nothing is known of: %s", got)
	}

	// A task settled against what one worker returned.
	p.outvote(liar, beta, gamma)
	if got := p.standing(liar); got != "0 agreed, 1 outvoted, 10 owed" {
		t.Errorf("the worker that was outvoted: %s", got)
	}
	for _, w := range []*rawWorker{beta, gamma} {
		if got := p.standing(w); got != "1 agreed, 0 outvoted, 0 owed" {
			t.Errorf("a worker that agreed: %s", got)
		}
	}

	// A task that is never settled counts for nobody, whatever was returned.
	id := p.submit(verifiedPrimes(1, 2)).GetJobId()
	first, second := liar.assignment(), beta.assignment()
	liar.returns(first, `{"count":1}`)
	beta.returns(second, `{"count":2}`)
	gamma.returns(gamma.assignment(), `{"count":3}`)
	if failed := p.wait(id); failed.GetState() != pb.JobState_JOB_STATE_FAILED || !strings.Contains(failed.GetError(), "could not be verified") {
		t.Fatalf("the job nobody agreed on %v: %s", failed.GetState(), failed.GetError())
	}
	if got := p.standing(liar) + "; " + p.standing(beta) + "; " + p.standing(gamma); got != "0 agreed, 1 outvoted, 10 owed; 1 agreed, 0 outvoted, 0 owed; 1 agreed, 0 outvoted, 0 owed" {
		t.Errorf("after a task that was never settled: %s", got)
	}

	// An attempt that fails is not a result. The one that follows it is,
	// and a result that agrees is one fewer owed.
	id = p.submit(verifiedPrimes(1, 2)).GetJobId()
	first, second = liar.assignment(), beta.assignment()
	liar.fails(first, "out of memory")
	liar.returns(liar.assignment(), `{"count":7}`)
	beta.returns(second, `{"count":7}`)
	if done := p.wait(id); done.GetState() != pb.JobState_JOB_STATE_SUCCEEDED {
		t.Fatalf("the job with an attempt that failed %v: %s", done.GetState(), done.GetError())
	}
	if got := p.standing(liar) + "; " + p.standing(beta); got != "1 agreed, 1 outvoted, 9 owed; 2 agreed, 0 outvoted, 0 owed" {
		t.Errorf("after an attempt that failed and a task that was settled: %s", got)
	}

	// A job that is not verified counts for nothing, and takes the word of
	// a worker on probation as it takes anyone's.
	id = p.submit(primesJob(pb.ScheduleMode_SCHEDULE_MODE_DISTRIBUTED, 1)).GetJobId()
	liar.returns(liar.assignment(), `{"count":1}`)
	if done := p.wait(id); done.GetState() != pb.JobState_JOB_STATE_SUCCEEDED || string(done.GetResult()) != `{"count":1}` {
		t.Errorf("a job that is not verified, run by a worker on probation: %v, %s", done.GetState(), done.GetResult())
	}
	if got := p.standing(liar); got != "1 agreed, 1 outvoted, 9 owed" {
		t.Errorf("after a job that was not verified: %s", got)
	}
}

func TestAWorkerOnProbationHasItsTasksVerifiedInAJobThatVerifiesAShare(t *testing.T) {
	p := startPool(t, runtime.Builtin())
	// The first two have room for whatever is going, the first the more.
	liar := p.connectRaw(hello("liar", 6, "primes"))
	beta := p.connectRaw(hello("beta", 3, "primes"))
	gamma := p.connectRaw(hello("gamma", 1, "primes"))
	for _, w := range []*rawWorker{liar, beta, gamma} {
		w.welcome()
	}
	// Outvoted in a job that verifies every task, which holds a worker on
	// probation to nothing more than it holds anyone.
	if events := p.events(p.outvote(liar, beta, gamma).GetJobId(), 0); count(events, "task-rechecked") != 0 {
		t.Errorf("the events of a job all of which is verified:\n%s", story(events))
	}

	// The next job verifies one of its two tasks. Both go to the worker on
	// probation, so the other is verified too, and goes to a second worker.
	job := p.submit(spotChecked(2, 0.5))
	id := job.GetJobId()
	if job.GetSpec().GetVerifyShare() != 0.5 || job.GetTasks()[0].GetVerify() != 2 || job.GetTasks()[1].GetVerify() != 2 {
		t.Errorf("the job once its tasks were handed out: %v", job)
	}
	bothTasks(liar, beta)
	done := p.wait(id)
	if done.GetState() != pb.JobState_JOB_STATE_SUCCEEDED || string(done.GetResult()) != `{"count":14}` {
		t.Fatalf("the job %v: %s, %s", done.GetState(), done.GetResult(), done.GetError())
	}
	events := p.events(id, 0)
	told := story(events)
	if count(events, "task-rechecked") != 1 || count(events, "task-started") != 4 || count(events, "task-result") != 4 ||
		!strings.Contains(told, "task-rechecked liar: was outvoted in another job and has 10 more results to agree on before its word is taken alone, so this task is now to be verified by 2 workers") {
		t.Errorf("the job's events:\n%s", told)
	}
	if record := p.recordText(id); strings.Count(record, `"verify":2`) != 3 || strings.Count(record, `"agreed":true`) != 4 {
		t.Errorf("the job's record: %s", record)
	}
	// Both tasks were verified and settled, so both count.
	if got := p.standing(liar) + "; " + p.standing(beta); got != "2 agreed, 1 outvoted, 8 owed; 3 agreed, 0 outvoted, 0 owed" {
		t.Errorf("after the job: %s", got)
	}
}

func TestAWorkerIsOffProbationOnceEnoughOfItsResultsHaveAgreed(t *testing.T) {
	p := startPool(t, runtime.Builtin())
	liar := p.connectRaw(hello("liar", 14, "primes"))
	beta := p.connectRaw(hello("beta", 12, "primes"))
	gamma := p.connectRaw(hello("gamma", 1, "primes"))
	for _, w := range []*rawWorker{liar, beta, gamma} {
		w.welcome()
	}
	p.outvote(liar, beta, gamma)

	// Ten tasks, each for the same two workers, who agree on nine of them
	// for now.
	working := p.submit(verifiedPrimes(10, 2)).GetJobId()
	var mine, others []*pb.TaskAssignment
	for range 10 {
		mine, others = append(mine, liar.assignment()), append(others, beta.assignment())
	}
	for i := range 9 {
		liar.returns(mine[i], `{"count":7}`)
		beta.returns(others[i], `{"count":7}`)
	}
	waitFor(t, func() bool { return p.standing(liar) == "9 agreed, 1 outvoted, 1 owed" })

	// With one still owed, its word alone is not taken.
	checked := p.submit(spotChecked(2, 0.5)).GetJobId()
	waitFor(t, func() bool { return count(p.eventsSoFar(checked), "task-started") == 4 })
	if told := story(p.eventsSoFar(checked)); strings.Count(told, "task-rechecked") != 1 ||
		!strings.Contains(told, "task-rechecked liar: was outvoted in another job and has 1 more result to agree on before its word is taken alone, so this task is now to be verified by 2 workers") {
		t.Errorf("the events of a job handed to a worker with one result owed:\n%s", told)
	}

	// The tenth settles what it owed.
	liar.returns(mine[9], `{"count":7}`)
	beta.returns(others[9], `{"count":7}`)
	if done := p.wait(working); done.GetState() != pb.JobState_JOB_STATE_SUCCEEDED {
		t.Fatalf("the job of ten tasks %v: %s", done.GetState(), done.GetError())
	}
	if got := p.standing(liar); got != "10 agreed, 1 outvoted, 0 owed" {
		t.Errorf("after ten results that agreed: %s", got)
	}
	bothTasks(liar, beta)
	if done := p.wait(checked); done.GetState() != pb.JobState_JOB_STATE_SUCCEEDED {
		t.Fatalf("the job handed out while one was owed %v: %s", done.GetState(), done.GetError())
	}
	if got := p.standing(liar); got != "12 agreed, 1 outvoted, 0 owed" {
		t.Errorf("after two more: %s", got)
	}

	// From here its word is taken again: of the two tasks of the next job
	// one is run once, by it, and that one counts for nothing.
	job := p.submit(spotChecked(2, 0.5))
	checkedTask(t, job)
	for range 2 {
		liar.returns(liar.assignment(), `{"count":7}`)
	}
	beta.returns(beta.assignment(), `{"count":7}`)
	done := p.wait(job.GetJobId())
	if events := p.events(job.GetJobId(), 0); done.GetState() != pb.JobState_JOB_STATE_SUCCEEDED || count(events, "task-rechecked") != 0 || count(events, "task-started") != 3 {
		t.Errorf("the job %v: %s\n%s", done.GetState(), done.GetError(), story(events))
	}
	if got := p.standing(liar) + "; " + p.standing(beta); got != "13 agreed, 1 outvoted, 0 owed; 14 agreed, 0 outvoted, 0 owed" {
		t.Errorf("after a job one of whose tasks was run once: %s", got)
	}
}

func TestATaskVerifiedAfterAllIsCountedOnceWhenItIsSettled(t *testing.T) {
	p := startPool(t, runtime.Builtin())
	liar := p.connectRaw(hello("liar", 5, "primes"))
	beta := p.connectRaw(hello("beta", 2, "primes"))
	gamma := p.connectRaw(hello("gamma", 1, "primes"))
	for _, w := range []*rawWorker{liar, beta, gamma} {
		w.welcome()
	}
	job := p.submit(spotChecked(2, 0.5))
	id := job.GetJobId()
	checked, spared := checkedTask(t, job)
	mine := map[string]*pb.TaskAssignment{}
	for range 2 {
		a := liar.assignment()
		mine[a.GetTaskId()] = a
	}
	second := beta.assignment()

	// The task that is run once succeeds on one result, which is counted
	// for nobody.
	liar.returns(mine[job.GetTasks()[spared].GetTaskId()], `{"count":1}`)
	waitFor(t, func() bool { return p.job(id).GetTasks()[spared].GetState() == pb.TaskState_TASK_STATE_SUCCEEDED })
	if got := p.standing(liar); got != "0 agreed, 0 outvoted, 0 owed" {
		t.Errorf("after a task run once: %s", got)
	}

	// Outvoted on the other, it has the first verified after all, and is
	// outvoted on that too: two results, each counted when its task was
	// settled, and the others' four.
	liar.returns(mine[job.GetTasks()[checked].GetTaskId()], `{"count":1}`)
	beta.returns(second, `{"count":7}`)
	gamma.returns(gamma.assignment(), `{"count":7}`)
	beta.returns(beta.assignment(), `{"count":7}`)
	gamma.returns(gamma.assignment(), `{"count":7}`)
	if done := p.wait(id); done.GetState() != pb.JobState_JOB_STATE_SUCCEEDED || string(done.GetResult()) != `{"count":14}` {
		t.Fatalf("the job %v: %s, %s", done.GetState(), done.GetResult(), done.GetError())
	}
	if got := p.standing(liar) + "; " + p.standing(beta) + "; " + p.standing(gamma); got != "0 agreed, 2 outvoted, 10 owed; 2 agreed, 0 outvoted, 0 owed; 2 agreed, 0 outvoted, 0 owed" {
		t.Errorf("after the job: %s", got)
	}
}

func TestAWorkersStandingSurvivesACoordinatorRestart(t *testing.T) {
	file := filepath.Join(t.TempDir(), "node.db")
	workloads := runtime.NewRegistry(runtime.Primes{})
	db := journalIn(t, file)
	before := startPoolWith(t, workloads, sameStore, db)
	crew := before.crew("liar", "beta", "gamma")
	liar := crew[0]
	before.outvote(liar, crew[1], crew[2])
	if saved, err := db.LoadStandings(); err != nil || len(saved) != 3 {
		t.Fatalf("the standings on record: %+v, %v", saved, err)
	}

	// A crash saves nothing on the way out: what is known of the workers
	// is what was written as each task was settled.
	after := startPoolWith(t, workloads, sameStore, journalIn(t, file))
	ident := before.rawIdents[liar]
	if err := after.access.Admit(ident.ID(), access.Worker, time.Now()); err != nil {
		t.Fatal(err)
	}
	back := after.connectRawAs(after.credentialsFor(ident), hello("liar", 6, "primes"))
	back.id = ident.ID()
	back.welcome()
	if got := after.standing(back); got != "0 agreed, 1 outvoted, 10 owed" {
		t.Errorf("the worker that was outvoted, after the restart: %s", got)
	}

	// It is still on probation, and a newcomer under another key is not.
	beta := after.connectRaw(hello("beta", 3, "primes"))
	beta.welcome()
	if got := after.standing(beta); got != "0 agreed, 0 outvoted, 0 owed" {
		t.Errorf("a worker with a new key: %s", got)
	}
	id := after.submit(spotChecked(2, 0.5)).GetJobId()
	bothTasks(back, beta)
	if done := after.wait(id); done.GetState() != pb.JobState_JOB_STATE_SUCCEEDED {
		t.Fatalf("the job after the restart %v: %s", done.GetState(), done.GetError())
	}
	if told := story(after.events(id, 0)); strings.Count(told, "task-rechecked liar: was outvoted in another job and has 10 more results to agree on") != 1 {
		t.Errorf("the job's events:\n%s", told)
	}
	saved, err := db.LoadStandings()
	if err != nil || len(saved) != 4 {
		t.Fatalf("the standings on record: %+v, %v", saved, err)
	}
	for _, s := range saved {
		if want := (jobmodel.Standing{NodeID: ident.ID(), Agreed: 2, Outvoted: 1, Probation: 8}); s.NodeID == ident.ID() && s != want {
			t.Errorf("the standing on record of the worker that was outvoted: %+v", s)
		}
	}
}

// unsure is a journal that keeps jobs and cannot keep the standing of
// workers, nor, once it is unreadable, recall any.
type unsure struct {
	coordinator.Journal
	unreadable bool
}

var errUnsure = errors.New("the standing table is gone")

func (*unsure) SaveStanding(jobmodel.Standing) error { return errUnsure }

func (u *unsure) LoadStandings() ([]jobmodel.Standing, error) {
	if u.unreadable {
		return nil, errUnsure
	}
	return nil, nil
}

func TestAJobCarriesOnWhenAWorkersStandingCannotBeKept(t *testing.T) {
	journal := &unsure{Journal: journalIn(t, filepath.Join(t.TempDir(), "node.db"))}
	p := startPoolWith(t, runtime.Builtin(), sameStore, journal)
	crew := p.crew("liar", "beta", "gamma")
	p.outvote(crew[0], crew[1], crew[2])
	if logs := p.logs.String(); strings.Count(logs, "could not save a worker's standing") != 3 || !strings.Contains(logs, errUnsure.Error()) {
		t.Errorf("what was logged of the standings that could not be saved:\n%s", logs)
	}
	// It is still held while the coordinator runs.
	if got := p.standing(crew[0]); got != "0 agreed, 1 outvoted, 10 owed" {
		t.Errorf("the worker that was outvoted: %s", got)
	}

	// A coordinator that cannot read back what it knew of its workers does
	// not start as if it had known nothing.
	journal.unreadable = true
	coord := coordinator.New(coordinator.Config{Workloads: runtime.Builtin(), Store: storage.NewMemory(), Journal: journal, Standings: journal, Log: quiet})
	defer coord.Close()
	if _, err := coord.Recover(); !errors.Is(err, errUnsure) {
		t.Errorf("recovering with standings that cannot be read: %v", err)
	}
}

func TestANodeIsListedWithItsStandingOnlyOnceItHasOne(t *testing.T) {
	for want, node := range map[string]*pb.NodeInfo{
		"": {NodeId: "12D3KooWa", Name: "new"},
		"rig: results for verified tasks: 12 agreed, 0 outvoted":                                      {Name: "rig", VerifiedAgreed: 12},
		"liar: results for verified tasks: 3 agreed, 2 outvoted; on probation, 7 more to agree":       {Name: "liar", VerifiedAgreed: 3, VerifiedOutvoted: 2, Probation: 7},
		"12D3KooWb: results for verified tasks: 0 agreed, 1 outvoted; on probation, 10 more to agree": {NodeId: "12D3KooWb", VerifiedOutvoted: 1, Probation: 10},
	} {
		if got := standingOf(node); got != want {
			t.Errorf("a node with %d agreed, %d outvoted and %d owed is shown as %q, want %q", node.GetVerifiedAgreed(), node.GetVerifiedOutvoted(), node.GetProbation(), got, want)
		}
	}
}

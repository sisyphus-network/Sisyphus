package main

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/sisyphus-network/Sisyphus/apps/sisyphusd/coordinator"
	pb "github.com/sisyphus-network/Sisyphus/packages/protocol/sisyphus/v1"
	"github.com/sisyphus-network/Sisyphus/packages/runtime"
	"github.com/sisyphus-network/Sisyphus/packages/sealed"
	"github.com/sisyphus-network/Sisyphus/packages/storage"
)

// primesBelowAHundred is one task's worth of work, and its true answer.
const (
	hundred          = `{"from":0,"to":100}`
	primesBelow100   = `{"count":25}`
	audited          = "the coordinator ran the task itself and got the same result"
	auditedOtherwise = "the coordinator ran the task itself and got"
)

// A job can have the coordinating node run a share of its tasks again
// itself. A worker that returned what the node gets is believed and gains
// by it; one that returned something else is not, and the node's own result
// is the task's. It needs one worker and no more.
func TestTheCoordinatorChecksAWorkersResultByRunningTheTaskItself(t *testing.T) {
	p := startPool(t, runtime.Builtin())
	w := p.connectRaw(hello("only", 1, "primes"))
	w.welcome()
	run := func(returned string) (*pb.Job, string) {
		t.Helper()
		id := p.submit(&pb.JobSpec{Workload: "primes", Params: []byte(hundred), MaxTasks: 1, AuditShare: 1}).GetJobId()
		a := w.assignment()
		w.reply(&pb.TaskResult{TaskId: a.GetTaskId(), Attempt: a.GetAttempt(), Outcome: &pb.TaskResult_Output{Output: []byte(returned)}})
		done := p.wait(id)
		return done, story(p.events(id, 0))
	}

	honest, told := run(primesBelow100)
	if honest.GetState() != pb.JobState_JOB_STATE_SUCCEEDED || string(honest.GetResult()) != primesBelow100 || honest.GetSpec().GetAuditShare() != 1 {
		t.Fatalf("the job: %v %s, audited %v", honest.GetState(), honest.GetResult(), honest.GetSpec().GetAuditShare())
	}
	if !strings.Contains(told, "task-result only: returned a result, which the coordinator checks by running the task itself") || !strings.Contains(told, audited) || strings.Contains(told, "task-disagreed") {
		t.Errorf("the events of a job whose worker was honest:\n%s", told)
	}
	if got := p.standing(w); got != "1 agreed, 0 outvoted, 0 owed" {
		t.Errorf("an honest worker's standing: %s", got)
	}
	// The job's record says it was asked for, as a job's that was not
	// audited does not.
	if record := p.recordText(honest.GetJobId()); !strings.Contains(record, `"audit_share":1`) {
		t.Errorf("the record of an audited job:\n%s", record)
	}
	plain := p.submit(&pb.JobSpec{Workload: "primes", Params: []byte(hundred), MaxTasks: 1}).GetJobId()
	w.returns(w.assignment(), primesBelow100)
	p.wait(plain)
	if record := p.recordText(plain); strings.Contains(record, "audit_share") {
		t.Errorf("the record of a job that was not audited:\n%s", record)
	}

	// The same task, and a worker that says seven.
	lied, told := run(`{"count":7}`)
	if lied.GetState() != pb.JobState_JOB_STATE_SUCCEEDED || string(lied.GetResult()) != primesBelow100 {
		t.Fatalf("the job a worker lied to: %v %s", lied.GetState(), lied.GetResult())
	}
	if !strings.Contains(told, "task-disagreed: only returned ") || !strings.Contains(told, auditedOtherwise) || !strings.Contains(told, "the coordinator's own result is taken, and not only's") {
		t.Errorf("the events of a job whose worker lied:\n%s", told)
	}
	if !strings.Contains(p.logs.String(), "a worker returned another result for a task than this node got by running it itself") {
		t.Error("nothing was logged of the lie")
	}
	if got := p.standing(w); got != "1 agreed, 1 outvoted, 10 owed" {
		t.Errorf("the standing of a worker caught by an audit: %s", got)
	}

	// A share that is next to nothing picks next to no task: the result is
	// taken as it comes, as from a job that asked for no audit.
	id := p.submit(&pb.JobSpec{Workload: "primes", Params: []byte(hundred), MaxTasks: 1, AuditShare: 1e-12}).GetJobId()
	a := w.assignment()
	w.reply(&pb.TaskResult{TaskId: a.GetTaskId(), Attempt: a.GetAttempt(), Outcome: &pb.TaskResult_Output{Output: []byte(`{"count":7}`)}})
	if unchecked := p.wait(id); string(unchecked.GetResult()) != `{"count":7}` || strings.Contains(story(p.events(id, 0)), "the coordinator") {
		t.Errorf("a task that was not picked: %s\n%s", unchecked.GetResult(), story(p.events(id, 0)))
	}
}

// failsHere is a workload that a worker runs and this node cannot.
type failsHere struct{ gated }

func (failsHere) Name() string { return "elsewhere" }

func (failsHere) Execute(context.Context, runtime.Blobs, []byte) ([]byte, error) {
	return nil, errors.New("no such program on this machine")
}

func TestWhatAnAuditIsRefusedForAndWhatItDoesWhenItCannotCompare(t *testing.T) {
	p := startPool(t, runtime.NewRegistry(runtime.Primes{}, runtime.Graph{}, failsHere{}))
	for name, tt := range map[string]struct {
		spec *pb.JobSpec
		code codes.Code
		want string
	}{
		"a share above one":     {&pb.JobSpec{Workload: "primes", Params: []byte(hundred), AuditShare: 2}, codes.InvalidArgument, "from 0 to 1"},
		"a share below nothing": {&pb.JobSpec{Workload: "primes", Params: []byte(hundred), AuditShare: -0.5}, codes.InvalidArgument, "from 0 to 1"},
		"with verify as well":   {&pb.JobSpec{Workload: "primes", Params: []byte(hundred), AuditShare: 0.5, Verify: 2}, codes.InvalidArgument, "a job asks for one or the other"},
	} {
		if _, err := p.client.SubmitJob(p.ctx, &pb.SubmitJobRequest{Spec: tt.spec}); status.Code(err) != tt.code || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("%s: %v", name, err)
		}
	}

	// A task this node fails to run itself has nothing to be set beside:
	// the worker's result stands, and the job says it went unchecked.
	w := p.connectRaw(hello("only", 1, "elsewhere", "primes"))
	w.welcome()
	id := p.submit(&pb.JobSpec{Workload: "elsewhere", MaxTasks: 1, AuditShare: 1}).GetJobId()
	a := w.assignment()
	w.reply(&pb.TaskResult{TaskId: a.GetTaskId(), Attempt: a.GetAttempt(), Outcome: &pb.TaskResult_Output{Output: []byte("ok")}})
	if done := p.wait(id); done.GetState() != pb.JobState_JOB_STATE_SUCCEEDED {
		t.Fatalf("the job: %v %s", done.GetState(), done.GetError())
	}
	if told := story(p.events(id, 0)); !strings.Contains(told, "could not run the task itself to check it (no such program on this machine), so the result is taken unchecked") {
		t.Errorf("the events of a job this node could not check:\n%s", told)
	}
	if got := p.standing(w); got != "0 agreed, 0 outvoted, 0 owed" {
		t.Errorf("the standing of a worker whose result could not be checked: %s", got)
	}

	// A job made of steps hands the audit on to each of them.
	graph := p.submit(&pb.JobSpec{Workload: "graph", Params: []byte(`{"steps":[{"name":"count","workload":"primes","params":` + hundred + `}]}`), AuditShare: 1}).GetJobId()
	step := w.assignment()
	w.reply(&pb.TaskResult{TaskId: step.GetTaskId(), Attempt: step.GetAttempt(), Outcome: &pb.TaskResult_Output{Output: []byte(primesBelow100)}})
	if done := p.wait(graph); done.GetState() != pb.JobState_JOB_STATE_SUCCEEDED {
		t.Fatalf("the graph: %v %s", done.GetState(), done.GetError())
	}
	if told := story(p.events(step.GetJobId(), 0)); !strings.Contains(told, audited) {
		t.Errorf("the events of an audited graph's step:\n%s", told)
	}
}

// A node that is no worker for a workload cannot run it again, and says so
// when asked to audit it.
func TestANodeThatCannotRunAWorkloadDoesNotAuditIt(t *testing.T) {
	store := storage.NewMemory()
	for name, audits := range map[string]*runtime.Registry{"a node that runs nothing": nil, "a node that runs other things": runtime.NewRegistry(runtime.WordCount{})} {
		coord := coordinator.New(coordinator.Config{Workloads: runtime.Builtin(), Audits: audits, Store: store, Log: quiet})
		_, err := coord.Submit(context.Background(), &pb.JobSpec{Workload: "primes", Params: []byte(hundred), AuditShare: 0.5})
		if status.Code(err) != codes.FailedPrecondition || !strings.Contains(err.Error(), "it cannot run primes: it is not a worker for that workload") {
			t.Errorf("%s: %v", name, err)
		}
		coord.Close()
	}
}

// A task whose job is cancelled while the node is running it again is left
// alone when the node has finished: it is no longer that result's to settle.
func TestAnAuditThatOutlastsItsJobSettlesNothing(t *testing.T) {
	g := gated{started: make(chan struct{}, 16), release: make(chan struct{})}
	p := startPool(t, runtime.NewRegistry(g))
	w := p.connectRaw(hello("only", 1, "gated"))
	w.welcome()
	id := p.submit(&pb.JobSpec{Workload: "gated", MaxTasks: 1, AuditShare: 1}).GetJobId()
	a := w.assignment()
	w.reply(&pb.TaskResult{TaskId: a.GetTaskId(), Attempt: a.GetAttempt(), Outcome: &pb.TaskResult_Output{Output: []byte("ok")}})
	<-g.started // the node has begun running the task itself
	if _, err := p.client.CancelJob(p.ctx, &pb.CancelJobRequest{JobId: id}); err != nil {
		t.Fatal(err)
	}
	close(g.release)
	if done := p.wait(id); done.GetState() != pb.JobState_JOB_STATE_CANCELLED {
		t.Errorf("the job: %v", done.GetState())
	}
	if told := story(p.eventsSoFar(id)); strings.Contains(told, "task-succeeded") {
		t.Errorf("a cancelled job's task was settled by its audit:\n%s", told)
	}
}

// A private job is audited as any other: the node holds the job's key, and
// seals what it stores as the job's workers do.
func TestAPrivateJobIsAuditedLikeAnyOther(t *testing.T) {
	p := startPool(t, runtime.Builtin())
	p.startWorker("a", 2)
	p.waitForWorkers(1)
	key, text := sealed.NewKey(), sampleText()
	input := p.sealedFile(key, text)
	job := p.wait(p.submit(&pb.JobSpec{Workload: "wordcount", Params: []byte(`{"input":"` + input.String() + `"}`), MaxTasks: 2, Key: key[:], AuditShare: 1}).GetJobId())
	if job.GetState() != pb.JobState_JOB_STATE_SUCCEEDED {
		t.Fatalf("the job: %v %s", job.GetState(), job.GetError())
	}
	if _, want := wordCountLocally(t, text); p.unsealedTable(job, key) != want {
		t.Error("an audited private job counted the words wrong")
	}
	if told := story(p.events(job.GetJobId(), 0)); strings.Count(told, audited) != 2 || strings.Contains(told, "task-disagreed") {
		t.Errorf("the events of an audited private job:\n%s", told)
	}
}

// A standing that cannot be kept does not stop the job it was earned in.
func TestAnAuditedJobCarriesOnWhenAStandingCannotBeKept(t *testing.T) {
	journal := &unsure{Journal: journalIn(t, filepath.Join(t.TempDir(), "node.db"))}
	p := startPoolWith(t, runtime.Builtin(), sameStore, journal)
	w := p.connectRaw(hello("only", 1, "primes"))
	w.welcome()
	id := p.submit(&pb.JobSpec{Workload: "primes", Params: []byte(hundred), MaxTasks: 1, AuditShare: 1}).GetJobId()
	a := w.assignment()
	w.reply(&pb.TaskResult{TaskId: a.GetTaskId(), Attempt: a.GetAttempt(), Outcome: &pb.TaskResult_Output{Output: []byte(primesBelow100)}})
	if done := p.wait(id); done.GetState() != pb.JobState_JOB_STATE_SUCCEEDED {
		t.Fatalf("the job: %v %s", done.GetState(), done.GetError())
	}
	if !strings.Contains(p.logs.String(), "could not save a worker's standing") {
		t.Error("nothing was logged of the standing that could not be kept")
	}
}

// From the command line, on a node that is its own worker.
func TestAJobIsAuditedFromTheCommandLine(t *testing.T) {
	addr, _ := startNode(t, "--slots", "2")
	out := mustCLI(t, "job", "submit", "--addr", addr, "--params", hundred, "--tasks", "2", "--audit-share", "1")
	if !strings.Contains(out, primesBelow100) {
		t.Fatalf("an audited job:\n%s", out)
	}
	id := strings.TrimSuffix(strings.Fields(out[strings.Index(out, "job "):])[1], ":")
	if logs := mustCLI(t, "job", "logs", "--addr", addr, id); strings.Count(logs, audited) != 2 {
		t.Errorf("the job's log:\n%s", logs)
	}
}

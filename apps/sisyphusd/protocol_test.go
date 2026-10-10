package main

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/status"

	"github.com/sisyphus-network/Sisyphus/apps/sisyphusd/access"
	pb "github.com/sisyphus-network/Sisyphus/packages/protocol/sisyphus/v1"
	"github.com/sisyphus-network/Sisyphus/packages/runtime"
)

// These tests speak the worker protocol to a real coordinator by hand, to
// see how it treats workers that are broken, buggy or dishonest.

type rawWorker struct {
	t *testing.T
	// id is the node ID the worker's key gives it.
	id     string
	stream grpc.BidiStreamingClient[pb.WorkerMessage, pb.CoordinatorMessage]
	cancel context.CancelFunc
}

// connectRaw opens a worker stream as a newly admitted worker and sends
// first, if it is not nil.
func (p *pool) connectRaw(first *pb.WorkerMessage) *rawWorker {
	p.t.Helper()
	ident, creds := p.admit(access.Worker)
	w := p.connectRawAs(creds, first)
	w.id = ident.ID()
	p.rawIdents[w] = ident
	return w
}

// connectRawAs opens a worker stream with the given credentials, whoever
// they belong to, and sends first, if it is not nil.
func (p *pool) connectRawAs(creds credentials.TransportCredentials, first *pb.WorkerMessage) *rawWorker {
	p.t.Helper()
	conn, err := grpc.NewClient(p.addr, grpc.WithTransportCredentials(creds))
	if err != nil {
		p.t.Fatal(err)
	}
	p.t.Cleanup(func() { conn.Close() })
	ctx, cancel := context.WithCancel(p.ctx)
	p.t.Cleanup(cancel)
	stream, err := pb.NewCoordinatorServiceClient(conn).Connect(ctx)
	if err != nil {
		p.t.Fatal(err)
	}
	w := &rawWorker{t: p.t, stream: stream, cancel: cancel}
	if first != nil {
		w.send(first)
	}
	return w
}

func hello(name string, slots uint32, workloads ...string) *pb.WorkerMessage {
	return &pb.WorkerMessage{Kind: &pb.WorkerMessage_Hello{Hello: &pb.Hello{
		Name:         name,
		// It takes a private job's keys as they are now given.
		Capabilities: &pb.NodeCapabilities{TaskSlots: slots, Workloads: workloads, Labels: []string{runtime.SealingLabel}},
	}}}
}

func (w *rawWorker) send(msg *pb.WorkerMessage) {
	w.t.Helper()
	if err := w.stream.Send(msg); err != nil {
		w.t.Fatalf("send: %v", err)
	}
}

func (w *rawWorker) reply(result *pb.TaskResult) {
	w.t.Helper()
	w.send(&pb.WorkerMessage{Kind: &pb.WorkerMessage_TaskResult{TaskResult: result}})
}

// welcome reads the coordinator's first message, which must be a Welcome.
func (w *rawWorker) welcome() {
	w.t.Helper()
	msg, err := w.stream.Recv()
	if err != nil || msg.GetWelcome() == nil {
		w.t.Fatalf("expected a Welcome, got %v, %v", msg, err)
	}
}

// assignment reads the next task the coordinator hands this worker.
func (w *rawWorker) assignment() *pb.TaskAssignment {
	w.t.Helper()
	msg, err := w.stream.Recv()
	if err != nil || msg.GetAssignment() == nil {
		w.t.Fatalf("expected an assignment, got %v, %v", msg, err)
	}
	return msg.GetAssignment()
}

func TestCoordinatorRejectsWorkersThatDoNotIntroduceThemselves(t *testing.T) {
	p := startPool(t, runtime.Builtin())
	tests := []struct {
		name  string
		first *pb.WorkerMessage
		want  codes.Code
	}{
		{"says nothing and hangs up", nil, codes.Unknown},
		{"starts with a heartbeat", &pb.WorkerMessage{Kind: &pb.WorkerMessage_Heartbeat{Heartbeat: &pb.Heartbeat{}}}, codes.InvalidArgument},
		{"claims a million task slots", hello("greedy", 1_000_000, "primes"), codes.InvalidArgument},
	}
	for _, tt := range tests {
		w := p.connectRaw(tt.first)
		w.stream.CloseSend()
		if _, err := w.stream.Recv(); status.Code(err) != tt.want {
			t.Errorf("%s: error %v, want code %v", tt.name, err, tt.want)
		}
	}
	p.waitForWorkers(0)
}

func TestWorkerThatStatesNoCapabilitiesIsListedButGivenNoWork(t *testing.T) {
	p := startPool(t, runtime.Builtin())
	w := p.connectRaw(&pb.WorkerMessage{Kind: &pb.WorkerMessage_Hello{Hello: &pb.Hello{Name: "bare"}}})
	w.welcome()

	listed, err := p.client.ListNodes(p.ctx, &pb.ListNodesRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if nodes := listed.GetNodes(); len(nodes) != 1 || nodes[0].GetName() != "bare" || nodes[0].GetNodeId() != w.id || nodes[0].GetCapabilities().GetTaskSlots() != 0 {
		t.Errorf("nodes: %v", nodes)
	}
	job := p.submit(primesJob(pb.ScheduleMode_SCHEDULE_MODE_DISTRIBUTED, 1))
	got, err := p.client.GetJob(p.ctx, &pb.GetJobRequest{JobId: job.GetJobId()})
	if err != nil || got.GetJob().GetState() != pb.JobState_JOB_STATE_PENDING {
		t.Errorf("job is %v (%v), want it left pending", got.GetJob().GetState(), err)
	}
}

func TestWorkerLeavesThePoolWhenItHangsUpOrIsCutOff(t *testing.T) {
	p := startPool(t, runtime.Builtin())

	polite := p.connectRaw(hello("polite", 1, "primes"))
	polite.welcome()
	p.waitForWorkers(1)
	polite.stream.CloseSend()
	if _, err := polite.stream.Recv(); !errors.Is(err, io.EOF) {
		t.Errorf("after hanging up cleanly: %v, want a clean end of stream", err)
	}
	p.waitForWorkers(0)

	abrupt := p.connectRaw(hello("abrupt", 1, "primes"))
	abrupt.welcome()
	p.waitForWorkers(1)
	abrupt.cancel()
	p.waitForWorkers(0)
}

func TestCoordinatorIgnoresAResultForATaskTheWorkerWasNotGiven(t *testing.T) {
	p := startPool(t, runtime.Builtin())
	w := p.connectRaw(hello("confused", 1, "primes"))
	w.welcome()
	w.reply(&pb.TaskResult{TaskId: "no-such-job/0", Attempt: 1, Outcome: &pb.TaskResult_Output{Output: []byte(`{"count":1}`)}})

	// The worker is still in good standing: it is given the next job's task
	// and its answer is taken.
	job := p.submit(primesJob(pb.ScheduleMode_SCHEDULE_MODE_DISTRIBUTED, 1))
	task := w.assignment()
	// Right task, wrong attempt: also ignored.
	w.reply(&pb.TaskResult{TaskId: task.GetTaskId(), Attempt: task.GetAttempt() + 5, Outcome: &pb.TaskResult_Output{Output: []byte(`{"count":2}`)}})
	w.reply(&pb.TaskResult{TaskId: task.GetTaskId(), Attempt: task.GetAttempt(), Outcome: &pb.TaskResult_Output{Output: []byte(`{"count":3}`)}})

	if done := p.wait(job.GetJobId()); string(done.GetResult()) != `{"count":3}` {
		t.Errorf("result %s (%s), want the one valid reply's count", done.GetResult(), done.GetError())
	}
}

func TestResultWithNoOutcomeCountsAsAFailedAttempt(t *testing.T) {
	p := startPool(t, runtime.Builtin())
	w := p.connectRaw(hello("mute", 1, "primes"))
	w.welcome()
	job := p.submit(primesJob(pb.ScheduleMode_SCHEDULE_MODE_DISTRIBUTED, 1))
	for attempt := uint32(1); attempt <= 3; attempt++ {
		task := w.assignment()
		if task.GetAttempt() != attempt {
			t.Fatalf("assignment is attempt %d, want %d", task.GetAttempt(), attempt)
		}
		w.reply(&pb.TaskResult{TaskId: task.GetTaskId(), Attempt: task.GetAttempt()})
	}
	done := p.wait(job.GetJobId())
	if want := "task 0 failed after 3 attempts: worker reported no outcome"; done.GetError() != want {
		t.Errorf("error %q, want %q", done.GetError(), want)
	}
}

func TestWorkIsNotGivenToAWorkerThatCannotRunIt(t *testing.T) {
	p := startPool(t, runtime.Builtin())
	other := p.connectRaw(hello("other", 8, "alchemy"))
	other.welcome()
	p.startWorker("able", 2)
	p.waitForWorkers(2)

	// With no task count given, the split follows the slots of workers able
	// to run the workload: two, not ten.
	job := p.wait(p.submit(primesJob(pb.ScheduleMode_SCHEDULE_MODE_DISTRIBUTED, 0)).GetJobId())
	if len(job.GetTasks()) != 2 {
		t.Errorf("job split into %d tasks, want 2", len(job.GetTasks()))
	}
	for _, task := range job.GetTasks() {
		if task.GetNodeName() != "able" {
			t.Errorf("task %d ran on %q", task.GetIndex(), task.GetNodeName())
		}
	}
	if string(job.GetResult()) != primesBelowTwoMillion {
		t.Errorf("result %s (%s)", job.GetResult(), job.GetError())
	}
}

// partlyBroken is a workload whose first task always fails and whose other
// tasks block until released.
type partlyBroken struct {
	broken
	blocked chan struct{}
	release chan struct{}
}

func (partlyBroken) Name() string { return "partly-broken" }

func (partlyBroken) Split(context.Context, runtime.Blobs, []byte, int) ([][]byte, error) {
	return [][]byte{[]byte("fail"), []byte("block"), []byte("block")}, nil
}

func (w partlyBroken) Execute(ctx context.Context, _ runtime.Blobs, payload []byte) ([]byte, error) {
	if string(payload) == "fail" {
		return nil, errors.New("disk on fire")
	}
	w.blocked <- struct{}{}
	select {
	case <-w.release:
		return []byte("late"), nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func startPartlyBrokenJob(t *testing.T) (p *pool, w partlyBroken, stopWorker func(), failed *pb.Job) {
	t.Helper()
	w = partlyBroken{blocked: make(chan struct{}, 2), release: make(chan struct{})}
	p = startPool(t, runtime.NewRegistry(w, runtime.Primes{}))
	stopWorker = p.startWorker("a", 3)
	p.waitForWorkers(1)
	job := p.submit(&pb.JobSpec{Workload: "partly-broken"})
	<-w.blocked
	<-w.blocked
	failed = p.wait(job.GetJobId())
	if failed.GetState() != pb.JobState_JOB_STATE_FAILED {
		t.Fatalf("job is %v, want failed", failed.GetState())
	}
	return p, w, stopWorker, failed
}

func TestResultsArrivingAfterAJobHasFailedAreIgnored(t *testing.T) {
	p, w, _, failed := startPartlyBrokenJob(t)
	close(w.release)

	// Once the late results are in, the worker's slots are free again and
	// the failed job is unchanged.
	other := p.wait(p.submit(primesJob(pb.ScheduleMode_SCHEDULE_MODE_DISTRIBUTED, 3)).GetJobId())
	if string(other.GetResult()) != primesBelowTwoMillion {
		t.Errorf("a job after the failed one: %s (%s)", other.GetResult(), other.GetError())
	}
	got, err := p.client.GetJob(p.ctx, &pb.GetJobRequest{JobId: failed.GetJobId()})
	if err != nil || got.GetJob().GetState() != pb.JobState_JOB_STATE_FAILED || got.GetJob().GetError() != failed.GetError() {
		t.Errorf("the failed job changed: %v, %v", got.GetJob(), err)
	}
}

func TestWorkerLeavingAfterItsJobFailedChangesNothing(t *testing.T) {
	p, _, stopWorker, failed := startPartlyBrokenJob(t)
	stopWorker()
	p.waitForWorkers(0)

	got, err := p.client.GetJob(p.ctx, &pb.GetJobRequest{JobId: failed.GetJobId()})
	if err != nil || got.GetJob().GetError() != failed.GetError() {
		t.Errorf("the failed job changed: %v, %v", got.GetJob(), err)
	}
	for _, task := range got.GetJob().GetTasks()[1:] {
		if task.GetAttempt() != 1 {
			t.Errorf("task %d of a failed job was retried", task.GetIndex())
		}
	}
}

type splitsIntoNothing struct{ broken }

func (splitsIntoNothing) Name() string { return "nothing" }

func (splitsIntoNothing) Split(context.Context, runtime.Blobs, []byte, int) ([][]byte, error) {
	return nil, nil
}

func TestSubmissionsTheCoordinatorCannotSchedule(t *testing.T) {
	p := startPool(t, runtime.NewRegistry(splitsIntoNothing{}, runtime.Primes{}))
	_, err := p.client.SubmitJob(p.ctx, &pb.SubmitJobRequest{Spec: &pb.JobSpec{Workload: "nothing"}})
	if status.Code(err) != codes.Internal {
		t.Errorf("a workload that splits into no tasks: %v, want Internal", err)
	}
	_, err = p.client.SubmitJob(p.ctx, &pb.SubmitJobRequest{Spec: primesJob(pb.ScheduleMode(99), 1)})
	if status.Code(err) != codes.InvalidArgument || !strings.Contains(err.Error(), "unknown schedule mode 99") {
		t.Errorf("an unknown schedule mode: %v", err)
	}
}

func TestWatchEndsWhenTheWatcherFailsOrGivesUp(t *testing.T) {
	p := startPool(t, runtime.Builtin())
	pending := p.submit(primesJob(pb.ScheduleMode_SCHEDULE_MODE_DISTRIBUTED, 1)) // no workers, so it stays pending

	gone := errors.New("watcher went away")
	err := p.coord.Watch(p.ctx, pending.GetJobId(), func(*pb.Job) error { return gone })
	if !errors.Is(err, gone) {
		t.Errorf("Watch returned %v, want the watcher's own error", err)
	}

	ctx, cancel := context.WithCancel(p.ctx)
	seen := 0
	err = p.coord.Watch(ctx, pending.GetJobId(), func(*pb.Job) error {
		seen++
		cancel()
		return nil
	})
	if status.Code(err) != codes.Canceled || seen != 1 {
		t.Errorf("Watch returned %v after %d update(s), want Canceled after 1", err, seen)
	}
}

func TestCoordinatorTracksWhenItLastHeardFromAWorker(t *testing.T) {
	p := startPool(t, runtime.Builtin())
	p.heartbeat = 10 * time.Millisecond
	p.startWorker("a", 1)
	p.waitForWorkers(1)

	for {
		listed, err := p.client.ListNodes(p.ctx, &pb.ListNodesRequest{})
		if err != nil {
			t.Fatal(err)
		}
		node := listed.GetNodes()[0]
		if node.GetLastSeenAt().AsTime().After(node.GetConnectedAt().AsTime()) {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestWorkersAreToldWhichSwarmKeyIsCurrent(t *testing.T) {
	p := startPool(t, runtime.Builtin())
	early := p.connectRaw(hello("early", 1, "primes"))
	msg, err := early.stream.Recv()
	if err != nil || msg.GetWelcome() == nil || msg.GetWelcome().GetSwarmFingerprint() != "" {
		t.Fatalf("welcome from a pool with no private network: %v, %v", msg, err)
	}

	p.coord.AnnounceSwarm("first-key")
	// A worker already connected is told at once.
	msg, err = early.stream.Recv()
	if err != nil || msg.GetSwarmUpdate().GetSwarmFingerprint() != "first-key" {
		t.Errorf("a connected worker received %v, %v; want word of the key", msg, err)
	}
	// One that connects afterwards is told as it is welcomed.
	late := p.connectRaw(hello("late", 1, "primes"))
	msg, err = late.stream.Recv()
	if err != nil || msg.GetWelcome().GetSwarmFingerprint() != "first-key" {
		t.Errorf("a later worker's welcome: %v, %v", msg, err)
	}

	// Several changes in a row all arrive, in order, though nothing is
	// reading in between.
	for _, key := range []string{"second-key", "third-key", "fourth-key"} {
		p.coord.AnnounceSwarm(key)
	}
	for _, want := range []string{"second-key", "third-key", "fourth-key"} {
		msg, err = early.stream.Recv()
		if err != nil || msg.GetSwarmUpdate().GetSwarmFingerprint() != want {
			t.Errorf("received %v, %v; want word of %s", msg, err, want)
		}
	}
}

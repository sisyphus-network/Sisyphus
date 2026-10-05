package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"strconv"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"

	"github.com/excho0/Sisyphus/apps/sisyphusd/api"
	"github.com/excho0/Sisyphus/apps/sisyphusd/coordinator"
	"github.com/excho0/Sisyphus/apps/sisyphusd/worker"
	pb "github.com/excho0/Sisyphus/packages/protocol/sisyphus/v1"
	"github.com/excho0/Sisyphus/packages/runtime"
	"github.com/excho0/Sisyphus/packages/storage"
)

// These tests run a real coordinator and real workers in one process, talking
// gRPC over loopback TCP.

const primesBelowTwoMillion = `{"count":148933}`

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

type pool struct {
	t         *testing.T
	ctx       context.Context
	addr      string
	client    pb.NodeServiceClient
	workloads *runtime.Registry
}

func startPool(t *testing.T, workloads *runtime.Registry) *pool {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := api.NewServer(coordinator.New("coordinator", workloads, quiet), storage.NewMemory())
	go srv.Serve(lis)
	t.Cleanup(srv.Stop)

	conn, err := grpc.NewClient(lis.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	return &pool{t: t, ctx: ctx, addr: lis.Addr().String(), client: pb.NewNodeServiceClient(conn), workloads: workloads}
}

// startWorker runs a worker until the test ends or the returned stop function
// is called, whichever comes first. stop waits for the worker to exit.
func (p *pool) startWorker(id string, slots int) (stop func()) {
	p.t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	w := &worker.Worker{NodeID: id, Coordinator: p.addr, Slots: slots, Workloads: p.workloads, Log: quiet}
	go func() {
		defer close(done)
		w.Run(ctx)
	}()
	stop = func() {
		cancel()
		<-done
	}
	p.t.Cleanup(stop)
	return stop
}

func (p *pool) waitForWorkers(n int) {
	p.t.Helper()
	for {
		listed, err := p.client.ListNodes(p.ctx, &pb.ListNodesRequest{})
		if err != nil {
			p.t.Fatalf("waiting for %d workers: %v", n, err)
		}
		if len(listed.GetNodes()) == n {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func (p *pool) submit(spec *pb.JobSpec) *pb.Job {
	p.t.Helper()
	submitted, err := p.client.SubmitJob(p.ctx, &pb.SubmitJobRequest{Spec: spec})
	if err != nil {
		p.t.Fatal(err)
	}
	return submitted.GetJob()
}

// wait follows a job to its terminal state.
func (p *pool) wait(jobID string) *pb.Job {
	p.t.Helper()
	stream, err := p.client.WatchJob(p.ctx, &pb.WatchJobRequest{JobId: jobID})
	if err != nil {
		p.t.Fatal(err)
	}
	var last *pb.Job
	for {
		event, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return last
		}
		if err != nil {
			p.t.Fatalf("watching job %s: %v", jobID, err)
		}
		last = event.GetJob()
	}
}

func primesJob(mode pb.ScheduleMode, tasks uint32) *pb.JobSpec {
	return &pb.JobSpec{Workload: "primes", Params: []byte(`{"from":0,"to":2000000}`), Mode: mode, MaxTasks: tasks}
}

func TestDistributedJobRunsAcrossWorkers(t *testing.T) {
	p := startPool(t, runtime.Builtin())
	for _, id := range []string{"a", "b", "c"} {
		p.startWorker(id, 2)
	}
	p.waitForWorkers(3)

	job := p.wait(p.submit(primesJob(pb.ScheduleMode_SCHEDULE_MODE_DISTRIBUTED, 12)).GetJobId())

	if job.GetState() != pb.JobState_JOB_STATE_SUCCEEDED {
		t.Fatalf("job %v: %s", job.GetState(), job.GetError())
	}
	if string(job.GetResult()) != primesBelowTwoMillion {
		t.Errorf("result = %s, want %s", job.GetResult(), primesBelowTwoMillion)
	}
	if len(job.GetTasks()) != 12 {
		t.Errorf("job ran as %d tasks, want 12", len(job.GetTasks()))
	}
	ranOn := make(map[string]int)
	for _, task := range job.GetTasks() {
		if task.GetState() != pb.TaskState_TASK_STATE_SUCCEEDED || task.GetAttempt() != 1 {
			t.Errorf("task %d: %v after %d attempt(s)", task.GetIndex(), task.GetState(), task.GetAttempt())
		}
		ranOn[task.GetNodeId()]++
	}
	if len(ranOn) != 3 {
		t.Errorf("tasks ran on %v, want all three workers used", ranOn)
	}
	if job.GetFinishedAt() == nil {
		t.Error("finished job has no finish time")
	}
}

func TestDistributedJobDefaultsToOneTaskPerSlot(t *testing.T) {
	p := startPool(t, runtime.Builtin())
	p.startWorker("a", 2)
	p.startWorker("b", 3)
	p.waitForWorkers(2)

	job := p.wait(p.submit(primesJob(pb.ScheduleMode_SCHEDULE_MODE_DISTRIBUTED, 0)).GetJobId())

	if len(job.GetTasks()) != 5 {
		t.Errorf("job ran as %d tasks, want 5", len(job.GetTasks()))
	}
	if string(job.GetResult()) != primesBelowTwoMillion {
		t.Errorf("result = %s, want %s", job.GetResult(), primesBelowTwoMillion)
	}
}

func TestFullWorkerJobRunsAsOneTask(t *testing.T) {
	p := startPool(t, runtime.Builtin())
	p.startWorker("a", 4)
	p.startWorker("b", 4)
	p.waitForWorkers(2)

	// max_tasks is ignored in full-worker mode.
	job := p.wait(p.submit(primesJob(pb.ScheduleMode_SCHEDULE_MODE_FULL_WORKER, 8)).GetJobId())

	if len(job.GetTasks()) != 1 {
		t.Fatalf("job ran as %d tasks, want 1", len(job.GetTasks()))
	}
	if string(job.GetResult()) != primesBelowTwoMillion {
		t.Errorf("result = %s, want %s", job.GetResult(), primesBelowTwoMillion)
	}
}

func TestJobWaitsForAWorker(t *testing.T) {
	p := startPool(t, runtime.Builtin())
	submitted := p.submit(primesJob(pb.ScheduleMode_SCHEDULE_MODE_DISTRIBUTED, 3))
	if submitted.GetState() != pb.JobState_JOB_STATE_PENDING {
		t.Fatalf("job with no workers is %v, want pending", submitted.GetState())
	}

	p.startWorker("late", 1)

	job := p.wait(submitted.GetJobId())
	if string(job.GetResult()) != primesBelowTwoMillion {
		t.Errorf("result = %s (%s), want %s", job.GetResult(), job.GetError(), primesBelowTwoMillion)
	}
}

// gated is a workload whose tasks block until released, so a test can act
// while they are running.
type gated struct {
	started chan struct{}
	release chan struct{}
}

func (gated) Name() string { return "gated" }

func (gated) Split(_ []byte, parts int) ([][]byte, error) {
	return make([][]byte, parts), nil
}

func (g gated) Execute(ctx context.Context, _ []byte) ([]byte, error) {
	g.started <- struct{}{}
	select {
	case <-g.release:
		return []byte("ok"), nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (gated) Aggregate(outputs [][]byte) ([]byte, error) {
	return []byte(strconv.Itoa(len(outputs))), nil
}

func TestTasksMoveToAnotherWorkerWhenTheirsDies(t *testing.T) {
	g := gated{started: make(chan struct{}, 16), release: make(chan struct{})}
	p := startPool(t, runtime.NewRegistry(g))

	stopDoomed := p.startWorker("doomed", 2)
	p.waitForWorkers(1)
	submitted := p.submit(&pb.JobSpec{Workload: "gated", MaxTasks: 2})
	<-g.started
	<-g.started

	// Kill the worker while it holds both tasks, then bring up a replacement.
	stopDoomed()
	p.waitForWorkers(0)
	p.startWorker("survivor", 2)
	<-g.started
	<-g.started
	close(g.release)

	job := p.wait(submitted.GetJobId())
	if job.GetState() != pb.JobState_JOB_STATE_SUCCEEDED {
		t.Fatalf("job %v: %s", job.GetState(), job.GetError())
	}
	if string(job.GetResult()) != "2" {
		t.Errorf("result = %s, want 2", job.GetResult())
	}
	for _, task := range job.GetTasks() {
		if task.GetNodeId() != "survivor" || task.GetAttempt() != 2 {
			t.Errorf("task %d finished on %q at attempt %d, want survivor at attempt 2", task.GetIndex(), task.GetNodeId(), task.GetAttempt())
		}
	}
}

type broken struct{}

func (broken) Name() string                                { return "broken" }
func (broken) Split(_ []byte, parts int) ([][]byte, error) { return make([][]byte, parts), nil }
func (broken) Aggregate([][]byte) ([]byte, error)          { return nil, errors.New("unreachable") }
func (broken) Execute(context.Context, []byte) ([]byte, error) {
	return nil, errors.New("disk on fire")
}

func TestJobFailsAfterATaskExhaustsItsAttempts(t *testing.T) {
	p := startPool(t, runtime.NewRegistry(broken{}))
	p.startWorker("a", 1)
	p.waitForWorkers(1)

	job := p.wait(p.submit(&pb.JobSpec{Workload: "broken", MaxTasks: 1}).GetJobId())

	if job.GetState() != pb.JobState_JOB_STATE_FAILED {
		t.Fatalf("job is %v, want failed", job.GetState())
	}
	if want := "task 0 failed after 3 attempts: disk on fire"; job.GetError() != want {
		t.Errorf("error = %q, want %q", job.GetError(), want)
	}
	if task := job.GetTasks()[0]; task.GetState() != pb.TaskState_TASK_STATE_FAILED || task.GetAttempt() != 3 {
		t.Errorf("task is %v after %d attempts", task.GetState(), task.GetAttempt())
	}
}

type panicky struct{ broken }

func (panicky) Name() string                                    { return "panicky" }
func (panicky) Execute(context.Context, []byte) ([]byte, error) { panic("oh no") }

func TestPanickingWorkloadFailsTheTaskNotTheWorker(t *testing.T) {
	p := startPool(t, runtime.NewRegistry(panicky{}))
	p.startWorker("a", 1)
	p.waitForWorkers(1)

	job := p.wait(p.submit(&pb.JobSpec{Workload: "panicky", MaxTasks: 1}).GetJobId())

	if want := "task 0 failed after 3 attempts: workload panicked: oh no"; job.GetError() != want {
		t.Errorf("error = %q, want %q", job.GetError(), want)
	}
	p.waitForWorkers(1)
}

func TestSecondWorkerWithSameNodeIDIsRejected(t *testing.T) {
	p := startPool(t, runtime.Builtin())
	p.startWorker("twin", 1)
	p.waitForWorkers(1)

	twin := &worker.Worker{NodeID: "twin", Coordinator: p.addr, Slots: 1, Workloads: p.workloads, Log: quiet}
	err := twin.Run(p.ctx)
	if status.Code(errors.Unwrap(err)) != codes.AlreadyExists {
		t.Errorf("second worker returned %v, want an AlreadyExists rejection", err)
	}
}

func TestInvalidRequestsAreRejected(t *testing.T) {
	p := startPool(t, runtime.Builtin())
	tests := []struct {
		name string
		call func() error
		want codes.Code
	}{
		{"unknown workload", func() error {
			_, err := p.client.SubmitJob(p.ctx, &pb.SubmitJobRequest{Spec: &pb.JobSpec{Workload: "nope"}})
			return err
		}, codes.InvalidArgument},
		{"bad params", func() error {
			_, err := p.client.SubmitJob(p.ctx, &pb.SubmitJobRequest{Spec: &pb.JobSpec{Workload: "primes", Params: []byte(`{"from":9,"to":1}`)}})
			return err
		}, codes.InvalidArgument},
		{"too many tasks", func() error {
			_, err := p.client.SubmitJob(p.ctx, &pb.SubmitJobRequest{Spec: primesJob(pb.ScheduleMode_SCHEDULE_MODE_DISTRIBUTED, 1_000_000)})
			return err
		}, codes.InvalidArgument},
		{"get unknown job", func() error {
			_, err := p.client.GetJob(p.ctx, &pb.GetJobRequest{JobId: "missing"})
			return err
		}, codes.NotFound},
		{"watch unknown job", func() error {
			stream, err := p.client.WatchJob(p.ctx, &pb.WatchJobRequest{JobId: "missing"})
			if err != nil {
				return err
			}
			_, err = stream.Recv()
			return err
		}, codes.NotFound},
	}
	for _, tt := range tests {
		if got := status.Code(tt.call()); got != tt.want {
			t.Errorf("%s: code %v, want %v", tt.name, got, tt.want)
		}
	}
}

package main

import (
	"context"
	"flag"
	"strings"
	"testing"
	"time"

	"github.com/sisyphus-network/Sisyphus/apps/sisyphusd/access"
	"github.com/sisyphus-network/Sisyphus/apps/sisyphusd/worker"
	"github.com/sisyphus-network/Sisyphus/packages/idle"
	pb "github.com/sisyphus-network/Sisyphus/packages/protocol/sisyphus/v1"
	"github.com/sisyphus-network/Sisyphus/packages/runtime"
)

func heartbeatSaying(unavailable bool) *pb.WorkerMessage {
	return &pb.WorkerMessage{Kind: &pb.WorkerMessage_Heartbeat{Heartbeat: &pb.Heartbeat{Unavailable: unavailable}}}
}

func stateOf(t *testing.T, p *pool, id string) pb.JobState {
	t.Helper()
	got, err := p.client.GetJob(p.ctx, &pb.GetJobRequest{JobId: id})
	if err != nil {
		t.Fatal(err)
	}
	return got.GetJob().GetState()
}

// A coordinator gives a worker no new tasks while the worker says its
// owner keeps it from them, from its hello on, and lets it finish those it
// has.
func TestACoordinatorGivesNoTasksToAWorkerItsOwnerKeepsFromThem(t *testing.T) {
	p := startPool(t, runtime.Builtin())
	first := hello("laptop", 2, "primes")
	first.GetHello().Unavailable = true
	w := p.connectRaw(first)
	w.welcome()
	job := p.submit(primesJob(pb.ScheduleMode_SCHEDULE_MODE_DISTRIBUTED, 1))
	if state := stateOf(t, p, job.GetJobId()); state != pb.JobState_JOB_STATE_PENDING {
		t.Fatalf("a job with only a worker that takes no tasks is %v", state)
	}

	// The owner leaves the machine: the task is given to it.
	w.send(heartbeatSaying(false))
	task := w.assignment()
	// The owner comes back: the task it has runs on to its end.
	w.send(heartbeatSaying(true))
	output, err := runtime.Primes{}.Execute(context.Background(), nil, task.GetPayload())
	if err != nil {
		t.Fatal(err)
	}
	w.reply(&pb.TaskResult{TaskId: task.GetTaskId(), Attempt: task.GetAttempt(), Outcome: &pb.TaskResult_Output{Output: output}})
	if done := p.wait(job.GetJobId()); done.GetState() != pb.JobState_JOB_STATE_SUCCEEDED {
		t.Errorf("the job the worker finished is %v: %s", done.GetState(), done.GetError())
	}
	// And it is given nothing more.
	next := p.submit(primesJob(pb.ScheduleMode_SCHEDULE_MODE_DISTRIBUTED, 1))
	if state := stateOf(t, p, next.GetJobId()); state != pb.JobState_JOB_STATE_PENDING {
		t.Errorf("a job submitted with the owner back is %v", state)
	}
}

// A node paused by its owner takes no tasks: a job waits until a worker
// that takes them joins, and none of it runs on the paused node.
func TestAPausedNodeTakesNoTasks(t *testing.T) {
	addr := freeAddr(t)
	startDaemon(t, "--listen", addr, "--name", "rig", "--share", "paused")
	waitForOutput(t, "rig", "nodes", "--addr", addr)
	id := strings.TrimSpace(mustCLI(t, "job", "submit", "--addr", addr, "--detach", "--workload", "primes", "--tasks", "2", "--params", `{"from":1,"to":1000}`))
	time.Sleep(200 * time.Millisecond)
	if out := mustCLI(t, "job", "get", "--addr", addr, id); !strings.Contains(out, "pending") {
		t.Errorf("a job with only a paused worker:\n%s", out)
	}
	startDaemon(t, "--role", "worker", "--coordinator", addr, "--name", "helper")
	out := waitForOutput(t, "succeeded in", "job", "get", "--addr", addr, id)
	if strings.Contains(out, "on rig") {
		t.Errorf("a task ran on the paused node:\n%s", out)
	}
}

// A node that shares only while idle, on a machine that cannot tell when
// it was last used, says so as it starts and takes nothing.
func TestANodeThatCannotTellWhetherItIsIdleWarnsAndTakesNothing(t *testing.T) {
	was := idleDetector
	idleDetector = func() idle.Detector { return idle.Unknown{} }
	t.Cleanup(func() { idleDetector = was })
	logs := captureLogs(t)
	addr := freeAddr(t)
	startDaemon(t, "--listen", addr, "--name", "rig", "--share", "idle", "--idle-after", "1m")
	waitForOutput(t, "rig", "nodes", "--addr", addr)
	for _, want := range []string{"--share idle: this machine cannot tell when it was last used", `level=WARN msg="taking no new tasks" node=rig`} {
		if !strings.Contains(logs.String(), want) {
			t.Errorf("no %q in:\n%s", want, logs.String())
		}
	}
}

func TestWhenANodeSharesMustMakeSense(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"--share", "sometimes"}, `--share: "sometimes" is not a way to share: give always, idle or paused`},
		{[]string{"--role", "coordinator", "--share", "idle"}, "--share and --idle-after say when a worker takes tasks, and need the worker role"},
		{[]string{"--role", "coordinator", "--share", "paused"}, "need the worker role"},
		{[]string{"--role", "coordinator", "--idle-after", "10m"}, "need the worker role"},
		{[]string{"--idle-after", "10m"}, "--idle-after is how long the machine must go unused with --share idle, and needs it"},
		{[]string{"--share", "paused", "--idle-after", "10m"}, "needs it"},
		{[]string{"--share", "idle", "--idle-after", "0s"}, "--idle-after must be more than 0"},
	} {
		_, err := cli(t, append([]string{"run", "--data-dir", t.TempDir(), "--listen", freeAddr(t)}, tc.args...)...)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%v: %v, want %q", tc.args, err, tc.want)
		}
	}
	// Always, said or not, on a coordinator-only node changes nothing.
	if _, err := sharingFrom(flag.NewFlagSet("run", flag.ContinueOnError), "always", time.Minute, false); err != nil {
		t.Errorf("--share always on a coordinator-only node: %v", err)
	}
}

// A node that works for another node its owner trusts is kept from new
// tasks there just as in its own pool.
func TestAGuestWorkerSharesAsItsNodeDoes(t *testing.T) {
	p := startPool(t, runtime.Builtin())
	ident, _ := p.admit(access.Worker)
	throttled := make(chan bool, 1)
	guests := &guestWork{ident: ident, dataDir: t.TempDir(), template: &worker.Worker{
		Name: "guest", Slots: 2, Workloads: p.workloads, Log: quiet,
		Sharing: &idle.Policy{Mode: idle.Paused}, Throttle: func(_ context.Context, low bool) { throttled <- low },
	}}
	ctx, stop := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- guests.work(ctx, p.ident.ID(), p.addr) }()
	p.waitForWorkers(1)
	if low := <-throttled; !low {
		t.Error("the paused guest worker's tasks were not throttled")
	}
	job := p.submit(primesJob(pb.ScheduleMode_SCHEDULE_MODE_DISTRIBUTED, 1))
	if state := stateOf(t, p, job.GetJobId()); state != pb.JobState_JOB_STATE_PENDING {
		t.Errorf("a job with only a paused guest worker is %v", state)
	}
	stop()
	if err := <-done; err != nil {
		t.Error(err)
	}
}

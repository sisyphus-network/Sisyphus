package worker_test

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sisyphus-network/Sisyphus/apps/sisyphusd/worker"
	"github.com/sisyphus-network/Sisyphus/packages/idle"
	pb "github.com/sisyphus-network/Sisyphus/packages/protocol/sisyphus/v1"
	"github.com/sisyphus-network/Sisyphus/packages/runtime"
	"github.com/sisyphus-network/Sisyphus/packages/storage"
)

// owner is a Detector whose answer the test changes, as an owner leaving
// and coming back to the machine would.
type owner struct {
	mu    sync.Mutex
	since time.Duration
	known bool
	asked int
}

func (o *owner) set(since time.Duration, known bool) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.since, o.known = since, known
}

func (o *owner) Idle(context.Context) (time.Duration, bool) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.asked++
	return o.since, o.known
}

func (o *owner) times() int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.asked
}

// said is a log that keeps what is written to it.
type said struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (s *said) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(p)
}

func (s *said) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.String()
}

// lingering is a workload whose tasks run until let go, and say whether
// they were stopped.
type lingering struct {
	started chan context.Context
	release chan struct{}
}

func (lingering) Name() string { return "lingering" }
func (lingering) Split(context.Context, runtime.Blobs, []byte, int) ([][]byte, error) {
	return nil, nil
}
func (lingering) Aggregate(context.Context, runtime.Blobs, [][]byte) ([]byte, error) { return nil, nil }
func (l lingering) Execute(ctx context.Context, _ runtime.Blobs, _ []byte) ([]byte, error) {
	l.started <- ctx
	select {
	case <-l.release:
		return []byte("finished"), nil
	case <-ctx.Done():
		return nil, errors.New("stopped")
	}
}

// startSharing runs a worker with the given sharing policy against a
// listening coordinator, and returns the coordinator and the worker's hello.
func startSharing(t *testing.T, sharing *idle.Policy, throttle func(context.Context, bool), log *slog.Logger, heartbeats time.Duration, workloads ...runtime.Workload) (*listeningCoordinator, *pb.Hello) {
	t.Helper()
	coordinator := &listeningCoordinator{assign: make(chan *pb.TaskAssignment, 1), heard: make(chan *pb.WorkerMessage, 64)}
	srv := newServer()
	pb.RegisterCoordinatorServiceServer(srv, coordinator)
	addr, _ := serve(t, srv)
	runWorker(t, &worker.Worker{
		Name: "laptop", Coordinator: addr, Credentials: workerCreds, Slots: 2,
		Workloads: runtime.NewRegistry(append(workloads, runtime.Primes{})...), Blobs: storage.NewMemory(), Log: log,
		HeartbeatInterval: heartbeats, Sharing: sharing, Throttle: throttle,
	})
	t.Cleanup(func() { close(coordinator.assign) })
	return coordinator, (<-coordinator.heard).GetHello()
}

// A worker says as it connects whether its owner lets it take new tasks,
// for each way of sharing and whatever its owner is doing.
func TestAWorkerSaysWhetherItsOwnerLetsItTakeTasks(t *testing.T) {
	away, back := 10*time.Minute, 30*time.Second
	for _, tc := range []struct {
		name        string
		mode        idle.Mode
		since       time.Duration
		known       bool
		battery     bool
		unavailable bool
	}{
		{name: "always, owner there", mode: idle.Always, since: back, known: true},
		{name: "always, not known", mode: idle.Always},
		{name: "paused, owner away", mode: idle.Paused, since: away, known: true, unavailable: true},
		{name: "idle, owner away", mode: idle.WhenIdle, since: away, known: true},
		{name: "idle, nobody logged in", mode: idle.WhenIdle, since: idle.Forever, known: true},
		{name: "idle, owner back", mode: idle.WhenIdle, since: back, known: true, unavailable: true},
		{name: "idle, away not long enough", mode: idle.WhenIdle, since: 4 * time.Minute, known: true, unavailable: true},
		{name: "idle, not known", mode: idle.WhenIdle, unavailable: true},
		{name: "idle, away but on battery", mode: idle.WhenIdle, since: away, known: true, battery: true, unavailable: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			detector := &owner{since: tc.since, known: tc.known}
			policy := &idle.Policy{Mode: tc.mode, After: 5 * time.Minute, Detector: detector, OnBattery: func() bool { return tc.battery }}
			_, hello := startSharing(t, policy, nil, quiet(), time.Hour)
			if hello.GetUnavailable() != tc.unavailable {
				t.Errorf("unavailable %v, want %v", hello.GetUnavailable(), tc.unavailable)
			}
		})
	}
	// With no policy, it always takes them.
	if _, hello := startSharing(t, nil, nil, quiet(), time.Hour); hello.GetUnavailable() {
		t.Error("a worker with no policy said it takes no tasks")
	}
}

// heartbeat waits for the next heartbeat the worker sends, passing over
// anything else it says.
func heartbeat(t *testing.T, coordinator *listeningCoordinator) *pb.Heartbeat {
	t.Helper()
	for {
		select {
		case msg := <-coordinator.heard:
			if beat := msg.GetHeartbeat(); beat != nil {
				return beat
			}
		case <-time.After(5 * time.Second):
			t.Fatal("no heartbeat")
		}
	}
}

// heartbeatSaying waits for a heartbeat that says the worker is available
// or not, as given.
func heartbeatSaying(t *testing.T, coordinator *listeningCoordinator, unavailable bool) *pb.Heartbeat {
	t.Helper()
	for {
		if beat := heartbeat(t, coordinator); beat.GetUnavailable() == unavailable {
			return beat
		}
	}
}

// When the owner of a worker that shares only while they are away comes
// back, it tells its coordinator at once that it takes no new tasks and
// has those running give way, but lets them finish; when the owner has
// gone again, it takes tasks again. Every heartbeat says which it is.
func TestAWorkerStopsTakingTasksWhenItsOwnerComesBack(t *testing.T) {
	worker.ShortenSharingPoll(t, 5*time.Millisecond)
	detector := &owner{since: 10 * time.Minute, known: true}
	throttled := make(chan bool, 8)
	log := new(said)
	work := lingering{started: make(chan context.Context, 1), release: make(chan struct{})}
	coordinator, hello := startSharing(t, &idle.Policy{Mode: idle.WhenIdle, After: 5 * time.Minute, Detector: detector},
		func(_ context.Context, low bool) { throttled <- low }, slog.New(slog.NewTextHandler(log, nil)), 20*time.Millisecond, work)
	if hello.GetUnavailable() {
		t.Fatal("with its owner away, the worker said it takes no tasks")
	}
	if low := <-throttled; low {
		t.Error("with its owner away, the worker's tasks were throttled")
	}

	coordinator.assign <- &pb.TaskAssignment{JobId: "j", TaskId: "j/0", Workload: "lingering"}
	running := <-work.started

	// The owner comes back.
	detector.set(0, true)
	if beat := heartbeatSaying(t, coordinator, true); beat.GetRunningTasks() != 1 {
		t.Errorf("the heartbeat says %d tasks are running", beat.GetRunningTasks())
	}
	if low := <-throttled; !low {
		t.Error("with its owner back, the worker's tasks were not throttled")
	}
	// Every heartbeat says so while they are there.
	for range 3 {
		if !heartbeat(t, coordinator).GetUnavailable() {
			t.Error("a later heartbeat says the worker takes tasks")
		}
	}
	if running.Err() != nil {
		t.Error("the running task was stopped")
	}
	if !strings.Contains(log.String(), `msg="taking no new tasks" node=laptop because="its owner is using it" running=1`) {
		t.Errorf("the log does not say the worker takes no new tasks:\n%s", log)
	}

	// The owner goes away again.
	detector.set(10*time.Minute, true)
	heartbeatSaying(t, coordinator, false)
	if low := <-throttled; low {
		t.Error("with its owner gone again, the worker's tasks are still throttled")
	}
	if !strings.Contains(log.String(), `msg="taking new tasks" node=laptop`) {
		t.Errorf("the log does not say the worker takes tasks again:\n%s", log)
	}

	// The machine can no longer tell: it takes its owner to be there, and
	// warns.
	detector.set(0, false)
	heartbeatSaying(t, coordinator, true)
	if low := <-throttled; !low {
		t.Error("not knowing, the worker's tasks were not throttled")
	}
	if !strings.Contains(log.String(), `level=WARN msg="taking no new tasks" node=laptop because="it cannot tell`) {
		t.Errorf("the log does not warn that the worker cannot tell:\n%s", log)
	}
	// From not knowing to the owner being there is no change in whether
	// it takes tasks: it is said in the log and not to the coordinator.
	detector.set(0, true)
	for deadline := time.Now().Add(5 * time.Second); strings.Count(log.String(), `because="its owner is using it"`) < 2; time.Sleep(5 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("the log does not say the owner is back:\n%s", log)
		}
	}
	select {
	case low := <-throttled:
		t.Errorf("throttled %v again with no change in whether the worker takes tasks", low)
	default:
	}

	// The task finishes as it would have.
	close(work.release)
	for {
		if result := (<-coordinator.heard).GetTaskResult(); result != nil {
			if string(result.GetOutput()) != "finished" {
				t.Errorf("the task ended with %v", result)
			}
			break
		}
	}
}

// A paused worker has whatever it is given give way, and never asks
// whether its owner is there.
func TestAPausedWorkerNeverAsksAfterItsOwner(t *testing.T) {
	worker.ShortenSharingPoll(t, time.Millisecond)
	detector := &owner{since: time.Hour, known: true}
	var throttled atomic.Int32
	_, hello := startSharing(t, &idle.Policy{Mode: idle.Paused, Detector: detector},
		func(_ context.Context, low bool) {
			if low {
				throttled.Add(1)
			}
		}, quiet(), time.Hour)
	time.Sleep(20 * time.Millisecond)
	if !hello.GetUnavailable() || throttled.Load() != 1 || detector.times() != 0 {
		t.Errorf("unavailable %v, throttled %d times, owner asked after %d times", hello.GetUnavailable(), throttled.Load(), detector.times())
	}
}

package worker_test

import (
	"bytes"
	"context"
	"log/slog"
	"slices"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"

	"github.com/sisyphus-network/Sisyphus/apps/sisyphusd/worker"
	pb "github.com/sisyphus-network/Sisyphus/packages/protocol/sisyphus/v1"
	"github.com/sisyphus-network/Sisyphus/packages/runtime"
	"github.com/sisyphus-network/Sisyphus/packages/sealed"
	"github.com/sisyphus-network/Sisyphus/packages/storage"
)

// scriptedCoordinator welcomes a worker, hands it one assignment and reports
// what comes back.
type scriptedCoordinator struct {
	pb.UnimplementedCoordinatorServiceServer
	assignment *pb.TaskAssignment
	hello      chan *pb.Hello
	result     chan *pb.TaskResult
}

func (c *scriptedCoordinator) Connect(stream grpc.BidiStreamingServer[pb.WorkerMessage, pb.CoordinatorMessage]) error {
	first, err := stream.Recv()
	if err != nil {
		return err
	}
	c.hello <- first.GetHello()
	stream.Send(&pb.CoordinatorMessage{Kind: &pb.CoordinatorMessage_Welcome{Welcome: &pb.Welcome{CoordinatorId: "scripted", SwarmFingerprint: "key-at-welcome"}}})
	stream.Send(&pb.CoordinatorMessage{Kind: &pb.CoordinatorMessage_SwarmUpdate{SwarmUpdate: &pb.SwarmUpdate{SwarmFingerprint: "key-after-change"}}})
	stream.Send(&pb.CoordinatorMessage{Kind: &pb.CoordinatorMessage_Assignment{Assignment: c.assignment}})
	for {
		msg, err := stream.Recv()
		if err != nil {
			return nil
		}
		if result := msg.GetTaskResult(); result != nil {
			c.result <- result
		}
	}
}

func runWorker(t *testing.T, w *worker.Worker) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		w.Run(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})
}

func TestWorkerAdvertisesItselfAndReportsAnUnknownWorkload(t *testing.T) {
	coordinator := &scriptedCoordinator{
		assignment: &pb.TaskAssignment{TaskId: "job/0", JobId: "job", Attempt: 2, Workload: "alchemy"},
		hello:      make(chan *pb.Hello, 1),
		result:     make(chan *pb.TaskResult, 1),
	}
	srv := newServer()
	pb.RegisterCoordinatorServiceServer(srv, coordinator)
	addr, _ := serve(t, srv)
	stated := make(chan string, 4)
	runWorker(t, &worker.Worker{
		Name: "w", Coordinator: addr, Credentials: workerCreds, Slots: 3,
		Workloads: runtime.Builtin(), Blobs: storage.NewMemory(), Log: quiet(),
		OnSwarm: func(fingerprint string) { stated <- fingerprint },
	})
	// The worker passes on what the coordinator says about the swarm key,
	// on joining and when it changes.
	if first, second := <-stated, <-stated; first != "key-at-welcome" || second != "key-after-change" {
		t.Errorf("the worker was told of %q then %q", first, second)
	}

	hello := <-coordinator.hello
	caps := hello.GetCapabilities()
	if hello.GetName() != "w" || caps.GetTaskSlots() != 3 || caps.GetCpuCores() == 0 || caps.GetOs() == "" {
		t.Errorf("hello: %v", hello)
	}
	if got := strings.Join(caps.GetWorkloads(), ","); got != "primes,wordcount" {
		t.Errorf("advertised workloads %q", got)
	}

	result := <-coordinator.result
	if result.GetTaskId() != "job/0" || result.GetAttempt() != 2 {
		t.Errorf("result is for task %q attempt %d", result.GetTaskId(), result.GetAttempt())
	}
	if !strings.Contains(result.GetError(), `unknown workload "alchemy"`) {
		t.Errorf("result error %q, want it to name the unknown workload", result.GetError())
	}
}

// A private job's task comes with keys: its job's own and those of its
// sealed inputs. One that comes with keys that are not keys is refused,
// and so is one that comes with a sealing key whole and no other, as a
// coordinator from before each job had a key gives it: run as though it
// had none, it would store unsealed what it was to seal.
func TestWorkerRefusesATaskWhoseKeysAreNotKeys(t *testing.T) {
	good := &pb.SealingKey{Id: make([]byte, 16), Key: make([]byte, sealed.KeySize)}
	for name, tt := range map[string]struct {
		keys  []*pb.SealingKey
		whole []byte
		want  string
	}{
		"a whole key and no other": {nil, make([]byte, sealed.KeySize), "a sealing key whole and no key of its job's own"},
		"a key that is too short":  {[]*pb.SealingKey{{Id: make([]byte, 16), Key: []byte("too short")}}, nil, "a key of 9 bytes whose ID is of 16, which is not a key"},
		"an ID that is too short":  {[]*pb.SealingKey{good, {Id: []byte("short"), Key: make([]byte, sealed.KeySize)}}, nil, "a key of 32 bytes whose ID is of 5, which is not a key"},
		"a whole key too short":    {[]*pb.SealingKey{good}, []byte("too short"), "key of 9 bytes, which is not a key"},
	} {
		coordinator := &scriptedCoordinator{
			assignment: &pb.TaskAssignment{TaskId: "job/0", JobId: "job", Attempt: 1, Workload: "primes", Payload: []byte(`{"from":0,"to":10}`), Keys: tt.keys, Key: tt.whole},
			hello:      make(chan *pb.Hello, 1),
			result:     make(chan *pb.TaskResult, 1),
		}
		srv := newServer()
		pb.RegisterCoordinatorServiceServer(srv, coordinator)
		addr, _ := serve(t, srv)
		runWorker(t, &worker.Worker{
			Name: "w", Coordinator: addr, Credentials: workerCreds, Slots: 1,
			Workloads: runtime.Builtin(), Blobs: storage.NewMemory(), Log: quiet(),
		})
		// It says, as it joins, that it takes keys as they are now given.
		if hello := <-coordinator.hello; !slices.Contains(hello.GetCapabilities().GetLabels(), runtime.SealingLabel) {
			t.Errorf("%s: the worker introduced itself with labels %v", name, hello.GetCapabilities().GetLabels())
		}
		if result := <-coordinator.result; !strings.Contains(result.GetError(), tt.want) {
			t.Errorf("%s: result %v, want the task refused for its keys", name, result)
		}
	}
}

func TestWorkerKeepsRetryingAnAddressItCannotUse(t *testing.T) {
	var logs bytes.Buffer
	w := &worker.Worker{
		Name: "w", Coordinator: "bad\x00address", Credentials: workerCreds, Slots: 1,
		Workloads: runtime.Builtin(), Blobs: storage.NewMemory(),
		Log: slog.New(slog.NewTextHandler(&logs, nil)),
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if err := w.Run(ctx); err != nil {
		t.Errorf("Run returned %v; a cancelled worker should stop quietly", err)
	}
	if !strings.Contains(logs.String(), "invalid control character") {
		t.Errorf("the reason was not logged:\n%s", logs.String())
	}
}

func TestWorkerStopsAtOnceWhenAlreadyCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	w := &worker.Worker{
		Name: "w", Coordinator: "127.0.0.1:1", Credentials: workerCreds, Slots: 1,
		Workloads: runtime.Builtin(), Blobs: storage.NewMemory(), Log: quiet(),
	}
	done := make(chan error, 1)
	go func() { done <- w.Run(ctx) }()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Run returned %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a cancelled worker kept running")
	}
}

package worker_test

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
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
	if got := strings.Join(caps.GetWorkloads(), ","); got != "forecast,forecasts,primes,wordcount" {
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

// A model task's result says what the worker's model server ran it with,
// whether the task succeeds or fails; other work's says nothing of models.
func TestWorkerSendsWhatAModelTaskRanWith(t *testing.T) {
	ollama := func(answers int) *httptest.Server {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/api/version":
				io.WriteString(w, `{"version":"0.12.11"}`)
			case "/api/tags":
				io.WriteString(w, `{"models":[{"name":"llama3.1:8b","digest":"46e0c10c039e","details":{"quantization_level":"Q4_K_M"}}]}`)
			default:
				w.WriteHeader(answers)
				io.WriteString(w, `{"model":"llama3.1:8b","message":{"role":"assistant","content":"Hi."},"done":true}`)
			}
		}))
		t.Cleanup(server.Close)
		return server
	}
	for name, tt := range map[string]struct {
		workload, payload string
		answers           int
		failed            bool
		used              bool
	}{
		"a model task that succeeds": {"chat", `{"model":"llama3.1:8b","temperature":0,"messages":[{"role":"user","content":"Hi"}]}`, http.StatusOK, false, true},
		"a model task that fails":    {"chat", `{"model":"llama3.1:8b","temperature":0,"messages":[{"role":"user","content":"Hi"}]}`, http.StatusInternalServerError, true, true},
		"work that asks no model":    {"primes", `{"from":0,"to":10}`, http.StatusOK, false, false},
	} {
		coordinator := &scriptedCoordinator{
			assignment: &pb.TaskAssignment{TaskId: "job/0", JobId: "job", Attempt: 1, Workload: tt.workload, Payload: []byte(tt.payload)},
			hello:      make(chan *pb.Hello, 1),
			result:     make(chan *pb.TaskResult, 1),
		}
		srv := newServer()
		pb.RegisterCoordinatorServiceServer(srv, coordinator)
		addr, _ := serve(t, srv)
		runWorker(t, &worker.Worker{
			Name: "w", Coordinator: addr, Credentials: workerCreds, Slots: 1,
			Workloads: runtime.Builtin().With(runtime.Chat{URL: ollama(tt.answers).URL, Owner: runtime.Owner{Options: map[string]any{"num_ctx": 8192}}}),
			Blobs:     storage.NewMemory(), Log: quiet(),
		})
		<-coordinator.hello
		result := <-coordinator.result
		if failed := result.GetError() != ""; failed != tt.failed {
			t.Errorf("%s: result %v", name, result)
		}
		used := result.GetUsed()
		if !tt.used {
			if used != nil {
				t.Errorf("%s: sent %v", name, used)
			}
			continue
		}
		if used.GetServer() != "ollama" || used.GetVersion() != "0.12.11" || used.GetModel() != "llama3.1:8b" || used.GetDigest() != "46e0c10c039e" || used.GetQuantization() != "Q4_K_M" ||
			!strings.Contains(string(used.GetOptions()), `"num_ctx":8192`) || !strings.Contains(string(used.GetOptions()), `"temperature":0`) {
			t.Errorf("%s: sent %v", name, used)
		}
	}
}

// welcomingCoordinator welcomes a worker, telling it the address it sees it
// at, and keeps the connection until the worker goes.
type welcomingCoordinator struct {
	pb.UnimplementedCoordinatorServiceServer
	observed string
	hello    chan *pb.Hello
}

func (c *welcomingCoordinator) Connect(stream grpc.BidiStreamingServer[pb.WorkerMessage, pb.CoordinatorMessage]) error {
	first, err := stream.Recv()
	if err != nil {
		return err
	}
	c.hello <- first.GetHello()
	stream.Send(&pb.CoordinatorMessage{Kind: &pb.CoordinatorMessage_Welcome{Welcome: &pb.Welcome{CoordinatorId: "welcoming", ObservedAddress: c.observed}}})
	for {
		if _, err := stream.Recv(); err != nil {
			return nil
		}
	}
}

// A worker says where it reached its coordinator, and while it is
// connected, is where its coordinator sees it, if that is a global address.
func TestAWorkerIsWhereItsCoordinatorSeesIt(t *testing.T) {
	for observed, want := range map[string]netip.Addr{
		"193.0.6.139":    netip.MustParseAddr("193.0.6.139"),
		"10.0.0.5":       {},
		"100.64.0.9":     {},
		"":               {},
		"not an address": {},
	} {
		coordinator := &welcomingCoordinator{observed: observed, hello: make(chan *pb.Hello, 1)}
		srv := newServer()
		pb.RegisterCoordinatorServiceServer(srv, coordinator)
		addr, _ := serve(t, srv)
		w := &worker.Worker{Name: "w", Coordinator: addr, Credentials: workerCreds, Slots: 1, Workloads: runtime.Builtin(), Blobs: storage.NewMemory(), Log: quiet()}
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() {
			defer close(done)
			w.Run(ctx)
		}()
		// It reached the coordinator at the address it was given, here a
		// loopback one, which the coordinator would take as nowhere.
		if hello := <-coordinator.hello; hello.GetDialledAddress() != "127.0.0.1" {
			t.Errorf("the worker says it reached its coordinator at %q", hello.GetDialledAddress())
		}
		deadline := time.Now().Add(5 * time.Second)
		for !w.Connected() && time.Now().Before(deadline) {
			time.Sleep(10 * time.Millisecond)
		}
		if got := w.Observed(); got != want {
			t.Errorf("told it is at %q, the worker is at %v, want %v", observed, got, want)
		}
		cancel()
		<-done
		if got := w.Observed(); got.IsValid() {
			t.Errorf("no longer connected, the worker is still at %v", got)
		}
	}
}

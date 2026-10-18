package worker_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"

	"github.com/sisyphus-network/Sisyphus/apps/sisyphusd/worker"
	pb "github.com/sisyphus-network/Sisyphus/packages/protocol/sisyphus/v1"
	"github.com/sisyphus-network/Sisyphus/packages/runtime"
	"github.com/sisyphus-network/Sisyphus/packages/storage"
)

// listeningCoordinator welcomes a worker, gives it what it is handed on
// assign, and passes on every message the worker sends, in order.
type listeningCoordinator struct {
	pb.UnimplementedCoordinatorServiceServer
	assign chan *pb.TaskAssignment
	heard  chan *pb.WorkerMessage
}

func (c *listeningCoordinator) Connect(stream grpc.BidiStreamingServer[pb.WorkerMessage, pb.CoordinatorMessage]) error {
	first, err := stream.Recv()
	if err != nil {
		return err
	}
	c.heard <- first
	stream.Send(&pb.CoordinatorMessage{Kind: &pb.CoordinatorMessage_Welcome{Welcome: &pb.Welcome{CoordinatorId: "listening"}}})
	go func() {
		for a := range c.assign {
			stream.Send(&pb.CoordinatorMessage{Kind: &pb.CoordinatorMessage_Assignment{Assignment: a}})
		}
	}()
	for {
		msg, err := stream.Recv()
		if err != nil {
			return nil
		}
		c.heard <- msg
	}
}

// ollamaWith is an Ollama whose models can be changed behind the worker's
// back, as its owner would by hand. Asked for a model it does not have, it
// says so as Ollama does.
type ollamaWith struct {
	mu     sync.Mutex
	models []string
}

func (o *ollamaWith) set(models ...string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.models = models
}

func (o *ollamaWith) serve(t *testing.T) string {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		o.mu.Lock()
		models := slices.Clone(o.models)
		o.mu.Unlock()
		switch r.URL.Path {
		case "/api/tags":
			io.WriteString(w, `{"models":[`)
			for i, m := range models {
				if i > 0 {
					io.WriteString(w, ",")
				}
				io.WriteString(w, `{"name":"`+m+`"}`)
			}
			io.WriteString(w, `]}`)
		case "/api/ps":
			io.WriteString(w, `{"models":[]}`)
		case "/api/chat":
			w.WriteHeader(http.StatusNotFound)
			io.WriteString(w, `{"error":"model 'llama3.1:8b' not found"}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)
	return server.URL
}

// startListened runs a worker against a listening coordinator, with a
// heartbeat too slow to matter, and returns the coordinator once it has
// heard the worker's hello.
func startListened(t *testing.T, ollama string) (*listeningCoordinator, *worker.Worker, *pb.Hello) {
	t.Helper()
	coordinator := &listeningCoordinator{assign: make(chan *pb.TaskAssignment, 1), heard: make(chan *pb.WorkerMessage, 16)}
	t.Cleanup(func() { close(coordinator.assign) })
	srv := newServer()
	pb.RegisterCoordinatorServiceServer(srv, coordinator)
	addr, _ := serve(t, srv)
	w := &worker.Worker{
		Name: "w", Coordinator: addr, Credentials: workerCreds, Slots: 1,
		Workloads: runtime.Builtin().With(runtime.Chat{URL: ollama}),
		Blobs:     storage.NewMemory(), Log: quiet(), HeartbeatInterval: time.Hour,
	}
	runWorker(t, w)
	return coordinator, w, (<-coordinator.heard).GetHello()
}

// The worker tells its coordinator what it serves when told that has
// changed, not on a timer, and once for word that came twice.
func TestAWorkerSaysWhatItServesWhenToldItChanged(t *testing.T) {
	ollama := &ollamaWith{models: []string{"llama3.1:8b"}}
	coordinator, w, hello := startListened(t, ollama.serve(t))
	if !slices.Contains(hello.GetCapabilities().GetLabels(), "model:llama3.1:8b") {
		t.Fatalf("hello labels = %v", hello.GetCapabilities().GetLabels())
	}

	// A model fetched outside the node is not noticed by itself.
	ollama.set("llama3.1:8b", "qwen3:8b")
	select {
	case msg := <-coordinator.heard:
		t.Fatalf("the worker spoke unprompted: %v", msg)
	case <-time.After(100 * time.Millisecond):
	}

	w.OffersChanged()
	w.OffersChanged()
	labels := (<-coordinator.heard).GetHeartbeat().GetLabels().GetLabels()
	if !slices.Contains(labels, "model:qwen3:8b") {
		t.Fatalf("labels after word of a change = %v", labels)
	}
	// Word of a change that changed nothing sends nothing.
	w.OffersChanged()
	select {
	case msg := <-coordinator.heard:
		t.Fatalf("the worker sent %v for a change that changed nothing", msg)
	case <-time.After(100 * time.Millisecond):
	}
}

// A model task that finds its model gone has the worker say it no longer
// serves it before the task's failure is reported, so the coordinator does
// not give the task back to it.
func TestAWorkerSaysAModelIsGoneBeforeReportingTheTaskThatFoundItGone(t *testing.T) {
	ollama := &ollamaWith{models: []string{"llama3.1:8b"}}
	coordinator, _, _ := startListened(t, ollama.serve(t))

	ollama.set()
	coordinator.assign <- &pb.TaskAssignment{
		TaskId: "job/0", JobId: "job", Attempt: 1, Workload: "chat", Needs: []string{"model:llama3.1:8b"},
		Payload: []byte(`{"model":"llama3.1:8b","messages":[{"role":"user","content":"Hi"}]}`),
	}
	var order []string
	for len(order) == 0 || order[len(order)-1] != "result" {
		switch msg := <-coordinator.heard; {
		case msg.GetHeartbeat().GetLabels() != nil:
			if slices.Contains(msg.GetHeartbeat().GetLabels().GetLabels(), "model:llama3.1:8b") {
				t.Fatalf("the worker still says it serves the model: %v", msg)
			}
			order = append(order, "labels")
		case msg.GetTaskResult() != nil:
			if msg.GetTaskResult().GetError() == "" {
				t.Fatalf("the task succeeded without its model: %v", msg)
			}
			order = append(order, "result")
		}
	}
	if !slices.Equal(order, []string{"labels", "result"}) {
		t.Errorf("the coordinator heard %v, want the labels before the result", order)
	}
}

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

// A model server too slow to say what it has delays a task's result by no
// more than the worker's wait, and changes nothing the worker said.
func TestASlowModelServerDoesNotHoldUpATasksResult(t *testing.T) {
	worker.ShortenOffersWait(t, 100*time.Millisecond)
	hung := make(chan struct{})
	t.Cleanup(func() { close(hung) })
	var slow sync.Once
	ollama := &ollamaWith{models: []string{"llama3.1:8b"}}
	listed := ollama.serve(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The loaded models are asked for only when the worker says what it
		// offers: the first time, for the hello, they are given, and every
		// later time the asking hangs until the asker gives up.
		answered := false
		if r.URL.Path == "/api/ps" {
			slow.Do(func() { answered = true })
		}
		if r.URL.Path == "/api/ps" && !answered {
			select {
			case <-r.Context().Done():
			case <-hung:
			}
			return
		}
		proxied, err := http.Get(listed + r.URL.Path)
		if err != nil {
			return
		}
		defer proxied.Body.Close()
		w.WriteHeader(proxied.StatusCode)
		io.Copy(w, proxied.Body)
	}))
	t.Cleanup(server.Close)
	coordinator, _, _ := startListened(t, server.URL)

	began := time.Now()
	coordinator.assign <- &pb.TaskAssignment{
		TaskId: "job/0", JobId: "job", Attempt: 1, Workload: "chat", Needs: []string{"model:llama3.1:8b"},
		Payload: []byte(`{"model":"llama3.1:8b","messages":[{"role":"user","content":"Hi"}]}`),
	}
	msg := <-coordinator.heard
	if msg.GetTaskResult() == nil {
		t.Fatalf("the worker sent %v before the task's result", msg)
	}
	if took := time.Since(began); took > 2*time.Second {
		t.Errorf("the result took %v behind a model server that did not answer", took)
	}
}

// What a model server said when asked earlier never replaces what it said
// when asked later, whichever answer arrives first.
func TestAnEarlierAskingNeverReplacesALaterOne(t *testing.T) {
	ollama := &ollamaWith{models: []string{"llama3.1:8b", "qwen3:8b"}}
	listed := ollama.serve(t)
	firstAsked, release := make(chan struct{}), make(chan struct{})
	var tags sync.Mutex // guards asked
	asked := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Only saying what the worker offers asks for the loaded models.
		// The hello's asking is the first; the first task's, by then past
		// reading the model list, is held until the second task's has been
		// answered and acted on.
		if r.URL.Path == "/api/ps" {
			tags.Lock()
			asked++
			n := asked
			tags.Unlock()
			if n == 2 {
				close(firstAsked)
				<-release
			}
		}
		proxied, err := http.Get(listed + r.URL.Path)
		if err != nil {
			return
		}
		defer proxied.Body.Close()
		w.WriteHeader(proxied.StatusCode)
		io.Copy(w, proxied.Body)
	}))
	t.Cleanup(server.Close)
	coordinator := &listeningCoordinator{assign: make(chan *pb.TaskAssignment, 2), heard: make(chan *pb.WorkerMessage, 16)}
	t.Cleanup(func() { close(coordinator.assign) })
	srv := newServer()
	pb.RegisterCoordinatorServiceServer(srv, coordinator)
	addr, _ := serve(t, srv)
	runWorker(t, &worker.Worker{
		Name: "w", Coordinator: addr, Credentials: workerCreds, Slots: 2,
		Workloads: runtime.Builtin().With(runtime.Chat{URL: server.URL}),
		Blobs:     storage.NewMemory(), Log: quiet(), HeartbeatInterval: time.Hour,
	})
	<-coordinator.heard
	task := func(id string) *pb.TaskAssignment {
		return &pb.TaskAssignment{
			TaskId: id, JobId: "job", Attempt: 1, Workload: "chat", Needs: []string{"model:llama3.1:8b"},
			Payload: []byte(`{"model":"llama3.1:8b","messages":[{"role":"user","content":"Hi"}]}`),
		}
	}
	coordinator.assign <- task("job/0")
	<-firstAsked
	// The second task's asking, later, finds qwen3:8b gone and says so.
	ollama.set("llama3.1:8b")
	coordinator.assign <- task("job/1")
	labels := (<-coordinator.heard).GetHeartbeat().GetLabels().GetLabels()
	if slices.Contains(labels, "model:qwen3:8b") {
		t.Fatalf("the later asking's labels = %v", labels)
	}
	if result := (<-coordinator.heard).GetTaskResult(); result.GetTaskId() != "job/1" {
		t.Fatalf("heard %v, want the second task's result", result)
	}
	// The first asking's answer, older, arrives last and is not sent.
	close(release)
	msg := <-coordinator.heard
	if msg.GetTaskResult().GetTaskId() != "job/0" {
		t.Errorf("heard %v before the first task's result, want nothing more", msg)
	}
}

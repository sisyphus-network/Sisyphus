package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc/metadata"

	"github.com/sisyphus-network/Sisyphus/packages/nodedb"
	nodepb "github.com/sisyphus-network/Sisyphus/packages/protocol/sisyphus/node/v1"
	pb "github.com/sisyphus-network/Sisyphus/packages/protocol/sisyphus/v1"
)

// A model server's replies as OpenAI's dialect has them, which is how a
// worker asks its own: one that asks for a tool, and one that is only text.
// asksFor is a worker's model asking for a tool, in the dialect a worker
// speaks to its Ollama. The arguments are written as OpenAI writes them, as
// JSON in a string, and the worker passes them on as OpenAI would have.
func asksFor(tool, arguments string) string {
	var written string
	json.Unmarshal([]byte(arguments), &written)
	return wants(tool, written)
}

// replies is a worker's model answering in words.
func replies(text string) string { return says(text) }

// reportQuickly has the workers of the nodes a test starts report in to
// their coordinators as often as a test needs, not as a real pool does.
func reportQuickly(t *testing.T) {
	t.Helper()
	every := workerHeartbeat
	workerHeartbeat = 100 * time.Millisecond
	t.Cleanup(func() { workerHeartbeat = every })
}

func TestThePoolThinksOnAWorkersModelAndThePlannerPlansWithIt(t *testing.T) {
	reportQuickly(t)
	served := newModel(t,
		asksFor("run_job", `"{\"workload\":\"primes\",\"params\":{\"from\":0,\"to\":100}}"`),
		replies("There are 25 primes below 100."),
		replies("Fetched and ready."),
	)
	dataDir := t.TempDir()
	addr, apiAddr, thinkAddr := freeAddr(t), freeAddr(t), freeAddr(t)
	startDaemon(t, "--data-dir", dataDir, "--listen", addr, "--name", "rig", "--slots", "2",
		"--api-listen", apiAddr, "--inference-listen", thinkAddr, "--models-from", served.URL)
	waitForOutput(t, "rig", "nodes", "--addr", addr)
	client := desktop(t, apiAddr)
	tokenFile := filepath.Join(dataDir, "api.token")
	token, err := os.ReadFile(tokenFile)
	if err != nil {
		t.Fatal(err)
	}

	// The worker says which models it serves, and the pool offers them.
	workers, err := client.ListWorkers(context.Background(), &nodepb.ListWorkersRequest{})
	if err != nil || len(workers.GetWorkers()) != 1 || strings.Join(workers.GetWorkers()[0].GetModels(), ",") != "test-model,another-model" {
		t.Fatalf("workers = %v, %v", workers, err)
	}
	// And which of them is in memory, and how much of it on a graphics card.
	if loaded := workers.GetWorkers()[0].GetLoadedModels(); len(loaded) != 1 || loaded[0].GetName() != "test-model" || loaded[0].GetOnCardPercent() != 75 {
		t.Errorf("the worker's loaded models = %v", loaded)
	}
	if out := mustCLI(t, "nodes", "--addr", addr); !strings.Contains(out, "rig serves test-model (loaded, 75% on a graphics card), another-model\n") {
		t.Errorf("nodes says of the models:\n%s", out)
	}
	req, _ := http.NewRequest(http.MethodGet, "http://"+thinkAddr+"/v1/models", nil)
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(token)))
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	listed, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if res.StatusCode != http.StatusOK || !strings.Contains(string(listed), `"test-model"`) {
		t.Fatalf("the pool's models: %s %s", res.Status, listed)
	}

	// The node's own planner, told to plan with the pool's model, thinks
	// on the pool and computes on it.
	mustCLI(t, "model", "set", "--data-dir", dataDir, "--provider", "openai", "--url", "http://"+thinkAddr+"/v1", "--model", "test-model", "--api-key-file", tokenFile)
	out := mustCLI(t, "ask", "--data-dir", dataDir, "--api", apiAddr, "How many primes below 100?")
	if !strings.Contains(out, "[run_job") || !strings.Contains(out, `"count":25`) || !strings.Contains(out, "There are 25 primes below 100.") {
		t.Fatalf("the planner said:\n%s", out)
	}
	// Each time it thought was a job, as was what it had computed.
	jobs, err := client.ListJobs(tokenOf(t, dataDir), &nodepb.ListJobsRequest{})
	if err != nil {
		t.Fatal(err)
	}
	ran := map[string]int{}
	for _, job := range jobs.GetJobs() {
		ran[job.GetWorkload()]++
	}
	if ran["chat"] != 2 || ran["primes"] != 1 {
		t.Errorf("the jobs run were %v", ran)
	}

	// A conversation with a model no worker serves waits for one that does.
	waiting := mustCLI(t, "job", "submit", "--addr", addr, "--data-dir", dataDir, "--detach", "--workload", "chat",
		"--params", `{"model":"unserved-model","messages":[{"role":"user","content":"Hi"}]}`)
	held := mustCLI(t, "job", "get", "--addr", addr, "--data-dir", dataDir, strings.TrimSpace(waiting))
	if !strings.Contains(held, strings.ToLower(strings.TrimPrefix(pb.JobState_JOB_STATE_PENDING.String(), "JOB_STATE_"))) {
		t.Errorf("a conversation with a model nobody serves:\n%s", held)
	}
	// The worker's owner fetches the model. The worker says so the next
	// time it reports in, and the conversation that was waiting is had.
	served.mu.Lock()
	served.fetched = "unserved-model"
	served.mu.Unlock()
	waitFor(t, func() bool {
		job, err := client.GetJob(tokenOf(t, dataDir), &nodepb.GetJobRequest{JobId: strings.TrimSpace(waiting)})
		return err == nil && job.GetJob().GetState() == nodepb.JobState_JOB_STATE_SUCCEEDED && strings.Contains(string(job.GetJob().GetResult()), "Fetched and ready.")
	})
	workers, err = client.ListWorkers(context.Background(), &nodepb.ListWorkersRequest{})
	if err != nil || strings.Join(workers.GetWorkers()[0].GetModels(), ",") != "test-model,another-model,unserved-model" {
		t.Errorf("the worker's models after fetching one: %v, %v", workers, err)
	}
}

func TestThePoolsModelsAreOfferedOnThisMachineByACoordinator(t *testing.T) {
	taken, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer taken.Close()
	spoiled := t.TempDir()
	if err := os.Mkdir(filepath.Join(spoiled, "api.token"), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct {
		name string
		args []string
		want string
	}{
		{"every interface", []string{"--data-dir", t.TempDir(), "--inference-listen", "0.0.0.0:11435"}, "must be a loopback address"},
		{"not an address", []string{"--data-dir", t.TempDir(), "--inference-listen", "nonsense"}, "must be a loopback address"},
		{"a node with no pool", []string{"--data-dir", t.TempDir(), "--role", "worker", "--coordinator", "127.0.0.1:1", "--inference-listen", freeAddr(t)}, "is for a node that coordinates a pool"},
		{"an address in use", []string{"--data-dir", t.TempDir(), "--inference-listen", taken.Addr().String()}, "address already in use"},
		{"a token that cannot be read", []string{"--data-dir", spoiled, "--inference-listen", freeAddr(t)}, "read API token"},
	} {
		_, err := cli(t, append([]string{"run", "--listen", freeAddr(t)}, tt.args...)...)
		if err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("--inference-listen %s: error %v, want one containing %q", tt.name, err, tt.want)
		}
	}
}

func TestAMachineThatUsesAPoolOffersItsModelsToItsOwnPrograms(t *testing.T) {
	served := newModel(t, replies("Hello from the pool."), replies("And again."))
	addr, thinkAddr := freeAddr(t), freeAddr(t)
	startDaemon(t, "--data-dir", t.TempDir(), "--listen", addr, "--name", "rig", "--slots", "2", "--models-from", served.URL)
	waitForOutput(t, "rig", "nodes", "--addr", addr)
	// Another machine joins as a client, and offers the pool's models to
	// its own programs through the coordinator.
	laptop := t.TempDir()
	mustCLI(t, "pool", "join", "--data-dir", laptop, "--addr", addr, invite(t, addr, "client"))
	ctx, stop := context.WithCancel(context.Background())
	stopped := make(chan error, 1)
	go func() {
		stopped <- run(ctx, []string{"inference", "--data-dir", laptop, "--addr", addr, "--listen", thinkAddr})
	}()
	tokenFile := filepath.Join(laptop, "api.token")
	waitFor(t, func() bool {
		conn, err := net.Dial("tcp", thinkAddr)
		if err == nil {
			conn.Close()
		}
		_, noToken := os.Stat(tokenFile)
		return err == nil && noToken == nil
	})
	token, _ := os.ReadFile(tokenFile)
	ask := func(method, path, body string) (int, string) {
		req, _ := http.NewRequest(method, "http://"+thinkAddr+path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(token)))
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		said, _ := io.ReadAll(res.Body)
		return res.StatusCode, string(said)
	}
	if status, body := ask(http.MethodGet, "/v1/models", ""); status != http.StatusOK || !strings.Contains(body, `"test-model"`) {
		t.Fatalf("the pool's models as the client lists them: %d %s", status, body)
	}
	if status, body := ask(http.MethodPost, "/v1/chat/completions", `{"model":"test-model","messages":[{"role":"user","content":"Hi"}]}`); status != http.StatusOK || !strings.Contains(body, "Hello from the pool.") {
		t.Fatalf("a reply through the client: %d %s", status, body)
	}
	// Asked for as a stream, with the model named on the worker that has it.
	if status, body := ask(http.MethodPost, "/v1/chat/completions", `{"model":"test-model@rig","stream":true,"messages":[{"role":"user","content":"Hi"}]}`); status != http.StatusOK || !strings.Contains(body, "And again.") || !strings.HasSuffix(body, "data: [DONE]\n\n") {
		t.Fatalf("a streamed reply through the client: %d %s", status, body)
	}
	// What the coordinator refuses, the client is told.
	if status, body := ask(http.MethodPost, "/v1/chat/completions", `{"model":"test-model"}`); status != http.StatusBadRequest || !strings.Contains(body, "must have messages") {
		t.Errorf("a request with nothing to say: %d %s", status, body)
	}
	stop()
	if err := <-stopped; err != nil {
		t.Errorf("the service stopped with %v", err)
	}

	taken, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer taken.Close()
	spoiled := t.TempDir()
	mustCLI(t, "pool", "join", "--data-dir", spoiled, "--addr", addr, invite(t, addr, "client"))
	os.Mkdir(filepath.Join(spoiled, "api.token"), 0o700)
	for _, tt := range []struct {
		name string
		args []string
		want string
	}{
		{"a flag there is none of", []string{"--no-such-flag"}, "flag provided but not defined"},
		{"something extra", []string{"extra"}, `unexpected argument "extra"`},
		{"every interface", []string{"--listen", "0.0.0.0:11435"}, "must be a loopback address"},
		{"a pool not joined", []string{"--data-dir", t.TempDir(), "--addr", addr, "--listen", freeAddr(t)}, "reach the pool at"},
		{"no place for a key", []string{"--data-dir", filepath.Join(tokenFile, "under-a-file"), "--addr", addr, "--listen", freeAddr(t)}, "not a directory"},
		{"an address in use", []string{"--data-dir", laptop, "--addr", addr, "--listen", taken.Addr().String()}, "address already in use"},
		{"a token that cannot be read", []string{"--data-dir", spoiled, "--addr", addr, "--listen", freeAddr(t)}, "read API token"},
	} {
		if err := run(context.Background(), append([]string{"inference"}, tt.args...)); err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("inference with %s: %v, want %q", tt.name, err, tt.want)
		}
	}
}

func TestAWorkerOffersOnlyTheModelsItsOwnerNamesAsTheyAskForThem(t *testing.T) {
	served := newModel(t, says("Hello."))
	dataDir := t.TempDir()
	addr, apiAddr := freeAddr(t), freeAddr(t)
	startDaemon(t, "--data-dir", dataDir, "--listen", addr, "--name", "rig", "--api-listen", apiAddr,
		"--models-from", served.URL, "--models-only", " test-model, ,",
		"--models-gpu-layers", "0", "--models-threads", "4", "--models-keep-alive", "1m")
	waitForOutput(t, "rig", "nodes", "--addr", addr)
	workers, err := desktop(t, apiAddr).ListWorkers(context.Background(), &nodepb.ListWorkersRequest{})
	if err != nil || len(workers.GetWorkers()) != 1 || strings.Join(workers.GetWorkers()[0].GetModels(), ",") != "test-model" {
		t.Fatalf("workers = %v, %v", workers, err)
	}
	// A reply is asked of the worker's Ollama with the owner's settings on
	// it, and those the owner left to Ollama left out.
	if out := mustCLI(t, "job", "submit", "--addr", addr, "--workload", "chat", "--params", `{"model":"test-model","messages":[{"role":"user","content":"Hi"}]}`); !strings.Contains(out, "Hello.") {
		t.Fatalf("the reply: %s", out)
	}
	served.mu.Lock()
	defer served.mu.Unlock()
	asked := served.asked[len(served.asked)-1]
	if options, _ := asked["options"].(map[string]any); options["num_gpu"] != float64(0) || options["num_thread"] != float64(4) || len(options) != 2 || asked["keep_alive"] != "1m" {
		t.Errorf("the worker's Ollama was asked %v", asked)
	}
}

func TestAWorkerLendsTheModelsSavedFromTheDesktopWhenStartedWithoutFlags(t *testing.T) {
	served := newModel(t, says("Hello."))
	dataDir := t.TempDir()
	db, err := nodedb.Open(filepath.Join(dataDir, "node.db"))
	if err != nil {
		t.Fatal(err)
	}
	saved := nodedb.ModelLending{Enabled: true, URL: served.URL, Only: []string{"test-model"}, GPULayers: 0, KeepAlive: "1m"}
	if err := db.SetModelLending(saved); err != nil {
		t.Fatal(err)
	}
	db.Close()
	addr, apiAddr := freeAddr(t), freeAddr(t)
	startDaemon(t, "--data-dir", dataDir, "--listen", addr, "--name", "rig", "--api-listen", apiAddr)
	waitForOutput(t, "rig", "nodes", "--addr", addr)
	client := desktop(t, apiAddr)
	workers, err := client.ListWorkers(context.Background(), &nodepb.ListWorkersRequest{})
	if err != nil || len(workers.GetWorkers()) != 1 || strings.Join(workers.GetWorkers()[0].GetModels(), ",") != "test-model" {
		t.Fatalf("workers = %v, %v", workers, err)
	}
	token, _ := os.ReadFile(filepath.Join(dataDir, "api.token"))
	authorized := metadata.AppendToOutgoingContext(context.Background(), "authorization", "Bearer "+strings.TrimSpace(string(token)))
	state, err := client.GetModelLending(authorized, &nodepb.GetModelLendingRequest{})
	if err != nil || state.GetSetByFlags() || state.GetRestartNeeded() || state.GetRunning().GetOllamaUrl() != served.URL {
		t.Errorf("lending = %v, %v; want what was saved, running", state, err)
	}
	if out := mustCLI(t, "job", "submit", "--addr", addr, "--workload", "chat", "--params", `{"model":"test-model","messages":[{"role":"user","content":"Hi"}]}`); !strings.Contains(out, "Hello.") {
		t.Fatalf("the reply: %s", out)
	}
	served.mu.Lock()
	defer served.mu.Unlock()
	asked := served.asked[len(served.asked)-1]
	if options, _ := asked["options"].(map[string]any); options["num_gpu"] != float64(0) || len(options) != 1 || asked["keep_alive"] != "1m" {
		t.Errorf("the worker's Ollama was asked %v", asked)
	}
}

func TestANodeWhoseSavedLendingCannotBeReadDoesNotStart(t *testing.T) {
	dataDir := t.TempDir()
	db, err := nodedb.Open(filepath.Join(dataDir, "node.db"))
	if err != nil {
		t.Fatal(err)
	}
	db.Close()
	raw, err := sql.Open("sqlite", filepath.Join(dataDir, "node.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`DROP TABLE model_lending`); err != nil {
		t.Fatal(err)
	}
	raw.Close()
	if _, err := cli(t, "run", "--data-dir", dataDir, "--listen", freeAddr(t)); err == nil || !strings.Contains(err.Error(), "load model lending") {
		t.Errorf("a node whose lending cannot be read: %v", err)
	}
}

package main

import (
	"net"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ipfs/go-cid"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"

	"github.com/excho0/Sisyphus/apps/sisyphusd/access"
	pb "github.com/excho0/Sisyphus/packages/protocol/sisyphus/v1"
	"github.com/excho0/Sisyphus/packages/runtime"
)

// wordcountOn runs a wordcount job over input as the given number of tasks
// and fails the test unless it succeeds.
func (p *pool) wordcountOn(input string, tasks uint32) *pb.Job {
	p.t.Helper()
	job := p.wait(p.submit(&pb.JobSpec{Workload: "wordcount", Params: []byte(`{"input":"` + input + `"}`), MaxTasks: tasks}).GetJobId())
	if job.GetState() != pb.JobState_JOB_STATE_SUCCEEDED {
		p.t.Fatalf("job %v: %s", job.GetState(), job.GetError())
	}
	return job
}

func TestWorkersFetchFromEachOtherRatherThanTheCoordinator(t *testing.T) {
	p := startPool(t, runtime.Builtin())
	input := p.upload(sampleText())

	// The first worker can only get the input from the coordinator.
	p.startWorker("first", 2)
	p.waitForWorkers(1)
	p.wordcountOn(input, 2)
	if got := p.blobGets.Load(); got != 1 {
		t.Fatalf("coordinator served %d downloads to a lone worker, want 1", got)
	}

	// A second worker gets it from the first.
	p.startWorker("second", 2)
	p.waitForWorkers(2)
	job := p.wordcountOn(input, 4)
	ranOn := make(map[string]bool)
	for _, task := range job.GetTasks() {
		ranOn[task.GetNodeName()] = true
	}
	if !ranOn["second"] {
		t.Fatalf("no task ran on the second worker: %v", ranOn)
	}
	if has, _ := p.workerStores["second"].Has(p.ctx, cid.MustParse(input)); !has {
		t.Error("the second worker does not hold the input")
	}
	if got := p.blobGets.Load(); got != 1 {
		t.Errorf("coordinator has now served %d downloads, want still 1: the second worker should have fetched from the first", got)
	}
}

// locate asks the coordinator, as a newly admitted worker, who holds a blob.
func (p *pool) locate(creds credentials.TransportCredentials, blob string) []string {
	p.t.Helper()
	conn, err := grpc.NewClient(p.addr, grpc.WithTransportCredentials(creds))
	if err != nil {
		p.t.Fatal(err)
	}
	defer conn.Close()
	located, err := pb.NewBlobServiceClient(conn).Locate(p.ctx, &pb.LocateBlobRequest{Cid: blob})
	if err != nil {
		p.t.Fatal(err)
	}
	var names []string
	for _, holder := range located.GetHolders() {
		names = append(names, holder.GetAddress())
	}
	return names
}

func TestCoordinatorKnowsWhichWorkersHoldABlob(t *testing.T) {
	p := startPool(t, runtime.Builtin())
	serving := func(name, address string) *rawWorker {
		msg := hello(name, 1, "primes")
		msg.GetHello().ServeAddress = address
		w := p.connectRaw(msg)
		w.welcome()
		return w
	}
	a, b := serving("a", "10.0.0.1:7701"), serving("b", "10.0.0.2:7701")
	silent := serving("silent", "") // holds blobs but does not serve them
	_, asker := p.admit(access.Worker)

	// Each worker is handed one task and reports what it opened and stored.
	job := p.submit(primesJob(pb.ScheduleMode_SCHEDULE_MODE_DISTRIBUTED, 3))
	report := func(w *rawWorker, read, written []string) {
		task := w.assignment()
		w.reply(&pb.TaskResult{
			TaskId: task.GetTaskId(), Attempt: task.GetAttempt(), Outcome: &pb.TaskResult_Output{Output: []byte(`{"count":1}`)},
			ReadBlobs: read, WrittenBlobs: written,
		})
	}
	report(a, []string{"input"}, []string{"part-a"})
	report(b, []string{"input"}, nil)
	report(silent, []string{"input"}, nil)
	p.wait(job.GetJobId())

	if got := strings.Join(p.locate(asker, "input"), " "); got != "10.0.0.1:7701 10.0.0.2:7701" && got != "10.0.0.2:7701 10.0.0.1:7701" {
		t.Errorf("holders of the input: %q, want the two workers that serve", got)
	}
	if got := p.locate(asker, "part-a"); len(got) != 1 || got[0] != "10.0.0.1:7701" {
		t.Errorf("holders of what a stored: %v", got)
	}
	if got := p.locate(asker, "nobody-has-this"); len(got) != 0 {
		t.Errorf("holders of an unknown blob: %v", got)
	}
	// A worker is not pointed at itself.
	if got := p.locate(p.credentialsFor(p.rawIdents[a]), "input"); len(got) != 1 || got[0] != "10.0.0.2:7701" {
		t.Errorf("holders of the input, as seen by one of them: %v", got)
	}

	// A worker that has gone is no longer offered.
	a.cancel()
	p.waitForWorkers(2)
	if got := p.locate(asker, "input"); len(got) != 1 || got[0] != "10.0.0.2:7701" {
		t.Errorf("holders after one left: %v", got)
	}
}

func TestServeFlagUsage(t *testing.T) {
	taken, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer taken.Close()
	addr := freeAddr(t)
	startDaemon(t, "--role", "coordinator", "--listen", addr)
	joined := t.TempDir()
	nodeID(t, joined)
	if _, err := as(t, joined, "pool", "join", "--addr", addr, invite(t, addr, "worker")); err != nil {
		t.Fatal(err)
	}

	worker := []string{"run", "--role", "worker", "--coordinator", addr, "--data-dir", joined}
	tests := []struct {
		name string
		args []string
		want string
	}{
		{"on a coordinator", []string{"run", "--serve", "127.0.0.1:0", "--data-dir", t.TempDir()}, "--serve is for worker-only nodes"},
		{"on every interface, with nothing to advertise", append(worker, "--serve", "0.0.0.0:7701"), "give that address with --advertise"},
		{"not an address", append(worker, "--serve", "nonsense"), "needs an address other workers can connect to"},
		{"an address in use", append(worker, "--serve", taken.Addr().String()), "address already in use"},
	}
	for _, tt := range tests {
		if _, err := cli(t, tt.args...); err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("--serve %s: error %v, want one containing %q", tt.name, err, tt.want)
		}
	}
}

func TestWorkerDaemonsServeEachOther(t *testing.T) {
	addr := freeAddr(t)
	startDaemon(t, "--role", "coordinator", "--listen", addr)
	firstDir, secondDir := t.TempDir(), t.TempDir()
	text := strings.Repeat("the boulder rolls down, and Sisyphus walks after it.\n", 20_000)
	input := strings.TrimSpace(mustCLI(t, "blob", "put", "--addr", addr, writeFile(t, text)))

	// Listening on every interface is fine so long as others are told where
	// to come.
	port := freeAddr(t)[len("127.0.0.1:"):]
	startDaemon(t, "--role", "worker", "--coordinator", addr, "--data-dir", firstDir, "--name", "first", "--slots", "2",
		"--serve", "0.0.0.0:"+port, "--advertise", "127.0.0.1:"+port)
	waitForOutput(t, "first", "nodes", "--addr", addr)
	mustCLI(t, "job", "submit", "--addr", addr, "--workload", "wordcount", "--tasks", "2", "--params", `{"input":"`+input+`"}`)

	startDaemon(t, "--role", "worker", "--coordinator", addr, "--data-dir", secondDir, "--name", "second", "--slots", "2",
		"--serve", freeAddr(t))
	waitForOutput(t, "second", "nodes", "--addr", addr)
	out := mustCLI(t, "job", "submit", "--addr", addr, "--workload", "wordcount", "--tasks", "4", "--params", `{"input":"`+input+`"}`)
	if !strings.Contains(out, "on second") || !strings.Contains(out, `"words":180000`) {
		t.Fatalf("job output:\n%s", out)
	}
	// The second worker has the input in its cache.
	if blocks, _ := filepath.Glob(filepath.Join(secondDir, "cache", "blocks", "*", "*.data")); len(blocks) < 4 {
		t.Errorf("the second worker's cache holds %d block files", len(blocks))
	}
}

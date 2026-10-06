package main

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ipfs/go-cid"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"

	"github.com/sisyphus-network/Sisyphus/apps/sisyphusd/access"
	"github.com/sisyphus-network/Sisyphus/apps/sisyphusd/worker"
	pb "github.com/sisyphus-network/Sisyphus/packages/protocol/sisyphus/v1"
	"github.com/sisyphus-network/Sisyphus/packages/runtime"
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
	// Wait for the coordinator to be up before storing anything on it.
	waitForOutput(t, "no workers connected", "nodes", "--addr", addr)
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

func TestWorkersWithNoPortOpenFetchFromEachOther(t *testing.T) {
	addr := freeAddr(t)
	startDaemon(t, "--role", "coordinator", "--listen", addr)
	firstDir, secondDir := t.TempDir(), t.TempDir()
	waitForOutput(t, "no workers connected", "nodes", "--addr", addr)
	text := strings.Repeat("the boulder rolls down, and Sisyphus walks after it.\n", 20_000)
	input := strings.TrimSpace(mustCLI(t, "blob", "put", "--addr", addr, writeFile(t, text)))

	// Neither worker is told to serve anything or given an address.
	startDaemon(t, "--role", "worker", "--coordinator", addr, "--data-dir", firstDir, "--name", "first", "--slots", "2")
	waitForOutput(t, "first", "nodes", "--addr", addr)
	mustCLI(t, "job", "submit", "--addr", addr, "--workload", "wordcount", "--tasks", "2", "--params", `{"input":"`+input+`"}`)

	logs := captureLogs(t)
	startDaemon(t, "-v", "--role", "worker", "--coordinator", addr, "--data-dir", secondDir, "--name", "second", "--slots", "2")
	waitForOutput(t, "second", "nodes", "--addr", addr)
	out := mustCLI(t, "job", "submit", "--addr", addr, "--workload", "wordcount", "--tasks", "4", "--params", `{"input":"`+input+`"}`)
	if !strings.Contains(out, "on second") || !strings.Contains(out, `"words":180000`) {
		t.Fatalf("job output:\n%s", out)
	}
	// The second worker got the input from the first, which it knew only
	// by name, and not from the coordinator.
	want := `msg="fetched a blob from a fellow worker" cid=` + input + ` node=` + nodeID(t, firstDir) + ` at=p2p`
	if !strings.Contains(logs.String(), want) {
		t.Errorf("the second worker did not log %s", want)
	}
}

func TestAWorkerWhoseRecordOfItsCoordinatorIsNoNodeIDSaysSo(t *testing.T) {
	dataDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dataDir, "known.json"), []byte(`{"127.0.0.1:1":"not-a-node-id"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := cli(t, "run", "--role", "worker", "--coordinator", "127.0.0.1:1", "--data-dir", dataDir)
	if err == nil || !strings.Contains(err.Error(), "relay address") {
		t.Errorf("error %v, want the coordinator's ID refused", err)
	}
}

func TestACoordinatorsAddressAsARelay(t *testing.T) {
	for hostport, want := range map[string][]string{
		"10.0.0.5:7700":        {"/ip4/10.0.0.5/tcp/7700/p2p/12D3KooWx"},
		"rig.example.net:7700": {"/dns/rig.example.net/tcp/7700/p2p/12D3KooWx"},
		"no port":              nil,
	} {
		if got := relayAddrs(hostport, "12D3KooWx"); !slices.Equal(got, want) {
			t.Errorf("relayAddrs(%q) = %v, want %v", hostport, got, want)
		}
	}
}

func TestTheCoordinatorKnowsWhichWorkersRelayAndWhatTheyHaveCarried(t *testing.T) {
	p := startPool(t, runtime.Builtin())
	p.heartbeat = 10 * time.Millisecond
	p.startWorker("plain", 1)
	const at = "/ip4/203.0.113.7/tcp/7701/p2p/12D3KooWAMv5mPojCf7tz1PH9F3VCPzqCyc8t2onRa6ihxoPh7TK"
	var connections, carried atomic.Uint64
	p.tune = func(w *worker.Worker) {
		w.RelayAddresses = []string{at}
		w.Relayed = func() (uint64, uint64) { return connections.Load(), carried.Load() }
	}
	p.startWorker("full-node", 1)
	p.waitForWorkers(2)

	relaying := func() (found *pb.NodeInfo) {
		for _, node := range p.coord.Nodes() {
			if node.GetName() == "full-node" {
				found = node
			}
		}
		return found
	}
	if got := relaying().GetRelayAddresses(); !slices.Equal(got, []string{at}) {
		t.Errorf("the full node relays at %v", got)
	}
	// What it has carried arrives with its next report.
	connections.Store(3)
	carried.Store(5 << 20)
	waitFor(t, func() bool { return relaying().GetRelayedConnections() == 3 && relaying().GetRelayedBytes() == 5<<20 })

	// A worker asks which members relay, and is told of that one alone.
	_, creds := p.admit(access.Worker)
	conn, err := grpc.NewClient(p.addr, grpc.WithTransportCredentials(creds))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	members := worker.NewMembers(conn)
	if got := members.Relays(p.ctx); !slices.Equal(got, []string{at}) {
		t.Errorf("relays as a worker is told: %v", got)
	}
	// With the coordinator out of reach there is nothing to add.
	gone, cancel := context.WithCancel(p.ctx)
	cancel()
	if got := members.Relays(gone); len(got) != 0 {
		t.Errorf("relays with nobody to ask: %v", got)
	}
}

func TestByteCounts(t *testing.T) {
	for n, want := range map[uint64]string{
		0: "0 B", 1023: "1023 B", 1024: "1.0 KiB", 1536: "1.5 KiB", 5 << 20: "5.0 MiB", 3 << 30: "3.0 GiB", 1 << 62: "4096.0 PiB",
	} {
		if got := byteCount(n); got != want {
			t.Errorf("byteCount(%d) = %q, want %q", n, got, want)
		}
	}
}

func TestOnlyAWorkerOnlyNodeIsToldToRelay(t *testing.T) {
	_, err := cli(t, "run", "--data-dir", t.TempDir(), "--listen", freeAddr(t), "--relay", freeAddr(t))
	if err == nil || !strings.Contains(err.Error(), "--relay is for worker-only nodes") {
		t.Errorf("error %v", err)
	}
}

func TestAWorkerWithAPortOpenRelaysForThePool(t *testing.T) {
	logs := captureLogs(t)
	addr := freeAddr(t)
	startDaemon(t, "--role", "coordinator", "--listen", addr)
	waitForOutput(t, "no workers connected", "nodes", "--addr", addr)
	relayAddr := freeAddr(t)
	fullDir := t.TempDir()
	startDaemon(t, "--role", "worker", "--coordinator", addr, "--data-dir", fullDir, "--name", "full-node", "--slots", "1", "--relay", relayAddr)
	startDaemon(t, "--role", "worker", "--coordinator", addr, "--name", "behind-nat", "--slots", "1")
	waitForOutput(t, "behind-nat", "nodes", "--addr", addr)
	out := waitForOutput(t, "full-node", "nodes", "--addr", addr)
	if !strings.Contains(out, "RELAYED") {
		t.Errorf("nodes output has no column for what was relayed:\n%s", out)
	}

	// The one that relays is listed as doing so, with what it has carried,
	// and the one that does not is not.
	host, port, _ := net.SplitHostPort(relayAddr)
	if !strings.Contains(logs.String(), `msg="relaying between the pool's members" addrs=[/ip4/`+host+`/tcp/`+port+`/p2p/`+nodeID(t, fullDir)+`]`) {
		t.Errorf("the full node did not say where it relays; the log:\n%s", logs)
	}
	for _, line := range strings.Split(out, "\n") {
		relays := strings.Contains(line, "0 B in 0 connections")
		if strings.HasPrefix(line, "full-node") != relays && (strings.HasPrefix(line, "full-node") || strings.HasPrefix(line, "behind-nat")) {
			t.Errorf("nodes line %q", line)
		}
	}
}

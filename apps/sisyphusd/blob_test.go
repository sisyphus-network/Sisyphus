package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"google.golang.org/grpc"

	pb "github.com/sisyphus-network/Sisyphus/packages/protocol/sisyphus/v1"
)

// CIDs that Kubo gives these inputs; packages/storage tests the full set.
const (
	helloCID    = "bafkreifjjcie6lypi6ny7amxnfftagclbuxndqonfipmb64f2km2devei4"
	megabyteCID = "bafybeiczwqoyribvfkrxcuhcjnwva43kuvcrl2h57ergkeszwguynobh5a"
)

func megabyte() []byte {
	b := make([]byte, 1_000_000)
	state := uint64(1)
	for i := range b {
		state = state*6364136223846793005 + 1442695040888963407
		b[i] = byte(state >> 56)
	}
	return b
}

// startNode runs a daemon and returns its address once it is serving.
func startNode(t *testing.T, args ...string) (addr string, stop func()) {
	t.Helper()
	addr = freeAddr(t)
	stop = startDaemon(t, append([]string{"--listen", addr, "--slots", "1"}, args...)...)
	waitForOutput(t, "primes", "nodes", "--addr", addr)
	return addr, stop
}

func TestBlobRoundTripsThroughANode(t *testing.T) {
	addr, _ := startNode(t)
	dir := t.TempDir()
	source := filepath.Join(dir, "source")
	if err := os.WriteFile(source, megabyte(), 0o600); err != nil {
		t.Fatal(err)
	}

	out, err := cli(t, "blob", "put", "--addr", addr, source)
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(out) != megabyteCID {
		t.Fatalf("blob put printed %q, want the CID Kubo gives, %s", out, megabyteCID)
	}

	out, err = cli(t, "blob", "stat", "--addr", addr, megabyteCID)
	if err != nil || out != megabyteCID+": 1000000 bytes\n" {
		t.Errorf("blob stat printed %q, error %v", out, err)
	}

	fetched := filepath.Join(dir, "fetched")
	if _, err := cli(t, "blob", "get", "--addr", addr, "-o", fetched, megabyteCID); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(fetched); !bytes.Equal(got, megabyte()) {
		t.Errorf("fetched file has %d bytes that differ from the source", len(got))
	}

	out, err = cli(t, "blob", "get", "--addr", addr, megabyteCID)
	if err != nil || !bytes.Equal([]byte(out), megabyte()) {
		t.Errorf("blob get to standard output returned %d bytes, error %v", len(out), err)
	}
}

func TestBlobPutReadsStandardInput(t *testing.T) {
	addr, _ := startNode(t)
	stdin = strings.NewReader("hello world\n")
	defer func() { stdin = os.Stdin }()

	out, err := cli(t, "blob", "put", "--addr", addr, "-")
	if err != nil || strings.TrimSpace(out) != helloCID {
		t.Errorf("blob put - printed %q, error %v", out, err)
	}
}

func TestBlobsSurviveANodeRestart(t *testing.T) {
	dataDir := t.TempDir()
	source := filepath.Join(t.TempDir(), "source")
	if err := os.WriteFile(source, megabyte(), 0o600); err != nil {
		t.Fatal(err)
	}

	addr, stop := startNode(t, "--data-dir", dataDir)
	if _, err := cli(t, "blob", "put", "--addr", addr, source); err != nil {
		t.Fatal(err)
	}
	stop()

	addr, _ = startNode(t, "--data-dir", dataDir)
	out, err := cli(t, "blob", "get", "--addr", addr, megabyteCID)
	if err != nil || !bytes.Equal([]byte(out), megabyte()) {
		t.Errorf("after a restart blob get returned %d bytes, error %v", len(out), err)
	}
}

func TestBlobCommandErrors(t *testing.T) {
	addr, _ := startNode(t)
	target := filepath.Join(t.TempDir(), "out")
	tests := []struct {
		args []string
		want string
	}{
		{[]string{"blob"}, "expected blob put, get, stat, pin, unpin, pins, gc, replicas or restore"},
		{[]string{"blob", "put"}, "expected one file"},
		{[]string{"blob", "put", "--addr", addr, filepath.Join(t.TempDir(), "absent")}, "no such file"},
		{[]string{"blob", "get"}, "expected exactly one CID"},
		{[]string{"blob", "get", "--addr", addr, "not-a-cid"}, `invalid CID "not-a-cid"`},
		{[]string{"blob", "get", "--addr", addr, "-o", target, helloCID}, "not found"},
		{[]string{"blob", "stat"}, "expected exactly one CID"},
		{[]string{"blob", "stat", "--addr", addr, "not-a-cid"}, `invalid CID "not-a-cid"`},
		{[]string{"blob", "stat", "--addr", addr, helloCID}, "not found"},
	}
	for _, tt := range tests {
		_, err := cli(t, tt.args...)
		if err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("%v: error %v, want one containing %q", tt.args, err, tt.want)
		}
	}
	if entries, _ := os.ReadDir(filepath.Dir(target)); len(entries) != 0 {
		t.Errorf("a failed download left files behind: %v", entries)
	}
}

// lyingNode is a blob service that answers every request with data and a CID
// that do not belong together.
type lyingNode struct {
	pb.UnimplementedBlobServiceServer
}

func (lyingNode) Get(_ *pb.GetBlobRequest, stream grpc.ServerStreamingServer[pb.GetBlobResponse]) error {
	return stream.Send(&pb.GetBlobResponse{Data: []byte("not what you asked for")})
}

func (lyingNode) Put(stream grpc.ClientStreamingServer[pb.PutBlobRequest, pb.PutBlobResponse]) error {
	for {
		if _, err := stream.Recv(); err != nil {
			break
		}
	}
	return stream.SendAndClose(&pb.PutBlobResponse{Cid: megabyteCID})
}

func startLyingNode(t *testing.T) string {
	t.Helper()
	return serveFake(t, func(srv *grpc.Server) { pb.RegisterBlobServiceServer(srv, lyingNode{}) })
}

func TestBlobGetRejectsDataThatDoesNotMatchItsCID(t *testing.T) {
	addr := startLyingNode(t)
	target := filepath.Join(t.TempDir(), "out")

	_, err := cli(t, "blob", "get", "--addr", addr, "-o", target, helloCID)
	if err == nil || !strings.Contains(err.Error(), "not the "+helloCID+" asked for") {
		t.Errorf("error %v, want a hash mismatch", err)
	}
	if entries, _ := os.ReadDir(filepath.Dir(target)); len(entries) != 0 {
		t.Errorf("a rejected download left files behind: %v", entries)
	}
}

func TestBlobPutRejectsACIDThatDoesNotMatchWhatWasSent(t *testing.T) {
	addr := startLyingNode(t)
	stdin = strings.NewReader("hello world\n")
	defer func() { stdin = os.Stdin }()

	out, err := cli(t, "blob", "put", "--addr", addr, "-")
	if err == nil || !strings.Contains(err.Error(), "the bytes sent hash to "+helloCID) {
		t.Errorf("error %v, want a hash mismatch", err)
	}
	if out != "" {
		t.Errorf("a rejected upload still printed %q", out)
	}
}

func TestTwoNodesCannotShareADataDirectory(t *testing.T) {
	dataDir := t.TempDir()
	startNode(t, "--data-dir", dataDir)

	_, err := cli(t, "run", "--listen", freeAddr(t), "--data-dir", dataDir)
	if err == nil || !strings.Contains(err.Error(), "open blob store") {
		t.Errorf("second node on the same data directory: error %v, want a refusal", err)
	}
}

func TestCLIRunsAJobOverAStoredFileAcrossTwoNodes(t *testing.T) {
	addr, _ := startNode(t)
	// A second, worker-only node with a data directory of its own.
	startDaemon(t, "--role", "worker", "--coordinator", addr, "--name", "helper", "--slots", "3", "--sync-cache", "--max-cache-bytes", "50000000")
	waitForOutput(t, "helper", "nodes", "--addr", addr)

	source := filepath.Join(t.TempDir(), "essay.txt")
	text := strings.Repeat("The struggle itself toward the heights is enough to fill a man's heart.\n", 20_000)
	if err := os.WriteFile(source, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := cli(t, "blob", "put", "--addr", addr, source)
	if err != nil {
		t.Fatal(err)
	}
	input := strings.TrimSpace(out)

	out, err = cli(t, "job", "submit", "--addr", addr, "--workload", "wordcount", "--tasks", "8", "--params", `{"input":"`+input+`"}`)
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if !strings.Contains(out, "on helper") {
		t.Errorf("no task ran on the worker-only node:\n%s", out)
	}
	var result struct {
		Output          string
		Words, Distinct int
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if err := json.Unmarshal([]byte(lines[len(lines)-1]), &result); err != nil {
		t.Fatalf("last line of output is not the result: %v\n%s", err, out)
	}
	// Fourteen words a line ("man's" is two), thirteen of them distinct.
	if result.Words != 14*20_000 || result.Distinct != 13 {
		t.Errorf("counted %d words, %d distinct; want 280000 and 13", result.Words, result.Distinct)
	}

	table, err := cli(t, "blob", "get", "--addr", addr, result.Output)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(table, "40000\tthe\n20000\ta\n20000\tenough\n") {
		t.Errorf("result table starts:\n%.80s", table)
	}
}

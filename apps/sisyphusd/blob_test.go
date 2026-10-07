package main

import (
	"bytes"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"google.golang.org/grpc"

	pb "github.com/excho0/Sisyphus/packages/protocol/sisyphus/v1"
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
		{[]string{"blob"}, `expected "blob put", "blob get" or "blob stat"`},
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
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := grpc.NewServer()
	pb.RegisterBlobServiceServer(srv, lyingNode{})
	go srv.Serve(lis)
	t.Cleanup(srv.Stop)
	return lis.Addr().String()
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

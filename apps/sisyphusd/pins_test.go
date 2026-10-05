package main

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"

	pb "github.com/excho0/Sisyphus/packages/protocol/sisyphus/v1"
	"github.com/excho0/Sisyphus/packages/storage"
)

func writeFile(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// pinLines returns the lines of "blob pins" that mention id.
func pinLines(t *testing.T, addr, id string) []string {
	t.Helper()
	out, err := cli(t, "blob", "pins", "--addr", addr)
	if err != nil {
		t.Fatal(err)
	}
	var lines []string
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, id) {
			lines = append(lines, strings.Join(strings.Fields(line)[1:], " "))
		}
	}
	return lines
}

func has(lines []string, want string) bool {
	for _, line := range lines {
		if line == want {
			return true
		}
	}
	return false
}

func TestBlobPutPinsWhatItStoresUnlessToldNotTo(t *testing.T) {
	addr, _ := startNode(t)

	out, err := cli(t, "blob", "pins", "--addr", addr)
	if err != nil || out != "nothing is pinned\n" {
		t.Errorf("pins on a new node printed %q, error %v", out, err)
	}

	kept := strings.TrimSpace(mustCLI(t, "blob", "put", "--addr", addr, writeFile(t, "kept until unpinned")))
	if lines := pinLines(t, addr, kept); !has(lines, "user released") {
		t.Errorf("blob put left pins %v, want one for the user until released", lines)
	}

	timed := strings.TrimSpace(mustCLI(t, "blob", "put", "--addr", addr, "--ttl", "36h", writeFile(t, "kept for a while")))
	lines := pinLines(t, addr, timed)
	var until time.Time
	for _, line := range lines {
		if rest, ok := strings.CutPrefix(line, "user "); ok {
			until, _ = time.Parse(time.RFC3339, rest)
		}
	}
	if left := time.Until(until); left < 35*time.Hour || left > 36*time.Hour {
		t.Errorf("blob put --ttl 36h left pins %v, want one for the user about 36 hours out", lines)
	}

	loose := strings.TrimSpace(mustCLI(t, "blob", "put", "--addr", addr, "--no-pin", writeFile(t, "not pinned")))
	lines = pinLines(t, addr, loose)
	if len(lines) != 1 || !strings.HasPrefix(lines[0], "recent ") {
		t.Errorf("blob put --no-pin left pins %v, want only the short one every new blob gets", lines)
	}
}

func mustCLI(t *testing.T, args ...string) string {
	t.Helper()
	out, err := cli(t, args...)
	if err != nil {
		t.Fatalf("%v: %v", args, err)
	}
	return out
}

func TestBlobPinAndUnpin(t *testing.T) {
	addr, _ := startNode(t)
	id := strings.TrimSpace(mustCLI(t, "blob", "put", "--addr", addr, "--no-pin", writeFile(t, "pin me later")))

	mustCLI(t, "blob", "pin", "--addr", addr, id)
	if lines := pinLines(t, addr, id); !has(lines, "user released") {
		t.Errorf("after blob pin: %v", lines)
	}
	mustCLI(t, "blob", "pin", "--addr", addr, "--ttl", "1h", id)
	if lines := pinLines(t, addr, id); has(lines, "user released") {
		t.Errorf("blob pin --ttl did not replace the earlier pin: %v", lines)
	}
	mustCLI(t, "blob", "unpin", "--addr", addr, id)
	for _, line := range pinLines(t, addr, id) {
		if strings.HasPrefix(line, "user ") {
			t.Errorf("still pinned for the user after blob unpin: %s", line)
		}
	}

	if _, err := cli(t, "blob", "pin", "--addr", addr, helloCID); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Errorf("pinning a blob the node lacks: %v", err)
	}
}

func TestBlobGCReportsWhatItRemoved(t *testing.T) {
	addr, _ := startNode(t)
	// Nothing stored moments ago can be collected yet.
	mustCLI(t, "blob", "put", "--addr", addr, "--no-pin", writeFile(t, "too new to collect"))
	if out := mustCLI(t, "blob", "gc", "--addr", addr); out != "dropped 0 expired pin(s), removed 0 block(s), freed 0 bytes\n" {
		t.Errorf("printed %q", out)
	}
}

func TestJobsPinTheirBlobsOnARunningNode(t *testing.T) {
	addr, _ := startNode(t, "--retain", "48h")
	input := strings.TrimSpace(mustCLI(t, "blob", "put", "--addr", addr, "--no-pin", writeFile(t, "roll the boulder up the hill")))
	out := mustCLI(t, "job", "submit", "--addr", addr, "--workload", "wordcount", "--params", `{"input":"`+input+`"}`)
	jobID := regexp.MustCompile(`job ([0-9a-f]{16}) succeeded`).FindStringSubmatch(out)
	if jobID == nil {
		t.Fatalf("no finished job in:\n%s", out)
	}

	var until time.Time
	for _, line := range pinLines(t, addr, input) {
		if rest, ok := strings.CutPrefix(line, "job:"+jobID[1]+" "); ok {
			until, _ = time.Parse(time.RFC3339, rest)
		}
	}
	if left := time.Until(until); left < 47*time.Hour || left > 48*time.Hour {
		t.Errorf("the job's input is pinned until %v, want about 48 hours out", until)
	}
}

func TestNodeRefusesUploadsBeyondItsQuota(t *testing.T) {
	addr, _ := startNode(t, "--max-store-bytes", "1")
	_, err := cli(t, "blob", "put", "--addr", addr, writeFile(t, "no room"))
	if err == nil || !strings.Contains(err.Error(), "store is full") {
		t.Errorf("error %v, want the store reported full", err)
	}
}

func TestNodeLetsOpenJobPinsLapseWhenItRestarts(t *testing.T) {
	dataDir := t.TempDir()
	// A pin left open by a job that was running when the node last stopped.
	store, err := storage.OpenLocal(filepath.Join(dataDir, "blobs"))
	if err != nil {
		t.Fatal(err)
	}
	orphan, err := store.Put(context.Background(), strings.NewReader("input of a job that never finished"))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Pin(context.Background(), "job:lost", time.Time{}, orphan); err != nil {
		t.Fatal(err)
	}
	store.Close()

	addr, _ := startNode(t, "--data-dir", dataDir, "--retain", "2h", "--gc-interval", "0")
	lines := pinLines(t, addr, orphan.String())
	var until time.Time
	for _, line := range lines {
		if rest, ok := strings.CutPrefix(line, "job:lost "); ok {
			until, _ = time.Parse(time.RFC3339, rest)
		}
	}
	if left := time.Until(until); left < time.Hour || left > 2*time.Hour {
		t.Errorf("the lost job's pin is %v, want it to lapse about two hours out", lines)
	}
}

func TestNodeWillNotStartIfItCannotUpdateItsPins(t *testing.T) {
	dataDir := t.TempDir()
	// A directory where the pin file's temporary copy must go.
	if err := os.MkdirAll(filepath.Join(dataDir, "blobs", "pins.json.tmp"), 0o700); err != nil {
		t.Fatal(err)
	}
	_, err := cli(t, "run", "--data-dir", dataDir, "--listen", freeAddr(t))
	if err == nil || !strings.Contains(err.Error(), "save pins") {
		t.Errorf("error %v, want a failure to save pins", err)
	}
}

func TestPeriodicCollection(t *testing.T) {
	dir := t.TempDir()
	store, err := storage.OpenLocal(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	lapsed, err := store.Put(ctx, strings.NewReader("pinned until a minute ago"))
	if err != nil {
		t.Fatal(err)
	}
	for _, pin := range store.Pins() {
		store.Unpin(pin.Owner, pin.CID)
	}
	if err := store.Pin(ctx, "user", time.Now().Add(-time.Minute), lapsed); err != nil {
		t.Fatal(err)
	}

	logs := new(syncBuffer)
	collecting, stop := context.WithCancel(ctx)
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		collectPeriodically(collecting, store, 5*time.Millisecond, slog.New(slog.NewTextHandler(logs, nil)))
	}()
	waitFor(t, func() bool { return strings.Contains(logs.String(), `msg="collected garbage" expired_pins=1 blocks=1`) })
	if held, _ := store.Has(ctx, lapsed); held {
		t.Error("the collector logged a collection but the blob is still there")
	}

	// A collection that fails is logged and the collector carries on.
	blocker := filepath.Join(dir, "pins.json.tmp")
	if err := os.Mkdir(blocker, 0o700); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return strings.Contains(logs.String(), "garbage collection failed") })
	if err := os.Remove(blocker); err != nil {
		t.Fatal(err)
	}

	stop()
	<-stopped
	// Collections that found nothing said nothing.
	if n := strings.Count(logs.String(), "collected garbage"); n != 1 {
		t.Errorf("logged %d collections, want only the one that removed something", n)
	}
}

// serveFake runs a stand-in node, with a key of its own, offering whatever
// services register adds. Commands sent to its address are run with that
// same key, so they reach it expecting, and finding, their own node.
func serveFake(t *testing.T, register func(*grpc.Server)) (addr string) {
	t.Helper()
	dataDir := t.TempDir()
	ident, err := loadIdentity(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := grpc.NewServer(grpc.Creds(credentials.NewTLS(ident.ServerTLS())))
	register(srv)
	go srv.Serve(lis)
	t.Cleanup(srv.Stop)
	dataDirs.Store(lis.Addr().String(), dataDir)
	return lis.Addr().String()
}

func waitFor(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !condition() {
		if time.Now().After(deadline) {
			t.Fatal("condition not met in time")
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// pinlessNode stores uploads faithfully but cannot pin, list or collect.
type pinlessNode struct {
	pb.UnimplementedBlobServiceServer
}

func (pinlessNode) Put(stream grpc.ClientStreamingServer[pb.PutBlobRequest, pb.PutBlobResponse]) error {
	var received bytes.Buffer
	for {
		msg, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		received.Write(msg.GetData())
	}
	c, err := storage.CID(stream.Context(), &received)
	if err != nil {
		return err
	}
	return stream.SendAndClose(&pb.PutBlobResponse{Cid: c.String()})
}

func TestBlobCommandsReportANodeThatCannotPin(t *testing.T) {
	addr := serveFake(t, func(srv *grpc.Server) { pb.RegisterBlobServiceServer(srv, pinlessNode{}) })

	stored, err := storage.CID(context.Background(), strings.NewReader("stored but not pinned"))
	if err != nil {
		t.Fatal(err)
	}
	out, err := cli(t, "blob", "put", "--addr", addr, writeFile(t, "stored but not pinned"))
	if err == nil || !strings.Contains(err.Error(), "stored "+stored.String()+" but could not pin it") {
		t.Errorf("blob put: error %v, want one naming the stored blob", err)
	}
	if out != "" {
		t.Errorf("blob put printed %q despite failing", out)
	}
	for _, command := range [][]string{{"blob", "pins"}, {"blob", "gc"}, {"blob", "unpin", stored.String()}} {
		args := append(append([]string{}, command[:2]...), "--addr", addr)
		args = append(args, command[2:]...)
		if _, err := cli(t, args...); err == nil || !strings.Contains(err.Error(), "Unimplemented") {
			t.Errorf("%v: error %v, want the node's refusal", command, err)
		}
	}
}

func TestPinCommandUsage(t *testing.T) {
	tests := []struct {
		args []string
		want string
	}{
		{[]string{"blob", "pin"}, "expected exactly one CID"},
		{[]string{"blob", "unpin"}, "expected exactly one CID"},
		{[]string{"blob", "pins", "extra"}, `unexpected argument "extra"`},
		{[]string{"blob", "gc", "extra"}, `unexpected argument "extra"`},
		{[]string{"blob", "pin", "--no-such-flag"}, "flag provided but not defined"},
		{[]string{"blob", "unpin", "--no-such-flag"}, "flag provided but not defined"},
		{[]string{"blob", "pins", "--no-such-flag"}, "flag provided but not defined"},
		{[]string{"blob", "gc", "--no-such-flag"}, "flag provided but not defined"},
		{[]string{"blob", "pin", "--addr", badAddr, helloCID}, "invalid control character"},
		{[]string{"blob", "unpin", "--addr", badAddr, helloCID}, "invalid control character"},
		{[]string{"blob", "pins", "--addr", badAddr}, "invalid control character"},
		{[]string{"blob", "gc", "--addr", badAddr}, "invalid control character"},
	}
	for _, tt := range tests {
		_, err := cli(t, tt.args...)
		if err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("%v: error %v, want one containing %q", tt.args, err, tt.want)
		}
	}
}

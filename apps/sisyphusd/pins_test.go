package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ipfs/go-cid"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"

	pb "github.com/sisyphus-network/Sisyphus/packages/protocol/sisyphus/v1"
	"github.com/sisyphus-network/Sisyphus/packages/storage"
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

func startEventCollector(t *testing.T, store *storage.Store, interval time.Duration, logs *syncBuffer) (context.CancelFunc, <-chan struct{}) {
	t.Helper()
	changes := newGCChangeSignals()
	store.OnGCChange(changes.notify)
	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		collectOnChanges(ctx, store, interval, changes, slog.New(slog.NewTextHandler(logs, nil)))
	}()
	return cancel, stopped
}

func TestGarbageCollectionOptions(t *testing.T) {
	for _, tt := range []struct {
		name        string
		args        []string
		wantEnabled bool
		wantEvery   time.Duration
	}{
		{name: "default event-driven", wantEnabled: true},
		{name: "explicitly disabled", args: []string{"--gc-interval=0"}},
		{name: "optional anti-entropy", args: []string{"--gc-interval=2m"}, wantEnabled: true, wantEvery: 2 * time.Minute},
	} {
		t.Run(tt.name, func(t *testing.T) {
			fs := flag.NewFlagSet("test", flag.ContinueOnError)
			interval := fs.Duration("gc-interval", 0, "")
			if err := fs.Parse(tt.args); err != nil {
				t.Fatal(err)
			}
			got := garbageCollectionOptionsFrom(fs, *interval)
			if got.enabled != tt.wantEnabled || got.interval != tt.wantEvery {
				t.Fatalf("garbage collection options = %+v, want enabled=%t interval=%s", got, tt.wantEnabled, tt.wantEvery)
			}
		})
	}
}

func TestEventDrivenCollectionSweepsAtStartupAndAfterUnpin(t *testing.T) {
	dir := t.TempDir()
	store, err := storage.OpenLocal(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	orphan, err := store.Put(ctx, strings.NewReader("orphaned before startup"))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Unpin(storage.GraceOwner, orphan); err != nil {
		t.Fatal(err)
	}

	logs := new(syncBuffer)
	cancel, stopped := startEventCollector(t, store, 0, logs)
	waitFor(t, func() bool {
		held, _ := store.Has(ctx, orphan)
		return !held
	})

	owned, err := store.PutFor(ctx, "user", strings.NewReader("released after startup"))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Unpin("user", owned); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool {
		held, _ := store.Has(ctx, owned)
		return !held
	})
	cancel()
	<-stopped
}

func TestEventDrivenCollectionCoalescesPinReleases(t *testing.T) {
	dir := t.TempDir()
	store, err := storage.OpenLocal(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	orphan, err := store.Put(ctx, strings.NewReader("startup sweep"))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Unpin(storage.GraceOwner, orphan); err != nil {
		t.Fatal(err)
	}
	var cids []cid.Cid
	for _, content := range []string{"first release", "second release", "third release"} {
		cid, err := store.PutFor(ctx, "user", strings.NewReader(content))
		if err != nil {
			t.Fatal(err)
		}
		cids = append(cids, cid)
	}

	logs := new(syncBuffer)
	cancel, stopped := startEventCollector(t, store, 0, logs)
	// Wait for the startup sweep to finish before releasing the other blobs;
	// otherwise a release during its mark/sweep can correctly require a second pass.
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(logs.String(), `blocks=1`) && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if !strings.Contains(logs.String(), `blocks=1`) {
		cancel()
		<-stopped
		t.Fatalf("collector did not finish its startup sweep: %s", logs.String())
	}
	for _, cid := range cids {
		if err := store.Unpin("user", cid); err != nil {
			t.Fatal(err)
		}
	}
	deadline = time.Now().Add(5 * time.Second)
	for !strings.Contains(logs.String(), `blocks=3`) && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if !strings.Contains(logs.String(), `blocks=3`) {
		cancel()
		<-stopped
		t.Fatalf("collector did not sweep all released blobs: %s", logs.String())
	}
	cancel()
	<-stopped
	time.Sleep(150 * time.Millisecond)
	if n := strings.Count(logs.String(), "collected garbage"); n != 2 {
		t.Errorf("startup plus coalesced releases caused %d collection sweeps, want 2", n)
	}
}

func TestEventDrivenCollectionWaitsForPinExpiry(t *testing.T) {
	dir := t.TempDir()
	store, err := storage.OpenLocal(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	cid, err := store.PutFor(context.Background(), "temporary", strings.NewReader("expires on a deadline"))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Pin(context.Background(), "temporary", time.Now().Add(40*time.Millisecond), cid); err != nil {
		t.Fatal(err)
	}
	logs := new(syncBuffer)
	cancel, stopped := startEventCollector(t, store, 0, logs)
	waitFor(t, func() bool { return strings.Contains(logs.String(), `expired_pins=1 blocks=1`) })
	if held, _ := store.Has(context.Background(), cid); held {
		t.Error("the expired pin's blob survived its deadline collection")
	}
	cancel()
	<-stopped
}

func TestEventDrivenCollectionDebounceIsBoundedFromFirstRelease(t *testing.T) {
	priorDebounce := gcChangeDebounce
	gcChangeDebounce = 300 * time.Millisecond
	t.Cleanup(func() { gcChangeDebounce = priorDebounce })

	dir := t.TempDir()
	store, err := storage.OpenLocal(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	orphan, err := store.Put(ctx, strings.NewReader("startup sweep"))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Unpin(storage.GraceOwner, orphan); err != nil {
		t.Fatal(err)
	}
	var cids []cid.Cid
	for _, content := range []string{"first sustained release", "second sustained release", "third sustained release"} {
		cid, err := store.PutFor(ctx, "user", strings.NewReader(content))
		if err != nil {
			t.Fatal(err)
		}
		cids = append(cids, cid)
	}

	logs := new(syncBuffer)
	cancel, stopped := startEventCollector(t, store, 0, logs)
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(logs.String(), `blocks=1`) && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if !strings.Contains(logs.String(), `blocks=1`) {
		cancel()
		<-stopped
		t.Fatalf("collector did not finish its startup sweep: %s", logs.String())
	}
	if err := store.Unpin("user", cids[0]); err != nil {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond)
	if err := store.Unpin("user", cids[1]); err != nil {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond)
	if err := store.Unpin("user", cids[2]); err != nil {
		t.Fatal(err)
	}
	deadline = time.Now().Add(250 * time.Millisecond)
	for {
		held, err := store.Has(ctx, cids[0])
		if err != nil {
			cancel()
			<-stopped
			t.Fatal(err)
		}
		if !held {
			break
		}
		if time.Now().After(deadline) {
			cancel()
			<-stopped
			t.Fatalf("first released blob remained past the first-release debounce window: %s", logs.String())
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	<-stopped
}

func TestEventDrivenCollectionRetriesFailureWithBackoff(t *testing.T) {
	priorRetry, priorMaximum := gcRetryInitial, gcRetryMaximum
	gcRetryInitial, gcRetryMaximum = 100*time.Millisecond, 100*time.Millisecond
	t.Cleanup(func() { gcRetryInitial, gcRetryMaximum = priorRetry, priorMaximum })

	dir := t.TempDir()
	store, err := storage.OpenLocal(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	cid, err := store.PutFor(ctx, "released", strings.NewReader("retry after pin journal failure"))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Unpin("released", cid); err != nil {
		t.Fatal(err)
	}
	grace, err := store.Put(ctx, strings.NewReader("grace pin must not delay retries"))
	if err != nil {
		t.Fatal(err)
	}
	blocker := filepath.Join(dir, "pins.json.tmp")
	if err := os.Mkdir(blocker, 0o700); err != nil {
		t.Fatal(err)
	}

	logs := new(syncBuffer)
	cancel, stopped := startEventCollector(t, store, 0, logs)
	waitFor(t, func() bool { return strings.Contains(logs.String(), "garbage collection failed") })
	if err := os.Remove(blocker); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		held, err := store.Has(ctx, cid)
		if err != nil {
			t.Fatal(err)
		}
		if !held {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if held, err := store.Has(ctx, cid); err != nil || held {
		t.Fatalf("failed collection did not retry before an unrelated grace pin expires: held=%t err=%v logs=%s", held, err, logs.String())
	}
	if held, err := store.Has(ctx, grace); err != nil || !held {
		t.Fatalf("retry deleted the blob kept by its grace pin: held=%t err=%v", held, err)
	}
	cancel()
	<-stopped
	if n := strings.Count(logs.String(), "garbage collection failed"); n != 1 {
		t.Errorf("logged %d failed collections, want one before retry", n)
	}
}

func TestPeriodicGarbageCollectionIsOptIn(t *testing.T) {
	dir := t.TempDir()
	store, err := storage.OpenLocal(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, err := store.PutFor(context.Background(), "held", strings.NewReader("still held")); err != nil {
		t.Fatal(err)
	}
	logs := new(syncBuffer)
	cancel, stopped := startEventCollector(t, store, 10*time.Millisecond, logs)
	pinFile := filepath.Join(dir, "pins.json")
	var first os.FileInfo
	waitFor(t, func() bool {
		first, err = os.Stat(pinFile)
		return err == nil
	})
	waitFor(t, func() bool {
		current, statErr := os.Stat(pinFile)
		return statErr == nil && !os.SameFile(first, current)
	})
	cancel()
	<-stopped
}

func TestJobPrunerWaitsForChangeAndDeadline(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var due time.Time
	armed, pruned := atomic.Bool{}, atomic.Bool{}
	checked := make(chan struct{}, 4)
	changed := make(chan struct{}, 1)
	prunedOnce := make(chan struct{}, 1)
	logs := new(syncBuffer)
	next := func() time.Time {
		if armed.Load() && !pruned.Load() {
			at := due
			checked <- struct{}{}
			return at
		}
		checked <- struct{}{}
		return time.Time{}
	}
	prune := func(now time.Time) (int, error) {
		if now.Before(due) {
			t.Errorf("pruned before deadline: %v < %v", now, due)
		}
		pruned.Store(true)
		prunedOnce <- struct{}{}
		return 1, nil
	}
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		runJobPruner(ctx, next, prune, changed, slog.New(slog.NewTextHandler(logs, nil)))
	}()
	<-checked
	due = time.Now().Add(time.Hour)
	armed.Store(true)
	changed <- struct{}{}
	<-checked
	due = time.Now().Add(30 * time.Millisecond)
	changed <- struct{}{}
	select {
	case <-prunedOnce:
	case <-time.After(time.Second):
		t.Fatal("the job was not pruned at its retention deadline")
	}
	cancel()
	<-stopped
	if !strings.Contains(logs.String(), `msg="forgot jobs that finished long ago" jobs=1`) {
		t.Fatalf("successful pruning did not log the number of forgotten jobs: %s", logs.String())
	}
}

func TestJobPrunerStopsWaitingOnCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	due := time.Now().Add(time.Hour)
	checked := make(chan struct{}, 1)
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		runJobPruner(ctx, func() time.Time {
			checked <- struct{}{}
			return due
		}, func(time.Time) (int, error) { return 0, nil }, make(chan struct{}), slog.New(slog.DiscardHandler))
	}()
	<-checked
	cancel()
	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("the pruner did not stop when its context was cancelled")
	}
}

func TestJobPrunerHandlesAnAlreadyDueDeadline(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	due := time.Now().Add(-time.Second)
	pruned := make(chan struct{}, 1)
	prunedDone := atomic.Bool{}
	logs := new(syncBuffer)
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		runJobPruner(ctx, func() time.Time {
			if prunedDone.Load() {
				return time.Time{}
			}
			return due
		}, func(time.Time) (int, error) {
			prunedDone.Store(true)
			pruned <- struct{}{}
			return 0, nil
		}, make(chan struct{}), slog.New(slog.NewTextHandler(logs, nil)))
	}()
	select {
	case <-pruned:
	case <-time.After(time.Second):
		t.Fatal("an overdue record was not pruned")
	}
	cancel()
	<-stopped
	if strings.Contains(logs.String(), "forgot jobs that finished long ago") {
		t.Errorf("pruning no jobs produced a misleading log: %s", logs.String())
	}
}

func TestNotifyPruneCoalescesChanges(t *testing.T) {
	changes := make(chan struct{}, 1)
	notifyPrune(changes)
	notifyPrune(changes)
	select {
	case <-changes:
	default:
		t.Fatal("the change notification was lost")
	}
	select {
	case <-changes:
		t.Fatal("duplicate notifications were not coalesced")
	default:
	}
}

func TestJobPrunerRetriesJournalFailureWithBackoff(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	due := time.Now().Add(10 * time.Millisecond)
	var attempts atomic.Int32
	finished := make(chan struct{}, 1)
	prune := func(time.Time) (int, error) {
		if attempts.Add(1) == 1 {
			return 0, errors.New("the database is locked")
		}
		finished <- struct{}{}
		return 1, nil
	}
	next := func() time.Time {
		if attempts.Load() >= 2 {
			return time.Time{}
		}
		return due
	}
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		runJobPruner(ctx, next, prune, make(chan struct{}), slog.New(slog.DiscardHandler))
	}()
	select {
	case <-finished:
	case <-time.After(3 * time.Second):
		t.Fatal("the failed prune was not retried")
	}
	cancel()
	<-stopped
	if got := attempts.Load(); got != 2 {
		t.Errorf("prune attempts = %d, want one failure and one retry", got)
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
	deadline := time.Now().Add(60 * time.Second)
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

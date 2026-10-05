package worker_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"

	"github.com/excho0/Sisyphus/apps/sisyphusd/api"
	"github.com/excho0/Sisyphus/apps/sisyphusd/coordinator"
	"github.com/excho0/Sisyphus/apps/sisyphusd/worker"
	pb "github.com/excho0/Sisyphus/packages/protocol/sisyphus/v1"
	"github.com/excho0/Sisyphus/packages/runtime"
	"github.com/excho0/Sisyphus/packages/storage"
)

var ctx = context.Background()

func quiet() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// serve runs srv on a loopback port and returns its address and a function
// that stops it.
func serve(t *testing.T, srv *grpc.Server) (addr string, stop func()) {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go srv.Serve(lis)
	t.Cleanup(srv.Stop)
	return lis.Addr().String(), srv.Stop
}

// startCoordinator serves a real coordinator's blob service over store.
func startCoordinator(t *testing.T, store *storage.Store) (addr string, stop func()) {
	t.Helper()
	coord := coordinator.New(coordinator.Config{ID: "coordinator", Workloads: runtime.Builtin(), Store: store, Log: quiet()})
	t.Cleanup(coord.Close)
	return serve(t, api.NewServer(api.Config{Coordinator: coord, Store: store}))
}

func dial(t *testing.T, addr string, local *storage.Store) *worker.RemoteBlobs {
	t.Helper()
	blobs, err := worker.DialBlobs(addr, local)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { blobs.Close() })
	return blobs
}

func read(t *testing.T, blob storage.Blob) []byte {
	t.Helper()
	defer blob.Close()
	data, err := io.ReadAll(blob)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestOpenFetchesFromTheCoordinatorOnceThenUsesTheLocalCopy(t *testing.T) {
	remote, local := storage.NewMemory(), storage.NewMemory()
	data := bytes.Repeat([]byte("the boulder rolls back down. "), 40_000) // spans several chunks
	c, err := remote.Put(ctx, bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	addr, stopCoordinator := startCoordinator(t, remote)
	blobs := dial(t, addr, local)

	blob, err := blobs.Open(ctx, c)
	if err != nil {
		t.Fatal(err)
	}
	if got := read(t, blob); !bytes.Equal(got, data) {
		t.Errorf("fetched %d bytes that differ from the %d stored", len(got), len(data))
	}
	if has, _ := local.Has(ctx, c); !has {
		t.Error("the fetched blob was not kept locally")
	}

	// With the coordinator gone, the blob must still open.
	stopCoordinator()
	blob, err = blobs.Open(ctx, c)
	if err != nil {
		t.Fatalf("opening a cached blob with the coordinator down: %v", err)
	}
	if got := read(t, blob); !bytes.Equal(got, data) {
		t.Error("the cached blob differs from the original")
	}
}

func TestPutAlsoStoresTheBlobOnTheCoordinator(t *testing.T) {
	remote, local := storage.NewMemory(), storage.NewMemory()
	addr, _ := startCoordinator(t, remote)
	blobs := dial(t, addr, local)
	data := bytes.Repeat([]byte("up the hill again. "), 40_000)

	c, err := blobs.Put(ctx, bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	for name, store := range map[string]*storage.Store{"worker": local, "coordinator": remote} {
		blob, err := store.Open(ctx, c)
		if err != nil {
			t.Fatalf("%s store: %v", name, err)
		}
		if got := read(t, blob); !bytes.Equal(got, data) {
			t.Errorf("%s store holds %d bytes that differ from the %d stored", name, len(got), len(data))
		}
	}
}

func TestOpenFailsForABlobNobodyHas(t *testing.T) {
	addr, _ := startCoordinator(t, storage.NewMemory())
	blobs := dial(t, addr, storage.NewMemory())
	absent, err := storage.CID(ctx, strings.NewReader("never stored"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := blobs.Open(ctx, absent); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Errorf("error %v, want one saying the blob was not found", err)
	}
}

func TestPutFailsWhenTheCoordinatorIsUnreachable(t *testing.T) {
	addr, stopCoordinator := startCoordinator(t, storage.NewMemory())
	stopCoordinator()
	blobs := dial(t, addr, storage.NewMemory())
	if _, err := blobs.Put(ctx, strings.NewReader("output")); err == nil || !strings.Contains(err.Error(), "upload to coordinator") {
		t.Errorf("error %v, want an upload failure", err)
	}
}

// impostor is a blob service that sends the same bytes whatever is asked for.
type impostor struct {
	pb.UnimplementedBlobServiceServer
}

func (impostor) Get(_ *pb.GetBlobRequest, stream grpc.ServerStreamingServer[pb.GetBlobResponse]) error {
	return stream.Send(&pb.GetBlobResponse{Data: []byte("not what you asked for")})
}

func TestOpenRejectsBytesThatDoNotMatchTheCID(t *testing.T) {
	srv := grpc.NewServer()
	pb.RegisterBlobServiceServer(srv, impostor{})
	addr, _ := serve(t, srv)
	local := storage.NewMemory()
	blobs := dial(t, addr, local)

	wanted, err := storage.CID(ctx, strings.NewReader("the real input"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := blobs.Open(ctx, wanted); err == nil || !strings.Contains(err.Error(), "not the "+wanted.String()+" asked for") {
		t.Errorf("error %v, want a hash mismatch", err)
	}
	if has, _ := local.Has(ctx, wanted); has {
		t.Error("the wrong bytes were stored under the requested CID")
	}
}

func TestDialBlobsRejectsAMalformedAddress(t *testing.T) {
	if _, err := worker.DialBlobs("bad\x00address", storage.NewMemory()); err == nil {
		t.Error("dialled an address containing a control character")
	}
}

// stalledNode is a blob service that starts a download and never finishes
// it until released.
type stalledNode struct {
	pb.UnimplementedBlobServiceServer
	started chan struct{}
	release chan struct{}
}

func (s stalledNode) Get(_ *pb.GetBlobRequest, stream grpc.ServerStreamingServer[pb.GetBlobResponse]) error {
	s.started <- struct{}{}
	select {
	case <-s.release:
	case <-stream.Context().Done():
	}
	return stream.Send(&pb.GetBlobResponse{Data: []byte("the input")})
}

// A task waiting on another task's download of the same blob must still stop
// when it is cancelled, and must not start a second download.
func TestOpenWaitingOnAnotherDownloadStopsWhenCancelled(t *testing.T) {
	node := stalledNode{started: make(chan struct{}, 4), release: make(chan struct{})}
	srv := grpc.NewServer()
	pb.RegisterBlobServiceServer(srv, node)
	addr, _ := serve(t, srv)
	blobs := dial(t, addr, storage.NewMemory())
	wanted, err := storage.CID(ctx, strings.NewReader("the input"))
	if err != nil {
		t.Fatal(err)
	}

	first := make(chan error, 1)
	go func() {
		blob, err := blobs.Open(ctx, wanted)
		if err == nil {
			blob.Close()
		}
		first <- err
	}()
	<-node.started // the first task's download is under way

	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := blobs.Open(cancelled, wanted); !errors.Is(err, context.Canceled) {
		t.Errorf("waiting task: error %v, want context.Canceled", err)
	}

	// A task that is not cancelled waits and then gets the blob.
	patient := make(chan error, 1)
	go func() {
		blob, err := blobs.Open(ctx, wanted)
		if err == nil {
			blob.Close()
		}
		patient <- err
	}()
	time.Sleep(50 * time.Millisecond) // let it reach the wait

	close(node.release)
	if err := <-first; err != nil {
		t.Errorf("the task doing the download: %v", err)
	}
	if err := <-patient; err != nil {
		t.Errorf("the task that waited: %v", err)
	}
	if got := len(node.started); got != 0 {
		t.Errorf("%d further download(s) were started for the same blob", got)
	}
}

// cachedRun stands for one run of a worker-only node: it opens the cache in
// dir, connects to the coordinator at addr, and closes both when the test
// ends or stop is called.
func cachedRun(t *testing.T, dir, addr string) (blobs *worker.RemoteBlobs, cache *storage.Store, stop func()) {
	t.Helper()
	cache, err := storage.OpenCache(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	blobs, err = worker.DialBlobs(addr, cache)
	if err != nil {
		t.Fatal(err)
	}
	stopped := false
	stop = func() {
		if !stopped {
			stopped = true
			blobs.Close()
			cache.Close()
		}
	}
	t.Cleanup(stop)
	return blobs, cache, stop
}

// largestBlock returns the file holding the biggest block in a cache.
func largestBlock(t *testing.T, dir string) string {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(dir, "blocks", "*", "*.data"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no block files under %s (%v)", dir, err)
	}
	largest, size := "", int64(-1)
	for _, file := range files {
		if info, err := os.Stat(file); err == nil && info.Size() > size {
			largest, size = file, info.Size()
		}
	}
	return largest
}

var cachedData = bytes.Repeat([]byte("one must imagine Sisyphus happy. "), 30_000)

func TestCacheFromAnEarlierRunIsUsedWithoutTheCoordinator(t *testing.T) {
	remote := storage.NewMemory()
	c, err := remote.Put(ctx, bytes.NewReader(cachedData))
	if err != nil {
		t.Fatal(err)
	}
	addr, stopCoordinator := startCoordinator(t, remote)
	dir := t.TempDir()

	first, _, endFirstRun := cachedRun(t, dir, addr)
	blob, err := first.Open(ctx, c)
	if err != nil {
		t.Fatal(err)
	}
	blob.Close()
	endFirstRun()
	stopCoordinator()

	second, _, _ := cachedRun(t, dir, addr)
	blob, err = second.Open(ctx, c)
	if err != nil {
		t.Fatalf("a blob cached by an earlier run needed the coordinator: %v", err)
	}
	if got := read(t, blob); !bytes.Equal(got, cachedData) {
		t.Error("the cached blob differs from the original")
	}
}

func TestDamagedCacheEntryIsDownloadedAgain(t *testing.T) {
	remote := storage.NewMemory()
	c, err := remote.Put(ctx, bytes.NewReader(cachedData))
	if err != nil {
		t.Fatal(err)
	}
	addr, stopCoordinator := startCoordinator(t, remote)
	dir := t.TempDir()

	first, _, endFirstRun := cachedRun(t, dir, addr)
	blob, err := first.Open(ctx, c)
	if err != nil {
		t.Fatal(err)
	}
	blob.Close()
	endFirstRun()

	// What a power cut during the first run could have left behind.
	if err := os.Truncate(largestBlock(t, dir), 1000); err != nil {
		t.Fatal(err)
	}

	second, cache, endSecondRun := cachedRun(t, dir, addr)
	blob, err = second.Open(ctx, c)
	if err != nil {
		t.Fatal(err)
	}
	if got := read(t, blob); !bytes.Equal(got, cachedData) {
		t.Error("a task was given the damaged copy")
	}
	if err := cache.Verify(ctx, c); err != nil {
		t.Errorf("the cache was not repaired: %v", err)
	}
	endSecondRun()

	// With the coordinator gone, a damaged entry cannot be made good, and
	// must not be handed to a task as it is.
	if err := os.Truncate(largestBlock(t, dir), 1000); err != nil {
		t.Fatal(err)
	}
	stopCoordinator()
	third, _, _ := cachedRun(t, dir, addr)
	if _, err := third.Open(ctx, c); err == nil || !strings.Contains(err.Error(), "fetch from coordinator") {
		t.Errorf("error %v, want a failed download", err)
	}
}

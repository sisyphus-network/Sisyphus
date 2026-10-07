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

	"github.com/ipfs/go-cid"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"

	"github.com/excho0/Sisyphus/apps/sisyphusd/access"
	"github.com/excho0/Sisyphus/apps/sisyphusd/api"
	"github.com/excho0/Sisyphus/apps/sisyphusd/coordinator"
	"github.com/excho0/Sisyphus/apps/sisyphusd/worker"
	"github.com/excho0/Sisyphus/packages/identity"
	pb "github.com/excho0/Sisyphus/packages/protocol/sisyphus/v1"
	"github.com/excho0/Sisyphus/packages/runtime"
	"github.com/excho0/Sisyphus/packages/storage"
)

var ctx = context.Background()

func quiet() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// The stand-in coordinators in these tests all answer with one key, and the
// workers all connect with another that expects it.
var (
	coordinatorIdent = mustIdentity()
	workerIdent      = mustIdentity()
	workerCreds      = credentials.NewTLS(workerIdent.ClientTLS(coordinatorIdent.ID()))
)

func mustIdentity() *identity.Identity {
	dir, err := os.MkdirTemp("", "sisyphus-worker-test")
	if err != nil {
		panic(err)
	}
	defer os.RemoveAll(dir)
	ident, _, err := identity.LoadOrCreate(filepath.Join(dir, "node.key"))
	if err != nil {
		panic(err)
	}
	return ident
}

// newServer returns a gRPC server that answers as the coordinator.
func newServer(opts ...grpc.ServerOption) *grpc.Server {
	return grpc.NewServer(append(opts, grpc.Creds(credentials.NewTLS(coordinatorIdent.ServerTLS())))...)
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

// startCoordinator serves a real coordinator's blob service over store, with
// the test worker admitted.
func startCoordinator(t *testing.T, store *storage.Store) (addr string, stop func()) {
	t.Helper()
	admitted, err := access.Open("", coordinatorIdent.ID())
	if err != nil {
		t.Fatal(err)
	}
	if err := admitted.Admit(workerIdent.ID(), access.Worker, time.Now()); err != nil {
		t.Fatal(err)
	}
	coord := coordinator.New(coordinator.Config{ID: coordinatorIdent.ID(), Workloads: runtime.Builtin(), Store: store, Log: quiet()})
	t.Cleanup(coord.Close)
	return serve(t, api.NewServer(api.Config{Identity: coordinatorIdent, Access: admitted, Coordinator: coord, Store: store}))
}

func dial(t *testing.T, addr string, local *storage.Store) *worker.RemoteBlobs {
	t.Helper()
	blobs, err := worker.DialBlobs(addr, workerCreds, local, 0)
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
	srv := newServer()
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
	if _, err := worker.DialBlobs("bad\x00address", workerCreds, storage.NewMemory(), 0); err == nil {
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
	srv := newServer()
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
	return limitedRun(t, dir, addr, 0)
}

// limitedRun is cachedRun with a limit on the cache's size.
func limitedRun(t *testing.T, dir, addr string, maxBytes uint64) (blobs *worker.RemoteBlobs, cache *storage.Store, stop func()) {
	t.Helper()
	cache, err := storage.OpenCache(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	blobs, err = worker.DialBlobs(addr, workerCreds, cache, maxBytes)
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

// Eviction tests use blobs of 300 kB and a cache limited to 800 kB: room for
// two, not three.
const (
	evictionBlobSize = 300_000
	evictionLimit    = 800_000
)

// evictionSetup stores n distinct blobs on a coordinator and returns their
// CIDs and the coordinator's address.
func evictionSetup(t *testing.T, n int) (cids []cid.Cid, remote *storage.Store, addr string) {
	t.Helper()
	remote = storage.NewMemory()
	for i := range n {
		data := bytes.Repeat([]byte{byte('a' + i)}, evictionBlobSize)
		c, err := remote.Put(ctx, bytes.NewReader(data))
		if err != nil {
			t.Fatal(err)
		}
		cids = append(cids, c)
	}
	addr, _ = startCoordinator(t, remote)
	return cids, remote, addr
}

// use opens and closes a blob, as a task reading an input does.
func use(t *testing.T, blobs *worker.RemoteBlobs, c cid.Cid) {
	t.Helper()
	blob, err := blobs.Open(ctx, c)
	if err != nil {
		t.Fatal(err)
	}
	if got := read(t, blob); len(got) != evictionBlobSize {
		t.Fatalf("read %d bytes of a %d-byte blob", len(got), evictionBlobSize)
	}
}

// cached reports which of the blobs the cache holds, as a string of letters:
// "ac" means the first and third.
func cached(t *testing.T, cache *storage.Store, cids []cid.Cid) string {
	t.Helper()
	var held []byte
	for i, c := range cids {
		if has, err := cache.Has(ctx, c); err != nil {
			t.Fatal(err)
		} else if has {
			held = append(held, byte('a'+i))
		}
	}
	return string(held)
}

func TestCacheEvictsTheLeastRecentlyUsedBlobWhenOverItsLimit(t *testing.T) {
	cids, _, addr := evictionSetup(t, 4)
	blobs, cache, _ := limitedRun(t, t.TempDir(), addr, evictionLimit)

	use(t, blobs, cids[0])
	use(t, blobs, cids[1])
	if got := cached(t, cache, cids); got != "ab" {
		t.Fatalf("cache holds %q with room to spare, want ab", got)
	}
	use(t, blobs, cids[2])
	if got := cached(t, cache, cids); got != "bc" {
		t.Errorf("after a third blob the cache holds %q, want the oldest gone: bc", got)
	}

	// Using a blob again makes it the newest.
	use(t, blobs, cids[1])
	use(t, blobs, cids[3])
	if got := cached(t, cache, cids); got != "bd" {
		t.Errorf("cache holds %q, want the one used longest ago gone: bd", got)
	}
	if size, _ := cache.Size(ctx); size > evictionLimit {
		t.Errorf("cache is %d bytes, over its %d limit", size, evictionLimit)
	}

	// An evicted blob is simply downloaded again when next wanted.
	use(t, blobs, cids[0])
	if got := cached(t, cache, cids); got != "ad" {
		t.Errorf("cache holds %q after re-fetching an evicted blob, want ad", got)
	}
}

func TestCacheNeverEvictsABlobATaskHasOpen(t *testing.T) {
	cids, _, addr := evictionSetup(t, 3)
	blobs, cache, _ := limitedRun(t, t.TempDir(), addr, evictionLimit)

	// The oldest blob is still open when the cache fills.
	open, err := blobs.Open(ctx, cids[0])
	if err != nil {
		t.Fatal(err)
	}
	use(t, blobs, cids[1])
	use(t, blobs, cids[2])
	if got := cached(t, cache, cids); got != "ac" {
		t.Errorf("cache holds %q, want the open blob kept and the next oldest gone: ac", got)
	}
	if got := read(t, open); len(got) != evictionBlobSize {
		t.Errorf("the open blob read back as %d bytes", len(got))
	}

	// Closing twice must not make the cache think a second holder let go.
	second, err := blobs.Open(ctx, cids[2])
	if err != nil {
		t.Fatal(err)
	}
	first, err := blobs.Open(ctx, cids[2])
	if err != nil {
		t.Fatal(err)
	}
	first.Close()
	first.Close()
	use(t, blobs, cids[0])
	use(t, blobs, cids[1])
	if got := cached(t, cache, cids); !strings.Contains(got, "c") {
		t.Errorf("cache holds %q: a blob still open was evicted after another holder closed twice", got)
	}
	second.Close()
}

func TestCacheStaysOverItsLimitRatherThanEvictOpenBlobs(t *testing.T) {
	cids, _, addr := evictionSetup(t, 3)
	blobs, cache, _ := limitedRun(t, t.TempDir(), addr, evictionLimit)

	var open []storage.Blob
	for _, c := range cids {
		blob, err := blobs.Open(ctx, c)
		if err != nil {
			t.Fatal(err)
		}
		open = append(open, blob)
	}
	if got := cached(t, cache, cids); got != "abc" {
		t.Errorf("cache holds %q, want all three open blobs kept", got)
	}
	for _, blob := range open {
		if got := read(t, blob); len(got) != evictionBlobSize {
			t.Errorf("an open blob read back as %d bytes", len(got))
		}
	}
}

func TestCacheEvictsLeftoversFromEarlierRunsFirst(t *testing.T) {
	cids, _, addr := evictionSetup(t, 4)
	dir := t.TempDir()

	earlier, _, endEarlierRun := limitedRun(t, dir, addr, evictionLimit)
	use(t, earlier, cids[0])
	use(t, earlier, cids[1])
	endEarlierRun()

	blobs, cache, _ := limitedRun(t, dir, addr, evictionLimit)
	use(t, blobs, cids[1]) // one leftover is used again in this run
	use(t, blobs, cids[2])
	if got := cached(t, cache, cids); got != "bc" {
		t.Errorf("cache holds %q, want the leftover this run never used gone: bc", got)
	}
}

func TestCacheCountsAndEvictsWhatTasksStore(t *testing.T) {
	cids, remote, addr := evictionSetup(t, 2)
	blobs, cache, _ := limitedRun(t, t.TempDir(), addr, evictionLimit)

	output, err := blobs.Put(ctx, bytes.NewReader(bytes.Repeat([]byte("z"), evictionBlobSize)))
	if err != nil {
		t.Fatal(err)
	}
	use(t, blobs, cids[0])
	use(t, blobs, cids[1])

	if has, _ := cache.Has(ctx, output); has {
		t.Error("a task's output, the oldest thing in a full cache, was not evicted")
	}
	if has, _ := remote.Has(ctx, output); !has {
		t.Error("the output is not on the coordinator")
	}
}

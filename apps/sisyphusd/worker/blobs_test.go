package worker_test

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net"
	"strings"
	"testing"

	"google.golang.org/grpc"

	"github.com/excho0/Sisyphus/apps/sisyphusd/api"
	"github.com/excho0/Sisyphus/apps/sisyphusd/coordinator"
	"github.com/excho0/Sisyphus/apps/sisyphusd/worker"
	pb "github.com/excho0/Sisyphus/packages/protocol/sisyphus/v1"
	"github.com/excho0/Sisyphus/packages/runtime"
	"github.com/excho0/Sisyphus/packages/storage"
)

var ctx = context.Background()

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
	coord := coordinator.New("coordinator", runtime.Builtin(), store, slog.New(slog.NewTextHandler(io.Discard, nil)))
	t.Cleanup(coord.Close)
	return serve(t, api.NewServer(coord, store))
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

package worker_test

import (
	"bytes"
	"context"
	"io"
	"net"
	"testing"

	"github.com/ipfs/go-cid"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"

	"github.com/excho0/Sisyphus/apps/sisyphusd/api"
	"github.com/excho0/Sisyphus/apps/sisyphusd/worker"
	"github.com/excho0/Sisyphus/packages/identity"
	pb "github.com/excho0/Sisyphus/packages/protocol/sisyphus/v1"
	"github.com/excho0/Sisyphus/packages/storage"
)

// directory is a coordinator that says who holds a blob and can supply none
// itself, so that whatever a worker ends up with came from a peer.
type directory struct {
	pb.UnimplementedBlobServiceServer
	holders []*pb.BlobHolder
	asked   int
}

func (d *directory) Locate(context.Context, *pb.LocateBlobRequest) (*pb.LocateBlobResponse, error) {
	d.asked++
	return &pb.LocateBlobResponse{Holders: d.holders}, nil
}

// peer is another worker of the pool, with an identity of its own.
type peer struct {
	ident *identity.Identity
	addr  string
	store *storage.Store
}

// startPeer serves srv as a new node and returns that node. A nil srv means
// the read-only blob service a real worker runs, admitting everyone.
func startPeer(t *testing.T, serve func(*grpc.Server)) *peer {
	t.Helper()
	p := &peer{ident: mustIdentity(), store: storage.NewMemory()}
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var srv *grpc.Server
	if serve == nil {
		srv = api.NewPeerServer(p.ident, p.store, func(context.Context, string) (bool, error) { return true, nil })
	} else {
		srv = grpc.NewServer(grpc.Creds(credentials.NewTLS(p.ident.ServerTLS())))
		serve(srv)
	}
	go srv.Serve(lis)
	t.Cleanup(srv.Stop)
	p.addr = lis.Addr().String()
	return p
}

func (p *peer) holder() *pb.BlobHolder {
	return &pb.BlobHolder{NodeId: p.ident.ID(), Address: p.addr}
}

// workerWith returns a worker's blob access whose coordinator is dir.
func workerWith(t *testing.T, dir *directory) (*worker.RemoteBlobs, *storage.Store) {
	t.Helper()
	srv := newServer()
	pb.RegisterBlobServiceServer(srv, dir)
	addr, _ := serve(t, srv)
	local := storage.NewMemory()
	blobs := dial(t, addr, local)
	blobs.PeerCredentials = func(nodeID string) credentials.TransportCredentials {
		return credentials.NewTLS(workerIdent.ClientTLS(nodeID))
	}
	return blobs, local
}

func TestWorkerTriesEachHolderUntilOneSuppliesTheBlob(t *testing.T) {
	c, err := storage.CID(ctx, bytes.NewReader(cachedData))
	if err != nil {
		t.Fatal(err)
	}
	// Holders the coordinator still lists but which cannot supply the blob.
	gone := startPeer(t, nil).holder()
	gone.Address = "127.0.0.1:1" // nothing listens here
	emptied := startPeer(t, nil) // up, but no longer has it
	lying := startPeer(t, func(srv *grpc.Server) { pb.RegisterBlobServiceServer(srv, lyingPeer{}) })
	// And one that can.
	good := startPeer(t, nil)
	if _, err := good.store.Put(ctx, bytes.NewReader(cachedData)); err != nil {
		t.Fatal(err)
	}

	dir := &directory{holders: []*pb.BlobHolder{
		{NodeId: "12D3KooWexample", Address: "bad\x00address"},
		gone, emptied.holder(), lying.holder(), good.holder(),
	}}
	blobs, local := workerWith(t, dir)

	blob, err := blobs.Open(ctx, c)
	if err != nil {
		t.Fatalf("opening a blob only a peer holds: %v", err)
	}
	if got := read(t, blob); !bytes.Equal(got, cachedData) {
		t.Errorf("read %d bytes that differ from the %d the peer holds", len(got), len(cachedData))
	}
	if err := local.Verify(ctx, c); err != nil {
		t.Errorf("the blob in the worker's cache does not verify: %v", err)
	}
	// Once it has the blob it does not ask again.
	blob, err = blobs.Open(ctx, c)
	if err != nil {
		t.Fatal(err)
	}
	blob.Close()
	if dir.asked != 1 {
		t.Errorf("the coordinator was asked for holders %d times, want once", dir.asked)
	}
}

// lyingPeer claims to have every blob and sends the wrong bytes for it.
type lyingPeer struct {
	pb.UnimplementedBlobServiceServer
}

func (lyingPeer) Stat(context.Context, *pb.StatBlobRequest) (*pb.StatBlobResponse, error) {
	return &pb.StatBlobResponse{Size: 1}, nil
}

func (lyingPeer) Get(_ *pb.GetBlobRequest, stream grpc.ServerStreamingServer[pb.GetBlobResponse]) error {
	return stream.Send(&pb.GetBlobResponse{Data: []byte("not what you asked for")})
}

func TestWorkerFallsBackToTheCoordinatorWhenNoPeerCanSupplyTheBlob(t *testing.T) {
	remote := storage.NewMemory()
	c, err := remote.Put(ctx, bytes.NewReader(cachedData))
	if err != nil {
		t.Fatal(err)
	}
	// A real coordinator, which knows of no holders.
	addr, _ := startCoordinator(t, remote)
	blobs := dial(t, addr, storage.NewMemory())
	blobs.PeerCredentials = func(nodeID string) credentials.TransportCredentials {
		return credentials.NewTLS(workerIdent.ClientTLS(nodeID))
	}
	fellBack := 0
	blobs.OnFallback = func(cid.Cid) { fellBack++ }

	blob, err := blobs.Open(ctx, c)
	if err != nil {
		t.Fatal(err)
	}
	if got := read(t, blob); !bytes.Equal(got, cachedData) {
		t.Error("the blob from the coordinator differs from the original")
	}
	if fellBack != 1 {
		t.Errorf("fell back to the coordinator %d times, want once", fellBack)
	}
}

func TestWorkerGoesToTheCoordinatorWhenItCannotSayWhoHoldsABlob(t *testing.T) {
	// A coordinator that predates the question: it has the blob, but no
	// answer to who else does.
	old := &oldCoordinator{store: storage.NewMemory()}
	c, err := old.store.Put(ctx, bytes.NewReader(cachedData))
	if err != nil {
		t.Fatal(err)
	}
	srv := newServer()
	pb.RegisterBlobServiceServer(srv, old)
	addr, _ := serve(t, srv)
	blobs := dial(t, addr, storage.NewMemory())
	blobs.PeerCredentials = func(nodeID string) credentials.TransportCredentials {
		return credentials.NewTLS(workerIdent.ClientTLS(nodeID))
	}

	blob, err := blobs.Open(ctx, c)
	if err != nil {
		t.Fatalf("opening a blob when holders cannot be located: %v", err)
	}
	if got := read(t, blob); !bytes.Equal(got, cachedData) {
		t.Error("the blob differs from the original")
	}
}

// oldCoordinator serves blobs but does not implement Locate.
type oldCoordinator struct {
	pb.UnimplementedBlobServiceServer
	store *storage.Store
}

func (o *oldCoordinator) Get(req *pb.GetBlobRequest, stream grpc.ServerStreamingServer[pb.GetBlobResponse]) error {
	blob, err := o.store.Open(stream.Context(), cid.MustParse(req.GetCid()))
	if err != nil {
		return err
	}
	defer blob.Close()
	data, err := io.ReadAll(blob)
	if err != nil {
		return err
	}
	return stream.Send(&pb.GetBlobResponse{Data: data})
}

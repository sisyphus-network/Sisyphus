package api

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/ipfs/go-cid"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pb "github.com/excho0/Sisyphus/packages/protocol/sisyphus/v1"
	"github.com/excho0/Sisyphus/packages/storage"
)

// These tests call the blob service's handlers directly with streams and
// stores that fail, covering what a real connection only does when a disk or
// a client breaks at the wrong moment. The working paths are tested over
// real connections in the daemon's own tests.

var errDisk = errors.New("disk on fire")

// brokenStore is a store whose operations fail on request.
type brokenStore struct {
	*storage.Store
	failPut, failOpen, failRead bool
}

func (b brokenStore) Put(ctx context.Context, r io.Reader) (cid.Cid, error) {
	if b.failPut {
		return cid.Undef, errDisk
	}
	return b.Store.Put(ctx, r)
}

func (b brokenStore) Open(ctx context.Context, c cid.Cid) (storage.Blob, error) {
	if b.failOpen {
		return nil, errDisk
	}
	blob, err := b.Store.Open(ctx, c)
	if err != nil || !b.failRead {
		return blob, err
	}
	return unreadableBlob{blob}, nil
}

type unreadableBlob struct{ storage.Blob }

func (unreadableBlob) Read([]byte) (int, error) { return 0, errDisk }

// upload is the server's side of a client's upload.
type upload struct {
	grpc.ServerStream
	chunks  [][]byte
	recvErr error // returned after the chunks instead of a clean end
}

func (u *upload) Context() context.Context { return context.Background() }

func (u *upload) Recv() (*pb.PutBlobRequest, error) {
	if len(u.chunks) > 0 {
		chunk := u.chunks[0]
		u.chunks = u.chunks[1:]
		return &pb.PutBlobRequest{Data: chunk}, nil
	}
	if u.recvErr != nil {
		return nil, u.recvErr
	}
	return nil, io.EOF
}

func (u *upload) SendAndClose(*pb.PutBlobResponse) error { return nil }

// download is the server's side of a client's download.
type download struct {
	grpc.ServerStream
	sendErr error
}

func (d *download) Context() context.Context { return context.Background() }

func (d *download) Send(*pb.GetBlobResponse) error { return d.sendErr }

func storeWithBlob(t *testing.T) (*storage.Store, string) {
	t.Helper()
	store := storage.NewMemory()
	c, err := store.Put(context.Background(), strings.NewReader("a stored blob"))
	if err != nil {
		t.Fatal(err)
	}
	return store, c.String()
}

func TestPutReportsAStoreFailureAsInternal(t *testing.T) {
	service := &blobService{store: brokenStore{Store: storage.NewMemory(), failPut: true}}
	err := service.Put(&upload{chunks: [][]byte{[]byte("data")}})
	if status.Code(err) != codes.Internal || !strings.Contains(err.Error(), "disk on fire") {
		t.Errorf("error %v, want Internal naming the cause", err)
	}
}

// A client that goes away mid-upload is its own doing, not a server fault,
// so its status must reach the caller unchanged.
func TestPutPassesOnTheClientsOwnFailure(t *testing.T) {
	service := &blobService{store: storage.NewMemory()}
	gone := status.Error(codes.Canceled, "client went away")
	err := service.Put(&upload{chunks: [][]byte{[]byte("data")}, recvErr: gone})
	if status.Code(err) != codes.Canceled {
		t.Errorf("error %v, want the client's Canceled status", err)
	}
}

func TestGetStopsWhenTheClientCannotBeSentTo(t *testing.T) {
	store, id := storeWithBlob(t)
	service := &blobService{store: store}
	gone := status.Error(codes.Unavailable, "client went away")
	if err := service.Get(&pb.GetBlobRequest{Cid: id}, &download{sendErr: gone}); !errors.Is(err, gone) {
		t.Errorf("error %v, want the send failure", err)
	}
}

func TestGetReportsAReadFailureAsInternal(t *testing.T) {
	store, id := storeWithBlob(t)
	service := &blobService{store: brokenStore{Store: store, failRead: true}}
	err := service.Get(&pb.GetBlobRequest{Cid: id}, &download{})
	if status.Code(err) != codes.Internal || !strings.Contains(err.Error(), "disk on fire") {
		t.Errorf("error %v, want Internal naming the cause", err)
	}
}

func TestOpenFailureOtherThanNotFoundIsInternal(t *testing.T) {
	store, id := storeWithBlob(t)
	service := &blobService{store: brokenStore{Store: store, failOpen: true}}
	_, err := service.Stat(context.Background(), &pb.StatBlobRequest{Cid: id})
	if status.Code(err) != codes.Internal {
		t.Errorf("Stat: error %v, want Internal", err)
	}
	if err := service.Get(&pb.GetBlobRequest{Cid: id}, &download{}); status.Code(err) != codes.Internal {
		t.Errorf("Get: error %v, want Internal", err)
	}
}

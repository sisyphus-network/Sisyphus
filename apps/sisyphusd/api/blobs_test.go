package api

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

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
	failPin, failGC, failSize   bool
	// used, if set, is what the store claims to occupy on disk.
	used uint64
}

func (b brokenStore) Pin(ctx context.Context, owner string, expires time.Time, cids ...cid.Cid) error {
	if b.failPin {
		return errDisk
	}
	return b.Store.Pin(ctx, owner, expires, cids...)
}

func (b brokenStore) Unpin(owner string, cids ...cid.Cid) error {
	if b.failPin {
		return errDisk
	}
	return b.Store.Unpin(owner, cids...)
}

func (b brokenStore) GC(ctx context.Context, now time.Time) (storage.Collected, error) {
	if b.failGC {
		return storage.Collected{}, errDisk
	}
	return b.Store.GC(ctx, now)
}

func (b brokenStore) Size(context.Context) (uint64, error) {
	if b.failSize {
		return 0, errDisk
	}
	return b.used, nil
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

func TestPinAndUnpinRejectWhatTheyCannotActOn(t *testing.T) {
	store, id := storeWithBlob(t)
	ctx := context.Background()
	absent, err := storage.CID(ctx, strings.NewReader("never stored"))
	if err != nil {
		t.Fatal(err)
	}
	working := &blobService{store: store}
	failing := &blobService{store: brokenStore{Store: store, failPin: true}}

	if _, err := working.Pin(ctx, &pb.PinBlobRequest{Cid: "nope"}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("Pin of a malformed CID: %v", err)
	}
	if _, err := working.Pin(ctx, &pb.PinBlobRequest{Cid: absent.String()}); status.Code(err) != codes.NotFound {
		t.Errorf("Pin of a blob the store lacks: %v", err)
	}
	if _, err := failing.Pin(ctx, &pb.PinBlobRequest{Cid: id}); status.Code(err) != codes.Internal {
		t.Errorf("Pin when the store fails: %v", err)
	}
	if _, err := working.Unpin(ctx, &pb.UnpinBlobRequest{Cid: "nope"}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("Unpin of a malformed CID: %v", err)
	}
	if _, err := failing.Unpin(ctx, &pb.UnpinBlobRequest{Cid: id}); status.Code(err) != codes.Internal {
		t.Errorf("Unpin when the store fails: %v", err)
	}
	if _, err := (&blobService{store: brokenStore{Store: store, failGC: true}}).CollectGarbage(ctx, &pb.CollectGarbageRequest{}); status.Code(err) != codes.Internal {
		t.Errorf("CollectGarbage when the store fails: %v", err)
	}
}

func TestPinsAreHeldForTheUserWithTheRequestedLifetime(t *testing.T) {
	store, id := storeWithBlob(t)
	ctx := context.Background()
	service := &blobService{store: store}

	before := time.Now()
	if _, err := service.Pin(ctx, &pb.PinBlobRequest{Cid: id, TtlSeconds: 3600}); err != nil {
		t.Fatal(err)
	}
	userPin := func() *pb.Pin {
		listed, err := service.ListPins(ctx, &pb.ListPinsRequest{})
		if err != nil {
			t.Fatal(err)
		}
		for _, pin := range listed.GetPins() {
			if pin.GetOwner() == "user" {
				return pin
			}
		}
		return nil
	}
	pin := userPin()
	if pin == nil || pin.GetCid() != id {
		t.Fatalf("no pin held for the user on %s: %v", id, pin)
	}
	if expires := pin.GetExpiresAt().AsTime(); expires.Before(before.Add(time.Hour)) || expires.After(time.Now().Add(time.Hour)) {
		t.Errorf("pin expires %v, want an hour from now", expires)
	}

	// Without a lifetime the pin holds until released.
	if _, err := service.Pin(ctx, &pb.PinBlobRequest{Cid: id}); err != nil {
		t.Fatal(err)
	}
	if pin := userPin(); pin.GetExpiresAt() != nil {
		t.Errorf("a pin with no lifetime expires at %v", pin.GetExpiresAt().AsTime())
	}

	if _, err := service.Unpin(ctx, &pb.UnpinBlobRequest{Cid: id}); err != nil {
		t.Fatal(err)
	}
	if pin := userPin(); pin != nil {
		t.Errorf("still pinned for the user after unpinning: %v", pin)
	}
}

func TestCollectGarbageReportsWhatItRemoved(t *testing.T) {
	store, id := storeWithBlob(t)
	ctx := context.Background()
	// Replace the blob's pins with one that has already lapsed.
	c, err := cid.Decode(id)
	if err != nil {
		t.Fatal(err)
	}
	for _, pin := range store.Pins() {
		store.Unpin(pin.Owner, pin.CID)
	}
	if err := store.Pin(ctx, "user", time.Now().Add(-time.Minute), c); err != nil {
		t.Fatal(err)
	}

	done, err := (&blobService{store: store}).CollectGarbage(ctx, &pb.CollectGarbageRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if done.GetExpiredPins() != 1 || done.GetBlocksRemoved() != 1 || done.GetBytesFreed() != uint64(len("a stored blob")) {
		t.Errorf("reported %v", done)
	}
}

func TestUploadsAreRefusedOnceTheStoreIsFull(t *testing.T) {
	data := []byte("twenty bytes of data")
	tests := []struct {
		name  string
		store brokenStore
		want  codes.Code
	}{
		{"room to spare", brokenStore{used: 50}, codes.OK},
		{"exactly enough room", brokenStore{used: 80}, codes.OK},
		{"one byte short", brokenStore{used: 81}, codes.ResourceExhausted},
		{"already full", brokenStore{used: 100}, codes.ResourceExhausted},
		{"over the limit already", brokenStore{used: 500}, codes.ResourceExhausted},
		{"its size cannot be measured", brokenStore{failSize: true}, codes.Internal},
	}
	for _, tt := range tests {
		tt.store.Store = storage.NewMemory()
		service := &blobService{store: tt.store, quota: 100}
		err := service.Put(&upload{chunks: [][]byte{data[:12], data[12:]}})
		if status.Code(err) != tt.want {
			t.Errorf("%s: %v, want %v", tt.name, err, tt.want)
		}
	}

	// With no limit set, the store's size is not even consulted.
	unlimited := &blobService{store: brokenStore{Store: storage.NewMemory(), failSize: true}}
	if err := unlimited.Put(&upload{chunks: [][]byte{data}}); err != nil {
		t.Errorf("upload with no limit: %v", err)
	}
}

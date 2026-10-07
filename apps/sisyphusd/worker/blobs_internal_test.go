package worker

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/ipfs/go-cid"
	"google.golang.org/grpc"

	pb "github.com/sisyphus-network/Sisyphus/packages/protocol/sisyphus/v1"
	"github.com/sisyphus-network/Sisyphus/packages/storage"
)

var errLocal = errors.New("local disk broke")

// faultyLocal is a worker's own store misbehaving after a successful Put.
type faultyLocal struct {
	*storage.Store
	// failOpen makes reading back fail; swapWith makes it return other data.
	failOpen bool
	swapWith cid.Cid
	failPin  bool
}

func (f faultyLocal) Pin(ctx context.Context, owner string, expires time.Time, cids ...cid.Cid) error {
	if f.failPin {
		return errLocal
	}
	return f.Store.Pin(ctx, owner, expires, cids...)
}

func (f faultyLocal) Open(ctx context.Context, c cid.Cid) (storage.Blob, error) {
	if f.failOpen {
		return nil, errLocal
	}
	if f.swapWith.Defined() {
		return f.Store.Open(ctx, f.swapWith)
	}
	return f.Store.Open(ctx, c)
}

// echoNode acknowledges every upload with the CID of what it received.
type echoNode struct {
	pb.BlobServiceClient
}

func (echoNode) Put(ctx context.Context, _ ...grpc.CallOption) (grpc.ClientStreamingClient[pb.PutBlobRequest, pb.PutBlobResponse], error) {
	return &echoUpload{hasher: storage.NewHasher(ctx)}, nil
}

type echoUpload struct {
	grpc.ClientStream
	hasher *storage.Hasher
}

func (u *echoUpload) Send(req *pb.PutBlobRequest) error {
	_, err := u.hasher.Write(req.GetData())
	return err
}

func (u *echoUpload) CloseAndRecv() (*pb.PutBlobResponse, error) {
	c, err := u.hasher.Sum()
	if err != nil {
		return nil, err
	}
	return &pb.PutBlobResponse{Cid: c.String()}, nil
}

func TestPutFailsWhenTheSourceFails(t *testing.T) {
	blobs := newRemoteBlobs(storage.NewMemory(), echoNode{}, 0)
	broken := io.MultiReader(strings.NewReader("partial"), iotestErr{})
	if _, err := blobs.Put(context.Background(), broken); !errors.Is(err, errLocal) {
		t.Errorf("error %v, want the source's error", err)
	}
}

type iotestErr struct{}

func (iotestErr) Read([]byte) (int, error) { return 0, errLocal }

func TestPutFailsWhenTheBlobCannotBeReadBackForUpload(t *testing.T) {
	blobs := newRemoteBlobs(faultyLocal{Store: storage.NewMemory(), failOpen: true}, echoNode{}, 0)
	if _, err := blobs.Put(context.Background(), strings.NewReader("output")); !errors.Is(err, errLocal) {
		t.Errorf("error %v, want the local store's error", err)
	}
}

// If the local store hands back different bytes than it was given, the
// coordinator would hold something other than the CID the task reports.
func TestPutFailsWhenTheLocalStoreReturnsDifferentBytes(t *testing.T) {
	store := storage.NewMemory()
	other, err := store.Put(context.Background(), strings.NewReader("something else entirely"))
	if err != nil {
		t.Fatal(err)
	}
	blobs := newRemoteBlobs(faultyLocal{Store: store, swapWith: other}, echoNode{}, 0)
	_, err = blobs.Put(context.Background(), strings.NewReader("output"))
	if err == nil || !strings.Contains(err.Error(), "read back from the local store as "+other.String()) {
		t.Errorf("error %v, want a mismatch naming %s", err, other)
	}
}

// A blob that cannot be pinned could be evicted while a task is using it, so
// failing to pin must fail the operation.
func TestBlobsThatCannotBePinnedAreNotHandedOut(t *testing.T) {
	store := storage.NewMemory()
	cached, err := store.Put(context.Background(), strings.NewReader("already in the cache"))
	if err != nil {
		t.Fatal(err)
	}
	blobs := newRemoteBlobs(faultyLocal{Store: store, failPin: true}, echoNode{}, 0)

	if _, err := blobs.Open(context.Background(), cached); !errors.Is(err, errLocal) {
		t.Errorf("Open: %v, want the pin failure", err)
	}
	if _, err := blobs.Put(context.Background(), strings.NewReader("output")); !errors.Is(err, errLocal) {
		t.Errorf("Put: %v, want the pin failure", err)
	}
}

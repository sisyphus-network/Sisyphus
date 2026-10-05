package worker

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/ipfs/go-cid"
	"google.golang.org/grpc"

	pb "github.com/excho0/Sisyphus/packages/protocol/sisyphus/v1"
	"github.com/excho0/Sisyphus/packages/storage"
)

var errLocal = errors.New("local disk broke")

// faultyLocal is a worker's own store misbehaving after a successful Put.
type faultyLocal struct {
	*storage.Store
	// failOpen makes reading back fail; swapWith makes it return other data.
	failOpen bool
	swapWith cid.Cid
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
	blobs := &RemoteBlobs{local: storage.NewMemory(), remote: echoNode{}}
	broken := io.MultiReader(strings.NewReader("partial"), iotestErr{})
	if _, err := blobs.Put(context.Background(), broken); !errors.Is(err, errLocal) {
		t.Errorf("error %v, want the source's error", err)
	}
}

type iotestErr struct{}

func (iotestErr) Read([]byte) (int, error) { return 0, errLocal }

func TestPutFailsWhenTheBlobCannotBeReadBackForUpload(t *testing.T) {
	blobs := &RemoteBlobs{local: faultyLocal{Store: storage.NewMemory(), failOpen: true}, remote: echoNode{}}
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
	blobs := &RemoteBlobs{local: faultyLocal{Store: store, swapWith: other}, remote: echoNode{}}
	_, err = blobs.Put(context.Background(), strings.NewReader("output"))
	if err == nil || !strings.Contains(err.Error(), "read back from the local store as "+other.String()) {
		t.Errorf("error %v, want a mismatch naming %s", err, other)
	}
}

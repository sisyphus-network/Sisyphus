package worker

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"

	"github.com/ipfs/go-cid"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/excho0/Sisyphus/apps/sisyphusd/blobclient"
	pb "github.com/excho0/Sisyphus/packages/protocol/sisyphus/v1"
	"github.com/excho0/Sisyphus/packages/storage"
)

// RemoteBlobs is what a worker-only node gives its workloads for blob
// access: the node's own store, used as a cache in front of the
// coordinator's. A blob is downloaded the first time a task opens it and
// kept, and every blob a task stores is also uploaded, so the coordinator
// holds a job's outputs before it hears the task has finished.
type RemoteBlobs struct {
	local  *storage.Store
	conn   *grpc.ClientConn
	remote pb.BlobServiceClient

	mu       sync.Mutex
	fetching map[cid.Cid]*fetch
}

// fetch is a download in progress that other tasks wanting the same blob
// wait for instead of starting their own.
type fetch struct {
	done chan struct{}
	err  error
}

// DialBlobs returns blob access backed by local and the coordinator at the
// given address. It does not connect until first used.
func DialBlobs(coordinator string, local *storage.Store) (*RemoteBlobs, error) {
	conn, err := grpc.NewClient(coordinator, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return nil, err
	}
	return &RemoteBlobs{
		local:    local,
		conn:     conn,
		remote:   pb.NewBlobServiceClient(conn),
		fetching: make(map[cid.Cid]*fetch),
	}, nil
}

func (b *RemoteBlobs) Close() error {
	return b.conn.Close()
}

func (b *RemoteBlobs) Open(ctx context.Context, c cid.Cid) (storage.Blob, error) {
	blob, err := b.local.Open(ctx, c)
	if !errors.Is(err, storage.ErrNotFound) {
		return blob, err
	}
	if err := b.fetch(ctx, c); err != nil {
		return nil, fmt.Errorf("fetch from coordinator: %w", err)
	}
	return b.local.Open(ctx, c)
}

func (b *RemoteBlobs) fetch(ctx context.Context, c cid.Cid) error {
	b.mu.Lock()
	f, running := b.fetching[c]
	if !running {
		f = &fetch{done: make(chan struct{})}
		b.fetching[c] = f
	}
	b.mu.Unlock()

	if running {
		select {
		case <-f.done:
			return f.err
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	f.err = blobclient.Fetch(ctx, b.remote, c, b.local)
	b.mu.Lock()
	delete(b.fetching, c)
	b.mu.Unlock()
	close(f.done)
	return f.err
}

func (b *RemoteBlobs) Put(ctx context.Context, r io.Reader) (cid.Cid, error) {
	c, err := b.local.Put(ctx, r)
	if err != nil {
		return cid.Undef, err
	}
	blob, err := b.local.Open(ctx, c)
	if err != nil {
		return cid.Undef, err
	}
	defer blob.Close()
	uploaded, err := blobclient.Upload(ctx, b.remote, blob)
	if err != nil {
		return cid.Undef, fmt.Errorf("upload to coordinator: %w", err)
	}
	if !uploaded.Equals(c) {
		return cid.Undef, fmt.Errorf("blob %s read back from the local store as %s", c, uploaded)
	}
	return c, nil
}

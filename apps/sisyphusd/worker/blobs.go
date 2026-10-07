package worker

import (
	"context"
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
//
// The cache outlives the process, but a copy left by an earlier run may be
// incomplete if that run ended badly. So the first time a blob is opened in
// a run, the cached copy is checked in full and downloaded again if it does
// not hold up.
type RemoteBlobs struct {
	local  localStore
	conn   *grpc.ClientConn
	remote pb.BlobServiceClient

	mu sync.Mutex
	// ready holds the blobs known to be sound in the local store: checked,
	// downloaded or stored during this run.
	ready map[cid.Cid]struct{}
	// preparing holds the blobs being checked or downloaded right now.
	preparing map[cid.Cid]*preparation
}

// localStore is the part of a storage.Store that RemoteBlobs uses.
type localStore interface {
	Open(ctx context.Context, c cid.Cid) (storage.Blob, error)
	Put(ctx context.Context, r io.Reader) (cid.Cid, error)
	Verify(ctx context.Context, c cid.Cid) error
}

// preparation is a blob being made ready, which other tasks wanting the same
// blob wait for instead of starting their own.
type preparation struct {
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
		local:     local,
		conn:      conn,
		remote:    pb.NewBlobServiceClient(conn),
		ready:     make(map[cid.Cid]struct{}),
		preparing: make(map[cid.Cid]*preparation),
	}, nil
}

func (b *RemoteBlobs) Close() error {
	return b.conn.Close()
}

func (b *RemoteBlobs) Open(ctx context.Context, c cid.Cid) (storage.Blob, error) {
	if err := b.prepare(ctx, c); err != nil {
		return nil, err
	}
	return b.local.Open(ctx, c)
}

// prepare makes sure the local store holds a sound copy of c, once per run.
func (b *RemoteBlobs) prepare(ctx context.Context, c cid.Cid) error {
	b.mu.Lock()
	if _, ok := b.ready[c]; ok {
		b.mu.Unlock()
		return nil
	}
	p, running := b.preparing[c]
	if !running {
		p = &preparation{done: make(chan struct{})}
		b.preparing[c] = p
	}
	b.mu.Unlock()

	if running {
		select {
		case <-p.done:
			return p.err
		case <-ctx.Done():
			return ctx.Err()
		}
	}

	// A copy that checks out is used as it is; anything else, from "not
	// there" to "damaged", is put right by downloading it.
	if b.local.Verify(ctx, c) != nil {
		if err := blobclient.Fetch(ctx, b.remote, c, b.local); err != nil {
			p.err = fmt.Errorf("fetch from coordinator: %w", err)
		}
	}
	b.mu.Lock()
	delete(b.preparing, c)
	if p.err == nil {
		b.ready[c] = struct{}{}
	}
	b.mu.Unlock()
	close(p.done)
	return p.err
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
	b.mu.Lock()
	b.ready[c] = struct{}{}
	b.mu.Unlock()
	return c, nil
}

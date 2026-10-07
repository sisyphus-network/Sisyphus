package worker

import (
	"context"
	"fmt"
	"io"
	"net"
	"sync"
	"time"

	"github.com/ipfs/go-cid"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"

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

	// limit is the most disk the cache should use; zero means no limit.
	limit uint64
	// trimming lets eviction run alone. Whatever adds a blob to the cache
	// holds it shared until the blob is pinned, so a blob is never swept
	// between arriving and being pinned.
	trimming sync.RWMutex

	// OnFallback is called when a blob the local store could not produce,
	// which with a store on the pool's private network means the network
	// did not supply it, was downloaded from the coordinator instead.
	OnFallback func(c cid.Cid)
	// PeerCredentials, if set, lets blobs be fetched from other workers of
	// the pool before the coordinator is asked for them.
	PeerCredentials PeerCredentialsFunc
	// OnPeerFetch, if set, is told of each blob fetched from another worker
	// and which one it came from.
	OnPeerFetch func(c cid.Cid, holder *pb.BlobHolder)
	// PeerDialer connects to a worker that gave no address of its own, by
	// its node ID. It must be set if any worker of the pool does that.
	PeerDialer func(ctx context.Context, nodeID string) (net.Conn, error)

	mu sync.Mutex
	// ready holds the blobs known to be sound in the local store: checked,
	// downloaded or stored during this run. Each is pinned, and maps to
	// when it was last opened.
	ready map[cid.Cid]time.Time
	// preparing holds the blobs being checked or downloaded right now.
	preparing map[cid.Cid]*preparation
	// held counts, per blob, the tasks that have it open or are opening it.
	held map[cid.Cid]int
}

// cacheOwner holds the pin on every blob this run has used. Blobs left by
// earlier runs and not used since are unpinned, and so are the first to go
// when the cache is trimmed.
const cacheOwner = "cache"

// localStore is the part of a storage.Store that RemoteBlobs uses.
type localStore interface {
	Open(ctx context.Context, c cid.Cid) (storage.Blob, error)
	Put(ctx context.Context, r io.Reader) (cid.Cid, error)
	Verify(ctx context.Context, c cid.Cid) error
	Pin(ctx context.Context, owner string, expires time.Time, cids ...cid.Cid) error
	Unpin(owner string, cids ...cid.Cid) error
	GC(ctx context.Context, now time.Time) (storage.Collected, error)
	Size(ctx context.Context) (uint64, error)
}

// preparation is a blob being made ready, which other tasks wanting the same
// blob wait for instead of starting their own.
type preparation struct {
	done chan struct{}
	err  error
}

// DialBlobs returns blob access backed by local and the coordinator at the
// given address, reached with the given credentials. It does not connect
// until first used. If maxBytes is not zero, the least recently used blobs
// are evicted from local whenever it grows past that size.
func DialBlobs(coordinator string, creds credentials.TransportCredentials, local *storage.Store, maxBytes uint64) (*RemoteBlobs, error) {
	conn, err := grpc.NewClient(coordinator, grpc.WithTransportCredentials(creds))
	if err != nil {
		return nil, err
	}
	b := newRemoteBlobs(local, pb.NewBlobServiceClient(conn), maxBytes)
	b.conn = conn
	return b, nil
}

func newRemoteBlobs(local localStore, remote pb.BlobServiceClient, maxBytes uint64) *RemoteBlobs {
	return &RemoteBlobs{
		local:      local,
		remote:     remote,
		limit:      maxBytes,
		OnFallback: func(cid.Cid) {},
		ready:      make(map[cid.Cid]time.Time),
		preparing:  make(map[cid.Cid]*preparation),
		held:       make(map[cid.Cid]int),
	}
}

// Conn returns the connection to the coordinator, for other calls to it.
func (b *RemoteBlobs) Conn() grpc.ClientConnInterface {
	return b.conn
}

func (b *RemoteBlobs) Close() error {
	return b.conn.Close()
}

// Open returns a blob, downloading it first if the cache lacks a sound copy.
// The blob cannot be evicted until it is closed.
func (b *RemoteBlobs) Open(ctx context.Context, c cid.Cid) (storage.Blob, error) {
	b.hold(c)
	blob, err := b.open(ctx, c)
	if err != nil {
		b.release(c)
		return nil, err
	}
	return &heldBlob{Blob: blob, release: func() { b.release(c) }}, nil
}

func (b *RemoteBlobs) open(ctx context.Context, c cid.Cid) (storage.Blob, error) {
	if err := b.prepare(ctx, c); err != nil {
		return nil, err
	}
	return b.local.Open(ctx, c)
}

func (b *RemoteBlobs) hold(c cid.Cid) {
	b.mu.Lock()
	b.held[c]++
	b.mu.Unlock()
}

func (b *RemoteBlobs) release(c cid.Cid) {
	b.mu.Lock()
	if b.held[c]--; b.held[c] == 0 {
		delete(b.held, c)
	}
	b.mu.Unlock()
}

// heldBlob is an open blob that tells the cache when it is closed.
type heldBlob struct {
	storage.Blob
	once    sync.Once
	release func()
}

func (h *heldBlob) Close() error {
	h.once.Do(h.release)
	return h.Blob.Close()
}

// prepare makes sure the local store holds a sound copy of c, once per run.
func (b *RemoteBlobs) prepare(ctx context.Context, c cid.Cid) error {
	b.mu.Lock()
	if _, ok := b.ready[c]; ok {
		b.ready[c] = time.Now()
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

	p.err = b.obtain(ctx, c)
	b.mu.Lock()
	delete(b.preparing, c)
	if p.err == nil {
		b.ready[c] = time.Now()
	}
	b.mu.Unlock()
	close(p.done)
	if p.err == nil {
		b.trim(ctx)
	}
	return p.err
}

// obtain leaves a sound, pinned copy of c in the local store.
func (b *RemoteBlobs) obtain(ctx context.Context, c cid.Cid) error {
	b.trimming.RLock()
	defer b.trimming.RUnlock()
	// A copy that checks out is used as it is; anything else, from "not
	// there" to "damaged", is put right by downloading it.
	if b.local.Verify(ctx, c) != nil && !b.fetchFromPeers(ctx, c) {
		if err := blobclient.Fetch(ctx, b.remote, c, b.local); err != nil {
			return fmt.Errorf("fetch from coordinator: %w", err)
		}
		b.OnFallback(c)
	}
	return b.local.Pin(ctx, cacheOwner, time.Time{}, c)
}

// trim evicts blobs until the cache is within its limit: first everything
// this run has not used, then what it has, least recently opened first.
// Blobs that tasks have open are never evicted, so a cache whose open blobs
// alone exceed the limit stays over it.
func (b *RemoteBlobs) trim(ctx context.Context) {
	if b.limit == 0 || !b.over(ctx) {
		return
	}
	b.trimming.Lock()
	defer b.trimming.Unlock()
	// Collecting as of the far future ignores the short pin every new blob
	// gets, leaving only this run's own pins to decide what stays.
	collect := func() { b.local.GC(ctx, time.Now().AddDate(100, 0, 0)) }
	collect()
	for b.over(ctx) {
		victim, found := b.leastRecentlyUsed()
		if !found {
			return
		}
		b.local.Unpin(cacheOwner, victim)
		collect()
	}
}

func (b *RemoteBlobs) over(ctx context.Context) bool {
	size, err := b.local.Size(ctx)
	return err == nil && size > b.limit
}

// leastRecentlyUsed removes from the ready set, and returns, the blob no
// task has open that was opened longest ago.
func (b *RemoteBlobs) leastRecentlyUsed() (victim cid.Cid, found bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	var oldest time.Time
	for c, used := range b.ready {
		if b.held[c] > 0 {
			continue
		}
		if !found || used.Before(oldest) {
			victim, oldest, found = c, used, true
		}
	}
	delete(b.ready, victim)
	return victim, found
}

func (b *RemoteBlobs) Put(ctx context.Context, r io.Reader) (cid.Cid, error) {
	c, err := b.store(ctx, r)
	if err != nil {
		return cid.Undef, err
	}
	b.mu.Lock()
	b.ready[c] = time.Now()
	b.mu.Unlock()
	b.trim(ctx)
	return c, nil
}

// store puts a blob in the local store, pinned, and on the coordinator.
func (b *RemoteBlobs) store(ctx context.Context, r io.Reader) (cid.Cid, error) {
	b.trimming.RLock()
	defer b.trimming.RUnlock()
	c, err := b.local.Put(ctx, r)
	if err != nil {
		return cid.Undef, err
	}
	if err := b.local.Pin(ctx, cacheOwner, time.Time{}, c); err != nil {
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

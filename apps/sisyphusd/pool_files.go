package main

import (
	"context"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/ipfs/go-cid"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pb "github.com/sisyphus-network/Sisyphus/packages/protocol/sisyphus/v1"
	"github.com/sisyphus-network/Sisyphus/packages/storage"
)

// poolBlobs is what poolFiles needs of a worker's way to its coordinator's
// store. A *worker.RemoteBlobs is one: it keeps what it handles in the
// worker's cache as well.
type poolBlobs interface {
	Open(ctx context.Context, c cid.Cid) (storage.Blob, error)
	Put(ctx context.Context, r io.Reader) (cid.Cid, error)
}

// poolFileHolds is where a node keeps what its own owners hold of the files
// kept for it in a pool. A nodedb.DB is one.
type poolFileHolds interface {
	PoolFileHolds(coordinatorID, cid string, now time.Time) (map[string]time.Time, error)
	HoldPoolFile(coordinatorID, cid, owner string, expires, now time.Time) error
	ReleasePoolFile(coordinatorID, cid, owner string) error
}

// poolFiles is the local API's file store on a node that has joined
// another's pool as a client of it (#500). A file the user stores goes to
// the pool's coordinator, as this node, which holds it to the client's
// share; what it is called stays on this machine, in the node's own list of
// files.
//
// The coordinator keeps one pin of each file for this node, whoever on the
// node wants it: the user, a chat it is attached to, the draft of one. The
// node keeps who those are, and the pin lasts as long as the longest of
// them, and goes when the last of them lets go. The pin is changed on the
// coordinator before the node notes the change, so that a failure between
// the two keeps a file longer, never shorter.
type poolFiles struct {
	blobs  poolBlobs
	client pb.BlobServiceClient
	holds  poolFileHolds
	// coordinator is the ID of the pool's coordinator, which the holds are
	// kept under.
	coordinator func() string
	// available says whether the pool's files may be reached now: the node
	// holds the client role there, and is connected.
	available func() error

	// mu keeps one change to the holds on a file, and to its pin, from
	// overtaking another.
	mu sync.Mutex
}

func (f *poolFiles) Available() error { return f.available() }

func (f *poolFiles) Open(ctx context.Context, c cid.Cid) (storage.Blob, error) {
	if err := f.available(); err != nil {
		return nil, err
	}
	return f.blobs.Open(ctx, c)
}

func (f *poolFiles) Put(ctx context.Context, r io.Reader) (cid.Cid, error) {
	if err := f.available(); err != nil {
		return cid.Undef, err
	}
	return f.blobs.Put(ctx, r)
}

// Size is what the store holds for this node's own quota, which a joined
// node does not set: the pool's coordinator holds it to the client's share.
func (*poolFiles) Size(context.Context) (uint64, error) { return 0, nil }

// Pin has owner hold each file until expires, or for good if it is the zero
// time. A file the pool no longer has is storage.ErrNotFound.
func (f *poolFiles) Pin(ctx context.Context, owner string, expires time.Time, cids ...cid.Cid) error {
	return f.change(ctx, owner, &expires, cids)
}

// Unpin lets go of owner's hold on each file.
func (f *poolFiles) Unpin(owner string, cids ...cid.Cid) error {
	return f.change(context.Background(), owner, nil, cids)
}

// change has owner hold each file until *expires, or no longer if expires
// is nil, and sets the file's pin on the coordinator to what all the holds
// on it now call for.
func (f *poolFiles) change(ctx context.Context, owner string, expires *time.Time, cids []cid.Cid) error {
	if err := f.available(); err != nil {
		return err
	}
	if _, bounded := ctx.Deadline(); !bounded {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, poolCallTimeout)
		defer cancel()
	}
	coordinator := f.coordinator()
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range cids {
		holds, err := f.holds.PoolFileHolds(coordinator, c.String(), time.Now())
		if err != nil {
			return err
		}
		delete(holds, owner)
		if expires != nil {
			holds[owner] = *expires
		}
		if err := f.keep(ctx, c, holds); err != nil {
			return err
		}
		if expires != nil {
			err = f.holds.HoldPoolFile(coordinator, c.String(), owner, *expires, time.Now())
		} else {
			err = f.holds.ReleasePoolFile(coordinator, c.String(), owner)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// keep sets c's pin on the coordinator to last as long as the longest of
// holds, or takes it away if there are none.
func (f *poolFiles) keep(ctx context.Context, c cid.Cid, holds map[string]time.Time) error {
	if len(holds) == 0 {
		_, err := f.client.Unpin(ctx, &pb.UnpinBlobRequest{Cid: c.String()})
		return err
	}
	var last time.Time
	for _, expires := range holds {
		if expires.IsZero() {
			last = time.Time{}
			break
		}
		if expires.After(last) {
			last = expires
		}
	}
	var ttl uint64
	if !last.IsZero() {
		ttl = uint64(max(time.Until(last), time.Second) / time.Second)
	}
	_, err := f.client.Pin(ctx, &pb.PinBlobRequest{Cid: c.String(), TtlSeconds: ttl})
	if status.Code(err) == codes.NotFound {
		return fmt.Errorf("%w: %v", storage.ErrNotFound, err)
	}
	return err
}

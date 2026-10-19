package main

import (
	"context"
	"errors"
	"io"
	"time"

	"github.com/ipfs/go-cid"

	"github.com/sisyphus-network/Sisyphus/apps/sisyphusd/api"
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

// poolFiles is the local API's file store on a node that has joined
// another's pool as a client of it (#500). A file the user stores goes to
// the pool's coordinator, as this node, which holds it to the client's
// share; what it is called stays on this machine, in the node's own list of
// files. Only the user's own files are kept this way: chat attachments,
// which other owners keep for a while, are not yet.
type poolFiles struct {
	blobs  poolBlobs
	client pb.BlobServiceClient
	// available says whether the pool's files may be reached now: the node
	// holds the client role there, and is connected.
	available func() error
}

func (f poolFiles) Available() error { return f.available() }

func (f poolFiles) Open(ctx context.Context, c cid.Cid) (storage.Blob, error) {
	if err := f.available(); err != nil {
		return nil, err
	}
	return f.blobs.Open(ctx, c)
}

func (f poolFiles) Put(ctx context.Context, r io.Reader) (cid.Cid, error) {
	if err := f.available(); err != nil {
		return cid.Undef, err
	}
	return f.blobs.Put(ctx, r)
}

// Size is what the store holds for this node's own quota, which a joined
// node does not set: the pool's coordinator holds it to the client's share.
func (poolFiles) Size(context.Context) (uint64, error) { return 0, nil }

// Pin keeps the user's files on the coordinator, for as long as asked.
func (f poolFiles) Pin(ctx context.Context, owner string, expires time.Time, cids ...cid.Cid) error {
	if owner != api.UserOwner {
		return errOnlyUserFiles
	}
	var ttl uint64
	if !expires.IsZero() {
		ttl = uint64(max(time.Until(expires), time.Second) / time.Second)
	}
	for _, c := range cids {
		if _, err := f.client.Pin(ctx, &pb.PinBlobRequest{Cid: c.String(), TtlSeconds: ttl}); err != nil {
			return err
		}
	}
	return nil
}

// Unpin releases the user's files on the coordinator. Nothing else is kept
// there for this node, so releasing for any other owner changes nothing.
func (f poolFiles) Unpin(owner string, cids ...cid.Cid) error {
	if owner != api.UserOwner {
		return nil
	}
	var errs []error
	for _, c := range cids {
		_, err := f.client.Unpin(context.Background(), &pb.UnpinBlobRequest{Cid: c.String()})
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

var errOnlyUserFiles = errors.New("a node that has joined another's pool keeps only the user's own files there")

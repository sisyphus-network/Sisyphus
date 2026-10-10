package runtime

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/ipfs/go-cid"

	"github.com/sisyphus-network/Sisyphus/packages/sealed"
	"github.com/sisyphus-network/Sisyphus/packages/storage"
)

// SealingLabel is the label of a worker that takes the keys of a private
// job as they are now given: one for the job and one for each sealed input,
// and not a sealing key whole. A private job's tasks go only to such
// workers, since one from before would store what it was to seal unsealed.
const SealingLabel = "sealing:2"

// Sealed returns a Blobs that keeps what a job stores from everyone without
// the job's key. Whatever is stored through it is sealed first, with the
// job's own key, so the store and anyone else who can fetch from it hold
// only ciphertext. Whatever is opened through it is unsealed if it was
// sealed and keys has the key to it, and passed through as it is if it was
// not sealed, so a job with keys can still read inputs that are not secret.
//
// The CIDs it deals in are those of the sealed blobs. A workload needs to
// know none of this.
func Sealed(blobs Blobs, keys sealed.Keys, own sealed.Grant) Blobs {
	return sealedBlobs{blobs: blobs, keys: keys, own: own}
}

type sealedBlobs struct {
	blobs Blobs
	keys  sealed.Keys
	own   sealed.Grant
}

func (s sealedBlobs) Put(ctx context.Context, r io.Reader) (cid.Cid, error) {
	return s.blobs.Put(ctx, sealed.EncryptWith(s.own, r))
}

func (s sealedBlobs) Open(ctx context.Context, c cid.Cid) (storage.Blob, error) {
	blob, err := s.blobs.Open(ctx, c)
	if err != nil {
		return nil, err
	}
	opened, err := sealed.Open(s.keys, blob, blob.Size())
	if errors.Is(err, sealed.ErrNotSealed) {
		// Not secret: hand it over from the beginning, as stored.
		_, err = blob.Seek(0, io.SeekStart)
		if err == nil {
			return blob, nil
		}
	}
	if err != nil {
		blob.Close()
		return nil, err
	}
	return opened, nil
}

// Open returns a Blobs for a job that has no keys. It stores and opens
// blobs as they are, and refuses to open one that is sealed: a job that is
// not private has no key to it, and would otherwise compute on the sealed
// bytes as though they were the data, and return an answer about nothing.
func Open(blobs Blobs) Blobs {
	return openBlobs{blobs}
}

type openBlobs struct{ Blobs }

func (o openBlobs) Open(ctx context.Context, c cid.Cid) (storage.Blob, error) {
	blob, err := o.Blobs.Open(ctx, c)
	if err != nil {
		return nil, err
	}
	head := make([]byte, sealed.HeaderSize)
	n, _ := io.ReadFull(blob, head) // a short blob has a short head
	if sealed.IsSealed(head[:n]) {
		blob.Close()
		return nil, fmt.Errorf("blob %s is sealed, and this job is not private, so it has no key to it: submit the job as private to have it read private data", c)
	}
	if _, err := blob.Seek(0, io.SeekStart); err != nil {
		blob.Close()
		return nil, err
	}
	return blob, nil
}

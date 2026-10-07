package runtime

import (
	"context"
	"errors"
	"io"

	"github.com/ipfs/go-cid"

	"github.com/excho0/Sisyphus/packages/sealed"
	"github.com/excho0/Sisyphus/packages/storage"
)

// Sealed returns a Blobs that keeps what a job stores from everyone without
// the job's key. Whatever is stored through it is sealed first, so the store
// and anyone else who can fetch from it hold only ciphertext. Whatever is
// opened through it is unsealed if it was sealed, and passed through as it
// is if not, so a job with a key can still read inputs that are not secret.
//
// The CIDs it deals in are those of the sealed blobs. A workload needs to
// know none of this.
func Sealed(blobs Blobs, key sealed.Key) Blobs {
	return sealedBlobs{blobs: blobs, key: key}
}

type sealedBlobs struct {
	blobs Blobs
	key   sealed.Key
}

func (s sealedBlobs) Put(ctx context.Context, r io.Reader) (cid.Cid, error) {
	return s.blobs.Put(ctx, sealed.Encrypt(s.key, r))
}

func (s sealedBlobs) Open(ctx context.Context, c cid.Cid) (storage.Blob, error) {
	blob, err := s.blobs.Open(ctx, c)
	if err != nil {
		return nil, err
	}
	opened, err := sealed.Open(s.key, blob, blob.Size())
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

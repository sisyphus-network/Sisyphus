package storage

import (
	"context"
	"io"

	"github.com/ipfs/go-cid"
)

// Hasher computes the CID of the bytes written to it without storing them.
// Use it to check data against the CID it was requested by while streaming it
// somewhere else.
type Hasher struct {
	pipe *io.PipeWriter
	done chan struct{}
	cid  cid.Cid
	err  error
}

func NewHasher(ctx context.Context) *Hasher {
	r, w := io.Pipe()
	h := &Hasher{pipe: w, done: make(chan struct{})}
	go func() {
		defer close(h.done)
		h.cid, h.err = CID(ctx, r)
		// Unblocks a writer if hashing stopped early.
		r.CloseWithError(h.err)
	}()
	return h
}

func (h *Hasher) Write(p []byte) (int, error) {
	return h.pipe.Write(p)
}

// Sum returns the CID of everything written. The Hasher cannot be used again.
func (h *Hasher) Sum() (cid.Cid, error) {
	h.pipe.Close()
	<-h.done
	return h.cid, h.err
}

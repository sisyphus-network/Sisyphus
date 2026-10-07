package runtime

import (
	"context"
	"io"
	"sort"
	"sync"

	"github.com/ipfs/go-cid"

	"github.com/excho0/Sisyphus/packages/storage"
)

// Recorder is a Blobs that notes which blobs were opened and which were
// stored through it, so the coordinator can tell what a job consumed and
// produced without workloads having to say.
type Recorder struct {
	blobs Blobs

	mu      sync.Mutex
	opened  map[cid.Cid]struct{}
	written map[cid.Cid]struct{}
}

// Record wraps blobs in a Recorder.
func Record(blobs Blobs) *Recorder {
	return &Recorder{blobs: blobs, opened: make(map[cid.Cid]struct{}), written: make(map[cid.Cid]struct{})}
}

func (r *Recorder) Open(ctx context.Context, c cid.Cid) (storage.Blob, error) {
	blob, err := r.blobs.Open(ctx, c)
	if err == nil {
		r.mu.Lock()
		r.opened[c] = struct{}{}
		r.mu.Unlock()
	}
	return blob, err
}

func (r *Recorder) Put(ctx context.Context, src io.Reader) (cid.Cid, error) {
	c, err := r.blobs.Put(ctx, src)
	if err == nil {
		r.mu.Lock()
		r.written[c] = struct{}{}
		r.mu.Unlock()
	}
	return c, err
}

// Read returns the CIDs of blobs that were opened, in order. A blob stored
// through the Recorder and then opened is not included: it was produced
// here, not consumed.
func (r *Recorder) Read() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var read []string
	for c := range r.opened {
		if _, own := r.written[c]; !own {
			read = append(read, c.String())
		}
	}
	sort.Strings(read)
	return read
}

// Written returns the CIDs of blobs that were stored, in order.
func (r *Recorder) Written() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	written := make([]string, 0, len(r.written))
	for c := range r.written {
		written = append(written, c.String())
	}
	sort.Strings(written)
	return written
}

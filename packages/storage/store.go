// Package storage is Sisyphus's content-addressed artifact store. Job inputs,
// outputs and other large data live here and are referred to everywhere else
// by CID.
//
// Blobs are stored the way IPFS stores files: split into blocks, linked into
// a UnixFS DAG, and named by the CID of the root. The CID of a blob is
// therefore the one `ipfs add --cid-version=1` gives the same bytes, so a
// blob keeps its name when it moves between a node's disk, an S3 bucket and
// the IPFS network.
package storage

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/gofrs/flock"
	"github.com/ipfs/boxo/blockservice"
	"github.com/ipfs/boxo/blockstore"
	chunker "github.com/ipfs/boxo/chunker"
	"github.com/ipfs/boxo/exchange/offline"
	"github.com/ipfs/boxo/ipld/merkledag"
	"github.com/ipfs/boxo/ipld/unixfs/importer/balanced"
	"github.com/ipfs/boxo/ipld/unixfs/importer/helpers"
	unixfsio "github.com/ipfs/boxo/ipld/unixfs/io"
	"github.com/ipfs/go-cid"
	"github.com/ipfs/go-datastore"
	dssync "github.com/ipfs/go-datastore/sync"
	flatfs "github.com/ipfs/go-ds-flatfs"
	ipld "github.com/ipfs/go-ipld-format"
	"github.com/multiformats/go-multihash"
)

// The import parameters below decide what CID a given blob gets. Changing any
// of them renames every blob, so they are fixed here rather than taken from
// library defaults that may move.
const (
	chunkSize    = 256 << 10 // fixed-size chunks
	linksPerNode = 174       // fan-out of the balanced DAG
)

var cidBuilder = cid.V1Builder{Codec: cid.DagProtobuf, MhType: multihash.SHA2_256}

// ErrNotFound reports that a store does not hold the requested blob.
var ErrNotFound = errors.New("blob not found")

// Store holds blobs addressed by CID. It is safe for concurrent use.
type Store struct {
	blocks blockstore.Blockstore
	dag    ipld.DAGService
	closer io.Closer
	lock   *flock.Flock // nil for stores that are not on disk
}

// OpenLocal opens, creating if needed, a store kept in dir on this machine.
// Only one process may have a directory open at a time.
func OpenLocal(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	lock := flock.New(filepath.Join(dir, "LOCK"))
	locked, err := lock.TryLock()
	if err != nil {
		return nil, fmt.Errorf("open blob store %s: %w", dir, err)
	}
	if !locked {
		return nil, fmt.Errorf("open blob store %s: in use by another process", dir)
	}
	// The same on-disk layout Kubo uses for its block store.
	ds, err := flatfs.CreateOrOpen(filepath.Join(dir, "blocks"), flatfs.NextToLast(2), true)
	if err != nil {
		lock.Unlock()
		return nil, fmt.Errorf("open blob store %s: %w", dir, err)
	}
	return newStore(ds, lock), nil
}

// NewMemory returns a store that keeps blobs in memory.
func NewMemory() *Store {
	return newStore(dssync.MutexWrap(datastore.NewMapDatastore()), nil)
}

func newStore(ds datastore.Batching, lock *flock.Flock) *Store {
	blocks := blockstore.NewBlockstoreNoPrefix(ds)
	return &Store{
		blocks: blocks,
		dag:    merkledag.NewDAGService(blockservice.New(blocks, offline.Exchange(blocks))),
		closer: ds,
		lock:   lock,
	}
}

func (s *Store) Close() error {
	err := s.closer.Close()
	if s.lock != nil {
		err = errors.Join(err, s.lock.Unlock())
	}
	return err
}

// Put stores everything read from r and returns the blob's CID. Storing the
// same bytes twice gives the same CID and uses no extra space.
func (s *Store) Put(ctx context.Context, r io.Reader) (cid.Cid, error) {
	params := helpers.DagBuilderParams{
		Maxlinks:   linksPerNode,
		RawLeaves:  true,
		CidBuilder: cidBuilder,
		Dagserv:    contextDAG{ctx: ctx, DAGService: s.dag},
	}
	builder, err := params.New(chunker.NewSizeSplitter(r, chunkSize))
	if err != nil {
		return cid.Undef, err
	}
	root, err := balanced.Layout(builder)
	if err != nil {
		return cid.Undef, err
	}
	return root.Cid(), nil
}

// Blob is an open blob. Close it when done.
type Blob interface {
	io.ReadSeekCloser
	Size() uint64
}

// Open returns the blob with the given CID, or ErrNotFound.
func (s *Store) Open(ctx context.Context, c cid.Cid) (Blob, error) {
	root, err := s.dag.Get(ctx, c)
	if err != nil {
		if ipld.IsNotFound(err) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	reader, err := unixfsio.NewDagReader(ctx, root, s.dag)
	if err != nil {
		return nil, fmt.Errorf("%s is not a blob: %w", c, err)
	}
	return reader, nil
}

// Has reports whether the store holds the blob with the given CID.
func (s *Store) Has(ctx context.Context, c cid.Cid) (bool, error) {
	// Put writes a blob's root block last, so a root that is present means
	// the whole blob is.
	return s.blocks.Has(ctx, c)
}

// CID returns the CID that Put would give the bytes read from r, without
// storing them.
func CID(ctx context.Context, r io.Reader) (cid.Cid, error) {
	return newStore(dssync.MutexWrap(datastore.NewNullDatastore()), nil).Put(ctx, r)
}

// contextDAG makes the importer, which writes blocks with a background
// context, honour the caller's context instead. It checks for cancellation
// itself because not every backend does.
type contextDAG struct {
	ctx context.Context
	ipld.DAGService
}

func (d contextDAG) Add(_ context.Context, node ipld.Node) error {
	if err := d.ctx.Err(); err != nil {
		return err
	}
	return d.DAGService.Add(d.ctx, node)
}

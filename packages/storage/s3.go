package storage

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/gofrs/flock"
	blocks "github.com/ipfs/go-block-format"
	"github.com/ipfs/go-cid"
	ipld "github.com/ipfs/go-ipld-format"
)

// Bucket is the part of an S3-style object store that a store keeps its
// blocks in: one object for each block, named by the block's CID. An
// s3.Client is one.
type Bucket interface {
	Put(ctx context.Context, name string, data []byte) error
	// Get returns an object, and Size how long it is; both return
	// ErrNoObject for one that is not there.
	Get(ctx context.Context, name string) ([]byte, error)
	Size(ctx context.Context, name string) (int, error)
	Remove(ctx context.Context, name string) error
	// List calls visit with the name and size of every object, until it
	// returns false.
	List(ctx context.Context, visit func(name string, size int64) bool) error
}

// ErrNoObject is what a Bucket returns for an object it does not hold.
var ErrNoObject = errors.New("no such object")

// OpenBucket opens a durable store whose blocks are kept as objects in a
// bucket, for a pool that runs or rents object storage: Ceph, MinIO,
// SeaweedFS, Storj, Wasabi, AWS. A blob has the same CID wherever its bytes
// are, so nothing else need know. Pins are this store's own, kept in dir as
// OpenLocal keeps them.
//
// A block is an object and reading one is a request, so a store in a bucket
// far away is slower than a disk: a blob of a gigabyte is four thousand
// blocks. Nothing is kept on this machine but the pins.
func OpenBucket(bucket Bucket, dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("open blob store %s: %w", dir, err)
	}
	lock := flock.New(filepath.Join(dir, "LOCK"))
	locked, err := lock.TryLock()
	if err != nil {
		return nil, fmt.Errorf("open blob store %s: %w", dir, err)
	}
	if !locked {
		return nil, fmt.Errorf("open blob store %s: in use by another process", dir)
	}
	held := bucketBlocks{bucket}
	store := newStoreOver(held, false)
	store.size = held.size
	store.close = func() error { return nil } // the bucket is its owner's to close
	store.lock = lock
	store.pinFile = filepath.Join(dir, "pins.json")
	if store.pins, err = loadPins(store.pinFile); err != nil {
		store.Close()
		return nil, fmt.Errorf("open blob store %s: %w", dir, err)
	}
	return store, nil
}

// bucketBlocks keeps blocks as objects in a bucket.
type bucketBlocks struct {
	bucket Bucket
}

// blockPrefix begins the name of every object that is a block, so that a
// bucket may hold other things beside them and they are left alone.
const blockPrefix = "blocks/"

func objectName(c cid.Cid) string { return blockPrefix + c.String() }

func (b bucketBlocks) Put(ctx context.Context, block blocks.Block) error {
	return b.bucket.Put(ctx, objectName(block.Cid()), block.RawData())
}

func (b bucketBlocks) PutMany(ctx context.Context, many []blocks.Block) error {
	for _, block := range many {
		if err := b.Put(ctx, block); err != nil {
			return err
		}
	}
	return nil
}

func (b bucketBlocks) Get(ctx context.Context, c cid.Cid) (blocks.Block, error) {
	data, err := b.bucket.Get(ctx, objectName(c))
	if err != nil {
		return nil, b.translate(err, c)
	}
	return blocks.NewBlockWithCid(data, c)
}

func (b bucketBlocks) GetSize(ctx context.Context, c cid.Cid) (int, error) {
	size, err := b.bucket.Size(ctx, objectName(c))
	return size, b.translate(err, c)
}

func (b bucketBlocks) Has(ctx context.Context, c cid.Cid) (bool, error) {
	_, err := b.bucket.Size(ctx, objectName(c))
	if errors.Is(err, ErrNoObject) {
		return false, nil
	}
	return err == nil, err
}

func (b bucketBlocks) DeleteBlock(ctx context.Context, c cid.Cid) error {
	return b.bucket.Remove(ctx, objectName(c))
}

// AllKeysChan lists the blocks in the bucket. The listing is read to its
// end before anything is handed on, so that a failure part-way is reported
// and does not pass for a short list, which a garbage collection would act
// on.
func (b bucketBlocks) AllKeysChan(ctx context.Context) (<-chan cid.Cid, error) {
	var all []cid.Cid
	err := b.bucket.List(ctx, func(name string, _ int64) bool {
		if id, is := strings.CutPrefix(name, blockPrefix); is {
			if c, err := cid.Decode(id); err == nil {
				all = append(all, c)
			}
		}
		return true
	})
	if err != nil {
		return nil, err
	}
	keys := make(chan cid.Cid, len(all))
	for _, c := range all {
		keys <- c
	}
	close(keys)
	return keys, nil
}

// size adds up the blocks in the bucket.
func (b bucketBlocks) size(ctx context.Context) (uint64, error) {
	var total uint64
	err := b.bucket.List(ctx, func(name string, size int64) bool {
		if strings.HasPrefix(name, blockPrefix) {
			total += uint64(size)
		}
		return true
	})
	return total, err
}

// translate turns the bucket's "not there" into the one the rest of the
// store recognises.
func (bucketBlocks) translate(err error, c cid.Cid) error {
	if errors.Is(err, ErrNoObject) {
		return ipld.ErrNotFound{Cid: c}
	}
	return err
}

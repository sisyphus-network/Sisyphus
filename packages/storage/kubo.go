package storage

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/gofrs/flock"
	blocks "github.com/ipfs/go-block-format"
	"github.com/ipfs/go-cid"
	ipld "github.com/ipfs/go-ipld-format"

	"github.com/excho0/Sisyphus/packages/kubo"
)

// KuboAPI is the part of a Kubo daemon that a store keeps its blocks in. A
// kubo.Client is one.
type KuboAPI interface {
	BlockPut(ctx context.Context, codec string, data []byte) (string, error)
	BlockGet(ctx context.Context, id string) ([]byte, error)
	BlockSize(ctx context.Context, id string) (int, error)
	BlockRemove(ctx context.Context, id string) error
	LocalBlocks(ctx context.Context, visit func(id string) bool) error
	Pinned(ctx context.Context, visit func(id string)) error
	RepoSize(ctx context.Context) (uint64, error)
}

// OpenKubo opens a durable store whose blocks are kept by a Kubo daemon, so
// that anything stored is at once an ordinary IPFS object on that daemon.
// Pins are this store's own, kept in dir as OpenLocal keeps them. Kubo's own
// pins are respected: a collection never deletes a block Kubo has pinned, so
// `ipfs pin add` keeps a blob whatever this store thinks of it. Kubo's
// garbage collector knows nothing of this store's pins, though, and running
// `ipfs repo gc` by hand would delete what it holds.
func OpenKubo(api KuboAPI, dir string) (*Store, error) {
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
	store := newStoreOver(kuboBlocks{api}, false)
	store.size = api.RepoSize
	store.close = func() error { return nil } // the daemon is its starter's to stop
	store.lock = lock
	store.pinFile = filepath.Join(dir, "pins.json")
	if store.pins, err = loadPins(store.pinFile); err != nil {
		store.Close()
		return nil, fmt.Errorf("open blob store %s: %w", dir, err)
	}
	return store, nil
}

// kuboBlocks keeps blocks in a Kubo daemon.
type kuboBlocks struct {
	api KuboAPI
}

// codecNames gives Kubo's name for each kind of block a store writes.
var codecNames = map[uint64]string{cid.Raw: "raw", cid.DagProtobuf: "dag-pb"}

func (k kuboBlocks) Put(ctx context.Context, block blocks.Block) error {
	codec, ok := codecNames[block.Cid().Type()]
	if !ok {
		return fmt.Errorf("cannot store block %s in Kubo: its format is neither raw nor dag-pb", block.Cid())
	}
	stored, err := k.api.BlockPut(ctx, codec, block.RawData())
	if err != nil {
		return err
	}
	// Kubo names the block itself, from its bytes. If that is not the name
	// the block came with, something is wrong with one of them.
	if stored != block.Cid().String() {
		return fmt.Errorf("Kubo stored block %s as %s", block.Cid(), stored)
	}
	return nil
}

func (k kuboBlocks) PutMany(ctx context.Context, many []blocks.Block) error {
	for _, block := range many {
		if err := k.Put(ctx, block); err != nil {
			return err
		}
	}
	return nil
}

func (k kuboBlocks) Get(ctx context.Context, c cid.Cid) (blocks.Block, error) {
	data, err := k.api.BlockGet(ctx, c.String())
	if err != nil {
		return nil, k.translate(err, c)
	}
	return blocks.NewBlockWithCid(data, c)
}

func (k kuboBlocks) GetSize(ctx context.Context, c cid.Cid) (int, error) {
	size, err := k.api.BlockSize(ctx, c.String())
	return size, k.translate(err, c)
}

func (k kuboBlocks) Has(ctx context.Context, c cid.Cid) (bool, error) {
	_, err := k.api.BlockSize(ctx, c.String())
	if errors.Is(err, kubo.ErrNotFound) {
		return false, nil
	}
	return err == nil, err
}

func (k kuboBlocks) DeleteBlock(ctx context.Context, c cid.Cid) error {
	return k.api.BlockRemove(ctx, c.String())
}

// AllKeysChan lists the blocks that are this store's to keep or delete:
// every block the daemon holds except those Kubo itself has pinned, which
// are their owner's business.
func (k kuboBlocks) AllKeysChan(ctx context.Context) (<-chan cid.Cid, error) {
	pinned := make(map[string]struct{})
	err := k.api.Pinned(ctx, func(id string) {
		if c, err := cid.Decode(id); err == nil {
			pinned[string(c.Hash())] = struct{}{}
		}
	})
	if err != nil {
		return nil, err
	}
	// Listing is one long reply. Read it all before handing anything on, so
	// that a failure part-way is reported rather than passing for a short
	// list, which a garbage collection would act on.
	var all []cid.Cid
	err = k.api.LocalBlocks(ctx, func(id string) bool {
		if c, err := cid.Decode(id); err == nil {
			if _, kept := pinned[string(c.Hash())]; !kept {
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

// translate turns Kubo's "not found" into the one the rest of the store
// recognises.
func (kuboBlocks) translate(err error, c cid.Cid) error {
	if errors.Is(err, kubo.ErrNotFound) {
		return ipld.ErrNotFound{Cid: c}
	}
	return err
}

package storage

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"

	blocks "github.com/ipfs/go-block-format"
)

// memoryBucket is a bucket kept in memory, which can be made to fail.
type memoryBucket struct {
	objects map[string][]byte
	fail    error
}

var errBucket = errors.New("the object store is down")

func newBucket() *memoryBucket { return &memoryBucket{objects: map[string][]byte{}} }

func (b *memoryBucket) Put(_ context.Context, name string, data []byte) error {
	if b.fail != nil {
		return b.fail
	}
	b.objects[name] = bytes.Clone(data)
	return nil
}

func (b *memoryBucket) Get(_ context.Context, name string) ([]byte, error) {
	data, held := b.objects[name]
	if b.fail != nil {
		return nil, b.fail
	}
	if !held {
		return nil, ErrNoObject
	}
	return data, nil
}

func (b *memoryBucket) Size(ctx context.Context, name string) (int, error) {
	data, err := b.Get(ctx, name)
	return len(data), err
}

func (b *memoryBucket) Remove(_ context.Context, name string) error {
	delete(b.objects, name)
	return b.fail
}

func (b *memoryBucket) List(_ context.Context, visit func(string, int64) bool) error {
	if b.fail != nil {
		return b.fail
	}
	names := make([]string, 0, len(b.objects))
	for name := range b.objects {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if !visit(name, int64(len(b.objects[name]))) {
			break
		}
	}
	return nil
}

func openBucket(t *testing.T, bucket Bucket, dir string) *Store {
	t.Helper()
	s, err := OpenBucket(bucket, dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestAStoreInABucketKeepsEachBlockAsAnObject(t *testing.T) {
	bucket := newBucket()
	// The bucket may hold other things, which are not the store's.
	bucket.objects["notes.txt"] = []byte("someone else's")
	bucket.objects["blocks/not-a-cid"] = []byte("nor is this a block")
	dir := t.TempDir()
	s := openBucket(t, bucket, dir)

	a := put(t, s, blobA)
	blob, err := s.Open(ctx, a)
	if err != nil {
		t.Fatal(err)
	}
	read, _ := io.ReadAll(blob)
	blob.Close()
	if !bytes.Equal(read, blobA) || blob.Size() != uint64(len(blobA)) {
		t.Fatalf("read back %d bytes of %d", len(read), len(blobA))
	}
	// 700,000 bytes is three blocks and a node that lists them.
	if len(bucket.objects) != 6 {
		t.Errorf("the bucket holds %d objects", len(bucket.objects))
	}
	if held, err := s.Has(ctx, a); err != nil || !held {
		t.Errorf("Has = %v, %v", held, err)
	}
	if err := s.Verify(ctx, a); err != nil {
		t.Errorf("Verify: %v", err)
	}
	// Its size is that of its blocks, not of what else is in the bucket.
	if size, err := s.Size(ctx); err != nil || size < uint64(len(blobA)) || size > uint64(len(blobA))+1000 {
		t.Errorf("Size = %d, %v", size, err)
	}

	// Pins and collection work as on any store, and are kept beside it:
	// what is pinned stays, what is not goes, and what is not the store's
	// is left alone.
	b := put(t, s, blobB)
	if err := s.Pin(ctx, "someone", time.Time{}, a); err != nil {
		t.Fatal(err)
	}
	collected, err := s.GC(ctx, afterGrace())
	if err != nil || collected.Blocks == 0 {
		t.Fatalf("GC = %+v, %v", collected, err)
	}
	if held, _ := s.Has(ctx, b); held {
		t.Error("an unpinned blob outlived the collection")
	}
	if _, err := s.Open(ctx, b); !errors.Is(err, ErrNotFound) {
		t.Errorf("opening what was collected: %v", err)
	}
	if held, _ := s.Has(ctx, a); !held || string(bucket.objects["notes.txt"]) != "someone else's" {
		t.Error("the collection took what it should have left")
	}
	s.Close()
	again := openBucket(t, bucket, dir)
	if pins := again.Pins(); len(pins) != 1 || pins[0].CID != a {
		t.Errorf("pins after reopening: %v", pins)
	}
}

func TestAStoreInABucketReportsABucketThatFails(t *testing.T) {
	bucket := newBucket()
	s := openBucket(t, bucket, t.TempDir())
	c := put(t, s, blobA)
	bucket.fail = errBucket
	if _, err := s.Put(ctx, bytes.NewReader([]byte("more"))); !errors.Is(err, errBucket) {
		t.Errorf("Put: %v", err)
	}
	if _, err := s.Open(ctx, c); !errors.Is(err, errBucket) {
		t.Errorf("Open: %v", err)
	}
	if _, err := s.Has(ctx, c); !errors.Is(err, errBucket) {
		t.Errorf("Has: %v", err)
	}
	if _, err := s.GC(ctx, afterGrace()); !errors.Is(err, errBucket) {
		t.Errorf("GC: %v", err)
	}
	if _, err := s.Size(ctx); !errors.Is(err, errBucket) {
		t.Errorf("Size: %v", err)
	}
	held := bucketBlocks{bucket}
	if _, err := held.GetSize(ctx, c); !errors.Is(err, errBucket) {
		t.Errorf("GetSize: %v", err)
	}
	bucket.fail = nil
	if size, err := held.GetSize(ctx, c); err != nil || size == 0 {
		t.Errorf("GetSize = %d, %v", size, err)
	}
	// Several blocks at once are stored one after another, and stop at
	// the first that cannot be.
	block, _ := held.Get(ctx, c)
	if err := held.PutMany(ctx, []blocks.Block{block, block}); err != nil {
		t.Errorf("PutMany: %v", err)
	}
	bucket.fail = errBucket
	if err := held.PutMany(ctx, []blocks.Block{block}); !errors.Is(err, errBucket) {
		t.Errorf("PutMany into a bucket that fails: %v", err)
	}
}

func TestOpeningAStoreInABucketFails(t *testing.T) {
	bucket := newBucket()
	file := filepath.Join(t.TempDir(), "a-file")
	os.WriteFile(file, nil, 0o600)
	if _, err := OpenBucket(bucket, filepath.Join(file, "store")); err == nil {
		t.Error("opened a store beneath a regular file")
	}
	lockIsDir := t.TempDir()
	os.Mkdir(filepath.Join(lockIsDir, "LOCK"), 0o700)
	if _, err := OpenBucket(bucket, lockIsDir); err == nil {
		t.Error("opened a store whose lock file cannot be created")
	}
	dir := t.TempDir()
	first := openBucket(t, bucket, dir)
	if second, err := OpenBucket(bucket, dir); err == nil {
		second.Close()
		t.Error("opened a store directory that is already open")
	}
	first.Close()
	badPins := t.TempDir()
	os.WriteFile(filepath.Join(badPins, "pins.json"), []byte("not json"), 0o600)
	if _, err := OpenBucket(bucket, badPins); err == nil {
		t.Error("opened a store whose pins cannot be read")
	}
	os.Remove(filepath.Join(badPins, "pins.json"))
	openBucket(t, bucket, badPins)
}

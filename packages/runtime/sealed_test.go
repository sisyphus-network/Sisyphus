package runtime

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/ipfs/go-cid"

	"github.com/sisyphus-network/Sisyphus/packages/sealed"
	"github.com/sisyphus-network/Sisyphus/packages/storage"
)

// sealedAs is a job's view of a store as its coordinator has it: holding
// the sealing key itself, and sealing what it stores with the job's own.
func sealedAs(blobs Blobs, key sealed.Key) Blobs {
	return Sealed(blobs, key, key.ForJob("the-job"))
}

// readFrom reads a blob whole through a job's view of a store.
func readFrom(t *testing.T, blobs Blobs, c cid.Cid) string {
	t.Helper()
	blob, err := blobs.Open(ctx, c)
	if err != nil {
		t.Fatal(err)
	}
	defer blob.Close()
	data, err := io.ReadAll(blob)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// A worker is given the keys to its job's data and not the key they were
// derived from: it opens what the job stored and the inputs it was given
// keys to, and nothing else the same sealing key seals.
func TestAJobsKeysOpenItsOwnDataAndNoOtherJobs(t *testing.T) {
	store := storage.NewMemory()
	key := sealed.NewKey()
	ours, theirs := key.ForJob("ours"), key.ForJob("theirs")
	other, err := Sealed(store, key, theirs).Put(ctx, strings.NewReader("another job's data"))
	if err != nil {
		t.Fatal(err)
	}
	worker := Sealed(store, sealed.NewRing(ours), ours)
	stored, err := worker.Put(ctx, strings.NewReader("this job's data"))
	if err != nil {
		t.Fatal(err)
	}
	if got := readFrom(t, worker, stored); got != "this job's data" {
		t.Errorf("a worker read back %q of what its job stored", got)
	}
	if _, err := worker.Open(ctx, other); !errors.Is(err, sealed.ErrNoKey) {
		t.Errorf("a worker opening another job's data: %v, want ErrNoKey", err)
	}
	// Given that job's key as well, as for an input, it reads it; and the
	// holder of the sealing key reads both.
	if got := readFrom(t, Sealed(store, sealed.NewRing(ours, theirs), ours), other); got != "another job's data" {
		t.Errorf("with the key to it, a worker read %q", got)
	}
	if got := readFrom(t, sealedAs(store, key), stored); got != "this job's data" {
		t.Errorf("the holder of the sealing key read %q", got)
	}
}

func TestSealedBlobsKeepPlaintextOutOfTheStore(t *testing.T) {
	store := storage.NewMemory()
	key := sealed.NewKey()
	private := sealedAs(store, key)
	secret := bytes.Repeat([]byte("the boulder's whereabouts are confidential. "), 5000)

	c, err := private.Put(ctx, bytes.NewReader(secret))
	if err != nil {
		t.Fatal(err)
	}
	// What the store holds under that CID is not the secret.
	stored := readBlob(t, store, c.String())
	if strings.Contains(stored, "confidential") {
		t.Error("the store holds the plaintext")
	}
	if !sealed.IsSealed([]byte(stored)) {
		t.Error("what the store holds is not a sealed blob")
	}

	// With the key it reads back whole, and any part of it can be read.
	blob, err := private.Open(ctx, c)
	if err != nil {
		t.Fatal(err)
	}
	defer blob.Close()
	if blob.Size() != uint64(len(secret)) {
		t.Errorf("size %d, want the plaintext's %d", blob.Size(), len(secret))
	}
	if _, err := blob.Seek(100_000, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	part := make([]byte, 50)
	if _, err := io.ReadFull(blob, part); err != nil || !bytes.Equal(part, secret[100_000:100_050]) {
		t.Errorf("a read from the middle: %q, %v", part, err)
	}

	// With another key it does not.
	other, err := sealedAs(store, sealed.NewKey()).Open(ctx, c)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	if _, err := io.ReadAll(other); !errors.Is(err, sealed.ErrCorrupt) {
		t.Errorf("reading with the wrong key: %v, want ErrCorrupt", err)
	}
}

func TestSealedBlobsPassUnsealedOnesThrough(t *testing.T) {
	store := storage.NewMemory()
	public, err := store.Put(ctx, strings.NewReader("an input that is no secret"))
	if err != nil {
		t.Fatal(err)
	}
	blob, err := sealedAs(store, sealed.NewKey()).Open(ctx, public)
	if err != nil {
		t.Fatal(err)
	}
	defer blob.Close()
	if got, _ := io.ReadAll(blob); string(got) != "an input that is no secret" {
		t.Errorf("an unsealed blob read through a sealing handle as %q", got)
	}
}

func TestSealedBlobsReportStorageFailures(t *testing.T) {
	store := storage.NewMemory()
	key := sealed.NewKey()
	public, err := store.Put(ctx, strings.NewReader("an input that is no secret"))
	if err != nil {
		t.Fatal(err)
	}
	absent, err := storage.CID(ctx, strings.NewReader("never stored"))
	if err != nil {
		t.Fatal(err)
	}

	if _, err := sealedAs(store, key).Open(ctx, absent); !errors.Is(err, storage.ErrNotFound) {
		t.Errorf("opening a blob the store lacks: %v", err)
	}
	if _, err := sealedAs(failingBlobs{Store: store, failPut: true}, key).Put(ctx, strings.NewReader("x")); !errors.Is(err, errBlobs) {
		t.Errorf("storing when the store fails: %v", err)
	}
	// A blob that cannot be read is neither sealed nor passable through.
	if _, err := sealedAs(failingBlobs{Store: store, failRead: true}, key).Open(ctx, public); !errors.Is(err, errBlobs) {
		t.Errorf("opening a blob that cannot be read: %v", err)
	}
	// One that reads but cannot be rewound cannot be handed over from its start.
	if _, err := sealedAs(unseekableBlobs{store}, key).Open(ctx, public); !errors.Is(err, errBlobs) {
		t.Errorf("opening an unsealed blob that cannot be rewound: %v", err)
	}
}

// unseekableBlobs hands out blobs that fail to seek the second time, which is
// when an unsealed blob is rewound to be passed through.
type unseekableBlobs struct{ *storage.Store }

func (u unseekableBlobs) Open(ctx context.Context, c cid.Cid) (storage.Blob, error) {
	blob, err := u.Store.Open(ctx, c)
	if err != nil {
		return nil, err
	}
	return &unseekableBlob{Blob: blob}, nil
}

type unseekableBlob struct {
	storage.Blob
	seeks int
}

func (u *unseekableBlob) Seek(offset int64, whence int) (int64, error) {
	if u.seeks++; u.seeks > 1 {
		return 0, errBlobs
	}
	return u.Blob.Seek(offset, whence)
}

// A job that is not private has no key to a sealed blob. It is refused
// one, where it would otherwise compute on the sealed bytes as though they
// were the data; anything else it opens and stores as it is.
func TestAJobThatIsNotPrivateIsRefusedASealedBlob(t *testing.T) {
	store := storage.NewMemory()
	key := sealed.NewKey()
	private, err := store.Put(ctx, sealed.Encrypt(key, strings.NewReader("what only a private job may read")))
	if err != nil {
		t.Fatal(err)
	}
	job := Open(store)
	if _, err := job.Open(ctx, private); err == nil || !strings.Contains(err.Error(), "is sealed, and this job is not private") {
		t.Errorf("a sealed blob opened by a job that is not private: %v", err)
	}
	public, err := job.Put(ctx, strings.NewReader("what anyone may read"))
	if err != nil {
		t.Fatal(err)
	}
	if got := readFrom(t, job, public); got != "what anyone may read" {
		t.Errorf("a blob that is not sealed read as %q", got)
	}
	// One shorter than a sealed blob's head is not sealed.
	short, _ := job.Put(ctx, strings.NewReader("hi"))
	if got := readFrom(t, job, short); got != "hi" {
		t.Errorf("a short blob read as %q", got)
	}
	absent, _ := storage.CID(ctx, strings.NewReader("never stored"))
	if _, err := job.Open(ctx, absent); !errors.Is(err, storage.ErrNotFound) {
		t.Errorf("a blob the store does not hold: %v", err)
	}
	// One that cannot be rewound cannot be handed over from its start.
	if _, err := Open(neverSeeks{store}).Open(ctx, public); !errors.Is(err, errBlobs) {
		t.Errorf("a blob that cannot be rewound: %v", err)
	}
}

// neverSeeks hands out blobs that cannot be rewound at all.
type neverSeeks struct{ *storage.Store }

func (n neverSeeks) Open(ctx context.Context, c cid.Cid) (storage.Blob, error) {
	blob, err := n.Store.Open(ctx, c)
	if err != nil {
		return nil, err
	}
	return &unseekableBlob{Blob: blob, seeks: 1}, nil
}

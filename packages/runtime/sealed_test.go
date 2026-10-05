package runtime

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/ipfs/go-cid"

	"github.com/excho0/Sisyphus/packages/sealed"
	"github.com/excho0/Sisyphus/packages/storage"
)

func TestSealedBlobsKeepPlaintextOutOfTheStore(t *testing.T) {
	store := storage.NewMemory()
	key := sealed.NewKey()
	private := Sealed(store, key)
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
	other, err := Sealed(store, sealed.NewKey()).Open(ctx, c)
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
	blob, err := Sealed(store, sealed.NewKey()).Open(ctx, public)
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

	if _, err := Sealed(store, key).Open(ctx, absent); !errors.Is(err, storage.ErrNotFound) {
		t.Errorf("opening a blob the store lacks: %v", err)
	}
	if _, err := Sealed(failingBlobs{Store: store, failPut: true}, key).Put(ctx, strings.NewReader("x")); !errors.Is(err, errBlobs) {
		t.Errorf("storing when the store fails: %v", err)
	}
	// A blob that cannot be read is neither sealed nor passable through.
	if _, err := Sealed(failingBlobs{Store: store, failRead: true}, key).Open(ctx, public); !errors.Is(err, errBlobs) {
		t.Errorf("opening a blob that cannot be read: %v", err)
	}
	// One that reads but cannot be rewound cannot be handed over from its start.
	if _, err := Sealed(unseekableBlobs{store}, key).Open(ctx, public); !errors.Is(err, errBlobs) {
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

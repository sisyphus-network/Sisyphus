package runtime

import (
	"reflect"
	"strings"
	"testing"

	"github.com/excho0/Sisyphus/packages/storage"
)

func TestRecorderNotesWhatWasReadAndWritten(t *testing.T) {
	store := storage.NewMemory()
	input, err := store.Put(ctx, strings.NewReader("an input"))
	if err != nil {
		t.Fatal(err)
	}
	other, err := store.Put(ctx, strings.NewReader("another input"))
	if err != nil {
		t.Fatal(err)
	}
	absent, err := storage.CID(ctx, strings.NewReader("never stored"))
	if err != nil {
		t.Fatal(err)
	}

	rec := Record(store)
	for _, c := range []string{input.String(), other.String(), input.String()} {
		blob, err := rec.Open(ctx, mustCID(t, c))
		if err != nil {
			t.Fatal(err)
		}
		blob.Close()
	}
	if _, err := rec.Open(ctx, absent); err == nil {
		t.Fatal("opened a blob the store lacks")
	}
	output, err := rec.Put(ctx, strings.NewReader("an output"))
	if err != nil {
		t.Fatal(err)
	}
	// Reading back its own output does not make that output an input.
	blob, err := rec.Open(ctx, output)
	if err != nil {
		t.Fatal(err)
	}
	blob.Close()
	if _, err := rec.Put(ctx, failingReader{}); err == nil {
		t.Fatal("stored from a reader that fails")
	}

	wantRead := []string{input.String(), other.String()}
	if wantRead[0] > wantRead[1] {
		wantRead[0], wantRead[1] = wantRead[1], wantRead[0]
	}
	if got := rec.Read(); !reflect.DeepEqual(got, wantRead) {
		t.Errorf("read %v, want %v", got, wantRead)
	}
	if got := rec.Written(); !reflect.DeepEqual(got, []string{output.String()}) {
		t.Errorf("written %v, want only %s", got, output)
	}
}

type failingReader struct{}

func (failingReader) Read([]byte) (int, error) { return 0, errBlobs }

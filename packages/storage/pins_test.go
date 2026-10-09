package storage

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ipfs/boxo/blockstore"
	"github.com/ipfs/go-cid"
)

// afterGrace is a time by which the grace pin on anything stored during a
// test has lapsed.
func afterGrace() time.Time { return time.Now().Add(GracePeriod + time.Minute) }

func put(t *testing.T, s *Store, data []byte) cid.Cid {
	t.Helper()
	c, err := s.Put(ctx, bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// intact reports whether the store still returns exactly data for c.
func intact(t *testing.T, s *Store, c cid.Cid, data []byte) bool {
	t.Helper()
	blob, err := s.Open(ctx, c)
	if err != nil {
		return false
	}
	defer blob.Close()
	got, err := io.ReadAll(blob)
	return err == nil && bytes.Equal(got, data)
}

func gc(t *testing.T, s *Store, now time.Time) Collected {
	t.Helper()
	done, err := s.GC(ctx, now)
	if err != nil {
		t.Fatal(err)
	}
	return done
}

func openLocal(t *testing.T, dir string) *Store {
	t.Helper()
	s, err := OpenLocal(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// Two blobs that differ throughout, and each spans several blocks.
var (
	blobA = pattern(700_000)
	blobB = bytes.Repeat([]byte("the boulder rolls back down. "), 30_000)
)

func TestGCRemovesWhatIsNotPinnedAndKeepsWhatIs(t *testing.T) {
	s := NewMemory()
	a, b := put(t, s, blobA), put(t, s, blobB)
	if err := s.Pin(ctx, "user", time.Time{}, a); err != nil {
		t.Fatal(err)
	}

	done := gc(t, s, afterGrace())

	if !intact(t, s, a, blobA) {
		t.Error("the pinned blob was damaged")
	}
	if has, _ := s.Has(ctx, b); has {
		t.Error("the unpinned blob survived")
	}
	if done.ExpiredPins != 2 {
		t.Errorf("dropped %d expired pins, want the two grace pins", done.ExpiredPins)
	}
	// A blob's blocks are its data plus one small root.
	if done.Blocks < 2 || done.Bytes < uint64(len(blobB)) || done.Bytes > uint64(len(blobB))+1000 {
		t.Errorf("removed %d blocks, %d bytes; the unpinned blob is %d bytes", done.Blocks, done.Bytes, len(blobB))
	}

	if again := gc(t, s, afterGrace()); again != (Collected{}) {
		t.Errorf("a second collection removed %+v", again)
	}
}

func TestANewBlobIsKeptForTheGracePeriod(t *testing.T) {
	s := NewMemory()
	a := put(t, s, blobA)

	if done := gc(t, s, time.Now().Add(GracePeriod-time.Minute)); done != (Collected{}) {
		t.Errorf("collection within the grace period removed %+v", done)
	}
	if !intact(t, s, a, blobA) {
		t.Error("a blob stored moments ago was collected")
	}
}

// Blobs that share blocks must not lose them when only one is collected.
func TestGCKeepsBlocksSharedWithAPinnedBlob(t *testing.T) {
	s := NewMemory()
	// Same first two chunks, different endings.
	shared := pattern(2 * chunkSize)
	kept := append(append([]byte{}, shared...), []byte("kept ending")...)
	dropped := append(append([]byte{}, shared...), []byte("dropped ending, somewhat longer")...)
	k, d := put(t, s, kept), put(t, s, dropped)
	if err := s.Pin(ctx, "user", time.Time{}, k); err != nil {
		t.Fatal(err)
	}

	done := gc(t, s, afterGrace())

	if !intact(t, s, k, kept) {
		t.Fatal("collecting one blob damaged another that shares its blocks")
	}
	if has, _ := s.Has(ctx, d); has {
		t.Error("the unpinned blob survived")
	}
	// Only the dropped blob's own ending and root go; the shared chunks stay.
	if done.Blocks != 2 {
		t.Errorf("removed %d blocks, want 2", done.Blocks)
	}
}

func TestPinsLapseAtTheirExpiry(t *testing.T) {
	s := NewMemory()
	a := put(t, s, blobA)
	expiry := time.Now().Add(3 * time.Hour)
	if err := s.Pin(ctx, "job:1", expiry, a); err != nil {
		t.Fatal(err)
	}

	gc(t, s, expiry.Add(-time.Minute))
	if !intact(t, s, a, blobA) {
		t.Fatal("blob collected before its pin expired")
	}
	gc(t, s, expiry.Add(time.Minute))
	if has, _ := s.Has(ctx, a); has {
		t.Error("blob kept after its pin expired")
	}
}

func TestABlobIsKeptUntilEveryOwnerReleasesIt(t *testing.T) {
	s := NewMemory()
	a := put(t, s, blobA)
	for _, owner := range []string{"job:1", "user"} {
		if err := s.Pin(ctx, owner, time.Time{}, a); err != nil {
			t.Fatal(err)
		}
	}

	if err := s.Unpin("job:1", a); err != nil {
		t.Fatal(err)
	}
	gc(t, s, afterGrace())
	if !intact(t, s, a, blobA) {
		t.Fatal("blob collected while one owner still pins it")
	}

	if err := s.Unpin("user", a); err != nil {
		t.Fatal(err)
	}
	gc(t, s, afterGrace())
	if has, _ := s.Has(ctx, a); has {
		t.Error("blob kept after its last pin was released")
	}
}

func TestPinningAgainReplacesTheExpiry(t *testing.T) {
	s := NewMemory()
	a := put(t, s, blobA)
	soon := time.Now().Add(2 * time.Hour)
	if err := s.Pin(ctx, "user", soon, a); err != nil {
		t.Fatal(err)
	}
	if err := s.Pin(ctx, "user", time.Time{}, a); err != nil {
		t.Fatal(err)
	}
	gc(t, s, soon.Add(time.Hour))
	if !intact(t, s, a, blobA) {
		t.Error("a pin extended to 'until released' still lapsed at its old expiry")
	}
}

func TestPinRequiresTheBlob(t *testing.T) {
	s := NewMemory()
	absent, err := CID(ctx, strings.NewReader("never stored"))
	if err != nil {
		t.Fatal(err)
	}
	a := put(t, s, blobA)
	if err := s.Pin(ctx, "user", time.Time{}, a, absent); !errors.Is(err, ErrNotFound) {
		t.Errorf("pinning a blob the store lacks: %v, want ErrNotFound", err)
	}
	// Nothing in a failed request is pinned.
	for _, pin := range s.Pins() {
		if pin.Owner == "user" {
			t.Errorf("a failed pin request still pinned %s", pin.CID)
		}
	}
	if err := s.Unpin("user", absent); err != nil {
		t.Errorf("releasing a pin that was never held: %v", err)
	}
}

func TestPinsAreListedInOrder(t *testing.T) {
	s := NewMemory()
	a, b := put(t, s, blobA), put(t, s, blobB)
	expiry := time.Now().Add(time.Hour).Truncate(time.Second)
	if err := s.Pin(ctx, "user", time.Time{}, a, b); err != nil {
		t.Fatal(err)
	}
	if err := s.Pin(ctx, "job:1", expiry, a); err != nil {
		t.Fatal(err)
	}

	var got []string
	for _, pin := range s.Pins() {
		if pin.Owner == GraceOwner {
			continue
		}
		entry := pin.CID.String() + " " + pin.Owner
		if !pin.Expires.IsZero() {
			entry += " expires"
			if !pin.Expires.Equal(expiry) {
				t.Errorf("pin expiry %v, want %v", pin.Expires, expiry)
			}
		}
		got = append(got, entry)
	}
	first, second := a.String(), b.String()
	want := []string{first + " job:1 expires", first + " user", second + " user"}
	if first > second {
		want = []string{second + " user", first + " job:1 expires", first + " user"}
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("pins:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

func TestExpireOpenPinsOnlyTouchesOpenPinsOfMatchingOwners(t *testing.T) {
	s := NewMemory()
	a := put(t, s, blobA)
	existing := time.Now().Add(5 * time.Hour).Truncate(time.Second)
	deadline := time.Now().Add(2 * time.Hour).Truncate(time.Second)
	for owner, expires := range map[string]time.Time{"job:open": {}, "job:timed": existing, "user": {}} {
		if err := s.Pin(ctx, owner, expires, a); err != nil {
			t.Fatal(err)
		}
	}

	if err := s.ExpireOpenPins(func(owner string) bool { return strings.HasPrefix(owner, "job:") }, deadline); err != nil {
		t.Fatal(err)
	}

	want := map[string]time.Time{"job:open": deadline, "job:timed": existing, "user": {}}
	for _, pin := range s.Pins() {
		if expected, ok := want[pin.Owner]; ok && !pin.Expires.Equal(expected) {
			t.Errorf("%s pin expires %v, want %v", pin.Owner, pin.Expires, expected)
		}
	}
}

func TestPinsSurviveReopening(t *testing.T) {
	dir := t.TempDir()
	s, err := OpenLocal(dir)
	if err != nil {
		t.Fatal(err)
	}
	a, b := put(t, s, blobA), put(t, s, blobB)
	expiry := time.Now().Add(48 * time.Hour)
	if err := s.Pin(ctx, "user", time.Time{}, a); err != nil {
		t.Fatal(err)
	}
	if err := s.Pin(ctx, "job:1", expiry, b); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	s = openLocal(t, dir)
	owners := make(map[string]Pin)
	for _, pin := range s.Pins() {
		owners[pin.Owner] = pin
	}
	if pin := owners["user"]; !pin.CID.Equals(a) || !pin.Expires.IsZero() {
		t.Errorf("user pin after reopening: %+v", pin)
	}
	if pin := owners["job:1"]; !pin.CID.Equals(b) || !pin.Expires.Equal(expiry) {
		t.Errorf("job pin after reopening: %+v, want expiry %v", pin, expiry)
	}

	gc(t, s, afterGrace())
	if !intact(t, s, a, blobA) || !intact(t, s, b, blobB) {
		t.Error("a blob pinned before reopening was collected after it")
	}
}

func TestSizeFollowsWhatIsStored(t *testing.T) {
	s := openLocal(t, t.TempDir())
	empty, err := s.Size(ctx)
	if err != nil {
		t.Fatal(err)
	}
	put(t, s, blobA)
	full, _ := s.Size(ctx)
	if full < empty+uint64(len(blobA)) {
		t.Errorf("size went from %d to %d after storing %d bytes", empty, full, len(blobA))
	}
	gc(t, s, afterGrace())
	// Emptied directories remain, so it does not return quite to where it began.
	if after, _ := s.Size(ctx); after > full-uint64(len(blobA)) {
		t.Errorf("size is %d after collecting everything, was %d when full", after, full)
	}

	if size, err := NewMemory().Size(ctx); err != nil || size != 0 {
		t.Errorf("memory store size %d, %v; want 0", size, err)
	}
}

// Storing takes the collector's lock, so a blob being written is never
// swept from under its writer.
func TestStoringWhileCollectingLosesNothing(t *testing.T) {
	s := NewMemory()
	const writers, each = 4, 5
	stored := make([][]cid.Cid, writers)
	var wg sync.WaitGroup
	stop := make(chan struct{})
	collecting := make(chan struct{})
	go func() {
		defer close(collecting)
		for {
			select {
			case <-stop:
				return
			default:
				s.GC(ctx, time.Now())
			}
		}
	}()
	for w := range writers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range each {
				data := append(pattern(300_000), byte(w), byte(i))
				c, err := s.Put(ctx, bytes.NewReader(data))
				if err != nil {
					t.Error(err)
					return
				}
				stored[w] = append(stored[w], c)
			}
		}()
	}
	wg.Wait()
	close(stop)
	<-collecting

	for w := range writers {
		for i, c := range stored[w] {
			if !intact(t, s, c, append(pattern(300_000), byte(w), byte(i))) {
				t.Errorf("blob %d of writer %d was damaged by a concurrent collection", i, w)
			}
		}
	}
}

func TestGCStopsBeforeDeletingIfAPinnedBlobIsBroken(t *testing.T) {
	s := NewMemory()
	a, b := put(t, s, blobA), put(t, s, blobB)
	if err := s.Pin(ctx, "user", time.Time{}, a); err != nil {
		t.Fatal(err)
	}
	// Remove the pinned blob's root behind the store's back.
	if err := s.blocks.DeleteBlock(ctx, a); err != nil {
		t.Fatal(err)
	}

	done, err := s.GC(ctx, afterGrace())
	if err == nil || !strings.Contains(err.Error(), "pinned blob "+a.String()) {
		t.Errorf("error %v, want one naming the broken blob", err)
	}
	if done.Blocks != 0 {
		t.Errorf("deleted %d blocks despite failing", done.Blocks)
	}
	if has, _ := s.Has(ctx, b); !has {
		t.Error("a failed collection still deleted an unpinned blob")
	}
}

// failingBlocks is a block store whose lookups or listings fail, as a disk
// that has gone bad would make them.
type failingBlocks struct {
	blockstore.Blockstore
	failHas, failList bool
}

var errBadDisk = errors.New("bad disk")

func (f failingBlocks) Has(ctx context.Context, c cid.Cid) (bool, error) {
	if f.failHas {
		return false, errBadDisk
	}
	return f.Blockstore.Has(ctx, c)
}

func (f failingBlocks) AllKeysChan(ctx context.Context) (<-chan cid.Cid, error) {
	if f.failList {
		return nil, errBadDisk
	}
	return f.Blockstore.AllKeysChan(ctx)
}

func TestPinAndGCReportABlockStoreThatFails(t *testing.T) {
	s := NewMemory()
	a := put(t, s, blobA)

	s.blocks = failingBlocks{Blockstore: s.blocks, failHas: true}
	if err := s.Pin(ctx, "user", time.Time{}, a); !errors.Is(err, errBadDisk) {
		t.Errorf("Pin: %v, want the block store's error", err)
	}

	s.blocks = failingBlocks{Blockstore: s.blocks.(failingBlocks).Blockstore, failList: true}
	if _, err := s.GC(ctx, afterGrace()); !errors.Is(err, errBadDisk) {
		t.Errorf("GC: %v, want the block store's error", err)
	}
}

func TestPinFileCannotBeWritten(t *testing.T) {
	dir := t.TempDir()
	s := openLocal(t, dir)
	a := put(t, s, blobA)

	// The temporary file's name is taken by a directory.
	blocker := filepath.Join(dir, "pins.json.tmp")
	if err := os.Mkdir(blocker, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := s.Pin(ctx, "user", time.Time{}, a); err == nil || !strings.Contains(err.Error(), "save pins") {
		t.Errorf("Pin: %v, want a save failure", err)
	}
	if _, err := s.GC(ctx, time.Now()); err == nil || !strings.Contains(err.Error(), "save pins") {
		t.Errorf("GC: %v, want a save failure", err)
	}
	if !intact(t, s, a, blobA) {
		t.Error("a collection that could not save its pins deleted a blob")
	}
	if err := os.Remove(blocker); err != nil {
		t.Fatal(err)
	}

	// The pin file's own name is taken by a directory that is not empty.
	if err := os.MkdirAll(filepath.Join(dir, "pins.json", "occupied"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := s.Unpin("user", a); err == nil || !strings.Contains(err.Error(), "save pins") {
		t.Errorf("Unpin: %v, want a save failure", err)
	}
}

func TestOpeningAStoreWithABadPinFileFails(t *testing.T) {
	tests := map[string]func(path string) error{
		"not JSON":       func(path string) error { return os.WriteFile(path, []byte("not json"), 0o600) },
		"not a CID":      func(path string) error { return os.WriteFile(path, []byte(`[{"cid":"nope","owner":"user"}]`), 0o600) },
		"is a directory": func(path string) error { return os.Mkdir(path, 0o700) },
	}
	for name, spoil := range tests {
		dir := t.TempDir()
		if err := spoil(filepath.Join(dir, "pins.json")); err != nil {
			t.Fatal(err)
		}
		if s, err := OpenLocal(dir); err == nil {
			s.Close()
			t.Errorf("%s: opened a store whose pins cannot be read", name)
		}
		// Refusing to open is better than opening with no pins, which would
		// let the next collection delete everything. The failed open must
		// still release the directory.
		if err := os.RemoveAll(filepath.Join(dir, "pins.json")); err != nil {
			t.Fatal(err)
		}
		s, err := OpenLocal(dir)
		if err != nil {
			t.Fatalf("%s: opening after a failed open: %v", name, err)
		}
		s.Close()
	}
}

func TestGCReportsABlockItCannotDelete(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root can delete from a read-only directory")
	}
	dir := t.TempDir()
	s := openLocal(t, dir)
	put(t, s, []byte("one small unpinned blob"))

	shards, err := filepath.Glob(filepath.Join(dir, "blocks", "*", "*.data"))
	if err != nil || len(shards) != 1 {
		t.Fatalf("expected one block file, found %v (%v)", shards, err)
	}
	shard := filepath.Dir(shards[0])
	if err := os.Chmod(shard, 0o500); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(shard, 0o700)

	if _, err := s.GC(ctx, afterGrace()); err == nil || !strings.Contains(err.Error(), "collect garbage") {
		t.Errorf("error %v, want a collection failure", err)
	}
}

func TestPinningOrUnpinningNothingDoesNothing(t *testing.T) {
	dir := t.TempDir()
	s := openLocal(t, dir)
	if err := s.Pin(ctx, "user", time.Time{}); err != nil {
		t.Error(err)
	}
	if err := s.Unpin("user"); err != nil {
		t.Error(err)
	}
	// Neither touched the disk.
	if _, err := os.Stat(filepath.Join(dir, "pins.json")); !os.IsNotExist(err) {
		t.Errorf("an empty pin request wrote the pin file (stat error: %v)", err)
	}
}

package storage

import (
	"strings"
	"testing"
	"time"

	"github.com/ipfs/go-cid"
)

// clientOf groups pins as a coordinator does: a client's own, and those of
// its job, count towards it; the rest towards nobody.
func clientOf(owner string) string {
	switch {
	case strings.HasPrefix(owner, "user:"):
		return strings.TrimPrefix(owner, "user:")
	case owner == "job:1":
		return "alice"
	}
	return ""
}

func rootSize(t *testing.T, s *Store, c cid.Cid) uint64 {
	t.Helper()
	size, err := s.blocks.GetSize(ctx, c)
	if err != nil {
		t.Fatal(err)
	}
	return uint64(size)
}

// What a group keeps is every block its pins reach, each once; a block
// kept by two groups counts in full for each.
func TestUsageCountsWhatEachGroupKeepsOnce(t *testing.T) {
	s := NewMemory()
	// Same first two chunks, different endings.
	shared := pattern(2 * chunkSize)
	kept := append(append([]byte{}, shared...), []byte("kept ending")...)
	other := append(append([]byte{}, shared...), []byte("another ending, somewhat longer")...)
	k, o := put(t, s, kept), put(t, s, other)
	now := time.Now()
	pin := func(owner string, expires time.Time, c cid.Cid) {
		t.Helper()
		if err := s.Pin(ctx, owner, expires, c); err != nil {
			t.Fatal(err)
		}
	}
	pin("user:alice", time.Time{}, k)
	pin("job:1", now.Add(time.Hour), o) // alice's job
	pin("job:1", now.Add(time.Hour), k) // and the same blob again
	pin("user:bob", time.Time{}, k)
	pin("user", time.Time{}, o)                 // the node's own user: nobody's
	pin("user:carol", now.Add(-time.Minute), k) // lapsed

	used, err := s.Usage(ctx, now, clientOf)
	if err != nil {
		t.Fatal(err)
	}
	bob := uint64(len(kept)) + rootSize(t, s, k)
	alice := uint64(len(kept)+len(other)-len(shared)) + rootSize(t, s, k) + rootSize(t, s, o)
	if len(used) != 2 || used["bob"] != bob || used["alice"] != alice {
		t.Errorf("usage %v, want alice %d and bob %d", used, alice, bob)
	}
}

// A pin on a block that is not there, as no pin should be, is an error:
// whatever count came of it would be wrong.
func TestUsageOfABlobThatIsGoneFails(t *testing.T) {
	s := NewMemory()
	c := put(t, s, []byte("a small blob"))
	if err := s.Pin(ctx, "user:dave", time.Time{}, c); err != nil {
		t.Fatal(err)
	}
	if err := s.blocks.DeleteBlock(ctx, c); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Usage(ctx, time.Now(), clientOf); err == nil || !strings.Contains(err.Error(), "usage of dave") {
		t.Errorf("usage of a blob that is gone: %v", err)
	}
}

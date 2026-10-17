package storage

import (
	"maps"
	"math/rand/v2"
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

func pinFor(t *testing.T, s *Store, owner string, expires time.Time, c cid.Cid) {
	t.Helper()
	if err := s.Pin(ctx, owner, expires, c); err != nil {
		t.Fatal(err)
	}
}

// walked is what each group keeps as of now, found the slow way, by
// walking every pin: what the index must always agree with.
func walked(t *testing.T, s *Store, now time.Time, group func(string) string) map[string]uint64 {
	t.Helper()
	blocks := make(map[string]map[string]uint64)
	for _, pin := range s.Pins() {
		name := group(pin.Owner)
		if name == "" || !pin.Expires.IsZero() && pin.Expires.Before(now) {
			continue
		}
		r, err := s.reachOf(ctx, pin.CID)
		if err != nil {
			t.Fatal(err)
		}
		if blocks[name] == nil {
			blocks[name] = make(map[string]uint64)
		}
		for _, b := range r.blocks {
			blocks[name][b.hash] = b.size
		}
	}
	used := make(map[string]uint64)
	for name, kept := range blocks {
		for _, size := range kept {
			used[name] += size
		}
	}
	return used
}

// What a group keeps is every block its pins reach, each once; a block
// kept by two groups counts in full for each.
func TestUsageCountsWhatEachGroupKeepsOnce(t *testing.T) {
	s := NewMemory()
	if got := s.Usage(time.Now()); got != nil {
		t.Errorf("with no groups named, usage %v", got)
	}
	// Same first two chunks, different endings.
	shared := pattern(2 * chunkSize)
	kept := append(append([]byte{}, shared...), []byte("kept ending")...)
	other := append(append([]byte{}, shared...), []byte("another ending, somewhat longer")...)
	k, o := put(t, s, kept), put(t, s, other)
	now := time.Now()
	// Pins held before the groups are named are counted as well as those
	// placed after.
	pinFor(t, s, "user:alice", time.Time{}, k)
	if err := s.GroupPins(ctx, clientOf); err != nil {
		t.Fatal(err)
	}
	pinFor(t, s, "job:1", now.Add(time.Hour), o) // alice's job
	pinFor(t, s, "job:1", now.Add(time.Hour), k) // and the same blob again
	pinFor(t, s, "user:bob", time.Time{}, k)
	pinFor(t, s, "user", time.Time{}, o)                 // the node's own user: nobody's
	pinFor(t, s, "user:carol", now.Add(-time.Minute), k) // lapsed

	used := s.Usage(now)
	bob := uint64(len(kept)) + rootSize(t, s, k)
	alice := uint64(len(kept)+len(other)-len(shared)) + rootSize(t, s, k) + rootSize(t, s, o)
	if len(used) != 2 || used["bob"] != bob || used["alice"] != alice {
		t.Errorf("usage %v, want alice %d and bob %d", used, alice, bob)
	}

	// Releasing one of alice's pins frees only what no other still keeps.
	if err := s.Unpin("job:1", o, k); err != nil {
		t.Fatal(err)
	}
	if used := s.Usage(now); used["alice"] != bob {
		t.Errorf("alice keeps %d once her job's pins are gone, want %d", used["alice"], bob)
	}
	if err := s.Unpin("user:alice", k); err != nil {
		t.Fatal(err)
	}
	if used := s.Usage(now); len(used) != 1 {
		t.Errorf("usage %v once alice keeps nothing", used)
	}
}

// A pin stops counting when it lapses, though only collection drops it; a
// pin given a later expiry before then counts on, and one placed again
// after it lapsed counts again.
func TestAPinStopsCountingWhenItLapses(t *testing.T) {
	s := NewMemory()
	if err := s.GroupPins(ctx, clientOf); err != nil {
		t.Fatal(err)
	}
	c := put(t, s, pattern(3*chunkSize))
	start := time.Now()
	pinFor(t, s, "user:alice", start.Add(time.Minute), c)
	whole := s.Usage(start)["alice"]
	if whole == 0 {
		t.Fatal("a pin that has not lapsed is not counted")
	}
	pinFor(t, s, "user:alice", start.Add(time.Hour), c) // later, before it lapses
	if got := s.Usage(start.Add(2 * time.Minute))["alice"]; got != whole {
		t.Errorf("a pin given more time stopped counting by its old expiry: %d", got)
	}
	if got := s.Usage(start.Add(2 * time.Hour))["alice"]; got != 0 {
		t.Errorf("a pin that lapsed still counts %d", got)
	}
	pinFor(t, s, "user:alice", time.Time{}, c)
	if got := s.Usage(start.Add(2 * time.Hour))["alice"]; got != whole {
		t.Errorf("a pin placed again counts %d", got)
	}

	// A pin of one that has gone is given an expiry, and lapses by it.
	if err := s.ExpireOpenPins(func(owner string) bool { return owner == "user:alice" }, start.Add(3*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if got := s.Usage(start.Add(4 * time.Hour))["alice"]; got != 0 {
		t.Errorf("an orphaned pin past its new expiry counts %d", got)
	}
	// Collection drops it; a pin it never counted is ignored there too.
	pinFor(t, s, "user", start.Add(time.Minute), c)
	if _, err := s.GC(ctx, start.Add(5*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if used := s.Usage(start.Add(5 * time.Hour)); len(used) != 0 {
		t.Errorf("usage %v after collection", used)
	}
}

// Whatever is pinned, released, lapsed or collected, in whatever order,
// the index says what walking every pin would.
func TestUsageAgreesWithAWalkOfEveryPin(t *testing.T) {
	s := NewMemory()
	group := func(owner string) string {
		if name, found := strings.CutPrefix(owner, "user:"); found {
			return name
		}
		if strings.HasPrefix(owner, "job:") {
			return "alice"
		}
		return ""
	}
	if err := s.GroupPins(ctx, group); err != nil {
		t.Fatal(err)
	}
	// Blobs that share chunks with each other, and one inside another.
	base := pattern(4 * chunkSize)
	var blobs []cid.Cid
	for i := range 5 {
		data := append(append([]byte{}, base[:(i%3+1)*chunkSize]...), []byte(strings.Repeat("x", i+1))...)
		blobs = append(blobs, put(t, s, data))
	}
	blobs = append(blobs, put(t, s, base[:chunkSize])) // a single chunk of the others
	owners := []string{"user:alice", "user:bob", "job:1", "job:2", "user", "pool:f"}
	rng := rand.New(rand.NewPCG(1, 2))
	now := time.Now()
	for step := range 400 {
		owner, c := owners[rng.IntN(len(owners))], blobs[rng.IntN(len(blobs))]
		switch rng.IntN(6) {
		case 0, 1:
			pinFor(t, s, owner, time.Time{}, c)
		case 2:
			pinFor(t, s, owner, now.Add(time.Duration(rng.IntN(60))*time.Second), c)
		case 3:
			if err := s.Unpin(owner, c); err != nil {
				t.Fatal(err)
			}
		case 4:
			now = now.Add(time.Duration(rng.IntN(20)) * time.Second)
		case 5:
			if step%20 == 0 {
				if _, err := s.GC(ctx, now); err != nil {
					t.Fatal(err)
				}
				// What collection swept is put back for later steps.
				for i, b := range blobs {
					if has, _ := s.blocks.Has(ctx, b); !has {
						data := append(append([]byte{}, base[:(i%3+1)*chunkSize]...), []byte(strings.Repeat("x", i+1))...)
						if i == len(blobs)-1 {
							data = base[:chunkSize]
						}
						put(t, s, data)
					}
				}
			}
		}
		got, want := s.Usage(now), walked(t, s, now, group)
		maps.DeleteFunc(got, func(_ string, n uint64) bool { return n == 0 })
		if !maps.Equal(got, want) {
			t.Fatalf("step %d: index %v, walk %v", step, got, want)
		}
	}
}

// A pin that reaches a block the store does not have, as no pin should,
// cannot be counted: one already held is left out, and said so, and a new
// one is refused.
func TestAPinOfABlobThatIsMissingABlockIsNotCounted(t *testing.T) {
	s := NewMemory()
	whole := put(t, s, pattern(3*chunkSize))
	small := put(t, s, []byte("a small blob"))
	pinFor(t, s, "user:dave", time.Time{}, whole)
	pinFor(t, s, "user:erin", time.Time{}, small)
	links, err := s.links(ctx, whole)
	if err != nil || len(links) == 0 {
		t.Fatalf("links %v, %v", links, err)
	}
	if err := s.blocks.DeleteBlock(ctx, links[0].Cid); err != nil {
		t.Fatal(err)
	}
	if err := s.GroupPins(ctx, clientOf); err == nil || !strings.Contains(err.Error(), "pin of user:dave") {
		t.Errorf("grouping pins with one that cannot be counted: %v", err)
	}
	used := s.Usage(time.Now())
	if _, counted := used["dave"]; counted || used["erin"] != rootSize(t, s, small) {
		t.Errorf("usage %v", used)
	}
	if err := s.Pin(ctx, "user:frank", time.Time{}, whole); err == nil {
		t.Error("a blob missing a block was pinned for a client")
	}
	// One that counts towards nobody is pinned as before.
	pinFor(t, s, "user", time.Time{}, whole)
}

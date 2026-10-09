package replication

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ipfs/go-cid"

	"github.com/sisyphus-network/Sisyphus/apps/sisyphusd/blobclient"
	pb "github.com/sisyphus-network/Sisyphus/packages/protocol/sisyphus/v1"
	"github.com/sisyphus-network/Sisyphus/packages/storage"
)

var ctx = context.Background()

// logged collects what a manager or follower logs.
type logged struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *logged) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

func (l *logged) count(text string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return strings.Count(l.buf.String(), text)
}

func (l *logged) logger() *slog.Logger { return slog.New(slog.NewTextHandler(l, nil)) }

// graceOf sets the grace period for the managers a test goes on to make.
func graceOf(t *testing.T, grace time.Duration) {
	t.Helper()
	usual := Grace
	Grace = grace
	t.Cleanup(func() { Grace = usual })
}

// put stores text and returns its CID, as a string.
func put(t *testing.T, store *storage.Store, text string) string {
	t.Helper()
	c, err := store.Put(ctx, strings.NewReader(text))
	if err != nil {
		t.Fatal(err)
	}
	return c.String()
}

// keep stores text and pins it for the user, until released.
func keep(t *testing.T, store *storage.Store, text string) string {
	t.Helper()
	id := put(t, store, text)
	if err := store.Pin(ctx, "user", time.Time{}, cid.MustParse(id)); err != nil {
		t.Fatal(err)
	}
	return id
}

// rig is a manager over a store of its own, and the stores of followers it
// can fetch from by name.
type rig struct {
	*Manager
	store *storage.Store
	logs  *logged
	// followers are the stores of the followers that serve blobs, by ID.
	followers map[string]*storage.Store
}

// newRig returns a manager, wanting so many copies, of a store that already
// has each of the given texts pinned.
func newRig(t *testing.T, replicas int, pinned ...string) *rig {
	t.Helper()
	r := &rig{store: storage.NewMemory(), logs: new(logged), followers: make(map[string]*storage.Store)}
	for _, text := range pinned {
		keep(t, r.store, text)
	}
	r.Manager = New(Config{
		ID: "node", Store: r.store, StoreID: "new", Replicas: replicas, Log: r.logs.logger(),
		Address: func(id string) string {
			if r.followers[id] == nil {
				return ""
			}
			return id + ":7701"
		},
		Fetch: func(ctx context.Context, from *pb.BlobHolder, c cid.Cid, into blobclient.Putter) error {
			blob, err := r.followers[from.GetNodeId()].Open(ctx, c)
			if err != nil {
				return err
			}
			defer blob.Close()
			_, err = into.Put(ctx, blob)
			return err
		},
	})
	return r
}

// obey has a follower ask what to hold, saying it holds what it was last
// told to, and returns what it is told now.
func (r *rig) obey(id string, holding []string) []string {
	hold, _ := r.Replicate(id, "node/new", holding)
	return hold
}

// settle has the followers ask in turn, each doing as it is told, until
// nothing changes, and returns what each ends up holding. What one is told
// depends on what the others said they held when they last asked, so a
// round in which nothing changes may only mean that word has yet to get
// round: it takes two.
func (r *rig) settle(t *testing.T, holding map[string][]string, ids ...string) map[string][]string {
	t.Helper()
	quiet := 0
	for range 20 {
		changed := false
		for _, id := range ids {
			now := r.obey(id, holding[id])
			changed = changed || !slices.Equal(now, holding[id])
			holding[id] = now
		}
		if quiet++; changed {
			quiet = 0
		}
		if quiet == 2 {
			return holding
		}
	}
	t.Fatal("the followers never settled on who holds what")
	return nil
}

// holdersOf names the followers holding each blob.
func holdersOf(holding map[string][]string) map[string][]string {
	holders := make(map[string][]string)
	for id, blobs := range holding {
		for _, blob := range blobs {
			holders[blob] = append(holders[blob], id)
		}
	}
	for _, ids := range holders {
		slices.Sort(ids)
	}
	return holders
}

func TestAFollowerJoiningMovesOnlyWhatItTakesOver(t *testing.T) {
	graceOf(t, time.Minute)
	r := newRig(t, 2)
	for i := range 40 {
		keep(t, r.store, fmt.Sprintf("blob %d", i))
	}
	holding := r.settle(t, make(map[string][]string), "a", "b", "c")
	before := holdersOf(holding)
	if len(before) != 40 {
		t.Fatalf("%d blobs are held, want all 40", len(before))
	}
	for blob, ids := range before {
		if len(ids) != 2 {
			t.Errorf("blob %s is held by %v, want two of the three followers", blob, ids)
		}
	}
	for _, id := range []string{"a", "b", "c"} {
		if n := len(holding[id]); n < 15 || n > 38 {
			t.Errorf("follower %s holds %d of 40 blobs, want about two thirds", id, n)
		}
	}

	// Asking again, in another order, changes nothing.
	again := holdersOf(r.settle(t, holding, "c", "a", "b"))
	for blob, ids := range before {
		if !slices.Equal(again[blob], ids) {
			t.Errorf("blob %s moved from %v to %v with no change of followers", blob, ids, again[blob])
		}
	}

	// A fourth follower takes a share, and nothing passes between the
	// other three.
	after := holdersOf(r.settle(t, holding, "a", "b", "c", "d"))
	moved := 0
	for blob, was := range before {
		now := after[blob]
		if len(now) != 2 {
			t.Errorf("blob %s is now held by %v, want two followers", blob, now)
		}
		if slices.Equal(now, was) {
			continue
		}
		moved++
		kept := 0
		for _, id := range now {
			if slices.Contains(was, id) {
				kept++
			}
		}
		if !slices.Contains(now, "d") || kept != 1 {
			t.Errorf("blob %s moved from %v to %v, want one of its holders to have given way to the newcomer", blob, was, now)
		}
	}
	if moved == 0 || moved == 40 {
		t.Errorf("%d of 40 blobs moved when a fourth follower joined, want a share of them", moved)
	}
}

func TestACopyIsKeptUntilItsReplacementIsHeld(t *testing.T) {
	graceOf(t, time.Minute)
	r := newRig(t, 1)
	var all []string
	for i := range 20 {
		all = append(all, keep(t, r.store, fmt.Sprintf("blob %d", i)))
	}
	slices.Sort(all)

	// A lone follower holds everything.
	first := r.obey("a", nil)
	if !slices.Equal(first, all) {
		t.Fatalf("a lone follower is to hold %d blobs, want all 20", len(first))
	}
	// A second is given a share, which it has yet to fetch.
	share := r.obey("b", nil)
	if len(share) == 0 || len(share) == 20 {
		t.Fatalf("a second follower is given %d of 20 blobs, want a share", len(share))
	}
	// Until it says it holds them, the first gives nothing up.
	if still := r.obey("a", first); !slices.Equal(still, all) {
		t.Errorf("the first follower is to hold %d blobs while the second has fetched none, want still all 20", len(still))
	}
	// Half way there, it gives up only what the second now holds.
	half := share[:len(share)/2+1]
	r.obey("b", half)
	if now := r.obey("a", first); len(now) != 20-len(half) {
		t.Errorf("the first follower is to hold %d blobs with %d moved, want %d", len(now), len(half), 20-len(half))
	}
	r.obey("b", share)
	rest := r.obey("a", first)
	if len(rest) != 20-len(share) {
		t.Errorf("the first follower is to hold %d blobs once the second holds its %d, want the other %d", len(rest), len(share), 20-len(share))
	}
	for _, blob := range rest {
		if slices.Contains(share, blob) {
			t.Errorf("blob %s is still with both followers", blob)
		}
	}
}

func TestACoordinatorThatHasJustStartedWaitsToHearWhoHoldsWhat(t *testing.T) {
	graceOf(t, time.Second)
	r := newRig(t, 1, "had before it stopped", "had this too")
	old := r.Status("").GetBlobs()
	if len(old) != 2 || !r.Status("").GetSettling() {
		t.Fatalf("a manager started over two pinned blobs reports %v", r.Status(""))
	}
	fresh := keep(t, r.store, "pinned since it started")

	// A follower that holds nothing is handed only what is new: the rest
	// may be held already by one that has yet to ask.
	if hold := r.obey("empty", nil); !slices.Equal(hold, []string{fresh}) {
		t.Errorf("a follower with nothing is to hold %v, want only the blob pinned since the start, %s", hold, fresh)
	}
	// One that held copies keeps them, whoever they would now go to.
	had := []string{old[0].GetCid(), old[1].GetCid()}
	if hold := r.obey("full", had); !slices.Contains(hold, had[0]) || !slices.Contains(hold, had[1]) {
		t.Errorf("a follower that held both old blobs is to hold %v", hold)
	}

	// Once the wait is over, everything is handed out as usual.
	time.Sleep(1100 * time.Millisecond)
	if r.Status("").GetSettling() {
		t.Error("the manager is still waiting after its grace period")
	}
	holding := r.settle(t, map[string][]string{"full": had}, "empty", "full")
	for blob, ids := range holdersOf(holding) {
		if len(ids) != 1 {
			t.Errorf("blob %s ended up with %v, want one follower", blob, ids)
		}
	}
	if len(holdersOf(holding)) != 3 {
		t.Errorf("%d blobs are held, want all three", len(holdersOf(holding)))
	}
}

func TestOnlyWhatSomebodyAskedToKeepIsCopied(t *testing.T) {
	graceOf(t, time.Minute)
	r := newRig(t, 1)
	loose := put(t, r.store, "stored, with only the short pin every new blob gets")
	lapsed, lasting, twice := put(t, r.store, "its pin has run out"), put(t, r.store, "kept for a day"), put(t, r.store, "kept by two")
	pin := func(owner string, expires time.Time, id string) {
		t.Helper()
		if err := r.store.Pin(ctx, owner, expires, cid.MustParse(id)); err != nil {
			t.Fatal(err)
		}
	}
	pin("job:1", time.Now().Add(-time.Minute), lapsed)
	pin("job:1", time.Now().Add(24*time.Hour), lasting)
	pin("job:1", time.Time{}, twice)
	pin("user", time.Time{}, twice)
	// The record of a job is pinned too, and is not a file: it is not
	// handed to followers.
	record, err := r.store.PutNodes(ctx, []byte{0xa0})
	if err != nil {
		t.Fatal(err)
	}
	pin("record:1", time.Time{}, record.String())

	want := []string{lasting, twice}
	slices.Sort(want)
	hold := r.obey("a", []string{loose, lapsed})
	if !slices.Equal(hold, want) {
		t.Errorf("a follower is to hold %v, want the two blobs with a pin still in force, once each: %v", hold, want)
	}
	if listed := r.Status(twice).GetBlobs(); len(listed) != 1 || listed[0].GetCid() != twice {
		t.Errorf("asked about one blob, the manager reports %v", listed)
	}
}

func TestAFollowerNotHeardFromIsGivenUpOn(t *testing.T) {
	graceOf(t, 400*time.Millisecond)
	r := newRig(t, 1)
	blob := keep(t, r.store, "held by a follower about to vanish")
	r.followers["a"] = storage.NewMemory()
	r.obey("a", r.obey("a", nil))
	if status := r.Status(""); !slices.Equal(status.GetFollowers(), []string{"a"}) || !slices.Equal(status.GetBlobs()[0].GetHolders(), []string{"a"}) {
		t.Fatalf("with one follower holding the blob the manager reports %v", status)
	}
	if holders := r.Holders(blob, ""); len(holders) != 1 || holders[0].GetNodeId() != "a" || holders[0].GetAddress() != "a:7701" {
		t.Errorf("the blob's holders are %v, want the follower at its address", holders)
	}
	if holders := r.Holders(blob, "a"); len(holders) != 0 {
		t.Errorf("a follower asking who else holds a blob is told of %v", holders)
	}

	time.Sleep(500 * time.Millisecond)
	if status := r.Status(""); len(status.GetFollowers()) != 0 || len(status.GetBlobs()[0].GetHolders()) != 0 {
		t.Errorf("after the grace period the manager still reports %v", status)
	}
	if len(r.Holders(blob, "")) != 0 {
		t.Error("a follower that has gone is still named as holding a blob")
	}
	if r.logs.count("a storage follower has not been heard from") != 1 {
		t.Error("giving up on a follower was not logged, once")
	}
	if r.logs.count("a storage follower is here") != 1 {
		t.Error("a follower's arrival was not logged, once")
	}
}

func TestCopiesOfAStoreThatWasLostAreKeptUntilRestored(t *testing.T) {
	graceOf(t, time.Minute)
	r := newRig(t, 1)
	pinned := keep(t, r.store, "pinned in the store the node has now")
	// The follower holds two blobs the lost store had pinned, and can
	// supply one of them for now.
	theirs := storage.NewMemory()
	r.followers["a"] = theirs
	servable := put(t, theirs, "held by the follower, and sound")
	unservable := put(t, storage.NewMemory(), "held by the follower, it says")
	holding := []string{servable, unservable, pinned, "not-a-cid"}
	slices.Sort(holding)

	hold, store := r.Replicate("a", "node/old", holding)
	slices.Sort(hold)
	if !slices.Equal(hold, holding) || store != "node/old" {
		t.Fatalf("a follower with copies of a lost store is to hold %v for %q, want everything it has, for the old store still", hold, store)
	}
	r.Replicate("a", "node/old", holding)
	if r.logs.count("holds blobs from a store this node had before") != 1 {
		t.Error("finding copies of a lost store was not logged, once")
	}
	if status := r.Status(""); status.GetFromEarlierStore() != 3 {
		t.Errorf("the manager reports %d blobs from an earlier store, want 3", status.GetFromEarlierStore())
	}
	// Nothing is taken up before the node's owner says so.
	if has, _ := r.store.Has(ctx, cid.MustParse(servable)); has {
		t.Fatal("the node fetched a blob on a follower's word alone")
	}

	restored, failed := r.Restore(ctx, "user")
	if want := []string{unservable, "not-a-cid"}; restored != 1 || !slices.Equal(failed, want) {
		t.Errorf("restoring took back %d blobs and failed on %v, want 1 and %v", restored, failed, want)
	}
	if !slices.ContainsFunc(r.store.Pins(), func(pin storage.Pin) bool {
		return pin.CID.String() == servable && pin.Owner == "user" && pin.Expires.IsZero()
	}) {
		t.Errorf("the restored blob is not pinned for the user until released: %+v", r.store.Pins())
	}
	if status := r.Status(""); status.GetFromEarlierStore() != 2 {
		t.Errorf("after restoring one blob the manager reports %d left from an earlier store, want 2", status.GetFromEarlierStore())
	}
	// While anything is left, the follower goes on holding for the old
	// store.
	if _, store := r.Replicate("a", "node/old", holding); store != "node/old" {
		t.Errorf("with blobs still to restore the follower is told its store is %q", store)
	}

	// Once the follower can supply the rest, a second go finishes the job,
	// and the follower moves on to the new store.
	put(t, theirs, "held by the follower, it says")
	holding = slices.DeleteFunc(holding, func(id string) bool { return id == "not-a-cid" })
	r.Replicate("a", "node/old", holding)
	if restored, failed := r.Restore(ctx, "user"); restored != 1 || len(failed) != 0 {
		t.Errorf("restoring again took back %d blobs and failed on %v, want 1 and none", restored, failed)
	}
	hold, store = r.Replicate("a", "node/old", holding)
	if store != "node/new" || !slices.Equal(hold, holding) {
		t.Errorf("after everything was restored the follower is to hold %v for %q, want all three for the new store", hold, store)
	}
	if status := r.Status(""); status.GetFromEarlierStore() != 0 || len(status.GetBlobs()) != 3 {
		t.Errorf("after everything was restored the manager reports %v", status)
	}
}

func TestCopiesOfAnotherNodesStoreAreNotKept(t *testing.T) {
	graceOf(t, time.Minute)
	r := newRig(t, 1)
	pinned := keep(t, r.store, "pinned")
	foreign := put(t, storage.NewMemory(), "held for a pool this follower was in before")
	for _, store := range []string{"other-node/old", "", "node/new"} {
		hold, now := r.Replicate("a", store, []string{foreign})
		if !slices.Equal(hold, []string{pinned}) || now != "node/new" {
			t.Errorf("a follower holding a blob for store %q is to hold %v for %q, want only what this node has pinned", store, hold, now)
		}
	}
	if status := r.Status(""); status.GetFromEarlierStore() != 0 {
		t.Errorf("the manager counts %d blobs as left from an earlier store", status.GetFromEarlierStore())
	}
}

func TestALostBlobIsFetchedBackFromAFollowerThatCanSupplyIt(t *testing.T) {
	graceOf(t, time.Minute)
	r := newRig(t, 1)
	const text = "lost by the node, held by its followers"
	lost := put(t, storage.NewMemory(), text)
	c := cid.MustParse(lost)
	kept := Recovering{Store: r.store, From: r.Manager}

	// Nobody is known to hold it.
	if _, err := kept.Open(ctx, c); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("opening a blob no follower holds: %v", err)
	}
	// Three say they do: one serves nothing, one turns out not to have it,
	// and the third does.
	r.followers["b"], r.followers["c"] = storage.NewMemory(), storage.NewMemory()
	for _, id := range []string{"a", "b", "c"} {
		r.Replicate(id, "node/new", []string{lost})
	}
	if _, err := kept.Open(ctx, c); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("opening a blob no follower can supply: %v", err)
	}
	if r.logs.count("could not fetch a blob back from a storage follower") != 2 {
		t.Error("the followers that failed to supply the blob were not logged")
	}
	put(t, r.followers["c"], text)
	blob, err := kept.Open(ctx, c)
	if err != nil {
		t.Fatalf("opening a blob a follower holds: %v", err)
	}
	defer blob.Close()
	var got bytes.Buffer
	if _, err := got.ReadFrom(blob); err != nil || got.String() != text {
		t.Errorf("the blob fetched back reads %q, error %v", got.String(), err)
	}
	// Now that the store has it, no follower is asked again.
	before := r.logs.count("fetched back from a storage follower")
	if again, err := kept.Open(ctx, c); err != nil {
		t.Errorf("opening the blob a second time: %v", err)
	} else {
		again.Close()
	}
	if before != 1 || r.logs.count("fetched back from a storage follower") != 1 {
		t.Error("the blob was not fetched back exactly once")
	}
}

func TestANodeThatAsksNothingOfFollowersGivesThemNothingToHold(t *testing.T) {
	store := storage.NewMemory()
	pinned := keep(t, store, "pinned on a node that keeps no copies")
	off := Off(store)
	hold, name := off.Replicate("a", "", []string{pinned})
	if len(hold) != 0 || name != "/" {
		t.Errorf("a follower of a node that keeps no copies is to hold %v for %q", hold, name)
	}
	if holders := off.Holders(pinned, ""); len(holders) != 0 {
		t.Errorf("the blob's holders are said to be %v", holders)
	}
	if status := off.Status(""); status.GetWanted() != 0 || len(status.GetBlobs()) != 1 {
		t.Errorf("the node reports %v", status)
	}
}

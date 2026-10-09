package replication

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/ipfs/go-cid"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/sisyphus-network/Sisyphus/packages/identity"
	pb "github.com/sisyphus-network/Sisyphus/packages/protocol/sisyphus/v1"
	"github.com/sisyphus-network/Sisyphus/packages/storage"
)

// These tests check that a coordinator signs what it tells followers to
// hold, and takes back from a list only what it signed for.

// listed is one line of a list: a blob, and until when it is kept. The zero
// time is no expiry.
func listed(id string, until time.Time) *pb.KeptBlob {
	blob := &pb.KeptBlob{Cid: id}
	if !until.IsZero() {
		blob.KeepUntil = timestamppb.New(until)
	}
	return blob
}

// signedBy returns a list of a store, signed with a node's key as that node
// would sign it.
func signedBy(ident *identity.Identity, store string, blobs ...*pb.KeptBlob) *pb.KeepList {
	list := &pb.KeepList{Store: store, Sequence: uint64(time.Now().UnixNano()), Blobs: blobs}
	list.Signature = ident.Sign(signedBytes(list))
	return list
}

// show has a follower that keeps lists say what it holds for the store the
// node had before, and show a list, and returns what it is told.
func (r *rig) show(id string, list *pb.KeepList, holding ...string) *pb.ReplicateResponse {
	return r.Replicate(ctx, id, &pb.ReplicateRequest{Store: r.old, Holding: holding, KeepsLists: true, List: list})
}

// restoredPins lists the pins a store holds on what was taken back, as the
// expiry of each by CID.
func restoredPins(store *storage.Store) map[string]time.Time {
	pins := make(map[string]time.Time)
	for _, pin := range store.Pins() {
		if pin.Owner == RestoredOwner {
			pins[pin.CID.String()] = pin.Expires
		}
	}
	return pins
}

// cidsOf names the blobs of a list, in order.
func cidsOf(list *pb.KeepList) []string {
	var ids []string
	for _, blob := range list.GetBlobs() {
		ids = append(ids, blob.GetCid())
	}
	slices.Sort(ids)
	return ids
}

func TestAFollowerThatKeepsListsIsToldWhatToHoldInOneTheNodeSigned(t *testing.T) {
	graceOf(t, time.Minute)
	r := newRig(t, 1)
	day, week := time.Now().Add(24*time.Hour), time.Now().Add(7*24*time.Hour)
	open, twice, lasting := put(t, r.store, "kept until released"), put(t, r.store, "kept by two, for a day and for a week"), put(t, r.store, "kept by two, one of them until released")
	put(t, r.store, "stored, and never pinned")
	for _, pin := range []storage.Pin{
		{CID: cid.MustParse(open), Owner: "user"},
		{CID: cid.MustParse(twice), Owner: "job:1", Expires: week},
		{CID: cid.MustParse(twice), Owner: "user", Expires: day},
		{CID: cid.MustParse(lasting), Owner: "job:1", Expires: day},
		{CID: cid.MustParse(lasting), Owner: "user"},
	} {
		if err := r.store.Pin(ctx, pin.Owner, pin.Expires, pin.CID); err != nil {
			t.Fatal(err)
		}
	}

	told := r.Replicate(ctx, "a", &pb.ReplicateRequest{KeepsLists: true})
	list := told.GetList()
	if len(told.GetHold()) != 0 || told.GetShowList() || told.GetStore() != r.now || list.GetStore() != r.now {
		t.Fatalf("a follower that keeps lists is told %v, want a list for the node's store and nothing beside it", told)
	}
	want := map[string]time.Time{open: {}, twice: week, lasting: {}}
	if len(list.GetBlobs()) != len(want) {
		t.Errorf("the list names %v, want the three pinned blobs", cidsOf(list))
	}
	for _, blob := range list.GetBlobs() {
		until, named := want[blob.GetCid()]
		if !named {
			t.Errorf("the list names %s, which is not pinned", blob.GetCid())
		} else if until.IsZero() != (blob.GetKeepUntil() == nil) || (!until.IsZero() && !blob.GetKeepUntil().AsTime().Equal(until)) {
			t.Errorf("the list keeps %s until %v, want the latest of its pins' expiries, %v", blob.GetCid(), blob.GetKeepUntil(), until)
		}
	}
	// Anyone can check the signature against the node's ID, and the node
	// knows it for its own.
	if valid, err := identity.Verify(r.ident.ID(), signedBytes(list), list.GetSignature()); err != nil || !valid {
		t.Errorf("the list's signature is not the node's: valid %v, error %v", valid, err)
	}
	if !r.signedHere(list) {
		t.Error("the node does not know a list it signed")
	}

	// Each list is later than the last, however soon it is asked for.
	last := list.GetSequence()
	for range 50 {
		next := r.Replicate(ctx, "a", &pb.ReplicateRequest{KeepsLists: true, Store: r.now, Holding: cidsOf(list)}).GetList().GetSequence()
		if next <= last {
			t.Fatalf("a list has sequence %d after one with %d", next, last)
		}
		last = next
	}
	// A follower that does not say it keeps lists is told as before.
	if told := r.Replicate(ctx, "b", &pb.ReplicateRequest{}); told.GetList() != nil || len(told.GetHold()) == 0 {
		t.Errorf("a follower that keeps no lists is told %v", told)
	}
}

func TestAListChangedAfterItWasSignedIsNotTheNodes(t *testing.T) {
	graceOf(t, time.Minute)
	r := newRig(t, 1)
	theirs := storage.NewMemory()
	r.followers["a"] = theirs
	kept, planted := put(t, theirs, "what the lost store kept"), put(t, theirs, "what a follower would like kept")
	lapsed := put(t, theirs, "what the lost store kept, until a while ago")
	soon, past := time.Now().Add(time.Hour), time.Now().Add(-time.Hour)
	genuine := func() *pb.KeepList {
		return signedBy(r.ident, r.old, listed(kept, soon), listed(lapsed, past))
	}
	if !r.signedHere(genuine()) {
		t.Fatal("the node does not know a list signed with its key")
	}

	changes := map[string]func(*pb.KeepList){
		"a blob added":                 func(l *pb.KeepList) { l.Blobs = append(l.Blobs, listed(planted, time.Time{})) },
		"a blob put in place of one":   func(l *pb.KeepList) { l.Blobs[0].Cid = planted },
		"a blob taken off":             func(l *pb.KeepList) { l.Blobs = l.Blobs[:1] },
		"blobs put in another order":   func(l *pb.KeepList) { slices.Reverse(l.Blobs) },
		"an expiry put off":            func(l *pb.KeepList) { l.Blobs[1].KeepUntil = timestamppb.New(soon) },
		"an expiry removed":            func(l *pb.KeepList) { l.Blobs[1].KeepUntil = nil },
		"an expiry at the epoch added": func(l *pb.KeepList) { l.Blobs[0].KeepUntil = timestamppb.New(time.Unix(0, 0)) },
		"another store named":          func(l *pb.KeepList) { l.Store = r.ident.ID() + "/other" },
		"another sequence":             func(l *pb.KeepList) { l.Sequence++ },
		"a signature cut short":        func(l *pb.KeepList) { l.Signature = l.Signature[:32] },
		"no signature":                 func(l *pb.KeepList) { l.Signature = nil },
	}
	for name, change := range changes {
		list := genuine()
		change(list)
		if r.signedHere(list) {
			t.Errorf("a list with %s is taken for one the node signed", name)
		}
		told := r.show("a", list, kept, planted, lapsed)
		if told.GetList() != nil || told.GetShowList() || told.GetStore() != r.old || len(told.GetHold()) != 3 {
			t.Errorf("shown a list with %s the node answers %v, want the follower left holding what it has for the earlier store", name, told)
		}
	}
	if pins := restoredPins(r.store); len(pins) != 0 {
		t.Errorf("lists that were changed after signing brought back %v", pins)
	}
	for _, id := range []string{kept, planted, lapsed} {
		if holds(t, r.store, id) {
			t.Errorf("the node fetched %s on the strength of a list that was changed", id)
		}
	}
	if n := r.logs.count("showed a list of what to hold that this node did not sign"); n != 1 {
		t.Errorf("a follower showing lists the node did not sign was logged %d times, want once", n)
	}
	// What it holds is still there for the node's owner to restore.
	if status := r.Status(""); status.GetFromEarlierStore() != 3 || status.GetUnsignedFromEarlierStore() != 3 || status.GetRestored() != 0 {
		t.Errorf("the node reports %v, want three blobs held from an earlier store with no list of its own to vouch for them", status)
	}
}

func TestAListSignedWithAnotherKeyIsIgnored(t *testing.T) {
	graceOf(t, time.Minute)
	r := newRig(t, 1)
	theirs := storage.NewMemory()
	r.followers["a"] = theirs
	planted := put(t, theirs, "listed by somebody who is not the node")
	forged := signedBy(newIdentity(t), r.old, listed(planted, time.Time{}))

	// Asked without a list, the follower is told to show the one it has.
	// With one that is not the node's it is no further on.
	for range 3 {
		asked := r.Replicate(ctx, "a", &pb.ReplicateRequest{Store: r.old, Holding: []string{planted}, KeepsLists: true})
		if !asked.GetShowList() || asked.GetList() != nil || asked.GetStore() != r.old || !slices.Equal(asked.GetHold(), []string{planted}) {
			t.Fatalf("a follower holding a blob for an earlier store is told %v, want to keep it and show its list", asked)
		}
		told := r.show("a", forged, planted)
		if told.GetShowList() || told.GetList() != nil || told.GetStore() != r.old || !slices.Equal(told.GetHold(), []string{planted}) {
			t.Fatalf("shown a list another node signed, the node answers %v", told)
		}
	}
	if holds(t, r.store, planted) || len(restoredPins(r.store)) != 0 {
		t.Error("the node took back a blob named by a list another node signed")
	}
	if n := r.logs.count("showed a list of what to hold that this node did not sign"); n != 1 {
		t.Errorf("the forged list was logged %d times, want once", n)
	}
	if n := r.logs.count("holds blobs from a store this node had before"); n != 1 {
		t.Errorf("what the follower is left holding was logged %d times, want once", n)
	}
	if status := r.Status(""); status.GetFromEarlierStore() != 1 || status.GetUnsignedFromEarlierStore() != 1 {
		t.Errorf("the node reports %v", status)
	}

	// The owner can still have it fetched, on the follower's word.
	if restored, failed := r.Restore(ctx, "user"); restored != 1 || len(failed) != 0 {
		t.Errorf("restoring by hand took back %d blobs and failed on %v", restored, failed)
	}
	// After which the follower holds for the store the node has.
	if told := r.Replicate(ctx, "a", &pb.ReplicateRequest{Store: r.old, Holding: []string{planted}, KeepsLists: true}); told.GetStore() != r.now || !slices.Equal(cidsOf(told.GetList()), []string{planted}) {
		t.Errorf("after the blob was restored by hand the follower is told %v", told)
	}
}

func TestACoordinatorTakesBackWhatAListItSignedNames(t *testing.T) {
	graceOf(t, time.Minute)
	r := newRig(t, 1)
	theirs := storage.NewMemory()
	r.followers["a"] = theirs
	week := time.Now().Add(7 * 24 * time.Hour)
	open, timed := put(t, theirs, "kept until released"), put(t, theirs, "kept for a week")
	lapsed, unlisted := put(t, theirs, "kept until a minute ago"), put(t, theirs, "held for the earlier store, and on no list")
	const late = "listed, and not on the follower yet"
	missing := put(t, storage.NewMemory(), late)
	list := signedBy(r.ident, r.old, listed(open, time.Time{}), listed(timed, week), listed(lapsed, time.Now().Add(-time.Minute)), listed(missing, time.Time{}), listed("not-a-cid", time.Time{}))
	holding := []string{open, timed, lapsed, unlisted, missing}

	// On the follower's word alone nothing comes back: it is told to keep
	// what it has and to show its list.
	asked := r.Replicate(ctx, "a", &pb.ReplicateRequest{Store: r.old, Holding: holding, KeepsLists: true})
	if !asked.GetShowList() || len(asked.GetHold()) != 5 || len(r.store.Pins()) != 0 {
		t.Fatalf("before any list is shown the node answers %v and has pins %+v", asked, r.store.Pins())
	}
	if r.logs.count("holds blobs from a store this node had before") != 0 {
		t.Error("what a follower is left holding was logged before it had the chance to show its list")
	}

	// Shown the list, the node takes back what it names and can be fetched.
	// What has lapsed the follower drops. What the list does not name is
	// neither taken back nor dropped: it is left for the node's owner.
	told := r.show("a", list, holding...)
	want := []string{open, timed, missing}
	slices.Sort(want)
	got := slices.Clone(told.GetHold())
	slices.Sort(got)
	if all := slices.Sorted(slices.Values(append(slices.Clone(want), unlisted))); !slices.Equal(got, all) || told.GetStore() != r.old || told.GetList() != nil || told.GetShowList() {
		t.Errorf("with one listed blob still to come the follower is told %v, want to hold the two taken back, the one to come and the one on no list, for the earlier store still", told)
	}
	pins := restoredPins(r.store)
	if len(pins) != 2 || !pins[open].IsZero() || !pins[timed].Equal(week) {
		t.Errorf("the node pinned as restored %v, want one blob until released and one for a week", pins)
	}
	for _, pin := range r.store.Pins() {
		if pin.Owner != RestoredOwner && pin.Owner != storage.GraceOwner {
			t.Errorf("taking back left a pin %+v", pin)
		}
	}
	if holds(t, r.store, lapsed) || holds(t, r.store, unlisted) {
		t.Error("the node fetched a blob whose expiry had passed, or one its list does not name")
	}
	if status := r.Status(""); status.GetRestored() != 2 || status.GetFromEarlierStore() != 2 || status.GetUnsignedFromEarlierStore() != 1 || status.GetRestoring() {
		t.Errorf("the node reports %v, want two blobs taken back, one to come that a list of its own names, and one that none does", status)
	}
	if r.logs.count("took back from a storage follower") != 1 || r.logs.count("could not take back everything") != 1 {
		t.Error("what was taken back, and that some could not be, was not logged, once each")
	}

	// Once the follower has the rest, and no longer holds what was on no
	// list, the next ask finishes the job, and it is given a list for the
	// store the node has now.
	put(t, theirs, late)
	told = r.show("a", list, want...)
	if told.GetStore() != r.now || !slices.Equal(cidsOf(told.GetList()), want) || len(told.GetHold()) != 0 {
		t.Errorf("with everything taken back the follower is told %v, want a list of all three for the new store", told)
	}
	for _, blob := range told.GetList().GetBlobs() {
		if blob.GetCid() == timed && !blob.GetKeepUntil().AsTime().Equal(week) {
			t.Errorf("the new list keeps the blob that had a week left until %v", blob.GetKeepUntil().AsTime())
		}
	}
	if r.logs.count("holds blobs from a store this node had before") != 1 {
		t.Error("what the follower was left holding after showing its list was not logged, once")
	}

	// The owner releases one. The old list, shown again, does not bring it
	// back while the node runs.
	if err := r.store.Unpin(RestoredOwner, cid.MustParse(open)); err != nil {
		t.Fatal(err)
	}
	// A pin the node took back that has since run out is no longer counted.
	if err := r.store.Pin(ctx, RestoredOwner, time.Now().Add(-time.Second), cid.MustParse(missing)); err != nil {
		t.Fatal(err)
	}
	r.show("a", list, want...)
	if pins := restoredPins(r.store); len(pins) != 2 || !pins[timed].Equal(week) {
		t.Errorf("after one blob was released and the list shown again the node has restored pins %v", pins)
	}
	if status := r.Status(""); status.GetRestored() != 1 {
		t.Errorf("with one pin released and one run out the node counts %d blobs as taken back, want 1", status.GetRestored())
	}

	// A node started again knows what it took back by its pins: a list that
	// would keep a blob for less long changes nothing.
	again := New(r.config(1))
	shorter := signedBy(r.ident, r.old, listed(timed, time.Now().Add(time.Hour)))
	again.Replicate(ctx, "a", &pb.ReplicateRequest{Store: r.old, Holding: want, KeepsLists: true, List: shorter})
	if pins := restoredPins(r.store); !pins[timed].Equal(week) {
		t.Errorf("a list with an earlier expiry moved a restored pin to %v", pins[timed])
	}
}

func TestOfTwoListsThatNameABlobTheLaterExpiryWins(t *testing.T) {
	graceOf(t, time.Minute)
	r := newRig(t, 2)
	r.followers["a"], r.followers["b"] = storage.NewMemory(), storage.NewMemory()
	texts := []string{"longer on the second list", "until released on the first list", "longer on the first list", "until released on the second list", "on the second list only"}
	var ids []string
	for _, text := range texts {
		ids = append(ids, put(t, r.followers["a"], text))
		put(t, r.followers["b"], text)
	}
	day, week := time.Now().Add(24*time.Hour), time.Now().Add(7*24*time.Hour)
	first := signedBy(r.ident, r.old, listed(ids[0], day), listed(ids[1], time.Time{}), listed(ids[2], week), listed(ids[3], day))
	second := signedBy(r.ident, r.old, listed(ids[0], week), listed(ids[1], day), listed(ids[2], day), listed(ids[3], time.Time{}), listed(ids[4], day))

	r.show("a", first, ids[:4]...)
	r.show("b", second, ids...)
	want := map[string]time.Time{ids[0]: week, ids[1]: {}, ids[2]: week, ids[3]: {}, ids[4]: day}
	pins := restoredPins(r.store)
	if len(pins) != len(want) {
		t.Fatalf("two lists brought back %d blobs, want all %d", len(pins), len(want))
	}
	for i, id := range ids {
		if !pins[id].Equal(want[id]) {
			t.Errorf("the blob %q is kept until %v, want %v", texts[i], pins[id], want[id])
		}
	}
	// Both followers then hold for the new store, each with a list of it.
	for _, id := range []string{"a", "b"} {
		if told := r.show(id, first, ids...); told.GetStore() != r.now || len(told.GetList().GetBlobs()) != 5 {
			t.Errorf("follower %s is told %v, want a list of all five for the new store", id, told)
		}
	}
}

func TestAFollowerThatCannotSupplyWhatItsListNamesIsNotTriedForEveryBlob(t *testing.T) {
	graceOf(t, time.Minute)
	r := newRig(t, 1)
	var ids []*pb.KeptBlob
	var holding []string
	const texts = 6
	for i := range texts {
		id := put(t, storage.NewMemory(), string(rune('a'+i))+" blob the follower says it holds")
		ids, holding = append(ids, listed(id, time.Time{})), append(holding, id)
	}
	list := signedBy(r.ident, r.old, ids...)
	tried := 0
	r.fetching = func() { tried++ }

	// The follower is not connected as a worker yet: nothing is tried, and
	// it is told to keep everything.
	told := r.show("a", list, holding...)
	if len(told.GetHold()) != texts || told.GetStore() != r.old || tried != 0 || r.logs.count("not connected as a worker yet") != 1 {
		t.Errorf("a follower that cannot be reached is told %v, after %d fetches were tried", told, tried)
	}
	// Connected, and without any of them, it is tried for a few and the
	// rest wait for the next time it asks.
	r.followers["a"] = storage.NewMemory()
	told = r.show("a", list, holding...)
	if len(told.GetHold()) != texts || told.GetStore() != r.old {
		t.Errorf("a follower that has none of what its list names is told %v", told)
	}
	if tried != takeBackTries || r.logs.count("could not take back everything") != 2 {
		t.Errorf("%d fetches were tried and failed, want %d, and one line in the log for the lot", tried, takeBackTries)
	}
	if status := r.Status(""); status.GetFromEarlierStore() != texts || status.GetUnsignedFromEarlierStore() != 0 || status.GetRestored() != 0 {
		t.Errorf("the node reports %v", status)
	}
}

func TestANodeSaysWhileItIsTakingAListBack(t *testing.T) {
	graceOf(t, time.Minute)
	r := newRig(t, 1)
	r.followers["a"] = storage.NewMemory()
	id := put(t, r.followers["a"], "fetched while the node is asked how it is doing")
	started, carryOn := make(chan struct{}), make(chan struct{})
	r.fetching = func() {
		close(started)
		<-carryOn
	}
	done := make(chan *pb.ReplicateResponse)
	go func() { done <- r.show("a", signedBy(r.ident, r.old, listed(id, time.Time{})), id) }()

	<-started
	if status := r.Status(""); !status.GetRestoring() || status.GetRestored() != 0 {
		t.Errorf("part way through taking a list back the node reports %v", status)
	}
	close(carryOn)
	if told := <-done; told.GetStore() != r.now {
		t.Errorf("the follower is told %v", told)
	}
	if status := r.Status(""); status.GetRestoring() || status.GetRestored() != 1 {
		t.Errorf("with the list taken back the node reports %v", status)
	}
}

func TestAListOfTheStoreTheNodeHasBringsNothingBack(t *testing.T) {
	graceOf(t, time.Minute)
	r := newRig(t, 1)
	r.followers["a"] = storage.NewMemory()
	pinned := keep(t, r.store, "pinned in the store the node has")
	dropped := put(t, r.followers["a"], "pinned once in this store, and released")
	// The follower was told to let the blob go, kept the list that said so,
	// and stopped before it had done it or taken up the store's new name.
	list := signedBy(r.ident, r.now, listed(pinned, time.Time{}))
	replayed := signedBy(r.ident, r.now, listed(dropped, time.Time{}))
	for _, shown := range []*pb.KeepList{list, replayed} {
		told := r.show("a", shown, pinned, dropped)
		if told.GetStore() != r.now || !slices.Equal(cidsOf(told.GetList()), []string{pinned}) {
			t.Errorf("shown a list of the present store the node answers %v, want only what it has pinned", told)
		}
	}
	if holds(t, r.store, dropped) || len(restoredPins(r.store)) != 0 {
		t.Error("a list of the store the node has now brought a released blob back")
	}
}

func TestCopiesHeldForTheNodeUnderALostKeyAreKeptForItsOwnerToRestore(t *testing.T) {
	graceOf(t, time.Minute)
	r := newRig(t, 1)
	lost := newIdentity(t)
	config := r.config(1)
	config.Formerly = lost.ID()
	m := New(config)
	theirs := storage.NewMemory()
	r.followers["a"] = theirs
	id := put(t, theirs, "kept by the node when it had another key")
	before := lost.ID() + "/old"
	list := signedBy(lost, before, listed(id, time.Time{}))

	asked := m.Replicate(ctx, "a", &pb.ReplicateRequest{Store: before, Holding: []string{id}, KeepsLists: true})
	if !asked.GetShowList() || asked.GetStore() != before || !slices.Equal(asked.GetHold(), []string{id}) {
		t.Fatalf("a follower holding a blob for the node the owner says this one was is told %v", asked)
	}
	// The list is signed with a key the node no longer has. It cannot be
	// told from a forgery, so nothing comes back until the owner says so.
	told := m.Replicate(ctx, "a", &pb.ReplicateRequest{Store: before, Holding: []string{id}, KeepsLists: true, List: list})
	if told.GetStore() != before || !slices.Equal(told.GetHold(), []string{id}) || holds(t, r.store, id) {
		t.Fatalf("shown a list signed with the lost key the node answers %v", told)
	}
	if status := m.Status(""); status.GetUnsignedFromEarlierStore() != 1 || status.GetRestored() != 0 {
		t.Errorf("the node reports %v", status)
	}
	if restored, failed := m.Restore(ctx, "user"); restored != 1 || len(failed) != 0 {
		t.Fatalf("restoring by hand took back %d blobs and failed on %v", restored, failed)
	}
	if !slices.ContainsFunc(r.store.Pins(), func(pin storage.Pin) bool {
		return pin.CID.String() == id && pin.Owner == "user" && pin.Expires.IsZero()
	}) {
		t.Errorf("the blob restored by hand is not pinned for the user until released: %+v", r.store.Pins())
	}

	// A node that was not told it had another key takes the same copies for
	// another pool's, and has them dropped.
	if told := r.Replicate(ctx, "a", &pb.ReplicateRequest{Store: before, Holding: []string{"unpinned"}, KeepsLists: true}); told.GetShowList() || len(told.GetList().GetBlobs()) != 1 {
		t.Errorf("a node with no earlier key is told nothing of one, and answers %v", told)
	}
}

func TestAListIsKeptInAFileAndReplacedWhole(t *testing.T) {
	file := filepath.Join(t.TempDir(), "kept.list")
	if list, err := loadList(file); list != nil || err != nil {
		t.Fatalf("reading a list that was never kept: %v, error %v", list, err)
	}
	ident := newIdentity(t)
	first := signedBy(ident, "node/first", listed("a", time.Time{}), listed("b", time.Now()))
	second := signedBy(ident, "node/first", listed("c", time.Time{}))
	for _, list := range []*pb.KeepList{first, second} {
		if err := saveList(file, list); err != nil {
			t.Fatal(err)
		}
		kept, err := loadList(file)
		if err != nil || !proto.Equal(kept, list) {
			t.Errorf("the list read back is %v, error %v, want the one kept, %v", kept, err, list)
		}
	}
	// Nothing but the list is left beside it, and only its owner reads it.
	entries, err := os.ReadDir(filepath.Dir(file))
	if err != nil || len(entries) != 1 {
		t.Errorf("keeping a list twice left %d files, error %v", len(entries), err)
	}
	if info, err := os.Stat(file); err != nil || info.Mode().Perm()&0o077 != 0 {
		t.Errorf("the list is kept with mode %v, error %v", info.Mode(), err)
	}

	// What cannot be read or kept is an error, not an empty list.
	if err := os.WriteFile(file, []byte("not a list"), 0o600); err != nil {
		t.Fatal(err)
	}
	if list, err := loadList(file); list != nil || err == nil {
		t.Errorf("reading a file that holds no list: %v, error %v", list, err)
	}
	if list, err := loadList(filepath.Dir(file)); list != nil || err == nil {
		t.Errorf("reading a directory as a list: %v, error %v", list, err)
	}
	if err := saveList(filepath.Join(t.TempDir(), "absent", "kept.list"), first); err == nil {
		t.Error("keeping a list in a directory that is not there succeeded")
	}
	inTheWay := filepath.Join(t.TempDir(), "kept.list")
	if err := os.Mkdir(inTheWay, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := saveList(inTheWay, first); err == nil {
		t.Error("keeping a list where a directory is in the way succeeded")
	}
}

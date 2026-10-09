package main

import (
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ipfs/go-cid"
	"google.golang.org/protobuf/proto"

	"github.com/sisyphus-network/Sisyphus/apps/sisyphusd/coordinator"
	"github.com/sisyphus-network/Sisyphus/apps/sisyphusd/replication"
	pb "github.com/sisyphus-network/Sisyphus/packages/protocol/sisyphus/v1"
	"github.com/sisyphus-network/Sisyphus/packages/runtime"
	"github.com/sisyphus-network/Sisyphus/packages/storage"
)

// These tests check that a coordinator which has lost its store takes back
// from its followers what it had signed for, and nothing else.

// comesBack stops a pool's coordinator and returns the pool of the one that
// takes its place: it has an empty store under another name, the key and
// whatever else as says, and the first one's followers, each with the copies
// and the signed list it had.
func (p *pool) comesBack(as replication.Config) *pool {
	p.t.Helper()
	for _, unfollow := range p.unfollow {
		unfollow()
	}
	p.stop()
	as.StoreID = "second"
	q := startPoolAs(p.t, runtime.Builtin(), func(store *storage.Store) coordinator.Store { return store }, nil, as)
	for name, copies := range p.copies {
		q.startMember(name, 1, copies, p.lists[name])
	}
	q.waitForWorkers(len(p.copies))
	return q
}

// keptUntil returns until when a store keeps each blob it has pinned for an
// owner, by CID. The zero time is until released.
func keptUntil(store *storage.Store, owner string) map[string]time.Time {
	until := make(map[string]time.Time)
	for _, pin := range store.Pins() {
		if pin.Owner == owner {
			until[pin.CID.String()] = pin.Expires
		}
	}
	return until
}

// keptList reads the list a follower keeps.
func (p *pool) keptList(name string) *pb.KeepList {
	p.t.Helper()
	data, err := os.ReadFile(p.lists[name])
	list := new(pb.KeepList)
	if err == nil {
		err = proto.Unmarshal(data, list)
	}
	if err != nil {
		p.t.Fatal(err)
	}
	return list
}

func TestACoordinatorThatLostItsStoreTakesBackWhatItSignedForWithoutBeingAsked(t *testing.T) {
	replicateQuickly(t, time.Minute)
	p := startReplicatedPool(t, 2, "a", "b", "c")
	texts := map[string]string{}
	keep := func(text string, pins ...storage.Pin) string {
		t.Helper()
		id := p.upload(text)
		for _, pin := range pins {
			if err := p.store.Pin(p.ctx, pin.Owner, pin.Expires, cid.MustParse(id)); err != nil {
				t.Fatal(err)
			}
		}
		texts[id] = text
		return id
	}
	day, week := time.Now().Add(24*time.Hour), time.Now().Add(7*24*time.Hour)
	open := keep("kept until released", storage.Pin{Owner: "user"})
	timed := keep("kept for a day", storage.Pin{Owner: "user", Expires: day})
	twice := keep("kept by a job for a week and by its owner for a day", storage.Pin{Owner: "job:1", Expires: week}, storage.Pin{Owner: "user", Expires: day})
	released := keep("released before the store was lost", storage.Pin{Owner: "user"})
	p.waitForCopies(2)
	if _, err := p.blobs.Unpin(p.ctx, &pb.UnpinBlobRequest{Cid: released}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return len(p.copiesOf(released)) == 0 })
	// One is kept for a short while only, and the followers stop asking
	// before it has run out: they still hold it, and their lists name it.
	lapses := time.Now().Add(3 * time.Second)
	brief := keep("kept for a moment", storage.Pin{Owner: "job:2", Expires: lapses})
	waitFor(t, func() bool { return len(p.copiesOf(brief)) == 2 || time.Now().After(lapses) })

	// The store is lost. The coordinator that comes back has the same key.
	q := p.comesBack(replication.Config{Identity: p.ident, Replicas: 2})
	time.Sleep(time.Until(lapses))
	want := map[string]time.Time{open: {}, timed: day, twice: week}
	waitFor(t, func() bool {
		got := keptUntil(q.store, replication.RestoredOwner)
		for id := range want {
			if _, back := got[id]; !back {
				return false
			}
		}
		return true
	})
	// Each list names two thirds of what was kept, and between them they
	// name everything. What two lists name comes back once.
	got := keptUntil(q.store, replication.RestoredOwner)
	for id, until := range want {
		if !got[id].Equal(until) {
			t.Errorf("%q is kept until %v, want %v, as the store that was lost had it", texts[id], got[id], until)
		}
		if read := stored(t, q.store, cid.MustParse(id)); string(read) != texts[id] {
			t.Errorf("%q came back as %q", texts[id], read)
		}
	}
	for _, pin := range q.store.Pins() {
		if pin.Owner != replication.RestoredOwner && pin.Owner != storage.GraceOwner {
			t.Errorf("the store has a pin %+v, want everything that came back held as restored", pin)
		}
		if id := pin.CID.String(); id == released || (id == brief && pin.Owner == replication.RestoredOwner && pin.Expires.After(lapses)) {
			t.Errorf("the store has a pin %+v on %q", pin, texts[id])
		}
	}
	if q.holds(released) {
		t.Error("a blob released before the store was lost came back")
	}

	// The followers carry on for the new store, and drop what had lapsed.
	q.waitForCopies(2)
	waitFor(t, func() bool {
		listed := q.replicas()
		return len(q.copiesOf(brief)) == 0 && listed.GetFromEarlierStore() == 0 && !listed.GetRestoring()
	})
	for name, store := range q.copies {
		for _, pin := range store.Pins() {
			if pin.Owner != storage.GraceOwner && pin.Owner != "pool:"+p.ident.ID()+"/second" {
				t.Errorf("follower %s has a pin %+v, want every one held for the coordinator's new store", name, pin)
			}
		}
		if list := q.keptList(name); list.GetStore() != p.ident.ID()+"/second" {
			t.Errorf("follower %s keeps a list for %q, want one for the new store", name, list.GetStore())
		}
	}
	if listed := q.replicas(); listed.GetRestored() != 3 || len(listed.GetBlobs()) != 3 || listed.GetUnsignedFromEarlierStore() != 0 {
		t.Errorf("the coordinator reports %v, want three blobs taken back and copied", listed)
	}
	if !strings.Contains(q.logs.String(), "took back from a storage follower") || strings.Contains(q.logs.String(), "did not sign") {
		t.Errorf("the coordinator logged:\n%s", q.logs.String())
	}

	// Its owner sees what happened, has nothing to run, and can let go of
	// what came back.
	client := q.clientOf()
	if out := mustCLI(t, "blob", "replicas", "--addr", q.addr, "--data-dir", client); !strings.Contains(out, "3 blob(s) are kept that this node took back from its followers after losing its store") || strings.Contains(out, "blob restore") {
		t.Errorf("blob replicas says:\n%s", out)
	}
	if out := mustCLI(t, "blob", "restore", "--addr", q.addr, "--data-dir", client); !strings.HasPrefix(out, "restored 0 blob(s), pinned until unpinned\nfollowers hold nothing from an earlier store") {
		t.Errorf("blob restore printed %q", out)
	}
	mustCLI(t, "blob", "unpin", "--addr", q.addr, "--data-dir", client, open)
	waitFor(t, func() bool { return len(q.copiesOf(open)) == 0 })
	if _, kept := keptUntil(q.store, replication.RestoredOwner)[open]; kept {
		t.Error("blob unpin left the pin on a blob that was taken back")
	}
}

func TestAFollowerThatAddsToItsListBringsNothingBack(t *testing.T) {
	replicateQuickly(t, time.Minute)
	p := startReplicatedPool(t, 1, "a")
	kept := p.keep("what the coordinator chose to keep")
	p.waitForCopies(1)
	p.unfollow["a"]()

	// The follower puts a blob of its own among the copies, and on the list
	// the coordinator signed.
	planted, err := p.copies["a"].Put(p.ctx, strings.NewReader("what a follower would like the pool to keep"))
	if err == nil {
		err = p.copies["a"].Pin(p.ctx, "pool:"+p.ident.ID()+"/first", time.Time{}, planted)
	}
	list := p.keptList("a")
	if len(list.GetBlobs()) != 1 || len(list.GetSignature()) == 0 {
		t.Fatalf("the follower keeps the list %v, want the one blob, signed", list)
	}
	list.Blobs = append(list.Blobs, &pb.KeptBlob{Cid: planted.String()})
	data, _ := proto.Marshal(list)
	if err == nil {
		err = os.WriteFile(p.lists["a"], data, 0o600)
	}
	if err != nil {
		t.Fatal(err)
	}

	q := p.comesBack(replication.Config{Identity: p.ident, Replicas: 1})
	waitFor(t, func() bool {
		return strings.Contains(q.logs.String(), "showed a list of what to hold that this node did not sign")
	})
	// Nothing comes back, not even what the list named before it was added
	// to: the list is the node's or it is not.
	waitFor(t, func() bool { return q.replicas().GetUnsignedFromEarlierStore() == 2 })
	for range 20 {
		if pins := q.store.Pins(); len(pins) != 0 || q.holds(kept) || q.holds(planted.String()) {
			t.Fatalf("a list that was added to brought back pins %+v", pins)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if strings.Contains(q.logs.String(), "took back") {
		t.Error("the coordinator logged that it took something back")
	}
	// The follower is left holding both, for the owner to decide.
	if holders := q.copiesOf(kept); !slices.Equal(holders, []string{"a"}) {
		t.Errorf("the blob the coordinator had kept is now held by %v", holders)
	}
	out := mustCLI(t, "blob", "replicas", "--addr", q.addr, "--data-dir", q.clientOf())
	if !strings.Contains(out, `followers hold 2 blob(s) from a store this node had before that no list signed by this node names`) || !strings.Contains(out, `"sisyphusd blob restore" does`) || strings.Contains(out, "took back") {
		t.Errorf("blob replicas says:\n%s", out)
	}
}

func TestACoordinatorThatLostItsKeyTooRestoresOnlyOnItsOwnersWord(t *testing.T) {
	replicateQuickly(t, time.Minute)
	p := startReplicatedPool(t, 1, "a")
	const text = "kept by a coordinator that then lost its disk, key and all"
	id := p.upload(text)
	if _, err := p.blobs.Pin(p.ctx, &pb.PinBlobRequest{Cid: id, TtlSeconds: 3600}); err != nil {
		t.Fatal(err)
	}
	p.waitForCopies(1)

	// The coordinator that comes back is another node, as far as anyone can
	// check. Its owner has told it the ID it had, so it keeps what the
	// follower holds for that node, and takes none of it back.
	q := p.comesBack(replication.Config{Identity: newIdentity(t), Formerly: p.ident.ID(), Replicas: 1})
	waitFor(t, func() bool {
		return strings.Contains(q.logs.String(), "showed a list of what to hold that this node did not sign")
	})
	waitFor(t, func() bool { return q.replicas().GetUnsignedFromEarlierStore() == 1 })
	if pins := q.store.Pins(); len(pins) != 0 || q.holds(id) {
		t.Fatalf("a coordinator with a new key took back pins %+v on the strength of a list signed with its old one", pins)
	}
	if holders := q.copiesOf(id); !slices.Equal(holders, []string{"a"}) {
		t.Fatalf("the blob is now held by %v, want the follower to have kept it", holders)
	}
	client := q.clientOf()
	if out := mustCLI(t, "blob", "replicas", "--addr", q.addr, "--data-dir", client); !strings.Contains(out, `followers hold 1 blob(s) from a store this node had before that no list signed by this node names`) {
		t.Errorf("blob replicas says:\n%s", out)
	}

	// The command still does what it did: everything back, for the user,
	// until unpinned. Until when the lost store kept it is not believed.
	if out := mustCLI(t, "blob", "restore", "--addr", q.addr, "--data-dir", client); out != "restored 1 blob(s), pinned until unpinned\n" {
		t.Errorf("blob restore printed %q", out)
	}
	if got := stored(t, q.store, cid.MustParse(id)); string(got) != text {
		t.Errorf("the blob came back as %q", got)
	}
	if until, pinned := keptUntil(q.store, "user")[id]; !pinned || !until.IsZero() || len(keptUntil(q.store, replication.RestoredOwner)) != 0 {
		t.Errorf("the restored blob has pins %+v, want one for the user until released", q.store.Pins())
	}
	// The follower then holds it for the new node, with a list that node
	// signed.
	q.waitForCopies(1)
	waitFor(t, func() bool { return q.keptList("a").GetStore() == q.ident.ID()+"/second" })
	if listed := q.replicas(); listed.GetFromEarlierStore() != 0 || listed.GetRestored() != 0 {
		t.Errorf("the coordinator reports %v", listed)
	}
}

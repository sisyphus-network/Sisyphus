package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ipfs/go-cid"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/status"

	"github.com/sisyphus-network/Sisyphus/apps/sisyphusd/access"
	"github.com/sisyphus-network/Sisyphus/apps/sisyphusd/blobclient"
	"github.com/sisyphus-network/Sisyphus/apps/sisyphusd/coordinator"
	"github.com/sisyphus-network/Sisyphus/apps/sisyphusd/replication"
	"github.com/sisyphus-network/Sisyphus/packages/identity"
	pb "github.com/sisyphus-network/Sisyphus/packages/protocol/sisyphus/v1"
	"github.com/sisyphus-network/Sisyphus/packages/runtime"
	"github.com/sisyphus-network/Sisyphus/packages/storage"
)

// These tests check that a pool's stored data is kept on its storage
// followers as well as on its coordinator.

// replicateQuickly makes followers ask often, and a coordinator give up on
// one it has not heard from after the given grace period, for the nodes a
// test goes on to start.
func replicateQuickly(t *testing.T, grace time.Duration) {
	t.Helper()
	interval, usual := replication.Interval, replication.Grace
	replication.Interval, replication.Grace = 20*time.Millisecond, grace
	t.Cleanup(func() { replication.Interval, replication.Grace = interval, usual })
}

// startReplicatedPool starts a pool whose coordinator wants so many copies
// of what it has pinned, and the named storage followers.
func startReplicatedPool(t *testing.T, replicas int, followers ...string) *pool {
	t.Helper()
	p := startPoolKeeping(t, runtime.Builtin(), func(store *storage.Store) coordinator.Store { return store }, nil, replicas)
	for _, name := range followers {
		p.startFollower(name)
	}
	p.waitForWorkers(len(followers))
	return p
}

// keep stores text on the coordinator and pins it there, as "blob put" does.
func (p *pool) keep(text string) string {
	p.t.Helper()
	id := p.upload(text)
	if _, err := p.blobs.Pin(p.ctx, &pb.PinBlobRequest{Cid: id}); err != nil {
		p.t.Fatal(err)
	}
	return id
}

// copiesOf names the followers whose stores hold a blob, in order.
func (p *pool) copiesOf(id string) []string {
	p.t.Helper()
	var holders []string
	for name, store := range p.copies {
		has, err := store.Has(p.ctx, cid.MustParse(id))
		if err != nil {
			p.t.Fatal(err)
		}
		if has {
			holders = append(holders, name)
		}
	}
	slices.Sort(holders)
	return holders
}

// replicas asks the coordinator which followers hold each pinned blob.
func (p *pool) replicas() *pb.ReplicasResponse {
	p.t.Helper()
	listed, err := p.blobs.Replicas(p.ctx, &pb.ReplicasRequest{})
	if err != nil {
		p.t.Fatal(err)
	}
	return listed
}

// waitForCopies waits until the coordinator has heard that every blob it
// has pinned is held by so many followers.
func (p *pool) waitForCopies(n int) {
	p.t.Helper()
	waitFor(p.t, func() bool {
		listed := p.replicas()
		for _, blob := range listed.GetBlobs() {
			if len(blob.GetHolders()) != n {
				return false
			}
		}
		return len(listed.GetBlobs()) > 0
	})
}

func TestPinnedDataEndsUpOnAsManyFollowersAsAsked(t *testing.T) {
	replicateQuickly(t, time.Minute)
	p := startReplicatedPool(t, 2, "a", "b", "c")
	var kept []string
	for i := range 6 {
		kept = append(kept, p.keep(fmt.Sprintf("blob %d of a pool that keeps two copies", i)))
	}
	// A blob nobody has asked to keep is not worth copying.
	loose := p.upload("stored, and left to lapse")
	p.waitForCopies(2)

	shares := make(map[string]int)
	for i, id := range kept {
		holders := p.copiesOf(id)
		if len(holders) != 2 {
			t.Errorf("blob %d is held by %v, want two followers", i, holders)
		}
		for _, name := range holders {
			shares[name]++
			if got := stored(t, p.copies[name], cid.MustParse(id)); string(got) != fmt.Sprintf("blob %d of a pool that keeps two copies", i) {
				t.Errorf("follower %s holds %q as blob %d", name, got, i)
			}
		}
	}
	if len(shares) != 3 {
		t.Errorf("six blobs went to %v, want some to each of three followers", shares)
	}
	if holders := p.copiesOf(loose); len(holders) != 0 {
		t.Errorf("a blob that was never pinned is held by %v", holders)
	}
	// What a follower holds, it holds for the pool and until told otherwise.
	for name, store := range p.copies {
		for _, pin := range store.Pins() {
			if pin.Owner != storage.GraceOwner && (pin.Owner != "pool:"+p.ident.ID()+"/first" || !pin.Expires.IsZero()) {
				t.Errorf("follower %s has a pin %+v, want every one held for the coordinator's store until released", name, pin)
			}
		}
	}

	listed := p.replicas()
	if listed.GetWanted() != 2 || len(listed.GetFollowers()) != 3 || len(listed.GetBlobs()) != 6 || listed.GetSettling() {
		t.Errorf("the coordinator reports %v, want six blobs on two of three followers each", listed)
	}
	one, err := p.blobs.Replicas(p.ctx, &pb.ReplicasRequest{Cid: kept[0]})
	if err != nil || len(one.GetBlobs()) != 1 || one.GetBlobs()[0].GetCid() != kept[0] {
		t.Errorf("asked about one blob, the coordinator reports %v, error %v", one, err)
	}
	if _, err := p.blobs.Replicas(p.ctx, &pb.ReplicasRequest{Cid: "not-a-cid"}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("asked about something that is no CID: %v", err)
	}
}

func TestALostFollowerIsReplacedAfterTheGracePeriod(t *testing.T) {
	replicateQuickly(t, 500*time.Millisecond)
	p := startPoolKeeping(t, runtime.Builtin(), func(store *storage.Store) coordinator.Store { return store }, nil, 2)
	stops := make(map[string]func())
	for _, name := range []string{"a", "b", "c"} {
		stops[name] = p.startFollower(name)
	}
	var kept []string
	for i := range 6 {
		kept = append(kept, p.keep(fmt.Sprintf("blob %d of a pool about to lose a follower", i)))
	}
	p.waitForCopies(2)

	// Lose whichever follower holds the first blob.
	lost := p.copiesOf(kept[0])[0]
	stops[lost]()
	lostAt := time.Now()
	// It is still counted on, but nobody is sent to it for a blob.
	_, asker := p.admit(access.Worker)
	if holders := p.locate(asker, kept[0]); len(holders) != 1 || slices.Contains(holders, p.addresses[lost]) {
		t.Errorf("with one of its two holders gone the first blob is said to be held at %v, want only the other's address", holders)
	}
	if listed := p.replicas(); len(listed.GetFollowers()) != 3 {
		t.Errorf("a follower just lost is already given up on: %d are counted", len(listed.GetFollowers()))
	}

	// The two that remain end up holding everything between them.
	waitFor(t, func() bool {
		for _, id := range kept {
			if holders := p.copiesOf(id); len(slices.DeleteFunc(holders, func(name string) bool { return name == lost })) != 2 {
				return false
			}
		}
		return true
	})
	// The grace period runs from when the follower was last heard from,
	// which is some while before it was stopped: an interval at least, and
	// more on a machine that is busy. Well short of it is too soon.
	if waited := time.Since(lostAt); waited < 250*time.Millisecond {
		t.Errorf("the lost follower's share was handed on after %v, long before the grace period was over", waited)
	}
	p.waitForCopies(2)
	if listed := p.replicas(); len(listed.GetFollowers()) != 2 || slices.Contains(listed.GetFollowers(), p.workerIdents[lost].ID()) {
		t.Errorf("after the grace period the coordinator still counts on %v", listed.GetFollowers())
	}
	if !strings.Contains(p.logs.String(), "a storage follower has not been heard from") {
		t.Error("the coordinator did not log that it gave up on a follower")
	}
}

func TestAReleasedPinIsDroppedByItsFollowers(t *testing.T) {
	replicateQuickly(t, time.Minute)
	p := startReplicatedPool(t, 2, "a", "b")
	released, stays := p.keep("kept for a while"), p.keep("kept for good")
	p.waitForCopies(2)

	if _, err := p.blobs.Unpin(p.ctx, &pb.UnpinBlobRequest{Cid: released}); err != nil {
		t.Fatal(err)
	}
	// The followers stop holding it, and delete it, without waiting for
	// the hour a store gives a new blob.
	waitFor(t, func() bool { return len(p.copiesOf(released)) == 0 })
	if holders := p.copiesOf(stays); len(holders) != 2 {
		t.Errorf("the blob still pinned is now held by %v", holders)
	}
	waitFor(t, func() bool { return len(p.replicas().GetBlobs()) == 1 })
}

// clientOf admits a new node to the pool as a client and returns the data
// directory its commands are run from.
func (p *pool) clientOf() string {
	p.t.Helper()
	dataDir := p.t.TempDir()
	ident, err := loadIdentity(dataDir)
	if err != nil {
		p.t.Fatal(err)
	}
	if err := p.access.Admit(ident.ID(), access.Client, time.Now()); err != nil {
		p.t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dataDir, "known.json"), []byte(`{"`+p.addr+`":"`+p.ident.ID()+`"}`), 0o600); err != nil {
		p.t.Fatal(err)
	}
	return dataDir
}

func TestTooFewFollowersIsReported(t *testing.T) {
	replicateQuickly(t, time.Minute)
	p := startReplicatedPool(t, 3, "only")
	id := p.keep("wanted in three places")
	p.waitForCopies(1)
	client := p.clientOf()

	out := mustCLI(t, "blob", "replicas", "--addr", p.addr, "--data-dir", client)
	for _, want := range []string{
		"1 follower(s) connected; each pinned blob is to be held by 3",
		"that is 2 too few",
		id + "  1/3     " + p.workerIdents["only"].ID(),
		"1 of 1 blob(s) have fewer copies than wanted",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("blob replicas does not say %q:\n%s", want, out)
		}
	}

	// A second follower helps, and is still not enough.
	p.startFollower("second")
	p.waitForCopies(2)
	out = mustCLI(t, "blob", "replicas", "--addr", p.addr, "--data-dir", client, id)
	if !strings.Contains(out, "that is 1 too few") || !strings.Contains(out, "  2/3  ") {
		t.Errorf("with two followers blob replicas says:\n%s", out)
	}
}

func TestAFollowerHoldsOnlyWhatItsCoordinatorAssigns(t *testing.T) {
	replicateQuickly(t, time.Minute)
	p := startReplicatedPool(t, 1, "a", "b")
	var kept []string
	for i := range 6 {
		kept = append(kept, p.keep(fmt.Sprintf("blob %d of a pool that keeps one copy", i)))
	}
	p.waitForCopies(1)
	for i, id := range kept {
		if holders := p.copiesOf(id); len(holders) != 1 {
			t.Errorf("blob %d is held by %v, want exactly the follower it was assigned to", i, holders)
		}
	}

	// A fellow member can read what a follower holds and can do nothing
	// else: there is no call by which to hand it something to keep.
	member, _ := p.admit(access.Worker)
	follower := pb.NewBlobServiceClient(p.dialMember(member, "a"))
	if _, err := blobclient.Upload(p.ctx, follower, strings.NewReader("pushed at a follower")); status.Code(err) != codes.Unimplemented {
		t.Errorf("storing a blob on a follower: %v, want it refused as something a follower does not do", err)
	}
	foreign := cid.MustParse(helloCID)
	if _, err := follower.Pin(p.ctx, &pb.PinBlobRequest{Cid: foreign.String()}); status.Code(err) != codes.Unimplemented {
		t.Errorf("pinning a blob on a follower: %v, want it refused", err)
	}
	if _, err := follower.Replicate(p.ctx, &pb.ReplicateRequest{}); status.Code(err) != codes.Unimplemented {
		t.Errorf("telling a follower what to hold, as its coordinator would: %v, want it refused", err)
	}
	held := p.copiesOf(kept[0])[0]
	var fetched bytes.Buffer
	if err := blobclient.Download(p.ctx, pb.NewBlobServiceClient(p.dialMember(member, held)), cid.MustParse(kept[0]), &fetched); err != nil || fetched.String() != "blob 0 of a pool that keeps one copy" {
		t.Errorf("fetching from a follower a blob it holds for the pool: %q, error %v", fetched.String(), err)
	}

	// Nor does a follower change what is kept by what it says it holds:
	// the coordinator does not take up a blob on a follower's word, and
	// tells it to drop one it finds itself with.
	planted, err := p.copies["a"].Put(p.ctx, strings.NewReader("found its way into a follower's store"))
	if err == nil {
		err = p.copies["a"].Pin(p.ctx, "pool:"+p.ident.ID()+"/first", time.Time{}, planted)
	}
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return !slices.Contains(p.copiesOf(planted.String()), "a") })
	for _, pin := range p.store.Pins() {
		if pin.CID == planted {
			t.Errorf("the coordinator pinned %+v because a follower held it", pin)
		}
	}
	if p.holds(planted.String()) {
		t.Error("the coordinator fetched a blob because a follower held it")
	}

	// And only workers are followers.
	_, client := p.admit(access.Client)
	asClient, err := grpc.NewClient(p.addr, grpc.WithTransportCredentials(client))
	if err != nil {
		t.Fatal(err)
	}
	defer asClient.Close()
	if _, err := pb.NewBlobServiceClient(asClient).Replicate(p.ctx, &pb.ReplicateRequest{}); status.Code(err) != codes.PermissionDenied {
		t.Errorf("a client asking what to hold: %v, want it refused", err)
	}
}

// dialMember connects one member of the pool to the blob service of the
// follower with the given name.
func (p *pool) dialMember(member *identity.Identity, name string) *grpc.ClientConn {
	p.t.Helper()
	conn, err := grpc.NewClient(p.addresses[name], grpc.WithTransportCredentials(credentials.NewTLS(member.ClientTLS(p.workerIdents[name].ID()))))
	if err != nil {
		p.t.Fatal(err)
	}
	p.t.Cleanup(func() { conn.Close() })
	return conn
}

func TestACoordinatorFetchesBackABlobItsStoreHasLost(t *testing.T) {
	replicateQuickly(t, time.Minute)
	p := startReplicatedPool(t, 1, "a")
	const text = "held by a follower when the coordinator lost it"
	id := p.keep(text)
	p.waitForCopies(1)

	// A worker that wants the blob is told the follower has it.
	_, asker := p.admit(access.Worker)
	if holders := p.locate(asker, id); !slices.Equal(holders, []string{p.addresses["a"]}) {
		t.Errorf("the blob is said to be held at %v, want the follower's address %s", holders, p.addresses["a"])
	}

	// The follower hears no more from the coordinator, as if it had not
	// yet asked again, and the coordinator's store loses everything.
	p.unfollow["a"]()
	if err := p.store.Unpin("user", cid.MustParse(id)); err != nil {
		t.Fatal(err)
	}
	if _, err := p.store.GC(p.ctx, afterGrace()); err != nil {
		t.Fatal(err)
	}
	if p.holds(id) {
		t.Fatal("the coordinator's store still holds the blob")
	}

	var fetched bytes.Buffer
	if err := blobclient.Download(p.ctx, p.blobs, cid.MustParse(id), &fetched); err != nil || fetched.String() != text {
		t.Fatalf("fetching the lost blob from the coordinator: %q, error %v", fetched.String(), err)
	}
	if !p.holds(id) {
		t.Error("the coordinator did not keep the blob it fetched back")
	}
	if !strings.Contains(p.logs.String(), "fetched back from a storage follower") {
		t.Error("the coordinator did not log where the blob came from")
	}
	// A blob nobody holds is still not found.
	if _, err := p.blobs.Stat(p.ctx, &pb.StatBlobRequest{Cid: helloCID}); status.Code(err) != codes.NotFound {
		t.Errorf("asking for a blob no node holds: %v", err)
	}
}

// These run real nodes, started as the command line starts them.

func TestACoordinatorThatLostItsStoreTakesItBackFromAFollower(t *testing.T) {
	replicateQuickly(t, time.Minute)
	addr, coordinatorDir, copies := freeAddr(t), t.TempDir(), filepath.Join(t.TempDir(), "copies")
	stop := startDaemon(t, "--listen", addr, "--data-dir", coordinatorDir, "--replicas", "1", "--slots", "1")
	// The follower has no port open: the coordinator reaches it by name.
	startDaemon(t, "--role", "worker", "--coordinator", addr, "--replica-dir", copies, "--name", "follower", "--slots", "1")
	waitForOutput(t, "follower", "nodes", "--addr", addr)

	const text, passing = "the one thing this pool was asked to keep for good", "and the one it was asked to keep for three days"
	id := strings.TrimSpace(mustCLI(t, "blob", "put", "--addr", addr, writeFile(t, text)))
	timed := strings.TrimSpace(mustCLI(t, "blob", "put", "--addr", addr, "--ttl", "72h", writeFile(t, passing)))
	waitFor(t, func() bool { return strings.Count(mustCLI(t, "blob", "replicas", "--addr", addr), "  1/1  ") == 2 })
	if out := mustCLI(t, "blob", "replicas", "--addr", addr); strings.Contains(out, "too few") || strings.Contains(out, "fewer copies") || strings.Contains(out, "took back") {
		t.Errorf("with one copy wanted and one held, blob replicas says:\n%s", out)
	}
	// The pin on the second, as "blob pins" prints it: for the user, until
	// a time three days off.
	until := slices.DeleteFunc(pinLines(t, addr, timed), func(line string) bool { return !strings.HasPrefix(line, "user 2") })
	if len(until) != 1 {
		t.Fatalf("the blob stored for three days has pins %v", pinLines(t, addr, timed))
	}
	// The follower has on its disk, beside the copies, what the coordinator
	// signed.
	if _, err := os.Stat(filepath.Join(copies, "kept.list")); err != nil {
		t.Errorf("the follower keeps no signed list: %v", err)
	}

	// The coordinator comes back with the same key and none of its stored
	// data: the disk it was on has been replaced.
	stop()
	if err := os.RemoveAll(filepath.Join(coordinatorDir, "blobs")); err != nil {
		t.Fatal(err)
	}
	startDaemon(t, "--listen", addr, "--data-dir", coordinatorDir, "--replicas", "1", "--slots", "1")

	// Nobody runs anything. Once the follower has found its way to the
	// restarted node again, which is looked to every ten seconds, the node
	// takes back what it had signed for, each blob until when it was kept.
	waitFor(t, func() bool {
		out, err := cli(t, "blob", "pins", "--addr", addr)
		return err == nil && strings.Count(out, "  restored  ") == 2
	})
	if !has(pinLines(t, addr, id), "restored released") || !has(pinLines(t, addr, timed), strings.Replace(until[0], "user", "restored", 1)) {
		t.Errorf("what came back has pins %v and %v, want one until released and one until when it was kept before, %v", pinLines(t, addr, id), pinLines(t, addr, timed), until)
	}
	for blob, want := range map[string]string{id: text, timed: passing} {
		if out := mustCLI(t, "blob", "get", "--addr", addr, blob); out != want {
			t.Errorf("the blob taken back reads %q, want %q", out, want)
		}
		if lines := pinLines(t, addr, blob); slices.ContainsFunc(lines, func(line string) bool { return strings.HasPrefix(line, "user") }) {
			t.Errorf("the blob taken back has pins %v, want none for the user: who had pinned it was not in the list", lines)
		}
	}
	// The follower then holds them for the new store, as it did for the old.
	var out string
	waitFor(t, func() bool {
		out = mustCLI(t, "blob", "replicas", "--addr", addr)
		return strings.Count(out, "  1/1  ") == 2 && !strings.Contains(out, "from a store this node had before")
	})
	if !strings.Contains(out, `2 blob(s) are kept that this node took back from its followers after losing its store`) {
		t.Errorf("after taking its data back the coordinator reports:\n%s", out)
	}
	// There is nothing left for the command to do, and it says why.
	out = mustCLI(t, "blob", "restore", "--addr", addr)
	if !strings.HasPrefix(out, "restored 0 blob(s), pinned until unpinned\nfollowers hold nothing from an earlier store") || !strings.Contains(out, "takes back by itself what it had signed for") {
		t.Errorf("blob restore with nothing left to restore printed %q", out)
	}

	// What came back is the owner's to let go of.
	mustCLI(t, "blob", "unpin", "--addr", addr, id)
	if lines := pinLines(t, addr, id); has(lines, "restored released") {
		t.Errorf("after blob unpin the blob taken back still has pins %v", lines)
	}
	waitFor(t, func() bool {
		out := mustCLI(t, "blob", "replicas", "--addr", addr)
		return strings.Count(out, "  1/1  ") == 1 && strings.Contains(out, "1 blob(s) are kept that this node took back")
	})
}

func TestACoordinatorWithANewKeyRestoresOnItsOwnersWordWhatAFollowerHoldsForItsOldOne(t *testing.T) {
	replicateQuickly(t, time.Minute)
	addr, lostDir, followerDir, copies := freeAddr(t), t.TempDir(), t.TempDir(), filepath.Join(t.TempDir(), "copies")
	stop := startDaemon(t, "--listen", addr, "--data-dir", lostDir, "--replicas", "1", "--slots", "1")
	stopFollower := startDaemon(t, "--role", "worker", "--coordinator", addr, "--data-dir", followerDir, "--replica-dir", copies, "--name", "follower", "--slots", "1")
	waitForOutput(t, "follower", "nodes", "--addr", addr)
	const text = "kept by a coordinator whose whole disk was then lost"
	id := strings.TrimSpace(mustCLI(t, "blob", "put", "--addr", addr, "--ttl", "72h", writeFile(t, text)))
	waitForOutput(t, "  1/1  ", "blob", "replicas", "--addr", addr)
	was, err := loadIdentity(lostDir)
	if err != nil {
		t.Fatal(err)
	}

	// The coordinator's disk is gone, key and all. Its owner starts another
	// in its place, tells it the ID the first had, and has the follower join
	// it: to the follower it is another node at the address of the one it
	// knew.
	stopFollower()
	stop()
	startDaemon(t, "--listen", addr, "--replicas", "1", "--slots", "1", "--formerly", was.ID())
	mustCLI(t, "pool", "join", "--addr", addr, "--data-dir", followerDir, invite(t, addr, "worker"))
	startDaemon(t, "--role", "worker", "--coordinator", addr, "--data-dir", followerDir, "--replica-dir", copies, "--name", "follower", "--slots", "1")

	// The follower's list is signed with a key the new node does not have,
	// so nothing comes back by itself, and the node says what would.
	out := waitForOutput(t, "that no list signed by this node names", "blob", "replicas", "--addr", addr)
	if !strings.Contains(out, "followers hold 1 blob(s)") || !strings.Contains(out, `"sisyphusd blob restore" does`) || !strings.Contains(out, "nothing is pinned") {
		t.Errorf("a coordinator with a new key reports:\n%s", out)
	}
	if out := mustCLI(t, "blob", "pins", "--addr", addr); !strings.Contains(out, "nothing is pinned") {
		t.Fatalf("the new coordinator already has pins:\n%s", out)
	}

	// On its owner's word it takes back everything the follower holds, for
	// the user and until unpinned: the three days are not believed.
	waitForOutput(t, "restored 1 blob(s), pinned until unpinned\n", "blob", "restore", "--addr", addr)
	if lines := pinLines(t, addr, id); !has(lines, "user released") {
		t.Errorf("the restored blob has pins %v, want one held for the user until released", lines)
	}
	if out := mustCLI(t, "blob", "get", "--addr", addr, id); out != text {
		t.Errorf("the restored blob reads %q", out)
	}
	// The follower then holds it for the new node.
	waitFor(t, func() bool {
		out := mustCLI(t, "blob", "replicas", "--addr", addr)
		return strings.Contains(out, "  1/1  ") && !strings.Contains(out, "from a store this node had before")
	})
}

func TestReplicationFlagsAreChecked(t *testing.T) {
	nameIsDir, nameCannotBeSaved := t.TempDir(), t.TempDir()
	for _, dir := range []string{nameIsDir, nameCannotBeSaved} {
		if err := os.Mkdir(filepath.Join(dir, "blobs"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(filepath.Join(nameIsDir, "blobs", "store.id"), 0o700); err != nil {
		t.Fatal(err)
	}
	// A link to nowhere reads as a file that is not there, and cannot be
	// written through.
	if err := os.Symlink(filepath.Join(nameCannotBeSaved, "absent", "store.id"), filepath.Join(nameCannotBeSaved, "blobs", "store.id")); err != nil {
		t.Fatal(err)
	}
	addr := freeAddr(t)
	startDaemon(t, "--listen", addr)

	tests := []struct {
		args []string
		want string
	}{
		{[]string{"--replicas", "-1"}, "--replicas is a number of copies"},
		{[]string{"--coordinator", addr, "--role", "worker", "--replicas", "2"}, "a worker-only node offers to hold copies with --replica-dir"},
		{[]string{"--replica-dir", t.TempDir()}, "--replica-dir is for worker-only nodes"},
		{[]string{"--formerly", "not-a-node-id"}, `--formerly: node ID "not-a-node-id"`},
		{[]string{"--coordinator", addr, "--role", "worker", "--formerly", newIdentity(t).ID()}, "--formerly is for a node that coordinates a pool"},
		{[]string{"--coordinator", addr, "--role", "worker", "--join", invite(t, addr, "worker"), "--replica-dir", writeFile(t, "not a directory")}, "--replica-dir: open blob store"},
		{[]string{"--listen", freeAddr(t), "--data-dir", nameIsDir}, "read the store's name"},
		{[]string{"--listen", freeAddr(t), "--data-dir", nameCannotBeSaved}, "save the store's name"},
	}
	for _, tt := range tests {
		args := tt.args
		if !slices.Contains(args, "--data-dir") {
			args = append(args, "--data-dir", t.TempDir())
		}
		_, err := cli(t, append([]string{"run", "--discovery", "off"}, args...)...)
		if err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("run %v: error %v, want one containing %q", tt.args, err, tt.want)
		}
	}
}

// reporting is a node that says of its followers whatever a test has it say.
type reporting struct {
	pb.UnimplementedBlobServiceServer
	replicas *pb.ReplicasResponse
	restored *pb.RestoreResponse
}

func (r *reporting) Replicas(context.Context, *pb.ReplicasRequest) (*pb.ReplicasResponse, error) {
	return r.replicas, nil
}

func (r *reporting) Restore(context.Context, *pb.RestoreRequest) (*pb.RestoreResponse, error) {
	return r.restored, nil
}

func TestBlobReplicasPrintsWhatTheNodeReports(t *testing.T) {
	node := new(reporting)
	addr := serveFake(t, func(srv *grpc.Server) { pb.RegisterBlobServiceServer(srv, node) })
	tests := []struct {
		name string
		said *pb.ReplicasResponse
		want string
	}{
		{"a node that keeps no copies", &pb.ReplicasResponse{Blobs: []*pb.ReplicatedBlob{{Cid: helloCID}}},
			"this node asks no followers to hold copies of its data: it was started without --replicas\n"},
		{"one that keeps none, and whose followers hold what it lost and signed for nothing of", &pb.ReplicasResponse{FromEarlierStore: 2, UnsignedFromEarlierStore: 2},
			"followers hold 2 blob(s) from a store this node had before that no list signed by this node names: it does not take those back by itself, and \"sisyphusd blob restore\" does\n" +
				"this node asks no followers to hold copies of its data: it was started without --replicas\n"},
		{"one that is taking back what it lost", &pb.ReplicasResponse{Wanted: 1, Followers: []string{"a"}, Restoring: true, Restored: 4, FromEarlierStore: 7, UnsignedFromEarlierStore: 7},
			"this node is taking back what a follower holds from a store it had before: 4 blob(s) are back so far\n" +
				"followers hold 7 blob(s) from a store this node had before that no list signed by this node names: it does not take those back by itself, and \"sisyphusd blob restore\" does\n" +
				"1 follower(s) connected; each pinned blob is to be held by 1\n" +
				"nothing is pinned\n"},
		{"one that has taken most of it back, and has some to come and some it will not take", &pb.ReplicasResponse{Wanted: 1, Followers: []string{"a"}, Restored: 4, FromEarlierStore: 3, UnsignedFromEarlierStore: 1, Blobs: []*pb.ReplicatedBlob{{Cid: "back", Holders: []string{"a"}}}},
			"4 blob(s) are kept that this node took back from its followers after losing its store: \"sisyphusd blob pins\" lists them as held for \"restored\"\n" +
				"followers hold 2 blob(s) from a store this node had before that it has yet to take back: it tries again each time a follower asks what to hold, and there is nothing to run\n" +
				"followers hold 1 blob(s) from a store this node had before that no list signed by this node names: it does not take those back by itself, and \"sisyphusd blob restore\" does\n" +
				"1 follower(s) connected; each pinned blob is to be held by 1\n" +
				"CID   COPIES  HELD BY\n" +
				"back  1/1     a\n"},
		{"one that has just started, with nothing pinned", &pb.ReplicasResponse{Wanted: 2, Followers: []string{"a", "b"}, Settling: true},
			"2 follower(s) connected; each pinned blob is to be held by 2\n" +
				"the node started a short while ago: followers that hold copies from before are given time to say so before any are handed what it already had\n" +
				"nothing is pinned\n"},
		{"one with a blob nobody holds yet", &pb.ReplicasResponse{Wanted: 1, Followers: []string{"a"}, Blobs: []*pb.ReplicatedBlob{{Cid: "held", Holders: []string{"a"}}, {Cid: "unheld"}}},
			"1 follower(s) connected; each pinned blob is to be held by 1\n" +
				"CID     COPIES  HELD BY\n" +
				"held    1/1     a\n" +
				"unheld  0/1     -\n" +
				"1 of 2 blob(s) have fewer copies than wanted\n"},
		{"one with more copies than it asked for", &pb.ReplicasResponse{Wanted: 1, Followers: []string{"a", "b"}, Blobs: []*pb.ReplicatedBlob{{Cid: "moving", Holders: []string{"a", "b"}}}},
			"2 follower(s) connected; each pinned blob is to be held by 1\n" +
				"CID     COPIES  HELD BY\n" +
				"moving  2/1     a,b\n"},
	}
	for _, tt := range tests {
		node.replicas = tt.said
		if out := mustCLI(t, "blob", "replicas", "--addr", addr); out != tt.want {
			t.Errorf("%s: blob replicas printed\n%swant\n%s", tt.name, out, tt.want)
		}
	}
}

func TestBlobRestoreNamesWhatCouldNotBeFetchedBack(t *testing.T) {
	node := &reporting{restored: &pb.RestoreResponse{Restored: 3, Failed: []string{"first", "second"}}}
	addr := serveFake(t, func(srv *grpc.Server) { pb.RegisterBlobServiceServer(srv, node) })
	out, err := cli(t, "blob", "restore", "--addr", addr)
	if out != "restored 3 blob(s), pinned until unpinned\n" {
		t.Errorf("blob restore printed %q", out)
	}
	if err == nil || !strings.Contains(err.Error(), "2 blob(s) could not be fetched back from any follower: first, second") {
		t.Errorf("blob restore: error %v, want one naming the blobs left behind", err)
	}
}

func TestReplicationCommandUsage(t *testing.T) {
	unable := serveFake(t, func(srv *grpc.Server) { pb.RegisterBlobServiceServer(srv, pinlessNode{}) })
	tests := []struct {
		args []string
		want string
	}{
		{[]string{"blob", "replicas", helloCID, megabyteCID}, "expected at most one CID"},
		{[]string{"blob", "restore", "extra"}, `unexpected argument "extra"`},
		{[]string{"blob", "replicas", "--no-such-flag"}, "flag provided but not defined"},
		{[]string{"blob", "restore", "--no-such-flag"}, "flag provided but not defined"},
		{[]string{"blob", "replicas", "--addr", badAddr}, "invalid control character"},
		{[]string{"blob", "restore", "--addr", badAddr}, "invalid control character"},
		{[]string{"blob", "replicas", "--addr", unable}, "Unimplemented"},
		{[]string{"blob", "restore", "--addr", unable}, "Unimplemented"},
	}
	for _, tt := range tests {
		_, err := cli(t, tt.args...)
		if err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("%v: error %v, want one containing %q", tt.args, err, tt.want)
		}
	}
}

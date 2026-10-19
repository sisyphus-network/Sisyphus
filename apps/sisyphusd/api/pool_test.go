package api

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/ipfs/go-cid"

	"github.com/sisyphus-network/Sisyphus/apps/sisyphusd/access"
	"github.com/sisyphus-network/Sisyphus/apps/sisyphusd/coordinator"
	nodepb "github.com/sisyphus-network/Sisyphus/packages/protocol/sisyphus/node/v1"
	pb "github.com/sisyphus-network/Sisyphus/packages/protocol/sisyphus/v1"
	"github.com/sisyphus-network/Sisyphus/packages/runtime"
	"github.com/sisyphus-network/Sisyphus/packages/storage"
)

// These tests cover what the pool service does when its access list cannot
// be saved, and the details of invitations. The working paths, and who may
// call what, are tested over real connections in the daemon's own tests and
// in the access package.

var errList = errors.New("access list is read-only")

// stuckList is an access list whose changes cannot be saved.
type stuckList struct{ accessList }

func (stuckList) Redeem(string, string, time.Time) (access.Role, error) { return "", errList }

func (stuckList) Remove(string) (bool, error) { return false, errList }

func (stuckList) Revoke(string, access.Role) (bool, error) { return false, errList }

func newList(t *testing.T) *access.List {
	t.Helper()
	l, err := access.Open(access.InMemory(), "owner-id")
	if err != nil {
		t.Fatal(err)
	}
	return l
}

func TestPoolServiceReportsAListThatCannotBeSaved(t *testing.T) {
	service := &poolService{id: "owner-id", access: stuckList{newList(t)}}
	ctx := context.Background()
	if _, err := service.Join(ctx, &pb.JoinRequest{Token: "any"}); status.Code(err) != codes.Internal {
		t.Errorf("Join: %v, want Internal", err)
	}
	if _, err := service.RemoveMember(ctx, &pb.RemoveMemberRequest{NodeId: "any"}); status.Code(err) != codes.Internal {
		t.Errorf("RemoveMember: %v, want Internal", err)
	}
}

func TestInvitationsCarryTheNodesIDAndLastAsLongAsAsked(t *testing.T) {
	list := newList(t)
	service := &poolService{id: "owner-id", access: list}
	ctx := context.Background()

	for _, tt := range []struct {
		ttlSeconds uint64
		want       time.Duration
	}{{0, time.Hour}, {90, 90 * time.Second}} {
		before := time.Now()
		invited, err := service.Invite(ctx, &pb.InviteRequest{Role: pb.Role_ROLE_CLIENT, TtlSeconds: tt.ttlSeconds})
		if err != nil {
			t.Fatal(err)
		}
		if got := invited.GetInvitation(); len(got) != len("owner-id:")+32 || got[:9] != "owner-id:" {
			t.Errorf("invitation %q, want this node's ID, a colon and a token", got)
		}
		expires := invited.GetExpiresAt().AsTime()
		if expires.Before(before.Add(tt.want)) || expires.After(time.Now().Add(tt.want)) {
			t.Errorf("a %d-second invitation expires %v, want %v from now", tt.ttlSeconds, expires, tt.want)
		}
		// The token admits the node that presents it, in the role asked for.
		role, err := list.Redeem(invited.GetInvitation()[9:], "joiner", time.Now())
		if err != nil || role != access.Client {
			t.Errorf("redeeming the invitation: %q, %v", role, err)
		}
	}

	if _, err := service.Invite(ctx, &pb.InviteRequest{}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("an invitation with no role: %v, want InvalidArgument", err)
	}
}

// network is a private IPFS network a node claims to run.
type network struct {
	addresses []string
	err       error
	// rekeyErr makes changing the key fail; rekeyed counts the changes.
	rekeyErr error
	rekeyed  *int
}

func (network) Key() string { return "the-shared-secret" }

func (n network) Addresses(context.Context) ([]string, error) { return n.addresses, n.err }

func (network) Local(context.Context) (string, error) { return "127.0.0.1:4101", nil }

func (n network) Fingerprint() string { return fmt.Sprintf("key-%d", *n.rekeyed) }

func (n network) Rekey(context.Context) error {
	if n.rekeyErr != nil {
		return n.rekeyErr
	}
	*n.rekeyed++
	return nil
}

func TestSwarmTellsMembersHowToJoinThePrivateNetwork(t *testing.T) {
	ctx := context.Background()
	changes := 0
	running := &poolService{swarm: network{addresses: []string{"/ip4/10.0.0.5/tcp/4101/p2p/12D3KooWexample"}, rekeyed: &changes}}
	got, err := running.Swarm(ctx, &pb.SwarmRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if got.GetSwarmKey() != "the-shared-secret" || len(got.GetAddresses()) != 1 || got.GetAddresses()[0] != "/ip4/10.0.0.5/tcp/4101/p2p/12D3KooWexample" {
		t.Errorf("reply %v", got)
	}

	if _, err := (&poolService{}).Swarm(ctx, &pb.SwarmRequest{}); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("a node with no private network: %v, want FailedPrecondition", err)
	}
	broken := &poolService{swarm: network{err: errors.New("kubo went away"), rekeyed: &changes}}
	if _, err := broken.Swarm(ctx, &pb.SwarmRequest{}); status.Code(err) != codes.Internal {
		t.Errorf("a node whose Kubo does not answer: %v, want Internal", err)
	}
}

func newCoordinator(t *testing.T) *coordinator.Coordinator {
	t.Helper()
	c := coordinator.New(coordinator.Config{
		ID: "owner-id", Workloads: runtime.Builtin(), Store: storage.NewMemory(),
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	t.Cleanup(c.Close)
	return c
}

type relaySnapshotStream struct {
	grpc.ServerStream
	ctx     context.Context
	updates chan *pb.RelaysResponse
}

func (s *relaySnapshotStream) Context() context.Context { return s.ctx }
func (s *relaySnapshotStream) Send(update *pb.RelaysResponse) error {
	s.updates <- update
	return nil
}

func TestWatchRelaysSendsInitialSnapshotAndStops(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	stream := &relaySnapshotStream{ctx: ctx, updates: make(chan *pb.RelaysResponse, 1)}
	service := &poolService{coordinator: newCoordinator(t)}
	done := make(chan error, 1)
	go func() {
		done <- service.WatchRelays(&pb.RelaysRequest{}, stream)
	}()
	select {
	case update := <-stream.updates:
		if len(update.GetAddresses()) != 0 {
			t.Fatalf("initial relay list = %v, want empty", update.GetAddresses())
		}
	case <-time.After(eventualWait):
		t.Fatal("WatchRelays did not send its initial snapshot")
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("WatchRelays: %v", err)
		}
	case <-time.After(eventualWait):
		t.Fatal("WatchRelays did not stop after cancellation")
	}
}

func TestRemovingAMemberChangesTheSwarmKey(t *testing.T) {
	ctx := context.Background()
	list := newList(t)
	list.Admit("leaver", access.Worker, time.Now())
	list.Admit("another", access.Worker, time.Now())
	changes := 0
	service := &poolService{id: "owner-id", access: list, coordinator: newCoordinator(t), swarm: network{rekeyed: &changes}}

	if _, err := service.RemoveMember(ctx, &pb.RemoveMemberRequest{NodeId: "leaver"}); err != nil {
		t.Fatal(err)
	}
	if changes != 1 {
		t.Errorf("the key was changed %d times, want once", changes)
	}
	// Removing someone who is not a member changes nothing.
	if _, err := service.RemoveMember(ctx, &pb.RemoveMemberRequest{NodeId: "stranger"}); status.Code(err) != codes.NotFound || changes != 1 {
		t.Errorf("removing a non-member: %v, key changed %d times", err, changes)
	}

	// If the key cannot be changed the caller must be told: the node is off
	// the list but still holds the key.
	stuck := &poolService{id: "owner-id", access: list, coordinator: newCoordinator(t), swarm: network{rekeyed: &changes, rekeyErr: errors.New("kubo would not restart")}}
	_, err := stuck.RemoveMember(ctx, &pb.RemoveMemberRequest{NodeId: "another"})
	if status.Code(err) != codes.Internal || !strings.Contains(err.Error(), "was removed, but it still holds the key") {
		t.Errorf("error %v, want a warning that the key is unchanged", err)
	}
	if _, onList := list.Roles("another"); onList {
		t.Error("the node is still on the list")
	}
}

func TestRekeyOnRequest(t *testing.T) {
	ctx := context.Background()
	changes := 0
	service := &poolService{id: "owner-id", coordinator: newCoordinator(t), swarm: network{rekeyed: &changes}}
	got, err := service.Rekey(ctx, &pb.RekeyRequest{})
	if err != nil || got.GetSwarmFingerprint() != "key-1" {
		t.Errorf("Rekey = %v, %v; want the new key's fingerprint", got, err)
	}

	if _, err := (&poolService{}).Rekey(ctx, &pb.RekeyRequest{}); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("a node with no private network: %v, want FailedPrecondition", err)
	}
	stuck := &poolService{coordinator: newCoordinator(t), swarm: network{rekeyed: &changes, rekeyErr: errors.New("kubo would not restart")}}
	if _, err := stuck.Rekey(ctx, &pb.RekeyRequest{}); status.Code(err) != codes.Internal {
		t.Errorf("a key that cannot be changed: %v, want Internal", err)
	}
}

// The relay list is also given whole for a client that does not watch it,
// and a node that coordinates no pool has none to watch.
func TestRelaysAskedOnceAndWithoutAPool(t *testing.T) {
	service := &poolService{coordinator: newCoordinator(t)}
	if listed, err := service.Relays(context.Background(), &pb.RelaysRequest{}); err != nil || len(listed.GetAddresses()) != 0 {
		t.Errorf("Relays = %v, %v; want an empty list", listed, err)
	}
	stream := &relaySnapshotStream{ctx: context.Background(), updates: make(chan *pb.RelaysResponse, 1)}
	if err := (&poolService{}).WatchRelays(&pb.RelaysRequest{}, stream); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("WatchRelays without a pool: %v, want failed precondition", err)
	}
}

// A member that holds both roles is listed with both, worker first, by the
// pool and by the local API (#474).
func TestAMemberWithBothRolesIsListedWithBoth(t *testing.T) {
	const both = "12D3KooWAMv5mPojCf7tz1PH9F3VCPzqCyc8t2onRa6ihxoPh7TK"
	list := newList(t)
	list.Admit(both, access.Client, time.Now())
	list.Admit(both, access.Worker, time.Now())
	pool := &poolService{id: "owner-id", access: list}

	listed, err := pool.ListMembers(context.Background(), &pb.ListMembersRequest{})
	if err != nil || len(listed.GetMembers()) != 1 {
		t.Fatalf("ListMembers = %v, %v", listed, err)
	}
	if m := listed.GetMembers()[0]; m.GetRole() != pb.Role_ROLE_WORKER || !slices.Equal(m.GetRoles(), []pb.Role{pb.Role_ROLE_WORKER, pb.Role_ROLE_CLIENT}) {
		t.Errorf("the pool lists the member as %v", m)
	}

	local := &localService{cfg: LocalConfig{Pool: &PoolAdmin{service: pool}}}
	members, err := local.ListMembers(context.Background(), &nodepb.ListMembersRequest{})
	if err != nil || len(members.GetMembers()) != 1 {
		t.Fatalf("local ListMembers = %v, %v", members, err)
	}
	if m := members.GetMembers()[0]; m.GetRole() != nodepb.PoolRole_POOL_ROLE_WORKER || !slices.Equal(m.GetRoles(), []nodepb.PoolRole{nodepb.PoolRole_POOL_ROLE_WORKER, nodepb.PoolRole_POOL_ROLE_CLIENT}) {
		t.Errorf("the local API lists the member as %v", m)
	}
}

// unpinFails is a store whose pins cannot be let go.
type unpinFails struct{ *storage.Store }

func (unpinFails) Unpin(string, ...cid.Cid) error { return errDisk }

// Removing a client lets go of what it kept in its own name, and nobody
// else's; if that fails, the node is still removed and the key still
// changed, and the caller is told (#509).
func TestRemovingAClientLetsGoOfWhatItKept(t *testing.T) {
	ctx := context.Background()
	store := storage.NewMemory()
	a, err := store.Put(ctx, strings.NewReader("a"))
	if err != nil {
		t.Fatal(err)
	}
	b, err := store.Put(ctx, strings.NewReader("b"))
	if err != nil {
		t.Fatal(err)
	}
	for _, pin := range []struct {
		owner string
		c     cid.Cid
	}{{clientOwner + "leaver", a}, {clientOwner + "leaver", b}, {clientOwner + "another", a}, {userOwner, b}} {
		if err := store.Pin(ctx, pin.owner, time.Time{}, pin.c); err != nil {
			t.Fatal(err)
		}
	}
	// Who holds pins, apart from the store's own grace pins on uploads.
	owners := func() []string {
		var held []string
		for _, pin := range store.Pins() {
			if pin.Expires.IsZero() {
				held = append(held, pin.Owner)
			}
		}
		slices.Sort(held)
		return slices.Compact(held)
	}
	list := newList(t)
	list.Admit("leaver", access.Client, time.Now())
	list.Admit("another", access.Client, time.Now())
	list.Admit("quiet", access.Client, time.Now())
	changes := 0
	service := &poolService{id: "owner-id", access: list, coordinator: newCoordinator(t), swarm: network{rekeyed: &changes}, pins: store}

	if _, err := service.RemoveMember(ctx, &pb.RemoveMemberRequest{NodeId: "leaver"}); err != nil {
		t.Fatal(err)
	}
	if got := owners(); !slices.Equal(got, []string{userOwner, clientOwner + "another"}) {
		t.Errorf("after the leaver was removed, pins are held for %q", got)
	}
	// One that kept nothing is removed all the same.
	if _, err := service.RemoveMember(ctx, &pb.RemoveMemberRequest{NodeId: "quiet"}); err != nil {
		t.Fatal(err)
	}

	stuck := &poolService{id: "owner-id", access: list, coordinator: newCoordinator(t), swarm: network{rekeyed: &changes}, pins: unpinFails{store}}
	_, err = stuck.RemoveMember(ctx, &pb.RemoveMemberRequest{NodeId: "another"})
	if status.Code(err) != codes.Internal || !strings.Contains(err.Error(), "could not be let go yet") {
		t.Errorf("removing a client whose pins cannot be let go: %v", err)
	}
	if _, onList := list.Roles("another"); onList || changes != 3 {
		t.Errorf("the client is still on the list (%v), or the key was changed %d times, want 3", onList, changes)
	}

	// The next start lets go of what is left for former members, and of
	// nothing a member keeps.
	if err := ReleaseFormerClients(unpinFails{store}, func(string) bool { return false }); !errors.Is(err, errDisk) {
		t.Errorf("letting go with a store that cannot: %v", err)
	}
	if err := ReleaseFormerClients(store, func(id string) bool { return id == "another" }); err != nil || !slices.Equal(owners(), []string{userOwner, clientOwner + "another"}) {
		t.Errorf("letting go of former members' pins with another still a member: %v, held for %q", err, owners())
	}
	if err := ReleaseFormerClients(store, func(string) bool { return false }); err != nil || !slices.Equal(owners(), []string{userOwner}) {
		t.Errorf("letting go of former members' pins: %v, held for %q", err, owners())
	}
	if clientPinsOf(nil) != nil || clientPinsOf(store) == nil {
		t.Error("a missing store has pins, or a store has none")
	}
}

// A member takes itself out of the pool as its owner would: off the list,
// what it kept let go, and the key changed. It can take out no one else,
// and may ask again until it has an answer (#509).
func TestAMemberLeavesThePool(t *testing.T) {
	ctx := context.Background()
	store := storage.NewMemory()
	c, err := store.Put(ctx, strings.NewReader("kept"))
	if err != nil {
		t.Fatal(err)
	}
	keep := func(owner string) {
		t.Helper()
		if err := store.Pin(ctx, clientOwner+owner, time.Time{}, c); err != nil {
			t.Fatal(err)
		}
	}
	kept := func(owner string) bool {
		for _, pin := range store.Pins() {
			if pin.Owner == clientOwner+owner {
				return true
			}
		}
		return false
	}
	keep("leaver")
	list := newList(t)
	list.Admit("leaver", access.Client, time.Now())
	list.Admit("stayer", access.Worker, time.Now())
	changes := 0
	service := &poolService{id: "owner-id", access: list, coordinator: newCoordinator(t), swarm: network{rekeyed: &changes}, pins: store}
	before := caller
	who := "leaver"
	caller = func(context.Context) string { return who }
	t.Cleanup(func() { caller = before })

	// What it kept cannot be let go: it has not left, and may ask again.
	stuck := &poolService{id: "owner-id", access: list, coordinator: newCoordinator(t), swarm: network{rekeyed: &changes}, pins: unpinFails{store}}
	if _, err := stuck.Leave(ctx, &pb.LeaveRequest{}); status.Code(err) != codes.Internal || !strings.Contains(err.Error(), "has not left") {
		t.Errorf("leaving with what it kept stuck: %v", err)
	}
	if _, onList := list.Roles("leaver"); !onList || changes != 0 {
		t.Errorf("after a leave that failed, on the list: %v; key changed %d times", onList, changes)
	}

	if _, err := service.Leave(ctx, &pb.LeaveRequest{}); err != nil {
		t.Fatal(err)
	}
	if _, onList := list.Roles("leaver"); onList || changes != 1 || kept("leaver") {
		t.Errorf("after leaving, on the list: %v; key changed %d times, want once; still kept: %v", onList, changes, kept("leaver"))
	}
	if _, stays := list.Roles("stayer"); !stays {
		t.Error("another member left with it")
	}
	// Asked again, as when the answer was lost, it has left: and anything
	// it kept since is let go, without the key changing again.
	keep("leaver")
	if _, err := service.Leave(ctx, &pb.LeaveRequest{}); err != nil || changes != 1 || kept("leaver") {
		t.Errorf("leaving again: %v; key changed %d times; still kept: %v", err, changes, kept("leaver"))
	}

	// A key that cannot be changed is the pool's to change: the node has
	// left all the same.
	failing := &poolService{id: "owner-id", access: list, coordinator: newCoordinator(t), swarm: network{rekeyed: &changes, rekeyErr: errors.New("kubo would not restart")}, pins: store}
	who = "stayer"
	if _, err := failing.Leave(ctx, &pb.LeaveRequest{}); err != nil {
		t.Errorf("leaving when the key cannot be changed: %v", err)
	}
	if _, onList := list.Roles("stayer"); onList {
		t.Error("a node that left while the key could not be changed is still on the list")
	}
	broken := &poolService{id: "owner-id", access: stuckList{newList(t)}, coordinator: newCoordinator(t)}
	if _, err := broken.Leave(ctx, &pb.LeaveRequest{}); status.Code(err) != codes.Internal {
		t.Errorf("leaving a pool whose list cannot be changed: %v", err)
	}
}

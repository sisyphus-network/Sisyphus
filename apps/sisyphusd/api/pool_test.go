package api

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/excho0/Sisyphus/apps/sisyphusd/access"
	"github.com/excho0/Sisyphus/apps/sisyphusd/coordinator"
	pb "github.com/excho0/Sisyphus/packages/protocol/sisyphus/v1"
	"github.com/excho0/Sisyphus/packages/runtime"
	"github.com/excho0/Sisyphus/packages/storage"
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

func newList(t *testing.T) *access.List {
	t.Helper()
	l, err := access.Open("", "owner-id")
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
	if _, onList := list.Role("another"); onList {
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

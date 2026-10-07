package api

import (
	"context"
	"errors"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/excho0/Sisyphus/apps/sisyphusd/access"
	pb "github.com/excho0/Sisyphus/packages/protocol/sisyphus/v1"
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

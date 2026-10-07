package api

import (
	"context"
	"errors"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/excho0/Sisyphus/apps/sisyphusd/access"
	"github.com/excho0/Sisyphus/apps/sisyphusd/coordinator"
	pb "github.com/excho0/Sisyphus/packages/protocol/sisyphus/v1"
)

// defaultInviteTTL is how long an invitation lasts if its issuer does not say.
const defaultInviteTTL = time.Hour

type poolService struct {
	pb.UnimplementedPoolServiceServer
	// id is this node's own ID, which every invitation carries.
	id          string
	access      accessList
	coordinator *coordinator.Coordinator
}

// accessList is the part of an access.List that the service uses.
type accessList interface {
	Invite(role access.Role, ttl time.Duration, now time.Time) (string, error)
	Redeem(token, id string, now time.Time) (access.Role, error)
	Remove(id string) (bool, error)
	Members() []access.Member
}

var (
	toRole   = map[pb.Role]access.Role{pb.Role_ROLE_WORKER: access.Worker, pb.Role_ROLE_CLIENT: access.Client}
	fromRole = map[access.Role]pb.Role{access.Worker: pb.Role_ROLE_WORKER, access.Client: pb.Role_ROLE_CLIENT}
)

func (s *poolService) Join(ctx context.Context, req *pb.JoinRequest) (*pb.JoinResponse, error) {
	role, err := s.access.Redeem(req.GetToken(), access.Caller(ctx), time.Now())
	if errors.Is(err, access.ErrBadInvite) {
		return nil, status.Error(codes.PermissionDenied, "invitation is unknown, already used or expired")
	}
	if err != nil {
		return nil, status.Errorf(codes.Internal, "admit node: %v", err)
	}
	return &pb.JoinResponse{Role: fromRole[role]}, nil
}

func (s *poolService) Invite(_ context.Context, req *pb.InviteRequest) (*pb.InviteResponse, error) {
	ttl := defaultInviteTTL
	if req.GetTtlSeconds() > 0 {
		ttl = time.Duration(req.GetTtlSeconds()) * time.Second
	}
	now := time.Now()
	token, err := s.access.Invite(toRole[req.GetRole()], ttl, now)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	return &pb.InviteResponse{Invitation: s.id + ":" + token, ExpiresAt: timestamppb.New(now.Add(ttl))}, nil
}

func (s *poolService) ListMembers(context.Context, *pb.ListMembersRequest) (*pb.ListMembersResponse, error) {
	var res pb.ListMembersResponse
	for _, m := range s.access.Members() {
		res.Members = append(res.Members, &pb.Member{NodeId: m.ID, Role: fromRole[m.Role], JoinedAt: timestamppb.New(m.Joined)})
	}
	return &res, nil
}

func (s *poolService) RemoveMember(_ context.Context, req *pb.RemoveMemberRequest) (*pb.RemoveMemberResponse, error) {
	removed, err := s.access.Remove(req.GetNodeId())
	if err != nil {
		return nil, status.Errorf(codes.Internal, "remove node: %v", err)
	}
	if !removed {
		return nil, status.Errorf(codes.NotFound, "node %s is not a member", req.GetNodeId())
	}
	// Being off the list stops it connecting again; this ends the
	// connection it has now.
	s.coordinator.Remove(req.GetNodeId())
	return &pb.RemoveMemberResponse{}, nil
}

package api

import (
	"context"
	"errors"
	"github.com/libp2p/go-libp2p/core/peer"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/sisyphus-network/Sisyphus/apps/sisyphusd/access"
	"github.com/sisyphus-network/Sisyphus/apps/sisyphusd/coordinator"
	pb "github.com/sisyphus-network/Sisyphus/packages/protocol/sisyphus/v1"
)

// defaultInviteTTL is how long an invitation lasts if its issuer does not say.
const defaultInviteTTL = time.Hour

type poolService struct {
	pb.UnimplementedPoolServiceServer
	// id is this node's own ID, which every invitation carries.
	id          string
	access      accessList
	coordinator *coordinator.Coordinator
	// swarm is the pool's private IPFS network, or nil if this node does
	// not run one.
	swarm Swarm
	// cluster is the pool's IPFS Cluster, or nil if this node runs no peer
	// of it.
	cluster Cluster
	// workFor is the list of nodes this one takes work from, or nil if it
	// keeps none.
	workFor WorkFor
}

// Swarm is a private IPFS network this node is on.
type Swarm interface {
	// Key returns the secret its members share.
	Key() string
	// Addresses returns where the network's daemons can be reached by a
	// node joining it.
	Addresses(ctx context.Context) ([]string, error)
	// Local returns where this node's own daemon accepts members, as a host
	// and port that only this node need be able to reach.
	Local(ctx context.Context) (string, error)
	// Fingerprint identifies the current key without revealing it.
	Fingerprint() string
	// Rekey replaces the key with a new one and moves this node's daemon
	// onto it.
	Rekey(ctx context.Context) error
}

// Cluster is an IPFS Cluster this node runs a peer of.
type Cluster interface {
	// Local returns where this node's own peer accepts the other members,
	// as a host and port that only this node need be able to reach.
	Local(ctx context.Context) (string, error)
	// Status says which members the cluster hears from and which of them
	// hold each of the pool's pins.
	Status(ctx context.Context) (*pb.ClusterStatusResponse, error)
}

// noCluster is what a node without a cluster peer answers when asked about
// one.
const noCluster = "this node does not run an IPFS Cluster peer; start it with --kubo --cluster"

// accessList is the part of an access.List that the service uses.
type accessList interface {
	Invite(role access.Role, ttl time.Duration, now time.Time) (string, error)
	Redeem(token, id string, now time.Time) (access.Role, error)
	Remove(id string) (bool, error)
	Members() []access.Member
	Role(id string) (access.Role, bool)
	Admit(id string, role access.Role, now time.Time) error
}

var (
	toRole   = map[pb.Role]access.Role{pb.Role_ROLE_WORKER: access.Worker, pb.Role_ROLE_CLIENT: access.Client}
	fromRole = map[access.Role]pb.Role{access.Worker: pb.Role_ROLE_WORKER, access.Client: pb.Role_ROLE_CLIENT}
)

func (s *poolService) Join(ctx context.Context, req *pb.JoinRequest) (*pb.JoinResponse, error) {
	if req.GetToken() == "" {
		// A node can be admitted without an invitation, by this node's
		// owner trusting it directly. It joins by asking whether it has been.
		role, admitted := s.access.Role(access.Caller(ctx))
		if !admitted || role == access.Owner {
			return nil, status.Error(codes.PermissionDenied, "this node has not been trusted by the node it is joining; ask its owner to trust it, or for an invitation")
		}
		return &pb.JoinResponse{Role: fromRole[role]}, nil
	}
	role, err := s.access.Redeem(req.GetToken(), access.Caller(ctx), time.Now())
	if errors.Is(err, access.ErrBadInvite) {
		return nil, status.Error(codes.PermissionDenied, "invitation is unknown, already used or expired")
	}
	if err != nil {
		return nil, status.Errorf(codes.Internal, "admit node: %v", err)
	}
	return &pb.JoinResponse{Role: fromRole[role]}, nil
}

func (s *poolService) Relays(context.Context, *pb.RelaysRequest) (*pb.RelaysResponse, error) {
	return &pb.RelaysResponse{Addresses: s.coordinator.Relays()}, nil
}

func (s *poolService) IsMember(_ context.Context, req *pb.IsMemberRequest) (*pb.IsMemberResponse, error) {
	_, member := s.access.Role(req.GetNodeId())
	return &pb.IsMemberResponse{Member: member}, nil
}

func (s *poolService) Swarm(ctx context.Context, _ *pb.SwarmRequest) (*pb.SwarmResponse, error) {
	if s.swarm == nil {
		return nil, status.Error(codes.FailedPrecondition, "this node does not run a private IPFS network; start it with --kubo")
	}
	addresses, err := s.swarm.Addresses(ctx)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "ask Kubo for its addresses: %v", err)
	}
	return &pb.SwarmResponse{SwarmKey: s.swarm.Key(), Addresses: addresses, Cluster: s.cluster != nil}, nil
}

func (s *poolService) ClusterStatus(ctx context.Context, _ *pb.ClusterStatusRequest) (*pb.ClusterStatusResponse, error) {
	if s.cluster == nil {
		return nil, status.Error(codes.FailedPrecondition, noCluster)
	}
	state, err := s.cluster.Status(ctx)
	if err != nil {
		return nil, status.Errorf(codes.Unavailable, "ask the cluster peer: %v", err)
	}
	return state, nil
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
	if s.workFor != nil {
		res.WorksFor = s.workFor.List()
	}
	return &res, nil
}

func (s *poolService) SetWorkFor(_ context.Context, req *pb.SetWorkForRequest) (*pb.SetWorkForResponse, error) {
	if s.workFor == nil {
		return nil, status.Error(codes.FailedPrecondition, "this node runs no worker, so it takes no work")
	}
	if _, err := peer.Decode(req.GetNodeId()); err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "%q is not a node ID", req.GetNodeId())
	}
	if err := s.workFor.Set(req.GetNodeId(), req.GetWilling()); err != nil {
		return nil, status.Errorf(codes.Internal, "%v", err)
	}
	return &pb.SetWorkForResponse{}, nil
}

func (s *poolService) RemoveMember(ctx context.Context, req *pb.RemoveMemberRequest) (*pb.RemoveMemberResponse, error) {
	if err := s.removeMember(ctx, req.GetNodeId()); err != nil {
		return nil, err
	}
	return &pb.RemoveMemberResponse{}, nil
}

// removeMember takes a node out of the pool in every respect: off the list,
// disconnected, and without a working key to the pool's private network.
func (s *poolService) removeMember(ctx context.Context, nodeID string) error {
	removed, err := s.access.Remove(nodeID)
	if err != nil {
		return status.Errorf(codes.Internal, "remove node: %v", err)
	}
	if !removed {
		return status.Errorf(codes.NotFound, "node %s is not a member", nodeID)
	}
	// Being off the list stops it connecting again; this ends the
	// connection it has now.
	s.coordinator.Remove(nodeID)
	// It still holds the key to the pool's private network, so that changes
	// too.
	if s.swarm != nil {
		if err := s.rekey(ctx); err != nil {
			return status.Errorf(codes.Internal, "node %s was removed, but it still holds the key to the pool's private IPFS network, which could not be changed: %v", nodeID, err)
		}
	}
	return nil
}

func (s *poolService) Rekey(ctx context.Context, _ *pb.RekeyRequest) (*pb.RekeyResponse, error) {
	if s.swarm == nil {
		return nil, status.Error(codes.FailedPrecondition, "this node does not run a private IPFS network; start it with --kubo")
	}
	if err := s.rekey(ctx); err != nil {
		return nil, status.Errorf(codes.Internal, "change the swarm key: %v", err)
	}
	return &pb.RekeyResponse{SwarmFingerprint: s.swarm.Fingerprint()}, nil
}

// rekey changes the key of the pool's private network and tells the
// connected workers, who then ask for the new one.
func (s *poolService) rekey(ctx context.Context) error {
	if err := s.swarm.Rekey(ctx); err != nil {
		return err
	}
	s.coordinator.AnnounceSwarm(s.swarm.Fingerprint())
	return nil
}

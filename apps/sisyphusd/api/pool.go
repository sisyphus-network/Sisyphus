package api

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/ipfs/go-cid"
	"github.com/libp2p/go-libp2p/core/peer"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/sisyphus-network/Sisyphus/apps/sisyphusd/access"
	"github.com/sisyphus-network/Sisyphus/apps/sisyphusd/coordinator"
	pb "github.com/sisyphus-network/Sisyphus/packages/protocol/sisyphus/v1"
	"github.com/sisyphus-network/Sisyphus/packages/storage"
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
	// pins is what the pool's store keeps for its clients, or nil if this
	// node keeps no store.
	pins ClientPins
}

// ClientPins is the part of a store that keeps pins for a pool's clients.
// A *storage.Store is one.
type ClientPins interface {
	Pins() []storage.Pin
	Unpin(owner string, cids ...cid.Cid) error
}

// clientPinsOf is a store's pins, or nil for no store.
func clientPinsOf(store *storage.Store) ClientPins {
	if store == nil {
		return nil
	}
	return store
}

// releaseClient lets go of everything a store keeps for a client in its own
// name: what it pinned counts against its share, and once it is no longer
// a member nobody is left who could release it (#509).
func releaseClient(store ClientPins, client string) error {
	owner := clientOwner + client
	var held []cid.Cid
	for _, pin := range store.Pins() {
		if pin.Owner == owner {
			held = append(held, pin.CID)
		}
	}
	if len(held) == 0 {
		return nil
	}
	return store.Unpin(owner, held...)
}

// ReleaseFormerClients lets go of what a store keeps for each client that
// is no longer a member: one removed while its release failed, or before
// removal released anything at all (#509).
func ReleaseFormerClients(store ClientPins, member func(id string) bool) error {
	var gone []string
	for _, pin := range store.Pins() {
		if client, ok := strings.CutPrefix(pin.Owner, clientOwner); ok && !member(client) && !slices.Contains(gone, client) {
			gone = append(gone, client)
		}
	}
	var errs []error
	for _, client := range gone {
		errs = append(errs, releaseClient(store, client))
	}
	return errors.Join(errs...)
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
	Roles(id string) ([]access.Role, bool)
	Has(id string, role access.Role) bool
	Admit(id string, role access.Role, now time.Time) error
	Revoke(id string, role access.Role) (bool, error)
}

var (
	toRole   = map[pb.Role]access.Role{pb.Role_ROLE_WORKER: access.Worker, pb.Role_ROLE_CLIENT: access.Client}
	fromRole = map[access.Role]pb.Role{access.Worker: pb.Role_ROLE_WORKER, access.Client: pb.Role_ROLE_CLIENT}
)

func (s *poolService) Join(ctx context.Context, req *pb.JoinRequest) (*pb.JoinResponse, error) {
	if req.GetToken() == "" {
		// A node can be admitted without an invitation, by this node's
		// owner trusting it directly. It joins by asking whether it has been.
		roles, admitted := s.access.Roles(access.Caller(ctx))
		if !admitted || slices.Contains(roles, access.Owner) {
			return nil, status.Error(codes.PermissionDenied, "this node has not been trusted by the node it is joining; ask its owner to trust it, or for an invitation")
		}
		return &pb.JoinResponse{Role: fromRole[roles[0]]}, nil
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

func (s *poolService) WatchRelays(_ *pb.RelaysRequest, stream grpc.ServerStreamingServer[pb.RelaysResponse]) error {
	if s.coordinator == nil {
		return status.Error(codes.FailedPrecondition, "this node does not coordinate a pool")
	}
	return s.coordinator.WatchRelays(stream.Context(), func(addresses []string) error {
		return stream.Send(&pb.RelaysResponse{Addresses: addresses})
	})
}

func (s *poolService) IsMember(_ context.Context, req *pb.IsMemberRequest) (*pb.IsMemberResponse, error) {
	_, member := s.access.Roles(req.GetNodeId())
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

func (s *poolService) ListMembers(ctx context.Context, _ *pb.ListMembersRequest) (*pb.ListMembersResponse, error) {
	// What each client keeps, on a node that coordinates and so keeps
	// anything for clients.
	var stored map[string]uint64
	if s.coordinator != nil {
		stored = s.coordinator.Stored()
	}
	var res pb.ListMembersResponse
	for _, m := range s.access.Members() {
		member := &pb.Member{NodeId: m.ID, Role: fromRole[m.Roles[0]], JoinedAt: timestamppb.New(m.Joined), StoredBytes: stored[m.ID]}
		for _, role := range m.Roles {
			member.Roles = append(member.Roles, fromRole[role])
		}
		res.Members = append(res.Members, member)
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

// Leave takes the caller out of the pool, as RemoveMember would take it out:
// a member that is done with the pool goes without asking the owner, and
// what it kept here as a client is let go with it (#509).
//
// It can be asked again until it has answered: what the caller kept goes
// first, while it is still a member, and a caller that is no longer one is
// told it has left. An error therefore means the caller has not been taken
// off the list, and may ask again.
func (s *poolService) Leave(ctx context.Context, _ *pb.LeaveRequest) (*pb.LeaveResponse, error) {
	id := caller(ctx)
	if s.pins != nil {
		if err := releaseClient(s.pins, id); err != nil {
			return nil, status.Errorf(codes.Internal, "what this node kept here could not be let go, so it has not left; ask again: %v", err)
		}
	}
	removed, err := s.access.Remove(id)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "remove node: %v", err)
	}
	if !removed {
		return &pb.LeaveResponse{}, nil
	}
	s.coordinator.Remove(id)
	// The node has left. The key it holds to the pool's private network is
	// the pool's to change, not its to wait for: a change that fails is
	// said here, for the pool's owner, and can be made with pool rekey.
	if s.swarm != nil {
		if err := s.rekey(ctx); err != nil {
			slog.Warn("a node left the pool, but the key to the pool's private IPFS network could not be changed; run pool rekey", "node", id, "error", err)
		}
	}
	return &pb.LeaveResponse{}, nil
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
	// What it kept here as a client is let go, so that it no longer counts.
	// If that fails, it is let go the next time the node starts.
	var released error
	if s.pins != nil {
		released = releaseClient(s.pins, nodeID)
	}
	// It still holds the key to the pool's private network, so that changes
	// too.
	if s.swarm != nil {
		if err := s.rekey(ctx); err != nil {
			return status.Errorf(codes.Internal, "node %s was removed, but it still holds the key to the pool's private IPFS network, which could not be changed: %v", nodeID, err)
		}
	}
	if released != nil {
		return status.Errorf(codes.Internal, "node %s was removed, but what it kept here could not be let go yet; it will be when this node next starts: %v", nodeID, released)
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

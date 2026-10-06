package api

import (
	"context"
	"crypto/subtle"
	"time"

	"github.com/libp2p/go-libp2p/core/peer"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	"github.com/excho0/Sisyphus/apps/sisyphusd/access"
	nodepb "github.com/excho0/Sisyphus/packages/protocol/sisyphus/node/v1"
)

// The local API is how a desktop client on the same machine watches and
// steers its node. It is the sisyphus.node.v1 contract the desktop client
// was written against, served here in terms of pools: a node's peers are the
// other nodes of its pool, and trusting a peer for compute admits it to the
// pool as a worker.
//
// It listens on loopback without TLS, as that client expects. Anything on
// the machine can therefore read from it. Calls that change something need
// the token the node keeps in its data directory, which only the node's own
// user can read.

// LocalConfig describes the node a local server speaks for.
type LocalConfig struct {
	// NodeID and Version identify the node and its software.
	NodeID  string
	Version string
	// Listen returns the addresses other nodes reach this one at, as
	// multiaddresses.
	Listen func() []string
	// Peers returns the other nodes of this node's pool, in a stable order.
	Peers func() []*nodepb.Peer
	// Pool, if set, lets peers be admitted to and removed from the pool. A
	// node that does not coordinate a pool has none.
	Pool *PoolAdmin
	// Token is what a caller must present to change anything.
	Token string
	// Poll is how often a peer watcher is checked for news. Zero means once
	// a second.
	Poll time.Duration
}

// PoolAdmin admits nodes to a coordinator's pool and removes them, exactly
// as the pool commands do.
type PoolAdmin struct {
	service *poolService
}

// NewPoolAdmin returns the means to change the membership of cfg's pool.
func NewPoolAdmin(cfg Config) *PoolAdmin {
	return &PoolAdmin{service: &poolService{
		id: cfg.Identity.ID(), access: cfg.Access, coordinator: cfg.Coordinator, swarm: cfg.Swarm,
	}}
}

// NewLocalServer returns a gRPC server for the local API.
func NewLocalServer(cfg LocalConfig) *grpc.Server {
	if cfg.Poll == 0 {
		cfg.Poll = time.Second
	}
	srv := grpc.NewServer()
	nodepb.RegisterNodeServiceServer(srv, &localService{cfg: cfg})
	return srv
}

type localService struct {
	nodepb.UnimplementedNodeServiceServer
	cfg LocalConfig
}

func (s *localService) GetNodeInfo(context.Context, *nodepb.GetNodeInfoRequest) (*nodepb.GetNodeInfoResponse, error) {
	// The country is left empty. Finding it means telling an outside service
	// this node's address, which a node should not do unasked.
	return &nodepb.GetNodeInfoResponse{PeerId: s.cfg.NodeID, DaemonVersion: s.cfg.Version, ListenAddresses: s.cfg.Listen()}, nil
}

func (s *localService) ListPeers(context.Context, *nodepb.ListPeersRequest) (*nodepb.ListPeersResponse, error) {
	return &nodepb.ListPeersResponse{Peers: s.cfg.Peers(), Revision: 1}, nil
}

// WatchPeers sends the peers as they stand and again, in full, whenever
// they change, with a revision that counts up from one.
func (s *localService) WatchPeers(_ *nodepb.WatchPeersRequest, stream grpc.ServerStreamingServer[nodepb.ListPeersResponse]) error {
	last := &nodepb.ListPeersResponse{Peers: s.cfg.Peers(), Revision: 1}
	if err := stream.Send(last); err != nil {
		return err
	}
	poll := time.NewTicker(s.cfg.Poll)
	defer poll.Stop()
	for {
		select {
		case <-poll.C:
			now := &nodepb.ListPeersResponse{Peers: s.cfg.Peers(), Revision: last.GetRevision()}
			if proto.Equal(now, last) {
				continue
			}
			now.Revision++
			if err := stream.Send(now); err != nil {
				return err
			}
			last = now
		case <-stream.Context().Done():
			return nil
		}
	}
}

// A pool is joined by invitation, not by dialling an address, and a node
// finds its pool's members through its coordinator, so the address-book
// calls have nothing to act on here.
const noAddressBook = "this node joins a pool by invitation and has no address book: use `sisyphusd pool invite` and `sisyphusd pool join`"

func (s *localService) GetBootstrapPeers(context.Context, *nodepb.GetBootstrapPeersRequest) (*nodepb.GetBootstrapPeersResponse, error) {
	return &nodepb.GetBootstrapPeersResponse{}, nil
}

func (s *localService) SetBootstrapPeers(context.Context, *nodepb.SetBootstrapPeersRequest) (*nodepb.SetBootstrapPeersResponse, error) {
	return nil, status.Error(codes.Unimplemented, noAddressBook)
}

func (s *localService) ConnectPeer(context.Context, *nodepb.ConnectPeerRequest) (*nodepb.ConnectPeerResponse, error) {
	return nil, status.Error(codes.Unimplemented, noAddressBook)
}

// SetPeerComputeTrust admits a node to the pool as a worker, or removes it.
func (s *localService) SetPeerComputeTrust(ctx context.Context, req *nodepb.SetPeerComputeTrustRequest) (*nodepb.SetPeerComputeTrustResponse, error) {
	if err := s.authorize(ctx); err != nil {
		return nil, err
	}
	if s.cfg.Pool == nil {
		return nil, status.Error(codes.FailedPrecondition, "only the node that coordinates a pool can change who is trusted in it")
	}
	if _, err := peer.Decode(req.GetPeerId()); err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "%q is not a node ID", req.GetPeerId())
	}
	pool := s.cfg.Pool.service
	if req.GetTrusted() {
		if err := pool.access.Admit(req.GetPeerId(), access.Worker, time.Now()); err != nil {
			return nil, status.Errorf(codes.Internal, "admit node: %v", err)
		}
	} else if err := pool.removeMember(ctx, req.GetPeerId()); err != nil {
		return nil, err
	}
	return &nodepb.SetPeerComputeTrustResponse{Trusted: req.GetTrusted()}, nil
}

// authorize checks that the caller has presented the node's token, as
// "authorization: Bearer <token>".
func (s *localService) authorize(ctx context.Context) error {
	md, _ := metadata.FromIncomingContext(ctx)
	for _, presented := range md.Get("authorization") {
		if subtle.ConstantTimeCompare([]byte(presented), []byte("Bearer "+s.cfg.Token)) == 1 {
			return nil
		}
	}
	return status.Error(codes.PermissionDenied, "changing this node needs its API token, which is in api.token in the node's data directory")
}

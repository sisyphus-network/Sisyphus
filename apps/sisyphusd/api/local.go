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

	"github.com/sisyphus-network/Sisyphus/apps/sisyphusd/access"
	"github.com/sisyphus-network/Sisyphus/packages/nodedb"
	nodepb "github.com/sisyphus-network/Sisyphus/packages/protocol/sisyphus/node/v1"
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
	// Connect, if set, connects this node's libp2p host to the node at a
	// multiaddress ending in /p2p/ and its ID.
	Connect func(ctx context.Context, address string) error
	// Book, if set, is the node's address book, and Bootstrap connects the
	// node to the addresses in it.
	Book      AddressBook
	Bootstrap func(ctx context.Context, addresses []string)
	// Country returns the two-letter code of the country the node is in,
	// if it has been allowed to find out, and otherwise nothing.
	Country func() string
	// Token is what a caller must present to change anything.
	Token string
	// Poll is how often a peer watcher is checked for news. Zero means once
	// a second.
	Poll time.Duration
}

// AddressBook is where a node keeps the addresses it starts from. A
// nodedb.DB is one.
type AddressBook interface {
	BootstrapPeers() ([]nodedb.BootstrapPeer, error)
	SetBootstrapPeers([]nodedb.BootstrapPeer) error
	AddBootstrapPeer(nodedb.BootstrapPeer) error
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
	if cfg.Country == nil {
		cfg.Country = func() string { return "" }
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
	return &nodepb.GetNodeInfoResponse{
		PeerId: s.cfg.NodeID, DaemonVersion: s.cfg.Version, ListenAddresses: s.cfg.Listen(), CountryCode: s.cfg.Country(),
	}, nil
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

const noAddressBook = "this node keeps no address book"

// GetBootstrapPeers returns the node's address book: the nodes it connects
// to when it starts, to find the rest from.
func (s *localService) GetBootstrapPeers(context.Context, *nodepb.GetBootstrapPeersRequest) (*nodepb.GetBootstrapPeersResponse, error) {
	if s.cfg.Book == nil {
		return &nodepb.GetBootstrapPeersResponse{}, nil
	}
	saved, err := s.cfg.Book.BootstrapPeers()
	if err != nil {
		return nil, status.Errorf(codes.Internal, "%v", err)
	}
	return &nodepb.GetBootstrapPeersResponse{Peers: bookEntries(saved)}, nil
}

// SetBootstrapPeers replaces the address book and connects to what is now
// in it. Each entry is a node's ID and an address ending in /p2p/ and that
// same ID.
func (s *localService) SetBootstrapPeers(ctx context.Context, req *nodepb.SetBootstrapPeersRequest) (*nodepb.SetBootstrapPeersResponse, error) {
	if err := s.authorize(ctx); err != nil {
		return nil, err
	}
	if s.cfg.Book == nil {
		return nil, status.Error(codes.FailedPrecondition, noAddressBook)
	}
	var entries []nodedb.BootstrapPeer
	var addresses []string
	for _, entry := range req.GetPeers() {
		info, err := peer.AddrInfoFromString(entry.GetAddress())
		if err != nil {
			return nil, status.Errorf(codes.InvalidArgument, "%q is not an address ending in /p2p/ and a node ID", entry.GetAddress())
		}
		if info.ID.String() != entry.GetPeerId() {
			return nil, status.Errorf(codes.InvalidArgument, "the address %s is not one of node %q", entry.GetAddress(), entry.GetPeerId())
		}
		entries = append(entries, nodedb.BootstrapPeer{PeerID: entry.GetPeerId(), Address: entry.GetAddress()})
		addresses = append(addresses, entry.GetAddress())
	}
	if err := s.cfg.Book.SetBootstrapPeers(entries); err != nil {
		return nil, status.Errorf(codes.Internal, "%v", err)
	}
	// Whoever answers, answers in its own time; the book is saved already.
	go s.cfg.Bootstrap(context.WithoutCancel(ctx), addresses)
	saved, _ := s.cfg.Book.BootstrapPeers() // just written, so readable
	return &nodepb.SetBootstrapPeersResponse{Peers: bookEntries(saved)}, nil
}

func bookEntries(saved []nodedb.BootstrapPeer) []*nodepb.BootstrapPeer {
	entries := make([]*nodepb.BootstrapPeer, 0, len(saved))
	for _, p := range saved {
		entries = append(entries, &nodepb.BootstrapPeer{PeerId: p.PeerID, Address: p.Address})
	}
	return entries
}

// ConnectPeer connects this node's libp2p host to the node at an address,
// and on success notes the address in the node's address book. Connecting
// to a node gives it no part in this node's pool; that is what trust is for.
func (s *localService) ConnectPeer(ctx context.Context, req *nodepb.ConnectPeerRequest) (*nodepb.ConnectPeerResponse, error) {
	if err := s.authorize(ctx); err != nil {
		return nil, err
	}
	info, err := peer.AddrInfoFromString(req.GetAddress())
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "%q is not an address ending in /p2p/ and a node ID", req.GetAddress())
	}
	if s.cfg.Connect == nil {
		return nil, status.Error(codes.FailedPrecondition, "this node runs no libp2p host to connect with")
	}
	if err := s.cfg.Connect(ctx, req.GetAddress()); err != nil {
		return nil, status.Errorf(codes.Unavailable, "connect: %v", err)
	}
	if s.cfg.Book != nil {
		// The node is connected whether or not that can be written down.
		s.cfg.Book.AddBootstrapPeer(nodedb.BootstrapPeer{PeerID: info.ID.String(), Address: req.GetAddress()})
	}
	return &nodepb.ConnectPeerResponse{PeerId: info.ID.String()}, nil
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

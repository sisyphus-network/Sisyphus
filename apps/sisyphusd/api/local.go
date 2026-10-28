package api

import (
	"context"
	"crypto/subtle"
	"net"
	"net/netip"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/libp2p/go-libp2p/core/peer"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/sisyphus-network/Sisyphus/apps/sisyphusd/access"
	"github.com/sisyphus-network/Sisyphus/apps/sisyphusd/planner"
	"github.com/sisyphus-network/Sisyphus/packages/ai"
	"github.com/sisyphus-network/Sisyphus/packages/geo/citytable"
	"github.com/sisyphus-network/Sisyphus/packages/nodedb"
	nodepb "github.com/sisyphus-network/Sisyphus/packages/protocol/sisyphus/node/v1"
	pb "github.com/sisyphus-network/Sisyphus/packages/protocol/sisyphus/v1"
	"github.com/sisyphus-network/Sisyphus/packages/runtime"
	"github.com/sisyphus-network/Sisyphus/packages/sealed"
)

var localNetworkInterfaces = net.Interfaces
var localNetworkInterfaceAddresses = func(network net.Interface) ([]net.Addr, error) { return network.Addrs() }

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
	// if it has been allowed to find out, and otherwise nothing, and how it
	// was found: from the node's own addresses, as the pool sees it, or by
	// looking it up.
	Country func() (code, source string)
	// NodeInfoChanges publishes changes to node-info values discovered while
	// the daemon is running. It is shared by all API clients watching node info.
	NodeInfoChanges *NodeInfoChanges
	// BrowserDeviceChanges publishes changes to the enrolled browser list.
	BrowserDeviceChanges *NodeInfoChanges
	// JoinedPool, if set, says which pool this node works for and how it
	// stands with it, and JoinedPoolChanges publishes each change to that.
	// A node that coordinates its own pool has joined none.
	JoinedPool        func() *nodepb.JoinedPool
	JoinedPoolChanges *NodeInfoChanges
	// LeavePool, if set, has the node leave the pool it works for: the
	// coordinator lets go of what it kept for the node, then the node
	// forgets the pool (#509).
	LeavePool func(ctx context.Context) error
	// Jobs, if set, is the pool this node coordinates, for the calls about
	// its workers and jobs. Workloads are the workloads the node knows.
	Jobs      JobControl
	Workloads *runtime.Registry
	// Assistant, if set, is the node's planner: its model, its
	// conversations, and the asking of questions.
	Assistant Assistant
	// WorkFor, if set, is the list of nodes this one takes work from. A
	// node that runs no worker has none.
	WorkFor WorkFor
	// Store, if set, is the pool's store, for the calls about files;
	// Files is then the list of what was stored through them, and
	// MaxStoreBytes the most disk the store may use, or zero for no limit.
	Store FileStore
	Files FileList
	// FileScope identifies the pool whose file catalogue this API serves.
	// The empty scope is the node's own pool and preserves the local contract.
	FileScope     func() string
	MaxStoreBytes uint64
	// SealingKey, if set, returns the key this node seals private files
	// and jobs with, making one the first time it is asked.
	SealingKey func() (sealed.Key, error)
	// Join, if set, has the node join the pool of the node at an address
	// with an invitation from it, and says which node that is and what
	// the node was admitted as. A node that runs no worker has none.
	Join func(ctx context.Context, address, invitation string) (id string, role access.Role, err error)
	// Token is what a caller must present to change anything.
	Token string
	// BrowserAccess keeps one-time pairing codes and revocable browser
	// credentials in the node database. Nil uses volatile storage (tests only).
	BrowserAccess BrowserAccessStore
	// WebAccess controls the separate, opt-in HTTPS listener for browser clients.
	WebAccess WebAccessController
	// LocationDiscovery stores optional location sources. The saved settings
	// take effect when the daemon restarts; running values are the settings it
	// started with.
	LocationDiscovery         LocationDiscoveryStore
	LocationDiscoveryRunning  nodedb.LocationDiscovery
	GatewayDiscoverySupported bool
	// OwnerLocation, if set, keeps where the owner says the node is. A
	// change takes effect at once.
	OwnerLocation OwnerLocationStore
	// CityTable, if set, keeps the city table the owner may download, and
	// City, if set, says where it puts the node: the place, and which of
	// the node's addresses it was found by, as in Country, or no source
	// if the table does not place the node or is not there.
	CityTable CityTableStore
	City      func() (place citytable.Place, source string)
	// PoolLimits stores the limits the node, as a coordinator, holds each
	// member to. The saved limits take effect when the daemon restarts;
	// running values are the limits it started with. RunsCoordinator says
	// whether the node has the coordinator role, without which the limits
	// hold no one to anything.
	PoolLimits        PoolLimitsStore
	PoolLimitsRunning nodedb.PoolLimits
	RunsCoordinator   bool
	// AllowLAN accepts requests addressed to one of this host's private IPs.
	// The listener must separately be enabled on a private interface.
	AllowLAN bool
	// Lending, if set, keeps how this machine lends its Ollama models to
	// the pool. Lent is how the node runs with them now, and LentByFlags
	// says that --models-from decided it in place of what is saved.
	Lending     LendingStore
	Lent        nodedb.ModelLending
	LentByFlags bool
	// LiveLending replaces model allowlists and request limits while an
	// already-enabled Ollama at the same address keeps serving the worker.
	LiveLending *runtime.ModelServer
	// OffersChanged, if set, is told when a call may have changed which
	// models this node's worker serves: a model fetched or removed, lending
	// changed, or the owner looking at what is lent. The worker then asks
	// its model server again, which it does not do on a timer.
	OffersChanged func()
	// LocationChanged, if set, is told when the owner changes or clears the
	// node's location, so that its workers tell their pools at once.
	LocationChanged func()
	// PairingOrigin is the LAN URL peers should open, without a trailing slash.
	PairingOrigin string
}

// BrowserAccessStore is the durable part of browser pairing. Credentials
// passed to and returned from it are hashes, never reusable bearer secrets.
type BrowserAccessStore interface {
	SaveBrowserPairing(codeHash []byte, expiresAt time.Time) error
	ConsumeBrowserPairing(codeHash []byte, now time.Time, device nodedb.BrowserDevice) (bool, error)
	BrowserDevices() ([]nodedb.BrowserDevice, error)
	DeleteBrowserDevice(id string) error
}

// WebAccessController owns persisted settings and the lifetime of the
// optional HTTPS listener. The private key never crosses the local API.
type WebAccessController interface {
	Get() (nodedb.WebAccessConfig, string, bool, error)
	Set(context.Context, nodedb.WebAccessConfig) (nodedb.WebAccessConfig, string, bool, error)
}

// AddressBook is where a node keeps the addresses it starts from. A
// nodedb.DB is one.
type AddressBook interface {
	BootstrapPeers() ([]nodedb.BootstrapPeer, error)
	SetBootstrapPeers([]nodedb.BootstrapPeer) error
	AddBootstrapPeer(nodedb.BootstrapPeer) error
}

// JobControl is what the local API needs of a coordinator. A
// coordinator.Coordinator is one.
type JobControl interface {
	Submit(ctx context.Context, spec *pb.JobSpec) (*pb.Job, error)
	Cancel(jobID string) (*pb.Job, error)
	WatchEvents(ctx context.Context, jobID string, after uint64, fn func(*pb.JobEvent) error) error
	Get(jobID string) (*pb.Job, error)
	Jobs() []*pb.Job
	Nodes() []*pb.NodeInfo
}

// Assistant is what the local API needs of a node's planner.
type Assistant interface {
	ModelConfig() (nodedb.ModelConfig, bool, error)
	SetModelConfig(nodedb.ModelConfig) error
	Models(ctx context.Context, at *nodedb.ModelConfig) ([]ai.Model, error)
	PullModel(ctx context.Context, at *nodedb.ModelConfig, model string, progress func(ai.Progress)) error
	RemoveModel(ctx context.Context, at *nodedb.ModelConfig, model string) error
	Chats() ([]nodedb.Chat, error)
	Chat(id string) ([]ai.Message, error)
	DeleteChat(id string) error
	Ask(ctx context.Context, chatID, text string, report func(chatID string, e planner.Event)) (string, error)
}

// WorkFor is the list of nodes a node is willing to take tasks from. It
// works for one of them whenever that node, for its part, will have it.
type WorkFor interface {
	Set(id string, willing bool) error
	List() []string
}

// PoolAdmin admits nodes to a coordinator's pool and removes them, exactly
// as the pool commands do.
type PoolAdmin struct {
	service *poolService
}

// NewPoolAdmin returns the means to change the membership of cfg's pool.
func NewPoolAdmin(cfg Config) *PoolAdmin {
	return &PoolAdmin{service: &poolService{
		id: cfg.Identity.ID(), access: cfg.Access, coordinator: cfg.Coordinator, swarm: cfg.Swarm, pins: clientPinsOf(cfg.Store),
	}}
}

// NewLocalServer returns a gRPC server for the local API.
func NewLocalServer(cfg LocalConfig) *grpc.Server {
	srv := grpc.NewServer()
	nodepb.RegisterNodeServiceServer(srv, &localService{cfg: normal(cfg)})
	return srv
}

// normal fills in what a configuration may leave out.
func normal(cfg LocalConfig) LocalConfig {
	if cfg.Country == nil {
		cfg.Country = func() (string, string) { return "", "" }
	}
	return cfg
}

type localService struct {
	nodepb.UnimplementedNodeServiceServer
	cfg          LocalConfig
	lendingMu    sync.RWMutex
	pendingOnce  sync.Once
	pendingQueue *pendingActionManager
}

func (s *localService) actionConfirmations() *pendingActionManager {
	s.pendingOnce.Do(func() { s.pendingQueue = newPendingActionManager() })
	return s.pendingQueue
}

func (s *localService) GetWebAccessConfig(ctx context.Context, _ *nodepb.GetWebAccessConfigRequest) (*nodepb.WebAccessConfig, error) {
	if err := s.authorize(ctx); err != nil {
		return nil, err
	}
	if s.cfg.WebAccess == nil {
		return &nodepb.WebAccessConfig{}, nil
	}
	config, fingerprint, restartRequired, err := s.cfg.WebAccess.Get()
	if err != nil {
		return nil, asError(err)
	}
	return webAccessProtoConfig(config, fingerprint, restartRequired), nil
}

func (s *localService) SetWebAccessConfig(ctx context.Context, req *nodepb.SetWebAccessConfigRequest) (*nodepb.WebAccessConfig, error) {
	if err := s.authorize(ctx); err != nil {
		return nil, err
	}
	if s.cfg.WebAccess == nil {
		return nil, status.Error(codes.FailedPrecondition, "web access configuration is unavailable")
	}
	addresses := req.GetListenAddresses()
	if len(addresses) == 0 && req.GetListenAddress() != "" {
		addresses = []string{req.GetListenAddress()}
	}
	listenAddress := req.GetListenAddress()
	if len(addresses) > 0 {
		listenAddress = addresses[0]
	}
	config, fingerprint, restartRequired, err := s.cfg.WebAccess.Set(ctx, nodedb.WebAccessConfig{
		Enabled: req.GetEnabled(), ListenAddress: listenAddress, ListenAddresses: addresses, PublicURL: req.GetPublicUrl(),
	})
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "%v", err)
	}
	return webAccessProtoConfig(config, fingerprint, restartRequired), nil
}

func webAccessProtoConfig(config nodedb.WebAccessConfig, fingerprint string, restartRequired bool) *nodepb.WebAccessConfig {
	addresses := config.ListenAddresses
	if len(addresses) == 0 && config.ListenAddress != "" {
		addresses = []string{config.ListenAddress}
	}
	return &nodepb.WebAccessConfig{
		Enabled: config.Enabled, ListenAddress: config.ListenAddress, ListenAddresses: addresses,
		PublicUrl: config.PublicURL, CertificateFingerprint: fingerprint, RestartRequired: restartRequired,
	}
}

func (s *localService) ListNetworkInterfaces(ctx context.Context, _ *nodepb.ListNetworkInterfacesRequest) (*nodepb.ListNetworkInterfacesResponse, error) {
	if err := s.authorize(ctx); err != nil {
		return nil, err
	}
	interfaces, err := localNetworkInterfaces()
	if err != nil {
		return nil, status.Errorf(codes.Internal, "list network interfaces: %v", err)
	}
	response := &nodepb.ListNetworkInterfacesResponse{}
	seen := make(map[string]struct{})
	for _, network := range interfaces {
		if network.Flags&net.FlagUp == 0 || network.Flags&net.FlagRunning == 0 || network.Flags&net.FlagLoopback != 0 {
			continue
		}
		addresses, err := localNetworkInterfaceAddresses(network)
		if err != nil {
			continue
		}
		for _, item := range addresses {
			prefix, err := netip.ParsePrefix(item.String())
			if err != nil {
				continue
			}
			address := prefix.Addr().Unmap()
			if !address.IsValid() || address.IsLoopback() || address.IsUnspecified() || address.IsMulticast() || address.IsLinkLocalUnicast() {
				continue
			}
			value := address.String()
			if _, exists := seen[value]; exists {
				continue
			}
			seen[value] = struct{}{}
			response.Interfaces = append(response.Interfaces, &nodepb.NetworkInterface{Name: network.Name, Address: value})
		}
	}
	sort.Slice(response.Interfaces, func(i, j int) bool {
		if response.Interfaces[i].Name == response.Interfaces[j].Name {
			return response.Interfaces[i].Address < response.Interfaces[j].Address
		}
		return response.Interfaces[i].Name < response.Interfaces[j].Name
	})
	return response, nil
}

type attachmentAssistant interface {
	AskWithFiles(context.Context, string, string, []string, func(string, planner.Event)) (string, error)
}

func (s *localService) GetNodeInfo(ctx context.Context, _ *nodepb.GetNodeInfoRequest) (*nodepb.GetNodeInfoResponse, error) {
	return s.nodeInfo(ctx), nil
}

// nodeInfo describes the node to the caller of ctx. Anything on the machine
// may ask, so where the owner says the node is goes only to a caller with
// the node's API token.
func (s *localService) nodeInfo(ctx context.Context) *nodepb.GetNodeInfoResponse {
	info := &nodepb.GetNodeInfoResponse{
		PeerId: s.cfg.NodeID, DaemonVersion: s.cfg.Version, Workloads: s.workloads().Names(),
	}
	if s.cfg.Listen != nil {
		info.ListenAddresses = s.cfg.Listen()
	}
	if s.cfg.Country != nil {
		info.CountryCode, info.CountrySource = s.cfg.Country()
	}
	if _, ok := s.cfg.Assistant.(attachmentAssistant); ok && s.cfg.Store != nil && s.cfg.Files != nil {
		info.Capabilities = append(info.Capabilities, "chat-file-references-v1")
		info.Capabilities = append(info.Capabilities, "chat-attachment-retention-v1")
	}
	if s.cfg.OwnerLocation != nil && s.authorize(ctx) == nil {
		// A location that cannot be read is left out, as an unknown country is.
		info.OwnerLocation, _ = s.ownerLocation()
	}
	info.CityEstimate = s.cityEstimate()
	return info
}

// WatchNodeState streams complete node-and-peer snapshots when their sources
// notify a change. It does not poll; callers that need several shared domains
// can use WatchSharedState.
func (s *localService) WatchNodeState(_ *nodepb.GetNodeInfoRequest, stream grpc.ServerStreamingServer[nodepb.NodeState]) error {
	var changes <-chan struct{}
	var unsubscribe func()
	if s.cfg.NodeInfoChanges != nil {
		changes, unsubscribe = s.cfg.NodeInfoChanges.subscribe()
		defer unsubscribe()
	}
	return watchNodeState(stream.Context(), changes, func() *nodepb.NodeState { return s.nodeState(stream.Context()) }, stream.Send)
}

// WatchSharedState streams typed, authoritative snapshots for shared domains.
// The token is required because some domains contain owner-only state. Changes
// are coalesced by the notifier; each wake reads the latest committed state,
// so a slow subscriber cannot miss the final value. A reconnect starts with a
// fresh baseline rather than replaying an unbounded event log.
func (s *localService) WatchSharedState(req *nodepb.WatchSharedStateRequest, stream grpc.ServerStreamingServer[nodepb.SharedStateEvent]) error {
	if err := s.authorize(stream.Context()); err != nil {
		return err
	}
	domains, err := sharedStateDomains(req.GetDomains())
	if err != nil {
		return err
	}
	var changes <-chan struct{}
	var unsubscribe func()
	if s.cfg.NodeInfoChanges != nil {
		changes, unsubscribe = s.cfg.NodeInfoChanges.subscribe()
		defer unsubscribe()
	}

	last := make(map[nodepb.SharedStateDomain]*nodepb.SharedStateEvent, len(domains))
	var revision uint64
	sendCurrent := func(domain nodepb.SharedStateDomain, snapshot bool) error {
		current := s.sharedStateSnapshot(stream.Context(), domain)
		if previous, ok := last[domain]; ok && sharedStateEqual(previous, current) {
			return nil
		}
		revision++
		current.Revision = revision
		current.Snapshot = snapshot
		if err := stream.Send(current); err != nil {
			return err
		}
		last[domain] = proto.Clone(current).(*nodepb.SharedStateEvent)
		return nil
	}
	for _, domain := range domains {
		if err := sendCurrent(domain, true); err != nil {
			return err
		}
	}
	for {
		select {
		case <-changes:
			for _, domain := range domains {
				if err := sendCurrent(domain, false); err != nil {
					return err
				}
			}
		case <-stream.Context().Done():
			return nil
		}
	}
}

func sharedStateDomains(requested []nodepb.SharedStateDomain) ([]nodepb.SharedStateDomain, error) {
	if len(requested) == 0 {
		return []nodepb.SharedStateDomain{
			nodepb.SharedStateDomain_SHARED_STATE_DOMAIN_NODE,
			nodepb.SharedStateDomain_SHARED_STATE_DOMAIN_PEERS,
			nodepb.SharedStateDomain_SHARED_STATE_DOMAIN_WORKERS,
			nodepb.SharedStateDomain_SHARED_STATE_DOMAIN_PAIRED_BROWSERS,
			nodepb.SharedStateDomain_SHARED_STATE_DOMAIN_JOINED_POOL,
		}, nil
	}
	seen := make(map[nodepb.SharedStateDomain]struct{}, len(requested))
	domains := make([]nodepb.SharedStateDomain, 0, len(requested))
	for _, domain := range requested {
		if domain < nodepb.SharedStateDomain_SHARED_STATE_DOMAIN_NODE || domain > nodepb.SharedStateDomain_SHARED_STATE_DOMAIN_JOINED_POOL {
			return nil, status.Errorf(codes.InvalidArgument, "unsupported shared-state domain %s", domain)
		}
		if _, ok := seen[domain]; ok {
			continue
		}
		seen[domain] = struct{}{}
		domains = append(domains, domain)
	}
	sort.Slice(domains, func(i, j int) bool { return domains[i] < domains[j] })
	return domains, nil
}

func (s *localService) sharedStateSnapshot(ctx context.Context, domain nodepb.SharedStateDomain) *nodepb.SharedStateEvent {
	event := &nodepb.SharedStateEvent{Domain: domain}
	switch domain {
	case nodepb.SharedStateDomain_SHARED_STATE_DOMAIN_NODE:
		event.State = &nodepb.SharedStateEvent_Node{Node: s.nodeState(ctx)}
	case nodepb.SharedStateDomain_SHARED_STATE_DOMAIN_PEERS:
		var peers []*nodepb.Peer
		if s.cfg.Peers != nil {
			peers = s.cfg.Peers()
		}
		event.State = &nodepb.SharedStateEvent_Peers{Peers: &nodepb.ListPeersResponse{Peers: peers}}
	case nodepb.SharedStateDomain_SHARED_STATE_DOMAIN_WORKERS:
		if err := s.jobsAvailable(); err != nil {
			event.Error = err.Error()
		} else {
			event.State = &nodepb.SharedStateEvent_Workers{Workers: s.workerList(true)}
		}
	case nodepb.SharedStateDomain_SHARED_STATE_DOMAIN_PAIRED_BROWSERS:
		state, err := s.pairedBrowsers()
		if err != nil {
			event.Error = err.Error()
		} else {
			event.State = &nodepb.SharedStateEvent_PairedBrowsers{PairedBrowsers: state}
		}
	case nodepb.SharedStateDomain_SHARED_STATE_DOMAIN_JOINED_POOL:
		state := &nodepb.JoinedPool{}
		if s.cfg.JoinedPool != nil {
			state = s.cfg.JoinedPool()
		}
		event.State = &nodepb.SharedStateEvent_JoinedPool{JoinedPool: state}
	}
	return event
}

func sharedStateEqual(left, right *nodepb.SharedStateEvent) bool {
	left = proto.Clone(left).(*nodepb.SharedStateEvent)
	right = proto.Clone(right).(*nodepb.SharedStateEvent)
	left.Revision, right.Revision = 0, 0
	left.Snapshot, right.Snapshot = false, false
	return proto.Equal(left, right)
}

// WatchPairedBrowsers streams the authorized paired-device list on changes.
func (s *localService) WatchPairedBrowsers(_ *nodepb.WatchPairedBrowsersRequest, stream grpc.ServerStreamingServer[nodepb.PairedBrowsersState]) error {
	if err := s.authorize(stream.Context()); err != nil {
		return err
	}
	var changes <-chan struct{}
	var unsubscribe func()
	if s.cfg.BrowserDeviceChanges != nil {
		changes, unsubscribe = s.cfg.BrowserDeviceChanges.subscribe()
		defer unsubscribe()
	}
	state, err := s.pairedBrowsers()
	if err != nil {
		return status.Errorf(codes.Internal, "list paired browsers: %v", err)
	}
	revision := uint64(1)
	if err := stream.Send(&nodepb.PairedBrowsersState{Devices: state.Devices, Revision: revision}); err != nil {
		return err
	}
	for {
		select {
		case <-changes:
		case <-stream.Context().Done():
			return nil
		}
		next, err := s.pairedBrowsers()
		if err != nil {
			return status.Errorf(codes.Internal, "list paired browsers: %v", err)
		}
		if proto.Equal(state, next) {
			continue
		}
		revision++
		if err := stream.Send(&nodepb.PairedBrowsersState{Devices: next.Devices, Revision: revision}); err != nil {
			return err
		}
		state = next
	}
}

// RequestActionConfirmation holds a destructive request until an authenticated
// owner client decides or the request expires/cancels.
func (s *localService) RequestActionConfirmation(ctx context.Context, req *nodepb.RequestActionConfirmationRequest) (*nodepb.RequestActionConfirmationResponse, error) {
	if err := s.authorize(ctx); err != nil {
		return nil, err
	}
	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "an action description is required")
	}
	approved, err := s.actionConfirmations().request(ctx, strings.TrimSpace(req.GetAction()))
	if err != nil {
		return nil, err
	}
	return &nodepb.RequestActionConfirmationResponse{Approved: approved}, nil
}

// WatchPendingActionConfirmations sends the current queue and then each
// change. Any authenticated owner client can receive the request, including a
// paired remote browser.
func (s *localService) WatchPendingActionConfirmations(_ *nodepb.WatchPendingActionConfirmationsRequest, stream grpc.ServerStreamingServer[nodepb.PendingActionConfirmationsState]) error {
	if err := s.authorize(stream.Context()); err != nil {
		return err
	}
	manager := s.actionConfirmations()
	changes, unsubscribe := manager.subscribe()
	defer unsubscribe()
	state := manager.snapshot()
	if err := stream.Send(state); err != nil {
		return err
	}
	for {
		select {
		case <-changes:
			next := manager.snapshot()
			if next.GetRevision() == state.GetRevision() {
				continue
			}
			if err := stream.Send(next); err != nil {
				return err
			}
			state = next
		case <-stream.Context().Done():
			return nil
		}
	}
}

// ResolveActionConfirmation records the owner's one-time decision.
func (s *localService) ResolveActionConfirmation(ctx context.Context, req *nodepb.ResolveActionConfirmationRequest) (*nodepb.ResolveActionConfirmationResponse, error) {
	if err := s.authorize(ctx); err != nil {
		return nil, err
	}
	if req == nil || strings.TrimSpace(req.GetId()) == "" {
		return nil, status.Error(codes.InvalidArgument, "a pending action ID is required")
	}
	if err := s.actionConfirmations().resolve(req.GetId(), req.GetApproved()); err != nil {
		return nil, err
	}
	return &nodepb.ResolveActionConfirmationResponse{}, nil
}
func (s *localService) pairedBrowsers() (*nodepb.PairedBrowsersState, error) {
	state := &nodepb.PairedBrowsersState{}
	if s.cfg.BrowserAccess == nil {
		return state, nil
	}
	devices, err := s.cfg.BrowserAccess.BrowserDevices()
	if err != nil {
		return nil, err
	}
	sort.Slice(devices, func(i, j int) bool { return devices[i].ID < devices[j].ID })
	for _, device := range devices {
		state.Devices = append(state.Devices, &nodepb.PairedBrowser{Id: device.ID, Name: device.Name, CreatedAtMs: device.CreatedAt.UnixMilli()})
	}
	return state, nil
}

func (s *localService) nodeState(ctx context.Context) *nodepb.NodeState {
	state := &nodepb.NodeState{Info: s.nodeInfo(ctx)}
	if s.cfg.Peers != nil {
		state.Peers = s.cfg.Peers()
	}
	return state
}

func watchNodeState(ctx context.Context, changes <-chan struct{}, current func() *nodepb.NodeState, send func(*nodepb.NodeState) error) error {
	return watchWithChanges(ctx, changes, current, func(now *nodepb.NodeState, revision uint64) error {
		update := proto.Clone(now).(*nodepb.NodeState)
		update.Revision = revision
		return send(update)
	})
}

func watchWithChanges[T proto.Message](ctx context.Context, changes <-chan struct{}, current func() T, send func(T, uint64) error) error {
	last := current()
	revision := uint64(1)
	if err := send(last, revision); err != nil {
		return err
	}
	for {
		select {
		case <-changes:
		case <-ctx.Done():
			return nil
		}
		now := current()
		if proto.Equal(now, last) {
			continue
		}
		revision++
		if err := send(now, revision); err != nil {
			return err
		}
		last = now
	}
}

// NodeInfoChanges broadcasts coalesced node-info changes to independent
// subscribers, so one local or remote client cannot consume another's event.
type NodeInfoChanges struct {
	mu          sync.Mutex
	subscribers map[chan struct{}]struct{}
}

func NewNodeInfoChanges() *NodeInfoChanges {
	return &NodeInfoChanges{subscribers: make(map[chan struct{}]struct{})}
}

func (c *NodeInfoChanges) Notify() {
	c.mu.Lock()
	defer c.mu.Unlock()
	for subscriber := range c.subscribers {
		select {
		case subscriber <- struct{}{}:
		default:
		}
	}
}

func (c *NodeInfoChanges) subscribe() (<-chan struct{}, func()) {
	subscriber := make(chan struct{}, 1)
	c.mu.Lock()
	c.subscribers[subscriber] = struct{}{}
	c.mu.Unlock()
	return subscriber, func() {
		c.mu.Lock()
		delete(c.subscribers, subscriber)
		c.mu.Unlock()
	}
}

func (s *localService) ListPeers(context.Context, *nodepb.ListPeersRequest) (*nodepb.ListPeersResponse, error) {
	return &nodepb.ListPeersResponse{Peers: s.cfg.Peers(), Revision: 1}, nil
}

// WatchPeers sends the peers as they stand and again, in full, whenever
// they change, with a revision that counts up from one.
func (s *localService) WatchPeers(_ *nodepb.WatchPeersRequest, stream grpc.ServerStreamingServer[nodepb.ListPeersResponse]) error {
	var changes <-chan struct{}
	var unsubscribe func()
	if s.cfg.NodeInfoChanges != nil {
		changes, unsubscribe = s.cfg.NodeInfoChanges.subscribe()
		defer unsubscribe()
	}
	return watchWithChanges(stream.Context(), changes,
		func() *nodepb.ListPeersResponse { return &nodepb.ListPeersResponse{Peers: s.cfg.Peers()} },
		func(now *nodepb.ListPeersResponse, revision uint64) error {
			return stream.Send(&nodepb.ListPeersResponse{Peers: now.GetPeers(), Revision: revision})
		})
}

// errNoPool is the answer to a call about a pool on a node that runs none.
var errNoPool = status.Error(codes.FailedPrecondition, "this node coordinates no pool, so it has no workers or jobs of its own")

// jobsAvailable says whether the node has a pool's workers and jobs to
// offer: those of the pool it coordinates, or, on a node that has joined
// another's pool, that pool's, when the JobControl says it may reach them.
func (s *localService) jobsAvailable() error {
	if s.cfg.Jobs == nil {
		return errNoPool
	}
	if gated, ok := s.cfg.Jobs.(interface{ Available() error }); ok {
		return gated.Available()
	}
	return nil
}

// ListWorkers lists the nodes connected to this one as its workers. Where
// their owners let the pool see they are is given only with the token, as
// this node's own location is: it is for the pool's members, not for any
// caller that can reach the API.
func (s *localService) ListWorkers(ctx context.Context, _ *nodepb.ListWorkersRequest) (*nodepb.ListWorkersResponse, error) {
	if err := s.jobsAvailable(); err != nil {
		return nil, err
	}
	return s.workerList(s.authorize(ctx) == nil), nil
}

// WatchWorkers streams worker state when the coordinator reports changes.
func (s *localService) WatchWorkers(_ *nodepb.ListWorkersRequest, stream grpc.ServerStreamingServer[nodepb.ListWorkersResponse]) error {
	if err := s.authorize(stream.Context()); err != nil {
		return err
	}
	if err := s.jobsAvailable(); err != nil {
		return err
	}
	var changes <-chan struct{}
	var unsubscribe func()
	if s.cfg.NodeInfoChanges != nil {
		changes, unsubscribe = s.cfg.NodeInfoChanges.subscribe()
		defer unsubscribe()
	}
	list := func() *nodepb.ListWorkersResponse { return s.workerList(true) }
	return watchWithChanges(stream.Context(), changes, list, func(now *nodepb.ListWorkersResponse, revision uint64) error {
		update := proto.Clone(now).(*nodepb.ListWorkersResponse)
		update.Revision = revision
		return stream.Send(update)
	})
}

// workerList lists the pool's workers, with where they are shared to be if
// places is set.
func (s *localService) workerList(places bool) *nodepb.ListWorkersResponse {
	res := new(nodepb.ListWorkersResponse)
	for _, node := range s.cfg.Jobs.Nodes() {
		c := node.GetCapabilities()
		var place *nodepb.SharedLocation
		if places {
			place = localSharedLocation(node.GetSharedLocation())
		}
		res.Workers = append(res.Workers, &nodepb.Worker{
			PeerId: node.GetNodeId(), Name: node.GetName(), Hostname: c.GetHostname(), Os: c.GetOs(), Arch: c.GetArch(),
			CpuCores: c.GetCpuCores(), TaskSlots: c.GetTaskSlots(), RunningTasks: node.GetRunningTasks(), Workloads: c.GetWorkloads(),
			Relays: len(node.GetRelayAddresses()) > 0, RelayedConnections: node.GetRelayedConnections(), RelayedBytes: node.GetRelayedBytes(),
			CpuModel: c.GetCpuModel(), MemoryBytes: c.GetMemoryBytes(), Gpus: localGPUs(c.GetGpus()),
			Models:         runtime.Models(c.GetLabels()),
			LoadedModels:   localLoaded(c.GetLabels()),
			VerifiedAgreed: node.GetVerifiedAgreed(), VerifiedOutvoted: node.GetVerifiedOutvoted(), Probation: node.GetProbation(),
			SharedLocation: place,
		})
	}
	return res
}

// SubmitJob gives the pool a job. It spends the pool's time, so it needs
// the node's token.
func (s *localService) SubmitJob(ctx context.Context, req *nodepb.SubmitJobRequest) (*nodepb.SubmitJobResponse, error) {
	if err := s.authorize(ctx); err != nil {
		return nil, err
	}
	if err := s.jobsAvailable(); err != nil {
		return nil, err
	}
	spec := &pb.JobSpec{
		Workload: req.GetWorkload(), Params: req.GetParams(), Mode: pb.ScheduleMode(req.GetMode()), MaxTasks: req.GetMaxTasks(),
		TaskTimeoutSeconds: req.GetTaskTimeoutSeconds(), MinMemoryBytes: req.GetMinMemoryBytes(), MinGpus: req.GetMinGpus(),
		Verify: req.GetVerify(), VerifyShare: req.GetVerifyShare(), AuditShare: req.GetAuditShare(),
	}
	if req.GetPrivate() {
		key, err := s.sealingKey()
		if err != nil {
			return nil, err
		}
		spec.Key = key[:]
	}
	job, err := s.cfg.Jobs.Submit(ctx, spec)
	if err != nil {
		return nil, err
	}
	return &nodepb.SubmitJobResponse{Job: localJob(job)}, nil
}

// CancelJob stops a job that has not finished. Like submitting one, it
// needs the node's token.
func (s *localService) CancelJob(ctx context.Context, req *nodepb.CancelJobRequest) (*nodepb.CancelJobResponse, error) {
	if err := s.authorize(ctx); err != nil {
		return nil, err
	}
	if err := s.jobsAvailable(); err != nil {
		return nil, err
	}
	job, err := s.cfg.Jobs.Cancel(req.GetJobId())
	if err != nil {
		return nil, err
	}
	return &nodepb.CancelJobResponse{Job: localJob(job)}, nil
}

// WatchJobEvents sends what has happened to a job, and goes on as more
// does until the job is over.
//
// Reading a job needs the token, as reading a conversation does: what a
// job was given, what it returned and what it logged are its owner's, and
// a conversation with a model run on the pool is a job like any other.
func (s *localService) WatchJobEvents(req *nodepb.WatchJobEventsRequest, stream grpc.ServerStreamingServer[nodepb.JobEvent]) error {
	if err := s.authorize(stream.Context()); err != nil {
		return err
	}
	if err := s.jobsAvailable(); err != nil {
		return err
	}
	return s.cfg.Jobs.WatchEvents(stream.Context(), req.GetJobId(), req.GetAfterSeq(), func(e *pb.JobEvent) error {
		return stream.Send(&nodepb.JobEvent{
			Seq: e.GetSeq(), AtMs: millis(e.GetAt()), Kind: e.GetKind(), TaskIndex: e.GetTaskIndex(), WorkerName: e.GetNodeName(), Text: e.GetText(),
		})
	})
}

func (s *localService) GetJob(ctx context.Context, req *nodepb.GetJobRequest) (*nodepb.GetJobResponse, error) {
	if err := s.authorize(ctx); err != nil {
		return nil, err
	}
	if err := s.jobsAvailable(); err != nil {
		return nil, err
	}
	job, err := s.cfg.Jobs.Get(req.GetJobId())
	if err != nil {
		return nil, err
	}
	return &nodepb.GetJobResponse{Job: localJob(job)}, nil
}

// ListJobs lists the jobs this node has on record, newest first.
func (s *localService) ListJobs(ctx context.Context, _ *nodepb.ListJobsRequest) (*nodepb.ListJobsResponse, error) {
	if err := s.authorize(ctx); err != nil {
		return nil, err
	}
	if err := s.jobsAvailable(); err != nil {
		return nil, err
	}
	return &nodepb.ListJobsResponse{Jobs: s.jobs(), Revision: 1}, nil
}

// WatchJobs sends the jobs as they stand and again, in full, whenever any
// of them changes.
func (s *localService) WatchJobs(_ *nodepb.WatchJobsRequest, stream grpc.ServerStreamingServer[nodepb.ListJobsResponse]) error {
	if err := s.authorize(stream.Context()); err != nil {
		return err
	}
	if err := s.jobsAvailable(); err != nil {
		return err
	}
	var changes <-chan struct{}
	var unsubscribe func()
	if s.cfg.NodeInfoChanges != nil {
		changes, unsubscribe = s.cfg.NodeInfoChanges.subscribe()
		defer unsubscribe()
	}
	return watchWithChanges(stream.Context(), changes,
		func() *nodepb.ListJobsResponse { return &nodepb.ListJobsResponse{Jobs: s.jobs()} },
		func(now *nodepb.ListJobsResponse, revision uint64) error {
			return stream.Send(&nodepb.ListJobsResponse{Jobs: now.GetJobs(), Revision: revision})
		})
}

func (s *localService) jobs() []*nodepb.Job {
	recorded := s.cfg.Jobs.Jobs()
	jobs := make([]*nodepb.Job, 0, len(recorded))
	for _, job := range recorded {
		jobs = append(jobs, localJob(job))
	}
	return jobs
}

// localJob gives a job in the local API's own terms, which stand alone so
// that a client needs only the one protocol file. The states and modes are
// numbered as the pool's own are.
func localJob(job *pb.Job) *nodepb.Job {
	out := &nodepb.Job{
		JobId: job.GetJobId(), Workload: job.GetSpec().GetWorkload(), Params: job.GetSpec().GetParams(),
		Mode: nodepb.JobMode(job.GetSpec().GetMode()), State: nodepb.JobState(job.GetState()),
		Result: job.GetResult(), Error: job.GetError(),
		CreatedAtMs: millis(job.GetCreatedAt()), FinishedAtMs: millis(job.GetFinishedAt()),
		InputBlobs: job.GetInputBlobs(), OutputBlobs: job.GetOutputBlobs(),
		Progress: job.GetProgress(), TaskTimeoutSeconds: job.GetSpec().GetTaskTimeoutSeconds(),
		MinMemoryBytes: job.GetSpec().GetMinMemoryBytes(), MinGpus: job.GetSpec().GetMinGpus(),
		Private: job.GetPrivate(), ParentJobId: job.GetParentJobId(), Step: job.GetStep(),
		RecordCid: job.GetRecordCid(), Verify: job.GetSpec().GetVerify(), VerifyShare: job.GetSpec().GetVerifyShare(), AuditShare: job.GetSpec().GetAuditShare(),
	}
	for _, task := range job.GetTasks() {
		out.Tasks = append(out.Tasks, &nodepb.JobTask{
			Index: task.GetIndex(), State: nodepb.JobState(task.GetState()), Attempt: task.GetAttempt(),
			PeerId: task.GetNodeId(), WorkerName: task.GetNodeName(), Error: task.GetError(), Progress: task.GetProgress(),
			Verify: task.GetVerify(),
		})
	}
	return out
}

// localLoaded is the models a worker's labels say its server has in memory.
func localLoaded(labels []string) []*nodepb.LoadedModel {
	var out []*nodepb.LoadedModel
	for _, m := range runtime.LoadedModels(labels) {
		out = append(out, &nodepb.LoadedModel{Name: m.Model, OnCardPercent: uint32(m.OnCard)})
	}
	return out
}

func localGPUs(gpus []*pb.Gpu) []*nodepb.WorkerGpu {
	var out []*nodepb.WorkerGpu
	for _, gpu := range gpus {
		out = append(out, &nodepb.WorkerGpu{Name: gpu.GetName(), MemoryBytes: gpu.GetMemoryBytes()})
	}
	return out
}

// millis is a time as milliseconds since the Unix epoch, or zero for none.
func millis(t *timestamppb.Timestamp) int64 {
	if t == nil {
		return 0
	}
	return t.AsTime().UnixMilli()
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

// SetPeerComputeTrust sets both sides of this node's trust in a peer at
// once, as far as the node has them: a node that runs no pool has no work
// to give, and trusts by taking work alone.
func (s *localService) SetPeerComputeTrust(ctx context.Context, req *nodepb.SetPeerComputeTrustRequest) (*nodepb.SetPeerComputeTrustResponse, error) {
	gives, takes := req.GetTrusted() && s.cfg.Pool != nil, req.GetTrusted() && s.cfg.WorkFor != nil
	if req.GetTrusted() && !gives && !takes {
		return nil, status.Error(codes.FailedPrecondition, "this node runs no pool and no worker, so it has no work to give and takes none")
	}
	if err := s.permit(ctx, req.GetPeerId(), gives, takes); err != nil {
		return nil, err
	}
	return &nodepb.SetPeerComputeTrustResponse{Trusted: req.GetTrusted()}, nil
}

// SetPeerComputePermissions sets the two sides of this node's trust in a
// peer separately: whether it gives the peer work, which is admitting it to
// the pool as a worker, and whether it takes work from the peer.
func (s *localService) SetPeerComputePermissions(ctx context.Context, req *nodepb.SetPeerComputePermissionsRequest) (*nodepb.SetPeerComputePermissionsResponse, error) {
	if req.GetGivesWork() && s.cfg.Pool == nil {
		return nil, status.Error(codes.FailedPrecondition, "this node runs no pool, so it has no work to give")
	}
	if req.GetTakesWork() && s.cfg.WorkFor == nil {
		return nil, status.Error(codes.FailedPrecondition, "this node runs no worker, so it takes no work")
	}
	if err := s.permit(ctx, req.GetPeerId(), req.GetGivesWork(), req.GetTakesWork()); err != nil {
		return nil, err
	}
	return &nodepb.SetPeerComputePermissionsResponse{GivesWork: req.GetGivesWork(), TakesWork: req.GetTakesWork()}, nil
}

// permit makes the two sides of this node's trust in a peer what they are
// asked to be. A side the node does not have is left alone.
func (s *localService) permit(ctx context.Context, id string, gives, takes bool) error {
	if err := s.authorize(ctx); err != nil {
		return err
	}
	if _, err := peer.Decode(id); err != nil {
		return status.Errorf(codes.InvalidArgument, "%q is not a node ID", id)
	}
	if s.cfg.Pool != nil {
		pool := s.cfg.Pool.service
		switch {
		case gives && !pool.access.Has(id, access.Worker):
			if err := pool.access.Admit(id, access.Worker, time.Now()); err != nil {
				return status.Errorf(codes.Internal, "admit node: %v", err)
			}
		case !gives && pool.access.Has(id, access.Worker) && pool.access.Has(id, access.Client):
			// It stays a client: it stops being given work, and only the
			// connection it works on ends.
			if _, err := pool.access.Revoke(id, access.Worker); err != nil {
				return status.Errorf(codes.Internal, "stop giving the node work: %v", err)
			}
			pool.coordinator.Remove(id)
		case !gives && pool.access.Has(id, access.Worker):
			if err := pool.removeMember(ctx, id); err != nil {
				return err
			}
		}
	}
	if s.cfg.WorkFor != nil {
		if err := s.cfg.WorkFor.Set(id, takes); err != nil {
			return status.Errorf(codes.Internal, "%v", err)
		}
	}
	return nil
}

// errNoPlanner is the answer to a call about the planner on a node that
// has none.
var errNoPlanner = status.Error(codes.FailedPrecondition, "this node has no planner")

// asError gives an error from the planner as a gRPC status, keeping the one
// it has if it has one.
func asError(err error) error {
	if _, is := status.FromError(err); is {
		return err
	}
	return status.Errorf(codes.Internal, "%v", err)
}

// GetModelConfig says which model the node plans with, and whether a key
// is set for its service, but not the key.
func (s *localService) GetModelConfig(context.Context, *nodepb.GetModelConfigRequest) (*nodepb.ModelConfig, error) {
	if s.cfg.Assistant == nil {
		return nil, errNoPlanner
	}
	cfg, _, err := s.cfg.Assistant.ModelConfig()
	if err != nil {
		return nil, asError(err)
	}
	return &nodepb.ModelConfig{Provider: cfg.Provider, BaseUrl: maskAddress(cfg.BaseURL), Model: cfg.Model, HasApiKey: cfg.APIKey != ""}, nil
}

func (s *localService) SetModelConfig(ctx context.Context, req *nodepb.SetModelConfigRequest) (*nodepb.ModelConfig, error) {
	if err := s.authorize(ctx); err != nil {
		return nil, err
	}
	if s.cfg.Assistant == nil {
		return nil, errNoPlanner
	}
	current, _, err := s.cfg.Assistant.ModelConfig()
	if err != nil {
		return nil, asError(err)
	}
	address, err := modelAddress(req.GetBaseUrl(), current.BaseURL)
	if err != nil {
		return nil, err
	}
	cfg := nodedb.ModelConfig{Provider: req.GetProvider(), BaseURL: address, Model: req.GetModel(), APIKey: req.GetApiKey()}
	if req.GetKeepApiKey() {
		// The key is kept for the service it was given for and no other:
		// a change of provider or address is saved without one, and the
		// answer says so.
		if sameService(current, cfg) {
			cfg.APIKey = current.APIKey
		}
	}
	if err := s.cfg.Assistant.SetModelConfig(cfg); err != nil {
		return nil, asError(err)
	}
	return &nodepb.ModelConfig{Provider: cfg.Provider, BaseUrl: maskAddress(cfg.BaseURL), Model: cfg.Model, HasApiKey: cfg.APIKey != ""}, nil
}

func (s *localService) ListModelProfiles(ctx context.Context, _ *nodepb.ListModelProfilesRequest) (*nodepb.ListModelProfilesResponse, error) {
	if err := s.authorize(ctx); err != nil {
		return nil, err
	}
	profiles, ok := s.cfg.Assistant.(interface {
		ModelProfiles() ([]nodedb.ModelProfile, error)
	})
	if !ok {
		return nil, errNoPlanner
	}
	items, err := profiles.ModelProfiles()
	if err != nil {
		return nil, asError(err)
	}
	response := &nodepb.ListModelProfilesResponse{}
	for _, p := range items {
		response.Profiles = append(response.Profiles, &nodepb.ModelProfile{Id: p.ID, Name: p.Name, Provider: p.Config.Provider, BaseUrl: maskAddress(p.Config.BaseURL), Model: p.Config.Model, HasApiKey: p.Config.APIKey != "", Active: p.Active})
	}
	return response, nil
}

func (s *localService) SaveModelProfile(ctx context.Context, req *nodepb.SaveModelProfileRequest) (*nodepb.ModelProfile, error) {
	if err := s.authorize(ctx); err != nil {
		return nil, err
	}
	profiles, ok := s.cfg.Assistant.(interface {
		ModelProfiles() ([]nodedb.ModelProfile, error)
		SaveModelProfile(nodedb.ModelProfile) error
	})
	if !ok {
		return nil, errNoPlanner
	}
	all, err := profiles.ModelProfiles()
	if err != nil {
		return nil, asError(err)
	}
	// The address may be this profile's own, or the planner's when a
	// profile is made from it, shown masked to whoever is saving.
	var held []string
	for _, existing := range all {
		if existing.ID == req.GetId() {
			held = append(held, existing.Config.BaseURL)
		}
	}
	if current, _, err := s.cfg.Assistant.ModelConfig(); err == nil {
		held = append(held, current.BaseURL)
	}
	address, err := modelAddress(req.GetBaseUrl(), held...)
	if err != nil {
		return nil, err
	}
	config := nodedb.ModelConfig{Provider: req.GetProvider(), BaseURL: address, Model: req.GetModel(), APIKey: req.GetApiKey()}
	if req.GetKeepApiKey() {
		for _, existing := range all {
			if existing.ID == req.GetId() && sameService(existing.Config, config) {
				config.APIKey = existing.Config.APIKey
				break
			}
		}
	}
	p := nodedb.ModelProfile{ID: req.GetId(), Name: req.GetName(), Config: config, Active: req.GetActive()}
	if p.ID == "" || p.Name == "" || p.Config.Provider == "" {
		return nil, status.Error(codes.InvalidArgument, "profile id, name and provider are required")
	}
	if err := profiles.SaveModelProfile(p); err != nil {
		return nil, asError(err)
	}
	return &nodepb.ModelProfile{Id: p.ID, Name: p.Name, Provider: p.Config.Provider, BaseUrl: maskAddress(p.Config.BaseURL), Model: p.Config.Model, HasApiKey: p.Config.APIKey != "", Active: p.Active}, nil
}

func (s *localService) DeleteModelProfile(ctx context.Context, req *nodepb.DeleteModelProfileRequest) (*nodepb.DeleteModelProfileResponse, error) {
	if err := s.authorize(ctx); err != nil {
		return nil, err
	}
	profiles, ok := s.cfg.Assistant.(interface{ DeleteModelProfile(string) error })
	if !ok {
		return nil, errNoPlanner
	}
	if err := profiles.DeleteModelProfile(req.GetId()); err != nil {
		return nil, asError(err)
	}
	return &nodepb.DeleteModelProfileResponse{}, nil
}

// Ask puts a question to the planner and sends what it does as it does it.
// It spends the pool's time and perhaps a service's credit, and what is
// said is the owner's business, so it needs the token, as do the calls that
// read conversations back.
func (s *localService) Ask(req *nodepb.AskRequest, stream grpc.ServerStreamingServer[nodepb.AskEvent]) error {
	if err := s.authorize(stream.Context()); err != nil {
		return err
	}
	if s.cfg.Assistant == nil {
		return errNoPlanner
	}
	report := func(chatID string, e planner.Event) {
		// A caller that has gone is found out when the planner next looks
		// at its context.
		stream.Send(&nodepb.AskEvent{ChatId: chatID, Kind: e.Kind, Text: e.Text, JobId: e.JobID, Tool: e.Tool})
	}
	var chatID string
	var err error
	if len(req.GetAttachmentCids()) > 0 {
		if len(req.GetAttachmentCids()) > 128 {
			return status.Error(codes.InvalidArgument, "at most 128 attachments may be retained per turn")
		}
		withFiles, ok := s.cfg.Assistant.(attachmentAssistant)
		if !ok {
			return status.Error(codes.Unimplemented, "planner attachment ownership is unavailable")
		}
		chatID, err = withFiles.AskWithFiles(stream.Context(), req.GetChatId(), req.GetText(), req.GetAttachmentCids(), report)
	} else {
		chatID, err = s.cfg.Assistant.Ask(stream.Context(), req.GetChatId(), req.GetText(), report)
	}
	if err != nil {
		return asError(err)
	}
	return stream.Send(&nodepb.AskEvent{ChatId: chatID, Kind: "done"})
}

func (s *localService) ListChats(ctx context.Context, _ *nodepb.ListChatsRequest) (*nodepb.ListChatsResponse, error) {
	if err := s.authorize(ctx); err != nil {
		return nil, err
	}
	if s.cfg.Assistant == nil {
		return nil, errNoPlanner
	}
	chats, err := s.cfg.Assistant.Chats()
	if err != nil {
		return nil, asError(err)
	}
	res := new(nodepb.ListChatsResponse)
	for _, c := range chats {
		res.Chats = append(res.Chats, &nodepb.ChatSummary{ChatId: c.ID, Title: c.Title, CreatedAtMs: c.Created.UnixMilli()})
	}
	return res, nil
}

func (s *localService) GetChat(ctx context.Context, req *nodepb.GetChatRequest) (*nodepb.GetChatResponse, error) {
	if err := s.authorize(ctx); err != nil {
		return nil, err
	}
	if s.cfg.Assistant == nil {
		return nil, errNoPlanner
	}
	messages, err := s.cfg.Assistant.Chat(req.GetChatId())
	if err != nil {
		return nil, asError(err)
	}
	res := new(nodepb.GetChatResponse)
	for _, m := range messages {
		out := &nodepb.ChatMessage{Role: m.Role, Content: m.Content, Tool: m.Name}
		for _, call := range m.Calls {
			out.Calls = append(out.Calls, &nodepb.ChatToolCall{Name: call.Name, Arguments: string(call.Arguments)})
		}
		res.Messages = append(res.Messages, out)
	}
	return res, nil
}

func (s *localService) DeleteChat(ctx context.Context, req *nodepb.DeleteChatRequest) (*nodepb.DeleteChatResponse, error) {
	if err := s.authorize(ctx); err != nil {
		return nil, err
	}
	if s.cfg.Assistant == nil {
		return nil, errNoPlanner
	}
	if err := s.cfg.Assistant.DeleteChat(req.GetChatId()); err != nil {
		return nil, asError(err)
	}
	return &nodepb.DeleteChatResponse{}, nil
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
	return status.Error(codes.PermissionDenied, "this needs the node's API token, which is in api.token in the node's data directory")
}

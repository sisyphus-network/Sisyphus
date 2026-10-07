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
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/sisyphus-network/Sisyphus/apps/sisyphusd/access"
	"github.com/sisyphus-network/Sisyphus/apps/sisyphusd/planner"
	"github.com/sisyphus-network/Sisyphus/packages/ai"
	"github.com/sisyphus-network/Sisyphus/packages/nodedb"
	nodepb "github.com/sisyphus-network/Sisyphus/packages/protocol/sisyphus/node/v1"
	pb "github.com/sisyphus-network/Sisyphus/packages/protocol/sisyphus/v1"
	"github.com/sisyphus-network/Sisyphus/packages/sealed"
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
	// Jobs, if set, is the pool this node coordinates, for the calls about
	// its workers and jobs. Workloads are the workloads the node knows.
	Jobs      JobControl
	Workloads []string
	// Assistant, if set, is the node's planner: its model, its
	// conversations, and the asking of questions.
	Assistant Assistant
	// WorkFor, if set, is the list of nodes this one takes work from. A
	// node that runs no worker has none.
	WorkFor WorkFor
	// Store, if set, is the pool's store, for the calls about files;
	// Files is then the list of what was stored through them, and
	// MaxStoreBytes the most disk the store may use, or zero for no limit.
	Store         FileStore
	Files         FileList
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
		PeerId: s.cfg.NodeID, DaemonVersion: s.cfg.Version, ListenAddresses: s.cfg.Listen(), CountryCode: s.cfg.Country(), Workloads: s.cfg.Workloads,
	}, nil
}

func (s *localService) ListPeers(context.Context, *nodepb.ListPeersRequest) (*nodepb.ListPeersResponse, error) {
	return &nodepb.ListPeersResponse{Peers: s.cfg.Peers(), Revision: 1}, nil
}

// WatchPeers sends the peers as they stand and again, in full, whenever
// they change, with a revision that counts up from one.
func (s *localService) WatchPeers(_ *nodepb.WatchPeersRequest, stream grpc.ServerStreamingServer[nodepb.ListPeersResponse]) error {
	return watch(stream.Context(), s.cfg.Poll,
		func() *nodepb.ListPeersResponse { return &nodepb.ListPeersResponse{Peers: s.cfg.Peers()} },
		func(now *nodepb.ListPeersResponse, revision uint64) error {
			return stream.Send(&nodepb.ListPeersResponse{Peers: now.GetPeers(), Revision: revision})
		})
}

// watch sends what current returns at once and again whenever it has
// changed, numbering what it sends from one, until ctx ends or a send fails.
func watch[T proto.Message](ctx context.Context, every time.Duration, current func() T, send func(now T, revision uint64) error) error {
	last, revision := current(), uint64(1)
	if err := send(last, revision); err != nil {
		return err
	}
	poll := time.NewTicker(every)
	defer poll.Stop()
	for {
		select {
		case <-poll.C:
			now := current()
			if proto.Equal(now, last) {
				continue
			}
			revision++
			if err := send(now, revision); err != nil {
				return err
			}
			last = now
		case <-ctx.Done():
			return nil
		}
	}
}

// errNoPool is the answer to a call about a pool on a node that runs none.
var errNoPool = status.Error(codes.FailedPrecondition, "this node coordinates no pool, so it has no workers or jobs of its own")

// ListWorkers lists the nodes connected to this one as its workers.
func (s *localService) ListWorkers(context.Context, *nodepb.ListWorkersRequest) (*nodepb.ListWorkersResponse, error) {
	if s.cfg.Jobs == nil {
		return nil, errNoPool
	}
	res := new(nodepb.ListWorkersResponse)
	for _, node := range s.cfg.Jobs.Nodes() {
		c := node.GetCapabilities()
		res.Workers = append(res.Workers, &nodepb.Worker{
			PeerId: node.GetNodeId(), Name: node.GetName(), Hostname: c.GetHostname(), Os: c.GetOs(), Arch: c.GetArch(),
			CpuCores: c.GetCpuCores(), TaskSlots: c.GetTaskSlots(), RunningTasks: node.GetRunningTasks(), Workloads: c.GetWorkloads(),
			Relays: len(node.GetRelayAddresses()) > 0, RelayedConnections: node.GetRelayedConnections(), RelayedBytes: node.GetRelayedBytes(),
			CpuModel: c.GetCpuModel(), MemoryBytes: c.GetMemoryBytes(), Gpus: localGPUs(c.GetGpus()),
		})
	}
	return res, nil
}

// SubmitJob gives the pool a job. It spends the pool's time, so it needs
// the node's token.
func (s *localService) SubmitJob(ctx context.Context, req *nodepb.SubmitJobRequest) (*nodepb.SubmitJobResponse, error) {
	if err := s.authorize(ctx); err != nil {
		return nil, err
	}
	if s.cfg.Jobs == nil {
		return nil, errNoPool
	}
	spec := &pb.JobSpec{
		Workload: req.GetWorkload(), Params: req.GetParams(), Mode: pb.ScheduleMode(req.GetMode()), MaxTasks: req.GetMaxTasks(),
		TaskTimeoutSeconds: req.GetTaskTimeoutSeconds(), MinMemoryBytes: req.GetMinMemoryBytes(), MinGpus: req.GetMinGpus(),
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
	if s.cfg.Jobs == nil {
		return nil, errNoPool
	}
	job, err := s.cfg.Jobs.Cancel(req.GetJobId())
	if err != nil {
		return nil, err
	}
	return &nodepb.CancelJobResponse{Job: localJob(job)}, nil
}

// WatchJobEvents sends what has happened to a job, and goes on as more
// does until the job is over.
func (s *localService) WatchJobEvents(req *nodepb.WatchJobEventsRequest, stream grpc.ServerStreamingServer[nodepb.JobEvent]) error {
	if s.cfg.Jobs == nil {
		return errNoPool
	}
	return s.cfg.Jobs.WatchEvents(stream.Context(), req.GetJobId(), req.GetAfterSeq(), func(e *pb.JobEvent) error {
		return stream.Send(&nodepb.JobEvent{
			Seq: e.GetSeq(), AtMs: millis(e.GetAt()), Kind: e.GetKind(), TaskIndex: e.GetTaskIndex(), WorkerName: e.GetNodeName(), Text: e.GetText(),
		})
	})
}

func (s *localService) GetJob(_ context.Context, req *nodepb.GetJobRequest) (*nodepb.GetJobResponse, error) {
	if s.cfg.Jobs == nil {
		return nil, errNoPool
	}
	job, err := s.cfg.Jobs.Get(req.GetJobId())
	if err != nil {
		return nil, err
	}
	return &nodepb.GetJobResponse{Job: localJob(job)}, nil
}

// ListJobs lists the jobs this node has on record, newest first.
func (s *localService) ListJobs(context.Context, *nodepb.ListJobsRequest) (*nodepb.ListJobsResponse, error) {
	if s.cfg.Jobs == nil {
		return nil, errNoPool
	}
	return &nodepb.ListJobsResponse{Jobs: s.jobs(), Revision: 1}, nil
}

// WatchJobs sends the jobs as they stand and again, in full, whenever any
// of them changes.
func (s *localService) WatchJobs(_ *nodepb.WatchJobsRequest, stream grpc.ServerStreamingServer[nodepb.ListJobsResponse]) error {
	if s.cfg.Jobs == nil {
		return errNoPool
	}
	return watch(stream.Context(), s.cfg.Poll,
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
		Private: job.GetPrivate(),
	}
	for _, task := range job.GetTasks() {
		out.Tasks = append(out.Tasks, &nodepb.JobTask{
			Index: task.GetIndex(), State: nodepb.JobState(task.GetState()), Attempt: task.GetAttempt(),
			PeerId: task.GetNodeId(), WorkerName: task.GetNodeName(), Error: task.GetError(), Progress: task.GetProgress(),
		})
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
		role, member := pool.access.Role(id)
		switch {
		case gives && role != access.Worker:
			if err := pool.access.Admit(id, access.Worker, time.Now()); err != nil {
				return status.Errorf(codes.Internal, "admit node: %v", err)
			}
		case !gives && member && role == access.Worker:
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
	return &nodepb.ModelConfig{Provider: cfg.Provider, BaseUrl: cfg.BaseURL, Model: cfg.Model, HasApiKey: cfg.APIKey != ""}, nil
}

func (s *localService) SetModelConfig(ctx context.Context, req *nodepb.SetModelConfigRequest) (*nodepb.ModelConfig, error) {
	if err := s.authorize(ctx); err != nil {
		return nil, err
	}
	if s.cfg.Assistant == nil {
		return nil, errNoPlanner
	}
	cfg := nodedb.ModelConfig{Provider: req.GetProvider(), BaseURL: req.GetBaseUrl(), Model: req.GetModel(), APIKey: req.GetApiKey()}
	if req.GetKeepApiKey() {
		current, _, err := s.cfg.Assistant.ModelConfig()
		if err != nil {
			return nil, asError(err)
		}
		cfg.APIKey = current.APIKey
	}
	if err := s.cfg.Assistant.SetModelConfig(cfg); err != nil {
		return nil, asError(err)
	}
	return &nodepb.ModelConfig{Provider: cfg.Provider, BaseUrl: cfg.BaseURL, Model: cfg.Model, HasApiKey: cfg.APIKey != ""}, nil
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
	chatID, err := s.cfg.Assistant.Ask(stream.Context(), req.GetChatId(), req.GetText(), func(chatID string, e planner.Event) {
		// A caller that has gone is found out when the planner next looks
		// at its context.
		stream.Send(&nodepb.AskEvent{ChatId: chatID, Kind: e.Kind, Text: e.Text, JobId: e.JobID, Tool: e.Tool})
	})
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
	return status.Error(codes.PermissionDenied, "changing this node needs its API token, which is in api.token in the node's data directory")
}

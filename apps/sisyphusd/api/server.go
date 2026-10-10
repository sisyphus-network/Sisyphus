// Package api exposes a coordinator over gRPC: NodeService for clients and
// CoordinatorService for workers, on one server.
package api

import (
	"context"
	"slices"
	"sort"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/status"

	"github.com/sisyphus-network/Sisyphus/apps/sisyphusd/access"
	"github.com/sisyphus-network/Sisyphus/apps/sisyphusd/coordinator"
	"github.com/sisyphus-network/Sisyphus/apps/sisyphusd/replication"
	"github.com/sisyphus-network/Sisyphus/packages/identity"
	pb "github.com/sisyphus-network/Sisyphus/packages/protocol/sisyphus/v1"
	"github.com/sisyphus-network/Sisyphus/packages/storage"
)

type Config struct {
	// Identity is the key the node answers with.
	Identity *identity.Identity
	// Access says which other nodes may call, and what.
	Access      *access.List
	Coordinator *coordinator.Coordinator
	Store       *storage.Store
	// MaxStoreBytes is the most disk the store may use before uploads are
	// refused. Zero means no limit.
	MaxStoreBytes uint64
	// Swarm is the private IPFS network admitted nodes are told how to
	// join, if this node runs one.
	Swarm Swarm
	// Cluster is the pool's IPFS Cluster, if this node runs a peer of it.
	Cluster Cluster
	// WorkFor is the list of nodes this one takes work from, if it keeps
	// one.
	WorkFor WorkFor
	// Names is the names this node answers for, which admitted nodes
	// publish their own among and resolve.
	Names *Names
	// Replication has the node's storage followers keep copies of what is
	// in Store. Without it nothing is asked of them.
	Replication *replication.Manager
}

// NewServer returns a gRPC server for a node running the coordinator role:
// the job and worker services for its coordinator, blob transfer and pinning
// for its store, the names it answers for, and management of who may
// connect. Every connection is TLS with both ends identified by their node
// keys, and every call is checked against the caller's role.
func NewServer(cfg Config, opts ...grpc.ServerOption) *grpc.Server {
	opts = append(opts, grpc.Creds(credentials.NewTLS(cfg.Identity.ServerTLS())))
	opts = append(opts, cfg.Access.ServerOptions()...)
	srv := grpc.NewServer(append(opts,
		// Pings notice workers that vanish without closing their connection.
		grpc.KeepaliveParams(keepalive.ServerParameters{Time: 10 * time.Second, Timeout: 5 * time.Second}),
		grpc.KeepaliveEnforcementPolicy(keepalive.EnforcementPolicy{MinTime: 5 * time.Second, PermitWithoutStream: true}),
	)...)
	pb.RegisterCoordinatorServiceServer(srv, cfg.Coordinator)
	pb.RegisterNodeServiceServer(srv, &nodeService{coordinator: cfg.Coordinator, owner: cfg.Identity.ID()})
	if cfg.Replication == nil {
		cfg.Replication = replication.Off(cfg.Store)
	}
	pb.RegisterBlobServiceServer(srv, &blobService{
		// A blob the store turns out to lack is fetched back from a
		// follower that holds a copy.
		store: replication.Recovering{Store: cfg.Store, From: cfg.Replication}, quota: cfg.MaxStoreBytes,
		// What a follower holds for the pool, a worker can fetch from it.
		holders: func(blob, asker string) []*pb.BlobHolder {
			return mergeHolders(cfg.Coordinator.Holders(blob, asker), cfg.Replication.Holders(blob, asker))
		},
		replication: cfg.Replication, owner: cfg.Identity.ID(),
	})
	pb.RegisterPoolServiceServer(srv, &poolService{
		id: cfg.Identity.ID(), access: cfg.Access, coordinator: cfg.Coordinator, swarm: cfg.Swarm, cluster: cfg.Cluster, workFor: cfg.WorkFor,
	})
	pb.RegisterNameServiceServer(srv, &nameService{names: cfg.Names})
	tunnels := &tunnelService{}
	if cfg.Swarm != nil {
		tunnels.swarm = cfg.Swarm.Local
	}
	if cfg.Cluster != nil {
		tunnels.cluster = cfg.Cluster.Local
	}
	pb.RegisterTunnelServiceServer(srv, tunnels)
	return srv
}

// mergeHolders joins two lists of a blob's holders, each ordered by node
// ID, into one, naming no node twice.
func mergeHolders(a, b []*pb.BlobHolder) []*pb.BlobHolder {
	merged := append(a, b...)
	sort.SliceStable(merged, func(i, j int) bool { return merged[i].GetNodeId() < merged[j].GetNodeId() })
	return slices.CompactFunc(merged, func(x, y *pb.BlobHolder) bool { return x.GetNodeId() == y.GetNodeId() })
}

type nodeService struct {
	pb.UnimplementedNodeServiceServer
	coordinator *coordinator.Coordinator
	// owner is the ID of the node itself, which may see every job.
	owner string
}

// guest returns the ID of whoever is calling, if it is a node admitted to
// the pool and not the node itself: someone to be kept to what is theirs.
// A call that did not come through the access package is the node's own.
func guest(ctx context.Context, owner string) (id string, is bool) {
	id = access.Caller(ctx)
	return id, id != "" && id != owner
}

// own checks that a job is the caller's to see and to stop. A client is
// one of several and sees the jobs it submitted; another's job is, to it,
// a job that is not there, which is also all it is told.
func (s *nodeService) own(ctx context.Context, jobID string) error {
	client, is := guest(ctx, s.owner)
	if !is {
		return nil
	}
	job, err := s.coordinator.Get(jobID)
	if err != nil {
		return err
	}
	if job.GetSubmitterId() != client {
		return status.Errorf(codes.NotFound, "job %q not found", jobID)
	}
	return nil
}

func (s *nodeService) SubmitJob(ctx context.Context, req *pb.SubmitJobRequest) (*pb.SubmitJobResponse, error) {
	job, err := s.coordinator.Submit(ctx, req.GetSpec())
	if err != nil {
		return nil, err
	}
	return &pb.SubmitJobResponse{Job: job}, nil
}

func (s *nodeService) GetJob(ctx context.Context, req *pb.GetJobRequest) (*pb.GetJobResponse, error) {
	if err := s.own(ctx, req.GetJobId()); err != nil {
		return nil, err
	}
	job, err := s.coordinator.Get(req.GetJobId())
	if err != nil {
		return nil, err
	}
	return &pb.GetJobResponse{Job: job}, nil
}

func (s *nodeService) WatchJob(req *pb.WatchJobRequest, stream grpc.ServerStreamingServer[pb.WatchJobResponse]) error {
	if err := s.own(stream.Context(), req.GetJobId()); err != nil {
		return err
	}
	return s.coordinator.Watch(stream.Context(), req.GetJobId(), func(job *pb.Job) error {
		return stream.Send(&pb.WatchJobResponse{Job: job})
	})
}

func (s *nodeService) CancelJob(ctx context.Context, req *pb.CancelJobRequest) (*pb.CancelJobResponse, error) {
	if err := s.own(ctx, req.GetJobId()); err != nil {
		return nil, err
	}
	job, err := s.coordinator.Cancel(req.GetJobId())
	if err != nil {
		return nil, err
	}
	return &pb.CancelJobResponse{Job: job}, nil
}

func (s *nodeService) WatchJobEvents(req *pb.WatchJobEventsRequest, stream grpc.ServerStreamingServer[pb.JobEvent]) error {
	if err := s.own(stream.Context(), req.GetJobId()); err != nil {
		return err
	}
	return s.coordinator.WatchEvents(stream.Context(), req.GetJobId(), req.GetAfterSeq(), stream.Send)
}

func (s *nodeService) GetJobRecord(ctx context.Context, req *pb.GetJobRecordRequest) (*pb.JobRecord, error) {
	if err := s.own(ctx, req.GetJobId()); err != nil {
		return nil, err
	}
	return s.coordinator.Record(ctx, req.GetJobId(), req.GetVerify(), req.GetKey())
}

func (s *nodeService) ListNodes(context.Context, *pb.ListNodesRequest) (*pb.ListNodesResponse, error) {
	return &pb.ListNodesResponse{Nodes: s.coordinator.Nodes()}, nil
}

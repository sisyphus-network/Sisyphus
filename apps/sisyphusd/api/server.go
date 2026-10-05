// Package api exposes a coordinator over gRPC: NodeService for clients and
// CoordinatorService for workers, on one server.
package api

import (
	"context"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/keepalive"

	"github.com/excho0/Sisyphus/apps/sisyphusd/coordinator"
	pb "github.com/excho0/Sisyphus/packages/protocol/sisyphus/v1"
)

// NewServer returns a gRPC server serving both services for c. It has no
// transport security or authentication yet, so only expose it to machines
// you trust.
func NewServer(c *coordinator.Coordinator) *grpc.Server {
	srv := grpc.NewServer(
		// Pings notice workers that vanish without closing their connection.
		grpc.KeepaliveParams(keepalive.ServerParameters{Time: 10 * time.Second, Timeout: 5 * time.Second}),
		grpc.KeepaliveEnforcementPolicy(keepalive.EnforcementPolicy{MinTime: 5 * time.Second, PermitWithoutStream: true}),
	)
	pb.RegisterCoordinatorServiceServer(srv, c)
	pb.RegisterNodeServiceServer(srv, &nodeService{coordinator: c})
	return srv
}

type nodeService struct {
	pb.UnimplementedNodeServiceServer
	coordinator *coordinator.Coordinator
}

func (s *nodeService) SubmitJob(_ context.Context, req *pb.SubmitJobRequest) (*pb.SubmitJobResponse, error) {
	job, err := s.coordinator.Submit(req.GetSpec())
	if err != nil {
		return nil, err
	}
	return &pb.SubmitJobResponse{Job: job}, nil
}

func (s *nodeService) GetJob(_ context.Context, req *pb.GetJobRequest) (*pb.GetJobResponse, error) {
	job, err := s.coordinator.Get(req.GetJobId())
	if err != nil {
		return nil, err
	}
	return &pb.GetJobResponse{Job: job}, nil
}

func (s *nodeService) WatchJob(req *pb.WatchJobRequest, stream grpc.ServerStreamingServer[pb.WatchJobResponse]) error {
	return s.coordinator.Watch(stream.Context(), req.GetJobId(), func(job *pb.Job) error {
		return stream.Send(&pb.WatchJobResponse{Job: job})
	})
}

func (s *nodeService) ListNodes(context.Context, *pb.ListNodesRequest) (*pb.ListNodesResponse, error) {
	return &pb.ListNodesResponse{Nodes: s.coordinator.Nodes()}, nil
}

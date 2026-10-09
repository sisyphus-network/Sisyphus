package api

import (
	"context"
	"net"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/sisyphus-network/Sisyphus/apps/sisyphusd/tunnel"
	pb "github.com/sisyphus-network/Sisyphus/packages/protocol/sisyphus/v1"
)

// tunnelService lets members reach this node's Kubo daemon, and its cluster
// peer if it runs one, through the node's own port.
type tunnelService struct {
	pb.UnimplementedTunnelServiceServer
	// swarm returns where this node's Kubo accepts members of the pool's
	// private network, as a host and port. It is nil on a node without one.
	swarm func(ctx context.Context) (string, error)
	// cluster does the same for this node's IPFS Cluster peer.
	cluster func(ctx context.Context) (string, error)
}

func (s *tunnelService) Open(stream grpc.BidiStreamingServer[pb.TunnelData, pb.TunnelData]) error {
	first, err := stream.Recv()
	if err != nil {
		return status.Error(codes.InvalidArgument, "a tunnel was opened and closed again with nothing said")
	}
	find, service, absent := s.swarm, "Kubo", "this node does not run a private IPFS network; start it with --kubo"
	switch first.GetTarget() {
	case pb.TunnelTarget_TUNNEL_TARGET_SWARM:
	case pb.TunnelTarget_TUNNEL_TARGET_CLUSTER:
		find, service, absent = s.cluster, "cluster peer", noCluster
	default:
		return status.Errorf(codes.InvalidArgument, "the first message of a tunnel must name a service this node tunnels to, not %v", first.GetTarget())
	}
	if find == nil {
		return status.Error(codes.FailedPrecondition, absent)
	}
	addr, err := find(stream.Context())
	if err != nil {
		return status.Errorf(codes.Unavailable, "find this node's %s: %v", service, err)
	}
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		return status.Errorf(codes.Unavailable, "connect to this node's %s: %v", service, err)
	}
	conn.Write(first.GetData())
	return tunnel.Serve(conn.(*net.TCPConn), stream)
}

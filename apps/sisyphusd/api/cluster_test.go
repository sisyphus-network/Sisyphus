package api

import (
	"context"
	"errors"
	"net"
	"strings"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"

	pb "github.com/sisyphus-network/Sisyphus/packages/protocol/sisyphus/v1"
)

// members is a cluster that says what it is told to.
type members struct {
	local string
	state *pb.ClusterStatusResponse
	err   error
}

func (m members) Local(context.Context) (string, error) { return m.local, m.err }

func (m members) Status(context.Context) (*pb.ClusterStatusResponse, error) { return m.state, m.err }

func TestClusterStatusIsTheClusterPeersOwnAccount(t *testing.T) {
	ctx := context.Background()
	state := &pb.ClusterStatusResponse{Replicas: 2, Members: []*pb.ClusterMember{{NodeId: "12D3KooWone", Name: "hub"}}}
	running := &poolService{cluster: members{state: state}}
	if got, err := running.ClusterStatus(ctx, &pb.ClusterStatusRequest{}); err != nil || got != state {
		t.Errorf("ClusterStatus = %v, %v", got, err)
	}

	_, err := (&poolService{}).ClusterStatus(ctx, &pb.ClusterStatusRequest{})
	if status.Code(err) != codes.FailedPrecondition || !strings.Contains(err.Error(), "--kubo --cluster") {
		t.Errorf("a node with no cluster peer: %v, want FailedPrecondition and how to get one", err)
	}
	broken := &poolService{cluster: members{err: errors.New("the peer went away")}}
	if _, err := broken.ClusterStatus(ctx, &pb.ClusterStatusRequest{}); status.Code(err) != codes.Unavailable || !strings.Contains(err.Error(), "the peer went away") {
		t.Errorf("a node whose cluster peer does not answer: %v, want Unavailable", err)
	}
}

func TestSwarmSaysWhetherThePoolHasACluster(t *testing.T) {
	ctx := context.Background()
	changes := 0
	without := &poolService{swarm: network{rekeyed: &changes}}
	if got, err := without.Swarm(ctx, &pb.SwarmRequest{}); err != nil || got.GetCluster() {
		t.Errorf("a pool without a cluster: %v, %v", got, err)
	}
	with := &poolService{swarm: network{rekeyed: &changes}, cluster: members{}}
	if got, err := with.Swarm(ctx, &pb.SwarmRequest{}); err != nil || !got.GetCluster() {
		t.Errorf("a pool with a cluster: %v, %v", got, err)
	}
}

// openTunnel opens a tunnel to the cluster peer of a node whose peer is
// found by cluster, sends data and returns what comes back.
func openTunnel(t *testing.T, cluster func(context.Context) (string, error), data string) (string, error) {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := grpc.NewServer()
	// The node's Kubo is elsewhere: a tunnel to the cluster must not end
	// up there.
	pb.RegisterTunnelServiceServer(srv, &tunnelService{swarm: at("127.0.0.1:1"), cluster: cluster})
	go srv.Serve(lis)
	t.Cleanup(srv.Stop)
	conn, err := grpc.NewClient(lis.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	stream, err := pb.NewTunnelServiceClient(conn).Open(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := stream.Send(&pb.TunnelData{Target: pb.TunnelTarget_TUNNEL_TARGET_CLUSTER, Data: []byte(data)}); err != nil {
		t.Fatal(err)
	}
	reply, err := stream.Recv()
	return string(reply.GetData()), err
}

func TestATunnelReachesTheNodesClusterPeer(t *testing.T) {
	// Standing in for the cluster peer: something that answers what is said
	// to it.
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer lis.Close()
	go func() {
		conn, err := lis.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		buffer := make([]byte, 64)
		n, _ := conn.Read(buffer)
		conn.Write(append([]byte("the cluster peer heard: "), buffer[:n]...))
	}()
	if got, err := openTunnel(t, at(lis.Addr().String()), "hello"); err != nil || got != "the cluster peer heard: hello" {
		t.Errorf("reply %q, %v", got, err)
	}

	if _, err := openTunnel(t, nil, "hello"); status.Code(err) != codes.FailedPrecondition || !strings.Contains(err.Error(), "does not run an IPFS Cluster peer") {
		t.Errorf("a node with no cluster peer: %v, want FailedPrecondition", err)
	}
	lost := func(context.Context) (string, error) { return "", errors.New("the peer is restarting") }
	if _, err := openTunnel(t, lost, "hello"); status.Code(err) != codes.Unavailable || !strings.Contains(err.Error(), "find this node's cluster peer") {
		t.Errorf("a cluster peer that cannot be found: %v, want Unavailable", err)
	}
}

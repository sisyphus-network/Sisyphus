package api

import (
	"context"
	"errors"
	"net"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"

	pb "github.com/sisyphus-network/Sisyphus/packages/protocol/sisyphus/v1"
)

// startTunnels serves the tunnel service of a node whose Kubo is found by
// swarm, and returns a client for it.
func startTunnels(t *testing.T, swarm func(context.Context) (string, error)) pb.TunnelServiceClient {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := grpc.NewServer()
	pb.RegisterTunnelServiceServer(srv, &tunnelService{swarm: swarm})
	go srv.Serve(lis)
	t.Cleanup(srv.Stop)
	conn, err := grpc.NewClient(lis.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	return pb.NewTunnelServiceClient(conn)
}

// at is a node's Kubo found at a fixed address.
func at(addr string) func(context.Context) (string, error) {
	return func(context.Context) (string, error) { return addr, nil }
}

func TestATunnelReachesTheNodesKubo(t *testing.T) {
	// Standing in for Kubo: something that answers each thing said to it.
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
		for {
			n, err := conn.Read(buffer)
			if err != nil {
				return
			}
			conn.Write(append([]byte("heard: "), buffer[:n]...))
		}
	}()

	stream, err := startTunnels(t, at(lis.Addr().String())).Open(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	// The first message may carry bytes as well as the target.
	for _, msg := range []*pb.TunnelData{
		{Target: pb.TunnelTarget_TUNNEL_TARGET_SWARM, Data: []byte("one")},
		{Data: []byte("two")},
	} {
		if err := stream.Send(msg); err != nil {
			t.Fatal(err)
		}
		reply, err := stream.Recv()
		if err != nil || string(reply.GetData()) != "heard: "+string(msg.GetData()) {
			t.Fatalf("reply %q, %v", reply.GetData(), err)
		}
	}
}

func TestTunnelsThatCannotBeMade(t *testing.T) {
	closed, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	closed.Close()

	for _, tt := range []struct {
		name  string
		swarm func(context.Context) (string, error)
		first *pb.TunnelData
		want  codes.Code
	}{
		{"no target named", at(closed.Addr().String()), &pb.TunnelData{Data: []byte("hello")}, codes.InvalidArgument},
		{"a node with no private network", nil, &pb.TunnelData{Target: pb.TunnelTarget_TUNNEL_TARGET_SWARM}, codes.FailedPrecondition},
		{"a Kubo that cannot be found", func(context.Context) (string, error) { return "", errors.New("kubo is restarting") },
			&pb.TunnelData{Target: pb.TunnelTarget_TUNNEL_TARGET_SWARM}, codes.Unavailable},
		{"a Kubo that is not listening", at(closed.Addr().String()), &pb.TunnelData{Target: pb.TunnelTarget_TUNNEL_TARGET_SWARM}, codes.Unavailable},
	} {
		stream, err := startTunnels(t, tt.swarm).Open(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if err := stream.Send(tt.first); err != nil {
			t.Fatal(err)
		}
		if _, err := stream.Recv(); status.Code(err) != tt.want {
			t.Errorf("%s: %v, want %v", tt.name, err, tt.want)
		}
	}

	// A caller that opens a tunnel and says nothing at all.
	stream, err := startTunnels(t, at(closed.Addr().String())).Open(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	stream.CloseSend()
	if _, err := stream.Recv(); status.Code(err) != codes.InvalidArgument {
		t.Errorf("a silent caller: %v, want InvalidArgument", err)
	}
}

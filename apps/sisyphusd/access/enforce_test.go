package access

import (
	"context"
	"io"
	"net"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"

	"github.com/sisyphus-network/Sisyphus/packages/identity"
	pb "github.com/sisyphus-network/Sisyphus/packages/protocol/sisyphus/v1"
)

func newIdentity(t *testing.T) *identity.Identity {
	t.Helper()
	ident, _, err := identity.LoadOrCreate(filepath.Join(t.TempDir(), "node.key"))
	if err != nil {
		t.Fatal(err)
	}
	return ident
}

// echo answers two calls with the ID of whoever made them, so tests can see
// both that a call got through and who the server took the caller for.
type echo struct {
	pb.UnimplementedNodeServiceServer
	pb.UnimplementedBlobServiceServer
}

func (echo) ListNodes(ctx context.Context, _ *pb.ListNodesRequest) (*pb.ListNodesResponse, error) {
	return &pb.ListNodesResponse{Nodes: []*pb.NodeInfo{{NodeId: Caller(ctx)}}}, nil
}

func (echo) Get(_ *pb.GetBlobRequest, stream grpc.ServerStreamingServer[pb.GetBlobResponse]) error {
	return stream.Send(&pb.GetBlobResponse{Data: []byte(Caller(stream.Context()))})
}

// guarded is a server protected by an access list, and the nodes that call it.
type guarded struct {
	t      *testing.T
	addr   string
	server *identity.Identity
	list   *List
}

func startGuarded(t *testing.T) *guarded {
	t.Helper()
	server := newIdentity(t)
	list, err := Open(InMemory(), server.ID())
	if err != nil {
		t.Fatal(err)
	}
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := grpc.NewServer(append(list.ServerOptions(), grpc.Creds(credentials.NewTLS(server.ServerTLS())))...)
	pb.RegisterNodeServiceServer(srv, echo{})
	pb.RegisterBlobServiceServer(srv, echo{})
	pb.RegisterPoolServiceServer(srv, pb.UnimplementedPoolServiceServer{})
	pb.RegisterCoordinatorServiceServer(srv, pb.UnimplementedCoordinatorServiceServer{})
	go srv.Serve(lis)
	t.Cleanup(srv.Stop)
	return &guarded{t: t, addr: lis.Addr().String(), server: server, list: list}
}

func (g *guarded) dialAs(ident *identity.Identity) *grpc.ClientConn {
	g.t.Helper()
	conn, err := grpc.NewClient(g.addr, grpc.WithTransportCredentials(credentials.NewTLS(ident.ClientTLS(g.server.ID()))))
	if err != nil {
		g.t.Fatal(err)
	}
	g.t.Cleanup(func() { conn.Close() })
	return conn
}

// try makes each kind of call over conn and reports how each ended. A call
// that got past the guard to a stand-in with no behaviour of its own ends
// Unimplemented; one that was refused ends PermissionDenied.
func try(conn *grpc.ClientConn) map[string]codes.Code {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	result := make(map[string]codes.Code)

	_, err := pb.NewNodeServiceClient(conn).ListNodes(ctx, &pb.ListNodesRequest{})
	result["list nodes"] = status.Code(err)
	_, err = pb.NewNodeServiceClient(conn).SubmitJob(ctx, &pb.SubmitJobRequest{})
	result["submit job"] = status.Code(err)
	_, err = pb.NewBlobServiceClient(conn).Stat(ctx, &pb.StatBlobRequest{})
	result["stat blob"] = status.Code(err)
	_, err = pb.NewBlobServiceClient(conn).CollectGarbage(ctx, &pb.CollectGarbageRequest{})
	result["collect garbage"] = status.Code(err)
	_, err = pb.NewPoolServiceClient(conn).Join(ctx, &pb.JoinRequest{})
	result["join"] = status.Code(err)
	_, err = pb.NewPoolServiceClient(conn).Invite(ctx, &pb.InviteRequest{})
	result["invite"] = status.Code(err)
	_, err = pb.NewPoolServiceClient(conn).Swarm(ctx, &pb.SwarmRequest{})
	result["swarm key"] = status.Code(err)
	_, err = pb.NewPoolServiceClient(conn).RemoveMember(ctx, &pb.RemoveMemberRequest{})
	result["remove member"] = status.Code(err)

	_, err = pb.NewBlobServiceClient(conn).Replicate(ctx, &pb.ReplicateRequest{})
	result["ask what to hold"] = status.Code(err)
	_, err = pb.NewBlobServiceClient(conn).Replicas(ctx, &pb.ReplicasRequest{})
	result["list copies"] = status.Code(err)
	_, err = pb.NewBlobServiceClient(conn).Restore(ctx, &pb.RestoreRequest{})
	result["restore"] = status.Code(err)

	get, err := pb.NewBlobServiceClient(conn).Get(ctx, &pb.GetBlobRequest{})
	if err == nil {
		_, err = get.Recv()
	}
	result["get blob"] = status.Code(err)
	work, err := pb.NewCoordinatorServiceClient(conn).Connect(ctx)
	if err == nil {
		_, err = work.Recv()
	}
	result["take tasks"] = status.Code(err)
	return result
}

func TestEachRoleMayMakeOnlyItsOwnCalls(t *testing.T) {
	g := startGuarded(t)
	worker, client, stranger := newIdentity(t), newIdentity(t), newIdentity(t)
	if err := g.list.Admit(worker.ID(), Worker, now); err != nil {
		t.Fatal(err)
	}
	if err := g.list.Admit(client.ID(), Client, now); err != nil {
		t.Fatal(err)
	}

	const yes, no, stub = codes.OK, codes.PermissionDenied, codes.Unimplemented
	tests := []struct {
		who   string
		ident *identity.Identity
		want  map[string]codes.Code
	}{
		{"the owner", g.server, map[string]codes.Code{
			"list nodes": yes, "submit job": stub, "stat blob": stub, "get blob": yes, "collect garbage": stub,
			"take tasks": stub, "join": stub, "invite": stub, "remove member": stub, "swarm key": stub,
			"ask what to hold": stub, "list copies": stub, "restore": stub,
		}},
		{"a worker", worker, map[string]codes.Code{
			"list nodes": no, "submit job": no, "stat blob": stub, "get blob": yes, "collect garbage": no,
			"take tasks": stub, "join": stub, "invite": no, "remove member": no, "swarm key": stub,
			"ask what to hold": stub, "list copies": no, "restore": no,
		}},
		{"a client", client, map[string]codes.Code{
			"list nodes": yes, "submit job": stub, "stat blob": stub, "get blob": yes, "collect garbage": stub,
			"take tasks": no, "join": stub, "invite": no, "remove member": no, "swarm key": stub,
			"ask what to hold": no, "list copies": stub, "restore": stub,
		}},
		{"a node never admitted", stranger, map[string]codes.Code{
			"list nodes": no, "submit job": no, "stat blob": no, "get blob": no, "collect garbage": no,
			"take tasks": no, "join": stub, "invite": no, "remove member": no, "swarm key": no,
			"ask what to hold": no, "list copies": no, "restore": no,
		}},
	}
	for _, tt := range tests {
		got := try(g.dialAs(tt.ident))
		for call, want := range tt.want {
			if got[call] != want {
				t.Errorf("%s, %s: ended %v, want %v", tt.who, call, got[call], want)
			}
		}
	}
}

func TestHandlersLearnWhoIsCalling(t *testing.T) {
	g := startGuarded(t)
	client := newIdentity(t)
	if err := g.list.Admit(client.ID(), Client, now); err != nil {
		t.Fatal(err)
	}
	conn := g.dialAs(client)
	ctx := context.Background()

	listed, err := pb.NewNodeServiceClient(conn).ListNodes(ctx, &pb.ListNodesRequest{})
	if err != nil || listed.GetNodes()[0].GetNodeId() != client.ID() {
		t.Errorf("a plain call's handler saw %v (%v), want %s", listed, err, client.ID())
	}
	stream, err := pb.NewBlobServiceClient(conn).Get(ctx, &pb.GetBlobRequest{})
	if err != nil {
		t.Fatal(err)
	}
	msg, err := stream.Recv()
	if err != nil || string(msg.GetData()) != client.ID() {
		t.Errorf("a streaming call's handler saw %q (%v), want %s", msg.GetData(), err, client.ID())
	}
	if _, err := stream.Recv(); err != io.EOF {
		t.Errorf("stream ended with %v", err)
	}

	if got := Caller(ctx); got != "" {
		t.Errorf("Caller outside any call is %q", got)
	}
}

func TestRefusalsSayWhoWasRefusedAndWhy(t *testing.T) {
	g := startGuarded(t)
	worker, stranger := newIdentity(t), newIdentity(t)
	if err := g.list.Admit(worker.ID(), Worker, now); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	_, err := pb.NewNodeServiceClient(g.dialAs(worker)).ListNodes(ctx, &pb.ListNodesRequest{})
	if want := "node " + worker.ID() + " is admitted as a worker, which may not call ListNodes"; err == nil || !strings.Contains(err.Error(), want) {
		t.Errorf("a worker's refusal: %v, want %q", err, want)
	}
	_, err = pb.NewNodeServiceClient(g.dialAs(stranger)).ListNodes(ctx, &pb.ListNodesRequest{})
	if want := "node " + stranger.ID() + " has not been admitted"; err == nil || !strings.Contains(err.Error(), want) {
		t.Errorf("a stranger's refusal: %v, want %q", err, want)
	}
}

func TestACallerWhoCannotBeIdentifiedIsRefused(t *testing.T) {
	l, err := Open(InMemory(), "owner-id")
	if err != nil {
		t.Fatal(err)
	}
	for name, ctx := range map[string]context.Context{
		"no connection details at all": context.Background(),
		"a connection that is not TLS": peer.NewContext(context.Background(), &peer.Peer{}),
	} {
		if _, err := l.authorize(ctx, pb.NodeService_ListNodes_FullMethodName); status.Code(err) != codes.Unauthenticated {
			t.Errorf("%s: %v, want Unauthenticated", name, err)
		}
	}
}

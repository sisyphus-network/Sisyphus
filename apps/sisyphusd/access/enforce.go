package access

import (
	"context"
	"strings"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"

	"github.com/sisyphus-network/Sisyphus/packages/identity"
	pb "github.com/sisyphus-network/Sisyphus/packages/protocol/sisyphus/v1"
)

// allowed lists, for each call, the roles that may make it. The owner may
// make every call and is not listed. A call that is not here at all is open
// to the owner only, so a new one is closed until someone decides otherwise.
var allowed = map[string][]Role{
	// Anyone who can prove who they are may try to join; the token decides.
	pb.PoolService_Join_FullMethodName: {Worker, Client, anyone},

	// The swarm key lets its holder read everything on the pool's private
	// network, which is what members are admitted to do.
	pb.PoolService_Swarm_FullMethodName: {Worker, Client},

	// A tunnel reaches this node's Kubo, where the swarm key is still
	// needed to say anything.
	pb.TunnelService_Open_FullMethodName: {Worker, Client},

	pb.CoordinatorService_Connect_FullMethodName: {Worker},

	pb.BlobService_Put_FullMethodName:  {Worker, Client},
	pb.BlobService_Get_FullMethodName:  {Worker, Client},
	pb.BlobService_Stat_FullMethodName: {Worker, Client},
	// Workers fetch from each other, and ask who is a fellow member before
	// serving one that asks.
	pb.BlobService_Locate_FullMethodName:   {Worker},
	pb.PoolService_IsMember_FullMethodName: {Worker},
	// And keep a place on the members that relay.
	pb.PoolService_Relays_FullMethodName: {Worker},
	// A storage follower asks what to hold. Asking changes nothing that
	// this node keeps.
	pb.BlobService_Replicate_FullMethodName: {Worker},

	pb.BlobService_Pin_FullMethodName:            {Client},
	pb.BlobService_Unpin_FullMethodName:          {Client},
	pb.BlobService_ListPins_FullMethodName:       {Client},
	pb.BlobService_CollectGarbage_FullMethodName: {Client},
	pb.BlobService_Replicas_FullMethodName:       {Client},
	pb.BlobService_Restore_FullMethodName:        {Client},
	// Which members hold what is pinned is part of managing stored data.
	pb.PoolService_ClusterStatus_FullMethodName: {Client},

	// A member publishes under its own name, which the service holds it to,
	// and resolves the names of the pool.
	pb.NameService_Publish_FullMethodName: {Worker, Client},
	pb.NameService_Resolve_FullMethodName: {Worker, Client},

	pb.NodeService_SubmitJob_FullMethodName:      {Client},
	pb.NodeService_GetJob_FullMethodName:         {Client},
	pb.NodeService_WatchJob_FullMethodName:       {Client},
	pb.NodeService_CancelJob_FullMethodName:      {Client},
	pb.NodeService_WatchJobEvents_FullMethodName: {Client},
	pb.NodeService_ListNodes_FullMethodName:      {Client},
}

// anyone stands for a node that has no role yet.
const anyone Role = ""

// Identify returns the node ID of whoever is making the call that ctx
// belongs to, from the key it connected with.
func Identify(ctx context.Context) (string, error) {
	p, _ := peer.FromContext(ctx)
	var tlsInfo credentials.TLSInfo
	if p != nil {
		tlsInfo, _ = p.AuthInfo.(credentials.TLSInfo)
	}
	id, err := identity.PeerID(tlsInfo.State)
	if err != nil {
		return "", status.Errorf(codes.Unauthenticated, "cannot tell who is calling: %v", err)
	}
	return id, nil
}

// authorize identifies the caller of a method and checks it may call it,
// returning the caller's ID.
func (l *List) authorize(ctx context.Context, method string) (string, error) {
	id, err := Identify(ctx)
	if err != nil {
		return "", err
	}
	role, _ := l.Role(id)
	if role == Owner {
		return id, nil
	}
	for _, permitted := range allowed[method] {
		if role == permitted {
			return id, nil
		}
	}
	name := method[strings.LastIndex(method, "/")+1:]
	if role == anyone {
		return "", status.Errorf(codes.PermissionDenied, "node %s has not been admitted to this node", id)
	}
	return "", status.Errorf(codes.PermissionDenied, "node %s is admitted as a %s, which may not call %s", id, role, name)
}

type callerKey struct{}

// Caller returns the ID of the node making the call that ctx belongs to, or
// "" if the call did not pass through this package's interceptors.
func Caller(ctx context.Context) string {
	id, _ := ctx.Value(callerKey{}).(string)
	return id
}

// ServerOptions returns the gRPC options that make a server identify every
// caller and refuse calls its role does not permit. Handlers can find out
// who is calling with Caller.
func (l *List) ServerOptions() []grpc.ServerOption {
	return []grpc.ServerOption{
		grpc.ChainUnaryInterceptor(func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
			id, err := l.authorize(ctx, info.FullMethod)
			if err != nil {
				return nil, err
			}
			return handler(context.WithValue(ctx, callerKey{}, id), req)
		}),
		grpc.ChainStreamInterceptor(func(srv any, stream grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
			id, err := l.authorize(stream.Context(), info.FullMethod)
			if err != nil {
				return err
			}
			return handler(srv, &identifiedStream{ServerStream: stream, ctx: context.WithValue(stream.Context(), callerKey{}, id)})
		}),
	}
}

// identifiedStream is a stream whose context says who the caller is.
type identifiedStream struct {
	grpc.ServerStream
	ctx context.Context
}

func (s *identifiedStream) Context() context.Context { return s.ctx }

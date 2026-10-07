package api

import (
	"context"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/status"

	"github.com/excho0/Sisyphus/apps/sisyphusd/access"
	"github.com/excho0/Sisyphus/packages/identity"
	pb "github.com/excho0/Sisyphus/packages/protocol/sisyphus/v1"
	"github.com/excho0/Sisyphus/packages/storage"
)

// NewPeerServer returns a gRPC server by which a worker lets the other
// members of its pool fetch blobs from its store, so that they need not all
// come from the coordinator. It answers only Get and Stat, and only for
// callers that isMember vouches for. A worker does not keep the list of
// members itself; isMember asks whoever does.
func NewPeerServer(ident *identity.Identity, store *storage.Store, isMember func(ctx context.Context, nodeID string) (bool, error)) *grpc.Server {
	// check refuses a call unless it comes from a member.
	check := func(ctx context.Context) error {
		caller, err := access.Identify(ctx)
		if err != nil {
			return err
		}
		member, err := isMember(ctx, caller)
		if err != nil {
			return status.Errorf(codes.Unavailable, "cannot check whether node %s is a member of the pool: %v", caller, err)
		}
		if !member {
			return status.Errorf(codes.PermissionDenied, "node %s is not a member of this node's pool", caller)
		}
		return nil
	}
	srv := grpc.NewServer(
		grpc.Creds(credentials.NewTLS(ident.ServerTLS())),
		grpc.UnaryInterceptor(func(ctx context.Context, req any, _ *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
			if err := check(ctx); err != nil {
				return nil, err
			}
			return handler(ctx, req)
		}),
		grpc.StreamInterceptor(func(srv any, stream grpc.ServerStream, _ *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
			if err := check(stream.Context()); err != nil {
				return err
			}
			return handler(srv, stream)
		}),
	)
	pb.RegisterBlobServiceServer(srv, &peerBlobService{blobs: blobService{store: store}})
	return srv
}

// peerBlobService is the read-only part of the blob service. Every other
// call is refused as unimplemented.
type peerBlobService struct {
	pb.UnimplementedBlobServiceServer
	blobs blobService
}

func (s *peerBlobService) Get(req *pb.GetBlobRequest, stream grpc.ServerStreamingServer[pb.GetBlobResponse]) error {
	return s.blobs.Get(req, stream)
}

func (s *peerBlobService) Stat(ctx context.Context, req *pb.StatBlobRequest) (*pb.StatBlobResponse, error) {
	return s.blobs.Stat(ctx, req)
}

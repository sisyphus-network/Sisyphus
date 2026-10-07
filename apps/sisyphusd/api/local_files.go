package api

import (
	"context"
	"time"

	"github.com/ipfs/go-cid"
	"github.com/libp2p/go-libp2p/core/peer"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/sisyphus-network/Sisyphus/apps/sisyphusd/access"
	"github.com/sisyphus-network/Sisyphus/packages/nodedb"
	nodepb "github.com/sisyphus-network/Sisyphus/packages/protocol/sisyphus/node/v1"
)

// FileList is where a node keeps the names of the files stored through
// the local API. A nodedb.DB is one.
type FileList interface {
	Files() ([]nodedb.File, error)
	AddFile(nodedb.File) error
	RemoveFile(cid string) error
}

// errNoStore is the answer to a call about files on a node that keeps no
// store for a pool.
var errNoStore = status.Error(codes.FailedPrecondition, "this node coordinates no pool, so it has no store of files")

// StoreFile puts what the caller sends in the store, keeps it there, and
// lists it under the name given.
func (s *localService) StoreFile(stream grpc.ClientStreamingServer[nodepb.StoreFileRequest, nodepb.File]) error {
	ctx := stream.Context()
	if err := s.authorize(ctx); err != nil {
		return err
	}
	if s.cfg.Store == nil {
		return errNoStore
	}
	var name string
	c, size, err := storeUpload(ctx, s.cfg.Store, s.cfg.MaxStoreBytes, func() ([]byte, error) {
		msg, err := stream.Recv()
		if name == "" {
			name = msg.GetName()
		}
		return msg.GetData(), err
	})
	if err != nil {
		return err
	}
	// Held for the user, as `blob pin` holds it, until it is removed.
	if err := s.cfg.Store.Pin(ctx, userOwner, time.Time{}, c); err != nil {
		return status.Errorf(codes.Internal, "keep file: %v", err)
	}
	file := nodedb.File{CID: c.String(), Name: name, Size: size, Stored: time.Now()}
	if err := s.cfg.Files.AddFile(file); err != nil {
		return status.Errorf(codes.Internal, "%v", err)
	}
	return stream.SendAndClose(localFile(file))
}

// FetchFile sends what is stored under a CID. It needs the token, as the
// calls that only describe the node do not: this is the content of the
// owner's files.
func (s *localService) FetchFile(req *nodepb.FetchFileRequest, stream grpc.ServerStreamingServer[nodepb.FetchFileResponse]) error {
	if err := s.authorize(stream.Context()); err != nil {
		return err
	}
	if s.cfg.Store == nil {
		return errNoStore
	}
	return sendBlob(stream.Context(), s.cfg.Store, req.GetCid(), func(data []byte) error {
		return stream.Send(&nodepb.FetchFileResponse{Data: data})
	})
}

func (s *localService) ListFiles(context.Context, *nodepb.ListFilesRequest) (*nodepb.ListFilesResponse, error) {
	if s.cfg.Store == nil {
		return nil, errNoStore
	}
	files, err := s.cfg.Files.Files()
	if err != nil {
		return nil, status.Errorf(codes.Internal, "%v", err)
	}
	var res nodepb.ListFilesResponse
	for _, file := range files {
		res.Files = append(res.Files, localFile(file))
	}
	return &res, nil
}

func (s *localService) RemoveFile(ctx context.Context, req *nodepb.RemoveFileRequest) (*nodepb.RemoveFileResponse, error) {
	if err := s.authorize(ctx); err != nil {
		return nil, err
	}
	if s.cfg.Store == nil {
		return nil, errNoStore
	}
	c, err := cid.Decode(req.GetCid())
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid CID %q: %v", req.GetCid(), err)
	}
	// Off the list first: a file listed but no longer kept would be a
	// promise the store does not make.
	if err := s.cfg.Files.RemoveFile(c.String()); err != nil {
		return nil, status.Errorf(codes.Internal, "%v", err)
	}
	if err := s.cfg.Store.Unpin(userOwner, c); err != nil {
		return nil, status.Errorf(codes.Internal, "stop keeping file: %v", err)
	}
	return &nodepb.RemoveFileResponse{}, nil
}

func localFile(file nodedb.File) *nodepb.File {
	return &nodepb.File{Cid: file.CID, Name: file.Name, SizeBytes: file.Size, StoredAtMs: file.Stored.UnixMilli()}
}

var (
	toLocalRole   = map[access.Role]nodepb.PoolRole{access.Worker: nodepb.PoolRole_POOL_ROLE_WORKER, access.Client: nodepb.PoolRole_POOL_ROLE_CLIENT}
	fromLocalRole = map[nodepb.PoolRole]access.Role{nodepb.PoolRole_POOL_ROLE_WORKER: access.Worker, nodepb.PoolRole_POOL_ROLE_CLIENT: access.Client}
)

// CreateInvitation makes an invitation to the pool, as `pool invite` does.
func (s *localService) CreateInvitation(ctx context.Context, req *nodepb.CreateInvitationRequest) (*nodepb.CreateInvitationResponse, error) {
	if err := s.authorize(ctx); err != nil {
		return nil, err
	}
	if s.cfg.Pool == nil {
		return nil, errNoPool
	}
	ttl := defaultInviteTTL
	if req.GetTtlSeconds() > 0 {
		ttl = time.Duration(req.GetTtlSeconds()) * time.Second
	}
	now := time.Now()
	pool := s.cfg.Pool.service
	token, err := pool.access.Invite(fromLocalRole[req.GetRole()], ttl, now)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	return &nodepb.CreateInvitationResponse{Invitation: pool.id + ":" + token, ExpiresAtMs: now.Add(ttl).UnixMilli()}, nil
}

func (s *localService) ListMembers(context.Context, *nodepb.ListMembersRequest) (*nodepb.ListMembersResponse, error) {
	if s.cfg.Pool == nil {
		return nil, errNoPool
	}
	var res nodepb.ListMembersResponse
	for _, m := range s.cfg.Pool.service.access.Members() {
		res.Members = append(res.Members, &nodepb.PoolMember{PeerId: m.ID, Role: toLocalRole[m.Role], JoinedAtMs: m.Joined.UnixMilli()})
	}
	return &res, nil
}

// RemoveMember takes a node out of the pool in every respect, as `pool
// remove` does.
func (s *localService) RemoveMember(ctx context.Context, req *nodepb.RemoveMemberRequest) (*nodepb.RemoveMemberResponse, error) {
	if err := s.authorize(ctx); err != nil {
		return nil, err
	}
	if s.cfg.Pool == nil {
		return nil, errNoPool
	}
	if _, err := peer.Decode(req.GetPeerId()); err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "%q is not a node ID", req.GetPeerId())
	}
	if err := s.cfg.Pool.service.removeMember(ctx, req.GetPeerId()); err != nil {
		return nil, err
	}
	return &nodepb.RemoveMemberResponse{}, nil
}

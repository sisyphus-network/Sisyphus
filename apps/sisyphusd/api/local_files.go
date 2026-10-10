package api

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"time"

	"github.com/ipfs/go-cid"
	"github.com/libp2p/go-libp2p/core/peer"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/sisyphus-network/Sisyphus/apps/sisyphusd/access"
	"github.com/sisyphus-network/Sisyphus/packages/nodedb"
	nodepb "github.com/sisyphus-network/Sisyphus/packages/protocol/sisyphus/node/v1"
	"github.com/sisyphus-network/Sisyphus/packages/sealed"
	"github.com/sisyphus-network/Sisyphus/packages/storage"
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

const chatDraftOwner = "chat-draft"
const chatDraftRetention = 24 * time.Hour

// StoreFile puts what the caller sends in the store, sealed first if it is
// to be private, keeps it there, and lists it under the name given.
func (s *localService) StoreFile(stream grpc.ClientStreamingServer[nodepb.StoreFileRequest, nodepb.File]) error {
	ctx := stream.Context()
	if err := s.authorize(ctx); err != nil {
		return err
	}
	if s.cfg.Store == nil {
		return errNoStore
	}
	// The first message says what the file is called and whether to seal
	// it, and may hold the start of it too. A caller that sends nothing has
	// stored an empty file with no name.
	first, ended := stream.Recv()
	if ended != nil && !errors.Is(ended, io.EOF) {
		return ended
	}
	start, started := first.GetData(), false
	if first.GetChatAttachment() && !first.GetPrivate() {
		return status.Error(codes.InvalidArgument, "chat attachments must be private")
	}
	file, err := s.keep(ctx, first.GetName(), first.GetPrivate(), first.GetChatAttachment(), func() ([]byte, error) {
		if !started {
			started = true
			return start, ended
		}
		msg, err := stream.Recv()
		return msg.GetData(), err
	})
	if err != nil {
		return err
	}
	return stream.SendAndClose(file)
}

// keep stores a file that arrives in pieces, sealed if it is private, and
// puts it on record as one of the user's. recv gives each piece and then
// io.EOF.
func (s *localService) keep(ctx context.Context, name string, private, attachment bool, recv func() ([]byte, error)) (*nodepb.File, error) {
	as := plain
	if private {
		key, err := s.sealingKey()
		if err != nil {
			return nil, err
		}
		as = func(sent io.Reader) io.Reader { return sealed.Encrypt(key, sent) }
	}
	c, size, err := storeUpload(ctx, s.cfg.Store, s.cfg.MaxStoreBytes, as, recv)
	if err != nil {
		return nil, err
	}
	// Drafts never receive a permanent user pin. Identical concurrent draft
	// uploads share a bounded grace pin; do not roll it back by CID, because
	// another upload may still be using it. Each chat gains its own pin at Ask.
	owner, expires := userOwner, time.Time{}
	if attachment {
		owner, expires = chatDraftOwner, time.Now().Add(chatDraftRetention)
	}
	if err := s.cfg.Store.Pin(ctx, owner, expires, c); err != nil {
		return nil, status.Errorf(codes.Internal, "keep file: %v", err)
	}
	file := nodedb.File{CID: c.String(), Name: name, Size: size, Stored: time.Now(), Private: private}
	if err := s.cfg.Files.AddFile(file); err != nil {
		return nil, status.Errorf(codes.Internal, "%v", err)
	}
	return localFile(file), nil
}

// errNoKey is the answer to a call for something private on a node that
// keeps no key to seal with.
var errNoKey = status.Error(codes.FailedPrecondition, "this node keeps no key to seal private files and jobs with")

// sealingKey returns the key this node seals with.
func (s *localService) sealingKey() (sealed.Key, error) {
	if s.cfg.SealingKey == nil {
		return sealed.Key{}, errNoKey
	}
	key, err := s.cfg.SealingKey()
	if err != nil {
		return sealed.Key{}, status.Errorf(codes.Internal, "the node's sealing key: %v", err)
	}
	return key, nil
}

// FetchFile sends what is stored under a CID, unsealed if it was sealed
// with this node's key: a private file, or what a private job produced.
// It needs the token, as the calls that only describe the node do not:
// this is the content of the owner's files.
func (s *localService) FetchFile(req *nodepb.FetchFileRequest, stream grpc.ServerStreamingServer[nodepb.FetchFileResponse]) error {
	ctx := stream.Context()
	if err := s.authorize(ctx); err != nil {
		return err
	}
	if s.cfg.Store == nil {
		return errNoStore
	}
	blob, err := openBlob(ctx, s.cfg.Store, req.GetCid())
	if err != nil {
		return err
	}
	defer blob.Close()
	send := func(data []byte) error { return stream.Send(&nodepb.FetchFileResponse{Data: data}) }

	// A sealed blob says so in its first bytes.
	start := make([]byte, 8)
	n, _ := io.ReadFull(blob, start)
	if !sealed.IsSealed(start[:n]) {
		if _, err := blob.Seek(0, io.SeekStart); err != nil {
			return status.Errorf(codes.Internal, "read blob: %v", err)
		}
		return sendAll(blob, send)
	}
	key, err := s.sealingKey()
	if err != nil {
		return err
	}
	opened, err := sealed.Open(key, blob, blob.Size())
	if err != nil {
		return status.Errorf(codes.FailedPrecondition, "blob %s: %v", req.GetCid(), err)
	}
	return sendAll(opened, send)
}

func (s *localService) ListFiles(ctx context.Context, _ *nodepb.ListFilesRequest) (*nodepb.ListFilesResponse, error) {
	if s.cfg.Store == nil {
		return nil, errNoStore
	}
	files, err := s.cfg.Files.Files()
	if err != nil {
		return nil, status.Errorf(codes.Internal, "%v", err)
	}
	var res nodepb.ListFilesResponse
	// Pins are local metadata even for remote stores. Only unprotected rows
	// need a storage read to distinguish a collected draft from a live file.
	kept := make(map[string]bool)
	if pins, ok := s.cfg.Store.(interface{ Pins() []storage.Pin }); ok {
		now := time.Now()
		for _, pin := range pins.Pins() {
			if pin.Expires.IsZero() || pin.Expires.After(now) {
				kept[pin.CID.String()] = true
			}
		}
	}
	for _, file := range files {
		c, err := cid.Decode(file.CID)
		if err != nil {
			slog.Warn("skipping invalid file catalogue CID", "error", err)
			continue
		}
		if kept[c.String()] {
			res.Files = append(res.Files, localFile(file))
			continue
		}
		// Collection can remove an abandoned upload while its metadata remains.
		// Do not delete that metadata from a read: a concurrent upload/restore
		// may be using it, and chat references must remain intact.
		blob, err := s.cfg.Store.Open(ctx, c)
		if errors.Is(err, storage.ErrNotFound) {
			continue
		}
		if err != nil {
			return nil, status.Errorf(codes.Internal, "check listed file: %v", err)
		}
		blob.Close()
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
		if errors.Is(err, nodedb.ErrFileReferenced) {
			return nil, status.Error(codes.FailedPrecondition, "file is still referenced; release its owners before removing it")
		}
		return nil, status.Errorf(codes.Internal, "%v", err)
	}
	if err := unpinForUser(s.cfg.Store, c); err != nil {
		return nil, status.Errorf(codes.Internal, "stop keeping file: %v", err)
	}
	return &nodepb.RemoveFileResponse{}, nil
}

func localFile(file nodedb.File) *nodepb.File {
	return &nodepb.File{Cid: file.CID, Name: file.Name, SizeBytes: file.Size, StoredAtMs: file.Stored.UnixMilli(), Private: file.Private}
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

// errNoWorker is the answer to a call to join a pool on a node that could
// not work for it.
var errNoWorker = status.Error(codes.FailedPrecondition, "this node runs no worker that takes work from other nodes, so it cannot join a pool; start it with a role that includes worker")

// JoinPool has this node join another node's pool with an invitation and
// start working for it.
func (s *localService) JoinPool(ctx context.Context, req *nodepb.JoinPoolRequest) (*nodepb.JoinPoolResponse, error) {
	if err := s.authorize(ctx); err != nil {
		return nil, err
	}
	if s.cfg.Join == nil {
		return nil, errNoWorker
	}
	if req.GetAddress() == "" || req.GetInvitation() == "" {
		return nil, status.Error(codes.InvalidArgument, "joining a pool takes the address of the node that runs it and an invitation from it")
	}
	id, role, err := s.cfg.Join(ctx, req.GetAddress(), req.GetInvitation())
	if err != nil {
		return nil, asError(err)
	}
	return &nodepb.JoinPoolResponse{PeerId: id, Role: toLocalRole[role]}, nil
}

package api

import (
	"context"
	"errors"
	"io"
	"time"

	"github.com/ipfs/go-cid"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/sisyphus-network/Sisyphus/apps/sisyphusd/access"
	pb "github.com/sisyphus-network/Sisyphus/packages/protocol/sisyphus/v1"
	"github.com/sisyphus-network/Sisyphus/packages/storage"
)

// blobChunkSize is how much of a blob goes in one stream message, well under
// gRPC's default 4 MiB message limit.
const blobChunkSize = 256 << 10

// userOwner is who pins made through the service are held for.
const userOwner = "user"

type blobService struct {
	pb.UnimplementedBlobServiceServer
	store blobStore
	// quota is the most disk the store may use; zero means no limit.
	quota uint64
	// holders names the workers likely to hold a blob, other than the one
	// asking.
	holders func(blob, asker string) []*pb.BlobHolder
}

// FileStore is the part of a storage.Store needed to put things in it,
// read them back, and keep them there.
type FileStore interface {
	Open(ctx context.Context, c cid.Cid) (storage.Blob, error)
	Put(ctx context.Context, r io.Reader) (cid.Cid, error)
	Size(ctx context.Context) (uint64, error)
	Pin(ctx context.Context, owner string, expires time.Time, cids ...cid.Cid) error
	Unpin(owner string, cids ...cid.Cid) error
}

// blobStore is the part of a storage.Store that the service uses.
type blobStore interface {
	FileStore
	Pins() []storage.Pin
	GC(ctx context.Context, now time.Time) (storage.Collected, error)
}

func (s *blobService) Put(stream grpc.ClientStreamingServer[pb.PutBlobRequest, pb.PutBlobResponse]) error {
	c, size, err := storeUpload(stream.Context(), s.store, s.quota, func() ([]byte, error) {
		msg, err := stream.Recv()
		return msg.GetData(), err
	})
	if err != nil {
		return err
	}
	return stream.SendAndClose(&pb.PutBlobResponse{Cid: c.String(), Size: size})
}

// storeUpload stores what recv returns, piece by piece until it returns io.EOF,
// and says what CID it was stored under and how much there was. With a
// quota, it refuses what would not fit.
func storeUpload(ctx context.Context, store FileStore, quota uint64, recv func() ([]byte, error)) (cid.Cid, uint64, error) {
	upload := &uploadReader{recv: recv}
	if quota > 0 {
		used, err := store.Size(ctx)
		if err != nil {
			return cid.Undef, 0, status.Errorf(codes.Internal, "measure store: %v", err)
		}
		if used >= quota {
			return cid.Undef, 0, status.Errorf(codes.ResourceExhausted, "store is full: %d of %d bytes used", used, quota)
		}
		upload.limit = quota - used
	}
	c, err := store.Put(ctx, upload)
	if err != nil {
		if _, ok := status.FromError(err); ok {
			return cid.Undef, 0, err
		}
		return cid.Undef, 0, status.Errorf(codes.Internal, "store blob: %v", err)
	}
	return c, upload.size, nil
}

// uploadReader presents the data of an upload stream as one io.Reader.
type uploadReader struct {
	recv func() ([]byte, error)
	rest []byte
	size uint64
	// limit is the most bytes the upload may carry; zero means no limit.
	limit uint64
}

func (r *uploadReader) Read(p []byte) (int, error) {
	for len(r.rest) == 0 {
		data, err := r.recv()
		if err != nil {
			return 0, err // io.EOF once the client has sent everything
		}
		r.rest = data
	}
	n := copy(p, r.rest)
	r.rest = r.rest[n:]
	r.size += uint64(n)
	// What was stored before the limit was passed is unpinned, so the next
	// collection removes it.
	if r.limit > 0 && r.size > r.limit {
		return 0, status.Errorf(codes.ResourceExhausted, "upload exceeds the %d bytes left in the store", r.limit)
	}
	return n, nil
}

func (s *blobService) Get(req *pb.GetBlobRequest, stream grpc.ServerStreamingServer[pb.GetBlobResponse]) error {
	return sendBlob(stream.Context(), s.store, req.GetCid(), func(data []byte) error {
		return stream.Send(&pb.GetBlobResponse{Data: data})
	})
}

// sendBlob sends what is stored under a CID, piece by piece.
func sendBlob(ctx context.Context, store FileStore, id string, send func([]byte) error) error {
	blob, err := openBlob(ctx, store, id)
	if err != nil {
		return err
	}
	defer blob.Close()

	buf := make([]byte, blobChunkSize)
	for {
		n, err := io.ReadFull(blob, buf)
		if n > 0 {
			if err := send(buf[:n]); err != nil {
				return err
			}
		}
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return nil
		}
		if err != nil {
			return status.Errorf(codes.Internal, "read blob: %v", err)
		}
	}
}

func (s *blobService) Locate(ctx context.Context, req *pb.LocateBlobRequest) (*pb.LocateBlobResponse, error) {
	return &pb.LocateBlobResponse{Holders: s.holders(req.GetCid(), access.Caller(ctx))}, nil
}

func (s *blobService) Stat(ctx context.Context, req *pb.StatBlobRequest) (*pb.StatBlobResponse, error) {
	blob, err := openBlob(ctx, s.store, req.GetCid())
	if err != nil {
		return nil, err
	}
	defer blob.Close()
	return &pb.StatBlobResponse{Size: blob.Size()}, nil
}

func openBlob(ctx context.Context, store FileStore, id string) (storage.Blob, error) {
	c, err := cid.Decode(id)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid CID %q: %v", id, err)
	}
	blob, err := store.Open(ctx, c)
	if errors.Is(err, storage.ErrNotFound) {
		return nil, status.Errorf(codes.NotFound, "blob %s not found", c)
	}
	if err != nil {
		return nil, status.Errorf(codes.Internal, "open blob: %v", err)
	}
	return blob, nil
}

func (s *blobService) Pin(ctx context.Context, req *pb.PinBlobRequest) (*pb.PinBlobResponse, error) {
	c, err := cid.Decode(req.GetCid())
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid CID %q: %v", req.GetCid(), err)
	}
	var expires time.Time
	if ttl := req.GetTtlSeconds(); ttl > 0 {
		expires = time.Now().Add(time.Duration(ttl) * time.Second)
	}
	err = s.store.Pin(ctx, userOwner, expires, c)
	if errors.Is(err, storage.ErrNotFound) {
		return nil, status.Errorf(codes.NotFound, "blob %s not found", c)
	}
	if err != nil {
		return nil, status.Errorf(codes.Internal, "pin blob: %v", err)
	}
	return &pb.PinBlobResponse{}, nil
}

func (s *blobService) Unpin(_ context.Context, req *pb.UnpinBlobRequest) (*pb.UnpinBlobResponse, error) {
	c, err := cid.Decode(req.GetCid())
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid CID %q: %v", req.GetCid(), err)
	}
	if err := s.store.Unpin(userOwner, c); err != nil {
		return nil, status.Errorf(codes.Internal, "unpin blob: %v", err)
	}
	return &pb.UnpinBlobResponse{}, nil
}

func (s *blobService) ListPins(context.Context, *pb.ListPinsRequest) (*pb.ListPinsResponse, error) {
	var res pb.ListPinsResponse
	for _, pin := range s.store.Pins() {
		listed := &pb.Pin{Cid: pin.CID.String(), Owner: pin.Owner}
		if !pin.Expires.IsZero() {
			listed.ExpiresAt = timestamppb.New(pin.Expires)
		}
		res.Pins = append(res.Pins, listed)
	}
	return &res, nil
}

func (s *blobService) CollectGarbage(ctx context.Context, _ *pb.CollectGarbageRequest) (*pb.CollectGarbageResponse, error) {
	done, err := s.store.GC(ctx, time.Now())
	if err != nil {
		return nil, status.Errorf(codes.Internal, "%v", err)
	}
	return &pb.CollectGarbageResponse{
		ExpiredPins:   uint32(done.ExpiredPins),
		BlocksRemoved: uint64(done.Blocks),
		BytesFreed:    done.Bytes,
	}, nil
}

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

	"github.com/excho0/Sisyphus/apps/sisyphusd/access"
	pb "github.com/excho0/Sisyphus/packages/protocol/sisyphus/v1"
	"github.com/excho0/Sisyphus/packages/storage"
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

// blobStore is the part of a storage.Store that the service uses.
type blobStore interface {
	Open(ctx context.Context, c cid.Cid) (storage.Blob, error)
	Put(ctx context.Context, r io.Reader) (cid.Cid, error)
	Size(ctx context.Context) (uint64, error)
	Pin(ctx context.Context, owner string, expires time.Time, cids ...cid.Cid) error
	Unpin(owner string, cids ...cid.Cid) error
	Pins() []storage.Pin
	GC(ctx context.Context, now time.Time) (storage.Collected, error)
}

func (s *blobService) Put(stream grpc.ClientStreamingServer[pb.PutBlobRequest, pb.PutBlobResponse]) error {
	upload := &uploadReader{stream: stream}
	if s.quota > 0 {
		used, err := s.store.Size(stream.Context())
		if err != nil {
			return status.Errorf(codes.Internal, "measure store: %v", err)
		}
		if used >= s.quota {
			return status.Errorf(codes.ResourceExhausted, "store is full: %d of %d bytes used", used, s.quota)
		}
		upload.limit = s.quota - used
	}
	c, err := s.store.Put(stream.Context(), upload)
	if err != nil {
		if _, ok := status.FromError(err); ok {
			return err
		}
		return status.Errorf(codes.Internal, "store blob: %v", err)
	}
	return stream.SendAndClose(&pb.PutBlobResponse{Cid: c.String(), Size: upload.size})
}

// uploadReader presents the data of an upload stream as one io.Reader.
type uploadReader struct {
	stream grpc.ClientStreamingServer[pb.PutBlobRequest, pb.PutBlobResponse]
	rest   []byte
	size   uint64
	// limit is the most bytes the upload may carry; zero means no limit.
	limit uint64
}

func (r *uploadReader) Read(p []byte) (int, error) {
	for len(r.rest) == 0 {
		msg, err := r.stream.Recv()
		if err != nil {
			return 0, err // io.EOF once the client has sent everything
		}
		r.rest = msg.GetData()
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
	blob, err := s.open(stream.Context(), req.GetCid())
	if err != nil {
		return err
	}
	defer blob.Close()

	buf := make([]byte, blobChunkSize)
	for {
		n, err := io.ReadFull(blob, buf)
		if n > 0 {
			if err := stream.Send(&pb.GetBlobResponse{Data: buf[:n]}); err != nil {
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
	blob, err := s.open(ctx, req.GetCid())
	if err != nil {
		return nil, err
	}
	defer blob.Close()
	return &pb.StatBlobResponse{Size: blob.Size()}, nil
}

func (s *blobService) open(ctx context.Context, id string) (storage.Blob, error) {
	c, err := cid.Decode(id)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid CID %q: %v", id, err)
	}
	blob, err := s.store.Open(ctx, c)
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

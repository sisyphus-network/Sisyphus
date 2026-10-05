package api

import (
	"context"
	"errors"
	"io"

	"github.com/ipfs/go-cid"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pb "github.com/excho0/Sisyphus/packages/protocol/sisyphus/v1"
	"github.com/excho0/Sisyphus/packages/storage"
)

// blobChunkSize is how much of a blob goes in one stream message, well under
// gRPC's default 4 MiB message limit.
const blobChunkSize = 256 << 10

type blobService struct {
	pb.UnimplementedBlobServiceServer
	store *storage.Store
}

func (s *blobService) Put(stream grpc.ClientStreamingServer[pb.PutBlobRequest, pb.PutBlobResponse]) error {
	upload := &uploadReader{stream: stream}
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

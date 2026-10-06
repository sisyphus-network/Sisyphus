// Package blobclient moves blobs to and from a node's BlobService, checking
// everything against its CID so the node at the other end need not be
// trusted to return or store the right bytes.
package blobclient

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/ipfs/go-cid"
	"google.golang.org/grpc"

	pb "github.com/sisyphus-network/Sisyphus/packages/protocol/sisyphus/v1"
	"github.com/sisyphus-network/Sisyphus/packages/storage"
)

// chunkSize is how much of a blob goes in one upload message, well under
// gRPC's default 4 MiB message limit.
const chunkSize = 256 << 10

// Upload sends everything read from r to the node as one blob and returns
// its CID. It fails if the node reports a CID other than that of the bytes
// sent.
func Upload(ctx context.Context, client pb.BlobServiceClient, r io.Reader) (cid.Cid, error) {
	stream, err := client.Put(ctx)
	if err != nil {
		return cid.Undef, err
	}
	hasher := storage.NewHasher(ctx)
	buf := make([]byte, chunkSize)
	for {
		n, readErr := io.ReadFull(r, buf)
		if n > 0 {
			if _, err := hasher.Write(buf[:n]); err != nil {
				return cid.Undef, err
			}
			if err := stream.Send(&pb.PutBlobRequest{Data: buf[:n]}); err != nil {
				// The node's reason for ending the upload comes from
				// CloseAndRecv, not Send.
				break
			}
		}
		if errors.Is(readErr, io.EOF) || errors.Is(readErr, io.ErrUnexpectedEOF) {
			break
		}
		if readErr != nil {
			return cid.Undef, readErr
		}
	}
	stored, err := stream.CloseAndRecv()
	if err != nil {
		return cid.Undef, err
	}
	sent, err := hasher.Sum()
	if err != nil {
		return cid.Undef, err
	}
	if stored.GetCid() != sent.String() {
		return cid.Undef, fmt.Errorf("node stored the upload as %s but the bytes sent hash to %s", stored.GetCid(), sent)
	}
	return sent, nil
}

// Download copies a blob from the node to dst. It fails, after writing, if
// the bytes received do not hash to want, so dst must not be trusted until
// Download returns nil.
func Download(ctx context.Context, client pb.BlobServiceClient, want cid.Cid, dst io.Writer) error {
	src, err := open(ctx, client, want)
	if err != nil {
		return err
	}
	hasher := storage.NewHasher(ctx)
	if _, err := io.Copy(io.MultiWriter(hasher, dst), src); err != nil {
		hasher.Sum()
		return err
	}
	got, err := hasher.Sum()
	if err != nil {
		return err
	}
	return check(got, want)
}

// Putter is somewhere a blob can be stored, such as a storage.Store.
type Putter interface {
	Put(ctx context.Context, r io.Reader) (cid.Cid, error)
}

// Fetch copies a blob from the node into a local store, failing if the bytes
// received do not hash to want.
func Fetch(ctx context.Context, client pb.BlobServiceClient, want cid.Cid, into Putter) error {
	src, err := open(ctx, client, want)
	if err != nil {
		return err
	}
	// Storing computes the CID, so there is no need to hash separately. Bytes
	// that turn out wrong stay in the store under their own, different CID,
	// where nothing will ask for them.
	got, err := into.Put(ctx, src)
	if err != nil {
		return err
	}
	return check(got, want)
}

func check(got, want cid.Cid) error {
	if !got.Equals(want) {
		return fmt.Errorf("node returned data that hashes to %s, not the %s asked for", got, want)
	}
	return nil
}

func open(ctx context.Context, client pb.BlobServiceClient, c cid.Cid) (io.Reader, error) {
	stream, err := client.Get(ctx, &pb.GetBlobRequest{Cid: c.String()})
	if err != nil {
		return nil, err
	}
	return &downloadReader{stream: stream}, nil
}

// downloadReader presents the data of a download stream as one io.Reader.
type downloadReader struct {
	stream grpc.ServerStreamingClient[pb.GetBlobResponse]
	rest   []byte
}

func (r *downloadReader) Read(p []byte) (int, error) {
	for len(r.rest) == 0 {
		msg, err := r.stream.Recv()
		if err != nil {
			return 0, err // io.EOF once the node has sent everything
		}
		r.rest = msg.GetData()
	}
	n := copy(p, r.rest)
	r.rest = r.rest[n:]
	return n, nil
}

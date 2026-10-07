package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/ipfs/go-cid"

	pb "github.com/excho0/Sisyphus/packages/protocol/sisyphus/v1"
	"github.com/excho0/Sisyphus/packages/storage"
)

// uploadChunkSize is how much of a file goes in one upload message.
const uploadChunkSize = 256 << 10

func blobCommand(ctx context.Context, args []string) error {
	if len(args) > 0 {
		switch args[0] {
		case "put":
			return blobPut(ctx, args[1:])
		case "get":
			return blobGet(ctx, args[1:])
		case "stat":
			return blobStat(ctx, args[1:])
		}
	}
	return errors.New(`expected "blob put", "blob get" or "blob stat"`)
}

func blobPut(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("sisyphusd blob put", flag.ContinueOnError)
	addr := fs.String("addr", defaultAddr, "node address")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New(`expected one file to store, or "-" for standard input`)
	}
	var in io.Reader = stdin
	if path := fs.Arg(0); path != "-" {
		file, err := os.Open(path)
		if err != nil {
			return err
		}
		defer file.Close()
		in = file
	}

	conn, err := connect(*addr)
	if err != nil {
		return err
	}
	defer conn.Close()
	stream, err := pb.NewBlobServiceClient(conn).Put(ctx)
	if err != nil {
		return err
	}

	// Hash locally while uploading, so a node cannot hand back a CID for
	// anything other than the bytes sent.
	hasher := storage.NewHasher(ctx)
	buf := make([]byte, uploadChunkSize)
	for {
		n, readErr := io.ReadFull(in, buf)
		if n > 0 {
			if _, err := hasher.Write(buf[:n]); err != nil {
				return err
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
			return readErr
		}
	}
	stored, err := stream.CloseAndRecv()
	if err != nil {
		return err
	}
	sent, err := hasher.Sum()
	if err != nil {
		return err
	}
	if stored.GetCid() != sent.String() {
		return fmt.Errorf("node stored the upload as %s but the bytes sent hash to %s", stored.GetCid(), sent)
	}
	fmt.Fprintln(stdout, stored.GetCid())
	return nil
}

func blobGet(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("sisyphusd blob get", flag.ContinueOnError)
	addr := fs.String("addr", defaultAddr, "node address")
	output := fs.String("o", "", "file to write the blob to (default: standard output)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("expected exactly one CID")
	}
	want, err := cid.Decode(fs.Arg(0))
	if err != nil {
		return fmt.Errorf("invalid CID %q: %w", fs.Arg(0), err)
	}

	conn, err := connect(*addr)
	if err != nil {
		return err
	}
	defer conn.Close()

	if *output == "" {
		return fetchBlob(ctx, pb.NewBlobServiceClient(conn), want, stdout)
	}
	// Download beside the destination and rename into place, so a failed or
	// corrupt download never leaves a file under the requested name.
	tmp, err := os.CreateTemp(filepath.Dir(*output), ".sisyphus-download-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	err = fetchBlob(ctx, pb.NewBlobServiceClient(conn), want, tmp)
	if closeErr := tmp.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	return os.Rename(tmp.Name(), *output)
}

// fetchBlob copies a blob from a node to dst and fails unless the bytes
// received hash to the CID asked for.
func fetchBlob(ctx context.Context, client pb.BlobServiceClient, want cid.Cid, dst io.Writer) error {
	stream, err := client.Get(ctx, &pb.GetBlobRequest{Cid: want.String()})
	if err != nil {
		return err
	}
	hasher := storage.NewHasher(ctx)
	for {
		msg, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
		if _, err := hasher.Write(msg.GetData()); err != nil {
			return err
		}
		if _, err := dst.Write(msg.GetData()); err != nil {
			return err
		}
	}
	got, err := hasher.Sum()
	if err != nil {
		return err
	}
	if !got.Equals(want) {
		return fmt.Errorf("node returned data that hashes to %s, not the %s asked for", got, want)
	}
	return nil
}

func blobStat(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("sisyphusd blob stat", flag.ContinueOnError)
	addr := fs.String("addr", defaultAddr, "node address")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("expected exactly one CID")
	}

	conn, err := connect(*addr)
	if err != nil {
		return err
	}
	defer conn.Close()
	stat, err := pb.NewBlobServiceClient(conn).Stat(ctx, &pb.StatBlobRequest{Cid: fs.Arg(0)})
	if err != nil {
		return err
	}
	fmt.Fprintf(stdout, "%s: %d bytes\n", fs.Arg(0), stat.GetSize())
	return nil
}

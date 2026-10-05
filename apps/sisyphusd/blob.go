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

	"github.com/excho0/Sisyphus/apps/sisyphusd/blobclient"
	pb "github.com/excho0/Sisyphus/packages/protocol/sisyphus/v1"
)

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
	// Upload checks the node's answer against a hash of the bytes sent.
	stored, err := blobclient.Upload(ctx, pb.NewBlobServiceClient(conn), in)
	if err != nil {
		return err
	}
	fmt.Fprintln(stdout, stored)
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
		return blobclient.Download(ctx, pb.NewBlobServiceClient(conn), want, stdout)
	}
	// Download beside the destination and rename into place, so a failed or
	// corrupt download never leaves a file under the requested name.
	tmp, err := os.CreateTemp(filepath.Dir(*output), ".sisyphus-download-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	err = blobclient.Download(ctx, pb.NewBlobServiceClient(conn), want, tmp)
	if closeErr := tmp.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	return os.Rename(tmp.Name(), *output)
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

package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"text/tabwriter"
	"time"

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
		case "pin":
			return blobPin(ctx, args[1:])
		case "unpin":
			return blobUnpin(ctx, args[1:])
		case "pins":
			return blobPins(ctx, args[1:])
		case "gc":
			return blobGC(ctx, args[1:])
		}
	}
	return errors.New("expected blob put, get, stat, pin, unpin, pins or gc")
}

func blobPut(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("sisyphusd blob put", flag.ContinueOnError)
	node := targetFlags(fs)
	ttl := fs.Duration("ttl", 0, "how long the node keeps the blob; 0 keeps it until unpinned")
	noPin := fs.Bool("no-pin", false, "do not pin the blob: the node may delete it after an hour")
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

	conn, err := node.connect()
	if err != nil {
		return err
	}
	defer conn.Close()
	// Upload checks the node's answer against a hash of the bytes sent.
	client := pb.NewBlobServiceClient(conn)
	stored, err := blobclient.Upload(ctx, client, in)
	if err != nil {
		return err
	}
	if !*noPin {
		pin := &pb.PinBlobRequest{Cid: stored.String(), TtlSeconds: uint64(ttl.Seconds())}
		if _, err := client.Pin(ctx, pin); err != nil {
			return fmt.Errorf("stored %s but could not pin it: %w", stored, err)
		}
	}
	fmt.Fprintln(stdout, stored)
	return nil
}

func blobGet(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("sisyphusd blob get", flag.ContinueOnError)
	node := targetFlags(fs)
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

	conn, err := node.connect()
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
	node := targetFlags(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("expected exactly one CID")
	}

	conn, err := node.connect()
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

func blobPin(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("sisyphusd blob pin", flag.ContinueOnError)
	node := targetFlags(fs)
	ttl := fs.Duration("ttl", 0, "how long the node keeps the blob; 0 keeps it until unpinned")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("expected exactly one CID")
	}

	conn, err := node.connect()
	if err != nil {
		return err
	}
	defer conn.Close()
	pin := &pb.PinBlobRequest{Cid: fs.Arg(0), TtlSeconds: uint64(ttl.Seconds())}
	_, err = pb.NewBlobServiceClient(conn).Pin(ctx, pin)
	return err
}

func blobUnpin(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("sisyphusd blob unpin", flag.ContinueOnError)
	node := targetFlags(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("expected exactly one CID")
	}

	conn, err := node.connect()
	if err != nil {
		return err
	}
	defer conn.Close()
	_, err = pb.NewBlobServiceClient(conn).Unpin(ctx, &pb.UnpinBlobRequest{Cid: fs.Arg(0)})
	return err
}

func blobPins(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("sisyphusd blob pins", flag.ContinueOnError)
	node := targetFlags(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}

	conn, err := node.connect()
	if err != nil {
		return err
	}
	defer conn.Close()
	listed, err := pb.NewBlobServiceClient(conn).ListPins(ctx, &pb.ListPinsRequest{})
	if err != nil {
		return err
	}
	if len(listed.GetPins()) == 0 {
		fmt.Fprintln(stdout, "nothing is pinned")
		return nil
	}
	tw := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "CID\tOWNER\tKEPT UNTIL")
	for _, pin := range listed.GetPins() {
		until := "released"
		if pin.GetExpiresAt() != nil {
			until = pin.GetExpiresAt().AsTime().UTC().Format(time.RFC3339)
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\n", pin.GetCid(), pin.GetOwner(), until)
	}
	return tw.Flush()
}

func blobGC(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("sisyphusd blob gc", flag.ContinueOnError)
	node := targetFlags(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}

	conn, err := node.connect()
	if err != nil {
		return err
	}
	defer conn.Close()
	done, err := pb.NewBlobServiceClient(conn).CollectGarbage(ctx, &pb.CollectGarbageRequest{})
	if err != nil {
		return err
	}
	fmt.Fprintf(stdout, "dropped %d expired pin(s), removed %d block(s), freed %d bytes\n",
		done.GetExpiredPins(), done.GetBlocksRemoved(), done.GetBytesFreed())
	return nil
}

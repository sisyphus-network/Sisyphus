package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/ipfs/go-cid"

	"github.com/sisyphus-network/Sisyphus/apps/sisyphusd/blobclient"
	pb "github.com/sisyphus-network/Sisyphus/packages/protocol/sisyphus/v1"
	"github.com/sisyphus-network/Sisyphus/packages/sealed"
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
		case "replicas":
			return blobReplicas(ctx, args[1:])
		case "restore":
			return blobRestore(ctx, args[1:])
		}
	}
	return errors.New("expected blob put, get, stat, pin, unpin, pins, gc, replicas or restore")
}

func blobPut(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("sisyphusd blob put", flag.ContinueOnError)
	node := targetFlags(fs)
	ttl := fs.Duration("ttl", 0, "how long the node keeps the blob; 0 keeps it until unpinned")
	noPin := fs.Bool("no-pin", false, "do not pin the blob: the node may delete it after an hour")
	keyFile := fs.String("key-file", "", "seal the file with this key before it leaves this machine, so that only holders of the key can read it")
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

	if *keyFile != "" {
		key, err := readKey(*keyFile)
		if err != nil {
			return err
		}
		in = sealed.Encrypt(key, in)
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
	keyFile := fs.String("key-file", "", "the key the blob was sealed with, to unseal it after fetching")
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

	// fetch writes the blob, checked against its CID, to dst: as stored, or
	// with a key, unsealed.
	client := pb.NewBlobServiceClient(conn)
	fetch := func(dst io.Writer) error { return blobclient.Download(ctx, client, want, dst) }
	if *keyFile != "" {
		key, err := readKey(*keyFile)
		if err != nil {
			return err
		}
		fetch = func(dst io.Writer) error { return fetchSealed(ctx, client, want, key, dst) }
	}

	if *output == "" {
		return fetch(stdout)
	}
	// Download beside the destination and rename into place, so a failed or
	// corrupt download never leaves a file under the requested name.
	tmp, err := os.CreateTemp(filepath.Dir(*output), ".sisyphus-download-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	err = fetch(tmp)
	if closeErr := tmp.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	return os.Rename(tmp.Name(), *output)
}

// fetchSealed downloads a sealed blob and writes what it opens to with key
// to dst. A blob that does not match its CID is not opened at all; a wrong
// key is found as the blob is opened.
func fetchSealed(ctx context.Context, client pb.BlobServiceClient, want cid.Cid, key sealed.Key, dst io.Writer) error {
	// Unsealing reads the blob out of order, so it is fetched to a file first.
	tmp, err := os.CreateTemp("", "sisyphus-sealed-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	defer tmp.Close()
	counted := &countingWriter{w: tmp}
	if err := blobclient.Download(ctx, client, want, counted); err != nil {
		return err
	}
	opened, err := sealed.Open(key, tmp, counted.n)
	if errors.Is(err, sealed.ErrNotSealed) {
		return fmt.Errorf("blob %s is not sealed; fetch it without --key-file", want)
	}
	if err != nil {
		return err
	}
	_, err = io.Copy(dst, opened)
	return err
}

// countingWriter counts the bytes written through it.
type countingWriter struct {
	w io.Writer
	n uint64
}

func (c *countingWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += uint64(n)
	return n, err
}

// readKey reads a sealing key from a file made by "sisyphusd key new".
func readKey(file string) (sealed.Key, error) {
	text, err := os.ReadFile(file)
	if err != nil {
		return sealed.Key{}, fmt.Errorf("read key: %w", err)
	}
	key, err := sealed.ParseKey(strings.TrimSpace(string(text)))
	if err != nil {
		return sealed.Key{}, fmt.Errorf("%s: %w", file, err)
	}
	return key, nil
}

// keyCommand makes sealing keys.
func keyCommand(args []string) error {
	if len(args) != 2 || args[0] != "new" {
		return errors.New(`expected "key new <file>"`)
	}
	// Created, never overwritten: replacing a key loses whatever it sealed.
	file, err := os.OpenFile(args[1], os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("create key file: %w", err)
	}
	fmt.Fprintln(file, sealed.NewKey().String())
	return file.Close()
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

// blobReplicas shows how many of a node's storage followers hold each blob
// it has pinned, against how many should.
func blobReplicas(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("sisyphusd blob replicas", flag.ContinueOnError)
	node := targetFlags(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 1 {
		return errors.New("expected at most one CID")
	}

	conn, err := node.connect()
	if err != nil {
		return err
	}
	defer conn.Close()
	listed, err := pb.NewBlobServiceClient(conn).Replicas(ctx, &pb.ReplicasRequest{Cid: fs.Arg(0)})
	if err != nil {
		return err
	}
	wanted, followers := int(listed.GetWanted()), len(listed.GetFollowers())
	if stranded := listed.GetFromEarlierStore(); stranded > 0 {
		fmt.Fprintf(stdout, "followers hold %d blob(s) from a store this node had before, which it no longer has pinned: \"sisyphusd blob restore\" fetches them back\n", stranded)
	}
	if wanted == 0 {
		fmt.Fprintln(stdout, "this node asks no followers to hold copies of its data: it was started without --replicas")
		return nil
	}
	fmt.Fprintf(stdout, "%d follower(s) connected; each pinned blob is to be held by %d\n", followers, wanted)
	if followers < wanted {
		fmt.Fprintf(stdout, "that is %d too few: start more worker-only nodes with --replica-dir\n", wanted-followers)
	}
	if listed.GetSettling() {
		fmt.Fprintln(stdout, "the node started a short while ago: followers that hold copies from before are given time to say so before any are handed what it already had")
	}
	if len(listed.GetBlobs()) == 0 {
		fmt.Fprintln(stdout, "nothing is pinned")
		return nil
	}
	tw := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "CID\tCOPIES\tHELD BY")
	short := 0
	for _, blob := range listed.GetBlobs() {
		holders := "-"
		if len(blob.GetHolders()) > 0 {
			holders = strings.Join(blob.GetHolders(), ",")
		}
		if len(blob.GetHolders()) < wanted {
			short++
		}
		fmt.Fprintf(tw, "%s\t%d/%d\t%s\n", blob.GetCid(), len(blob.GetHolders()), wanted, holders)
	}
	tw.Flush()
	if short > 0 {
		fmt.Fprintf(stdout, "%d of %d blob(s) have fewer copies than wanted\n", short, len(listed.GetBlobs()))
	}
	return nil
}

// blobRestore has a node whose store was lost fetch back what its storage
// followers still hold of it.
func blobRestore(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("sisyphusd blob restore", flag.ContinueOnError)
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
	done, err := pb.NewBlobServiceClient(conn).Restore(ctx, &pb.RestoreRequest{})
	if err != nil {
		return err
	}
	fmt.Fprintf(stdout, "restored %d blob(s), pinned until unpinned\n", done.GetRestored())
	if failed := done.GetFailed(); len(failed) > 0 {
		return fmt.Errorf("%d blob(s) could not be fetched back from any follower: %s", len(failed), strings.Join(failed, ", "))
	}
	return nil
}

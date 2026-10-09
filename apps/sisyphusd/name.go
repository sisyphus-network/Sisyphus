package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"time"

	"github.com/ipfs/go-cid"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/sisyphus-network/Sisyphus/packages/names"
	pb "github.com/sisyphus-network/Sisyphus/packages/protocol/sisyphus/v1"
)

func nameCommand(ctx context.Context, args []string) error {
	if len(args) > 0 {
		switch args[0] {
		case "publish":
			return namePublish(ctx, args[1:])
		case "resolve":
			return nameResolve(ctx, args[1:])
		}
	}
	return errors.New("expected name publish or resolve")
}

// namePublish points this node's name at a file. The record is signed here,
// with the key in the data directory, and handed to the node addressed,
// which keeps it and answers with it from then on.
func namePublish(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("sisyphusd name publish", flag.ContinueOnError)
	node := targetFlags(fs)
	lifetime := fs.Duration("lifetime", names.DefaultLifetime, "how long the record is good for; publish again before then to keep the name working")
	ttl := fs.Duration("ttl", names.DefaultTTL, "how long whoever reads the record may go on using it before asking again")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("expected exactly one CID")
	}
	value, err := cid.Decode(fs.Arg(0))
	if err != nil {
		return fmt.Errorf("invalid CID %q: %w", fs.Arg(0), err)
	}
	if *lifetime < time.Second {
		return errors.New("--lifetime must be at least a second")
	}
	if *ttl < 0 {
		return errors.New("--ttl cannot be negative")
	}
	ident, err := loadIdentity(*node.dataDir)
	if err != nil {
		return err
	}
	conn, err := node.connect()
	if err != nil {
		return err
	}
	defer conn.Close()
	client := pb.NewNameServiceClient(conn)

	// A record replaces the one before it by having a higher sequence
	// number, and the node that keeps the records knows the last one used.
	var sequence uint64
	held, err := client.Resolve(ctx, &pb.ResolveNameRequest{Name: ident.ID()})
	switch {
	case status.Code(err) == codes.NotFound:
	case err != nil:
		return err
	default:
		last, err := names.Parse(ident.ID(), held.GetRecord())
		if err != nil {
			return fmt.Errorf("the node at %s gave, as this node's last record, one that is not: %w", *node.addr, err)
		}
		sequence = last.Sequence + 1
	}
	record := names.Make(ident, value, sequence, *lifetime, *ttl, time.Now())
	if _, err := client.Publish(ctx, &pb.PublishNameRequest{Record: record.Bytes()}); err != nil {
		return err
	}
	fmt.Fprintln(stdout, record.Name)
	return nil
}

// nameResolve asks a node what a name stands for, and believes the answer
// only as far as the name's own node signed it.
func nameResolve(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("sisyphusd name resolve", flag.ContinueOnError)
	node := targetFlags(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("expected exactly one name, which is a node ID")
	}
	id, err := names.ID(fs.Arg(0))
	if err != nil {
		return err
	}
	conn, err := node.connect()
	if err != nil {
		return err
	}
	defer conn.Close()
	held, err := pb.NewNameServiceClient(conn).Resolve(ctx, &pb.ResolveNameRequest{Name: id})
	if err != nil {
		return err
	}
	record, err := names.Check(id, held.GetRecord(), time.Now())
	if err != nil {
		return fmt.Errorf("the node at %s gave a record for %s that cannot be relied on: %w", *node.addr, id, err)
	}
	fmt.Fprintln(stdout, record.Value)
	return nil
}

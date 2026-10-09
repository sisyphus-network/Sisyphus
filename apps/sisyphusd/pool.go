package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"github.com/libp2p/go-libp2p/core/peer"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"

	"github.com/sisyphus-network/Sisyphus/packages/identity"
	pb "github.com/sisyphus-network/Sisyphus/packages/protocol/sisyphus/v1"
)

// A node keeps, in known.json in its data directory, the ID of the node it
// expects to find at each address it has joined. Connecting anywhere else,
// it expects to find itself: its own daemon, started from the same data
// directory.

func loadKnown(dataDir string) (map[string]string, error) {
	known := make(map[string]string)
	data, err := os.ReadFile(filepath.Join(dataDir, "known.json"))
	if errors.Is(err, os.ErrNotExist) {
		return known, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read known nodes: %w", err)
	}
	if err := json.Unmarshal(data, &known); err != nil {
		return nil, fmt.Errorf("known nodes in %s: %w", dataDir, err)
	}
	return known, nil
}

// target is the node a client command talks to and the identity it talks as.
type target struct {
	addr    *string
	dataDir *string
}

// targetFlags adds the flags every client command takes.
func targetFlags(fs *flag.FlagSet) *target {
	return &target{
		addr:    fs.String("addr", defaultAddr, "address of the node to talk to"),
		dataDir: fs.String("data-dir", defaultDataDir(), "directory holding this node's key and the nodes it has joined"),
	}
}

// connect opens a connection to the target, as this node, that succeeds only
// if the node expected at that address answers.
func (t *target) connect() (*grpc.ClientConn, error) {
	ident, err := loadIdentity(*t.dataDir)
	if err != nil {
		return nil, err
	}
	known, err := loadKnown(*t.dataDir)
	if err != nil {
		return nil, err
	}
	expected := known[*t.addr]
	if expected == "" {
		expected = ident.ID()
	}
	return grpc.NewClient(*t.addr, grpc.WithTransportCredentials(credentials.NewTLS(ident.ClientTLS(expected))))
}

// joinPool redeems an invitation from the node at addr and remembers that
// node, so that later connections to addr are known to reach it. It returns
// the role the node was admitted in and the nodes it now knows.
func joinPool(ctx context.Context, dataDir string, ident *identity.Identity, addr, invitation string) (pb.Role, map[string]string, error) {
	// A node that has been trusted directly joins with the ID of the node
	// that trusts it, and no token.
	inviter, token, _ := strings.Cut(invitation, ":")
	if _, err := peer.Decode(inviter); err != nil {
		return 0, nil, errors.New("an invitation looks like <node ID>:<token>, or for a node that has been trusted without one, <node ID>")
	}
	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(credentials.NewTLS(ident.ClientTLS(inviter))))
	if err != nil {
		return 0, nil, err
	}
	defer conn.Close()
	joined, err := pb.NewPoolServiceClient(conn).Join(ctx, &pb.JoinRequest{Token: token})
	if err != nil {
		return 0, nil, fmt.Errorf("join %s: %w", addr, err)
	}

	known, err := loadKnown(dataDir)
	if err != nil {
		return 0, nil, err
	}
	known[addr] = inviter
	data, _ := json.MarshalIndent(known, "", "\t") // a map of strings always encodes
	if err := os.WriteFile(filepath.Join(dataDir, "known.json"), data, 0o600); err != nil {
		return 0, nil, fmt.Errorf("remember the node joined: %w", err)
	}
	return joined.GetRole(), known, nil
}

var roleNames = map[pb.Role]string{pb.Role_ROLE_WORKER: "worker", pb.Role_ROLE_CLIENT: "client"}

func poolCommand(ctx context.Context, args []string) error {
	if len(args) > 0 {
		switch args[0] {
		case "invite":
			return poolInvite(ctx, args[1:])
		case "join":
			return poolJoin(ctx, args[1:])
		case "members":
			return poolMembers(ctx, args[1:])
		case "remove":
			return poolRemove(ctx, args[1:])
		case "rekey":
			return poolRekey(ctx, args[1:])
		case "work-for":
			return poolWorkFor(ctx, args[1:])
		case "cluster":
			return showCluster(ctx, args[1:])
		}
	}
	return errors.New("expected pool invite, join, members, remove, work-for, rekey or cluster")
}

func poolInvite(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("sisyphusd pool invite", flag.ContinueOnError)
	node := targetFlags(fs)
	role := fs.String("role", "worker", "what the invited node may be: worker (runs tasks) or client (submits jobs and manages data)")
	ttl := fs.Duration("ttl", time.Hour, "how long the invitation can be used for")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}
	if *ttl < time.Second {
		return errors.New("--ttl must be at least a second")
	}
	req := &pb.InviteRequest{TtlSeconds: uint64(ttl.Seconds())}
	switch *role {
	case "worker":
		req.Role = pb.Role_ROLE_WORKER
	case "client":
		req.Role = pb.Role_ROLE_CLIENT
	default:
		return fmt.Errorf("unknown role %q", *role)
	}

	conn, err := node.connect()
	if err != nil {
		return err
	}
	defer conn.Close()
	invited, err := pb.NewPoolServiceClient(conn).Invite(ctx, req)
	if err != nil {
		return err
	}
	fmt.Fprintln(stdout, invited.GetInvitation())
	return nil
}

func poolJoin(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("sisyphusd pool join", flag.ContinueOnError)
	node := targetFlags(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("expected exactly one invitation")
	}
	ident, err := loadIdentity(*node.dataDir)
	if err != nil {
		return err
	}
	role, _, err := joinPool(ctx, *node.dataDir, ident, *node.addr, fs.Arg(0))
	if err != nil {
		return err
	}
	fmt.Fprintf(stdout, "joined %s as a %s\n", *node.addr, roleNames[role])
	return nil
}

func poolMembers(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("sisyphusd pool members", flag.ContinueOnError)
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
	listed, err := pb.NewPoolServiceClient(conn).ListMembers(ctx, &pb.ListMembersRequest{})
	if err != nil {
		return err
	}
	if len(listed.GetMembers()) == 0 {
		fmt.Fprintln(stdout, "no nodes have been admitted")
	}
	tw := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
	if len(listed.GetMembers()) > 0 {
		fmt.Fprintln(tw, "NODE\tROLE\tJOINED")
	}
	for _, m := range listed.GetMembers() {
		fmt.Fprintf(tw, "%s\t%s\t%s\n", m.GetNodeId(), roleNames[m.GetRole()], m.GetJoinedAt().AsTime().UTC().Format(time.RFC3339))
	}
	tw.Flush()
	// The other side of trust: whom this node will take tasks from.
	for i, id := range listed.GetWorksFor() {
		if i == 0 {
			fmt.Fprintln(stdout, "\nthis node will work for:")
		}
		fmt.Fprintln(stdout, id)
	}
	return nil
}

// poolWorkFor says whether this node will take tasks from another.
func poolWorkFor(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("sisyphusd pool work-for", flag.ContinueOnError)
	node := targetFlags(fs)
	stop := fs.Bool("stop", false, "stop working for the node, and take no more tasks from it")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("expected exactly one node ID")
	}
	conn, err := node.connect()
	if err != nil {
		return err
	}
	defer conn.Close()
	if _, err := pb.NewPoolServiceClient(conn).SetWorkFor(ctx, &pb.SetWorkForRequest{NodeId: fs.Arg(0), Willing: !*stop}); err != nil {
		return err
	}
	if *stop {
		fmt.Fprintf(stdout, "no longer working for node %s\n", fs.Arg(0))
	} else {
		fmt.Fprintf(stdout, "will work for node %s whenever it will have this one\n", fs.Arg(0))
	}
	return nil
}

func poolRemove(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("sisyphusd pool remove", flag.ContinueOnError)
	node := targetFlags(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("expected exactly one node ID")
	}

	conn, err := node.connect()
	if err != nil {
		return err
	}
	defer conn.Close()
	_, err = pb.NewPoolServiceClient(conn).RemoveMember(ctx, &pb.RemoveMemberRequest{NodeId: fs.Arg(0)})
	return err
}

func poolRekey(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("sisyphusd pool rekey", flag.ContinueOnError)
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
	rekeyed, err := pb.NewPoolServiceClient(conn).Rekey(ctx, &pb.RekeyRequest{})
	if err != nil {
		return err
	}
	fmt.Fprintf(stdout, "the pool's private IPFS network has a new key (%s)\n", rekeyed.GetSwarmFingerprint())
	return nil
}

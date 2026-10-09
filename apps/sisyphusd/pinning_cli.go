package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/ipfs/go-cid"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/sisyphus-network/Sisyphus/apps/sisyphusd/pinning"
	pb "github.com/sisyphus-network/Sisyphus/packages/protocol/sisyphus/v1"
)

// pinningService is the pinning service a command talks to: somebody
// else's, or another node's, anywhere that speaks the IPFS Pinning Service
// API.
type pinningService struct {
	address *string
	keyFile *string
}

// serviceFlags adds the flags that say which pinning service a command is
// for.
func serviceFlags(fs *flag.FlagSet) *pinningService {
	return &pinningService{
		address: fs.String("service", "", "address of the pinning service, such as https://api.pinata.cloud/psa, or http://host:port for a node started with --pinning-listen"),
		keyFile: fs.String("key-file", "", "a file holding the key the service gave; for a node, a copy of its pinning.token"),
	}
}

// client returns a client of the service, with its key read from the file.
// The key is given in a file so that it is in no list of running commands
// and no shell history.
func (p *pinningService) client() (*pinning.Client, error) {
	if *p.address == "" || *p.keyFile == "" {
		return nil, errors.New("say which pinning service with --service <address> and --key-file <file holding its key>")
	}
	key, err := os.ReadFile(*p.keyFile)
	if err != nil {
		return nil, fmt.Errorf("read the pinning service's key: %w", err)
	}
	return pinning.NewClient(*p.address, strings.TrimSpace(string(key)))
}

// remotePoll is how often --wait asks a pinning service after a request.
var remotePoll = 2 * time.Second

// blobPinRemote asks a pinning service to keep a blob. The service is told
// the blob's CID, the name if one is given, and where the node's Kubo can be
// reached, since that is where the service will fetch the blob from.
func blobPinRemote(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("sisyphusd blob pin-remote", flag.ContinueOnError)
	node := targetFlags(fs)
	service := serviceFlags(fs)
	wait := fs.Bool("wait", false, "wait until the service has the blob, or has failed to get it")
	noOrigins := fs.Bool("no-origins", false, "do not tell the service this node's addresses; it must then find the blob by itself")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() < 1 || fs.NArg() > 2 {
		return errors.New("expected a CID, and after it a name for the service to keep it under if one is wanted")
	}
	c, err := cid.Decode(fs.Arg(0))
	if err != nil {
		return fmt.Errorf("invalid CID %q: %w", fs.Arg(0), err)
	}
	client, err := service.client()
	if err != nil {
		return err
	}
	pin := pinning.Pin{CID: c.String(), Name: fs.Arg(1)}
	if !*noOrigins {
		if pin.Origins, err = kuboAddresses(ctx, node); err != nil {
			return err
		}
	}

	asked, err := client.Add(ctx, pin)
	if err != nil {
		return err
	}
	fmt.Fprintln(stdout, asked.RequestID, asked.Status)
	if *wait && (asked.Status == pinning.Queued || asked.Status == pinning.Pinning) {
		id := asked.RequestID
		if asked, err = client.Wait(ctx, id, remotePoll); err != nil {
			return fmt.Errorf("waiting for request %s: %w", id, err)
		}
		fmt.Fprintln(stdout, asked.RequestID, asked.Status)
	}
	if asked.Status == pinning.Failed {
		return fmt.Errorf("the service could not pin %s: %s", c, why(asked))
	}
	return nil
}

// why is what a service says of a request that failed.
func why(status pinning.Status) string {
	if said := status.Info[pinning.Details]; said != "" {
		return said
	}
	if len(status.Info) > 0 {
		return fmt.Sprint(status.Info)
	}
	return "it did not say why"
}

// kuboAddresses asks a node where its pool's Kubo daemons can be reached
// from another machine. A node without Kubo has no such addresses, which
// is not an error: a service can keep only what it can fetch some other
// way, and will say so.
func kuboAddresses(ctx context.Context, node *target) ([]string, error) {
	conn, err := node.connect()
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	swarm, err := pb.NewPoolServiceClient(conn).Swarm(ctx, &pb.SwarmRequest{})
	if status.Code(err) == codes.FailedPrecondition {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("ask the node at %s where its Kubo can be reached: %w (--no-origins asks the service without saying)", *node.addr, err)
	}
	// A request names twenty origins at most.
	addresses := swarm.GetAddresses()
	return addresses[:min(len(addresses), 20)], nil
}

func blobPinsRemote(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("sisyphusd blob pins-remote", flag.ContinueOnError)
	service := serviceFlags(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}
	client, err := service.client()
	if err != nil {
		return err
	}
	listed, err := client.List(ctx)
	if err != nil {
		return err
	}
	if len(listed) == 0 {
		fmt.Fprintln(stdout, "the service has been asked to keep nothing")
		return nil
	}
	tw := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "REQUEST\tCID\tSTATUS\tASKED\tNAME")
	for _, request := range listed {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", request.RequestID, request.Pin.CID, request.Status, request.Created.UTC().Format(time.RFC3339), request.Pin.Name)
		if request.Status == pinning.Failed {
			// Under the request, what the service says went wrong.
			fmt.Fprintln(tw, "  "+why(request))
		}
	}
	return tw.Flush()
}

func blobUnpinRemote(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("sisyphusd blob unpin-remote", flag.ContinueOnError)
	service := serviceFlags(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("expected exactly one request ID, as blob pin-remote printed it and blob pins-remote lists it")
	}
	client, err := service.client()
	if err != nil {
		return err
	}
	return client.Remove(ctx, fs.Arg(0))
}

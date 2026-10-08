package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"path/filepath"

	"github.com/sisyphus-network/Sisyphus/apps/sisyphusd/inference"
	pb "github.com/sisyphus-network/Sisyphus/packages/protocol/sisyphus/v1"
)

// serveInference offers the language models of a pool this machine has
// joined, to programs on this machine, until it is stopped. It is for a
// machine that uses a pool without coordinating it: it reaches the pool as
// the job commands do, as a client of the node that coordinates it, so
// nothing is opened to the network and the coordinator knows who asks.
func serveInference(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("sisyphusd inference", flag.ContinueOnError)
	node := targetFlags(fs)
	listen := fs.String("listen", "127.0.0.1:11435", "loopback address to offer the pool's models on, as a service speaking OpenAI's dialect")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}
	if host, _, err := net.SplitHostPort(*listen); err != nil || !net.ParseIP(host).IsLoopback() {
		return errors.New("--listen must be a loopback address such as 127.0.0.1:11435: the service is for programs on this machine only")
	}
	pool, hangUp, err := dial(node)
	if err != nil {
		return err
	}
	defer hangUp()
	// Asked now, so that a pool not joined or not there is said at once
	// and not at the first request.
	if _, err := pool.ListNodes(ctx, &pb.ListNodesRequest{}); err != nil {
		return fmt.Errorf("reach the pool at %s: %w", *node.addr, err)
	}
	tokenFile := filepath.Join(*node.dataDir, "api.token")
	token, err := apiToken(tokenFile)
	if err != nil {
		return err
	}
	lis, err := net.Listen("tcp", *listen)
	if err != nil {
		return err
	}
	server := &http.Server{Handler: inference.Handler(inference.Remote{Node: pool}, token)}
	fmt.Fprintf(stdout, "the models of the pool at %s are offered at http://%s/v1\nthe key is in %s\n", *node.addr, lis.Addr(), tokenFile)
	go server.Serve(lis)
	<-ctx.Done()
	return server.Close()
}

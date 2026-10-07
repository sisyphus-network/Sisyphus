package main

import (
	"context"
	"flag"
	"fmt"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/sisyphus-network/Sisyphus/apps/sisyphusd/mcpserver"
	nodepb "github.com/sisyphus-network/Sisyphus/packages/protocol/sisyphus/node/v1"
	"github.com/sisyphus-network/Sisyphus/packages/runtime"
)

// agentTransport is how an agent is spoken to: over standard input and
// output, which is how an agent starts a server of its own. Tests put
// something else here.
var agentTransport mcp.Transport = &mcp.StdioTransport{}

// serveAgent offers a running node to an AI agent, as a Model Context
// Protocol server, until the agent hangs up. It holds the node's token, so
// the agent can do whatever the node's owner can do through the local API
// with these tools, and no more.
func serveAgent(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("sisyphusd mcp", flag.ContinueOnError)
	dataDir := fs.String("data-dir", defaultDataDir(), "directory holding the node's data, where its API token is")
	apiAddr := fs.String("api", "127.0.0.1:50051", "address of the node's local API, as given to its --api-listen")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}
	conn, token, err := localAPI(*dataDir, *apiAddr)
	if err != nil {
		return err
	}
	defer conn.Close()
	server := mcpserver.New(mcpserver.Config{
		Node: nodepb.NewNodeServiceClient(conn), Token: token,
		// Every workload there is, so that whichever the node has can be described.
		Workloads: runtime.WithContainers(), Version: version,
	})
	return server.Run(ctx, agentTransport)
}

package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"google.golang.org/grpc"

	"github.com/sisyphus-network/Sisyphus/apps/sisyphusd/mcpserver"
	nodepb "github.com/sisyphus-network/Sisyphus/packages/protocol/sisyphus/node/v1"
	pb "github.com/sisyphus-network/Sisyphus/packages/protocol/sisyphus/v1"
	"github.com/sisyphus-network/Sisyphus/packages/runtime"
	"github.com/sisyphus-network/Sisyphus/skills"
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
	addr := fs.String("addr", defaultAddr, "the node's main address, as given to its --listen, where what it keeps, its name and its jobs' records are reached with the node's key")
	filesUnder := fs.String("files-under", ".", "the directory the agent may store files from and fetch them to, and nowhere outside it; / for anywhere you may")
	readOnly := fs.Bool("read-only", false, "offer only the tools that look: no jobs run, nothing stored, fetched to disk or changed")
	administer := fs.Bool("admin", false, "also offer the tools that change the node itself: its pool's members and invitations, the nodes it trusts, joining another pool, the model it plans with, and clearing or restoring its store")
	images := fs.String("images", "", "the only container images the agent may run, separated by commas; any, if empty")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}
	// A directory that cannot be made out is no directory, and is refused next.
	under, _ := filepath.Abs(*filesUnder)
	if info, err := os.Stat(under); err != nil || !info.IsDir() {
		return fmt.Errorf("--files-under: %s is not a directory", *filesUnder)
	}
	var listed []string
	for _, image := range strings.Split(*images, ",") {
		if image = strings.TrimSpace(image); image != "" {
			listed = append(listed, image)
		}
	}
	conn, token, err := localAPI(*dataDir, *apiAddr)
	if err != nil {
		return err
	}
	defer conn.Close()
	main := &mainAddress{node: &target{addr: addr, dataDir: dataDir}}
	defer main.close()
	server := mcpserver.New(mcpserver.Config{
		Node: nodepb.NewNodeServiceClient(conn), Token: token, Main: main.reach,
		FilesUnder: under, ReadOnly: *readOnly, Admin: *administer, Images: listed,
		// Every workload there is, so that whichever the node has can be described.
		Workloads: runtime.WithContainers().With(runtime.Graph{}), Version: version,
	})
	return server.Run(ctx, agentTransport)
}

// mainAddress is a node's main address as an agent's server reaches it: as
// the command line does, with the node's key, and not before a tool needs
// it. The local API has no word of what the node keeps, of its name or of
// its jobs' records, and the main address has.
type mainAddress struct {
	node *target

	mu   sync.Mutex
	conn *grpc.ClientConn
	main *mcpserver.Main
}

// reach returns the node's main services, making the connection the first
// time. One that could not be made is tried again the next time.
func (m *mainAddress) reach() (*mcpserver.Main, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.main != nil {
		return m.main, nil
	}
	ident, err := loadIdentity(*m.node.dataDir)
	if err != nil {
		return nil, err
	}
	conn, err := m.node.connect()
	if err != nil {
		return nil, err
	}
	dataDir := *m.node.dataDir
	m.conn, m.main = conn, &mcpserver.Main{
		Blobs: pb.NewBlobServiceClient(conn), Names: pb.NewNameServiceClient(conn),
		Jobs: pb.NewNodeServiceClient(conn), Pool: pb.NewPoolServiceClient(conn),
		Identity: ident, SealingKey: func() ([]byte, error) { return heldSealingKey(dataDir) },
	}
	return m.main, nil
}

// close ends the connection, if one was made.
func (m *mainAddress) close() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.conn != nil {
		m.conn.Close()
	}
}

// heldSealingKey returns the key a node seals private jobs with, which is
// the one "job record --key-file" is given for a job the node made private.
// It is read and never made: a node with none has made no job private.
func heldSealingKey(dataDir string) ([]byte, error) {
	key, err := readKey(filepath.Join(dataDir, "private.key"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("there is no private.key in %s: this node has made no job private", dataDir)
	}
	if err != nil {
		return nil, err
	}
	return key[:], nil
}

// skillCommand hands out the skill that teaches an agent to use a pool,
// which the daemon carries: printed, or put where an agent keeps skills.
func skillCommand(args []string) error {
	if len(args) == 0 || (args[0] != "show" && args[0] != "install") {
		return errors.New("expected skill show or install")
	}
	fs := flag.NewFlagSet("sisyphusd skill "+args[0], flag.ContinueOnError)
	home, _ := os.UserHomeDir() // with no home, --dir says where
	dir := fs.String("dir", filepath.Join(home, ".claude", "skills"), "install: the directory an agent keeps its skills in; Claude Code's is the default")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}
	if args[0] == "show" {
		text, _ := skills.Files.ReadFile("sisyphus/SKILL.md") // carried in the program
		fmt.Fprint(stdout, string(text))
		return nil
	}
	into := filepath.Join(*dir, "sisyphus")
	if err := os.MkdirAll(into, 0o755); err != nil {
		return err
	}
	files, _ := skills.Files.ReadDir("sisyphus") // carried in the program
	for _, file := range files {
		text, _ := skills.Files.ReadFile("sisyphus/" + file.Name())
		if err := os.WriteFile(filepath.Join(into, file.Name()), text, 0o644); err != nil {
			return err
		}
	}
	fmt.Fprintf(stdout, "the skill is in %s\n", into)
	return nil
}

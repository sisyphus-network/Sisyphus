package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"os"
	goruntime "runtime"
	"strconv"
	"strings"

	"github.com/excho0/Sisyphus/apps/sisyphusd/api"
	"github.com/excho0/Sisyphus/apps/sisyphusd/coordinator"
	"github.com/excho0/Sisyphus/apps/sisyphusd/worker"
	"github.com/excho0/Sisyphus/packages/runtime"
)

func runDaemon(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("sisyphusd run", flag.ContinueOnError)
	roles := fs.String("role", "coordinator,worker", "comma-separated roles this node runs: coordinator, worker")
	listen := fs.String("listen", defaultAddr, "coordinator role: address to serve workers and clients on (no authentication yet; only expose it to a trusted network)")
	join := fs.String("coordinator", "", "worker-only node: host:port of the coordinator to join")
	nodeID := fs.String("node-id", "", "name of this node in the pool (default: hostname plus a random suffix)")
	slots := fs.Int("slots", goruntime.NumCPU(), "worker role: how many tasks to run at once")
	verbose := fs.Bool("v", false, "log per-task detail")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}

	var isCoordinator, isWorker bool
	for _, role := range strings.Split(*roles, ",") {
		switch strings.TrimSpace(role) {
		case "coordinator":
			isCoordinator = true
		case "worker":
			isWorker = true
		default:
			return fmt.Errorf("unknown role %q", role)
		}
	}
	if isCoordinator && *join != "" {
		return errors.New("--coordinator is for worker-only nodes; a coordinator's own worker joins it automatically")
	}
	if !isCoordinator && *join == "" {
		return errors.New("a worker-only node needs --coordinator host:port")
	}
	if *slots < 1 {
		return errors.New("--slots must be at least 1")
	}
	if *nodeID == "" {
		*nodeID = defaultNodeID()
	}

	level := slog.LevelInfo
	if *verbose {
		level = slog.LevelDebug
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))
	workloads := runtime.Builtin()

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	// Each role reports once when it stops; the first to stop ends the node.
	stopped := make(chan error, 2)

	if isCoordinator {
		lis, err := net.Listen("tcp", *listen)
		if err != nil {
			return err
		}
		srv := api.NewServer(coordinator.New(*nodeID, workloads, log))
		// Workers hold streams open indefinitely, so a graceful stop would
		// never finish.
		defer srv.Stop()
		log.Info("coordinator listening", "addr", lis.Addr().String(), "node", *nodeID)
		go func() { stopped <- srv.Serve(lis) }()
		*join = loopback(lis.Addr())
	}

	if isWorker {
		w := &worker.Worker{NodeID: *nodeID, Coordinator: *join, Slots: *slots, Workloads: workloads, Log: log}
		go func() { stopped <- w.Run(ctx) }()
	}

	select {
	case err := <-stopped:
		return err
	case <-ctx.Done():
		log.Info("shutting down")
		return nil
	}
}

// loopback returns an address the local worker can dial to reach a listener
// on this machine, including one bound to every interface.
func loopback(addr net.Addr) string {
	tcp, ok := addr.(*net.TCPAddr)
	if !ok || !tcp.IP.IsUnspecified() {
		return addr.String()
	}
	return net.JoinHostPort("127.0.0.1", strconv.Itoa(tcp.Port))
}

func defaultNodeID() string {
	hostname, err := os.Hostname()
	if err != nil {
		hostname = "node"
	}
	var suffix [3]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		panic(err)
	}
	return hostname + "-" + hex.EncodeToString(suffix[:])
}

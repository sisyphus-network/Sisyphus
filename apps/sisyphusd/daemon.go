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
	"path/filepath"
	goruntime "runtime"
	"strconv"
	"strings"
	"time"

	"github.com/excho0/Sisyphus/apps/sisyphusd/api"
	"github.com/excho0/Sisyphus/apps/sisyphusd/coordinator"
	"github.com/excho0/Sisyphus/apps/sisyphusd/worker"
	"github.com/excho0/Sisyphus/packages/runtime"
	"github.com/excho0/Sisyphus/packages/storage"
)

func runDaemon(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("sisyphusd run", flag.ContinueOnError)
	roles := fs.String("role", "coordinator,worker", "comma-separated roles this node runs: coordinator, worker")
	listen := fs.String("listen", defaultAddr, "coordinator role: address to serve workers and clients on (no authentication yet; only expose it to a trusted network)")
	join := fs.String("coordinator", "", "worker-only node: host:port of the coordinator to join")
	nodeID := fs.String("node-id", "", "name of this node in the pool (default: hostname plus a random suffix)")
	slots := fs.Int("slots", goruntime.NumCPU(), "worker role: how many tasks to run at once")
	dataDir := fs.String("data-dir", defaultDataDir(), "directory for this node's stored data; nodes sharing a machine each need their own")
	retain := fs.Duration("retain", 7*24*time.Hour, "coordinator role: how long a job's inputs and results are kept after it finishes")
	gcInterval := fs.Duration("gc-interval", time.Hour, "coordinator role: how often to delete stored data nothing is keeping; 0 never does")
	maxStore := fs.Uint64("max-store-bytes", 0, "coordinator role: refuse uploads once stored data uses this much disk; 0 means no limit")
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

	// Every node keeps a blob store. A coordinator's is durable, because it
	// is where a job's inputs and results live. A worker-only node's is a
	// cache of what its coordinator holds, kept in a directory of its own so
	// the two kinds never mix. Deferred calls run last-in first-out, so the
	// store closes only after everything using it has stopped.
	openStore, storeDir := storage.OpenLocal, "blobs"
	if !isCoordinator {
		openStore, storeDir = storage.OpenCache, "cache"
	}
	store, err := openStore(filepath.Join(*dataDir, storeDir))
	if err != nil {
		return fmt.Errorf("%w (nodes sharing a machine each need their own --data-dir)", err)
	}
	defer store.Close()
	// A node that is its own coordinator reads and writes the one store
	// directly; a worker-only node fetches into its cache.
	var blobs runtime.Blobs = store

	if isCoordinator {
		lis, err := net.Listen("tcp", *listen)
		if err != nil {
			return err
		}
		// Jobs are not remembered across a restart, so nothing is left to
		// release the pins they held open. Let those lapse instead.
		if err := store.ExpireOpenPins("job:", time.Now().Add(*retain)); err != nil {
			lis.Close()
			return err
		}
		coord := coordinator.New(coordinator.Config{ID: *nodeID, Workloads: workloads, Store: store, Retain: *retain, Log: log})
		defer coord.Close()
		srv := api.NewServer(api.Config{Coordinator: coord, Store: store, MaxStoreBytes: *maxStore})
		// Workers hold streams open indefinitely, so a graceful stop would
		// never finish.
		defer srv.Stop()
		log.Info("coordinator listening", "addr", lis.Addr().String(), "node", *nodeID)
		go func() { stopped <- srv.Serve(lis) }()
		*join = loopback(lis.Addr())

		if *gcInterval > 0 {
			collected := make(chan struct{})
			// The store must outlive the collector.
			defer func() {
				cancel()
				<-collected
			}()
			go func() {
				defer close(collected)
				collectPeriodically(ctx, store, *gcInterval, log)
			}()
		}
	} else {
		remote, err := worker.DialBlobs(*join, store)
		if err != nil {
			return err
		}
		defer remote.Close()
		blobs = remote
	}

	if isWorker {
		w := &worker.Worker{NodeID: *nodeID, Coordinator: *join, Slots: *slots, Workloads: workloads, Blobs: blobs, Log: log}
		done := make(chan struct{})
		// Tasks must finish before the store they use closes.
		defer func() {
			cancel()
			<-done
		}()
		go func() {
			defer close(done)
			stopped <- w.Run(ctx)
		}()
	}

	select {
	case err := <-stopped:
		return err
	case <-ctx.Done():
		log.Info("shutting down")
		return nil
	}
}

// collectPeriodically garbage-collects store every interval until ctx ends.
func collectPeriodically(ctx context.Context, store *storage.Store, interval time.Duration, log *slog.Logger) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			done, err := store.GC(ctx, time.Now())
			if err != nil {
				log.Warn("garbage collection failed", "error", err)
				continue
			}
			if done != (storage.Collected{}) {
				log.Info("collected garbage", "expired_pins", done.ExpiredPins, "blocks", done.Blocks, "bytes", done.Bytes)
			}
		case <-ctx.Done():
			return
		}
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

// hostname is os.Hostname; tests replace it.
var hostname = os.Hostname

func defaultNodeID() string {
	name, err := hostname()
	if err != nil {
		name = "node"
	}
	var suffix [3]byte
	rand.Read(suffix[:]) // never fails; see crypto/rand
	return name + "-" + hex.EncodeToString(suffix[:])
}

// defaultDataDir is ~/.sisyphus, falling back to the working directory when
// the home directory is unknown.
func defaultDataDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ".sisyphus"
	}
	return filepath.Join(home, ".sisyphus")
}

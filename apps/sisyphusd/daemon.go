package main

import (
	"context"
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

	"google.golang.org/grpc/credentials"

	"github.com/excho0/Sisyphus/apps/sisyphusd/access"
	"github.com/excho0/Sisyphus/apps/sisyphusd/api"
	"github.com/excho0/Sisyphus/apps/sisyphusd/coordinator"
	"github.com/excho0/Sisyphus/apps/sisyphusd/worker"
	"github.com/excho0/Sisyphus/packages/identity"
	"github.com/excho0/Sisyphus/packages/kubo"
	"github.com/excho0/Sisyphus/packages/runtime"
	"github.com/excho0/Sisyphus/packages/storage"
)

func runDaemon(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("sisyphusd run", flag.ContinueOnError)
	roles := fs.String("role", "coordinator,worker", "comma-separated roles this node runs: coordinator, worker")
	listen := fs.String("listen", defaultAddr, "coordinator role: address to serve workers and clients on")
	join := fs.String("coordinator", "", "worker-only node: host:port of its coordinator")
	invitation := fs.String("join", "", "worker-only node: an invitation from the coordinator, needed the first time this node connects to it")
	name := fs.String("name", defaultName(), "a label for people to recognise this node by")
	slots := fs.Int("slots", goruntime.NumCPU(), "worker role: how many tasks to run at once")
	dataDir := fs.String("data-dir", defaultDataDir(), "directory for this node's stored data; nodes sharing a machine each need their own")
	retain := fs.Duration("retain", 7*24*time.Hour, "coordinator role: how long a job's inputs and results are kept after it finishes")
	gcInterval := fs.Duration("gc-interval", time.Hour, "coordinator role: how often to delete stored data nothing is keeping; 0 never does")
	maxStore := fs.Uint64("max-store-bytes", 0, "coordinator role: refuse uploads once stored data uses this much disk; 0 means no limit")
	maxCache := fs.Uint64("max-cache-bytes", 0, "worker-only node: evict the least recently used cached blobs once the cache uses this much disk; 0 means no limit")
	useKubo := fs.Bool("kubo", false, "coordinator role: keep stored data in a Kubo (IPFS) daemon that this node starts and runs alongside itself; needs the ipfs program installed")
	syncCache := fs.Bool("sync-cache", false, "worker-only node: wait for the disk when caching a blob; slower, but the cache then survives a power cut without downloading again")
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
	if isCoordinator && *invitation != "" {
		return errors.New("--join is for worker-only nodes")
	}
	if !isCoordinator && *useKubo {
		return errors.New("--kubo is, for now, only for nodes with the coordinator role")
	}

	level := slog.LevelInfo
	if *verbose {
		level = slog.LevelDebug
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))
	workloads := runtime.Builtin()

	// The node's key lives beside its data and is created on first run.
	ident, err := loadIdentity(*dataDir)
	if err != nil {
		return err
	}
	log.Info("node identity", "id", ident.ID())

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	// Each role reports once when it stops; the first to stop ends the node.
	stopped := make(chan error, 2)

	// Every node keeps a blob store. A coordinator's is durable, because it
	// is where a job's inputs and results live. A worker-only node's is a
	// cache of what its coordinator holds, kept in a directory of its own so
	// the two kinds never mix. Deferred calls run last-in first-out, so the
	// store closes only after everything using it has stopped.
	var store *storage.Store
	if *useKubo {
		// Kubo runs as the same peer as this node, on a repository beside
		// the node's other data, and is stopped when the node stops.
		sidecar, startErr := kubo.Start(ctx, kubo.Config{Repo: filepath.Join(*dataDir, "ipfs"), Identity: ident})
		if startErr != nil {
			return startErr
		}
		defer sidecar.Stop()
		log.Info("kubo started", "repo", filepath.Join(*dataDir, "ipfs"))
		store, err = storage.OpenKubo(sidecar, filepath.Join(*dataDir, "kubo-pins"))
	} else if isCoordinator {
		store, err = storage.OpenLocal(filepath.Join(*dataDir, "blobs"))
	} else {
		store, err = storage.OpenCache(filepath.Join(*dataDir, "cache"), *syncCache)
	}
	if err != nil {
		return fmt.Errorf("%w (nodes sharing a machine each need their own --data-dir)", err)
	}
	defer store.Close()
	// A node that is its own coordinator reads and writes the one store
	// directly; a worker-only node fetches into its cache.
	var blobs runtime.Blobs = store
	// coordinatorID is the ID of the node this one's worker answers to.
	var coordinatorID string

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
		admitted, err := access.Open(filepath.Join(*dataDir, "access.json"), ident.ID())
		if err != nil {
			lis.Close()
			return err
		}
		coord := coordinator.New(coordinator.Config{ID: ident.ID(), Workloads: workloads, Store: store, Retain: *retain, Log: log})
		defer coord.Close()
		srv := api.NewServer(api.Config{
			Identity: ident, Access: admitted, Coordinator: coord, Store: store, MaxStoreBytes: *maxStore,
		})
		// Workers hold streams open indefinitely, so a graceful stop would
		// never finish.
		defer srv.Stop()
		log.Info("coordinator listening", "addr", lis.Addr().String(), "name", *name)
		go func() { stopped <- srv.Serve(lis) }()
		// The node's own worker connects to it as any other would, and
		// expects to find the node itself at the other end.
		*join = loopback(lis.Addr())
		coordinatorID = ident.ID()

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
		// A worker only ever talks to the coordinator it joined. The first
		// time, an invitation says which node that is and gets this one
		// admitted; after that the coordinator's ID is remembered.
		if *invitation != "" {
			if _, err := joinPool(ctx, *dataDir, ident, *join, *invitation); err != nil {
				return err
			}
		}
		known, err := loadKnown(*dataDir)
		if err != nil {
			return err
		}
		if coordinatorID = known[*join]; coordinatorID == "" {
			return fmt.Errorf("this node has not joined a coordinator at %s: start it once with --join and an invitation from that coordinator", *join)
		}
		remote, err := worker.DialBlobs(*join, credentials.NewTLS(ident.ClientTLS(coordinatorID)), store, *maxCache)
		if err != nil {
			return err
		}
		defer remote.Close()
		blobs = remote
	}

	if isWorker {
		w := &worker.Worker{
			Name: *name, Coordinator: *join, Credentials: credentials.NewTLS(ident.ClientTLS(coordinatorID)),
			Slots: *slots, Workloads: workloads, Blobs: blobs, Log: log,
		}
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

// loadIdentity returns the key of the node whose data is in dataDir, making
// the directory and the key if this is the node's first run.
func loadIdentity(dataDir string) (*identity.Identity, error) {
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return nil, err
	}
	ident, _, err := identity.LoadOrCreate(filepath.Join(dataDir, "node.key"))
	return ident, err
}

// showIdentity prints a node's ID.
func showIdentity(args []string) error {
	fs := flag.NewFlagSet("sisyphusd id", flag.ContinueOnError)
	dataDir := fs.String("data-dir", defaultDataDir(), "directory holding the node's key and data")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}
	ident, err := loadIdentity(*dataDir)
	if err != nil {
		return err
	}
	fmt.Fprintln(stdout, ident.ID())
	return nil
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

// defaultName is the machine's host name.
func defaultName() string {
	name, err := hostname()
	if err != nil {
		return "node"
	}
	return name
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

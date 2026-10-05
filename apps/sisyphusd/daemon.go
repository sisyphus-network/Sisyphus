package main

import (
	"context"
	"crypto/sha256"
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
	"sync"
	"time"

	"github.com/ipfs/go-cid"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"

	"github.com/excho0/Sisyphus/apps/sisyphusd/access"
	"github.com/excho0/Sisyphus/apps/sisyphusd/api"
	"github.com/excho0/Sisyphus/apps/sisyphusd/coordinator"
	"github.com/excho0/Sisyphus/apps/sisyphusd/worker"
	"github.com/excho0/Sisyphus/packages/identity"
	"github.com/excho0/Sisyphus/packages/kubo"
	pb "github.com/excho0/Sisyphus/packages/protocol/sisyphus/v1"
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
	useKubo := fs.Bool("kubo", false, "keep stored data in a Kubo (IPFS) daemon that this node starts and runs alongside itself, on a private network with the rest of its pool; needs the ipfs program installed, and for a worker, a coordinator that uses it too")
	swarmPort := fs.Int("swarm-port", 4101, "coordinator role with --kubo: TCP port the pool's private IPFS network reaches this node on; 0 picks one at each start")
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

	// coordinatorID is the ID of the node this one's worker answers to: the
	// node itself, or for a worker-only node the coordinator it joined.
	coordinatorID := ident.ID()
	if !isCoordinator {
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
	}
	creds := credentials.NewTLS(ident.ClientTLS(coordinatorID))

	// With --kubo, Kubo runs as the same peer as this node, on a repository
	// beside the node's other data, and is stopped when the node stops. A
	// coordinator's starts the pool's private network; a worker's joins it.
	var sidecar *kubo.Daemon
	var swarm *poolSwarm
	if *useKubo {
		network := &kubo.Swarm{Port: *swarmPort}
		if isCoordinator {
			network.Key, err = swarmKey(filepath.Join(*dataDir, "swarm.key"))
		} else {
			network.Port = 0
			network.Key, network.Peers, err = fetchSwarm(ctx, *join, creds)
		}
		if err != nil {
			return err
		}
		if sidecar, err = kubo.Start(ctx, kubo.Config{Repo: filepath.Join(*dataDir, "ipfs"), Identity: ident, Swarm: network}); err != nil {
			return err
		}
		defer sidecar.Stop()
		swarm = &poolSwarm{key: network.Key, daemon: sidecar, keyFile: filepath.Join(*dataDir, "swarm.key"), port: *swarmPort}
		log.Info("kubo started", "repo", filepath.Join(*dataDir, "ipfs"))
	}

	// Every node keeps a blob store. A coordinator's is durable, because it
	// is where a job's inputs and results live. A worker-only node's is a
	// cache of what the pool holds, kept apart from any durable store. With
	// Kubo, either kind keeps its blocks there. Deferred calls run last-in
	// first-out, so the store closes only after everything using it has
	// stopped.
	var store *storage.Store
	switch {
	case isCoordinator && *useKubo:
		store, err = storage.OpenKubo(sidecar, filepath.Join(*dataDir, "kubo-pins"))
	case isCoordinator:
		store, err = storage.OpenLocal(filepath.Join(*dataDir, "blobs"))
	case *useKubo:
		store = storage.OpenKuboCache(sidecar)
	default:
		store, err = storage.OpenCache(filepath.Join(*dataDir, "cache"), *syncCache)
	}
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
		admitted, err := access.Open(filepath.Join(*dataDir, "access.json"), ident.ID())
		if err != nil {
			lis.Close()
			return err
		}
		coord := coordinator.New(coordinator.Config{ID: ident.ID(), Workloads: workloads, Store: store, Retain: *retain, Log: log})
		defer coord.Close()
		config := api.Config{Identity: ident, Access: admitted, Coordinator: coord, Store: store, MaxStoreBytes: *maxStore}
		if swarm != nil {
			config.Swarm = swarm
			coord.AnnounceSwarm(swarm.Fingerprint())
		}
		srv := api.NewServer(config)
		// Workers hold streams open indefinitely, so a graceful stop would
		// never finish.
		defer srv.Stop()
		log.Info("coordinator listening", "addr", lis.Addr().String(), "name", *name)
		go func() { stopped <- srv.Serve(lis) }()
		// The node's own worker connects to it as any other would, and
		// expects to find the node itself at the other end.
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
		remote, err := worker.DialBlobs(*join, creds, store, *maxCache)
		if err != nil {
			return err
		}
		defer remote.Close()
		if swarm != nil {
			remote.OnFallback = warnOfFallback(log)
		}
		blobs = remote
	}

	if isWorker {
		w := &worker.Worker{
			Name: *name, Coordinator: *join, Credentials: creds,
			Slots: *slots, Workloads: workloads, Blobs: blobs, Log: log,
		}
		if swarm != nil && !isCoordinator {
			// Changing key restarts Kubo, which takes seconds, so it is
			// done beside the worker's own work and finished, or given up,
			// before Kubo is stopped.
			var following sync.WaitGroup
			defer following.Wait()
			w.OnSwarm = func(stated string) {
				following.Add(1)
				go func() {
					defer following.Done()
					swarm.adopt(ctx, *join, creds, stated, log)
				}()
			}
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

// poolSwarm is the private IPFS network of the pool this node belongs to,
// and this node's Kubo daemon on it.
type poolSwarm struct {
	daemon *kubo.Daemon
	// keyFile is where a coordinator keeps the key, and port where its Kubo
	// accepts the other members.
	keyFile string
	port    int

	// mu serialises changes of key.
	mu  sync.Mutex
	key string
}

func (s *poolSwarm) Key() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.key
}

// Fingerprint identifies the current key without revealing it.
func (s *poolSwarm) Fingerprint() string {
	return fingerprint(s.Key())
}

func fingerprint(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:8])
}

// Addresses returns where this node's Kubo can be reached, followed by where
// the members connected to it can be, so that a node joining the network
// connects to all of them and can fetch from any.
func (s *poolSwarm) Addresses(ctx context.Context) ([]string, error) {
	own, err := s.daemon.Addresses(ctx)
	if err != nil {
		return nil, err
	}
	others, err := s.daemon.PeerAddresses(ctx)
	return append(own, others...), err
}

// Rekey gives the pool's network a new key, on a coordinator: it saves the
// key and restarts this node's Kubo on it. Every other holder of the old key
// is then outside the network until it is given the new one.
func (s *poolSwarm) Rekey(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := kubo.NewSwarmKey()
	if err := os.WriteFile(s.keyFile, []byte(key), 0o600); err != nil {
		return fmt.Errorf("save swarm key: %w", err)
	}
	if err := s.daemon.Rekey(ctx, &kubo.Swarm{Key: key, Port: s.port}); err != nil {
		return err
	}
	s.key = key
	return nil
}

// adoptRetry is how long a worker waits before trying again to follow the
// pool to a new key.
var adoptRetry = 5 * time.Second

// adopt moves a worker's Kubo onto the pool's current key if the fingerprint
// its coordinator states shows that it is on another: it asks the
// coordinator for the key and the members' addresses and restarts on them.
// A failed attempt can leave Kubo stopped, and the worker with no store, so
// it keeps trying until it succeeds or ctx ends.
func (s *poolSwarm) adopt(ctx context.Context, coordinator string, creds credentials.TransportCredentials, stated string, log *slog.Logger) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if stated == fingerprint(s.key) {
		return
	}
	for {
		key, peers, err := fetchSwarm(ctx, coordinator, creds)
		if err == nil {
			err = s.daemon.Rekey(ctx, &kubo.Swarm{Key: key, Peers: peers})
		}
		if err == nil {
			s.key = key
			log.Info("moved to the new key of the pool's private IPFS network")
			return
		}
		log.Warn("could not follow the pool's private IPFS network to its new key; trying again", "in", adoptRetry.String(), "error", err)
		select {
		case <-time.After(adoptRetry):
		case <-ctx.Done():
			return
		}
	}
}

// warnOfFallback returns what a worker on the pool's private network does
// when the network fails to supply a blob and the coordinator has to: say
// so, since otherwise the only sign of a broken network is slowness.
func warnOfFallback(log *slog.Logger) func(cid.Cid) {
	return func(c cid.Cid) {
		log.Warn("the pool's private IPFS network did not supply a blob, which was downloaded from the coordinator instead; check that this node's Kubo can reach the others", "cid", c.String())
	}
}

// swarmKey returns the secret of the private network a coordinator runs,
// kept in file, generating it the first time.
func swarmKey(file string) (string, error) {
	data, err := os.ReadFile(file)
	if err == nil {
		return string(data), nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("read swarm key: %w", err)
	}
	key := kubo.NewSwarmKey()
	if err := os.WriteFile(file, []byte(key), 0o600); err != nil {
		return "", fmt.Errorf("save swarm key: %w", err)
	}
	return key, nil
}

// fetchSwarm asks the coordinator at addr how to join its pool's private
// network: the key, and where its own Kubo daemon is.
func fetchSwarm(ctx context.Context, addr string, creds credentials.TransportCredentials) (key string, peers []string, err error) {
	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(creds))
	if err != nil {
		return "", nil, err
	}
	defer conn.Close()
	swarm, err := pb.NewPoolServiceClient(conn).Swarm(ctx, &pb.SwarmRequest{})
	if err != nil {
		return "", nil, fmt.Errorf("ask the coordinator about its private IPFS network: %w", err)
	}
	return swarm.GetSwarmKey(), swarm.GetAddresses(), nil
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

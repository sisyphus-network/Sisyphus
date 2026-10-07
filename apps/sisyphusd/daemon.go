package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	goruntime "runtime"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ipfs/go-cid"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"

	"github.com/sisyphus-network/Sisyphus/apps/sisyphusd/access"
	"github.com/sisyphus-network/Sisyphus/apps/sisyphusd/api"
	"github.com/sisyphus-network/Sisyphus/apps/sisyphusd/coordinator"
	"github.com/sisyphus-network/Sisyphus/apps/sisyphusd/p2p"
	"github.com/sisyphus-network/Sisyphus/apps/sisyphusd/planner"
	"github.com/sisyphus-network/Sisyphus/apps/sisyphusd/tunnel"
	"github.com/sisyphus-network/Sisyphus/apps/sisyphusd/worker"
	"github.com/sisyphus-network/Sisyphus/packages/hardware"
	"github.com/sisyphus-network/Sisyphus/packages/identity"
	"github.com/sisyphus-network/Sisyphus/packages/kubo"
	"github.com/sisyphus-network/Sisyphus/packages/nodedb"
	nodepb "github.com/sisyphus-network/Sisyphus/packages/protocol/sisyphus/node/v1"
	pb "github.com/sisyphus-network/Sisyphus/packages/protocol/sisyphus/v1"
	"github.com/sisyphus-network/Sisyphus/packages/runtime"
	"github.com/sisyphus-network/Sisyphus/packages/storage"
)

func runDaemon(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("sisyphusd run", flag.ContinueOnError)
	roles := fs.String("role", "coordinator,worker", "comma-separated roles this node runs: coordinator, worker")
	listen := fs.String("listen", defaultAddr, "coordinator role: address to serve workers and clients on")
	join := fs.String("coordinator", "", "worker-only node: host:port of its coordinator")
	invitation := fs.String("join", "", "worker-only node: an invitation from the coordinator, needed the first time this node connects to it and ignored after that")
	serve := fs.String("serve", "", "worker-only node: address to serve its cached data on to the other workers of its pool, so that they need not all fetch it from the coordinator")
	advertise := fs.String("advertise", "", "worker-only node: the address other workers should use to reach --serve, if not the same")
	apiListen := fs.String("api-listen", "", "loopback address to serve the local API on, for the desktop client on this machine (it expects 127.0.0.1:50051); off if empty")
	name := fs.String("name", defaultName(), "a label for people to recognise this node by")
	slots := fs.Int("slots", goruntime.NumCPU(), "worker role: how many tasks to run at once")
	dataDir := fs.String("data-dir", defaultDataDir(), "directory for this node's stored data; nodes sharing a machine each need their own")
	maxMemory := fs.Uint64("offer-memory-mb", 0, "worker role: tell the pool this node has no more than this much memory, in mebibytes; 0 offers all it has")
	maxGPUs := fs.Int("offer-gpus", -1, "worker role: tell the pool this node has no more than this many graphics cards; -1 offers all it has")
	reciprocate := fs.Bool("work-for-trusted", true, "a node with both roles: also work for the nodes it trusts for compute, whenever they trust it back")
	relayAt := fs.String("relay", "", "worker-only node: listen on this address, as host:port, for the other members' libp2p hosts and relay between them; the port must be open to them. A coordinator relays already")
	discovery := fs.String("discovery", "on", "whether to look for other nodes, on this network and through the nodes already known, and let them find this one: on or off")
	bootstrap := fs.String("bootstrap", "", "addresses of nodes to connect to at startup, besides those in the address book: comma-separated, each ending in /p2p/<node ID>")
	locate := fs.Bool("locate-country", false, "ask ipapi.co which country this node's address is in, to show in the desktop client; this tells that service the address")
	keepJobs := fs.Duration("keep-jobs", 30*24*time.Hour, "coordinator role: how long a finished job can still be asked after; 0 keeps them for good")
	retain := fs.Duration("retain", 7*24*time.Hour, "coordinator role: how long a job's inputs and results are kept after it finishes")
	gcInterval := fs.Duration("gc-interval", time.Hour, "coordinator role: how often to delete stored data nothing is keeping; 0 never does")
	maxStore := fs.Uint64("max-store-bytes", 0, "coordinator role: refuse uploads once stored data uses this much disk; 0 means no limit")
	maxCache := fs.Uint64("max-cache-bytes", 0, "worker-only node: evict the least recently used cached blobs once the cache uses this much disk; 0 means no limit")
	useKubo := fs.Bool("kubo", false, "keep stored data in a Kubo (IPFS) daemon that this node starts and runs alongside itself, on a private network with the rest of its pool; needs the ipfs program installed, and for a worker, a coordinator that uses it too")
	swarmPort := fs.Int("swarm-port", 0, "coordinator role with --kubo: TCP port to open so that members' Kubo daemons can connect to this node's directly, which is faster; 0 opens none, and they reach it through --listen")
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
	if isCoordinator && *relayAt != "" {
		return errors.New("--relay is for worker-only nodes; a coordinator relays already, on --listen")
	}
	if *discovery != "on" && *discovery != "off" {
		return fmt.Errorf("--discovery is on or off, not %q", *discovery)
	}
	if host, _, err := net.SplitHostPort(*apiListen); *apiListen != "" && (err != nil || !net.ParseIP(host).IsLoopback()) {
		return errors.New("--api-listen must be a loopback address such as 127.0.0.1:50051: the local API is for programs on this machine only")
	}
	if isCoordinator && *invitation != "" {
		return errors.New("--join is for worker-only nodes")
	}
	if isCoordinator && *serve != "" {
		return errors.New("--serve is for worker-only nodes; a coordinator serves its data already")
	}
	if *advertise == "" {
		*advertise = *serve
	}
	if host, _, err := net.SplitHostPort(*advertise); *serve != "" && (err != nil || net.ParseIP(host).IsUnspecified()) {
		return errors.New("--serve needs an address other workers can connect to; if it listens on every interface, give that address with --advertise")
	}

	level := slog.LevelInfo
	if *verbose {
		level = slog.LevelDebug
	}
	log := slog.New(slog.NewTextHandler(stderr, &slog.HandlerOptions{Level: level}))
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

	// local is what the desktop client on this machine is told and may do.
	local := api.LocalConfig{NodeID: ident.ID(), Version: version, Workloads: workloads.Names()}

	// coordinatorID is the ID of the node this one's worker answers to: the
	// node itself, or for a worker-only node the coordinator it joined.
	coordinatorID := ident.ID()
	if !isCoordinator {
		// A worker only ever talks to the coordinator it joined. The first
		// time, an invitation says which node that is and gets this one
		// admitted; after that the coordinator's ID is remembered.
		known, err := loadKnown(*dataDir)
		if err != nil {
			return err
		}
		// An invitation works once, so it is only presented by a node that
		// has not joined yet. That lets a worker be restarted with the very
		// command it was first started with.
		if known[*join] == "" && *invitation != "" {
			if _, known, err = joinPool(ctx, *dataDir, ident, *join, *invitation); err != nil {
				return err
			}
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
	if *useKubo && isCoordinator {
		// With no port of its own opened, a coordinator's Kubo listens on
		// this machine only and members are brought to it through --listen.
		swarm = &poolSwarm{keyFile: filepath.Join(*dataDir, "swarm.key"), port: *swarmPort}
		key, err := swarmKey(swarm.keyFile)
		if err != nil {
			return err
		}
		network := &kubo.Swarm{Key: key, Port: *swarmPort, Loopback: *swarmPort == 0}
		if sidecar, err = kubo.Start(ctx, kubo.Config{Repo: filepath.Join(*dataDir, "ipfs"), Identity: ident, Swarm: network}); err != nil {
			return err
		}
		defer sidecar.Stop()
		swarm.key, swarm.daemon = key, sidecar
	}
	if *useKubo && !isCoordinator {
		var leave func()
		if swarm, leave, err = joinSwarm(ctx, ident, filepath.Join(*dataDir, "ipfs"), *join, coordinatorID, creds, log); err != nil {
			return err
		}
		defer leave()
		sidecar = swarm.daemon
	}
	if *useKubo {
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

	// What a node must not forget is kept in its database: the addresses
	// it starts from and, on a coordinator, who has been admitted, the
	// invitations still out, and its jobs, so that a restart picks up the
	// unfinished ones where they were.
	db, err := nodedb.Open(filepath.Join(*dataDir, "node.db"))
	if err != nil {
		return err
	}
	defer db.Close()
	// host is the node's libp2p host, started below according to its role.
	var host *p2p.Host
	// trustChanged hears of each node the coordinator admits or removes, and
	// worksFor says whether this node is working for another.
	trustChanged := new(notices)
	// planningPool is the pool the node's planner computes on, if it
	// coordinates one.
	var planningPool planner.Pool
	worksFor := func(string) bool { return false }
	// takes is the other side of trust: the nodes this one will take tasks
	// from, whatever else it does.
	takes, err := openWilling(db)
	if err != nil {
		return err
	}

	if isCoordinator {
		admitted, err := access.Open(db, ident.ID())
		if err == nil {
			// Earlier versions kept the list in a file of its own.
			err = admitted.Import(filepath.Join(*dataDir, "access.json"))
		}
		if err != nil {
			return err
		}
		admitted.OnChange(trustChanged.tell)
		// And a node that ran the Rust daemon before this one keeps what
		// that one knew.
		if err := importRustState(*dataDir, db, admitted, takes, log); err != nil {
			return err
		}
		coord := coordinator.New(coordinator.Config{
			ID: ident.ID(), Workloads: workloads, Store: store, Journal: db, Retain: *retain, KeepJobs: *keepJobs, Log: log,
		})
		defer coord.Close()
		unfinished, err := coord.Recover()
		if err == nil {
			_, err = coord.Prune(time.Now())
		}
		if err == nil {
			// A pin a job held open is released when the job ends. One whose
			// job is not among the unfinished has nobody left to release it:
			// let it lapse instead.
			err = store.ExpireOpenPins(func(owner string) bool {
				return coordinator.OwnsPin(owner) && !coord.Holds(owner)
			}, time.Now().Add(*retain))
		}
		if err != nil {
			return err
		}
		if unfinished > 0 {
			log.Info("took up the jobs left unfinished", "jobs", unfinished)
		}
		// The node listens on one port for two kinds of caller: its gRPC
		// clients and workers, and the libp2p hosts of its pool's members,
		// for which it relays so that two of them with no port open can
		// still reach each other.
		host, err = p2p.New(p2p.Config{Identity: ident, Listen: *listen, Relay: true, Discover: *discovery == "on", Log: log, Allow: func(id string) bool {
			_, admitted := admitted.Role(id)
			return admitted
		}})
		if err != nil {
			return err
		}
		defer host.Close()
		lis, _ := host.TLSListener() // fails only if asked for twice
		config := api.Config{Identity: ident, Access: admitted, Coordinator: coord, Store: store, MaxStoreBytes: *maxStore, WorkFor: takes}
		if swarm != nil {
			config.Swarm = swarm
			coord.AnnounceSwarm(swarm.Fingerprint())
		}
		srv := api.NewServer(config)
		local.Pool = api.NewPoolAdmin(config)
		local.Jobs = coord
		planningPool = coord
		local.Peers = func() []*nodepb.Peer { return poolPeers(ident.ID(), coord.Nodes(), admitted.Members()) }
		// Workers hold streams open indefinitely, so a graceful stop would
		// never finish. The coordinator is closed first, so that it knows
		// the workers it is about to lose are no fault of their tasks.
		defer func() {
			coord.Close()
			srv.Stop()
		}()
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
				collectPeriodically(ctx, store, coord.Prune, *gcInterval, log)
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
		// Blobs are fetched from fellow workers that have them, where the
		// coordinator knows of any, and served to fellow workers if asked.
		remote.PeerCredentials = func(nodeID string) credentials.TransportCredentials {
			return credentials.NewTLS(ident.ClientTLS(nodeID))
		}
		blobs = remote
		members := worker.NewMembers(remote.Conn())
		peers := api.NewPeerServer(ident, store, members.IsMember)
		defer peers.Stop()
		if *serve != "" {
			lis, err := net.Listen("tcp", *serve)
			if err != nil {
				return err
			}
			log.Info("serving cached data to fellow workers", "addr", lis.Addr().String(), "advertised", *advertise)
			go peers.Serve(lis)
		}
		// Whether or not it has a port open, the node can be reached by its
		// fellow workers through the coordinator, which relays for the pool,
		// and reaches them the same way. Two that can connect directly then
		// do. Only the coordinator and the pool's members are let in.
		// One that has a port open for the purpose relays as the coordinator
		// does, and every worker keeps a place on each member that relays.
		host, err = p2p.New(p2p.Config{Identity: ident, Listen: *relayAt, Relay: *relayAt != "", Via: relayAddrs(*join, coordinatorID), MoreRelays: members.Relays, Discover: *discovery == "on", Log: log, Allow: func(id string) bool {
			if id == coordinatorID {
				return true
			}
			asking, cancel := context.WithTimeout(ctx, memberCheck)
			defer cancel()
			member, _ := members.IsMember(asking, id)
			return member
		}})
		if err != nil {
			return err
		}
		defer host.Close()
		go peers.Serve(host.Listen(blobProtocol))
		remote.PeerDialer = func(ctx context.Context, nodeID string) (net.Conn, error) {
			return host.Dial(ctx, nodeID, blobProtocol)
		}
		remote.OnPeerFetch = func(c cid.Cid, holder *pb.BlobHolder) {
			log.Debug("fetched a blob from a fellow worker", "cid", c.String(), "node", holder.GetNodeId(), "at", holder.GetAddress(), "route", routeTo(host.Peers(), holder.GetNodeId()))
		}
		if *advertise == "" {
			// With no address of its own to give, the node is asked for by
			// name.
			*advertise = worker.ViaP2P
		}
	}

	if isWorker {
		w := &worker.Worker{
			Name: *name, Coordinator: *join, Credentials: creds,
			Slots: *slots, Workloads: workloads, Blobs: blobs, Log: log,
			ServeAddress: *advertise,
			// What the machine has, as far as its owner offers it.
			Hardware: hardware.Detect(ctx).Offer(*maxMemory<<20, *maxGPUs),
		}
		if *reciprocate {
			// The node works for each node it has said it will take tasks
			// from, whenever that node will have it. All of that shares the
			// node's slots with the work it was started to do.
			local.WorkFor = takes
			w.Limit, w.Pool = worker.NewSlots(*slots), coordinatorID
			guests := &guestWork{ident: ident, dataDir: *dataDir, template: w, kubo: *useKubo, syncCache: *syncCache, maxCache: *maxCache}
			mutual := newReciprocity(&reciprocity{
				// The node it already works for, being its coordinator, is
				// not one to work for a second time.
				trusted: func() []string {
					return slices.DeleteFunc(takes.List(), func(id string) bool { return id == coordinatorID })
				},
				addresses: func(id string) []string { return whereToAsk(host.Peers(), id) },
				ask:       func(ctx context.Context, id, addr string) bool { return trustsThisNode(ctx, ident, id, addr) },
				work:      guests.work,
				every:     reciprocityCheck, most: maxWorkedFor, log: log,
			})
			worksFor = mutual.workingFor
			// A change of trust at either end is acted on at once: this node
			// looks again when its own changes, and tells the node concerned,
			// which looks again when it hears.
			var words sync.WaitGroup
			takes.onChange(mutual.nudge)
			trustChanged.listen(func(id string) {
				mutual.nudge()
				words.Add(1)
				go func() {
					defer words.Done()
					sendWord(ctx, host, id)
				}()
			})
			hearing := host.Listen(trustProtocol)
			go hearWord(hearing, mutual.nudge)
			working := make(chan struct{})
			defer func() {
				cancel()
				hearing.Close()
				takes.onChange(nil)
				trustChanged.listen(nil)
				words.Wait()
				<-working
			}()
			go func() {
				defer close(working)
				mutual.run(ctx)
			}()
		}
		if *relayAt != "" {
			w.RelayAddresses = host.Addrs()
			w.Relayed = host.Carried
			log.Info("relaying between the pool's members", "addrs", w.RelayAddresses)
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
		if !isCoordinator {
			// A worker's pool, as far as it knows, is its coordinator.
			coordinatorAddr := *join
			local.Peers = func() []*nodepb.Peer {
				state := nodepb.PeerConnectionState_PEER_CONNECTION_STATE_DISCONNECTED
				if w.Connected() {
					state = nodepb.PeerConnectionState_PEER_CONNECTION_STATE_CONNECTED
				}
				return []*nodepb.Peer{{PeerId: coordinatorID, ConnectionState: state, KnownAddresses: multiaddrs(coordinatorAddr), TakesWork: true, ThisNodeWorksFor: w.Connected()}}
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

	// The node's planner is there whatever its role, so that its model can
	// be set and its conversations read; it has something to compute on
	// only where the node coordinates a pool.
	local.Assistant = &assistant{store: db, pool: planningPool, workloads: workloads}

	// Whatever its role, the node is connected to the nodes in its address
	// book and those named on the command line, and through them finds
	// others. That goes on beside everything else and ends with the node.
	book, err := db.BootstrapPeers()
	if err != nil {
		return err
	}
	looking, stopLooking := context.WithCancel(ctx)
	var searches sync.WaitGroup
	defer func() {
		stopLooking()
		searches.Wait()
	}()
	local.Connect, local.Listen, local.Book = host.Connect, host.Addrs, db
	local.Bootstrap = func(ctx context.Context, addresses []string) {
		if answered := host.Bootstrap(ctx, addresses); len(addresses) > 0 {
			log.Info("connected to the nodes this one starts from", "answered", answered, "of", len(addresses))
		}
	}
	searches.Add(1)
	go func() {
		defer searches.Done()
		local.Bootstrap(looking, startingPoints(book, *bootstrap))
	}()
	// The desktop client is shown the node's pool and, beside it, the nodes
	// its host has found.
	pool := local.Peers
	local.Peers = func() []*nodepb.Peer {
		return withTrust(withFound(pool(), host.Peers()), takes.List(), worksFor)
	}
	if *locate {
		var country atomic.Value
		searches.Add(1)
		go func() {
			defer searches.Done()
			country.Store(lookUpCountry(looking))
		}()
		local.Country = func() string {
			code, _ := country.Load().(string)
			return code
		}
	}

	if *apiListen != "" {
		lis, err := net.Listen("tcp", *apiListen)
		if err != nil {
			return err
		}
		if local.Token, err = apiToken(filepath.Join(*dataDir, "api.token")); err != nil {
			lis.Close()
			return err
		}
		desktop := api.NewLocalServer(local)
		defer desktop.Stop()
		log.Info("local API listening", "addr", lis.Addr().String())
		go desktop.Serve(lis)
	}

	select {
	case err := <-stopped:
		return err
	case <-ctx.Done():
		log.Info("shutting down")
		return nil
	}
}

// reciprocityCheck is how often a node looks over the nodes it trusts and
// asks those it does not yet work for whether they trust it, in case word
// of a change did not reach it, and
// maxWorkedFor how many it will work for at once.
var reciprocityCheck = time.Minute

const maxWorkedFor = 16

// routeTo says how this node's libp2p host is connected to another: directly,
// through a relay, or not at all. A direct connection beside a relayed one
// is what counts, since it is the one used.
func routeTo(found []p2p.Peer, id string) string {
	route := "none"
	for _, peer := range found {
		for _, conn := range peer.Conns {
			switch {
			case peer.ID != id:
			case !strings.Contains(conn, "/p2p-circuit"):
				route = "direct"
			case route == "none":
				route = "relayed"
			}
		}
	}
	return route
}

// startingPoints lists the addresses a node connects to at startup: its
// address book, then those given on the command line.
func startingPoints(book []nodedb.BootstrapPeer, flag string) []string {
	var addresses []string
	for _, entry := range book {
		addresses = append(addresses, entry.Address)
	}
	for _, address := range strings.Split(flag, ",") {
		if address = strings.TrimSpace(address); address != "" {
			addresses = append(addresses, address)
		}
	}
	return addresses
}

// withFound adds to a node's pool, as the local API lists it, the nodes its
// libp2p host knows of. A node in both is listed once, with the addresses
// the host knows for it, and as connected if either says so. One the host
// alone knows is no part of the pool and so is not trusted for compute.
func withFound(pool []*nodepb.Peer, found []p2p.Peer) []*nodepb.Peer {
	listed := make(map[string]*nodepb.Peer, len(pool))
	for _, member := range pool {
		listed[member.GetPeerId()] = member
	}
	for _, f := range found {
		entry := listed[f.ID]
		if entry == nil {
			entry = &nodepb.Peer{PeerId: f.ID, ConnectionState: nodepb.PeerConnectionState_PEER_CONNECTION_STATE_DISCONNECTED}
			listed[f.ID] = entry
			pool = append(pool, entry)
		}
		for _, address := range f.Addrs {
			if !slices.Contains(entry.KnownAddresses, address) {
				entry.KnownAddresses = append(entry.KnownAddresses, address)
			}
		}
		if f.Connected() {
			entry.ConnectionState = nodepb.PeerConnectionState_PEER_CONNECTION_STATE_CONNECTED
		}
	}
	sort.Slice(pool, func(a, b int) bool { return pool[a].GetPeerId() < pool[b].GetPeerId() })
	return pool
}

// withTrust fills in, for each node the local API lists, the side of trust
// that is this node's willingness to take its tasks, and whether it is
// doing so now. A node this one will work for is listed even if nothing
// else is known of it. A node is trusted for compute if either side holds.
func withTrust(peers []*nodepb.Peer, takes []string, worksFor func(id string) bool) []*nodepb.Peer {
	listed := make(map[string]*nodepb.Peer, len(peers))
	for _, peer := range peers {
		listed[peer.GetPeerId()] = peer
	}
	for _, id := range takes {
		if listed[id] == nil {
			listed[id] = &nodepb.Peer{PeerId: id, ConnectionState: nodepb.PeerConnectionState_PEER_CONNECTION_STATE_DISCONNECTED}
			peers = append(peers, listed[id])
		}
		listed[id].TakesWork = true
	}
	for _, peer := range peers {
		peer.ThisNodeWorksFor = peer.ThisNodeWorksFor || worksFor(peer.GetPeerId())
		peer.TrustedForCompute = peer.GetGivesWork() || peer.GetTakesWork()
	}
	sort.Slice(peers, func(a, b int) bool { return peers[a].GetPeerId() < peers[b].GetPeerId() })
	return peers
}

// countryURL is the service asked which country this node's address is in.
var countryURL = "https://ipapi.co/json/"

var countryCode = regexp.MustCompile(`^[A-Za-z]{2}$`)

// lookUpCountry asks an outside service which country the address this node
// reaches it from is in, and returns its two-letter code, or nothing if the
// service does not give one. The service learns the address by being asked.
func lookUpCountry(ctx context.Context) string {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, countryURL, nil) // the address is ours and well formed
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return ""
	}
	defer res.Body.Close()
	var answer struct {
		CountryCode string `json:"country_code"`
	}
	json.NewDecoder(io.LimitReader(res.Body, 64<<10)).Decode(&answer)
	if !countryCode.MatchString(answer.CountryCode) {
		return ""
	}
	return strings.ToUpper(answer.CountryCode)
}

// blobProtocol names the streams on which one worker's libp2p host asks
// another's for blobs. What runs over them is the same gRPC, with the same
// TLS, that a worker with a port open serves there.
const blobProtocol = "/sisyphus/blob/1"

// memberCheck is how long a worker waits for its coordinator to say whether
// a node that is connecting is a member of the pool.
const memberCheck = 5 * time.Second

// relayAddrs gives a coordinator's address as that of a libp2p relay.
func relayAddrs(hostport, id string) []string {
	var addrs []string
	for _, addr := range multiaddrs(hostport) {
		addrs = append(addrs, addr+"/p2p/"+id)
	}
	return addrs
}

// poolPeers describes the nodes admitted to a coordinator's pool, in order
// of ID, for the local API: whether each is connected, and whether it is
// trusted to run tasks.
func poolPeers(self string, connected []*pb.NodeInfo, members []access.Member) []*nodepb.Peer {
	online := make(map[string]bool, len(connected))
	for _, node := range connected {
		online[node.GetNodeId()] = true
	}
	peers := make([]*nodepb.Peer, 0, len(members))
	for _, member := range members {
		state := nodepb.PeerConnectionState_PEER_CONNECTION_STATE_DISCONNECTED
		if online[member.ID] {
			state = nodepb.PeerConnectionState_PEER_CONNECTION_STATE_CONNECTED
		}
		peers = append(peers, &nodepb.Peer{
			PeerId: member.ID, ConnectionState: state, GivesWork: member.Role == access.Worker,
			// A member whose worker is connected is working for this node.
			WorksForThisNode: online[member.ID],
		})
	}
	return peers
}

// multiaddrs gives a host:port address in the multiaddress form the local
// API speaks, or nothing if it is not an address.
func multiaddrs(hostport string) []string {
	host, port, err := net.SplitHostPort(hostport)
	if err != nil {
		return nil
	}
	kind := "dns"
	if ip := net.ParseIP(host); ip != nil {
		kind = "ip6"
		if ip.To4() != nil {
			kind = "ip4"
		}
	}
	return []string{"/" + kind + "/" + host + "/tcp/" + port}
}

// apiToken returns the secret a local program must present to change this
// node through the local API, kept in file and made the first time.
func apiToken(file string) (string, error) {
	data, err := os.ReadFile(file)
	if err == nil {
		return strings.TrimSpace(string(data)), nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("read API token: %w", err)
	}
	var secret [32]byte
	rand.Read(secret[:]) // never fails; see crypto/rand
	token := hex.EncodeToString(secret[:])
	if err := os.WriteFile(file, []byte(token+"\n"), 0o600); err != nil {
		return "", fmt.Errorf("save API token: %w", err)
	}
	return token, nil
}

// poolSwarm is the private IPFS network of the pool this node belongs to,
// and this node's Kubo daemon on it.
type poolSwarm struct {
	daemon *kubo.Daemon
	// keyFile is where a coordinator keeps the key, and port the port it has
	// opened for the other members' Kubo daemons to connect to its own, or
	// zero if it has opened none.
	keyFile string
	port    int
	// route is how a worker's Kubo reaches its coordinator's.
	route *route

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
//
// A node that has opened no port for its Kubo leaves its own addresses out:
// they lead nowhere from another machine, and it is reached by tunnel.
func (s *poolSwarm) Addresses(ctx context.Context) ([]string, error) {
	own, err := s.daemon.Addresses(ctx)
	if err != nil {
		return nil, err
	}
	if s.port == 0 {
		own = nil
	}
	others, err := s.daemon.PeerAddresses(ctx)
	return append(own, others...), err
}

// Local returns where on this machine the node's Kubo accepts members.
func (s *poolSwarm) Local(ctx context.Context) (string, error) {
	own, err := s.daemon.Addresses(ctx)
	if err != nil {
		return "", err
	}
	return loopbackOf(own)
}

// loopbackOf picks, from a Kubo daemon's addresses, the one that reaches it
// from its own machine, as a host and port.
func loopbackOf(addresses []string) (string, error) {
	for _, address := range addresses {
		if hostport, ok := tcpAddress(address); ok && strings.HasPrefix(hostport, "127.0.0.1:") {
			return hostport, nil
		}
	}
	return "", fmt.Errorf("Kubo is not listening on this machine's loopback address (it listens on %v)", addresses)
}

// tcpAddress gives the host and port of a multiaddress that names a TCP
// port at an IP address, such as /ip4/10.0.0.5/tcp/4101/p2p/12D3Koo.
func tcpAddress(multiaddr string) (hostport string, ok bool) {
	parts := strings.Split(multiaddr, "/")
	if len(parts) < 5 || (parts[1] != "ip4" && parts[1] != "ip6") || parts[3] != "tcp" {
		return "", false
	}
	return net.JoinHostPort(parts[2], parts[4]), true
}

// route is how a worker's Kubo gets to its coordinator's: straight to it
// where that works, and otherwise by a tunnel through the coordinator's own
// port.
type route struct {
	// coordinator is the coordinator's node ID, which is its Kubo's peer ID
	// too, and coordinatorAddr the address this node reaches it at.
	coordinator     string
	coordinatorAddr string
	// tunnel is the address on this machine that leads to the coordinator's
	// Kubo.
	tunnel string
	log    *slog.Logger
}

// probeTimeout is how long a worker gives the coordinator's Kubo to accept a
// direct connection before it settles for the tunnel.
var probeTimeout = 2 * time.Second

// choose takes the addresses a coordinator gave for the pool's Kubo daemons
// and returns the ones this worker's Kubo should use. The other members' are
// kept as they are. The coordinator's are kept if one of them accepts a
// connection, and are otherwise replaced with the tunnel.
func (r *route) choose(addresses []string) []string {
	var others, candidates []string
	host, _, _ := net.SplitHostPort(r.coordinatorAddr)
	sameMachine := net.ParseIP(host).IsLoopback() || host == "localhost"
	for _, address := range addresses {
		if !strings.HasSuffix(address, "/p2p/"+r.coordinator) {
			others = append(others, address)
			continue
		}
		// The coordinator's loopback address means this machine, here,
		// unless the two are the same machine.
		hostport, ok := tcpAddress(address)
		if ip, _, _ := net.SplitHostPort(hostport); ok && (sameMachine || !net.ParseIP(ip).IsLoopback()) {
			candidates = append(candidates, address)
		}
	}

	answers := make([]bool, len(candidates))
	var probes sync.WaitGroup
	for i, address := range candidates {
		probes.Add(1)
		go func() {
			defer probes.Done()
			hostport, _ := tcpAddress(address)
			conn, err := net.DialTimeout("tcp", hostport, probeTimeout)
			if err == nil {
				conn.Close()
				answers[i] = true
			}
		}()
	}
	probes.Wait()
	var direct []string
	for i, address := range candidates {
		if answers[i] {
			direct = append(direct, address)
		}
	}
	if len(direct) > 0 {
		r.log.Info("this node's Kubo connects to the coordinator's directly", "addresses", direct)
		return append(direct, others...)
	}
	r.log.Info("this node's Kubo reaches the coordinator's through the coordinator's own port")
	host, port, _ := net.SplitHostPort(r.tunnel)
	return append([]string{"/ip4/" + host + "/tcp/" + port + "/p2p/" + r.coordinator}, others...)
}

// fetch asks the coordinator how to join the pool's network and works out
// how this worker's Kubo will reach it.
func (s *poolSwarm) fetch(ctx context.Context, coordinator string, creds credentials.TransportCredentials) (key string, peers []string, err error) {
	key, addresses, err := fetchSwarm(ctx, coordinator, creds)
	if err != nil {
		return "", nil, err
	}
	return key, s.route.choose(addresses), nil
}

// listenLoopback opens a port on this machine only, for the system to pick.
var listenLoopback = func() (*net.TCPListener, error) {
	return net.ListenTCP("tcp", &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)})
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
	if err := s.daemon.Rekey(ctx, &kubo.Swarm{Key: key, Port: s.port, Loopback: s.port == 0}); err != nil {
		return err
	}
	s.key = key
	return nil
}

// joinSwarm starts a Kubo daemon, on the repository in repo, as a member of
// the private IPFS network of the pool coordinated at addr by the node with
// the given ID. The daemon runs until leave is called.
//
// A worker's Kubo makes its connections outwards. Where it cannot reach the
// coordinator's directly, it connects to a port on this machine that leads
// there through the coordinator's own.
func joinSwarm(ctx context.Context, ident *identity.Identity, repo, addr, coordinatorID string, creds credentials.TransportCredentials, log *slog.Logger) (swarm *poolSwarm, leave func(), err error) {
	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(creds))
	if err != nil {
		return nil, nil, err
	}
	lis, err := listenLoopback()
	if err != nil {
		conn.Close()
		return nil, nil, fmt.Errorf("listen for this node's Kubo: %w", err)
	}
	forwarding, stop := context.WithCancel(ctx)
	go tunnel.Forward(forwarding, lis, pb.NewTunnelServiceClient(conn), pb.TunnelTarget_TUNNEL_TARGET_SWARM, log)
	swarm = &poolSwarm{route: &route{coordinator: coordinatorID, coordinatorAddr: addr, tunnel: lis.Addr().String(), log: log}}
	key, peers, err := swarm.fetch(ctx, addr, creds)
	if err == nil {
		swarm.daemon, err = kubo.Start(ctx, kubo.Config{Repo: repo, Identity: ident, Swarm: &kubo.Swarm{Key: key, Peers: peers}})
	}
	if err != nil {
		stop()
		conn.Close()
		return nil, nil, err
	}
	swarm.key = key
	return swarm, func() {
		swarm.daemon.Stop()
		stop()
		conn.Close()
	}, nil
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
		key, peers, err := s.fetch(ctx, coordinator, creds)
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
	if err := adoptRustKey(dataDir); err != nil {
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

// collectPeriodically clears out, every interval until ctx ends, what has
// outlived its keeping: stored data nothing pins any longer, and the record
// of jobs that finished long ago.
func collectPeriodically(ctx context.Context, store *storage.Store, prune func(time.Time) (int, error), interval time.Duration, log *slog.Logger) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			done, err := store.GC(ctx, time.Now())
			if err != nil {
				log.Warn("garbage collection failed", "error", err)
			} else if done != (storage.Collected{}) {
				log.Info("collected garbage", "expired_pins", done.ExpiredPins, "blocks", done.Blocks, "bytes", done.Bytes)
			}
			forgotten, err := prune(time.Now())
			if err != nil {
				log.Warn("could not clear out old jobs", "error", err)
			} else if forgotten > 0 {
				log.Info("forgot jobs that finished long ago", "jobs", forgotten)
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
	return dataDirFor(home, goruntime.GOOS, os.Getenv, func(dir string) bool {
		_, err := os.Stat(dir)
		return err == nil
	})
}

// dataDirFor returns where a node keeps its data when not told where: the
// place its operating system sets aside for a program's own data, as the
// Rust daemon chose. A node that already has a ~/.sisyphus, which is where
// earlier versions kept everything, carries on using it.
func dataDirFor(home, goos string, env func(string) string, exists func(string) bool) string {
	if old := filepath.Join(home, ".sisyphus"); exists(old) {
		return old
	}
	switch goos {
	case "windows":
		if local := env("LOCALAPPDATA"); local != "" {
			return filepath.Join(local, "sisyphus")
		}
		return filepath.Join(home, "AppData", "Local", "sisyphus")
	case "darwin":
		return filepath.Join(home, "Library", "Application Support", "sisyphus")
	}
	if data := env("XDG_DATA_HOME"); data != "" {
		return filepath.Join(data, "sisyphus")
	}
	return filepath.Join(home, ".local", "share", "sisyphus")
}

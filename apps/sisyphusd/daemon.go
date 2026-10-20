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
	iofs "io/fs"
	"log/slog"
	"math"
	"net"
	"net/http"
	"net/netip"
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
	"google.golang.org/protobuf/proto"

	"github.com/sisyphus-network/Sisyphus/apps/sisyphusd/access"
	"github.com/sisyphus-network/Sisyphus/apps/sisyphusd/api"
	"github.com/sisyphus-network/Sisyphus/apps/sisyphusd/blobclient"
	"github.com/sisyphus-network/Sisyphus/apps/sisyphusd/coordinator"
	"github.com/sisyphus-network/Sisyphus/apps/sisyphusd/inference"
	"github.com/sisyphus-network/Sisyphus/apps/sisyphusd/p2p"
	"github.com/sisyphus-network/Sisyphus/apps/sisyphusd/pinning"
	"github.com/sisyphus-network/Sisyphus/apps/sisyphusd/planner"
	"github.com/sisyphus-network/Sisyphus/apps/sisyphusd/replication"
	"github.com/sisyphus-network/Sisyphus/apps/sisyphusd/tunnel"
	"github.com/sisyphus-network/Sisyphus/apps/sisyphusd/worker"
	"github.com/sisyphus-network/Sisyphus/packages/geo"
	"github.com/sisyphus-network/Sisyphus/packages/hardware"
	"github.com/sisyphus-network/Sisyphus/packages/identity"
	"github.com/sisyphus-network/Sisyphus/packages/ipfscluster"
	"github.com/sisyphus-network/Sisyphus/packages/kubo"
	"github.com/sisyphus-network/Sisyphus/packages/names"
	"github.com/sisyphus-network/Sisyphus/packages/natpmp"
	"github.com/sisyphus-network/Sisyphus/packages/nodedb"
	nodepb "github.com/sisyphus-network/Sisyphus/packages/protocol/sisyphus/node/v1"
	pb "github.com/sisyphus-network/Sisyphus/packages/protocol/sisyphus/v1"
	"github.com/sisyphus-network/Sisyphus/packages/runtime"
	"github.com/sisyphus-network/Sisyphus/packages/s3"
	"github.com/sisyphus-network/Sisyphus/packages/sealed"
	"github.com/sisyphus-network/Sisyphus/packages/storage"
	"github.com/sisyphus-network/Sisyphus/packages/stun"
)

func runDaemon(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("sisyphusd run", flag.ContinueOnError)
	roles := fs.String("role", "coordinator,worker", "comma-separated roles this node runs: coordinator, worker")
	listen := fs.String("listen", defaultAddr, "coordinator role: address to serve workers and clients on")
	join := fs.String("coordinator", "", "worker-only node: host:port of its coordinator")
	invitation := fs.String("join", "", "worker-only node: an invitation from the coordinator, needed the first time this node connects to it and ignored after that")
	serve := fs.String("serve", "", "worker-only node: address to serve its cached data on to the other workers of its pool, so that they need not all fetch it from the coordinator")
	advertise := fs.String("advertise", "", "worker-only node: the address other workers should use to reach --serve, if not the same")
	modelsOnly := fs.String("models-only", "", "worker role, with --models-from: offer only these of its models, separated by commas: a model as Ollama names it (qwen3:8b), a family (llama3.1, every size of it), or a pattern (gemma*); empty offers them all")
	modelsGPULayers := fs.Int("models-gpu-layers", -1, "worker role, with --models-from: how many of a model's layers Ollama may put on graphics cards for the pool's jobs; 0 keeps the models on the processor and in the machine's memory, -1 lets Ollama put on as many as fit")
	modelsThreads := fs.Int("models-threads", 0, "worker role, with --models-from: how many of the processor's threads a model may use for the pool's jobs; 0 lets Ollama decide")
	modelsContext := fs.Int("models-context", 0, "worker role, with --models-from: how many tokens of a conversation a model holds for the pool's jobs; more takes more memory; 0 lets Ollama decide")
	modelsKeepAlive := fs.String("models-keep-alive", "", "worker role, with --models-from: how long a model stays loaded after it last answered for the pool, such as 10m, or 0 to unload it at once; empty lets Ollama decide")
	modelsFrom := fs.String("models-from", "", "worker role: offer the language models of the Ollama at this address, such as http://127.0.0.1:11434, to the pool's jobs. Whoever may submit jobs can then use them, and this machine sees what they ask")
	inferenceListen := fs.String("inference-listen", "", "coordinator: loopback address to offer the pool's language models on, as a service speaking OpenAI's dialect whose key is the node's API token; off if empty")
	gatewayListen := fs.String("gateway-listen", "", "address to serve stored files on, read-only, by content ID as GET /ipfs/<cid> and by the names this node answers for as GET /ipns/<node ID>, to whoever shows the node's API token; off if empty")
	pinningListen := fs.String("pinning-listen", "", "coordinator: address to serve the IPFS Pinning Service API on, so that ipfs pin remote and other standard tools can have this node keep data; its key is in the file pinning.token in the data directory; off if empty")
	gatewayOpen := fs.Bool("gateway-open", false, "let anyone who can reach --gateway-listen and knows a file's content ID fetch it, without the token. Sealed files are never served")
	publishRecord := fs.Bool("publish-record", false, "coordinator role: keep a description of this node published under its name, which is its ID: its version, the workloads it takes and its pool's members by ID and role. Whoever may resolve the name can read it")
	webListen := fs.String("web-listen", "", "loopback address to serve the local API and web UI for development; off if empty")
	webOrigins := fs.String("web-origin", "", "the origins of the web pages that may use --web-listen, separated by commas, such as http://localhost:5173; pages from anywhere else are refused")
	webUIDir := fs.String("web-ui-dir", "", "directory containing the built web UI to serve beside the local API; needs --web-listen, and is still loopback-only")
	apiListen := fs.String("api-listen", "", "loopback address to serve the local API on, for the desktop client on this machine (it expects 127.0.0.1:50051); off if empty")
	name := fs.String("name", defaultName(), "a label for people to recognise this node by")
	slots := fs.Int("slots", goruntime.NumCPU(), "worker role: how many tasks to run at once")
	dataDir := fs.String("data-dir", defaultDataDir(), "directory for this node's stored data; nodes sharing a machine each need their own")
	taskDir := fs.String("task-dir", "", "with --containers: existing directory for task workspaces and staged inputs; put it on a dedicated filesystem or quota-backed mount to contain disk exhaustion")
	containerRuntime := fs.String("container-runtime", "", "with --containers: have Docker run the pool's tasks with this runtime in place of its usual, such as runsc (gVisor) or kata-runtime, which keep a task further from this machine than an ordinary container does; it must be installed and known to Docker")
	containersReadOnly := fs.Bool("containers-read-only", false, "with --containers: give a task nowhere to write but /output and /tmp, the second held in memory; an image that writes anywhere else will fail")
	containersTmp := fs.Int("containers-tmp-mb", 0, "with --containers-read-only: the most a task may write to /tmp, in megabytes; 0 leaves it to the task's memory")
	containersMemory := fs.Int("containers-memory-mb", 0, "with --containers: the most memory, in megabytes, a task may use, whatever its job asks; 0 leaves it to the job")
	containersCPUs := fs.Float64("containers-cpus", 0, "with --containers: the most processors a task may use, whatever its job asks; 0 leaves it to the job")
	containersTimeout := fs.Duration("containers-timeout", 0, "with --containers: the longest a task may run before this worker stops it, whatever its job allows; 0 leaves it to the job's --timeout")
	containersNetwork := fs.Bool("containers-network", false, "with --containers: let a task reach the network when its job asks to. Without it no task can, and a job that asks for the network is given to workers that allow it, or waits")
	maxJobsPerClient := fs.Int("max-jobs-per-client", 0, "coordinator: how many unfinished jobs one client may have at once; more are refused until one is over. 0 means no limit. The node's own jobs are not counted. An explicitly supplied value overrides the saved setting; if omitted, the saved value is used")
	maxTaskTimeout := fs.Duration("max-task-timeout", 0, "coordinator: the longest one attempt at a task of a client's job may run before it is stopped and tried again; a job may ask for less, and one that names no timeout gets this. 0 means no limit. The node's own jobs are not held to it. An explicitly supplied value overrides the saved setting; if omitted, the saved value is used")
	maxTasksPerClient := fs.Int("max-tasks-per-client", 0, "coordinator: how many unfinished tasks, waiting or running, one client may have across its jobs; a job that would take it past that is refused. 0 means no limit. The node's own jobs are not counted. An explicitly supplied value overrides the saved setting; if omitted, the saved value is used")
	hardenedContainers := fs.Bool("containers-hardened", false, "coordinator: give container jobs only to workers that run them hardened, with a sandboxing --container-runtime and --containers-read-only with a --containers-tmp-mb; such a job waits while none is connected. For a pool that takes container work from strangers")
	distinctAddresses := fs.Bool("verify-distinct-addresses", false, "coordinator: for jobs submitted with --verify, hand each task's copies only to workers at different network addresses, so that two workers on one machine or behind one router cannot vouch for each other. For a pool with strangers in it: in a pool whose workers share an address it leaves too few to verify with")
	maxConnections := fs.Int("max-connections", 0, "how many connections to take at once from other nodes and their command lines, on --listen; 0 takes as many as half the files the system lets this program have open, which on most machines is far more than a pool needs. An eighth of them, and at least 256, may come from one address")
	containers := fs.Bool("containers", false, "worker role: run container images for the pool's jobs, with Docker. This lets whoever may submit jobs to the pool run what they like on this machine")
	maxMemory := fs.Uint64("offer-memory-mb", 0, "worker role: tell the pool this node has no more than this much memory, in mebibytes; 0 offers all it has")
	maxGPUs := fs.Int("offer-gpus", -1, "worker role: tell the pool this node has no more than this many graphics cards; -1 offers all it has")
	reciprocate := fs.Bool("work-for-trusted", true, "a node with both roles: also work for the nodes it trusts for compute, whenever they trust it back")
	relayAt := fs.String("relay", "", "worker-only node: listen on this address, as host:port, for the other members' libp2p hosts and relay between them; the port must be open to them. A coordinator relays already")
	discovery := fs.String("discovery", "on", "whether to look for other nodes, on this network and through the nodes already known, and let them find this one: on or off")
	discoveryNetwork := fs.String("discovery-network", "", "with --discovery on, look only for nodes started with the same --discovery-network, and be found only by them: a separate network, such as one for a test run, of 1 to 32 lowercase letters, digits and hyphens. Empty is the network every Sisyphus node is on")
	bootstrap := fs.String("bootstrap", "", "addresses of nodes to connect to at startup, besides those in the address book: comma-separated, each ending in /p2p/<node ID>")
	stunServer := fs.String("stun-server", "", "if the node cannot tell which country it is in from its own addresses, its pool's view of it or its gateway, ask this STUN server, host or host:port (port 3478 if none is given), which address its traffic comes from. The server learns that address, so none is asked unless named here. Only a Binding request is sent; no port is opened. An explicitly supplied value overrides the saved setting; if omitted, the saved value is used")
	byGateway := fs.Bool("locate-gateway", false, "if the node cannot tell which country it is in from its own addresses or its pool's view of it, ask its default gateway for its external address by NAT-PMP. Only that read-only request is sent, and only to the gateway; no port is opened. The system's default route is used on Linux, macOS, the BSDs and Windows. An explicitly supplied value overrides the saved setting; if omitted, the saved value is used")
	locate := fs.Bool("locate-country", false, "if the node cannot tell which country it is in from its own addresses, its pool's view of it, its gateway or a STUN server, ask ipapi.co, which thereby learns its address and, from the request, that it is a Sisyphus node and which version; it is asked only when the node is asked where it is and nothing else has placed it. A country, or any refusal but a timeout or a rate limit, is kept until the node restarts; a timeout, a rate limit, a fault on its side or no answer lets it be asked again, the next time the node is asked where it is, once a wait has passed that starts at a minute and doubles with each failure in a row up to an hour, or is as long as the service's Retry-After says if that is longer, up to the same hour. Without this nothing is asked of anyone. An explicitly supplied value overrides the saved setting; if omitted, the saved value is used")
	keepJobs := fs.Duration("keep-jobs", 30*24*time.Hour, "coordinator role: how long a finished job can still be asked after; 0 keeps them for good. A client's job is kept at least as long as --retain, since its record says whose its data is")
	retain := fs.Duration("retain", 7*24*time.Hour, "coordinator role: how long a job's inputs and results are kept after it finishes")
	gcInterval := fs.Duration("gc-interval", 0, "coordinator role: optional full-store safety sweep interval; by default collection follows pin changes and expiry deadlines, while explicitly supplying 0 disables automatic collection")
	maxStore := fs.Uint64("max-store-bytes", 0, "coordinator role: refuse uploads once stored data uses this much disk; 0 means no limit")
	maxUploadPerAttempt := fs.Uint64("max-upload-bytes-per-attempt", 0, "coordinator role: the most a worker may upload here for one attempt at a task, across all the attempt's uploads; an upload that would take it past is refused. 0 means no limit. An explicitly supplied value overrides the saved setting; if omitted, the saved value is used")
	maxStorePerClient := fs.Uint64("max-store-bytes-per-client", 0, "coordinator role: the most one client may keep here, its uploads, pins and jobs' data counted as pool members shows; uploads and pins past it are refused, and so is a task of its job whose outputs would take it past, which fails the job. 0 means no limit. What workers upload for its jobs' attempts while they run counts towards it too, shared between them. The node's own are not counted. An explicitly supplied value overrides the saved setting; if omitted, the saved value is used")
	maxCache := fs.Uint64("max-cache-bytes", 0, "worker-only node: evict the least recently used cached blobs once the cache uses this much disk; 0 means no limit")
	s3Endpoint := fs.String("s3-endpoint", "", "coordinator: keep stored data in a bucket of the S3-style object store at this address, such as https://s3.wasabisys.com or http://127.0.0.1:9000, instead of on this machine's disk")
	s3Bucket := fs.String("s3-bucket", "", "the bucket, which must exist")
	s3Prefix := fs.String("s3-prefix", "", "put this before the name of every object, to share the bucket with other things")
	s3Region := fs.String("s3-region", "", "the bucket's region, for stores that have them")
	s3Credentials := fs.String("s3-credentials", "", "a file with the access key on its first line and the secret key on its second; they stay on this node")
	useKubo := fs.Bool("kubo", false, "keep stored data in a Kubo (IPFS) daemon that this node starts and runs alongside itself, on a private network with the rest of its pool; needs the ipfs program installed, and for a worker, a coordinator that uses it too")
	kuboAPISecret := fs.Bool("kubo-api-secret", false, "with --kubo, and not with --cluster: have Kubo's API, which listens on a loopback port, answer only a caller that shows a secret, kept in the file kubo.token in the data directory; the ipfs program then needs --api-auth basic:sisyphus:<the secret> to be used on the node's repository while the node runs")
	useCluster := fs.Bool("cluster", false, "with --kubo: also run an IPFS Cluster peer beside Kubo, so that what the pool's coordinator pins is kept by several of the pool's nodes; needs the ipfs-cluster-service program installed, and for a worker, a coordinator that uses it too")
	swarmPort := fs.Int("swarm-port", 0, "coordinator role with --kubo: TCP port to open so that members' Kubo daemons can connect to this node's directly, which is faster; 0 opens none, and they reach it through --listen")
	replicas := fs.Int("replicas", 0, "coordinator role: how many copies of everything this node has pinned are to be held by other nodes of the pool, besides its own. Without --cluster they are held by storage followers, and 0 asks none to; with --cluster they are held by the cluster's peers, and 0 means 1")
	formerly := fs.String("formerly", "", "coordinator role: the ID this node had before it lost its key; what storage followers hold for that node is kept until \"sisyphusd blob restore\" takes it back")
	replicaDir := fs.String("replica-dir", "", "worker-only node: be a storage follower, keeping in this directory, which must be used for nothing else, copies of whatever stored data its coordinator says to")
	syncCache := fs.Bool("sync-cache", false, "worker-only node: wait for the disk when caching a blob; slower, but the cache then survives a power cut without downloading again")
	verbose := fs.Bool("v", false, "log per-task detail")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}
	gcOptions := garbageCollectionOptionsFrom(fs, *gcInterval)
	if gcOptions.interval < 0 {
		return errors.New("--gc-interval must be zero or positive")
	}
	locationDiscoveryOverrides := locationDiscoveryFlagOverridesFrom(fs, byGateway, locate, stunServer)
	poolLimitsOverrides := poolLimitsFlagOverridesFrom(fs, maxJobsPerClient, maxTasksPerClient, maxTaskTimeout, maxStorePerClient, maxUploadPerAttempt)

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
	if *maxConnections != 0 && *maxConnections < 64 {
		return errors.New("--max-connections must be at least 64, or 0 to take as many as the machine can hold open: a worker alone holds several")
	}
	if host, _, err := net.SplitHostPort(*apiListen); *apiListen != "" && (err != nil || !net.ParseIP(host).IsLoopback()) {
		return errors.New("--api-listen must be a loopback address such as 127.0.0.1:50051: the local API is for programs on this machine only")
	}
	if host, _, err := net.SplitHostPort(*inferenceListen); *inferenceListen != "" && (err != nil || !net.ParseIP(host).IsLoopback()) {
		return errors.New("--inference-listen must be a loopback address such as 127.0.0.1:11435: the service is for programs on this machine only")
	}
	if *inferenceListen != "" && !isCoordinator {
		return errors.New("--inference-listen is for a node that coordinates a pool: it is that pool's models it offers")
	}
	if *pinningListen != "" && !isCoordinator {
		return errors.New("--pinning-listen is for a node that coordinates a pool: it is that pool's store the data is kept in")
	}
	if err := validateWebListen(*webListen); err != nil {
		return err
	}
	if *webUIDir != "" {
		if *webListen == "" {
			return errors.New("--web-ui-dir needs --web-listen")
		}
		info, err := os.Stat(*webUIDir)
		if err != nil || !info.IsDir() {
			return fmt.Errorf("--web-ui-dir must name a directory: %s", *webUIDir)
		}
		if _, err := os.Stat(filepath.Join(*webUIDir, "index.html")); err != nil {
			return fmt.Errorf("--web-ui-dir has no index.html: %s", *webUIDir)
		}
	}
	if *s3Endpoint != "" {
		switch {
		case !isCoordinator:
			return errors.New("--s3-endpoint is for a node that coordinates a pool: it is the pool's stored data that is kept there")
		case *useKubo:
			return errors.New("--s3-endpoint and --kubo each say where stored data is kept: choose one")
		case *s3Bucket == "":
			return errors.New("--s3-endpoint needs --s3-bucket, the bucket to keep stored data in")
		}
	}
	if *kuboAPISecret && !*useKubo {
		return errors.New("--kubo-api-secret needs --kubo: it is Kubo's API that is given the secret")
	}
	if *kuboAPISecret && *useCluster {
		return errors.New("--kubo-api-secret cannot be used with --cluster: an IPFS Cluster peer pins through Kubo's API and has no way to show it a secret")
	}
	if *useCluster && !*useKubo {
		return errors.New("--cluster needs --kubo: it is on Kubo that a cluster peer pins")
	}
	if *publishRecord && !isCoordinator {
		return errors.New("--publish-record is for a node that coordinates a pool: it is the pool it describes")
	}
	if isCoordinator && *invitation != "" {
		return errors.New("--join is for worker-only nodes")
	}
	if isCoordinator && *serve != "" {
		return errors.New("--serve is for worker-only nodes; a coordinator serves its data already")
	}
	if *replicas < 0 || (*replicas > 0 && !isCoordinator) {
		return errors.New("--replicas is a number of copies, for a node that coordinates a pool; a worker-only node offers to hold copies with --replica-dir")
	}
	if *formerly != "" {
		if !isCoordinator {
			return errors.New("--formerly is for a node that coordinates a pool: it names the node its storage followers held copies for before")
		}
		if _, err := identity.Verify(*formerly, nil, nil); err != nil {
			return fmt.Errorf("--formerly: %w", err)
		}
	}
	if isCoordinator && *replicaDir != "" {
		return errors.New("--replica-dir is for worker-only nodes; a coordinator holds its pool's data already")
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
	// A node can take in, split and combine any workload, since that needs
	// nothing but itself. What it will run as a worker is another matter:
	// container images only if its owner has said so.
	confined := runtime.Confinement{TaskDir: *taskDir, Runtime: *containerRuntime, ReadOnly: *containersReadOnly, TmpMB: *containersTmp, Network: *containersNetwork, MemoryMB: *containersMemory, CPUs: *containersCPUs, Timeout: *containersTimeout}
	if confined != (runtime.Confinement{}) && !*containers {
		return errors.New("--task-dir, --container-runtime, --containers-read-only, --containers-tmp-mb, --containers-network, --containers-memory-mb, --containers-cpus and --containers-timeout say how containers are run, and need --containers")
	}
	if confined.MemoryMB < 0 || confined.CPUs < 0 || confined.Timeout < 0 || confined.TmpMB < 0 {
		return errors.New("--containers-memory-mb, --containers-cpus, --containers-timeout and --containers-tmp-mb are the most a task may use, and cannot be less than nothing")
	}
	// The saved pool limits are never negative, so checking the flags
	// checks the limits the node runs with.
	if *maxTaskTimeout < 0 {
		return errors.New("--max-task-timeout cannot be negative")
	}
	if *maxJobsPerClient < 0 || *maxTasksPerClient < 0 {
		return errors.New("--max-jobs-per-client and --max-tasks-per-client cannot be negative")
	}
	if confined.TmpMB > 0 && !confined.ReadOnly {
		return errors.New("--containers-tmp-mb sizes the /tmp that --containers-read-only gives a task, and needs it: without it a task can write anywhere in its image")
	}
	if *taskDir != "" {
		abs, err := resolveTaskDir(*taskDir, absoluteTaskDir)
		if err != nil {
			return err
		}
		*taskDir = abs
		confined.TaskDir = abs
		if err := checkTaskDir(*taskDir); err != nil {
			return err
		}
	}
	workloads, runs := runtime.WithContainers(), runtime.Builtin()
	if *containers {
		if err := checkContainers(ctx, confined); err != nil {
			return fmt.Errorf("--containers: %w", err)
		}
		runs = runtime.ContainersConfined(confined)
	}
	// A graph is a job of jobs, which a coordinator carries out itself: no
	// worker runs one, so only what takes jobs in knows of it.
	workloads = workloads.With(runtime.Graph{})

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
	local := api.LocalConfig{NodeID: ident.ID(), Version: version, Workloads: workloads.Names(), NodeInfoChanges: api.NewNodeInfoChanges(), BrowserDeviceChanges: api.NewNodeInfoChanges()}

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
	// poolConn is a worker node's connection to its coordinator, through
	// which it also reaches the pool's jobs as a client of it.
	var poolConn grpc.ClientConnInterface
	var poolStore poolBlobs

	// With --kubo, Kubo runs as the same peer as this node, on a repository
	// beside the node's other data, and is stopped when the node stops. A
	// coordinator's starts the pool's private network; a worker's joins it.
	var sidecar *kubo.Daemon
	var swarm *poolSwarm
	// Kubo's API is on a loopback port that any program on the machine can
	// find and drive. Asked to, the node has it want a secret, which it
	// keeps beside its other secrets and shows with every call.
	kuboSecret := ""
	if *kuboAPISecret {
		if kuboSecret, err = secretIn(filepath.Join(*dataDir, "kubo.token"), "Kubo API secret"); err != nil {
			return err
		}
	}
	if *useKubo && isCoordinator {
		// With no port of its own opened, a coordinator's Kubo listens on
		// this machine only and members are brought to it through --listen.
		swarm = &poolSwarm{keyFile: filepath.Join(*dataDir, "swarm.key"), port: *swarmPort}
		key, err := swarmKey(swarm.keyFile)
		if err != nil {
			return err
		}
		network := &kubo.Swarm{Key: key, Port: *swarmPort, Loopback: *swarmPort == 0}
		if sidecar, err = kubo.Start(ctx, kubo.Config{Repo: filepath.Join(*dataDir, "ipfs"), Identity: ident, Swarm: network, APISecret: kuboSecret}); err != nil {
			return err
		}
		defer sidecar.Stop()
		swarm.key, swarm.daemon = key, sidecar
	}
	if *useKubo && !isCoordinator {
		var leave func()
		if swarm, leave, err = joinSwarm(ctx, ident, filepath.Join(*dataDir, "ipfs"), kuboSecret, *join, coordinatorID, creds, log); err != nil {
			return err
		}
		defer leave()
		sidecar = swarm.daemon
	}
	if *useKubo {
		log.Info("kubo started", "repo", filepath.Join(*dataDir, "ipfs"))
	}

	// With --cluster as well, a cluster peer runs beside Kubo, as the same
	// peer again, and is stopped before Kubo is. A coordinator's is the one
	// member of the pool's cluster that says what is pinned; a worker's
	// follows it.
	var cluster *poolCluster
	if *useCluster {
		peer := ipfscluster.Config{Dir: filepath.Join(*dataDir, "ipfs-cluster"), Identity: ident, Name: *name, Heartbeat: clusterHeartbeat}
		if isCoordinator {
			peer.Secret, peer.KuboAPI, peer.Trusted = clusterSecret(swarm.Key()), sidecar.Address(), []string{ident.ID()}
			if swarm.cluster, err = ipfscluster.Start(ctx, peer); err != nil {
				return err
			}
			defer swarm.cluster.Stop()
			cluster = &poolCluster{peer: swarm.cluster.Client, replicas: clusterHolders(*replicas)}
		} else {
			leave, err := joinCluster(ctx, swarm, peer)
			if err != nil {
				return err
			}
			defer leave()
		}
		log.Info("cluster peer started", "dir", peer.Dir)
	}

	// Every node keeps a blob store. A coordinator's is durable, because it
	// is where a job's inputs and results live. A worker-only node's is a
	// cache of what the pool holds, kept apart from any durable store. With
	// Kubo, either kind keeps its blocks there. Deferred calls run last-in
	// first-out, so the store closes only after everything using it has
	// stopped.
	var store *storage.Store
	// pinsDir is where a coordinator's store keeps its pins.
	pinsDir := filepath.Join(*dataDir, "blobs")
	switch {
	case *s3Endpoint != "":
		bucket, err := openBucket(ctx, *s3Endpoint, *s3Bucket, *s3Prefix, *s3Region, *s3Credentials)
		if err != nil {
			return err
		}
		log.Info("stored data is kept in a bucket", "endpoint", *s3Endpoint, "bucket", *s3Bucket)
		pinsDir = filepath.Join(*dataDir, "s3-pins")
		if store, err = storage.OpenBucket(bucket, pinsDir); err != nil {
			return explainStoreError(err)
		}
	case isCoordinator && *useKubo:
		pinsDir = filepath.Join(*dataDir, "kubo-pins")
		store, err = storage.OpenKubo(sidecar, pinsDir)
	case isCoordinator:
		store, err = storage.OpenLocal(pinsDir)
	case *useKubo:
		store = storage.OpenKuboCache(sidecar)
	default:
		store, err = storage.OpenCache(filepath.Join(*dataDir, "cache"), *syncCache)
	}
	if err != nil {
		return explainStoreError(err)
	}
	defer store.Close()
	// A node that is its own coordinator reads and writes the one store
	// directly; a worker-only node fetches into its cache.
	var blobs runtime.Blobs = store
	// readable is the store as the node's gateway reads it.
	var readable api.Opener = store

	// What a node must not forget is kept in its database: the addresses
	// it starts from and, on a coordinator, who has been admitted, the
	// invitations still out, and its jobs, so that a restart picks up the
	// unfinished ones where they were.
	db, err := nodedb.Open(filepath.Join(*dataDir, "node.db"))
	if err != nil {
		return err
	}
	defer db.Close()
	gatewayDiscoverySupported := supportsGatewayDiscovery(goruntime.GOOS)
	locationDiscoveryRunning, err := startupLocationDiscovery(db, nodedb.LocationDiscovery{
		LocateGateway: *byGateway,
		LocateCountry: *locate,
		StunServer:    *stunServer,
	}, gatewayDiscoverySupported, locationDiscoveryOverrides)
	if err != nil {
		return err
	}
	local.LocationDiscovery = db
	local.LocationDiscoveryRunning = locationDiscoveryRunning
	local.GatewayDiscoverySupported = gatewayDiscoverySupported
	poolLimits, err := startupPoolLimits(db, nodedb.PoolLimits{
		MaxJobsPerClient:         *maxJobsPerClient,
		MaxTasksPerClient:        *maxTasksPerClient,
		MaxTaskTimeout:           *maxTaskTimeout,
		MaxStoreBytesPerClient:   *maxStorePerClient,
		MaxUploadBytesPerAttempt: *maxUploadPerAttempt,
	}, poolLimitsOverrides)
	if err != nil {
		return err
	}
	local.PoolLimits = db
	local.PoolLimitsRunning = poolLimits
	local.RunsCoordinator = isCoordinator
	local.BrowserAccess = db
	var allowedWebOrigins []string
	for _, origin := range strings.Split(*webOrigins, ",") {
		if origin = strings.TrimSpace(origin); origin != "" {
			allowedWebOrigins = append(allowedWebOrigins, strings.TrimRight(origin, "/"))
		}
	}
	var webUI iofs.FS
	if *webUIDir != "" {
		webUI = os.DirFS(*webUIDir)
	}
	webAccess := &webAccessRuntime{db: db, dataDir: *dataDir, local: &local, origins: allowedWebOrigins, ui: webUI, log: log}
	local.WebAccess = webAccess
	// The models a worker lends are those its owner gave when starting it
	// or, if none were given, those saved from the desktop app.
	lent := nodedb.ModelLending{Enabled: *modelsFrom != "", URL: *modelsFrom, Only: listOf(*modelsOnly),
		GPULayers: *modelsGPULayers, Threads: *modelsThreads, Context: *modelsContext, KeepAlive: *modelsKeepAlive}
	if !lent.Enabled {
		if lent, err = db.ModelLending(); err != nil {
			return err
		}
	}
	if lent.Enabled {
		local.LiveLending = runtime.NewModelServer(lent.URL, runtime.OwnerFromSettings(lent.Only, lent.GPULayers, lent.Threads, lent.Context, lent.KeepAlive))
		runs = runs.With(runtime.WithLiveModels(local.LiveLending)...)
	}
	local.Lending, local.Lent, local.LentByFlags = db, lent, *modelsFrom != ""
	// host is the node's libp2p host, started below according to its role.
	var host *p2p.Host
	var mutual atomic.Pointer[reciprocity]
	// trustChanged hears of each node the coordinator admits or removes, and
	// worksFor says whether this node is working for another.
	trustChanged := new(notices)
	// planningPool is the pool the node's planner computes on: the one it
	// coordinates, or the one it has joined, as a client of it.
	var planningPool planner.Pool
	// attachmentPins keeps chat attachments for their chats: the store of
	// the pool the node coordinates, or of the one it has joined (#500).
	var attachmentPins attachmentPinStore
	// thinkingPool is the same pool as its models are asked of.
	var thinkingPool inference.Pool
	// offered says whether any worker now connected runs a workload. The
	// planner is told only of those, so that it asks for nothing the pool
	// would leave waiting. It is set where there is a pool to plan for.
	var offered func(workload string) bool
	worksFor := func(string) bool { return false }
	// seenAs is this node's address as the pool it is in sees it, if that
	// is a global one: as its workers reach it, for a coordinator, or as
	// its coordinator sees it, for a worker. Every node is one or the
	// other, and it is set below for each.
	var seenAs func() netip.Addr
	// takes is the other side of trust: the nodes this one will take tasks
	// from, whatever else it does.
	takes, err := openWilling(db)
	if err != nil {
		return err
	}
	// held is the names this node answers for. A node that coordinates no
	// pool is handed no records, and so answers for none.
	held := api.NewNames(db, func(string) bool { return false })

	if isCoordinator {
		recordChanges := make(chan struct{}, 1)
		// coord is the pool this node coordinates, once it has been made.
		var coord *coordinator.Coordinator
		admitted, err := access.Open(db, ident.ID())
		if err == nil {
			// Earlier versions kept the list in a file of its own.
			err = admitted.Import(filepath.Join(*dataDir, "access.json"))
		}
		if err != nil {
			return err
		}
		admitted.OnChange(func(id string) {
			// A connected worker is told the roles it now holds.
			if coord != nil {
				coord.RolesChanged(id)
			}
			trustChanged.tell(id)
			select {
			case recordChanges <- struct{}{}:
			default:
			}
			local.NodeInfoChanges.Notify()
		})
		// And a node that ran the Rust daemon before this one keeps what
		// that one knew.
		if err := importRustState(*dataDir, db, admitted, takes, log); err != nil {
			return err
		}
		// Storage followers hold copies of what the store has pinned, and
		// know it by a name kept beside its pins. A store that has lost its
		// pins has thereby lost its name, which is how the followers' copies
		// of what it held are told from blobs that were merely released.
		storeID, err := storeName(pinsDir)
		if err != nil {
			return err
		}
		// With a cluster it is the cluster's peers that keep the copies, and
		// storage followers are asked for none.
		followerCopies := *replicas
		if *useCluster {
			followerCopies = 0
		}
		replicated := replication.New(replication.Config{
			Identity: ident, Store: store, StoreID: storeID, Formerly: *formerly, Replicas: followerCopies, Log: log,
			Address: func(id string) string { return coord.ServeAddress(id) },
			// A follower is reached as one worker reaches another: at the
			// address it gave, or by name through the node's libp2p host.
			Fetch: func(ctx context.Context, from *pb.BlobHolder, c cid.Cid, into blobclient.Putter) error {
				return worker.FetchFromPeer(ctx, from, c, into,
					func(nodeID string) credentials.TransportCredentials {
						return credentials.NewTLS(ident.ClientTLS(nodeID))
					},
					func(ctx context.Context, nodeID string) (net.Conn, error) {
						return host.Dial(ctx, nodeID, blobProtocol)
					})
			},
		})
		replicationCtx, stopReplication := context.WithCancel(ctx)
		replicationDone := make(chan struct{})
		go func() {
			defer close(replicationDone)
			replicated.Run(replicationCtx)
		}()
		defer func() {
			stopReplication()
			<-replicationDone
		}()
		store.OnPinChange(replicated.PinsChanged)
		if followerCopies > 0 {
			log.Info("storage followers are to hold copies of what this node has pinned", "copies", followerCopies)
		}
		// Everything else reads the store through this: a blob it turns out
		// to lack is first fetched back from a follower.
		kept := replication.Recovering{Store: store, From: replicated}
		blobs, readable = kept, kept
		// A node that is its own worker can run a task again to check what
		// another worker returned for it.
		var audits *runtime.Registry
		if isWorker {
			audits = runs
		}
		pruneChanges := make(chan struct{}, 1)
		coord = coordinator.New(coordinator.Config{
			Name: *name, RolesOf: func(id string) []pb.Role { return rolesInPool(admitted, id) },
			ID: ident.ID(), Sign: ident.Sign, Workloads: workloads, Audits: audits, DistinctAddresses: *distinctAddresses, HardenedContainers: *hardenedContainers, MaxJobsPerClient: poolLimits.MaxJobsPerClient, MaxTasksPerClient: poolLimits.MaxTasksPerClient, MaxStoreBytesPerClient: poolLimits.MaxStoreBytesPerClient, MaxUploadBytesPerAttempt: poolLimits.MaxUploadBytesPerAttempt, MaxTaskTimeout: poolLimits.MaxTaskTimeout, Store: kept, ClientOf: api.ClientOf, Journal: db, Standings: db, Retain: *retain, KeepJobs: *keepJobs, Log: log,
			WorkersChanged: local.NodeInfoChanges.Notify,
			JobsChanged:    local.NodeInfoChanges.Notify,
			PruneChanged: func() {
				notifyPrune(pruneChanges)
			},
		})
		defer coord.Close()
		seenAs = coord.Observed
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
		if err == nil {
			// What a client kept here is let go when it is removed. One
			// whose release failed, or was removed before releasing was
			// done, is let go now.
			err = api.ReleaseFormerClients(store, func(id string) bool {
				_, member := admitted.Roles(id)
				return member
			})
		}
		if err != nil {
			return err
		}
		if unfinished > 0 {
			log.Info("took up the jobs left unfinished", "jobs", unfinished)
		}
		prunerStopped := make(chan struct{})
		defer func() {
			cancel()
			<-prunerStopped
		}()
		go func() {
			defer close(prunerStopped)
			runJobPruner(ctx, coord.NextPruneAt, coord.Prune, pruneChanges, log)
		}()
		// The node listens on one port for two kinds of caller: its gRPC
		// clients and workers, and the libp2p hosts of its pool's members,
		// for which it relays so that two of them with no port open can
		// still reach each other. The nodes whose work this one takes are
		// let in too: it is in their pools, if not they in its own.
		host, err = p2p.New(p2p.Config{Identity: ident, MaxConnections: *maxConnections, Listen: *listen, Relay: true, Discover: *discovery == "on", Network: *discoveryNetwork, Log: log, PeerChanged: local.NodeInfoChanges.Notify, Allow: func(id string) bool {
			_, admitted := admitted.Roles(id)
			return admitted || slices.Contains(takes.List(), id)
		}})
		if err != nil {
			return err
		}
		defer host.Close()
		trustListener := host.Listen(trustProtocol)
		trustCtx, stopTrust := context.WithCancel(ctx)
		trustDone := make(chan struct{})
		go func() {
			defer close(trustDone)
			serveTrustChanges(trustCtx, trustListener, trustChanged, func(id string) bool {
				return admitted.Has(id, access.Worker)
			})
		}()
		defer func() {
			stopTrust()
			<-trustDone
		}()
		lis, _ := host.TLSListener() // fails only if asked for twice
		// A coordinator answers for its own name and for those of the nodes
		// it has admitted. With Kubo, what it is handed goes on to the
		// pool's private network as well.
		held = api.NewNames(db, func(id string) bool {
			_, admitted := admitted.Roles(id)
			return admitted
		})
		if sidecar != nil {
			held.OnPublish(func(record *names.Record) { carryName(ctx, sidecar, record, log) })
		}
		config := api.Config{Identity: ident, Access: admitted, Coordinator: coord, Store: store, MaxStoreBytes: *maxStore, MaxStoreBytesPerClient: poolLimits.MaxStoreBytesPerClient, WorkFor: takes, Names: held, Replication: replicated}
		if swarm != nil {
			config.Swarm = swarm
			coord.AnnounceSwarm(swarm.Fingerprint())
		}
		if cluster != nil {
			config.Cluster = cluster
			// What the store pins, the cluster is told to pin, for as long
			// as the node runs. The store must outlive the telling.
			mirror := newClusterPins(store.Kept, cluster.peer, ident.ID(), clusterHolders(*replicas), log)
			store.OnPinChange(func() {
				mirror.nudge()
				replicated.PinsChanged()
			})
			mirrored := make(chan struct{})
			defer func() {
				cancel()
				<-mirrored
			}()
			go func() {
				defer close(mirrored)
				mirror.run(ctx)
			}()
		}
		var gcChanges *gcChangeSignals
		if gcOptions.enabled {
			gcChanges = newGCChangeSignals()
			store.OnGCChange(gcChanges.notify)
		}
		srv := api.NewServer(config)
		local.Pool = api.NewPoolAdmin(config)
		local.Jobs = coord
		local.Store, local.Files, local.MaxStoreBytes = kept, db, *maxStore
		attachmentPins = kept
		local.SealingKey = func() (sealed.Key, error) { return sealingKey(filepath.Join(*dataDir, "private.key")) }
		planningPool, thinkingPool = coord, coord
		offered = offeredBy(coord.Nodes)
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
		if *publishRecord {
			keeper := &recordKeeper{ident: ident, store: store, names: held, log: log, changed: recordChanges, describe: func() []byte {
				return describeNode(ident.ID(), version, workloads.Names(), admitted.Members())
			}}
			held.OnChange(func(id string) {
				if id == ident.ID() {
					keeper.nudge()
				}
			})
			described := make(chan struct{})
			// The store and the database must outlive the keeper.
			defer func() {
				cancel()
				<-described
			}()
			go func() {
				defer close(described)
				keeper.run(ctx)
			}()
		}
		// The node's own worker connects to it as any other would, and
		// expects to find the node itself at the other end.
		*join = loopback(lis.Addr())

		if gcChanges != nil {
			collected := make(chan struct{})
			// The store must outlive the collector.
			defer func() {
				cancel()
				<-collected
			}()
			go func() {
				defer close(collected)
				collectOnChanges(ctx, store, gcOptions.interval, gcChanges, log)
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
		poolConn, poolStore = remote.Conn(), remote
		members := worker.NewMembers(remote.Conn())
		go members.RunRelayWatch(ctx, log)
		// A storage follower keeps a second store, a durable one, for the
		// copies its coordinator has it hold, and serves what is in either.
		var served api.Opener = store
		if *replicaDir != "" {
			copies, err := storage.OpenLocal(*replicaDir)
			if err != nil {
				return fmt.Errorf("--replica-dir: %w", err)
			}
			defer copies.Close()
			served = api.Stores(copies, store)
			// The list the coordinator signed of what to hold is kept with
			// the copies: it is of no use without them, nor they without it
			// to a coordinator that has lost its store.
			follower := &replication.Follower{Store: copies, ListFile: filepath.Join(*replicaDir, "kept.list"), Coordinator: pb.NewBlobServiceClient(remote.Conn()), CoordinatorID: coordinatorID, Log: log}
			following := make(chan struct{})
			// The store of copies must outlive whatever fills it.
			defer func() {
				cancel()
				<-following
			}()
			go func() {
				defer close(following)
				follower.Run(ctx)
			}()
			log.Info("holding copies of the pool's stored data, as its coordinator directs", "dir", *replicaDir)
		}
		peers := api.NewPeerServer(ident, served, members.IsMember)
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
		host, err = p2p.New(p2p.Config{Identity: ident, MaxConnections: *maxConnections, Listen: *relayAt, Relay: *relayAt != "", Via: relayAddrs(*join, coordinatorID), MoreRelays: members.Relays, MoreRelaysChanged: members.RelayChanges(), Discover: *discovery == "on", Network: *discoveryNetwork, Log: log, PeerChanged: func() {
			local.NodeInfoChanges.Notify()
			if watcher := mutual.Load(); watcher != nil {
				watcher.nudge()
			}
		}, Allow: func(id string) bool {
			if id == coordinatorID || slices.Contains(takes.List(), id) {
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
			Slots: *slots, Workloads: runsAs(*name, runs), Blobs: blobs, Log: log,
			ServeAddress: *advertise,
			// What the machine has, as far as its owner offers it.
			Hardware:          hardware.Detect(ctx).Offer(*maxMemory<<20, *maxGPUs),
			HeartbeatInterval: workerHeartbeat,
		}
		local.OffersChanged = w.OffersChanged
		if *reciprocate {
			// The node works for each node it has said it will take tasks
			// from, whenever that node will have it. All of that shares the
			// node's slots with the work it was started to do.
			local.WorkFor = takes
			w.Limit, w.Pool = worker.NewSlots(*slots), coordinatorID
			guests := &guestWork{ident: ident, dataDir: *dataDir, template: w, kubo: *useKubo, kuboSecret: kuboSecret, syncCache: *syncCache, maxCache: *maxCache}
			local.OffersChanged = func() {
				w.OffersChanged()
				guests.offersChanged()
			}
			watcher := newReciprocity(&reciprocity{
				// The node it already works for, being its coordinator, is
				// not one to work for a second time.
				trusted: func() []string {
					return slices.DeleteFunc(takes.List(), func(id string) bool { return id == coordinatorID })
				},
				addresses: func(id string) []string { return whereToAsk(host.Peers(), id) },
				watch:     func(ctx context.Context, id string) (net.Conn, error) { return dialTrust(ctx, host, id) },
				work:      guests.work,
				changed:   local.NodeInfoChanges.Notify,
				most:      maxWorkedFor, log: log,
			})
			mutual.Store(watcher)
			worksFor = watcher.workingFor
			local.Join = joinFromDesktop(*dataDir, ident, host.Connect, db, takes, watcher.nudge)
			// Changes to local trust and to known peer connections immediately
			// reconcile the watcher set; remote trust changes arrive on streams.
			takes.onChange(watcher.nudge)
			working := make(chan struct{})
			defer func() {
				cancel()
				takes.onChange(nil)
				<-working
				mutual.Store(nil)
			}()
			go func() {
				defer close(working)
				watcher.run(ctx)
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
		// leaving is how a worker-only node leaves the pool it works for.
		var leaving *poolDeparture
		if !isCoordinator {
			seenAs = w.Observed
			// A worker's pool, as far as it knows, is its coordinator.
			coordinatorAddr := *join
			local.JoinedPool = func() *nodepb.JoinedPool { return joinedPool(w.JoinedPool(), coordinatorAddr) }
			local.JoinedPoolChanges = api.NewNodeInfoChanges()
			jobs := newPoolJobs(ctx, pb.NewNodeServiceClient(poolConn), db, w.JoinedPool, local.NodeInfoChanges.Notify)
			local.Jobs = jobs
			// Its user's files go to that pool too, kept there for this
			// node as a client, and so do its chats' attachments; their
			// names, and who on the node keeps each, stay here (#500).
			files := &poolFiles{blobs: poolStore, client: pb.NewBlobServiceClient(poolConn), holds: db, coordinator: func() string { return w.JoinedPool().ID }, available: jobs.Available}
			local.Store, local.Files, attachmentPins = files, db, files
			local.FileScope = func() string { return w.JoinedPool().ID }
			leaving = newPoolDeparture(pb.NewPoolServiceClient(poolConn), db.ForgetPool, coordinatorID, coordinatorAddr, *dataDir, log)
			local.LeavePool = leaving.leave
			// Its planner plans on that pool too, and only while it may.
			planningPool, offered = jobs, offeredBy(jobs.Nodes)
			local.SealingKey = func() (sealed.Key, error) { return sealingKey(filepath.Join(*dataDir, "private.key")) }
			w.OnPool = func() {
				local.JoinedPoolChanges.Notify()
				local.NodeInfoChanges.Notify()
				jobs.Resume()
			}
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
			err := w.Run(ctx)
			if leaving != nil {
				err = leaving.workerStopped(ctx, err)
			}
			stopped <- err
		}()
	}

	// The node's planner is there whatever its role, so that its model can
	// be set and its conversations read; it has something to compute on
	// only where the node coordinates a pool.
	planning := &assistant{store: db, pool: planningPool, workloads: workloads, offered: offered, sealingKey: local.SealingKey, filePins: attachmentPins, filePinChanges: make(chan struct{}, 1)}
	local.Assistant = planning
	// What a conversation's attachments ask of the store is queued with the
	// conversation, and applied here: at once, for what a stop left
	// undone, and again for whatever fails.
	pinCtx, stopPins := context.WithCancel(ctx)
	pinDone := make(chan struct{})
	defer func() { stopPins(); <-pinDone }()
	go func() {
		defer close(pinDone)
		replayFilePinsOnChanges(pinCtx, planning, filePinRetryInterval, log)
	}()

	// Whatever its role, the node is connected to the nodes in its address
	// book and those named on the command line, and through them finds
	// others. That goes on beside everything else and ends with the node.
	book, err := db.BootstrapPeers()
	if err != nil {
		return err
	}
	looking, stopLooking := context.WithCancel(ctx)
	var searches background
	defer func() {
		stopLooking()
		searches.stop()
	}()
	local.Connect, local.Listen, local.Book = host.Connect, host.Addrs, db
	local.Bootstrap = func(ctx context.Context, addresses []string) {
		if answered := host.Bootstrap(ctx, addresses); len(addresses) > 0 {
			log.Info("connected to the nodes this one starts from", "answered", answered, "of", len(addresses))
		}
	}
	searches.start(func() {
		local.Bootstrap(looking, startingPoints(book, *bootstrap))
	})
	// The desktop client is shown the node's pool and, beside it, the nodes
	// its host has found.
	pool := local.Peers
	local.Peers = func() []*nodepb.Peer {
		return withTrust(withFound(pool(), host.Peers()), takes.List(), worksFor)
	}
	// The node places itself by the addresses it is reached at, from a
	// table it carries. Only if told to does it ask anyone.
	where := &whereabouts{
		own: host.Addrs, seen: func() netip.Addr { return seenAs() },
		stunServer: locationDiscoveryRunning.StunServer, lookUp: locationDiscoveryRunning.LocateCountry,
		ctx: looking, start: searches.start, notify: local.NodeInfoChanges.Notify, log: log,
	}
	where.gatewayAnswered.Store(!locationDiscoveryRunning.LocateGateway)
	if locationDiscoveryRunning.LocateGateway {
		searches.start(func() {
			where.gateway.Store(gatewaysAddress(looking, log))
			where.gatewayAnswered.Store(true)
			local.NodeInfoChanges.Notify()
		})
	}
	where.stunAnswered.Store(locationDiscoveryRunning.StunServer == "")
	local.Country = where.country

	if *inferenceListen != "" {
		lis, err := net.Listen("tcp", *inferenceListen)
		if err != nil {
			return err
		}
		token, err := apiToken(filepath.Join(*dataDir, "api.token"))
		if err != nil {
			lis.Close()
			return err
		}
		models := &http.Server{Handler: inference.Handler(thinkingPool, token)}
		defer models.Close()
		log.Info("the pool's models are offered", "addr", lis.Addr().String())
		go models.Serve(lis)
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
		defer stopServing(desktop, localAPIGrace)
		log.Info("local API listening", "addr", lis.Addr().String())
		go desktop.Serve(lis)
	}

	if *gatewayListen != "" {
		lis, err := net.Listen("tcp", *gatewayListen)
		if err != nil {
			return err
		}
		token, err := apiToken(filepath.Join(*dataDir, "api.token"))
		if err != nil {
			lis.Close()
			return err
		}
		gateway := &http.Server{Handler: api.NewGateway(readable, held, token, *gatewayOpen)}
		defer gateway.Close()
		log.Info("gateway listening", "addr", lis.Addr().String(), "open", *gatewayOpen)
		if !*gatewayOpen {
			warnOfKeyInTheClear(log, lis.Addr(), "the gateway", "the node's API token")
		}
		go gateway.Serve(lis)
	}

	if *pinningListen != "" {
		lis, err := net.Listen("tcp", *pinningListen)
		if err != nil {
			return err
		}
		// A key of its own, not the node's API token: it is handed to other
		// programs and other machines, and should let them do no more than
		// ask for data to be kept.
		token, err := secretIn(filepath.Join(*dataDir, "pinning.token"), "pinning token")
		if err != nil {
			lis.Close()
			return err
		}
		config := pinning.Config{Store: store, Records: db, Token: token, MaxStoreBytes: *maxStore, Log: log}
		if sidecar != nil {
			// What the node does not hold, its Kubo fetches.
			config.Network = sidecar
		}
		service, err := pinning.NewService(config)
		if err != nil {
			lis.Close()
			return err
		}
		pins := &http.Server{Handler: service}
		// The fetches stop before the store they fill closes.
		defer service.Close()
		defer pins.Close()
		log.Info("pinning service listening", "addr", lis.Addr().String(), "fetches", sidecar != nil)
		warnOfKeyInTheClear(log, lis.Addr(), "the pinning service", "its key")
		go pins.Serve(lis)
	}

	if *webListen != "" {
		lis, err := net.Listen("tcp", *webListen)
		if err != nil {
			return err
		}
		if local.Token, err = apiToken(filepath.Join(*dataDir, "api.token")); err != nil {
			lis.Close()
			return err
		}
		if webAccess.browserAccess == nil {
			webAccess.browserAccess = api.NewBrowserAccessManager(local)
		}
		pages := &http.Server{Handler: api.NewWebAppHandlerWithBrowserAccess(local, allowedWebOrigins, webUI, webAccess.browserAccess)}
		defer pages.Close()
		log.Info("local API listening for development web pages", "addr", lis.Addr().String(), "origins", allowedWebOrigins, "ui", *webUIDir != "")
		go pages.Serve(lis)
	}
	return waitForDaemonStop(ctx, &local, webAccess, stopped, log)
}

type locationDiscoverySettingsStore interface {
	LocationDiscovery() (nodedb.LocationDiscovery, bool, error)
}

type locationDiscoveryFlagOverrides struct {
	LocateGateway *bool
	LocateCountry *bool
	StunServer    *string
}

func checkTaskDir(dir string) error {
	return checkTaskDirWith(dir, os.Stat, os.CreateTemp)
}

var absoluteTaskDir = filepath.Abs

func resolveTaskDir(dir string, abs func(string) (string, error)) (string, error) {
	resolved, err := abs(dir)
	if err != nil {
		return "", fmt.Errorf("resolve --task-dir: %w", err)
	}
	return resolved, nil
}

func checkTaskDirWith(dir string, stat func(string) (os.FileInfo, error), createTemp func(string, string) (*os.File, error)) error {
	info, err := stat(dir)
	if err != nil {
		return fmt.Errorf("--task-dir must name an existing, writable directory: %w", err)
	}
	if !info.IsDir() {
		return fmt.Errorf("--task-dir must name an existing directory: %s is not a directory", dir)
	}
	probe, err := createTemp(dir, ".sisyphus-write-check-")
	if err != nil {
		return fmt.Errorf("--task-dir must name an existing, writable directory: %w", err)
	}
	name := probe.Name()
	defer func() {
		probe.Close()
		os.Remove(name)
	}()
	return nil
}

func locationDiscoveryFlagOverridesFrom(fs *flag.FlagSet, locateGateway, locateCountry *bool, stunServer *string) locationDiscoveryFlagOverrides {
	var overrides locationDiscoveryFlagOverrides
	fs.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "locate-gateway":
			overrides.LocateGateway = locateGateway
		case "locate-country":
			overrides.LocateCountry = locateCountry
		case "stun-server":
			overrides.StunServer = stunServer
		}
	})
	return overrides
}

func startupLocationDiscovery(store locationDiscoverySettingsStore, defaults nodedb.LocationDiscovery, gatewayDiscoverySupported bool, overrides locationDiscoveryFlagOverrides) (nodedb.LocationDiscovery, error) {
	running := defaults
	saved, configured, err := store.LocationDiscovery()
	if err != nil {
		return nodedb.LocationDiscovery{}, err
	}
	if configured {
		running = saved
	}
	if overrides.LocateGateway != nil {
		running.LocateGateway = *overrides.LocateGateway
	}
	if overrides.LocateCountry != nil {
		running.LocateCountry = *overrides.LocateCountry
	}
	if overrides.StunServer != nil {
		running.StunServer = *overrides.StunServer
	}
	if !gatewayDiscoverySupported {
		running.LocateGateway = false
	}
	return running, nil
}

type poolLimitsStore interface {
	PoolLimits() (nodedb.PoolLimits, bool, error)
}

type poolLimitsFlagOverrides struct {
	MaxJobsPerClient         *int
	MaxTasksPerClient        *int
	MaxTaskTimeout           *time.Duration
	MaxStoreBytesPerClient   *uint64
	MaxUploadBytesPerAttempt *uint64
}

func poolLimitsFlagOverridesFrom(fs *flag.FlagSet, maxJobsPerClient, maxTasksPerClient *int, maxTaskTimeout *time.Duration, maxStoreBytesPerClient, maxUploadBytesPerAttempt *uint64) poolLimitsFlagOverrides {
	var overrides poolLimitsFlagOverrides
	fs.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "max-jobs-per-client":
			overrides.MaxJobsPerClient = maxJobsPerClient
		case "max-tasks-per-client":
			overrides.MaxTasksPerClient = maxTasksPerClient
		case "max-task-timeout":
			overrides.MaxTaskTimeout = maxTaskTimeout
		case "max-store-bytes-per-client":
			overrides.MaxStoreBytesPerClient = maxStoreBytesPerClient
		case "max-upload-bytes-per-attempt":
			overrides.MaxUploadBytesPerAttempt = maxUploadBytesPerAttempt
		}
	})
	return overrides
}

// startupPoolLimits works out the limits the node runs with: each one given
// on the command line, else the saved one, else the flag's default.
func startupPoolLimits(store poolLimitsStore, defaults nodedb.PoolLimits, overrides poolLimitsFlagOverrides) (nodedb.PoolLimits, error) {
	running := defaults
	saved, configured, err := store.PoolLimits()
	if err != nil {
		return nodedb.PoolLimits{}, err
	}
	if configured {
		running = saved
	}
	if overrides.MaxJobsPerClient != nil {
		running.MaxJobsPerClient = *overrides.MaxJobsPerClient
	}
	if overrides.MaxTasksPerClient != nil {
		running.MaxTasksPerClient = *overrides.MaxTasksPerClient
	}
	if overrides.MaxTaskTimeout != nil {
		running.MaxTaskTimeout = *overrides.MaxTaskTimeout
	}
	if overrides.MaxStoreBytesPerClient != nil {
		running.MaxStoreBytesPerClient = *overrides.MaxStoreBytesPerClient
	}
	if overrides.MaxUploadBytesPerAttempt != nil {
		running.MaxUploadBytesPerAttempt = *overrides.MaxUploadBytesPerAttempt
	}
	// The limits the node runs with are reported through the API as it
	// carries them, so one it cannot carry exactly is refused rather than
	// reported as something other than what is enforced.
	switch {
	case running.MaxJobsPerClient > math.MaxInt32:
		return nodedb.PoolLimits{}, fmt.Errorf("--max-jobs-per-client may be at most %d", math.MaxInt32)
	case running.MaxTasksPerClient > math.MaxInt32:
		return nodedb.PoolLimits{}, fmt.Errorf("--max-tasks-per-client may be at most %d", math.MaxInt32)
	case running.MaxTaskTimeout%time.Second != 0:
		return nodedb.PoolLimits{}, fmt.Errorf("--max-task-timeout must be a whole number of seconds, not %s", running.MaxTaskTimeout)
	}
	return running, nil
}

func supportsGatewayDiscovery(goos string) bool {
	switch goos {
	case "linux", "darwin", "windows", "freebsd", "openbsd", "netbsd", "dragonfly":
		return true
	default:
		return false
	}
}

func startSavedWebAccess(local *api.LocalConfig, webAccess *webAccessRuntime) error {
	config, err := webAccess.db.WebAccess()
	if err != nil || !config.Enabled {
		return err
	}
	if local.Token == "" {
		local.Token, err = apiToken(filepath.Join(webAccess.dataDir, "api.token"))
		if err != nil {
			return err
		}
	}
	return webAccess.startSaved()
}

// localAPIGrace is how long a stopping node lets the calls in progress on
// its local API finish, as the LeavePool whose success stops it (#509),
// before it ends those that would not, such as a watch.
const localAPIGrace = time.Second

// stoppable is a server that can stop at once, or once its calls in
// progress are over. A *grpc.Server is one.
type stoppable interface {
	GracefulStop()
	Stop()
}

// stopServing stops srv, letting the calls in progress finish for up to
// grace and then ending the rest.
func stopServing(srv stoppable, grace time.Duration) {
	stopped := make(chan struct{})
	go func() {
		srv.GracefulStop()
		close(stopped)
	}()
	select {
	case <-stopped:
	case <-time.After(grace):
		srv.Stop()
		<-stopped
	}
}

func waitForDaemonStop(ctx context.Context, local *api.LocalConfig, webAccess *webAccessRuntime, stopped <-chan error, log *slog.Logger) error {
	if err := startSavedWebAccess(local, webAccess); err != nil {
		return err
	}
	defer webAccess.Close()

	select {
	case err := <-stopped:
		return err
	case <-ctx.Done():
		log.Info("shutting down")
		return nil
	}
}

// workerHeartbeat is how often a node's worker reports in to its coordinator
// while idle, saying among other things what models it now serves. Zero
// leaves it to the worker. Tests put a shorter time in its place.
var workerHeartbeat time.Duration

// maxWorkedFor is how many trusted nodes this worker can work for at once.
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

// checkContainers is how a node finds out whether it can run containers.
var checkContainers = runtime.CheckContainers

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
	local := interfacePrefixes()
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
		// Where it is connected from says most about where it is.
		entry.CountryCode = placedOn(append(slices.Clone(f.Conns), f.Addrs...), local)
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

// openBucket opens the bucket a node keeps its stored data in, with the
// credentials in a file: the access key on the first line and the secret
// key on the second.
func openBucket(ctx context.Context, endpoint, bucket, prefix, region, credentialsFile string) (*s3.Client, error) {
	cfg := s3.Config{Endpoint: endpoint, Bucket: bucket, Prefix: prefix, Region: region}
	if credentialsFile != "" {
		keys, err := os.ReadFile(credentialsFile)
		if err != nil {
			return nil, fmt.Errorf("read the bucket's credentials: %w", err)
		}
		lines := strings.Fields(string(keys))
		if len(lines) != 2 {
			return nil, fmt.Errorf("%s should hold the access key on its first line and the secret key on its second", credentialsFile)
		}
		cfg.AccessKey, cfg.SecretKey = lines[0], lines[1]
	}
	return s3.Open(ctx, cfg)
}

// clusterHolders is how many of a cluster's peers hold each pin, given how
// many copies besides the coordinator's were asked for: those, and the
// coordinator's own, whose Kubo holds everything the node stores in any
// case. A cluster asked for none besides is given one, since a cluster of
// one holder is no cluster.
func clusterHolders(copies int) int {
	return max(copies, 1) + 1
}

// countryOf says which country an address is registered in. It is the
// table the daemon carries; tests put another here.
var countryOf = geo.Country

// interfacePrefixes are the networks attached to this machine. Some home
// routers use globally allocated IPv4 space on their LAN, so geo.Global alone
// cannot tell that those addresses are local and must not place a peer on the
// map. Tests replace this lookup with a known interface network.
var (
	interfacePrefixes = localInterfacePrefixes
	listInterfaces    = net.Interfaces
	interfaceAddrs    = func(network net.Interface) ([]net.Addr, error) { return network.Addrs() }
)

func localInterfacePrefixes() []netip.Prefix {
	interfaces, err := listInterfaces()
	if err != nil {
		return nil
	}
	var prefixes []netip.Prefix
	for _, network := range interfaces {
		addresses, err := interfaceAddrs(network)
		if err != nil {
			continue
		}
		for _, address := range addresses {
			prefix, err := netip.ParsePrefix(address.String())
			if err == nil {
				prefixes = append(prefixes, prefix.Masked())
			}
		}
	}
	return prefixes
}

func countryAt(addr netip.Addr, local []netip.Prefix) string {
	if !addr.IsValid() {
		return ""
	}
	addr = addr.Unmap()
	for _, prefix := range local {
		if prefix.Contains(addr) {
			return ""
		}
	}
	return countryOf(addr)
}

// placed returns the country of the first of some multiaddresses that has
// a place in the world, or nothing if none has.
func placed(multiaddrs []string) string {
	return placedOn(multiaddrs, interfacePrefixes())
}

func placedOn(multiaddrs []string, local []netip.Prefix) string {
	for _, multiaddr := range multiaddrs {
		parts := strings.Split(multiaddr, "/")
		if len(parts) < 3 || (parts[1] != "ip4" && parts[1] != "ip6") {
			continue
		}
		addr, err := netip.ParseAddr(parts[2])
		if err != nil {
			continue
		}
		if code := countryAt(addr, local); code != "" {
			return code
		}
	}
	return ""
}

// background runs a node's searches, and waits for them when it stops. A
// search started once it is stopping is not run, since nothing would wait
// for it.
type background struct {
	mu      sync.Mutex
	over    bool
	running sync.WaitGroup
}

func (b *background) start(f func()) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.over {
		return
	}
	b.running.Add(1)
	go func() {
		defer b.running.Done()
		f()
	}()
}

func (b *background) stop() {
	b.mu.Lock()
	b.over = true
	b.mu.Unlock()
	b.running.Wait()
}

// whereabouts is how a node works out where it is when it is asked.
type whereabouts struct {
	// own gives the node's own addresses, and seen its address as its
	// pool sees it.
	own  func() []string
	seen func() netip.Addr
	// stunServer is the STUN server to ask, if any, and lookUp whether
	// the lookup service may be asked.
	stunServer string
	lookUp     bool
	// ctx bounds what is asked, start runs a search beside everything
	// else, and notify tells those watching the node that it may have
	// been placed.
	ctx    context.Context
	start  func(func())
	notify func()
	log    *slog.Logger

	// gateway and stunned hold the addresses the gateway and the STUN
	// server gave, once they have answered.
	gateway, stunned              atomic.Value
	gatewayAnswered, stunAnswered atomic.Bool
	askingSTUN                    sync.Once
	lookups                       countryLookups
}

// country says where the node is and how that was found. A STUN server,
// and the lookup service after it, learn the node's address, so each is
// asked only once the node is asked where it is and nothing nearer has
// placed it: not its own addresses, nor its pool, nor its gateway once
// that has answered or given up, nor, for the lookup, the STUN server once
// that has. The STUN server is asked once; the lookup service again, after
// a while, only if it could not answer (see countryLookups). Neither is
// waited for: what they say is used the next time the node is asked.
func (w *whereabouts) country() (string, string) {
	external, _ := w.gateway.Load().(netip.Addr)
	reflexive, _ := w.stunned.Load().(netip.Addr)
	country, source := countryFrom(w.own(), w.seen(), external, reflexive, w.lookups.found())
	if source != "" || !w.gatewayAnswered.Load() {
		return country, source
	}
	if w.stunServer != "" {
		w.askingSTUN.Do(func() {
			w.start(func() {
				w.stunned.Store(stunnedAddress(w.ctx, w.stunServer, w.log))
				w.stunAnswered.Store(true)
				w.notify()
			})
		})
	}
	if w.lookUp && w.stunAnswered.Load() && w.lookups.begin(time.Now()) {
		w.start(func() {
			answer := lookUpCountry(w.ctx)
			w.lookups.end(time.Now(), answer)
			w.notify()
		})
	}
	return country, source
}

// countryRetryFirst is how long the lookup service is left after it first
// fails to answer, and countryRetryMost the longest it is ever left; the
// wait doubles from one to the other with each failure in a row. Tests
// shorten them.
var (
	countryRetryFirst = time.Minute
	countryRetryMost  = time.Hour
)

// countryLookups keeps track of the lookup service's answers. A definitive
// one, a country or a refusal, is kept for good and the service never
// asked again. One it could not give, a rate limit, a fault on its side or
// no answer at all, lets it be asked again once a wait has passed: the
// service's Retry-After if it gave one, but never less than the backoff
// nor more than countryRetryMost. Only one lookup is ever in flight.
type countryLookups struct {
	mu      sync.Mutex
	code    string
	settled bool
	busy    bool
	// after is when the service may next be asked, and wait the backoff
	// after the next failure, zero before the first.
	after time.Time
	wait  time.Duration
}

// found gives the country the service said, if it has said one.
func (l *countryLookups) found() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.code
}

// begin says whether the service is to be asked now, and if so counts the
// lookup as in flight until end is called.
func (l *countryLookups) begin(now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.settled || l.busy || now.Before(l.after) {
		return false
	}
	l.busy = true
	return true
}

// end records what the lookup begun last came to.
func (l *countryLookups) end(now time.Time, answer countryAnswer) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.busy = false
	if !answer.transient {
		l.code, l.settled = answer.code, true
		return
	}
	if l.wait == 0 {
		l.wait = countryRetryFirst
	}
	l.wait = min(l.wait, countryRetryMost)
	l.after = now.Add(min(max(answer.retryAfter, l.wait), countryRetryMost))
	l.wait = min(2*l.wait, countryRetryMost)
}

// countryFrom says which country a node is in, and how that was found: by
// the first of its own addresses that has a place in the world; failing
// that, behind a NAT, by its address as the pool sees it (see
// coordinator/observed.go); failing that, by the address its gateway says
// it shows the internet, if it was asked; failing that, by the address a
// STUN server the user named saw it come from; and failing that, by what
// an outside service said, if it was asked.
func countryFrom(own []string, seen, gateway, stunned netip.Addr, lookedUp string) (code, source string) {
	local := interfacePrefixes()
	if code := placedOwnOn(own, local, seen, gateway, stunned); code != "" {
		return code, "interface"
	}
	if code := countryAt(seen, local); code != "" {
		return code, "pool"
	}
	if code := countryAt(gateway, local); code != "" {
		return code, "gateway"
	}
	if code := countryAt(stunned, local); code != "" {
		return code, "stun"
	}
	if lookedUp != "" {
		return lookedUp, "lookup"
	}
	return "", ""
}

// placedOwnOn ignores an interface prefix as evidence of a LAN only when an
// independent observer saw this node at a different address. Without that
// NAT evidence, a public address on a directly connected interface is the
// best location evidence available (for example, on a VPS).
func placedOwnOn(multiaddrs []string, local []netip.Prefix, observed ...netip.Addr) string {
	for _, multiaddr := range multiaddrs {
		parts := strings.Split(multiaddr, "/")
		if len(parts) < 3 || (parts[1] != "ip4" && parts[1] != "ip6") {
			continue
		}
		addr, err := netip.ParseAddr(parts[2])
		if err != nil {
			continue
		}
		addr = addr.Unmap()
		if inLocalPrefix(addr, local) {
			seenElsewhere := false
			for _, external := range observed {
				external = external.Unmap()
				if external.IsValid() && external.Is4() == addr.Is4() && external != addr {
					seenElsewhere = true
					break
				}
			}
			if seenElsewhere {
				continue
			}
			if code := countryOf(addr); code != "" {
				return code
			}
			continue
		}
		if code := countryAt(addr, local); code != "" {
			return code
		}
	}
	return ""
}

func inLocalPrefix(addr netip.Addr, local []netip.Prefix) bool {
	for _, prefix := range local {
		if prefix.Contains(addr) {
			return true
		}
	}
	return false
}

// findGateway and askGateway find the default gateway and ask it for its
// external address; tests put others here.
var (
	findGateway = natpmp.Gateway
	askGateway  = natpmp.ExternalAddress
)

// gatewaysAddress asks the machine's default gateway, by NAT-PMP, which
// address it shows the internet, and returns it, or the zero address if
// there is no gateway, it does not answer within a few seconds, or what it
// says is no global address. The country is found from it as from any
// address, and it is never taken as one the node can be reached at.
func gatewaysAddress(ctx context.Context, log *slog.Logger) netip.Addr {
	gw, err := findGateway()
	if err != nil {
		log.Info("could not ask the gateway for the node's address", "error", err)
		return netip.Addr{}
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	external, err := askGateway(ctx, gw)
	if err != nil {
		log.Info("the gateway did not say the node's address", "gateway", gw.String(), "error", err)
		return netip.Addr{}
	}
	return external
}

// askSTUN asks a STUN server where the node's traffic comes from; tests
// put another here.
var askSTUN = stun.ReflexiveAddress

// stunnedAddress asks the STUN server the user named which address the
// node's traffic reaches it from, and returns it, or the zero address if
// the server does not answer within a few seconds. Like the gateway's
// answer, it is used only to place the node.
func stunnedAddress(ctx context.Context, server string, log *slog.Logger) netip.Addr {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	reflexive, err := askSTUN(ctx, server)
	if err != nil {
		log.Info("the STUN server did not say the node's address", "server", server, "error", err)
		return netip.Addr{}
	}
	return reflexive
}

// countryURL is the service asked which country this node's address is in.
var countryURL = "https://ipapi.co/json/"

var countryCode = regexp.MustCompile(`^[A-Za-z]{2}$`)

// countryAnswer is what came of asking the lookup service: the country it
// said, if any, or, if transient, that it could not answer now, and when
// it said to ask again, if it did.
type countryAnswer struct {
	code       string
	transient  bool
	retryAfter time.Duration
}

// lookUpCountry asks an outside service which country the address this node
// reaches it from is in. A 200 OK gives its two-letter code, or nothing if
// the service does not give one, and any other refusal nothing; both are
// definitive. A rate limit, a fault on the service's side or no answer in
// time is transient. The service learns the address by being asked.
func lookUpCountry(ctx context.Context) countryAnswer {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, countryURL, nil) // the address is ours and well formed
	req.Header.Set("User-Agent", "Sisyphus/"+version)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return countryAnswer{transient: true}
	}
	defer res.Body.Close()
	if res.StatusCode == http.StatusRequestTimeout || res.StatusCode == http.StatusTooManyRequests || res.StatusCode >= 500 {
		return countryAnswer{transient: true, retryAfter: retryAfter(res.Header.Get("Retry-After"), time.Now())}
	}
	if res.StatusCode != http.StatusOK {
		return countryAnswer{}
	}
	var answer struct {
		CountryCode string `json:"country_code"`
	}
	json.NewDecoder(io.LimitReader(res.Body, 64<<10)).Decode(&answer)
	if !countryCode.MatchString(answer.CountryCode) {
		return countryAnswer{}
	}
	return countryAnswer{code: strings.ToUpper(answer.CountryCode)}
}

// retryAfter reads a Retry-After header, in seconds or as an HTTP date, as
// how long from now to wait, or zero if it says nothing usable.
func retryAfter(header string, now time.Time) time.Duration {
	if seconds, err := strconv.Atoi(strings.TrimSpace(header)); err == nil {
		return time.Duration(min(max(seconds, 0), 1<<30)) * time.Second
	}
	if when, err := http.ParseTime(header); err == nil {
		return max(when.Sub(now), 0)
	}
	return 0
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
			PeerId: member.ID, ConnectionState: state, GivesWork: member.Has(access.Worker),
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
	return secretIn(file, "API token")
}

// secretIn returns the secret kept in file, making one the first time.
// what says what it is, for when it cannot be read or saved.
func secretIn(file, what string) (string, error) {
	data, err := os.ReadFile(file)
	if err == nil {
		return strings.TrimSpace(string(data)), nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("read %s: %w", what, err)
	}
	var secret [32]byte
	rand.Read(secret[:]) // never fails; see crypto/rand
	token := hex.EncodeToString(secret[:])
	if err := os.WriteFile(file, []byte(token+"\n"), 0o600); err != nil {
		return "", fmt.Errorf("save %s: %w", what, err)
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
	// route is how a worker's Kubo reaches its coordinator's, and forward
	// carries what connects to a port on this machine to a service on the
	// coordinator, through the coordinator's own port.
	route   *route
	forward func(lis *net.TCPListener, target pb.TunnelTarget)

	// cluster is this node's IPFS Cluster peer, if it runs one, which is
	// restarted whenever Kubo is: with the secret that goes with the key,
	// and connecting to clusterPeers. clustered is whether the pool's
	// coordinator runs a peer, as a worker was last told.
	cluster      *ipfscluster.Daemon
	clusterPeers []string
	clustered    bool

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
	key, addresses, clustered, err := fetchSwarm(ctx, coordinator, creds)
	if err != nil {
		return "", nil, err
	}
	s.clustered = clustered
	return key, s.route.choose(addresses), nil
}

// recluster restarts this node's cluster peer, if it runs one, after its
// Kubo has been restarted on key: the cluster's secret goes with the key,
// and Kubo is now at another address.
func (s *poolSwarm) recluster(ctx context.Context, key string) error {
	if s.cluster == nil {
		return nil
	}
	return s.cluster.Restart(ctx, clusterSecret(key), s.daemon.Address(), s.clusterPeers)
}

// listenLoopback opens a port on this machine only, for the system to pick.
var listenLoopback = func() (*net.TCPListener, error) {
	return net.ListenTCP("tcp", &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)})
}

// Rekey gives the pool's network a new key, on a coordinator: it saves the
// key and restarts this node's Kubo on it, and its cluster peer if it runs
// one. Every other holder of the old key is then outside the network until
// it is given the new one.
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
	return s.recluster(ctx, key)
}

// joinSwarm starts a Kubo daemon, on the repository in repo, as a member of
// the private IPFS network of the pool coordinated at addr by the node with
// the given ID. The daemon runs until leave is called.
//
// A worker's Kubo makes its connections outwards. Where it cannot reach the
// coordinator's directly, it connects to a port on this machine that leads
// there through the coordinator's own.
func joinSwarm(ctx context.Context, ident *identity.Identity, repo, secret, addr, coordinatorID string, creds credentials.TransportCredentials, log *slog.Logger) (swarm *poolSwarm, leave func(), err error) {
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
	swarm = &poolSwarm{route: &route{coordinator: coordinatorID, coordinatorAddr: addr, tunnel: lis.Addr().String(), log: log}}
	swarm.forward = func(lis *net.TCPListener, target pb.TunnelTarget) {
		go tunnel.Forward(forwarding, lis, pb.NewTunnelServiceClient(conn), target, log)
	}
	swarm.forward(lis, pb.TunnelTarget_TUNNEL_TARGET_SWARM)
	key, peers, err := swarm.fetch(ctx, addr, creds)
	if err == nil {
		swarm.daemon, err = kubo.Start(ctx, kubo.Config{Repo: repo, Identity: ident, Swarm: &kubo.Swarm{Key: key, Peers: peers}, APISecret: secret})
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
			err = s.recluster(ctx, key)
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

// kuboHandover is how long a node gives its Kubo to take a name's record
// and pass it to the peers that should hold it.
var kuboHandover = 30 * time.Second

// carryName gives a name's record to the node's Kubo, which keeps it and
// passes it to the other members' daemons, so that the pool's private
// network can resolve the name as well as the node can. The node holds the
// record whether or not this works.
func carryName(ctx context.Context, daemon *kubo.Daemon, record *names.Record, log *slog.Logger) {
	ctx, cancel := context.WithTimeout(ctx, kuboHandover)
	defer cancel()
	if err := daemon.PutName(ctx, record.Name, record.Bytes()); err != nil {
		log.Warn("a name's record is held but could not be given to Kubo, so the pool's private IPFS network does not carry it", "name", record.Name, "error", err)
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

// storeName returns the name a coordinator's store is known by to its
// storage followers, kept in a file beside the store's pins and made the
// first time.
func storeName(pinsDir string) (string, error) {
	file := filepath.Join(pinsDir, "store.id")
	data, err := os.ReadFile(file)
	if err == nil {
		return strings.TrimSpace(string(data)), nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("read the store's name: %w", err)
	}
	var random [8]byte
	rand.Read(random[:]) // never fails; see crypto/rand
	name := hex.EncodeToString(random[:])
	if err := os.WriteFile(file, []byte(name+"\n"), 0o600); err != nil {
		return "", fmt.Errorf("save the store's name: %w", err)
	}
	return name, nil
}

// fetchSwarm asks the coordinator at addr how to join its pool's private
// network: the key, where its own Kubo daemon is, and whether it runs a
// cluster peer beside it.
func fetchSwarm(ctx context.Context, addr string, creds credentials.TransportCredentials) (key string, peers []string, clustered bool, err error) {
	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(creds))
	if err != nil {
		return "", nil, false, err
	}
	defer conn.Close()
	swarm, err := pb.NewPoolServiceClient(conn).Swarm(ctx, &pb.SwarmRequest{})
	if err != nil {
		return "", nil, false, fmt.Errorf("ask the coordinator about its private IPFS network: %w", err)
	}
	return swarm.GetSwarmKey(), swarm.GetAddresses(), swarm.GetCluster(), nil
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

// warnOfKeyInTheClear says, of a service that listens over plain HTTP at an
// address other machines can reach, that the key it is used with crosses
// the network unencrypted. A service kept to this machine is said nothing
// of.
func warnOfKeyInTheClear(log *slog.Logger, addr net.Addr, service, key string) {
	if tcp, ok := addr.(*net.TCPAddr); ok && tcp.IP.IsLoopback() {
		return
	}
	log.Warn(service+" can be reached from other machines, over plain HTTP: "+key+" crosses the network unencrypted with every request. Put a proxy that speaks TLS in front of it, or keep it to a loopback address",
		"addr", addr.String())
}

// filePinRetryInterval is the first retry delay after a failed attachment
// pin operation. Successful/idle replay waits for a queue-change event.
const (
	filePinRetryInterval = 2 * time.Second
	filePinMaxRetry      = time.Minute
	filePinTimeout       = 10 * time.Second
)

// replayFilePinsOnChanges replays persisted work at startup and thereafter
// only when a queue mutation occurs. Failures use bounded exponential retry,
// since the external pinning service has no change notification of its own.
func replayFilePinsOnChanges(ctx context.Context, planning *assistant, retryInterval time.Duration, log *slog.Logger) {
	failing := false
	backoff := retryInterval
	for {
		if ctx.Err() != nil {
			return
		}
		attempt, cancel := context.WithTimeout(ctx, filePinTimeout)
		err := planning.replayFilePins(attempt)
		cancel()
		switch {
		case ctx.Err() != nil:
			return
		case err != nil && !failing:
			log.Warn("attachment pins could not be applied and will be retried", "error", err)
		case err == nil && failing:
			log.Info("attachment pins applied")
		}
		failing = err != nil
		// Since Go 1.23 a timer nothing refers to any longer is collected
		// whether or not it fired, so one left behind by a change or by ctx
		// ending costs nothing.
		var retry <-chan time.Time
		if err != nil {
			retry = time.After(backoff)
			backoff = min(backoff*2, filePinMaxRetry)
		} else {
			backoff = retryInterval
		}
		select {
		case <-planning.filePinChanges:
			if err != nil {
				backoff = retryInterval
			}
		case <-retry:
		case <-ctx.Done():
			return
		}
	}
}

type garbageCollectionOptions struct {
	enabled  bool
	interval time.Duration
}

func garbageCollectionOptionsFrom(fs *flag.FlagSet, interval time.Duration) garbageCollectionOptions {
	options := garbageCollectionOptions{enabled: true, interval: interval}
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "gc-interval" && interval == 0 {
			options.enabled = false
		}
	})
	return options
}

type gcChangeSignals struct {
	changed    chan struct{}
	mayCollect atomic.Bool
}

func newGCChangeSignals() *gcChangeSignals {
	return &gcChangeSignals{changed: make(chan struct{}, 1)}
}

func (s *gcChangeSignals) notify(mayCollect bool) {
	if mayCollect {
		s.mayCollect.Store(true)
	}
	select {
	case s.changed <- struct{}{}:
	default:
	}
}

var (
	gcChangeDebounce = 100 * time.Millisecond
	gcRetryInitial   = time.Second
	gcRetryMaximum   = time.Minute
)

// collectOnChanges sweeps once at startup, after changes that can release
// blocks, and when the next pin expires. A positive interval is an optional
// anti-entropy sweep; the default is event/deadline driven.
func collectOnChanges(ctx context.Context, store *storage.Store, interval time.Duration, changes *gcChangeSignals, log *slog.Logger) {
	var periodic *time.Ticker
	var periodicC <-chan time.Time
	if interval > 0 {
		periodic = time.NewTicker(interval)
		periodicC = periodic.C
		defer periodic.Stop()
	}
	deadline := time.NewTimer(time.Hour)
	debounce := time.NewTimer(time.Hour)
	deadline.Stop()
	debounce.Stop()
	defer deadline.Stop()
	defer debounce.Stop()

	nextRetry := time.Time{}
	retryDelay := gcRetryInitial
	debounceArmed := false
	collect := func() {
		done, err := store.GC(ctx, time.Now())
		if err != nil {
			log.Warn("garbage collection failed", "error", err, "retry_in", retryDelay.String())
			nextRetry = time.Now().Add(retryDelay)
			retryDelay = min(retryDelay*2, gcRetryMaximum)
			return
		}
		nextRetry = time.Time{}
		retryDelay = gcRetryInitial
		if done != (storage.Collected{}) {
			log.Info("collected garbage", "expired_pins", done.ExpiredPins, "blocks", done.Blocks, "bytes", done.Bytes)
		}
	}
	collect()

	for {
		var expires time.Time
		for _, pin := range store.Pins() {
			if !pin.Expires.IsZero() && (expires.IsZero() || pin.Expires.Before(expires)) {
				expires = pin.Expires
			}
		}
		if !nextRetry.IsZero() {
			// Retry a failed sweep before waiting on any pin expiry. In
			// particular, a new upload's grace pin must not defer retrying
			// cleanup of already released data for up to an hour.
			expires = nextRetry
		}
		deadlineC := resetTimerAt(deadline, expires)
		select {
		case <-changes.changed:
			if changes.mayCollect.Swap(false) && !debounceArmed {
				resetTimerFor(debounce, gcChangeDebounce)
				debounceArmed = true
			}
		case <-debounce.C:
			debounceArmed = false
			collect()
		case <-deadlineC:
			collect()
		case <-periodicC:
			collect()
		case <-ctx.Done():
			return
		}
	}
}

func resetTimerAt(timer *time.Timer, at time.Time) <-chan time.Time {
	if at.IsZero() {
		timer.Stop()
		return nil
	}
	delay := time.Until(at)
	if delay < 0 {
		delay = 0
	}
	timer.Reset(delay)
	return timer.C
}

func resetTimerFor(timer *time.Timer, delay time.Duration) {
	timer.Reset(delay)
}

// runJobPruner waits for a job's retention deadline or a job change that
// might add an earlier one. Database failures use bounded backoff instead of
// returning to the hourly garbage-collection interval.
func runJobPruner(ctx context.Context, nextDeadline func() time.Time, prune func(time.Time) (int, error), changed <-chan struct{}, log *slog.Logger) {
	const maxRetry = time.Minute
	retry := time.Second
	var retryAt time.Time
	for {
		due := nextDeadline()
		if due.IsZero() {
			select {
			case <-changed:
				continue
			case <-ctx.Done():
				return
			}
		}
		if !retryAt.IsZero() && due.Before(retryAt) {
			due = retryAt
		}
		wait := time.Until(due)
		if wait < 0 {
			wait = 0
		}
		timer := time.NewTimer(wait)
		select {
		case <-changed:
			timer.Stop()
			continue
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
			forgotten, err := prune(time.Now())
			if err != nil {
				log.Warn("could not clear out old jobs", "error", err)
				retryAt = time.Now().Add(retry)
				retry = min(retry*2, maxRetry)
			} else {
				if forgotten > 0 {
					log.Info("forgot jobs that finished long ago", "jobs", forgotten)
				}
				retryAt = time.Time{}
				retry = time.Second
			}
		}
	}
}

func notifyPrune(ch chan<- struct{}) {
	select {
	case ch <- struct{}{}:
	default:
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

// runsAs gives what the node with the given name runs as a worker, which is
// what it was started to run. Tests replace it to have a worker that does
// not do the work it is given.
var runsAs = func(_ string, runs *runtime.Registry) *runtime.Registry { return runs }

// hostname is os.Hostname; tests replace it.
var hostname = os.Hostname

func validateWebListen(address string) error {
	if address == "" {
		return nil
	}
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return errors.New("--web-listen must be a loopback address such as 127.0.0.1:50052")
	}
	ip := net.ParseIP(strings.Trim(host, "[]"))
	if ip == nil || !ip.IsLoopback() {
		return errors.New("--web-listen must be a loopback address; configure remote HTTPS access in Settings")
	}
	return nil
}

// defaultName is the machine's host name.
func defaultName() string {
	name, err := hostname()
	if err != nil {
		return "node"
	}
	return name
}

// explainStoreError says what to do when a node cannot open its store. The
// usual reason a data directory is in use is the desktop app: it starts a
// node of its own on the same directory when it opens, and keeps it for a
// few seconds after its window closes, so a node started by hand alongside
// it fails (#530).
func explainStoreError(err error) error {
	if errors.Is(err, storage.ErrInUse) {
		return fmt.Errorf("%w: another node on this machine has this data directory open. If the Sisyphus app is open, it is most likely the app's own node: quit the app, or wait a few seconds after closing its window, and start this one again. Otherwise, nodes sharing a machine each need their own --data-dir", err)
	}
	return err
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

// listOf reads a list separated by commas, leaving out what is blank.
func listOf(flag string) []string {
	var items []string
	for _, item := range strings.Split(flag, ",") {
		if item = strings.TrimSpace(item); item != "" {
			items = append(items, item)
		}
	}
	return items
}

// rolesInPool says what a member holds in this node's pool, as a coordinator
// tells its workers.
func rolesInPool(admitted *access.List, id string) []pb.Role {
	var roles []pb.Role
	for _, role := range []access.Role{access.Worker, access.Client} {
		if admitted.Has(id, role) {
			roles = append(roles, map[access.Role]pb.Role{access.Worker: pb.Role_ROLE_WORKER, access.Client: pb.Role_ROLE_CLIENT}[role])
		}
	}
	return roles
}

// offeredBy says whether any of a pool's workers now runs a workload. The
// planner is told only of those, so that it asks for nothing the pool would
// leave waiting.
func offeredBy(nodes func() []*pb.NodeInfo) func(workload string) bool {
	return func(workload string) bool {
		return slices.ContainsFunc(nodes(), func(node *pb.NodeInfo) bool {
			return slices.Contains(node.GetCapabilities().GetWorkloads(), workload)
		})
	}
}

// joinedPool says, for the local API, what a worker knows of the pool it
// works for, reached at address.
func joinedPool(pool worker.JoinedPool, address string) *nodepb.JoinedPool {
	joined := &nodepb.JoinedPool{CoordinatorId: pool.ID, CoordinatorName: pool.Name, Address: address, State: nodepb.JoinedPoolState_JOINED_POOL_STATE_CONNECTING}
	switch {
	case pool.Connected:
		joined.State = nodepb.JoinedPoolState_JOINED_POOL_STATE_CONNECTED
	case pool.Welcomed:
		joined.State = nodepb.JoinedPoolState_JOINED_POOL_STATE_DISCONNECTED
	}
	for _, role := range pool.Roles {
		joined.Roles = append(joined.Roles, map[pb.Role]nodepb.PoolRole{pb.Role_ROLE_WORKER: nodepb.PoolRole_POOL_ROLE_WORKER, pb.Role_ROLE_CLIENT: nodepb.PoolRole_POOL_ROLE_CLIENT}[role])
	}
	if pool.Counted {
		joined.WorkerCount = proto.Uint32(uint32(pool.Workers))
	}
	return joined
}

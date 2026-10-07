package main

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	nodepb "github.com/sisyphus-network/Sisyphus/packages/protocol/sisyphus/node/v1"
)

// desktop connects to a node's local API the way the desktop client does.
func desktop(t *testing.T, addr string) nodepb.NodeServiceClient {
	t.Helper()
	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	return nodepb.NewNodeServiceClient(conn)
}

// poolAsSeenBy returns a node's peers as its local API lists them, once the API
// is up.
func poolAsSeenBy(t *testing.T, client nodepb.NodeServiceClient) []*nodepb.Peer {
	t.Helper()
	var peers []*nodepb.Peer
	waitFor(t, func() bool {
		listed, err := client.ListPeers(context.Background(), &nodepb.ListPeersRequest{})
		peers = listed.GetPeers()
		return err == nil
	})
	return peers
}

const (
	connected    = nodepb.PeerConnectionState_PEER_CONNECTION_STATE_CONNECTED
	disconnected = nodepb.PeerConnectionState_PEER_CONNECTION_STATE_DISCONNECTED
)

func TestACoordinatorsLocalAPIShowsItsPoolAndChangesIt(t *testing.T) {
	coordinatorDir, workerDir := t.TempDir(), t.TempDir()
	addr, apiAddr := freeAddr(t), freeAddr(t)
	startDaemon(t, "--data-dir", coordinatorDir, "--listen", addr, "--slots", "1", "--api-listen", apiAddr)
	client := desktop(t, apiAddr)
	ctx := context.Background()

	// On its own, the node has no peers.
	if peers := poolAsSeenBy(t, client); len(peers) != 0 {
		t.Fatalf("a lone coordinator's peers: %v", peers)
	}
	self := nodeID(t, coordinatorDir)
	info, err := client.GetNodeInfo(ctx, &nodepb.GetNodeInfoRequest{})
	host, port, _ := net.SplitHostPort(addr)
	if err != nil || info.GetPeerId() != self || !slices.Equal(info.GetListenAddresses(), []string{"/ip4/" + host + "/tcp/" + port + "/p2p/" + self}) {
		t.Errorf("node info %v, %v", info, err)
	}

	// A worker that joins appears, connected and trusted.
	stopWorker := startDaemon(t, "--data-dir", workerDir, "--role", "worker", "--coordinator", addr, "--slots", "1")
	worker := nodeID(t, workerDir)
	state := func(id string) (found *nodepb.Peer) {
		for _, peer := range poolAsSeenBy(t, client) {
			if peer.GetPeerId() == id {
				found = peer
			}
		}
		return found
	}
	waitFor(t, func() bool { return state(worker).GetConnectionState() == connected })
	if !state(worker).GetTrustedForCompute() {
		t.Error("a worker is not shown as trusted for compute")
	}
	// When it stops it is still a member, only not connected.
	stopWorker()
	waitFor(t, func() bool { return state(worker).GetConnectionState() == disconnected })

	// The token the node keeps beside its key lets the desktop change the
	// pool, and nothing else does.
	tokenFile := filepath.Join(coordinatorDir, "api.token")
	token, err := os.ReadFile(tokenFile)
	if err != nil {
		t.Fatal(err)
	}
	if stat, _ := os.Stat(tokenFile); stat.Mode().Perm() != 0o600 {
		t.Errorf("the token file has mode %v, want it readable by its owner only", stat.Mode().Perm())
	}
	untrust := &nodepb.SetPeerComputeTrustRequest{PeerId: worker, Trusted: false}
	if _, err := client.SetPeerComputeTrust(ctx, untrust); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("a change without the token: %v, want PermissionDenied", err)
	}
	authorized := metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+strings.TrimSpace(string(token)))
	if _, err := client.SetPeerComputeTrust(authorized, untrust); err != nil {
		t.Fatal(err)
	}
	// The node is still known, having been met, but is trusted no longer.
	if left := state(worker); left.GetTrustedForCompute() {
		t.Errorf("a node no longer trusted is listed as %v", left)
	}
	if members := mustCLI(t, "pool", "members", "--addr", addr); strings.Contains(members, worker) {
		t.Errorf("the pool still has the worker as a member:\n%s", members)
	}
}

func TestTheAPITokenOutlivesTheNode(t *testing.T) {
	dataDir := t.TempDir()
	tokens := make([]string, 2)
	for i := range tokens {
		addr, apiAddr := freeAddr(t), freeAddr(t)
		stop := startDaemon(t, "--data-dir", dataDir, "--listen", addr, "--slots", "1", "--api-listen", apiAddr)
		poolAsSeenBy(t, desktop(t, apiAddr))
		token, err := os.ReadFile(filepath.Join(dataDir, "api.token"))
		if err != nil {
			t.Fatal(err)
		}
		tokens[i] = string(token)
		stop()
	}
	if tokens[0] != tokens[1] || len(strings.TrimSpace(tokens[0])) != 64 {
		t.Errorf("tokens across a restart: %q then %q, want one 64-character token", tokens[0], tokens[1])
	}
}

func TestAWorkersLocalAPIShowsItsCoordinator(t *testing.T) {
	coordinatorDir, workerDir := t.TempDir(), t.TempDir()
	addr, apiAddr := freeAddr(t), freeAddr(t)
	stopCoordinator := startDaemon(t, "--data-dir", coordinatorDir, "--listen", addr, "--slots", "1")
	startDaemon(t, "--data-dir", workerDir, "--role", "worker", "--coordinator", addr, "--slots", "1", "--api-listen", apiAddr)
	client := desktop(t, apiAddr)

	coordinator := func() (found *nodepb.Peer) {
		for _, peer := range poolAsSeenBy(t, client) {
			if peer.GetPeerId() == nodeID(t, coordinatorDir) {
				found = peer
			}
		}
		return found
	}
	waitFor(t, func() bool { return coordinator().GetConnectionState() == connected })
	host, port, _ := net.SplitHostPort(addr)
	if got := coordinator(); got.GetPeerId() != nodeID(t, coordinatorDir) || !slices.Contains(got.GetKnownAddresses(), "/ip4/"+host+"/tcp/"+port) {
		t.Errorf("the coordinator as its worker sees it: %v", got)
	}
	// A worker listens on a port of the system's choosing, which nobody had
	// to open, and says where.
	info, err := client.GetNodeInfo(context.Background(), &nodepb.GetNodeInfoRequest{})
	if err != nil || len(info.GetListenAddresses()) == 0 {
		t.Fatalf("node info %v, %v", info, err)
	}
	for _, address := range info.GetListenAddresses() {
		// Of either kind, on a machine that has both.
		if !strings.HasPrefix(address, "/ip4/") && !strings.HasPrefix(address, "/ip6/") || !strings.HasSuffix(address, "/p2p/"+nodeID(t, workerDir)) {
			t.Errorf("the worker gives %q as an address of its own", address)
		}
	}
	// A worker with no pool of its own has no work to give anyone.
	token, err := os.ReadFile(filepath.Join(workerDir, "api.token"))
	if err != nil {
		t.Fatal(err)
	}
	ctx := metadata.AppendToOutgoingContext(context.Background(), "authorization", "Bearer "+strings.TrimSpace(string(token)))
	if _, err := client.SetPeerComputePermissions(ctx, &nodepb.SetPeerComputePermissionsRequest{PeerId: nodeID(t, coordinatorDir), GivesWork: true}); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("a worker with no pool giving work: %v, want FailedPrecondition", err)
	}

	// The desktop's "connect to a peer" works between members: here, the
	// worker to its coordinator, at the address the coordinator gives.
	connected, err := client.ConnectPeer(ctx, &nodepb.ConnectPeerRequest{Address: "/ip4/" + host + "/tcp/" + port + "/p2p/" + nodeID(t, coordinatorDir)})
	if err != nil || connected.GetPeerId() != nodeID(t, coordinatorDir) {
		t.Errorf("ConnectPeer = %v, %v", connected, err)
	}
	// It admits nobody. A node that is no member is not even dialled.
	outsider := nodeID(t, t.TempDir())
	if _, err := client.ConnectPeer(ctx, &nodepb.ConnectPeerRequest{Address: "/ip4/" + host + "/tcp/" + port + "/p2p/" + outsider}); status.Code(err) != codes.Unavailable {
		t.Errorf("connecting to a node that is no member: %v, want Unavailable", err)
	}
	// A connection that was made goes in the address book, and one that
	// was not does not.
	book, err := client.GetBootstrapPeers(ctx, &nodepb.GetBootstrapPeersRequest{})
	if err != nil || len(book.GetPeers()) != 1 || book.GetPeers()[0].GetPeerId() != nodeID(t, coordinatorDir) {
		t.Errorf("the address book after connecting: %v, %v", book, err)
	}

	stopCoordinator()
	waitFor(t, func() bool { return coordinator().GetConnectionState() == disconnected })
}

func TestLocalAPIAddressMustBeOnThisMachine(t *testing.T) {
	taken, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer taken.Close()
	// A data directory whose token cannot be read or made.
	spoiled := t.TempDir()
	if err := os.Mkdir(filepath.Join(spoiled, "api.token"), 0o700); err != nil {
		t.Fatal(err)
	}

	for _, tt := range []struct {
		name, dataDir, apiListen, want string
	}{
		{"every interface", t.TempDir(), "0.0.0.0:50051", "must be a loopback address"},
		{"another machine's address", t.TempDir(), "192.0.2.7:50051", "must be a loopback address"},
		{"a name", t.TempDir(), "localhost:50051", "must be a loopback address"},
		{"not an address", t.TempDir(), "nonsense", "must be a loopback address"},
		{"an address in use", t.TempDir(), taken.Addr().String(), "address already in use"},
		{"a token that cannot be read", spoiled, freeAddr(t), "read API token"},
	} {
		_, err := cli(t, "run", "--data-dir", tt.dataDir, "--listen", freeAddr(t), "--api-listen", tt.apiListen)
		if err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("--api-listen %s: error %v, want one containing %q", tt.name, err, tt.want)
		}
	}
}

func TestAPITokenThatCannotBeSaved(t *testing.T) {
	if _, err := apiToken(filepath.Join(t.TempDir(), "no-such-directory", "api.token")); err == nil || !strings.Contains(err.Error(), "save API token") {
		t.Errorf("error %v, want the token not saved", err)
	}
}

func TestMultiaddrs(t *testing.T) {
	for hostport, want := range map[string][]string{
		"10.0.0.5:7700":          {"/ip4/10.0.0.5/tcp/7700"},
		"[2001:db8::1]:7700":     {"/ip6/2001:db8::1/tcp/7700"},
		"rig.example.net:7700":   {"/dns/rig.example.net/tcp/7700"},
		"":                       nil,
		"no port in this at all": nil,
	} {
		if got := multiaddrs(hostport); !slices.Equal(got, want) {
			t.Errorf("multiaddrs(%q) = %v, want %v", hostport, got, want)
		}
	}
}

func TestTheDesktopSubmitsAJobAndWatchesItRun(t *testing.T) {
	dataDir := t.TempDir()
	addr, apiAddr := freeAddr(t), freeAddr(t)
	startDaemon(t, "--data-dir", dataDir, "--listen", addr, "--name", "rig", "--slots", "2", "--api-listen", apiAddr)
	client := desktop(t, apiAddr)
	poolAsSeenBy(t, client)
	ctx := tokenOf(t, dataDir)

	info, err := client.GetNodeInfo(ctx, &nodepb.GetNodeInfoRequest{})
	if err != nil || !slices.Contains(info.GetWorkloads(), "primes") || !slices.Contains(info.GetWorkloads(), "wordcount") {
		t.Fatalf("node info %v, %v", info, err)
	}
	waitFor(t, func() bool {
		workers, err := client.ListWorkers(ctx, &nodepb.ListWorkersRequest{})
		return err == nil && len(workers.GetWorkers()) == 1 && workers.GetWorkers()[0].GetName() == "rig" && workers.GetWorkers()[0].GetTaskSlots() == 2
	})

	// A job from the command line first, then one from the desktop.
	mustCLI(t, "job", "submit", "--addr", addr, "--params", `{"from":0,"to":100}`)
	watching, cancel := context.WithCancel(ctx)
	defer cancel()
	stream, err := client.WatchJobs(watching, &nodepb.WatchJobsRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if first, err := stream.Recv(); err != nil || len(first.GetJobs()) != 1 || string(first.GetJobs()[0].GetResult()) != `{"count":25}` {
		t.Fatalf("jobs on record before the desktop's: %v, %v", first, err)
	}
	submitted, err := client.SubmitJob(ctx, &nodepb.SubmitJobRequest{Workload: "primes", Params: []byte(`{"from":0,"to":2000000}`), MaxTasks: 4})
	if err != nil {
		t.Fatal(err)
	}
	id := submitted.GetJob().GetJobId()
	// The watcher sees it through to the end, newest job first.
	for {
		update, err := stream.Recv()
		if err != nil {
			t.Fatal(err)
		}
		if newest := update.GetJobs()[0]; newest.GetJobId() == id && newest.GetState() == nodepb.JobState_JOB_STATE_SUCCEEDED {
			if string(newest.GetResult()) != primesBelowTwoMillion || len(newest.GetTasks()) != 4 || newest.GetTasks()[0].GetWorkerName() != "rig" || len(update.GetJobs()) != 2 {
				t.Errorf("the finished job: %v", newest)
			}
			break
		}
	}
	got, err := client.GetJob(ctx, &nodepb.GetJobRequest{JobId: id})
	if err != nil || got.GetJob().GetFinishedAtMs() < got.GetJob().GetCreatedAtMs() {
		t.Errorf("GetJob = %v, %v", got, err)
	}
}

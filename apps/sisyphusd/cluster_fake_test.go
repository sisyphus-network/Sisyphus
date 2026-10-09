package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ipfs/go-cid"
	"github.com/multiformats/go-multihash"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/sisyphus-network/Sisyphus/packages/ipfscluster"
	"github.com/sisyphus-network/Sisyphus/packages/kubo"
	pb "github.com/sisyphus-network/Sisyphus/packages/protocol/sisyphus/v1"
)

// These tests need no cluster program: a stand-in answers as a peer would.

// fakePeer stands in for a cluster peer's REST API. It keeps the pins it is
// given, and says what the test tells it to about everything else.
type fakePeer struct {
	mu sync.Mutex
	// members are the peers it says the cluster has, and status what it
	// says of each pin's copies, one JSON object to a line.
	members []ipfscluster.Peer
	status  string
	pins    map[string]ipfscluster.Pin
	// failing names the calls to refuse, as "GET /peers" or "POST /pins".
	failing map[string]bool
	// changes are the pins and unpins asked of it, and lists how many
	// times it has been asked for all its pins.
	changes []string
	lists   int
}

func (f *fakePeer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	path, id, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/"), "/")
	call := r.Method + " /" + path
	if f.failing[call] {
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprintf(w, `{"code":500,"message":"%s is out of order"}`, call)
		return
	}
	query := r.URL.Query()
	switch call {
	case "GET /peers":
		for _, member := range f.members {
			json.NewEncoder(w).Encode(member)
		}
	case "GET /allocations":
		f.lists++
		for _, pin := range f.pins {
			json.NewEncoder(w).Encode(pin)
		}
	case "GET /pins":
		fmt.Fprint(w, f.status)
	case "POST /pins":
		pin := ipfscluster.Pin{CID: id, Name: query.Get("name"), Allocations: strings.Split(query.Get("user-allocations"), ",")}
		fmt.Sscan(query.Get("replication-min"), &pin.Min)
		fmt.Sscan(query.Get("replication-max"), &pin.Max)
		f.pins[id] = pin
		f.changes = append(f.changes, fmt.Sprintf("pin %s on %d to %d, first %s", id, pin.Min, pin.Max, query.Get("user-allocations")))
		json.NewEncoder(w).Encode(pin)
	case "DELETE /pins":
		delete(f.pins, id)
		f.changes = append(f.changes, "unpin "+id)
		fmt.Fprint(w, "{}")
	}
}

// changed returns what has been asked of the peer since it was last asked,
// in order.
func (f *fakePeer) changed() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	changes := f.changes
	f.changes = nil
	slices.Sort(changes)
	return strings.Join(changes, "\n")
}

func (f *fakePeer) set(change func()) {
	f.mu.Lock()
	defer f.mu.Unlock()
	change()
}

// startFakePeer serves a fake peer with one member, and returns it with a
// client for it.
func startFakePeer(t *testing.T) (*fakePeer, *ipfscluster.Client) {
	t.Helper()
	peer := &fakePeer{members: []ipfscluster.Peer{{ID: "12D3KooWhub", Name: "hub"}}, pins: make(map[string]ipfscluster.Pin), failing: make(map[string]bool)}
	srv := httptest.NewServer(peer)
	t.Cleanup(srv.Close)
	return peer, ipfscluster.NewClient(srv.Listener.Addr().String(), "")
}

// Three CIDs, as a store's pins would name them, in the order they sort in.
var firstCID, secondCID, thirdCID = func() (string, string, string) {
	var ids []string
	for _, content := range []string{"one", "two", "three"} {
		hash, _ := multihash.Sum([]byte(content), multihash.SHA2_256, -1)
		ids = append(ids, cid.NewCidV1(cid.Raw, hash).String())
	}
	slices.Sort(ids)
	return ids[0], ids[1], ids[2]
}()

// keeping returns what a store that keeps the named blobs would list.
func keeping(kept *[]string) func() []cid.Cid {
	return func() []cid.Cid {
		var roots []cid.Cid
		for _, id := range *kept {
			roots = append(roots, cid.MustParse(id))
		}
		return roots
	}
}

func TestTheClustersPinsAreMadeToMatchTheStores(t *testing.T) {
	ctx := context.Background()
	peer, client := startFakePeer(t)
	// The cluster starts with a pin the store no longer has, a pin its owner
	// put there by hand, and one of the store's that is as it should be.
	peer.pins[firstCID] = ipfscluster.Pin{CID: firstCID, Name: clusterPinName, Min: 1, Max: 2}
	peer.pins["QmByHand"] = ipfscluster.Pin{CID: "QmByHand", Name: "mine", Min: 1, Max: 1}
	peer.pins[thirdCID] = ipfscluster.Pin{CID: thirdCID, Name: clusterPinName, Min: 1, Max: 2}
	kept := []string{secondCID, thirdCID}
	logs := new(syncBuffer)
	mirror := newClusterPins(keeping(&kept), client, "12D3KooWhub", 2, slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})))

	// Word of a change waits to be heard, and a second word while the
	// first is waiting adds nothing.
	mirror.nudge()
	mirror.nudge()
	if len(mirror.changed) != 1 {
		t.Errorf("after two words of a change, %d are waiting to be heard", len(mirror.changed))
	}

	// With one member there is one to hold each pin, though two are wanted.
	if err := mirror.sync(ctx); err != nil {
		t.Fatal(err)
	}
	if got, want := peer.changed(), "pin "+secondCID+" on 1 to 2, first 12D3KooWhub\nunpin "+firstCID; got != want {
		t.Errorf("the cluster was asked to:\n%s\nwant:\n%s", got, want)
	}
	if peer.pins["QmByHand"].Name != "mine" {
		t.Error("a pin put in the cluster by hand was touched")
	}
	if !strings.Contains(logs.String(), "pinned on the pool's cluster") || !strings.Contains(logs.String(), "unpinned from the pool's cluster") {
		t.Errorf("logged:\n%s", logs.String())
	}

	// Nothing has changed, so nothing is asked, nor the pins listed again.
	if err := mirror.sync(ctx); err != nil || peer.changed() != "" || peer.lists != 1 {
		t.Errorf("a second look: %v, asked %q, listed %d times", err, peer.changed(), peer.lists)
	}

	// A member that does not answer is no place to keep anything.
	peer.set(func() {
		peer.members = append(peer.members, ipfscluster.Peer{ID: "12D3KooWgone", Error: "dial backoff"})
	})
	if err := mirror.sync(ctx); err != nil || peer.changed() != "" {
		t.Errorf("with a member that does not answer: %v, asked %q", err, peer.changed())
	}
	// Once there are two members, every pin is to be held by both. A third
	// changes nothing: two is all that was asked for.
	peer.set(func() { peer.members[1] = ipfscluster.Peer{ID: "12D3KooWnorth", Name: "north"} })
	if err := mirror.sync(ctx); err != nil {
		t.Fatal(err)
	}
	if got, want := peer.changed(), "pin "+secondCID+" on 2 to 2, first 12D3KooWhub\npin "+thirdCID+" on 2 to 2, first 12D3KooWhub"; got != want {
		t.Errorf("with a second member the cluster was asked to:\n%s\nwant:\n%s", got, want)
	}
	peer.set(func() { peer.members = append(peer.members, ipfscluster.Peer{ID: "12D3KooWsouth", Name: "south"}) })
	if err := mirror.sync(ctx); err != nil || peer.changed() != "" {
		t.Errorf("with a third member: %v, asked %q", err, peer.changed())
	}
	// Nor is a pin loosened when a member goes: the cluster is left to find
	// it another holder.
	peer.set(func() { peer.members = peer.members[:1] })
	if err := mirror.sync(ctx); err != nil || peer.changed() != "" {
		t.Errorf("with one member again: %v, asked %q", err, peer.changed())
	}
	// Even with no member answering, a pin asks for one holder.
	peer.set(func() { peer.members = nil })
	kept = append(kept, firstCID)
	if err := mirror.sync(ctx); err != nil || peer.changed() != "pin "+firstCID+" on 1 to 2, first 12D3KooWhub" {
		t.Errorf("with no member answering: %v, asked %q", err, peer.changed())
	}

	// A node restarted to keep three copies asks again for everything.
	more := newClusterPins(keeping(&kept), client, "12D3KooWhub", 3, slog.New(slog.DiscardHandler))
	if err := more.sync(ctx); err != nil {
		t.Fatal(err)
	}
	if got := peer.changed(); strings.Count(got, " on 1 to 3, ") != 3 {
		t.Errorf("asked to keep three copies, the cluster was asked to:\n%s", got)
	}
}

func TestAClusterThatFailsIsAskedAgainFromTheStart(t *testing.T) {
	ctx := context.Background()
	peer, client := startFakePeer(t)
	peer.pins[firstCID] = ipfscluster.Pin{CID: firstCID, Name: clusterPinName, Min: 1, Max: 2}
	kept := []string{secondCID}
	mirror := newClusterPins(keeping(&kept), client, "12D3KooWhub", 2, slog.New(slog.DiscardHandler))

	// What a failed change left behind is not known, so the pins are listed
	// again before anything more is done: after the list itself failed, and
	// after the pin did. A failure to list the members changed nothing, and
	// what was known still is.
	for _, tt := range []struct {
		call   string
		listed int
	}{{"GET /allocations", 0}, {"GET /peers", 1}, {"POST /pins", 0}, {"DELETE /pins", 1}} {
		peer.set(func() { peer.failing[tt.call] = true })
		before := peer.lists
		if err := mirror.sync(ctx); err == nil || !strings.Contains(err.Error(), tt.call+" is out of order") {
			t.Errorf("with %s failing: %v", tt.call, err)
		}
		peer.set(func() { delete(peer.failing, tt.call) })
		if listed := peer.lists - before; listed != tt.listed {
			t.Errorf("with %s failing the pins were listed %d times, want %d", tt.call, listed, tt.listed)
		}
	}
	if err := mirror.sync(ctx); err != nil {
		t.Fatal(err)
	}
	if _, stale := peer.pins[firstCID]; stale || peer.pins[secondCID].Name != clusterPinName {
		t.Errorf("once the cluster answers again its pins are %v", peer.pins)
	}
}

func TestTheClustersPinsFollowTheStoresForAsLongAsTheNodeRuns(t *testing.T) {
	defer func(every time.Duration) { clusterSync = every }(clusterSync)
	clusterSync = 20 * time.Millisecond
	peer, client := startFakePeer(t)
	peer.failing["GET /peers"] = true
	var guard guarded
	kept := []string{firstCID}
	logs := new(syncBuffer)
	mirror := newClusterPins(func() []cid.Cid { return guard.read(keeping(&kept)) }, client, "12D3KooWhub", 1, slog.New(slog.NewTextHandler(logs, nil)))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		mirror.run(ctx)
	}()

	// While the cluster cannot be asked, the node says so and keeps trying.
	waitFor(t, func() bool {
		return strings.Count(logs.String(), "the pool's cluster does not yet pin what this node's store does") >= 2
	})
	peer.set(func() { delete(peer.failing, "GET /peers") })
	waitFor(t, func() bool { return peer.changed() == "pin "+firstCID+" on 1 to 1, first 12D3KooWhub" })

	// A change to the store's pins is acted on when the store says so, not
	// at the next look.
	changed := time.Now()
	guard.write(func() { kept = nil })
	mirror.nudge()
	mirror.nudge() // a second word while the first is unheard is no more work
	waitFor(t, func() bool { return peer.changed() == "unpin "+firstCID })
	if took := time.Since(changed); took > 5*time.Second {
		t.Errorf("a change to the store's pins took %v to reach the cluster", took)
	}

	// A pin that goes missing from the cluster is found missing at the next
	// look, and put back.
	guard.write(func() { kept = []string{secondCID} })
	mirror.nudge()
	waitFor(t, func() bool { return peer.changed() != "" })
	peer.set(func() { delete(peer.pins, secondCID) })
	waitFor(t, func() bool { return peer.changed() == "pin "+secondCID+" on 1 to 1, first 12D3KooWhub" })

	cancel()
	<-done
}

// guarded lets a test change what a store keeps while a mirror reads it.
type guarded struct{ mu sync.Mutex }

func (g *guarded) read(list func() []cid.Cid) []cid.Cid {
	g.mu.Lock()
	defer g.mu.Unlock()
	return list()
}

func (g *guarded) write(change func()) {
	g.mu.Lock()
	defer g.mu.Unlock()
	change()
}

func TestClusterSecretsGoWithSwarmKeys(t *testing.T) {
	one, another := kubo.NewSwarmKey(), kubo.NewSwarmKey()
	secret := clusterSecret(one)
	if !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(secret) {
		t.Errorf("the secret %q is not 64 hexadecimal digits", secret)
	}
	if secret != clusterSecret(one) || secret == clusterSecret(another) {
		t.Error("a secret should be the same for one key and different for another")
	}
	// The cluster's secret must not be the swarm's: a cluster peer and a
	// Kubo daemon that held the same one could be connected to each other.
	if strings.Contains(one, secret) {
		t.Error("the cluster's secret is the swarm key itself")
	}
}

func TestClusterStatusListsMembersAndTheCopiesOfThePoolsPins(t *testing.T) {
	ctx := context.Background()
	peer, client := startFakePeer(t)
	peer.members = []ipfscluster.Peer{{ID: "12D3KooWnorth", Name: "north"}, {ID: "12D3KooWhub", Name: "hub"}, {ID: "12D3KooWgone", Error: "dial backoff"}}
	peer.status = `{"cid":"` + secondCID + `","name":"sisyphus","peer_map":{"12D3KooWnorth":{"peername":"north","status":"pin_error","error":"context deadline exceeded"},"12D3KooWhub":{"peername":"hub","status":"pinned"},"12D3KooWsouth":{"peername":"south","status":"remote"}}}
{"cid":"QmByHand","name":"mine","peer_map":{"12D3KooWhub":{"peername":"hub","status":"pinned"}}}
{"cid":"` + firstCID + `","name":"sisyphus","peer_map":{"12D3KooWhub":{"peername":"hub","status":"pinning"}}}
`
	cluster := &poolCluster{peer: client, replicas: 2}
	state, err := cluster.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, member := range state.GetMembers() {
		got = append(got, fmt.Sprintf("member %s %q %q", member.GetNodeId(), member.GetName(), member.GetError()))
	}
	for _, pin := range state.GetPins() {
		for _, copied := range pin.GetCopies() {
			got = append(got, fmt.Sprintf("%s on %s %q: %s %q", pin.GetCid(), copied.GetNodeId(), copied.GetName(), copied.GetStatus(), copied.GetError()))
		}
	}
	// In order of ID, without the pin that is not the store's, and without
	// the member that was not chosen to hold anything.
	want := []string{
		`member 12D3KooWgone "" "dial backoff"`,
		`member 12D3KooWhub "hub" ""`,
		`member 12D3KooWnorth "north" ""`,
		firstCID + ` on 12D3KooWhub "hub": pinning ""`,
		secondCID + ` on 12D3KooWhub "hub": pinned ""`,
		secondCID + ` on 12D3KooWnorth "north": pin_error "context deadline exceeded"`,
	}
	if state.GetReplicas() != 2 || strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("%d replicas and:\n%s\nwant 2 and:\n%s", state.GetReplicas(), strings.Join(got, "\n"), strings.Join(want, "\n"))
	}

	for _, call := range []string{"GET /peers", "GET /pins"} {
		peer.set(func() { peer.failing = map[string]bool{call: true} })
		if _, err := cluster.Status(ctx); err == nil || !strings.Contains(err.Error(), call+" is out of order") {
			t.Errorf("with %s failing: %v", call, err)
		}
	}
	// Where the peer listens is asked of the peer too.
	if _, err := cluster.Local(ctx); err == nil {
		t.Error("a peer that says nothing of itself was found all the same")
	}
}

// clusterOf is a node that reports a fixed state for its pool's cluster.
type clusterOf struct {
	pb.UnimplementedPoolServiceServer
	state *pb.ClusterStatusResponse
}

func (c clusterOf) ClusterStatus(context.Context, *pb.ClusterStatusRequest) (*pb.ClusterStatusResponse, error) {
	if c.state == nil {
		return nil, status.Error(codes.Unavailable, "ask the cluster peer: it went away")
	}
	return c.state, nil
}

func TestPoolClusterCommand(t *testing.T) {
	state := &pb.ClusterStatusResponse{
		Replicas: 2,
		Members: []*pb.ClusterMember{
			{NodeId: "12D3KooWgone", Error: "dial backoff"},
			{NodeId: "12D3KooWhub", Name: "hub"},
			{NodeId: "12D3KooWnorth", Name: "north"},
		},
		Pins: []*pb.ClusterPin{
			{Cid: firstCID, Copies: []*pb.ClusterCopy{{NodeId: "12D3KooWhub", Name: "hub", Status: "pinned"}, {NodeId: "12D3KooWnorth", Name: "north", Status: "pinned"}}},
			{Cid: secondCID, Copies: []*pb.ClusterCopy{
				{NodeId: "12D3KooWhub", Name: "hub", Status: "pinned"},
				{NodeId: "12D3KooWnorth", Name: "north", Status: "pin_error", Error: "context deadline exceeded"},
				{NodeId: "12D3KooWnameless", Status: "pinning"},
			}},
			{Cid: thirdCID},
		},
	}
	addr := serveFake(t, func(srv *grpc.Server) { pb.RegisterPoolServiceServer(srv, clusterOf{state: state}) })
	want := `MEMBER         NAME   STATE
12D3KooWgone          dial backoff
12D3KooWhub    hub    answering
12D3KooWnorth  north  answering

each pin is to be held by 2 of them

` + fmt.Sprintf("%-*s  COPIES  HELD BY     WAITING FOR\n", len(firstCID), "CID") +
		firstCID + `  2/2     hub, north  -
` + secondCID + `  1/2     hub         north (pin_error: context deadline exceeded), 12D3KooWnameless (pinning)
` + thirdCID + `  0/2     -           -
`
	if out := mustCLI(t, "pool", "cluster", "--addr", addr); out != want {
		t.Errorf("printed:\n%s\nwant:\n%s", out, want)
	}

	empty := serveFake(t, func(srv *grpc.Server) {
		pb.RegisterPoolServiceServer(srv, clusterOf{state: &pb.ClusterStatusResponse{Replicas: 3}})
	})
	if out := mustCLI(t, "pool", "cluster", "--addr", empty); out != "MEMBER  NAME  STATE\n\neach pin is to be held by 3 of them\nnothing is pinned\n" {
		t.Errorf("for a cluster with nothing in it, printed:\n%s", out)
	}
}

func TestPoolClusterCommandFailures(t *testing.T) {
	broken := serveFake(t, func(srv *grpc.Server) { pb.RegisterPoolServiceServer(srv, clusterOf{}) })
	// A node that runs no cluster peer.
	plain, _ := startNode(t)
	for _, tt := range []struct {
		args []string
		want string
	}{
		{[]string{"pool", "cluster", "extra"}, `unexpected argument "extra"`},
		{[]string{"pool", "cluster", "--no-such-flag"}, "flag provided but not defined"},
		{[]string{"pool", "cluster", "--addr", badAddr}, "invalid control character"},
		{[]string{"pool", "cluster", "--addr", broken}, "ask the cluster peer: it went away"},
		{[]string{"pool", "cluster", "--addr", plain}, "does not run an IPFS Cluster peer; start it with --kubo --cluster"},
		{[]string{"run", "--cluster", "--data-dir", t.TempDir()}, "--cluster needs --kubo"},
		{[]string{"run", "--kubo", "--cluster", "--cluster-replicas", "0", "--data-dir", t.TempDir()}, "--cluster-replicas must be at least 1"},
	} {
		if _, err := cli(t, tt.args...); err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("%v: error %v, want one containing %q", tt.args, err, tt.want)
		}
	}
}

func TestAWorkersClusterPeerThatCannotBeStarted(t *testing.T) {
	ctx := context.Background()
	daemon := &kubo.Daemon{Client: kubo.NewClient("127.0.0.1:5001")}
	absent := ipfscluster.Config{Binary: filepath.Join(t.TempDir(), "absent"), Dir: t.TempDir()}

	// Its coordinator runs no cluster.
	alone := &poolSwarm{daemon: daemon, route: &route{coordinator: "12D3KooWhub"}}
	if _, err := joinCluster(ctx, alone, absent); err == nil || !strings.Contains(err.Error(), "the coordinator does not run an IPFS Cluster") {
		t.Errorf("with a coordinator that runs no cluster: %v", err)
	}

	// The program is not there to run. The port opened for it is closed
	// again.
	var forwarded *net.TCPListener
	swarm := &poolSwarm{daemon: daemon, route: &route{coordinator: "12D3KooWhub"}, clustered: true, key: kubo.NewSwarmKey()}
	swarm.forward = func(lis *net.TCPListener, target pb.TunnelTarget) {
		if target != pb.TunnelTarget_TUNNEL_TARGET_CLUSTER {
			t.Errorf("the port leads to %v, want the coordinator's cluster peer", target)
		}
		forwarded = lis
	}
	if _, err := joinCluster(ctx, swarm, absent); err == nil || !strings.Contains(err.Error(), "ipfs-cluster:") {
		t.Errorf("with no program to run: %v", err)
	}
	if want := "/ip4/" + strings.Replace(forwarded.Addr().String(), ":", "/tcp/", 1) + "/p2p/12D3KooWhub"; len(swarm.clusterPeers) != 1 || swarm.clusterPeers[0] != want {
		t.Errorf("the peer was to start from %v, want %s", swarm.clusterPeers, want)
	}
	if _, err := forwarded.Accept(); err == nil {
		t.Error("the port opened for a peer that never started is still open")
	}

	// No port can be opened for it.
	old := listenLoopback
	listenLoopback = func() (*net.TCPListener, error) { return nil, errors.New("no ports left") }
	defer func() { listenLoopback = old }()
	if _, err := joinCluster(ctx, swarm, absent); err == nil || !strings.Contains(err.Error(), "listen for this node's cluster peer: no ports left") {
		t.Errorf("with no port to listen on: %v", err)
	}
}

// onlyKubo puts on the PATH a directory that holds the ipfs program and
// nothing else, as on a machine where IPFS Cluster was never installed.
func onlyKubo(t *testing.T) {
	t.Helper()
	ipfs, err := exec.LookPath("ipfs")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := os.Symlink(ipfs, filepath.Join(dir, "ipfs")); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
}

func TestANodeToldToRunAClusterPeerItCannotRunDoesNotStart(t *testing.T) {
	requireKubo(t)
	// A worker whose coordinator runs Kubo and no cluster.
	addr := freeAddr(t)
	startDaemon(t, "--role", "coordinator", "--kubo", "--listen", addr)
	invitation := invite(t, addr, "worker")
	_, err := cli(t, "run", "--kubo", "--cluster", "--role", "worker", "--coordinator", addr, "--join", invitation, "--data-dir", t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "the coordinator does not run an IPFS Cluster for its pool") {
		t.Errorf("a worker whose coordinator runs no cluster: %v", err)
	}

	// A coordinator on a machine without the program.
	pgrep, _ := exec.LookPath("pgrep")
	onlyKubo(t)
	dataDir := t.TempDir()
	if _, err := cli(t, "run", "--kubo", "--cluster", "--data-dir", dataDir, "--listen", freeAddr(t)); err == nil || !strings.Contains(err.Error(), "ipfs-cluster:") {
		t.Errorf("a coordinator with no ipfs-cluster-service program: %v", err)
	}
	// Kubo, which had started, was stopped again.
	if out, _ := exec.Command(pgrep, "-f", filepath.Join(dataDir, "ipfs")).Output(); len(out) != 0 {
		t.Errorf("a Kubo daemon is still running for a node that failed to start: %s", out)
	}
}

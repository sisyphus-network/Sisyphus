package main

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/excho0/Sisyphus/packages/kubo"
)

// swarmPortOf returns the port a node's Kubo accepts members on, and the
// addresses it says it listens at.
func swarmPortOf(t *testing.T, dataDir string) (port string, addresses []string) {
	t.Helper()
	addresses = strings.Fields(ipfsIn(t, dataDir, "id", "-f", "<addrs>"))
	for _, address := range addresses {
		if hostport, ok := tcpAddress(address); ok {
			_, port, _ = net.SplitHostPort(hostport)
		}
	}
	return port, addresses
}

// connectionTo returns the address a node's Kubo is connected to a peer at.
func connectionTo(t *testing.T, dataDir, peer string) string {
	t.Helper()
	for _, line := range strings.Fields(peersOf(t, dataDir)) {
		if strings.HasSuffix(line, "/p2p/"+peer) {
			return line
		}
	}
	return ""
}

// captureLogs collects what the nodes started after it log, in place of
// printing it.
func captureLogs(t *testing.T) *syncBuffer {
	t.Helper()
	logs := new(syncBuffer)
	stderr = logs
	t.Cleanup(func() { stderr = os.Stderr })
	return logs
}

const boulderText = "the boulder rolls down, and Sisyphus walks after it.\n"

// runOverSwarm runs a job whose input the worker can only get from the
// pool's private network, and checks that is where it got it.
func runOverSwarm(t *testing.T, addr, workerDir string) {
	t.Helper()
	text := strings.Repeat(boulderText, 20_000)
	input := strings.TrimSpace(mustCLI(t, "blob", "put", "--addr", addr, writeFile(t, text)))
	out := mustCLI(t, "job", "submit", "--addr", addr, "--workload", "wordcount", "--tasks", "4", "--params", `{"input":"`+input+`"}`)
	if !strings.Contains(out, `"words":180000`) {
		t.Fatalf("job output:\n%s", out)
	}
	if got := ipfsIn(t, workerDir, "cat", "--offline", input); got != text {
		t.Errorf("the worker's Kubo holds %d bytes of the input, want all %d", len(got), len(text))
	}
}

func TestAPoolsPrivateNetworkNeedsNoPortOfItsOwn(t *testing.T) {
	requireKubo(t)
	addr := freeAddr(t)
	coordinatorDir, workerDir := t.TempDir(), t.TempDir()
	logs := captureLogs(t)
	startDaemon(t, "--role", "coordinator", "--kubo", "--listen", addr, "--data-dir", coordinatorDir)
	startDaemon(t, "--role", "worker", "--kubo", "--coordinator", addr, "--data-dir", workerDir, "--name", "hand", "--slots", "2")
	waitForOutput(t, "hand", "nodes", "--addr", addr)
	coordinator := nodeID(t, coordinatorDir)

	// The coordinator's Kubo listens to its own machine and nothing else.
	port, addresses := swarmPortOf(t, coordinatorDir)
	for _, address := range addresses {
		if !strings.HasPrefix(address, "/ip4/127.0.0.1/") {
			t.Errorf("the coordinator's Kubo listens at %s, want this machine only", address)
		}
	}
	// The worker's Kubo is connected to it all the same, at a port on its
	// own machine that is not the coordinator's Kubo's.
	waitFor(t, func() bool { return connectionTo(t, workerDir, coordinator) != "" })
	via := connectionTo(t, workerDir, coordinator)
	if !strings.HasPrefix(via, "/ip4/127.0.0.1/tcp/") || strings.Contains(via, "/tcp/"+port+"/") {
		t.Errorf("the worker's Kubo reaches the coordinator's at %s, want a tunnel on this machine and not port %s", via, port)
	}
	if !strings.Contains(logs.String(), "reaches the coordinator's through the coordinator's own port") {
		t.Error("the worker did not say that it uses the tunnel")
	}

	runOverSwarm(t, addr, workerDir)
	if strings.Contains(logs.String(), "did not supply a blob") {
		t.Error("the private network did not supply the input; the worker fell back to the coordinator")
	}

	// A change of key restarts both daemons, the coordinator's on another
	// port. The tunnel still finds it.
	mustCLI(t, "pool", "rekey", "--addr", addr)
	waitFor(t, func() bool { return strings.Contains(logs.String(), "moved to the new key") })
	waitFor(t, func() bool { return connectionTo(t, workerDir, coordinator) != "" })
}

func TestAnOpenSwarmPortIsUsedDirectly(t *testing.T) {
	requireKubo(t)
	addr := freeAddr(t)
	_, swarmPort, _ := net.SplitHostPort(freeAddr(t))
	coordinatorDir, workerDir := t.TempDir(), t.TempDir()
	logs := captureLogs(t)
	startDaemon(t, "--role", "coordinator", "--kubo", "--swarm-port", swarmPort, "--listen", addr, "--data-dir", coordinatorDir)
	startDaemon(t, "--role", "worker", "--kubo", "--coordinator", addr, "--data-dir", workerDir, "--name", "hand", "--slots", "2")
	waitForOutput(t, "hand", "nodes", "--addr", addr)
	coordinator := nodeID(t, coordinatorDir)

	waitFor(t, func() bool { return connectionTo(t, workerDir, coordinator) != "" })
	if via := connectionTo(t, workerDir, coordinator); !strings.Contains(via, "/tcp/"+swarmPort+"/") {
		t.Errorf("the worker's Kubo reaches the coordinator's at %s, want its open port %s", via, swarmPort)
	}
	if !strings.Contains(logs.String(), "connects to the coordinator's directly") {
		t.Error("the worker did not say that it connects directly")
	}
	runOverSwarm(t, addr, workerDir)
}

// reachable is a port that accepts connections, and unreachable one that
// refuses them.
func reachable(t *testing.T) (host, port string) {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { lis.Close() })
	host, port, _ = net.SplitHostPort(lis.Addr().String())
	return host, port
}

func unreachable(t *testing.T) (host, port string) {
	t.Helper()
	host, port, _ = net.SplitHostPort(freeAddr(t))
	return host, port
}

func TestAWorkerChoosesBetweenADirectConnectionAndTheTunnel(t *testing.T) {
	const coordinator, other = "12D3KooWcoordinator", "12D3KooWother"
	const tunnel = "/ip4/127.0.0.1/tcp/5555/p2p/" + coordinator
	_, open := reachable(t)
	_, closed := unreachable(t)
	fellow := "/ip4/10.1.2.3/tcp/4001/p2p/" + other
	onLoopback := func(port string) string { return "/ip4/127.0.0.1/tcp/" + port + "/p2p/" + coordinator }

	for _, tt := range []struct {
		name            string
		coordinatorAddr string
		given, want     []string
	}{
		{"the coordinator gives no address of its own", "127.0.0.1:7700",
			[]string{fellow}, []string{tunnel, fellow}},
		{"its address accepts a connection", "127.0.0.1:7700",
			[]string{onLoopback(open), fellow}, []string{onLoopback(open), fellow}},
		{"its address refuses", "127.0.0.1:7700",
			[]string{onLoopback(closed), fellow}, []string{tunnel, fellow}},
		{"some of its addresses accept and some refuse", "localhost:7700",
			[]string{onLoopback(closed), onLoopback(open)}, []string{onLoopback(open)}},
		// What listens on this machine's loopback is not the coordinator
		// when the coordinator is another machine.
		{"its loopback address, from another machine", "203.0.113.9:7700",
			[]string{onLoopback(open)}, []string{tunnel}},
		{"an address that is not a TCP port", "127.0.0.1:7700",
			[]string{"/ip4/127.0.0.1/udp/" + open + "/quic-v1/p2p/" + coordinator, "/dns/example.net/p2p/" + coordinator}, []string{tunnel}},
	} {
		r := &route{coordinator: coordinator, coordinatorAddr: tt.coordinatorAddr, tunnel: "127.0.0.1:5555", log: quiet}
		if got := r.choose(tt.given); !slices.Equal(got, tt.want) {
			t.Errorf("%s: chose %v, want %v", tt.name, got, tt.want)
		}
	}
}

func TestAnAddressThatNeverAnswersIsGivenUpOn(t *testing.T) {
	old := probeTimeout
	probeTimeout = 50 * time.Millisecond
	defer func() { probeTimeout = old }()
	const coordinator = "12D3KooWcoordinator"
	// An address reserved for documentation, which nothing answers at.
	r := &route{coordinator: coordinator, coordinatorAddr: "203.0.113.9:7700", tunnel: "127.0.0.1:5555", log: slog.New(slog.DiscardHandler)}
	started := time.Now()
	got := r.choose([]string{"/ip4/203.0.113.9/tcp/4101/p2p/" + coordinator})
	if !slices.Equal(got, []string{"/ip4/127.0.0.1/tcp/5555/p2p/" + coordinator}) {
		t.Errorf("chose %v, want the tunnel", got)
	}
	if took := time.Since(started); took > 10*time.Second {
		t.Errorf("gave up after %v", took)
	}
}

func TestFindingKubosOwnAddress(t *testing.T) {
	got, err := loopbackOf([]string{"/ip4/10.0.0.5/tcp/4101/p2p/x", "/ip6/::1/tcp/4101/p2p/x", "/ip4/127.0.0.1/udp/4101/quic-v1/p2p/x", "/ip4/127.0.0.1/tcp/4101/p2p/x"})
	if err != nil || got != "127.0.0.1:4101" {
		t.Errorf("loopbackOf = %q, %v", got, err)
	}
	if _, err := loopbackOf([]string{"/ip4/10.0.0.5/tcp/4101/p2p/x"}); err == nil || !strings.Contains(err.Error(), "not listening on this machine's loopback") {
		t.Errorf("with no loopback address: %v", err)
	}
	for multiaddr, want := range map[string]string{
		"/ip4/10.0.0.5/tcp/4101":          "10.0.0.5:4101",
		"/ip6/2001:db8::1/tcp/4101/p2p/x": "[2001:db8::1]:4101",
		"/dns/example.net/tcp/4101":       "",
		"/ip4/10.0.0.5/udp/4101/quic-v1":  "",
		"/ip4/10.0.0.5":                   "",
		"":                                "",
	} {
		if got, ok := tcpAddress(multiaddr); got != want || ok != (want != "") {
			t.Errorf("tcpAddress(%q) = %q, %v; want %q", multiaddr, got, ok, want)
		}
	}
}

func TestAWorkerThatCannotListenForItsKuboSaysSo(t *testing.T) {
	requireKubo(t)
	addr := freeAddr(t)
	startDaemon(t, "--role", "coordinator", "--kubo", "--listen", addr)
	invitation := invite(t, addr, "worker")

	old := listenLoopback
	listenLoopback = func() (*net.TCPListener, error) { return nil, errors.New("no ports left") }
	defer func() { listenLoopback = old }()
	_, err := cli(t, "run", "--kubo", "--role", "worker", "--coordinator", addr, "--join", invitation, "--data-dir", t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "listen for this node's Kubo: no ports left") {
		t.Errorf("error %v, want the failure to listen", err)
	}
}

func TestANodeWhoseKuboIsDownCannotSayWhereItIs(t *testing.T) {
	requireKubo(t)
	ident := newIdentity(t)
	daemon, err := kubo.Start(context.Background(), kubo.Config{Repo: filepath.Join(t.TempDir(), "ipfs"), Identity: ident, Swarm: &kubo.Swarm{Key: kubo.NewSwarmKey(), Loopback: true}})
	if err != nil {
		t.Fatal(err)
	}
	swarm := &poolSwarm{daemon: daemon}
	local, err := swarm.Local(context.Background())
	host, port, _ := net.SplitHostPort(local)
	if n, _ := strconv.Atoi(port); err != nil || host != "127.0.0.1" || n == 0 {
		t.Errorf("Local = %q, %v; want a port on this machine", local, err)
	}
	// With no port opened, its own addresses are not for others.
	if addresses, err := swarm.Addresses(context.Background()); err != nil || len(addresses) != 0 {
		t.Errorf("Addresses = %v, %v; want none", addresses, err)
	}
	daemon.Stop()
	if _, err := swarm.Local(context.Background()); err == nil {
		t.Error("a stopped Kubo was found all the same")
	}
}

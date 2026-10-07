package main

import (
	"context"
	"database/sql"
	"net"
	"net/http"
	"net/netip"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc/metadata"

	"github.com/sisyphus-network/Sisyphus/apps/sisyphusd/p2p"
	"github.com/sisyphus-network/Sisyphus/packages/nodedb"
	nodepb "github.com/sisyphus-network/Sisyphus/packages/protocol/sisyphus/node/v1"
)

// peerSeenBy returns a node as another's local API lists it, or nil.
func peerSeenBy(t *testing.T, client nodepb.NodeServiceClient, id string) (found *nodepb.Peer) {
	t.Helper()
	for _, peer := range poolAsSeenBy(t, client) {
		if peer.GetPeerId() == id {
			found = peer
		}
	}
	return found
}

// withToken is a context carrying the API token of the node in dataDir.
func tokenOf(t *testing.T, dataDir string) context.Context {
	t.Helper()
	token, err := os.ReadFile(filepath.Join(dataDir, "api.token"))
	if err != nil {
		t.Fatal(err)
	}
	return metadata.AppendToOutgoingContext(context.Background(), "authorization", "Bearer "+strings.TrimSpace(string(token)))
}

// onEveryInterface is a free address that other nodes on this machine's
// networks could reach, which is what a node announces itself at.
func onEveryInterface(t *testing.T) (listen, loopback string) {
	t.Helper()
	_, port, _ := net.SplitHostPort(freeAddr(t))
	return "0.0.0.0:" + port, "127.0.0.1:" + port
}

func TestNodesFindEachOtherAndTrustMakesOneAWorker(t *testing.T) {
	rigDir, laptopDir := t.TempDir(), t.TempDir()
	rigListen, rig := onEveryInterface(t)
	laptopListen, _ := onEveryInterface(t)
	apiAddr := freeAddr(t)
	startDaemon(t, "--data-dir", rigDir, "--role", "coordinator", "--listen", rigListen, "--discovery", "on", "--api-listen", apiAddr)
	dataDirs.Store(rig, rigDir)
	// A second node, minding its own business on the same network. Nobody
	// has told either of the other.
	stopLaptop := startDaemon(t, "--data-dir", laptopDir, "--listen", laptopListen, "--discovery", "on", "--slots", "1")
	client := desktop(t, apiAddr)
	laptop := nodeID(t, laptopDir)

	waitFor(t, func() bool { return peerSeenBy(t, client, laptop).GetConnectionState() == connected })
	seen := peerSeenBy(t, client, laptop)
	if seen.GetTrustedForCompute() || len(seen.GetKnownAddresses()) == 0 {
		t.Errorf("a node just found is listed as %v, want its addresses and no trust", seen)
	}
	// Found is not admitted: it can do nothing with the rig.
	if _, err := as(t, laptopDir, "pool", "join", "--addr", rig, nodeID(t, rigDir)); err == nil || !strings.Contains(err.Error(), "has not been trusted") {
		t.Errorf("a node that is not trusted joining without an invitation: %v", err)
	}
	// Nor does a node trust itself into its own pool.
	if _, err := cli(t, "pool", "join", "--addr", rig, nodeID(t, rigDir)); err == nil || !strings.Contains(err.Error(), "has not been trusted") {
		t.Errorf("a node joining itself: %v", err)
	}

	// The rig's owner trusts it for compute, from the desktop. No
	// invitation changes hands.
	if _, err := client.SetPeerComputeTrust(tokenOf(t, rigDir), &nodepb.SetPeerComputeTrustRequest{PeerId: laptop, Trusted: true}); err != nil {
		t.Fatal(err)
	}
	if seen := peerSeenBy(t, client, laptop); !seen.GetTrustedForCompute() {
		t.Errorf("after being trusted the node is listed as %v", seen)
	}
	// The laptop's owner puts it to work for the rig, naming the rig.
	stopLaptop()
	startDaemon(t, "--data-dir", laptopDir, "--role", "worker", "--coordinator", rig, "--join", nodeID(t, rigDir), "--name", "laptop", "--slots", "1", "--discovery", "on")
	waitForOutput(t, "laptop", "nodes", "--addr", rig)
	out := mustCLI(t, "job", "submit", "--addr", rig, "--params", `{"from":0,"to":100}`)
	if !strings.Contains(out, "on laptop") || !strings.Contains(out, `{"count":25}`) {
		t.Errorf("a job on the pool of one trusted node:\n%s", out)
	}
}

// waitForLogged waits until the nodes have logged want the given number of
// times, and shows what they did log if they never do.
func waitForLogged(t *testing.T, logs *syncBuffer, want string, times int) {
	t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for strings.Count(logs.String(), want) != times {
		if time.Now().After(deadline) {
			t.Fatalf("%q was logged %d times, want %d; the log:\n%s", want, strings.Count(logs.String(), want), times, logs)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestANodeStartsFromItsAddressBook(t *testing.T) {
	logs := captureLogs(t)
	seedDir, nodeDir := t.TempDir(), t.TempDir()
	seedAddr, apiAddr := freeAddr(t), freeAddr(t)
	startDaemon(t, "--data-dir", seedDir, "--role", "coordinator", "--listen", seedAddr, "--discovery", "on")
	host, port, _ := net.SplitHostPort(seedAddr)
	seed := "/ip4/" + host + "/tcp/" + port + "/p2p/" + nodeID(t, seedDir)
	waitForOutput(t, "no workers", "nodes", "--addr", seedAddr)

	// Told of the seed on the command line, once.
	stop := startDaemon(t, "--data-dir", nodeDir, "--listen", freeAddr(t), "--discovery", "on", "--api-listen", apiAddr,
		"--bootstrap", " "+seed+" , ")
	client := desktop(t, apiAddr)
	waitForLogged(t, logs, `msg="connected to the nodes this one starts from" answered=1 of=1`, 1)
	if seen := peerSeenBy(t, client, nodeID(t, seedDir)); seen.GetConnectionState() != connected {
		t.Errorf("the seed as the node sees it: %v", seen)
	}
	// And then given it to keep, from the desktop.
	entry := &nodepb.BootstrapPeer{PeerId: nodeID(t, seedDir), Address: seed}
	if _, err := client.SetBootstrapPeers(tokenOf(t, nodeDir), &nodepb.SetBootstrapPeersRequest{Peers: []*nodepb.BootstrapPeer{entry}}); err != nil {
		t.Fatal(err)
	}
	waitForLogged(t, logs, "answered=1 of=1", 2)
	stop()

	// Started again and told nothing, it goes to the seed by itself.
	apiAddr = freeAddr(t)
	startDaemon(t, "--data-dir", nodeDir, "--listen", freeAddr(t), "--discovery", "on", "--api-listen", apiAddr)
	client = desktop(t, apiAddr)
	waitForLogged(t, logs, "answered=1 of=1", 3)
	book, err := client.GetBootstrapPeers(context.Background(), &nodepb.GetBootstrapPeersRequest{})
	if err != nil || len(book.GetPeers()) != 1 || book.GetPeers()[0].GetAddress() != seed {
		t.Errorf("the address book after a restart: %v, %v", book, err)
	}
}

func TestDiscoveryFlagsThatAreRefused(t *testing.T) {
	if _, err := cli(t, "run", "--data-dir", t.TempDir(), "--listen", freeAddr(t), "--discovery", "sometimes"); err == nil || !strings.Contains(err.Error(), "--discovery is on or off") {
		t.Errorf("--discovery sometimes: %v", err)
	}
	// An address book that cannot be read.
	damaged := t.TempDir()
	file := filepath.Join(damaged, "node.db")
	journalIn(t, file).Close()
	raw, err := sql.Open("sqlite", file)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`DROP TABLE bootstrap_peers`); err != nil {
		t.Fatal(err)
	}
	raw.Close()
	if _, err := cli(t, "run", "--data-dir", damaged, "--listen", freeAddr(t)); err == nil || !strings.Contains(err.Error(), "load bootstrap peers") {
		t.Errorf("error %v, want the address book not loaded", err)
	}
	// Or a list of nodes worked for that cannot be.
	damaged = t.TempDir()
	file = filepath.Join(damaged, "node.db")
	journalIn(t, file).Close()
	if raw, err = sql.Open("sqlite", file); err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`DROP TABLE works_for`); err != nil {
		t.Fatal(err)
	}
	raw.Close()
	if _, err := cli(t, "run", "--data-dir", damaged, "--listen", freeAddr(t)); err == nil || !strings.Contains(err.Error(), "load the nodes worked for") {
		t.Errorf("error %v, want the list not loaded", err)
	}
}

// answering stands in for the service that says which country an address
// is in, giving the reply body it is told to.
func answering(t *testing.T, body string) {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte(body)) }))
	old := countryURL
	countryURL = server.URL
	t.Cleanup(func() {
		countryURL = old
		server.Close()
	})
}

func TestTheCountryIsLookedUpOnlyWhenAsked(t *testing.T) {
	asked := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		asked = true
		w.Write([]byte(`{"ip":"203.0.113.9","country_code":"nl"}`))
	}))
	defer server.Close()
	old := countryURL
	countryURL = server.URL
	defer func() { countryURL = old }()

	// Not asked for: nobody is told this node's address.
	apiAddr := freeAddr(t)
	stop := startDaemon(t, "--listen", freeAddr(t), "--api-listen", apiAddr)
	client := desktop(t, apiAddr)
	poolAsSeenBy(t, client)
	if info, err := client.GetNodeInfo(context.Background(), &nodepb.GetNodeInfoRequest{}); err != nil || info.GetCountryCode() != "" {
		t.Errorf("node info %v, %v", info, err)
	}
	stop()
	if asked {
		t.Fatal("the country was looked up without being asked for")
	}

	apiAddr = freeAddr(t)
	startDaemon(t, "--listen", freeAddr(t), "--api-listen", apiAddr, "--locate-country")
	client = desktop(t, apiAddr)
	waitFor(t, func() bool {
		info, err := client.GetNodeInfo(context.Background(), &nodepb.GetNodeInfoRequest{})
		return err == nil && info.GetCountryCode() == "NL"
	})
}

func TestCountryLookupsThatGiveNothing(t *testing.T) {
	ctx := context.Background()
	for name, body := range map[string]string{
		"a reply that is no JSON":      "<html>rate limited</html>",
		"a reply with no country":      `{"ip":"203.0.113.9"}`,
		"something that is not a code": `{"country_code":"Netherlands"}`,
		"a code with a digit in it":    `{"country_code":"N1"}`,
	} {
		answering(t, body)
		if got := lookUpCountry(ctx); got != "" {
			t.Errorf("%s: %q", name, got)
		}
	}
	// Nobody there to ask.
	old := countryURL
	countryURL = "http://" + freeAddr(t)
	defer func() { countryURL = old }()
	if got := lookUpCountry(ctx); got != "" {
		t.Errorf("with the service unreachable: %q", got)
	}
}

func TestWhereANodeStartsFrom(t *testing.T) {
	book := []nodedb.BootstrapPeer{{PeerID: "a", Address: "/ip4/10.0.0.1/tcp/7700/p2p/a"}}
	got := startingPoints(book, " /ip4/10.0.0.2/tcp/7700/p2p/b ,, /dns/rig.example.net/tcp/7700/p2p/c")
	want := []string{"/ip4/10.0.0.1/tcp/7700/p2p/a", "/ip4/10.0.0.2/tcp/7700/p2p/b", "/dns/rig.example.net/tcp/7700/p2p/c"}
	if !slices.Equal(got, want) {
		t.Errorf("starting points %v, want %v", got, want)
	}
	if got := startingPoints(nil, ""); len(got) != 0 {
		t.Errorf("with nothing given: %v", got)
	}
}

func TestThePoolAndTheNodesFoundAreListedTogether(t *testing.T) {
	pool := []*nodepb.Peer{
		{PeerId: "m-worker", ConnectionState: connected, TrustedForCompute: true},
		{PeerId: "c-away", ConnectionState: disconnected, TrustedForCompute: true, KnownAddresses: []string{"/ip4/10.0.0.3/tcp/7700"}},
	}
	found := []p2p.Peer{
		// A member the host is connected to though its worker is not.
		{ID: "c-away", Addrs: []string{"/ip4/10.0.0.3/tcp/7700", "/ip4/192.168.1.3/tcp/7700"}, Conns: []string{"/ip4/192.168.1.3/tcp/7700"}},
		{ID: "z-stranger", Addrs: []string{"/ip4/192.168.1.9/tcp/40000"}, Conns: []string{"/ip4/192.168.1.9/tcp/40000"}},
		{ID: "a-heard-of", Addrs: []string{"/ip4/192.168.1.7/tcp/40000"}},
	}
	got := withFound(pool, found)
	type row struct {
		id        string
		state     nodepb.PeerConnectionState
		trusted   bool
		addresses int
	}
	var rows []row
	for _, p := range got {
		rows = append(rows, row{p.GetPeerId(), p.GetConnectionState(), p.GetTrustedForCompute(), len(p.GetKnownAddresses())})
	}
	want := []row{
		{"a-heard-of", disconnected, false, 1},
		{"c-away", connected, true, 2},
		{"m-worker", connected, true, 0},
		{"z-stranger", connected, false, 1},
	}
	if !slices.Equal(rows, want) {
		t.Errorf("listed %v, want %v", rows, want)
	}
}

func TestHowANodeIsConnectedToAnother(t *testing.T) {
	relayed := "/ip4/203.0.113.9/tcp/7700/p2p/12D3KooWrelay/p2p-circuit"
	found := []p2p.Peer{
		{ID: "through-a-relay", Conns: []string{relayed}},
		{ID: "left-the-relay", Conns: []string{relayed, "/ip4/198.51.100.4/tcp/40000"}},
		{ID: "direct-first", Conns: []string{"/ip4/198.51.100.4/tcp/40000", relayed}},
		{ID: "known-only", Addrs: []string{"/ip4/198.51.100.5/tcp/40000"}},
	}
	for id, want := range map[string]string{
		"through-a-relay": "relayed", "left-the-relay": "direct", "direct-first": "direct", "known-only": "none", "never-heard-of": "none",
	} {
		if got := routeTo(found, id); got != want {
			t.Errorf("route to %s: %s, want %s", id, got, want)
		}
	}
}

func TestANodePlacesItselfAndItsPeersWithoutAskingAnyone(t *testing.T) {
	// The table here has this machine's own address in a country of its
	// own, where the real one has it nowhere.
	old := countryOf
	countryOf = func(addr netip.Addr) string {
		if addr.IsLoopback() {
			return "XT"
		}
		return ""
	}
	defer func() { countryOf = old }()

	addr, apiAddr := freeAddr(t), freeAddr(t)
	startDaemon(t, "--data-dir", t.TempDir(), "--listen", addr, "--api-listen", apiAddr)
	client := desktop(t, apiAddr)
	poolAsSeenBy(t, client)
	if info, err := client.GetNodeInfo(context.Background(), &nodepb.GetNodeInfoRequest{}); err != nil || info.GetCountryCode() != "XT" {
		t.Fatalf("node info %v, %v", info, err)
	}
	startDaemon(t, "--role", "worker", "--coordinator", addr, "--join", invite(t, addr, "worker"), "--data-dir", t.TempDir(), "--name", "second")
	waitFor(t, func() bool {
		peers, err := client.ListPeers(context.Background(), &nodepb.ListPeersRequest{})
		return err == nil && len(peers.GetPeers()) == 1 && peers.GetPeers()[0].GetCountryCode() == "XT"
	})

	// Only an address with a place in the world places a node.
	for _, tt := range []struct {
		addrs []string
		want  string
	}{
		{[]string{"/dns4/example.org/tcp/7700", "/ip4/not-an-address/tcp/1", "nonsense", "/ip4/10.0.0.5/tcp/7700", "/ip6/::1/tcp/7700"}, "XT"},
		{[]string{"/ip4/10.0.0.5/tcp/7700", "/ip4/192.168.1.4/tcp/7700/p2p-circuit"}, ""},
		{nil, ""},
	} {
		if got := placed(tt.addrs); got != tt.want {
			t.Errorf("placed(%v) = %q, want %q", tt.addrs, got, tt.want)
		}
	}
	// And with the table the daemon carries, a real address is placed.
	countryOf = old
	if got := placed([]string{"/ip4/192.168.1.4/tcp/7700", "/ip4/8.8.8.8/tcp/7700"}); got != "US" {
		t.Errorf("a node at 8.8.8.8 is placed in %q", got)
	}
}

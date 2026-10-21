package main

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
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
	stop = startDaemon(t, "--listen", freeAddr(t), "--api-listen", apiAddr, "--locate-country")
	client = desktop(t, apiAddr)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	stream, err := client.WatchNodeState(ctx, &nodepb.GetNodeInfoRequest{})
	if err != nil {
		t.Fatal(err)
	}
	for {
		state, err := stream.Recv()
		if err != nil {
			t.Fatalf("waiting for the live location update: %v", err)
		}
		if state.GetInfo().GetCountryCode() == "NL" {
			break
		}
	}
	stop()
}

// A node with an address of its own in the world is placed by it; one
// behind a NAT by its address as the pool sees it; then by what its
// gateway says, if that was asked; then by what a STUN server saw, if one
// was named; and only then by an outside service, if that was asked.
func TestWhereANodeIsComesFirstFromItsOwnAddresses(t *testing.T) {
	oldLocal := interfacePrefixes
	interfacePrefixes = func() []netip.Prefix { return nil }
	defer func() { interfacePrefixes = oldLocal }()
	own, private := []string{"/ip4/8.8.8.8/tcp/7700"}, []string{"/ip4/192.168.1.4/tcp/7700"}
	seen, gateway, stunned := netip.MustParseAddr("193.0.6.139"), netip.MustParseAddr("8.8.4.4"), netip.MustParseAddr("2001:67c:2e8:22::c100:68b")
	none := netip.Addr{}
	for _, tt := range []struct {
		own                    []string
		seen, gateway, stunned netip.Addr
		lookedUp               string
		wantCode, wantFrom     string
		wantBy                 netip.Addr
	}{
		{own, seen, gateway, stunned, "FR", "US", "interface", netip.MustParseAddr("8.8.8.8")},
		{private, seen, gateway, stunned, "FR", "NL", "pool", seen},
		{private, none, gateway, stunned, "FR", "US", "gateway", gateway},
		{private, none, none, stunned, "FR", "NL", "stun", stunned},
		{private, none, none, netip.MustParseAddr("::ffff:193.0.6.139"), "", "NL", "stun", seen},
		{private, none, netip.MustParseAddr("100.64.0.1"), netip.MustParseAddr("192.168.1.4"), "FR", "FR", "lookup", none},
		{private, none, none, none, "FR", "FR", "lookup", none},
		{private, netip.MustParseAddr("100.64.0.9"), none, none, "", "", "", none},
	} {
		if code, from, by := countryFrom(tt.own, tt.seen, tt.gateway, tt.stunned, tt.lookedUp); code != tt.wantCode || from != tt.wantFrom || by != tt.wantBy {
			t.Errorf("countryFrom(%v, %v, %v, %v, %q) = %q, %q, %v; want %q, %q, %v", tt.own, tt.seen, tt.gateway, tt.stunned, tt.lookedUp, code, from, by, tt.wantCode, tt.wantFrom, tt.wantBy)
		}
	}
}

func TestAHomeNetworkUsingPublicAddressSpaceIsNotPlacedByItsLanAddress(t *testing.T) {
	oldCountry, oldLocal := countryOf, interfacePrefixes
	countryOf = func(netip.Addr) string { return "US" }
	interfacePrefixes = func() []netip.Prefix {
		return []netip.Prefix{netip.MustParsePrefix("12.0.0.0/24")}
	}
	defer func() {
		countryOf, interfacePrefixes = oldCountry, oldLocal
	}()

	localPeer := []string{"/ip4/12.0.0.48/tcp/7700"}
	if got := placed(localPeer); got != "" {
		t.Errorf("local-network address placed a peer in %q", got)
	}
	if got := placed(append(localPeer, "/ip4/8.8.8.8/tcp/7700")); got != "US" {
		t.Errorf("a public peer address after the local one was placed in %q", got)
	}

	code, source, _ := countryFrom(localPeer, netip.MustParseAddr("12.0.0.49"), netip.Addr{}, netip.Addr{}, "IL")
	if code != "IL" || source != "lookup" {
		t.Errorf("local-network addresses placed this node as %q from %q; want IL from lookup", code, source)
	}
}

func TestPublicAddressOnOwnInterfaceIsUsedWithoutNATEvidence(t *testing.T) {
	oldCountry, oldLocal := countryOf, interfacePrefixes
	countryOf = func(netip.Addr) string { return "US" }
	interfacePrefixes = func() []netip.Prefix {
		return []netip.Prefix{netip.MustParsePrefix("203.0.113.0/24")}
	}
	defer func() { countryOf, interfacePrefixes = oldCountry, oldLocal }()

	own := []string{
		"/dns4/node.example/tcp/7700",
		"/ip4/not-an-address/tcp/7700",
		"/ip4/203.0.113.7/tcp/7700",
	}
	if code, source, _ := countryFrom(own, netip.Addr{}, netip.Addr{}, netip.Addr{}, ""); code != "US" || source != "interface" {
		t.Errorf("countryFrom without NAT evidence = %q from %q; want US from interface", code, source)
	}
	if code, source, _ := countryFrom(own, netip.MustParseAddr("203.0.113.7"), netip.Addr{}, netip.Addr{}, ""); code != "US" || source != "interface" {
		t.Errorf("countryFrom when the pool sees this same public address = %q from %q; want US from interface", code, source)
	}
	if code, source, _ := countryFrom(own, netip.MustParseAddr("198.51.100.8"), netip.Addr{}, netip.Addr{}, "CA"); code != "US" || source != "pool" {
		t.Errorf("countryFrom with a distinct observed address = %q from %q; want US from pool", code, source)
	}

	interfacePrefixes = func() []netip.Prefix {
		return []netip.Prefix{netip.MustParsePrefix("2001:db8::/64")}
	}
	own = []string{"/ip6/2001:db8::7/tcp/7700"}
	if code, source, _ := countryFrom(own, netip.MustParseAddr("198.51.100.8"), netip.Addr{}, netip.Addr{}, ""); code != "US" || source != "interface" {
		t.Errorf("IPv4 observation suppressed an own IPv6 address: got %q from %q; want US from interface", code, source)
	}
}

func TestLocalInterfacePrefixesHandlesEnumerationFailuresAndMalformedAddresses(t *testing.T) {
	oldList, oldAddrs := listInterfaces, interfaceAddrs
	t.Cleanup(func() { listInterfaces, interfaceAddrs = oldList, oldAddrs })

	listInterfaces = func() ([]net.Interface, error) { return nil, errors.New("interfaces unavailable") }
	if got := localInterfacePrefixes(); got != nil {
		t.Errorf("prefixes when interfaces cannot be listed = %v; want nil", got)
	}

	listInterfaces = func() ([]net.Interface, error) { return []net.Interface{{Index: 1}, {Index: 2}}, nil }
	interfaceAddrs = func(network net.Interface) ([]net.Addr, error) {
		if network.Index == 1 {
			return nil, errors.New("addresses unavailable")
		}
		return []net.Addr{
			fakeNetAddr("not an IP prefix"),
			fakeNetAddr("12.0.0.48/24"),
		}, nil
	}
	got := localInterfacePrefixes()
	want := []netip.Prefix{netip.MustParsePrefix("12.0.0.0/24")}
	if !slices.Equal(got, want) {
		t.Errorf("localInterfacePrefixes() = %v; want %v", got, want)
	}
}

type fakeNetAddr string

func (a fakeNetAddr) Network() string { return "test" }
func (a fakeNetAddr) String() string  { return string(a) }

// The gateway is asked only for its external address, and a gateway that
// is not there, or does not answer, places the node nowhere.
func TestTheGatewayIsAskedWhereTheNodeShowsTheInternet(t *testing.T) {
	oldFind, oldAsk := findGateway, askGateway
	defer func() { findGateway, askGateway = oldFind, oldAsk }()
	router := netip.MustParseAddr("192.168.1.1")
	findGateway = func() (netip.Addr, error) { return router, nil }
	askGateway = func(_ context.Context, gw netip.Addr) (netip.Addr, error) {
		if gw != router {
			t.Errorf("asked %v, not the gateway", gw)
		}
		return netip.MustParseAddr("193.0.6.139"), nil
	}
	if got := gatewaysAddress(context.Background(), quiet); got != netip.MustParseAddr("193.0.6.139") {
		t.Errorf("the gateway's address: %v", got)
	}
	askGateway = func(context.Context, netip.Addr) (netip.Addr, error) { return netip.Addr{}, errors.New("no answer") }
	if got := gatewaysAddress(context.Background(), quiet); got.IsValid() {
		t.Errorf("a gateway that does not answer gave %v", got)
	}
	findGateway = func() (netip.Addr, error) { return netip.Addr{}, errors.New("no default route") }
	if got := gatewaysAddress(context.Background(), quiet); got.IsValid() {
		t.Errorf("no gateway gave %v", got)
	}

	// Told to, a node asks its gateway, and says that is how it knows.
	findGateway = func() (netip.Addr, error) { return router, nil }
	askGateway = func(context.Context, netip.Addr) (netip.Addr, error) { return netip.MustParseAddr("193.0.6.139"), nil }
	apiAddr := freeAddr(t)
	startDaemon(t, "--listen", freeAddr(t), "--api-listen", apiAddr, "--locate-gateway")
	client := desktop(t, apiAddr)
	waitFor(t, func() bool {
		info, err := client.GetNodeInfo(context.Background(), &nodepb.GetNodeInfoRequest{})
		return err == nil && info.GetCountryCode() == "NL" && info.GetCountrySource() == "gateway"
	})
}

// A STUN server is asked only when one is named, and one that does not
// answer places the node nowhere.
func TestANamedSTUNServerIsAskedWhereTheNodeComesFrom(t *testing.T) {
	old := askSTUN
	defer func() { askSTUN = old }()
	askSTUN = func(context.Context, string) (netip.Addr, error) { return netip.Addr{}, errors.New("no answer") }
	if got := stunnedAddress(context.Background(), "stun.example.org", quiet); got.IsValid() {
		t.Errorf("a server that does not answer gave %v", got)
	}

	var asked atomic.Value
	var times atomic.Int32
	askSTUN = func(_ context.Context, server string) (netip.Addr, error) {
		asked.Store(server)
		times.Add(1)
		return netip.MustParseAddr("193.0.6.139"), nil
	}
	apiAddr := freeAddr(t)
	startDaemon(t, "--listen", freeAddr(t), "--api-listen", apiAddr, "--stun-server", "stun.example.org:19302")
	client := desktop(t, apiAddr)
	waitFor(t, func() bool {
		info, err := client.GetNodeInfo(context.Background(), &nodepb.GetNodeInfoRequest{})
		return err == nil && info.GetCountryCode() == "NL" && info.GetCountrySource() == "stun"
	})
	if got, _ := asked.Load().(string); got != "stun.example.org:19302" {
		t.Errorf("asked %q, not the server named", got)
	}
	if n := times.Load(); n != 1 {
		t.Errorf("asked %d times, however often the node was asked where it is", n)
	}
}

// The STUN server learns the node's address, so it is not asked when
// something nearer has placed the node: here, the gateway, which it waits
// for.
func TestNoSTUNServerIsAskedWhenTheNodeIsPlacedWithout(t *testing.T) {
	oldSTUN, oldFind, oldAsk := askSTUN, findGateway, askGateway
	defer func() { askSTUN, findGateway, askGateway = oldSTUN, oldFind, oldAsk }()
	answer := make(chan struct{})
	findGateway = func() (netip.Addr, error) { return netip.MustParseAddr("192.168.1.1"), nil }
	askGateway = func(ctx context.Context, _ netip.Addr) (netip.Addr, error) {
		select {
		case <-answer:
			return netip.MustParseAddr("193.0.6.139"), nil
		case <-ctx.Done():
			return netip.Addr{}, ctx.Err()
		}
	}
	var times atomic.Int32
	askSTUN = func(context.Context, string) (netip.Addr, error) {
		times.Add(1)
		return netip.MustParseAddr("8.8.8.8"), nil
	}
	apiAddr := freeAddr(t)
	startDaemon(t, "--listen", freeAddr(t), "--api-listen", apiAddr, "--locate-gateway", "--stun-server", "stun.example.org")
	client := desktop(t, apiAddr)
	// While the gateway has yet to answer, the node is placed nowhere, and
	// still the STUN server waits.
	if info, err := client.GetNodeInfo(context.Background(), &nodepb.GetNodeInfoRequest{}); err != nil || info.GetCountrySource() != "" {
		t.Fatalf("before the gateway answered: %v, %v", info, err)
	}
	close(answer)
	waitFor(t, func() bool {
		info, err := client.GetNodeInfo(context.Background(), &nodepb.GetNodeInfoRequest{})
		return err == nil && info.GetCountrySource() == "gateway"
	})
	client.GetNodeInfo(context.Background(), &nodepb.GetNodeInfoRequest{})
	if n := times.Load(); n != 0 {
		t.Errorf("the STUN server was asked %d times", n)
	}
}

// A search started once the node is stopping is not run, and stopping
// waits for those that are running.
func TestSearchesAreWaitedForAndNoneStartsAfter(t *testing.T) {
	var b background
	var done atomic.Bool
	release := make(chan struct{})
	b.start(func() { <-release; done.Store(true) })
	go close(release)
	b.stop()
	if !done.Load() {
		t.Error("stopping did not wait for a search")
	}
	b.start(func() { t.Error("a search ran after stopping") })
	b.stop()
}

// The lookup service learns the node's address, so it is not asked when
// something nearer has placed the node, and is asked, once, only after a
// STUN server that was named has answered or given up.
func TestTheCountryIsLookedUpOnlyWhenNothingNearerPlacesTheNode(t *testing.T) {
	var looked atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		looked.Add(1)
		w.Write([]byte(`{"country_code":"fr"}`))
	}))
	defer server.Close()
	oldURL, oldSTUN := countryURL, askSTUN
	defer func() { countryURL, askSTUN = oldURL, oldSTUN }()
	countryURL = server.URL

	// A STUN server that places the node is the last one asked.
	askSTUN = func(context.Context, string) (netip.Addr, error) { return netip.MustParseAddr("193.0.6.139"), nil }
	apiAddr := freeAddr(t)
	stop := startDaemon(t, "--listen", freeAddr(t), "--api-listen", apiAddr, "--stun-server", "stun.example.org", "--locate-country")
	client := desktop(t, apiAddr)
	waitFor(t, func() bool {
		info, err := client.GetNodeInfo(context.Background(), &nodepb.GetNodeInfoRequest{})
		return err == nil && info.GetCountrySource() == "stun"
	})
	client.GetNodeInfo(context.Background(), &nodepb.GetNodeInfoRequest{})
	stop()
	if n := looked.Load(); n != 0 {
		t.Errorf("with the node placed by STUN, the lookup service was asked %d times", n)
	}

	// One that does not answer is waited for, and then the service asked.
	answer := make(chan struct{})
	askSTUN = func(ctx context.Context, _ string) (netip.Addr, error) {
		select {
		case <-answer:
		case <-ctx.Done():
		}
		return netip.Addr{}, errors.New("no answer")
	}
	apiAddr = freeAddr(t)
	startDaemon(t, "--listen", freeAddr(t), "--api-listen", apiAddr, "--stun-server", "stun.example.org", "--locate-country")
	client = desktop(t, apiAddr)
	client.GetNodeInfo(context.Background(), &nodepb.GetNodeInfoRequest{})
	if n := looked.Load(); n != 0 {
		t.Errorf("before the STUN server answered, the lookup service was asked %d times", n)
	}
	close(answer)
	waitFor(t, func() bool {
		info, err := client.GetNodeInfo(context.Background(), &nodepb.GetNodeInfoRequest{})
		return err == nil && info.GetCountrySource() == "lookup" && info.GetCountryCode() == "FR"
	})
	client.GetNodeInfo(context.Background(), &nodepb.GetNodeInfoRequest{})
	if n := looked.Load(); n != 1 {
		t.Errorf("the lookup service was asked %d times", n)
	}
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
		if got := lookUpCountry(ctx); got != (countryAnswer{}) {
			t.Errorf("%s: %+v, want a definitive nothing", name, got)
		}
	}
	// Nobody there to ask: that may change, so it is not definitive.
	old := countryURL
	countryURL = "http://" + freeAddr(t)
	defer func() { countryURL = old }()
	if got := lookUpCountry(ctx); got != (countryAnswer{transient: true}) {
		t.Errorf("with the service unreachable: %+v", got)
	}
}

// replying stands in for the lookup service, giving each reply in turn
// with its status and Retry-After, and the last one from then on. It
// counts the lookups it is sent.
func replying(t *testing.T, replies ...lookupReply) *atomic.Int32 {
	t.Helper()
	var asked atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		reply := replies[min(int(asked.Add(1)), len(replies))-1]
		if reply.retryAfter != "" {
			w.Header().Set("Retry-After", reply.retryAfter)
		}
		w.WriteHeader(reply.status)
		w.Write([]byte(reply.body))
	}))
	old := countryURL
	countryURL = server.URL
	t.Cleanup(func() {
		countryURL = old
		server.Close()
	})
	return &asked
}

type lookupReply struct {
	status     int
	retryAfter string
	body       string
}

func TestCountryLookupIdentifiesTheDaemonAndTellsTransientFromDefinitive(t *testing.T) {
	t.Run("identifies the daemon", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if got, want := r.UserAgent(), "Sisyphus/"+version; got != want {
				t.Errorf("User-Agent = %q, want %q", got, want)
			}
			w.Write([]byte(`{"country_code":"il"}`))
		}))
		defer server.Close()
		old := countryURL
		countryURL = server.URL
		defer func() { countryURL = old }()
		if got := lookUpCountry(context.Background()); got != (countryAnswer{code: "IL"}) {
			t.Errorf("answer = %+v, want IL", got)
		}
	})

	for name, c := range map[string]struct {
		reply lookupReply
		want  countryAnswer
	}{
		"a rate limit is transient, and says when to ask again": {lookupReply{http.StatusTooManyRequests, "120", `{"country_code":"il"}`}, countryAnswer{transient: true, retryAfter: 2 * time.Minute}},
		"a fault on the service's side is transient":            {lookupReply{http.StatusServiceUnavailable, "", `{"country_code":"il"}`}, countryAnswer{transient: true}},
		"a request timeout is transient":                        {lookupReply{http.StatusRequestTimeout, "", ""}, countryAnswer{transient: true}},
		"a 404 is definitive":                                   {lookupReply{http.StatusNotFound, "120", `{"country_code":"il"}`}, countryAnswer{}},
		"a 403 is definitive":                                   {lookupReply{http.StatusForbidden, "", ""}, countryAnswer{}},
	} {
		t.Run(name, func(t *testing.T) {
			replying(t, c.reply)
			if got := lookUpCountry(context.Background()); got != c.want {
				t.Errorf("answer = %+v, want %+v", got, c.want)
			}
		})
	}
}

func TestRetryAfterIsReadInSecondsOrAsADate(t *testing.T) {
	now := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	for header, want := range map[string]time.Duration{
		"":                              0,
		"90":                            90 * time.Second,
		" 5 ":                           5 * time.Second,
		"-3":                            0,
		"99999999999":                   (1 << 30) * time.Second,
		"Sat, 17 Oct 2026 12:10:00 GMT": 10 * time.Minute,
		"Sat, 17 Oct 2026 11:00:00 GMT": 0,
		"soon":                          0,
	} {
		if got := retryAfter(header, now); got != want {
			t.Errorf("Retry-After %q: %v, want %v", header, got, want)
		}
	}
}

// After each failure in a row the lookup service is left longer, from the
// first wait doubling to the most, or as long as it says, within those.
// A definitive answer is kept for good.
func TestCountryLookupsBackOff(t *testing.T) {
	oldFirst, oldMost := countryRetryFirst, countryRetryMost
	defer func() { countryRetryFirst, countryRetryMost = oldFirst, oldMost }()
	countryRetryFirst, countryRetryMost = time.Minute, time.Hour

	var l countryLookups
	now := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	fail := func(retryAfter time.Duration, want time.Duration) {
		t.Helper()
		if !l.begin(now) {
			t.Fatalf("at %v the lookup was not begun", now)
		}
		if l.begin(now) {
			t.Fatal("a second lookup was begun with one in flight")
		}
		l.end(now, countryAnswer{transient: true, retryAfter: retryAfter})
		if l.begin(now.Add(want - time.Nanosecond)) {
			t.Fatalf("a lookup was begun before %v had passed", want)
		}
		now = now.Add(want)
	}
	fail(0, time.Minute)
	fail(0, 2*time.Minute)
	fail(time.Second, 4*time.Minute)     // never sooner than the backoff
	fail(30*time.Minute, 30*time.Minute) // as long as the service says
	fail(0, 16*time.Minute)
	fail(0, 32*time.Minute)
	fail(0, time.Hour)
	fail(0, time.Hour)
	fail(5*time.Hour, time.Hour) // never longer than the most
	if !l.begin(now) {
		t.Fatal("the lookup was not begun once the wait had passed")
	}
	l.end(now, countryAnswer{code: "DE"})
	if l.found() != "DE" || l.begin(now.Add(24*time.Hour)) {
		t.Errorf("after a country was found: %q, and asked again", l.found())
	}

	// A first wait longer than the most is held to the most.
	countryRetryFirst = 2 * time.Hour
	var long countryLookups
	long.begin(now)
	long.end(now, countryAnswer{transient: true})
	if long.begin(now.Add(time.Hour-time.Nanosecond)) || !long.begin(now.Add(time.Hour)) {
		t.Error("the first wait was not held to the most")
	}
}

// shortRetries makes the waits between lookups short for the rest of the
// test.
func shortRetries(t *testing.T, first, most time.Duration) {
	oldFirst, oldMost := countryRetryFirst, countryRetryMost
	countryRetryFirst, countryRetryMost = first, most
	t.Cleanup(func() { countryRetryFirst, countryRetryMost = oldFirst, oldMost })
}

// A lookup the service could not answer is tried again, once the wait has
// passed, the next time the node is asked where it is, and a country it
// then gives is shown to those watching.
func TestTheCountryIsLookedUpAgainAfterATransientFailure(t *testing.T) {
	shortRetries(t, 20*time.Millisecond, 100*time.Millisecond)
	asked := replying(t,
		lookupReply{status: http.StatusServiceUnavailable},
		lookupReply{status: http.StatusTooManyRequests},
		lookupReply{status: http.StatusOK, body: `{"country_code":"de"}`})
	apiAddr := freeAddr(t)
	startDaemon(t, "--listen", freeAddr(t), "--api-listen", apiAddr, "--locate-country")
	client := desktop(t, apiAddr)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	stream, err := client.WatchNodeState(ctx, &nodepb.GetNodeInfoRequest{})
	if err != nil {
		t.Fatal(err)
	}
	// The watch asks once; asking again now and then lets the retries due
	// be made, and the watch is told of the country they find.
	go func() {
		for ctx.Err() == nil {
			client.GetNodeInfo(ctx, &nodepb.GetNodeInfoRequest{})
			time.Sleep(5 * time.Millisecond)
		}
	}()
	for {
		state, err := stream.Recv()
		if err != nil {
			t.Fatalf("waiting for the country found on retrying: %v", err)
		}
		if state.GetInfo().GetCountryCode() == "DE" && state.GetInfo().GetCountrySource() == "lookup" {
			break
		}
	}
	cancel()
	time.Sleep(50 * time.Millisecond)
	if n := asked.Load(); n != 3 {
		t.Errorf("the lookup service was asked %d times, want 3", n)
	}
}

// placing gives what places a node with nothing nearer than the lookup
// service to place it by, and the searches it runs.
func placing(t *testing.T) (*whereabouts, *background) {
	t.Helper()
	searches := new(background)
	t.Cleanup(searches.stop)
	w := &whereabouts{
		own:    func() []string { return nil },
		seen:   func() netip.Addr { return netip.Addr{} },
		lookUp: true, ctx: context.Background(), start: searches.start, notify: func() {},
		log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	w.gatewayAnswered.Store(true)
	w.stunAnswered.Store(true)
	return w, searches
}

func TestCountryLookupsAreRetriedOnlyWhenTransient(t *testing.T) {
	shortRetries(t, time.Millisecond, 5*time.Millisecond)
	for name, c := range map[string]struct {
		replies []lookupReply
		want    int32
	}{
		"a 404":                       {[]lookupReply{{status: http.StatusNotFound}, {status: http.StatusOK, body: `{"country_code":"de"}`}}, 1},
		"a 200 with no country":       {[]lookupReply{{status: http.StatusOK, body: `{}`}, {status: http.StatusOK, body: `{"country_code":"de"}`}}, 1},
		"a 500 and then a country":    {[]lookupReply{{status: http.StatusInternalServerError}, {status: http.StatusOK, body: `{"country_code":"de"}`}}, 2},
		"a 408 and then a country":    {[]lookupReply{{status: http.StatusRequestTimeout}, {status: http.StatusOK, body: `{"country_code":"de"}`}}, 2},
		"a 429 twice and then a 404":  {[]lookupReply{{status: http.StatusTooManyRequests}, {status: http.StatusTooManyRequests}, {status: http.StatusNotFound}}, 3},
		"a 502, then a 200 with none": {[]lookupReply{{status: http.StatusBadGateway}, {status: http.StatusOK, body: `{"country_code":"N1"}`}}, 2},
	} {
		t.Run(name, func(t *testing.T) {
			asked := replying(t, c.replies...)
			w, searches := placing(t)
			deadline := time.Now().Add(300 * time.Millisecond)
			for time.Now().Before(deadline) {
				w.country()
				time.Sleep(time.Millisecond)
			}
			searches.stop()
			if n := asked.Load(); n != c.want {
				t.Errorf("the lookup service was asked %d times, want %d", n, c.want)
			}
		})
	}
}

// A rate limit's Retry-After is waited out, though the backoff is shorter.
func TestARateLimitsRetryAfterIsWaitedOut(t *testing.T) {
	shortRetries(t, time.Millisecond, 5*time.Second)
	var times []time.Time
	var responseAt time.Time
	var mu sync.Mutex
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		times = append(times, time.Now())
		first := len(times) == 1
		mu.Unlock()
		if first {
			// A delayed response shows that Retry-After starts when the
			// service answers, not when the request begins.
			time.Sleep(100 * time.Millisecond)
			mu.Lock()
			responseAt = time.Now()
			mu.Unlock()
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.Write([]byte(`{"country_code":"se"}`))
	}))
	defer server.Close()
	old := countryURL
	countryURL = server.URL
	defer func() { countryURL = old }()
	w, searches := placing(t)
	waitFor(t, func() bool {
		code, _ := w.country()
		time.Sleep(time.Millisecond)
		return code == "SE"
	})
	searches.stop()
	mu.Lock()
	defer mu.Unlock()
	if len(times) != 2 || times[1].Sub(responseAt) < time.Second {
		t.Errorf("asked at %v after response at %v, want twice with a second after the response", times, responseAt)
	}
}

// With nobody there to ask, the service is tried again after the wait.
func TestACountryLookupWithNoAnswerIsRetried(t *testing.T) {
	shortRetries(t, 10*time.Millisecond, 10*time.Millisecond)
	old := countryURL
	countryURL = "http://" + freeAddr(t)
	defer func() { countryURL = old }()
	w, searches := placing(t)
	if _, source := w.country(); source != "" {
		t.Fatalf("placed by %q", source)
	}
	waitFor(t, func() bool {
		w.lookups.mu.Lock()
		defer w.lookups.mu.Unlock()
		return w.lookups.wait > 0 && !w.lookups.busy
	})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte(`{"country_code":"jp"}`)) }))
	defer server.Close()
	countryURL = server.URL
	waitFor(t, func() bool {
		code, source := w.country()
		return code == "JP" && source == "lookup"
	})
	searches.stop()
}

// Once something nearer places the node, the lookup service is not asked
// again, however long it is left.
func TestNoCountryLookupOnceTheNodeIsPlacedNearer(t *testing.T) {
	shortRetries(t, time.Millisecond, time.Millisecond)
	asked := replying(t, lookupReply{status: http.StatusServiceUnavailable})
	w, searches := placing(t)
	waitFor(t, func() bool {
		w.country()
		return asked.Load() >= 2
	})
	// Its own address now has a place in the world.
	w.own = func() []string { return []string{"/ip4/193.0.6.139/tcp/4001"} }
	searches.stop()
	before := asked.Load()
	searches = new(background)
	w.start = searches.start
	for range 50 {
		if _, source := w.country(); source != "interface" {
			t.Fatalf("placed by %q", source)
		}
		time.Sleep(time.Millisecond)
	}
	searches.stop()
	if n := asked.Load(); n != before {
		t.Errorf("with the node placed by its own address, the service was asked %d more times", n-before)
	}
}

// However many ask at once where the node is, one lookup is in flight.
func TestOneCountryLookupIsInFlightAtATime(t *testing.T) {
	shortRetries(t, time.Nanosecond, time.Nanosecond)
	var inFlight, most, asked atomic.Int32
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		asked.Add(1)
		n := inFlight.Add(1)
		defer inFlight.Add(-1)
		for {
			m := most.Load()
			if n <= m || most.CompareAndSwap(m, n) {
				break
			}
		}
		<-release
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer server.Close()
	old := countryURL
	countryURL = server.URL
	defer func() { countryURL = old }()
	w, searches := placing(t)

	var askers sync.WaitGroup
	for range 32 {
		askers.Add(1)
		go func() {
			defer askers.Done()
			for range 20 {
				w.country()
			}
		}()
	}
	askers.Wait()
	waitFor(t, func() bool { return asked.Load() == 1 })
	close(release)
	// Released, each retry still waits for the one before.
	deadline := time.Now().Add(100 * time.Millisecond)
	for time.Now().Before(deadline) {
		var again sync.WaitGroup
		for range 8 {
			again.Add(1)
			go func() {
				defer again.Done()
				w.country()
			}()
		}
		again.Wait()
	}
	searches.stop()
	if m := most.Load(); m != 1 {
		t.Errorf("%d lookups were in flight at once", m)
	}
	if asked.Load() < 2 {
		t.Errorf("the lookup service was asked %d times, want it retried", asked.Load())
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
	old, oldLocal := countryOf, interfacePrefixes
	countryOf = func(addr netip.Addr) string {
		if addr.IsLoopback() {
			return "XT"
		}
		return ""
	}
	interfacePrefixes = func() []netip.Prefix { return nil }
	defer func() { countryOf, interfacePrefixes = old, oldLocal }()

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

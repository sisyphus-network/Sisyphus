package ipfscluster

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// These tests need no cluster program: a stand-in HTTP server answers as a
// peer would, and shell scripts stand in for a program that fails to start.

// answering returns a client for a server that handles every call with the
// given function.
func answering(t *testing.T, handle http.HandlerFunc) *Client {
	t.Helper()
	srv := httptest.NewServer(handle)
	t.Cleanup(srv.Close)
	return NewClient(srv.Listener.Addr().String(), "the password")
}

// misbehaving returns a client for a server that answers every call with
// the given status and body.
func misbehaving(t *testing.T, status int, body string) *Client {
	t.Helper()
	return answering(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(status)
		fmt.Fprint(w, body)
	})
}

// everyCall makes each kind of call on a client and returns the errors.
func everyCall(c *Client) map[string]error {
	errs := make(map[string]error)
	_, errs["ID"] = c.ID(ctx)
	_, errs["Listening"] = c.Listening(ctx)
	_, errs["Peers"] = c.Peers(ctx)
	_, errs["Pin"] = c.Pin(ctx, "bafkexample", PinOptions{Min: 1, Max: 2})
	errs["Unpin"] = c.Unpin(ctx, "bafkexample")
	_, errs["Pins"] = c.Pins(ctx)
	_, errs["Status"] = c.Status(ctx)
	return errs
}

func TestEveryCallReportsThePeersOwnErrors(t *testing.T) {
	failing := misbehaving(t, http.StatusInternalServerError, `{"code":500,"message":"not enough peers to allocate CID"}`)
	for call, err := range everyCall(failing) {
		if err == nil || !strings.Contains(err.Error(), "not enough peers to allocate CID (HTTP 500)") {
			t.Errorf("%s: %v, want the peer's message", call, err)
		}
	}

	// An error that is not the peer's JSON, as a proxy in front of it might
	// send.
	plain := misbehaving(t, http.StatusBadGateway, "upstream is down\n")
	if _, err := plain.ID(ctx); err == nil || !strings.Contains(err.Error(), "ipfs-cluster GET /id: upstream is down (HTTP 502)") {
		t.Errorf("a plain-text error: %v", err)
	}
}

func TestRepliesThatMakeNoSenseAreErrors(t *testing.T) {
	garbled := misbehaving(t, http.StatusOK, "this is not JSON")
	for call, err := range everyCall(garbled) {
		if call != "Unpin" && (err == nil || !strings.Contains(err.Error(), "reading the reply")) {
			t.Errorf("%s accepted a reply that is not JSON: %v", call, err)
		}
	}
}

func TestCallsFailWhenThePeerCannotBeReached(t *testing.T) {
	// Nothing is listening here.
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := lis.Addr().String()
	lis.Close()
	for call, err := range everyCall(NewClient(addr, "")) {
		if err == nil {
			t.Errorf("%s succeeded with no peer", call)
		}
	}
	// An address no request can even be made for.
	if _, err := NewClient("bad\x00address", "").ID(ctx); err == nil {
		t.Error("a call to a malformed address succeeded")
	}
}

func TestCallsSayWhoIsCallingAndWhatTheyWant(t *testing.T) {
	var asked []string
	c := answering(t, func(w http.ResponseWriter, r *http.Request) {
		user, password, _ := r.BasicAuth()
		asked = append(asked, fmt.Sprintf("%s:%s %s %s?%s", user, password, r.Method, r.URL.Path, r.URL.RawQuery))
		fmt.Fprint(w, `{"cid":"bafkexample","name":"boulder","replication_factor_min":1,"replication_factor_max":2,"allocations":["12D3KooWone"]}`)
	})
	pinned, err := c.Pin(ctx, "bafkexample", PinOptions{Name: "boulder", Min: 1, Max: 2, Prefer: []string{"12D3KooWone", "12D3KooWtwo"}})
	if err != nil || pinned.CID != "bafkexample" || pinned.Name != "boulder" || pinned.Min != 1 || pinned.Max != 2 || len(pinned.Allocations) != 1 {
		t.Errorf("Pin = %+v, %v", pinned, err)
	}
	if _, err := c.Pin(ctx, "bafkexample", PinOptions{Min: 3, Max: 3}); err != nil {
		t.Fatal(err)
	}
	if err := c.Unpin(ctx, "bafkexample"); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"sisyphusd:the password POST /pins/bafkexample?mode=recursive&name=boulder&replication-max=2&replication-min=1&user-allocations=12D3KooWone%2C12D3KooWtwo",
		"sisyphusd:the password POST /pins/bafkexample?mode=recursive&name=&replication-max=3&replication-min=3",
		"sisyphusd:the password DELETE /pins/bafkexample?",
	}
	if strings.Join(asked, "\n") != strings.Join(want, "\n") {
		t.Errorf("the peer was asked:\n%s\nwant:\n%s", strings.Join(asked, "\n"), strings.Join(want, "\n"))
	}
}

func TestUnpinningWhatIsNotPinnedIsNotAnError(t *testing.T) {
	absent := misbehaving(t, http.StatusNotFound, `{"code":404,"message":"cannot unpin pin uncommitted to state: not found"}`)
	if err := absent.Unpin(ctx, "bafkexample"); err != nil {
		t.Errorf("Unpin of something not pinned: %v", err)
	}
	// Anything else not found is still an error.
	if _, err := absent.ID(ctx); !errors.Is(err, errNotFound) {
		t.Errorf("ID from a peer that answers 404: %v", err)
	}
}

func TestListsAreReadOneItemAfterAnother(t *testing.T) {
	c := answering(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/peers":
			fmt.Fprintln(w, `{"id":"12D3KooWone","peername":"first","addresses":["/ip4/127.0.0.1/tcp/9096/p2p/12D3KooWone"]}`)
			fmt.Fprintln(w, `{"id":"12D3KooWtwo","error":"dial backoff"}`)
		case "/allocations":
			fmt.Fprintln(w, `{"cid":"bafkone","name":"a","replication_factor_min":2,"replication_factor_max":3,"allocations":["12D3KooWone","12D3KooWtwo"]}`)
			fmt.Fprintln(w, `{"cid":"bafktwo"}`)
		case "/pins":
			fmt.Fprintln(w, `{"cid":"bafkone","name":"a","peer_map":{"12D3KooWone":{"peername":"first","status":"pinned"},"12D3KooWtwo":{"peername":"second","status":"pin_error","error":"context deadline exceeded"},"12D3KooWthree":{"status":"remote"}}}`)
		}
	})
	peers, err := c.Peers(ctx)
	if err != nil || len(peers) != 2 || peers[0].Name != "first" || len(peers[0].Addresses) != 1 || peers[1].Error != "dial backoff" {
		t.Errorf("Peers = %+v, %v", peers, err)
	}
	pins, err := c.Pins(ctx)
	if err != nil || len(pins) != 2 || pins[0].Min != 2 || pins[0].Max != 3 || len(pins[0].Allocations) != 2 || pins[1].CID != "bafktwo" {
		t.Errorf("Pins = %+v, %v", pins, err)
	}
	all, err := c.Status(ctx)
	if err != nil || len(all) != 1 || all[0].CID != "bafkone" || all[0].Members["12D3KooWtwo"].Error != "context deadline exceeded" {
		t.Fatalf("Status = %+v, %v", all, err)
	}
	if holders := all[0].Holders(); len(holders) != 1 || holders[0] != "12D3KooWone" {
		t.Errorf("Holders = %v, want only the member that has pinned it", holders)
	}
}

// A list is sent as it is made, so a peer that fails part-way can only say
// so after the items it has already sent. A short list must not pass for a
// whole one: what is missing from it would be taken for unpinned.
func TestAListThatFailsPartWayIsAnError(t *testing.T) {
	c := answering(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Trailer", "X-Stream-Error")
		fmt.Fprintln(w, `{"cid":"bafkone"}`)
		w.Header().Set("X-Stream-Error", "the state could not be read")
	})
	if pins, err := c.Pins(ctx); err == nil || !strings.Contains(err.Error(), "the state could not be read") {
		t.Errorf("Pins = %+v, %v; want the failure the peer reported at the end", pins, err)
	}
}

func TestListeningFindsThePeersLoopbackAddress(t *testing.T) {
	addresses := []string{"/ip4/10.0.0.5/tcp/9096/p2p/12D3KooWone", "/ip4/127.0.0.1/udp/9096/quic-v1/p2p/12D3KooWone", "/dns4/example"}
	c := answering(t, func(w http.ResponseWriter, _ *http.Request) {
		json.NewEncoder(w).Encode(Peer{ID: "12D3KooWone", Addresses: addresses})
	})
	if _, err := c.Listening(ctx); err == nil || !strings.Contains(err.Error(), "not listening on this machine's loopback address") {
		t.Errorf("Listening with no loopback TCP address: %v", err)
	}
	addresses = append(addresses, "/ip4/127.0.0.1/tcp/34945/p2p/12D3KooWone")
	if got, err := c.Listening(ctx); err != nil || got != "127.0.0.1:34945" {
		t.Errorf("Listening = %q, %v", got, err)
	}
}

// fakeProgram returns the path of a script that stands in for
// ipfs-cluster-service.
func fakeProgram(t *testing.T, script string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "ipfs-cluster-service")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+script+"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

// initialised returns a directory that looks set up, so that Start goes
// straight to configuring and running the peer.
func initialised(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "service.json"), []byte(`{"cluster":{"peername":"old"},"api":"not a section"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestStartFailures(t *testing.T) {
	ident := newIdentity(t)
	short := 300 * time.Millisecond

	configIsDir := t.TempDir()
	os.Mkdir(filepath.Join(configIsDir, "service.json"), 0o700)
	configIsGarbage := t.TempDir()
	os.WriteFile(filepath.Join(configIsGarbage, "service.json"), []byte("not json"), 0o600)
	identityIsDir := initialised(t)
	os.Mkdir(filepath.Join(identityIsDir, "identity.json"), 0o700)
	logIsDir := initialised(t)
	os.Mkdir(filepath.Join(logIsDir, "daemon.log"), 0o700)
	notAProgram := filepath.Join(t.TempDir(), "ipfs-cluster-service")
	os.WriteFile(notAProgram, []byte("this is not a program"), 0o700)

	tests := []struct {
		name string
		cfg  Config
		want string
	}{
		{"there is no such program", Config{Binary: filepath.Join(t.TempDir(), "absent"), Dir: t.TempDir()}, "no such file"},
		{"setting up the peer fails", Config{Binary: fakeProgram(t, `echo "starting up"; echo "error: disk is full" >&2; exit 1`), Dir: t.TempDir()}, "error: disk is full"},
		{"the settings cannot be read", Config{Binary: fakeProgram(t, "exit 0"), Dir: configIsDir}, "is a directory"},
		{"the settings are not JSON", Config{Binary: fakeProgram(t, "exit 0"), Dir: configIsGarbage}, "invalid character"},
		{"the identity cannot be written", Config{Binary: fakeProgram(t, "exit 0"), Dir: identityIsDir}, "is a directory"},
		{"the log cannot be written", Config{Binary: fakeProgram(t, "exit 0"), Dir: logIsDir}, "is a directory"},
		{"the program cannot be run", Config{Binary: notAProgram, Dir: initialised(t)}, "exec format error"},
		{"the peer exits at once", Config{Binary: fakeProgram(t, `echo "error: the datastore is locked"; exit 1`), Dir: initialised(t)}, "stopped while starting: error: the datastore is locked"},
		{"the peer never listens", Config{Binary: fakeProgram(t, "exec sleep 30"), Dir: initialised(t), ReadyTimeout: short}, "did not answer within 300ms"},
	}
	for _, tt := range tests {
		tt.cfg.Identity, tt.cfg.KuboAPI = ident, "127.0.0.1:5001"
		d, err := Start(ctx, tt.cfg)
		if err == nil {
			d.Stop()
			t.Errorf("%s: Start succeeded", tt.name)
			continue
		}
		if !strings.Contains(err.Error(), tt.want) {
			t.Errorf("%s: error %v, want one containing %q", tt.name, err, tt.want)
		}
	}
}

func TestStartFailsIfNoPortCanBeFoundForTheAPI(t *testing.T) {
	old := listen
	listen = func() (net.Listener, error) { return nil, errors.New("no ports left") }
	defer func() { listen = old }()
	_, err := Start(ctx, Config{Binary: fakeProgram(t, "exit 0"), Dir: initialised(t), Identity: newIdentity(t)})
	if err == nil || !strings.Contains(err.Error(), "find a port for the peer's API: no ports left") {
		t.Errorf("error %v, want the failure to listen", err)
	}
}

func TestStartFailsIfTheSettingsCannotBeRewritten(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root can write to a read-only file")
	}
	dir := initialised(t)
	if err := os.Chmod(filepath.Join(dir, "service.json"), 0o400); err != nil {
		t.Fatal(err)
	}
	_, err := Start(ctx, Config{Binary: fakeProgram(t, "exit 0"), Dir: dir, Identity: newIdentity(t), KuboAPI: "127.0.0.1:5001"})
	if err == nil || !strings.Contains(err.Error(), "permission denied") {
		t.Errorf("error %v, want the settings reported unwritable", err)
	}
}

func TestStartGivesUpWhenCancelled(t *testing.T) {
	cancelled, cancel := context.WithCancel(ctx)
	go func() {
		time.Sleep(100 * time.Millisecond)
		cancel()
	}()
	_, err := Start(cancelled, Config{Binary: fakeProgram(t, "exec sleep 30"), Dir: initialised(t), Identity: newIdentity(t), KuboAPI: "127.0.0.1:5001"})
	if !errors.Is(err, context.Canceled) {
		t.Errorf("error %v, want context.Canceled", err)
	}
}

func TestStopKillsAPeerThatWillNotShutDown(t *testing.T) {
	defer func(grace time.Duration) { stopGrace = grace }(stopGrace)
	stopGrace = 200 * time.Millisecond

	// A peer that ignores the request to stop.
	stubborn := fakeProgram(t, `trap "" INT; while :; do sleep 1; done`)
	started := time.Now()
	_, err := Start(ctx, Config{Binary: stubborn, Dir: initialised(t), Identity: newIdentity(t), KuboAPI: "127.0.0.1:5001", ReadyTimeout: 100 * time.Millisecond})
	if err == nil {
		t.Fatal("Start succeeded")
	}
	// Start stops the peer before reporting that it never answered; had it
	// not been killed, that would have hung.
	if took := time.Since(started); took > 5*time.Second {
		t.Errorf("giving up on a stubborn peer took %v", took)
	}
}

// The program reads settings from its environment before its files. A
// setting left in the environment of whoever starts the node must not
// reach it: a secret from there would put the peer in some other cluster.
func TestThePeerIsNotGivenClusterSettingsFromTheEnvironment(t *testing.T) {
	t.Setenv("CLUSTER_SECRET", "from the environment")
	t.Setenv("SISYPHUS_TEST_KEPT", "kept")
	dir := initialised(t)
	seen := filepath.Join(t.TempDir(), "environment")
	program := fakeProgram(t, `env > "`+seen+`"; exit 1`)
	if _, err := Start(ctx, Config{Binary: program, Dir: dir, Identity: newIdentity(t), KuboAPI: "127.0.0.1:5001"}); err == nil {
		t.Fatal("Start succeeded")
	}
	environment := string(readFile(seen))
	if strings.Contains(environment, "CLUSTER_SECRET") {
		t.Error("the program was started with CLUSTER_SECRET set")
	}
	for _, want := range []string{"SISYPHUS_TEST_KEPT=kept\n", "IPFS_CLUSTER_PATH=" + dir + "\n"} {
		if !strings.Contains(environment, want) {
			t.Errorf("the program's environment lacks %q", want)
		}
	}
}

func TestConfigureSetsWhatAPoolNeedsAndKeepsTheRest(t *testing.T) {
	dir := t.TempDir()
	original := `{
		"cluster": {"peername": "old", "secret": "old", "listen_multiaddress": ["/ip4/0.0.0.0/tcp/9096", "/ip4/0.0.0.0/udp/9096/quic"], "disable_repinning": true, "state_sync_interval": "5m0s"},
		"consensus": {"crdt": {"cluster_name": "ipfs-cluster", "trusted_peers": ["*"]}},
		"api": {"ipfsproxy": {"listen_multiaddress": "/ip4/127.0.0.1/tcp/9095"}, "pinsvcapi": {}, "restapi": {"http_listen_multiaddress": "/ip4/127.0.0.1/tcp/9094", "idle_timeout": "2m0s"}},
		"ipfs_connector": {"ipfshttp": {"node_multiaddress": "/ip4/127.0.0.1/tcp/5001", "pin_timeout": "2m0s"}},
		"informer": {"disk": {"metric_ttl": "30s", "metric_type": "freespace"}},
		"datastore": {"pebble": {"pebble_options": {"cache_size_bytes": 1073741824, "max_open_files": 1000}}}
	}`
	if err := os.WriteFile(filepath.Join(dir, "service.json"), []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	ident := newIdentity(t)
	cfg := Config{
		Dir: dir, Identity: ident, Name: "hand", Secret: testSecret, KuboAPI: "127.0.0.1:40123",
		Trusted: []string{"12D3KooWcoordinator"}, Follower: true, Peers: []string{"/ip4/127.0.0.1/tcp/40999/p2p/12D3KooWcoordinator"},
		Heartbeat: 4 * time.Second,
	}
	if err := configure(cfg, "127.0.0.1:40456", "the password"); err != nil {
		t.Fatal(err)
	}

	var edited struct {
		Cluster struct {
			Name      string   `json:"peername"`
			Secret    string   `json:"secret"`
			Listen    []string `json:"listen_multiaddress"`
			Peers     []string `json:"peer_addresses"`
			MDNS      string   `json:"mdns_interval"`
			Follower  bool     `json:"follower_mode"`
			NoRepin   bool     `json:"disable_repinning"`
			Ping      string   `json:"monitor_ping_interval"`
			StateSync string   `json:"state_sync_interval"`
		} `json:"cluster"`
		Consensus struct {
			CRDT struct {
				ClusterName string   `json:"cluster_name"`
				Trusted     []string `json:"trusted_peers"`
				Rebroadcast string   `json:"rebroadcast_interval"`
			} `json:"crdt"`
		} `json:"consensus"`
		API  map[string]map[string]any `json:"api"`
		IPFS struct {
			HTTP struct {
				Node       string `json:"node_multiaddress"`
				PinTimeout string `json:"pin_timeout"`
			} `json:"ipfshttp"`
		} `json:"ipfs_connector"`
		Informer map[string]map[string]any `json:"informer"`
		Monitor  struct {
			PubSub struct {
				Check string `json:"check_interval"`
			} `json:"pubsubmon"`
		} `json:"monitor"`
		Datastore struct {
			Pebble struct {
				Options map[string]any `json:"pebble_options"`
			} `json:"pebble"`
		} `json:"datastore"`
	}
	data, _ := os.ReadFile(filepath.Join(dir, "service.json"))
	if err := json.Unmarshal(data, &edited); err != nil {
		t.Fatal(err)
	}
	c := edited.Cluster
	if c.Name != "hand" || c.Secret != testSecret || c.MDNS != "0s" || !c.Follower || c.NoRepin || c.Ping != "4s" {
		t.Errorf("cluster settings: %+v", c)
	}
	// It listens for other members on this machine only, over TCP only.
	if len(c.Listen) != 1 || c.Listen[0] != "/ip4/127.0.0.1/tcp/0" || len(c.Peers) != 1 || c.Peers[0] != cfg.Peers[0] {
		t.Errorf("listens on %v and starts from %v", c.Listen, c.Peers)
	}
	if sent := edited.Consensus.CRDT.Rebroadcast; sent != "8s" {
		t.Errorf("the state is sent round every %s, want every two heartbeats", sent)
	}
	if trusted := edited.Consensus.CRDT.Trusted; len(trusted) != 1 || trusted[0] != "12D3KooWcoordinator" {
		t.Errorf("trusts %v, want only the coordinator", trusted)
	}
	// Only the one API is left, where the client will look for it.
	rest := edited.API["restapi"]
	if len(edited.API) != 1 || rest["http_listen_multiaddress"] != "/ip4/127.0.0.1/tcp/40456" || fmt.Sprint(rest["basic_auth_credentials"]) != "map[sisyphusd:the password]" {
		t.Errorf("API settings: %v", edited.API)
	}
	if edited.IPFS.HTTP.Node != "/ip4/127.0.0.1/tcp/40123" || edited.Monitor.PubSub.Check != "4s" {
		t.Errorf("pins on %s, checks every %s", edited.IPFS.HTTP.Node, edited.Monitor.PubSub.Check)
	}
	for _, informer := range []string{"disk", "pinqueue", "tags"} {
		if edited.Informer[informer]["metric_ttl"] != "8s" {
			t.Errorf("the %s informer's settings: %v", informer, edited.Informer[informer])
		}
	}
	if edited.Datastore.Pebble.Options["cache_size_bytes"] != float64(64<<20) {
		t.Errorf("datastore settings: %v", edited.Datastore.Pebble.Options)
	}
	// What was not mentioned is as it was.
	if c.StateSync != "5m0s" || edited.Consensus.CRDT.ClusterName != "ipfs-cluster" || rest["idle_timeout"] != "2m0s" || edited.IPFS.HTTP.PinTimeout != "2m0s" ||
		edited.Informer["disk"]["metric_type"] != "freespace" || edited.Datastore.Pebble.Options["max_open_files"] != float64(1000) {
		t.Errorf("settings that should have been left alone were changed:\n%s", data)
	}
	for _, file := range []string{"service.json", "identity.json"} {
		if info, _ := os.Stat(filepath.Join(dir, file)); info.Mode().Perm() != 0o600 {
			t.Errorf("%s, which holds a secret, has mode %o", file, info.Mode().Perm())
		}
	}
	var identity struct {
		ID string `json:"id"`
	}
	json.Unmarshal(readFile(filepath.Join(dir, "identity.json")), &identity)
	if identity.ID != ident.ID() {
		t.Errorf("the peer's identity is %q, want the node's, %s", identity.ID, ident.ID())
	}
}

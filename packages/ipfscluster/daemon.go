package ipfscluster

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/sisyphus-network/Sisyphus/packages/identity"
)

// Config says how to run a cluster peer.
type Config struct {
	// Binary is the ipfs-cluster-service program to run. Empty means the
	// one on the PATH.
	Binary string
	// Dir is the directory for the peer's settings and its record of the
	// cluster's pins, which is created and set up if it does not exist. It
	// must not be shared with another peer.
	Dir string
	// Identity is the key the peer runs with, so that it is the same peer
	// as the node it serves.
	Identity *identity.Identity
	// Name is a label for people to recognise the peer by.
	Name string
	// Secret is what the members of one cluster share, as 64 hexadecimal
	// digits. Only peers holding the same secret can connect to each other.
	Secret string
	// KuboAPI is the host and port of the API of the Kubo daemon the peer
	// pins on.
	KuboAPI string
	// Trusted are the IDs of the members whose changes to what is pinned
	// the peer acts on. Changes from anyone else are ignored.
	Trusted []string
	// Follower makes the peer refuse to change what is pinned itself: it
	// keeps what the trusted members say and nothing more.
	Follower bool
	// Peers are members to connect to at start-up: their addresses, each
	// ending in /p2p/ and the peer's ID.
	Peers []string
	// Heartbeat is how often members tell each other they are there and
	// how much room they have. A member unheard of for two heartbeats is
	// taken to be gone. Zero means fifteen seconds.
	Heartbeat time.Duration
	// ReadyTimeout is how long to wait for the peer to start answering.
	// Zero means a minute.
	ReadyTimeout time.Duration
}

// Daemon is a running cluster peer and a client for it. The client stays
// valid if the peer is restarted; calls made meanwhile fail.
type Daemon struct {
	*Client
	binary string

	// mu serialises starting and stopping.
	mu     sync.Mutex
	cfg    Config
	cmd    *exec.Cmd
	exited chan struct{}
}

// Start sets up the directory if need be, starts a cluster peer on it and
// waits until it answers. The peer listens on loopback ports of its own
// choosing, both for API calls and for the other members, who must be
// brought to it some other way.
func Start(ctx context.Context, cfg Config) (*Daemon, error) {
	name := cfg.Binary
	if name == "" {
		name = "ipfs-cluster-service"
	}
	binary, err := exec.LookPath(name)
	if err != nil {
		return nil, fmt.Errorf("ipfs-cluster: %w", err)
	}
	d := &Daemon{Client: &Client{http: &http.Client{}}, cfg: cfg, binary: binary}
	if d.cfg.ReadyTimeout == 0 {
		d.cfg.ReadyTimeout = time.Minute
	}
	if d.cfg.Heartbeat == 0 {
		d.cfg.Heartbeat = 15 * time.Second
	}
	if err := d.launch(ctx); err != nil {
		return nil, err
	}
	return d, nil
}

// Restart stops the peer and starts it again with another secret, Kubo
// address and members to connect to, keeping its directory and identity. If
// it returns an error the peer is not running.
func (d *Daemon) Restart(ctx context.Context, secret, kuboAPI string, peers []string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.stop()
	d.cfg.Secret, d.cfg.KuboAPI, d.cfg.Peers = secret, kuboAPI, peers
	return d.launch(ctx)
}

// listen opens a loopback port for the system to pick.
var listen = func() (net.Listener, error) { return net.Listen("tcp", "127.0.0.1:0") }

// launch writes the peer's settings from d.cfg, starts it and waits until it
// answers, then points the client at it.
func (d *Daemon) launch(ctx context.Context) error {
	cfg := d.cfg
	run := func(args ...string) *exec.Cmd {
		cmd := exec.Command(d.binary, args...)
		// The program takes settings from its environment in preference to
		// its files. None of those may come from whoever started the node.
		for _, variable := range os.Environ() {
			if !strings.HasPrefix(variable, "CLUSTER_") {
				cmd.Env = append(cmd.Env, variable)
			}
		}
		cmd.Env = append(cmd.Env, "IPFS_CLUSTER_PATH="+cfg.Dir)
		return cmd
	}

	configFile := filepath.Join(cfg.Dir, "service.json")
	if _, err := os.Stat(configFile); errors.Is(err, os.ErrNotExist) {
		if out, err := run("init", "--consensus", "crdt").CombinedOutput(); err != nil {
			return fmt.Errorf("ipfs-cluster: setting up a peer in %s: %w: %s", cfg.Dir, err, lastLine(out))
		}
	}
	// The program has no way to say which port it picked for its API, so one
	// is picked for it: a port that was free a moment ago.
	lis, err := listen()
	if err != nil {
		return fmt.Errorf("ipfs-cluster: find a port for the peer's API: %w", err)
	}
	api := lis.Addr().String()
	lis.Close()
	var random [16]byte
	rand.Read(random[:]) // never fails; see crypto/rand
	password := hex.EncodeToString(random[:])
	if err := configure(cfg, api, password); err != nil {
		return fmt.Errorf("ipfs-cluster: %w", err)
	}

	logFile := filepath.Join(cfg.Dir, "daemon.log")
	output, err := os.OpenFile(logFile, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("ipfs-cluster: %w", err)
	}
	defer output.Close()
	cmd := run("daemon")
	cmd.Stdout, cmd.Stderr = output, output
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("ipfs-cluster: %w", err)
	}
	d.cmd, d.exited = cmd, make(chan struct{})
	go func(exited chan struct{}) {
		cmd.Wait()
		close(exited)
	}(d.exited)

	deadline := time.NewTimer(cfg.ReadyTimeout)
	defer deadline.Stop()
	poll := time.NewTicker(50 * time.Millisecond)
	defer poll.Stop()
	probe := NewClient(api, password)
	for {
		select {
		case <-poll.C:
			if _, err := probe.ID(ctx); err == nil {
				d.Client.point(api, password)
				return nil
			}
		case <-d.exited:
			return fmt.Errorf("ipfs-cluster: the peer stopped while starting: %s (see %s)", lastLine(readFile(logFile)), logFile)
		case <-deadline.C:
			d.stop()
			return fmt.Errorf("ipfs-cluster: the peer did not answer within %s (see %s)", cfg.ReadyTimeout, logFile)
		case <-ctx.Done():
			d.stop()
			return ctx.Err()
		}
	}
}

// stopGrace is how long Stop waits for the peer to shut down by itself.
var stopGrace = 10 * time.Second

// Stop asks the peer to shut down and waits for it, killing it if it takes
// more than ten seconds. It waits for a restart in progress.
func (d *Daemon) Stop() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.stop()
}

func (d *Daemon) stop() {
	d.cmd.Process.Signal(os.Interrupt)
	select {
	case <-d.exited:
	case <-time.After(stopGrace):
		d.cmd.Process.Kill()
		<-d.exited
	}
}

// configure edits a peer's settings: the node's own key as its identity,
// the cluster's secret, the Kubo daemon to pin on, whose word to take on
// what is pinned, and an API that listens at the given loopback address and
// asks for the given password. It listens nowhere else, and looks for no
// peers by itself. Settings it does not mention are left alone.
func configure(cfg Config, api, password string) error {
	configFile := filepath.Join(cfg.Dir, "service.json")
	data, err := os.ReadFile(configFile)
	if err != nil {
		return err
	}
	var config map[string]any
	if err := json.Unmarshal(data, &config); err != nil {
		return fmt.Errorf("%s: %w", configFile, err)
	}
	// section returns the settings found by following path down from the
	// top, making whatever is missing on the way.
	section := func(path ...string) map[string]any {
		at := config
		for _, name := range path {
			inner, ok := at[name].(map[string]any)
			if !ok {
				inner = make(map[string]any)
				at[name] = inner
			}
			at = inner
		}
		return at
	}
	heartbeat, twice := cfg.Heartbeat.String(), (2 * cfg.Heartbeat).String()

	cluster := section("cluster")
	cluster["peername"] = cfg.Name
	cluster["secret"] = cfg.Secret
	cluster["listen_multiaddress"] = []string{"/ip4/127.0.0.1/tcp/0"}
	cluster["peer_addresses"] = append([]string{}, cfg.Peers...)
	cluster["mdns_interval"] = "0s"
	cluster["enable_relay_hop"] = false
	cluster["follower_mode"] = cfg.Follower
	// When a holder is gone for good, a trusted member finds another.
	cluster["disable_repinning"] = false
	cluster["monitor_ping_interval"] = heartbeat
	crdt := section("consensus", "crdt")
	crdt["trusted_peers"] = append([]string{}, cfg.Trusted...)
	// A member that was out of hearing when a pin was made or let go, as
	// one is for a moment after the coordinator restarts, learns of it when
	// the state is next sent round. The program would do that once a
	// minute.
	crdt["rebroadcast_interval"] = twice

	// Of the three APIs the program offers, only the one this package uses
	// is kept.
	rest := section("api", "restapi")
	rest["http_listen_multiaddress"] = multiaddr(api)
	rest["basic_auth_credentials"] = map[string]string{apiUser: password}
	config["api"] = map[string]any{"restapi": rest}

	section("ipfs_connector", "ipfshttp")["node_multiaddress"] = multiaddr(cfg.KuboAPI)
	section("monitor", "pubsubmon")["check_interval"] = heartbeat
	for _, informer := range []string{"disk", "pinqueue", "tags"} {
		section("informer", informer)["metric_ttl"] = twice
	}
	// The program would otherwise set aside a gibibyte of memory for
	// reading its record of the pins.
	section("datastore", "pebble", "pebble_options")["cache_size_bytes"] = 64 << 20

	identityFile := filepath.Join(cfg.Dir, "identity.json")
	ident, _ := json.MarshalIndent(map[string]string{
		"id": cfg.Identity.ID(), "private_key": base64.StdEncoding.EncodeToString(cfg.Identity.Libp2pKey()),
	}, "", "  ") // strings always encode
	if err := os.WriteFile(identityFile, ident, 0o600); err != nil {
		return err
	}
	edited, _ := json.MarshalIndent(config, "", "  ") // what was decoded always encodes
	return os.WriteFile(configFile, edited, 0o600)
}

// multiaddr gives a host and port in the form the program's settings take.
func multiaddr(hostport string) string {
	host, port, _ := net.SplitHostPort(hostport)
	return "/ip4/" + host + "/tcp/" + port
}

// readFile returns a file's contents, or nothing if it cannot be read.
func readFile(path string) []byte {
	data, _ := os.ReadFile(path)
	return data
}

// lastLine returns the last line of a program's output that says anything.
func lastLine(output []byte) string {
	lines := strings.Split(strings.TrimSpace(string(output)), "\n")
	return strings.TrimSpace(lines[len(lines)-1])
}

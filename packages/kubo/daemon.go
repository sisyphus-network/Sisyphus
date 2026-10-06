package kubo

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/excho0/Sisyphus/packages/identity"
)

// Config says how to run a Kubo daemon.
type Config struct {
	// Binary is the ipfs program to run. Empty means the one on the PATH.
	Binary string
	// Repo is the directory for Kubo's repository, which is created and
	// set up if it does not exist. It must not be shared with another
	// daemon.
	Repo string
	// Identity is the key Kubo runs with, so that it is the same peer as
	// the node it serves.
	Identity *identity.Identity
	// ReadyTimeout is how long to wait for the daemon to start answering.
	// Zero means a minute.
	ReadyTimeout time.Duration
	// Swarm, if set, puts the daemon on a private network with the other
	// daemons that hold the same key. Without it the daemon runs offline.
	Swarm *Swarm
}

// Swarm describes a private network of Kubo daemons. Only daemons holding
// the same key can connect to each other; to anything else, including the
// public IPFS network, they are unreachable and unintelligible.
type Swarm struct {
	// Key is the network's shared secret, as NewSwarmKey makes it.
	Key string
	// Port is the TCP port to accept other members on. Zero lets the
	// system choose.
	Port int
	// Loopback has the daemon accept connections from its own machine only.
	// Members elsewhere must then be brought to it some other way.
	Loopback bool
	// Peers are members to connect to and stay connected to: their
	// addresses, each ending in /p2p/ and the peer's ID.
	Peers []string
	// PeerTimeout is how long to look among peers for a block before
	// giving it up as not found. Zero means thirty seconds.
	PeerTimeout time.Duration
}

// NewSwarmKey generates the secret for a new private network, in the form
// Kubo keeps it in its swarm.key file.
func NewSwarmKey() string {
	var secret [32]byte
	rand.Read(secret[:]) // never fails; see crypto/rand
	return "/key/swarm/psk/1.0.0/\n/base16/\n" + hex.EncodeToString(secret[:]) + "\n"
}

// Daemon is a running Kubo daemon and a client for it. The client stays
// valid if the daemon is restarted by Rekey: calls made meanwhile wait.
type Daemon struct {
	*Client
	cfg    Config
	binary string
	cmd    *exec.Cmd
	exited chan struct{}
}

// Start sets up the repository if need be, starts a Kubo daemon on it and
// waits until it answers. The daemon listens for API calls on a loopback
// port of its own choosing. It exchanges blocks with the members of
// cfg.Swarm, or with nobody if there is none.
func Start(ctx context.Context, cfg Config) (*Daemon, error) {
	name := cfg.Binary
	if name == "" {
		name = "ipfs"
	}
	binary, err := exec.LookPath(name)
	if err != nil {
		return nil, fmt.Errorf("kubo: %w", err)
	}
	d := &Daemon{Client: &Client{http: &http.Client{}}, cfg: cfg, binary: binary}
	if d.cfg.ReadyTimeout == 0 {
		d.cfg.ReadyTimeout = time.Minute
	}
	if err := d.launch(ctx); err != nil {
		return nil, err
	}
	return d, nil
}

// Rekey restarts the daemon as a member of swarm in place of the one it was
// started on, keeping its repository and identity. Calls to the daemon made
// while it restarts wait for it. If it returns an error the daemon is not
// running.
func (d *Daemon) Rekey(ctx context.Context, swarm *Swarm) error {
	d.Client.mu.Lock()
	defer d.Client.mu.Unlock()
	d.stop()
	d.cfg.Swarm = swarm
	return d.launch(ctx)
}

// launch configures the repository for d.cfg, starts the daemon and waits
// until it answers, then points the client at it. The caller must make sure
// no call is in progress on the client.
func (d *Daemon) launch(ctx context.Context) error {
	cfg := d.cfg
	run := func(args ...string) *exec.Cmd {
		cmd := exec.Command(d.binary, args...)
		cmd.Env = append(os.Environ(), "IPFS_PATH="+cfg.Repo)
		return cmd
	}

	configFile := filepath.Join(cfg.Repo, "config")
	if _, err := os.Stat(configFile); errors.Is(err, os.ErrNotExist) {
		if out, err := run("init", "--empty-repo").CombinedOutput(); err != nil {
			return fmt.Errorf("kubo: setting up a repository in %s: %w: %s", cfg.Repo, err, lastLine(out))
		}
	}
	if err := configure(configFile, cfg.Identity, cfg.Swarm); err != nil {
		return fmt.Errorf("kubo: %w", err)
	}
	args := []string{"daemon", "--offline"}
	var env []string
	peerTimeout := time.Duration(0)
	if cfg.Swarm != nil {
		args = []string{"daemon"}
		// Makes Kubo refuse to start rather than fall back to the public
		// network if it cannot find the key.
		env = []string{"LIBP2P_FORCE_PNET=1"}
		if peerTimeout = cfg.Swarm.PeerTimeout; peerTimeout == 0 {
			peerTimeout = 30 * time.Second
		}
	}

	// Kubo writes its API address here once it is listening. One left by an
	// earlier run would point at a port nobody is on.
	apiFile := filepath.Join(cfg.Repo, "api")
	os.Remove(apiFile)
	logFile := filepath.Join(cfg.Repo, "daemon.log")
	output, err := os.OpenFile(logFile, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("kubo: %w", err)
	}
	defer output.Close()
	cmd := run(args...)
	cmd.Env = append(cmd.Env, env...)
	cmd.Stdout, cmd.Stderr = output, output
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("kubo: %w", err)
	}
	d.cmd, d.exited = cmd, make(chan struct{})
	go func(exited chan struct{}) {
		cmd.Wait()
		close(exited)
	}(d.exited)

	deadline := time.NewTimer(cfg.ReadyTimeout)
	defer deadline.Stop()
	poll := time.NewTicker(20 * time.Millisecond)
	defer poll.Stop()
	// The daemon is asked directly whether it is up, not through d.Client,
	// which may be held still for the restart this is part of.
	var probe *Client
	for {
		select {
		case <-poll.C:
			if probe == nil {
				// Not listening yet; the address file appears when it is.
				if addr, ok := apiAddress(apiFile); ok {
					probe = NewClient(addr)
				}
				continue
			}
			if _, err := probe.ID(ctx); err == nil {
				d.Client.base, d.Client.PeerTimeout = probe.base, peerTimeout
				return nil
			}
		case <-d.exited:
			return fmt.Errorf("kubo: the daemon stopped while starting: %s (see %s)", lastLine(readFile(logFile)), logFile)
		case <-deadline.C:
			d.stop()
			return fmt.Errorf("kubo: the daemon did not answer within %s (see %s)", cfg.ReadyTimeout, logFile)
		case <-ctx.Done():
			d.stop()
			return ctx.Err()
		}
	}
}

// stopGrace is how long Stop waits for the daemon to shut down by itself.
var stopGrace = 10 * time.Second

// Stop asks the daemon to shut down and waits for it, killing it if it
// takes more than ten seconds. It waits for a restart in progress.
func (d *Daemon) Stop() {
	d.Client.mu.Lock()
	defer d.Client.mu.Unlock()
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

// configure edits a repository's settings: the node's own key as Kubo's
// identity, an API port chosen at start-up, and nothing that reaches out to
// the public IPFS network. With a swarm it also installs the swarm's key and
// the members to stay connected to. Settings it does not mention are left
// alone.
func configure(configFile string, ident *identity.Identity, swarm *Swarm) error {
	data, err := os.ReadFile(configFile)
	if err != nil {
		return err
	}
	var config map[string]any
	if err := json.Unmarshal(data, &config); err != nil {
		return fmt.Errorf("%s: %w", configFile, err)
	}
	set := func(section, key string, value any) {
		inner, ok := config[section].(map[string]any)
		if !ok {
			inner = make(map[string]any)
			config[section] = inner
		}
		inner[key] = value
	}
	set("Identity", "PeerID", ident.ID())
	set("Identity", "PrivKey", base64.StdEncoding.EncodeToString(ident.Libp2pKey()))
	set("Addresses", "API", "/ip4/127.0.0.1/tcp/0")
	set("Addresses", "Gateway", []string{})
	set("Addresses", "Swarm", []string{})
	set("Discovery", "MDNS", map[string]any{"Enabled": false})
	set("AutoConf", "Enabled", false)
	config["Bootstrap"] = []string{}
	set("Peering", "Peers", []any{})

	keyFile := filepath.Join(filepath.Dir(configFile), "swarm.key")
	if swarm == nil {
		// A key left from an earlier run as a swarm member is of no use to
		// an offline daemon.
		os.Remove(keyFile)
	} else {
		// A private network runs over plain TCP only, and finds content
		// through its own members rather than public indexers.
		listen := "0.0.0.0"
		if swarm.Loopback {
			listen = "127.0.0.1"
		}
		set("Addresses", "Swarm", []string{fmt.Sprintf("/ip4/%s/tcp/%d", listen, swarm.Port)})
		set("Routing", "Type", "dht")
		set("AutoTLS", "Enabled", false)
		set("Swarm", "Transports", map[string]any{"Network": map[string]any{
			"QUIC": false, "WebTransport": false, "Websocket": false, "WebRTCDirect": false,
		}})
		// Each member is both somewhere to start from and someone to stay
		// connected to.
		addressesOf := make(map[string][]string)
		var order []string
		for _, peer := range swarm.Peers {
			address, id, ok := strings.Cut(peer, "/p2p/")
			if !ok || address == "" || id == "" {
				return fmt.Errorf("swarm member address %q does not end in /p2p/ and a peer ID", peer)
			}
			if _, seen := addressesOf[id]; !seen {
				order = append(order, id)
			}
			addressesOf[id] = append(addressesOf[id], address)
		}
		peering := make([]any, 0, len(order))
		for _, id := range order {
			peering = append(peering, map[string]any{"ID": id, "Addrs": addressesOf[id]})
		}
		config["Bootstrap"] = swarm.Peers
		set("Peering", "Peers", peering)
		if err := os.WriteFile(keyFile, []byte(swarm.Key), 0o600); err != nil {
			return err
		}
	}

	edited, _ := json.MarshalIndent(config, "", "  ") // what was decoded always encodes
	return os.WriteFile(configFile, edited, 0o600)
}

// apiAddress reads the host and port Kubo is listening on from the address
// file it writes, which holds something like /ip4/127.0.0.1/tcp/5001.
func apiAddress(apiFile string) (string, bool) {
	parts := strings.Split(strings.TrimSpace(string(readFile(apiFile))), "/")
	if len(parts) != 5 || parts[3] != "tcp" {
		return "", false
	}
	return parts[2] + ":" + parts[4], true
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

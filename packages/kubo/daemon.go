package kubo

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
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
}

// Daemon is a running Kubo daemon and a client for it.
type Daemon struct {
	*Client
	cmd    *exec.Cmd
	exited chan struct{}
}

// Start sets up the repository if need be, starts a Kubo daemon on it and
// waits until it answers. The daemon listens for API calls on a loopback
// port of its own choosing and, for now, runs offline: it exchanges nothing
// with other peers.
func Start(ctx context.Context, cfg Config) (*Daemon, error) {
	name := cfg.Binary
	if name == "" {
		name = "ipfs"
	}
	binary, err := exec.LookPath(name)
	if err != nil {
		return nil, fmt.Errorf("kubo: %w", err)
	}
	run := func(args ...string) *exec.Cmd {
		cmd := exec.Command(binary, args...)
		cmd.Env = append(os.Environ(), "IPFS_PATH="+cfg.Repo)
		return cmd
	}

	configFile := filepath.Join(cfg.Repo, "config")
	if _, err := os.Stat(configFile); errors.Is(err, os.ErrNotExist) {
		if out, err := run("init", "--empty-repo").CombinedOutput(); err != nil {
			return nil, fmt.Errorf("kubo: setting up a repository in %s: %w: %s", cfg.Repo, err, lastLine(out))
		}
	}
	if err := configure(configFile, cfg.Identity); err != nil {
		return nil, fmt.Errorf("kubo: %w", err)
	}

	// Kubo writes its API address here once it is listening. One left by an
	// earlier run would point at a port nobody is on.
	apiFile := filepath.Join(cfg.Repo, "api")
	os.Remove(apiFile)
	logFile := filepath.Join(cfg.Repo, "daemon.log")
	output, err := os.OpenFile(logFile, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return nil, fmt.Errorf("kubo: %w", err)
	}
	defer output.Close()
	cmd := run("daemon", "--offline")
	cmd.Stdout, cmd.Stderr = output, output
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("kubo: %w", err)
	}
	d := &Daemon{cmd: cmd, exited: make(chan struct{})}
	go func() {
		cmd.Wait()
		close(d.exited)
	}()

	timeout := cfg.ReadyTimeout
	if timeout == 0 {
		timeout = time.Minute
	}
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	poll := time.NewTicker(20 * time.Millisecond)
	defer poll.Stop()
	for {
		select {
		case <-poll.C:
			if d.Client == nil {
				// Not listening yet; the address file appears when it is.
				if addr, ok := apiAddress(apiFile); ok {
					d.Client = NewClient(addr)
				}
				continue
			}
			if _, err := d.ID(ctx); err == nil {
				return d, nil
			}
		case <-d.exited:
			return nil, fmt.Errorf("kubo: the daemon stopped while starting: %s (see %s)", lastLine(readFile(logFile)), logFile)
		case <-deadline.C:
			d.Stop()
			return nil, fmt.Errorf("kubo: the daemon did not answer within %s (see %s)", timeout, logFile)
		case <-ctx.Done():
			d.Stop()
			return nil, ctx.Err()
		}
	}
}

// stopGrace is how long Stop waits for the daemon to shut down by itself.
var stopGrace = 10 * time.Second

// Stop asks the daemon to shut down and waits for it, killing it if it
// takes more than ten seconds.
func (d *Daemon) Stop() {
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
// the public IPFS network. Settings it does not mention are left alone.
func configure(configFile string, ident *identity.Identity) error {
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

package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/excho0/Sisyphus/packages/kubo"
)

// requireKubo skips a test that needs the real ipfs program when it is not
// installed, or fails it if SISYPHUS_REQUIRE_KUBO says it must be.
func requireKubo(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("ipfs"); err != nil {
		if os.Getenv("SISYPHUS_REQUIRE_KUBO") != "" {
			t.Fatal("ipfs is not installed, and SISYPHUS_REQUIRE_KUBO is set")
		}
		t.Skip("ipfs is not installed")
	}
}

// ipfsIn runs the ipfs command against the Kubo repository of the node whose
// data is in dataDir.
func ipfsIn(t *testing.T, dataDir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("ipfs", args...)
	cmd.Env = append(os.Environ(), "IPFS_PATH="+filepath.Join(dataDir, "ipfs"))
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("ipfs %v: %v", args, err)
	}
	return string(out)
}

func TestNodeWithKuboKeepsJobDataWhereTheIPFSCommandCanReadIt(t *testing.T) {
	requireKubo(t)
	dataDir := t.TempDir()
	addr, _ := startNode(t, "--kubo", "--swarm-port", "0", "--data-dir", dataDir, "--slots", "2")

	// Kubo runs as the same peer as the node.
	if got, want := strings.TrimSpace(ipfsIn(t, dataDir, "id", "-f", "<id>")), nodeID(t, dataDir); got != want {
		t.Errorf("Kubo is peer %s, the node is %s", got, want)
	}

	text := strings.Repeat("the boulder rolls down, and Sisyphus walks after it.\n", 20_000)
	input := strings.TrimSpace(mustCLI(t, "blob", "put", "--addr", addr, writeFile(t, text)))
	if got := ipfsIn(t, dataDir, "cat", input); got != text {
		t.Errorf("ipfs cat of the uploaded input returned %d bytes, want the %d uploaded", len(got), len(text))
	}

	out := mustCLI(t, "job", "submit", "--addr", addr, "--workload", "wordcount", "--tasks", "4", "--params", `{"input":"`+input+`"}`)
	result := regexp.MustCompile(`"output":"([a-z0-9]+)"`).FindStringSubmatch(out)
	if result == nil {
		t.Fatalf("no result in:\n%s", out)
	}
	// The job's result is an ordinary IPFS file too.
	table := ipfsIn(t, dataDir, "cat", result[1])
	if !strings.HasPrefix(table, "20000\tafter\n20000\tand\n20000\tboulder\n") {
		t.Errorf("ipfs cat of the result starts:\n%.80s", table)
	}
	if viaNode := mustCLI(t, "blob", "get", "--addr", addr, result[1]); viaNode != table {
		t.Error("the node and the ipfs command return different bytes for the result")
	}
}

func TestKuboFlagFailures(t *testing.T) {
	// A PATH with no ipfs on it.
	t.Setenv("PATH", t.TempDir())
	_, err := cli(t, "run", "--kubo", "--data-dir", t.TempDir(), "--listen", freeAddr(t))
	if err == nil || !strings.Contains(err.Error(), "kubo:") {
		t.Errorf("starting with no ipfs program: %v", err)
	}

	// The pool's swarm key cannot be read, or cannot be created.
	keyIsDir := t.TempDir()
	os.Mkdir(filepath.Join(keyIsDir, "swarm.key"), 0o700)
	if _, err := cli(t, "run", "--kubo", "--data-dir", keyIsDir, "--listen", freeAddr(t)); err == nil || !strings.Contains(err.Error(), "read swarm key") {
		t.Errorf("a swarm key that cannot be read: %v", err)
	}
	if os.Getuid() != 0 {
		readOnly := t.TempDir()
		nodeID(t, readOnly)
		os.Chmod(readOnly, 0o500)
		defer os.Chmod(readOnly, 0o700)
		if _, err := cli(t, "run", "--kubo", "--data-dir", readOnly, "--listen", freeAddr(t)); err == nil || !strings.Contains(err.Error(), "save swarm key") {
			t.Errorf("a swarm key that cannot be saved: %v", err)
		}
	}

	// A worker whose record of its coordinator holds an unusable address.
	badKnown := t.TempDir()
	os.WriteFile(filepath.Join(badKnown, "known.json"), []byte(`{"bad\u0000address":"12D3KooWexample"}`), 0o600)
	if _, err := cli(t, "run", "--kubo", "--role", "worker", "--coordinator", badAddr, "--data-dir", badKnown); err == nil || !strings.Contains(err.Error(), "invalid control character") {
		t.Errorf("a worker with an unusable coordinator address: %v", err)
	}
}

func TestWorkerWithKuboNeedsACoordinatorThatRunsASwarm(t *testing.T) {
	addr := freeAddr(t)
	startDaemon(t, "--role", "coordinator", "--listen", addr) // no --kubo
	invitation := invite(t, addr, "worker")

	_, err := cli(t, "run", "--kubo", "--role", "worker", "--coordinator", addr, "--join", invitation, "--data-dir", t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "does not run a private IPFS network") {
		t.Errorf("error %v, want the coordinator's refusal", err)
	}
}

// peersOf returns the IDs of the peers a node's Kubo is connected to.
func peersOf(t *testing.T, dataDir string) string {
	t.Helper()
	return ipfsIn(t, dataDir, "swarm", "peers")
}

func TestAPoolWithKuboSharesDataOverItsOwnPrivateNetwork(t *testing.T) {
	requireKubo(t)
	addr := freeAddr(t)
	coordinatorDir, workerDir := t.TempDir(), t.TempDir()
	startDaemon(t, "--role", "coordinator", "--kubo", "--swarm-port", "0", "--listen", addr, "--data-dir", coordinatorDir)
	startDaemon(t, "--role", "worker", "--kubo", "--coordinator", addr, "--data-dir", workerDir, "--name", "hand", "--slots", "2")
	waitForOutput(t, "hand", "nodes", "--addr", addr)

	// The two Kubo daemons found each other, as the nodes they belong to.
	waitFor(t, func() bool { return strings.Contains(peersOf(t, workerDir), nodeID(t, coordinatorDir)) })
	if !strings.Contains(peersOf(t, coordinatorDir), nodeID(t, workerDir)) {
		t.Errorf("the coordinator's Kubo is not connected to the worker's:\n%s", peersOf(t, coordinatorDir))
	}
	// Both hold the same swarm key, which the worker was given on asking.
	coordinatorKey, _ := os.ReadFile(filepath.Join(coordinatorDir, "ipfs", "swarm.key"))
	workerKey, _ := os.ReadFile(filepath.Join(workerDir, "ipfs", "swarm.key"))
	if len(coordinatorKey) == 0 || string(coordinatorKey) != string(workerKey) {
		t.Errorf("swarm keys differ or are missing: %q and %q", coordinatorKey, workerKey)
	}

	text := strings.Repeat("the boulder rolls down, and Sisyphus walks after it.\n", 20_000)
	input := strings.TrimSpace(mustCLI(t, "blob", "put", "--addr", addr, writeFile(t, text)))
	out := mustCLI(t, "job", "submit", "--addr", addr, "--workload", "wordcount", "--tasks", "4", "--params", `{"input":"`+input+`"}`)
	if !strings.Contains(out, `"words":180000`) || !strings.Contains(out, "on hand") {
		t.Fatalf("job output:\n%s", out)
	}
	// The worker's Kubo now holds the input, which it was never sent by the
	// node: it asked the swarm.
	if got := ipfsIn(t, workerDir, "cat", "--offline", input); got != text {
		t.Errorf("the worker's Kubo holds %d bytes of the input, want all %d", len(got), len(text))
	}
}

func TestCoordinatorKeepsItsSwarmKeyAcrossRestarts(t *testing.T) {
	requireKubo(t)
	dataDir := t.TempDir()
	_, stop := startNode(t, "--kubo", "--swarm-port", "0", "--data-dir", dataDir)
	first, _ := os.ReadFile(filepath.Join(dataDir, "swarm.key"))
	stop()

	startNode(t, "--kubo", "--swarm-port", "0", "--data-dir", dataDir)
	second, _ := os.ReadFile(filepath.Join(dataDir, "swarm.key"))
	if len(first) == 0 || string(first) != string(second) {
		t.Errorf("the swarm key changed across a restart: %q then %q", first, second)
	}
	if info, _ := os.Stat(filepath.Join(dataDir, "swarm.key")); info.Mode().Perm() != 0o600 {
		t.Errorf("the swarm key is stored with mode %o", info.Mode().Perm())
	}
}

func TestNodeWithKuboWillNotStartIfItsPinsCannotBeKept(t *testing.T) {
	requireKubo(t)
	dataDir := t.TempDir()
	// A file where the directory for the store's pins must go.
	if err := os.WriteFile(filepath.Join(dataDir, "kubo-pins"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := cli(t, "run", "--kubo", "--swarm-port", "0", "--data-dir", dataDir, "--listen", freeAddr(t))
	if err == nil || !strings.Contains(err.Error(), "open blob store") {
		t.Errorf("error %v, want the store refused", err)
	}
	// Kubo, which had started, was stopped again.
	if out, _ := exec.Command("pgrep", "-f", filepath.Join(dataDir, "ipfs")).Output(); len(out) != 0 {
		t.Errorf("a Kubo daemon is still running for a node that failed to start: %s", out)
	}
}

func TestPoolSwarmReportsAKuboThatDoesNotAnswer(t *testing.T) {
	// Nothing is listening where this Kubo is supposed to be.
	addr := freeAddr(t)
	swarm := &poolSwarm{key: "secret", daemon: &kubo.Daemon{Client: kubo.NewClient(addr)}}

	if _, err := swarm.Addresses(context.Background()); err == nil {
		t.Error("Addresses succeeded with no Kubo to ask")
	}
	if swarm.Key() != "secret" {
		t.Errorf("Key = %q", swarm.Key())
	}
}

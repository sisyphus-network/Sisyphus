package main

import (
	"context"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/ipfs/go-cid"

	"github.com/excho0/Sisyphus/packages/identity"
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
	addr, _ := startNode(t, "--kubo", "--data-dir", dataDir, "--slots", "2")

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
	startDaemon(t, "--role", "coordinator", "--kubo", "--listen", addr, "--data-dir", coordinatorDir)
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
	_, stop := startNode(t, "--kubo", "--data-dir", dataDir)
	first, _ := os.ReadFile(filepath.Join(dataDir, "swarm.key"))
	stop()

	startNode(t, "--kubo", "--data-dir", dataDir)
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
	_, err := cli(t, "run", "--kubo", "--data-dir", dataDir, "--listen", freeAddr(t))
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

// A removed node still holds the key it was given. The pool must move to a
// new one that the node is not given, and the remaining workers must follow.
func TestRemovingAMemberMovesThePoolToAKeyItDoesNotHave(t *testing.T) {
	requireKubo(t)
	addr := freeAddr(t)
	coordinatorDir, stayerDir, leaverDir := t.TempDir(), t.TempDir(), t.TempDir()
	startDaemon(t, "--role", "coordinator", "--kubo", "--listen", addr, "--data-dir", coordinatorDir)
	startDaemon(t, "--role", "worker", "--kubo", "--coordinator", addr, "--data-dir", stayerDir, "--name", "stayer", "--slots", "2")

	// The node to be removed is run here, to see it stop.
	invitation := invite(t, addr, "worker")
	leaverStopped := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		leaverStopped <- run(ctx, []string{"run", "--role", "worker", "--kubo", "--coordinator", addr, "--join", invitation, "--data-dir", leaverDir, "--name", "leaver"})
	}()
	waitFor(t, func() bool {
		out, _ := cli(t, "nodes", "--addr", addr)
		return strings.Contains(out, "stayer") && strings.Contains(out, "leaver")
	})

	keyOf := func(dataDir string) string {
		key, _ := os.ReadFile(filepath.Join(dataDir, "ipfs", "swarm.key"))
		return string(key)
	}
	oldKey := keyOf(coordinatorDir)
	if oldKey == "" || keyOf(leaverDir) != oldKey {
		t.Fatalf("before removal the leaver does not hold the pool's key")
	}

	mustCLI(t, "pool", "remove", "--addr", addr, nodeID(t, leaverDir))
	if err := <-leaverStopped; err == nil || !strings.Contains(err.Error(), "removed from the pool") {
		t.Errorf("the removed node stopped with %v", err)
	}

	// The coordinator is on a new key, saved for its next start...
	newKey := keyOf(coordinatorDir)
	if newKey == oldKey {
		t.Fatal("the pool's key did not change")
	}
	if saved, _ := os.ReadFile(filepath.Join(coordinatorDir, "swarm.key")); string(saved) != newKey {
		t.Error("the new key was not saved for the coordinator's next start")
	}
	// ...the remaining worker follows it there and reconnects...
	waitFor(t, func() bool { return keyOf(stayerDir) == newKey })
	waitFor(t, func() bool {
		// Not before its Kubo is back up: run against a repository whose
		// daemon is down, the ipfs command takes the repository's lock
		// itself, and the daemon then cannot start.
		if _, err := os.Stat(filepath.Join(stayerDir, "ipfs", "api")); err != nil {
			return false
		}
		cmd := exec.Command("ipfs", "swarm", "peers")
		cmd.Env = append(os.Environ(), "IPFS_PATH="+filepath.Join(stayerDir, "ipfs"))
		out, _ := cmd.Output()
		return strings.Contains(string(out), nodeID(t, coordinatorDir))
	})
	// ...and the removed node was never given it.
	if keyOf(leaverDir) != oldKey {
		t.Error("the removed node's Kubo has the new key")
	}

	// Work still gets done, with data moving over the new network.
	text := strings.Repeat("one must imagine Sisyphus happy.\n", 20_000)
	input := strings.TrimSpace(mustCLI(t, "blob", "put", "--addr", addr, writeFile(t, text)))
	out := mustCLI(t, "job", "submit", "--addr", addr, "--workload", "wordcount", "--tasks", "2", "--params", `{"input":"`+input+`"}`)
	if !strings.Contains(out, `"words":100000`) || !strings.Contains(out, "on stayer") {
		t.Errorf("job after the key change:\n%s", out)
	}

	// Anyone still holding the old key, as the removed node does, is shut out.
	coordinatorAddrs := strings.Fields(ipfsIn(t, coordinatorDir, "id", "-f", "<addrs>"))
	intruder, err := kubo.Start(context.Background(), kubo.Config{
		Repo: filepath.Join(t.TempDir(), "ipfs"), Identity: mustIdentity(t, leaverDir),
		Swarm: &kubo.Swarm{Key: oldKey, Peers: coordinatorAddrs, PeerTimeout: 3 * time.Second},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer intruder.Stop()
	if err := intruder.Connect(context.Background(), coordinatorAddrs[0]); err == nil {
		t.Error("a Kubo with the old key connected to the coordinator's")
	}
	if _, err := intruder.BlockGet(context.Background(), input); err == nil {
		t.Error("a Kubo with the old key fetched the job's input")
	}
}

func mustIdentity(t *testing.T, dataDir string) *identity.Identity {
	t.Helper()
	ident, err := loadIdentity(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	return ident
}

func TestPoolRekeyCommand(t *testing.T) {
	requireKubo(t)
	dataDir := t.TempDir()
	addr, _ := startNode(t, "--kubo", "--data-dir", dataDir)
	before, _ := os.ReadFile(filepath.Join(dataDir, "swarm.key"))

	out := mustCLI(t, "pool", "rekey", "--addr", addr)
	after, _ := os.ReadFile(filepath.Join(dataDir, "swarm.key"))
	if string(after) == string(before) || len(after) == 0 {
		t.Error("pool rekey did not change the key")
	}
	if want := "the pool's private IPFS network has a new key (" + fingerprint(string(after)) + ")\n"; out != want {
		t.Errorf("printed %q, want %q", out, want)
	}
	// The node still works, with its data, on the new key.
	id := strings.TrimSpace(mustCLI(t, "blob", "put", "--addr", addr, writeFile(t, "stored after the change")))
	if got := mustCLI(t, "blob", "get", "--addr", addr, id); got != "stored after the change" {
		t.Errorf("blob get after a key change: %q", got)
	}
}

func TestPoolRekeyFailures(t *testing.T) {
	// A node with no private network to rekey.
	addr, _ := startNode(t)
	if _, err := cli(t, "pool", "rekey", "--addr", addr); err == nil || !strings.Contains(err.Error(), "does not run a private IPFS network") {
		t.Errorf("rekeying a node without Kubo: %v", err)
	}
	for _, tt := range []struct {
		args []string
		want string
	}{
		{[]string{"pool", "rekey", "extra"}, `unexpected argument "extra"`},
		{[]string{"pool", "rekey", "--no-such-flag"}, "flag provided but not defined"},
		{[]string{"pool", "rekey", "--addr", badAddr}, "invalid control character"},
	} {
		if _, err := cli(t, tt.args...); err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("%v: error %v, want one containing %q", tt.args, err, tt.want)
		}
	}
}

func TestSwarmKeyChangesThatFail(t *testing.T) {
	requireKubo(t)
	ctx := context.Background()
	repo := filepath.Join(t.TempDir(), "ipfs")
	daemon, err := kubo.Start(ctx, kubo.Config{Repo: repo, Identity: mustIdentity(t, t.TempDir()), Swarm: &kubo.Swarm{Key: kubo.NewSwarmKey()}})
	if err != nil {
		t.Fatal(err)
	}
	defer daemon.Stop()
	logs := new(syncBuffer)
	log := slog.New(slog.NewTextHandler(logs, nil))

	// A worker told of a key that is already its own does nothing. Told of
	// another when its coordinator cannot be asked, it says so and keeps
	// trying for as long as it is running.
	worker := &poolSwarm{daemon: daemon, key: "current"}
	worker.adopt(ctx, badAddr, nil, fingerprint("current"), log)
	if logs.String() != "" {
		t.Errorf("being told of its own key made the worker log:\n%s", logs.String())
	}
	defer func(retry time.Duration) { adoptRetry = retry }(adoptRetry)
	adoptRetry = 20 * time.Millisecond
	running, stop := context.WithTimeout(ctx, 150*time.Millisecond)
	defer stop()
	worker.adopt(running, badAddr, nil, fingerprint("another"), log)
	if attempts := strings.Count(logs.String(), "could not follow the pool's private IPFS network to its new key"); attempts < 2 || worker.Key() != "current" {
		t.Errorf("a key change the worker could not follow: %d attempts, key %q, log:\n%s", attempts, worker.Key(), logs.String())
	}

	// A coordinator that cannot save the new key keeps the old one.
	unsaved := &poolSwarm{daemon: daemon, key: "current", keyFile: filepath.Join(t.TempDir(), "missing", "swarm.key")}
	if err := unsaved.Rekey(ctx); err == nil || !strings.Contains(err.Error(), "save swarm key") || unsaved.Key() != "current" {
		t.Errorf("Rekey with nowhere to save: %v, key %q", err, unsaved.Key())
	}
	// One whose Kubo will not restart reports that.
	if os.Getuid() != 0 {
		os.Chmod(filepath.Join(repo, "config"), 0o400)
		stuck := &poolSwarm{daemon: daemon, key: "current", keyFile: filepath.Join(t.TempDir(), "swarm.key")}
		if err := stuck.Rekey(ctx); err == nil || stuck.Key() != "current" {
			t.Errorf("Rekey when Kubo cannot be reconfigured: %v, key %q", err, stuck.Key())
		}
		os.Chmod(filepath.Join(repo, "config"), 0o600)
	}
}

func TestWorkerSaysSoWhenThePrivateNetworkFailsIt(t *testing.T) {
	logs := new(syncBuffer)
	warn := warnOfFallback(slog.New(slog.NewTextHandler(logs, nil)))
	warn(cid.MustParse(helloCID))
	if !strings.Contains(logs.String(), "did not supply a blob") || !strings.Contains(logs.String(), helloCID) {
		t.Errorf("logged:\n%s", logs.String())
	}
}

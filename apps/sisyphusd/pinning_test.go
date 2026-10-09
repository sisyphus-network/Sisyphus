package main

import (
	"database/sql"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sisyphus-network/Sisyphus/apps/sisyphusd/pinning"
	"github.com/sisyphus-network/Sisyphus/packages/kubo"
	"github.com/sisyphus-network/Sisyphus/packages/nodedb"
)

// startKeeper runs a node that serves the pinning API, and returns its
// address, the address of its service and the file holding the service's
// key.
func startKeeper(t *testing.T, dataDir string, args ...string) (addr, service, keyFile string) {
	t.Helper()
	addr, at := freeAddr(t), freeAddr(t)
	startDaemon(t, append([]string{"--data-dir", dataDir, "--listen", addr, "--name", "keeper", "--pinning-listen", at}, args...)...)
	waitForOutput(t, "keeper", "nodes", "--addr", addr)
	keyFile = filepath.Join(dataDir, "pinning.token")
	waitFor(t, func() bool {
		conn, err := net.Dial("tcp", at)
		if err == nil {
			conn.Close()
		}
		return err == nil
	})
	return addr, "http://" + at, keyFile
}

func TestOneNodesDataIsKeptByAnotherThroughThePinningAPI(t *testing.T) {
	addr := freeAddr(t)
	startDaemon(t, "--listen", addr, "--name", "asker")
	waitForOutput(t, "asker", "nodes", "--addr", addr)
	keeperDir := t.TempDir()
	keeper, service, keyFile := startKeeper(t, keeperDir)

	// What both nodes hold, the keeper pins as soon as it is asked.
	shared := writeFile(t, "the boulder rolls")
	id := strings.TrimSpace(mustCLI(t, "blob", "put", "--addr", addr, shared))
	mustCLI(t, "blob", "put", "--addr", keeper, "--no-pin", shared)
	out := mustCLI(t, "blob", "pin-remote", "--addr", addr, "--service", service, "--key-file", keyFile, "--wait", id, "boulder.txt")
	asked := strings.Fields(out)
	if len(asked) != 2 || asked[1] != "pinned" {
		t.Fatalf("blob pin-remote printed %q, want a request ID and pinned", out)
	}
	if lines := pinLines(t, keeper, id); !has(lines, "pinning-service:"+asked[0]+" released") {
		t.Errorf("the keeper's pins on the blob: %v", lines)
	}
	listed := mustCLI(t, "blob", "pins-remote", "--service", service, "--key-file", keyFile)
	if lines := strings.Split(strings.TrimSpace(listed), "\n"); len(lines) != 2 || !strings.HasPrefix(lines[0], "REQUEST") ||
		strings.Join(strings.Fields(lines[1])[:3], " ") != asked[0]+" "+id+" pinned" || !strings.HasSuffix(lines[1], "boulder.txt") {
		t.Errorf("blob pins-remote printed:\n%s", listed)
	}

	// What only the asker holds, a keeper without Kubo cannot come by.
	only := strings.TrimSpace(mustCLI(t, "blob", "put", "--addr", addr, writeFile(t, "kept on one node only")))
	out, err := cli(t, "blob", "pin-remote", "--addr", addr, "--service", service, "--key-file", keyFile, only)
	lost := strings.Fields(out)
	if err == nil || !strings.Contains(err.Error(), "the service could not pin "+only) || !strings.Contains(err.Error(), "no way to fetch it") || len(lost) != 2 || lost[1] != "failed" {
		t.Fatalf("pinning what the keeper cannot fetch printed %q and returned: %v", out, err)
	}

	// Removed, a request no longer keeps the data.
	mustCLI(t, "blob", "unpin-remote", "--service", service, "--key-file", keyFile, asked[0])
	if lines := pinLines(t, keeper, id); has(lines, "pinning-service:"+asked[0]+" released") {
		t.Errorf("the keeper's pins on the blob after the request was removed: %v", lines)
	}
	if _, err := cli(t, "blob", "unpin-remote", "--service", service, "--key-file", keyFile, asked[0]); err == nil || !strings.Contains(err.Error(), "NOT_FOUND") {
		t.Errorf("removing a request twice: %v", err)
	}
	mustCLI(t, "blob", "unpin-remote", "--service", service, "--key-file", keyFile, lost[0])
	if listed := mustCLI(t, "blob", "pins-remote", "--service", service, "--key-file", keyFile); listed != "the service has been asked to keep nothing\n" {
		t.Errorf("blob pins-remote with nothing asked printed %q", listed)
	}

	// The service has a key of its own, which is not the one that controls
	// the node.
	if _, err := os.Stat(filepath.Join(keeperDir, "api.token")); err == nil {
		t.Error("serving the pinning API made the node an API token")
	}
	wrongKey := writeFile(t, "not-the-key\n")
	if _, err := cli(t, "blob", "pins-remote", "--service", service, "--key-file", wrongKey); err == nil || !strings.Contains(err.Error(), "UNAUTHORIZED") {
		t.Errorf("listing with another key: %v", err)
	}
}

func TestAPinningServiceIsWaitedForAndItsFailuresReported(t *testing.T) {
	before := remotePoll
	remotePoll = time.Millisecond
	defer func() { remotePoll = before }()
	const blob = "bafkreigh2akiscaildcqabsyg3dfr6chu3fgpregiymsck7e7aqa4s52zy"

	// A service that takes a request in and then answers these, in turn,
	// when asked after it.
	var mu sync.Mutex
	var sent pinning.Pin
	var answers []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if r.Method == http.MethodPost {
			json.NewDecoder(r.Body).Decode(&sent)
		}
		answer := answers[0]
		answers = answers[1:]
		if answer == "" {
			http.Error(w, "down for maintenance", http.StatusServiceUnavailable)
			return
		}
		io.WriteString(w, answer)
	}))
	defer server.Close()
	keyFile := writeFile(t, "the-key\n")
	pin := func(more ...string) (string, error) {
		return cli(t, append([]string{"blob", "pin-remote", "--no-origins", "--service", server.URL, "--key-file", keyFile}, more...)...)
	}

	answers = []string{`{"requestid":"r1","status":"queued"}`, `{"requestid":"r1","status":"pinning"}`, `{"requestid":"r1","status":"pinned"}`}
	if out, err := pin("--wait", blob, "noise"); err != nil || out != "r1 queued\nr1 pinned\n" {
		t.Errorf("waiting for a request that is fetched printed %q and returned: %v", out, err)
	}
	// The service was told what to keep and under what name, and with
	// --no-origins nothing of where from.
	if sent.CID != blob || sent.Name != "noise" || sent.Origins != nil {
		t.Errorf("the service was sent %+v", sent)
	}
	// Without --wait the request is left with the service.
	answers = []string{`{"requestid":"r2","status":"queued"}`}
	if out, err := pin(blob); err != nil || out != "r2 queued\n" {
		t.Errorf("a request not waited for printed %q and returned: %v", out, err)
	}
	for _, tt := range []struct {
		name    string
		answers []string
		out     string
		want    string
	}{
		{"with a reason", []string{`{"requestid":"r3","status":"pinning"}`, `{"requestid":"r3","status":"failed","info":{"status_details":"nobody has it"}}`},
			"r3 pinning\nr3 failed\n", "the service could not pin " + blob + ": nobody has it"},
		{"with a reason in words of its own", []string{`{"requestid":"r4","status":"failed","info":{"cause":"quota"}}`},
			"r4 failed\n", "the service could not pin " + blob + ": map[cause:quota]"},
		{"with no reason", []string{`{"requestid":"r5","status":"failed"}`},
			"r5 failed\n", "the service could not pin " + blob + ": it did not say why"},
		{"to answer when asked after", []string{`{"requestid":"r6","status":"queued"}`, ""},
			"r6 queued\n", "waiting for request r6: the pinning service refused (HTTP 503): down for maintenance"},
		{"to take the request", []string{""}, "", "the pinning service refused (HTTP 503): down for maintenance"},
	} {
		answers = tt.answers
		if out, err := pin("--wait", blob); err == nil || !strings.Contains(err.Error(), tt.want) || out != tt.out {
			t.Errorf("a service that fails %s: printed %q and returned %v, want %q and %q", tt.name, out, err, tt.out, tt.want)
		}
	}

	// A listing shows each request with its state, and under one that
	// failed, why.
	answers = []string{`{"count":2,"results":[{"requestid":"r2","status":"pinned","created":"2026-10-09T12:00:00Z","pin":{"cid":"` + blob + `","name":"noise"}},{"requestid":"r1","status":"failed","created":"2026-10-08T12:00:00Z","pin":{"cid":"` + blob + `"},"info":{"status_details":"nobody has it"}}]}`}
	out, err := cli(t, "blob", "pins-remote", "--service", server.URL, "--key-file", keyFile)
	if lines := strings.Split(out, "\n"); err != nil || len(lines) != 5 || strings.Join(strings.Fields(lines[0]), " ") != "REQUEST CID STATUS ASKED NAME" ||
		strings.Join(strings.Fields(lines[1]), " ") != "r2 "+blob+" pinned 2026-10-09T12:00:00Z noise" || strings.Join(strings.Fields(lines[2]), " ") != "r1 "+blob+" failed 2026-10-08T12:00:00Z" ||
		lines[3] != "  nobody has it" {
		t.Errorf("blob pins-remote printed %q and returned: %v", out, err)
	}

	// What is wrong with a command is said before any service is asked.
	answers = nil
	unreadable := filepath.Join(t.TempDir(), "no-such-key")
	brokenKnown := t.TempDir()
	os.WriteFile(filepath.Join(brokenKnown, "known.json"), []byte("not json"), 0o600)
	for _, tt := range []struct {
		args []string
		want string
	}{
		{[]string{"blob", "pin-remote", "--no-such-flag"}, "flag provided but not defined"},
		{[]string{"blob", "pin-remote", "--service", server.URL, "--key-file", keyFile}, "expected a CID"},
		{[]string{"blob", "pin-remote", "--service", server.URL, "--key-file", keyFile, blob, "name", "more"}, "expected a CID"},
		{[]string{"blob", "pin-remote", "--service", server.URL, "--key-file", keyFile, "not-a-cid"}, `invalid CID "not-a-cid"`},
		{[]string{"blob", "pin-remote", "--key-file", keyFile, blob}, "--service <address> and --key-file"},
		{[]string{"blob", "pin-remote", "--service", server.URL, blob}, "--service <address> and --key-file"},
		{[]string{"blob", "pin-remote", "--service", server.URL, "--key-file", unreadable, blob}, "read the pinning service's key"},
		{[]string{"blob", "pin-remote", "--service", "pins.example.com", "--key-file", keyFile, blob}, "looks like https://host/path"},
		// The node is asked where its Kubo is, unless told not to be.
		{[]string{"blob", "pin-remote", "--addr", freeAddr(t), "--data-dir", t.TempDir(), "--service", server.URL, "--key-file", keyFile, blob}, "where its Kubo can be reached"},
		{[]string{"blob", "pin-remote", "--addr", badAddr, "--data-dir", brokenKnown, "--service", server.URL, "--key-file", keyFile, blob}, "invalid character"},
		{[]string{"blob", "pins-remote", "--no-such-flag"}, "flag provided but not defined"},
		{[]string{"blob", "pins-remote", "--service", server.URL, "--key-file", keyFile, "more"}, `unexpected argument "more"`},
		{[]string{"blob", "pins-remote", "--service", server.URL}, "--service <address> and --key-file"},
		{[]string{"blob", "unpin-remote", "--no-such-flag"}, "flag provided but not defined"},
		{[]string{"blob", "unpin-remote", "--service", server.URL, "--key-file", keyFile}, "expected exactly one request ID"},
		{[]string{"blob", "unpin-remote", "--key-file", keyFile, "r1"}, "--service <address> and --key-file"},
	} {
		if _, err := cli(t, tt.args...); err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("%v: %v, want %q", tt.args, err, tt.want)
		}
	}
}

func TestPinningListenFlagFailures(t *testing.T) {
	coordinator := freeAddr(t)
	startDaemon(t, "--role", "coordinator", "--listen", coordinator)
	taken, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer taken.Close()
	spoiled := t.TempDir()
	os.Mkdir(filepath.Join(spoiled, "pinning.token"), 0o700)

	// A database whose record of a request cannot be read.
	damaged := t.TempDir()
	db, err := nodedb.Open(filepath.Join(damaged, "node.db"))
	if err != nil {
		t.Fatal(err)
	}
	db.Close()
	raw, _ := sql.Open("sqlite", "file:"+filepath.Join(damaged, "node.db"))
	if _, err := raw.Exec(`INSERT INTO pin_requests VALUES ('r1', 'bafyone', '', 'not a list', '{}', 'pinned', '', 1, '[]')`); err != nil {
		t.Fatal(err)
	}
	raw.Close()

	for _, tt := range []struct {
		name string
		args []string
		want string
	}{
		{"on a worker-only node", []string{"--role", "worker", "--coordinator", coordinator, "--data-dir", t.TempDir(), "--pinning-listen", freeAddr(t)}, "--pinning-listen is for a node that coordinates a pool"},
		{"at an address in use", []string{"--data-dir", t.TempDir(), "--listen", freeAddr(t), "--pinning-listen", taken.Addr().String()}, "address already in use"},
		{"with a key that cannot be read", []string{"--data-dir", spoiled, "--listen", freeAddr(t), "--pinning-listen", freeAddr(t)}, "read pinning token"},
		{"with requests that cannot be read", []string{"--data-dir", damaged, "--listen", freeAddr(t), "--pinning-listen", freeAddr(t)}, "load pin requests"},
	} {
		if _, err := cli(t, append([]string{"run", "--discovery", "off"}, tt.args...)...); err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("--pinning-listen %s: %v, want %q", tt.name, err, tt.want)
		}
	}
}

// ipfsAt runs the ipfs command against a Kubo repository of its own, which
// is no node's.
func ipfsAt(repo string, args ...string) (string, error) {
	cmd := exec.Command("ipfs", args...)
	cmd.Env = append(os.Environ(), "IPFS_PATH="+repo, "LIBP2P_FORCE_PNET=1")
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// startIPFS runs a Kubo daemon of the test's own, as somebody with the ipfs
// program and the pool's swarm key would, and returns its repository. It is
// no part of any node.
func startIPFS(t *testing.T, swarmKey []byte) (repo string) {
	t.Helper()
	repo = filepath.Join(t.TempDir(), "ipfs")
	if out, err := ipfsAt(repo, "init", "--empty-repo"); err != nil {
		t.Fatalf("ipfs init: %v: %s", err, out)
	}
	if err := os.WriteFile(filepath.Join(repo, "swarm.key"), swarmKey, 0o600); err != nil {
		t.Fatal(err)
	}
	_, swarmPort, _ := net.SplitHostPort(freeAddr(t))
	_, apiPort, _ := net.SplitHostPort(freeAddr(t))
	for _, setting := range [][]string{
		{"--json", "Addresses.Swarm", `["/ip4/127.0.0.1/tcp/` + swarmPort + `"]`},
		{"Addresses.API", "/ip4/127.0.0.1/tcp/" + apiPort},
		{"--json", "Addresses.Gateway", "[]"},
		{"--json", "Bootstrap", "[]"},
		{"--json", "AutoConf.Enabled", "false"},
		{"--json", "AutoTLS.Enabled", "false"},
		{"--json", "Discovery.MDNS.Enabled", "false"},
		{"Routing.Type", "dht"},
	} {
		if out, err := ipfsAt(repo, append([]string{"config"}, setting...)...); err != nil {
			t.Fatalf("ipfs config %v: %v: %s", setting, err, out)
		}
	}
	daemon := exec.Command("ipfs", "daemon")
	daemon.Env = append(os.Environ(), "IPFS_PATH="+repo, "LIBP2P_FORCE_PNET=1")
	said := &syncBuffer{}
	daemon.Stdout, daemon.Stderr = said, said
	if err := daemon.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		daemon.Process.Signal(os.Interrupt)
		daemon.Wait()
	})
	deadline := time.Now().Add(time.Minute)
	for !strings.Contains(said.String(), "Daemon is ready") {
		if time.Now().After(deadline) {
			t.Fatalf("the ipfs daemon did not start:\n%s", said.String())
		}
		time.Sleep(20 * time.Millisecond)
	}
	return repo
}

func TestTheIPFSCommandPinsToANodeAsItsRemotePinningService(t *testing.T) {
	requireKubo(t)
	mustIPFS := func(repo string, args ...string) string {
		t.Helper()
		out, err := ipfsAt(repo, args...)
		if err != nil {
			t.Fatalf("ipfs %v: %v: %s", args, err, out)
		}
		return out
	}

	// A node with Kubo, whose swarm the ipfs program's own daemon is on.
	nodeDir := t.TempDir()
	_, swarmPort, _ := net.SplitHostPort(freeAddr(t))
	addr, service, keyFile := startKeeper(t, nodeDir, "--kubo", "--swarm-port", swarmPort)
	swarmKey, err := os.ReadFile(filepath.Join(nodeDir, "swarm.key"))
	if err != nil {
		t.Fatal(err)
	}
	key, _ := os.ReadFile(keyFile)
	repo := startIPFS(t, swarmKey)
	mustIPFS(repo, "pin", "remote", "service", "add", "sisyphus", service, strings.TrimSpace(string(key)))
	if listed := mustIPFS(repo, "pin", "remote", "service", "ls", "--stat"); !strings.Contains(listed, "sisyphus "+service+" 0/0/0/0") {
		t.Errorf("the service as ipfs lists it:\n%s", listed)
	}

	// What ipfs holds and the node does not, the node's Kubo fetches from
	// it, and ipfs waits until it has.
	noise := filepath.Join(t.TempDir(), "noise")
	os.WriteFile(noise, megabyte(), 0o600)
	added := strings.TrimSpace(mustIPFS(repo, "add", "--cid-version=1", "-Q", noise))
	if added != megabyteCID {
		t.Fatalf("ipfs add gave %s, want %s", added, megabyteCID)
	}
	out := mustIPFS(repo, "pin", "remote", "add", "--service=sisyphus", "--name=noise", added)
	if !strings.Contains(out, "CID:    "+added) || !strings.Contains(out, "Name:   noise") || !strings.Contains(out, "Status: pinned") {
		t.Fatalf("ipfs pin remote add printed:\n%s", out)
	}
	fetched := filepath.Join(t.TempDir(), "fetched")
	mustCLI(t, "blob", "get", "--addr", addr, "-o", fetched, added)
	if got, _ := os.ReadFile(fetched); string(got) != string(megabyte()) {
		t.Errorf("the node holds %d bytes of what it pinned, not the megabyte asked for", len(got))
	}
	if lines := pinLines(t, addr, added); len(lines) != 1 || !strings.HasPrefix(lines[0], "pinning-service:") {
		t.Errorf("the node's pins on what it fetched: %v", lines)
	}
	if listed := mustIPFS(repo, "pin", "remote", "ls", "--service=sisyphus", "--name=noise"); !strings.Contains(listed, added+"\tpinned\tnoise") {
		t.Errorf("ipfs pin remote ls printed:\n%s", listed)
	}

	// What the node holds already it pins without ipfs having it at all.
	held := strings.TrimSpace(mustCLI(t, "blob", "put", "--addr", addr, "--no-pin", writeFile(t, "the boulder rolls")))
	if out := mustIPFS(repo, "pin", "remote", "add", "--service=sisyphus", held); !strings.Contains(out, "Status: pinned") {
		t.Errorf("ipfs pin remote add of what the node holds printed:\n%s", out)
	}
	if listed := mustIPFS(repo, "pin", "remote", "service", "ls", "--stat"); !strings.Contains(listed, "0/0/2/0") {
		t.Errorf("the service's counts as ipfs lists them:\n%s", listed)
	}
	// And what is removed through ipfs is the node's to collect.
	mustIPFS(repo, "pin", "remote", "rm", "--service=sisyphus", "--cid="+added)
	if lines := pinLines(t, addr, added); len(lines) != 0 {
		t.Errorf("the node's pins on what was removed: %v", lines)
	}
	if listed := mustIPFS(repo, "pin", "remote", "ls", "--service=sisyphus"); strings.Contains(listed, added) || !strings.Contains(listed, held+"\tpinned") {
		t.Errorf("ipfs pin remote ls after the removal printed:\n%s", listed)
	}

	// A node without Kubo serves the same commands, for what it holds, and
	// fails what it would have to fetch.
	plain, plainService, plainKeyFile := startKeeper(t, t.TempDir())
	key, _ = os.ReadFile(plainKeyFile)
	mustIPFS(repo, "pin", "remote", "service", "add", "plain", plainService, strings.TrimSpace(string(key)))
	held = strings.TrimSpace(mustCLI(t, "blob", "put", "--addr", plain, "--no-pin", writeFile(t, "kept on a disk")))
	if out := mustIPFS(repo, "pin", "remote", "add", "--service=plain", "--name=disk", held); !strings.Contains(out, "Status: pinned") {
		t.Errorf("ipfs pin remote add to a node without Kubo printed:\n%s", out)
	}
	if out, err := ipfsAt(repo, "pin", "remote", "add", "--service=plain", added); err == nil || !strings.Contains(out, "failed to pin") {
		t.Errorf("ipfs pin remote add of what a node without Kubo does not hold: %v:\n%s", err, out)
	}
	if listed := mustIPFS(repo, "pin", "remote", "ls", "--service=plain", "--status=failed,pinned"); !strings.Contains(listed, held+"\tpinned\tdisk") || !strings.Contains(listed, added+"\tfailed") {
		t.Errorf("ipfs pin remote ls of a node without Kubo printed:\n%s", listed)
	}
}

func TestANodeWithKuboHasItsDataKeptByAServiceOnThePoolsNetwork(t *testing.T) {
	requireKubo(t)
	// Two nodes, each coordinating a pool of its own, are on one private
	// network when they are given the same swarm key.
	swarmKey := kubo.NewSwarmKey()
	askerDir, keeperDir := t.TempDir(), t.TempDir()
	for _, dir := range []string{askerDir, keeperDir} {
		if err := os.WriteFile(filepath.Join(dir, "swarm.key"), []byte(swarmKey), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	_, askerPort, _ := net.SplitHostPort(freeAddr(t))
	_, keeperPort, _ := net.SplitHostPort(freeAddr(t))
	keeper, service, keyFile := startKeeper(t, keeperDir, "--kubo", "--swarm-port", keeperPort)
	addr, _ := startNode(t, "--kubo", "--swarm-port", askerPort, "--data-dir", askerDir)

	// The asker tells the service where its Kubo is, and the service's
	// Kubo fetches from there.
	source := filepath.Join(t.TempDir(), "noise")
	os.WriteFile(source, megabyte(), 0o600)
	id := strings.TrimSpace(mustCLI(t, "blob", "put", "--addr", addr, source))
	out := mustCLI(t, "blob", "pin-remote", "--addr", addr, "--service", service, "--key-file", keyFile, "--wait", id, "noise")
	lines := strings.Split(strings.TrimSpace(out), "\n")
	last := strings.Fields(lines[len(lines)-1])
	if len(last) != 2 || last[1] != "pinned" {
		t.Fatalf("blob pin-remote --wait printed %q, want it to end pinned", out)
	}
	fetched := filepath.Join(t.TempDir(), "fetched")
	mustCLI(t, "blob", "get", "--addr", keeper, "-o", fetched, id)
	if got, _ := os.ReadFile(fetched); string(got) != string(megabyte()) {
		t.Errorf("the keeper holds %d bytes of what it pinned, not the megabyte asked for", len(got))
	}
	if lines := pinLines(t, keeper, id); !has(lines, "pinning-service:"+last[0]+" released") {
		t.Errorf("the keeper's pins on what it fetched: %v", lines)
	}
	// The service was told the asker's Kubo address, and nothing else of
	// where to look.
	key, _ := os.ReadFile(keyFile)
	req, _ := http.NewRequest(http.MethodGet, service+"/pins/"+last[0], nil)
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(key)))
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var shown pinning.Status
	json.NewDecoder(res.Body).Decode(&shown)
	if len(shown.Pin.Origins) == 0 || len(shown.Delegates) == 0 {
		t.Fatalf("the request as the service shows it: %+v", shown)
	}
	for _, origin := range shown.Pin.Origins {
		if !strings.HasSuffix(origin, "/tcp/"+askerPort+"/p2p/"+nodeID(t, askerDir)) {
			t.Errorf("the service was told of %s, which is not the asker's Kubo", origin)
		}
	}
	for _, delegate := range shown.Delegates {
		if !strings.HasSuffix(delegate, "/tcp/"+keeperPort+"/p2p/"+nodeID(t, keeperDir)) {
			t.Errorf("the service gives %s as where to bring data, which is not its Kubo", delegate)
		}
	}
}

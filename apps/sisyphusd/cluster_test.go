package main

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/sisyphus-network/Sisyphus/packages/ipfscluster"
)

// requireCluster skips a test that needs the real ipfs-cluster-service and
// ipfs programs when either is not installed, or fails it if
// SISYPHUS_REQUIRE_CLUSTER or SISYPHUS_REQUIRE_KUBO says it must be there.
func requireCluster(t *testing.T) {
	t.Helper()
	requireKubo(t)
	if _, err := exec.LookPath("ipfs-cluster-service"); err != nil {
		if os.Getenv("SISYPHUS_REQUIRE_CLUSTER") != "" {
			t.Fatal("ipfs-cluster-service is not installed, and SISYPHUS_REQUIRE_CLUSTER is set")
		}
		t.Skip("ipfs-cluster-service is not installed")
	}
}

// quickCluster makes the members of the clusters a test starts tell each
// other of themselves every second, and coordinators compare their pins
// with their cluster's as often, so that a test need not wait as long as a
// real pool does.
func quickCluster(t *testing.T) {
	t.Helper()
	heartbeat, sync := clusterHeartbeat, clusterSync
	clusterHeartbeat, clusterSync = time.Second, time.Second
	t.Cleanup(func() { clusterHeartbeat, clusterSync = heartbeat, sync })
}

// clusterPeerOf returns a client for the cluster peer of the node whose
// data is in dataDir, as a program on its machine that had read the peer's
// settings would have.
func clusterPeerOf(t *testing.T, dataDir string) *ipfscluster.Client {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dataDir, "ipfs-cluster", "service.json"))
	if err != nil {
		t.Fatal(err)
	}
	var settings struct {
		API struct {
			REST struct {
				Listen   string            `json:"http_listen_multiaddress"`
				Accounts map[string]string `json:"basic_auth_credentials"`
			} `json:"restapi"`
		} `json:"api"`
	}
	if err := json.Unmarshal(data, &settings); err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(settings.API.REST.Listen, "/")
	return ipfscluster.NewClient(parts[2]+":"+parts[4], settings.API.REST.Accounts["sisyphusd"])
}

// copiesOf waits until the pool's cluster reports want for a pin, such as
// "2/2", and returns the report.
func copiesOf(t *testing.T, addr, id, want string) string {
	t.Helper()
	line := regexp.MustCompile(`(?m)^` + id + ` +` + want + ` .*$`)
	var out string
	waitFor(t, func() bool {
		out, _ = cli(t, "pool", "cluster", "--addr", addr)
		return line.MatchString(out)
	})
	return line.FindString(out)
}

func TestAPoolWithAClusterKeepsWhatItsCoordinatorPinsOnSeveralNodes(t *testing.T) {
	requireCluster(t)
	quickCluster(t)
	addr := freeAddr(t)
	coordinatorDir := t.TempDir()
	dirs := map[string]string{"north": t.TempDir(), "south": t.TempDir()}
	stopCoordinator := startDaemon(t, "--role", "coordinator", "--kubo", "--cluster", "--listen", addr, "--data-dir", coordinatorDir, "--name", "hub")
	for name, dir := range dirs {
		startDaemon(t, "--role", "worker", "--kubo", "--cluster", "--coordinator", addr, "--data-dir", dir, "--name", name, "--slots", "1")
	}
	// The three cluster peers find each other, each as the node it serves.
	// The workers have no way to the coordinator's but through its port.
	waitForOutput(t, "south", "pool", "cluster", "--addr", addr)
	members := waitForOutput(t, "north", "pool", "cluster", "--addr", addr)
	for _, dir := range []string{coordinatorDir, dirs["north"], dirs["south"]} {
		if !strings.Contains(members, nodeID(t, dir)) {
			t.Errorf("node %s is not among the cluster's members:\n%s", nodeID(t, dir), members)
		}
	}
	if !strings.Contains(members, "each pin is to be held by 2 of them\nnothing is pinned\n") {
		t.Errorf("the cluster's state before anything is stored:\n%s", members)
	}

	// What the coordinator pins is held by two nodes: the coordinator, which
	// has it anyway, and one worker, which was sent nothing by the node and
	// fetched it over the pool's private network.
	text := strings.Repeat("the boulder rolls down, and Sisyphus walks after it.\n", 20_000)
	id := strings.TrimSpace(mustCLI(t, "blob", "put", "--addr", addr, writeFile(t, text)))
	held := copiesOf(t, addr, id, "2/2")
	if !strings.Contains(held, "hub") {
		t.Errorf("the coordinator is not one of the holders: %s", held)
	}
	holder, spare := "north", "south"
	if strings.Contains(held, "south") {
		holder, spare = "south", "north"
	}
	if got := ipfsIn(t, dirs[holder], "cat", "--offline", id); got != text {
		t.Errorf("the worker said to hold the file has %d bytes of it, want all %d", len(got), len(text))
	}

	// A worker's peer follows: it can change nothing, for itself or the pool.
	follower := clusterPeerOf(t, dirs[spare])
	ctx := context.Background()
	if _, err := follower.Pin(ctx, id, ipfscluster.PinOptions{Min: 3, Max: 3}); err == nil || !strings.Contains(err.Error(), "follower") {
		t.Errorf("a worker's cluster peer pinning: %v, want a refusal", err)
	}
	if err := follower.Unpin(ctx, id); err == nil || !strings.Contains(err.Error(), "follower") {
		t.Errorf("a worker's cluster peer unpinning: %v, want a refusal", err)
	}

	// The store's pins are the cluster's. A pin taken out of the cluster
	// behind the node's back is put back, and one put in under the store's
	// name is taken out. One put in by hand under another name is left.
	hub := clusterPeerOf(t, coordinatorDir)
	stray := strings.TrimSpace(mustCLI(t, "blob", "put", "--addr", addr, "--no-pin", writeFile(t, "stored, and pinned by nobody")))
	byHand := strings.TrimSpace(mustCLI(t, "blob", "put", "--addr", addr, "--no-pin", writeFile(t, "pinned on the cluster by its owner")))
	if err := hub.Unpin(ctx, id); err != nil {
		t.Fatal(err)
	}
	if _, err := hub.Pin(ctx, stray, ipfscluster.PinOptions{Name: clusterPinName, Min: 1, Max: 1}); err != nil {
		t.Fatal(err)
	}
	if _, err := hub.Pin(ctx, byHand, ipfscluster.PinOptions{Name: "mine", Min: 1, Max: 1}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool {
		pins, err := hub.Pins(ctx)
		var names []string
		for _, pin := range pins {
			names = append(names, pin.CID+" "+pin.Name)
		}
		return err == nil && len(pins) == 2 && strings.Contains(strings.Join(names, "\n"), id+" "+clusterPinName) && strings.Contains(strings.Join(names, "\n"), byHand+" mine")
	})
	copiesOf(t, addr, id, "2/2")

	// A restarted coordinator finds its cluster again, and the workers it.
	stopCoordinator()
	startDaemon(t, "--role", "coordinator", "--kubo", "--cluster", "--listen", addr, "--data-dir", coordinatorDir, "--name", "hub")
	waitForOutput(t, "north", "pool", "cluster", "--addr", addr)
	waitForOutput(t, "south", "pool", "cluster", "--addr", addr)
	copiesOf(t, addr, id, "2/2")

	// Once nothing pins it on the node, nothing pins it on the cluster.
	mustCLI(t, "blob", "unpin", "--addr", addr, id)
	waitForOutput(t, "nothing is pinned", "pool", "cluster", "--addr", addr)
	waitFor(t, func() bool { return !strings.Contains(ipfsIn(t, dirs[holder], "pin", "ls", "--type=recursive"), id) })
}

// secretOf returns the secret the cluster peer of the node in dataDir was
// last started with.
func secretOf(dataDir string) string {
	data, _ := os.ReadFile(filepath.Join(dataDir, "ipfs-cluster", "service.json"))
	var settings struct {
		Cluster struct {
			Secret string `json:"secret"`
		} `json:"cluster"`
	}
	json.Unmarshal(data, &settings)
	return settings.Cluster.Secret
}

// The cluster's secret goes with the key of the pool's private network, so
// that a node shut out of the one is shut out of the other.
func TestAPoolsClusterFollowsItsNetworkToANewKey(t *testing.T) {
	requireCluster(t)
	quickCluster(t)
	addr := freeAddr(t)
	coordinatorDir, workerDir := t.TempDir(), t.TempDir()
	startDaemon(t, "--role", "coordinator", "--kubo", "--cluster", "--listen", addr, "--data-dir", coordinatorDir, "--name", "hub")
	startDaemon(t, "--role", "worker", "--kubo", "--cluster", "--coordinator", addr, "--data-dir", workerDir, "--name", "hand", "--slots", "1")
	waitForOutput(t, "hand", "pool", "cluster", "--addr", addr)
	before := secretOf(coordinatorDir)
	if key, _ := os.ReadFile(filepath.Join(coordinatorDir, "swarm.key")); before != clusterSecret(string(key)) || secretOf(workerDir) != before {
		t.Fatalf("the cluster's secret is %q on the coordinator and %q on the worker, want the one derived from the swarm key", before, secretOf(workerDir))
	}

	mustCLI(t, "pool", "rekey", "--addr", addr)
	key, _ := os.ReadFile(filepath.Join(coordinatorDir, "swarm.key"))
	after := clusterSecret(string(key))
	if after == before || secretOf(coordinatorDir) != after {
		t.Fatalf("after a change of key the coordinator's cluster peer has secret %q, want %q", secretOf(coordinatorDir), after)
	}
	waitFor(t, func() bool { return secretOf(workerDir) == after })

	// The two are one cluster again, and it still does its work.
	waitForOutput(t, "hand", "pool", "cluster", "--addr", addr)
	id := strings.TrimSpace(mustCLI(t, "blob", "put", "--addr", addr, writeFile(t, "stored after the change of key")))
	if held := copiesOf(t, addr, id, "2/2"); !strings.Contains(held, "hand") {
		t.Errorf("after the change of key the pin is held by: %s", held)
	}
}

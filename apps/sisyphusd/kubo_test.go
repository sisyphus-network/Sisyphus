package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
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

	_, err = cli(t, "run", "--kubo", "--role", "worker", "--coordinator", "127.0.0.1:1", "--data-dir", t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "--kubo is, for now, only for nodes with the coordinator role") {
		t.Errorf("--kubo on a worker-only node: %v", err)
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

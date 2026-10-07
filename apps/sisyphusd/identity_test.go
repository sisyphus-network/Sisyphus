package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/excho0/Sisyphus/packages/identity"
)

func TestIDCommandPrintsAStableNodeID(t *testing.T) {
	dataDir := filepath.Join(t.TempDir(), "node") // not yet created
	first := strings.TrimSpace(mustCLI(t, "id", "--data-dir", dataDir))
	if !strings.HasPrefix(first, "12D3KooW") {
		t.Errorf("printed %q, want a node ID", first)
	}
	if again := strings.TrimSpace(mustCLI(t, "id", "--data-dir", dataDir)); again != first {
		t.Errorf("the ID changed from %s to %s between calls", first, again)
	}
	if other := strings.TrimSpace(mustCLI(t, "id", "--data-dir", t.TempDir())); other == first {
		t.Errorf("two data directories share the ID %s", first)
	}
}

func TestIDCommandUsage(t *testing.T) {
	for _, tt := range []struct {
		args []string
		want string
	}{
		{[]string{"id", "extra"}, `unexpected argument "extra"`},
		{[]string{"id", "--no-such-flag"}, "flag provided but not defined"},
	} {
		if _, err := cli(t, tt.args...); err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("%v: error %v, want one containing %q", tt.args, err, tt.want)
		}
	}
}

func TestDaemonUsesTheKeyInItsDataDirectory(t *testing.T) {
	dataDir := t.TempDir()
	startNode(t, "--data-dir", dataDir)

	// The running node created the key; the id command now reads the same one.
	ident, created, err := identity.LoadOrCreate(filepath.Join(dataDir, "node.key"))
	if err != nil {
		t.Fatal(err)
	}
	if created {
		t.Fatal("a running node had not created its key")
	}
	if got := strings.TrimSpace(mustCLI(t, "id", "--data-dir", dataDir)); got != ident.ID() {
		t.Errorf("id printed %s, the node's key gives %s", got, ident.ID())
	}
}

func TestNodeWillNotStartWithoutAUsableKey(t *testing.T) {
	// The data directory's path runs through a regular file.
	file := filepath.Join(t.TempDir(), "a-file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := cli(t, "run", "--data-dir", filepath.Join(file, "data")); err == nil {
		t.Error("started with a data directory that cannot be created")
	}

	// The key file holds something that is not a key.
	dataDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dataDir, "node.key"), []byte("not a key"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, command := range [][]string{{"run", "--listen", freeAddr(t)}, {"id"}} {
		_, err := cli(t, append(command, "--data-dir", dataDir)...)
		if err == nil || !strings.Contains(err.Error(), "is not PEM") {
			t.Errorf("%v: error %v, want the bad key reported", command[0], err)
		}
	}
}

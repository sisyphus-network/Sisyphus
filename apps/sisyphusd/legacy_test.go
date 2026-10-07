package main

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sisyphus-network/Sisyphus/apps/sisyphusd/access"
	"github.com/sisyphus-network/Sisyphus/packages/identity"
	"github.com/sisyphus-network/Sisyphus/packages/nodedb"
	nodepb "github.com/sisyphus-network/Sisyphus/packages/protocol/sisyphus/node/v1"
)

// What the Rust daemon said of itself when it made the database these tests
// use, and what it was then told: one bootstrap peer on its command line,
// and one node to trust, through its own API.
const (
	rustNodeID    = "12D3KooWPXF17Xj3HJkmBoTLPEm7N2fyh2CGbce5iV8XMVKrobRV"
	rustBootstrap = "/ip4/10.0.0.9/tcp/7400/p2p/12D3KooWAMv5mPojCf7tz1PH9F3VCPzqCyc8t2onRa6ihxoPh7TK"
	rustTrusted   = "12D3KooWGMC9eNSqjbuxQN5gg7emLLsXsBxYAanwqcmCUALXtquU"
)

// afterRust returns a data directory as the Rust daemon left it.
func afterRust(t *testing.T) string {
	t.Helper()
	dataDir := t.TempDir()
	source, err := os.Open(filepath.Join("..", "..", "packages", "nodedb", "testdata", "rust-node.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	copied, err := os.OpenFile(rustDatabase(dataDir), os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer copied.Close()
	if _, err := io.Copy(copied, source); err != nil {
		t.Fatal(err)
	}
	return dataDir
}

func TestANodeThatRanTheRustDaemonCarriesOnAsItself(t *testing.T) {
	dataDir := afterRust(t)
	// Any command that needs the node's key finds the one it already had.
	if got := nodeID(t, dataDir); got != rustNodeID {
		t.Fatalf("the node's ID is now %s, and under the Rust daemon it was %s", got, rustNodeID)
	}
	if info, err := os.Stat(filepath.Join(dataDir, "node.key")); err != nil || info.Mode().Perm() != 0o600 {
		t.Errorf("the adopted key is stored as %v, %v", info, err)
	}

	addr, apiAddr := freeAddr(t), freeAddr(t)
	stop := startDaemon(t, "--data-dir", dataDir, "--listen", addr, "--slots", "1", "--api-listen", apiAddr)
	client := desktop(t, apiAddr)
	poolAsSeenBy(t, client)
	info, err := client.GetNodeInfo(context.Background(), &nodepb.GetNodeInfoRequest{})
	if err != nil || info.GetPeerId() != rustNodeID {
		t.Errorf("node info %v, %v", info, err)
	}
	// Its address book and the node it trusted came with it.
	book, err := client.GetBootstrapPeers(context.Background(), &nodepb.GetBootstrapPeersRequest{})
	if err != nil || len(book.GetPeers()) != 1 || book.GetPeers()[0].GetAddress() != rustBootstrap {
		t.Errorf("address book %v, %v", book, err)
	}
	// That daemon's trust had one side, and here it is both.
	if trusted := peerSeenBy(t, client, rustTrusted); !trusted.GetTrustedForCompute() || !trusted.GetGivesWork() || !trusted.GetTakesWork() {
		t.Errorf("the node the Rust daemon trusted is listed as %v", trusted)
	}
	if members := mustCLI(t, "pool", "members", "--addr", addr); !strings.Contains(members, rustTrusted) {
		t.Errorf("pool members:\n%s", members)
	}
	stop()

	// The old database is set aside, so that a node un-trusted later does
	// not come back at the next start.
	if _, err := os.Stat(rustDatabase(dataDir)); !os.IsNotExist(err) {
		t.Errorf("the Rust daemon's database is still in place: %v", err)
	}
	if _, err := os.Stat(rustDatabase(dataDir) + ".imported"); err != nil {
		t.Errorf("the Rust daemon's database was not kept: %v", err)
	}
	addr = freeAddr(t)
	startDaemon(t, "--data-dir", dataDir, "--listen", addr, "--slots", "1")
	waitForOutput(t, "primes", "nodes", "--addr", addr)
	if got := nodeID(t, dataDir); got != rustNodeID {
		t.Errorf("after a second start the node's ID is %s", got)
	}
}

func TestANodeWithAKeyOfItsOwnKeepsIt(t *testing.T) {
	dataDir := t.TempDir()
	own := nodeID(t, dataDir)
	// A Rust database turning up beside it later changes nothing.
	source := afterRust(t)
	if err := os.Rename(rustDatabase(source), rustDatabase(dataDir)); err != nil {
		t.Fatal(err)
	}
	if got := nodeID(t, dataDir); got != own {
		t.Errorf("the node's ID changed from %s to %s", own, got)
	}
}

func TestARustDatabaseThatCannotBeReadStopsTheNodeRatherThanRenamingIt(t *testing.T) {
	dataDir := t.TempDir()
	if err := os.WriteFile(rustDatabase(dataDir), []byte(strings.Repeat("not a database ", 100)), 0o600); err != nil {
		t.Fatal(err)
	}
	// Making a new key here would give the node a new ID without a word.
	if _, err := cli(t, "id", "--data-dir", dataDir); err == nil || !strings.Contains(err.Error(), "Rust daemon's database") {
		t.Errorf("error %v, want the database reported", err)
	}
	if _, err := os.Stat(filepath.Join(dataDir, "node.key")); !os.IsNotExist(err) {
		t.Error("a new key was made for a node whose old one could not be read")
	}

	// One that goes bad between the key being adopted and the rest carried
	// over stops the node too.
	dataDir = afterRust(t)
	nodeID(t, dataDir)
	raw, err := sql.Open("sqlite", rustDatabase(dataDir))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`DELETE FROM node_identity`); err != nil {
		t.Fatal(err)
	}
	raw.Close()
	if _, err := cli(t, "run", "--data-dir", dataDir, "--listen", freeAddr(t)); err == nil || !strings.Contains(err.Error(), "node identity") {
		t.Errorf("error %v, want the database reported", err)
	}
}

// refusing takes nothing that is carried over to it.
type refusing struct{}

var errRefused = errors.New("the database is locked")

func (refusing) AddBootstrapPeer(nodedb.BootstrapPeer) error { return errRefused }
func (refusing) Admit(string, access.Role, time.Time) error  { return errRefused }
func (refusing) Set(string, bool) error                      { return errRefused }

func TestStateThatCannotBeCarriedOverIsLeftForNextTime(t *testing.T) {
	dataDir := afterRust(t)
	if err := importRustState(dataDir, refusing{}, refusing{}, refusing{}, quiet); !errors.Is(err, errRefused) {
		t.Errorf("error %v, want the refusal", err)
	}
	if _, err := os.Stat(rustDatabase(dataDir)); err != nil {
		t.Errorf("the database was set aside though nothing was carried over: %v", err)
	}
	// A key that is not a node's key is not adopted.
	if err := identity.Adopt(filepath.Join(t.TempDir(), "node.key"), []byte("not a key")); err == nil {
		t.Error("something that is no key was adopted")
	}
}

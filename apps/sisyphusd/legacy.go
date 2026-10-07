package main

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/sisyphus-network/Sisyphus/apps/sisyphusd/access"
	"github.com/sisyphus-network/Sisyphus/packages/identity"
	"github.com/sisyphus-network/Sisyphus/packages/nodedb"
)

// Before this daemon there was one written in Rust, which kept everything a
// node had in one database, node.sqlite3, in the same data directory. A
// node that ran it carries on here as itself: with the same key, and so the
// same ID, the same address book, and the same nodes trusted for compute.

// rustDatabase is where the Rust daemon kept a node's state.
func rustDatabase(dataDir string) string {
	return filepath.Join(dataDir, "node.sqlite3")
}

// adoptRustKey gives a node that has no key of its own yet the key the Rust
// daemon made for it, if there is one. Every command that needs the node's
// key does this first, so that none of them makes a new one in its place.
func adoptRustKey(dataDir string) error {
	keyFile := filepath.Join(dataDir, "node.key")
	if _, err := os.Stat(keyFile); err == nil {
		return nil
	}
	if _, err := os.Stat(rustDatabase(dataDir)); err != nil {
		return nil
	}
	old, err := nodedb.ReadRustNode(rustDatabase(dataDir))
	if err != nil {
		return err
	}
	return identity.Adopt(keyFile, old.Key)
}

// rustBook and rustTrust are the parts of a node's state the Rust daemon's
// are carried over into.
type rustBook interface {
	AddBootstrapPeer(nodedb.BootstrapPeer) error
}

type rustTrust interface {
	Admit(id string, role access.Role, now time.Time) error
}

// importRustState carries the Rust daemon's address book and trusted nodes
// over, and sets its database aside so that this is done once. It is done
// by a node that runs a pool, since trusting a node is admitting it to one;
// on any other the database is left where it is for the day the node does.
func importRustState(dataDir string, book rustBook, trust rustTrust, log *slog.Logger) error {
	file := rustDatabase(dataDir)
	if _, err := os.Stat(file); err != nil {
		return nil
	}
	old, err := nodedb.ReadRustNode(file)
	if err != nil {
		return err
	}
	for _, entry := range old.Bootstrap {
		err = errors.Join(err, book.AddBootstrapPeer(entry))
	}
	for _, id := range old.Trusted {
		err = errors.Join(err, trust.Admit(id, access.Worker, time.Now()))
	}
	if err != nil {
		return fmt.Errorf("carry over the Rust daemon's state: %w", err)
	}
	// Its write-ahead log goes with it, or the next start would find a
	// database with none of its changes.
	for _, suffix := range []string{"", "-wal", "-shm"} {
		os.Rename(file+suffix, file+".imported"+suffix)
	}
	log.Info("carried over the state of the Rust daemon", "bootstrap_peers", len(old.Bootstrap), "trusted", len(old.Trusted), "set_aside", file+".imported")
	return nil
}

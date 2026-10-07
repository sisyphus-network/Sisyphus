package nodedb

import (
	"database/sql"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// rustDatabase returns a copy of a database made by the Rust daemon itself:
// run once with one bootstrap peer on its command line, and told through
// its own API to trust one node.
func rustDatabase(t *testing.T) string {
	t.Helper()
	source, err := os.Open(filepath.Join("testdata", "rust-node.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	file := filepath.Join(t.TempDir(), "node.sqlite3")
	copied, err := os.OpenFile(file, os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer copied.Close()
	if _, err := io.Copy(copied, source); err != nil {
		t.Fatal(err)
	}
	return file
}

// edit runs statements on a database as nothing in this package would.
func edit(t *testing.T, file string, statements ...string) {
	t.Helper()
	raw, err := sql.Open("sqlite", file)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	for _, statement := range statements {
		if _, err := raw.Exec(statement); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}
}

func TestReadingWhatTheRustDaemonKept(t *testing.T) {
	node, err := ReadRustNode(rustDatabase(t))
	if err != nil {
		t.Fatal(err)
	}
	// An Ed25519 key in libp2p's encoding is 68 bytes.
	if len(node.Key) != 68 {
		t.Errorf("the key is %d bytes", len(node.Key))
	}
	wantBook := []BootstrapPeer{{
		PeerID:  "12D3KooWAMv5mPojCf7tz1PH9F3VCPzqCyc8t2onRa6ihxoPh7TK",
		Address: "/ip4/10.0.0.9/tcp/7400/p2p/12D3KooWAMv5mPojCf7tz1PH9F3VCPzqCyc8t2onRa6ihxoPh7TK",
	}}
	if !reflect.DeepEqual(node.Bootstrap, wantBook) {
		t.Errorf("address book %v, want %v", node.Bootstrap, wantBook)
	}
	if !reflect.DeepEqual(node.Trusted, []string{"12D3KooWGMC9eNSqjbuxQN5gg7emLLsXsBxYAanwqcmCUALXtquU"}) {
		t.Errorf("trusted nodes %v", node.Trusted)
	}
}

func TestReadingARustDatabaseFromBeforeItHadAnAddressBook(t *testing.T) {
	file := rustDatabase(t)
	edit(t, file, `DROP TABLE bootstrap_peers`, `DROP TABLE trusted_compute_peers`)
	node, err := ReadRustNode(file)
	if err != nil || len(node.Key) != 68 || len(node.Bootstrap) != 0 || len(node.Trusted) != 0 {
		t.Errorf("read %+v, %v; want the key and nothing else", node, err)
	}
}

func TestRustDatabasesThatCannotBeRead(t *testing.T) {
	for name, tt := range map[string]struct {
		damage []string
		want   string
	}{
		"no key in it":              {[]string{`DELETE FROM node_identity`}, "node identity"},
		"a key in a later encoding": {[]string{`UPDATE node_identity SET encoding_version = 2`}, "encoding 2"},
		"a damaged address book": {[]string{`DROP TABLE bootstrap_peers`, `CREATE TABLE bootstrap_peers (peer_id, address)`,
			`INSERT INTO bootstrap_peers DEFAULT VALUES`}, "address book"},
		"a damaged list of trusted nodes": {[]string{`DROP TABLE trusted_compute_peers`, `CREATE TABLE trusted_compute_peers (peer_id)`,
			`INSERT INTO trusted_compute_peers DEFAULT VALUES`}, "trusted nodes"},
	} {
		file := rustDatabase(t)
		edit(t, file, tt.damage...)
		if _, err := ReadRustNode(file); err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("%s: %v, want an error containing %q", name, err, tt.want)
		}
	}
	if _, err := ReadRustNode(filepath.Join(t.TempDir(), "not-there.sqlite3")); err == nil {
		t.Error("a database that is not there was read")
	}
}

package nodedb

import (
	"database/sql"
	"fmt"
	"net/url"
)

// RustNode is what the Rust daemon, which came before this one, kept in its
// database: the node's key, its address book, and the nodes it trusted for
// compute.
type RustNode struct {
	// Key is the node's private key in libp2p's encoding.
	Key       []byte
	Bootstrap []BootstrapPeer
	Trusted   []string
}

// ReadRustNode reads the database the Rust daemon kept in file. It changes
// nothing there.
func ReadRustNode(file string) (*RustNode, error) {
	handle, _ := sql.Open("sqlite", "file:"+url.PathEscape(file)+"?_pragma=busy_timeout(5000)") // only an unknown driver fails here
	defer handle.Close()
	old := &DB{sql: handle}
	node := new(RustNode)

	var version int
	if err := handle.QueryRow(`SELECT encoding_version, private_key FROM node_identity WHERE singleton_id = 1`).Scan(&version, &node.Key); err != nil {
		return nil, fmt.Errorf("read the Rust daemon's database %s: node identity: %w", file, err)
	}
	if version != 1 {
		return nil, fmt.Errorf("the Rust daemon's database %s keeps its key in encoding %d, which this version cannot read", file, version)
	}
	// The address book and the trusted nodes came in later versions of that
	// daemon, so a database may have neither table.
	tables := make(map[string]bool)
	old.read(func(rows *sql.Rows) error {
		var name string
		err := rows.Scan(&name)
		tables[name] = true
		return err
	}, `SELECT name FROM sqlite_master WHERE type = 'table'`)

	if tables["bootstrap_peers"] {
		err := old.read(func(rows *sql.Rows) error {
			var p BootstrapPeer
			err := rows.Scan(&p.PeerID, &p.Address)
			node.Bootstrap = append(node.Bootstrap, p)
			return err
		}, `SELECT peer_id, address FROM bootstrap_peers ORDER BY peer_id, address`)
		if err != nil {
			return nil, fmt.Errorf("read the Rust daemon's database %s: address book: %w", file, err)
		}
	}
	if tables["trusted_compute_peers"] {
		err := old.read(func(rows *sql.Rows) error {
			var id string
			err := rows.Scan(&id)
			node.Trusted = append(node.Trusted, id)
			return err
		}, `SELECT peer_id FROM trusted_compute_peers ORDER BY peer_id`)
		if err != nil {
			return nil, fmt.Errorf("read the Rust daemon's database %s: trusted nodes: %w", file, err)
		}
	}
	return node, nil
}

package nodedb

import (
	"database/sql"
	"fmt"
)

// BootstrapPeer is an entry in a node's address book: a node, and an
// address to find it at, as a multiaddress without the node's ID.
type BootstrapPeer struct {
	PeerID  string
	Address string
}

// BootstrapPeers returns the address book, ordered by node and address.
func (db *DB) BootstrapPeers() ([]BootstrapPeer, error) {
	var peers []BootstrapPeer
	err := db.read(func(rows *sql.Rows) error {
		var p BootstrapPeer
		if err := rows.Scan(&p.PeerID, &p.Address); err != nil {
			return err
		}
		peers = append(peers, p)
		return nil
	}, `SELECT peer_id, address FROM bootstrap_peers ORDER BY peer_id, address`)
	if err != nil {
		return nil, fmt.Errorf("load bootstrap peers: %w", err)
	}
	return peers, nil
}

// SetBootstrapPeers replaces the address book with the given entries.
func (db *DB) SetBootstrapPeers(peers []BootstrapPeer) error {
	return db.durably("save bootstrap peers", func(b *batch) {
		b.exec(`DELETE FROM bootstrap_peers`)
		for _, p := range peers {
			b.exec(`INSERT OR IGNORE INTO bootstrap_peers (peer_id, address) VALUES (?, ?)`, p.PeerID, p.Address)
		}
	})
}

// AddBootstrapPeer adds an entry to the address book, if it is not there
// already.
func (db *DB) AddBootstrapPeer(p BootstrapPeer) error {
	return db.durably("save bootstrap peer", func(b *batch) {
		b.exec(`INSERT OR IGNORE INTO bootstrap_peers (peer_id, address) VALUES (?, ?)`, p.PeerID, p.Address)
	})
}

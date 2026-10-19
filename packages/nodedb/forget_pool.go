package nodedb

// ForgetPool forgets what this node kept about the pool the coordinator with
// the given ID runs, once it has left it (#509): which files it stored there,
// who on the node held them, which jobs it submitted, where it found the
// coordinator, and that it works for it. It is one transaction, so a node
// that fails partway still has all of it, and forgetting again is the retry.
//
// A file a chat still attaches stays in the node's records, though no pool's
// list shows it, so that the chat can still say what it attached (#518).
// Forgetting a pool never joined is no error.
func (db *DB) ForgetPool(coordinatorID string) error {
	return db.durably("forget the pool", func(b *batch) {
		b.exec(`DELETE FROM pool_file_holds WHERE coordinator_id = ?`, coordinatorID)
		b.exec(`DELETE FROM file_pool_scopes WHERE coordinator_id = ?`, coordinatorID)
		b.exec(`DELETE FROM files WHERE NOT EXISTS (SELECT 1 FROM file_pool_scopes s WHERE s.cid = files.cid)
			AND NOT EXISTS (SELECT 1 FROM file_references r WHERE r.cid = files.cid)`)
		b.exec(`DELETE FROM pool_jobs WHERE coordinator_id = ?`, coordinatorID)
		b.exec(`DELETE FROM bootstrap_peers WHERE peer_id = ?`, coordinatorID)
		b.exec(`DELETE FROM works_for WHERE node_id = ?`, coordinatorID)
	})
}

package nodedb

import (
	"database/sql"
	"fmt"
	"time"
)

// WorksFor returns the IDs of the nodes this one is willing to take tasks
// from, in order.
func (db *DB) WorksFor() ([]string, error) {
	var ids []string
	err := db.read(func(rows *sql.Rows) error {
		var id string
		err := rows.Scan(&id)
		ids = append(ids, id)
		return err
	}, `SELECT node_id FROM works_for ORDER BY node_id`)
	if err != nil {
		return nil, fmt.Errorf("load the nodes worked for: %w", err)
	}
	return ids, nil
}

// SetWorksFor records whether this node is willing to take tasks from the
// node with the given ID, which is synced to disk before it returns.
func (db *DB) SetWorksFor(id string, willing bool, now time.Time) error {
	return db.durably("save a node worked for", func(b *batch) {
		if willing {
			b.exec(`INSERT OR IGNORE INTO works_for (node_id, since_ns) VALUES (?, ?)`, id, nanos(now))
		} else {
			b.exec(`DELETE FROM works_for WHERE node_id = ?`, id)
		}
	})
}

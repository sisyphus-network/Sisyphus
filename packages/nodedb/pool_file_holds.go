package nodedb

import (
	"database/sql"
	"fmt"
	"time"
)

// PoolFileHolds returns, for each owner on this node that holds a file
// kept in the pool the coordinator with the given ID runs, when its hold
// ends, the zero time for one that does not. Those already over at now are
// left out.
func (db *DB) PoolFileHolds(coordinatorID, cid string, now time.Time) (map[string]time.Time, error) {
	holds := make(map[string]time.Time)
	err := db.read(func(rows *sql.Rows) error {
		var owner string
		var expires int64
		err := rows.Scan(&owner, &expires)
		holds[owner] = moment(expires)
		return err
	}, `SELECT owner, expires_ns FROM pool_file_holds WHERE coordinator_id = ? AND cid = ? AND (expires_ns = 0 OR expires_ns > ?)`, coordinatorID, cid, nanos(now))
	if err != nil {
		return nil, fmt.Errorf("load the holds on a file kept in the pool: %w", err)
	}
	return holds, nil
}

// HoldPoolFile records that an owner on this node holds a file kept in the
// pool the coordinator with the given ID runs, until expires, or for good
// if it is the zero time; holding it again replaces the earlier expiry.
// The holds already over at now are forgotten.
func (db *DB) HoldPoolFile(coordinatorID, cid, owner string, expires, now time.Time) error {
	return db.durably("save a hold on a file kept in the pool", func(b *batch) {
		b.exec(`DELETE FROM pool_file_holds WHERE expires_ns != 0 AND expires_ns <= ?`, nanos(now))
		b.exec(`INSERT OR REPLACE INTO pool_file_holds (coordinator_id, cid, owner, expires_ns) VALUES (?, ?, ?, ?)`, coordinatorID, cid, owner, nanos(expires))
	})
}

// ReleasePoolFile forgets an owner's hold on a file kept in the pool the
// coordinator with the given ID runs.
func (db *DB) ReleasePoolFile(coordinatorID, cid, owner string) error {
	return db.durably("release a hold on a file kept in the pool", func(b *batch) {
		b.exec(`DELETE FROM pool_file_holds WHERE coordinator_id = ? AND cid = ? AND owner = ?`, coordinatorID, cid, owner)
	})
}

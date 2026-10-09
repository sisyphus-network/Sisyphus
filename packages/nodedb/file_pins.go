package nodedb

import (
	"database/sql"
	"fmt"
)

// FilePinOperation is an idempotent storage change committed with ownership.
// Process globally in Seq order, with one consumer, and acknowledge only
// after Pin/Unpin succeeds. Replaying after a crash is safe.
type FilePinOperation struct {
	Seq        int64
	Owner, CID string
	Keep       bool
}

// PendingFilePins returns a bounded oldest-first batch. Do not skip a failed
// operation: later releases must not overtake an earlier retain.
func (db *DB) PendingFilePins() ([]FilePinOperation, error) {
	ops := []FilePinOperation{}
	err := db.read(func(rows *sql.Rows) error {
		var op FilePinOperation
		if err := rows.Scan(&op.Seq, &op.Owner, &op.CID, &op.Keep); err != nil {
			return err
		}
		ops = append(ops, op)
		return nil
	}, `SELECT seq, owner, cid, keep FROM file_pin_operations ORDER BY seq LIMIT 128`)
	if err != nil {
		return nil, fmt.Errorf("load file pin operations: %w", err)
	}
	return ops, nil
}

// CompleteFilePin forgets only the applied operation. Repeating an
// acknowledgement is harmless; pending changes remain durable.
func (db *DB) CompleteFilePin(seq int64) error {
	return db.durably("complete file pin operation", func(b *batch) {
		b.exec(`DELETE FROM file_pin_operations WHERE seq = ?`, seq)
	})
}

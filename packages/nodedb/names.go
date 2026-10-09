package nodedb

import (
	"database/sql"
	"fmt"
	"time"
)

// NameRecord returns the signed record kept for a name, or nil if there is
// none.
func (db *DB) NameRecord(name string) ([]byte, error) {
	var record []byte
	err := db.read(func(rows *sql.Rows) error {
		return rows.Scan(&record)
	}, `SELECT record FROM names WHERE name = ?`, name)
	if err != nil {
		return nil, fmt.Errorf("load the record of a name: %w", err)
	}
	return record, nil
}

// SetNameRecord keeps a signed record as the latest for a name, in place of
// any kept before. It is synced to disk before it returns.
func (db *DB) SetNameRecord(name string, record []byte, now time.Time) error {
	return db.durably("save the record of a name", func(b *batch) {
		b.exec(`INSERT OR REPLACE INTO names (name, record, received_ns) VALUES (?, ?, ?)`, name, record, nanos(now))
	})
}

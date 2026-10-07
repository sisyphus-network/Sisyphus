package nodedb

import (
	"database/sql"
	"fmt"
	"time"
)

// File is a file the node's owner has stored: what it is called, and the
// CID its content is stored under.
type File struct {
	CID    string
	Name   string
	Size   uint64
	Stored time.Time
	// Private is whether it was sealed with the node's key.
	Private bool
}

// Files lists the stored files, newest first.
func (db *DB) Files() ([]File, error) {
	var files []File
	err := db.read(func(rows *sql.Rows) error {
		var f File
		var size, stored int64
		if err := rows.Scan(&f.CID, &f.Name, &size, &stored, &f.Private); err != nil {
			return err
		}
		f.Size, f.Stored = uint64(size), moment(stored)
		files = append(files, f)
		return nil
	}, `SELECT cid, name, size_bytes, stored_at_ns, private FROM files ORDER BY stored_at_ns DESC, cid`)
	if err != nil {
		return nil, fmt.Errorf("load files: %w", err)
	}
	return files, nil
}

// AddFile records a stored file. The same content stored again is the same
// file, under the name and time it was last stored with.
func (db *DB) AddFile(f File) error {
	return db.durably("save file", func(b *batch) {
		b.exec(`INSERT OR REPLACE INTO files (cid, name, size_bytes, stored_at_ns, private) VALUES (?, ?, ?, ?, ?)`,
			f.CID, f.Name, int64(f.Size), nanos(f.Stored), f.Private)
	})
}

// RemoveFile forgets a stored file. Forgetting one that is not there is
// not an error.
func (db *DB) RemoveFile(cid string) error {
	return db.durably("remove file", func(b *batch) {
		b.exec(`DELETE FROM files WHERE cid = ?`, cid)
	})
}

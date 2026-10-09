package nodedb

import (
	"database/sql"
	"errors"
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
		b.exec(`INSERT INTO files (cid, name, size_bytes, stored_at_ns, private) VALUES (?, ?, ?, ?, ?)
			ON CONFLICT(cid) DO UPDATE SET name=excluded.name, size_bytes=excluded.size_bytes, stored_at_ns=excluded.stored_at_ns, private=excluded.private`,
			f.CID, f.Name, int64(f.Size), nanos(f.Stored), f.Private)
	})
}

// ErrFileReferenced means another owner still needs a catalogue entry.
var ErrFileReferenced = errors.New("file is still referenced")

// RemoveFile forgets an unreferenced stored file. Forgetting one that is
// not there is not an error. The check and deletion share a transaction;
// the foreign key remains a second guard against dangling references.
func (db *DB) RemoveFile(cid string) error {
	return db.durably("remove file", func(b *batch) {
		var referenced bool
		b.err = b.tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM file_references WHERE cid = ?)`, cid).Scan(&referenced)
		if b.err == nil && referenced {
			b.err = ErrFileReferenced
		}
		b.exec(`DELETE FROM files WHERE cid = ?`, cid)
	})
}

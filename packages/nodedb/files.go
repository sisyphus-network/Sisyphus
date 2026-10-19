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
	return db.files(`SELECT cid, name, size_bytes, stored_at_ns, private FROM files ORDER BY stored_at_ns DESC, cid`)
}

// FilesInPool lists files this node has stored through the pool identified by
// coordinatorID, newest first. The empty ID is the node's own pool (and the
// legacy scope for files that predate joined-pool file tracking).
func (db *DB) FilesInPool(coordinatorID string) ([]File, error) {
	return db.files(`SELECT f.cid, s.name, s.size_bytes, s.stored_at_ns, s.private
		FROM files f JOIN file_pool_scopes s ON s.cid = f.cid
		WHERE s.coordinator_id = ? ORDER BY s.stored_at_ns DESC, s.cid`, coordinatorID)
}

func (db *DB) files(query string, args ...any) ([]File, error) {
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
	}, query, args...)
	if err != nil {
		return nil, fmt.Errorf("load files: %w", err)
	}
	return files, nil
}

// AddFile records a stored file. The same content stored again is the same
// file, under the name and time it was last stored with.
func (db *DB) AddFile(f File) error {
	return db.AddFileInPool("", f)
}

// AddFileInPool records a stored file in the catalogue for one pool. The
// content row remains shared by CID, while its display metadata is scoped so
// storing the same content under another name in another pool cannot rename
// the first pool's entry.
func (db *DB) AddFileInPool(coordinatorID string, f File) error {
	return db.durably("save file", func(b *batch) {
		b.exec(`INSERT INTO files (cid, name, size_bytes, stored_at_ns, private) VALUES (?, ?, ?, ?, ?)
			ON CONFLICT(cid) DO UPDATE SET name=excluded.name, size_bytes=excluded.size_bytes, stored_at_ns=excluded.stored_at_ns, private=excluded.private`,
			f.CID, f.Name, int64(f.Size), nanos(f.Stored), f.Private)
		b.exec(`INSERT INTO file_pool_scopes (coordinator_id, cid, name, size_bytes, stored_at_ns, private)
			VALUES (?, ?, ?, ?, ?, ?)
			ON CONFLICT(coordinator_id, cid) DO UPDATE SET name=excluded.name, size_bytes=excluded.size_bytes, stored_at_ns=excluded.stored_at_ns, private=excluded.private`,
			coordinatorID, f.CID, f.Name, int64(f.Size), nanos(f.Stored), f.Private)
	})
}

// ErrFileReferenced means another owner still needs a catalogue entry.
var ErrFileReferenced = errors.New("file is still referenced")

// RemoveFile forgets an unreferenced stored file. Forgetting one that is
// not there is not an error. The check and deletion share a transaction;
// the foreign key remains a second guard against dangling references.
func (db *DB) RemoveFile(cid string) error {
	return db.RemoveFileInPool("", cid)
}

// RemoveFileInPool removes this pool's catalogue entry. Shared content and
// metadata used by another pool remain; an entry referenced by a chat is
// conservatively retained until that reference is released.
func (db *DB) RemoveFileInPool(coordinatorID, cid string) error {
	return db.durably("remove file", func(b *batch) {
		var referenced bool
		b.err = b.tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM file_references WHERE cid = ?)`, cid).Scan(&referenced)
		if b.err == nil && referenced {
			b.err = ErrFileReferenced
		}
		b.exec(`DELETE FROM file_pool_scopes WHERE coordinator_id = ? AND cid = ?`, coordinatorID, cid)
		b.exec(`DELETE FROM files WHERE cid = ? AND NOT EXISTS (SELECT 1 FROM file_pool_scopes WHERE cid = ?)`, cid, cid)
	})
}

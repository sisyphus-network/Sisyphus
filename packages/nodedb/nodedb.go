// Package nodedb keeps a node's state in a SQLite database in its data
// directory, so that it survives the node being restarted.
package nodedb

import (
	"database/sql"
	"embed"
	"fmt"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"

	_ "modernc.org/sqlite" // the database driver, in pure Go
)

// Migrations are numbered SQL files, applied in order and never edited once
// released: a change to the schema is a new file. This is the convention
// the Rust daemon's database uses too.
//
//go:embed migrations/*.sql
var migrations embed.FS

// DB is a node's database. Its methods may be called from several
// goroutines.
type DB struct {
	sql *sql.DB
}

// Open opens the database in the given file, creating it if need be and
// bringing its schema up to date.
func Open(file string) (*DB, error) {
	// The database holds the keys of private jobs, so only its owner may
	// read it. SQLite gives the files it keeps beside it the same mode.
	created, err := os.OpenFile(file, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open node database: %w", err)
	}
	created.Close()

	pragmas := url.Values{"_pragma": {
		// Readers do not block the writer, and a commit costs no disk sync.
		"journal_mode(WAL)",
		// A crash of the machine may lose the last moments of work but never
		// corrupts; what must not be lost is synced explicitly.
		"synchronous(NORMAL)",
		"foreign_keys(ON)",
		"busy_timeout(5000)",
		// Deleted content is overwritten rather than left in the file.
		"secure_delete(ON)",
	}}
	handle, _ := sql.Open("sqlite", "file:"+url.PathEscape(file)+"?"+pragmas.Encode()) // only an unknown driver fails here
	// One connection: SQLite allows one writer at a time anyway, and this
	// way they queue here rather than fail there.
	handle.SetMaxOpenConns(1)
	db := &DB{sql: handle}
	if err := db.migrate(); err != nil {
		handle.Close()
		return nil, fmt.Errorf("open node database %s: %w", file, err)
	}
	return db, nil
}

// Close closes the database.
func (db *DB) Close() error {
	return db.sql.Close()
}

// migrate applies the migrations the database has not had yet.
func (db *DB) migrate() error {
	if _, err := db.sql.Exec(`CREATE TABLE IF NOT EXISTS schema_migrations (
		version INTEGER PRIMARY KEY,
		name TEXT NOT NULL,
		applied_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP
	) STRICT`); err != nil {
		return err
	}
	var applied int
	// MAX of no rows is NULL, hence the COALESCE; a query this plain cannot
	// fail on a database the statement above just succeeded on.
	db.sql.QueryRow(`SELECT COALESCE(MAX(version), 0) FROM schema_migrations`).Scan(&applied)

	files, _ := migrations.ReadDir("migrations") // embedded, so always readable
	names := make([]string, 0, len(files))
	for _, file := range files {
		names = append(names, file.Name())
	}
	sort.Strings(names)
	latest := version(names[len(names)-1])
	if applied > latest {
		return fmt.Errorf("it was made by a newer sisyphusd (schema version %d; this one knows up to %d)", applied, latest)
	}
	for _, name := range names {
		if version(name) <= applied {
			continue
		}
		script, _ := migrations.ReadFile("migrations/" + name)
		err := db.write(func(b *batch) {
			b.exec(string(script))
			b.exec(`INSERT INTO schema_migrations (version, name) VALUES (?, ?)`, version(name), strings.TrimSuffix(name, ".sql"))
		})
		if err != nil {
			return fmt.Errorf("migration %s: %w", name, err)
		}
	}
	return nil
}

// version is the number a migration file's name starts with.
func version(name string) int {
	number, _, _ := strings.Cut(name, "_")
	v, _ := strconv.Atoi(number) // the files are ours, and numbered
	return v
}

// batch is a set of statements that take effect together or not at all.
// After one fails the rest do nothing, so a caller need not check each.
type batch struct {
	tx  *sql.Tx
	err error
}

func (b *batch) exec(statement string, args ...any) {
	if b.err == nil {
		_, b.err = b.tx.Exec(statement, args...)
	}
}

// write runs fn's statements as one transaction.
func (db *DB) write(fn func(*batch)) error {
	tx, err := db.sql.Begin()
	if err != nil {
		return err
	}
	b := &batch{tx: tx}
	fn(b)
	if b.err != nil {
		tx.Rollback()
		return b.err
	}
	return tx.Commit()
}

// sync makes everything written so far safe against the machine itself
// failing, and leaves nothing of it in the write-ahead log.
func (db *DB) sync() error {
	_, err := db.sql.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`)
	return err
}

// read runs a query and calls row for each row of its result.
func (db *DB) read(row func(*sql.Rows) error, query string, args ...any) error {
	rows, err := db.sql.Query(query, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		if err := row(rows); err != nil {
			return err
		}
	}
	return rows.Err()
}

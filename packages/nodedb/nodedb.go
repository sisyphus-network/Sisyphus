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
	sql        *sql.DB
	checkpoint func() error
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
	var newest int
	// MAX of no rows is NULL, hence the COALESCE; a query this plain cannot
	// fail on a database the statement above just succeeded on.
	db.sql.QueryRow(`SELECT COALESCE(MAX(version), 0) FROM schema_migrations`).Scan(&newest)

	files, _ := migrations.ReadDir("migrations") // embedded, so always readable
	names := make([]string, 0, len(files))
	for _, file := range files {
		names = append(names, file.Name())
	}
	sort.Strings(names)
	if err := alignMigrationHistory(db.sql, names); err != nil {
		return fmt.Errorf("align migration history: %w", err)
	}
	latest := version(names[len(names)-1])
	if newest > latest {
		return fmt.Errorf("it was made by a newer sisyphusd (schema version %d; this one knows up to %d)", newest, latest)
	}
	for _, name := range names {
		// Whether a database has had a migration is asked of each, and not
		// taken from the highest it has had: two pieces of work each add
		// the next number, and the one with the higher may arrive first.
		var applied bool
		db.sql.QueryRow(`SELECT EXISTS(SELECT 1 FROM schema_migrations WHERE version = ?)`, version(name)).Scan(&applied)
		if applied {
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

// alignMigrationHistory repairs databases written while migrations were
// numbered differently. Migration names, unlike their numbers, identify the
// schema change; some released builds recorded the same named migrations at
// older numbers after earlier work was added to the sequence.
func alignMigrationHistory(handle *sql.DB, names []string) error {
	type entry struct {
		version int
		name    string
	}
	type update struct {
		entry
		target int
		name   string
	}

	current := make(map[string]entry, len(names))
	for _, name := range names {
		suffix := migrationSuffix(name)
		current[suffix] = entry{version: version(name), name: strings.TrimSuffix(name, ".sql")}
	}

	rows, err := handle.Query(`SELECT version, name FROM schema_migrations ORDER BY version`)
	if err != nil {
		return err
	}
	var entries []entry
	for rows.Next() {
		var item entry
		if err := rows.Scan(&item.version, &item.name); err != nil {
			rows.Close()
			return err
		}
		entries = append(entries, item)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}

	updates := make([]update, 0)
	finalVersions := make(map[int]string, len(entries))
	for _, item := range entries {
		target, known := current[migrationSuffix(item.name)]
		if !known {
			target = entry{version: item.version, name: item.name}
		}
		if previous, exists := finalVersions[target.version]; exists {
			return fmt.Errorf("migrations %q and %q both resolve to version %d", previous, item.name, target.version)
		}
		finalVersions[target.version] = item.name
		if target.version != item.version || target.name != item.name {
			updates = append(updates, update{entry: item, target: target.version, name: target.name})
		}
	}
	if len(updates) == 0 {
		return nil
	}

	// Move changed keys out of the way first, since renumbering can contain
	// cycles (for example, old migration 16 becoming 18 while old 18 becomes
	// 21). The transaction makes the bookkeeping update atomic.
	tx, err := handle.Begin()
	if err != nil {
		return err
	}
	rollback := func(err error) error {
		_ = tx.Rollback()
		return err
	}
	for i, item := range updates {
		if _, err := tx.Exec(`UPDATE schema_migrations SET version = ? WHERE version = ?`, -i-1, item.version); err != nil {
			return rollback(err)
		}
	}
	for i, item := range updates {
		if _, err := tx.Exec(`UPDATE schema_migrations SET version = ?, name = ? WHERE version = ?`, item.target, item.name, -i-1); err != nil {
			return rollback(err)
		}
	}
	return tx.Commit()
}

func migrationSuffix(name string) string {
	name = strings.TrimSuffix(name, ".sql")
	_, suffix, found := strings.Cut(name, "_")
	if !found {
		return name
	}
	return suffix
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
	if db.checkpoint != nil {
		return db.checkpoint()
	}
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

package nodedb

import (
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Member is a node admitted to this one.
type Member struct {
	ID string
	// Role is "worker" or "client".
	Role   string
	Joined time.Time
}

// Members returns every admitted node, ordered by ID.
func (db *DB) Members() ([]Member, error) {
	var members []Member
	err := db.read(func(rows *sql.Rows) error {
		var m Member
		var joined int64
		if err := rows.Scan(&m.ID, &m.Role, &joined); err != nil {
			return err
		}
		m.Joined = moment(joined)
		members = append(members, m)
		return nil
	}, `SELECT node_id, role, joined_at_ns FROM members ORDER BY node_id`)
	if err != nil {
		return nil, fmt.Errorf("load members: %w", err)
	}
	return members, nil
}

// SaveMember admits a node, or changes the role of one already admitted.
// It is synced to disk before SaveMember returns.
func (db *DB) SaveMember(m Member) error {
	return db.durably("save member", func(b *batch) {
		b.exec(`INSERT INTO members (node_id, role, joined_at_ns) VALUES (?, ?, ?)
			ON CONFLICT (node_id) DO UPDATE SET role = excluded.role, joined_at_ns = excluded.joined_at_ns`,
			m.ID, m.Role, nanos(m.Joined))
	})
}

// DeleteMember takes a node off the list, which is synced to disk before
// DeleteMember returns. A node that was not on it is not an error.
func (db *DB) DeleteMember(id string) error {
	return db.durably("delete member", func(b *batch) {
		b.exec(`DELETE FROM members WHERE node_id = ?`, id)
	})
}

// SaveInvitation records an invitation under the hash of its token, and
// forgets those that expired before now.
func (db *DB) SaveInvitation(tokenHash, role string, expires, now time.Time) error {
	return db.durably("save invitation", func(b *batch) {
		b.exec(`DELETE FROM invitations WHERE expires_at_ns < ?`, nanos(now))
		b.exec(`INSERT INTO invitations (token_hash, role, expires_at_ns) VALUES (?, ?, ?)`, tokenHash, role, nanos(expires))
	})
}

// TakeInvitation removes the invitation recorded under a token's hash and
// returns what it was. found is false if there was none: an invitation can
// be taken once.
func (db *DB) TakeInvitation(tokenHash string) (role string, expires time.Time, found bool, err error) {
	var expiresNanos int64
	err = db.sql.QueryRow(`DELETE FROM invitations WHERE token_hash = ? RETURNING role, expires_at_ns`, tokenHash).Scan(&role, &expiresNanos)
	if errors.Is(err, sql.ErrNoRows) {
		return "", time.Time{}, false, nil
	}
	if err != nil {
		return "", time.Time{}, false, fmt.Errorf("take invitation: %w", err)
	}
	return role, moment(expiresNanos), true, nil
}

// durably runs fn's statements as one transaction and syncs it to disk.
func (db *DB) durably(what string, fn func(*batch)) error {
	if err := db.write(fn); err != nil {
		return fmt.Errorf("%s: %w", what, err)
	}
	return db.sync()
}

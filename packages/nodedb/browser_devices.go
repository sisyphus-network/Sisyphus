package nodedb

import (
	"crypto/subtle"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// BrowserDevice is a browser paired with this node. TokenHash is an
// authentication verifier and must never be returned to a client.
type BrowserDevice struct {
	ID, Name             string
	TokenHash            []byte
	CreatedAt, ExpiresAt time.Time
}

// SaveBrowserPairing replaces the one outstanding pairing-code verifier.
func (db *DB) SaveBrowserPairing(codeHash []byte, expiresAt time.Time) error {
	return db.durably("save browser pairing", func(b *batch) {
		b.exec(`INSERT INTO browser_pairings (singleton_id, code_hash, expires_at_ns) VALUES (1, ?, ?)
			ON CONFLICT (singleton_id) DO UPDATE SET code_hash = excluded.code_hash, expires_at_ns = excluded.expires_at_ns`, codeHash, nanos(expiresAt))
	})
}

// ConsumeBrowserPairing atomically consumes a live code and records its
// resulting device. A wrong or expired code changes nothing.
func (db *DB) ConsumeBrowserPairing(codeHash []byte, now time.Time, device BrowserDevice) (bool, error) {
	tx, err := db.sql.Begin()
	if err != nil {
		return false, fmt.Errorf("begin browser pairing: %w", err)
	}
	defer tx.Rollback()
	var stored []byte
	var expires int64
	err = tx.QueryRow(`SELECT code_hash, expires_at_ns FROM browser_pairings WHERE singleton_id = 1`).Scan(&stored, &expires)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read browser pairing: %w", err)
	}
	if nanos(now) >= expires || len(stored) != len(codeHash) || subtle.ConstantTimeCompare(stored, codeHash) != 1 {
		return false, nil
	}
	if _, err := tx.Exec(`INSERT INTO browser_devices (device_id, name, token_hash, created_at_ns, expires_at_ns) VALUES (?, ?, ?, ?, ?)`, device.ID, device.Name, device.TokenHash, nanos(device.CreatedAt), nanos(device.ExpiresAt)); err != nil {
		return false, fmt.Errorf("save paired browser: %w", err)
	}
	if _, err := tx.Exec(`DELETE FROM browser_pairings WHERE singleton_id = 1`); err != nil {
		return false, fmt.Errorf("consume browser pairing: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("commit browser pairing: %w", err)
	}
	if err := db.sync(); err != nil {
		return false, fmt.Errorf("sync paired browser: %w", err)
	}
	return true, nil
}

// BrowserDevices lists paired browsers, including private token verifiers
// for internal daemon authentication. Callers must sanitize before encoding.
func (db *DB) BrowserDevices() ([]BrowserDevice, error) {
	var devices []BrowserDevice
	err := db.read(func(rows *sql.Rows) error {
		var device BrowserDevice
		var created, expires int64
		if err := rows.Scan(&device.ID, &device.Name, &device.TokenHash, &created, &expires); err != nil {
			return err
		}
		device.CreatedAt = moment(created)
		device.ExpiresAt = moment(expires)
		devices = append(devices, device)
		return nil
	}, `SELECT device_id, name, token_hash, created_at_ns, expires_at_ns FROM browser_devices ORDER BY created_at_ns, device_id`)
	if err != nil {
		return nil, fmt.Errorf("list paired browsers: %w", err)
	}
	return devices, nil
}

// DeleteBrowserDevice revokes a paired browser's credential.
func (db *DB) DeleteBrowserDevice(id string) error {
	return db.durably("revoke paired browser", func(b *batch) {
		b.exec(`DELETE FROM browser_devices WHERE device_id = ?`, id)
	})
}

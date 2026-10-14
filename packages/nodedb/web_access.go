package nodedb

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
)

// WebAccessConfig controls the optional HTTPS listener for remote browser
// access. The zero value deliberately leaves remote access disabled.
type WebAccessConfig struct {
	Enabled         bool
	ListenAddress   string
	ListenAddresses []string
	PublicURL       string
}

// WebAccess returns the saved configuration. A new node has remote access
// disabled and no network address configured.
func (db *DB) WebAccess() (WebAccessConfig, error) {
	var config WebAccessConfig
	var enabled int
	var addressesJSON string
	err := db.sql.QueryRow(`SELECT enabled, listen_address, public_url, listen_addresses FROM web_access_config WHERE singleton_id = 1`).Scan(&enabled, &config.ListenAddress, &config.PublicURL, &addressesJSON)
	if errors.Is(err, sql.ErrNoRows) {
		return WebAccessConfig{}, nil
	}
	if err != nil {
		return WebAccessConfig{}, fmt.Errorf("load web access configuration: %w", err)
	}
	config.Enabled = enabled != 0
	if addressesJSON != "" {
		if err := json.Unmarshal([]byte(addressesJSON), &config.ListenAddresses); err != nil {
			return WebAccessConfig{}, fmt.Errorf("decode saved web listener addresses: %w", err)
		}
	}
	return config, nil
}

// SetWebAccess replaces the optional remote browser-access configuration.
func (db *DB) SetWebAccess(config WebAccessConfig) error {
	addressesJSON, err := json.Marshal(config.ListenAddresses)
	if err != nil {
		return fmt.Errorf("encode web listener addresses: %w", err)
	}
	return db.durably("save web access configuration", func(b *batch) {
		b.exec(`INSERT INTO web_access_config (singleton_id, enabled, listen_address, public_url, listen_addresses) VALUES (1, ?, ?, ?, ?)
			ON CONFLICT (singleton_id) DO UPDATE SET enabled = excluded.enabled, listen_address = excluded.listen_address, public_url = excluded.public_url, listen_addresses = excluded.listen_addresses`, boolInt(config.Enabled), config.ListenAddress, config.PublicURL, string(addressesJSON))
	})
}

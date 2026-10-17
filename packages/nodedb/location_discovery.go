package nodedb

import (
	"database/sql"
	"errors"
	"fmt"
)

// LocationDiscovery is the node owner's consent for optional sources used
// to estimate the node's network location.
type LocationDiscovery struct {
	LocateGateway bool
	LocateCountry bool
	StunServer    string
}

// LocationDiscovery returns saved settings and whether the owner has saved
// any. No row means the daemon's startup flags provide the defaults.
func (db *DB) LocationDiscovery() (LocationDiscovery, bool, error) {
	var gateway, country int
	var settings LocationDiscovery
	err := db.sql.QueryRow(`SELECT locate_gateway, locate_country, stun_server FROM location_discovery WHERE singleton_id = 1`).
		Scan(&gateway, &country, &settings.StunServer)
	if errors.Is(err, sql.ErrNoRows) {
		return LocationDiscovery{}, false, nil
	}
	if err != nil {
		return LocationDiscovery{}, false, fmt.Errorf("load location discovery settings: %w", err)
	}
	settings.LocateGateway, settings.LocateCountry = gateway != 0, country != 0
	return settings, true, nil
}

// SetLocationDiscovery replaces the node's saved discovery settings.
func (db *DB) SetLocationDiscovery(settings LocationDiscovery) error {
	return db.durably("save location discovery settings", func(b *batch) {
		b.exec(`INSERT INTO location_discovery (singleton_id, locate_gateway, locate_country, stun_server) VALUES (1, ?, ?, ?)
			ON CONFLICT (singleton_id) DO UPDATE SET locate_gateway = excluded.locate_gateway,
				locate_country = excluded.locate_country, stun_server = excluded.stun_server`,
			boolInt(settings.LocateGateway), boolInt(settings.LocateCountry), settings.StunServer)
	})
}

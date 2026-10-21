package nodedb

import (
	"database/sql"
	"errors"
	"fmt"
)

// OwnerLocation is where the node's owner says the node is, in degrees,
// and how much of it other members of a pool may see: one of "exact",
// "about-1km", "city", "country" or "nothing".
type OwnerLocation struct {
	Latitude  float64
	Longitude float64
	Share     string
}

// OwnerLocation returns the saved location and whether the owner has saved
// one.
func (db *DB) OwnerLocation() (OwnerLocation, bool, error) {
	var location OwnerLocation
	err := db.sql.QueryRow(`SELECT latitude, longitude, share FROM owner_location WHERE singleton_id = 1`).
		Scan(&location.Latitude, &location.Longitude, &location.Share)
	if errors.Is(err, sql.ErrNoRows) {
		return OwnerLocation{}, false, nil
	}
	if err != nil {
		return OwnerLocation{}, false, fmt.Errorf("load owner location: %w", err)
	}
	return location, true, nil
}

// SetOwnerLocation replaces the node's saved location.
func (db *DB) SetOwnerLocation(location OwnerLocation) error {
	return db.durably("save owner location", func(b *batch) {
		b.exec(`INSERT INTO owner_location (singleton_id, latitude, longitude, share) VALUES (1, ?, ?, ?)
			ON CONFLICT (singleton_id) DO UPDATE SET latitude = excluded.latitude,
				longitude = excluded.longitude, share = excluded.share`,
			location.Latitude, location.Longitude, location.Share)
	})
}

// ClearOwnerLocation forgets the node's saved location, if it has one.
func (db *DB) ClearOwnerLocation() error {
	return db.durably("clear owner location", func(b *batch) {
		b.exec(`DELETE FROM owner_location WHERE singleton_id = 1`)
	})
}

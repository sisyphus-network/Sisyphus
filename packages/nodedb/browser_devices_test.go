package nodedb

import (
	"crypto/sha256"
	"errors"
	"testing"
	"time"
)

func TestBrowserPairingPersistsAndCanBeUsedOnlyOnce(t *testing.T) {
	db, file := newDB(t)
	now := time.Date(2026, 10, 11, 10, 0, 0, 0, time.UTC)
	codeHash := sha256.Sum256([]byte("01234567"))
	if err := db.SaveBrowserPairing(codeHash[:], now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	wrong := sha256.Sum256([]byte("76543210"))
	tokenHash := sha256.Sum256([]byte("secret"))
	device := BrowserDevice{ID: "device-1", Name: "Phone", TokenHash: tokenHash[:], CreatedAt: now, ExpiresAt: now.Add(30 * 24 * time.Hour)}
	if consumed, err := db.ConsumeBrowserPairing(wrong[:], now, device); err != nil || consumed {
		t.Fatalf("wrong code consumed: %v, %v", consumed, err)
	}
	if consumed, err := db.ConsumeBrowserPairing(codeHash[:], now, device); err != nil || !consumed {
		t.Fatalf("valid code not consumed: %v, %v", consumed, err)
	}
	db = reopen(t, db, file)
	devices, err := db.BrowserDevices()
	if err != nil || len(devices) != 1 || devices[0].ID != device.ID || devices[0].Name != device.Name || string(devices[0].TokenHash) != string(device.TokenHash) || !devices[0].CreatedAt.Equal(now) || !devices[0].ExpiresAt.Equal(device.ExpiresAt) {
		t.Fatalf("device after restart: %+v, %v", devices, err)
	}
	otherHash := sha256.Sum256([]byte("other"))
	if consumed, err := db.ConsumeBrowserPairing(codeHash[:], now, BrowserDevice{ID: "device-2", Name: "Other", TokenHash: otherHash[:], CreatedAt: now, ExpiresAt: now.Add(time.Hour)}); err != nil || consumed {
		t.Fatalf("pairing code was reusable: %v, %v", consumed, err)
	}
	if err := db.DeleteBrowserDevice("device-1"); err != nil {
		t.Fatal(err)
	}
	if devices, err := db.BrowserDevices(); err != nil || len(devices) != 0 {
		t.Fatalf("device after revocation: %+v, %v", devices, err)
	}
}

func TestBrowserPairingPersistenceErrors(t *testing.T) {
	t.Run("begin transaction", func(t *testing.T) {
		db, _ := newDB(t)
		if err := db.Close(); err != nil {
			t.Fatal(err)
		}
		if _, err := db.ConsumeBrowserPairing(nil, time.Now(), BrowserDevice{}); err == nil {
			t.Fatal("closed database began a pairing transaction")
		}
	})
	t.Run("read pairing", func(t *testing.T) {
		db, _ := newDB(t)
		if _, err := db.sql.Exec(`DROP TABLE browser_pairings`); err != nil {
			t.Fatal(err)
		}
		if _, err := db.ConsumeBrowserPairing(nil, time.Now(), BrowserDevice{}); err == nil {
			t.Fatal("missing pairing table was not reported")
		}
	})
	t.Run("save device", func(t *testing.T) {
		db, _ := newDB(t)
		now := time.Now().UTC()
		hash := sha256.Sum256([]byte("pair"))
		if err := db.SaveBrowserPairing(hash[:], now.Add(time.Hour)); err != nil {
			t.Fatal(err)
		}
		device := BrowserDevice{ID: "duplicate", Name: "Phone", TokenHash: hash[:], CreatedAt: now, ExpiresAt: now.Add(time.Hour)}
		if _, err := db.ConsumeBrowserPairing(hash[:], now, device); err != nil {
			t.Fatal(err)
		}
		if err := db.SaveBrowserPairing(hash[:], now.Add(time.Hour)); err != nil {
			t.Fatal(err)
		}
		if _, err := db.ConsumeBrowserPairing(hash[:], now, device); err == nil {
			t.Fatal("duplicate device ID was saved")
		}
	})
	t.Run("consume pairing", func(t *testing.T) {
		db, _ := newDB(t)
		now := time.Now().UTC()
		hash := sha256.Sum256([]byte("pair"))
		if err := db.SaveBrowserPairing(hash[:], now.Add(time.Hour)); err != nil {
			t.Fatal(err)
		}
		if _, err := db.sql.Exec(`CREATE TRIGGER fail_pairing_delete BEFORE DELETE ON browser_pairings BEGIN SELECT RAISE(ABORT, 'delete rejected'); END`); err != nil {
			t.Fatal(err)
		}
		device := BrowserDevice{ID: "device", Name: "Phone", TokenHash: hash[:], CreatedAt: now, ExpiresAt: now.Add(time.Hour)}
		if _, err := db.ConsumeBrowserPairing(hash[:], now, device); err == nil {
			t.Fatal("pairing delete failure was ignored")
		}
	})
	t.Run("commit transaction", func(t *testing.T) {
		db, _ := newDB(t)
		now := time.Now().UTC()
		hash := sha256.Sum256([]byte("pair"))
		if err := db.SaveBrowserPairing(hash[:], now.Add(time.Hour)); err != nil {
			t.Fatal(err)
		}
		_, err := db.sql.Exec(`CREATE TABLE guard_parent (id INTEGER PRIMARY KEY);
			CREATE TABLE guard_child (id INTEGER REFERENCES guard_parent(id) DEFERRABLE INITIALLY DEFERRED);
			CREATE TRIGGER fail_pairing_commit AFTER DELETE ON browser_pairings BEGIN INSERT INTO guard_child VALUES (1); END`)
		if err != nil {
			t.Fatal(err)
		}
		device := BrowserDevice{ID: "device", Name: "Phone", TokenHash: hash[:], CreatedAt: now, ExpiresAt: now.Add(time.Hour)}
		if _, err := db.ConsumeBrowserPairing(hash[:], now, device); err == nil {
			t.Fatal("deferred constraint did not fail the transaction commit")
		}
	})
	t.Run("checkpoint", func(t *testing.T) {
		db, _ := newDB(t)
		now := time.Now().UTC()
		hash := sha256.Sum256([]byte("pair"))
		if err := db.SaveBrowserPairing(hash[:], now.Add(time.Hour)); err != nil {
			t.Fatal(err)
		}
		db.checkpoint = func() error { return errors.New("checkpoint failed") }
		device := BrowserDevice{ID: "device", Name: "Phone", TokenHash: hash[:], CreatedAt: now, ExpiresAt: now.Add(time.Hour)}
		if _, err := db.ConsumeBrowserPairing(hash[:], now, device); err == nil {
			t.Fatal("checkpoint failure was ignored")
		}
	})
}

func TestBrowserDeviceListReportsDatabaseAndRowErrors(t *testing.T) {
	t.Run("query", func(t *testing.T) {
		db, _ := newDB(t)
		if _, err := db.sql.Exec(`DROP TABLE browser_devices`); err != nil {
			t.Fatal(err)
		}
		if _, err := db.BrowserDevices(); err == nil {
			t.Fatal("missing devices table was not reported")
		}
	})
	t.Run("scan", func(t *testing.T) {
		db, _ := newDB(t)
		_, err := db.sql.Exec(`DROP TABLE browser_devices;
			CREATE TABLE browser_devices (device_id TEXT, name TEXT, token_hash BLOB, created_at_ns TEXT, expires_at_ns INTEGER);
			INSERT INTO browser_devices VALUES ('d', 'Phone', x'00', 'not-a-time', 1)`)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.BrowserDevices(); err == nil {
			t.Fatal("malformed row was not reported")
		}
	})
}

func TestExpiredBrowserPairingCannotBeConsumed(t *testing.T) {
	db, _ := newDB(t)
	now := time.Now().UTC()
	hash := sha256.Sum256([]byte("pair-code"))
	if err := db.SaveBrowserPairing(hash[:], now); err != nil {
		t.Fatal(err)
	}
	deviceHash := sha256.Sum256([]byte("hash"))
	if consumed, err := db.ConsumeBrowserPairing(hash[:], now.Add(time.Nanosecond), BrowserDevice{ID: "d", Name: "Browser", TokenHash: deviceHash[:], CreatedAt: now, ExpiresAt: now.Add(time.Hour)}); err != nil || consumed {
		t.Fatalf("expired code consumed: %v, %v", consumed, err)
	}
}

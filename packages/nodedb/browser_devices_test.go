package nodedb

import (
	"crypto/sha256"
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

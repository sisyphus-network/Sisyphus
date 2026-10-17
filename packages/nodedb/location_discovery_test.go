package nodedb

import (
	"strings"
	"testing"
)

func TestLocationDiscoveryIsUnsetUntilSavedAndThenReplaced(t *testing.T) {
	db, _ := newDB(t)
	got, set, err := db.LocationDiscovery()
	if err != nil || set || got != (LocationDiscovery{}) {
		t.Fatalf("unset settings = %+v, set %v, err %v", got, set, err)
	}
	want := LocationDiscovery{LocateGateway: true, LocateCountry: true, StunServer: "stun.example.org:3478"}
	if err := db.SetLocationDiscovery(want); err != nil {
		t.Fatal(err)
	}
	if got, set, err = db.LocationDiscovery(); err != nil || !set || got != want {
		t.Fatalf("saved settings = %+v, set %v, err %v; want %+v", got, set, err, want)
	}
	want = LocationDiscovery{StunServer: "stun2.example.org"}
	if err := db.SetLocationDiscovery(want); err != nil {
		t.Fatal(err)
	}
	if got, set, err = db.LocationDiscovery(); err != nil || !set || got != want {
		t.Fatalf("replaced settings = %+v, set %v, err %v; want %+v", got, set, err, want)
	}
}

func TestLocationDiscoveryReportsMissingTable(t *testing.T) {
	db, _ := newDB(t)
	if _, err := db.sql.Exec(`DROP TABLE location_discovery`); err != nil {
		t.Fatal(err)
	}
	if _, _, err := db.LocationDiscovery(); err == nil || !strings.Contains(err.Error(), "load location discovery settings") {
		t.Errorf("load from missing table: %v", err)
	}
	if err := db.SetLocationDiscovery(LocationDiscovery{}); err == nil || !strings.Contains(err.Error(), "save location discovery settings") {
		t.Errorf("save to missing table: %v", err)
	}
}

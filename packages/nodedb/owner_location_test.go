package nodedb

import (
	"strings"
	"testing"
)

func TestOwnerLocationIsUnsetUntilSavedThenReplacedAndCleared(t *testing.T) {
	db, file := newDB(t)
	got, set, err := db.OwnerLocation()
	if err != nil || set || got != (OwnerLocation{}) {
		t.Fatalf("unset location = %+v, set %v, err %v", got, set, err)
	}
	if err := db.ClearOwnerLocation(); err != nil {
		t.Fatalf("clearing no location: %v", err)
	}
	want := OwnerLocation{Latitude: 51.50735, Longitude: -0.12776, Share: "about-1km"}
	if err := db.SetOwnerLocation(want); err != nil {
		t.Fatal(err)
	}
	db = reopen(t, db, file)
	if got, set, err = db.OwnerLocation(); err != nil || !set || got != want {
		t.Fatalf("saved location = %+v, set %v, err %v; want %+v", got, set, err, want)
	}
	want = OwnerLocation{Latitude: -90, Longitude: 180, Share: "exact"}
	if err := db.SetOwnerLocation(want); err != nil {
		t.Fatal(err)
	}
	if got, set, err = db.OwnerLocation(); err != nil || !set || got != want {
		t.Fatalf("replaced location = %+v, set %v, err %v; want %+v", got, set, err, want)
	}
	if err := db.ClearOwnerLocation(); err != nil {
		t.Fatal(err)
	}
	if got, set, err = db.OwnerLocation(); err != nil || set || got != (OwnerLocation{}) {
		t.Fatalf("cleared location = %+v, set %v, err %v", got, set, err)
	}
}

// The table holds only a place on the Earth and a share it knows, whoever
// writes to it.
func TestOwnerLocationRefusesWhatIsNoPlaceOrShare(t *testing.T) {
	db, _ := newDB(t)
	for _, location := range []OwnerLocation{
		{Latitude: 90.5, Share: "exact"},
		{Longitude: -180.5, Share: "exact"},
		{Share: "street"},
	} {
		if err := db.SetOwnerLocation(location); err == nil || !strings.Contains(err.Error(), "save owner location") {
			t.Errorf("save %+v: %v", location, err)
		}
	}
	if _, set, err := db.OwnerLocation(); err != nil || set {
		t.Errorf("after refused saves: set %v, err %v", set, err)
	}
}

func TestOwnerLocationReportsMissingTable(t *testing.T) {
	db, _ := newDB(t)
	if _, err := db.sql.Exec(`DROP TABLE owner_location`); err != nil {
		t.Fatal(err)
	}
	if _, _, err := db.OwnerLocation(); err == nil || !strings.Contains(err.Error(), "load owner location") {
		t.Errorf("load from missing table: %v", err)
	}
	if err := db.SetOwnerLocation(OwnerLocation{Share: "country"}); err == nil || !strings.Contains(err.Error(), "save owner location") {
		t.Errorf("save to missing table: %v", err)
	}
	if err := db.ClearOwnerLocation(); err == nil || !strings.Contains(err.Error(), "clear owner location") {
		t.Errorf("clear in missing table: %v", err)
	}
}

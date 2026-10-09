package nodedb

import (
	"strings"
	"testing"
	"time"
)

func TestTheRecordOfANameIsKeptAcrossReopening(t *testing.T) {
	db, file := newDB(t)
	if record, err := db.NameRecord("rig"); err != nil || record != nil {
		t.Fatalf("the record of a name never published: %q, %v", record, err)
	}
	now := time.Now()
	for name, records := range map[string][]string{"rig": {"first", "second"}, "laptop": {"its own"}} {
		for _, record := range records {
			if err := db.SetNameRecord(name, []byte(record), now); err != nil {
				t.Fatal(err)
			}
		}
	}
	db = reopen(t, db, file)
	// The latest of each, and each its own.
	for name, want := range map[string]string{"rig": "second", "laptop": "its own"} {
		if record, err := db.NameRecord(name); err != nil || string(record) != want {
			t.Errorf("the record of %s after reopening: %q, %v", name, record, err)
		}
	}

	db.Close()
	if _, err := db.NameRecord("rig"); err == nil || !strings.Contains(err.Error(), "load the record of a name") {
		t.Errorf("NameRecord on a closed database: %v", err)
	}
	if err := db.SetNameRecord("rig", []byte("third"), now); err == nil || !strings.Contains(err.Error(), "save the record of a name") {
		t.Errorf("SetNameRecord on a closed database: %v", err)
	}
}

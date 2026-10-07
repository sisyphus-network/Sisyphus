package nodedb

import (
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestTheNodesWorkedForAreKeptAcrossReopening(t *testing.T) {
	db, file := newDB(t)
	now := time.Now()
	for _, id := range []string{"b", "a", "c", "a"} {
		if err := db.SetWorksFor(id, true, now); err != nil {
			t.Fatal(err)
		}
	}
	// One is given up, and giving up one never taken on is no error.
	for _, id := range []string{"c", "never"} {
		if err := db.SetWorksFor(id, false, now); err != nil {
			t.Fatal(err)
		}
	}
	db = reopen(t, db, file)
	if got, err := db.WorksFor(); err != nil || !reflect.DeepEqual(got, []string{"a", "b"}) {
		t.Errorf("worked for after reopening: %v, %v", got, err)
	}

	loosen(t, db, "works_for", "node_id, since_ns")
	if _, err := db.WorksFor(); err == nil || !strings.Contains(err.Error(), "load the nodes worked for") {
		t.Errorf("with a damaged table: %v", err)
	}
	db.Close()
	if err := db.SetWorksFor("a", true, now); err == nil || !strings.Contains(err.Error(), "save a node worked for") {
		t.Errorf("on a closed database: %v", err)
	}
}

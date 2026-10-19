package nodedb

import (
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestTheHoldsOnFilesKeptInAPoolAreKeptAcrossReopening(t *testing.T) {
	db, file := newDB(t)
	now := time.Unix(1_800_000_000, 0)
	later, sooner := now.Add(time.Hour), now.Add(time.Minute)
	for _, hold := range []struct {
		coordinator, cid, owner string
		expires                 time.Time
	}{
		{"c", "a", "user", time.Time{}},
		{"c", "a", "chat-draft", sooner},
		{"c", "a", "chat-draft", later}, // held again: the later expiry
		{"c", "a", "chat:1", now.Add(-time.Second)},
		{"c", "b", "chat:1", time.Time{}},
		{"other", "a", "user", sooner},
	} {
		if err := db.HoldPoolFile(hold.coordinator, hold.cid, hold.owner, hold.expires, now.Add(-time.Hour)); err != nil {
			t.Fatal(err)
		}
	}
	// One is released, and releasing one never held is no error.
	for _, owner := range []string{"user", "nobody"} {
		if err := db.ReleasePoolFile("c", "b", owner); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.ReleasePoolFile("c", "b", "chat:1"); err != nil {
		t.Fatal(err)
	}
	db = reopen(t, db, file)
	// Those over at now are left out; each owner once.
	if got, err := db.PoolFileHolds("c", "a", now); err != nil || !reflect.DeepEqual(got, map[string]time.Time{"chat-draft": time.Unix(0, later.UnixNano()), "user": {}}) {
		t.Errorf("holds on a after reopening: %v, %v", got, err)
	}
	for _, none := range []struct{ coordinator, cid string }{{"c", "b"}, {"nobody", "a"}} {
		if got, err := db.PoolFileHolds(none.coordinator, none.cid, now); err != nil || len(got) != 0 {
			t.Errorf("holds on %s in %s: %v, %v", none.cid, none.coordinator, got, err)
		}
	}
	// A hold placed later forgets those over by then.
	if err := db.HoldPoolFile("c", "c", "user", time.Time{}, now); err != nil {
		t.Fatal(err)
	}
	var left int
	if err := db.sql.QueryRow(`SELECT count(*) FROM pool_file_holds WHERE owner = 'chat:1'`).Scan(&left); err != nil || left != 0 {
		t.Errorf("holds over before the last was placed: %d left, %v", left, err)
	}

	loosen(t, db, "pool_file_holds", "coordinator_id DEFAULT 'c', cid DEFAULT 'a', owner, expires_ns DEFAULT 'never'")
	if _, err := db.PoolFileHolds("c", "a", now); err == nil || !strings.Contains(err.Error(), "load the holds on a file kept in the pool") {
		t.Errorf("with a damaged table: %v", err)
	}
	db.Close()
	if err := db.HoldPoolFile("c", "a", "user", time.Time{}, now); err == nil || !strings.Contains(err.Error(), "save a hold on a file kept in the pool") {
		t.Errorf("on a closed database: %v", err)
	}
	if err := db.ReleasePoolFile("c", "a", "user"); err == nil || !strings.Contains(err.Error(), "release a hold on a file kept in the pool") {
		t.Errorf("on a closed database: %v", err)
	}
}

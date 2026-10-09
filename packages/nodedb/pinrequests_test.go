package nodedb

import (
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestPinRequestsAreKeptAcrossReopening(t *testing.T) {
	db, file := newDB(t)
	if requests, err := db.PinRequests(); err != nil || len(requests) != 0 {
		t.Fatalf("a new list of requests: %v, %v", requests, err)
	}
	// Times come back in the machine's own zone.
	at := submitted.Local()
	plain := PinRequest{ID: "r1", CID: "bafyone", Status: "pinned", Created: at}
	full := PinRequest{
		ID: "r2", CID: "bafytwo", Name: "results.tar", Origins: []string{"/ip4/10.0.0.5/tcp/4001/p2p/12D3KooWexample"},
		Meta: map[string]string{"app": "tests"}, Status: "queued", Created: at.Add(time.Minute), Holds: []string{"bafyzero"},
	}
	for _, r := range []PinRequest{plain, full} {
		if err := db.SavePinRequest(r); err != nil {
			t.Fatal(err)
		}
	}
	db = reopen(t, db, file)
	got, err := db.PinRequests()
	if err != nil || !reflect.DeepEqual(got, []PinRequest{full, plain}) {
		t.Fatalf("the requests after reopening: %+v, %v", got, err)
	}

	// Saved again, a request is as it was last saved.
	full.Status, full.Info, full.Holds = "failed", "nobody has it", nil
	if err := db.SavePinRequest(full); err != nil {
		t.Fatal(err)
	}
	if got, _ := db.PinRequests(); !reflect.DeepEqual(got, []PinRequest{full, plain}) {
		t.Errorf("after saving one again: %+v", got)
	}

	for _, id := range []string{"r1", "r1"} {
		if err := db.RemovePinRequest(id); err != nil {
			t.Fatal(err)
		}
	}
	if got, _ := db.PinRequests(); !reflect.DeepEqual(got, []PinRequest{full}) {
		t.Errorf("after removing one: %+v", got)
	}
}

func TestPinRequestFailuresAreReported(t *testing.T) {
	db, _ := newDB(t)
	if _, err := db.sql.Exec(`INSERT INTO pin_requests VALUES ('r1', 'bafyone', '', 'not a list', '{}', 'pinned', '', 1, '[]')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.PinRequests(); err == nil || !strings.Contains(err.Error(), "load pin requests") {
		t.Errorf("with origins that are no list: %v", err)
	}
	loosen(t, db, "pin_requests", "request_id, cid, name, origins, meta, status, info, created_at_ns, holds")
	if _, err := db.PinRequests(); err == nil || !strings.Contains(err.Error(), "load pin requests") {
		t.Errorf("with a damaged table: %v", err)
	}
	db.Close()
	if err := db.SavePinRequest(PinRequest{ID: "r1"}); err == nil || !strings.Contains(err.Error(), "save pin request") {
		t.Errorf("SavePinRequest on a closed database: %v", err)
	}
	if err := db.RemovePinRequest("r1"); err == nil || !strings.Contains(err.Error(), "remove pin request") {
		t.Errorf("RemovePinRequest on a closed database: %v", err)
	}
}

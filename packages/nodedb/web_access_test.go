package nodedb

import (
	"reflect"
	"testing"
)

func TestWebAccessDefaultsToDisabledAndPersists(t *testing.T) {
	db, file := newDB(t)
	got, err := db.WebAccess()
	if err != nil || !reflect.DeepEqual(got, WebAccessConfig{}) {
		t.Fatalf("default web access = %+v, %v; want disabled zero config", got, err)
	}
	want := WebAccessConfig{Enabled: true, ListenAddress: "12.0.0.48:50052", PublicURL: "https://node.example.test"}
	if err := db.SetWebAccess(want); err != nil {
		t.Fatal(err)
	}
	db = reopen(t, db, file)
	got, err = db.WebAccess()
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("saved web access = %+v, %v; want %+v", got, err, want)
	}
	if err := db.SetWebAccess(WebAccessConfig{ListenAddress: want.ListenAddress, PublicURL: want.PublicURL}); err != nil {
		t.Fatal(err)
	}
	got, err = db.WebAccess()
	if err != nil || !reflect.DeepEqual(got, WebAccessConfig{ListenAddress: want.ListenAddress, PublicURL: want.PublicURL}) {
		t.Fatalf("disabled web access = %+v, %v", got, err)
	}
	multiple := WebAccessConfig{ListenAddress: "192.168.1.20:50052", ListenAddresses: []string{"192.168.1.20:50052", "10.0.0.8:50052"}, PublicURL: "https://node.example.test"}
	if err := db.SetWebAccess(multiple); err != nil {
		t.Fatal(err)
	}
	got, err = db.WebAccess()
	if err != nil || !reflect.DeepEqual(got, multiple) {
		t.Fatalf("multiple web access addresses = %+v, %v; want %+v", got, err, multiple)
	}
}

func TestWebAccessRejectsClosedDatabaseAndMalformedStoredAddresses(t *testing.T) {
	db, _ := newDB(t)
	if err := db.SetWebAccess(WebAccessConfig{ListenAddress: "192.0.2.20:50052"}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.sql.Exec(`UPDATE web_access_config SET listen_addresses = 'not-json' WHERE singleton_id = 1`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.WebAccess(); err == nil {
		t.Fatal("malformed saved listener addresses unexpectedly decoded")
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := db.WebAccess(); err == nil {
		t.Fatal("closed database read unexpectedly succeeded")
	}
	if err := db.SetWebAccess(WebAccessConfig{}); err == nil {
		t.Fatal("closed database write unexpectedly succeeded")
	}
}

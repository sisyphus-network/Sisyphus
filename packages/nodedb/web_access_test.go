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

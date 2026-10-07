package nodedb

import (
	"reflect"
	"strings"
	"testing"
)

func TestTheAddressBookIsKeptAcrossReopening(t *testing.T) {
	db, file := newDB(t)
	if peers, err := db.BootstrapPeers(); err != nil || len(peers) != 0 {
		t.Fatalf("a new address book: %v, %v", peers, err)
	}
	b1 := BootstrapPeer{"12D3KooWb", "/ip4/10.0.0.2/tcp/7700"}
	a1 := BootstrapPeer{"12D3KooWa", "/ip4/10.0.0.1/tcp/7700"}
	a2 := BootstrapPeer{"12D3KooWa", "/dns/rig.example.net/tcp/7700"}
	// Entered twice, an entry is there once.
	for _, p := range []BootstrapPeer{b1, a1, a1, a2} {
		if err := db.AddBootstrapPeer(p); err != nil {
			t.Fatal(err)
		}
	}
	db = reopen(t, db, file)
	got, err := db.BootstrapPeers()
	if err != nil || !reflect.DeepEqual(got, []BootstrapPeer{a2, a1, b1}) {
		t.Fatalf("the address book after reopening: %v, %v", got, err)
	}

	// Setting it replaces all of it.
	c1 := BootstrapPeer{"12D3KooWc", "/ip6/2001:db8::1/tcp/7700"}
	if err := db.SetBootstrapPeers([]BootstrapPeer{c1, c1, a1}); err != nil {
		t.Fatal(err)
	}
	if got, _ := db.BootstrapPeers(); !reflect.DeepEqual(got, []BootstrapPeer{a1, c1}) {
		t.Errorf("after replacing it: %v", got)
	}
	if err := db.SetBootstrapPeers(nil); err != nil {
		t.Fatal(err)
	}
	if got, _ := db.BootstrapPeers(); len(got) != 0 {
		t.Errorf("after emptying it: %v", got)
	}
}

func TestAddressBookFailuresAreReported(t *testing.T) {
	db, _ := newDB(t)
	loosen(t, db, "bootstrap_peers", "peer_id, address")
	if _, err := db.BootstrapPeers(); err == nil || !strings.Contains(err.Error(), "load bootstrap peers") {
		t.Errorf("with a damaged table: %v", err)
	}
	db.Close()
	if err := db.SetBootstrapPeers(nil); err == nil || !strings.Contains(err.Error(), "save bootstrap peers") {
		t.Errorf("SetBootstrapPeers on a closed database: %v", err)
	}
	if err := db.AddBootstrapPeer(BootstrapPeer{"a", "b"}); err == nil || !strings.Contains(err.Error(), "save bootstrap peer") {
		t.Errorf("AddBootstrapPeer on a closed database: %v", err)
	}
}

package api

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/ipfs/boxo/ipns"
	"github.com/ipfs/go-cid"

	"github.com/sisyphus-network/Sisyphus/packages/names"
	"github.com/sisyphus-network/Sisyphus/packages/sealed"
	"github.com/sisyphus-network/Sisyphus/packages/storage"
)

// noNames is the names of a node that holds none.
type noNames struct{}

func (noNames) Held(string) (*names.Record, error) { return nil, ErrNoRecord }

// shelf keeps the records of names in memory, and fails on request.
type shelf struct {
	records            map[string][]byte
	failLoad, failSave bool
}

func (s *shelf) NameRecord(name string) ([]byte, error) {
	if s.failLoad {
		return nil, errDisk
	}
	return s.records[name], nil
}

func (s *shelf) SetNameRecord(name string, record []byte, _ time.Time) error {
	if s.failSave {
		return errDisk
	}
	s.records[name] = record
	return nil
}

// everyone answers for any node.
func everyone(string) bool { return true }

func TestTheLatestRecordOfANameIsTheOneHeld(t *testing.T) {
	node, now := newIdentity(t), time.Now()
	ctx := context.Background()
	store := storage.NewMemory()
	first, _ := store.Put(ctx, strings.NewReader("first"))
	second, _ := store.Put(ctx, strings.NewReader("second"))
	record := func(value cid.Cid, sequence uint64, lifetime time.Duration) *names.Record {
		return names.Make(node, value, sequence, lifetime, time.Minute, now)
	}
	kept := &shelf{records: make(map[string][]byte)}
	held := NewNames(kept, everyone)
	var carried []uint64
	held.OnPublish(func(r *names.Record) { carried = append(carried, r.Sequence) })

	if _, err := held.Held(node.ID()); !errors.Is(err, ErrNoRecord) {
		t.Fatalf("before anything is published: %v", err)
	}
	for _, tt := range []struct {
		about   string
		offered *names.Record
		refused bool
		value   cid.Cid
	}{
		{"the first", record(first, 5, time.Hour), false, first},
		{"a newer one", record(second, 6, time.Hour), false, second},
		{"an older one", record(first, 5, 48*time.Hour), true, second},
		{"one as new but good for no longer", record(first, 6, time.Minute), true, second},
		{"the one held, again", record(second, 6, time.Hour), false, second},
		{"one as new and good for longer", record(first, 6, 2*time.Hour), false, first},
	} {
		err := held.Publish(tt.offered, now)
		if refused := errors.Is(err, ErrOlderRecord); refused != tt.refused || (err != nil && !refused) {
			t.Errorf("publishing %s: %v, want refused %v", tt.about, err, tt.refused)
		}
		if got, err := held.Held(node.ID()); err != nil || got.Value != tt.value {
			t.Errorf("after %s the name stands for %v, %v, want %s", tt.about, got, err, tt.value)
		}
	}
	// Each record taken in was passed on, and none that was refused.
	if len(carried) != 4 {
		t.Errorf("records passed on: %v", carried)
	}

	// A record of a node not answered for is not given out, though held.
	if _, err := NewNames(kept, func(string) bool { return false }).Held(node.ID()); !errors.Is(err, ErrNoRecord) {
		t.Errorf("the record of a node not answered for: %v", err)
	}

	// One that can no longer be read is as good as none, and gives way to
	// any that can.
	kept.records[node.ID()] = []byte("not a record")
	if _, err := held.Held(node.ID()); !errors.Is(err, ErrNoRecord) {
		t.Errorf("a record that cannot be read: %v", err)
	}
	if err := held.Publish(record(first, 0, time.Hour), now); err != nil {
		t.Errorf("publishing over a record that cannot be read: %v", err)
	}

	kept.failSave = true
	if err := held.Publish(record(first, 9, time.Hour), now); !errors.Is(err, errDisk) {
		t.Errorf("publishing when the record cannot be saved: %v", err)
	}
	kept.failLoad = true
	if err := held.Publish(record(first, 9, time.Hour), now); !errors.Is(err, errDisk) {
		t.Errorf("publishing when the record held cannot be read: %v", err)
	}
	if _, err := held.Held(node.ID()); !errors.Is(err, errDisk) {
		t.Errorf("the record when it cannot be loaded: %v", err)
	}
}

func TestWhatANameStandsForIsFetchedByTheNameWithAPlainGET(t *testing.T) {
	node, stranger, now := newIdentity(t), newIdentity(t), time.Now()
	ctx := context.Background()
	store := storage.NewMemory()
	page, _ := store.Put(ctx, strings.NewReader("<html><body>the boulder rolls</body></html>"))
	secret, _ := store.Put(ctx, sealed.Encrypt(sealed.NewKey(), strings.NewReader("for the owner's eyes")))
	kept := &shelf{records: make(map[string][]byte)}
	held := NewNames(kept, everyone)
	published := names.Make(node, page, 1, time.Hour, 5*time.Minute, now)
	if err := held.Publish(published, now); err != nil {
		t.Fatal(err)
	}
	owners := NewGateway(store, held, "the-token", false)
	token := []string{"Authorization", "Bearer the-token"}
	// As IPFS programs spell the name.
	spelled := mustSpell(t, node.ID())

	for _, name := range []string{node.ID(), spelled} {
		status, body, header := fetchFrom(owners, http.MethodGet, "/ipns/"+name, token...)
		if status != http.StatusOK || !strings.Contains(body, "the boulder rolls") || !strings.HasPrefix(header.Get("Content-Type"), "text/html") ||
			header.Get("X-Ipfs-Path") != "/ipns/"+name || header.Get("X-Ipfs-Roots") != page.String() || header.Get("Etag") != `"`+page.String()+`"` {
			t.Fatalf("GET by name %s: %d %q %v", name, status, body, header)
		}
		// What a name stands for changes, so it is kept only as long as
		// its record allows.
		if keep := header.Get("Cache-Control"); strings.Contains(keep, "immutable") || !(keep == "public, max-age=300" || keep == "public, max-age=299") {
			t.Errorf("a name's answer may be kept: %q", keep)
		}
	}
	if status, body, header := fetchFrom(owners, http.MethodHead, "/ipns/"+node.ID()+"?token=the-token"); status != http.StatusOK || body != "" || header.Get("Content-Length") != "43" {
		t.Errorf("HEAD by name: %d %q %v", status, body, header)
	}

	// Asked for the record, the gateway gives it as its node signed it,
	// which whoever asked can check without trusting the gateway.
	for about, ask := range map[string][]string{
		"in the Accept header": {"/ipns/" + node.ID(), "Accept", "application/vnd.ipfs.ipns-record"},
		"in the address":       {"/ipns/" + node.ID() + "?format=ipns-record"},
	} {
		status, body, header := fetchFrom(owners, http.MethodGet, ask[0], append(ask[1:], token...)...)
		if status != http.StatusOK || header.Get("Content-Type") != "application/vnd.ipfs.ipns-record" || strings.Contains(header.Get("Cache-Control"), "immutable") ||
			header.Get("Etag") == "" || !strings.Contains(header.Get("Content-Disposition"), node.ID()+".ipns-record") {
			t.Fatalf("the record, asked for %s: %d %v", about, status, header)
		}
		got, err := names.Check(node.ID(), []byte(body), time.Now())
		if err != nil || got.Value != page || got.Sequence != 1 {
			t.Errorf("the record, asked for %s: %+v, %v", about, got, err)
		}
	}

	// A name pointed at a sealed file gives the file to nobody, and the
	// record, which only says where it points, as before.
	if err := held.Publish(names.Make(node, secret, 2, time.Hour, time.Minute, now), now); err != nil {
		t.Fatal(err)
	}
	if status, body, _ := fetchFrom(owners, http.MethodGet, "/ipns/"+node.ID(), token...); status != http.StatusForbidden || !strings.Contains(body, "sealed") {
		t.Errorf("a name pointing at a sealed file: %d %q", status, body)
	}
	if status, _, _ := fetchFrom(owners, http.MethodGet, "/ipns/"+node.ID(), append([]string{"Accept", names.ContentType}, token...)...); status != http.StatusOK {
		t.Errorf("the record of a name pointing at a sealed file: %d", status)
	}

	// Another node's record expired, and a third never published.
	late := newIdentity(t)
	if err := held.Publish(names.Make(late, page, 1, time.Hour, time.Minute, now.Add(-2*time.Hour)), now); err != nil {
		t.Fatal(err)
	}
	for about, tt := range map[string]struct {
		path, key string
		status    int
		want      string
	}{
		"without the token":               {"/ipns/" + node.ID(), "", http.StatusUnauthorized, "show the node's API token"},
		"for the record without it":       {"/ipns/" + node.ID() + "?format=ipns-record", "", http.StatusUnauthorized, "show the node's API token"},
		"for a name nobody published":     {"/ipns/" + stranger.ID(), "the-token", http.StatusNotFound, "holds no record for that name"},
		"for a name whose record expired": {"/ipns/" + late.ID(), "the-token", http.StatusNotFound, "expired at"},
		"for an expired record itself":    {"/ipns/" + late.ID() + "?format=ipns-record", "the-token", http.StatusNotFound, "expired at"},
		"for what is no name":             {"/ipns/rig", "the-token", http.StatusBadRequest, "not a name"},
		"for a path under a name":         {"/ipns/" + node.ID() + "/index.html", "the-token", http.StatusNotFound, "/ipns/<node ID>"},
		"for no name at all":              {"/ipns/", "the-token", http.StatusNotFound, "/ipns/<node ID>"},
	} {
		if status, body, _ := fetchFrom(owners, http.MethodGet, tt.path, "Authorization", "Bearer "+tt.key); status != tt.status || !strings.Contains(body, tt.want) {
			t.Errorf("asked %s: %d %q, want %d and %q", about, status, body, tt.status, tt.want)
		}
	}
	kept.failLoad = true
	if status, body, _ := fetchFrom(owners, http.MethodGet, "/ipns/"+node.ID(), token...); status != http.StatusInternalServerError || !strings.Contains(body, "disk on fire") {
		t.Errorf("when the record cannot be loaded: %d %q", status, body)
	}
	kept.failLoad = false

	// Opened, a gateway answers for its names to anyone.
	anyones := NewGateway(store, held, "the-token", true)
	if status, _, header := fetchFrom(anyones, http.MethodGet, "/ipns/"+node.ID()+"?format=ipns-record"); status != http.StatusOK || header.Get("Access-Control-Allow-Origin") != "*" {
		t.Errorf("the record from an open gateway: %d %v", status, header)
	}
	if status, _, _ := fetchFrom(anyones, http.MethodGet, "/ipns/"+node.ID()); status != http.StatusForbidden {
		t.Errorf("a name pointing at a sealed file, from an open gateway: %d", status)
	}
}

// mustSpell returns a node's name as IPFS programs print it.
func mustSpell(t *testing.T, id string) string {
	t.Helper()
	spelled, err := ipns.NameFromString(id)
	if err != nil {
		t.Fatal(err)
	}
	return spelled.String()
}

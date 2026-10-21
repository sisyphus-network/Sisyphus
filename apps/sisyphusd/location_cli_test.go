package main

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"

	"github.com/sisyphus-network/Sisyphus/packages/nodedb"
	nodepb "github.com/sisyphus-network/Sisyphus/packages/protocol/sisyphus/node/v1"
)

// A location given while the node is stopped is in its info once it runs,
// and one given through its API is there at once.
func TestARunningNodeGivesItsOwnersLocation(t *testing.T) {
	dataDir := t.TempDir()
	mustCLI(t, "location", "set", "--data-dir", dataDir, "--lat", "48.8566", "--lon", "2.3522", "--share", "city")
	addr, apiAddr := freeAddr(t), freeAddr(t)
	startDaemon(t, "--data-dir", dataDir, "--listen", addr, "--slots", "1", "--api-listen", apiAddr)
	client := desktop(t, apiAddr)
	poolAsSeenBy(t, client)
	ctx := tokenOf(t, dataDir)
	info, err := client.GetNodeInfo(ctx, &nodepb.GetNodeInfoRequest{})
	if location := info.GetOwnerLocation(); err != nil || location.GetLatitude() != 48.8566 || location.GetLongitude() != 2.3522 || location.GetShare() != nodepb.LocationShare_LOCATION_SHARE_CITY {
		t.Fatalf("node info %v, %v", info, err)
	}
	if info, err := client.GetNodeInfo(context.Background(), &nodepb.GetNodeInfoRequest{}); err != nil || info.GetOwnerLocation() != nil {
		t.Fatalf("node info without the token %v, %v", info, err)
	}
	if _, err := client.SetNodeLocation(ctx, &nodepb.SetNodeLocationRequest{Location: &nodepb.NodeLocation{Latitude: 1, Longitude: 2}}); err != nil {
		t.Fatal(err)
	}
	got, err := client.GetNodeLocation(ctx, &nodepb.GetNodeLocationRequest{})
	if location := got.GetLocation(); err != nil || location.GetLatitude() != 1 || location.GetShare() != nodepb.LocationShare_LOCATION_SHARE_COUNTRY {
		t.Fatalf("location set through the API %v, %v", got, err)
	}
}

// The pool sees as much of a node's location as its owner shares, rounded
// as the owner chose, from when the node connects and again at once when
// the owner changes it through the API, without a restart.
func TestAPoolSeesAsMuchOfAMembersPinAsItsOwnerShares(t *testing.T) {
	dataDir := t.TempDir()
	mustCLI(t, "location", "set", "--data-dir", dataDir, "--lat", "48.8566", "--lon", "2.3522", "--share", "city")
	addr, apiAddr := freeAddr(t), freeAddr(t)
	startDaemon(t, "--data-dir", dataDir, "--listen", addr, "--name", "rig", "--slots", "1", "--api-listen", apiAddr)
	client := desktop(t, apiAddr)
	ctx := tokenOf(t, dataDir)
	// sees waits until the node, as the pool lists it, is shared to be
	// where want says, or nowhere if want is nil.
	sees := func(ctx context.Context, want *nodepb.SharedLocation) {
		t.Helper()
		waitFor(t, func() bool {
			listed, err := client.ListWorkers(ctx, &nodepb.ListWorkersRequest{})
			workers := listed.GetWorkers()
			return err == nil && len(workers) == 1 && proto.Equal(workers[0].GetSharedLocation(), want)
		})
	}
	sees(ctx, &nodepb.SharedLocation{Latitude: 48.9, Longitude: 2.4})
	// And so does the command line, below its table.
	waitForOutput(t, "rig: shared place 48.9, 2.4\n", "nodes", "--addr", addr)
	// Without the token, the list says nothing of where anyone is.
	sees(context.Background(), nil)

	set := func(latitude, longitude float64, share nodepb.LocationShare) {
		t.Helper()
		if _, err := client.SetNodeLocation(ctx, &nodepb.SetNodeLocationRequest{Location: &nodepb.NodeLocation{Latitude: latitude, Longitude: longitude, Share: share}}); err != nil {
			t.Fatal(err)
		}
	}
	set(-33.86785, 151.20732, nodepb.LocationShare_LOCATION_SHARE_ABOUT_1KM)
	sees(ctx, &nodepb.SharedLocation{Latitude: -33.87, Longitude: 151.21})
	set(-33.86785, 151.20732, nodepb.LocationShare_LOCATION_SHARE_EXACT)
	sees(ctx, &nodepb.SharedLocation{Latitude: -33.86785, Longitude: 151.20732})
	set(-33.86785, 151.20732, nodepb.LocationShare_LOCATION_SHARE_COUNTRY)
	sees(ctx, nil)
	set(-33.86785, 151.20732, nodepb.LocationShare_LOCATION_SHARE_CITY)
	sees(ctx, &nodepb.SharedLocation{Latitude: -33.9, Longitude: 151.2})
	if _, err := client.SetNodeLocation(ctx, &nodepb.SetNodeLocationRequest{}); err != nil {
		t.Fatal(err)
	}
	sees(ctx, nil)
}

// What a worker sends of where it is comes from the owner's saved pin,
// rounded to the share the owner chose, and is nothing when it cannot be
// read.
func TestWhatAWorkerSharesOfItsOwnersPin(t *testing.T) {
	for _, tt := range []struct {
		saved    ownerLocationStub
		lat, lon float64
		ok       bool
	}{
		{ownerLocationStub{location: nodedb.OwnerLocation{Latitude: 51.50735, Longitude: -0.12776, Share: "about-1km"}, set: true}, 51.51, -0.13, true},
		{ownerLocationStub{location: nodedb.OwnerLocation{Latitude: 51.50735, Longitude: -0.12776, Share: "nothing"}, set: true}, 0, 0, false},
		{ownerLocationStub{}, 0, 0, false},
		{ownerLocationStub{location: nodedb.OwnerLocation{Latitude: 51.50735, Longitude: -0.12776, Share: "exact"}, set: true, err: errors.New("disk on fire")}, 0, 0, false},
	} {
		lat, lon, ok := sharedLocation(tt.saved)()
		if lat != tt.lat || lon != tt.lon || ok != tt.ok {
			t.Errorf("%+v is shared as %v, %v, %v; want %v, %v, %v", tt.saved, lat, lon, ok, tt.lat, tt.lon, tt.ok)
		}
	}
}

type ownerLocationStub struct {
	location nodedb.OwnerLocation
	set      bool
	err      error
}

func (s ownerLocationStub) OwnerLocation() (nodedb.OwnerLocation, bool, error) {
	return s.location, s.set, s.err
}

func TestTheOwnerSetsShowsAndClearsTheLocationFromTheCommandLine(t *testing.T) {
	dataDir := t.TempDir()
	if out := mustCLI(t, "location", "show", "--data-dir", dataDir); !strings.Contains(out, "no location has been set") {
		t.Fatalf("show before set: %q", out)
	}
	if out := mustCLI(t, "location", "set", "--data-dir", dataDir, "--lat", "-33.86785", "--lon", "151.20732"); !strings.Contains(out, "this node is at -33.86785, 151.20732") || !strings.Contains(out, "its pools are given no coordinates") {
		t.Fatalf("set: %q", out)
	}
	// Without --share, the privacy default.
	if out := mustCLI(t, "location", "show", "--data-dir", dataDir); out != "latitude:  -33.86785\nlongitude: 151.20732\nshare:     country\n" {
		t.Fatalf("show after set: %q", out)
	}
	if out := mustCLI(t, "location", "set", "--data-dir", dataDir, "--lat", "51.5", "--lon", "-0.12", "--share", "about-1km"); !strings.Contains(out, "its pools see 51.5, -0.12 from when it next connects") {
		t.Fatalf("set with a share: %q", out)
	}
	if out := mustCLI(t, "location", "show", "--data-dir", dataDir); !strings.Contains(out, "longitude: -0.12\nshare:     about-1km") {
		t.Fatalf("show after replacing: %q", out)
	}
	if out := mustCLI(t, "location", "clear", "--data-dir", dataDir); !strings.Contains(out, "forgotten") {
		t.Fatalf("clear: %q", out)
	}
	if out := mustCLI(t, "location", "show", "--data-dir", dataDir); !strings.Contains(out, "no location has been set") {
		t.Fatalf("show after clear: %q", out)
	}
}

func TestTheLocationCommandRefusesWhatIsNoPlace(t *testing.T) {
	dataDir := t.TempDir()
	blocked := filepath.Join(t.TempDir(), "in-the-way")
	if err := os.WriteFile(blocked, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct {
		args []string
		want string
	}{
		{[]string{"location"}, "expected location show, set or clear"},
		{[]string{"location", "move", "--data-dir", dataDir}, "expected location show, set or clear"},
		{[]string{"location", "show", "--no-such-flag"}, "flag provided but not defined"},
		{[]string{"location", "show", "--data-dir", dataDir, "extra"}, `unexpected argument "extra"`},
		{[]string{"location", "set", "--data-dir", dataDir, "--lat", "1"}, "give the location as --lat and --lon"},
		{[]string{"location", "set", "--data-dir", dataDir, "--lon", "1"}, "give the location as --lat and --lon"},
		{[]string{"location", "set", "--data-dir", dataDir, "--lat", "90.5", "--lon", "0"}, "latitude must be"},
		{[]string{"location", "set", "--data-dir", dataDir, "--lat", "NaN", "--lon", "0"}, "latitude must be"},
		{[]string{"location", "set", "--data-dir", dataDir, "--lat", "0", "--lon", "-181"}, "longitude must be"},
		{[]string{"location", "set", "--data-dir", dataDir, "--lat", "0", "--lon", "Inf"}, "longitude must be"},
		{[]string{"location", "set", "--data-dir", dataDir, "--lat", "0", "--lon", "0", "--share", "street"}, `unknown share "street"`},
		{[]string{"location", "show", "--data-dir", filepath.Join(blocked, "sub")}, "not a directory"},
	} {
		if _, err := cli(t, tt.args...); err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("%v: error %v, want one containing %q", tt.args, err, tt.want)
		}
	}
	if out := mustCLI(t, "location", "show", "--data-dir", dataDir); !strings.Contains(out, "no location has been set") {
		t.Errorf("a refused location was saved: %q", out)
	}
	spoiled := t.TempDir()
	if err := os.Mkdir(filepath.Join(spoiled, "node.db"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := cli(t, "location", "show", "--data-dir", spoiled); err == nil || !strings.Contains(err.Error(), "open node database") {
		t.Errorf("with a database that cannot be opened: %v", err)
	}
	// And one whose location cannot be read or written.
	damaged := t.TempDir()
	withoutOwnerLocation(t, filepath.Join(damaged, "node.db"))
	for _, tt := range []struct {
		args []string
		want string
	}{
		{[]string{"location", "show", "--data-dir", damaged}, "load owner location"},
		{[]string{"location", "set", "--data-dir", damaged, "--lat", "0", "--lon", "0"}, "save owner location"},
		{[]string{"location", "clear", "--data-dir", damaged}, "clear owner location"},
	} {
		if _, err := cli(t, tt.args...); err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("%v: error %v, want one containing %q", tt.args, err, tt.want)
		}
	}
}

// withoutOwnerLocation makes a node database at file whose owner_location table is
// gone.
func withoutOwnerLocation(t *testing.T, file string) {
	t.Helper()
	db, err := nodedb.Open(file)
	if err != nil {
		t.Fatal(err)
	}
	db.Close()
	raw, err := sql.Open("sqlite", file)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	if _, err := raw.Exec(`DROP TABLE owner_location`); err != nil {
		t.Fatal(err)
	}
}

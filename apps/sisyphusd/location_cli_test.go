package main

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"

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

func TestTheOwnerSetsShowsAndClearsTheLocationFromTheCommandLine(t *testing.T) {
	dataDir := t.TempDir()
	if out := mustCLI(t, "location", "show", "--data-dir", dataDir); !strings.Contains(out, "no location has been set") {
		t.Fatalf("show before set: %q", out)
	}
	if out := mustCLI(t, "location", "set", "--data-dir", dataDir, "--lat", "-33.86785", "--lon", "151.20732"); !strings.Contains(out, "this node is at -33.86785, 151.20732") || !strings.Contains(out, "nothing is sent to the pool yet") {
		t.Fatalf("set: %q", out)
	}
	// Without --share, the privacy default.
	if out := mustCLI(t, "location", "show", "--data-dir", dataDir); out != "latitude:  -33.86785\nlongitude: 151.20732\nshare:     country\n" {
		t.Fatalf("show after set: %q", out)
	}
	mustCLI(t, "location", "set", "--data-dir", dataDir, "--lat", "51.5", "--lon", "-0.12", "--share", "about-1km")
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

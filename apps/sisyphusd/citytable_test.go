package main

import (
	"bytes"
	"compress/gzip"
	"errors"
	"flag"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"path/filepath"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/sisyphus-network/Sisyphus/packages/geo/citytable"
	nodepb "github.com/sisyphus-network/Sisyphus/packages/protocol/sisyphus/node/v1"
)

// cityTableCSV is a city table in DB-IP's form, with Google's resolver's
// block in Mountain View.
const cityTableCSV = `8.8.8.0,8.8.8.255,NA,US,California,"Mountain View",37.422,-122.085
`

// publishCityTable has the city table downloaded from a server of the
// test's, which publishes cityTableCSV as every month's, and failing with
// status if it is not 200.
func publishCityTable(t *testing.T, code int) {
	t.Helper()
	var table bytes.Buffer
	z := gzip.NewWriter(&table)
	z.Write([]byte(cityTableCSV))
	z.Close()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(code)
		w.Write(table.Bytes())
	}))
	t.Cleanup(server.Close)
	old := cityTableURL
	cityTableURL = server.URL + "/dbip-city-lite-%s.csv.gz"
	t.Cleanup(func() { cityTableURL = old })
}

func TestTheOwnerDownloadsDescribesAndRemovesTheCityTableFromTheCommandLine(t *testing.T) {
	publishCityTable(t, http.StatusOK)
	dataDir := t.TempDir()
	if out := mustCLI(t, "location", "city-table", "status", "--data-dir", dataDir); out != "no city table is downloaded\n" {
		t.Fatalf("status before downloading: %q", out)
	}
	out := mustCLI(t, "location", "city-table", "download", "--data-dir", dataDir)
	for _, want := range []string{"db-ip.com, which sees this machine's address", "edition:    20", "size:       0.0 MB", "IP Geolocation by DB-IP, https://db-ip.com"} {
		if !strings.Contains(out, want) {
			t.Errorf("download said %q; want %q in it", out, want)
		}
	}
	if out := mustCLI(t, "location", "city-table", "status", "--data-dir", dataDir); !strings.Contains(out, "edition:") || !strings.Contains(out, "IP Geolocation by DB-IP") {
		t.Fatalf("status after downloading: %q", out)
	}
	if out := mustCLI(t, "location", "city-table", "remove", "--data-dir", dataDir); out != "no city table is downloaded\n" {
		t.Fatalf("remove: %q", out)
	}
	if out, err := cli(t, "location", "city-table", "download", "-h"); !errors.Is(err, flag.ErrHelp) || !strings.Contains(out, "Downloading it shows db-ip.com this machine's address") || !strings.Contains(out, "-data-dir") {
		t.Errorf("help: %q, %v", out, err)
	}
}

func TestTheCityTableCommandRefusesWhatItDoesNotKnow(t *testing.T) {
	publishCityTable(t, http.StatusInternalServerError)
	dataDir := t.TempDir()
	for args, want := range map[string]string{
		"location city-table":               "expected location city-table download, status or remove",
		"location city-table fetch":         "expected location city-table download, status or remove",
		"location city-table status now":    `unexpected argument "now"`,
		"location city-table status --fast": "flag provided but not defined",
		"location city-table download":      "500 Internal Server Error",
		"location city-table status --data-dir " + filepath.Join(dataDir, "x", "y"): "",
	} {
		args := strings.Fields(args)
		if len(args) > 2 && !strings.Contains(strings.Join(args, " "), "--data-dir") {
			args = append(args, "--data-dir", dataDir)
		}
		out, err := cli(t, args...)
		if want == "" {
			if err != nil || out != "no city table is downloaded\n" {
				t.Errorf("%v: %q, %v", args, out, err)
			}
			continue
		}
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%v: %v; want %q", args, err, want)
		}
	}
}

// The node finds its city by the address that placed it in its country,
// and only once the owner has downloaded the table.
func TestTheNodeFindsItsCityByTheAddressThatPlacedIt(t *testing.T) {
	publishCityTable(t, http.StatusOK)
	oldLocal := interfacePrefixes
	interfacePrefixes = func() []netip.Prefix { return nil }
	defer func() { interfacePrefixes = oldLocal }()
	store := citytable.Store{Dir: t.TempDir(), URL: cityTableURL}
	own := []string{"/ip4/192.168.1.4/tcp/7700"}
	w, _ := placing(t)
	w.own = func() []string { return own }
	if place, source := w.city(); source != "" {
		t.Fatalf("found %+v by %q with no table kept", place, source)
	}
	w.cities = &citytable.Finder{Store: store}
	if place, source := w.city(); source != "" {
		t.Fatalf("found %+v by %q before the table was downloaded", place, source)
	}
	if _, err := store.Download(t.Context()); err != nil {
		t.Fatal(err)
	}
	if place, source := w.city(); source != "" {
		t.Fatalf("found %+v by %q with no public address", place, source)
	}
	own = []string{"/ip4/8.8.4.4/tcp/7700"}
	if place, source := w.city(); source != "" {
		t.Fatalf("found %+v by %q, an address the table does not place", place, source)
	}
	own = []string{"/ip4/192.168.1.4/tcp/7700"}
	w.stunned.Store(netip.MustParseAddr("8.8.8.8"))
	want := citytable.Place{City: "Mountain View", Region: "California", Country: "US", Latitude: 37.422, Longitude: -122.085}
	if place, source := w.city(); place != want || source != "stun" {
		t.Fatalf("found %+v by %q; want %+v by stun", place, source, want)
	}
}

// A running node's API downloads, describes and removes the table, and only
// for a caller with the token.
func TestARunningNodeDownloadsTheCityTableWhenAsked(t *testing.T) {
	publishCityTable(t, http.StatusOK)
	dataDir := t.TempDir()
	addr, apiAddr := freeAddr(t), freeAddr(t)
	startDaemon(t, "--data-dir", dataDir, "--listen", addr, "--slots", "1", "--api-listen", apiAddr)
	client := desktop(t, apiAddr)
	poolAsSeenBy(t, client)
	state, err := client.GetCityTable(t.Context(), &nodepb.GetCityTableRequest{})
	if err != nil || state.GetInstalled() || state.GetAttribution() != "IP Geolocation by DB-IP" || state.GetAttributionUrl() != "https://db-ip.com" {
		t.Fatalf("before downloading: %v, %v", state, err)
	}
	if _, err := client.DownloadCityTable(t.Context(), &nodepb.DownloadCityTableRequest{}); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("downloaded without the token: %v", err)
	}
	if _, err := client.RemoveCityTable(t.Context(), &nodepb.RemoveCityTableRequest{}); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("removed without the token: %v", err)
	}
	ctx := tokenOf(t, dataDir)
	state, err = client.DownloadCityTable(ctx, &nodepb.DownloadCityTableRequest{})
	if err != nil || !state.GetInstalled() || state.GetSizeBytes() == 0 || state.GetDownloadedAtMs() == 0 || len(state.GetEdition()) != len("2026-10") {
		t.Fatalf("downloaded: %v, %v", state, err)
	}
	if out := mustCLI(t, "location", "city-table", "status", "--data-dir", dataDir); !strings.Contains(out, "edition:    "+state.GetEdition()) {
		t.Fatalf("the command line sees %q", out)
	}
	if state, err = client.RemoveCityTable(ctx, &nodepb.RemoveCityTableRequest{}); err != nil || state.GetInstalled() {
		t.Fatalf("removed: %v, %v", state, err)
	}
}

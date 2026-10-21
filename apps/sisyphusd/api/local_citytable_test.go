package api

import (
	"context"
	"errors"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/sisyphus-network/Sisyphus/packages/geo/citytable"
	"github.com/sisyphus-network/Sisyphus/packages/nodedb"
	nodepb "github.com/sisyphus-network/Sisyphus/packages/protocol/sisyphus/node/v1"
)

type memoryCityTable struct {
	status                 citytable.Status
	statusErr, downloadErr error
	removeErr              error
	downloads, removals    int
}

func (m *memoryCityTable) Status() (citytable.Status, error) { return m.status, m.statusErr }

func (m *memoryCityTable) Download(context.Context) (citytable.Status, error) {
	if m.downloadErr != nil {
		return citytable.Status{}, m.downloadErr
	}
	m.downloads++
	m.status = citytable.Status{Installed: true, Edition: "2026-10", Bytes: 93_000_000, Downloaded: time.UnixMilli(1_790_000_000_000)}
	return m.status, nil
}

func (m *memoryCityTable) Remove() error {
	if m.removeErr != nil {
		return m.removeErr
	}
	m.removals++
	m.status = citytable.Status{}
	return nil
}

func TestTheCityTableIsDownloadedAndRemovedOnlyWithTheToken(t *testing.T) {
	table := &memoryCityTable{}
	changes := NewNodeInfoChanges()
	updates, unsubscribe := changes.subscribe()
	defer unsubscribe()
	client := serveLocal(t, LocalConfig{CityTable: table, NodeInfoChanges: changes})
	state, err := client.GetCityTable(context.Background(), &nodepb.GetCityTableRequest{})
	if err != nil || state.GetInstalled() || state.GetDownloadedAtMs() != 0 || state.GetAttribution() != citytable.Attribution || state.GetAttributionUrl() != citytable.AttributionURL {
		t.Fatalf("before downloading %v, %v", state, err)
	}
	if _, err := client.DownloadCityTable(context.Background(), &nodepb.DownloadCityTableRequest{}); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("download without the token: %v", err)
	}
	if _, err := client.RemoveCityTable(context.Background(), &nodepb.RemoveCityTableRequest{}); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("remove without the token: %v", err)
	}
	if table.downloads != 0 || table.removals != 0 {
		t.Fatalf("done without the token: %+v", table)
	}
	ctx := withToken("the-token")
	state, err = client.DownloadCityTable(ctx, &nodepb.DownloadCityTableRequest{})
	if err != nil || !state.GetInstalled() || state.GetEdition() != "2026-10" || state.GetSizeBytes() != 93_000_000 || state.GetDownloadedAtMs() != 1_790_000_000_000 || state.GetAttribution() != citytable.Attribution {
		t.Fatalf("downloaded %v, %v", state, err)
	}
	waitForChange(t, updates)
	if state, err = client.GetCityTable(context.Background(), &nodepb.GetCityTableRequest{}); err != nil || state.GetEdition() != "2026-10" {
		t.Fatalf("after downloading %v, %v", state, err)
	}
	state, err = client.RemoveCityTable(ctx, &nodepb.RemoveCityTableRequest{})
	if err != nil || state.GetInstalled() || state.GetAttribution() != citytable.Attribution || table.removals != 1 {
		t.Fatalf("removed %v, %v", state, err)
	}
	waitForChange(t, updates)
}

func waitForChange(t *testing.T, updates <-chan struct{}) {
	t.Helper()
	select {
	case <-updates:
	case <-time.After(5 * time.Second):
		t.Fatal("those watching the node were not told")
	}
}

func TestTheCityTableSaysWhyItCannot(t *testing.T) {
	ctx := withToken("the-token")
	none := serveLocal(t, LocalConfig{})
	if _, err := none.GetCityTable(ctx, &nodepb.GetCityTableRequest{}); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("get with no table: %v", err)
	}
	if _, err := none.DownloadCityTable(ctx, &nodepb.DownloadCityTableRequest{}); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("download with no table: %v", err)
	}
	if _, err := none.RemoveCityTable(ctx, &nodepb.RemoveCityTableRequest{}); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("remove with no table: %v", err)
	}
	failing := serveLocal(t, LocalConfig{CityTable: &memoryCityTable{
		statusErr: errors.New("unreadable"), downloadErr: errors.New("db-ip.com is down"), removeErr: errors.New("busy"),
	}})
	if _, err := failing.GetCityTable(ctx, &nodepb.GetCityTableRequest{}); status.Code(err) != codes.Internal {
		t.Errorf("get: %v", err)
	}
	if _, err := failing.DownloadCityTable(ctx, &nodepb.DownloadCityTableRequest{}); status.Code(err) != codes.Unavailable || status.Convert(err).Message() != "db-ip.com is down" {
		t.Errorf("download: %v", err)
	}
	if _, err := failing.RemoveCityTable(ctx, &nodepb.RemoveCityTableRequest{}); status.Code(err) != codes.Internal {
		t.Errorf("remove: %v", err)
	}
}

// Node info gives where the city table puts the node, with its credit,
// unless the owner has said where it is.
func TestNodeInfoGivesTheCityUnlessTheOwnerHasPinnedTheNode(t *testing.T) {
	mountainView := citytable.Place{City: "Mountain View", Region: "California", Country: "US", Latitude: 37.422, Longitude: -122.085}
	source := "pool"
	owner := &memoryLocation{}
	service := &localService{cfg: LocalConfig{OwnerLocation: owner, City: func() (citytable.Place, string) { return mountainView, source }}}
	info, err := service.GetNodeInfo(context.Background(), &nodepb.GetNodeInfoRequest{})
	want := &nodepb.CityEstimate{
		City: "Mountain View", Region: "California", CountryCode: "US", Latitude: 37.422, Longitude: -122.085, Source: "pool",
		Attribution: "IP Geolocation by DB-IP", AttributionUrl: "https://db-ip.com",
	}
	if got := info.GetCityEstimate(); err != nil || got.String() != want.String() {
		t.Fatalf("city %v, %v; want %v", got, err, want)
	}
	for name, change := range map[string]func(){
		"no place": func() { source = "" },
		"the owner's pin": func() {
			owner.location, owner.set = nodedb.OwnerLocation{Latitude: 1, Longitude: 2, Share: "city"}, true
		},
		"a pin that could not be read": func() { owner.err = errors.New("locked") },
	} {
		source, owner.set, owner.err = "pool", false, nil
		change()
		if info, err := service.GetNodeInfo(context.Background(), &nodepb.GetNodeInfoRequest{}); err != nil || info.GetCityEstimate() != nil {
			t.Errorf("%s: city %v, %v", name, info.GetCityEstimate(), err)
		}
	}
	// A node that keeps no pin at all still gives the city.
	alone := serveLocal(t, LocalConfig{City: func() (citytable.Place, string) { return mountainView, "stun" }})
	if info, err := alone.GetNodeInfo(context.Background(), &nodepb.GetNodeInfoRequest{}); err != nil || info.GetCityEstimate().GetSource() != "stun" {
		t.Errorf("without a pin store: %v, %v", info, err)
	}
}

package api

import (
	"context"
	"errors"
	"math"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/sisyphus-network/Sisyphus/packages/nodedb"
	nodepb "github.com/sisyphus-network/Sisyphus/packages/protocol/sisyphus/node/v1"
)

type memoryLocation struct {
	location nodedb.OwnerLocation
	set      bool
	err      error
}

func (m *memoryLocation) OwnerLocation() (nodedb.OwnerLocation, bool, error) {
	return m.location, m.set, m.err
}

func (m *memoryLocation) SetOwnerLocation(location nodedb.OwnerLocation) error {
	if m.err != nil {
		return m.err
	}
	m.location, m.set = location, true
	return nil
}

func (m *memoryLocation) ClearOwnerLocation() error {
	if m.err != nil {
		return m.err
	}
	m.location, m.set = nodedb.OwnerLocation{}, false
	return nil
}

func TestTheOwnerSetsReadsAndClearsTheNodesLocation(t *testing.T) {
	db, err := nodedb.Open(filepath.Join(t.TempDir(), "node.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	client := serveLocal(t, LocalConfig{OwnerLocation: db})
	if _, err := client.GetNodeLocation(context.Background(), &nodepb.GetNodeLocationRequest{}); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("Get without token = %v", err)
	}
	if _, err := client.SetNodeLocation(context.Background(), &nodepb.SetNodeLocationRequest{}); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("Set without token = %v", err)
	}
	ctx := withToken("the-token")
	if got, err := client.GetNodeLocation(ctx, &nodepb.GetNodeLocationRequest{}); err != nil || got.GetLocation() != nil {
		t.Fatalf("no location yet = %v, %v", got, err)
	}
	set, err := client.SetNodeLocation(ctx, &nodepb.SetNodeLocationRequest{Location: &nodepb.NodeLocation{
		Latitude: 51.50735, Longitude: -0.12776, Share: nodepb.LocationShare_LOCATION_SHARE_ABOUT_1KM,
	}})
	want := &nodepb.NodeLocation{Latitude: 51.50735, Longitude: -0.12776, Share: nodepb.LocationShare_LOCATION_SHARE_ABOUT_1KM}
	if err != nil || !sameLocation(set.GetLocation(), want) {
		t.Fatalf("set location = %v, %v", set, err)
	}
	if got, err := client.GetNodeLocation(ctx, &nodepb.GetNodeLocationRequest{}); err != nil || !sameLocation(got.GetLocation(), want) {
		t.Fatalf("read location = %v, %v", got, err)
	}
	if saved, _, _ := db.OwnerLocation(); saved != (nodedb.OwnerLocation{Latitude: 51.50735, Longitude: -0.12776, Share: "about-1km"}) {
		t.Fatalf("saved location = %+v", saved)
	}
	// Every share is saved as itself, except that none is the privacy
	// default.
	for given, saved := range map[nodepb.LocationShare]nodepb.LocationShare{
		nodepb.LocationShare_LOCATION_SHARE_UNSPECIFIED: nodepb.LocationShare_LOCATION_SHARE_COUNTRY,
		nodepb.LocationShare_LOCATION_SHARE_NOTHING:     nodepb.LocationShare_LOCATION_SHARE_NOTHING,
		nodepb.LocationShare_LOCATION_SHARE_COUNTRY:     nodepb.LocationShare_LOCATION_SHARE_COUNTRY,
		nodepb.LocationShare_LOCATION_SHARE_CITY:        nodepb.LocationShare_LOCATION_SHARE_CITY,
		nodepb.LocationShare_LOCATION_SHARE_ABOUT_1KM:   nodepb.LocationShare_LOCATION_SHARE_ABOUT_1KM,
		nodepb.LocationShare_LOCATION_SHARE_EXACT:       nodepb.LocationShare_LOCATION_SHARE_EXACT,
	} {
		if _, err := client.SetNodeLocation(ctx, &nodepb.SetNodeLocationRequest{Location: &nodepb.NodeLocation{Latitude: -90, Longitude: 180, Share: given}}); err != nil {
			t.Fatalf("set share %v: %v", given, err)
		}
		if got, err := client.GetNodeLocation(ctx, &nodepb.GetNodeLocationRequest{}); err != nil || got.GetLocation().GetShare() != saved {
			t.Errorf("share %v read back as %v, %v; want %v", given, got, err, saved)
		}
	}
	cleared, err := client.SetNodeLocation(ctx, &nodepb.SetNodeLocationRequest{})
	if err != nil || cleared.GetLocation() != nil {
		t.Fatalf("clear = %v, %v", cleared, err)
	}
	if _, set, err := db.OwnerLocation(); err != nil || set {
		t.Fatalf("after clearing: set %v, err %v", set, err)
	}
}

func sameLocation(got, want *nodepb.NodeLocation) bool {
	return got != nil && got.GetLatitude() == want.GetLatitude() && got.GetLongitude() == want.GetLongitude() && got.GetShare() == want.GetShare()
}

func TestTheNodesLocationIsValidatedAndUnavailableStoresAreReported(t *testing.T) {
	ctx := withToken("the-token")
	without := serveLocal(t, LocalConfig{})
	if _, err := without.GetNodeLocation(ctx, &nodepb.GetNodeLocationRequest{}); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("Get without store = %v", err)
	}
	if _, err := without.SetNodeLocation(ctx, &nodepb.SetNodeLocationRequest{}); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("Set without store = %v", err)
	}
	store := &memoryLocation{}
	client := serveLocal(t, LocalConfig{OwnerLocation: store})
	for _, test := range []struct {
		name     string
		location *nodepb.NodeLocation
		want     string
	}{
		{"latitude past a pole", &nodepb.NodeLocation{Latitude: 90.01}, "latitude"},
		{"latitude past the other pole", &nodepb.NodeLocation{Latitude: -90.01}, "latitude"},
		{"latitude not a number", &nodepb.NodeLocation{Latitude: math.NaN()}, "latitude"},
		{"latitude infinite", &nodepb.NodeLocation{Latitude: math.Inf(1)}, "latitude"},
		{"longitude past the antimeridian", &nodepb.NodeLocation{Longitude: 180.01}, "longitude"},
		{"longitude past it the other way", &nodepb.NodeLocation{Longitude: -180.01}, "longitude"},
		{"longitude not a number", &nodepb.NodeLocation{Longitude: math.NaN()}, "longitude"},
		{"longitude infinite", &nodepb.NodeLocation{Longitude: math.Inf(-1)}, "longitude"},
		{"unknown share", &nodepb.NodeLocation{Share: nodepb.LocationShare(42)}, "unknown location share 42"},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := client.SetNodeLocation(ctx, &nodepb.SetNodeLocationRequest{Location: test.location})
			if status.Code(err) != codes.InvalidArgument || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Set invalid location = %v", err)
			}
		})
	}
	if store.set {
		t.Errorf("an invalid location was saved: %+v", store.location)
	}
	// A share the node does not know, which the database would not hold,
	// is read as the privacy default.
	store.location, store.set = nodedb.OwnerLocation{Latitude: 1, Longitude: 2, Share: "street"}, true
	if got, err := client.GetNodeLocation(ctx, &nodepb.GetNodeLocationRequest{}); err != nil || got.GetLocation().GetShare() != nodepb.LocationShare_LOCATION_SHARE_COUNTRY {
		t.Errorf("unknown saved share = %v, %v", got, err)
	}
	broken := serveLocal(t, LocalConfig{OwnerLocation: &memoryLocation{err: errors.New("disk gone")}})
	if _, err := broken.GetNodeLocation(ctx, &nodepb.GetNodeLocationRequest{}); status.Code(err) != codes.Internal {
		t.Errorf("Get from broken store = %v", err)
	}
	if _, err := broken.SetNodeLocation(ctx, &nodepb.SetNodeLocationRequest{Location: &nodepb.NodeLocation{}}); status.Code(err) != codes.Internal {
		t.Errorf("Set to broken store = %v", err)
	}
	if _, err := broken.SetNodeLocation(ctx, &nodepb.SetNodeLocationRequest{}); status.Code(err) != codes.Internal {
		t.Errorf("Clear in broken store = %v", err)
	}
	if info, err := broken.GetNodeInfo(ctx, &nodepb.GetNodeInfoRequest{}); err != nil || info.GetOwnerLocation() != nil {
		t.Errorf("node info with a broken store = %v, %v", info, err)
	}
}

// The owner's location is in the node's info at once, for those who hold
// the node's token, and every watcher of the node's state is told of it.
func TestTheNodesLocationShowsInItsInfoAtOnceAndOnlyToTheOwner(t *testing.T) {
	changes := NewNodeInfoChanges()
	client := serveLocal(t, LocalConfig{OwnerLocation: &memoryLocation{}, NodeInfoChanges: changes})
	owner := withToken("the-token")
	ctx, cancel := context.WithCancel(owner)
	defer cancel()
	watching, err := client.WatchNodeState(ctx, &nodepb.GetNodeInfoRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if first, err := watching.Recv(); err != nil || first.GetInfo().GetOwnerLocation() != nil {
		t.Fatalf("first state = %v, %v", first, err)
	}
	strangerCtx, stopStranger := context.WithCancel(context.Background())
	defer stopStranger()
	stranger, err := client.WatchNodeState(strangerCtx, &nodepb.GetNodeInfoRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if first, err := stranger.Recv(); err != nil || first.GetInfo().GetOwnerLocation() != nil {
		t.Fatalf("stranger's first state = %v, %v", first, err)
	}

	want := &nodepb.NodeLocation{Latitude: -33.86785, Longitude: 151.20732, Share: nodepb.LocationShare_LOCATION_SHARE_EXACT}
	if _, err := client.SetNodeLocation(owner, &nodepb.SetNodeLocationRequest{Location: want}); err != nil {
		t.Fatal(err)
	}
	if got := recvWithin(t, watching); !sameLocation(got.GetInfo().GetOwnerLocation(), want) || got.GetRevision() != 2 {
		t.Fatalf("state after setting = %v", got)
	}
	if info, err := client.GetNodeInfo(owner, &nodepb.GetNodeInfoRequest{}); err != nil || !sameLocation(info.GetOwnerLocation(), want) {
		t.Fatalf("owner's node info = %v, %v", info, err)
	}
	if info, err := client.GetNodeInfo(context.Background(), &nodepb.GetNodeInfoRequest{}); err != nil || info.GetOwnerLocation() != nil || info.GetPeerId() == "" {
		t.Fatalf("stranger's node info = %v, %v", info, err)
	}

	if _, err := client.SetNodeLocation(owner, &nodepb.SetNodeLocationRequest{}); err != nil {
		t.Fatal(err)
	}
	if got := recvWithin(t, watching); got.GetInfo().GetOwnerLocation() != nil || got.GetRevision() != 3 {
		t.Fatalf("state after clearing = %v", got)
	}
	// The stranger saw nothing change, so was sent nothing more.
	stopStranger()
	if extra, err := stranger.Recv(); err == nil {
		t.Fatalf("the stranger was sent %v", extra)
	}
}

func recvWithin(t *testing.T, stream interface {
	Recv() (*nodepb.NodeState, error)
}) *nodepb.NodeState {
	t.Helper()
	got := make(chan *nodepb.NodeState, 1)
	failed := make(chan error, 1)
	go func() {
		state, err := stream.Recv()
		if err != nil {
			failed <- err
			return
		}
		got <- state
	}()
	select {
	case state := <-got:
		return state
	case err := <-failed:
		t.Fatal(err)
	case <-time.After(eventualWait):
		t.Fatal("no node state came")
	}
	return nil
}

package api

import (
	"context"
	"errors"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/sisyphus-network/Sisyphus/packages/nodedb"
	nodepb "github.com/sisyphus-network/Sisyphus/packages/protocol/sisyphus/node/v1"
)

type memoryDiscovery struct {
	settings nodedb.LocationDiscovery
	set      bool
	err      error
}

func (m *memoryDiscovery) LocationDiscovery() (nodedb.LocationDiscovery, bool, error) {
	return m.settings, m.set, m.err
}

func (m *memoryDiscovery) SetLocationDiscovery(settings nodedb.LocationDiscovery) error {
	if m.err != nil {
		return m.err
	}
	m.settings, m.set = settings, true
	return nil
}

func TestLocationDiscoverySettingsAreAuthorizedAndPersisted(t *testing.T) {
	store := &memoryDiscovery{}
	running := nodedb.LocationDiscovery{LocateGateway: true}
	client := serveLocal(t, LocalConfig{LocationDiscovery: store, LocationDiscoveryRunning: running, GatewayDiscoverySupported: true})
	if _, err := client.GetLocationDiscoverySettings(context.Background(), &nodepb.GetLocationDiscoverySettingsRequest{}); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("Get without token = %v", err)
	}
	if _, err := client.SetLocationDiscoverySettings(context.Background(), &nodepb.SetLocationDiscoverySettingsRequest{}); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("Set without token = %v", err)
	}
	ctx := withToken("the-token")
	state, err := client.GetLocationDiscoverySettings(ctx, &nodepb.GetLocationDiscoverySettingsRequest{})
	if err != nil || !state.GetSaved().GetLocateGateway() || state.GetConfigured() || state.GetRestartNeeded() {
		t.Fatalf("initial settings = %v, %v", state, err)
	}
	state, err = client.SetLocationDiscoverySettings(ctx, &nodepb.SetLocationDiscoverySettingsRequest{Settings: &nodepb.LocationDiscoverySettings{
		LocateCountry: true, StunServer: " stun.example.org:3478 ",
	}})
	if err != nil || !state.GetConfigured() || !state.GetRestartNeeded() || state.GetSaved().GetStunServer() != "stun.example.org:3478" || !state.GetRunning().GetLocateGateway() {
		t.Fatalf("saved settings = %v, %v", state, err)
	}
	state, err = client.GetLocationDiscoverySettings(ctx, &nodepb.GetLocationDiscoverySettingsRequest{})
	if err != nil || !state.GetConfigured() || !state.GetSaved().GetLocateCountry() || state.GetSaved().GetStunServer() != "stun.example.org:3478" {
		t.Fatalf("read saved settings = %v, %v", state, err)
	}
}

func TestWatchSharedStateStreamsLocationDiscoveryToIndependentClients(t *testing.T) {
	running := nodedb.LocationDiscovery{LocateCountry: true}
	store := &memoryDiscovery{settings: running, set: true}
	changes := NewNodeInfoChanges()
	client := serveLocal(t, LocalConfig{LocationDiscovery: store, LocationDiscoveryRunning: running, GatewayDiscoverySupported: true, NodeInfoChanges: changes})
	request := &nodepb.WatchSharedStateRequest{Domains: []nodepb.SharedStateDomain{nodepb.SharedStateDomain_SHARED_STATE_DOMAIN_LOCATION_DISCOVERY}}
	ctx1, cancel1 := context.WithCancel(withToken("the-token"))
	defer cancel1()
	ctx2, cancel2 := context.WithCancel(withToken("the-token"))
	defer cancel2()
	first, err := client.WatchSharedState(ctx1, request)
	if err != nil {
		t.Fatal(err)
	}
	second, err := client.WatchSharedState(ctx2, request)
	if err != nil {
		t.Fatal(err)
	}
	watchers := []nodepb.NodeService_WatchSharedStateClient{first, second}
	for _, watcher := range watchers {
		event, err := watcher.Recv()
		state := event.GetLocationDiscovery()
		if err != nil || event.GetRevision() != 1 || !event.GetSnapshot() || state == nil || !state.GetSaved().GetLocateCountry() {
			t.Fatalf("initial location-discovery snapshot = %v, %v", event, err)
		}
	}
	requestToSet := &nodepb.SetLocationDiscoverySettingsRequest{Settings: &nodepb.LocationDiscoverySettings{
		LocateGateway: true, StunServer: "stun.example.org:3478",
	}}
	if _, err := client.SetLocationDiscoverySettings(withToken("the-token"), requestToSet); err != nil {
		t.Fatal(err)
	}
	for _, watcher := range watchers {
		event, err := watcher.Recv()
		state := event.GetLocationDiscovery()
		if err != nil || event.GetRevision() != 2 || event.GetSnapshot() || state == nil || !state.GetSaved().GetLocateGateway() || state.GetSaved().GetStunServer() != "stun.example.org:3478" || !state.GetRestartNeeded() {
			t.Fatalf("updated location-discovery event = %v, %v", event, err)
		}
	}
}

func TestLocationDiscoverySettingsValidateAndReportUnavailableStores(t *testing.T) {
	ctx := withToken("the-token")
	without := serveLocal(t, LocalConfig{})
	if _, err := without.GetLocationDiscoverySettings(ctx, &nodepb.GetLocationDiscoverySettingsRequest{}); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("Get without store = %v", err)
	}
	if _, err := without.SetLocationDiscoverySettings(ctx, &nodepb.SetLocationDiscoverySettingsRequest{}); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("Set without store = %v", err)
	}
	for _, test := range []struct {
		name     string
		settings *nodepb.LocationDiscoverySettings
		want     string
	}{
		{"missing settings", nil, "settings are required"},
		{"invalid scheme", &nodepb.LocationDiscoverySettings{StunServer: "https://stun.example.org"}, "host[:port]"},
		{"invalid port", &nodepb.LocationDiscoverySettings{StunServer: "stun.example.org:65536"}, "host[:port]"},
		{"invalid host", &nodepb.LocationDiscoverySettings{StunServer: "bad host"}, "host[:port]"},
	} {
		t.Run(test.name, func(t *testing.T) {
			client := serveLocal(t, LocalConfig{LocationDiscovery: &memoryDiscovery{}, GatewayDiscoverySupported: true})
			_, err := client.SetLocationDiscoverySettings(ctx, &nodepb.SetLocationDiscoverySettingsRequest{Settings: test.settings})
			if status.Code(err) != codes.InvalidArgument || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Set invalid settings = %v", err)
			}
		})
	}
	unsupported := serveLocal(t, LocalConfig{LocationDiscovery: &memoryDiscovery{}, GatewayDiscoverySupported: false})
	if _, err := unsupported.SetLocationDiscoverySettings(ctx, &nodepb.SetLocationDiscoverySettingsRequest{Settings: &nodepb.LocationDiscoverySettings{LocateGateway: true}}); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("gateway on unsupported daemon = %v", err)
	}
	broken := serveLocal(t, LocalConfig{LocationDiscovery: &memoryDiscovery{err: errors.New("disk gone")}})
	if _, err := broken.GetLocationDiscoverySettings(ctx, &nodepb.GetLocationDiscoverySettingsRequest{}); status.Code(err) != codes.Internal {
		t.Errorf("Get from broken store = %v", err)
	}
	if _, err := broken.SetLocationDiscoverySettings(ctx, &nodepb.SetLocationDiscoverySettingsRequest{Settings: &nodepb.LocationDiscoverySettings{LocateCountry: true}}); status.Code(err) != codes.Internal {
		t.Errorf("Set to broken store = %v", err)
	}
}

func TestLocationDiscoveryStateIgnoresUnsupportedGatewayWhenComparing(t *testing.T) {
	saved := nodedb.LocationDiscovery{LocateGateway: true, LocateCountry: true}
	running := nodedb.LocationDiscovery{LocateCountry: true}
	state := locationDiscoveryState(saved, running, false, true)
	if !state.GetSaved().GetLocateGateway() || state.GetGatewaySupported() || state.GetRestartNeeded() || !state.GetConfigured() {
		t.Fatalf("unsupported gateway state = %v", state)
	}
}

func TestNormalizeSTUNServerAcceptsSupportedAddresses(t *testing.T) {
	for input, want := range map[string]string{
		"": "", "stun.example.org": "stun.example.org", "stun.example.org.": "stun.example.org.",
		"STUN.example.org:003478": "STUN.example.org:3478", "127.0.0.1": "127.0.0.1",
		"[2001:db8::1]:3478": "[2001:db8::1]:3478", "2001:db8::1": "2001:db8::1",
	} {
		got, err := normalizeSTUNServer(input)
		if err != nil || got != want {
			t.Errorf("normalize %q = %q, %v; want %q", input, got, err, want)
		}
	}
	for _, input := range []string{"bad host", "host/path", "host:0", "host:65536", ":3478", "-host", "host..test", strings.Repeat("a", 254), strings.Repeat("a", 261)} {
		if got, err := normalizeSTUNServer(input); err == nil || got != "" {
			t.Errorf("normalize invalid %q = %q, %v", input, got, err)
		}
	}
}

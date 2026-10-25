package mcpserver

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"google.golang.org/grpc"

	nodepb "github.com/sisyphus-network/Sisyphus/packages/protocol/sisyphus/node/v1"
)

func (n *node) GetPoolLimits(context.Context, *nodepb.GetPoolLimitsRequest, ...grpc.CallOption) (*nodepb.PoolLimitsState, error) {
	return &nodepb.PoolLimitsState{Saved: &nodepb.PoolLimits{MaxJobsPerClient: 4}, Coordinator: true}, n.fail("GetPoolLimits")
}

func (n *node) GetModelLending(context.Context, *nodepb.GetModelLendingRequest, ...grpc.CallOption) (*nodepb.ModelLendingState, error) {
	lending := &nodepb.ModelLending{Enabled: true, Only: []string{"llama3.1:8b"}}
	return &nodepb.ModelLendingState{Saved: lending, Running: lending}, n.fail("GetModelLending")
}

func (n *node) ListModelProfiles(context.Context, *nodepb.ListModelProfilesRequest, ...grpc.CallOption) (*nodepb.ListModelProfilesResponse, error) {
	return &nodepb.ListModelProfilesResponse{Profiles: []*nodepb.ModelProfile{{Name: "home", Provider: "ollama", HasApiKey: true, Active: true}}}, n.fail("ListModelProfiles")
}

func (n *node) GetNodeLocation(context.Context, *nodepb.GetNodeLocationRequest, ...grpc.CallOption) (*nodepb.GetNodeLocationResponse, error) {
	return &nodepb.GetNodeLocationResponse{Location: &nodepb.NodeLocation{Latitude: 52.37, Longitude: 4.9, Share: nodepb.LocationShare_LOCATION_SHARE_EXACT}}, n.fail("GetNodeLocation")
}

func (n *node) GetLocationDiscoverySettings(context.Context, *nodepb.GetLocationDiscoverySettingsRequest, ...grpc.CallOption) (*nodepb.LocationDiscoverySettingsState, error) {
	return &nodepb.LocationDiscoverySettingsState{Saved: &nodepb.LocationDiscoverySettings{LocateCountry: true}}, n.fail("GetLocationDiscoverySettings")
}

func (n *node) GetCityTable(context.Context, *nodepb.GetCityTableRequest, ...grpc.CallOption) (*nodepb.CityTableState, error) {
	return &nodepb.CityTableState{Installed: true, Edition: "2026-10"}, n.fail("GetCityTable")
}

func (n *node) GetWebAccessConfig(context.Context, *nodepb.GetWebAccessConfigRequest, ...grpc.CallOption) (*nodepb.WebAccessConfig, error) {
	return &nodepb.WebAccessConfig{Enabled: true, ListenAddress: "192.168.1.5:7443"}, n.fail("GetWebAccessConfig")
}

func (n *node) ListNetworkInterfaces(context.Context, *nodepb.ListNetworkInterfacesRequest, ...grpc.CallOption) (*nodepb.ListNetworkInterfacesResponse, error) {
	return &nodepb.ListNetworkInterfacesResponse{Interfaces: []*nodepb.NetworkInterface{{Name: "eth0", Address: "192.168.1.5"}}}, n.fail("ListNetworkInterfaces")
}

func (n *node) GetJoinedPool(context.Context, *nodepb.GetJoinedPoolRequest, ...grpc.CallOption) (*nodepb.JoinedPool, error) {
	return &nodepb.JoinedPool{CoordinatorId: "coordinator-1", Roles: []nodepb.PoolRole{nodepb.PoolRole_POOL_ROLE_WORKER}}, n.fail("GetJoinedPool")
}

// Each settings tool shows the node's reply by the API's field names, even
// to a read-only server, and says so when the node fails it.
func TestTheSettingsToolsShowWhatTheNodeSaysAndWhenItFails(t *testing.T) {
	ctx := context.Background()
	for tool, want := range map[string]struct{ call, shows string }{
		"get_pool_limits":         {"GetPoolLimits", `"max_jobs_per_client":4`},
		"get_model_lending":       {"GetModelLending", `"only":["llama3.1:8b"]`},
		"list_model_profiles":     {"ListModelProfiles", `"has_api_key":true`},
		"get_location":            {"GetNodeLocation", `"latitude":52.37`},
		"get_location_discovery":  {"GetLocationDiscoverySettings", `"locate_country":true`},
		"get_city_table":          {"GetCityTable", `"edition":"2026-10"`},
		"get_web_access":          {"GetWebAccessConfig", `"listen_address":"192.168.1.5:7443"`},
		"list_network_interfaces": {"ListNetworkInterfaces", `"name":"eth0"`},
		"get_joined_pool":         {"GetJoinedPool", `"roles":["POOL_ROLE_WORKER"]`},
	} {
		for _, failing := range []string{"", want.call} {
			session, _ := agentOf(t, &node{failing: failing}, Config{ReadOnly: true})
			result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: tool})
			if err != nil {
				t.Fatalf("%s: %v", tool, err)
			}
			text := result.Content[0].(*mcp.TextContent).Text
			if failing != "" {
				if !result.IsError || !strings.Contains(text, "the node is down") {
					t.Errorf("%s with the node failing: %t %s", tool, result.IsError, text)
				}
				continue
			}
			if result.IsError || !strings.Contains(text, want.shows) {
				t.Errorf("%s: %s, want %s", tool, text, want.shows)
			}
		}
	}
}

// A reply the node's message cannot be written as is the tool's error, not
// JSON that does not parse.
func TestAReplyThatCannotBeWrittenIsAnError(t *testing.T) {
	if _, err := reply(&nodepb.CityTableState{Edition: "\xff"}, nil); err == nil || !strings.Contains(err.Error(), "UTF-8") {
		t.Errorf("a reply with invalid UTF-8: %v", err)
	}
}

// The node's answer to a change is what it was given, so that a test sees
// what reached it.
func (n *node) SetPoolLimits(_ context.Context, req *nodepb.SetPoolLimitsRequest, _ ...grpc.CallOption) (*nodepb.PoolLimitsState, error) {
	return &nodepb.PoolLimitsState{Saved: req.GetLimits(), RestartNeeded: true}, n.fail("SetPoolLimits")
}

func (n *node) SetModelLending(_ context.Context, req *nodepb.SetModelLendingRequest, _ ...grpc.CallOption) (*nodepb.ModelLendingState, error) {
	return &nodepb.ModelLendingState{Saved: req.GetLending()}, n.fail("SetModelLending")
}

func (n *node) SetNodeLocation(_ context.Context, req *nodepb.SetNodeLocationRequest, _ ...grpc.CallOption) (*nodepb.GetNodeLocationResponse, error) {
	return &nodepb.GetNodeLocationResponse{Location: req.GetLocation()}, n.fail("SetNodeLocation")
}

func (n *node) SetLocationDiscoverySettings(_ context.Context, req *nodepb.SetLocationDiscoverySettingsRequest, _ ...grpc.CallOption) (*nodepb.LocationDiscoverySettingsState, error) {
	return &nodepb.LocationDiscoverySettingsState{Saved: req.GetSettings()}, n.fail("SetLocationDiscoverySettings")
}

func (n *node) DownloadCityTable(context.Context, *nodepb.DownloadCityTableRequest, ...grpc.CallOption) (*nodepb.CityTableState, error) {
	return &nodepb.CityTableState{Installed: true, Edition: "2026-10"}, n.fail("DownloadCityTable")
}

// A change to a setting changes only what it was given, keeps the rest as
// the node had it saved, and is the node's error when the node fails to
// show what it has or to save the change.
func TestTheSettersChangeOnlyWhatTheyAreGiven(t *testing.T) {
	ctx := context.Background()
	for _, c := range []struct {
		tool, args   string
		calls        []string
		shows, hides []string
	}{
		{"set_pool_limits", `{"max_tasks_per_client":9}`, []string{"GetPoolLimits", "SetPoolLimits"},
			[]string{`"max_jobs_per_client":4`, `"max_tasks_per_client":9`, `"restart_needed":true`}, nil},
		{"set_pool_limits", `{"max_jobs_per_client":0}`, []string{"GetPoolLimits", "SetPoolLimits"},
			nil, []string{"max_jobs_per_client"}},
		{"set_model_lending", `{"keep_alive":"10m"}`, []string{"GetModelLending", "SetModelLending"},
			[]string{`"enabled":true`, `"only":["llama3.1:8b"]`, `"keep_alive":"10m"`}, nil},
		{"set_model_lending", `{"only":[]}`, []string{"GetModelLending", "SetModelLending"},
			[]string{`"enabled":true`}, []string{"only"}},
		{"set_location", `{"latitude":48.85,"longitude":2.35,"share":"city"}`, []string{"SetNodeLocation"},
			[]string{`"latitude":48.85`, `"share":"LOCATION_SHARE_CITY"`}, nil},
		{"set_location", `{"latitude":48.85,"longitude":2.35}`, []string{"GetNodeLocation", "SetNodeLocation"},
			[]string{`"longitude":2.35`, `"share":"LOCATION_SHARE_EXACT"`}, nil},
		{"set_location_discovery", `{"stun_server":"stun.example.org:3478"}`, []string{"GetLocationDiscoverySettings", "SetLocationDiscoverySettings"},
			[]string{`"locate_country":true`, `"stun_server":"stun.example.org:3478"`}, nil},
		{"download_city_table", `{}`, []string{"DownloadCityTable"},
			[]string{`"installed":true`, `"edition":"2026-10"`}, nil},
	} {
		for _, failing := range append([]string{""}, c.calls...) {
			session, _ := agentOf(t, &node{failing: failing}, Config{Admin: true})
			result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: c.tool, Arguments: json.RawMessage(c.args)})
			if err != nil {
				t.Fatalf("%s %s: %v", c.tool, c.args, err)
			}
			text := result.Content[0].(*mcp.TextContent).Text
			if failing != "" {
				if !result.IsError || !strings.Contains(text, "the node is down") {
					t.Errorf("%s %s with %s failing: %t %s", c.tool, c.args, failing, result.IsError, text)
				}
				continue
			}
			if result.IsError {
				t.Errorf("%s %s: %s", c.tool, c.args, text)
			}
			for _, want := range c.shows {
				if !strings.Contains(text, want) {
					t.Errorf("%s %s: %s, want %s", c.tool, c.args, text, want)
				}
			}
			for _, unwanted := range c.hides {
				if strings.Contains(text, unwanted) {
					t.Errorf("%s %s: %s, want no %s", c.tool, c.args, text, unwanted)
				}
			}
		}
	}
}

// A location is shared only in the ways the API knows of, by their names.
func TestALocationSharedInAnUnknownWayIsRefused(t *testing.T) {
	for _, share := range []string{"street", "unspecified"} {
		_, err := serving(&node{}).setLocation(context.Background(), locationArgs{Latitude: 1, Longitude: 2, Share: share})
		if err == nil || !strings.Contains(err.Error(), "about_1km") {
			t.Errorf("sharing by %s: %v", share, err)
		}
	}
}

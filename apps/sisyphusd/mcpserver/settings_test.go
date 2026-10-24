package mcpserver

import (
	"context"
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
	return &nodepb.ModelLendingState{Running: &nodepb.ModelLending{Enabled: true, Only: []string{"llama3.1:8b"}}}, n.fail("GetModelLending")
}

func (n *node) ListModelProfiles(context.Context, *nodepb.ListModelProfilesRequest, ...grpc.CallOption) (*nodepb.ListModelProfilesResponse, error) {
	return &nodepb.ListModelProfilesResponse{Profiles: []*nodepb.ModelProfile{{Name: "home", Provider: "ollama", HasApiKey: true, Active: true}}}, n.fail("ListModelProfiles")
}

func (n *node) GetNodeLocation(context.Context, *nodepb.GetNodeLocationRequest, ...grpc.CallOption) (*nodepb.GetNodeLocationResponse, error) {
	return &nodepb.GetNodeLocationResponse{Location: &nodepb.NodeLocation{Latitude: 52.37, Longitude: 4.9}}, n.fail("GetNodeLocation")
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

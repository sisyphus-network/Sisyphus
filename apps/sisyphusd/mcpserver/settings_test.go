package mcpserver

import (
	"context"
	"encoding/json"
	"slices"
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
	return &nodepb.ListModelProfilesResponse{Profiles: []*nodepb.ModelProfile{{Id: "home", Name: "home", Provider: "ollama", HasApiKey: true, Active: true}}}, n.fail("ListModelProfiles")
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
	n.webAccessCalls = append(n.webAccessCalls, "get")
	n.webAccessGetCount++
	config := n.webAccess
	if config == nil {
		config = &nodepb.WebAccessConfig{Enabled: true, ListenAddress: "192.168.1.5:7443"}
	}
	response := &nodepb.WebAccessConfig{
		Enabled: config.GetEnabled(), ListenAddress: config.GetListenAddress(),
		ListenAddresses: slices.Clone(config.GetListenAddresses()), PublicUrl: config.GetPublicUrl(),
		CertificateFingerprint: config.GetCertificateFingerprint(), RestartRequired: config.GetRestartRequired(),
	}
	if n.webAccessFailGetAt == n.webAccessGetCount {
		return nil, errDown
	}
	return response, n.fail("GetWebAccessConfig")
}

func (n *node) SetWebAccessConfig(_ context.Context, req *nodepb.SetWebAccessConfigRequest, _ ...grpc.CallOption) (*nodepb.WebAccessConfig, error) {
	n.webAccessCalls = append(n.webAccessCalls, "set")
	n.webAccessRequest = req
	if err := n.fail("SetWebAccessConfig"); err != nil {
		return nil, err
	}
	n.webAccess = &nodepb.WebAccessConfig{
		Enabled: req.GetEnabled(), ListenAddress: req.GetListenAddress(),
		ListenAddresses: slices.Clone(req.GetListenAddresses()), PublicUrl: req.GetPublicUrl(),
	}
	return n.webAccess, nil
}

func (n *node) RequestActionConfirmation(_ context.Context, req *nodepb.RequestActionConfirmationRequest, _ ...grpc.CallOption) (*nodepb.RequestActionConfirmationResponse, error) {
	n.webAccessCalls = append(n.webAccessCalls, "approve")
	n.webAccessAction = req.GetAction()
	if n.webAccessChangeOnApproval != nil {
		n.webAccess = n.webAccessChangeOnApproval
	}
	if err := n.fail("RequestActionConfirmation"); err != nil {
		return nil, err
	}
	return &nodepb.RequestActionConfirmationResponse{Approved: n.webAccessApproval}, nil
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

func TestWebAccessExpansionsWaitForTheNodeOwner(t *testing.T) {
	ctx := context.Background()
	current := &nodepb.WebAccessConfig{
		Enabled: true, ListenAddress: "192.168.1.5:7443",
		ListenAddresses: []string{"192.168.1.5:7443"}, PublicUrl: "https://node.example.test:7443",
	}
	n := &node{webAccess: current, webAccessApproval: true}
	session, _ := agentOf(t, n, Config{Admin: true})
	result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "set_web_access", Arguments: json.RawMessage(`{"listen_addresses":["192.168.1.5:7443","192.168.1.6:7443"]}`)})
	if err != nil || result.IsError {
		t.Fatalf("set_web_access = %v, %v", result, err)
	}
	if want := []string{"get", "approve", "get", "set"}; !slices.Equal(n.webAccessCalls, want) {
		t.Fatalf("node calls = %v, want %v", n.webAccessCalls, want)
	}
	for _, want := range []string{"enabled=true", "192.168.1.5:7443", "192.168.1.6:7443", "https://node.example.test:7443", "browser UI and API"} {
		if !strings.Contains(n.webAccessAction, want) {
			t.Errorf("owner approval action %q does not explain %q", n.webAccessAction, want)
		}
	}
	if !slices.Equal(n.webAccessRequest.GetListenAddresses(), []string{"192.168.1.5:7443", "192.168.1.6:7443"}) {
		t.Errorf("saved listener addresses = %v", n.webAccessRequest.GetListenAddresses())
	}
}

func TestWebAccessDisableIsImmediateAndDoesNotAskForApproval(t *testing.T) {
	n := &node{webAccess: &nodepb.WebAccessConfig{
		Enabled: true, ListenAddress: "192.168.1.5:7443", ListenAddresses: []string{"192.168.1.5:7443"}, PublicUrl: "https://node.example.test:7443",
	}}
	session, _ := agentOf(t, n, Config{Admin: true})
	result, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: "set_web_access", Arguments: json.RawMessage(`{"enabled":false}`)})
	if err != nil || result.IsError {
		t.Fatalf("disable web access = %v, %v", result, err)
	}
	if want := []string{"get", "set"}; !slices.Equal(n.webAccessCalls, want) {
		t.Fatalf("node calls = %v, want %v", n.webAccessCalls, want)
	}
	if n.webAccessRequest.GetEnabled() {
		t.Error("web access remains enabled")
	}
}

func TestWebAccessRefusalAndStaleApprovalNeverChangeSettings(t *testing.T) {
	current := &nodepb.WebAccessConfig{
		Enabled: false, ListenAddress: "192.168.1.5:7443",
		ListenAddresses: []string{"192.168.1.5:7443"}, PublicUrl: "https://node.example.test:7443",
	}
	args := json.RawMessage(`{"enabled":true}`)
	t.Run("owner refuses", func(t *testing.T) {
		n := &node{webAccess: current, webAccessApproval: false}
		session, _ := agentOf(t, n, Config{Admin: true})
		result, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: "set_web_access", Arguments: args})
		if err != nil || !result.IsError || !strings.Contains(result.Content[0].(*mcp.TextContent).Text, "did not approve") {
			t.Fatalf("refused change = %v, %v", result, err)
		}
		if slices.Contains(n.webAccessCalls, "set") {
			t.Errorf("refused change was applied: %v", n.webAccessCalls)
		}
	})
	t.Run("settings changed while awaiting approval", func(t *testing.T) {
		changed := &nodepb.WebAccessConfig{
			Enabled: false, ListenAddress: "192.168.1.7:7443",
			ListenAddresses: []string{"192.168.1.7:7443"}, PublicUrl: "https://other.example.test:7443",
		}
		n := &node{webAccess: current, webAccessApproval: true, webAccessChangeOnApproval: changed}
		session, _ := agentOf(t, n, Config{Admin: true})
		result, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: "set_web_access", Arguments: args})
		if err != nil || !result.IsError || !strings.Contains(result.Content[0].(*mcp.TextContent).Text, "changed while approval was pending") {
			t.Fatalf("stale approval = %v, %v", result, err)
		}
		if slices.Contains(n.webAccessCalls, "set") {
			t.Errorf("stale approval was applied: %v", n.webAccessCalls)
		}
	})
}

func TestWebAccessApprovalRejectsURLsThatCouldExposeCredentials(t *testing.T) {
	n := &node{webAccess: &nodepb.WebAccessConfig{
		Enabled: true, ListenAddress: "192.168.1.5:7443",
		ListenAddresses: []string{"192.168.1.5:7443"}, PublicUrl: "https://node.example.test:7443",
	}, webAccessApproval: true}
	session, _ := agentOf(t, n, Config{Admin: true})
	result, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: "set_web_access", Arguments: json.RawMessage(`{"public_url":"https://user:secret@node.example.test:7443"}`)})
	if err != nil || !result.IsError {
		t.Fatalf("credential URL = %v, %v", result, err)
	}
	if strings.Contains(result.Content[0].(*mcp.TextContent).Text, "secret") || slices.Contains(n.webAccessCalls, "approve") || slices.Contains(n.webAccessCalls, "set") {
		t.Errorf("invalid credential URL was exposed or applied: result=%v calls=%v", result, n.webAccessCalls)
	}
}

func TestWebAccessToolRejectsMissingArgsAndReportsNodeErrors(t *testing.T) {
	t.Run("missing arguments", func(t *testing.T) {
		n := &node{}
		session, _ := agentOf(t, n, Config{Admin: true})
		result, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: "set_web_access", Arguments: json.RawMessage(`{}`)})
		if err != nil || !result.IsError || !strings.Contains(result.Content[0].(*mcp.TextContent).Text, "give at least one") {
			t.Fatalf("missing arguments = %v, %v", result, err)
		}
		if len(n.webAccessCalls) != 0 {
			t.Fatalf("missing arguments called the node: %v", n.webAccessCalls)
		}
	})

	t.Run("initial read fails", func(t *testing.T) {
		n := &node{failing: "GetWebAccessConfig"}
		session, _ := agentOf(t, n, Config{Admin: true})
		result, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: "set_web_access", Arguments: json.RawMessage(`{"enabled":false}`)})
		if err != nil || !result.IsError || !strings.Contains(result.Content[0].(*mcp.TextContent).Text, "the node is down") {
			t.Fatalf("initial read failure = %v, %v", result, err)
		}
	})

	t.Run("owner confirmation request fails", func(t *testing.T) {
		n := &node{webAccess: &nodepb.WebAccessConfig{ListenAddress: "192.168.1.5:7443", PublicUrl: "https://node.example.test:7443"}, failing: "RequestActionConfirmation"}
		session, _ := agentOf(t, n, Config{Admin: true})
		result, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: "set_web_access", Arguments: json.RawMessage(`{"enabled":true}`)})
		if err != nil || !result.IsError || !strings.Contains(result.Content[0].(*mcp.TextContent).Text, "the node is down") {
			t.Fatalf("confirmation failure = %v, %v", result, err)
		}
		if slices.Contains(n.webAccessCalls, "set") {
			t.Fatalf("confirmation failure still changed settings: %v", n.webAccessCalls)
		}
	})

	t.Run("post-approval read fails", func(t *testing.T) {
		n := &node{
			webAccess:          &nodepb.WebAccessConfig{ListenAddress: "192.168.1.5:7443", PublicUrl: "https://node.example.test:7443"},
			webAccessApproval:  true,
			webAccessFailGetAt: 2,
		}
		session, _ := agentOf(t, n, Config{Admin: true})
		result, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: "set_web_access", Arguments: json.RawMessage(`{"enabled":true}`)})
		if err != nil || !result.IsError || !strings.Contains(result.Content[0].(*mcp.TextContent).Text, "the node is down") {
			t.Fatalf("post-approval read failure = %v, %v", result, err)
		}
		if slices.Contains(n.webAccessCalls, "set") {
			t.Fatalf("post-approval read failure still changed settings: %v", n.webAccessCalls)
		}
	})
}

func TestWebAccessUnchangedSettingsAreNotWritten(t *testing.T) {
	n := &node{}
	session, _ := agentOf(t, n, Config{Admin: true})
	result, err := session.CallTool(context.Background(), &mcp.CallToolParams{
		Name: "set_web_access", Arguments: json.RawMessage(`{"listen_addresses":["192.168.1.5:7443"]}`),
	})
	if err != nil || result.IsError {
		t.Fatalf("unchanged settings = %v, %v", result, err)
	}
	if want := []string{"get"}; !slices.Equal(n.webAccessCalls, want) {
		t.Fatalf("unchanged settings node calls = %v, want %v", n.webAccessCalls, want)
	}
}

func TestWebAccessEmptyListenerListIsPassedThrough(t *testing.T) {
	n := &node{}
	session, _ := agentOf(t, n, Config{Admin: true})
	result, err := session.CallTool(context.Background(), &mcp.CallToolParams{
		Name: "set_web_access", Arguments: json.RawMessage(`{"listen_addresses":[]}`),
	})
	if err != nil || result.IsError {
		t.Fatalf("empty listener list = %v, %v", result, err)
	}
	if n.webAccessRequest.GetListenAddress() != "" || len(n.webAccessRequest.GetListenAddresses()) != 0 {
		t.Fatalf("empty listener list became %#v", n.webAccessRequest)
	}
}

func TestWebAccessAddressesHandlesNilSingularAndEmptyConfigs(t *testing.T) {
	if got := webAccessAddresses(nil); got != nil {
		t.Errorf("nil config addresses = %v, want nil", got)
	}
	if got := webAccessAddresses(&nodepb.WebAccessConfig{ListenAddress: "192.168.1.5:7443"}); !slices.Equal(got, []string{"192.168.1.5:7443"}) {
		t.Errorf("singular listener address = %v", got)
	}
	if got := webAccessAddresses(&nodepb.WebAccessConfig{}); got != nil {
		t.Errorf("empty config addresses = %v, want nil", got)
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

func (n *node) SaveModelProfile(_ context.Context, req *nodepb.SaveModelProfileRequest, _ ...grpc.CallOption) (*nodepb.ModelProfile, error) {
	if !req.GetKeepApiKey() {
		return nil, errDown
	}
	return &nodepb.ModelProfile{Id: req.GetId(), Name: req.GetName(), Provider: req.GetProvider(), BaseUrl: req.GetBaseUrl(), Model: req.GetModel(), Active: req.GetActive(), HasApiKey: req.GetId() == "home"}, n.fail("SaveModelProfile")
}

// A profile already saved keeps what the agent left out, a new one is made
// from what it gave, and neither is ever sent a key: the saved one stays.
func TestAModelProfileIsSavedWithOnlyWhatIsGiven(t *testing.T) {
	ctx := context.Background()
	for _, c := range []struct {
		args         string
		calls        []string
		shows, hides []string
	}{
		{`{"id":"home","model":"qwen3:8b"}`, []string{"ListModelProfiles", "SaveModelProfile"},
			[]string{`"name":"home"`, `"provider":"ollama"`, `"model":"qwen3:8b"`, `"active":true`, `"has_api_key":true`}, nil},
		{`{"id":"home","active":false}`, []string{"ListModelProfiles", "SaveModelProfile"},
			[]string{`"name":"home"`}, []string{"active"}},
		{`{"id":"cloud","name":"cloud","provider":"anthropic","model":"claude"}`, []string{"ListModelProfiles", "SaveModelProfile"},
			[]string{`"id":"cloud"`, `"provider":"anthropic"`}, []string{"has_api_key", "active"}},
	} {
		for _, failing := range append([]string{""}, c.calls...) {
			session, _ := agentOf(t, &node{failing: failing}, Config{Admin: true})
			result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "save_model_profile", Arguments: json.RawMessage(c.args)})
			if err != nil {
				t.Fatalf("%s: %v", c.args, err)
			}
			text := result.Content[0].(*mcp.TextContent).Text
			if failing != "" {
				if !result.IsError || !strings.Contains(text, "the node is down") {
					t.Errorf("%s with %s failing: %t %s", c.args, failing, result.IsError, text)
				}
				continue
			}
			if result.IsError {
				t.Errorf("%s: %s", c.args, text)
			}
			for _, want := range c.shows {
				if !strings.Contains(text, want) {
					t.Errorf("%s: %s, want %s", c.args, text, want)
				}
			}
			for _, unwanted := range c.hides {
				if strings.Contains(text, unwanted) {
					t.Errorf("%s: %s, want no %s", c.args, text, unwanted)
				}
			}
		}
	}
}

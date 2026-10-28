package mcpserver

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"slices"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	nodepb "github.com/sisyphus-network/Sisyphus/packages/protocol/sisyphus/node/v1"
)

// settings gives a server the tools that show how the node is set up: what
// it holds its pool's clients to, which models it lends, where it says it
// is, how browsers reach it, and which pool it works for (#650). Each
// answers with the node's own reply. The API gives out no key, only
// whether one is set, and masks any credential in a model service's
// address (#718).
func (s *server) settings(out *mcp.Server) {
	seen := &mcp.ToolAnnotations{ReadOnlyHint: true}
	for _, t := range []struct {
		name, description string
		get               func(context.Context) (proto.Message, error)
	}{
		{"get_pool_limits", "Says what this node, as its pool's coordinator, holds each client to: jobs and tasks at once, the longest task timeout, and bytes stored and uploaded. saved takes effect when the node next starts, running holds now.",
			func(ctx context.Context) (proto.Message, error) {
				return s.Node.GetPoolLimits(ctx, &nodepb.GetPoolLimitsRequest{})
			}},
		{"get_model_lending", "Says whether this machine lends its Ollama models to the pool, which of them and with what settings, as saved and as running now.",
			func(ctx context.Context) (proto.Message, error) {
				return s.Node.GetModelLending(ctx, &nodepb.GetModelLendingRequest{})
			}},
		{"list_model_profiles", "Lists the saved model services the planner can be switched between, which is active, and whether each has a key set. The keys themselves are never shown.",
			func(ctx context.Context) (proto.Message, error) {
				return s.Node.ListModelProfiles(ctx, &nodepb.ListModelProfilesRequest{})
			}},
		{"get_location", "Says where the node's owner has said the node is, and how much of that other members of a pool may see. Nothing is set if the owner has given no location.",
			func(ctx context.Context) (proto.Message, error) {
				return s.Node.GetNodeLocation(ctx, &nodepb.GetNodeLocationRequest{})
			}},
		{"get_location_discovery", "Says whether the node works out where it is by itself, from its gateway or its country, and which STUN server it asks, as saved and as running now.",
			func(ctx context.Context) (proto.Message, error) {
				return s.Node.GetLocationDiscoverySettings(ctx, &nodepb.GetLocationDiscoverySettingsRequest{})
			}},
		{"get_city_table", "Says whether the free city table the node finds an address's city in is installed, which month's it is and how big.",
			func(ctx context.Context) (proto.Message, error) {
				return s.Node.GetCityTable(ctx, &nodepb.GetCityTableRequest{})
			}},
		{"get_web_access", "Says whether browsers may reach the node's web interface, on which addresses, and the fingerprint of the certificate it shows them.",
			func(ctx context.Context) (proto.Message, error) {
				return s.Node.GetWebAccessConfig(ctx, &nodepb.GetWebAccessConfigRequest{})
			}},
		{"list_network_interfaces", "Lists this machine's usable addresses, loopback aside, any of which the web interface can be set to listen on.",
			func(ctx context.Context) (proto.Message, error) {
				return s.Node.ListNetworkInterfaces(ctx, &nodepb.ListNetworkInterfacesRequest{})
			}},
		{"get_joined_pool", "Says which pool this node works for, if it joined another node's: its coordinator, how the node stands with it, and its roles there. A node that coordinates its own pool has joined none, and is told so.",
			func(ctx context.Context) (proto.Message, error) {
				return s.Node.GetJoinedPool(ctx, &nodepb.GetJoinedPoolRequest{})
			}},
	} {
		add(s, out, reads, &mcp.Tool{Name: t.name, Annotations: seen, Description: t.description}, doing(s, func(ctx context.Context, _ none) (any, error) {
			return reply(t.get(ctx))
		}))
	}
}

// reply is a node's answer as an agent is shown it: its fields by their
// names in the API, and those that are not set left out.
func reply(m proto.Message, err error) (any, error) {
	if err != nil {
		return nil, err
	}
	b, err := protojson.MarshalOptions{UseProtoNames: true}.Marshal(m)
	if err != nil {
		return nil, err
	}
	return json.RawMessage(b), nil
}

// setters gives a server with --admin the tools that change how the node is
// set up (#650). Each changes only what it is given: the rest is kept as
// the node has it saved, so an agent never resets a setting it left out.
// These read-then-write updates are not atomic; a concurrent settings save
// through another client can be overwritten.
// Each answers with the node's reply, which says when a change waits for
// the node to start again.
func (s *server) setters(out *mcp.Server) {
	add(s, out, admin, &mcp.Tool{Name: "set_pool_limits",
		Description: "Changes what this node, as its pool's coordinator, holds each client to. Only the limits given change; 0 is no limit. They take effect when the node next starts."}, doing(s, s.setPoolLimits))
	add(s, out, admin, &mcp.Tool{Name: "set_model_lending",
		Description: "Changes whether this machine lends its Ollama models to the pool, which of them and with what settings. Only what is given changes. Turning lending on or off, or moving Ollama, takes effect when the node next starts."}, doing(s, s.setModelLending))
	add(s, out, admin, &mcp.Tool{Name: "set_location",
		Description: "Saves where the node is and how much of that other members of a pool may see: nothing, the country, the city, about a kilometre, or exactly. The share is kept as it was if not given, and is the country if none was ever set."}, doing(s, s.setLocation))
	add(s, out, admin, &mcp.Tool{Name: "set_location_discovery",
		Description: "Changes whether the node works out where it is by itself, from its gateway or its country, and which STUN server it asks. Only what is given changes."}, doing(s, s.setLocationDiscovery))
	add(s, out, admin, &mcp.Tool{Name: "save_model_profile",
		Description: "Saves one of the model services the planner can be switched between, and with active switches the planner to it. A profile already saved changes only in what is given; a new one needs a name and a provider. No key is ever given: the saved one is kept while the provider and address stay the same, and has_api_key in the answer says whether one is set. The base_url list_model_profiles shows, credentials masked, can be given back as url as it is."}, doing(s, s.saveModelProfile))
	add(s, out, admin, &mcp.Tool{Name: "set_web_access",
		Description: "Changes this node's HTTPS browser access: whether it is enabled, which local interface addresses it listens on, and the HTTPS origin browsers use. Read get_web_access and list_network_interfaces first. Enabling access, adding a listener address while enabled, or changing its advertised URL waits for the node owner's approval in the Sisyphus app; disabling access or removing addresses is immediate. Client-side approval hints are not trusted."}, doing(s, s.setWebAccess))
	add(s, out, admin, &mcp.Tool{Name: "download_city_table",
		Description: "Downloads the month's free city table from DB-IP, a few hundred megabytes, so that the node can find the city of an address and not only its country. Returns when it is installed."}, doing(s, func(ctx context.Context, _ none) (any, error) {
		return reply(s.Node.DownloadCityTable(ctx, &nodepb.DownloadCityTableRequest{}))
	}))
}

type webAccessArgs struct {
	Enabled         *bool     `json:"enabled,omitempty" jsonschema:"whether remote browser access is enabled; enabling it requires owner approval"`
	ListenAddresses *[]string `json:"listen_addresses,omitempty" jsonschema:"specific local interface IP:port addresses from list_network_interfaces; adding one while enabled requires owner approval"`
	PublicURL       *string   `json:"public_url,omitempty" jsonschema:"the HTTPS origin browsers use, such as https://node.example.net:50052; changing it while enabled requires owner approval"`
}

func (s *server) setWebAccess(ctx context.Context, args webAccessArgs) (any, error) {
	if args.Enabled == nil && args.ListenAddresses == nil && args.PublicURL == nil {
		return nil, fmt.Errorf("give at least one of enabled, listen_addresses or public_url")
	}
	current, err := s.Node.GetWebAccessConfig(s.as(ctx), &nodepb.GetWebAccessConfigRequest{})
	if err != nil {
		return nil, err
	}
	next := &nodepb.WebAccessConfig{
		Enabled: current.GetEnabled(), ListenAddress: current.GetListenAddress(),
		ListenAddresses: webAccessAddresses(current), PublicUrl: current.GetPublicUrl(),
	}
	if args.Enabled != nil {
		next.Enabled = *args.Enabled
	}
	if args.ListenAddresses != nil {
		next.ListenAddresses = slices.Clone(*args.ListenAddresses)
		if len(next.ListenAddresses) == 0 {
			next.ListenAddress = ""
		} else {
			next.ListenAddress = next.ListenAddresses[0]
		}
	}
	if args.PublicURL != nil {
		next.PublicUrl = *args.PublicURL
	}
	if sameWebAccess(next, current) {
		return reply(current, nil)
	}

	if webAccessNeedsOwnerApproval(current, next) {
		if err := validateApprovalURL(next.GetPublicUrl()); err != nil {
			return nil, err
		}
		decision, err := s.Node.RequestActionConfirmation(s.as(ctx), &nodepb.RequestActionConfirmationRequest{
			Action: webAccessApprovalAction(next),
		})
		if err != nil {
			return nil, err
		}
		if decision == nil || !decision.GetApproved() {
			return nil, fmt.Errorf("the node owner did not approve this change; no change was made")
		}
		// Do not apply a target that was approved against a stale baseline.
		latest, err := s.Node.GetWebAccessConfig(s.as(ctx), &nodepb.GetWebAccessConfigRequest{})
		if err != nil {
			return nil, err
		}
		if !sameWebAccess(current, latest) {
			return nil, fmt.Errorf("web access changed while approval was pending; read the current settings and try again")
		}
	}
	return reply(s.Node.SetWebAccessConfig(s.as(ctx), webAccessRequest(next)))
}

func webAccessAddresses(config *nodepb.WebAccessConfig) []string {
	if config == nil {
		return nil
	}
	if len(config.GetListenAddresses()) > 0 {
		return slices.Clone(config.GetListenAddresses())
	}
	if config.GetListenAddress() != "" {
		return []string{config.GetListenAddress()}
	}
	return nil
}

func webAccessRequest(config *nodepb.WebAccessConfig) *nodepb.SetWebAccessConfigRequest {
	request := &nodepb.SetWebAccessConfigRequest{
		Enabled: config.GetEnabled(), PublicUrl: config.GetPublicUrl(), ListenAddresses: webAccessAddresses(config),
	}
	if len(request.GetListenAddresses()) > 0 {
		request.ListenAddress = request.GetListenAddresses()[0]
	}
	return request
}

func sameWebAccess(first, second *nodepb.WebAccessConfig) bool {
	return first.GetEnabled() == second.GetEnabled() && first.GetPublicUrl() == second.GetPublicUrl() && slices.Equal(webAccessAddresses(first), webAccessAddresses(second))
}

func webAccessNeedsOwnerApproval(current, next *nodepb.WebAccessConfig) bool {
	if !next.GetEnabled() {
		return false
	}
	if !current.GetEnabled() {
		return true
	}
	oldAddresses := make(map[string]struct{}, len(webAccessAddresses(current)))
	for _, address := range webAccessAddresses(current) {
		oldAddresses[address] = struct{}{}
	}
	for _, address := range webAccessAddresses(next) {
		if _, exists := oldAddresses[address]; !exists {
			return true
		}
	}
	return next.GetPublicUrl() != "" && next.GetPublicUrl() != current.GetPublicUrl()
}

func validateApprovalURL(raw string) error {
	parsed, err := url.Parse(raw)
	if raw == "" || err != nil || parsed.Scheme != "https" || parsed.Hostname() == "" || parsed.User != nil || (parsed.Path != "" && parsed.Path != "/") || parsed.RawQuery != "" || parsed.Fragment != "" {
		return fmt.Errorf("public_url must be an HTTPS origin without credentials, a path, a query or a fragment")
	}
	return nil
}

func webAccessApprovalAction(config *nodepb.WebAccessConfig) string {
	return fmt.Sprintf("Apply remote browser access: enabled=true; HTTPS listeners=[%s]; browser URL=%s. Devices that can reach these interfaces can use this node's browser UI and API.",
		strings.Join(webAccessAddresses(config), ", "), config.GetPublicUrl())
}

// set puts a value given into a setting, and leaves the setting as it was
// if none was.
func set[T any](setting *T, given *T) {
	if given != nil {
		*setting = *given
	}
}

type poolLimitsArgs struct {
	MaxJobsPerClient         *uint32 `json:"max_jobs_per_client,omitempty" jsonschema:"how many unfinished jobs one client may have at once"`
	MaxTasksPerClient        *uint32 `json:"max_tasks_per_client,omitempty" jsonschema:"how many unfinished tasks, waiting or running, one client may have across its jobs"`
	MaxTaskTimeoutSeconds    *uint64 `json:"max_task_timeout_seconds,omitempty" jsonschema:"the longest one attempt at a client's task may run, in seconds"`
	MaxStoreBytesPerClient   *uint64 `json:"max_store_bytes_per_client,omitempty" jsonschema:"the most one client may keep in the coordinator's store, in bytes"`
	MaxUploadBytesPerAttempt *uint64 `json:"max_upload_bytes_per_attempt,omitempty" jsonschema:"the most a worker may upload for one attempt at a task, in bytes"`
}

func (s *server) setPoolLimits(ctx context.Context, args poolLimitsArgs) (any, error) {
	state, err := s.Node.GetPoolLimits(ctx, &nodepb.GetPoolLimitsRequest{})
	if err != nil {
		return nil, err
	}
	limits := cmp.Or(state.GetSaved(), &nodepb.PoolLimits{})
	set(&limits.MaxJobsPerClient, args.MaxJobsPerClient)
	set(&limits.MaxTasksPerClient, args.MaxTasksPerClient)
	set(&limits.MaxTaskTimeoutSeconds, args.MaxTaskTimeoutSeconds)
	set(&limits.MaxStoreBytesPerClient, args.MaxStoreBytesPerClient)
	set(&limits.MaxUploadBytesPerAttempt, args.MaxUploadBytesPerAttempt)
	return reply(s.Node.SetPoolLimits(ctx, &nodepb.SetPoolLimitsRequest{Limits: limits}))
}

type lendingArgs struct {
	Enabled   *bool     `json:"enabled,omitempty" jsonschema:"whether the models are lent at all"`
	OllamaURL *string   `json:"ollama_url,omitempty" jsonschema:"where this machine's Ollama is, such as http://127.0.0.1:11434"`
	Only      *[]string `json:"only,omitempty" jsonschema:"the models lent: a model as Ollama names it (qwen3:8b), a family (llama3.1) or a pattern (gemma*). An empty list lends them all"`
	GPULayers *int32    `json:"gpu_layers,omitempty" jsonschema:"how many of a model's layers may go on graphics cards: 0 for none, -1 for as many as fit"`
	Threads   *int32    `json:"threads,omitempty" jsonschema:"the processor threads a model may use; 0 lets Ollama decide"`
	Context   *int32    `json:"context,omitempty" jsonschema:"the tokens of a conversation a model holds; 0 lets Ollama decide"`
	KeepAlive *string   `json:"keep_alive,omitempty" jsonschema:"how long a model stays loaded after it answered, such as 10m, or 0 to unload it at once; empty lets Ollama decide"`
}

func (s *server) setModelLending(ctx context.Context, args lendingArgs) (any, error) {
	state, err := s.Node.GetModelLending(ctx, &nodepb.GetModelLendingRequest{})
	if err != nil {
		return nil, err
	}
	lending := cmp.Or(state.GetSaved(), &nodepb.ModelLending{})
	set(&lending.Enabled, args.Enabled)
	set(&lending.OllamaUrl, args.OllamaURL)
	set(&lending.Only, args.Only)
	set(&lending.GpuLayers, args.GPULayers)
	set(&lending.Threads, args.Threads)
	set(&lending.Context, args.Context)
	set(&lending.KeepAlive, args.KeepAlive)
	return reply(s.Node.SetModelLending(ctx, &nodepb.SetModelLendingRequest{Lending: lending}))
}

type locationArgs struct {
	Latitude  float64 `json:"latitude" jsonschema:"in degrees from -90 to 90, north positive"`
	Longitude float64 `json:"longitude" jsonschema:"in degrees from -180 to 180, east positive"`
	Share     string  `json:"share,omitempty" jsonschema:"how much of it other members of a pool may see: nothing, country, city, about_1km or exact"`
}

func (s *server) setLocation(ctx context.Context, args locationArgs) (any, error) {
	share := nodepb.LocationShare_LOCATION_SHARE_UNSPECIFIED
	if args.Share != "" {
		named, ok := nodepb.LocationShare_value["LOCATION_SHARE_"+strings.ToUpper(args.Share)]
		if !ok || named == 0 {
			return nil, fmt.Errorf("share is nothing, country, city, about_1km or exact, not %q", args.Share)
		}
		share = nodepb.LocationShare(named)
	} else {
		saved, err := s.Node.GetNodeLocation(ctx, &nodepb.GetNodeLocationRequest{})
		if err != nil {
			return nil, err
		}
		share = saved.GetLocation().GetShare()
	}
	return reply(s.Node.SetNodeLocation(ctx, &nodepb.SetNodeLocationRequest{Location: &nodepb.NodeLocation{Latitude: args.Latitude, Longitude: args.Longitude, Share: share}}))
}

type discoveryArgs struct {
	LocateGateway *bool   `json:"locate_gateway,omitempty" jsonschema:"whether the node asks its network's gateway where it is"`
	LocateCountry *bool   `json:"locate_country,omitempty" jsonschema:"whether the node works out its country from its public address"`
	STUNServer    *string `json:"stun_server,omitempty" jsonschema:"the STUN server the node asks for its public address, as host:port"`
}

func (s *server) setLocationDiscovery(ctx context.Context, args discoveryArgs) (any, error) {
	state, err := s.Node.GetLocationDiscoverySettings(ctx, &nodepb.GetLocationDiscoverySettingsRequest{})
	if err != nil {
		return nil, err
	}
	settings := cmp.Or(state.GetSaved(), &nodepb.LocationDiscoverySettings{})
	set(&settings.LocateGateway, args.LocateGateway)
	set(&settings.LocateCountry, args.LocateCountry)
	set(&settings.StunServer, args.STUNServer)
	return reply(s.Node.SetLocationDiscoverySettings(ctx, &nodepb.SetLocationDiscoverySettingsRequest{Settings: settings}))
}

type profileArgs struct {
	ID       string  `json:"id" jsonschema:"the profile's ID, as list_model_profiles shows it, or a new one"`
	Name     *string `json:"name,omitempty" jsonschema:"what the profile is called"`
	Provider *string `json:"provider,omitempty" jsonschema:"ollama, anthropic or openai, the last also for any service that speaks as OpenAI does"`
	URL      *string `json:"url,omitempty" jsonschema:"where the service is, if not in that provider's usual place"`
	Model    *string `json:"model,omitempty" jsonschema:"the model the planner plans with under this profile"`
	Active   *bool   `json:"active,omitempty" jsonschema:"whether the planner is switched to this profile"`
}

func (s *server) saveModelProfile(ctx context.Context, args profileArgs) (any, error) {
	listed, err := s.Node.ListModelProfiles(ctx, &nodepb.ListModelProfilesRequest{})
	if err != nil {
		return nil, err
	}
	saved := &nodepb.ModelProfile{Id: args.ID}
	for _, p := range listed.GetProfiles() {
		if p.GetId() == args.ID {
			saved = p
		}
	}
	set(&saved.Name, args.Name)
	set(&saved.Provider, args.Provider)
	set(&saved.BaseUrl, args.URL)
	set(&saved.Model, args.Model)
	set(&saved.Active, args.Active)
	// An agent is never given a key to pass on: the one already there stays.
	return reply(s.Node.SaveModelProfile(ctx, &nodepb.SaveModelProfileRequest{Id: saved.GetId(), Name: saved.GetName(), Provider: saved.GetProvider(), BaseUrl: saved.GetBaseUrl(), Model: saved.GetModel(), Active: saved.GetActive(), KeepApiKey: true}))
}

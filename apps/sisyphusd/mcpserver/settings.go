package mcpserver

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
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
// whether one is set, though a model service's address is shown as it was
// saved, credentials and all if it has them (#718).
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
	add(s, out, admin, &mcp.Tool{Name: "download_city_table",
		Description: "Downloads the month's free city table from DB-IP, a few hundred megabytes, so that the node can find the city of an address and not only its country. Returns when it is installed."}, doing(s, func(ctx context.Context, _ none) (any, error) {
		return reply(s.Node.DownloadCityTable(ctx, &nodepb.DownloadCityTableRequest{}))
	}))
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

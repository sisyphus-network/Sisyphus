package mcpserver

import (
	"context"
	"encoding/json"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	nodepb "github.com/sisyphus-network/Sisyphus/packages/protocol/sisyphus/node/v1"
)

// settings gives a server the tools that show how the node is set up: what
// it holds its pool's clients to, which models it lends, where it says it
// is, how browsers reach it, and which pool it works for (#650). Each
// answers with the node's own reply. None has a secret in it, since the
// node's API gives none out.
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
	return json.RawMessage(protojson.MarshalOptions{UseProtoNames: true}.Format(m)), nil
}

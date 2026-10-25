package mcpserver

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	nodepb "github.com/sisyphus-network/Sisyphus/packages/protocol/sisyphus/node/v1"
)

const noConfirmation = "this client cannot ask you to confirm destructive actions; no change was made"

const confirmationLifetime = 5 * time.Minute
const maxPendingConfirmations = 1024

type pendingConfirmation struct {
	action  string
	expires time.Time
}

func confirmDestructive(s *server, req *mcp.CallToolRequest, action string) (*mcp.CallToolResult, bool, error) {
	if req == nil || req.Session == nil {
		return nil, false, errors.New(noConfirmation)
	}
	initialized := req.Session.InitializeParams()
	if initialized == nil || initialized.Capabilities == nil || initialized.Capabilities.Elicitation == nil {
		return nil, false, errors.New(noConfirmation)
	}
	if req.Params == nil {
		return nil, false, errors.New(noConfirmation)
	}
	response, hasResponse := req.Params.InputResponses["confirmation"]
	if !hasResponse {
		nonce := make([]byte, 32)
		entropy := s.confirmationEntropy
		if entropy == nil {
			entropy = rand.Reader
		}
		if _, err := io.ReadFull(entropy, nonce); err != nil {
			return nil, false, errors.New(noConfirmation)
		}
		state := base64.RawURLEncoding.EncodeToString(nonce)
		s.confirmationMu.Lock()
		now := time.Now()
		for token, pending := range s.confirmations {
			if !pending.expires.After(now) {
				delete(s.confirmations, token)
			}
		}
		if s.confirmations == nil {
			s.confirmations = make(map[string]pendingConfirmation)
		}
		if len(s.confirmations) >= maxPendingConfirmations {
			s.confirmationMu.Unlock()
			return nil, false, errors.New("too many destructive confirmations are already pending; no change was made")
		}
		s.confirmations[state] = pendingConfirmation{action: action, expires: now.Add(confirmationLifetime)}
		s.confirmationMu.Unlock()
		return &mcp.CallToolResult{
			InputRequests: mcp.InputRequestMap{"confirmation": &mcp.ElicitParams{
				Message: "Confirm this destructive action: " + action,
				RequestedSchema: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"confirm": map[string]any{"type": "boolean", "title": "I confirm this action"},
					},
					"required": []string{"confirm"},
				},
			}},
			RequestState: state,
		}, false, nil
	}
	if req.Params.RequestState == "" {
		return nil, false, errors.New("confirmation does not match this destructive action; no change was made")
	}
	s.confirmationMu.Lock()
	pending, valid := s.confirmations[req.Params.RequestState]
	if valid && pending.action == action && pending.expires.After(time.Now()) {
		delete(s.confirmations, req.Params.RequestState)
	} else {
		valid = false
	}
	s.confirmationMu.Unlock()
	if !valid {
		return nil, false, errors.New("confirmation does not match this destructive action or has expired; no change was made")
	}
	result, ok := response.(*mcp.ElicitResult)
	if !ok || result == nil || result.Action != "accept" {
		return nil, false, errors.New("destructive action was not confirmed; no change was made")
	}
	confirmed, ok := result.Content["confirm"].(bool)
	if !ok || !confirmed {
		return nil, false, errors.New("destructive action was not confirmed; no change was made")
	}
	return nil, true, nil
}

func confirmed[In any](s *server, action func(In) string, do func(context.Context, In) (any, error)) mcp.ToolHandlerFor[In, any] {
	return func(ctx context.Context, req *mcp.CallToolRequest, in In) (*mcp.CallToolResult, any, error) {
		pending, confirmed, err := confirmDestructive(s, req, action(in))
		if err != nil {
			return shown(nil, err)
		}
		if !confirmed {
			return pending, nil, nil
		}
		return shown(do(s.as(ctx), in))
	}
}

func confirmedKeeping[In any](s *server, action func(In) string, do func(context.Context, *Main, In) (any, error)) mcp.ToolHandlerFor[In, any] {
	return func(ctx context.Context, req *mcp.CallToolRequest, in In) (*mcp.CallToolResult, any, error) {
		pending, confirmed, err := confirmDestructive(s, req, action(in))
		if err != nil {
			return shown(nil, err)
		}
		if !confirmed {
			return pending, nil, nil
		}
		main, err := s.main()
		if err != nil {
			return shown(nil, err)
		}
		return shown(do(ctx, main, in))
	}
}

type modelProfileIDArgs struct {
	ID string `json:"id" jsonschema:"the ID of the saved model-provider profile"`
}

func (s *server) destructive(out *mcp.Server) {
	add(s, out, destructive, &mcp.Tool{Name: "delete_model_profile", Annotations: &mcp.ToolAnnotations{DestructiveHint: ptr(true)},
		Description: "Permanently deletes a saved model-provider profile. It does not reveal or return its API key."}, confirmed(s, func(args modelProfileIDArgs) string {
		return fmt.Sprintf("permanently delete model-provider profile %q", args.ID)
	}, s.deleteModelProfile))
	add(s, out, destructive, &mcp.Tool{Name: "remove_city_table", Annotations: &mcp.ToolAnnotations{DestructiveHint: ptr(true)},
		Description: "Deletes this node's local city lookup database. Location lookups will be unavailable until it is downloaded again."}, confirmed(s, func(none) string {
		return "delete this node's local city lookup database"
	}, s.removeCityTable))
	add(s, out, destructive, &mcp.Tool{Name: "leave_pool", Annotations: &mcp.ToolAnnotations{DestructiveHint: ptr(true)},
		Description: "Leaves the pool this node works for. The node stops, and forgets that pool's stored files, submitted jobs, and coordinator address; it cannot be undone from the same tool set."}, confirmed(s, func(none) string {
		return "leave the joined pool, forget its files, jobs and coordinator, and stop this node"
	}, s.leavePool))
}

func (s *server) deleteModelProfile(ctx context.Context, args modelProfileIDArgs) (any, error) {
	if _, err := s.Node.DeleteModelProfile(ctx, &nodepb.DeleteModelProfileRequest{Id: args.ID}); err != nil {
		return nil, err
	}
	return map[string]any{"deleted": args.ID}, nil
}

func (s *server) removeCityTable(ctx context.Context, _ none) (any, error) {
	state, err := s.Node.RemoveCityTable(ctx, &nodepb.RemoveCityTableRequest{})
	if err != nil {
		return nil, err
	}
	return map[string]any{"installed": state.GetInstalled(), "edition": state.GetEdition()}, nil
}

func (s *server) leavePool(ctx context.Context, _ none) (any, error) {
	if _, err := s.Node.LeavePool(ctx, &nodepb.LeavePoolRequest{}); err != nil {
		return nil, err
	}
	return map[string]any{"left": true}, nil
}

package ai

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
	"github.com/anthropics/anthropic-sdk-go/packages/param"
)

// claude speaks to Anthropic's models through Anthropic's own client. It is
// not a third dialect of the plain JSON the other two share: a reply from
// Claude is a list of blocks, some of them its reasoning, and the service
// wants its earlier replies handed back exactly as it gave them.
type claude struct{ client anthropic.Client }

func newClaude(cfg Config) claude {
	// With no key given the client looks where Anthropic's tools keep one:
	// ANTHROPIC_API_KEY, or a profile signed in with `ant auth login`.
	var opts []option.RequestOption
	if cfg.APIKey != "" {
		opts = append(opts, option.WithAPIKey(cfg.APIKey))
	}
	if cfg.BaseURL != "" {
		opts = append(opts, option.WithBaseURL(cfg.BaseURL))
	}
	return claude{anthropic.NewClient(opts...)}
}

// How long a reply may be: as long as the model allows, up to most, and
// usual if the service will not say what the model allows.
const (
	mostTokens  = 32000
	usualTokens = 16000
)

func (c claude) Chat(ctx context.Context, req Request, said func(string)) (Message, error) {
	room := int64(usualTokens)
	if info, err := c.client.Models.Get(ctx, req.Model, anthropic.ModelGetParams{}); err == nil && info.MaxTokens > 0 {
		room = min(info.MaxTokens, mostTokens)
	}
	reply, err := c.chat(ctx, req, room, true, said)
	if disowned(err) {
		// The service will not take back reasoning from a conversation
		// that has since changed, which this one has if the pool's
		// workloads have. It is asked again without the reasoning.
		reply, err = c.chat(ctx, req, room, false, said)
	}
	return reply, err
}

// chat asks once. verbatim says whether Claude's earlier replies are sent
// as it gave them, reasoning and all, or made again from their text and
// calls.
func (c claude) chat(ctx context.Context, req Request, room int64, verbatim bool, said func(string)) (Message, error) {
	params := anthropic.MessageNewParams{Model: req.Model, MaxTokens: room}
	// Results of tools go in one message from the user, however many
	// there are; results is whether the last message is one of those.
	results := false
	for _, m := range req.Messages {
		switch m.Role {
		case System:
			params.System = append(params.System, anthropic.TextBlockParam{Text: m.Content})
		case ToolRole:
			block := anthropic.NewToolResultBlock(m.CallID, m.Content, false)
			if results {
				last := &params.Messages[len(params.Messages)-1]
				last.Content = append(last.Content, block)
				continue
			}
			params.Messages = append(params.Messages, anthropic.NewUserMessage(block))
		case Assistant:
			params.Messages = append(params.Messages, c.earlier(m, verbatim))
		default:
			params.Messages = append(params.Messages, anthropic.NewUserMessage(anthropic.NewTextBlock(m.Content)))
		}
		results = m.Role == ToolRole
	}
	for _, t := range req.Tools {
		params.Tools = append(params.Tools, anthropic.ToolUnionParam{OfTool: &anthropic.ToolParam{
			Name:        t.Name,
			Description: anthropic.String(t.Description),
			InputSchema: param.Override[anthropic.ToolInputSchemaParam](t.Parameters),
		}})
	}

	stream := c.client.Messages.NewStreaming(ctx, params)
	defer stream.Close()
	var whole anthropic.Message
	for stream.Next() {
		event := stream.Current()
		if err := whole.Accumulate(event); err != nil {
			return Message{}, fmt.Errorf("the model's reply is not what Anthropic sends: %w", err)
		}
		if delta, ok := event.AsAny().(anthropic.ContentBlockDeltaEvent); ok {
			if text, ok := delta.Delta.AsAny().(anthropic.TextDelta); ok {
				said(text.Text)
			}
		}
	}
	if err := stream.Err(); err != nil {
		return Message{}, explain(err)
	}

	reply := Message{Role: Assistant, From: Anthropic}
	reply.Raw, _ = json.Marshal(whole.ToParam()) // made of what was just decoded, which encodes
	for _, block := range whole.Content {
		switch b := block.AsAny().(type) {
		case anthropic.TextBlock:
			reply.Content += b.Text
		case anthropic.ToolUseBlock:
			reply.Calls = append(reply.Calls, Call{ID: b.ID, Name: b.Name, Arguments: b.Input})
		}
	}
	switch whole.StopReason {
	case anthropic.StopReasonRefusal:
		return reply, fmt.Errorf("the model declined to answer: %s", strings.TrimSpace(string(whole.StopDetails.Category)+" "+whole.StopDetails.Explanation))
	case anthropic.StopReasonMaxTokens:
		return reply, errors.New("the model's reply was cut short: it ran out of room before it finished")
	}
	return reply, nil
}

// earlier puts one of the assistant's earlier messages as Claude is to be
// given it.
func (c claude) earlier(m Message, verbatim bool) anthropic.MessageParam {
	if verbatim && m.From == Anthropic && len(m.Raw) > 0 {
		return param.Override[anthropic.MessageParam](m.Raw)
	}
	var blocks []anthropic.ContentBlockParamUnion
	if m.Content != "" {
		blocks = append(blocks, anthropic.NewTextBlock(m.Content))
	}
	for _, call := range m.Calls {
		arguments := call.Arguments
		if !json.Valid(arguments) {
			arguments = json.RawMessage(`{}`)
		}
		blocks = append(blocks, anthropic.NewToolUseBlock(call.ID, arguments, call.Name))
	}
	return anthropic.NewAssistantMessage(blocks...)
}

// disowned reports whether the service refused a request because of the
// reasoning in it.
func disowned(err error) bool {
	var refused *anthropic.Error
	return errors.As(err, &refused) && refused.StatusCode == http.StatusBadRequest && strings.Contains(refused.RawJSON(), "`thinking` block")
}

// explain puts what the service refused in words for whoever set it up.
func explain(err error) error {
	var refused *anthropic.Error
	if err == nil {
		return nil
	}
	if strings.Contains(err.Error(), "no Anthropic credentials found") {
		return errors.New("Anthropic needs a key and this node has none: give it one with `sisyphusd model set --api-key-file`, or from the desktop, or set ANTHROPIC_API_KEY where the node runs")
	}
	if !errors.As(err, &refused) {
		return fmt.Errorf("reach the model service: %w", err)
	}
	switch refused.StatusCode {
	case http.StatusUnauthorized, http.StatusForbidden:
		return fmt.Errorf("Anthropic refused the key: %w", err)
	case http.StatusNotFound:
		return fmt.Errorf("Anthropic has no such model: %w", err)
	case http.StatusTooManyRequests:
		return fmt.Errorf("Anthropic is being asked too much just now; try again shortly: %w", err)
	}
	return fmt.Errorf("the model service answered: %w", err)
}

func (c claude) Models(ctx context.Context) ([]string, error) {
	var names []string
	pages := c.client.Models.ListAutoPaging(ctx, anthropic.ModelListParams{})
	for pages.Next() {
		names = append(names, pages.Current().ID)
	}
	return names, explain(pages.Err())
}

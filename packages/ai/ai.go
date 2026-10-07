// Package ai talks to language models, wherever they run: a model server on
// this machine or a service elsewhere. It knows Ollama's dialect, the one
// OpenAI defined, which most other servers and services speak too, and
// Anthropic's.
package ai

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
)

// The roles a message can have.
const (
	System    = "system"
	User      = "user"
	Assistant = "assistant"
	// ToolRole is the role of a message carrying what a tool returned.
	ToolRole = "tool"
)

// Message is one turn of a conversation.
type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
	// Calls are the tools an assistant's message asks to have run.
	Calls []Call `json:"calls,omitempty"`
	// CallID and Name say, on a tool's message, which call it answers.
	CallID string `json:"call_id,omitempty"`
	Name   string `json:"name,omitempty"`
	// Raw is an assistant's message as the service that wrote it gave it,
	// for a service that wants its replies handed back unchanged, and From
	// is that service's provider. Anthropic is the one that does.
	Raw  json.RawMessage `json:"raw,omitempty"`
	From string          `json:"from,omitempty"`
}

// Call is a model's request that a tool be run.
type Call struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	// Arguments is a JSON object.
	Arguments json.RawMessage `json:"arguments"`
}

// Tool is something a model may ask to have run.
type Tool struct {
	Name        string
	Description string
	// Parameters is the JSON Schema of the tool's arguments.
	Parameters json.RawMessage
}

// Request is a conversation so far, and what the model may do next.
type Request struct {
	Model    string
	Messages []Message
	Tools    []Tool
}

// Config says which model service to use and how to reach it.
type Config struct {
	// Provider is "ollama", "openai" or "anthropic". The second is for
	// OpenAI itself and for anything that speaks its dialect.
	Provider string
	// BaseURL is where the service is, such as http://127.0.0.1:11434 for
	// Ollama or https://api.openai.com/v1 for OpenAI. Empty means that
	// provider's usual place.
	BaseURL string
	// APIKey is the service's key. Ollama needs none, and for Anthropic an
	// empty one means the key Anthropic's own tools are set up with.
	APIKey string
}

// Provider is a model service.
type Provider interface {
	// Chat has the model continue a conversation. It calls said with each
	// piece of the reply's text as it arrives, and returns the whole reply:
	// its text, and any tools it asks to have run.
	Chat(ctx context.Context, req Request, said func(text string)) (Message, error)
	// Models lists the models the service offers.
	Models(ctx context.Context) ([]Model, error)
}

// Model is a model a service offers.
type Model struct {
	// Name is what the model is asked for by.
	Name string
	// Label is what the service calls it for people to read, if anything.
	Label string
	// Size is how much room the model takes on the machine serving it, in
	// bytes, if the service says.
	Size int64
	// Tools says whether the model can ask for tools to be run, which a
	// planner's model must.
	Tools Support
}

// Support is whether a model can do something, as far as is known.
type Support int

const (
	Unknown Support = iota
	Yes
	No
)

// Library is a service whose models are fetched to the machine it runs on
// before they can be used, and can be removed from it. Ollama is one.
type Library interface {
	// Pull fetches a model, reporting how far it has got as it goes.
	Pull(ctx context.Context, model string, progress func(Progress)) error
	// Remove deletes a model from the machine.
	Remove(ctx context.Context, model string) error
}

// Progress is how far the fetching of a model has got.
type Progress struct {
	// Status is what is being done, in the service's words.
	Status string
	// Done and Total are bytes of the part being fetched; both are zero
	// when the step is not one that is measured.
	Done, Total int64
}

// Kind describes one of the providers there are, for whoever is choosing.
type Kind struct {
	// ID is the provider as a Config names it.
	ID string
	// Name is what people call it, and About a line on what it is for.
	Name, About string
	// Place is where the service is if not told.
	Place string
	// Key says whether the service wants a key: No for none, Yes for
	// always, Unknown for some of those that speak the dialect.
	Key Support
	// Fetches says whether its models are fetched before use: whether a
	// provider of this kind is a Library.
	Fetches bool
}

// Kinds lists the providers there are, the one that needs no account first.
func Kinds() []Kind {
	return []Kind{
		{ID: Ollama, Name: "Ollama", About: "Models that run on this machine, or one you name, served by Ollama. Nothing leaves it.", Place: usualPlace[Ollama], Key: No, Fetches: true},
		{ID: Anthropic, Name: "Anthropic", About: "Claude, from Anthropic's service.", Place: usualPlace[Anthropic], Key: Yes},
		{ID: OpenAI, Name: "OpenAI and compatible", About: "OpenAI's service, or anything that speaks as it does: most hosted services, and local servers such as vLLM, llama.cpp and LM Studio.", Place: usualPlace[OpenAI], Key: Unknown},
	}
}

// The providers there are, and where each is found if not told.
const (
	Ollama    = "ollama"
	OpenAI    = "openai"
	Anthropic = "anthropic"
)

var usualPlace = map[string]string{Ollama: "http://127.0.0.1:11434", OpenAI: "https://api.openai.com/v1", Anthropic: "https://api.anthropic.com"}

// New returns the provider cfg describes.
func New(cfg Config) (Provider, error) {
	base, known := usualPlace[cfg.Provider]
	if !known {
		return nil, fmt.Errorf("unknown model provider %q: the providers are %q, %q and %q", cfg.Provider, Ollama, OpenAI, Anthropic)
	}
	if cfg.Provider == Anthropic {
		return newClaude(cfg), nil
	}
	if cfg.BaseURL != "" {
		base = cfg.BaseURL
	}
	s := service{base: strings.TrimRight(base, "/"), key: cfg.APIKey, client: &http.Client{}}
	if cfg.Provider == Ollama {
		return ollama{s}, nil
	}
	return openai{s}, nil
}

// service is what the two dialects have in common: JSON over HTTP.
type service struct {
	base, key string
	client    *http.Client
}

// send makes a request and returns the body of a successful reply, which
// the caller must close. body, if not nil, is sent as JSON.
func (s service) send(ctx context.Context, method, path string, body any) (io.ReadCloser, error) {
	var payload io.Reader
	if body != nil {
		encoded, _ := json.Marshal(body) // the requests are plain structs, which always encode
		payload = bytes.NewReader(encoded)
	}
	req, err := http.NewRequestWithContext(ctx, method, s.base+path, payload)
	if err != nil {
		return nil, fmt.Errorf("model service address: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if s.key != "" {
		req.Header.Set("Authorization", "Bearer "+s.key)
	}
	res, err := s.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("reach the model service: %w", err)
	}
	if res.StatusCode != http.StatusOK {
		defer res.Body.Close()
		said, _ := io.ReadAll(io.LimitReader(res.Body, 4096))
		return nil, fmt.Errorf("the model service answered %s: %s", res.Status, strings.TrimSpace(string(said)))
	}
	return res.Body, nil
}

// lines calls each with every non-empty line of a streamed reply.
func lines(body io.Reader, each func(line []byte) error) error {
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 0, 64<<10), 4<<20)
	for scanner.Scan() {
		if line := bytes.TrimSpace(scanner.Bytes()); len(line) > 0 {
			if err := each(line); err != nil {
				return err
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("read the model's reply: %w", err)
	}
	return nil
}

// ollama speaks Ollama's own dialect.
type ollama struct{ service }

type ollamaMessage struct {
	Role      string       `json:"role"`
	Content   string       `json:"content"`
	ToolCalls []ollamaCall `json:"tool_calls,omitempty"`
	ToolName  string       `json:"tool_name,omitempty"`
}

type ollamaCall struct {
	Function struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	} `json:"function"`
}

// function is how both dialects describe a tool to a model.
type function struct {
	Type     string `json:"type"`
	Function struct {
		Name        string          `json:"name"`
		Description string          `json:"description"`
		Parameters  json.RawMessage `json:"parameters"`
	} `json:"function"`
}

func functions(tools []Tool) []function {
	out := make([]function, 0, len(tools))
	for _, t := range tools {
		f := function{Type: "function"}
		f.Function.Name, f.Function.Description, f.Function.Parameters = t.Name, t.Description, t.Parameters
		out = append(out, f)
	}
	return out
}

func (o ollama) Chat(ctx context.Context, req Request, said func(string)) (Message, error) {
	messages := make([]ollamaMessage, 0, len(req.Messages))
	for _, m := range req.Messages {
		out := ollamaMessage{Role: m.Role, Content: m.Content, ToolName: m.Name}
		for _, call := range m.Calls {
			var c ollamaCall
			c.Function.Name, c.Function.Arguments = call.Name, call.Arguments
			out.ToolCalls = append(out.ToolCalls, c)
		}
		messages = append(messages, out)
	}
	body, err := o.send(ctx, http.MethodPost, "/api/chat", map[string]any{
		"model": req.Model, "messages": messages, "tools": functions(req.Tools), "stream": true,
	})
	if err != nil {
		return Message{}, err
	}
	defer body.Close()

	reply := Message{Role: Assistant}
	err = lines(body, func(line []byte) error {
		var part struct {
			Message ollamaMessage `json:"message"`
			Error   string        `json:"error"`
		}
		if err := json.Unmarshal(line, &part); err != nil {
			return fmt.Errorf("the model's reply is not what Ollama sends: %w", err)
		}
		if part.Error != "" {
			return fmt.Errorf("the model service reported: %s", part.Error)
		}
		if part.Message.Content != "" {
			reply.Content += part.Message.Content
			said(part.Message.Content)
		}
		for _, c := range part.Message.ToolCalls {
			// Ollama gives calls no IDs, so they are numbered here.
			reply.Calls = append(reply.Calls, Call{ID: fmt.Sprintf("call-%d", len(reply.Calls)+1), Name: c.Function.Name, Arguments: c.Function.Arguments})
		}
		return nil
	})
	return reply, err
}

func (o ollama) Models(ctx context.Context) ([]Model, error) {
	var listed struct {
		Models []struct {
			Name string `json:"name"`
			Size int64  `json:"size"`
		} `json:"models"`
	}
	if err := o.fetch(ctx, "/api/tags", &listed); err != nil {
		return nil, err
	}
	models := make([]Model, 0, len(listed.Models))
	for _, m := range listed.Models {
		models = append(models, Model{Name: m.Name, Size: m.Size, Tools: o.tools(ctx, m.Name)})
	}
	return models, nil
}

// tools asks whether a model can call tools. A server too old to say
// leaves it unknown.
func (o ollama) tools(ctx context.Context, model string) Support {
	body, err := o.send(ctx, http.MethodPost, "/api/show", map[string]any{"model": model})
	if err != nil {
		return Unknown
	}
	defer body.Close()
	var shown struct {
		Capabilities []string `json:"capabilities"`
	}
	if json.NewDecoder(body).Decode(&shown) != nil || len(shown.Capabilities) == 0 {
		return Unknown
	}
	if slices.Contains(shown.Capabilities, "tools") {
		return Yes
	}
	return No
}

func (o ollama) Pull(ctx context.Context, model string, progress func(Progress)) error {
	body, err := o.send(ctx, http.MethodPost, "/api/pull", map[string]any{"model": model, "stream": true})
	if err != nil {
		return err
	}
	defer body.Close()
	return lines(body, func(line []byte) error {
		var step struct {
			Status    string `json:"status"`
			Completed int64  `json:"completed"`
			Total     int64  `json:"total"`
			Error     string `json:"error"`
		}
		if err := json.Unmarshal(line, &step); err != nil {
			return fmt.Errorf("what came back is not what Ollama sends: %w", err)
		}
		if step.Error != "" {
			return fmt.Errorf("the model could not be fetched: %s", step.Error)
		}
		progress(Progress{Status: step.Status, Done: step.Completed, Total: step.Total})
		return nil
	})
}

func (o ollama) Remove(ctx context.Context, model string) error {
	body, err := o.send(ctx, http.MethodDelete, "/api/delete", map[string]any{"model": model})
	if err != nil {
		return err
	}
	return body.Close()
}

// fetch gets a JSON document from the service.
func (s service) fetch(ctx context.Context, path string, into any) error {
	body, err := s.send(ctx, http.MethodGet, path, nil)
	if err != nil {
		return err
	}
	defer body.Close()
	if err := json.NewDecoder(body).Decode(into); err != nil {
		return fmt.Errorf("the model service's reply is not JSON: %w", err)
	}
	return nil
}

// openai speaks the dialect OpenAI defined.
type openai struct{ service }

type openaiMessage struct {
	Role       string       `json:"role"`
	Content    string       `json:"content"`
	ToolCalls  []openaiCall `json:"tool_calls,omitempty"`
	ToolCallID string       `json:"tool_call_id,omitempty"`
}

type openaiCall struct {
	Index    int    `json:"index"`
	ID       string `json:"id,omitempty"`
	Type     string `json:"type,omitempty"`
	Function struct {
		Name string `json:"name,omitempty"`
		// Arguments is a JSON object as a string, which arrives in pieces.
		Arguments string `json:"arguments"`
	} `json:"function"`
}

func (o openai) Chat(ctx context.Context, req Request, said func(string)) (Message, error) {
	messages := make([]openaiMessage, 0, len(req.Messages))
	for _, m := range req.Messages {
		out := openaiMessage{Role: m.Role, Content: m.Content, ToolCallID: m.CallID}
		for _, call := range m.Calls {
			c := openaiCall{Index: len(out.ToolCalls), ID: call.ID, Type: "function"}
			c.Function.Name, c.Function.Arguments = call.Name, string(call.Arguments)
			out.ToolCalls = append(out.ToolCalls, c)
		}
		messages = append(messages, out)
	}
	request := map[string]any{"model": req.Model, "messages": messages, "stream": true}
	if len(req.Tools) > 0 {
		request["tools"] = functions(req.Tools)
	}
	body, err := o.send(ctx, http.MethodPost, "/chat/completions", request)
	if err != nil {
		return Message{}, err
	}
	defer body.Close()

	reply := Message{Role: Assistant}
	// A call's name and ID come once and its arguments in pieces, each
	// piece saying which call it belongs to.
	var arguments []string
	err = lines(body, func(line []byte) error {
		data, isData := bytes.CutPrefix(line, []byte("data:"))
		if data = bytes.TrimSpace(data); !isData || string(data) == "[DONE]" {
			return nil
		}
		var part struct {
			Choices []struct {
				Delta openaiMessage `json:"delta"`
			} `json:"choices"`
			Error *struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.Unmarshal(data, &part); err != nil {
			return fmt.Errorf("the model's reply is not what an OpenAI-style service sends: %w", err)
		}
		if part.Error != nil {
			return fmt.Errorf("the model service reported: %s", part.Error.Message)
		}
		for _, choice := range part.Choices {
			if choice.Delta.Content != "" {
				reply.Content += choice.Delta.Content
				said(choice.Delta.Content)
			}
			for _, c := range choice.Delta.ToolCalls {
				for c.Index >= len(reply.Calls) {
					reply.Calls = append(reply.Calls, Call{})
					arguments = append(arguments, "")
				}
				if c.ID != "" {
					reply.Calls[c.Index].ID = c.ID
				}
				if c.Function.Name != "" {
					reply.Calls[c.Index].Name = c.Function.Name
				}
				arguments[c.Index] += c.Function.Arguments
			}
		}
		return nil
	})
	for i := range reply.Calls {
		reply.Calls[i].Arguments = json.RawMessage(arguments[i])
	}
	return reply, err
}

func (o openai) Models(ctx context.Context) ([]Model, error) {
	var listed struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := o.fetch(ctx, "/models", &listed); err != nil {
		return nil, err
	}
	// Nothing in the dialect says what a model can do.
	models := make([]Model, 0, len(listed.Data))
	for _, m := range listed.Data {
		models = append(models, Model{Name: m.ID})
	}
	return models, nil
}

package ai

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fake is a model service that records what it is asked and answers each
// path with what it was given.
type fake struct {
	*httptest.Server
	answers map[string]string
	status  int
	asked   map[string]map[string]any
	headers http.Header
}

func newFake(t *testing.T, answers map[string]string) *fake {
	t.Helper()
	f := &fake{answers: answers, status: http.StatusOK, asked: make(map[string]map[string]any)}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.headers = r.Header
		body, _ := io.ReadAll(r.Body)
		var asked map[string]any
		json.Unmarshal(body, &asked)
		f.asked[r.URL.Path] = asked
		w.WriteHeader(f.status)
		io.WriteString(w, f.answers[r.URL.Path])
	}))
	t.Cleanup(f.Close)
	return f
}

func provider(t *testing.T, cfg Config) Provider {
	t.Helper()
	p, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

var conversation = Request{
	Model: "some-model",
	Messages: []Message{
		{Role: System, Content: "You plan computations."},
		{Role: User, Content: "How many primes below 100?"},
		{Role: Assistant, Calls: []Call{{ID: "call-1", Name: "run_job", Arguments: json.RawMessage(`{"workload":"primes"}`)}}},
		{Role: ToolRole, CallID: "call-1", Name: "run_job", Content: `{"count":25}`},
	},
	Tools: []Tool{{Name: "run_job", Description: "Runs a job.", Parameters: json.RawMessage(`{"type":"object"}`)}},
}

func TestOllamaChatStreamsTextAndCollectsToolCalls(t *testing.T) {
	f := newFake(t, map[string]string{"/api/chat": strings.Join([]string{
		`{"message":{"role":"assistant","content":"There "},"done":false}`,
		``,
		`{"message":{"role":"assistant","content":"are 25."},"done":false}`,
		`{"message":{"role":"assistant","content":"","tool_calls":[{"function":{"name":"run_job","arguments":{"workload":"primes","tasks":2}}},{"function":{"name":"get_job","arguments":{"job_id":"abc"}}}]},"done":false}`,
		`{"message":{"role":"assistant","content":""},"done":true}`,
	}, "\n")})
	var heard []string
	reply, err := provider(t, Config{Provider: Ollama, BaseURL: f.URL + "/"}).Chat(context.Background(), conversation, func(text string) { heard = append(heard, text) })
	if err != nil {
		t.Fatal(err)
	}
	if reply.Role != Assistant || reply.Content != "There are 25." || strings.Join(heard, "|") != "There |are 25." {
		t.Errorf("reply %+v, heard %q", reply, heard)
	}
	if len(reply.Calls) != 2 || reply.Calls[0].Name != "run_job" || string(reply.Calls[0].Arguments) != `{"workload":"primes","tasks":2}` ||
		reply.Calls[0].ID != "call-1" || reply.Calls[1].ID != "call-2" || reply.Calls[1].Name != "get_job" {
		t.Errorf("tool calls %+v", reply.Calls)
	}

	// What it was asked: the model, the conversation in Ollama's shape, the
	// tools, and to stream.
	asked := f.asked["/api/chat"]
	messages := asked["messages"].([]any)
	called := messages[2].(map[string]any)["tool_calls"].([]any)[0].(map[string]any)["function"].(map[string]any)
	answered := messages[3].(map[string]any)
	tool := asked["tools"].([]any)[0].(map[string]any)["function"].(map[string]any)
	if asked["model"] != "some-model" || asked["stream"] != true || len(messages) != 4 ||
		called["name"] != "run_job" || called["arguments"].(map[string]any)["workload"] != "primes" ||
		answered["role"] != "tool" || answered["tool_name"] != "run_job" || answered["content"] != `{"count":25}` ||
		tool["name"] != "run_job" || tool["description"] != "Runs a job." {
		t.Errorf("Ollama was asked %v", asked)
	}
	if f.headers.Get("Authorization") != "" {
		t.Error("a key was sent that was never given")
	}
}

func TestOpenAIChatStreamsTextAndAssemblesToolCalls(t *testing.T) {
	f := newFake(t, map[string]string{"/chat/completions": strings.Join([]string{
		`: a comment the service sends to keep the connection open`,
		`data: {"choices":[{"delta":{"role":"assistant","content":"Let me "}}]}`,
		`data: {"choices":[{"delta":{"content":"check."}}]}`,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_a","type":"function","function":{"name":"run_job","arguments":"{\"workload\":"}}]}}]}`,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":1,"id":"call_b","type":"function","function":{"name":"get_job","arguments":"{}"}}]}}]}`,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"primes\"}"}}]}}]}`,
		`data: {"choices":[]}`,
		`data: [DONE]`,
	}, "\n\n")})
	var heard []string
	p := provider(t, Config{Provider: OpenAI, BaseURL: f.URL, APIKey: "sk-test"})
	reply, err := p.Chat(context.Background(), conversation, func(text string) { heard = append(heard, text) })
	if err != nil {
		t.Fatal(err)
	}
	if reply.Content != "Let me check." || strings.Join(heard, "|") != "Let me |check." {
		t.Errorf("reply %+v, heard %q", reply, heard)
	}
	if len(reply.Calls) != 2 || reply.Calls[0].ID != "call_a" || reply.Calls[0].Name != "run_job" || string(reply.Calls[0].Arguments) != `{"workload":"primes"}` ||
		reply.Calls[1].ID != "call_b" || reply.Calls[1].Name != "get_job" || string(reply.Calls[1].Arguments) != `{}` {
		t.Errorf("tool calls %+v", reply.Calls)
	}

	asked := f.asked["/chat/completions"]
	messages := asked["messages"].([]any)
	called := messages[2].(map[string]any)["tool_calls"].([]any)[0].(map[string]any)
	answered := messages[3].(map[string]any)
	if asked["model"] != "some-model" || asked["stream"] != true || called["id"] != "call-1" || called["type"] != "function" ||
		called["function"].(map[string]any)["arguments"] != `{"workload":"primes"}` ||
		answered["role"] != "tool" || answered["tool_call_id"] != "call-1" || len(asked["tools"].([]any)) != 1 {
		t.Errorf("the service was asked %v", asked)
	}
	if got := f.headers.Get("Authorization"); got != "Bearer sk-test" {
		t.Errorf("the key was sent as %q", got)
	}

	// With no tools to offer, none are mentioned: some services refuse an
	// empty list.
	plain := Request{Model: "some-model", Messages: []Message{{Role: User, Content: "hello"}}}
	if _, err := p.Chat(context.Background(), plain, func(string) {}); err != nil {
		t.Fatal(err)
	}
	if _, mentioned := f.asked["/chat/completions"]["tools"]; mentioned {
		t.Error("an empty list of tools was sent")
	}
}

func TestListingModels(t *testing.T) {
	openaiFake := newFake(t, map[string]string{"/models": `{"data":[{"id":"gpt-5"},{"id":"gpt-5-mini"}]}`})
	got, err := provider(t, Config{Provider: OpenAI, BaseURL: openaiFake.URL}).Models(context.Background())
	if err != nil || len(got) != 2 || got[0] != (Model{Name: "gpt-5"}) || got[1].Name != "gpt-5-mini" {
		t.Errorf("the OpenAI-style service's models: %v, %v", got, err)
	}
}

func TestProvidersAndWhereTheyAreFound(t *testing.T) {
	if _, err := New(Config{Provider: "carrier-pigeon"}); err == nil || !strings.Contains(err.Error(), `unknown model provider "carrier-pigeon"`) {
		t.Errorf("an unknown provider: %v", err)
	}
	// Told nothing else, each is looked for in its usual place.
	if p := provider(t, Config{Provider: Ollama}).(ollama); p.base != "http://127.0.0.1:11434" {
		t.Errorf("Ollama is looked for at %s", p.base)
	}
	if p := provider(t, Config{Provider: OpenAI}).(openai); p.base != "https://api.openai.com/v1" {
		t.Errorf("OpenAI is looked for at %s", p.base)
	}
}

func TestRepliesThatCannotBeUsed(t *testing.T) {
	ctx := context.Background()
	quiet := func(string) {}
	for name, tt := range map[string]struct {
		provider, path, answer, want string
	}{
		"Ollama reporting an error":       {Ollama, "/api/chat", `{"error":"model 'nope' not found"}`, "model 'nope' not found"},
		"something that is not Ollama":    {Ollama, "/api/chat", `<html>hello</html>`, "not what Ollama sends"},
		"a service reporting an error":    {OpenAI, "/chat/completions", `data: {"error":{"message":"quota exceeded"}}`, "quota exceeded"},
		"something that is not a service": {OpenAI, "/chat/completions", `data: <html>`, "not what an OpenAI-style service sends"},
		"a line too long to be a reply":   {Ollama, "/api/chat", strings.Repeat("x", 5<<20), "read the model's reply"},
	} {
		f := newFake(t, map[string]string{tt.path: tt.answer})
		_, err := provider(t, Config{Provider: tt.provider, BaseURL: f.URL}).Chat(ctx, conversation, quiet)
		if err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("%s: %v, want an error containing %q", name, err, tt.want)
		}
	}

	// A service that refuses, one that is not there, and an address that is none.
	refusing := newFake(t, map[string]string{"/api/chat": `{"error":"unauthorized"}`, "/api/tags": "no", "/chat/completions": "no", "/models": "{"})
	refusing.status = http.StatusUnauthorized
	for _, kind := range []string{Ollama, OpenAI} {
		p := provider(t, Config{Provider: kind, BaseURL: refusing.URL})
		if _, err := p.Chat(ctx, conversation, quiet); err == nil || !strings.Contains(err.Error(), "401 Unauthorized") {
			t.Errorf("%s refused: %v", kind, err)
		}
		if _, err := p.Models(ctx); err == nil || !strings.Contains(err.Error(), "401 Unauthorized") {
			t.Errorf("%s refusing to list models: %v", kind, err)
		}
	}
	refusing.status = http.StatusOK
	for _, kind := range []string{Ollama, OpenAI} {
		if _, err := provider(t, Config{Provider: kind, BaseURL: refusing.URL}).Models(ctx); err == nil || !strings.Contains(err.Error(), "not JSON") {
			t.Errorf("%s listing models as something that is not JSON: %v", kind, err)
		}
	}
	gone := newFake(t, nil)
	gone.Close()
	if _, err := provider(t, Config{Provider: Ollama, BaseURL: gone.URL}).Chat(ctx, conversation, quiet); err == nil || !strings.Contains(err.Error(), "reach the model service") {
		t.Errorf("a service that is not there: %v", err)
	}
	if _, err := provider(t, Config{Provider: Ollama, BaseURL: "http://bad\x00address"}).Chat(ctx, conversation, quiet); err == nil || !strings.Contains(err.Error(), "model service address") {
		t.Errorf("an address that is none: %v", err)
	}
}

func TestTwoConfigurationsOfOneServiceAreKnownForOne(t *testing.T) {
	for name, tt := range map[string]struct {
		a, b Config
		same bool
	}{
		"the usual place, said and unsaid": {Config{Provider: Ollama}, Config{Provider: Ollama, BaseURL: "http://127.0.0.1:11434/"}, true},
		"nothing said twice":               {Config{Provider: Anthropic}, Config{Provider: Anthropic}, true},
		"another address":                  {Config{Provider: OpenAI}, Config{Provider: OpenAI, BaseURL: "https://example.org/v1"}, false},
		"another provider at one address":  {Config{Provider: Ollama, BaseURL: "http://h:1"}, Config{Provider: OpenAI, BaseURL: "http://h:1"}, false},
	} {
		if SameService(tt.a, tt.b) != tt.same || SameService(tt.b, tt.a) != tt.same {
			t.Errorf("%s: taken for the same service: %v", name, !tt.same)
		}
	}
}

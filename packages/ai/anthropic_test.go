package ai

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// anthropicFake stands in for Anthropic. It answers each request for a
// message with the next of the replies it was given, and records what it
// was asked.
type anthropicFake struct {
	*httptest.Server
	replies []reply
	asked   []map[string]any
	key     string
	// room is what it says a model's longest reply is; zero has it say it
	// knows no such model.
	room int
}

// reply is one answer: a stream of events, or a refusal.
type reply struct {
	status int
	body   string
}

func newAnthropicFake(t *testing.T, replies ...reply) *anthropicFake {
	t.Helper()
	f := &anthropicFake{replies: replies}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.key = r.Header.Get("X-Api-Key")
		// Told not to, the client does not try a refused request again.
		w.Header().Set("x-should-retry", "false")
		if strings.HasPrefix(r.URL.Path, "/v1/models") {
			w.Header().Set("Content-Type", "application/json")
		}
		switch {
		case r.URL.Path == "/v1/models":
			io.WriteString(w, `{"data":[{"id":"claude-opus-5-5","type":"model"},{"id":"claude-haiku-4-5","type":"model"}],"has_more":false}`)
		case strings.HasPrefix(r.URL.Path, "/v1/models/"):
			if f.room == 0 {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			fmt.Fprintf(w, `{"id":"claude-opus-5-5","type":"model","max_tokens":%d}`, f.room)
		default:
			body, _ := io.ReadAll(r.Body)
			var asked map[string]any
			json.Unmarshal(body, &asked)
			f.asked = append(f.asked, asked)
			next := f.replies[0]
			f.replies = f.replies[1:]
			if next.status != 0 {
				w.WriteHeader(next.status)
			}
			io.WriteString(w, next.body)
		}
	}))
	t.Cleanup(f.Close)
	return f
}

// events writes a stream as Anthropic sends one.
func events(each ...string) reply {
	var b strings.Builder
	for _, e := range each {
		var kind struct {
			Type string `json:"type"`
		}
		json.Unmarshal([]byte(e), &kind)
		fmt.Fprintf(&b, "event: %s\ndata: %s\n\n", kind.Type, e)
	}
	return reply{body: b.String()}
}

const started = `{"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","model":"claude-opus-5-5","content":[],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":3,"output_tokens":1}}}`

func stopped(reason string) string {
	return `{"type":"message_delta","delta":{"stop_reason":"` + reason + `","stop_sequence":null},"usage":{"output_tokens":9}}`
}

// thoughtThenCall is Claude reasoning, saying something, and asking for a
// tool.
var thoughtThenCall = events(started,
	`{"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":"","signature":""}}`,
	`{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"Count them."}}`,
	`{"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"signed"}}`,
	`{"type":"content_block_stop","index":0}`,
	`{"type":"content_block_start","index":1,"content_block":{"type":"text","text":""}}`,
	`{"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"Let me "}}`,
	`{"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"count."}}`,
	`{"type":"content_block_stop","index":1}`,
	`{"type":"content_block_start","index":2,"content_block":{"type":"tool_use","id":"toolu_1","name":"run_job","input":{}}}`,
	`{"type":"content_block_delta","index":2,"delta":{"type":"input_json_delta","partial_json":"{\"workload\":"}}`,
	`{"type":"content_block_delta","index":2,"delta":{"type":"input_json_delta","partial_json":"\"primes\"}"}}`,
	`{"type":"content_block_stop","index":2}`,
	stopped("tool_use"), `{"type":"message_stop"}`)

var answered = events(started,
	`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
	`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"25."}}`,
	`{"type":"content_block_stop","index":0}`,
	stopped("end_turn"), `{"type":"message_stop"}`)

func (f *anthropicFake) provider(t *testing.T, key string) Provider {
	t.Helper()
	return provider(t, Config{Provider: Anthropic, BaseURL: f.URL, APIKey: key})
}

func TestClaudeStreamsTextAndCollectsToolCallsAndKeepsItsReplyWhole(t *testing.T) {
	f := newAnthropicFake(t, thoughtThenCall)
	f.room = 128000
	// A conversation begun with another service, which asked for two tools.
	req := conversation
	req.Messages = []Message{
		{Role: System, Content: "You plan computations."},
		{Role: User, Content: "How many primes below 100?"},
		{Role: Assistant, Content: "Two jobs.", Calls: []Call{
			{ID: "call-1", Name: "run_job", Arguments: json.RawMessage(`{"workload":"primes"}`)},
			{ID: "call-2", Name: "run_job", Arguments: json.RawMessage(`not JSON`)},
		}},
		{Role: ToolRole, CallID: "call-1", Name: "run_job", Content: `{"count":25}`},
		{Role: ToolRole, CallID: "call-2", Name: "run_job", Content: `{"error":"what?"}`},
	}
	var heard []string
	got, err := f.provider(t, "sk-key").Chat(context.Background(), req, func(text string) { heard = append(heard, text) })
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(heard, "|") != "Let me |count." || got.Content != "Let me count." || got.Role != Assistant || got.From != Anthropic {
		t.Fatalf("heard %q, reply %+v", heard, got)
	}
	if len(got.Calls) != 1 || got.Calls[0].ID != "toolu_1" || got.Calls[0].Name != "run_job" || string(got.Calls[0].Arguments) != `{"workload":"primes"}` {
		t.Fatalf("calls = %+v", got.Calls)
	}
	// The reply is kept as it came, reasoning and its signature included.
	for _, want := range []string{`"thinking":"Count them."`, `"signature":"signed"`, `"toolu_1"`, `"role":"assistant"`} {
		if !strings.Contains(string(got.Raw), want) {
			t.Fatalf("kept reply %s lacks %s", got.Raw, want)
		}
	}

	asked := f.asked[0]
	sent, _ := json.Marshal(asked)
	if f.key != "sk-key" || asked["model"] != "some-model" || asked["max_tokens"] != float64(mostTokens) || asked["stream"] != true {
		t.Fatalf("key %q, asked %s", f.key, sent)
	}
	if system := asked["system"].([]any); len(system) != 1 || system[0].(map[string]any)["text"] != "You plan computations." {
		t.Fatalf("system = %v", system)
	}
	tool := asked["tools"].([]any)[0].(map[string]any)
	if tool["name"] != "run_job" || tool["description"] != "Runs a job." || tool["input_schema"].(map[string]any)["type"] != "object" {
		t.Fatalf("tool = %v", tool)
	}
	messages := asked["messages"].([]any)
	if len(messages) != 3 {
		t.Fatalf("messages = %s", sent)
	}
	said := messages[1].(map[string]any)["content"].([]any)
	if len(said) != 3 || said[0].(map[string]any)["text"] != "Two jobs." || said[1].(map[string]any)["id"] != "call-1" ||
		said[1].(map[string]any)["input"].(map[string]any)["workload"] != "primes" || len(said[2].(map[string]any)["input"].(map[string]any)) != 0 {
		t.Fatalf("the assistant's message was sent as %v", said)
	}
	// Both results go back in one message.
	results := messages[2].(map[string]any)
	if results["role"] != "user" || len(results["content"].([]any)) != 2 || results["content"].([]any)[1].(map[string]any)["tool_use_id"] != "call-2" {
		t.Fatalf("results = %v", results)
	}
}

func TestClaudeIsHandedItsEarlierRepliesAsItGaveThem(t *testing.T) {
	f := newAnthropicFake(t, thoughtThenCall, answered)
	p := f.provider(t, "sk-key")
	req := Request{Model: "claude-opus-5-5", Messages: []Message{{Role: User, Content: "How many primes below 100?"}}}
	first, err := p.Chat(context.Background(), req, func(string) {})
	if err != nil {
		t.Fatal(err)
	}
	req.Messages = append(req.Messages, first, Message{Role: ToolRole, CallID: "toolu_1", Content: `{"count":25}`})
	second, err := p.Chat(context.Background(), req, func(string) {})
	if err != nil || second.Content != "25." || len(second.Calls) != 0 {
		t.Fatalf("second = %+v, %v", second, err)
	}
	// The service did not say how long a reply may be, so the usual is asked for.
	if f.asked[0]["max_tokens"] != float64(usualTokens) {
		t.Fatalf("max_tokens = %v", f.asked[0]["max_tokens"])
	}
	back := f.asked[1]["messages"].([]any)[1].(map[string]any)["content"].([]any)
	if len(back) != 3 || back[0].(map[string]any)["type"] != "thinking" || back[0].(map[string]any)["signature"] != "signed" {
		t.Fatalf("handed back %v", back)
	}
}

func TestClaudeIsAskedAgainWithoutReasoningItNoLongerOwns(t *testing.T) {
	disowning := reply{status: http.StatusBadRequest, body: "{\"type\":\"error\",\"error\":{\"type\":\"invalid_request_error\",\"message\":\"messages.1.content.0: Invalid `signature` in `thinking` block. The block is bound to a different conversation.\"}}"}
	f := newAnthropicFake(t, thoughtThenCall, disowning, answered)
	p := f.provider(t, "sk-key")
	req := Request{Model: "claude-opus-5-5", Messages: []Message{{Role: User, Content: "How many?"}}}
	first, _ := p.Chat(context.Background(), req, func(string) {})
	req.Messages = append(req.Messages, first, Message{Role: ToolRole, CallID: "toolu_1", Content: `{"count":25}`})
	second, err := p.Chat(context.Background(), req, func(string) {})
	if err != nil || second.Content != "25." {
		t.Fatalf("second = %+v, %v", second, err)
	}
	again := f.asked[2]["messages"].([]any)[1].(map[string]any)["content"].([]any)
	if len(again) != 2 || again[0].(map[string]any)["type"] != "text" || again[1].(map[string]any)["id"] != "toolu_1" {
		t.Fatalf("asked again with %v", again)
	}
}

func TestWhatClaudeWillNotOrCannotAnswerIsReported(t *testing.T) {
	failure := func(status int) reply {
		return reply{status: status, body: `{"type":"error","error":{"type":"some_error","message":"no"}}`}
	}
	for name, c := range map[string]struct {
		reply reply
		want  string
	}{
		"a bad key":        {failure(http.StatusUnauthorized), "refused the key"},
		"no such model":    {failure(http.StatusNotFound), "no such model"},
		"too many":         {failure(http.StatusTooManyRequests), "try again shortly"},
		"something else":   {failure(http.StatusBadRequest), "the model service answered"},
		"a refusal":        {events(started, `{"type":"message_delta","delta":{"stop_reason":"refusal","stop_sequence":null,"stop_details":{"type":"refusal","category":"cyber","explanation":"Not that."}},"usage":{"output_tokens":1}}`, `{"type":"message_stop"}`), "declined to answer: cyber Not that."},
		"no room":          {events(started, stopped("max_tokens"), `{"type":"message_stop"}`), "cut short"},
		"a muddled stream": {events(started, `{"type":"content_block_start","index":4,"content_block":{"type":"text","text":""}}`), "not what Anthropic sends"},
	} {
		t.Run(name, func(t *testing.T) {
			f := newAnthropicFake(t, c.reply)
			_, err := f.provider(t, "sk-key").Chat(context.Background(), Request{Model: "m", Messages: []Message{{Role: User, Content: "hi"}}}, func(string) {})
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("err = %v, want %q", err, c.want)
			}
		})
	}
}

func TestClaudeSaysWhenItHasNoKey(t *testing.T) {
	for _, name := range []string{"ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "ANTHROPIC_PROFILE", "ANTHROPIC_FEDERATION_RULE_ID"} {
		t.Setenv(name, "")
	}
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	f := newAnthropicFake(t)
	if _, err := f.provider(t, "").Models(context.Background()); err == nil || !strings.Contains(err.Error(), "needs a key") {
		t.Fatalf("err = %v", err)
	}
}

func TestClaudeListsItsModelsAndSaysWhenItCannotBeReached(t *testing.T) {
	f := newAnthropicFake(t)
	models, err := f.provider(t, "sk-key").Models(context.Background())
	if err != nil || strings.Join(models, ",") != "claude-opus-5-5,claude-haiku-4-5" {
		t.Fatalf("models = %v, %v", models, err)
	}
	// A context that has ended is how a service that cannot be reached is
	// stood in for, without waiting out the client's retries.
	ended, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := f.provider(t, "sk-key").Models(ended); err == nil || !strings.Contains(err.Error(), "reach the model service") {
		t.Fatalf("err = %v", err)
	}
}

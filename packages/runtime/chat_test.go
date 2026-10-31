package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"
)

// modelServer stands in for an Ollama: it lists two models and answers a
// chat request as told, whole or, given pieces, a line to a piece.
func modelServer(t *testing.T, status int, reply string, pieces ...string) (*httptest.Server, *string) {
	t.Helper()
	asked := new(string)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/tags" {
			io.WriteString(w, `{"models":[{"name":"llama3.1:8b"},{"name":"qwen3:8b"}]}`)
			return
		}
		if r.URL.Path == "/api/ps" {
			// One is in memory, three quarters of it on a card. The rest is
			// what a server might say that means nothing: more on the card
			// than there is of it, no name, no size.
			io.WriteString(w, `{"models":[{"name":"llama3.1:8b","size":4000,"size_vram":3000},{"name":"qwen3:8b","size":10,"size_vram":90},{"name":"","size":5},{"name":"empty","size":0}]}`)
			return
		}
		body, _ := io.ReadAll(r.Body)
		*asked = r.Method + " " + r.URL.Path + " " + string(body)
		if len(pieces) > 0 {
			w.Header().Set("Content-Type", "application/x-ndjson")
			for _, piece := range pieces {
				fmt.Fprintf(w, "%s\n", piece)
				w.(http.Flusher).Flush()
				time.Sleep(time.Millisecond)
			}
			return
		}
		w.WriteHeader(status)
		io.WriteString(w, reply)
	}))
	t.Cleanup(server.Close)
	return server, asked
}

func TestAConversationIsOneTaskRunByAWorkersModelServer(t *testing.T) {
	ctx := context.Background()
	server, asked := modelServer(t, http.StatusOK, `{"model":"llama3.1:8b","message":{"role":"assistant","content":"Hello."},"done":true}`)
	chat := Chat{URL: server.URL + "/"}

	// However many parts are asked for there is one. The worker's server
	// is asked for the reply in pieces, by the name it knows the model by.
	payloads, err := chat.Split(ctx, nil, []byte(`{"model":"llama3.1:8b@rig","stream":false,"messages":[{"role":"user","content":"Hi"}]}`), 8)
	if err != nil || len(payloads) != 1 || !strings.Contains(string(payloads[0]), `"stream":true`) || !strings.Contains(string(payloads[0]), `"include_usage":true`) || !strings.Contains(string(payloads[0]), `"model":"llama3.1:8b"`) {
		t.Fatalf("Split = %s, %v", payloads, err)
	}
	// The server is asked in its own dialect, and a reply it sends whole is
	// taken at its word, and given back in OpenAI's.
	out, err := chat.Execute(ctx, nil, payloads[0])
	if err != nil || !strings.Contains(string(out), `"content":"Hello."`) || !strings.Contains(string(out), `"finish_reason":"stop"`) || !strings.Contains(string(out), `"object":"chat.completion"`) ||
		!strings.HasPrefix(*asked, `POST /api/chat {`) || !strings.Contains(*asked, `"stream":true`) || !strings.Contains(*asked, `"model":"llama3.1:8b"`) {
		t.Fatalf("Execute = %s, %v, having asked %s", out, err, *asked)
	}
	result, err := chat.Aggregate(ctx, nil, [][]byte{out})
	if err != nil || string(result) != string(out) {
		t.Fatalf("Aggregate = %s, %v", result, err)
	}
	if _, err := chat.Aggregate(ctx, nil, nil); err == nil || !strings.Contains(err.Error(), "one task") {
		t.Errorf("a conversation of no tasks: %v", err)
	}
	if chat.Name() != "chat" || !strings.Contains(chat.Describe(), `"model"`) {
		t.Errorf("chat calls itself %q and says %q", chat.Name(), chat.Describe())
	}

	for params, want := range map[string]string{
		`not JSON`:                       "not a chat request",
		`{"messages":[{"role":"user"}]}`: "must name a model",
		`{"model":"m"}`:                  "must have messages",
	} {
		if _, err := chat.Split(ctx, nil, []byte(params), 1); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("Split(%s): %v, want %q", params, err, want)
		}
	}
}

func TestAReplyThatComesInPiecesIsLoggedAsItComesAndPutTogether(t *testing.T) {
	server, _ := modelServer(t, 0, "",
		`{"model":"llama3.1:8b","message":{"role":"assistant","content":"Let me"}}`,
		`{"message":{"content":" count.\n","thinking":"Primes."}}`,
		`{"message":{"content":"","tool_calls":[{"function":{"name":"run_job","arguments":{"workload":"primes"}}}]}}`,
		// A call with no arguments has none to speak of.
		`{"message":{"content":"","tool_calls":[{"function":{"name":"get_job"}}]}}`,
		``,
		`{"done":true,"done_reason":"stop","prompt_eval_count":30,"eval_count":12}`,
	)
	said := &heard{}
	out, err := (Chat{URL: server.URL}).Execute(WithReporter(context.Background(), said), nil, []byte(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	var whole struct {
		ID, Model, Object string
		Usage             struct {
			TotalTokens int `json:"total_tokens"`
		}
		Choices []struct {
			FinishReason string `json:"finish_reason"`
			Message      struct {
				Role, Content, Reasoning string
				ToolCalls                []struct {
					ID, Type string
					Function struct{ Name, Arguments string }
				} `json:"tool_calls"`
			}
		}
	}
	if err := json.Unmarshal(out, &whole); err != nil {
		t.Fatalf("%s: %v", out, err)
	}
	message := whole.Choices[0].Message
	if !strings.HasPrefix(whole.ID, "chatcmpl-") || whole.Model != "llama3.1:8b" || whole.Object != "chat.completion" || whole.Usage.TotalTokens != 42 || whole.Choices[0].FinishReason != "tool_calls" ||
		message.Role != "assistant" || message.Content != "Let me count.\n" || message.Reasoning != "Primes." {
		t.Fatalf("put together: %s", out)
	}
	if calls := message.ToolCalls; len(calls) != 2 || calls[0].ID != "call_1" || calls[0].Type != "function" || calls[0].Function.Name != "run_job" || calls[0].Function.Arguments != `{"workload":"primes"}` ||
		calls[1].ID != "call_2" || calls[1].Function.Name != "get_job" || calls[1].Function.Arguments != "{}" {
		t.Errorf("the calls put together: %+v", calls)
	}
	// What was said was logged, each piece a JSON string, in order.
	var logged string
	for _, line := range said.lines {
		var piece string
		if err := json.Unmarshal([]byte(line), &piece); err != nil {
			t.Fatalf("logged %q: %v", line, err)
		}
		logged += piece
	}
	if logged != "Let me count.\n" {
		t.Errorf("logged %q", said.lines)
	}

	// Much said at once is passed on at once.
	long := &pieces{log: said.Log, last: time.Now()}
	said.lines = nil
	long.say(strings.Repeat("x", pieceSize))
	if len(said.lines) != 1 {
		t.Errorf("a long piece was logged %d times", len(said.lines))
	}
}

func TestPiecesThatCannotBePutTogetherFailTheTask(t *testing.T) {
	for want, pieces := range map[string][]string{
		"a piece that is not JSON":                 {`{"message":`},
		"the model server reported: out of memory": {`{"message":{"content":"So"}}`, `{"error":"out of memory"}`},
	} {
		server, _ := modelServer(t, 0, "", pieces...)
		if _, err := (Chat{URL: server.URL}).Execute(context.Background(), nil, []byte(`{}`)); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("pieces %v: %v, want %q", pieces, err, want)
		}
	}
	// A stream that stops before it is done.
	broken := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "1000")
		io.WriteString(w, "{\"message\":{\"content\":\"So\"}}\n")
	}))
	defer broken.Close()
	if _, err := (Chat{URL: broken.URL}).Execute(context.Background(), nil, []byte(`{}`)); err == nil || !strings.Contains(err.Error(), "read the model server's reply") {
		t.Errorf("a stream cut short: %v", err)
	}
}

func TestAWorkerOffersItsModelsAndAJobNeedsTheOneItNames(t *testing.T) {
	ctx := context.Background()
	server, _ := modelServer(t, http.StatusOK, `{}`)
	serving := Builtin().With(Chat{URL: server.URL})
	if got := strings.Join(serving.Offers(ctx), " "); got != "model:llama3.1:8b model:qwen3:8b loaded:llama3.1:8b=75 loaded:qwen3:8b=100" {
		t.Errorf("a worker with a model server offers %q", got)
	}
	if got := strings.Join(Models(append(serving.Offers(ctx), "region:eu")), " "); got != "llama3.1:8b qwen3:8b" {
		t.Errorf("its models are %q", got)
	}
	// Those in memory are read back out of the labels, and what is not a
	// share of a model is passed over.
	labels := append(serving.Offers(ctx), "loaded:none", "loaded:odd=most", "loaded:over=101", "loaded:under=-1", "loaded:a=b:latest=40")
	if got := fmt.Sprint(LoadedModels(labels)); got != "[{llama3.1:8b 75} {qwen3:8b 100} {a=b:latest 40}]" {
		t.Errorf("its models in memory are %s", got)
	}
	if got := strings.Join(serving.Names(), " "); got != "chat forecast forecasts primes wordcount" {
		t.Errorf("it runs %q", got)
	}
	// One that only takes such jobs in offers no model, only the
	// Chronos-Bolt and TimesFM images its containers run (and whether it
	// can upscale pictures, which images_test.go covers, or hold a miner
	// to its pool, which mining_test.go does); nor does one whose server
	// is not there or has no address to speak of offer any.
	upscale := func(label string) bool {
		return label == ImagesUpscaleLabel || label == ImagesUpscaleGPULabel || label == MiningEgressLabel
	}
	if got := slices.DeleteFunc(WithContainers().Offers(ctx), upscale); !slices.Equal(got, []string{ChronosLabel, TimesFMLabel}) {
		t.Errorf("a node with no model server offers %v", got)
	}
	server.Close()
	if got := serving.Offers(ctx); got != nil {
		t.Errorf("a worker whose model server has gone offers %v", got)
	}
	if got := (Chat{URL: "http://bad\x00address"}).Offers(ctx); got != nil {
		t.Errorf("a worker whose model server has no address offers %v", got)
	}

	if got := serving.Needs("chat", []byte(`{"model":"qwen3:8b"}`)); len(got) != 1 || got[0] != "model:qwen3:8b" {
		t.Errorf("a chat job needs %v", got)
	}
	if got := strings.Join(serving.Needs("chat", []byte(`{"model":"qwen3:8b@rig"}`)), " "); got != "model:qwen3:8b worker:rig" {
		t.Errorf("a chat job for one worker needs %q", got)
	}
	if got := serving.Needs("primes", []byte(`{}`)); got != nil {
		t.Errorf("a primes job needs %v", got)
	}
}

func TestAModelNamedLooselyIsTheOneItCanOnlyMean(t *testing.T) {
	served := []string{"llama3.1:8b", "llama3.3:70b", "qwen3:latest", "qwen3:8b", "Mistral:7b", "llama3.1:8b"}
	for asked, want := range map[string]string{
		"llama3.1:8b":     "llama3.1:8b",
		"LLAMA3.1:8B":     "llama3.1:8b",
		"qwen3":           "qwen3:latest", // the one with no tag is the latest
		"llama3.1":        "llama3.1:8b",  // the only one of its family
		"mistral":         "Mistral:7b",
		"llama3.1@rig":    "llama3.1:8b@rig",
		"llama3.1:8b@rig": "llama3.1:8b@rig",
		"gpt-5":           "",
		"llama3.1:70b":    "",
		"":                "",
	} {
		got, found := ResolveModel(asked, served)
		if got != want || found != (want != "") {
			t.Errorf("ResolveModel(%q) = %q, %v, want %q", asked, got, found, want)
		}
	}
	// A family of which the pool serves two names neither.
	if got, found := ResolveModel("llama3", []string{"llama3:8b", "llama3:70b"}); found {
		t.Errorf("a family of two was taken to mean %q", got)
	}
}

func TestAModelServerThatFailsFailsTheTask(t *testing.T) {
	ctx := context.Background()
	request := []byte(`{"model":"m","messages":[{"role":"user","content":"Hi"}]}`)
	refusing, _ := modelServer(t, http.StatusNotFound, `{"error":"model 'm' not found"}`)
	if _, err := (Chat{URL: refusing.URL}).Execute(ctx, nil, request); err == nil || !strings.Contains(err.Error(), "404 Not Found: {\"error\":\"model 'm' not found\"}") {
		t.Errorf("a server that refuses: %v", err)
	}
	muddled, _ := modelServer(t, http.StatusOK, `<html>`)
	if _, err := (Chat{URL: muddled.URL}).Execute(ctx, nil, request); err == nil || !strings.Contains(err.Error(), "not JSON") {
		t.Errorf("a reply that is not JSON: %v", err)
	}
	if _, err := (Chat{URL: muddled.URL}).Execute(ctx, nil, []byte(`not JSON`)); err == nil || !strings.Contains(err.Error(), "chat task") {
		t.Errorf("a task that is none: %v", err)
	}
	// A reply that stops short of the length it gave.
	short := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "100")
		io.WriteString(w, `{"message":`)
	}))
	defer short.Close()
	// Cut short in the middle of a line, what there is of it is no reply.
	if _, err := (Chat{URL: short.URL}).Execute(ctx, nil, request); err == nil || !strings.Contains(err.Error(), "not JSON") {
		t.Errorf("a reply cut short: %v", err)
	}
	short.Close()
	if _, err := (Chat{URL: short.URL}).Execute(ctx, nil, request); err == nil || !strings.Contains(err.Error(), "reach the model server") {
		t.Errorf("no server: %v", err)
	}
	if _, err := (Chat{URL: "http://bad\x00address"}).Execute(ctx, nil, request); err == nil || !strings.Contains(err.Error(), "model server address") {
		t.Errorf("an address that is none: %v", err)
	}
	if _, err := (Chat{}).Execute(ctx, nil, request); err == nil || !strings.Contains(err.Error(), "serves no language models") {
		t.Errorf("a worker with no server: %v", err)
	}
}

func TestAModelServerThatDoesNotAnswerDoesNotHoldAWorkerUp(t *testing.T) {
	old := tagsWait
	tagsWait = 50 * time.Millisecond
	defer func() { tagsWait = old }()
	release := make(chan struct{})
	silent := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { <-release }))
	defer silent.Close()
	defer close(release)
	began := time.Now()
	if got := (Chat{URL: silent.URL}).Offers(context.Background()); got != nil {
		t.Errorf("a server that says nothing offers %v", got)
	}
	if waited := time.Since(began); waited > 5*time.Second {
		t.Errorf("the worker waited %v to hear nothing", waited)
	}
}

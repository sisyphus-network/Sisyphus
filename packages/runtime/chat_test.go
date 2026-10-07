package runtime

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// modelServer stands in for an Ollama: it lists two models and answers a
// chat request as told.
func modelServer(t *testing.T, status int, reply string) (*httptest.Server, *string) {
	t.Helper()
	asked := new(string)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/tags" {
			io.WriteString(w, `{"models":[{"name":"llama3.1:8b"},{"name":"qwen3:8b"}]}`)
			return
		}
		body, _ := io.ReadAll(r.Body)
		*asked = r.Method + " " + r.URL.Path + " " + string(body)
		w.WriteHeader(status)
		io.WriteString(w, reply)
	}))
	t.Cleanup(server.Close)
	return server, asked
}

func TestAConversationIsOneTaskRunByAWorkersModelServer(t *testing.T) {
	ctx := context.Background()
	server, asked := modelServer(t, http.StatusOK, `{"choices":[{"message":{"role":"assistant","content":"Hello."}}]}`)
	chat := Chat{URL: server.URL + "/"}

	// However many parts are asked for there is one, asked for whole.
	payloads, err := chat.Split(ctx, nil, []byte(`{"model":"llama3.1:8b","stream":true,"stream_options":{"include_usage":true},"messages":[{"role":"user","content":"Hi"}]}`), 8)
	if err != nil || len(payloads) != 1 || !strings.Contains(string(payloads[0]), `"stream":false`) || strings.Contains(string(payloads[0]), "stream_options") {
		t.Fatalf("Split = %s, %v", payloads, err)
	}
	out, err := chat.Execute(ctx, nil, payloads[0])
	if err != nil || !strings.Contains(string(out), "Hello.") || !strings.HasPrefix(*asked, "POST /v1/chat/completions {") || !strings.Contains(*asked, `"llama3.1:8b"`) {
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

func TestAWorkerOffersItsModelsAndAJobNeedsTheOneItNames(t *testing.T) {
	ctx := context.Background()
	server, _ := modelServer(t, http.StatusOK, `{}`)
	serving := Builtin().With(Chat{URL: server.URL})
	if got := strings.Join(serving.Offers(ctx), " "); got != "model:llama3.1:8b model:qwen3:8b" {
		t.Errorf("a worker with a model server offers %q", got)
	}
	if got := strings.Join(Models(append(serving.Offers(ctx), "region:eu")), " "); got != "llama3.1:8b qwen3:8b" {
		t.Errorf("its models are %q", got)
	}
	if got := strings.Join(serving.Names(), " "); got != "chat primes wordcount" {
		t.Errorf("it runs %q", got)
	}
	// One that only takes such jobs in offers nothing, nor does one whose
	// server is not there.
	if got := WithContainers().Offers(ctx); got != nil {
		t.Errorf("a node with no model server offers %v", got)
	}
	server.Close()
	if got := serving.Offers(ctx); got != nil {
		t.Errorf("a worker whose model server has gone offers %v", got)
	}

	if got := serving.Needs("chat", []byte(`{"model":"qwen3:8b"}`)); len(got) != 1 || got[0] != "model:qwen3:8b" {
		t.Errorf("a chat job needs %v", got)
	}
	if got := serving.Needs("primes", []byte(`{}`)); got != nil {
		t.Errorf("a primes job needs %v", got)
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
	// A reply that stops short of the length it gave.
	short := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "100")
		io.WriteString(w, `{"choices":`)
	}))
	defer short.Close()
	if _, err := (Chat{URL: short.URL}).Execute(ctx, nil, request); err == nil || !strings.Contains(err.Error(), "read the model server's reply") {
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

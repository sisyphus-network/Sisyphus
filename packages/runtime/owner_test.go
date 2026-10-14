package runtime

import (
	"context"
	"net/http"
	"strings"
	"testing"
)

func TestAnOwnerOffersOnlyTheModelsTheyName(t *testing.T) {
	everything := Owner{}
	some := Owner{Only: []string{"llama3.1", " Qwen3:8B ", "gemma*", "phi4"}}
	for model, want := range map[string]bool{
		"llama3.1:8b":  true,  // the family
		"llama3.1:70b": true,  // every size of it
		"llama3.2:3b":  false, // another family
		"qwen3:8b":     true,  // named, in any case
		"qwen3:32b":    false, // another size of what is named
		"gemma3:4b":    true,  // a pattern
		"phi4:latest":  true,  // named without its tag
		"mistral":      false,
	} {
		if got := some.Allows(model); got != want {
			t.Errorf("offering %v, %s is allowed: %v", some.Only, model, got)
		}
		if !everything.Allows(model) {
			t.Errorf("offering everything, %s is not allowed", model)
		}
	}
}

func TestAWorkerServesNoModelItsOwnerDoesNotOffer(t *testing.T) {
	ctx := context.Background()
	server, asked := modelServer(t, http.StatusOK, `{}`)
	owner := Owner{Only: []string{"llama3.1"}}
	chat := Chat{URL: server.URL, Owner: owner}
	if got := strings.Join(chat.Offers(ctx), " "); got != "model:llama3.1:8b loaded:llama3.1:8b=75" {
		t.Errorf("offers %q", got)
	}
	// Asked for another anyway, by a coordinator that should have known
	// better, none of the three asks its server.
	*asked = ""
	_, chatErr := chat.Execute(ctx, nil, []byte(`{"model":"qwen3:8b","messages":[]}`))
	_, promptsErr := (Prompts{URL: server.URL, Owner: owner}).Execute(ctx, nil, []byte(`{"model":"qwen3:8b","prompts":["Hi"]}`))
	_, embedErr := (Embed{URL: server.URL, Owner: owner}).Execute(ctx, nil, []byte(`{"start":0,"request":{"model":"qwen3:8b","input":["Hi"]}}`))
	for _, err := range []error{chatErr, promptsErr, embedErr} {
		if err == nil || !strings.Contains(err.Error(), `does not offer the model "qwen3:8b"`) {
			t.Errorf("asked for a model not offered: %v", err)
		}
	}
	if *asked != "" {
		t.Errorf("the server was asked %s", *asked)
	}
}

func TestModelAllowlistChangesOnTheLiveModelServer(t *testing.T) {
	ctx := context.Background()
	server, asked := modelServer(t, http.StatusOK, `{"model":"qwen3:8b","message":{"role":"assistant","content":"ok"},"done":true}`)
	live := NewModelServer(server.URL, Owner{Only: []string{"llama3.1"}})
	chat := Chat{Live: live}
	if got := strings.Join(chat.Offers(ctx), " "); got != "model:llama3.1:8b loaded:llama3.1:8b=75" {
		t.Fatalf("initial offers %q", got)
	}

	live.Update(Owner{Only: []string{"qwen3"}, Options: map[string]any{"num_gpu": 0, "num_thread": 6, "num_ctx": 4096}, KeepAlive: "10m"})
	if got := strings.Join(chat.Offers(ctx), " "); got != "model:qwen3:8b loaded:qwen3:8b=100" {
		t.Fatalf("updated offers %q", got)
	}
	if _, err := chat.Execute(ctx, nil, []byte(`{"model":"qwen3:8b","messages":[{"role":"user","content":"hi"}]}`)); err != nil {
		t.Fatalf("chat with newly allowed model: %v", err)
	}
	for _, setting := range []string{`"num_gpu":0`, `"num_thread":6`, `"num_ctx":4096`, `"keep_alive":"10m"`} {
		if !strings.Contains(*asked, setting) {
			t.Errorf("live request is missing %s: %s", setting, *asked)
		}
	}
	for name, run := range map[string]func() error{
		"chat": func() error {
			_, err := chat.Execute(ctx, nil, []byte(`{"model":"llama3.1:8b","messages":[{"role":"user","content":"hi"}]}`))
			return err
		},
		"prompts": func() error {
			_, err := (Prompts{Live: live}).Execute(ctx, nil, []byte(`{"model":"llama3.1:8b","prompts":["hi"]}`))
			return err
		},
		"embed": func() error {
			_, err := (Embed{Live: live}).Execute(ctx, nil, []byte(`{"start":0,"request":{"model":"llama3.1:8b","input":["hi"]}}`))
			return err
		},
	} {
		if err := run(); err == nil || !strings.Contains(err.Error(), "does not offer") {
			t.Errorf("%s did not use the updated allowlist: %v", name, err)
		}
	}
}

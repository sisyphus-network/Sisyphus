package runtime

import (
	"encoding/json"
	"testing"
)

func TestARequestIsPutInOllamasDialectWithTheOwnersSettingsOverTheJobs(t *testing.T) {
	var request map[string]json.RawMessage
	json.Unmarshal([]byte(`{
		"model": "llava:7b",
		"messages": [
			{"role": "system", "content": "Be brief."},
			{"role": "user", "content": [{"type": "text", "text": "What is "}, {"type": "text", "text": "this?"}, {"type": "image_url", "image_url": {"url": "data:image/png;base64,iVBORw0"}}, {"type": "image_url", "image_url": {"url": "https://elsewhere/a.png"}}]},
			{"role": "assistant", "content": null, "tool_calls": [{"id": "c1", "type": "function", "function": {"name": "look", "arguments": "{\"at\":\"it\"}"}}, {"function": {"name": "odd", "arguments": "not JSON"}}]},
			{"role": "tool", "name": "look", "tool_call_id": "c1", "content": "a cat"}
		],
		"tools": [{"type": "function", "function": {"name": "look"}}],
		"temperature": 0.2, "max_tokens": 50, "seed": null,
		"response_format": {"type": "json_object"},
		"reasoning_effort": "low",
		"num_gpu": 99
	}`), &request)
	owner := Owner{Options: map[string]any{"num_gpu": 0, "num_ctx": 2048}, KeepAlive: "1m"}
	var got struct {
		Model    string
		Stream   bool
		Messages []struct {
			Role, Content string
			Images        []string
			ToolName      string `json:"tool_name"`
			ToolCalls     []struct {
				Function struct {
					Name      string
					Arguments map[string]string
				}
			} `json:"tool_calls"`
		}
		Tools     []any
		Options   map[string]any
		KeepAlive string `json:"keep_alive"`
		Format    any
		Think     string
	}
	if err := json.Unmarshal(toOllama(request, owner, true), &got); err != nil {
		t.Fatal(err)
	}
	m := got.Messages
	if got.Model != "llava:7b" || !got.Stream || len(m) != 4 || len(got.Tools) != 1 || got.KeepAlive != "1m" || got.Format != "json" || got.Think != "low" {
		t.Fatalf("translated: %+v", got)
	}
	if m[1].Content != "What is this?" || len(m[1].Images) != 1 || m[1].Images[0] != "iVBORw0" {
		t.Errorf("a message in parts: %+v", m[1])
	}
	if calls := m[2].ToolCalls; len(calls) != 2 || calls[0].Function.Name != "look" || calls[0].Function.Arguments["at"] != "it" || len(calls[1].Function.Arguments) != 0 {
		t.Errorf("the calls: %+v", calls)
	}
	if m[3].ToolName != "look" || m[3].Content != "a cat" {
		t.Errorf("a tool's answer: %+v", m[3])
	}
	// The job's settings that Ollama has are kept, an empty one dropped, and
	// the owner's put over them; a setting written where OpenAI has none
	// goes nowhere.
	if o := got.Options; o["temperature"] != 0.2 || o["num_predict"] != float64(50) || o["num_gpu"] != float64(0) || o["num_ctx"] != float64(2048) || len(o) != 4 {
		t.Errorf("the options: %v", o)
	}

	// A schema for the reply is passed on as it is; a request that asks
	// for nothing of the kind has no format, options or keeping alive.
	request = nil
	json.Unmarshal([]byte(`{"model":"m","messages":[],"response_format":{"type":"json_schema","json_schema":{"schema":{"type":"object"}}}}`), &request)
	if out := string(toOllama(request, Owner{}, false)); out != `{"format":{"type":"object"},"messages":[],"model":"m","stream":false}` {
		t.Errorf("with a schema: %s", out)
	}
}

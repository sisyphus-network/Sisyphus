package runtime

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"
)

// A worker's model server is spoken to in Ollama's own dialect, not the
// OpenAI one it also speaks: only its own takes the settings an owner puts
// on how a model uses the machine, such as how much of it goes on a
// graphics card, and how long it stays loaded. Jobs are still written, and
// answered, in OpenAI's dialect; what is between is translated here.

// toOllama turns a request in OpenAI's dialect into one in Ollama's, with
// the owner's settings over whatever the request asked.
func toOllama(request map[string]json.RawMessage, owner Owner, stream bool) []byte {
	var model string
	json.Unmarshal(request["model"], &model) // what is not a name names no model
	out := map[string]any{"model": model, "stream": stream}
	if raw, asked := request["messages"]; asked {
		var messages []map[string]json.RawMessage
		json.Unmarshal(raw, &messages) // what is not a list of messages is none
		said := make([]map[string]any, 0, len(messages))
		for _, m := range messages {
			said = append(said, ollamaMessage(m))
		}
		out["messages"] = said
	}
	if raw, asked := request["input"]; asked {
		out["input"] = raw
	}
	for _, same := range []string{"tools", "dimensions"} {
		if raw, asked := request[same]; asked {
			out[same] = raw
		}
	}
	// What OpenAI puts beside the messages, Ollama puts among its options.
	options := map[string]any{}
	for from, to := range map[string]string{
		"temperature": "temperature", "top_p": "top_p", "seed": "seed", "stop": "stop",
		"frequency_penalty": "frequency_penalty", "presence_penalty": "presence_penalty",
		"max_tokens": "num_predict", "max_completion_tokens": "num_predict",
	} {
		if raw, asked := request[from]; asked && string(raw) != "null" {
			options[to] = raw
		}
	}
	for setting, value := range owner.Options {
		options[setting] = value
	}
	if len(options) > 0 {
		out["options"] = options
	}
	if owner.KeepAlive != "" {
		out["keep_alive"] = owner.KeepAlive
	}
	var format struct {
		Type       string `json:"type"`
		JSONSchema struct {
			Schema json.RawMessage `json:"schema"`
		} `json:"json_schema"`
	}
	json.Unmarshal(request["response_format"], &format) // what is no format asks for none
	switch {
	case format.Type == "json_object":
		out["format"] = "json"
	case format.Type == "json_schema" && len(format.JSONSchema.Schema) > 0:
		out["format"] = format.JSONSchema.Schema
	}
	if effort, asked := request["reasoning_effort"]; asked {
		out["think"] = effort
	}
	return mustJSON(out)
}

// ollamaMessage is one message of a conversation in Ollama's dialect: its
// text in one piece, its pictures apart, and the arguments of the tools it
// called as JSON rather than JSON written in a string.
func ollamaMessage(m map[string]json.RawMessage) map[string]any {
	var role, name string
	json.Unmarshal(m["role"], &role)
	json.Unmarshal(m["name"], &name)
	out := map[string]any{"role": role}
	var text string
	if json.Unmarshal(m["content"], &text) != nil {
		// Content in parts: text, and pictures given as data.
		var parts []struct {
			Type     string `json:"type"`
			Text     string `json:"text"`
			ImageURL struct {
				URL string `json:"url"`
			} `json:"image_url"`
		}
		json.Unmarshal(m["content"], &parts)
		var images []string
		for _, part := range parts {
			text += part.Text
			if _, data, found := strings.Cut(part.ImageURL.URL, ";base64,"); found {
				images = append(images, data)
			}
		}
		if len(images) > 0 {
			out["images"] = images
		}
	}
	out["content"] = text
	if role == "tool" && name != "" {
		out["tool_name"] = name
	}
	var calls []struct {
		Function struct {
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
		} `json:"function"`
	}
	json.Unmarshal(m["tool_calls"], &calls)
	if len(calls) > 0 {
		said := make([]map[string]any, 0, len(calls))
		for _, call := range calls {
			arguments := json.RawMessage(call.Function.Arguments)
			if !json.Valid(arguments) {
				arguments = json.RawMessage("{}")
			}
			said = append(said, map[string]any{"function": map[string]any{"name": call.Function.Name, "arguments": arguments}})
		}
		out["tool_calls"] = said
	}
	return out
}

// ollamaReply puts a reply in Ollama's dialect, whole or in pieces, back
// together in OpenAI's.
type ollamaReply struct {
	model             string
	content, thinking strings.Builder
	calls             []map[string]any
	finish            string
	prompt, written   int
}

// add takes in one line of the reply and returns the text it adds.
func (r *ollamaReply) add(line []byte) (string, error) {
	var piece struct {
		Model   string `json:"model"`
		Message struct {
			Content   string `json:"content"`
			Thinking  string `json:"thinking"`
			ToolCalls []struct {
				Function struct {
					Name      string          `json:"name"`
					Arguments json.RawMessage `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"message"`
		DoneReason string `json:"done_reason"`
		Prompt     int    `json:"prompt_eval_count"`
		Written    int    `json:"eval_count"`
		Error      string `json:"error"`
	}
	if err := json.Unmarshal(line, &piece); err != nil {
		return "", fmt.Errorf("the model server sent a piece that is not JSON: %w", err)
	}
	if piece.Error != "" {
		return "", fmt.Errorf("the model server reported: %s", piece.Error)
	}
	if piece.Model != "" {
		r.model = piece.Model
	}
	r.content.WriteString(piece.Message.Content)
	r.thinking.WriteString(piece.Message.Thinking)
	for _, call := range piece.Message.ToolCalls {
		arguments := string(call.Function.Arguments)
		if arguments == "" {
			arguments = "{}"
		}
		// Ollama gives calls no IDs, so they are numbered here.
		r.calls = append(r.calls, map[string]any{
			"id": fmt.Sprintf("call_%d", len(r.calls)+1), "type": "function",
			"function": map[string]any{"name": call.Function.Name, "arguments": arguments},
		})
	}
	if piece.DoneReason != "" {
		r.finish = piece.DoneReason
	}
	r.prompt, r.written = max(r.prompt, piece.Prompt), max(r.written, piece.Written)
	return piece.Message.Content, nil
}

// read takes in a whole reply, a line at a time, passing on what each line
// adds to what the model has said.
func (r *ollamaReply) read(body io.Reader, said func(string)) error {
	lines := bufio.NewScanner(body)
	lines.Buffer(make([]byte, 0, 64<<10), 4<<20)
	for lines.Scan() {
		line := strings.TrimSpace(lines.Text())
		if line == "" {
			continue
		}
		text, err := r.add([]byte(line))
		if err != nil {
			return err
		}
		said(text)
	}
	if err := lines.Err(); err != nil {
		return fmt.Errorf("read the model server's reply: %w", err)
	}
	return nil
}

// json returns the reply as a server speaking OpenAI's dialect would have
// sent it whole.
func (r *ollamaReply) json() []byte {
	message := map[string]any{"role": "assistant", "content": r.content.String()}
	if r.thinking.Len() > 0 {
		message["reasoning"] = r.thinking.String()
	}
	finish := r.finish
	if len(r.calls) > 0 {
		message["tool_calls"] = r.calls
		finish = "tool_calls"
	}
	if finish == "" {
		finish = "stop"
	}
	now := time.Now()
	return mustJSON(map[string]any{
		"id": fmt.Sprintf("chatcmpl-%x", now.UnixNano()), "object": "chat.completion", "created": now.Unix(), "model": r.model,
		"choices": []any{map[string]any{"index": 0, "message": message, "finish_reason": finish}},
		"usage":   map[string]int{"prompt_tokens": r.prompt, "completion_tokens": r.written, "total_tokens": r.prompt + r.written},
	})
}

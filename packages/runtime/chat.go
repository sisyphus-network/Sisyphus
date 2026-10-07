package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// Chat has a language model on one of the pool's machines continue a
// conversation. It is how a pool thinks as well as computes: a worker
// whose owner has a model server offers its models, and a job names the
// model it wants and is given to a worker that has it.
//
// The job's parameters are a chat request as OpenAI defined it, and its
// result is the reply in the same form, so that anything able to talk to
// such a service can talk to the pool.
type Chat struct {
	// URL is where the worker's model server is: an Ollama. It is empty on
	// a node that takes such jobs in without running them.
	URL string
}

func (Chat) Name() string { return "chat" }

func (Chat) Describe() string {
	return `Has a language model served by one of the pool's workers continue a conversation. ` +
		`Parameters: a chat request as OpenAI defined it: {"model": "<a model a worker serves>", "messages": [{"role": "user", "content": "..."}], "tools": [optional]}. ` +
		`The result is the reply in the same form. The worker running it sees the conversation.`
}

// modelLabel is the label a worker has for each model it serves.
const modelLabel = "model:"

// Models picks the models out of a worker's labels.
func Models(labels []string) []string {
	var models []string
	for _, label := range labels {
		if name, is := strings.CutPrefix(label, modelLabel); is {
			models = append(models, name)
		}
	}
	return models
}

// modelOf returns the model a chat request names.
func modelOf(params []byte) string {
	var request struct {
		Model string `json:"model"`
	}
	json.Unmarshal(params, &request) // what is not a request names no model
	return request.Model
}

// Needs is the model the job asks for: its tasks go only to a worker that
// serves it.
func (Chat) Needs(params []byte) []string {
	return []string{modelLabel + modelOf(params)}
}

// Offers is the models the worker's server has, asked for each time the
// worker says what it is. A server that cannot be reached offers none.
func (c Chat) Offers(ctx context.Context) []string {
	if c.URL == "" {
		return nil
	}
	body, err := c.send(ctx, http.MethodGet, "/api/tags", nil)
	if err != nil {
		return nil
	}
	var listed struct {
		Models []struct {
			Name string `json:"name"`
		} `json:"models"`
	}
	json.Unmarshal(body, &listed) // a reply that is not a list lists nothing
	var labels []string
	for _, m := range listed.Models {
		labels = append(labels, modelLabel+m.Name)
	}
	return labels
}

// Split makes the one task a conversation is, however many parts are asked
// for: a reply cannot be shared out. The reply is asked for whole, since a
// task's output is given when the task is over.
func (Chat) Split(_ context.Context, _ Blobs, params []byte, _ int) ([][]byte, error) {
	var request map[string]json.RawMessage
	if err := json.Unmarshal(params, &request); err != nil {
		return nil, fmt.Errorf("chat parameters are not a chat request: %w", err)
	}
	if modelOf(params) == "" {
		return nil, errors.New("a chat request must name a model")
	}
	if len(request["messages"]) == 0 {
		return nil, errors.New("a chat request must have messages")
	}
	request["stream"] = json.RawMessage("false")
	delete(request, "stream_options")
	return [][]byte{mustJSON(request)}, nil
}

// maxReply is the longest reply taken from a model server.
const maxReply = 16 << 20

func (c Chat) Execute(ctx context.Context, _ Blobs, payload []byte) ([]byte, error) {
	if c.URL == "" {
		return nil, errors.New("this worker serves no language models")
	}
	reply, err := c.send(ctx, http.MethodPost, "/v1/chat/completions", payload)
	if err != nil {
		return nil, err
	}
	if !json.Valid(reply) {
		return nil, errors.New("the model server's reply is not JSON")
	}
	return reply, nil
}

func (Chat) Aggregate(_ context.Context, _ Blobs, outputs [][]byte) ([]byte, error) {
	if len(outputs) != 1 {
		return nil, fmt.Errorf("a conversation is one task, and %d reported", len(outputs))
	}
	return outputs[0], nil
}

// send makes a request of the worker's model server and returns what a
// successful reply holds.
func (c Chat) send(ctx context.Context, method, path string, body []byte) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(c.URL, "/")+path, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("model server address: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("reach the model server: %w", err)
	}
	defer res.Body.Close()
	said, err := io.ReadAll(io.LimitReader(res.Body, maxReply))
	if err != nil {
		return nil, fmt.Errorf("read the model server's reply: %w", err)
	}
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("the model server answered %s: %s", res.Status, strings.TrimSpace(string(said[:min(len(said), 4096)])))
	}
	return said, nil
}

package runtime

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Chat has a language model on one of the pool's machines continue a
// conversation. It is how a pool thinks as well as computes: a worker
// whose owner has a model server offers its models, and a job names the
// model it wants and is given to a worker that has it.
//
// The job's parameters are a chat request as OpenAI defined it, and its
// result is the reply in the same form, so that anything able to talk to
// such a service can talk to the pool. While the model writes, what it has
// written so far is logged in pieces, each a JSON string, so that whoever
// follows the job can show the reply as it comes.
//
// A model may be asked for on one worker by name, as "model@worker": the
// conversation then goes to that machine or waits for it, which is how to
// keep one on a machine you trust.
type Chat struct {
	// URL is where the worker's model server is: an Ollama. It is empty on
	// a node that takes such jobs in without running them.
	URL string
}

func (Chat) Name() string { return "chat" }

func (Chat) Describe() string {
	return `Has a language model served by one of the pool's workers continue a conversation. ` +
		`Parameters: a chat request as OpenAI defined it: {"model": "<a model a worker serves, exactly as the worker names it>", "messages": [{"role": "user", "content": "..."}], "tools": [optional]}. ` +
		`Write the model as "<model>@<worker's name>" to have that worker and no other run it. ` +
		`The result is the reply in the same form. The worker running it sees the conversation.`
}

// modelLabel is the label a worker has for each model it serves, and
// WorkerLabel the one it has for its own name.
const (
	modelLabel  = "model:"
	WorkerLabel = "worker:"
)

// ResolveModel finds, among the models a pool serves, the one a request
// means by asked, which may name it less exactly than the pool does: in
// another case, without the tag when there is a "latest", or by its family
// alone when the pool serves one model of that family. A worker asked for
// with "@" stays as asked. It returns false if the pool serves nothing that
// asked can be taken to mean, or more than one thing.
func ResolveModel(asked string, served []string) (string, bool) {
	name, worker, pinned := strings.Cut(asked, "@")
	if pinned {
		worker = "@" + worker
	}
	matching := func(same func(served string) bool) (string, int) {
		found, n := "", 0
		for _, s := range served {
			if same(s) && s != found {
				found, n = s, n+1
			}
		}
		return found, n
	}
	family := func(model string) string { f, _, _ := strings.Cut(model, ":"); return f }
	for _, same := range []func(string) bool{
		func(s string) bool { return s == name },
		func(s string) bool { return strings.EqualFold(s, name) },
		func(s string) bool { return strings.EqualFold(s, name+":latest") },
		func(s string) bool { return !strings.Contains(name, ":") && strings.EqualFold(family(s), name) },
	} {
		if found, n := matching(same); n == 1 {
			return found + worker, true
		} else if n > 1 {
			return "", false
		}
	}
	return "", false
}

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

// Needs is the model the job asks for, and the worker if it asks for one:
// its tasks go only to a worker that serves the model and has the name.
func (Chat) Needs(params []byte) []string { return modelNeeds(modelOf(params)) }

// Offers is the models the worker's server has, asked for each time the
// worker says what it is. A server that cannot be reached offers none.
func (c Chat) Offers(ctx context.Context) []string {
	if c.URL == "" {
		return nil
	}
	body, err := c.tags(ctx)
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
	// The worker is told the model by the name its server knows, and asks
	// for the reply in pieces, whatever the request said: the pieces are
	// logged as they come, and the task's output is the reply put together.
	model, _, _ := strings.Cut(modelOf(params), "@")
	request["model"] = mustJSON(model)
	request["stream"] = json.RawMessage("true")
	request["stream_options"] = json.RawMessage(`{"include_usage":true}`)
	return [][]byte{mustJSON(request)}, nil
}

// maxReply is the longest reply taken from a model server.
const maxReply = 16 << 20

func (c Chat) Execute(ctx context.Context, _ Blobs, payload []byte) ([]byte, error) {
	if c.URL == "" {
		return nil, errors.New("this worker serves no language models")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(c.URL, "/")+"/v1/chat/completions", bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("model server address: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("reach the model server: %w", err)
	}
	defer res.Body.Close()
	body := io.LimitReader(res.Body, maxReply)
	if res.StatusCode != http.StatusOK {
		said, _ := io.ReadAll(io.LimitReader(body, 4096))
		return nil, fmt.Errorf("the model server answered %s: %s", res.Status, strings.TrimSpace(string(said)))
	}
	// A server that does not send pieces sends the reply whole.
	if !strings.HasPrefix(res.Header.Get("Content-Type"), "text/event-stream") {
		reply, err := io.ReadAll(body)
		if err != nil {
			return nil, fmt.Errorf("read the model server's reply: %w", err)
		}
		if !json.Valid(reply) {
			return nil, errors.New("the model server's reply is not JSON")
		}
		return reply, nil
	}

	whole := newReply()
	said := &pieces{log: Report(ctx).Log, last: time.Now()}
	lines := bufio.NewScanner(body)
	lines.Buffer(make([]byte, 0, 64<<10), 4<<20)
	for lines.Scan() {
		data, isData := bytes.CutPrefix(bytes.TrimSpace(lines.Bytes()), []byte("data:"))
		if data = bytes.TrimSpace(data); !isData || string(data) == "[DONE]" {
			continue
		}
		text, err := whole.add(data)
		if err != nil {
			return nil, err
		}
		said.say(text)
	}
	if err := lines.Err(); err != nil {
		return nil, fmt.Errorf("read the model server's reply: %w", err)
	}
	said.flush()
	return whole.json(), nil
}

// pieces passes on what a model writes as it writes it, a few times a
// second rather than a word at a time: each piece is a line of the task's
// log, and a reply of a thousand words should not be a thousand lines.
type pieces struct {
	log     func(string)
	waiting strings.Builder
	last    time.Time
}

// pieceEvery is how often what has been written is passed on, and
// pieceSize how much is passed on without waiting.
const (
	pieceEvery = 100 * time.Millisecond
	pieceSize  = 1 << 10
)

func (p *pieces) say(text string) {
	p.waiting.WriteString(text)
	if p.waiting.Len() >= pieceSize || (p.waiting.Len() > 0 && time.Since(p.last) >= pieceEvery) {
		p.flush()
	}
}

func (p *pieces) flush() {
	if p.waiting.Len() > 0 {
		// As a JSON string, so that spaces and line ends survive the log.
		p.log(string(mustJSON(p.waiting.String())))
		p.waiting.Reset()
	}
	p.last = time.Now()
}

// reply puts a reply that came in pieces back together.
type reply struct {
	// top is what the pieces say of the reply as a whole: its ID, its
	// model, what it used.
	top map[string]any
	// message is the reply's message so far, calls the tools it asks
	// for, and finish why it ended.
	message map[string]any
	calls   []map[string]any
	finish  any
}

func newReply() *reply {
	return &reply{top: map[string]any{}, message: map[string]any{"role": "assistant", "content": ""}}
}

// add takes in one piece and returns the text it adds to what the model
// has said.
func (r *reply) add(data []byte) (string, error) {
	var piece map[string]any
	if err := json.Unmarshal(data, &piece); err != nil {
		return "", fmt.Errorf("the model server sent a piece that is not JSON: %w", err)
	}
	if failure, failed := piece["error"]; failed {
		return "", fmt.Errorf("the model server reported: %s", mustJSON(failure))
	}
	for key, value := range piece {
		if key != "choices" && value != nil {
			r.top[key] = value
		}
	}
	choices, _ := piece["choices"].([]any)
	if len(choices) == 0 {
		return "", nil
	}
	choice, _ := choices[0].(map[string]any)
	if reason := choice["finish_reason"]; reason != nil {
		r.finish = reason
	}
	delta, _ := choice["delta"].(map[string]any)
	text, _ := delta["content"].(string)
	for key, value := range delta {
		switch value := value.(type) {
		case string:
			// Text of any kind comes a little at a time; the role comes once.
			if before, _ := r.message[key].(string); key != "role" {
				r.message[key] = before + value
			}
		case []any:
			if key == "tool_calls" {
				r.call(value)
			}
		}
	}
	return text, nil
}

// call takes in pieces of the tools a reply asks for. A call's name and ID
// come once and its arguments a little at a time, each piece saying which
// call it is of.
func (r *reply) call(pieces []any) {
	for _, p := range pieces {
		piece, _ := p.(map[string]any)
		index := len(r.calls)
		if at, numbered := piece["index"].(float64); numbered {
			index = int(at)
		} else if id, _ := piece["id"].(string); id == "" && len(r.calls) > 0 {
			index = len(r.calls) - 1
		}
		if index < 0 || index > len(r.calls) {
			continue
		}
		if index == len(r.calls) {
			r.calls = append(r.calls, map[string]any{"type": "function", "function": map[string]any{"name": "", "arguments": ""}})
		}
		call := r.calls[index]
		if id, _ := piece["id"].(string); id != "" {
			call["id"] = id
		}
		into := call["function"].(map[string]any)
		from, _ := piece["function"].(map[string]any)
		if name, _ := from["name"].(string); name != "" {
			into["name"] = name
		}
		if arguments, _ := from["arguments"].(string); arguments != "" {
			into["arguments"] = into["arguments"].(string) + arguments
		}
	}
}

// json returns the reply whole, as a server that sent it whole would have.
func (r *reply) json() []byte {
	if len(r.calls) > 0 {
		r.message["tool_calls"] = r.calls
	}
	r.top["object"] = "chat.completion"
	r.top["choices"] = []any{map[string]any{"index": 0, "message": r.message, "finish_reason": r.finish}}
	return mustJSON(r.top)
}

func (Chat) Aggregate(_ context.Context, _ Blobs, outputs [][]byte) ([]byte, error) {
	if len(outputs) != 1 {
		return nil, fmt.Errorf("a conversation is one task, and %d reported", len(outputs))
	}
	return outputs[0], nil
}

// tags asks the worker's model server which models it has.
func (c Chat) tags(ctx context.Context) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(c.URL, "/")+"/api/tags", nil)
	if err != nil {
		return nil, err
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	return io.ReadAll(io.LimitReader(res.Body, maxReply))
}

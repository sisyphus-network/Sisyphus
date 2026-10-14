package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path"
	"strconv"
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
	// Owner is what the machine's owner allows of that server.
	Owner Owner
	// Live, when set, supplies the stable Ollama address and its current
	// owner policy. It lets model visibility and per-request limits change
	// without reconnecting the worker.
	Live *ModelServer
}

func (c Chat) settings() (string, Owner) {
	if c.Live != nil {
		return c.Live.Settings()
	}
	return c.URL, c.Owner
}

// Owner is what the owner of a worker allows the pool of its model server.
// The zero Owner allows everything the server has.
type Owner struct {
	// Only names the models offered: each a model as the server names it,
	// a family ("llama3.1", for every size and tag of it), or a pattern
	// with "*" in it. Empty offers every model the server has.
	Only []string
	// Options are Ollama's settings for running a model, put on every
	// request whatever the job asked: num_gpu, how many of a model's layers
	// go on graphics cards (0 for none); num_thread, how many of the
	// processor's threads it may use; num_ctx, how long a conversation it
	// holds. One left out is Ollama's to decide.
	Options map[string]any
	// KeepAlive is how long a model stays loaded after it last answered,
	// such as "10m", or "0" to unload it at once; empty is Ollama's own.
	KeepAlive string
}

// Allows reports whether the owner offers a model.
func (o Owner) Allows(model string) bool {
	if len(o.Only) == 0 {
		return true
	}
	model = strings.ToLower(model)
	family, _, _ := strings.Cut(model, ":")
	for _, only := range o.Only {
		only = strings.ToLower(strings.TrimSpace(only))
		if matched, _ := path.Match(only, model); matched || only == family || only+":latest" == model {
			return true
		}
	}
	return false
}

// refuses is the error for a model the owner does not offer, or nil.
func (o Owner) refuses(model string) error {
	if o.Allows(model) {
		return nil
	}
	return fmt.Errorf("this worker's owner does not offer the model %q", model)
}

func (Chat) Name() string { return "chat" }

func (Chat) Describe() string {
	return `Has a language model served by one of the pool's workers continue a conversation. ` +
		`Parameters: a chat request as OpenAI defined it: {"model": "<a model a worker serves, exactly as the worker names it>", "messages": [{"role": "user", "content": "..."}], "tools": [optional]}. ` +
		`Write the model as "<model>@<worker's name>", or "<model>@<worker's node ID>", to have that worker and no other run it. ` +
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
	url, owner := c.settings()
	if url == "" {
		return nil
	}
	body, err := c.askAt(ctx, url, "/api/tags")
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
		if owner.Allows(m.Name) {
			labels = append(labels, modelLabel+m.Name)
		}
	}
	return append(labels, c.loadedAt(ctx, url, owner)...)
}

// loadedLabel marks a model the worker's server has in memory now, and how
// much of it is on a graphics card: "loaded:<name>=<percent>".
const loadedLabel = "loaded:"

// Loaded is a model a worker's server has in memory, ready to answer.
type Loaded struct {
	Model string
	// OnCard is how much of the model is held by graphics cards, from 0 to
	// 100. The rest is in the machine's own memory, where the processor
	// does the work, and a reply comes that much slower.
	OnCard int
}

// LoadedModels picks out of a worker's labels the models its server had in
// memory when it last said what it offers.
func LoadedModels(labels []string) []Loaded {
	var loaded []Loaded
	for _, label := range labels {
		said, is := strings.CutPrefix(label, loadedLabel)
		at := strings.LastIndex(said, "=")
		if !is || at < 0 {
			continue
		}
		// A share that is no number, or no share, is some worker's invention.
		if share, err := strconv.Atoi(said[at+1:]); err == nil && share >= 0 && share <= 100 {
			loaded = append(loaded, Loaded{Model: said[:at], OnCard: share})
		}
	}
	return loaded
}

func (c Chat) loadedAt(ctx context.Context, url string, owner Owner) []string {
	body, _ := c.askAt(ctx, url, "/api/ps") // no answer is no list
	var running struct {
		Models []struct {
			Name string `json:"name"`
			Size uint64 `json:"size"`
			VRAM uint64 `json:"size_vram"`
		} `json:"models"`
	}
	json.Unmarshal(body, &running) // a reply that is not a list lists nothing
	var labels []string
	for _, m := range running.Models {
		if m.Name == "" || m.Size == 0 || !owner.Allows(m.Name) {
			continue
		}
		labels = append(labels, fmt.Sprintf("%s%s=%d", loadedLabel, m.Name, min(m.VRAM, m.Size)*100/m.Size))
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
	url, owner := c.settings()
	if url == "" {
		return nil, errors.New("this worker serves no language models")
	}
	if err := owner.refuses(modelOf(payload)); err != nil {
		return nil, err
	}
	var request map[string]json.RawMessage
	if err := json.Unmarshal(payload, &request); err != nil {
		return nil, fmt.Errorf("chat task: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(url, "/")+"/api/chat", bytes.NewReader(toOllama(request, owner, true)))
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
	whole := new(ollamaReply)
	said := &pieces{log: Report(ctx).Log, last: time.Now()}
	if err := whole.read(body, said.say); err != nil {
		return nil, err
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

func (Chat) Aggregate(_ context.Context, _ Blobs, outputs [][]byte) ([]byte, error) {
	if len(outputs) != 1 {
		return nil, fmt.Errorf("a conversation is one task, and %d reported", len(outputs))
	}
	return outputs[0], nil
}

// tagsWait is how long a worker waits to hear which models its server has.
// It asks whenever it says what it is, so a server that does not answer
// must not hold that up for long.
var tagsWait = 3 * time.Second

// ask asks the worker's model server about its models: which it has, or
// which it has in memory.
func (c Chat) ask(ctx context.Context, path string) ([]byte, error) {
	url, _ := c.settings()
	return c.askAt(ctx, url, path)
}

func (c Chat) askAt(ctx context.Context, url, path string) ([]byte, error) {
	ctx, done := context.WithTimeout(ctx, tagsWait)
	defer done()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(url, "/")+path, nil)
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

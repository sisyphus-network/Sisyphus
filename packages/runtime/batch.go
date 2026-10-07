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
	"sort"
	"strings"

	"github.com/ipfs/go-cid"
)

// Two workloads that put many of a pool's machines to thinking at once,
// where chat is one conversation on one machine: prompts has a model answer
// many prompts, and embed has one turn many texts into vectors. Each is
// shared out among the workers that serve the model.

// modelNeeds is what a job needs of a worker to use the model it names:
// that it serve it, and, for a model written "model@worker", that it be
// that worker.
func modelNeeds(model string) []string {
	name, worker, pinned := strings.Cut(model, "@")
	if pinned {
		return []string{modelLabel + name, WorkerLabel + worker}
	}
	return []string{modelLabel + name}
}

// askServer asks a worker's model server for something and returns the
// whole of a successful reply.
func askServer(ctx context.Context, url, path string, payload []byte) ([]byte, error) {
	if url == "" {
		return nil, errors.New("this worker serves no language models")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(url, "/")+path, bytes.NewReader(payload))
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

// shares cuts n things into at most parts runs of nearly equal length, and
// returns where each begins and ends.
func shares(n, parts int) [][2]int {
	parts = max(min(parts, n), 1)
	out := make([][2]int, 0, parts)
	for i := range parts {
		out = append(out, [2]int{n * i / parts, n * (i + 1) / parts})
	}
	return out
}

// Prompts has a language model answer many prompts, shared out among the
// workers that serve it.
type Prompts struct {
	// URL is where the worker's model server is; empty on a node that
	// takes such jobs in without running them.
	URL string
}

// PromptsParams says what to ask and of which model.
type PromptsParams struct {
	Model string `json:"model"`
	// System, if set, is said to the model before each prompt.
	System string `json:"system,omitempty"`
	// Prompts are what to ask; or Input is the CID of a stored text file
	// with a prompt on each line.
	Prompts []string `json:"prompts,omitempty"`
	Input   string   `json:"input,omitempty"`
	// Options are passed to the model server with each request, as a chat
	// request has them: temperature, max_tokens and so on.
	Options map[string]json.RawMessage `json:"options,omitempty"`
}

// promptsShare is one task's share: some of the prompts, and where in the
// whole list the first of them comes.
type promptsShare struct {
	PromptsParams
	Start int `json:"start"`
}

// answersShare is what one task returns.
type answersShare struct {
	Start   int      `json:"start"`
	Answers []string `json:"answers"`
}

func (Prompts) Name() string { return "prompts" }

func (Prompts) Describe() string {
	return `Has a language model served by the pool's workers answer many prompts, shared out among the workers that serve it. ` +
		`Parameters: {"model": "<a model workers serve, exactly as they name it>", "prompts": ["...", "..."], "system": "<optional instructions said before each>", "options": {<optional: temperature, max_tokens and the like>}}. ` +
		`Instead of "prompts", "input" may give the CID of a stored text file with one prompt on each line. ` +
		`The result is {"count": n, "answers": [...]} in the order asked, or for many answers {"count": n, "output": "<CID of a file with one answer, as a JSON string, on each line>"}. The workers running it see the prompts.`
}

func (Prompts) Needs(params []byte) []string { return modelNeeds(modelOf(params)) }

// maxPromptsFile is the longest file of prompts taken.
const maxPromptsFile = 64 << 20

func (Prompts) Split(ctx context.Context, blobs Blobs, params []byte, parts int) ([][]byte, error) {
	var p PromptsParams
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, fmt.Errorf("prompts parameters: %w", err)
	}
	if p.Model == "" {
		return nil, errors.New("prompts parameters must name a model")
	}
	if p.Input != "" {
		input, err := cid.Decode(p.Input)
		if err != nil {
			return nil, fmt.Errorf("prompts parameters: input is not a CID: %w", err)
		}
		blob, err := blobs.Open(ctx, input)
		if err != nil {
			return nil, fmt.Errorf("prompts input %s: %w", input, err)
		}
		lines := bufio.NewScanner(io.LimitReader(blob, maxPromptsFile))
		lines.Buffer(make([]byte, 0, 64<<10), 4<<20)
		for lines.Scan() {
			if line := strings.TrimSpace(lines.Text()); line != "" {
				p.Prompts = append(p.Prompts, line)
			}
		}
		blob.Close()
		if err := lines.Err(); err != nil {
			return nil, fmt.Errorf("prompts input %s: %w", input, err)
		}
		p.Input = ""
	}
	if len(p.Prompts) == 0 {
		return nil, errors.New("there are no prompts")
	}
	// The worker is told the model by the name its server knows.
	p.Model, _, _ = strings.Cut(p.Model, "@")
	all := p.Prompts
	var payloads [][]byte
	for _, share := range shares(len(all), parts) {
		p.Prompts = all[share[0]:share[1]]
		payloads = append(payloads, mustJSON(promptsShare{PromptsParams: p, Start: share[0]}))
	}
	return payloads, nil
}

func (w Prompts) Execute(ctx context.Context, _ Blobs, payload []byte) ([]byte, error) {
	var share promptsShare
	if err := json.Unmarshal(payload, &share); err != nil {
		return nil, fmt.Errorf("prompts task: %w", err)
	}
	report := Report(ctx)
	out := answersShare{Start: share.Start, Answers: make([]string, 0, len(share.Prompts))}
	for i, prompt := range share.Prompts {
		request := map[string]any{"model": share.Model, "stream": false}
		for option, value := range share.Options {
			request[option] = value
		}
		messages := []map[string]string{{"role": "user", "content": prompt}}
		if share.System != "" {
			messages = append([]map[string]string{{"role": "system", "content": share.System}}, messages...)
		}
		request["messages"] = messages
		said, err := askServer(ctx, w.URL, "/v1/chat/completions", mustJSON(request))
		if err != nil {
			return nil, fmt.Errorf("prompt %d: %w", share.Start+i+1, err)
		}
		var reply struct {
			Choices []struct {
				Message struct {
					Content string `json:"content"`
				} `json:"message"`
			} `json:"choices"`
		}
		if err := json.Unmarshal(said, &reply); err != nil || len(reply.Choices) == 0 {
			return nil, fmt.Errorf("prompt %d: the model server's reply is not a chat reply", share.Start+i+1)
		}
		out.Answers = append(out.Answers, reply.Choices[0].Message.Content)
		report.Progress(float64(i+1) / float64(len(share.Prompts)))
		report.Log(fmt.Sprintf("answered prompt %d", share.Start+i+1))
	}
	return mustJSON(out), nil
}

// maxInlineAnswers is how much of answers a job's result holds itself;
// more is stored and named.
const maxInlineAnswers = 256 << 10

func (Prompts) Aggregate(ctx context.Context, blobs Blobs, outputs [][]byte) ([]byte, error) {
	shares := make([]answersShare, len(outputs))
	for i, output := range outputs {
		if err := json.Unmarshal(output, &shares[i]); err != nil {
			return nil, fmt.Errorf("prompts task %d output: %w", i, err)
		}
	}
	sort.SliceStable(shares, func(a, b int) bool { return shares[a].Start < shares[b].Start })
	answers, size := []string{}, 0
	for _, share := range shares {
		for _, answer := range share.Answers {
			answers = append(answers, answer)
			size += len(answer)
		}
	}
	if size <= maxInlineAnswers {
		return mustJSON(map[string]any{"count": len(answers), "answers": answers}), nil
	}
	var file bytes.Buffer
	for _, answer := range answers {
		file.Write(mustJSON(answer))
		file.WriteByte('\n')
	}
	stored, err := blobs.Put(ctx, &file)
	if err != nil {
		return nil, fmt.Errorf("store the answers: %w", err)
	}
	return mustJSON(map[string]any{"count": len(answers), "output": stored.String()}), nil
}

// Embed has a model turn texts into vectors, shared out among the workers
// that serve it. Its parameters are an embeddings request as OpenAI defined
// it and its result the reply in the same form.
type Embed struct {
	// URL is where the worker's model server is; empty on a node that
	// takes such jobs in without running them.
	URL string
}

// MaxEmbedInputs is how many texts one embed job takes: what comes back
// for each is a thousand numbers or so, and a job's result is one message.
const MaxEmbedInputs = 128

func (Embed) Name() string { return "embed" }

func (Embed) Describe() string {
	return fmt.Sprintf(`Has an embedding model served by the pool's workers turn texts into vectors, shared out among the workers that serve it. `+
		`Parameters: an embeddings request as OpenAI defined it: {"model": "<an embedding model workers serve, exactly as they name it>", "input": ["...", "..."]}, with at most %d texts. `+
		`The result is the reply in the same form: {"data": [{"index": 0, "embedding": [...]}, ...]}. The workers running it see the texts.`, MaxEmbedInputs)
}

func (Embed) Needs(params []byte) []string { return modelNeeds(modelOf(params)) }

// embedShare is one task's share: a request for some of the texts, and
// where in the whole list the first of them comes.
type embedShare struct {
	Start   int                        `json:"start"`
	Request map[string]json.RawMessage `json:"request"`
}

// vectorsShare is what one task returns.
type vectorsShare struct {
	Start  int               `json:"start"`
	Model  string            `json:"model"`
	Data   []json.RawMessage `json:"data"`
	Tokens int               `json:"tokens"`
}

func (Embed) Split(_ context.Context, _ Blobs, params []byte, parts int) ([][]byte, error) {
	var request map[string]json.RawMessage
	if err := json.Unmarshal(params, &request); err != nil {
		return nil, fmt.Errorf("embed parameters are not an embeddings request: %w", err)
	}
	model := modelOf(params)
	if model == "" {
		return nil, errors.New("an embeddings request must name a model")
	}
	// One text may be given by itself, or several in a list.
	var texts []string
	var one string
	if json.Unmarshal(request["input"], &one) == nil {
		texts = []string{one}
	} else if err := json.Unmarshal(request["input"], &texts); err != nil || len(texts) == 0 {
		return nil, errors.New(`an embeddings request must have "input": a text or a list of texts`)
	}
	if len(texts) > MaxEmbedInputs {
		return nil, fmt.Errorf("an embeddings request may have at most %d texts, and this has %d: send them in several requests", MaxEmbedInputs, len(texts))
	}
	model, _, _ = strings.Cut(model, "@")
	request["model"] = mustJSON(model)
	var payloads [][]byte
	for _, share := range shares(len(texts), parts) {
		request["input"] = mustJSON(texts[share[0]:share[1]])
		payloads = append(payloads, mustJSON(embedShare{Start: share[0], Request: request}))
	}
	return payloads, nil
}

func (w Embed) Execute(ctx context.Context, _ Blobs, payload []byte) ([]byte, error) {
	var share embedShare
	if err := json.Unmarshal(payload, &share); err != nil {
		return nil, fmt.Errorf("embed task: %w", err)
	}
	said, err := askServer(ctx, w.URL, "/v1/embeddings", mustJSON(share.Request))
	if err != nil {
		return nil, err
	}
	var reply struct {
		Model string `json:"model"`
		Data  []struct {
			Index     int             `json:"index"`
			Embedding json.RawMessage `json:"embedding"`
		} `json:"data"`
		Usage struct {
			PromptTokens int `json:"prompt_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(said, &reply); err != nil || len(reply.Data) == 0 {
		return nil, errors.New("the model server's reply is not an embeddings reply")
	}
	sort.SliceStable(reply.Data, func(a, b int) bool { return reply.Data[a].Index < reply.Data[b].Index })
	out := vectorsShare{Start: share.Start, Model: reply.Model, Tokens: reply.Usage.PromptTokens}
	for _, d := range reply.Data {
		out.Data = append(out.Data, d.Embedding)
	}
	return mustJSON(out), nil
}

func (Embed) Aggregate(_ context.Context, _ Blobs, outputs [][]byte) ([]byte, error) {
	shares := make([]vectorsShare, len(outputs))
	for i, output := range outputs {
		if err := json.Unmarshal(output, &shares[i]); err != nil {
			return nil, fmt.Errorf("embed task %d output: %w", i, err)
		}
	}
	sort.SliceStable(shares, func(a, b int) bool { return shares[a].Start < shares[b].Start })
	type vector struct {
		Object    string          `json:"object"`
		Index     int             `json:"index"`
		Embedding json.RawMessage `json:"embedding"`
	}
	data, tokens, model := []vector{}, 0, ""
	for _, share := range shares {
		for _, embedding := range share.Data {
			data = append(data, vector{"embedding", len(data), embedding})
		}
		tokens += share.Tokens
		model = share.Model
	}
	return mustJSON(map[string]any{
		"object": "list", "model": model, "data": data,
		"usage": map[string]int{"prompt_tokens": tokens, "total_tokens": tokens},
	}), nil
}

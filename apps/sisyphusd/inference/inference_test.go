package inference

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	pb "github.com/sisyphus-network/Sisyphus/packages/protocol/sisyphus/v1"
)

// pool is a pool of two workers that serve models, which finishes every
// job as it is set to.
type pool struct {
	submitted []*pb.JobSpec
	cancelled []string
	// events is what a job logs before it ends, and result how it ends.
	events  []*pb.JobEvent
	result  string
	state   pb.JobState
	refuses error
	lost    error
	gone    error
}

func (p *pool) Submit(_ context.Context, spec *pb.JobSpec) (*pb.Job, error) {
	if p.refuses != nil {
		return nil, p.refuses
	}
	p.submitted = append(p.submitted, spec)
	return &pb.Job{JobId: "job-1"}, nil
}

func (p *pool) finished() *pb.Job {
	return &pb.Job{JobId: "job-1", State: p.state, Result: []byte(p.result), Error: "the worker's model server is down"}
}

func (p *pool) Watch(_ context.Context, _ string, fn func(*pb.Job) error) error {
	if p.lost != nil {
		return p.lost
	}
	return fn(p.finished())
}

func (p *pool) WatchEvents(_ context.Context, _ string, _ uint64, fn func(*pb.JobEvent) error) error {
	for _, e := range p.events {
		if err := fn(e); err != nil {
			return err
		}
	}
	return p.lost
}

func (p *pool) Get(string) (*pb.Job, error) { return p.finished(), p.gone }

func (p *pool) Cancel(jobID string) (*pb.Job, error) {
	p.cancelled = append(p.cancelled, jobID)
	return nil, nil
}

func (*pool) Nodes() []*pb.NodeInfo {
	return []*pb.NodeInfo{
		{Name: "rig", Capabilities: &pb.NodeCapabilities{Labels: []string{"model:qwen3:8b", "model:llama3.1:8b"}}},
		{Name: "den", Capabilities: &pb.NodeCapabilities{Labels: []string{"model:llama3.1:8b", "region:eu"}}},
		{},
	}
}

// ask makes a request of the service and returns its status and body.
func ask(t *testing.T, p *pool, method, path, key, body string) (int, string, http.Header) {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	rec := httptest.NewRecorder()
	Handler(p, "the-token").ServeHTTP(rec, req)
	said, _ := io.ReadAll(rec.Result().Body)
	return rec.Code, string(said), rec.Result().Header
}

const whole = `{"id":"c1","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"Two jobs.","tool_calls":[{"id":"a","type":"function","function":{"name":"run_job","arguments":"{}"}},{"id":"b","type":"function","function":{"name":"get_job","arguments":"{}"}},7]},"finish_reason":"tool_calls"}],"usage":{"total_tokens":9}}`

const hello = `{"model":"llama3.1:8b","messages":[{"role":"user","content":"Hi"}]}`

func TestThePoolsModelsAreListedOnceEach(t *testing.T) {
	status, body, _ := ask(t, &pool{}, http.MethodGet, "/v1/models", "the-token", "")
	var listed struct {
		Data []struct{ ID string }
	}
	json.Unmarshal([]byte(body), &listed)
	if status != http.StatusOK || len(listed.Data) != 2 || listed.Data[0].ID != "llama3.1:8b" || listed.Data[1].ID != "qwen3:8b" {
		t.Fatalf("models: %d %s", status, body)
	}
	for _, key := range []string{"", "another-token"} {
		if status, body, _ := ask(t, &pool{}, http.MethodGet, "/v1/models", key, ""); status != http.StatusUnauthorized || !strings.Contains(body, "invalid_api_key") {
			t.Errorf("with the key %q: %d %s", key, status, body)
		}
	}
}

func TestAReplyIsAJobGivenToAWorkerThatServesTheModel(t *testing.T) {
	p := &pool{state: pb.JobState_JOB_STATE_SUCCEEDED, result: whole}
	status, body, header := ask(t, p, http.MethodPost, "/v1/chat/completions", "the-token", hello)
	if status != http.StatusOK || body != whole || header.Get("X-Sisyphus-Job") != "job-1" || header.Get("Content-Type") != "application/json" {
		t.Fatalf("reply: %d %s %v", status, body, header)
	}
	spec := p.submitted[0]
	if spec.GetWorkload() != "chat" || !strings.Contains(string(spec.GetParams()), `"model":"llama3.1:8b"`) || !strings.Contains(string(spec.GetParams()), `"content":"Hi"`) || spec.GetMode() != pb.ScheduleMode_SCHEDULE_MODE_FULL_WORKER {
		t.Errorf("the job was %v", spec)
	}
	// A model named loosely is asked for as the pool knows it, and one
	// asked for on a worker by name stays on that worker.
	for asked, want := range map[string]string{"QWEN3": `"model":"qwen3:8b"`, "qwen3@rig": `"model":"qwen3:8b@rig"`, "llama3.1@den": `"model":"llama3.1:8b@den"`} {
		p.submitted = nil
		if status, body, _ := ask(t, p, http.MethodPost, "/v1/chat/completions", "the-token", strings.Replace(hello, "llama3.1:8b", asked, 1)); status != http.StatusOK || !strings.Contains(string(p.submitted[0].GetParams()), want) {
			t.Errorf("asked for %q: %d %s, the job was %s", asked, status, body, p.submitted)
		}
	}
}

// chunks reads a streamed reply: what each piece adds, and the last piece.
func chunks(t *testing.T, body string) (said string, last map[string]any, done bool) {
	t.Helper()
	for _, line := range strings.Split(body, "\n\n") {
		data := strings.TrimPrefix(line, "data: ")
		if data == "[DONE]" {
			done = true
			continue
		}
		if data == "" {
			continue
		}
		var piece map[string]any
		if err := json.Unmarshal([]byte(data), &piece); err != nil {
			t.Fatalf("the piece %q: %v", data, err)
		}
		last = piece
		if choices, _ := piece["choices"].([]any); len(choices) > 0 {
			text, _ := choices[0].(map[string]any)["delta"].(map[string]any)["content"].(string)
			said += text
		}
	}
	return said, last, done
}

func TestAReplyAskedForAsAStreamIsSentAsItIsWritten(t *testing.T) {
	streaming := strings.Replace(hello, "{", `{"stream":true,`, 1)
	p := &pool{state: pb.JobState_JOB_STATE_SUCCEEDED, result: whole, events: []*pb.JobEvent{
		{Kind: "submitted", Text: "chat, in 1 task"},
		{Kind: "log", Text: `"Two"`},
		{Kind: "log", Text: `" jobs."`},
		{Kind: "log", Text: `not a piece`},
		{Kind: "log", Text: `""`},
	}}
	status, body, header := ask(t, p, http.MethodPost, "/v1/chat/completions", "the-token", streaming)
	said, last, done := chunks(t, body)
	if status != http.StatusOK || header.Get("Content-Type") != "text/event-stream" || said != "Two jobs." || !done {
		t.Fatalf("streamed: %d %q", status, body)
	}
	// The last piece has what is known only at the end: the tools asked
	// for, each saying which it is, why the reply ended and what it used.
	choice := last["choices"].([]any)[0].(map[string]any)
	calls := choice["delta"].(map[string]any)["tool_calls"].([]any)
	if last["id"] != "chatcmpl-job-1" || last["model"] != "llama3.1:8b" || last["object"] != "chat.completion.chunk" || choice["finish_reason"] != "tool_calls" ||
		len(calls) != 3 || calls[0].(map[string]any)["index"] != float64(0) || calls[1].(map[string]any)["index"] != float64(1) || calls[1].(map[string]any)["id"] != "b" ||
		last["usage"].(map[string]any)["total_tokens"] != float64(9) {
		t.Errorf("the last piece: %v", last)
	}
	if _, repeated := choice["delta"].(map[string]any)["content"]; repeated {
		t.Errorf("what was said already was said again: %v", last)
	}

	// A worker that logged no pieces has its reply sent at the end, and a
	// reply with nothing in it ends as a reply does.
	p.events = nil
	if _, body, _ := ask(t, p, http.MethodPost, "/v1/chat/completions", "the-token", streaming); !strings.Contains(body, `"content":"Two jobs."`) {
		t.Errorf("a reply that came whole: %q", body)
	}
	p.result = `{}`
	if _, last, done := chunks(t, second(ask(t, p, http.MethodPost, "/v1/chat/completions", "the-token", streaming))); !done || last["choices"].([]any)[0].(map[string]any)["finish_reason"] != "stop" {
		t.Errorf("an empty reply ended with %v", last)
	}

	// A job that fails, or is not there to ask after, says so in the stream.
	for name, failing := range map[string]*pool{"failed": {state: pb.JobState_JOB_STATE_FAILED}, "gone": {state: pb.JobState_JOB_STATE_SUCCEEDED, gone: errors.New("pruned")}} {
		_, body, _ := ask(t, failing, http.MethodPost, "/v1/chat/completions", "the-token", streaming)
		if _, last, done := chunks(t, body); !done || last["error"] == nil {
			t.Errorf("a job that %s: %q", name, body)
		}
	}
	// A worker lost part way has another begin the reply again. What was
	// sent cannot be unsent, so the stream ends saying so, and the job is
	// stopped. One that had sent nothing yet just carries on.
	again := &pool{state: pb.JobState_JOB_STATE_SUCCEEDED, result: whole, events: []*pb.JobEvent{
		{Kind: "task-started", Text: "attempt 1"}, {Kind: "log", Text: `"Two"`}, {Kind: "task-lost"}, {Kind: "task-started", Text: "attempt 2"}, {Kind: "log", Text: `"Two jobs."`},
	}}
	_, body, _ = ask(t, again, http.MethodPost, "/v1/chat/completions", "the-token", streaming)
	if said, last, done := chunks(t, body); said != "Two" || !done || last["error"] == nil || !strings.Contains(body, "lost part way") || len(again.cancelled) != 1 {
		t.Errorf("a reply begun again: %q, cancelled %v", body, again.cancelled)
	}
	early := &pool{state: pb.JobState_JOB_STATE_SUCCEEDED, result: whole, events: []*pb.JobEvent{
		{Kind: "task-started"}, {Kind: "task-lost"}, {Kind: "task-started"}, {Kind: "log", Text: `"Two jobs."`},
	}}
	if said, _, _ := chunks(t, second(ask(t, early, http.MethodPost, "/v1/chat/completions", "the-token", streaming))); said != "Two jobs." || len(early.cancelled) != 0 {
		t.Errorf("a reply begun again before anything was sent: %q", said)
	}

	// A reply nobody waits for any longer is not made.
	left := &pool{lost: context.Canceled}
	ask(t, left, http.MethodPost, "/v1/chat/completions", "the-token", streaming)
	if len(left.cancelled) != 1 {
		t.Errorf("a stream nobody reads: cancelled %v", left.cancelled)
	}
}

func second(_ int, body string, _ http.Header) string { return body }

func TestWhatCannotBeAnsweredIsRefusedAsSuchAServiceRefuses(t *testing.T) {
	for name, tt := range map[string]struct {
		pool   *pool
		body   string
		status int
		want   string
	}{
		"not JSON":                   {&pool{}, `Hi`, http.StatusBadRequest, "not JSON"},
		"too long":                   {&pool{}, strings.Repeat("x", maxRequest+1), http.StatusRequestEntityTooLarge, "longer than this service takes"},
		"a model no worker serves":   {&pool{}, `{"model":"gpt-5"}`, http.StatusNotFound, `no worker connected to the pool serves a model that "gpt-5" can only mean; those served are: llama3.1:8b, qwen3:8b`},
		"a worker without the model": {&pool{}, `{"model":"qwen3:8b@den"}`, http.StatusNotFound, `the worker "den" is not connected to the pool, or serves no model that "qwen3:8b" can only mean; those served are: llama3.1:8b`},
		"a request the pool refuses": {&pool{refuses: errors.New("a chat request must have messages")}, hello, http.StatusBadRequest, "must have messages"},
		"a job that fails":           {&pool{state: pb.JobState_JOB_STATE_FAILED}, hello, http.StatusBadGateway, "job job-1 did not produce a reply: the worker's model server is down"},
	} {
		status, body, _ := ask(t, tt.pool, http.MethodPost, "/v1/chat/completions", "the-token", tt.body)
		var refusal struct {
			Error struct{ Message, Type string }
		}
		if json.Unmarshal([]byte(body), &refusal); status != tt.status || !strings.Contains(refusal.Error.Message, tt.want) || refusal.Error.Type == "" {
			t.Errorf("%s: %d %s", name, status, body)
		}
	}
	// A reply nobody waits for any longer is not made.
	gone := &pool{lost: context.Canceled}
	if status, _, _ := ask(t, gone, http.MethodPost, "/v1/chat/completions", "the-token", hello); status != http.StatusServiceUnavailable || len(gone.cancelled) != 1 || gone.cancelled[0] != "job-1" {
		t.Errorf("a reply not waited for: %d, cancelled %v", status, gone.cancelled)
	}
	if status, _, _ := ask(t, &pool{}, http.MethodGet, "/v1/images", "the-token", ""); status != http.StatusNotFound {
		t.Errorf("something this service does not do: %d", status)
	}
}

func TestTextsAreTurnedIntoVectorsByAJobSharedAmongWorkers(t *testing.T) {
	const vectors = `{"object":"list","data":[{"index":0,"embedding":[0.1]}]}`
	p := &pool{state: pb.JobState_JOB_STATE_SUCCEEDED, result: vectors}
	status, body, header := ask(t, p, http.MethodPost, "/v1/embeddings", "the-token", `{"model":"QWEN3","input":["a","b"]}`)
	if status != http.StatusOK || body != vectors || header.Get("X-Sisyphus-Job") != "job-1" {
		t.Fatalf("embeddings: %d %s", status, body)
	}
	spec := p.submitted[0]
	if spec.GetWorkload() != "embed" || !strings.Contains(string(spec.GetParams()), `"model":"qwen3:8b"`) || spec.GetMode() != pb.ScheduleMode_SCHEDULE_MODE_UNSPECIFIED {
		t.Errorf("the job was %v", spec)
	}
	for name, tt := range map[string]struct {
		pool   *pool
		body   string
		status int
	}{
		"a model no worker serves":   {&pool{}, `{"model":"gpt-5","input":"a"}`, http.StatusNotFound},
		"a request the pool refuses": {&pool{refuses: errors.New("at most 128 texts")}, `{"model":"qwen3:8b","input":"a"}`, http.StatusBadRequest},
		"a job that fails":           {&pool{state: pb.JobState_JOB_STATE_FAILED}, `{"model":"qwen3:8b","input":"a"}`, http.StatusBadGateway},
	} {
		if status, body, _ := ask(t, tt.pool, http.MethodPost, "/v1/embeddings", "the-token", tt.body); status != tt.status || !strings.Contains(body, `"error"`) {
			t.Errorf("%s: %d %s", name, status, body)
		}
	}
}

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
	result    string
	state     pb.JobState
	refuses   error
	lost      error
}

func (p *pool) Submit(_ context.Context, spec *pb.JobSpec) (*pb.Job, error) {
	if p.refuses != nil {
		return nil, p.refuses
	}
	p.submitted = append(p.submitted, spec)
	return &pb.Job{JobId: "job-1"}, nil
}

func (p *pool) Watch(_ context.Context, jobID string, fn func(*pb.Job) error) error {
	if p.lost != nil {
		return p.lost
	}
	return fn(&pb.Job{JobId: jobID, State: p.state, Result: []byte(p.result), Error: "the worker's model server is down"})
}

func (p *pool) Cancel(jobID string) (*pb.Job, error) {
	p.cancelled = append(p.cancelled, jobID)
	return nil, nil
}

func (*pool) Nodes() []*pb.NodeInfo {
	return []*pb.NodeInfo{
		{Capabilities: &pb.NodeCapabilities{Labels: []string{"model:qwen3:8b", "model:llama3.1:8b"}}},
		{Capabilities: &pb.NodeCapabilities{Labels: []string{"model:llama3.1:8b", "region:eu"}}},
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

const whole = `{"id":"c1","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"","tool_calls":[{"id":"a","type":"function","function":{"name":"run_job","arguments":"{}"}},{"id":"b","type":"function","function":{"name":"get_job","arguments":"{}"}}]},"finish_reason":"tool_calls"}]}`

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
	if spec.GetWorkload() != "chat" || string(spec.GetParams()) != hello || spec.GetMode() != pb.ScheduleMode_SCHEDULE_MODE_FULL_WORKER {
		t.Errorf("the job was %v", spec)
	}

	// Asked for as a stream, the reply comes as a stream of one piece, in
	// which each tool call says which it is.
	streamed := strings.Replace(hello, "{", `{"stream":true,`, 1)
	status, body, header = ask(t, p, http.MethodPost, "/v1/chat/completions", "the-token", streamed)
	data, done, _ := strings.Cut(strings.TrimPrefix(body, "data: "), "\n\n")
	var chunk struct {
		Object  string
		Choices []struct {
			Message any
			Delta   struct {
				ToolCalls []struct {
					Index int
					ID    string
				} `json:"tool_calls"`
			}
		}
	}
	if err := json.Unmarshal([]byte(data), &chunk); err != nil {
		t.Fatalf("the piece %q: %v", data, err)
	}
	calls := chunk.Choices[0].Delta.ToolCalls
	if status != http.StatusOK || header.Get("Content-Type") != "text/event-stream" || done != "data: [DONE]\n\n" || chunk.Object != "chat.completion.chunk" ||
		chunk.Choices[0].Message != nil || len(calls) != 2 || calls[0].Index != 0 || calls[1].Index != 1 || calls[1].ID != "b" {
		t.Errorf("streamed: %d %q", status, body)
	}
	// A reply of an odd shape is passed on for its reader to make of what it can.
	p.result = `{"choices":[null,{"message":{"tool_calls":[7]}}]}`
	if status, body, _ := ask(t, p, http.MethodPost, "/v1/chat/completions", "the-token", streamed); status != http.StatusOK || !strings.Contains(body, `"delta":{"tool_calls":[7]}`) {
		t.Errorf("an odd reply streamed: %d %q", status, body)
	}
}

func TestWhatCannotBeAnsweredIsRefusedAsSuchAServiceRefuses(t *testing.T) {
	for name, tt := range map[string]struct {
		pool   *pool
		body   string
		status int
		want   string
	}{
		"not JSON":                   {&pool{}, `Hi`, http.StatusBadRequest, "not JSON"},
		"too long":                   {&pool{}, strings.Repeat("x", maxRequest+1), http.StatusRequestEntityTooLarge, "longer than this service takes"},
		"a model no worker serves":   {&pool{}, `{"model":"gpt-5"}`, http.StatusNotFound, `those served are: llama3.1:8b, qwen3:8b`},
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
	if status, _, _ := ask(t, &pool{}, http.MethodGet, "/v1/embeddings", "the-token", ""); status != http.StatusNotFound {
		t.Errorf("something this service does not do: %d", status)
	}
}

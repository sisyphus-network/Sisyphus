// Package inference offers a pool's language models as a service that
// speaks the dialect OpenAI defined, so that whatever can talk to such a
// service can think on the pool: the node's own planner, an agent, a chat
// program.
//
// It serves nothing itself. A request for a reply becomes a chat job, which
// the coordinator gives to a worker serving the model asked for, and the
// job's result is the reply.
package inference

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"

	pb "github.com/sisyphus-network/Sisyphus/packages/protocol/sisyphus/v1"
	"github.com/sisyphus-network/Sisyphus/packages/runtime"
)

// Pool is the pool whose workers serve the models. A coordinator is one.
type Pool interface {
	Submit(ctx context.Context, spec *pb.JobSpec) (*pb.Job, error)
	Watch(ctx context.Context, jobID string, fn func(*pb.Job) error) error
	WatchEvents(ctx context.Context, jobID string, after uint64, fn func(*pb.JobEvent) error) error
	Get(jobID string) (*pb.Job, error)
	Cancel(jobID string) (*pb.Job, error)
	Nodes() []*pb.NodeInfo
}

// maxRequest is the longest request taken.
const maxRequest = 8 << 20

// Handler returns the service. Every request must show token, as a bearer
// token, which is where an OpenAI-style client puts its key.
func Handler(pool Pool, token string) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/models", func(w http.ResponseWriter, r *http.Request) {
		type model struct {
			ID      string `json:"id"`
			Object  string `json:"object"`
			OwnedBy string `json:"owned_by"`
		}
		listed := []model{}
		for _, name := range served(pool, "") {
			listed = append(listed, model{ID: name, Object: "model", OwnedBy: "sisyphus"})
		}
		answer(w, http.StatusOK, map[string]any{"object": "list", "data": listed})
	})
	mux.HandleFunc("POST /v1/chat/completions", func(w http.ResponseWriter, r *http.Request) { chat(pool, w, r) })
	mux.HandleFunc("POST /v1/embeddings", func(w http.ResponseWriter, r *http.Request) { embeddings(pool, w, r) })
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		shown := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if subtle.ConstantTimeCompare([]byte(shown), []byte(token)) != 1 {
			refuse(w, http.StatusUnauthorized, "invalid_api_key", "the key is not this node's token, which is in api.token in its data directory")
			return
		}
		mux.ServeHTTP(w, r)
	})
}

// served lists the models the pool's connected workers serve: all of them,
// or those of the worker with the given name.
func served(pool Pool, worker string) []string {
	var names []string
	for _, node := range pool.Nodes() {
		if worker == "" || node.GetName() == worker {
			names = append(names, runtime.Models(node.GetCapabilities().GetLabels())...)
		}
	}
	slices.Sort(names)
	return slices.Compact(names)
}

// asked reads a request that names a model, and has it name the model as
// the pool does. It returns false, having answered, if the request cannot
// be read or the pool serves nothing it can mean.
func asked(pool Pool, w http.ResponseWriter, r *http.Request) (body []byte, model string, stream, ok bool) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxRequest))
	if err != nil {
		refuse(w, http.StatusRequestEntityTooLarge, "invalid_request_error", "the request could not be read, or is longer than this service takes")
		return nil, "", false, false
	}
	var request map[string]json.RawMessage
	if err := json.Unmarshal(body, &request); err != nil {
		refuse(w, http.StatusBadRequest, "invalid_request_error", "the request is not JSON")
		return nil, "", false, false
	}
	var named string
	json.Unmarshal(request["model"], &named)   // one that is no string names no model
	json.Unmarshal(request["stream"], &stream) // and one that is no yes or no is a no
	// Asked of a pool with no worker serving the model, the job would wait
	// for one to come. Whoever is asking is told now instead. A model named
	// loosely is taken to be the one the pool serves that it can only mean.
	_, worker, _ := strings.Cut(named, "@")
	models := served(pool, worker)
	model, found := runtime.ResolveModel(named, models)
	if !found {
		where := "no worker connected to the pool serves a model"
		if worker != "" {
			where = fmt.Sprintf("the worker %q is not connected to the pool, or serves no model", worker)
		}
		refuse(w, http.StatusNotFound, "model_not_found", fmt.Sprintf("%s that %q can only mean; those served are: %s", where, strings.TrimSuffix(named, "@"+worker), strings.Join(models, ", ")))
		return nil, "", false, false
	}
	request["model"], _ = json.Marshal(model) // a string always encodes
	body, _ = json.Marshal(request)           // decoded from JSON a moment ago
	return body, model, stream, true
}

// finish runs a job to its end and answers with its result, or with why
// there is none.
func finish(pool Pool, w http.ResponseWriter, r *http.Request, job *pb.Job) {
	if err := pool.Watch(r.Context(), job.GetJobId(), func(finished *pb.Job) error { job = finished; return nil }); err != nil {
		// Whoever asked has gone, or the node is stopping: nobody is
		// waiting for the reply, so the workers need not make it.
		pool.Cancel(job.GetJobId())
		refuse(w, http.StatusServiceUnavailable, "server_error", "the reply was not waited for: "+err.Error())
		return
	}
	if job.GetState() != pb.JobState_JOB_STATE_SUCCEEDED {
		refuse(w, http.StatusBadGateway, "server_error", fmt.Sprintf("job %s did not produce a reply: %s", job.GetJobId(), job.GetError()))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Write(job.GetResult())
}

// embeddings turns texts into vectors, as a job shared out among the
// workers that serve the model.
func embeddings(pool Pool, w http.ResponseWriter, r *http.Request) {
	body, _, _, ok := asked(pool, w, r)
	if !ok {
		return
	}
	job, err := pool.Submit(r.Context(), &pb.JobSpec{Workload: "embed", Params: body})
	if err != nil {
		refuse(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	w.Header().Set("X-Sisyphus-Job", job.GetJobId())
	finish(pool, w, r, job)
}

func chat(pool Pool, w http.ResponseWriter, r *http.Request) {
	body, model, stream, ok := asked(pool, w, r)
	if !ok {
		return
	}
	job, err := pool.Submit(r.Context(), &pb.JobSpec{Workload: "chat", Params: body, Mode: pb.ScheduleMode_SCHEDULE_MODE_FULL_WORKER})
	if err != nil {
		refuse(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	w.Header().Set("X-Sisyphus-Job", job.GetJobId())
	if stream {
		streamed(pool, w, r, job.GetJobId(), model)
		return
	}
	finish(pool, w, r, job)
}

// streamed sends a reply as it is written. The worker logs what the model
// has said in pieces; each is sent on as it arrives, and when the job is
// over what remains of the reply is sent: the tools it asks for, why it
// ended and what it used.
func streamed(pool Pool, w http.ResponseWriter, r *http.Request, jobID, model string) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	flush := func() { http.NewResponseController(w).Flush() } // a writer that cannot is one that does not hold things back
	send := func(piece map[string]any) {
		piece["id"], piece["object"], piece["model"] = "chatcmpl-"+jobID, "chat.completion.chunk", model
		encoded, _ := json.Marshal(piece) // maps of strings always encode
		fmt.Fprintf(w, "data: %s\n\n", encoded)
		flush()
	}
	delta := func(d map[string]any, finish any) map[string]any {
		return map[string]any{"choices": []any{map[string]any{"index": 0, "delta": d, "finish_reason": finish}}}
	}
	send(delta(map[string]any{"role": "assistant", "content": ""}, nil))
	sent := false
	err := pool.WatchEvents(r.Context(), jobID, 0, func(e *pb.JobEvent) error {
		var text string
		if e.GetKind() == "log" && json.Unmarshal([]byte(e.GetText()), &text) == nil && text != "" {
			send(delta(map[string]any{"content": text}, nil))
			sent = true
		}
		return nil
	})
	if err != nil {
		pool.Cancel(jobID)
		return
	}
	job, err := pool.Get(jobID)
	if err != nil || job.GetState() != pb.JobState_JOB_STATE_SUCCEEDED {
		// The reply has begun, so its failure is said in the stream.
		failure := fmt.Sprintf("job %s did not produce a reply: %s", jobID, job.GetError())
		fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", mustJSON(map[string]any{"error": map[string]string{"message": failure, "type": "server_error", "code": "server_error"}}))
		flush()
		return
	}
	var whole struct {
		Choices []struct {
			Message      map[string]any `json:"message"`
			FinishReason any            `json:"finish_reason"`
		} `json:"choices"`
		Usage any `json:"usage"`
	}
	json.Unmarshal(job.GetResult(), &whole) // the workload returns nothing that is not JSON
	last, finish := map[string]any{}, any("stop")
	if len(whole.Choices) > 0 {
		finish = whole.Choices[0].FinishReason
		for key, value := range whole.Choices[0].Message {
			// What the model said has gone already, unless the worker
			// sent no pieces; the rest goes now.
			if key != "role" && (key != "content" || !sent) {
				last[key] = value
			}
		}
		// In a stream, each tool call says which it is.
		calls, _ := last["tool_calls"].([]any)
		for i, call := range calls {
			if call, is := call.(map[string]any); is {
				call["index"] = i
			}
		}
	}
	end := delta(last, finish)
	if whole.Usage != nil {
		end["usage"] = whole.Usage
	}
	send(end)
	fmt.Fprint(w, "data: [DONE]\n\n")
	flush()
}

// mustJSON encodes what always encodes.
func mustJSON(v any) []byte {
	encoded, _ := json.Marshal(v)
	return encoded
}

// answer writes v as JSON.
func answer(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v) // maps and structs of strings always encode
}

// refuse answers with an error in the form an OpenAI-style client expects.
func refuse(w http.ResponseWriter, status int, kind, message string) {
	answer(w, status, map[string]any{"error": map[string]string{"message": message, "type": kind, "code": kind}})
}

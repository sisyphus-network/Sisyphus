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
		for _, name := range served(pool) {
			listed = append(listed, model{ID: name, Object: "model", OwnedBy: "sisyphus"})
		}
		answer(w, http.StatusOK, map[string]any{"object": "list", "data": listed})
	})
	mux.HandleFunc("POST /v1/chat/completions", func(w http.ResponseWriter, r *http.Request) { chat(pool, w, r) })
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		shown := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if subtle.ConstantTimeCompare([]byte(shown), []byte(token)) != 1 {
			refuse(w, http.StatusUnauthorized, "invalid_api_key", "the key is not this node's token, which is in api.token in its data directory")
			return
		}
		mux.ServeHTTP(w, r)
	})
}

// served lists the models the pool's connected workers serve.
func served(pool Pool) []string {
	var names []string
	for _, node := range pool.Nodes() {
		names = append(names, runtime.Models(node.GetCapabilities().GetLabels())...)
	}
	slices.Sort(names)
	return slices.Compact(names)
}

func chat(pool Pool, w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxRequest))
	if err != nil {
		refuse(w, http.StatusRequestEntityTooLarge, "invalid_request_error", "the request could not be read, or is longer than this service takes")
		return
	}
	var request struct {
		Model  string `json:"model"`
		Stream bool   `json:"stream"`
	}
	if err := json.Unmarshal(body, &request); err != nil {
		refuse(w, http.StatusBadRequest, "invalid_request_error", "the request is not JSON")
		return
	}
	// Asked of a pool with no worker serving the model, the job would wait
	// for one to come. Whoever is asking is told now instead.
	if models := served(pool); !slices.Contains(models, request.Model) {
		refuse(w, http.StatusNotFound, "model_not_found", fmt.Sprintf("no worker connected to the pool serves the model %q; those served are: %s", request.Model, strings.Join(models, ", ")))
		return
	}
	job, err := pool.Submit(r.Context(), &pb.JobSpec{Workload: "chat", Params: body, Mode: pb.ScheduleMode_SCHEDULE_MODE_FULL_WORKER})
	if err != nil {
		refuse(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	if err := pool.Watch(r.Context(), job.GetJobId(), func(finished *pb.Job) error { job = finished; return nil }); err != nil {
		// Whoever asked has gone, or the node is stopping: nobody is
		// waiting for the reply, so the worker need not make it.
		pool.Cancel(job.GetJobId())
		refuse(w, http.StatusServiceUnavailable, "server_error", "the reply was not waited for: "+err.Error())
		return
	}
	if job.GetState() != pb.JobState_JOB_STATE_SUCCEEDED {
		refuse(w, http.StatusBadGateway, "server_error", fmt.Sprintf("job %s did not produce a reply: %s", job.GetJobId(), job.GetError()))
		return
	}
	w.Header().Set("X-Sisyphus-Job", job.GetJobId())
	if !request.Stream {
		w.Header().Set("Content-Type", "application/json")
		w.Write(job.GetResult())
		return
	}
	// The reply is whole by now, and one that was asked for as a stream is
	// sent as a stream of one piece.
	w.Header().Set("Content-Type", "text/event-stream")
	fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", piece(job.GetResult()))
}

// piece turns a whole reply into the one piece of a streamed reply: what
// was each choice's message is its delta, and each tool call says which
// call it is.
func piece(whole []byte) []byte {
	var reply map[string]any
	json.Unmarshal(whole, &reply) // the workload returns nothing that is not JSON
	reply["object"] = "chat.completion.chunk"
	choices, _ := reply["choices"].([]any)
	for _, c := range choices {
		choice, _ := c.(map[string]any)
		message, _ := choice["message"].(map[string]any)
		calls, _ := message["tool_calls"].([]any)
		for i, call := range calls {
			if call, is := call.(map[string]any); is {
				call["index"] = i
			}
		}
		if choice != nil {
			choice["delta"] = message
			delete(choice, "message")
		}
	}
	encoded, _ := json.Marshal(reply) // decoded from JSON a moment ago
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

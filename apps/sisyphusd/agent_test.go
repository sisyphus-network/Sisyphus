package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// agent starts `sisyphusd mcp` against a node and returns an agent's end of
// the conversation with it.
func agent(t *testing.T, dataDir, apiAddr string) *mcp.ClientSession {
	t.Helper()
	ours, theirs := mcp.NewInMemoryTransports()
	agentTransport = func() mcp.Transport { return theirs }
	t.Cleanup(func() { agentTransport = func() mcp.Transport { return &mcp.StdioTransport{} } })
	ctx, hangUp := context.WithCancel(context.Background())
	served := make(chan error, 1)
	go func() { served <- run(ctx, []string{"mcp", "--data-dir", dataDir, "--api", apiAddr}) }()
	session, err := mcp.NewClient(&mcp.Implementation{Name: "test-agent", Version: "1"}, nil).Connect(ctx, ours, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		session.Close()
		hangUp()
		<-served
	})
	return session
}

// use calls one of the server's tools and returns what it said, decoded,
// and whether it said it had failed.
func use(t *testing.T, session *mcp.ClientSession, tool string, args map[string]any) (said any, text string, failed bool) {
	t.Helper()
	if args == nil {
		args = map[string]any{}
	}
	result, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: tool, Arguments: args})
	if err != nil {
		t.Fatalf("%s: %v", tool, err)
	}
	text = result.Content[0].(*mcp.TextContent).Text
	json.Unmarshal([]byte(text), &said)
	return said, text, result.IsError
}

func TestAnAgentUsesThePoolThroughTheNode(t *testing.T) {
	dataDir := t.TempDir()
	addr, apiAddr := freeAddr(t), freeAddr(t)
	startDaemon(t, "--data-dir", dataDir, "--listen", addr, "--name", "rig", "--slots", "2", "--api-listen", apiAddr)
	poolAsSeenBy(t, desktop(t, apiAddr))
	waitForOutput(t, "rig", "nodes", "--addr", addr)
	session := agent(t, dataDir, apiAddr)

	// The agent is told what there is: the tools, and how to begin.
	tools, err := session.ListTools(context.Background(), nil)
	if err != nil || len(tools.Tools) != 10 {
		t.Fatalf("tools = %v, %v", tools, err)
	}
	if !strings.Contains(session.InitializeResult().Instructions, "pool_status") {
		t.Errorf("instructions: %q", session.InitializeResult().Instructions)
	}

	said, text, _ := use(t, session, "pool_status", nil)
	pool := said.(map[string]any)
	if workers := pool["workers"].([]any); len(workers) != 1 || workers[0].(map[string]any)["name"] != "rig" || pool["task_slots"] != float64(2) {
		t.Fatalf("pool_status: %s", text)
	}
	said, text, _ = use(t, session, "list_workloads", nil)
	described := map[string]string{}
	for _, w := range said.([]any) {
		w := w.(map[string]any)
		described[w["name"].(string)] = w["description"].(string)
		// The one worker runs no containers, and the agent is told so.
		want := float64(1)
		if w["name"] == "container" || w["name"] == "transcode" {
			want = 0
		}
		if w["workers_running_it"] != want {
			t.Errorf("%v is run by %v workers", w["name"], w["workers_running_it"])
		}
	}
	if !strings.Contains(described["wordcount"], "input") || described["primes"] == "" {
		t.Fatalf("list_workloads: %s", text)
	}

	// A file goes in, a job runs over it, and its result comes back.
	input := filepath.Join(t.TempDir(), "words.txt")
	os.WriteFile(input, []byte("the boulder the hill the boulder"), 0o600)
	said, text, failed := use(t, session, "store_file", map[string]any{"path": input})
	file := said.(map[string]any)
	if failed || file["name"] != "words.txt" || file["size_bytes"] != float64(32) {
		t.Fatalf("store_file: %s", text)
	}
	said, text, failed = use(t, session, "run_job", map[string]any{"workload": "wordcount", "params": map[string]any{"input": file["cid"]}, "tasks": 2})
	job := said.(map[string]any)
	if failed || job["state"] != "succeeded" || job["workload"] != "wordcount" || job["seconds"] == nil || !strings.Contains(text, "stored_outputs") {
		t.Fatalf("run_job: %s", text)
	}
	id := job["job_id"].(string)
	if _, again, _ := use(t, session, "get_job", map[string]any{"job_id": id}); !strings.Contains(again, `"state":"succeeded"`) || !strings.Contains(again, "stored_outputs") {
		t.Errorf("get_job: %s", again)
	}
	said, text, _ = use(t, session, "job_logs", map[string]any{"job_id": id})
	logs := said.(map[string]any)
	if logs["job_finished"] != true || !strings.Contains(text, `"kind":"submitted"`) || !strings.Contains(text, `"kind":"succeeded"`) || !strings.Contains(text, `"worker":"rig"`) {
		t.Errorf("job_logs: %s", text)
	}
	if said, text, _ = use(t, session, "job_logs", map[string]any{"job_id": id, "after_seq": logs["last_seq"]}); len(said.(map[string]any)["events"].([]any)) != 0 {
		t.Errorf("job_logs after the last event: %s", text)
	}

	// The result is a stored file, fetched as text or to a path.
	output := job["stored_outputs"].([]any)[0].(string)
	said, text, failed = use(t, session, "fetch_file", map[string]any{"cid": output})
	if failed || !strings.Contains(said.(map[string]any)["text"].(string), "boulder") {
		t.Fatalf("fetch_file: %s", text)
	}
	saved := filepath.Join(t.TempDir(), "counts.json")
	if _, text, failed = use(t, session, "fetch_file", map[string]any{"cid": output, "path": saved}); failed {
		t.Fatalf("fetch_file to a path: %s", text)
	}
	if written, _ := os.ReadFile(saved); !strings.Contains(string(written), "boulder") {
		t.Errorf("the file written: %q", written)
	}
	if _, text, _ = use(t, session, "list_files", nil); !strings.Contains(text, "words.txt") {
		t.Errorf("list_files: %s", text)
	}

	// A job set going and left is listed, and can be stopped.
	said, text, failed = use(t, session, "run_job", map[string]any{"workload": "primes", "params": map[string]any{"from": 0, "to": 4000000000}, "detach": true})
	left := said.(map[string]any)
	if failed || left["state"] == "succeeded" || left["note"] == nil {
		t.Fatalf("run_job, detached: %s", text)
	}
	said, text, _ = use(t, session, "list_jobs", nil)
	if jobs := said.([]any); len(jobs) != 2 || jobs[0].(map[string]any)["job_id"] != left["job_id"] || strings.Contains(text, "stored_outputs") {
		t.Errorf("list_jobs: %s", text)
	}
	// Its events go on, so the agent is given what there is so far.
	if said, text, failed = use(t, session, "job_logs", map[string]any{"job_id": left["job_id"]}); failed || said.(map[string]any)["job_finished"] != false || !strings.Contains(text, "submitted") {
		t.Errorf("job_logs of a running job: %s", text)
	}
	// A wait that runs out returns the job as it stands.
	if _, text, failed = use(t, session, "run_job", map[string]any{"workload": "primes", "params": map[string]any{"from": 0, "to": 4000000000}, "wait_seconds": 1}); failed || !strings.Contains(text, "has not finished") {
		t.Errorf("run_job with a short wait: %s", text)
	}
	if _, text, failed = use(t, session, "cancel_job", map[string]any{"job_id": left["job_id"]}); failed || !strings.Contains(text, `"state":"cancelled"`) {
		t.Errorf("cancel_job: %s", text)
	}

	// What the node refuses, the agent is told in the node's words.
	for tool, args := range map[string]map[string]any{
		"run_job":    {"workload": "no-such-workload"},
		"get_job":    {"job_id": "no-such-job"},
		"cancel_job": {"job_id": "no-such-job"},
		"job_logs":   {"job_id": "no-such-job"},
		"fetch_file": {"cid": "not-a-cid"},
		"store_file": {"path": filepath.Join(t.TempDir(), "absent")},
	} {
		if _, text, failed := use(t, session, tool, args); !failed || strings.Contains(text, "rpc error") {
			t.Errorf("%s of what is not there: failed %v, %s", tool, failed, text)
		}
	}
}

func TestOfferingANodeToAnAgentWhenItCannotBe(t *testing.T) {
	for _, tt := range []struct {
		args []string
		want string
	}{
		{[]string{"mcp", "--no-such-flag"}, "flag provided but not defined"},
		{[]string{"mcp", "extra"}, `unexpected argument "extra"`},
		{[]string{"mcp", "--data-dir", t.TempDir()}, "read the node's API token"},
	} {
		if _, err := cli(t, tt.args...); err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("%v: %v, want %q", tt.args, err, tt.want)
		}
	}
}

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
func agent(t *testing.T, dataDir, apiAddr string, more ...string) *mcp.ClientSession {
	t.Helper()
	ours, theirs := mcp.NewInMemoryTransports()
	stdio := agentTransport
	agentTransport = theirs
	t.Cleanup(func() { agentTransport = stdio })
	ctx, hangUp := context.WithCancel(context.Background())
	served := make(chan error, 1)
	go func() { served <- run(ctx, append([]string{"mcp", "--data-dir", dataDir, "--api", apiAddr}, more...)) }()
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
	session := agent(t, dataDir, apiAddr, "--files-under", "/")

	// The agent is told what there is: the tools, and how to begin. Those
	// that change the node itself are not among them unless asked for.
	tools, err := session.ListTools(context.Background(), nil)
	if err != nil || len(tools.Tools) != 19 {
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
		// The one worker runs no containers and serves no models, and
		// the agent is told so.
		want := float64(1)
		if w["name"] == "container" || w["name"] == "transcode" || w["name"] == "chat" {
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
		{[]string{"mcp", "--files-under", filepath.Join(t.TempDir(), "absent")}, "is not a directory"},
		{[]string{"mcp", "--files-under", os.Args[0]}, "is not a directory"},
	} {
		if _, err := cli(t, tt.args...); err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("%v: %v, want %q", tt.args, err, tt.want)
		}
	}
}

func TestAnAgentIsKeptToWhatItsOwnerAllows(t *testing.T) {
	dataDir := t.TempDir()
	addr, apiAddr := freeAddr(t), freeAddr(t)
	startDaemon(t, "--data-dir", dataDir, "--listen", addr, "--name", "rig", "--slots", "2", "--api-listen", apiAddr)
	poolAsSeenBy(t, desktop(t, apiAddr))
	waitForOutput(t, "rig", "nodes", "--addr", addr)
	names := func(session *mcp.ClientSession) string {
		tools, err := session.ListTools(context.Background(), nil)
		if err != nil {
			t.Fatal(err)
		}
		var listed []string
		for _, tool := range tools.Tools {
			listed = append(listed, tool.Name)
		}
		return strings.Join(listed, " ")
	}

	// Told to look and not touch, it is given nothing that runs or changes.
	looking := agent(t, dataDir, apiAddr, "--read-only", "--admin")
	if got := names(looking); strings.Contains(got, "run_job") || strings.Contains(got, "store_file") || strings.Contains(got, "create_invitation") || !strings.Contains(got, "pool_status") || !strings.Contains(got, "list_peers") {
		t.Errorf("a read-only agent has: %s", got)
	}
	looking.Close()

	// Kept to a directory and to one image, and let change the node.
	home := t.TempDir()
	inside, outside := filepath.Join(home, "in.txt"), filepath.Join(t.TempDir(), "out.txt")
	for _, file := range []string{inside, outside} {
		if err := os.WriteFile(file, []byte("some words"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	kept := agent(t, dataDir, apiAddr, "--files-under", home, "--images", "alpine:3.20, python:3-slim", "--admin")
	if got := names(kept); !strings.Contains(got, "create_invitation") || !strings.Contains(got, "set_peer_trust") || !strings.Contains(got, "run_job") {
		t.Errorf("an agent let change the node has: %s", got)
	}
	stored, text, failed := use(t, kept, "store_file", map[string]any{"path": inside})
	if failed {
		t.Fatalf("storing a file inside its directory: %s", text)
	}
	if _, text, failed := use(t, kept, "store_file", map[string]any{"path": outside}); !failed || !strings.Contains(text, "is outside") {
		t.Errorf("storing a file outside its directory: %s", text)
	}
	cid := stored.(map[string]any)["cid"]
	if _, text, failed := use(t, kept, "fetch_file", map[string]any{"cid": cid, "path": outside}); !failed || !strings.Contains(text, "is outside") {
		t.Errorf("fetching to outside its directory: %s", text)
	}
	// A file already there is not written over unasked.
	if _, text, failed := use(t, kept, "fetch_file", map[string]any{"cid": cid, "path": inside}); !failed || !strings.Contains(text, "already") {
		t.Errorf("fetching over a file: %s", text)
	}
	if _, text, failed := use(t, kept, "fetch_file", map[string]any{"cid": cid, "path": inside, "overwrite": true}); failed {
		t.Errorf("fetching over a file when told to: %s", text)
	}
	if _, text, failed := use(t, kept, "run_job", map[string]any{"workload": "container", "params": map[string]any{"image": "busybox"}}); !failed || !strings.Contains(text, "alpine:3.20, python:3-slim") {
		t.Errorf("running an image not listed: %s", text)
	}

	// It can do the owner's errands on the node: invite, see who is in.
	invited, text, failed := use(t, kept, "create_invitation", map[string]any{"role": "client", "ttl_hours": 1})
	if failed || invited.(map[string]any)["role"] != "client" || invited.(map[string]any)["invitation"] == "" {
		t.Errorf("create_invitation: %s", text)
	}
	if _, text, failed := use(t, kept, "list_members", nil); failed {
		t.Errorf("list_members: %s", text)
	}
	if _, text, failed := use(t, kept, "list_peers", nil); failed {
		t.Errorf("list_peers: %s", text)
	}
	if _, text, failed := use(t, kept, "remove_file", map[string]any{"cid": cid}); failed {
		t.Errorf("remove_file: %s", text)
	}
	// With no model set, the planner says what to do about it.
	if _, text, failed := use(t, kept, "ask_planner", map[string]any{"question": "How many primes below 100?"}); !failed || !strings.Contains(text, "has not been told which language model") {
		t.Errorf("ask_planner with no model: %s", text)
	}
}

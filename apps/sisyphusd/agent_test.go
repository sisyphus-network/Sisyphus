package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

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
	type connected struct {
		session *mcp.ClientSession
		err     error
	}
	connecting := make(chan connected, 1)
	go func() {
		session, err := mcp.NewClient(&mcp.Implementation{Name: "test-agent", Version: "1"}, nil).Connect(ctx, ours, nil)
		connecting <- connected{session, err}
	}()
	var session *mcp.ClientSession
	select {
	case got := <-connecting:
		if got.err != nil {
			t.Fatal(got.err)
		}
		session = got.session
	case err := <-served:
		// Nobody is left to answer, and the agent's end would wait for
		// good.
		hangUp()
		t.Fatalf("the server stopped before an agent had connected: %v", err)
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
	if err != nil || len(tools.Tools) != 32 {
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
		if slices.Contains([]any{"container", "transcode", "chat", "prompts", "embed"}, w["name"]) {
			want = 0
		}
		if w["name"] == "graph" {
			// A graph's steps are jobs, which no worker need know of.
			if w["workers_running_it"] != nil || !strings.Contains(w["run_by"].(string), "coordinator") {
				t.Errorf("a graph is said to be run by %v", w)
			}
			continue
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

// What the node keeps, its name and its jobs' records are at its main
// address and not in its local API, and the agent's server reaches them
// there as the command line does: with the node's key.
func TestAnAgentKeepsWhatAJobMadeAndChecksItsRecord(t *testing.T) {
	replicateQuickly(t, time.Minute)
	dataDir, copies := t.TempDir(), filepath.Join(t.TempDir(), "copies")
	addr, apiAddr := freeAddr(t), freeAddr(t)
	startDaemon(t, "--data-dir", dataDir, "--listen", addr, "--name", "rig", "--slots", "2", "--api-listen", apiAddr, "--replicas", "1")
	startDaemon(t, "--role", "worker", "--coordinator", addr, "--replica-dir", copies, "--name", "follower", "--slots", "1")
	waitForOutput(t, "follower", "nodes", "--addr", addr)
	nodeID := strings.TrimSpace(mustCLI(t, "id", "--data-dir", dataDir))
	session := agent(t, dataDir, apiAddr, "--addr", addr, "--files-under", "/", "--admin")

	// A job runs, whole on one worker, and stores its result.
	said, text, failed := use(t, session, "store_file", map[string]any{"path": writeFile(t, "the boulder the hill the boulder")})
	if failed {
		t.Fatalf("store_file: %s", text)
	}
	said, text, failed = use(t, session, "run_job", map[string]any{"workload": "wordcount", "params": map[string]any{"input": said.(map[string]any)["cid"]}, "mode": "full-worker"})
	job := said.(map[string]any)
	if failed || job["state"] != "succeeded" || job["tasks"] != float64(1) {
		t.Fatalf("run_job on one worker: %s", text)
	}
	id, output := job["job_id"].(string), job["stored_outputs"].([]any)[0].(string)

	// The job holds its output for a while. Pinned, the user holds it too,
	// for the day asked for.
	if _, text, _ = use(t, session, "list_pins", map[string]any{"cid": output}); !strings.Contains(text, `"held_for":"job:`+id+`"`) || strings.Contains(text, `"held_for":"user"`) {
		t.Errorf("list_pins before pinning: %s", text)
	}
	if _, text, failed = use(t, session, "pin_file", map[string]any{"cid": output, "ttl_hours": 24}); failed || !strings.Contains(text, `"pinned":"`+output+`"`) || !strings.Contains(text, `"held_for":"job:`+id+`"`) || !strings.Contains(text, `"held_for":"user"`) {
		t.Fatalf("pin_file: %s", text)
	}
	said, text, _ = use(t, session, "list_pins", map[string]any{"cid": output})
	var until time.Time
	for _, pin := range said.(map[string]any)["pins"].([]any) {
		if pin := pin.(map[string]any); pin["held_for"] == "user" {
			until, _ = time.Parse(time.RFC3339, pin["until"].(string))
		}
	}
	if left := time.Until(until); left < 23*time.Hour || left > 24*time.Hour {
		t.Errorf("a file pinned for a day is kept until %s: %s", until, text)
	}
	if _, text, failed = use(t, session, "list_pins", nil); failed || !strings.Contains(text, output) || !strings.Contains(text, `"held_for":"recent"`) {
		t.Errorf("list_pins: %s", text)
	}

	// The follower comes to hold a copy, and the agent is told it does.
	waitFor(t, func() bool {
		said, text, failed = use(t, session, "storage_status", map[string]any{"cid": output})
		return !failed && strings.Contains(text, `"copies":1`)
	})
	status := said.(map[string]any)
	held := status["followers"].(map[string]any)
	if status["kept_by"] != "followers" || held["copies_wanted"] != float64(1) || held["followers_connected"] != float64(1) || held["files_with_too_few_copies"] != float64(0) || held["too_few_followers_by"] != nil {
		t.Errorf("storage_status: %s", text)
	}

	// The job's record is the one the command line prints, and checks out.
	record, _, _ := strings.Cut(mustCLI(t, "job", "record", "--addr", addr, id), "\n")
	said, text, failed = use(t, session, "get_job_record", map[string]any{"job_id": id, "verify": true})
	if failed || said.(map[string]any)["record_cid"] != record || said.(map[string]any)["verified"] != true || !strings.Contains(text, `"workload":"wordcount"`) || !strings.Contains(text, output) || strings.Contains(text, "commitments") {
		t.Errorf("get_job_record, verified against %s: %s", record, text)
	}
	// The record is kept too, with a pin of its own, once it is written.
	waitFor(t, func() bool {
		_, text, _ = use(t, session, "list_pins", map[string]any{"cid": record})
		return strings.Contains(text, `"held_for":"record:`+id+`"`)
	})
	// The node has made no job private yet, so it has no key to check with.
	if _, text, failed = use(t, session, "get_job_record", map[string]any{"job_id": id, "check_commitments": true}); !failed || !strings.Contains(text, "there is no private.key") {
		t.Errorf("get_job_record with a key the node does not have: %s", text)
	}

	// The node's name is pointed at the result, and resolves to it.
	said, text, failed = use(t, session, "publish_name", map[string]any{"cid": output})
	if failed || said.(map[string]any)["name"] != nodeID || said.(map[string]any)["sequence"] != float64(0) {
		t.Fatalf("publish_name: %s", text)
	}
	if said, text, failed = use(t, session, "resolve_name", map[string]any{"name": nodeID}); failed || said.(map[string]any)["cid"] != output {
		t.Errorf("resolve_name: %s", text)
	}
	if got := strings.TrimSpace(mustCLI(t, "name", "resolve", "--addr", addr, nodeID)); got != output {
		t.Errorf("the command line resolves the name to %q", got)
	}
	// Pointed elsewhere, it stands for that instead.
	said, _, _ = use(t, session, "list_files", nil)
	input := said.([]any)[0].(map[string]any)["cid"]
	if said, text, failed = use(t, session, "publish_name", map[string]any{"cid": input, "lifetime_hours": 1}); failed || said.(map[string]any)["sequence"] != float64(1) {
		t.Errorf("publish_name again: %s", text)
	}
	if said, text, failed = use(t, session, "resolve_name", nil); failed || said.(map[string]any)["cid"] != input {
		t.Errorf("resolve_name of this node's own name: %s", text)
	}

	// A private job's record holds commitments, which the node's own key,
	// made when the job was, checks.
	said, text, failed = use(t, session, "store_file", map[string]any{"path": writeFile(t, "what only this pool may read"), "private": true})
	if failed {
		t.Fatalf("store_file, private: %s", text)
	}
	said, text, failed = use(t, session, "run_job", map[string]any{"workload": "wordcount", "params": map[string]any{"input": said.(map[string]any)["cid"]}, "private": true})
	if failed || said.(map[string]any)["state"] != "succeeded" {
		t.Fatalf("run_job, private: %s", text)
	}
	sealed := said.(map[string]any)["job_id"]
	if _, text, failed = use(t, session, "get_job_record", map[string]any{"job_id": sealed, "verify": true}); failed || !strings.Contains(text, "not checked: this is a private job's record") {
		t.Errorf("get_job_record of a private job: %s", text)
	}
	if _, text, failed = use(t, session, "get_job_record", map[string]any{"job_id": sealed, "check_commitments": true}); failed || !strings.Contains(text, "checked: this node's key and the values the node holds give all") || !strings.Contains(text, "verified") {
		t.Errorf("get_job_record of a private job, with its key: %s", text)
	}
	if _, text, failed = use(t, session, "get_job_record", map[string]any{"job_id": id, "check_commitments": true}); failed || !strings.Contains(text, "none to check") {
		t.Errorf("get_job_record of a job that is not private, with a key: %s", text)
	}

	// Released, the user's pin is gone and the job's remains.
	if _, text, failed = use(t, session, "unpin_file", map[string]any{"cid": output}); failed {
		t.Errorf("unpin_file: %s", text)
	}
	if _, text, _ = use(t, session, "list_pins", map[string]any{"cid": output}); strings.Contains(text, `"held_for":"user"`) || !strings.Contains(text, `"held_for":"job:`+id+`"`) {
		t.Errorf("list_pins after unpinning: %s", text)
	}
	// The owner's errands on the store: nothing has lapsed, nothing was lost.
	if said, text, failed = use(t, session, "collect_garbage", nil); failed || said.(map[string]any)["expired_pins_dropped"] != float64(0) {
		t.Errorf("collect_garbage: %s", text)
	}
	if said, text, failed = use(t, session, "restore_files", nil); failed || said.(map[string]any)["restored"] != float64(0) || !strings.Contains(text, "followers hold nothing") {
		t.Errorf("restore_files: %s", text)
	}

	// What the node refuses, the agent is told in the node's words.
	for tool, args := range map[string]map[string]any{
		"pin_file":       {"cid": "not-a-cid"},
		"unpin_file":     {"cid": "not-a-cid"},
		"get_job_record": {"job_id": "no-such-job"},
		"resolve_name":   {"name": "12D3KooWGzBpMNLkZC7dKsbCJPqbd6r6vr6pJfTNgqPHZTqnuTHs"},
		"storage_status": {"cid": "not-a-cid"},
	} {
		if _, text, failed := use(t, session, tool, args); !failed || strings.Contains(text, "rpc error") {
			t.Errorf("%s of what is not there: failed %v, %s", tool, failed, text)
		}
	}

	// An agent told to look may look at all of this and change none of it.
	looking := agent(t, dataDir, apiAddr, "--addr", addr, "--read-only")
	if _, text, failed = use(t, looking, "storage_status", nil); failed || !strings.Contains(text, `"kept_by":"followers"`) {
		t.Errorf("storage_status, read-only: %s", text)
	}
	tools, err := looking.ListTools(context.Background(), nil)
	if err != nil || len(tools.Tools) != 18 {
		t.Fatalf("read-only tools = %v, %v", tools, err)
	}
	for _, tool := range tools.Tools {
		if slices.Contains([]string{"pin_file", "unpin_file", "publish_name", "collect_garbage", "restore_files"}, tool.Name) {
			t.Errorf("a read-only agent has %s", tool.Name)
		}
	}
}

// A pool that keeps no copies says so, and a server whose node's main
// address is not where it was told still serves everything else.
func TestAnAgentIsToldOfANodeThatKeepsNoCopiesAndOfOneItCannotReach(t *testing.T) {
	dataDir := t.TempDir()
	addr, apiAddr := freeAddr(t), freeAddr(t)
	startDaemon(t, "--data-dir", dataDir, "--listen", addr, "--name", "rig", "--slots", "1", "--api-listen", apiAddr)
	waitForOutput(t, "rig", "nodes", "--addr", addr)

	session := agent(t, dataDir, apiAddr, "--addr", addr)
	if said, text, failed := use(t, session, "storage_status", nil); failed || said.(map[string]any)["kept_by"] != "this node alone" || said.(map[string]any)["note"] == nil {
		t.Errorf("storage_status of a node alone: %s", text)
	}
	if _, text, failed := use(t, session, "resolve_name", nil); !failed || strings.Contains(text, "rpc error") {
		t.Errorf("resolve_name before anything is published: %s", text)
	}
	session.Close()

	// Nothing listens where this one is told the node is.
	elsewhere := agent(t, dataDir, apiAddr, "--addr", freeAddr(t))
	if _, text, failed := use(t, elsewhere, "list_pins", nil); !failed || !strings.Contains(text, "connection") {
		t.Errorf("list_pins with nothing at the main address: %s", text)
	}
	if _, text, failed := use(t, elsewhere, "pool_status", nil); failed || !strings.Contains(text, `"name":"rig"`) {
		t.Errorf("pool_status with nothing at the main address: %s", text)
	}
}

func TestTheMainAddressIsReachedWithTheNodesKeyOrSaysWhyNot(t *testing.T) {
	addr := "127.0.0.1:1"
	// A key that is no key.
	broken := t.TempDir()
	os.WriteFile(filepath.Join(broken, "node.key"), []byte("not a key"), 0o600)
	main := &mainAddress{node: &target{addr: &addr, dataDir: &broken}}
	if _, err := main.reach(); err == nil {
		t.Error("a node whose key cannot be read was reached")
	}
	main.close()

	// A list of nodes joined that cannot be read, and then one that can:
	// what failed once is tried again.
	dataDir := t.TempDir()
	known := filepath.Join(dataDir, "known.json")
	os.WriteFile(known, []byte("{"), 0o600)
	main = &mainAddress{node: &target{addr: &addr, dataDir: &dataDir}}
	if _, err := main.reach(); err == nil || !strings.Contains(err.Error(), "known nodes") {
		t.Errorf("with a list of known nodes that cannot be read: %v", err)
	}
	os.Remove(known)
	reached, err := main.reach()
	if err != nil {
		t.Fatal(err)
	}
	if again, err := main.reach(); err != nil || again != reached {
		t.Errorf("reached a second time: %v, %v", again, err)
	}
	main.close()

	// The node's sealing key is read if it is there, and never made.
	if _, err := reached.SealingKey(); err == nil || !strings.Contains(err.Error(), "there is no private.key in "+dataDir) {
		t.Errorf("the sealing key of a node with none: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dataDir, "private.key")); !os.IsNotExist(err) {
		t.Errorf("asking for a sealing key made one: %v", err)
	}
	os.WriteFile(filepath.Join(dataDir, "private.key"), []byte("not a key"), 0o600)
	if _, err := reached.SealingKey(); err == nil || strings.Contains(err.Error(), "there is no private.key") {
		t.Errorf("a sealing key that is no key: %v", err)
	}
	os.Remove(filepath.Join(dataDir, "private.key"))
	made, err := sealingKey(filepath.Join(dataDir, "private.key"))
	if err != nil {
		t.Fatal(err)
	}
	if key, err := reached.SealingKey(); err != nil || string(key) != string(made[:]) {
		t.Errorf("the sealing key the node made: %v", err)
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
	// The planner picks what to run itself, so a server held to listed
	// images does not ask it.
	if _, text, failed := use(t, kept, "ask_planner", map[string]any{"question": "How many primes below 100?"}); !failed || !strings.Contains(text, "chooses for itself what to run") {
		t.Errorf("ask_planner through a server held to listed images: %s", text)
	}
}

func TestTheSkillIsHandedOut(t *testing.T) {
	if out := mustCLI(t, "skill", "show"); !strings.HasPrefix(out, "---\nname: sisyphus\n") {
		t.Errorf("skill show printed:\n%.200s", out)
	}
	dir := t.TempDir()
	out := mustCLI(t, "skill", "install", "--dir", dir)
	into := filepath.Join(dir, "sisyphus")
	if !strings.Contains(out, into) {
		t.Errorf("skill install said %q", out)
	}
	for _, name := range []string{"SKILL.md", "recipes.md"} {
		if text, err := os.ReadFile(filepath.Join(into, name)); err != nil || len(text) < 500 {
			t.Errorf("%s as installed: %d bytes, %v", name, len(text), err)
		}
	}
	// Told nowhere, it goes where Claude Code looks, under the user's home.
	if out := mustCLI(t, "skill", "install"); !strings.Contains(out, filepath.Join(".claude", "skills", "sisyphus")) {
		t.Errorf("skill install with no directory said %q", out)
	}

	blocked := filepath.Join(t.TempDir(), "a-file")
	os.WriteFile(blocked, nil, 0o600)
	fixed := filepath.Join(t.TempDir(), "fixed")
	os.MkdirAll(filepath.Join(fixed, "sisyphus", "SKILL.md"), 0o700) // a directory where a file must go
	for _, tt := range []struct {
		args []string
		want string
	}{
		{[]string{"skill"}, "expected skill show or install"},
		{[]string{"skill", "remove"}, "expected skill show or install"},
		{[]string{"skill", "show", "--no-such-flag"}, "flag provided but not defined"},
		{[]string{"skill", "show", "extra"}, `unexpected argument "extra"`},
		{[]string{"skill", "install", "--dir", blocked}, "not a directory"},
		{[]string{"skill", "install", "--dir", fixed}, "is a directory"},
	} {
		if _, err := cli(t, tt.args...); err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("%v: %v, want %q", tt.args, err, tt.want)
		}
	}
}

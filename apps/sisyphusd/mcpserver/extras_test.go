package mcpserver

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"google.golang.org/grpc"

	nodepb "github.com/sisyphus-network/Sisyphus/packages/protocol/sisyphus/node/v1"
)

func (n *node) DeleteChat(context.Context, *nodepb.DeleteChatRequest, ...grpc.CallOption) (*nodepb.DeleteChatResponse, error) {
	return &nodepb.DeleteChatResponse{}, n.fail("DeleteChat")
}

func (n *node) ListProviders(context.Context, *nodepb.ListProvidersRequest, ...grpc.CallOption) (*nodepb.ListProvidersResponse, error) {
	return &nodepb.ListProvidersResponse{Providers: []*nodepb.Provider{{Id: "ollama", Name: "Ollama", FetchesModels: true}}}, n.fail("ListProviders")
}

func (n *node) RemoveModel(context.Context, *nodepb.RemoveModelRequest, ...grpc.CallOption) (*nodepb.RemoveModelResponse, error) {
	return &nodepb.RemoveModelResponse{}, n.fail("RemoveModel")
}

// agentOf connects an agent to a server over the node n, and returns its
// end and what it is told of progress.
func agentOf(t *testing.T, n *node, cfg Config) (*mcp.ClientSession, func() []string) {
	t.Helper()
	cfg.Node, cfg.Token = n, "the-token"
	if cfg.Workloads == nil {
		cfg.Workloads = serving(n).Workloads
	}
	ours, theirs := mcp.NewInMemoryTransports()
	ctx, hangUp := context.WithCancel(context.Background())
	go New(cfg).Run(ctx, theirs)
	var mu sync.Mutex
	var told []string
	client := mcp.NewClient(&mcp.Implementation{Name: "t", Version: "1"}, &mcp.ClientOptions{
		ProgressNotificationHandler: func(_ context.Context, req *mcp.ProgressNotificationClientRequest) {
			mu.Lock()
			defer mu.Unlock()
			told = append(told, req.Params.Message)
		},
	})
	session, err := client.Connect(ctx, ours, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { session.Close(); hangUp() })
	return session, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), told...)
	}
}

func TestAnAgentThatAsksIsToldHowFarAJobHasGot(t *testing.T) {
	n := &node{
		job: &nodepb.Job{JobId: "job-1", State: nodepb.JobState_JOB_STATE_SUCCEEDED, FinishedAtMs: 2},
		events: []*nodepb.JobEvent{
			{Kind: "submitted", Text: "primes, in 2 tasks"}, {Kind: "log", Text: "counting"},
			{Kind: "task-succeeded"}, {Kind: "task-succeeded"}, {Kind: "succeeded"},
		},
	}
	session, told := agentOf(t, n, Config{})
	ctx := context.Background()
	asking := &mcp.CallToolParams{Name: "wait_for_job", Arguments: map[string]any{"job_id": "job-1", "wait_seconds": 5}}
	asking.SetProgressToken("p1")
	result, err := session.CallTool(ctx, asking)
	if err != nil || result.IsError || !strings.Contains(result.Content[0].(*mcp.TextContent).Text, `"state":"succeeded"`) {
		t.Fatalf("wait_for_job = %v, %v", result, err)
	}
	// What the job logged is not progress; the rest is, in order. Word of
	// progress travels beside the answer and may land a moment after it.
	heard := func(n int) []string {
		for deadline := time.Now().Add(10 * time.Second); len(told()) < n && time.Now().Before(deadline); {
			time.Sleep(time.Millisecond)
		}
		return told()
	}
	if got := strings.Join(heard(4), "|"); got != "submitted primes, in 2 tasks|task-succeeded|task-succeeded|succeeded" {
		t.Errorf("told %q", got)
	}
	// One that does not ask is told nothing, and run_job tells as well.
	if _, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "wait_for_job", Arguments: map[string]any{"job_id": "job-1"}}); err != nil || len(heard(4)) != 4 {
		t.Errorf("told %v without asking, %v", told(), err)
	}
	running := &mcp.CallToolParams{Name: "run_job", Arguments: map[string]any{"workload": "primes"}}
	running.SetProgressToken("p2")
	if _, err := session.CallTool(ctx, running); err != nil || len(heard(8)) != 8 {
		t.Errorf("run_job told %v, %v", told(), err)
	}
}

func TestAnAgentReadsThePoolAndTheSkillAndIsPrompted(t *testing.T) {
	n := &node{workers: []*nodepb.Worker{{Name: "rig", TaskSlots: 4}}}
	session, _ := agentOf(t, n, Config{ReadOnly: true})
	ctx := context.Background()
	listed, err := session.ListResources(ctx, nil)
	if err != nil || len(listed.Resources) != 4 {
		t.Fatalf("resources = %v, %v", listed, err)
	}
	for uri, want := range map[string]string{
		"sisyphus://pool":             `"name":"rig"`,
		"sisyphus://workloads":        `"name":"primes"`,
		"sisyphus://skill/SKILL.md":   "name: sisyphus",
		"sisyphus://skill/recipes.md": "# Recipes",
	} {
		read, err := session.ReadResource(ctx, &mcp.ReadResourceParams{URI: uri})
		if err != nil || !strings.Contains(read.Contents[0].Text, want) {
			t.Errorf("%s: %v, %v", uri, read, err)
		}
	}
	// What is read of the node is read as its owner.
	if n.token != "Bearer the-token" {
		t.Errorf("the node was shown %q", n.token)
	}
	n.failing = "ListWorkers"
	if _, err := session.ReadResource(ctx, &mcp.ReadResourceParams{URI: "sisyphus://pool"}); err == nil || !strings.Contains(err.Error(), "the node is down") || strings.Contains(err.Error(), "rpc error") {
		t.Errorf("the pool with the node down: %v", err)
	}

	prompts, err := session.ListPrompts(ctx, nil)
	if err != nil || len(prompts.Prompts) != 2 {
		t.Fatalf("prompts = %v, %v", prompts, err)
	}
	run, err := session.GetPrompt(ctx, &mcp.GetPromptParams{Name: "run-on-pool", Arguments: map[string]string{"task": "count the words of war-and-peace.txt"}})
	if err != nil || !strings.Contains(run.Messages[0].Content.(*mcp.TextContent).Text, "count the words of war-and-peace.txt") {
		t.Errorf("run-on-pool = %v, %v", run, err)
	}
	report, err := session.GetPrompt(ctx, &mcp.GetPromptParams{Name: "pool-report"})
	if err != nil || !strings.Contains(report.Messages[0].Content.(*mcp.TextContent).Text, "pool_status") {
		t.Errorf("pool-report = %v, %v", report, err)
	}
}

func TestEveryFileAJobStoredIsFetchedAndNamedByWhereTheResultNamesIt(t *testing.T) {
	const a, b, c, d = "bafkreih5u7lamz7ygaeqakgswmw5r5d5qytmkx4sndacbh4uvd3p5j4lqu", "bafkreig65te5vcaby4yjbatp2xsk2ylupl3m5eozxezvxu7v3i2x3o4lgm",
		"bafkreib7h4ffy57tfupgv7kssv4nzvval5ogzblssvw3vyfkg3huj2qxh4", "QmYwAPJzv5CZsnA625s3Xf2nemtYgPpHdWEz79ojWnPbdG"
	n := &node{content: []string{"some"}, job: &nodepb.Job{JobId: "job-1", State: nodepb.JobState_JOB_STATE_SUCCEEDED, FinishedAtMs: 2,
		Result:      []byte(`{"tasks":[{"index":0,"stdout":"` + a + `","files":{"part":"` + b + `","sub/dir":"` + c + `"}},{"stdout":"` + a + `","note":"not an ID"}]}`),
		OutputBlobs: []string{a, d},
	}}
	s := serving(n)
	dir := filepath.Join(t.TempDir(), "made", "for", "it")
	got, err := s.fetchOutputs(context.Background(), outputsArgs{JobID: "job-1", Dir: dir})
	if err != nil || len(got.(map[string]any)["files"].([]any)) != 4 {
		t.Fatalf("fetch_outputs = %v, %v", got, err)
	}
	names, _ := os.ReadDir(dir)
	var listed []string
	for _, name := range names {
		listed = append(listed, name.Name())
	}
	if strings.Join(listed, " ") != d+" tasks-0-files-part tasks-0-files-sub_dir tasks-0-stdout" {
		t.Errorf("fetched as %v", listed)
	}
	// What cannot be fetched says which, and a job that is not there says so.
	if _, err := s.fetchOutputs(context.Background(), outputsArgs{JobID: "job-1", Dir: dir}); err == nil || !strings.Contains(err.Error(), "output 1 of 4") {
		t.Errorf("fetching over what is there: %v", err)
	}
	if _, err := serving(&node{failing: "GetJob"}).fetchOutputs(context.Background(), outputsArgs{JobID: "job-1", Dir: dir}); err == nil {
		t.Error("the outputs of a job that cannot be had were fetched")
	}
	// A directory that cannot be made, being under a file.
	if _, err := s.fetch(context.Background(), fetchArgs{CID: a, Path: filepath.Join(dir, "tasks-0-stdout", "under", "a-file")}); err == nil || !strings.Contains(err.Error(), "not a directory") {
		t.Errorf("fetching to under a file: %v", err)
	}
}

func TestTheLastOfTheToolsSayWhatTheNodeSaid(t *testing.T) {
	ctx := context.Background()
	for call, tool := range map[string]func(*server) (any, error){
		"DeleteChat":    func(s *server) (any, error) { return s.deleteChat(ctx, chatArgs{ChatID: "chat-1"}) },
		"ListProviders": func(s *server) (any, error) { return s.listProviders(ctx, none{}) },
		"RemoveModel":   func(s *server) (any, error) { return s.removeModel(ctx, pullArgs{Model: "gemma3:4b"}) },
	} {
		if _, err := tool(serving(&node{failing: call})); err == nil {
			t.Errorf("with %s failing: no error", call)
		}
		if said, err := tool(serving(&node{})); err != nil || said == nil {
			t.Errorf("%s: %v, %v", call, said, err)
		}
	}
	if waited(0) != usualWait || waited(7).Seconds() != 7 {
		t.Errorf("waits of %v and %v", waited(0), waited(7))
	}
}

package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/sisyphus-network/Sisyphus/apps/sisyphusd/planner"
	"github.com/sisyphus-network/Sisyphus/packages/ai"
	"github.com/sisyphus-network/Sisyphus/packages/nodedb"
	nodepb "github.com/sisyphus-network/Sisyphus/packages/protocol/sisyphus/node/v1"
	"github.com/sisyphus-network/Sisyphus/packages/runtime"
)

// model is a stand-in for a model server, speaking Ollama's dialect. Each
// time it is asked to continue a conversation it gives the next of its
// replies, and it remembers what it was asked.
type model struct {
	*httptest.Server
	mu      sync.Mutex
	replies []string
	asked   []map[string]any
	// keys are the keys it was shown when asked for its models, pulled the
	// models it was told to fetch, and removed those it was told to delete.
	keys, pulled, removed []string
	// fetched is a model it has that it did not have at first.
	fetched string
}

// says is a reply that is only text; wants is one that asks for a tool.
func says(text string) string {
	encoded, _ := json.Marshal(text)
	return `{"message":{"role":"assistant","content":` + string(encoded) + `},"done":true}`
}

func wants(tool, arguments string) string {
	return `{"message":{"role":"assistant","content":"","tool_calls":[{"function":{"name":"` + tool + `","arguments":` + arguments + `}}]},"done":true}`
}

func newModel(t *testing.T, replies ...string) *model {
	t.Helper()
	m := &model{replies: replies}
	m.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var asked map[string]any
		json.Unmarshal(body, &asked)
		m.mu.Lock()
		defer m.mu.Unlock()
		named, _ := asked["model"].(string)
		switch r.URL.Path {
		case "/api/tags":
			m.keys = append(m.keys, r.Header.Get("Authorization"))
			if m.fetched != "" {
				io.WriteString(w, `{"models":[{"name":"test-model","size":4900000000},{"name":"another-model"},{"name":"`+m.fetched+`"}]}`)
				return
			}
			io.WriteString(w, `{"models":[{"name":"test-model","size":4900000000},{"name":"another-model"}]}`)
			return
		case "/api/show":
			if named == "test-model" {
				io.WriteString(w, `{"capabilities":["completion","tools"]}`)
			} else {
				io.WriteString(w, `{"capabilities":["completion"]}`)
			}
			return
		case "/api/pull":
			if named == "no-such-model" {
				io.WriteString(w, `{"error":"file does not exist"}`)
				return
			}
			m.pulled = append(m.pulled, named)
			io.WriteString(w, `{"status":"pulling manifest"}`+"\n"+`{"status":"pulling layer","total":200,"completed":50}`+"\n"+
				`{"status":"pulling layer","total":200,"completed":51}`+"\n"+`{"status":"pulling layer","total":200,"completed":200}`+"\n"+`{"status":"success"}`+"\n")
			return
		case "/api/delete":
			m.removed = append(m.removed, named)
			return
		}
		m.asked = append(m.asked, asked)
		if len(m.asked) > len(m.replies) {
			http.Error(w, "the model has run out of things to say", http.StatusInternalServerError)
			return
		}
		io.WriteString(w, m.replies[len(m.asked)-1])
	}))
	t.Cleanup(m.Close)
	return m
}

// messagesAsked returns how many messages the model was sent the nth time
// it was asked, counting from one.
func (m *model) messagesAsked(n int) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.asked[n-1]["messages"].([]any))
}

func TestAQuestionIsPlannedComputedAndAnswered(t *testing.T) {
	served := newModel(t,
		wants("run_job", `{"workload":"primes","params":{"from":0,"to":100},"tasks":2}`),
		says("There are 25 primes below 100. I ran one job."),
		wants("run_job", `{"workload":"primes","params":{"from":0,"to":1000}}`),
		says("And 168 below 1000."),
	)
	dataDir := t.TempDir()
	addr, apiAddr := freeAddr(t), freeAddr(t)
	startDaemon(t, "--data-dir", dataDir, "--listen", addr, "--name", "rig", "--slots", "2", "--api-listen", apiAddr)
	client := desktop(t, apiAddr)
	poolAsSeenBy(t, client)
	waitForOutput(t, "rig", "nodes", "--addr", addr)

	// Told which model to use, while it runs.
	if out := mustCLI(t, "model", "show", "--data-dir", dataDir); !strings.Contains(out, "no model has been set") {
		t.Errorf("model show before any was set:\n%s", out)
	}
	if out := mustCLI(t, "model", "set", "--data-dir", dataDir, "--url", served.URL, "--model", "test-model"); !strings.Contains(out, "now plans with test-model, from ollama") {
		t.Errorf("model set printed %q", out)
	}
	if out := mustCLI(t, "model", "show", "--data-dir", dataDir); !strings.Contains(out, "provider: ollama") || !strings.Contains(out, "model:    test-model") || !strings.Contains(out, served.URL) || !strings.Contains(out, "key:      none") {
		t.Errorf("model show:\n%s", out)
	}
	if out := mustCLI(t, "model", "list", "--data-dir", dataDir); out != "test-model  4.9 GB\nanother-model  (cannot call tools, so cannot plan)\n" {
		t.Errorf("model list:\n%s", out)
	}

	// The whole loop: the model asks for a job, the pool runs it, the model
	// reads the result and answers.
	out := mustCLI(t, "ask", "--data-dir", dataDir, "--api", apiAddr, "How", "many", "primes", "below", "100?")
	for _, want := range []string{`[run_job {"workload":"primes","params":{"from":0,"to":100},"tasks":2}]`, "[job ", `"result":{"count":25}`, `"state":"succeeded"`, "There are 25 primes below 100. I ran one job.", "(chat "} {
		if !strings.Contains(out, want) {
			t.Errorf("ask printed, without %q:\n%s", want, out)
		}
	}
	chatID := strings.TrimSuffix(out[strings.LastIndex(out, "(chat ")+len("(chat "):], ")\n")
	// The job it ran is a job like any other.
	if jobs, err := client.ListJobs(tokenOf(t, dataDir), &nodepb.ListJobsRequest{}); err != nil || len(jobs.GetJobs()) != 1 || string(jobs.GetJobs()[0].GetResult()) != `{"count":25}` {
		t.Errorf("jobs after the question: %v, %v", jobs, err)
	}

	// The conversation carries on, with what went before in front of the model.
	out = mustCLI(t, "ask", "--data-dir", dataDir, "--api", apiAddr, "--chat", chatID, "And below 1000?")
	if !strings.Contains(out, `"result":{"count":168}`) || !strings.Contains(out, "And 168 below 1000.") || !strings.Contains(out, "(chat "+chatID+")") {
		t.Errorf("the second question:\n%s", out)
	}
	// The model is told of the workloads the pool's workers run, and of no
	// others: here nobody runs containers.
	served.mu.Lock()
	told := served.asked[0]["messages"].([]any)[0].(map[string]any)["content"].(string)
	served.mu.Unlock()
	if !strings.Contains(told, "- primes:") || !strings.Contains(told, "- wordcount:") || strings.Contains(told, "- container:") {
		t.Errorf("the model was told:\n%s", told)
	}
	// System, question, call, result, answer, second question.
	if n := served.messagesAsked(3); n != 6 {
		t.Errorf("for the second question the model was sent %d messages, want the 6 so far", n)
	}

	// The desktop reads it back.
	ctx := tokenOf(t, dataDir)
	chats, err := client.ListChats(ctx, &nodepb.ListChatsRequest{})
	if err != nil || len(chats.GetChats()) != 1 || chats.GetChats()[0].GetChatId() != chatID || chats.GetChats()[0].GetTitle() != "How many primes below 100?" || chats.GetChats()[0].GetCreatedAtMs() == 0 {
		t.Fatalf("ListChats = %v, %v", chats, err)
	}
	chat, err := client.GetChat(ctx, &nodepb.GetChatRequest{ChatId: chatID})
	if err != nil {
		t.Fatal(err)
	}
	var roles []string
	for _, m := range chat.GetMessages() {
		roles = append(roles, m.GetRole())
	}
	if strings.Join(roles, " ") != "user assistant tool assistant user assistant tool assistant" {
		t.Errorf("the conversation's roles: %v", roles)
	}
	call, result := chat.GetMessages()[1], chat.GetMessages()[2]
	if len(call.GetCalls()) != 1 || call.GetCalls()[0].GetName() != "run_job" || !strings.Contains(call.GetCalls()[0].GetArguments(), `"primes"`) ||
		result.GetTool() != "run_job" || !strings.Contains(result.GetContent(), `"count":25`) {
		t.Errorf("the model's request %v and what it was told %v", call, result)
	}
	if _, err := client.DeleteChat(ctx, &nodepb.DeleteChatRequest{ChatId: chatID}); err != nil {
		t.Fatal(err)
	}
	if left, _ := client.ListChats(ctx, &nodepb.ListChatsRequest{}); len(left.GetChats()) != 0 {
		t.Errorf("chats after deleting the one: %v", left)
	}
	if _, err := client.GetChat(ctx, &nodepb.GetChatRequest{ChatId: chatID}); status.Code(err) != codes.NotFound {
		t.Errorf("reading a deleted chat: %v", err)
	}
}

func TestTheDesktopSetsTheModelAndTheKeyStaysPut(t *testing.T) {
	served := newModel(t)
	dataDir := t.TempDir()
	apiAddr := freeAddr(t)
	startDaemon(t, "--data-dir", dataDir, "--listen", freeAddr(t), "--api-listen", apiAddr)
	client := desktop(t, apiAddr)
	poolAsSeenBy(t, client)
	ctx := tokenOf(t, dataDir)

	if cfg, err := client.GetModelConfig(context.Background(), &nodepb.GetModelConfigRequest{}); err != nil || cfg.GetModel() != "" || cfg.GetHasApiKey() {
		t.Errorf("before any model was set: %v, %v", cfg, err)
	}
	// Asked before it has a model, the planner says what to do about it.
	stream, err := client.Ask(ctx, &nodepb.AskRequest{Text: "Anything."})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := stream.Recv(); status.Code(err) != codes.FailedPrecondition || !strings.Contains(err.Error(), "sisyphusd model set") {
		t.Errorf("asking with no model set: %v", err)
	}
	if _, err := client.ListModels(ctx, &nodepb.ListModelsRequest{}); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("listing models with none set: %v", err)
	}

	set, err := client.SetModelConfig(ctx, &nodepb.SetModelConfigRequest{Provider: "ollama", BaseUrl: served.URL, Model: "test-model", ApiKey: "sk-secret"})
	if err != nil || !set.GetHasApiKey() || set.GetModel() != "test-model" {
		t.Fatalf("SetModelConfig = %v, %v", set, err)
	}
	// The key is never given back, and changing the rest need not send it again.
	kept, err := client.SetModelConfig(ctx, &nodepb.SetModelConfigRequest{Provider: "ollama", BaseUrl: served.URL, Model: "another-model", KeepApiKey: true})
	if err != nil || !kept.GetHasApiKey() || kept.GetModel() != "another-model" {
		t.Errorf("changing the model and keeping the key: %v, %v", kept, err)
	}
	// The key is kept for the service it was given for and no other: a
	// change of address or of provider is saved without it.
	elsewhere := newModel(t)
	for name, moved := range map[string]*nodepb.SetModelConfigRequest{
		"another address":  {Provider: "ollama", BaseUrl: elsewhere.URL, Model: "test-model", KeepApiKey: true},
		"another provider": {Provider: "openai", BaseUrl: served.URL, Model: "test-model", KeepApiKey: true},
	} {
		carried, err := client.SetModelConfig(ctx, moved)
		if err != nil || carried.GetHasApiKey() {
			t.Errorf("the key was carried to %s: %v, %v", name, carried, err)
		}
		if _, err := client.SetModelConfig(ctx, &nodepb.SetModelConfigRequest{Provider: "ollama", BaseUrl: served.URL, Model: "another-model", ApiKey: "sk-secret"}); err != nil {
			t.Fatal(err)
		}
	}
	// The same address written with a stroke after it is the same service.
	if same, err := client.SetModelConfig(ctx, &nodepb.SetModelConfigRequest{Provider: "ollama", BaseUrl: served.URL + "/", Model: "another-model", KeepApiKey: true}); err != nil || !same.GetHasApiKey() {
		t.Errorf("the same service, written otherwise: %v, %v", same, err)
	}
	if out := mustCLI(t, "model", "show", "--data-dir", dataDir); !strings.Contains(out, "key:      set") || strings.Contains(out, "sk-secret") {
		t.Errorf("model show with a key set:\n%s", out)
	}
	dropped, err := client.SetModelConfig(ctx, &nodepb.SetModelConfigRequest{Provider: "ollama", BaseUrl: served.URL, Model: "test-model"})
	if err != nil || dropped.GetHasApiKey() {
		t.Errorf("setting a configuration with no key: %v, %v", dropped, err)
	}
	// The configured service's models may be listed by anyone on the machine.
	models, err := client.ListModels(context.Background(), &nodepb.ListModelsRequest{})
	if err != nil || strings.Join(models.GetModels(), ",") != "test-model,another-model" || len(models.GetDetails()) != 2 {
		t.Fatalf("ListModels = %v, %v", models, err)
	}
	if able, unable := models.GetDetails()[0], models.GetDetails()[1]; able.GetTools() != nodepb.Support_SUPPORT_YES || able.GetSizeBytes() != 4900000000 ||
		unable.GetName() != "another-model" || unable.GetTools() != nodepb.Support_SUPPORT_NO {
		t.Errorf("what is known of the models: %v", models.GetDetails())
	}

	for name, bad := range map[string]*nodepb.SetModelConfigRequest{
		"a provider there is none of": {Provider: "carrier-pigeon", Model: "m"},
		"no model named":              {Provider: "ollama"},
	} {
		if _, err := client.SetModelConfig(ctx, bad); status.Code(err) != codes.InvalidArgument {
			t.Errorf("%s: %v, want InvalidArgument", name, err)
		}
	}
	// What changes the node, spends on its behalf, or is its owner's
	// business needs the token.
	open := context.Background()
	_, set1 := client.SetModelConfig(open, &nodepb.SetModelConfigRequest{Provider: "ollama", Model: "m"})
	_, list := client.ListChats(open, &nodepb.ListChatsRequest{})
	_, get := client.GetChat(open, &nodepb.GetChatRequest{ChatId: "any"})
	_, del := client.DeleteChat(open, &nodepb.DeleteChatRequest{ChatId: "any"})
	asking, _ := client.Ask(open, &nodepb.AskRequest{Text: "Anything."})
	_, asked := asking.Recv()
	for call, err := range map[string]error{"SetModelConfig": set1, "ListChats": list, "GetChat": get, "DeleteChat": del, "Ask": asked} {
		if status.Code(err) != codes.PermissionDenied {
			t.Errorf("%s without the token: %v", call, err)
		}
	}
	// A question that is none, and a conversation that is not there.
	for name, tt := range map[string]struct {
		req  *nodepb.AskRequest
		want codes.Code
	}{
		"no question":     {&nodepb.AskRequest{}, codes.InvalidArgument},
		"an unknown chat": {&nodepb.AskRequest{ChatId: "no-such-chat", Text: "Hello?"}, codes.NotFound},
	} {
		stream, _ := client.Ask(ctx, tt.req)
		if _, err := stream.Recv(); status.Code(err) != tt.want {
			t.Errorf("%s: %v, want %v", name, err, tt.want)
		}
	}
}

func TestAModelThatFailsPartWayLeavesTheConversationAsItHappened(t *testing.T) {
	// It asks for a job and then has nothing more to say.
	served := newModel(t, wants("run_job", `{"workload":"primes","params":{"from":0,"to":100}}`))
	dataDir := t.TempDir()
	addr, apiAddr := freeAddr(t), freeAddr(t)
	startDaemon(t, "--data-dir", dataDir, "--listen", addr, "--slots", "1", "--api-listen", apiAddr)
	client := desktop(t, apiAddr)
	poolAsSeenBy(t, client)
	waitForOutput(t, "primes", "nodes", "--addr", addr)
	mustCLI(t, "model", "set", "--data-dir", dataDir, "--url", served.URL, "--model", "test-model")

	_, err := cli(t, "ask", "--data-dir", dataDir, "--api", apiAddr, "Count the primes.")
	if err == nil || !strings.Contains(err.Error(), "run out of things to say") {
		t.Fatalf("error %v, want the model's failure", err)
	}
	ctx := tokenOf(t, dataDir)
	chats, _ := client.ListChats(ctx, &nodepb.ListChatsRequest{})
	if len(chats.GetChats()) != 1 {
		t.Fatalf("chats on record: %v", chats)
	}
	chat, err := client.GetChat(ctx, &nodepb.GetChatRequest{ChatId: chats.GetChats()[0].GetChatId()})
	if err != nil || len(chat.GetMessages()) != 3 || chat.GetMessages()[2].GetRole() != "tool" {
		t.Errorf("the conversation as recorded: %v, %v; want the question, the request and the result", chat, err)
	}
}

func TestANodeWithNoPoolCanBeToldItsModelButHasNothingToPlanWith(t *testing.T) {
	served := newModel(t)
	addr, apiAddr := freeAddr(t), freeAddr(t)
	workerDir := t.TempDir()
	startDaemon(t, "--role", "coordinator", "--listen", addr)
	startDaemon(t, "--data-dir", workerDir, "--role", "worker", "--coordinator", addr, "--name", "hand", "--slots", "1", "--api-listen", apiAddr)
	client := desktop(t, apiAddr)
	poolAsSeenBy(t, client)
	ctx := tokenOf(t, workerDir)
	if _, err := client.SetModelConfig(ctx, &nodepb.SetModelConfigRequest{Provider: "ollama", BaseUrl: served.URL, Model: "test-model"}); err != nil {
		t.Fatal(err)
	}
	stream, _ := client.Ask(ctx, &nodepb.AskRequest{Text: "Anything."})
	if _, err := stream.Recv(); status.Code(err) != codes.FailedPrecondition || !strings.Contains(err.Error(), "coordinates no pool") {
		t.Errorf("asking a node with no pool: %v", err)
	}
}

func TestModelAndAskCommandsThatAreRefused(t *testing.T) {
	dataDir := t.TempDir()
	blocked := filepath.Join(t.TempDir(), "in-the-way")
	if err := os.WriteFile(blocked, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct {
		args []string
		want string
	}{
		{[]string{"model"}, "expected model set, show, list, providers, pull or remove"},
		{[]string{"model", "forget", "--data-dir", dataDir}, "expected model set, show, list, providers, pull or remove"},
		{[]string{"model", "pull", "--data-dir", dataDir}, "a model must be named"},
		{[]string{"model", "remove", "--data-dir", dataDir}, "a model must be named"},
		{[]string{"model", "pull", "--data-dir", dataDir, "--provider", "openai", "--model", "m"}, "are not fetched or removed"},
		{[]string{"model", "remove", "--data-dir", dataDir, "--provider", "pigeon", "--model", "m"}, "unknown model provider"},
		{[]string{"model", "show", "--no-such-flag"}, "flag provided but not defined"},
		{[]string{"model", "show", "--data-dir", dataDir, "extra"}, `unexpected argument "extra"`},
		{[]string{"model", "show", "--data-dir", filepath.Join(blocked, "sub")}, "not a directory"},
		{[]string{"model", "set", "--data-dir", dataDir}, "a model must be named"},
		{[]string{"model", "set", "--data-dir", dataDir, "--provider", "pigeon", "--model", "m"}, "unknown model provider"},
		{[]string{"model", "set", "--data-dir", dataDir, "--model", "m", "--api-key-file", filepath.Join(dataDir, "no-such-file")}, "read the service's key"},
		{[]string{"model", "list", "--data-dir", dataDir}, "has not been told which language model"},
		{[]string{"ask"}, "expected a question"},
		{[]string{"ask", "--no-such-flag"}, "flag provided but not defined"},
		{[]string{"ask", "--data-dir", dataDir, "Hello?"}, "read the node's API token"},
	} {
		if _, err := cli(t, tt.args...); err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("%v: error %v, want one containing %q", tt.args, err, tt.want)
		}
	}
	// A database that cannot be opened, and a key read from a file.
	spoiled := t.TempDir()
	if err := os.Mkdir(filepath.Join(spoiled, "node.db"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := cli(t, "model", "show", "--data-dir", spoiled); err == nil || !strings.Contains(err.Error(), "open node database") {
		t.Errorf("with a database that cannot be opened: %v", err)
	}
	// And one whose configuration cannot be read.
	damaged := t.TempDir()
	journalIn(t, filepath.Join(damaged, "node.db")).Close()
	raw, err := sql.Open("sqlite", filepath.Join(damaged, "node.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`DROP TABLE model_config`); err != nil {
		t.Fatal(err)
	}
	raw.Close()
	if _, err := cli(t, "model", "show", "--data-dir", damaged); err == nil || !strings.Contains(err.Error(), "load model configuration") {
		t.Errorf("with a configuration that cannot be read: %v", err)
	}
	keyFile := filepath.Join(dataDir, "key")
	os.WriteFile(keyFile, []byte("sk-from-a-file\n"), 0o600)
	mustCLI(t, "model", "set", "--data-dir", dataDir, "--provider", "openai", "--model", "gpt-5", "--api-key-file", keyFile)
	db := journalIn(t, filepath.Join(dataDir, "node.db"))
	if cfg, _, _ := db.ModelConfig(); cfg.APIKey != "sk-from-a-file" || cfg.Provider != "openai" {
		t.Errorf("the configuration set from the command line: %+v", cfg)
	}
	db.Close()
	// A token there, and nobody at the address to give it to.
	os.WriteFile(filepath.Join(dataDir, "api.token"), []byte("t"), 0o600)
	if _, err := cli(t, "ask", "--data-dir", dataDir, "--api", freeAddr(t), "Hello?"); err == nil {
		t.Error("asking a node that is not there succeeded")
	}
	if _, err := cli(t, "ask", "--data-dir", dataDir, "--api", "bad\x00address", "Hello?"); err == nil {
		t.Error("asking at an address that is none succeeded")
	}
}

func TestWhatThePlannerDoesAsItIsPrinted(t *testing.T) {
	long := strings.Repeat("x", 1000)
	for want, e := range map[string]*nodepb.AskEvent{
		"There are 25.":           {Kind: planner.Text, Text: "There are 25."},
		"\n[run_job {\"a\":1}]\n": {Kind: planner.Calling, Tool: "run_job", Text: `{"a":1}`},
		"[job abc123]\n":          {Kind: planner.Job, JobId: "abc123"},
		"[{\"count\":25}]\n":      {Kind: planner.Result, Text: `{"count":25}`},
		"\n\n(chat c0ffee)\n":     {Kind: "done", ChatId: "c0ffee"},
	} {
		if got := describeAsk(e); got != want {
			t.Errorf("%v is printed %q, want %q", e, got, want)
		}
	}
	if got := describeAsk(&nodepb.AskEvent{Kind: planner.Result, Text: long}); len(got) > 320 || !strings.HasSuffix(got, "…]\n") {
		t.Errorf("a long result is printed as %d bytes", len(got))
	}
}

// shelves is somewhere to keep an assistant's state that fails on demand.
type shelves struct {
	assistantStore
	failing string
}

var errShelf = errors.New("the database is locked")

func (s *shelves) ModelConfig() (nodedb.ModelConfig, bool, error) {
	if s.failing == "ModelConfig" {
		return nodedb.ModelConfig{}, false, errShelf
	}
	return s.assistantStore.ModelConfig()
}

func (s *shelves) Chats() ([]nodedb.Chat, error) {
	if s.failing == "Chats" {
		return nil, errShelf
	}
	return s.assistantStore.Chats()
}

func (s *shelves) CreateChat(c nodedb.Chat) error {
	if s.failing == "CreateChat" {
		return errShelf
	}
	return s.assistantStore.CreateChat(c)
}

func (s *shelves) ChatMessages(id string) ([]string, error) {
	if s.failing == "ChatMessages" {
		return nil, errShelf
	}
	return s.assistantStore.ChatMessages(id)
}

func (s *shelves) AppendChatMessages(id string, messages []string, now time.Time) error {
	if s.failing == "AppendChatMessages" {
		return errShelf
	}
	return s.assistantStore.AppendChatMessages(id, messages, now)
}

func TestAnAssistantWhoseStateCannotBeKeptSaysSo(t *testing.T) {
	served := newModel(t, says("One."), says("Two."), says("Three."), says("Four."))
	db := journalIn(t, filepath.Join(t.TempDir(), "node.db"))
	store := &shelves{assistantStore: db}
	p := startPool(t, runtime.Builtin())
	a := &assistant{store: store, pool: p.coord, workloads: runtime.Builtin(), offered: func(string) bool { return true }}
	if err := a.SetModelConfig(nodedb.ModelConfig{Provider: ai.Ollama, BaseURL: served.URL, Model: "test-model"}); err != nil {
		t.Fatal(err)
	}
	quiet := func(string, planner.Event) {}
	ctx := context.Background()
	chatID, err := a.Ask(ctx, "", strings.Repeat("A very long question indeed. ", 10), quiet)
	if err != nil {
		t.Fatal(err)
	}
	// A long question names its conversation by its beginning.
	if chats, _ := a.Chats(); len(chats) != 1 || len(chats[0].Title) > 70 || !strings.HasSuffix(chats[0].Title, "…") {
		t.Errorf("chats: %+v", chats)
	}

	for failing, call := range map[string]func() error{
		"ModelConfig":        func() error { _, err := a.Ask(ctx, chatID, "More?", quiet); return err },
		"CreateChat":         func() error { _, err := a.Ask(ctx, "", "A new one?", quiet); return err },
		"Chats":              func() error { _, err := a.Ask(ctx, chatID, "More?", quiet); return err },
		"ChatMessages":       func() error { _, err := a.Ask(ctx, chatID, "More?", quiet); return err },
		"AppendChatMessages": func() error { _, err := a.Ask(ctx, chatID, "More?", quiet); return err },
	} {
		store.failing = failing
		if err := call(); !errors.Is(err, errShelf) {
			t.Errorf("with %s failing: %v, want the store's error", failing, err)
		}
	}
	store.failing = "ModelConfig"
	if _, err := a.Models(ctx, nil); !errors.Is(err, errShelf) {
		t.Errorf("Models with the configuration unreadable: %v", err)
	}
}

func TestAttachmentRetentionFailureDoesNotCallTheModel(t *testing.T) {
	served := newModel(t)
	db := journalIn(t, filepath.Join(t.TempDir(), "node.db"))
	p := startPool(t, runtime.Builtin())
	a := &assistant{store: db, pool: p.coord, workloads: runtime.Builtin(), offered: func(string) bool { return true }}
	if err := a.SetModelConfig(nodedb.ModelConfig{Provider: ai.Ollama, BaseURL: served.URL, Model: "test-model"}); err != nil {
		t.Fatal(err)
	}
	id, err := a.AskWithFiles(context.Background(), "", "Question", []string{"missing"}, func(string, planner.Event) {
		t.Error("a rejected attachment turn emitted planner events")
	})
	if id == "" || status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("AskWithFiles: %q, %v", id, err)
	}
	served.mu.Lock()
	defer served.mu.Unlock()
	if len(served.asked) != 0 {
		t.Fatal("model was called before attachment ownership succeeded")
	}
	if messages, err := db.ChatMessages(id); err != nil || len(messages) != 0 {
		t.Fatalf("rejected question was saved: %v, %v", messages, err)
	}
}

func TestTheDesktopChoosesAServiceAndFetchesAModelForIt(t *testing.T) {
	saved, other := newModel(t), newModel(t)
	dataDir := t.TempDir()
	apiAddr := freeAddr(t)
	startDaemon(t, "--data-dir", dataDir, "--listen", freeAddr(t), "--api-listen", apiAddr)
	client := desktop(t, apiAddr)
	poolAsSeenBy(t, client)
	ctx := tokenOf(t, dataDir)

	// A picker starts from the providers there are, and can look at what
	// one offers before anything is saved.
	providers, err := client.ListProviders(context.Background(), &nodepb.ListProvidersRequest{})
	if err != nil || len(providers.GetProviders()) != 3 {
		t.Fatalf("ListProviders = %v, %v", providers, err)
	}
	at := func(m *model) *nodepb.ModelService { return &nodepb.ModelService{Provider: "ollama", BaseUrl: m.URL} }
	offered, err := client.ListModels(ctx, &nodepb.ListModelsRequest{Service: at(other)})
	if err != nil || len(offered.GetDetails()) != 2 {
		t.Fatalf("the models of a service not yet chosen: %v, %v", offered, err)
	}

	// The saved key goes to the service it was saved for and to no other.
	if _, err := client.SetModelConfig(ctx, &nodepb.SetModelConfigRequest{Provider: "ollama", BaseUrl: saved.URL, Model: "test-model", ApiKey: "sk-secret"}); err != nil {
		t.Fatal(err)
	}
	for _, m := range []*model{saved, other} {
		service := at(m)
		service.KeepApiKey = true
		if _, err := client.ListModels(ctx, &nodepb.ListModelsRequest{Service: service}); err != nil {
			t.Fatal(err)
		}
	}
	if got := saved.keys[len(saved.keys)-1]; got != "Bearer sk-secret" {
		t.Errorf("the saved service was shown %q", got)
	}
	if got := other.keys[len(other.keys)-1]; got != "" {
		t.Errorf("another service was shown the saved key: %q", got)
	}

	// Fetching a model reports how far it has got and ends when it is there.
	pulling, err := client.PullModel(ctx, &nodepb.PullModelRequest{Service: at(other), Model: "llama3.1:8b"})
	if err != nil {
		t.Fatal(err)
	}
	var steps []*nodepb.PullModelProgress
	for {
		step, err := pulling.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		steps = append(steps, step)
	}
	if len(steps) != 5 || steps[1].GetStatus() != "pulling layer" || steps[1].GetCompletedBytes() != 50 || steps[1].GetTotalBytes() != 200 || steps[4].GetStatus() != "success" || other.pulled[0] != "llama3.1:8b" {
		t.Errorf("steps = %v, pulled %v", steps, other.pulled)
	}
	// With no service named, it is the configured one that is told.
	if _, err := client.RemoveModel(ctx, &nodepb.RemoveModelRequest{Model: "another-model"}); err != nil || len(saved.removed) != 1 || saved.removed[0] != "another-model" {
		t.Errorf("removed %v, %v", saved.removed, err)
	}

	failed := func(req *nodepb.PullModelRequest) error {
		stream, _ := client.PullModel(ctx, req)
		_, err := stream.Recv()
		return err
	}
	_, unnamed := client.RemoveModel(ctx, &nodepb.RemoveModelRequest{})
	_, pigeon := client.ListModels(ctx, &nodepb.ListModelsRequest{Service: &nodepb.ModelService{Provider: "carrier-pigeon"}})
	_, hosted := client.RemoveModel(ctx, &nodepb.RemoveModelRequest{Service: &nodepb.ModelService{Provider: "openai"}, Model: "gpt-5"})
	for name, tt := range map[string]struct {
		err  error
		want codes.Code
	}{
		"pulling a model there is none of":          {failed(&nodepb.PullModelRequest{Service: at(other), Model: "no-such-model"}), codes.Internal},
		"pulling no model":                          {failed(&nodepb.PullModelRequest{Service: at(other)}), codes.InvalidArgument},
		"pulling from a service that fetches none":  {failed(&nodepb.PullModelRequest{Service: &nodepb.ModelService{Provider: "anthropic"}, Model: "m"}), codes.FailedPrecondition},
		"removing no model":                         {unnamed, codes.InvalidArgument},
		"removing from a service that fetches none": {hosted, codes.FailedPrecondition},
		"the models of a provider there is none of": {pigeon, codes.InvalidArgument},
	} {
		if status.Code(tt.err) != tt.want {
			t.Errorf("%s: %v, want %v", name, tt.err, tt.want)
		}
	}

	// Having the node call a service it is told of, or fetch or delete
	// anything, needs the token.
	open := context.Background()
	_, look := client.ListModels(open, &nodepb.ListModelsRequest{Service: at(other)})
	_, remove := client.RemoveModel(open, &nodepb.RemoveModelRequest{Model: "test-model"})
	stream, _ := client.PullModel(open, &nodepb.PullModelRequest{Model: "test-model"})
	_, pull := stream.Recv()
	for call, err := range map[string]error{"ListModels of a named service": look, "RemoveModel": remove, "PullModel": pull} {
		if status.Code(err) != codes.PermissionDenied {
			t.Errorf("%s without the token: %v", call, err)
		}
	}

	// And the same from the command line, which needs no running node.
	if out := mustCLI(t, "model", "providers"); !strings.Contains(out, "ollama") || !strings.Contains(out, "anthropic") || !strings.Contains(out, "openai") {
		t.Errorf("model providers:\n%s", out)
	}
	out := mustCLI(t, "model", "pull", "--data-dir", t.TempDir(), "--url", other.URL, "--model", "qwen3:8b")
	if out != "pulling manifest\npulling layer 25%\npulling layer 100%\nsuccess\nqwen3:8b is there to plan with: sisyphusd model set --model qwen3:8b\n" {
		t.Errorf("model pull printed:\n%s", out)
	}
	if out := mustCLI(t, "model", "remove", "--data-dir", t.TempDir(), "--url", other.URL, "--model", "qwen3:8b"); out != "removed qwen3:8b\n" || other.removed[0] != "qwen3:8b" {
		t.Errorf("model remove printed %q, removed %v", out, other.removed)
	}
	if _, err := cli(t, "model", "pull", "--data-dir", t.TempDir(), "--url", other.URL, "--model", "no-such-model"); err == nil || !strings.Contains(err.Error(), "could not be fetched") {
		t.Errorf("pulling a model there is none of: %v", err)
	}
}

func TestAModelIsListedWithWhatIsKnownOfIt(t *testing.T) {
	if got := describeModel(ai.Model{Name: "claude-opus-5-5", Label: "Claude Opus 5.5", Tools: ai.Yes}); got != "claude-opus-5-5  Claude Opus 5.5" {
		t.Errorf("a model with a name for people: %q", got)
	}
}

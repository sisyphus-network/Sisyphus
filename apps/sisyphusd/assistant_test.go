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
		if r.URL.Path == "/api/tags" {
			io.WriteString(w, `{"models":[{"name":"test-model"},{"name":"another-model"}]}`)
			return
		}
		body, _ := io.ReadAll(r.Body)
		var asked map[string]any
		json.Unmarshal(body, &asked)
		m.mu.Lock()
		defer m.mu.Unlock()
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
	if out := mustCLI(t, "model", "list", "--data-dir", dataDir); out != "test-model\nanother-model\n" {
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
	if jobs, err := client.ListJobs(context.Background(), &nodepb.ListJobsRequest{}); err != nil || len(jobs.GetJobs()) != 1 || string(jobs.GetJobs()[0].GetResult()) != `{"count":25}` {
		t.Errorf("jobs after the question: %v, %v", jobs, err)
	}

	// The conversation carries on, with what went before in front of the model.
	out = mustCLI(t, "ask", "--data-dir", dataDir, "--api", apiAddr, "--chat", chatID, "And below 1000?")
	if !strings.Contains(out, `"result":{"count":168}`) || !strings.Contains(out, "And 168 below 1000.") || !strings.Contains(out, "(chat "+chatID+")") {
		t.Errorf("the second question:\n%s", out)
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
	if out := mustCLI(t, "model", "show", "--data-dir", dataDir); !strings.Contains(out, "key:      set") || strings.Contains(out, "sk-secret") {
		t.Errorf("model show with a key set:\n%s", out)
	}
	dropped, err := client.SetModelConfig(ctx, &nodepb.SetModelConfigRequest{Provider: "ollama", BaseUrl: served.URL, Model: "test-model"})
	if err != nil || dropped.GetHasApiKey() {
		t.Errorf("setting a configuration with no key: %v, %v", dropped, err)
	}
	if models, err := client.ListModels(ctx, &nodepb.ListModelsRequest{}); err != nil || len(models.GetModels()) != 2 {
		t.Errorf("ListModels = %v, %v", models, err)
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
		{[]string{"model"}, "expected model set, show or list"},
		{[]string{"model", "forget", "--data-dir", dataDir}, "expected model set, show or list"},
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
	a := &assistant{store: store, pool: p.coord, workloads: runtime.Builtin()}
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
	if _, err := a.Models(ctx); !errors.Is(err, errShelf) {
		t.Errorf("Models with the configuration unreadable: %v", err)
	}
}

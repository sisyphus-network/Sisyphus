package mcpserver

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"google.golang.org/grpc"

	nodepb "github.com/sisyphus-network/Sisyphus/packages/protocol/sisyphus/node/v1"
)

// stream gives what it has and then ends, or breaks.
type stream[T any] struct {
	grpc.ClientStream
	left   []*T
	broken bool
}

func (s *stream[T]) Recv() (*T, error) {
	if len(s.left) == 0 {
		if s.broken {
			return nil, errDown
		}
		return nil, io.EOF
	}
	next := s.left[0]
	s.left = s.left[1:]
	return next, nil
}

func (n *node) RemoveFile(context.Context, *nodepb.RemoveFileRequest, ...grpc.CallOption) (*nodepb.RemoveFileResponse, error) {
	return &nodepb.RemoveFileResponse{}, n.fail("RemoveFile")
}

func (n *node) Ask(_ context.Context, req *nodepb.AskRequest, _ ...grpc.CallOption) (grpc.ServerStreamingClient[nodepb.AskEvent], error) {
	if err := n.fail("Ask"); err != nil {
		return nil, err
	}
	return &stream[nodepb.AskEvent]{broken: n.broken, left: []*nodepb.AskEvent{
		{ChatId: "chat-1", Kind: "call", Tool: "run_job", Text: `{"workload":"primes"}`},
		{ChatId: "chat-1", Kind: "job", JobId: "job-7"},
		{ChatId: "chat-1", Kind: "result", Text: `{"count":25}`},
		{ChatId: "chat-1", Kind: "text", Text: "There are "},
		{ChatId: "chat-1", Kind: "text", Text: "25."},
		{ChatId: "chat-1", Kind: "done"},
	}}, nil
}

func (n *node) ListChats(context.Context, *nodepb.ListChatsRequest, ...grpc.CallOption) (*nodepb.ListChatsResponse, error) {
	return &nodepb.ListChatsResponse{Chats: []*nodepb.ChatSummary{{ChatId: "chat-1", Title: "How many primes?", CreatedAtMs: 1791385246000}}}, n.fail("ListChats")
}

func (n *node) GetChat(context.Context, *nodepb.GetChatRequest, ...grpc.CallOption) (*nodepb.GetChatResponse, error) {
	return &nodepb.GetChatResponse{Messages: []*nodepb.ChatMessage{
		{Role: "user", Content: "How many primes?"},
		{Role: "assistant", Calls: []*nodepb.ChatToolCall{{Name: "run_job", Arguments: `{"workload":"primes"}`}}},
		{Role: "tool", Tool: "run_job", Content: `{"count":25}`},
	}}, n.fail("GetChat")
}

func (n *node) ListPeers(context.Context, *nodepb.ListPeersRequest, ...grpc.CallOption) (*nodepb.ListPeersResponse, error) {
	return &nodepb.ListPeersResponse{Peers: []*nodepb.Peer{
		{PeerId: "peer-1", ConnectionState: nodepb.PeerConnectionState_PEER_CONNECTION_STATE_CONNECTED, CountryCode: "NL", GivesWork: true, KnownAddresses: []string{"/ip4/193.0.6.139/tcp/7700"}},
		{PeerId: "peer-2"},
	}}, n.fail("ListPeers")
}

func (n *node) ListMembers(context.Context, *nodepb.ListMembersRequest, ...grpc.CallOption) (*nodepb.ListMembersResponse, error) {
	return &nodepb.ListMembersResponse{Members: []*nodepb.PoolMember{{PeerId: "peer-1", Role: nodepb.PoolRole_POOL_ROLE_WORKER}, {PeerId: "peer-3", Role: nodepb.PoolRole_POOL_ROLE_CLIENT}}}, n.fail("ListMembers")
}

func (n *node) GetModelConfig(context.Context, *nodepb.GetModelConfigRequest, ...grpc.CallOption) (*nodepb.ModelConfig, error) {
	return &nodepb.ModelConfig{Provider: "ollama", Model: "llama3.1:8b", HasApiKey: true}, n.fail("GetModelConfig")
}

func (n *node) SetModelConfig(_ context.Context, req *nodepb.SetModelConfigRequest, _ ...grpc.CallOption) (*nodepb.ModelConfig, error) {
	return &nodepb.ModelConfig{Provider: req.GetProvider(), Model: req.GetModel(), BaseUrl: req.GetBaseUrl(), HasApiKey: req.GetKeepApiKey()}, n.fail("SetModelConfig")
}

func (n *node) ListModels(context.Context, *nodepb.ListModelsRequest, ...grpc.CallOption) (*nodepb.ListModelsResponse, error) {
	return &nodepb.ListModelsResponse{Details: []*nodepb.Model{
		{Name: "llama3.1:8b", SizeBytes: 4900000000, Tools: nodepb.Support_SUPPORT_YES},
		{Name: "gemma3:4b", Tools: nodepb.Support_SUPPORT_NO},
		{Name: "claude-opus-5-5", Label: "Claude Opus 5.5"},
	}}, n.fail("ListModels")
}

func (n *node) PullModel(context.Context, *nodepb.PullModelRequest, ...grpc.CallOption) (grpc.ServerStreamingClient[nodepb.PullModelProgress], error) {
	if err := n.fail("PullModel"); err != nil {
		return nil, err
	}
	return &stream[nodepb.PullModelProgress]{broken: n.broken, left: []*nodepb.PullModelProgress{{Status: "pulling manifest"}, {Status: "pulling layer", TotalBytes: 4900, CompletedBytes: 10}, {Status: "success"}}}, nil
}

func (n *node) CreateInvitation(_ context.Context, req *nodepb.CreateInvitationRequest, _ ...grpc.CallOption) (*nodepb.CreateInvitationResponse, error) {
	return &nodepb.CreateInvitationResponse{Invitation: "invitation-for-" + req.GetRole().String(), ExpiresAtMs: int64(req.GetTtlSeconds()) * 1000}, n.fail("CreateInvitation")
}

func (n *node) RemoveMember(context.Context, *nodepb.RemoveMemberRequest, ...grpc.CallOption) (*nodepb.RemoveMemberResponse, error) {
	return &nodepb.RemoveMemberResponse{}, n.fail("RemoveMember")
}

func (n *node) JoinPool(context.Context, *nodepb.JoinPoolRequest, ...grpc.CallOption) (*nodepb.JoinPoolResponse, error) {
	return &nodepb.JoinPoolResponse{PeerId: "peer-9", Role: nodepb.PoolRole_POOL_ROLE_WORKER}, n.fail("JoinPool")
}

func (n *node) ConnectPeer(context.Context, *nodepb.ConnectPeerRequest, ...grpc.CallOption) (*nodepb.ConnectPeerResponse, error) {
	return &nodepb.ConnectPeerResponse{PeerId: "peer-9"}, n.fail("ConnectPeer")
}

func (n *node) SetPeerComputePermissions(context.Context, *nodepb.SetPeerComputePermissionsRequest, ...grpc.CallOption) (*nodepb.SetPeerComputePermissionsResponse, error) {
	return &nodepb.SetPeerComputePermissionsResponse{}, n.fail("SetPeerComputePermissions")
}

// each of the further tools, by the call it makes of the node.
var further = map[string]func(context.Context, *server) (any, error){
	"RemoveFile": func(ctx context.Context, s *server) (any, error) { return s.removeFile(ctx, cidArgs{CID: "cid-1"}) },
	"Ask": func(ctx context.Context, s *server) (any, error) {
		return s.askPlanner(ctx, askPlannerArgs{Question: "How many?"})
	},
	"ListChats":      func(ctx context.Context, s *server) (any, error) { return s.listChats(ctx, none{}) },
	"GetChat":        func(ctx context.Context, s *server) (any, error) { return s.getChat(ctx, chatArgs{ChatID: "chat-1"}) },
	"ListPeers":      func(ctx context.Context, s *server) (any, error) { return s.listPeers(ctx, none{}) },
	"ListMembers":    func(ctx context.Context, s *server) (any, error) { return s.listMembers(ctx, none{}) },
	"GetModelConfig": func(ctx context.Context, s *server) (any, error) { return s.getModel(ctx, none{}) },
	"ListModels":     func(ctx context.Context, s *server) (any, error) { return s.listModels(ctx, none{}) },
	"SetModelConfig": func(ctx context.Context, s *server) (any, error) {
		return s.setModel(ctx, setModelArgs{Provider: "ollama", Model: "qwen3:8b"})
	},
	"PullModel": func(ctx context.Context, s *server) (any, error) {
		return s.pullModel(ctx, pullArgs{Model: "qwen3:8b"})
	},
	"CreateInvitation": func(ctx context.Context, s *server) (any, error) {
		return s.invite(ctx, inviteArgs{Role: "client", TTLHours: 2})
	},
	"RemoveMember": func(ctx context.Context, s *server) (any, error) {
		return s.removeMember(ctx, peerArgs{PeerID: "peer-1"})
	},
	"JoinPool": func(ctx context.Context, s *server) (any, error) {
		return s.joinPool(ctx, joinArgs{Address: "203.0.113.9:7700", Invitation: "i"})
	},
	"ConnectPeer": func(ctx context.Context, s *server) (any, error) {
		return s.connectPeer(ctx, connectArgs{Address: "/ip4/203.0.113.9/tcp/7700/p2p/peer-9"})
	},
	"SetPeerComputePermissions": func(ctx context.Context, s *server) (any, error) {
		return s.setTrust(ctx, trustArgs{PeerID: "peer-1", GivesWork: true})
	},
	"SubmitJob": func(ctx context.Context, s *server) (any, error) {
		return s.askModel(ctx, askModelArgs{Model: "llama3.1:8b", Prompt: "Hi"})
	},
}

func TestTheFurtherToolsSayWhatTheNodeSaidAndWhenItFailsThem(t *testing.T) {
	ctx := context.Background()
	answered := map[string]string{}
	for call, tool := range further {
		if _, err := tool(ctx, serving(&node{failing: call})); err == nil || !strings.Contains(err.Error(), "the node is down") {
			t.Errorf("with %s failing: %v", call, err)
		}
		said, err := tool(ctx, serving(&node{job: &nodepb.Job{JobId: "job-1", State: nodepb.JobState_JOB_STATE_SUCCEEDED, FinishedAtMs: 2, Result: []byte(`{"choices":[{"message":{"content":"Hello."}}]}`)}}))
		if err != nil {
			t.Errorf("%s: %v", call, err)
		}
		result, _, _ := shown(said, nil)
		answered[call] = result.Content[0].(*mcp.TextContent).Text
	}
	for call, want := range map[string]string{
		"RemoveFile":                `{"removed":"cid-1"}`,
		"Ask":                       `{"answer":"There are 25.","chat_id":"chat-1","jobs":["job-7"]}`,
		"ListChats":                 `"started":"2026-10-07T15:00:46Z"`,
		"GetChat":                   `{"calls":[{"arguments":"{\"workload\":\"primes\"}","name":"run_job"}],"role":"assistant"}`,
		"ListPeers":                 `"country":"NL"`,
		"ListMembers":               `"peer_id":"peer-3","role":"client"`,
		"GetModelConfig":            `{"has_key":true,"model":"llama3.1:8b","provider":"ollama","url":""}`,
		"ListModels":                `{"calls_tools":false,"name":"gemma3:4b"},{"label":"Claude Opus 5.5","name":"claude-opus-5-5"}`,
		"SetModelConfig":            `"has_key":true,"model":"qwen3:8b"`,
		"PullModel":                 `{"fetched":"qwen3:8b","largest_part_bytes":4900}`,
		"CreateInvitation":          `"invitation":"invitation-for-POOL_ROLE_CLIENT","role":"client"`,
		"RemoveMember":              `{"removed":"peer-1"}`,
		"JoinPool":                  `{"as":"worker","joined":"peer-9"}`,
		"ConnectPeer":               `{"connected":"peer-9"}`,
		"SetPeerComputePermissions": `{"gives_work":true,"peer_id":"peer-1","takes_work":false}`,
		"SubmitJob":                 `"answer":"Hello."`,
	} {
		if !strings.Contains(answered[call], want) {
			t.Errorf("%s said %s, want %s in it", call, answered[call], want)
		}
	}
	if !strings.Contains(answered["ListPeers"], `"calls_tools":true`) && !strings.Contains(answered["ListModels"], `"calls_tools":true,"name":"llama3.1:8b","size_bytes":4900000000`) {
		t.Errorf("list_models said %s", answered["ListModels"])
	}

	// A stream that breaks part way is a failure, not an answer cut short.
	if _, err := serving(&node{broken: true}).askPlanner(ctx, askPlannerArgs{Question: "How many?"}); err == nil {
		t.Error("a planner that broke off was taken at its word")
	}
	if _, err := serving(&node{broken: true}).pullModel(ctx, pullArgs{Model: "m"}); err == nil {
		t.Error("a fetch that broke off was taken for done")
	}
	// A model's reply that is not in the usual form is passed on as it is,
	// with instructions for the model put first when there are any.
	odd := &node{job: &nodepb.Job{JobId: "job-1", State: nodepb.JobState_JOB_STATE_SUCCEEDED, FinishedAtMs: 2, Result: []byte(`{"choices":[]}`)}}
	if said, err := serving(odd).askModel(ctx, askModelArgs{Model: "m", Prompt: "Hi", System: "Be brief."}); err != nil || said.(map[string]any)["answer"] != nil || said.(map[string]any)["result"] == nil {
		t.Errorf("an odd reply: %v, %v", said, err)
	}
	for role, want := range map[string]string{"": "worker", "worker": "worker"} {
		if said, err := serving(&node{}).invite(ctx, inviteArgs{Role: role}); err != nil || said.(map[string]any)["role"] != want {
			t.Errorf("an invitation for %q: %v, %v", role, said, err)
		}
	}
	if _, err := serving(&node{}).invite(ctx, inviteArgs{Role: "owner"}); err == nil || !strings.Contains(err.Error(), "worker or client") {
		t.Errorf("an invitation to a role there is none of: %v", err)
	}
}

func TestAServerOffersOnlyTheToolsItsOwnerAllows(t *testing.T) {
	names := func(cfg Config) string {
		cfg.Node = &node{}
		ours, theirs := mcp.NewInMemoryTransports()
		ctx, hangUp := context.WithCancel(context.Background())
		defer hangUp()
		go New(cfg).Run(ctx, theirs)
		session, err := mcp.NewClient(&mcp.Implementation{Name: "t", Version: "1"}, nil).Connect(ctx, ours, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer session.Close()
		tools, err := session.ListTools(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		var listed []string
		for _, tool := range tools.Tools {
			listed = append(listed, tool.Name)
		}
		// A tool is a tool of the node as its owner: the token goes with it.
		if _, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "list_chats"}); err != nil {
			t.Fatal(err)
		}
		return strings.Join(listed, " ")
	}
	usual, looking, trusted := names(Config{}), names(Config{ReadOnly: true, Admin: true}), names(Config{Admin: true})
	if n := len(strings.Fields(usual)); n != 23 || strings.Contains(usual, "set_model") || !strings.Contains(usual, "ask_model") {
		t.Errorf("as usual, %d tools: %s", n, usual)
	}
	if n := len(strings.Fields(looking)); n != 14 || strings.Contains(looking, "run_job") || strings.Contains(looking, "join_pool") {
		t.Errorf("read-only, %d tools: %s", n, looking)
	}
	if n := len(strings.Fields(trusted)); n != 31 || !strings.Contains(trusted, "set_peer_trust") {
		t.Errorf("with admin, %d tools: %s", n, trusted)
	}
}

func TestFilesAreKeptToTheDirectoryGivenAndImagesToThoseListed(t *testing.T) {
	home, elsewhere := t.TempDir(), t.TempDir()
	os.WriteFile(filepath.Join(elsewhere, "secret"), []byte("s"), 0o600)
	os.Symlink(elsewhere, filepath.Join(home, "way-out"))
	os.Mkdir(filepath.Join(home, "sub"), 0o700)
	s := serving(&node{content: []string{"some"}})
	s.FilesUnder = home
	for path, inside := range map[string]bool{
		filepath.Join(home, "a.txt"):               true,
		filepath.Join(home, "sub", "new", "b.txt"): true,
		filepath.Join(home, "sub", "..", "c.txt"):  true,
		filepath.Join(home, "..", "d.txt"):         false,
		filepath.Join(elsewhere, "secret"):         false,
		filepath.Join(home, "way-out", "secret"):   false, // a link inside that leads outside
		filepath.Join(home, "way-out", "new", "e"): false,
		"/etc/passwd": false,
	} {
		if _, err := s.allowed(path); (err == nil) != inside {
			t.Errorf("allowed(%s): %v, and it should be allowed: %v", path, err, inside)
		}
	}
	ctx := context.Background()
	if _, err := s.store(ctx, storeArgs{Path: filepath.Join(elsewhere, "secret")}); err == nil || !strings.Contains(err.Error(), "is outside") {
		t.Errorf("storing from outside: %v", err)
	}
	if _, err := s.fetch(ctx, fetchArgs{CID: "c", Path: filepath.Join(elsewhere, "taken")}); err == nil || !strings.Contains(err.Error(), "is outside") {
		t.Errorf("fetching to outside: %v", err)
	}
	// A file already there is left alone unless told otherwise.
	kept := filepath.Join(home, "kept.txt")
	os.WriteFile(kept, []byte("mine"), 0o600)
	if _, err := s.fetch(ctx, fetchArgs{CID: "c", Path: kept}); err == nil || !strings.Contains(err.Error(), "already") {
		t.Errorf("fetching over a file: %v", err)
	}
	if was, _ := os.ReadFile(kept); string(was) != "mine" {
		t.Errorf("the file there was changed to %q", was)
	}
	if _, err := s.fetch(ctx, fetchArgs{CID: "c", Path: kept, Overwrite: true}); err != nil {
		t.Errorf("fetching over a file when told to: %v", err)
	}
	s.FilesUnder = filepath.Join(home, "gone")
	if _, err := s.allowed(kept); err == nil || !strings.Contains(err.Error(), "cannot be found") {
		t.Errorf("with its directory gone: %v", err)
	}

	s.Images = []string{"alpine:3.20"}
	for name, tt := range map[string]struct {
		args runJobArgs
		ok   bool
	}{
		"a listed image":   {runJobArgs{Workload: "container", Params: map[string]any{"image": "alpine:3.20"}}, true},
		"an unlisted one":  {runJobArgs{Workload: "container", Params: map[string]any{"image": "busybox"}}, false},
		"no image at all":  {runJobArgs{Workload: "container"}, false},
		"another workload": {runJobArgs{Workload: "primes"}, true},
	} {
		if err := s.permitted(tt.args); (err == nil) != tt.ok {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := s.run(ctx, runJobArgs{Workload: "container", Params: map[string]any{"image": "busybox"}}); err == nil || !strings.Contains(err.Error(), "alpine:3.20") {
		t.Errorf("running an image not listed: %v", err)
	}
}

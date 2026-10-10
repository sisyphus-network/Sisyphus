package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	nodepb "github.com/sisyphus-network/Sisyphus/packages/protocol/sisyphus/node/v1"
)

// What a tool does, which decides whether a server offers it.
type kind int

const (
	// reads is a tool that changes nothing.
	reads kind = iota
	// uses is one that uses the pool: runs a job, stores a file.
	uses
	// admin is one that changes the node itself or who is in its pool.
	admin
)

// offers reports whether the server offers tools of a kind: all of them
// that read, those that use the pool unless it is read-only, and those that
// change the node only if its owner said so.
func (s *server) offers(k kind) bool {
	switch k {
	case uses:
		return !s.ReadOnly
	case admin:
		return s.Admin && !s.ReadOnly
	}
	return true
}

// add gives a server a tool, if it offers tools of that kind.
func add[In any](s *server, out *mcp.Server, k kind, tool *mcp.Tool, handler mcp.ToolHandlerFor[In, any]) {
	if s.offers(k) {
		mcp.AddTool(out, tool, handler)
	}
}

// doing makes a tool of something done with the node as its owner.
func doing[In any](s *server, do func(context.Context, In) (any, error)) mcp.ToolHandlerFor[In, any] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, in In) (*mcp.CallToolResult, any, error) {
		return shown(do(s.as(ctx), in))
	}
}

// more gives a server the tools beyond running jobs and moving files.
func (s *server) more(out *mcp.Server) {
	seen := &mcp.ToolAnnotations{ReadOnlyHint: true}
	gone := &mcp.ToolAnnotations{DestructiveHint: ptr(true)}
	add(s, out, uses, &mcp.Tool{Name: "remove_file", Annotations: gone,
		Description: "Stops keeping a stored file. What it held can no longer be given to a job once the store has cleared it away."}, doing(s, s.removeFile))
	add(s, out, uses, &mcp.Tool{Name: "ask_model",
		Description: "Has a language model served by one of the pool's workers answer a prompt, and returns what it said. pool_status shows which models each worker serves. The worker that runs it sees the prompt."}, doing(s, s.askModel))
	add(s, out, uses, &mcp.Tool{Name: "ask_planner",
		Description: "Puts a question to the node's own planner, which reasons with the model the node is set to plan with and runs jobs on the pool to answer. Returns its answer and the jobs it ran. Give chat_id to carry a conversation on."}, doing(s, s.askPlanner))
	add(s, out, reads, &mcp.Tool{Name: "list_chats", Annotations: seen,
		Description: "Lists the conversations had with the node's planner, newest first."}, doing(s, s.listChats))
	add(s, out, reads, &mcp.Tool{Name: "get_chat", Annotations: seen,
		Description: "Returns a conversation with the planner in full: what was asked, what the model said, the tools it called and what they returned."}, doing(s, s.getChat))
	add(s, out, reads, &mcp.Tool{Name: "list_peers", Annotations: seen,
		Description: "Lists the nodes this node knows of: whether each is connected, the country its address is registered in, and which way work flows between them."}, doing(s, s.listPeers))
	add(s, out, reads, &mcp.Tool{Name: "list_address_book", Annotations: seen,
		Description: "Lists the nodes this node connects to each time it starts, each with its ID and address. connect_peer adds to them and forget_peer takes one away."}, doing(s, s.addressBook))
	add(s, out, reads, &mcp.Tool{Name: "list_members", Annotations: seen,
		Description: "Lists the nodes admitted to this node's pool by invitation, as workers or as clients. The node itself, whose pool it is, is not among them: pool_status shows it with the workers connected now."}, doing(s, s.listMembers))
	add(s, out, reads, &mcp.Tool{Name: "get_model", Annotations: seen,
		Description: "Says which language model the node's planner plans with, and whether a key is set for its service."}, doing(s, s.getModel))
	add(s, out, reads, &mcp.Tool{Name: "list_models", Annotations: seen,
		Description: "Lists the models the planner's configured service offers, and whether each can call tools, which a planner's model must."}, doing(s, s.listModels))

	add(s, out, admin, &mcp.Tool{Name: "set_model",
		Description: "Sets which language model the node's planner plans with. The key already saved is kept if the provider and address stay the same, and otherwise the service is saved without one: the answer's has_key says which."}, doing(s, s.setModel))
	add(s, out, admin, &mcp.Tool{Name: "pull_model",
		Description: "Fetches a model into the Ollama the planner is set to use, which can take minutes and gigabytes. Returns when it is there."}, doing(s, s.pullModel))
	add(s, out, admin, &mcp.Tool{Name: "create_invitation",
		Description: "Issues an invitation that lets another node join this node's pool, as a worker that runs its tasks or a client that submits jobs. Whoever holds it can join until it expires."}, doing(s, s.invite))
	add(s, out, admin, &mcp.Tool{Name: "remove_member", Annotations: gone,
		Description: "Takes a node out of this node's pool and disconnects it."}, doing(s, s.removeMember))
	add(s, out, admin, &mcp.Tool{Name: "join_pool",
		Description: "Joins another node's pool with an invitation it issued, and starts taking its work."}, doing(s, s.joinPool))
	add(s, out, admin, &mcp.Tool{Name: "connect_peer",
		Description: "Connects to a node at an address, such as /ip4/203.0.113.9/tcp/7700/p2p/<its ID>, and remembers it."}, doing(s, s.connectPeer))
	add(s, out, admin, &mcp.Tool{Name: "forget_peer", Annotations: gone,
		Description: "Takes a node out of this node's address book, so that it is not connected to when the node next starts. It stays connected now, and in the pool if it is a member."}, doing(s, s.forgetPeer))
	add(s, out, admin, &mcp.Tool{Name: "set_peer_trust",
		Description: "Sets which way work may flow between this node and another: whether this node gives it work, by admitting it to the pool, and whether it takes work from it."}, doing(s, s.setTrust))
}

func ptr[T any](v T) *T { return &v }

// allowed returns path as it really is, if the server may read and write
// there: inside the directory its owner gave, following links to where
// they lead, so that a link inside to somewhere outside is outside.
func (s *server) allowed(path string) (string, error) {
	if s.FilesUnder == "" {
		return path, nil
	}
	root, err := filepath.EvalSymlinks(s.FilesUnder)
	if err != nil {
		return "", fmt.Errorf("the directory this server keeps to cannot be found: %w", err)
	}
	// The part of the path that exists is followed to where it really is.
	really, rest := path, ""
	if abs, err := filepath.Abs(path); err == nil {
		really = abs
	}
	for range strings.Count(really, string(filepath.Separator)) + 1 {
		resolved, err := filepath.EvalSymlinks(really)
		if err == nil {
			really = resolved
			break
		}
		really, rest = filepath.Dir(really), filepath.Join(filepath.Base(really), rest)
	}
	full := filepath.Join(really, rest)
	if within, err := filepath.Rel(root, full); err != nil || within == ".." || strings.HasPrefix(within, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("%s is outside %s, the directory this server may read and write in; it was started with --files-under", path, s.FilesUnder)
	}
	return full, nil
}

// permitted reports, as an error, a job that would run a container image
// the server's owner has not listed, if any were listed. A job made of jobs
// is held to it in each of its steps, however deep, and an image a step
// would only learn from an earlier one is not a listed image.
func (s *server) permitted(args runJobArgs) error {
	if len(s.Images) == 0 || s.listed(args.Workload, args.Params) {
		return nil
	}
	return s.unlisted()
}

// unlisted is what an agent is told of something the list of images rules
// out.
func (s *server) unlisted() error {
	return fmt.Errorf("this server runs only the container images its owner listed with --images: %s", strings.Join(s.Images, ", "))
}

// listed reports whether a job of a workload with these parameters would
// run no container image but those listed.
func (s *server) listed(workload string, params map[string]any) bool {
	switch workload {
	case "container":
		name, _ := params["image"].(string)
		return slices.Contains(s.Images, name)
	case "graph":
		steps, _ := params["steps"].([]any)
		for _, each := range steps {
			step, _ := each.(map[string]any)
			of, _ := step["workload"].(string)
			its, _ := step["params"].(map[string]any)
			if !s.listed(of, its) {
				return false
			}
		}
	}
	return true
}

// jobMode is how a job is to be shared out, as an agent says it. Said
// nothing, the node is told nothing and does as it usually does.
func jobMode(said string) (nodepb.JobMode, error) {
	switch said {
	case "":
		return nodepb.JobMode_JOB_MODE_UNSPECIFIED, nil
	case "distributed":
		return nodepb.JobMode_JOB_MODE_DISTRIBUTED, nil
	case "full-worker":
		return nodepb.JobMode_JOB_MODE_FULL_WORKER, nil
	}
	return 0, fmt.Errorf("a mode is distributed or full-worker, not %q", said)
}

type cidArgs struct {
	CID string `json:"cid" jsonschema:"the content ID of the file"`
}

func (s *server) removeFile(ctx context.Context, args cidArgs) (any, error) {
	if _, err := s.Node.RemoveFile(ctx, &nodepb.RemoveFileRequest{Cid: args.CID}); err != nil {
		return nil, err
	}
	return map[string]any{"removed": args.CID}, nil
}

type askModelArgs struct {
	Model       string `json:"model" jsonschema:"a model a worker serves, as pool_status names it; add @ and a worker's name or peer_id to have that worker and no other answer"`
	Prompt      string `json:"prompt" jsonschema:"what to ask"`
	System      string `json:"system,omitempty" jsonschema:"instructions for the model, if any"`
	WaitSeconds uint32 `json:"wait_seconds,omitempty" jsonschema:"how long to wait for the answer; 300 if left out"`
}

func (s *server) askModel(ctx context.Context, args askModelArgs) (any, error) {
	type message struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	}
	messages := []message{{"user", args.Prompt}}
	if args.System != "" {
		messages = append([]message{{"system", args.System}}, messages...)
	}
	ran, err := s.run(ctx, runJobArgs{Workload: "chat", Params: map[string]any{"model": args.Model, "messages": messages}, WaitSeconds: args.WaitSeconds})
	if err != nil {
		return nil, err
	}
	job := ran.(map[string]any)
	// The reply is taken out of the form it comes in, where it is there.
	var reply struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if raw, finished := job["result"].(json.RawMessage); finished && json.Unmarshal(raw, &reply) == nil && len(reply.Choices) > 0 {
		delete(job, "result")
		job["answer"] = reply.Choices[0].Message.Content
	}
	return job, nil
}

type askPlannerArgs struct {
	Question string   `json:"question" jsonschema:"what to ask the planner"`
	ChatID   string   `json:"chat_id,omitempty" jsonschema:"the conversation to carry on, as an earlier call returned it; leave out to start one"`
	Files    []string `json:"files,omitempty" jsonschema:"content IDs of stored files the question is about, as store_file returned them; the planner is told of them and the node keeps them for as long as the conversation. If any is private, every job the planner runs in the conversation is private"`
}

func (s *server) askPlanner(ctx context.Context, args askPlannerArgs) (any, error) {
	if len(s.Images) > 0 {
		// The planner picks what to run itself, and nothing here sees it
		// before it runs.
		return nil, fmt.Errorf("the node's planner chooses for itself what to run, so it cannot be asked through a server held to listed images: %w", s.unlisted())
	}
	question, err := s.attaching(ctx, args.Question, args.Files)
	if err != nil {
		return nil, err
	}
	stream, err := s.Node.Ask(ctx, &nodepb.AskRequest{ChatId: args.ChatID, Text: question, AttachmentCids: args.Files})
	if err != nil {
		return nil, err
	}
	var answer strings.Builder
	jobs := []string{}
	chat := args.ChatID
	for {
		e, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
		chat = e.GetChatId()
		switch e.GetKind() {
		case "text":
			answer.WriteString(e.GetText())
		case "job":
			jobs = append(jobs, e.GetJobId())
		}
	}
	return map[string]any{"chat_id": chat, "answer": answer.String(), "jobs": jobs}, nil
}

// attaching returns a question with the files it is about listed after it,
// in the form the planner reads them in, which is the form the desktop app
// writes: by it the planner knows each file's name and content ID, and that
// a conversation with a private file is to run private jobs.
func (s *server) attaching(ctx context.Context, question string, cids []string) (string, error) {
	if len(cids) == 0 {
		return question, nil
	}
	listed, err := s.Node.ListFiles(ctx, &nodepb.ListFilesRequest{})
	if err != nil {
		return "", err
	}
	stored := map[string]*nodepb.File{}
	for _, f := range listed.GetFiles() {
		stored[f.GetCid()] = f
	}
	lines := []string{question, "", "[Sisyphus attachments]"}
	for _, id := range cids {
		f, ok := stored[id]
		if !ok {
			return "", fmt.Errorf("%s is not a file this node has stored: store it with store_file first, and list_files shows those there are", id)
		}
		name, _ := json.Marshal(f.GetName()) // a string always encodes
		lines = append(lines, fmt.Sprintf("- %s | cid:%s | image:false | private:%t", name, id, f.GetPrivate()))
	}
	return strings.Join(lines, "\n"), nil
}

func when(ms int64) string { return time.UnixMilli(ms).UTC().Format(time.RFC3339) }

func (s *server) listChats(ctx context.Context, _ none) (any, error) {
	listed, err := s.Node.ListChats(ctx, &nodepb.ListChatsRequest{})
	if err != nil {
		return nil, err
	}
	out := []map[string]any{}
	for _, c := range listed.GetChats() {
		out = append(out, map[string]any{"chat_id": c.GetChatId(), "title": c.GetTitle(), "started": when(c.GetCreatedAtMs())})
	}
	return out, nil
}

type chatArgs struct {
	ChatID string `json:"chat_id" jsonschema:"the conversation's ID, as list_chats or ask_planner gave it"`
}

func (s *server) getChat(ctx context.Context, args chatArgs) (any, error) {
	got, err := s.Node.GetChat(ctx, &nodepb.GetChatRequest{ChatId: args.ChatID})
	if err != nil {
		return nil, err
	}
	out := []map[string]any{}
	for _, m := range got.GetMessages() {
		message := map[string]any{"role": m.GetRole()}
		if m.GetContent() != "" {
			message["content"] = m.GetContent()
		}
		if m.GetTool() != "" {
			message["tool"] = m.GetTool()
		}
		var calls []map[string]string
		for _, call := range m.GetCalls() {
			calls = append(calls, map[string]string{"name": call.GetName(), "arguments": call.GetArguments()})
		}
		if calls != nil {
			message["calls"] = calls
		}
		out = append(out, message)
	}
	return out, nil
}

func (s *server) listPeers(ctx context.Context, _ none) (any, error) {
	listed, err := s.Node.ListPeers(ctx, &nodepb.ListPeersRequest{})
	if err != nil {
		return nil, err
	}
	out := []map[string]any{}
	for _, p := range listed.GetPeers() {
		peer := map[string]any{
			"peer_id": p.GetPeerId(), "connected": p.GetConnectionState() == nodepb.PeerConnectionState_PEER_CONNECTION_STATE_CONNECTED,
			"addresses":  p.GetKnownAddresses(),
			"gives_work": p.GetGivesWork(), "takes_work": p.GetTakesWork(),
			"works_for_this_node": p.GetWorksForThisNode(), "this_node_works_for": p.GetThisNodeWorksFor(),
		}
		if p.GetCountryCode() != "" {
			peer["country"] = p.GetCountryCode()
		}
		out = append(out, peer)
	}
	return out, nil
}

// role names a role as an agent says it and is told it.
func role(r nodepb.PoolRole) string {
	if r == nodepb.PoolRole_POOL_ROLE_CLIENT {
		return "client"
	}
	return "worker"
}

func (s *server) listMembers(ctx context.Context, _ none) (any, error) {
	listed, err := s.Node.ListMembers(ctx, &nodepb.ListMembersRequest{})
	if err != nil {
		return nil, err
	}
	out := []map[string]any{}
	for _, m := range listed.GetMembers() {
		out = append(out, map[string]any{"peer_id": m.GetPeerId(), "role": role(m.GetRole()), "joined": when(m.GetJoinedAtMs())})
	}
	return out, nil
}

func modelView(cfg *nodepb.ModelConfig) map[string]any {
	return map[string]any{"provider": cfg.GetProvider(), "url": cfg.GetBaseUrl(), "model": cfg.GetModel(), "has_key": cfg.GetHasApiKey()}
}

func (s *server) getModel(ctx context.Context, _ none) (any, error) {
	cfg, err := s.Node.GetModelConfig(ctx, &nodepb.GetModelConfigRequest{})
	if err != nil {
		return nil, err
	}
	return modelView(cfg), nil
}

func (s *server) listModels(ctx context.Context, _ none) (any, error) {
	listed, err := s.Node.ListModels(ctx, &nodepb.ListModelsRequest{})
	if err != nil {
		return nil, err
	}
	out := []map[string]any{}
	for _, m := range listed.GetDetails() {
		model := map[string]any{"name": m.GetName()}
		if m.GetLabel() != "" {
			model["label"] = m.GetLabel()
		}
		if m.GetSizeBytes() > 0 {
			model["size_bytes"] = m.GetSizeBytes()
		}
		switch m.GetTools() {
		case nodepb.Support_SUPPORT_YES:
			model["calls_tools"] = true
		case nodepb.Support_SUPPORT_NO:
			model["calls_tools"] = false
		}
		out = append(out, model)
	}
	return out, nil
}

type setModelArgs struct {
	Provider string `json:"provider" jsonschema:"ollama, anthropic or openai, the last also for any service that speaks as OpenAI does"`
	Model    string `json:"model" jsonschema:"the model to plan with"`
	URL      string `json:"url,omitempty" jsonschema:"where the service is, if not in that provider's usual place"`
}

func (s *server) setModel(ctx context.Context, args setModelArgs) (any, error) {
	// An agent is never given a key to pass on: the one already there stays.
	cfg, err := s.Node.SetModelConfig(ctx, &nodepb.SetModelConfigRequest{Provider: args.Provider, BaseUrl: args.URL, Model: args.Model, KeepApiKey: true})
	if err != nil {
		return nil, err
	}
	return modelView(cfg), nil
}

type pullArgs struct {
	Model string `json:"model" jsonschema:"the model to fetch, as Ollama's library names it, such as llama3.1:8b"`
}

func (s *server) pullModel(ctx context.Context, args pullArgs) (any, error) {
	stream, err := s.Node.PullModel(ctx, &nodepb.PullModelRequest{Model: args.Model})
	if err != nil {
		return nil, err
	}
	var bytes uint64
	for {
		step, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
		bytes = max(bytes, step.GetTotalBytes())
	}
	return map[string]any{"fetched": args.Model, "largest_part_bytes": bytes}, nil
}

type inviteArgs struct {
	Role     string `json:"role,omitempty" jsonschema:"worker, to run the pool's tasks, or client, to submit jobs to it; worker if left out"`
	TTLHours uint32 `json:"ttl_hours,omitempty" jsonschema:"how long the invitation can be used for; the node's usual time if left out"`
}

func (s *server) invite(ctx context.Context, args inviteArgs) (any, error) {
	as := nodepb.PoolRole_POOL_ROLE_WORKER
	switch args.Role {
	case "client":
		as = nodepb.PoolRole_POOL_ROLE_CLIENT
	case "", "worker":
	default:
		return nil, fmt.Errorf("a role is worker or client, not %q", args.Role)
	}
	issued, err := s.Node.CreateInvitation(ctx, &nodepb.CreateInvitationRequest{Role: as, TtlSeconds: uint64(args.TTLHours) * 3600})
	if err != nil {
		return nil, err
	}
	return map[string]any{"invitation": issued.GetInvitation(), "role": role(as), "expires": when(issued.GetExpiresAtMs())}, nil
}

func (s *server) addressBook(ctx context.Context, _ none) (any, error) {
	kept, err := s.Node.GetBootstrapPeers(ctx, &nodepb.GetBootstrapPeersRequest{})
	if err != nil {
		return nil, err
	}
	return map[string]any{"peers": entries(kept.GetPeers())}, nil
}

// entries is an address book as a tool shows it.
func entries(peers []*nodepb.BootstrapPeer) []map[string]any {
	shown := []map[string]any{}
	for _, p := range peers {
		shown = append(shown, map[string]any{"peer_id": p.GetPeerId(), "address": p.GetAddress()})
	}
	return shown
}

// forgetPeer takes one node out of the address book. The book is set as a
// whole, so it is read, and set again without the node.
func (s *server) forgetPeer(ctx context.Context, args peerArgs) (any, error) {
	kept, err := s.Node.GetBootstrapPeers(ctx, &nodepb.GetBootstrapPeersRequest{})
	if err != nil {
		return nil, err
	}
	var rest []*nodepb.BootstrapPeer
	for _, p := range kept.GetPeers() {
		if p.GetPeerId() != args.PeerID {
			rest = append(rest, p)
		}
	}
	if len(rest) == len(kept.GetPeers()) {
		return nil, fmt.Errorf("the address book has no node %q; list_address_book shows those it has", args.PeerID)
	}
	set, err := s.Node.SetBootstrapPeers(ctx, &nodepb.SetBootstrapPeersRequest{Peers: rest})
	if err != nil {
		return nil, err
	}
	return map[string]any{"forgot": args.PeerID, "peers": entries(set.GetPeers())}, nil
}

type peerArgs struct {
	PeerID string `json:"peer_id" jsonschema:"the node's ID"`
}

func (s *server) removeMember(ctx context.Context, args peerArgs) (any, error) {
	if _, err := s.Node.RemoveMember(ctx, &nodepb.RemoveMemberRequest{PeerId: args.PeerID}); err != nil {
		return nil, err
	}
	return map[string]any{"removed": args.PeerID}, nil
}

type joinArgs struct {
	Address    string `json:"address" jsonschema:"the address of the node to join, as host:port"`
	Invitation string `json:"invitation" jsonschema:"an invitation that node issued"`
}

func (s *server) joinPool(ctx context.Context, args joinArgs) (any, error) {
	joined, err := s.Node.JoinPool(ctx, &nodepb.JoinPoolRequest{Address: args.Address, Invitation: args.Invitation})
	if err != nil {
		return nil, err
	}
	return map[string]any{"joined": joined.GetPeerId(), "as": role(joined.GetRole())}, nil
}

type connectArgs struct {
	Address string `json:"address" jsonschema:"a multiaddress ending in /p2p/ and the node's ID"`
}

func (s *server) connectPeer(ctx context.Context, args connectArgs) (any, error) {
	connected, err := s.Node.ConnectPeer(ctx, &nodepb.ConnectPeerRequest{Address: args.Address})
	if err != nil {
		return nil, err
	}
	return map[string]any{"connected": connected.GetPeerId()}, nil
}

type trustArgs struct {
	PeerID    string `json:"peer_id" jsonschema:"the node's ID"`
	GivesWork bool   `json:"gives_work" jsonschema:"whether this node gives that node work: it is admitted to the pool as a worker"`
	TakesWork bool   `json:"takes_work" jsonschema:"whether this node takes work from that node"`
}

func (s *server) setTrust(ctx context.Context, args trustArgs) (any, error) {
	if _, err := s.Node.SetPeerComputePermissions(ctx, &nodepb.SetPeerComputePermissionsRequest{PeerId: args.PeerID, GivesWork: args.GivesWork, TakesWork: args.TakesWork}); err != nil {
		return nil, err
	}
	return map[string]any{"peer_id": args.PeerID, "gives_work": args.GivesWork, "takes_work": args.TakesWork}, nil
}

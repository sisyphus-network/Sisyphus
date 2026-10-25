package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"google.golang.org/grpc"

	nodepb "github.com/sisyphus-network/Sisyphus/packages/protocol/sisyphus/node/v1"
	pb "github.com/sisyphus-network/Sisyphus/packages/protocol/sisyphus/v1"
)

func confirmationServerSession(t *testing.T) *mcp.ServerSession {
	t.Helper()
	ours, theirs := mcp.NewInMemoryTransports()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	server := mcp.NewServer(&mcp.Implementation{Name: "test", Version: "1"}, nil)
	go server.Run(ctx, theirs)
	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "1"}, &mcp.ClientOptions{ElicitationHandler: func(context.Context, *mcp.ElicitRequest) (*mcp.ElicitResult, error) {
		return &mcp.ElicitResult{Action: "accept", Content: map[string]any{"confirm": true}}, nil
	}})
	session, err := client.Connect(ctx, ours, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { session.Close() })
	for serverSession := range server.Sessions() {
		return serverSession
	}
	t.Fatal("connected MCP server has no session")
	return nil
}

func confirmationRequest(session *mcp.ServerSession) *mcp.CallToolRequest {
	return &mcp.CallToolRequest{Session: session, Params: &mcp.CallToolParamsRaw{Arguments: json.RawMessage(`{}`)}}
}

func TestDestructiveConfirmationStateIsActionBoundExpiringAndOneTime(t *testing.T) {
	s := &server{}
	req := confirmationRequest(confirmationServerSession(t))
	pending, confirmed, err := confirmDestructive(s, req, "delete model profile profile-1")
	if err != nil || confirmed || pending == nil || pending.RequestState == "" {
		t.Fatalf("first call = (%#v, %t, %v), want a pending confirmation", pending, confirmed, err)
	}
	state := pending.RequestState
	req.Params.RequestState = state
	req.Params.InputResponses = mcp.InputResponseMap{"confirmation": &mcp.ElicitResult{Action: "accept", Content: map[string]any{"confirm": true}}}
	if _, _, err := confirmDestructive(s, req, "delete another model profile"); err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("confirmation state was not bound to its action: %v", err)
	}
	if _, ok := s.confirmations[state]; !ok {
		t.Fatal("mismatched action consumed the valid pending confirmation")
	}
	if _, confirmed, err := confirmDestructive(s, req, "delete model profile profile-1"); err != nil || !confirmed {
		t.Fatalf("valid confirmation = (%t, %v), want accepted", confirmed, err)
	}
	if _, confirmed, err := confirmDestructive(s, req, "delete model profile profile-1"); err == nil || confirmed {
		t.Fatalf("confirmation was reusable: confirmed=%t err=%v", confirmed, err)
	}
}

func TestDestructiveConfirmationRejectsMissingAndExpiredState(t *testing.T) {
	s := &server{}
	if _, _, err := confirmDestructive(s, nil, "delete a file"); err == nil {
		t.Fatal("nil request was accepted")
	}
	req := confirmationRequest(confirmationServerSession(t))
	if _, _, err := confirmDestructive(s, req, "delete a file"); err != nil {
		t.Fatal(err)
	}
	for state, confirmation := range s.confirmations {
		confirmation.expires = time.Now().Add(-time.Second)
		s.confirmations[state] = confirmation
	}
	for state := range s.confirmations {
		req.Params.RequestState = state
		req.Params.InputResponses = mcp.InputResponseMap{"confirmation": &mcp.ElicitResult{Action: "accept", Content: map[string]any{"confirm": true}}}
		if _, confirmed, err := confirmDestructive(s, req, "delete a file"); err == nil || confirmed || !strings.Contains(err.Error(), "expired") {
			t.Fatalf("expired confirmation = (%t, %v)", confirmed, err)
		}
	}
	req.Params.RequestState = ""
	req.Params.InputResponses = nil
	if _, _, err := confirmDestructive(s, req, "delete a file"); err != nil {
		t.Fatalf("a fresh confirmation after expiry cleanup failed: %v", err)
	}
	if len(s.confirmations) != 1 {
		t.Fatalf("expired confirmations were not pruned: %d remain", len(s.confirmations))
	}
}

func TestDestructiveConfirmationQueueHasABound(t *testing.T) {
	s := &server{confirmations: make(map[string]pendingConfirmation, maxPendingConfirmations)}
	for i := range maxPendingConfirmations {
		s.confirmations[fmt.Sprintf("pending-%d", i)] = pendingConfirmation{action: "another action", expires: time.Now().Add(time.Minute)}
	}
	_, _, err := confirmDestructive(s, confirmationRequest(confirmationServerSession(t)), "delete a file")
	if err == nil || !strings.Contains(err.Error(), "too many destructive confirmations") {
		t.Fatalf("full confirmation queue error = %v", err)
	}
	if len(s.confirmations) != maxPendingConfirmations {
		t.Fatalf("pending confirmations = %d, want bounded at %d", len(s.confirmations), maxPendingConfirmations)
	}
}

func TestDestructiveConfirmationRejectsMalformedRequestsAndResponses(t *testing.T) {
	s := &server{}
	session := confirmationServerSession(t)
	for name, req := range map[string]*mcp.CallToolRequest{
		"missing session": {Params: &mcp.CallToolParamsRaw{}},
		"missing params":  {Session: session},
	} {
		if _, _, err := confirmDestructive(s, req, "delete a file"); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
	s.confirmationEntropy = strings.NewReader("")
	if _, _, err := confirmDestructive(s, confirmationRequest(session), "delete a file"); err == nil || !strings.Contains(err.Error(), noConfirmation) {
		t.Fatalf("unavailable entropy = %v, want fail-closed refusal", err)
	}
	s.confirmationEntropy = nil
	req := confirmationRequest(session)
	pending, _, err := confirmDestructive(s, req, "delete a file")
	if err != nil {
		t.Fatal(err)
	}
	req.Params.RequestState = pending.RequestState
	//lint:ignore SA1019 A different MCP response type exercises this defensive rejection path.
	req.Params.InputResponses = mcp.InputResponseMap{"confirmation": &mcp.CreateMessageResult{}}
	if _, confirmed, err := confirmDestructive(s, req, "delete a file"); err == nil || confirmed {
		t.Fatalf("malformed response = (%t, %v), want refusal", confirmed, err)
	}
	req = confirmationRequest(session)
	_, _, err = confirmDestructive(s, req, "delete a file")
	if err != nil {
		t.Fatal(err)
	}
	req.Params.InputResponses = mcp.InputResponseMap{"confirmation": (*mcp.ElicitResult)(nil)}
	if _, confirmed, err := confirmDestructive(s, req, "delete a file"); err == nil || confirmed {
		t.Fatalf("nil elicitation response = (%t, %v), want refusal", confirmed, err)
	}
	req = confirmationRequest(session)
	_, _, err = confirmDestructive(s, req, "delete a file")
	if err != nil {
		t.Fatal(err)
	}
	req.Params.InputResponses = mcp.InputResponseMap{"confirmation": &mcp.ElicitResult{Action: "accept", Content: map[string]any{"confirm": true}}}
	if _, confirmed, err := confirmDestructive(s, req, "delete a file"); err == nil || confirmed {
		t.Fatalf("response without request state = (%t, %v), want refusal", confirmed, err)
	}
}

type destructiveNode struct {
	nodepb.NodeServiceClient
	called  []string
	failing string
}

func (n *destructiveNode) result(name string) error {
	if n.failing == name {
		return errors.New("node failed")
	}
	return nil
}

func (n *destructiveNode) RemoveFile(_ context.Context, req *nodepb.RemoveFileRequest, _ ...grpc.CallOption) (*nodepb.RemoveFileResponse, error) {
	n.called = append(n.called, "RemoveFile:"+req.GetCid())
	return &nodepb.RemoveFileResponse{}, n.result("RemoveFile")
}

func (n *destructiveNode) RemoveMember(_ context.Context, req *nodepb.RemoveMemberRequest, _ ...grpc.CallOption) (*nodepb.RemoveMemberResponse, error) {
	n.called = append(n.called, "RemoveMember:"+req.GetPeerId())
	return &nodepb.RemoveMemberResponse{}, n.result("RemoveMember")
}

func (n *destructiveNode) DeleteChat(_ context.Context, req *nodepb.DeleteChatRequest, _ ...grpc.CallOption) (*nodepb.DeleteChatResponse, error) {
	n.called = append(n.called, "DeleteChat:"+req.GetChatId())
	return &nodepb.DeleteChatResponse{}, n.result("DeleteChat")
}

func (n *destructiveNode) RemoveModel(_ context.Context, req *nodepb.RemoveModelRequest, _ ...grpc.CallOption) (*nodepb.RemoveModelResponse, error) {
	n.called = append(n.called, "RemoveModel:"+req.GetModel())
	return &nodepb.RemoveModelResponse{}, n.result("RemoveModel")
}

func (n *destructiveNode) DeleteModelProfile(_ context.Context, req *nodepb.DeleteModelProfileRequest, _ ...grpc.CallOption) (*nodepb.DeleteModelProfileResponse, error) {
	n.called = append(n.called, "DeleteModelProfile:"+req.GetId())
	return &nodepb.DeleteModelProfileResponse{}, n.result("DeleteModelProfile")
}

func (n *destructiveNode) RemoveCityTable(context.Context, *nodepb.RemoveCityTableRequest, ...grpc.CallOption) (*nodepb.CityTableState, error) {
	n.called = append(n.called, "RemoveCityTable")
	return &nodepb.CityTableState{}, n.result("RemoveCityTable")
}

func (n *destructiveNode) LeavePool(context.Context, *nodepb.LeavePoolRequest, ...grpc.CallOption) (*nodepb.LeavePoolResponse, error) {
	n.called = append(n.called, "LeavePool")
	return &nodepb.LeavePoolResponse{}, n.result("LeavePool")
}

func destructiveAgent(t *testing.T, n nodepb.NodeServiceClient, action string, confirm any, supports bool) *mcp.ClientSession {
	t.Helper()
	ours, theirs := mcp.NewInMemoryTransports()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go New(Config{Node: n, Token: "token", Admin: true}).Run(ctx, theirs)
	var options *mcp.ClientOptions
	if supports {
		options = &mcp.ClientOptions{ElicitationHandler: func(_ context.Context, req *mcp.ElicitRequest) (*mcp.ElicitResult, error) {
			if !strings.Contains(req.Params.Message, "Confirm this destructive action:") {
				t.Errorf("unexpected confirmation prompt %q", req.Params.Message)
			}
			content, _ := confirm.(map[string]any)
			return &mcp.ElicitResult{Action: action, Content: content}, nil
		}}
	}
	session, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, options).Connect(ctx, ours, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { session.Close() })
	return session
}

func callDestructive(t *testing.T, session *mcp.ClientSession, name string, args map[string]any) (*mcp.CallToolResult, error) {
	t.Helper()
	return session.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
}

func TestDestructiveCallsRequireExplicitClientConfirmation(t *testing.T) {
	for _, tc := range []struct {
		name, action string
		confirm      any
		allow        bool
		clientError  bool
	}{
		{name: "accepted", action: "accept", confirm: map[string]any{"confirm": true}, allow: true},
		{name: "accept without confirmation field", action: "accept", confirm: map[string]any{}, allow: false, clientError: true},
		{name: "explicitly declined", action: "decline", allow: false},
		{name: "dismissed", action: "cancel", allow: false},
		{name: "accepted but false", action: "accept", confirm: map[string]any{"confirm": false}, allow: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			n := &destructiveNode{}
			session := destructiveAgent(t, n, tc.action, tc.confirm, true)
			result, err := callDestructive(t, session, "remove_file", map[string]any{"cid": "file-cid"})
			if err != nil && !tc.clientError {
				t.Fatal(err)
			}
			if err == nil && tc.clientError {
				t.Fatal("client accepted a response that did not match the requested schema")
			}
			if err != nil {
				if len(n.called) != 0 {
					t.Fatalf("invalid response reached node: %v", n.called)
				}
				return
			}
			if result.IsError == tc.allow {
				t.Fatalf("result error=%t, want error=%t: %s", result.IsError, !tc.allow, result.Content[0].(*mcp.TextContent).Text)
			}
			if tc.allow && strings.Join(n.called, ",") != "RemoveFile:file-cid" {
				t.Fatalf("accepted action calls %v", n.called)
			}
			if !tc.allow && len(n.called) != 0 {
				t.Fatalf("unconfirmed action reached node: %v", n.called)
			}
		})
	}
}

func TestDestructiveCallsAreRefusedWithoutElicitationSupport(t *testing.T) {
	n := &destructiveNode{}
	session := destructiveAgent(t, n, "", nil, false)
	result, err := callDestructive(t, session, "remove_file", map[string]any{"cid": "file-cid"})
	if err != nil {
		t.Fatal(err)
	}
	if !result.IsError || !strings.Contains(result.Content[0].(*mcp.TextContent).Text, noConfirmation) {
		t.Fatalf("without elicitation the action should be refused, got %#v", result)
	}
	if len(n.called) != 0 {
		t.Fatalf("unsupported client reached node: %v", n.called)
	}
}

func TestDestructiveRPCsCallOnlyAfterConfirmation(t *testing.T) {
	for _, tc := range []struct {
		name, action, want string
		args               map[string]any
	}{
		{name: "remove_file", action: "remove_file", want: "RemoveFile:file-cid", args: map[string]any{"cid": "file-cid"}},
		{name: "remove_member", action: "remove_member", want: "RemoveMember:peer-1", args: map[string]any{"peer_id": "peer-1"}},
		{name: "delete_chat", action: "delete_chat", want: "DeleteChat:chat-1", args: map[string]any{"chat_id": "chat-1"}},
		{name: "remove_model", action: "remove_model", want: "RemoveModel:model-1", args: map[string]any{"model": "model-1"}},
		{name: "delete_model_profile", action: "delete_model_profile", want: "DeleteModelProfile:profile-1", args: map[string]any{"id": "profile-1"}},
		{name: "remove_city_table", action: "remove_city_table", want: "RemoveCityTable"},
		{name: "leave_pool", action: "leave_pool", want: "LeavePool"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			n := &destructiveNode{}
			session := destructiveAgent(t, n, "accept", map[string]any{"confirm": true}, true)
			result, err := callDestructive(t, session, tc.name, tc.args)
			if err != nil || result.IsError {
				t.Fatalf("tool failed: result=%s err=%v", result.Content[0].(*mcp.TextContent).Text, err)
			}
			if strings.Join(n.called, ",") != tc.want {
				t.Fatalf("called %v, want %q", n.called, tc.want)
			}
		})
	}
}

func TestDestructiveNodeRPCFailuresAreReported(t *testing.T) {
	for _, tc := range []struct {
		name, rpc string
		args      map[string]any
	}{
		{name: "remove_file", rpc: "RemoveFile", args: map[string]any{"cid": "file-cid"}},
		{name: "remove_member", rpc: "RemoveMember", args: map[string]any{"peer_id": "peer-1"}},
		{name: "delete_chat", rpc: "DeleteChat", args: map[string]any{"chat_id": "chat-1"}},
		{name: "remove_model", rpc: "RemoveModel", args: map[string]any{"model": "model-1"}},
		{name: "delete_model_profile", rpc: "DeleteModelProfile", args: map[string]any{"id": "profile-1"}},
		{name: "remove_city_table", rpc: "RemoveCityTable"},
		{name: "leave_pool", rpc: "LeavePool"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			n := &destructiveNode{failing: tc.rpc}
			session := destructiveAgent(t, n, "accept", map[string]any{"confirm": true}, true)
			result, err := callDestructive(t, session, tc.name, tc.args)
			if err != nil || !result.IsError {
				t.Fatalf("failed RPC should be reported: result=%#v err=%v", result, err)
			}
			if len(n.called) != 1 {
				t.Fatalf("RPC calls = %v, want the one attempted call", n.called)
			}
		})
	}
}

type destructiveBlobs struct {
	pb.BlobServiceClient
	called  []string
	failing string
}

func (b *destructiveBlobs) Unpin(context.Context, *pb.UnpinBlobRequest, ...grpc.CallOption) (*pb.UnpinBlobResponse, error) {
	b.called = append(b.called, "Unpin")
	return &pb.UnpinBlobResponse{}, b.result("Unpin")
}

func (b *destructiveBlobs) CollectGarbage(context.Context, *pb.CollectGarbageRequest, ...grpc.CallOption) (*pb.CollectGarbageResponse, error) {
	b.called = append(b.called, "CollectGarbage")
	return &pb.CollectGarbageResponse{}, b.result("CollectGarbage")
}

func (b *destructiveBlobs) result(name string) error {
	if b.failing == name {
		return errors.New("blob service failed")
	}
	return nil
}

func TestDestructiveStorageToolsAlsoRequireConfirmation(t *testing.T) {
	for _, tc := range []struct {
		name, want string
		args       map[string]any
	}{
		{name: "unpin_file", want: "Unpin", args: map[string]any{"cid": "file-cid"}},
		{name: "collect_garbage", want: "CollectGarbage"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			blobs := &destructiveBlobs{}
			n := &destructiveNode{}
			session := destructiveAgentWithMain(t, n, blobs, "accept", map[string]any{"confirm": true}, true)
			result, err := callDestructive(t, session, tc.name, tc.args)
			if err != nil || result.IsError {
				t.Fatalf("tool failed: result=%s err=%v", result.Content[0].(*mcp.TextContent).Text, err)
			}
			if strings.Join(blobs.called, ",") != tc.want {
				t.Fatalf("called %v, want %q", blobs.called, tc.want)
			}
		})
	}
}

func TestDestructiveStorageFailuresAndMissingMainAreReported(t *testing.T) {
	for _, tc := range []struct {
		name, failing string
		args          map[string]any
	}{
		{name: "unpin_file", failing: "Unpin", args: map[string]any{"cid": "file-cid"}},
		{name: "collect_garbage", failing: "CollectGarbage"},
	} {
		t.Run(tc.name+" failure", func(t *testing.T) {
			blobs := &destructiveBlobs{failing: tc.failing}
			n := &destructiveNode{}
			session := destructiveAgentWithMain(t, n, blobs, "accept", map[string]any{"confirm": true}, true)
			result, err := callDestructive(t, session, tc.name, tc.args)
			if err != nil || !result.IsError {
				t.Fatalf("failed blob RPC should be reported: result=%#v err=%v", result, err)
			}
		})
	}
	t.Run("main address unavailable", func(t *testing.T) {
		n := &destructiveNode{}
		session := destructiveAgentWithMain(t, n, &destructiveBlobs{}, "accept", map[string]any{"confirm": true}, true, errors.New("main unavailable"))
		result, err := callDestructive(t, session, "unpin_file", map[string]any{"cid": "file-cid"})
		if err != nil || !result.IsError {
			t.Fatalf("missing main connection should be reported: result=%#v err=%v", result, err)
		}
	})
	t.Run("confirmation unavailable", func(t *testing.T) {
		n := &destructiveNode{}
		session := destructiveAgentWithMain(t, n, &destructiveBlobs{}, "", nil, false)
		result, err := callDestructive(t, session, "unpin_file", map[string]any{"cid": "file-cid"})
		if err != nil || !result.IsError || len(n.called) != 0 {
			t.Fatalf("unconfirmed storage operation = (%#v, %v), node calls=%v", result, err, n.called)
		}
	})
}

func destructiveAgentWithMain(t *testing.T, n nodepb.NodeServiceClient, blobs pb.BlobServiceClient, action string, confirm any, supports bool, mainErr ...error) *mcp.ClientSession {
	t.Helper()
	ours, theirs := mcp.NewInMemoryTransports()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go New(Config{Node: n, Token: "token", Admin: true, Main: func() (*Main, error) {
		if len(mainErr) > 0 && mainErr[0] != nil {
			return nil, mainErr[0]
		}
		return &Main{Blobs: blobs}, nil
	}}).Run(ctx, theirs)
	var options *mcp.ClientOptions
	if supports {
		options = &mcp.ClientOptions{ElicitationHandler: func(_ context.Context, _ *mcp.ElicitRequest) (*mcp.ElicitResult, error) {
			content, _ := confirm.(map[string]any)
			return &mcp.ElicitResult{Action: action, Content: content}, nil
		}}
	}
	session, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, options).Connect(ctx, ours, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { session.Close() })
	return session
}

package api

import (
	"context"
	"errors"
	"net"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"

	nodepb "github.com/sisyphus-network/Sisyphus/packages/protocol/sisyphus/node/v1"
)

func TestPendingActionConfirmationTravelsThroughLocalAPI(t *testing.T) {
	n := startLocal(t, false)
	ctx, cancel := context.WithCancel(withToken("the-token"))
	defer cancel()
	stream, err := n.client.WatchPendingActionConfirmations(ctx, &nodepb.WatchPendingActionConfirmationsRequest{})
	if err != nil {
		t.Fatal(err)
	}
	defer stream.CloseSend()
	initial, err := stream.Recv()
	if err != nil || len(initial.GetActions()) != 0 {
		t.Fatalf("initial state = %#v, %v; want no pending actions", initial, err)
	}

	result := make(chan *nodepb.RequestActionConfirmationResponse, 1)
	requestErr := make(chan error, 1)
	go func() {
		response, err := n.client.RequestActionConfirmation(withToken("the-token"), &nodepb.RequestActionConfirmationRequest{Action: "remove saved profile"})
		result <- response
		requestErr <- err
	}()
	created, err := stream.Recv()
	if err != nil || len(created.GetActions()) != 1 || created.GetActions()[0].GetAction() != "remove saved profile" {
		t.Fatalf("pending state = %#v, %v", created, err)
	}
	if created.GetRevision() <= initial.GetRevision() || created.GetActions()[0].GetId() == "" || created.GetActions()[0].GetExpiresAtUnixMillis() <= created.GetActions()[0].GetRequestedAtUnixMillis() {
		t.Fatalf("pending action metadata is invalid: %#v", created)
	}
	if _, err := n.client.ResolveActionConfirmation(withToken("the-token"), &nodepb.ResolveActionConfirmationRequest{Id: created.GetActions()[0].GetId(), Approved: true}); err != nil {
		t.Fatal(err)
	}
	response := <-result
	if err := <-requestErr; err != nil || !response.GetApproved() {
		t.Fatalf("request result = %#v, %v; want approved", response, err)
	}
	removed, err := stream.Recv()
	if err != nil || len(removed.GetActions()) != 0 || removed.GetRevision() <= created.GetRevision() {
		t.Fatalf("resolved state = %#v, %v; want a newer empty state", removed, err)
	}
	if _, err := n.client.ResolveActionConfirmation(withToken("the-token"), &nodepb.ResolveActionConfirmationRequest{Id: created.GetActions()[0].GetId(), Approved: true}); status.Code(err) != codes.NotFound {
		t.Fatalf("second resolution error = %v, want not found", err)
	}
}

func TestPendingActionConfirmationRejectsAndExpires(t *testing.T) {
	manager := newPendingActionManager()
	manager.lifetime = 5 * time.Millisecond
	decision, err := manager.request(context.Background(), "delete a file")
	if err != nil || decision {
		t.Fatalf("expired request = %t, %v; want an unapproved decision", decision, err)
	}
	if state := manager.snapshot(); len(state.GetActions()) != 0 || state.GetRevision() != 2 {
		t.Fatalf("expired state = %#v; want an empty state after create and expiry", state)
	}

	manager.lifetime = time.Minute
	decisionCh := make(chan bool, 1)
	errCh := make(chan error, 1)
	go func() {
		decision, err := manager.request(context.Background(), "leave the pool")
		decisionCh <- decision
		errCh <- err
	}()
	var action *nodepb.PendingActionConfirmation
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if actions := manager.snapshot().GetActions(); len(actions) == 1 {
			action = actions[0]
			break
		}
		time.Sleep(time.Millisecond)
	}
	if action == nil {
		t.Fatal("action was not added to the pending queue")
	}
	manager.mu.Lock()
	resolved := manager.pending[action.GetId()]
	manager.mu.Unlock()
	if err := manager.resolve(action.GetId(), false); err != nil {
		t.Fatal(err)
	}
	if approved, err := <-decisionCh, <-errCh; err != nil || approved {
		t.Fatalf("rejected request = %t, %v; want denied", approved, err)
	}
	if manager.expire(resolved) {
		t.Fatal("already-resolved action expired a second time")
	}
	approved := &pendingAction{id: "approved", approved: true}
	if !manager.expire(approved) {
		t.Fatal("a prior approval was not preserved when the timer observed it")
	}
}

func TestPendingActionManagerValidationCapacityAndCancellation(t *testing.T) {
	manager := newPendingActionManager()
	for _, action := range []string{"", strings.Repeat("x", maxPendingActionText+1), "two\nlines", "tab\tcharacter", "nul\x00byte", "bidi\u202Eoverride"} {
		if _, err := manager.request(context.Background(), action); status.Code(err) != codes.InvalidArgument {
			t.Fatalf("request(%q) error = %v; want invalid argument", action, err)
		}
	}
	broken := newPendingActionManager()
	broken.random = strings.NewReader("")
	if _, err := broken.request(context.Background(), "valid action"); status.Code(err) != codes.Internal {
		t.Fatalf("random source error = %v; want internal", err)
	}

	full := newPendingActionManager()
	for i := range maxPendingActions {
		id := strings.Repeat("a", i+1)
		full.pending[id] = &pendingAction{id: id, action: "pending", expiresAt: time.Now().Add(time.Minute), decision: make(chan bool, 1)}
	}
	if _, err := full.request(context.Background(), "one more"); status.Code(err) != codes.ResourceExhausted {
		t.Fatalf("full queue error = %v; want resource exhausted", err)
	}

	cancelled, cancel := context.WithCancel(context.Background())
	finished := make(chan error, 1)
	go func() {
		_, err := manager.request(cancelled, "cancel this request")
		finished <- err
	}()
	deadline := time.Now().Add(time.Second)
	for len(manager.snapshot().GetActions()) == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	cancel()
	if err := <-finished; status.Code(err) != codes.Canceled {
		t.Fatalf("cancelled request error = %v; want canceled", err)
	}
	if len(manager.snapshot().GetActions()) != 0 {
		t.Fatal("cancelled action remained visible")
	}
	if manager.cancel(&pendingAction{id: "already-removed"}); len(manager.snapshot().GetActions()) != 0 {
		t.Fatal("cancelling a missing action changed the queue")
	}
}

func TestPendingActionManagerResolutionErrorsAndSubscriptions(t *testing.T) {
	manager := newPendingActionManager()
	changes, unsubscribe := manager.subscribe()
	manager.mu.Lock()
	manager.changedLocked()
	manager.changedLocked()
	manager.mu.Unlock()
	if len(changes) != 1 {
		t.Fatalf("change notifications queued %d signals; want coalesced to one", len(changes))
	}
	if err := manager.resolve("missing", true); status.Code(err) != codes.NotFound {
		t.Fatalf("missing action error = %v; want not found", err)
	}
	unsubscribe()
	unsubscribe()
	if len(manager.subscribers) != 0 {
		t.Fatal("unsubscribe left a subscriber behind")
	}

	item := &pendingAction{id: "expired", expiresAt: time.Now().Add(-time.Second), decision: make(chan bool, 1)}
	manager.pending[item.id] = item
	if err := manager.resolve(item.id, true); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("expired action error = %v; want failed precondition", err)
	}
	if len(manager.snapshot().GetActions()) != 0 {
		t.Fatal("expired action remained visible")
	}
	requested := time.Now()
	left := &pendingAction{id: "left", action: "older", requestedAt: requested.Add(-time.Second)}
	equalFirst := &pendingAction{id: "a", action: "same time A", requestedAt: requested}
	equalSecond := &pendingAction{id: "b", action: "same time B", requestedAt: requested}
	if !pendingActionLess(left, equalFirst) || pendingActionLess(equalFirst, left) || !pendingActionLess(equalFirst, equalSecond) || pendingActionLess(equalSecond, equalFirst) {
		t.Fatal("pending action ordering did not sort by request time and then ID")
	}
	manager.pending = map[string]*pendingAction{equalSecond.id: equalSecond, left.id: left, equalFirst.id: equalFirst}
	ordered := manager.snapshot().GetActions()
	if len(ordered) != 3 || ordered[0].GetId() != left.id || ordered[1].GetId() != equalFirst.id || ordered[2].GetId() != equalSecond.id {
		t.Fatalf("snapshot order = %#v", ordered)
	}
}

func TestPendingActionConfirmationRequiresTokenAndLoopback(t *testing.T) {
	service := &localService{cfg: LocalConfig{Token: "the-token"}}
	if _, err := service.RequestActionConfirmation(incomingToken("wrong-token"), &nodepb.RequestActionConfirmationRequest{Action: "remove a file"}); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("wrong token error = %v; want permission denied", err)
	}
	if _, err := service.RequestActionConfirmation(incomingToken("the-token"), &nodepb.RequestActionConfirmationRequest{Action: "remove a file"}); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("unauthorized request error = %v; want permission denied", err)
	}
	local := peer.NewContext(incomingToken("the-token"), &peer.Peer{Addr: &net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 1234}})
	if _, err := service.RequestActionConfirmation(local, nil); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("nil request error = %v; want invalid argument", err)
	}
	if _, err := service.RequestActionConfirmation(local, &nodepb.RequestActionConfirmationRequest{}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("empty action error = %v; want invalid argument", err)
	}
	unauthorized := peer.NewContext(incomingToken("the-token"), &peer.Peer{Addr: &net.TCPAddr{IP: net.ParseIP("192.0.2.8"), Port: 1234}})
	if _, err := service.RequestActionConfirmation(unauthorized, &nodepb.RequestActionConfirmationRequest{Action: "remove a file"}); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("remote request error = %v; want permission denied", err)
	}
	if err := requireLoopbackOwner(context.Background()); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("missing peer error = %v; want permission denied", err)
	}
	local = peer.NewContext(context.Background(), &peer.Peer{Addr: &net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 1234}})
	if err := requireLoopbackOwner(local); err != nil {
		t.Fatalf("loopback peer rejected: %v", err)
	}
	if err := requireLoopbackOwner(peer.NewContext(context.Background(), &peer.Peer{Addr: testAddress("not an IP")})); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("invalid peer address error = %v; want permission denied", err)
	}
}

func TestPendingActionStreamAuthorizationAndCanceledStream(t *testing.T) {
	service := &localService{cfg: LocalConfig{Token: "the-token"}}
	stream := &pendingActionTestStream{ctx: withToken("wrong-token")}
	if err := service.WatchPendingActionConfirmations(nil, stream); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("unauthorized stream error = %v; want permission denied", err)
	}
	remote := peer.NewContext(metadata.NewIncomingContext(context.Background(), metadata.Pairs("authorization", "Bearer the-token")), &peer.Peer{Addr: &net.TCPAddr{IP: net.ParseIP("192.0.2.8"), Port: 1234}})
	if err := service.WatchPendingActionConfirmations(nil, &pendingActionTestStream{ctx: remote}); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("remote stream error = %v; want permission denied", err)
	}
	ctx, cancel := context.WithCancel(peer.NewContext(incomingToken("the-token"), &peer.Peer{Addr: &net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 1234}}))
	cancel()
	stream = &pendingActionTestStream{ctx: ctx}
	if err := service.WatchPendingActionConfirmations(nil, stream); err != nil {
		t.Fatalf("canceled stream error = %v", err)
	}
	if len(stream.sent) != 1 {
		t.Fatalf("canceled stream sent %d initial snapshots, want one", len(stream.sent))
	}
	if _, err := service.ResolveActionConfirmation(incomingToken("wrong-token"), &nodepb.ResolveActionConfirmationRequest{Id: "missing"}); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("unauthorized resolution error = %v; want permission denied", err)
	}
	if _, err := service.ResolveActionConfirmation(peer.NewContext(incomingToken("the-token"), &peer.Peer{Addr: &net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 1234}}), nil); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("nil resolution error = %v; want invalid argument", err)
	}
	if _, err := service.ResolveActionConfirmation(incomingToken("the-token"), &nodepb.ResolveActionConfirmationRequest{Id: "missing"}); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("resolution without loopback peer error = %v; want permission denied", err)
	}
	local := peer.NewContext(incomingToken("the-token"), &peer.Peer{Addr: &net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 1234}})
	if err := service.WatchPendingActionConfirmations(nil, &pendingActionTestStream{ctx: local, err: errors.New("send failed"), errorAt: 1}); err == nil || err.Error() != "send failed" {
		t.Fatalf("initial stream send error = %v; want send failure", err)
	}
	if err := service.WatchPendingActionConfirmations(nil, &pendingActionTestStream{ctx: local, afterSend: func(sent int) {
		if sent != 1 {
			return
		}
		manager := service.actionConfirmations()
		manager.mu.Lock()
		for _, changes := range manager.subscribers {
			changes <- struct{}{} // An unchanged notification is ignored by the stream.
		}
		manager.mu.Unlock()
		time.AfterFunc(5*time.Millisecond, func() {
			manager.mu.Lock()
			item := &pendingAction{id: "streamed", action: "stream update", requestedAt: time.Now(), expiresAt: time.Now().Add(time.Minute)}
			manager.pending[item.id] = item
			manager.changedLocked()
			manager.mu.Unlock()
		})
	}, err: errors.New("update send failed"), errorAt: 2}); err == nil || err.Error() != "update send failed" {
		t.Fatalf("update stream send error = %v; want send failure", err)
	}
}

func incomingToken(token string) context.Context {
	return metadata.NewIncomingContext(context.Background(), metadata.Pairs("authorization", "Bearer "+token))
}

type pendingActionTestStream struct {
	grpc.ServerStreamingServer[nodepb.PendingActionConfirmationsState]
	ctx       context.Context
	sent      []*nodepb.PendingActionConfirmationsState
	err       error
	errorAt   int
	afterSend func(int)
}

func (s *pendingActionTestStream) Context() context.Context { return s.ctx }

func (s *pendingActionTestStream) Send(state *nodepb.PendingActionConfirmationsState) error {
	s.sent = append(s.sent, state)
	if s.afterSend != nil {
		s.afterSend(len(s.sent))
	}
	if s.errorAt == len(s.sent) {
		return s.err
	}
	return nil
}

type fixedAddress string

func (a fixedAddress) Network() string { return "tcp" }
func (a fixedAddress) String() string  { return string(a) }

func testAddress(address string) net.Addr { return fixedAddress(address) }

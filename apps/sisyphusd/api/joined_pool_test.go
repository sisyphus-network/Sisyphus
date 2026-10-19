package api

import (
	"context"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	nodepb "github.com/sisyphus-network/Sisyphus/packages/protocol/sisyphus/node/v1"
)

// joinedPoolStream is a WatchJoinedPool stream that keeps what it is sent,
// and fails a send when told to.
type joinedPoolStream struct {
	ctx  context.Context
	sent chan *nodepb.JoinedPool
	fail error
}

func (s *joinedPoolStream) Send(pool *nodepb.JoinedPool) error {
	if s.fail != nil {
		return s.fail
	}
	s.sent <- pool
	return nil
}
func (s *joinedPoolStream) Context() context.Context   { return s.ctx }
func (*joinedPoolStream) SetHeader(metadata.MD) error  { return nil }
func (*joinedPoolStream) SendHeader(metadata.MD) error { return nil }
func (*joinedPoolStream) SetTrailer(metadata.MD)       {}
func (*joinedPoolStream) SendMsg(any) error            { return nil }
func (*joinedPoolStream) RecvMsg(any) error            { return nil }

// A node that coordinates its own pool has joined none.
func TestANodeOfItsOwnPoolHasJoinedNone(t *testing.T) {
	s := &localService{}
	if _, err := s.GetJoinedPool(context.Background(), &nodepb.GetJoinedPoolRequest{}); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("GetJoinedPool on a coordinating node: %v", err)
	}
	if err := s.WatchJoinedPool(&nodepb.GetJoinedPoolRequest{}, &joinedPoolStream{ctx: context.Background()}); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("WatchJoinedPool on a coordinating node: %v", err)
	}
}

// The watch sends the pool at once, and again only when it has changed,
// and ends with its stream.
func TestTheJoinedPoolIsSentAsItStandsAndOnEachChange(t *testing.T) {
	state := make(chan nodepb.JoinedPoolState, 1)
	state <- nodepb.JoinedPoolState_JOINED_POOL_STATE_CONNECTING
	current := nodepb.JoinedPoolState_JOINED_POOL_STATE_CONNECTING
	changes := NewNodeInfoChanges()
	s := &localService{cfg: LocalConfig{
		JoinedPool: func() *nodepb.JoinedPool {
			select {
			case current = <-state:
			default:
			}
			return &nodepb.JoinedPool{Address: "rig:7700", State: current}
		},
		JoinedPoolChanges: changes,
	}}
	if got, err := s.GetJoinedPool(context.Background(), &nodepb.GetJoinedPoolRequest{}); err != nil || got.GetAddress() != "rig:7700" {
		t.Fatalf("GetJoinedPool = %v, %v", got, err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	stream := &joinedPoolStream{ctx: ctx, sent: make(chan *nodepb.JoinedPool, 4)}
	done := make(chan error, 1)
	go func() { done <- s.WatchJoinedPool(&nodepb.GetJoinedPoolRequest{}, stream) }()
	if first := <-stream.sent; first.GetState() != nodepb.JoinedPoolState_JOINED_POOL_STATE_CONNECTING {
		t.Errorf("first sent %v", first)
	}
	waitSubscribed(t, changes)
	// Word of a change that changes nothing sends nothing; a real one does.
	changes.Notify()
	state <- nodepb.JoinedPoolState_JOINED_POOL_STATE_CONNECTED
	changes.Notify()
	if next := <-stream.sent; next.GetState() != nodepb.JoinedPoolState_JOINED_POOL_STATE_CONNECTED {
		t.Errorf("after connecting, sent %v", next)
	}
	cancel()
	if err := <-done; status.Code(err) != codes.Canceled && err != context.Canceled {
		t.Errorf("the watch ended with %v", err)
	}
	select {
	case extra := <-stream.sent:
		t.Errorf("an unchanged pool was sent again: %v", extra)
	default:
	}

	// A send that fails ends the watch with its error.
	stream = &joinedPoolStream{ctx: context.Background(), fail: context.DeadlineExceeded}
	if err := s.WatchJoinedPool(&nodepb.GetJoinedPoolRequest{}, stream); err != context.DeadlineExceeded {
		t.Errorf("a failed send ended the watch with %v", err)
	}
}

// waitSubscribed waits for a watcher to subscribe to changes.
func waitSubscribed(t *testing.T, changes *NodeInfoChanges) {
	t.Helper()
	for {
		changes.mu.Lock()
		n := len(changes.subscribers)
		changes.mu.Unlock()
		if n > 0 {
			return
		}
		time.Sleep(time.Millisecond)
	}
}

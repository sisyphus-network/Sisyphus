package api

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"io"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	nodepb "github.com/sisyphus-network/Sisyphus/packages/protocol/sisyphus/node/v1"
)

const (
	pendingActionLifetime = 5 * time.Minute
	maxPendingActions     = 16
	maxPendingActionText  = 512
)

type pendingAction struct {
	id          string
	action      string
	requestedAt time.Time
	expiresAt   time.Time
	decision    chan bool
	approved    bool
}

// pendingActionManager holds one-time decisions requested by agents and
// broadcasts snapshots to authenticated owner clients without polling.
type pendingActionManager struct {
	mu          sync.Mutex
	pending     map[string]*pendingAction
	subscribers map[uint64]chan struct{}
	nextSub     uint64
	revision    uint64
	random      io.Reader
	lifetime    time.Duration
}

func newPendingActionManager() *pendingActionManager {
	return &pendingActionManager{
		pending: make(map[string]*pendingAction), subscribers: make(map[uint64]chan struct{}),
		random: rand.Reader, lifetime: pendingActionLifetime,
	}
}

func (m *pendingActionManager) request(ctx context.Context, action string) (bool, error) {
	if action == "" || len(action) > maxPendingActionText || strings.IndexFunc(action, func(r rune) bool {
		return unicode.IsControl(r) || unicode.Is(unicode.Cf, r)
	}) >= 0 {
		return false, status.Error(codes.InvalidArgument, "the action must be a short, single-line description")
	}
	nonce := make([]byte, 18)
	if _, err := io.ReadFull(m.random, nonce); err != nil {
		return false, status.Error(codes.Internal, "could not create a pending action")
	}
	now := time.Now()
	item := &pendingAction{
		id: base64.RawURLEncoding.EncodeToString(nonce), action: action,
		requestedAt: now, expiresAt: now.Add(m.lifetime), decision: make(chan bool, 1),
	}
	m.mu.Lock()
	if len(m.subscribers) == 0 {
		m.mu.Unlock()
		return false, status.Error(codes.FailedPrecondition, "open the Sisyphus app to approve this action; no change was made")
	}
	if len(m.pending) >= maxPendingActions {
		m.mu.Unlock()
		return false, status.Error(codes.ResourceExhausted, "too many actions are already waiting for approval")
	}
	m.pending[item.id] = item
	m.changedLocked()
	m.mu.Unlock()

	timer := time.NewTimer(time.Until(item.expiresAt))
	defer timer.Stop()
	select {
	case approved := <-item.decision:
		return approved, nil
	case <-ctx.Done():
		m.cancel(item)
		return false, status.FromContextError(ctx.Err()).Err()
	case <-timer.C:
		return m.expire(item), nil
	}
}

func (m *pendingActionManager) resolve(id string, approved bool) error {
	m.mu.Lock()
	item, ok := m.pending[id]
	if !ok {
		m.mu.Unlock()
		return status.Error(codes.NotFound, "the pending action no longer exists")
	}
	if !item.expiresAt.After(time.Now()) {
		delete(m.pending, id)
		m.changedLocked()
		m.mu.Unlock()
		return status.Error(codes.FailedPrecondition, "the pending action has expired")
	}
	delete(m.pending, id)
	item.approved = approved
	item.decision <- approved
	m.changedLocked()
	m.mu.Unlock()
	return nil
}

func (m *pendingActionManager) expire(item *pendingAction) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.pending[item.id] != item {
		return item.approved
	}
	delete(m.pending, item.id)
	m.changedLocked()
	return false
}

func (m *pendingActionManager) cancel(item *pendingAction) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.pending[item.id] != item {
		return
	}
	delete(m.pending, item.id)
	m.changedLocked()
}

func (m *pendingActionManager) subscribe() (<-chan struct{}, func()) {
	m.mu.Lock()
	m.nextSub++
	id := m.nextSub
	changes := make(chan struct{}, 1)
	m.subscribers[id] = changes
	m.mu.Unlock()
	return changes, func() {
		m.mu.Lock()
		delete(m.subscribers, id)
		m.mu.Unlock()
	}
}

func (m *pendingActionManager) snapshot() *nodepb.PendingActionConfirmationsState {
	m.mu.Lock()
	defer m.mu.Unlock()
	items := make([]*pendingAction, 0, len(m.pending))
	for _, item := range m.pending {
		items = append(items, item)
	}
	sort.Slice(items, func(i, j int) bool { return pendingActionLess(items[i], items[j]) })
	state := &nodepb.PendingActionConfirmationsState{Revision: m.revision}
	for _, item := range items {
		state.Actions = append(state.Actions, &nodepb.PendingActionConfirmation{
			Id: item.id, Action: item.action,
			RequestedAtUnixMillis: item.requestedAt.UnixMilli(), ExpiresAtUnixMillis: item.expiresAt.UnixMilli(),
		})
	}
	return state
}

func pendingActionLess(left, right *pendingAction) bool {
	if left.requestedAt.Equal(right.requestedAt) {
		return left.id < right.id
	}
	return left.requestedAt.Before(right.requestedAt)
}

func (m *pendingActionManager) changedLocked() {
	m.revision++
	for _, changes := range m.subscribers {
		select {
		case changes <- struct{}{}:
		default:
		}
	}
}

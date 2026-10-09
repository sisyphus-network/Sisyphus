// Package replication keeps copies of a pool's stored data on more than one
// node, so that the loss of any one of them loses none of it.
//
// What is kept is decided where it always was: by the pins of the node that
// coordinates the pool. Other members, its storage followers, each keep a
// durable store of their own and hold in it the blobs the coordinator tells
// them to. A follower asks for its list every so often, fetches what it
// lacks, drops what is no longer listed, and says what it holds. It decides
// nothing.
//
// The coordinator's side is a Manager and a follower's a Follower.
package replication

import (
	"context"
	"crypto/sha256"
	"errors"
	"log/slog"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/ipfs/go-cid"

	"github.com/sisyphus-network/Sisyphus/apps/sisyphusd/blobclient"
	pb "github.com/sisyphus-network/Sisyphus/packages/protocol/sisyphus/v1"
	"github.com/sisyphus-network/Sisyphus/packages/storage"
)

var (
	// Interval is how often a follower asks its coordinator what to hold.
	Interval = time.Minute
	// Grace is how long a coordinator goes on counting on a follower it has
	// stopped hearing from. After that the follower's share is given to
	// others. A coordinator that has just started waits as long before it
	// hands the blobs it already had to followers that lack them, so that
	// those which held copies before it stopped can say so first.
	Grace = 10 * time.Minute
)

// Store is the part of a coordinator's store that replication uses: its
// pins are what is copied, and what is fetched back goes into it.
type Store interface {
	Pins() []storage.Pin
	Pin(ctx context.Context, owner string, expires time.Time, cids ...cid.Cid) error
	blobclient.Putter
}

type Config struct {
	// ID is the coordinator's node ID.
	ID string
	// Store is the coordinator's store, and StoreID a name for it that is
	// lost if its pins are: a coordinator found with a store of another
	// name has lost the one its followers hold copies of.
	Store   Store
	StoreID string
	// Replicas is how many followers should hold each pinned blob. Zero
	// asks none to.
	Replicas int
	// Address returns where a node serves blobs to the pool's members, or
	// "" if it is not connected or serves none.
	Address func(nodeID string) string
	// Fetch copies a blob out of a member's store.
	Fetch func(ctx context.Context, from *pb.BlobHolder, c cid.Cid, into blobclient.Putter) error
	Log   *slog.Logger
}

// Manager is the coordinator's side of replication. It hears from the
// followers, tells each what to hold, and knows what each says it holds.
type Manager struct {
	store    Store
	storeID  string
	earlier  string // what the name of any earlier store of this node begins with
	replicas int
	address  func(nodeID string) string
	fetch    func(ctx context.Context, from *pb.BlobHolder, c cid.Cid, into blobclient.Putter) error
	log      *slog.Logger
	grace    time.Duration

	// atStart holds the blobs that were pinned when the manager started,
	// and settled is when it stops waiting for followers that may already
	// hold them.
	atStart map[string]struct{}
	settled time.Time

	mu        sync.Mutex
	followers map[string]*follower
}

// follower is what a coordinator knows of one follower.
type follower struct {
	// asked is when it last asked what to hold.
	asked time.Time
	// held is what it then said it holds.
	held map[string]struct{}
	// stranded lists what it holds from an earlier store of this node that
	// the present store has not pinned.
	stranded []string
}

// New returns a manager for a coordinator that has just started.
func New(cfg Config) *Manager {
	m := &Manager{
		store:     cfg.Store,
		storeID:   cfg.ID + "/" + cfg.StoreID,
		earlier:   cfg.ID + "/",
		replicas:  cfg.Replicas,
		address:   cfg.Address,
		fetch:     cfg.Fetch,
		log:       cfg.Log,
		grace:     Grace,
		followers: make(map[string]*follower),
	}
	now := time.Now()
	m.atStart, _ = m.pinned(now)
	m.settled = now.Add(m.grace)
	return m
}

// Off returns the manager of a node that asks nothing of followers.
func Off(store Store) *Manager {
	return New(Config{Store: store, Address: func(string) string { return "" }, Log: slog.New(slog.DiscardHandler)})
}

// pinned returns the blobs the store is keeping as of now, as a set and in
// order. The short pin every new blob gets does not count: a blob is copied
// once somebody has asked for it to be kept.
func (m *Manager) pinned(now time.Time) (set map[string]struct{}, order []string) {
	set = make(map[string]struct{})
	for _, pin := range m.store.Pins() {
		if pin.Owner == storage.GraceOwner || (!pin.Expires.IsZero() && pin.Expires.Before(now)) {
			continue
		}
		// Followers hold files. What else a node pins, such as the record
		// of a job, which is linked data and not a file, is not something
		// they can fetch as one, and stays with the coordinator.
		if kind := pin.CID.Type(); kind != cid.Raw && kind != cid.DagProtobuf {
			continue
		}
		id := pin.CID.String()
		if _, listed := set[id]; !listed {
			set[id] = struct{}{}
			order = append(order, id)
		}
	}
	return set, order
}

// liveLocked returns the followers heard from within the grace period, in
// order of ID, and forgets the rest.
func (m *Manager) liveLocked(now time.Time) []string {
	var live []string
	for id, f := range m.followers {
		if now.Sub(f.asked) > m.grace {
			delete(m.followers, id)
			m.log.Warn("a storage follower has not been heard from; others are to hold its share", "node", id, "for", m.grace.String())
			continue
		}
		live = append(live, id)
	}
	sort.Strings(live)
	return live
}

// chosen picks which of the followers are to hold a blob: those that score
// highest when their ID is hashed with the blob's. Each blob thereby ranks
// the followers in an order of its own, and a follower joining or leaving
// moves only the blobs it gains or gives up.
func (m *Manager) chosen(blob string, followers []string) []string {
	if len(followers) <= m.replicas {
		return followers
	}
	scores := make(map[string][sha256.Size]byte, len(followers))
	for _, id := range followers {
		scores[id] = sha256.Sum256([]byte(id + "\x00" + blob))
	}
	ranked := slices.Clone(followers)
	sort.Slice(ranked, func(a, b int) bool {
		x, y := scores[ranked[a]], scores[ranked[b]]
		return string(x[:]) > string(y[:])
	})
	return ranked[:m.replicas]
}

// Replicate takes a follower's word for what it holds, and for which store,
// and returns what it should hold from now on and the store to name next
// time.
func (m *Manager) Replicate(caller, store string, holding []string) (hold []string, storeID string) {
	now := time.Now()
	pinned, order := m.pinned(now)

	m.mu.Lock()
	defer m.mu.Unlock()
	before, known := m.followers[caller]
	if !known {
		m.log.Info("a storage follower is here", "node", caller, "holding", len(holding))
		before = &follower{}
	}
	f := &follower{asked: now, held: make(map[string]struct{}, len(holding))}
	for _, id := range holding {
		f.held[id] = struct{}{}
	}
	m.followers[caller] = f
	live := m.liveLocked(now)

	for _, blob := range order {
		chosen := m.chosen(blob, live)
		mine := slices.Contains(chosen, caller)
		_, has := f.held[blob]
		// A blob this node had when it started may be on followers it has
		// yet to hear from. A node that wants no copies waits for nobody.
		_, old := m.atStart[blob]
		waiting := old && m.replicas > 0 && now.Before(m.settled)
		// A follower keeps a copy that is to move elsewhere until those
		// that are to hold it say they do, so that a change of followers
		// never leaves fewer copies than there were.
		if (mine && !waiting) || (has && (mine || waiting || !m.allHoldLocked(chosen, blob))) {
			hold = append(hold, blob)
		}
	}

	// What a follower holds that is not pinned has been released, and it
	// drops it, unless it holds it for a store this node had before: then
	// this node has lost its pins, and that copy may be the only one left.
	if store != m.storeID && strings.HasPrefix(store, m.earlier) {
		for _, id := range holding {
			if _, kept := pinned[id]; !kept {
				f.stranded = append(f.stranded, id)
			}
		}
		sort.Strings(f.stranded)
	}
	if len(f.stranded) == 0 {
		return hold, m.storeID
	}
	if len(before.stranded) == 0 {
		m.log.Warn("a storage follower holds blobs from a store this node had before and no longer has pinned; it keeps them until they are restored", "node", caller, "blobs", len(f.stranded))
	}
	return append(hold, f.stranded...), store
}

// allHoldLocked reports whether every one of some followers says it holds a
// blob.
func (m *Manager) allHoldLocked(followers []string, blob string) bool {
	for _, id := range followers {
		if _, has := m.followers[id].held[blob]; !has {
			return false
		}
	}
	return true
}

// Status reports which followers hold each pinned blob, or only the one
// named.
func (m *Manager) Status(only string) *pb.ReplicasResponse {
	now := time.Now()
	_, order := m.pinned(now)

	m.mu.Lock()
	defer m.mu.Unlock()
	live := m.liveLocked(now)
	res := &pb.ReplicasResponse{
		Wanted: uint32(m.replicas), Followers: live,
		Settling: len(m.atStart) > 0 && now.Before(m.settled),
	}
	res.FromEarlierStore = uint32(len(m.strandedLocked(live)))
	for _, blob := range order {
		if only != "" && blob != only {
			continue
		}
		listed := &pb.ReplicatedBlob{Cid: blob}
		for _, id := range live {
			if _, has := m.followers[id].held[blob]; has {
				listed.Holders = append(listed.Holders, id)
			}
		}
		res.Blobs = append(res.Blobs, listed)
	}
	return res
}

// strandedLocked lists, in order, what the given followers hold from an
// earlier store.
func (m *Manager) strandedLocked(followers []string) []string {
	var stranded []string
	for _, id := range followers {
		stranded = append(stranded, m.followers[id].stranded...)
	}
	sort.Strings(stranded)
	return slices.Compact(stranded)
}

// Holders returns the followers, other than asker, that say they hold a
// blob and serve it, ordered by node ID.
func (m *Manager) Holders(blob, asker string) []*pb.BlobHolder {
	m.mu.Lock()
	defer m.mu.Unlock()
	var holders []*pb.BlobHolder
	for _, id := range m.liveLocked(time.Now()) {
		if _, has := m.followers[id].held[blob]; !has || id == asker {
			continue
		}
		if address := m.address(id); address != "" {
			holders = append(holders, &pb.BlobHolder{NodeId: id, Address: address})
		}
	}
	return holders
}

// Recover copies a blob the store lacks back into it from a follower that
// holds it, and reports whether it did.
func (m *Manager) Recover(ctx context.Context, c cid.Cid) bool {
	for _, holder := range m.Holders(c.String(), "") {
		err := m.fetch(ctx, holder, c, m.store)
		if err == nil {
			m.log.Info("fetched back from a storage follower a blob this node's store lacked", "cid", c.String(), "node", holder.GetNodeId())
			return true
		}
		m.log.Warn("could not fetch a blob back from a storage follower", "cid", c.String(), "node", holder.GetNodeId(), "error", err)
	}
	return false
}

// Restore fetches back what followers hold from an earlier store of this
// node and pins it for owner, until released. It returns how many blobs it
// restored and which it could not.
func (m *Manager) Restore(ctx context.Context, owner string) (restored int, failed []string) {
	m.mu.Lock()
	stranded := m.strandedLocked(m.liveLocked(time.Now()))
	m.mu.Unlock()

	done := make(map[string]struct{})
	for _, id := range stranded {
		c, err := cid.Decode(id)
		if err == nil {
			err = m.store.Pin(ctx, owner, time.Time{}, c)
		}
		if errors.Is(err, storage.ErrNotFound) && m.Recover(ctx, c) {
			err = m.store.Pin(ctx, owner, time.Time{}, c)
		}
		if err != nil {
			m.log.Warn("could not restore a blob from this node's storage followers", "cid", id, "error", err)
			failed = append(failed, id)
			continue
		}
		done[id] = struct{}{}
	}

	// A follower says again what it holds when it next asks; until then
	// what was restored is taken off what it was known to be left with.
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, f := range m.followers {
		f.stranded = slices.DeleteFunc(f.stranded, func(id string) bool {
			_, restored := done[id]
			return restored
		})
	}
	return len(done), failed
}

// Recovering is a coordinator's store as everything but replication itself
// uses it: a blob it is asked to open and turns out to lack is first fetched
// back from a follower that holds it.
type Recovering struct {
	*storage.Store
	From *Manager
}

func (r Recovering) Open(ctx context.Context, c cid.Cid) (storage.Blob, error) {
	blob, err := r.Store.Open(ctx, c)
	if errors.Is(err, storage.ErrNotFound) && r.From.Recover(ctx, c) {
		return r.Store.Open(ctx, c)
	}
	return blob, err
}

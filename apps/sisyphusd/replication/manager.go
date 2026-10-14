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
// The coordinator signs each list, and the follower keeps the last one. A
// coordinator that comes back with its key and without its store is shown
// the list, knows its own signature, and takes back what the list names. A
// follower's word alone still brings nothing back.
//
// The coordinator's side is a Manager and a follower's a Follower.
package replication

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/ipfs/go-cid"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/sisyphus-network/Sisyphus/apps/sisyphusd/blobclient"
	"github.com/sisyphus-network/Sisyphus/packages/identity"
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
	Open(ctx context.Context, c cid.Cid) (storage.Blob, error)
	blobclient.Putter
}

type Config struct {
	// Identity is the coordinator's key. Its ID names the node, and it signs
	// what followers are told to hold. A node without one signs nothing and
	// takes nothing back by itself.
	Identity *identity.Identity
	// Store is the coordinator's store, and StoreID a name for it that is
	// lost if its pins are: a coordinator found with a store of another
	// name has lost the one its followers hold copies of.
	Store   Store
	StoreID string
	// Formerly is the ID the node had under a key it has since lost, or "".
	// What followers hold for a store of that node is kept, as it is for an
	// earlier store of this one, until Restore takes it back. A list signed
	// with the lost key cannot be told for the node's own and is not acted
	// on.
	Formerly string
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
	identity *identity.Identity
	id       string
	store    Store
	storeID  string
	earlier  []string // what the name of any earlier store of this node begins with
	replicas int
	address  func(nodeID string) string
	fetch    func(ctx context.Context, from *pb.BlobHolder, c cid.Cid, into blobclient.Putter) error
	log      *slog.Logger
	grace    time.Duration

	// atStart holds the blobs that were pinned when the manager started,
	// and settled is when it stops waiting for followers that may already
	// hold them.
	atStart map[string]time.Time
	settled time.Time

	// taking is held while a list is being taken back, so that two followers
	// with the same blob listed do not race for its expiry.
	taking sync.Mutex

	mu        sync.Mutex
	followers map[string]*follower
	// sequence is the sequence of the last list signed.
	sequence uint64
	// restored holds what has been taken back from a list, and the expiry
	// each blob was given. A list shown again brings back nothing that was
	// taken once and released since, for as long as the node runs.
	restored map[string]time.Time
	// restoring counts the lists being taken back at the moment.
	restoring int
	// unshown counts the times a follower failed to show that it held a
	// blob it said it held.
	unshown uint32
}

// follower is what a coordinator knows of one follower.
type follower struct {
	// asked is when it last asked what to hold.
	asked time.Time
	// held is what it then said it holds, less what it was asked to show
	// it holds and did not.
	held map[string]struct{}
	// pending is what it was last asked to show it holds, shown what it
	// has shown and when, and failed what it has failed to show since.
	pending []challenge
	shown   map[string]time.Time
	failed  map[string]struct{}
	// stranded lists what it holds from an earlier store of this node that
	// the present store has not pinned, and unsigned those of them that no
	// list it showed, signed by this node, names.
	stranded []string
	unsigned []string
	// warned is whether what it is stranded with has been logged, and
	// refused whether a list it showed was found not to be this node's.
	warned  bool
	refused bool
}

// New returns a manager for a coordinator that has just started.
func New(cfg Config) *Manager {
	m := &Manager{
		identity:  cfg.Identity,
		store:     cfg.Store,
		replicas:  cfg.Replicas,
		address:   cfg.Address,
		fetch:     cfg.Fetch,
		log:       cfg.Log,
		grace:     Grace,
		followers: make(map[string]*follower),
		restored:  make(map[string]time.Time),
	}
	if cfg.Identity != nil {
		m.id = cfg.Identity.ID()
	}
	m.storeID, m.earlier = m.id+"/"+cfg.StoreID, []string{m.id + "/"}
	if cfg.Formerly != "" {
		m.earlier = append(m.earlier, cfg.Formerly+"/")
	}
	// What an earlier run took back is known by the pins it left.
	for _, pin := range m.store.Pins() {
		if pin.Owner == RestoredOwner {
			m.restored[pin.CID.String()] = pin.Expires
		}
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

// pinned returns the blobs the store is keeping as of now, in order, and
// until when each is kept: the latest expiry of the pins on it, or the zero
// time if one of them holds until released. The short pin every new blob
// gets does not count: a blob is copied once somebody has asked for it to be
// kept.
func (m *Manager) pinned(now time.Time) (until map[string]time.Time, order []string) {
	until = make(map[string]time.Time)
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
		kept, listed := until[id]
		if !listed {
			order = append(order, id)
		}
		if !listed || later(pin.Expires, kept) {
			until[id] = pin.Expires
		}
	}
	return until, order
}

// later reports whether a pin expiring at a outlasts one expiring at b. The
// zero time is no expiry, and outlasts any other.
func later(a, b time.Time) bool {
	return !b.IsZero() && (a.IsZero() || a.After(b))
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
// and tells it what to hold from now on and the store to name next time.
//
// A follower that shows a list this node signed for an earlier store has
// what the list names taken back first, so that the answer is to hold it for
// the store the node has now.
func (m *Manager) Replicate(ctx context.Context, caller string, req *pb.ReplicateRequest) *pb.ReplicateResponse {
	store, holding, shown := req.GetStore(), req.GetHolding(), req.GetList()
	vouched := shown != nil && m.signedHere(shown)
	// named is what a list of an earlier store names, and owed what of that
	// could not be taken back this time.
	var named, owed map[string]struct{}
	if vouched && shown.GetStore() != m.storeID {
		owed = m.takeBack(ctx, caller, shown)
		named = make(map[string]struct{}, len(shown.GetBlobs()))
		for _, blob := range shown.GetBlobs() {
			named[blob.GetCid()] = struct{}{}
		}
	}
	now := time.Now()
	pinned, order := m.pinned(now)

	// What the follower was last asked to show it holds is checked, and
	// what to ask it next chosen, before the lock is taken: both read the
	// store. A follower asks again at once with its answers, and is put no
	// new questions in the reply to those.
	m.mu.Lock()
	proven, failed := make(map[string]time.Time), make(map[string]struct{})
	var pending []challenge
	if prior := m.followers[caller]; prior != nil {
		pending = prior.pending
		for _, id := range holding {
			if at, was := prior.shown[id]; was {
				proven[id] = at
			}
			if _, was := prior.failed[id]; was {
				failed[id] = struct{}{}
			}
		}
	}
	m.mu.Unlock()
	// Questions still within their time and not yet answered are put
	// again as they were: a follower may ask more than once before it
	// answers, to show a list for one. Past their time they have failed.
	waiting := len(req.GetAnswers()) == 0 && len(pending) > 0 && now.Sub(pending[0].issued) <= challengeWindow
	var verdicts map[string]bool
	if !waiting {
		verdicts = m.judge(ctx, now, pending, req.GetAnswers())
	}
	for id, held := range verdicts {
		delete(proven, id)
		delete(failed, id)
		if held {
			proven[id] = now
			continue
		}
		failed[id] = struct{}{}
		m.log.Warn("a storage follower did not show that it holds a blob it says it holds; it is not counted as holding it until it does", "node", caller, "cid", id)
	}
	var ask []challenge
	switch {
	case waiting:
		ask = pending
	case req.GetAnswersChallenges() && len(req.GetAnswers()) == 0:
		ask = m.challenges(ctx, now, holding, pinned, failed, proven)
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	for _, held := range verdicts {
		if !held {
			m.unshown++
		}
	}
	before, known := m.followers[caller]
	if !known {
		m.log.Info("a storage follower is here", "node", caller, "holding", len(holding))
		before = &follower{}
	}
	f := &follower{asked: now, held: make(map[string]struct{}, len(holding)), pending: ask, shown: proven, failed: failed}
	for _, id := range holding {
		if _, lacks := failed[id]; !lacks {
			f.held[id] = struct{}{}
		}
	}
	m.followers[caller] = f
	live := m.liveLocked(now)

	// A list that is not this node's is as good as none. A follower that
	// shows none this time is asked for it again, and is not logged twice.
	f.refused = (shown != nil && !vouched) || (shown == nil && before.refused)
	if f.refused && !before.refused {
		m.log.Warn("a storage follower showed a list of what to hold that this node did not sign; it is ignored, and nothing is taken back on the strength of it", "node", caller, "store", shown.GetStore())
	}

	var hold []string
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
	// For what a list this node signed names there is no need to guess. It
	// has been taken back, and is pinned; or it could not be fetched this
	// time, and is still to be kept; or it has lapsed, or was released after
	// it came back, and goes. A follower that shows a list of the store the
	// node has now was told by that list what to drop, and stopped before
	// it had.
	current := vouched && shown.GetStore() == m.storeID
	earlier := !current && store != m.storeID && slices.ContainsFunc(m.earlier, func(prefix string) bool {
		return strings.HasPrefix(store, prefix)
	})
	for _, id := range holding {
		_, kept := pinned[id]
		_, listed := named[id]
		_, due := owed[id]
		switch {
		case kept, listed && !due, !listed && !earlier:
		case listed:
			f.stranded = append(f.stranded, id)
		default:
			f.stranded, f.unsigned = append(f.stranded, id), append(f.unsigned, id)
		}
	}
	sort.Strings(f.stranded)

	// A follower that says it keeps lists is told what to hold in one.
	lists := req.GetKeepsLists() && m.identity != nil
	if len(f.stranded) > 0 {
		// It goes on holding for the earlier store, and keeps the list it
		// has. If it has yet to show that list, it is asked to, and what it
		// is left with is logged once it has answered.
		ask := lists && shown == nil
		f.warned = before.warned
		if !ask && !f.warned {
			m.log.Warn("a storage follower holds blobs from a store this node had before and no longer has pinned; it keeps them until they are restored", "node", caller, "blobs", len(f.stranded), "in_no_list_this_node_signed", len(f.unsigned))
			f.warned = true
		}
		return &pb.ReplicateResponse{Hold: append(hold, f.stranded...), Store: store, ShowList: ask, Challenges: questions(f.pending)}
	}
	if lists {
		return &pb.ReplicateResponse{Store: m.storeID, List: m.signLocked(now, hold, pinned), Challenges: questions(f.pending)}
	}
	return &pb.ReplicateResponse{Hold: hold, Store: m.storeID, Challenges: questions(f.pending)}
}

// questions puts challenges as they are sent.
func questions(asked []challenge) []*pb.Challenge {
	var sent []*pb.Challenge
	for _, c := range asked {
		sent = append(sent, &pb.Challenge{Cid: c.cid, Offset: c.offset, Length: c.length, Nonce: c.nonce})
	}
	return sent
}

// signLocked returns a signed list of the given blobs, each with the expiry
// the store has it pinned until.
func (m *Manager) signLocked(now time.Time, hold []string, until map[string]time.Time) *pb.KeepList {
	m.sequence = max(uint64(now.UnixNano()), m.sequence+1)
	list := &pb.KeepList{Store: m.storeID, Sequence: m.sequence}
	for _, id := range hold {
		blob := &pb.KeptBlob{Cid: id}
		if expires := until[id]; !expires.IsZero() {
			blob.KeepUntil = timestamppb.New(expires)
		}
		list.Blobs = append(list.Blobs, blob)
	}
	list.Signature = m.identity.Sign(signedBytes(list))
	return list
}

// signedHere reports whether a list carries this node's signature.
func (m *Manager) signedHere(list *pb.KeepList) bool {
	if m.identity == nil {
		return false
	}
	signed, _ := identity.Verify(m.id, signedBytes(list), list.GetSignature()) // fails only for an ID that is not one
	return signed
}

// takeBackTries is how many blobs in a row may fail to come back before the
// rest of a list is left for the next time its follower asks. A follower
// that cannot be reached costs seconds for each blob tried.
const takeBackTries = 3

// takeBack fetches from a follower what a list signed by this node names,
// and pins each blob as restored until the expiry the list gives. What has
// lapsed is passed over. It returns what is still to come: the blobs it
// could not fetch or pin this time.
func (m *Manager) takeBack(ctx context.Context, caller string, list *pb.KeepList) (owed map[string]struct{}) {
	m.taking.Lock()
	defer m.taking.Unlock()
	m.mu.Lock()
	m.restoring++
	m.mu.Unlock()
	defer func() {
		m.mu.Lock()
		m.restoring--
		m.mu.Unlock()
	}()

	now := time.Now()
	from := &pb.BlobHolder{NodeId: caller, Address: m.address(caller)}
	owed = make(map[string]struct{})
	took, failures := 0, 0
	// first is the first thing to go wrong, which is as much as is logged:
	// a follower that cannot be reached fails alike for every blob.
	var first error
	if from.GetAddress() == "" {
		failures, first = takeBackTries, errors.New("the follower is not connected as a worker yet, or serves nothing")
	}
	for _, blob := range list.GetBlobs() {
		id := blob.GetCid()
		var expires time.Time
		if blob.GetKeepUntil() != nil {
			expires = blob.GetKeepUntil().AsTime()
		}
		if !expires.IsZero() && !expires.After(now) {
			continue
		}
		// Of two lists that name a blob, the one that keeps it longer wins.
		m.mu.Lock()
		had, done := m.restored[id]
		m.mu.Unlock()
		if done && !later(expires, had) {
			continue
		}
		if failures == takeBackTries {
			owed[id] = struct{}{}
			continue
		}
		if err := m.takeOne(ctx, from, id, expires); err != nil {
			if first == nil {
				first = fmt.Errorf("%s: %w", id, err)
			}
			owed[id] = struct{}{}
			failures++
			continue
		}
		failures = 0
		took++
		m.mu.Lock()
		m.restored[id] = expires
		m.mu.Unlock()
	}
	if len(owed) > 0 {
		m.log.Warn("could not take back everything a list signed by this node names; the rest is tried again when the follower next asks", "node", caller, "blobs", len(owed), "error", first)
	}
	if took > 0 {
		m.log.Info("took back from a storage follower what a store this node had before was keeping, on the strength of a list this node signed",
			"node", caller, "store", list.GetStore(), "signed", time.Unix(0, int64(list.GetSequence())).UTC().Format(time.RFC3339), "blobs", took, "still_to_come", len(owed))
	}
	return owed
}

// takeOne pins a blob as restored, fetching it from a follower first if the
// store lacks it. The store checks what arrives against the CID.
func (m *Manager) takeOne(ctx context.Context, from *pb.BlobHolder, id string, expires time.Time) error {
	c, err := cid.Decode(id)
	if err == nil {
		err = m.store.Pin(ctx, RestoredOwner, expires, c)
	}
	if errors.Is(err, storage.ErrNotFound) {
		if err = m.fetch(ctx, from, c, m.store); err == nil {
			err = m.store.Pin(ctx, RestoredOwner, expires, c)
		}
	}
	return err
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
		Settling: len(m.atStart) > 0 && now.Before(m.settled), ChallengesFailed: m.unshown,
	}
	res.FromEarlierStore = uint32(len(m.strandedLocked(live, false)))
	res.UnsignedFromEarlierStore = uint32(len(m.strandedLocked(live, true)))
	res.Restoring = m.restoring > 0
	for _, pin := range m.store.Pins() {
		if pin.Owner == RestoredOwner && (pin.Expires.IsZero() || pin.Expires.After(now)) {
			res.Restored++
		}
	}
	for _, blob := range order {
		if only != "" && blob != only {
			continue
		}
		listed := &pb.ReplicatedBlob{Cid: blob}
		for _, id := range live {
			if _, has := m.followers[id].held[blob]; has {
				listed.Holders = append(listed.Holders, id)
			}
			if _, has := m.followers[id].shown[blob]; has {
				listed.Shown = append(listed.Shown, id)
			}
		}
		res.Blobs = append(res.Blobs, listed)
	}
	return res
}

// strandedLocked lists, in order, what the given followers hold from an
// earlier store, or only what no list signed by this node names.
func (m *Manager) strandedLocked(followers []string, unsigned bool) []string {
	var stranded []string
	for _, id := range followers {
		if unsigned {
			stranded = append(stranded, m.followers[id].unsigned...)
		} else {
			stranded = append(stranded, m.followers[id].stranded...)
		}
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
// restored and which it could not. It takes the followers' word for what
// that store kept, so it is for the node's owner to ask for: what a list
// signed by this node names is taken back without it.
func (m *Manager) Restore(ctx context.Context, owner string) (restored int, failed []string) {
	m.mu.Lock()
	stranded := m.strandedLocked(m.liveLocked(time.Now()), false)
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

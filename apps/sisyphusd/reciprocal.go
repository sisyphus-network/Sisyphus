package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sisyphus-network/Sisyphus/apps/sisyphusd/p2p"
	"github.com/sisyphus-network/Sisyphus/apps/sisyphusd/worker"
	"github.com/sisyphus-network/Sisyphus/packages/identity"
	"github.com/sisyphus-network/Sisyphus/packages/storage"
	"google.golang.org/grpc/credentials"
)

// Trust in a node for compute has two sides: this node gives it tasks, and
// this node takes tasks from it. The first is the node's place in this
// node's pool. The second is what this file does: for every node this one
// will take tasks from, it watches whether that node trusts it back and,
// while it does, works for it beside whatever else this node is doing.
// Neither node is restarted, and either ends it by ending its trust.

// reciprocity keeps a node working for the nodes it trusts that trust it.
type reciprocity struct {
	// trusted returns the IDs of the nodes this one will take tasks from.
	trusted func() []string
	// addresses returns where a node might be asked, as host:port, best
	// guess first.
	addresses func(id string) []string
	// watch opens a stream that first reports whether the node trusts this
	// one, then reports each change until the stream ends.
	watch func(ctx context.Context, id string) (net.Conn, error)
	// work works for a node that trusts this one until told to stop.
	work func(ctx context.Context, id, addr string) error
	// changed publishes the locally visible reciprocal-work status.
	changed func()
	// most is how many nodes are worked for at once.
	most int
	log  *slog.Logger

	mu sync.Mutex
	// news is closed, and replaced, when something may have changed: this
	// node's trust in another, or another's in this one. told counts how
	// often, so that news arriving while nobody was waiting is not missed.
	news chan struct{}
	told int
	// employed holds the nodes this one is working for right now.
	employed map[string]bool
}

// nudge has the node look again at once, rather than when it next would
// have. It is called when this node's own trust changes and when another
// node sends word that its has.
func (r *reciprocity) nudge() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.told++
	close(r.news)
	r.news = make(chan struct{})
}

// pause waits for news or ctx to end. A positive retry delay also wakes a
// disconnected watcher so it can retry; zero waits only for an event.
// heard is advanced even when a change arrived while the caller was busy.
func (r *reciprocity) pause(ctx context.Context, heard *int, retry time.Duration) (ended bool) {
	r.mu.Lock()
	news, fresh := r.news, r.told != *heard
	*heard = r.told
	r.mu.Unlock()
	if fresh {
		return ctx.Err() != nil
	}
	var due <-chan time.Time
	if retry > 0 {
		due = time.After(retry)
	}
	select {
	case <-due:
	case <-news:
	case <-ctx.Done():
	}
	return ctx.Err() != nil
}

// heardSoFar is how much news there has been, for a caller about to look.
func (r *reciprocity) heardSoFar() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.told
}

// workingFor reports whether this node is working for the given one now.
func (r *reciprocity) workingFor(id string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.employed[id]
}

func (r *reciprocity) setEmployed(id string, employed bool) {
	r.mu.Lock()
	changed := r.employed[id] != employed
	r.employed[id] = employed
	publish := r.changed
	r.mu.Unlock()
	if changed && publish != nil {
		publish()
	}
}

// newReciprocity fills in what a reciprocity keeps for itself.
func newReciprocity(r *reciprocity) *reciprocity {
	r.news, r.employed = make(chan struct{}), make(map[string]bool)
	return r
}

// run works for whoever it should until ctx ends, and returns once all of
// that work has stopped.
func (r *reciprocity) run(ctx context.Context) {
	type engagement struct {
		stop  context.CancelFunc
		ended chan struct{}
	}
	working := make(map[string]engagement)
	for {
		heard := r.heardSoFar()
		trusted := r.trusted()
		for id, e := range working {
			if !slices.Contains(trusted, id) {
				e.stop()
				<-e.ended
				delete(working, id)
				r.log.Info("no longer working for a node this one no longer trusts", "node", id)
			}
		}
		for _, id := range trusted {
			if _, already := working[id]; already || len(working) >= r.most {
				continue
			}
			for_, stop := context.WithCancel(ctx)
			e := engagement{stop: stop, ended: make(chan struct{})}
			working[id] = e
			go func() {
				defer close(e.ended)
				r.workFor(for_, id)
			}()
		}
		if r.pause(ctx, &heard, 0) {
			for _, e := range working {
				e.stop()
				<-e.ended
			}
			return
		}
	}
}

// workFor works for one node whenever that node will have it, until ctx
// ends.
func (r *reciprocity) workFor(ctx context.Context, id string) {
	const minRetry = time.Second
	const maxRetry = time.Minute
	backoff := minRetry
	heard := r.heardSoFar()
	for {
		conn, err := r.watch(ctx, id)
		if err == nil {
			var received bool
			received, err = r.followTrust(ctx, id, conn)
			conn.Close()
			if received {
				backoff = minRetry
			}
		}
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			r.log.Debug("reciprocal trust stream ended; reconnecting", "node", id, "error", err)
		}
		if r.pause(ctx, &heard, backoff) {
			return
		}
		backoff = min(backoff*2, maxRetry)
	}
}

type trustUpdate struct {
	trusted bool
	err     error
}

func sendTrustUpdate(stop <-chan struct{}, updates chan<- trustUpdate, update trustUpdate) bool {
	select {
	case updates <- update:
		return true
	case <-stop:
		return false
	}
}

func readTrustUpdates(conn net.Conn, stop <-chan struct{}, updates chan<- trustUpdate) {
	for {
		var state [1]byte
		if _, err := io.ReadFull(conn, state[:]); err != nil {
			sendTrustUpdate(stop, updates, trustUpdate{err: err})
			return
		}
		if state[0] > 1 {
			sendTrustUpdate(stop, updates, trustUpdate{err: errors.New("invalid reciprocal trust state")})
			return
		}
		if !sendTrustUpdate(stop, updates, trustUpdate{trusted: state[0] == 1}) {
			return
		}
	}
}

// followTrust consumes the initial trust state and its live changes. Losing
// the stream stops guest work; reconnecting starts with a fresh state snapshot.
func (r *reciprocity) followTrust(ctx context.Context, id string, conn net.Conn) (received bool, err error) {
	updates := make(chan trustUpdate, 1)
	stopReading := make(chan struct{})
	defer close(stopReading)
	go readTrustUpdates(conn, stopReading, updates)

	const minWorkRetry = time.Second
	const maxWorkRetry = time.Minute
	workRetryDelay := minWorkRetry
	heard := r.heardSoFar()
	trusted := false
	var stop context.CancelFunc
	var workDone <-chan error
	var retry <-chan time.Time
	startWork := func() {
		if !trusted || stop != nil || retry != nil {
			return
		}
		addresses := r.addresses(id)
		if len(addresses) == 0 {
			r.log.Debug("trusted node has no usable address yet", "node", id)
			retry = time.After(workRetryDelay)
			workRetryDelay = min(workRetryDelay*2, maxWorkRetry)
			return
		}
		workCtx, cancel := context.WithCancel(ctx)
		stop = cancel
		done := make(chan error, 1)
		workDone = done
		r.setEmployed(id, true)
		r.log.Info("working for a node that trusts this one, as this one trusts it", "node", id, "addr", addresses[0])
		go func() { done <- r.workAtAddresses(workCtx, id, addresses) }()
	}
	stopWork := func() {
		if stop == nil {
			return
		}
		stop()
		workErr := <-workDone
		r.log.Info("stopped working for a node", "node", id, "reason", workErr)
		stop = nil
		workDone = nil
		r.setEmployed(id, false)
	}
	defer stopWork()

	for {
		r.mu.Lock()
		news := r.news
		fresh := r.told != heard
		if fresh {
			heard = r.told
		}
		r.mu.Unlock()
		if fresh {
			retry = nil
			startWork()
			continue
		}
		select {
		case update := <-updates:
			if update.err != nil {
				return received, update.err
			}
			received = true
			if !update.trusted {
				trusted = false
				retry = nil
				workRetryDelay = minWorkRetry
				stopWork()
				continue
			}
			trusted = true
			workRetryDelay = minWorkRetry
			startWork()
		case workErr := <-workDone:
			r.setEmployed(id, false)
			r.log.Info("stopped working for a node", "node", id, "reason", workErr)
			stop()
			stop = nil
			workDone = nil
			if trusted {
				retry = time.After(workRetryDelay)
				workRetryDelay = min(workRetryDelay*2, maxWorkRetry)
			}
		case <-retry:
			retry = nil
			startWork()
		case <-news:
			retry = nil
			startWork()
		case <-ctx.Done():
			return received, ctx.Err()
		}
	}
}

func (r *reciprocity) workAtAddresses(ctx context.Context, id string, addresses []string) error {
	var last error
	for _, addr := range addresses {
		if err := ctx.Err(); err != nil {
			return err
		}
		last = r.work(ctx, id, addr)
		if ctx.Err() != nil {
			return ctx.Err()
		}
	}
	return last
}

// trustProtocol names the versioned stream on which a coordinator reports
// whether it currently trusts the authenticated peer as a worker, followed
// by changes. Version 2 replaces the old one-shot nudge protocol.
const trustProtocol = "/sisyphus/trust/2"

// wordTimeout bounds a trust-state write that a peer has stopped reading.
const wordTimeout = 5 * time.Second

type cancelConn struct {
	net.Conn
	cancel context.CancelFunc
	once   sync.Once
}

func (c *cancelConn) Close() error {
	c.once.Do(c.cancel)
	return c.Conn.Close()
}

// dialTrust opens a persistent stream. The deadline only bounds stream
// establishment; after it opens, the stream lives until its peer or ctx ends.
func dialTrust(ctx context.Context, host *p2p.Host, id string) (net.Conn, error) {
	return dialTrustWith(ctx, func(ctx context.Context) (net.Conn, error) {
		return host.Dial(ctx, id, trustProtocol)
	})
}

func cancelTrustDialUnlessOpened(opened *atomic.Bool, cancel context.CancelFunc) {
	if !opened.Load() {
		cancel()
	}
}

func dialTrustWith(ctx context.Context, dial func(context.Context) (net.Conn, error)) (net.Conn, error) {
	return dialTrustWithTimeout(ctx, 15*time.Second, dial)
}

func dialTrustWithTimeout(ctx context.Context, timeout time.Duration, dial func(context.Context) (net.Conn, error)) (net.Conn, error) {
	dialCtx, cancel := context.WithCancel(ctx)
	var opened atomic.Bool
	timer := time.AfterFunc(timeout, func() { cancelTrustDialUnlessOpened(&opened, cancel) })
	conn, err := dial(dialCtx)
	opened.Store(true)
	timer.Stop()
	if err != nil {
		cancel()
		return nil, err
	}
	if dialCtx.Err() != nil {
		conn.Close()
		cancel()
		return nil, context.DeadlineExceeded
	}
	return &cancelConn{Conn: conn, cancel: cancel}, nil
}

// serveTrustChanges answers each authenticated peer with its current worker
// status and keeps that status current until the peer or node disconnects.
func serveTrustChanges(ctx context.Context, lis net.Listener, changes *notices, trusts func(id string) bool) {
	var handlers sync.WaitGroup
	go func() {
		<-ctx.Done()
		lis.Close()
	}()
	for {
		conn, err := lis.Accept()
		if err != nil {
			break
		}
		handlers.Add(1)
		go func() {
			defer handlers.Done()
			serveTrustChange(ctx, conn, changes, trusts)
		}()
	}
	handlers.Wait()
}

func serveTrustChange(ctx context.Context, conn net.Conn, changes *notices, trusts func(id string) bool) {
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()
	peerID := conn.RemoteAddr().String()
	serveTrustState(ctx, conn, peerID, changes, trusts)
}

func serveTrustState(ctx context.Context, conn net.Conn, peerID string, changes *notices, trusts func(id string) bool) {
	changed, unsubscribe := changes.watch(peerID)
	defer unsubscribe()
	disconnected := make(chan struct{})
	go func() {
		var unexpected [1]byte
		_, _ = conn.Read(unexpected[:])
		close(disconnected)
	}()
	write := func() error {
		conn.SetWriteDeadline(time.Now().Add(wordTimeout))
		state := byte(0)
		if trusts(peerID) {
			state = 1
		}
		_, err := conn.Write([]byte{state})
		return err
	}
	if err := write(); err != nil {
		return
	}
	for {
		select {
		case <-changed:
			if err := write(); err != nil {
				return
			}
		case <-disconnected:
			return
		case <-ctx.Done():
			return
		}
	}
}

// notices wakes active trust streams for a peer whose admission changed.
type notices struct {
	mu       sync.Mutex
	watchers map[string]map[chan struct{}]struct{}
}

func (n *notices) tell(id string) {
	n.mu.Lock()
	for watcher := range n.watchers[id] {
		select {
		case watcher <- struct{}{}:
		default:
		}
	}
	n.mu.Unlock()
}

func (n *notices) watch(id string) (<-chan struct{}, func()) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.watchers == nil {
		n.watchers = make(map[string]map[chan struct{}]struct{})
	}
	watcher := make(chan struct{}, 1)
	if n.watchers[id] == nil {
		n.watchers[id] = make(map[chan struct{}]struct{})
	}
	n.watchers[id][watcher] = struct{}{}
	return watcher, func() {
		n.mu.Lock()
		defer n.mu.Unlock()
		delete(n.watchers[id], watcher)
		if len(n.watchers[id]) == 0 {
			delete(n.watchers, id)
		}
	}
}

// willing is the list of nodes this one will take tasks from: one side of
// its trust, the other being whom it gives tasks to, which is its pool. It
// is kept in the node's database and told to whoever wants to know of a
// change.
type willing struct {
	store willingStore
	mu    sync.Mutex
	ids   []string
	// changed, if set, is called after each change.
	changed func()
}

type willingStore interface {
	WorksFor() ([]string, error)
	SetWorksFor(id string, willing bool, now time.Time) error
}

func openWilling(store willingStore) (*willing, error) {
	ids, err := store.WorksFor()
	if err != nil {
		return nil, err
	}
	return &willing{store: store, ids: ids}, nil
}

// List returns the IDs of the nodes this one will take tasks from.
func (w *willing) List() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return slices.Clone(w.ids)
}

// Set makes the node with the given ID one this node will take tasks from,
// or no longer one.
func (w *willing) Set(id string, will bool) error {
	w.mu.Lock()
	if err := w.store.SetWorksFor(id, will, time.Now()); err != nil {
		w.mu.Unlock()
		return err
	}
	w.ids = slices.DeleteFunc(w.ids, func(other string) bool { return other == id })
	if will {
		w.ids = append(w.ids, id)
		slices.Sort(w.ids)
	}
	changed := w.changed
	w.mu.Unlock()
	if changed != nil {
		changed()
	}
	return nil
}

// onChange has fn called after each change from now on.
func (w *willing) onChange(fn func()) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.changed = fn
}

// whereToAsk lists, as host:port, the addresses a node's libp2p host is
// known at. A node that coordinates a pool serves its workers on the port
// its host listens on, so one of them is where to ask it for work.
func whereToAsk(found []p2p.Peer, id string) []string {
	var addrs []string
	for _, peer := range found {
		if peer.ID != id {
			continue
		}
		for _, multiaddr := range peer.Addrs {
			if hostport, ok := tcpAddress(multiaddr); ok && !slices.Contains(addrs, hostport) {
				addrs = append(addrs, hostport)
			}
		}
	}
	return addrs
}

// guestWork is what a node needs to work for another beside its own pool.
type guestWork struct {
	ident   *identity.Identity
	dataDir string
	// template is the worker this node runs for its own pool; one like it
	// is run for each node worked for. They share its Limit, so that all of
	// them together run no more tasks at once than the node has slots for.
	template *worker.Worker
	// kubo has the node join the other's private IPFS network, with a Kubo
	// daemon of its own for the purpose, as it would as that node's worker
	// and nothing else.
	kubo bool
	// kuboSecret is what each such daemon's API wants shown, if the node's
	// own does.
	kuboSecret string
	syncCache  bool
	maxCache   uint64

	mu sync.Mutex
	// working is the workers now running for other nodes, so that word of
	// a change in what this node serves reaches each of them.
	working map[*worker.Worker]struct{}
}

// offersChanged passes word that what this node serves may have changed
// to each worker it runs for another node.
func (g *guestWork) offersChanged() {
	g.mu.Lock()
	defer g.mu.Unlock()
	for w := range g.working {
		w.OffersChanged()
	}
}

// work runs as a worker of the node with the given ID, at addr, until ctx
// ends or that node turns this one away. What its tasks fetch is kept under
// that node's name, apart from this node's own data and every other's.
func (g *guestWork) work(ctx context.Context, id, addr string) error {
	dir := filepath.Join(g.dataDir, "guest", id)
	// If this cannot be made, what goes in it below cannot be either, and
	// says so.
	os.MkdirAll(dir, 0o700)
	log := g.template.Log
	creds := credentials.NewTLS(g.ident.ClientTLS(id))
	w := &worker.Worker{
		Name: g.template.Name, Slots: g.template.Slots, Workloads: g.template.Workloads, Limit: g.template.Limit, Pool: id, Log: log, Hardware: g.template.Hardware,
		Coordinator: addr, Credentials: creds,
	}
	g.mu.Lock()
	if g.working == nil {
		g.working = make(map[*worker.Worker]struct{})
	}
	g.working[w] = struct{}{}
	g.mu.Unlock()
	defer func() {
		g.mu.Lock()
		delete(g.working, w)
		g.mu.Unlock()
	}()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	// A pool's private network has a key of its own, so a node needs one
	// Kubo daemon for each pool it is in. Where the other node runs no such
	// network, or it cannot be joined, the work is done without.
	var store *storage.Store
	onNetwork := false
	if g.kubo {
		swarm, leave, err := joinSwarm(ctx, g.ident, filepath.Join(dir, "ipfs"), g.kuboSecret, addr, id, creds, log)
		if err != nil {
			log.Info("working for a node without its private IPFS network", "node", id, "reason", err)
		} else {
			// Following the network to a new key goes on beside the work, and
			// is finished, or given up, before Kubo is stopped.
			var following sync.WaitGroup
			defer func() {
				cancel()
				following.Wait()
				leave()
			}()
			w.OnSwarm = func(stated string) {
				following.Add(1)
				go func() {
					defer following.Done()
					swarm.adopt(ctx, addr, creds, stated, log)
				}()
			}
			store, onNetwork = storage.OpenKuboCache(swarm.daemon), true
		}
	}
	if store == nil {
		var err error
		if store, err = storage.OpenCache(filepath.Join(dir, "cache"), g.syncCache); err != nil {
			return err
		}
	}
	defer store.Close()
	remote, err := worker.DialBlobs(addr, creds, store, g.maxCache)
	if err != nil {
		return err
	}
	defer remote.Close()
	if onNetwork {
		remote.OnFallback = warnOfFallback(log)
	}
	w.Blobs = remote
	return w.Run(ctx)
}

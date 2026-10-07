package main

import (
	"context"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"

	"github.com/sisyphus-network/Sisyphus/apps/sisyphusd/access"
	"github.com/sisyphus-network/Sisyphus/apps/sisyphusd/p2p"
	"github.com/sisyphus-network/Sisyphus/apps/sisyphusd/worker"
	"github.com/sisyphus-network/Sisyphus/packages/identity"
	pb "github.com/sisyphus-network/Sisyphus/packages/protocol/sisyphus/v1"
	"github.com/sisyphus-network/Sisyphus/packages/storage"
)

// Trusting a node for compute means two things: this node will give it
// tasks, and will take tasks from it. The first is the node's place in this
// node's pool. The second is what this file does: for every node this one
// trusts, it asks now and then whether that node trusts it back, and when
// it does, works for it, beside whatever else this node is doing. Neither
// node is restarted, and either ends it by ending its trust.

// reciprocity keeps a node working for the nodes it trusts that trust it.
type reciprocity struct {
	// trusted returns the IDs of the nodes this one trusts for compute.
	trusted func() []string
	// addresses returns where a node might be asked, as host:port, best
	// guess first.
	addresses func(id string) []string
	// ask asks the node at an address whether it trusts this one, and work
	// works for it there until told to stop or turned away.
	ask  func(ctx context.Context, id, addr string) bool
	work func(ctx context.Context, id, addr string) error
	// every is how often the trusted are looked over and asked again, and
	// most how many nodes are worked for at once.
	every time.Duration
	most  int
	log   *slog.Logger

	mu sync.Mutex
	// news is closed, and replaced, when something may have changed: this
	// node's trust in another, or another's in this one.
	news chan struct{}
	// employed holds the nodes this one is working for right now.
	employed map[string]bool
}

// nudge has the node look again at once, rather than when it next would
// have. It is called when this node's own trust changes and when another
// node sends word that its has.
func (r *reciprocity) nudge() {
	r.mu.Lock()
	defer r.mu.Unlock()
	close(r.news)
	r.news = make(chan struct{})
}

// pause waits for the usual interval, for news, or for ctx to end, and
// reports whether ctx has.
func (r *reciprocity) pause(ctx context.Context) (ended bool) {
	r.mu.Lock()
	news := r.news
	r.mu.Unlock()
	select {
	case <-time.After(r.every):
	case <-news:
	case <-ctx.Done():
	}
	return ctx.Err() != nil
}

// workingFor reports whether this node is working for the given one now.
func (r *reciprocity) workingFor(id string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.employed[id]
}

func (r *reciprocity) setEmployed(id string, employed bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.employed[id] = employed
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
		if r.pause(ctx) {
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
	for {
		for _, addr := range r.addresses(id) {
			if !r.ask(ctx, id, addr) {
				continue
			}
			r.log.Info("working for a node that trusts this one, as this one trusts it", "node", id, "addr", addr)
			r.setEmployed(id, true)
			err := r.work(ctx, id, addr)
			r.setEmployed(id, false)
			r.log.Info("stopped working for a node", "node", id, "reason", err)
			break
		}
		if r.pause(ctx) {
			return
		}
	}
}

// trustProtocol names the streams on which one node's libp2p host tells
// another that its trust in it has changed. Nothing is said on them beyond
// that; the node told asks in the ordinary way what the news is.
const trustProtocol = "/sisyphus/trust/1"

// wordTimeout is how long a node spends getting word to another.
const wordTimeout = 5 * time.Second

// sendWord tells the node with the given ID that this node's trust in it
// has changed, if it can be reached. A node that cannot be finds out when
// it next asks.
func sendWord(ctx context.Context, host *p2p.Host, id string) {
	ctx, cancel := context.WithTimeout(ctx, wordTimeout)
	defer cancel()
	conn, err := host.Dial(ctx, id, trustProtocol)
	if err != nil {
		return
	}
	// A stream is only opened in earnest when something is sent on it.
	conn.SetDeadline(time.Now().Add(wordTimeout))
	conn.Write([]byte{1})
	conn.Read(make([]byte, 1))
	conn.Close()
}

// hearWord calls heard each time another node sends word, until lis is
// closed.
func hearWord(lis net.Listener, heard func()) {
	for {
		conn, err := lis.Accept()
		if err != nil {
			return
		}
		conn.SetDeadline(time.Now().Add(wordTimeout))
		conn.Read(make([]byte, 1))
		conn.Close()
		heard()
	}
}

// notices passes news of whom this node trusts on to whoever has asked to
// be told, once anyone has. The list of members is opened before there is
// anyone to tell.
type notices struct {
	mu sync.Mutex
	to func(id string)
}

func (n *notices) tell(id string) {
	n.mu.Lock()
	to := n.to
	n.mu.Unlock()
	if to != nil {
		to(id)
	}
}

func (n *notices) listen(to func(id string)) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.to = to
}

// trustedWorkers lists the nodes admitted to a pool as workers.
func trustedWorkers(members []access.Member) []string {
	var ids []string
	for _, m := range members {
		if m.Role == access.Worker {
			ids = append(ids, m.ID)
		}
	}
	return ids
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

// askTimeout is how long a node is given to say whether it trusts this one.
const askTimeout = 5 * time.Second

// trustsThisNode asks the node expected at addr whether it has admitted
// this one as a worker.
func trustsThisNode(ctx context.Context, ident *identity.Identity, id, addr string) bool {
	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(credentials.NewTLS(ident.ClientTLS(id))))
	if err != nil {
		return false
	}
	defer conn.Close()
	ctx, cancel := context.WithTimeout(ctx, askTimeout)
	defer cancel()
	// Joining with no invitation is asking exactly that.
	joined, err := pb.NewPoolServiceClient(conn).Join(ctx, &pb.JoinRequest{})
	return err == nil && joined.GetRole() == pb.Role_ROLE_WORKER
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
	kubo      bool
	syncCache bool
	maxCache  uint64
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
		Name: g.template.Name, Slots: g.template.Slots, Workloads: g.template.Workloads, Limit: g.template.Limit, Pool: id, Log: log,
		Coordinator: addr, Credentials: creds,
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	// A pool's private network has a key of its own, so a node needs one
	// Kubo daemon for each pool it is in. Where the other node runs no such
	// network, or it cannot be joined, the work is done without.
	var store *storage.Store
	onNetwork := false
	if g.kubo {
		swarm, leave, err := joinSwarm(ctx, g.ident, filepath.Join(dir, "ipfs"), addr, id, creds, log)
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

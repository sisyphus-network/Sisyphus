package main

import (
	"context"
	"log/slog"
	"path/filepath"
	"slices"
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
		select {
		case <-time.After(r.every):
		case <-ctx.Done():
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
			err := r.work(ctx, id, addr)
			r.log.Info("stopped working for a node", "node", id, "reason", err)
			break
		}
		select {
		case <-time.After(r.every):
		case <-ctx.Done():
			return
		}
	}
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
	// is run for each node worked for. Its Limit is shared, so that all of
	// them together run no more tasks at once than the node has slots for.
	template  *worker.Worker
	syncCache bool
	maxCache  uint64
}

// work runs as a worker of the node with the given ID, at addr, until ctx
// ends or that node turns this one away. What its tasks fetch is cached
// apart from this node's own data and from every other node's.
func (g *guestWork) work(ctx context.Context, id, addr string) error {
	cache, err := storage.OpenCache(filepath.Join(g.dataDir, "guest", id), g.syncCache)
	if err != nil {
		return err
	}
	defer cache.Close()
	creds := credentials.NewTLS(g.ident.ClientTLS(id))
	remote, err := worker.DialBlobs(addr, creds, cache, g.maxCache)
	if err != nil {
		return err
	}
	defer remote.Close()
	w := &worker.Worker{
		Name: g.template.Name, Slots: g.template.Slots, Workloads: g.template.Workloads, Limit: g.template.Limit, Log: g.template.Log,
		Coordinator: addr, Credentials: creds, Blobs: remote,
	}
	return w.Run(ctx)
}

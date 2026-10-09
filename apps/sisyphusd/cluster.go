package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/ipfs/go-cid"

	"github.com/sisyphus-network/Sisyphus/packages/ipfscluster"
	pb "github.com/sisyphus-network/Sisyphus/packages/protocol/sisyphus/v1"
)

// With --cluster, a node runs an IPFS Cluster peer beside its Kubo. The
// peers of a pool form one cluster, in which the coordinator alone says what
// is pinned and the workers keep what they are allocated. What the
// coordinator's store pins, its cluster peer is told to pin, on as many
// members as --cluster-replicas asks for.

// clusterSecret derives the secret of a pool's cluster from the key of its
// private IPFS network, so that whoever has been given the one has the
// other, and a change of key shuts a removed node out of both.
func clusterSecret(swarmKey string) string {
	sum := sha256.Sum256([]byte("sisyphus pool cluster\n" + swarmKey))
	return hex.EncodeToString(sum[:])
}

// clusterPinName labels the cluster pins that stand for the store's own.
// A pin put in the cluster by hand has another name, or none, and is left
// alone.
const clusterPinName = "sisyphus"

// clusterHeartbeat is how often the members of a pool's cluster tell each
// other they are there; zero leaves it at the fifteen seconds the peer
// chooses. clusterSync is how often a coordinator compares all of its
// store's pins with all of the cluster's.
var (
	clusterHeartbeat time.Duration
	clusterSync      = time.Minute
)

// poolCluster is a pool's cluster as its coordinator's own peer sees it.
type poolCluster struct {
	peer *ipfscluster.Client
	// replicas is how many members each pin is meant to be held by.
	replicas int
}

// Local returns where on this machine the node's cluster peer accepts the
// other members.
func (c *poolCluster) Local(ctx context.Context) (string, error) {
	return c.peer.Listening(ctx)
}

// Status says which members the cluster hears from, and which of them hold
// each of the pins that stand for the store's own.
func (c *poolCluster) Status(ctx context.Context) (*pb.ClusterStatusResponse, error) {
	peers, err := c.peer.Peers(ctx)
	if err != nil {
		return nil, err
	}
	pins, err := c.peer.Status(ctx)
	if err != nil {
		return nil, err
	}
	state := &pb.ClusterStatusResponse{Replicas: uint32(c.replicas)}
	for _, peer := range peers {
		state.Members = append(state.Members, &pb.ClusterMember{NodeId: peer.ID, Name: peer.Name, Error: peer.Error})
	}
	sort.Slice(state.Members, func(a, b int) bool { return state.Members[a].GetNodeId() < state.Members[b].GetNodeId() })
	for _, pin := range pins {
		if pin.Name != clusterPinName {
			continue
		}
		listed := &pb.ClusterPin{Cid: pin.CID}
		for id, member := range pin.Members {
			// A member that was not chosen to hold the pin has no copy to
			// report on.
			if member.Status != ipfscluster.Remote {
				listed.Copies = append(listed.Copies, &pb.ClusterCopy{NodeId: id, Name: member.Name, Status: member.Status, Error: member.Error})
			}
		}
		sort.Slice(listed.Copies, func(a, b int) bool { return listed.Copies[a].GetNodeId() < listed.Copies[b].GetNodeId() })
		state.Pins = append(state.Pins, listed)
	}
	sort.Slice(state.Pins, func(a, b int) bool { return state.Pins[a].GetCid() < state.Pins[b].GetCid() })
	return state, nil
}

// clusterPins makes a pool's cluster pin what its coordinator's store pins,
// and nothing else under the store's name.
type clusterPins struct {
	// kept lists the blobs the store's pins keep.
	kept func() []cid.Cid
	peer *ipfscluster.Client
	// self is this node's ID. Its Kubo holds everything the store does, so
	// it is always the first member chosen to hold a pin.
	self     string
	replicas int
	log      *slog.Logger

	// changed holds a token while there is a change of the store's pins
	// that has not been looked at.
	changed chan struct{}
	// known is what the cluster is known to pin for the store, by CID, or
	// nil if that has to be asked again.
	known map[string]ipfscluster.Pin
}

func newClusterPins(kept func() []cid.Cid, peer *ipfscluster.Client, self string, replicas int, log *slog.Logger) *clusterPins {
	return &clusterPins{kept: kept, peer: peer, self: self, replicas: replicas, log: log, changed: make(chan struct{}, 1)}
}

// nudge says that the store's pins have changed. It never waits.
func (p *clusterPins) nudge() {
	select {
	case p.changed <- struct{}{}:
	default:
	}
}

// run brings the cluster's pins into line with the store's: at once, after
// every change to the store's, and every clusterSync in case either has
// moved without the other, until ctx ends.
func (p *clusterPins) run(ctx context.Context) {
	ticker := time.NewTicker(clusterSync)
	defer ticker.Stop()
	for {
		if err := p.sync(ctx); err != nil && ctx.Err() == nil {
			p.log.Warn("the pool's cluster does not yet pin what this node's store does; trying again", "in", clusterSync.String(), "error", err)
		}
		select {
		case <-p.changed:
		case <-ticker.C:
			// What the cluster pins is asked afresh, not assumed.
			p.known = nil
		case <-ctx.Done():
			return
		}
	}
}

// sync pins on the cluster each blob the store keeps that the cluster does
// not, or keeps on fewer members than it now could, and unpins what the
// store has let go of.
//
// Each pin asks for as many holders as --cluster-replicas says. The cluster
// refuses a pin it cannot find the least number of holders for, so that
// least number is no more than the members there are, and is raised as
// members join.
func (p *clusterPins) sync(ctx context.Context) error {
	if p.known == nil {
		pins, err := p.peer.Pins(ctx)
		if err != nil {
			return err
		}
		p.known = make(map[string]ipfscluster.Pin)
		for _, pin := range pins {
			if pin.Name == clusterPinName {
				p.known[pin.CID] = pin
			}
		}
	}
	peers, err := p.peer.Peers(ctx)
	if err != nil {
		return err
	}
	least := 0
	for _, peer := range peers {
		if peer.Error == "" && least < p.replicas {
			least++
		}
	}
	least = max(least, 1)

	wanted := make(map[string]struct{})
	for _, root := range p.kept() {
		id := root.String()
		wanted[id] = struct{}{}
		if pin, pinned := p.known[id]; pinned && pin.Min >= least && pin.Max == p.replicas {
			continue
		}
		pin, err := p.peer.Pin(ctx, id, ipfscluster.PinOptions{Name: clusterPinName, Min: least, Max: p.replicas, Prefer: []string{p.self}})
		if err != nil {
			// What is known may no longer be so.
			p.known = nil
			return err
		}
		p.known[id] = pin
		p.log.Debug("pinned on the pool's cluster", "cid", id, "members", len(pin.Allocations), "of", p.replicas)
	}
	for id := range p.known {
		if _, keep := wanted[id]; keep {
			continue
		}
		if err := p.peer.Unpin(ctx, id); err != nil {
			p.known = nil
			return err
		}
		delete(p.known, id)
		p.log.Debug("unpinned from the pool's cluster", "cid", id)
	}
	return nil
}

// joinCluster starts a cluster peer for a worker whose Kubo has joined the
// pool's private network, as a follower of the pool's cluster: it keeps what
// the coordinator's peer allocates to it and can change nothing. It reaches
// the coordinator's peer through the coordinator's own port. The peer runs
// until leave is called, which must be before the swarm is left.
func joinCluster(ctx context.Context, swarm *poolSwarm, cfg ipfscluster.Config) (leave func(), err error) {
	if !swarm.clustered {
		return nil, errors.New("the coordinator does not run an IPFS Cluster for its pool: start it with --cluster, or this node without")
	}
	lis, err := listenLoopback()
	if err != nil {
		return nil, fmt.Errorf("listen for this node's cluster peer: %w", err)
	}
	swarm.forward(lis, pb.TunnelTarget_TUNNEL_TARGET_CLUSTER)
	coordinator := swarm.route.coordinator
	swarm.clusterPeers = []string{fmt.Sprintf("/ip4/127.0.0.1/tcp/%d/p2p/%s", lis.Addr().(*net.TCPAddr).Port, coordinator)}
	cfg.Secret, cfg.KuboAPI, cfg.Peers = clusterSecret(swarm.Key()), swarm.daemon.Address(), swarm.clusterPeers
	cfg.Trusted, cfg.Follower = []string{coordinator}, true
	if swarm.cluster, err = ipfscluster.Start(ctx, cfg); err != nil {
		lis.Close()
		return nil, err
	}
	return swarm.cluster.Stop, nil
}

// showCluster prints how the pool's cluster stands.
func showCluster(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("sisyphusd pool cluster", flag.ContinueOnError)
	node := targetFlags(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}
	conn, err := node.connect()
	if err != nil {
		return err
	}
	defer conn.Close()
	state, err := pb.NewPoolServiceClient(conn).ClusterStatus(ctx, &pb.ClusterStatusRequest{})
	if err != nil {
		return err
	}

	tw := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "MEMBER\tNAME\tSTATE")
	for _, member := range state.GetMembers() {
		condition := "answering"
		if member.GetError() != "" {
			condition = member.GetError()
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\n", member.GetNodeId(), member.GetName(), condition)
	}
	tw.Flush()
	fmt.Fprintf(stdout, "\neach pin is to be held by %d of them\n", state.GetReplicas())
	if len(state.GetPins()) == 0 {
		fmt.Fprintln(stdout, "nothing is pinned")
		return nil
	}

	fmt.Fprintln(stdout)
	fmt.Fprintln(tw, "CID\tCOPIES\tHELD BY\tWAITING FOR")
	for _, pin := range state.GetPins() {
		var holders, waiting []string
		for _, copied := range pin.GetCopies() {
			name := copied.GetName()
			if name == "" {
				name = copied.GetNodeId()
			}
			switch {
			case copied.GetStatus() == ipfscluster.Pinned:
				holders = append(holders, name)
			case copied.GetError() != "":
				waiting = append(waiting, fmt.Sprintf("%s (%s: %s)", name, copied.GetStatus(), copied.GetError()))
			default:
				waiting = append(waiting, fmt.Sprintf("%s (%s)", name, copied.GetStatus()))
			}
		}
		fmt.Fprintf(tw, "%s\t%d/%d\t%s\t%s\n", pin.GetCid(), len(holders), state.GetReplicas(), orDash(holders), orDash(waiting))
	}
	tw.Flush()
	return nil
}

// orDash lists names for a table, in which a dash stands for none.
func orDash(names []string) string {
	if len(names) == 0 {
		return "-"
	}
	return strings.Join(names, ", ")
}

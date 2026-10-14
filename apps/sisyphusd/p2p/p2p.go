// Package p2p gives a node a libp2p host: the means for two nodes of a pool
// to reach each other whether or not either has a port open.
//
// A node that can be reached, which a coordinator always can, relays for
// those that cannot: each of them keeps a connection to it, and it joins
// two such connections when one node asks for another. Where their networks
// allow it the two then connect directly and the relay drops out.
//
// The host listens on the node's one port, beside its gRPC server. What
// arrives there is told apart by how it begins: a TLS handshake goes to
// gRPC and a libp2p one comes here.
package p2p

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net"
	"net/netip"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/libp2p/go-libp2p"
	dht "github.com/libp2p/go-libp2p-kad-dht"
	"github.com/libp2p/go-libp2p/core/control"
	"github.com/libp2p/go-libp2p/core/crypto"
	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/libp2p/go-libp2p/core/protocol"
	"github.com/libp2p/go-libp2p/core/routing"
	"github.com/libp2p/go-libp2p/core/transport"
	"github.com/libp2p/go-libp2p/p2p/discovery/mdns"
	rcmgr "github.com/libp2p/go-libp2p/p2p/host/resource-manager"
	"github.com/libp2p/go-libp2p/p2p/muxer/yamux"
	"github.com/libp2p/go-libp2p/p2p/net/swarm"
	"github.com/libp2p/go-libp2p/p2p/protocol/circuitv2/client"
	pbv2 "github.com/libp2p/go-libp2p/p2p/protocol/circuitv2/pb"
	"github.com/libp2p/go-libp2p/p2p/protocol/circuitv2/relay"
	"github.com/libp2p/go-libp2p/p2p/security/noise"
	"github.com/libp2p/go-libp2p/p2p/transport/tcp"
	"github.com/libp2p/go-libp2p/p2p/transport/tcpreuse"
	"github.com/libp2p/go-libp2p/x/rate"
	ma "github.com/multiformats/go-multiaddr"
	"go.uber.org/fx"

	"github.com/sisyphus-network/Sisyphus/packages/identity"
)

// Config describes the host a node wants.
type Config struct {
	// Identity is the node's key. The host's peer ID is the node's ID.
	Identity *identity.Identity
	// Listen is the address, as host:port, to accept connections on. Empty
	// means a port of the system's choosing on every interface, which other
	// nodes on the same network can reach and nobody need have opened.
	Listen string
	// Relay has the host carry connections between the nodes that Allow
	// admits. It is for a node others can reach.
	Relay bool
	// Via are the relays this host can be reached through, each a
	// multiaddress ending in /p2p/ and the relay's ID. A host with any
	// takes itself to be unreachable otherwise.
	Via []string
	// MoreRelays, if set, is asked from time to time for further relays to
	// keep a place on, in the same form. Those it stops naming are let go.
	MoreRelays func(ctx context.Context) []string
	// Allow says whether the node with the given ID is one of this node's
	// own: a member of its pool. Only those are relayed for. Unless Discover
	// is set, only those may connect at all, and everyone else is turned
	// away once the handshake has shown who they are.
	Allow func(id string) bool
	// Discover has the host look for other nodes and let them find it: on
	// its own network by multicast DNS, and beyond it through a distributed
	// hash table that it both uses and serves. Any node may then connect,
	// which is what lets strangers find each other. What a stranger can do
	// once connected is for the services on the host to decide.
	Discover bool
	// MaxConnections is how many connections the host takes at once, from
	// everyone. Zero leaves it to what the machine can hold open.
	MaxConnections int
	Log            *slog.Logger
}

// Host is a node's libp2p host.
type Host struct {
	host   host.Host
	relays []peer.AddrInfo
	// shared hands out what arrives on the listening port by kind.
	shared *tcpreuse.ConnMgr

	// How often to look to a place on a relay, and how long to try a
	// direct connection for, fixed when the host starts.
	placeCheck, directDial time.Duration

	// table is the distributed hash table, on a host that discovers.
	table *dht.IpfsDHT
	// ctx ends, through stop, when the host closes; kept counts what it has
	// running until then.
	ctx  context.Context
	stop context.CancelFunc
	kept sync.WaitGroup
	mu   sync.Mutex
	// renew is when the host's place on each relay is next due for
	// renewal. A relay it has no place on is absent.
	renew map[peer.ID]time.Time
	// lan is the host's announcing of itself on its own network.
	lan io.Closer
	// carried counts what the host has relayed for others.
	carried carried
}

// Carried returns what the host has relayed between other nodes since it
// started: how many times it joined two of them, and how many bytes passed
// between them, counting both directions. A host that does not relay has
// relayed nothing.
func (h *Host) Carried() (connections, bytes uint64) {
	return h.carried.connections.Load(), h.carried.bytes.Load()
}

// carried is told by the relay service of what it does, and keeps count of
// the two things worth knowing later: how often it was used and how much
// for. The rest of what it is told goes unrecorded.
type carried struct {
	connections, bytes atomic.Uint64
}

func (c *carried) ConnectionOpened()        { c.connections.Add(1) }
func (c *carried) BytesTransferred(cnt int) { c.bytes.Add(uint64(cnt)) }

func (*carried) RelayStatus(bool)                      {}
func (*carried) ConnectionClosed(time.Duration)        {}
func (*carried) ConnectionRequestHandled(pbv2.Status)  {}
func (*carried) ReservationAllowed(bool)               {}
func (*carried) ReservationClosed(int)                 {}
func (*carried) ReservationRequestHandled(pbv2.Status) {}

// room is how many connections a host takes: inAll at once from everyone,
// perAddress of them from one address, and new ones from one address at
// perSecond a second once perAddress have come at a run.
//
// libp2p's own limits are, from one address, eight at once, and sixteen at
// a run and then one every five seconds; and of connections that have not
// become libp2p connections to a peer, which it holds apart as passing
// through, a few dozen at once, by the machine's memory. They are set for a
// node among strangers on a public network. A pool's port is shared with
// its gRPC server, so they counted every connection a member makes there,
// none of which ever becomes a libp2p connection. Each command a person or
// a script runs is one: the seventeenth in a row was dropped as it was
// made. A pool's members are often behind one address: the machines of one
// house or office, each with a worker that holds several connections,
// beside a desktop, a command line and an agent's server. And a coordinator
// with little memory refused its sixty-fifth connection from anyone.
//
// What a machine can hold open is what its system lets a program have open
// at once, so that is what the limits are taken from, unless its owner
// says how many: half of it for connections, which leaves the other half
// for files and for the connections the node makes itself. A connection is
// still nothing until its handshake has shown a node that was admitted.
func room(asked, files int) (inAll, perAddress, perSecond int) {
	switch {
	case asked > 0:
		inAll = asked
	case files > 0:
		// However little that is, a pool has room for a household.
		inAll = max(files/2, 512)
	default:
		// A system that does not say is taken to allow what most do.
		inAll = 8192
	}
	perAddress = min(max(inAll/8, 256), inAll)
	perSecond = max(perAddress/8, 32)
	return inAll, perAddress, perSecond
}

// resources returns what limits a host's connections, streams and memory:
// libp2p's own limits, with room for a pool's members. asked is how many
// connections its owner said to take at once, or zero for as many as the
// machine can hold open.
func resources(asked int) network.ResourceManager {
	inAll, perAddress, perSecond := room(asked, openFiles())
	limits := rcmgr.DefaultLimits
	libp2p.SetDefaultServiceLimits(&limits)
	// Connections passing through are limited as all of them are, not
	// apart: to the gRPC server that is every connection there is.
	held := rcmgr.PartialLimitConfig{
		System:    rcmgr.ResourceLimits{ConnsInbound: rcmgr.LimitVal(inAll), Conns: rcmgr.LimitVal(2 * inAll), FD: rcmgr.Unlimited},
		Transient: rcmgr.ResourceLimits{ConnsInbound: rcmgr.Unlimited, Conns: rcmgr.Unlimited, FD: rcmgr.Unlimited},
	}.Build(limits.AutoScale())
	// The same shape as libp2p's own: one limit for an IPv4 address, and
	// for IPv6 one for a household's prefix and a wider one beyond it. This
	// machine's own addresses are not limited, as they are not by libp2p.
	often := rate.Limit{RPS: float64(perSecond), Burst: perAddress}
	wider := rate.Limit{RPS: float64(8 * perSecond), Burst: 8 * perAddress}
	manager, _ := rcmgr.NewResourceManager(rcmgr.NewFixedLimiter(held),
		rcmgr.WithLimitPerSubnet(
			[]rcmgr.ConnLimitPerSubnet{{PrefixLength: 32, ConnCount: perAddress}},
			[]rcmgr.ConnLimitPerSubnet{{PrefixLength: 56, ConnCount: perAddress}, {PrefixLength: 48, ConnCount: 8 * perAddress}},
		),
		rcmgr.WithConnRateLimiters(&rate.Limiter{
			NetworkPrefixLimits: []rate.PrefixLimit{{Prefix: netip.MustParsePrefix("127.0.0.0/8")}, {Prefix: netip.MustParsePrefix("::1/128")}},
			SubnetRateLimiter: rate.SubnetLimiter{
				IPv4SubnetLimits: []rate.SubnetLimit{{PrefixLength: 32, Limit: often}},
				IPv6SubnetLimits: []rate.SubnetLimit{{PrefixLength: 56, Limit: often}, {PrefixLength: 48, Limit: wider}},
				GracePeriod:      time.Minute,
			},
		}),
	) // fails only for an option that fails, and neither of these does
	return manager
}

// New starts a host.
func New(cfg Config) (*Host, error) {
	key, _ := crypto.UnmarshalPrivateKey(cfg.Identity.Libp2pKey()) // a node's key is always one libp2p can read
	// Left to itself the host takes a port of the system's choosing, on
	// both kinds of address.
	listen := []string{"/ip4/0.0.0.0/tcp/0", "/ip6/::/tcp/0"}
	if cfg.Listen != "" {
		var err error
		if listen, err = multiaddrsOf(cfg.Listen); err != nil {
			return nil, err
		}
	}
	h := &Host{renew: make(map[peer.ID]time.Time), placeCheck: placeCheck, directDial: directDial}
	for _, address := range cfg.Via {
		info, err := peer.AddrInfoFromString(address)
		if err != nil {
			return nil, fmt.Errorf("p2p: relay address %q: %w", address, err)
		}
		h.relays = append(h.relays, *info)
	}

	options := []libp2p.Option{
		libp2p.Identity(key),
		libp2p.ResourceManager(resources(cfg.MaxConnections)),
		// A machine without one of the two kinds of address listens on the
		// other; only failing at both stops the host.
		libp2p.ListenAddrStrings(listen...),
		// One transport and one way of securing and sharing it, which every
		// libp2p implementation has.
		libp2p.Transport(tcp.NewTCPTransport),
		libp2p.Security(noise.ID, noise.New),
		libp2p.Muxer(yamux.ID, yamux.DefaultTransport),
		libp2p.ShareTCPListener(),
		libp2p.WithFxOption(fx.Invoke(func(shared *tcpreuse.ConnMgr) { h.shared = shared })),
		libp2p.EnableHolePunching(),
		libp2p.DisableMetrics(),
		libp2p.UserAgent("sisyphusd"),
	}
	if cfg.Relay {
		members := relayRules(cfg.Allow)
		options = append(options,
			libp2p.ForceReachabilityPublic(),
			libp2p.EnableRelayService(relay.WithACL(members), relay.WithMetricsTracer(&h.carried), relay.WithResources(relay.Resources{
				// A pool's relay carries its members' data, not strangers'
				// handshakes, so nothing is rationed: no limit on how long
				// or how much, and room for many nodes behind one address.
				Limit:           nil,
				ReservationTTL:  time.Hour,
				MaxReservations: 4096,
				MaxCircuits:     256,
				BufferSize:      64 << 10,
				//lint:ignore SA1019 libp2p means to drop this field, and until it does a peer that reserves again before its place has lapsed needs room for more than one
				MaxReservationsPerPeer: 4,
				MaxReservationsPerIP:   4096,
				MaxReservationsPerASN:  4096,
			})),
		)
	}
	if len(h.relays) > 0 && !cfg.Relay {
		options = append(options, libp2p.ForceReachabilityPrivate())
	}
	h.ctx, h.stop = context.WithCancel(context.Background())
	if cfg.Discover {
		// With a table to ask, a node can be found by its ID alone.
		options = append(options, libp2p.Routing(func(inner host.Host) (routing.PeerRouting, error) {
			var err error
			h.table, err = dht.New(inner,
				// Every node answers as well as asks, so the table needs no
				// servers of its own. It is Sisyphus's, not IPFS's.
				dht.Mode(dht.ModeServer),
				dht.ProtocolPrefix(tablePrefix),
				// The stock table keeps only nodes with public addresses.
				// A pool on one private network has none.
				dht.RoutingTableFilter(func(any, peer.ID) bool { return true }),
				dht.QueryFilter(func(any, peer.AddrInfo) bool { return true }),
				dht.AddressFilter(nil),
			)
			return h.table, err
		}))
	} else {
		options = append(options, libp2p.ConnectionGater(gate(cfg.Allow)))
	}
	started, err := libp2p.New(options...)
	if err != nil {
		h.stop()
		return nil, fmt.Errorf("p2p: %w", err)
	}
	h.host = started
	if cfg.Discover {
		// Nodes on the same network announce themselves to each other.
		// Where multicast is not to be had this finds nobody, which is no
		// reason not to start.
		if h.lan, err = announce(started, found{h}); err != nil {
			cfg.Log.Warn("cannot look for nodes on this network; others must be given this node's address", "error", err)
		}
	}
	// A place on a relay is lost with the connection to it, whatever its
	// time had left to run.
	started.Network().Notify(&network.NotifyBundle{DisconnectedF: func(_ network.Network, conn network.Conn) {
		h.mu.Lock()
		defer h.mu.Unlock()
		delete(h.renew, conn.RemotePeer())
	}})
	if len(h.relays) > 0 {
		h.kept.Add(1)
		go h.keepPlaces(cfg.MoreRelays)
	}
	return h, nil
}

// tablePrefix names the hash table's protocol, and lanService the service
// that nodes announce on their own network. Both are Sisyphus's own, so
// that nodes find each other and not every other libp2p program in reach.
const (
	tablePrefix = "/sisyphus"
	lanService  = "_sisyphus._udp"
)

// meet is how long a host gives a node it has just heard of to answer.
const meet = 10 * time.Second

// found is told of each node announcing itself on this network.
type found struct{ host *Host }

func (f found) HandlePeerFound(info peer.AddrInfo) {
	h := f.host
	h.kept.Add(1)
	go func() {
		defer h.kept.Done()
		ctx, cancel := context.WithTimeout(h.ctx, meet)
		defer cancel()
		// Connecting is what puts the node in the table and in Peers. One
		// that does not answer is forgotten.
		h.call(ctx, info)
	}()
}

// announce starts announcing a host on its own network and listening for
// others doing the same.
var announce = func(h host.Host, each mdns.Notifee) (io.Closer, error) {
	service := mdns.NewMdnsService(h, lanService, each)
	return service, service.Start()
}

// Bootstrap connects the host to the nodes at the given addresses, each a
// multiaddress ending in /p2p/ and the node's ID, and has it ask them who
// else there is. It returns how many of them answered.
func (h *Host) Bootstrap(ctx context.Context, addresses []string) (answered int) {
	for _, address := range addresses {
		if h.Connect(ctx, address) == nil {
			answered++
		}
	}
	if h.table != nil {
		h.table.Bootstrap(ctx)
	}
	return answered
}

// placeCheck is how often a host looks to see that it still has its place
// on a relay.
var placeCheck = 10 * time.Second

// keepPlaces keeps the host connected to its relays and holding a place on
// each, which is what lets a relay bring other nodes to this one, until the
// host closes. A place lasts as long as the connection it was asked for
// on, and then only for a time, so it is asked for again when either runs
// out. The relays are those the host was started with and whatever more
// names besides, asked each time round.
//
// libp2p has a client that does this, but it will only take a place on a
// relay with a public address, and a pool on one private network has none.
func (h *Host) keepPlaces(more func(ctx context.Context) []string) {
	defer h.kept.Done()
	fixed := h.relays
	for {
		relays := fixed
		if more != nil {
			for _, address := range more(h.ctx) {
				// One that is no address, or is this host, is passed over.
				if info, err := peer.AddrInfoFromString(address); err == nil && info.ID != h.host.ID() {
					relays = append(relays[:len(relays):len(relays)], *info)
				}
			}
		}
		h.mu.Lock()
		h.relays = relays
		h.mu.Unlock()
		for _, r := range relays {
			if !h.placed(r.ID, time.Now()) {
				h.takePlace(r)
			}
		}
		select {
		case <-time.After(h.placeCheck):
		case <-h.ctx.Done():
			return
		}
	}
}

func (h *Host) takePlace(r peer.AddrInfo) {
	err := h.host.Connect(h.ctx, r)
	var place *client.Reservation
	if err == nil {
		place, err = client.Reserve(h.ctx, h.host, r)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.renew, r.ID)
	if err == nil {
		// Renew with half its time still to run.
		h.renew[r.ID] = time.Now().Add(time.Until(place.Expiration) / 2)
	}
}

// via returns the relays the host now uses.
func (h *Host) via() []peer.AddrInfo {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.relays
}

// placed reports whether the host holds a place on a relay that is not yet
// due for renewal.
func (h *Host) placed(relay peer.ID, now time.Time) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	renew, held := h.renew[relay]
	return held && now.Before(renew)
}

// Relayed reports whether the host can now be reached through one of its
// relays. It takes a moment after starting.
func (h *Host) Relayed() bool {
	for _, r := range h.via() {
		if h.placed(r.ID, time.Now()) {
			return true
		}
	}
	return false
}

// multiaddrsOf gives a host:port address as the multiaddresses of the TCP
// port to listen on. An address that names no host, such as ":7700", means
// every address the machine has, of both kinds.
func multiaddrsOf(hostport string) ([]string, error) {
	addr, err := net.ResolveTCPAddr("tcp", hostport)
	if err != nil {
		return nil, fmt.Errorf("p2p: listen address: %w", err)
	}
	switch {
	case addr.IP == nil:
		return []string{fmt.Sprintf("/ip4/0.0.0.0/tcp/%d", addr.Port), fmt.Sprintf("/ip6/::/tcp/%d", addr.Port)}, nil
	case addr.IP.To4() != nil:
		return []string{fmt.Sprintf("/ip4/%s/tcp/%d", addr.IP, addr.Port)}, nil
	}
	return []string{fmt.Sprintf("/ip6/%s/tcp/%d", addr.IP, addr.Port)}, nil
}

// Close stops the host and everything using it.
func (h *Host) Close() error {
	h.stop()
	h.kept.Wait()
	if h.table != nil {
		h.lan.Close()
		h.table.Close()
	}
	return h.host.Close()
}

// ID returns the host's peer ID, which is the node's ID.
func (h *Host) ID() string {
	return h.host.ID().String()
}

// Addrs returns the addresses the host can be connected to at, each ending
// in /p2p/ and its ID. A host listening on every interface has one for each.
func (h *Host) Addrs() []string {
	var addrs []string
	for _, addr := range h.host.Addrs() {
		if _, err := addr.ValueForProtocol(ma.P_CIRCUIT); err != nil {
			addrs = append(addrs, addr.String()+"/p2p/"+h.ID())
		}
	}
	return addrs
}

// listening returns the TCP addresses the host accepts connections on. The
// host also counts its relays' as places it listens, which are left out.
func (h *Host) listening() []ma.Multiaddr {
	var addrs []ma.Multiaddr
	for _, addr := range h.host.Network().ListenAddresses() {
		if _, err := addr.ValueForProtocol(ma.P_TCP); err == nil {
			addrs = append(addrs, addr)
		}
	}
	return addrs
}

// Peer is a node this host knows of.
type Peer struct {
	ID string
	// Addrs are the addresses the node is known to have.
	Addrs []string
	// Conns are the addresses of the host's connections to it now, if it
	// has any. One through a relay has /p2p-circuit in it.
	Conns []string
}

// Connected reports whether the host has a connection to the node.
func (p Peer) Connected() bool { return len(p.Conns) > 0 }

// Peers returns the nodes the host knows of, connected or not, ordered by
// ID. A node it has heard nothing of for a while drops out.
func (h *Host) Peers() []Peer {
	var peers []Peer
	for _, id := range h.host.Peerstore().PeersWithAddrs() {
		if id == h.host.ID() {
			continue
		}
		p := Peer{ID: id.String()}
		for _, addr := range h.host.Peerstore().Addrs(id) {
			p.Addrs = append(p.Addrs, addr.String())
		}
		for _, conn := range h.host.Network().ConnsToPeer(id) {
			p.Conns = append(p.Conns, conn.RemoteMultiaddr().String())
		}
		peers = append(peers, p)
	}
	sort.Slice(peers, func(a, b int) bool { return peers[a].ID < peers[b].ID })
	return peers
}

// Connect connects to the node at a multiaddress ending in /p2p/ and its ID.
func (h *Host) Connect(ctx context.Context, address string) error {
	info, err := peer.AddrInfoFromString(address)
	if err != nil {
		return fmt.Errorf("p2p: address %q: %w", address, err)
	}
	return h.call(ctx, *info)
}

// callAgain is how many more times a call that fails is made, and
// callPause the longest wait before each; the shortest is half of it, so
// that a node which is only just starting has time to.
const (
	callAgain = 3
	callPause = 300 * time.Millisecond
)

// call connects to a node, trying again if it fails.
//
// Two nodes that have just found each other may each call the other at the
// same instant, from and to the same two ports. The two calls then become
// one connection on which both sides think they are the caller, and it
// fails for both. Waiting a different time each and calling again is what
// lets one of them get there first.
func (h *Host) call(ctx context.Context, info peer.AddrInfo) error {
	for attempt := 0; ; attempt++ {
		err := h.host.Connect(ctx, info)
		if err == nil || attempt == callAgain {
			return err
		}
		select {
		case <-time.After(callPause/2 + rand.N(callPause/2)):
		case <-ctx.Done():
			return err
		}
		// A node that has just failed to answer is otherwise not called
		// again for some seconds.
		h.host.Network().(*swarm.Swarm).Backoff().Clear(info.ID)
	}
}

// TLSListener returns a listener for the connections arriving on the host's
// port that open with a TLS handshake rather than a libp2p one, which is to
// say the ones meant for the node's gRPC server. A host listening on both
// kinds of address gives one listener for the two.
func (h *Host) TLSListener() (net.Listener, error) {
	all := &tlsListener{conns: make(chan net.Conn), closed: make(chan struct{})}
	for _, addr := range h.listening() {
		gated, err := h.shared.DemultiplexedListen(addr, tcpreuse.DemultiplexedConnType_TLS)
		if err != nil {
			all.Close()
			return nil, fmt.Errorf("p2p: %w", err)
		}
		all.parts = append(all.parts, gated)
		go all.accept(gated)
	}
	return all, nil
}

type tlsListener struct {
	parts  []transport.GatedMaListener
	conns  chan net.Conn
	closed chan struct{}
	once   sync.Once
}

// accept passes on what one of the listener's parts accepts, until that
// part is closed.
func (l *tlsListener) accept(part transport.GatedMaListener) {
	for {
		conn, scope, err := part.Accept()
		if err != nil {
			return
		}
		select {
		case l.conns <- &scoped{Conn: conn, scope: scope}:
		case <-l.closed:
			scope.Done()
			conn.Close()
			return
		}
	}
}

func (l *tlsListener) Accept() (net.Conn, error) {
	select {
	case conn := <-l.conns:
		return conn, nil
	case <-l.closed:
		return nil, net.ErrClosed
	}
}

func (l *tlsListener) Close() error {
	l.once.Do(func() {
		close(l.closed)
		for _, part := range l.parts {
			part.Close()
		}
	})
	return nil
}

func (l *tlsListener) Addr() net.Addr { return l.parts[0].Addr() }

// scoped is a connection the host counted in, which must be counted out
// again when it closes.
type scoped struct {
	net.Conn
	scope network.ConnManagementScope
}

func (c *scoped) Close() error {
	c.scope.Done()
	return c.Conn.Close()
}

// directDial is how long a host that has reached a node through a relay
// tries to reach it directly before making do with the relay.
var directDial = 2 * time.Second

// Dial opens a stream to the node with the given ID, as a connection. If
// the host is not connected to the node it goes through its relays, and
// having found the node there, tries the addresses the node says it has.
func (h *Host) Dial(ctx context.Context, nodeID string, proto protocol.ID) (net.Conn, error) {
	id, err := peer.Decode(nodeID)
	if err != nil {
		return nil, fmt.Errorf("p2p: node ID %q: %w", nodeID, err)
	}
	if h.host.Network().Connectedness(id) != network.Connected {
		through := peer.AddrInfo{ID: id}
		for _, r := range h.via() {
			for _, addr := range r.Addrs {
				through.Addrs = append(through.Addrs, addr.Encapsulate(ma.StringCast("/p2p/"+r.ID.String()+"/p2p-circuit")))
			}
		}
		if err := h.host.Connect(ctx, through); err != nil {
			return nil, fmt.Errorf("p2p: reach node %s: %w", nodeID, err)
		}
		// Connect returns once the two have told each other their
		// addresses. If one of the node's own answers, later streams use
		// it, and if none does nothing is lost.
		direct, cancel := context.WithTimeout(network.WithForceDirectDial(ctx, "leave the relay"), h.directDial)
		h.host.Connect(direct, peer.AddrInfo{ID: id})
		cancel()
	}
	stream, err := h.host.NewStream(ctx, id, proto)
	if err != nil {
		return nil, fmt.Errorf("p2p: open a stream to node %s: %w", nodeID, err)
	}
	return &streamConn{Stream: stream}, nil
}

// Listen returns a listener for the streams other nodes open to this one
// under the given protocol, as connections.
func (h *Host) Listen(proto protocol.ID) net.Listener {
	l := &streamListener{host: h.host, proto: proto, streams: make(chan network.Stream), closed: make(chan struct{})}
	h.host.SetStreamHandler(proto, func(stream network.Stream) {
		select {
		case l.streams <- stream:
		case <-l.closed:
			stream.Reset()
		}
	})
	return l
}

type streamListener struct {
	host    host.Host
	proto   protocol.ID
	streams chan network.Stream
	closed  chan struct{}
}

func (l *streamListener) Accept() (net.Conn, error) {
	select {
	case stream := <-l.streams:
		return &streamConn{Stream: stream}, nil
	case <-l.closed:
		return nil, net.ErrClosed
	}
}

// Close stops the listener. It must be called once only.
func (l *streamListener) Close() error {
	l.host.RemoveStreamHandler(l.proto)
	close(l.closed)
	return nil
}

func (l *streamListener) Addr() net.Addr { return addr(l.host.ID().String()) }

// streamConn is a libp2p stream as a connection.
type streamConn struct{ network.Stream }

func (c *streamConn) LocalAddr() net.Addr  { return addr(c.Conn().LocalPeer().String()) }
func (c *streamConn) RemoteAddr() net.Addr { return addr(c.Conn().RemotePeer().String()) }

// addr is a node's ID standing as a network address.
type addr string

func (addr) Network() string  { return "p2p" }
func (a addr) String() string { return string(a) }

// gate lets through only the nodes allow admits.
func gate(allow func(id string) bool) *gater {
	return &gater{allow: allow}
}

type gater struct{ allow func(id string) bool }

func (g *gater) InterceptPeerDial(p peer.ID) bool { return g.allow(p.String()) }

// Who is at an address is not known until the handshake.
func (*gater) InterceptAddrDial(peer.ID, ma.Multiaddr) bool { return true }
func (*gater) InterceptAccept(network.ConnMultiaddrs) bool  { return true }
func (g *gater) InterceptSecured(_ network.Direction, p peer.ID, _ network.ConnMultiaddrs) bool {
	return g.allow(p.String())
}
func (*gater) InterceptUpgraded(network.Conn) (bool, control.DisconnectReason) { return true, 0 }

// relayRules lets the nodes allow admits be relayed to and from, and no
// others.
func relayRules(allow func(id string) bool) relay.ACLFilter {
	return acl{allow}
}

type acl struct{ allow func(id string) bool }

func (a acl) AllowReserve(p peer.ID, _ ma.Multiaddr) bool { return a.allow(p.String()) }
func (a acl) AllowConnect(src peer.ID, _ ma.Multiaddr, dest peer.ID) bool {
	return a.allow(src.String()) && a.allow(dest.String())
}

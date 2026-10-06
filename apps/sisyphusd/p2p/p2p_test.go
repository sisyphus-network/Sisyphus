package p2p

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"errors"
	"io"
	"log/slog"
	"net"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/p2p/discovery/mdns"

	"github.com/sisyphus-network/Sisyphus/packages/identity"
)

const echoProtocol = "/sisyphus/test-echo/1"

var quiet = slog.New(slog.DiscardHandler)

func newIdentity(t *testing.T) *identity.Identity {
	t.Helper()
	ident, _, err := identity.LoadOrCreate(filepath.Join(t.TempDir(), "node.key"))
	if err != nil {
		t.Fatal(err)
	}
	return ident
}

// pool is a set of nodes that admit each other and nobody else.
type pool struct {
	t       *testing.T
	mu      sync.Mutex
	members map[string]bool
}

func newPool(t *testing.T) *pool {
	return &pool{t: t, members: make(map[string]bool)}
}

func (p *pool) allow(id string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.members[id]
}

func (p *pool) admit(id string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.members[id] = true
}

// start starts a host for a new member of the pool.
func (p *pool) start(cfg Config) *Host {
	p.t.Helper()
	cfg.Identity = newIdentity(p.t)
	cfg.Allow = p.allow
	cfg.Log = quiet
	p.admit(cfg.Identity.ID())
	h, err := New(cfg)
	if err != nil {
		p.t.Fatal(err)
	}
	p.t.Cleanup(func() { h.Close() })
	return h
}

// relay starts a node that others can reach and that relays for the pool.
func (p *pool) relay() *Host {
	return p.start(Config{Listen: "127.0.0.1:0", Relay: true})
}

// behind starts a node that is reached through the given relay, and waits
// until it can be.
func (p *pool) behind(relay *Host) *Host {
	p.t.Helper()
	h := p.start(Config{Via: relay.Addrs()})
	waitFor(p.t, h.Relayed)
	return h
}

func waitFor(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for !condition() {
		if time.Now().After(deadline) {
			t.Fatal("condition not met in time")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// echo answers, on every stream a node is opened under the test protocol,
// with whatever it is sent.
func echo(t *testing.T, h *Host) net.Listener {
	t.Helper()
	lis := h.Listen(echoProtocol)
	go func() {
		for {
			conn, err := lis.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				io.Copy(conn, conn)
			}()
		}
	}()
	return lis
}

// roundTrip sends size random bytes down conn and checks they come back.
func roundTrip(t *testing.T, conn net.Conn, size int) {
	t.Helper()
	sent := make([]byte, size)
	rand.Read(sent)
	go conn.Write(sent)
	got := make([]byte, size)
	conn.SetReadDeadline(time.Now().Add(60 * time.Second))
	if _, err := io.ReadFull(conn, got); err != nil {
		t.Fatalf("reading the echo: %v", err)
	}
	if !bytes.Equal(got, sent) {
		t.Fatal("what came back is not what was sent")
	}
}

// routes returns the addresses of a host's connections to another.
func routes(h, to *Host) []string {
	for _, peer := range h.Peers() {
		if peer.ID == to.ID() {
			return peer.Conns
		}
	}
	return nil
}

func relayed(addrs []string) bool {
	return slices.ContainsFunc(addrs, func(a string) bool { return strings.Contains(a, "/p2p-circuit") })
}

func direct(addrs []string) bool {
	return slices.ContainsFunc(addrs, func(a string) bool { return !strings.Contains(a, "/p2p-circuit") })
}

func TestTwoNodesWithNoOpenPortReachEachOtherThroughARelay(t *testing.T) {
	// Leave the relay in place, as two nodes on different private networks
	// would have to.
	old := directDial
	directDial = 0
	defer func() { directDial = old }()

	p := newPool(t)
	relay := p.relay()
	a, b := p.behind(relay), p.behind(relay)
	echo(t, b)

	conn, err := a.Dial(context.Background(), b.ID(), echoProtocol)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	// Far more than a relay meant only for introductions would carry.
	roundTrip(t, conn, 8<<20)

	if got := routes(a, b); !relayed(got) || direct(got) {
		t.Errorf("a's connections to b: %v, want only one through the relay", got)
	}
	if conn.RemoteAddr().String() != b.ID() || conn.LocalAddr().String() != a.ID() || conn.RemoteAddr().Network() != "p2p" {
		t.Errorf("the connection is from %v to %v", conn.LocalAddr(), conn.RemoteAddr())
	}
}

func TestNodesThatCanReachEachOtherLeaveTheRelay(t *testing.T) {
	p := newPool(t)
	relay := p.relay()
	a, b := p.behind(relay), p.behind(relay)
	echo(t, b)

	// All a knows of b is its ID.
	conn, err := a.Dial(context.Background(), b.ID(), echoProtocol)
	if err != nil {
		t.Fatal(err)
	}
	roundTrip(t, conn, 1<<10)
	conn.Close()
	// The relay introduced them, and they found that they share a network.
	if got := routes(a, b); !direct(got) {
		t.Fatalf("a's connections to b after meeting: %v, want a direct one", got)
	}
	// Streams opened from then on do not touch the relay.
	relay.Close()
	again, err := a.Dial(context.Background(), b.ID(), echoProtocol)
	if err != nil {
		t.Fatal(err)
	}
	defer again.Close()
	roundTrip(t, again, 1<<20)
}

func TestOnePortServesBothTLSAndLibp2p(t *testing.T) {
	p := newPool(t)
	node := p.start(Config{Listen: "127.0.0.1:0", Relay: true})
	ident := newIdentity(t)
	lis, err := node.TLSListener()
	if err != nil {
		t.Fatal(err)
	}
	defer lis.Close()
	server := tls.NewListener(lis, ident.ServerTLS())
	go func() {
		for {
			conn, err := server.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				io.Copy(conn, conn)
			}()
		}
	}()

	// The port, as plain host:port, from the libp2p address.
	fields := strings.Split(node.Addrs()[0], "/")
	hostport := net.JoinHostPort(fields[2], fields[4])
	if !strings.HasSuffix(node.Addrs()[0], "/p2p/"+node.ID()) || lis.Addr().String() != hostport {
		t.Fatalf("libp2p listens at %v and TLS at %v", node.Addrs(), lis.Addr())
	}

	// A TLS client and a libp2p node use it at once.
	client, err := tls.Dial("tcp", hostport, newIdentity(t).ClientTLS(ident.ID()))
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	other := p.start(Config{})
	echo(t, node)
	if err := other.Connect(context.Background(), node.Addrs()[0]); err != nil {
		t.Fatal(err)
	}
	stream, err := other.Dial(context.Background(), node.ID(), echoProtocol)
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	roundTrip(t, client, 1<<20)
	roundTrip(t, stream, 1<<20)

	// The port's TLS side can be handed out once.
	if _, err := node.TLSListener(); err == nil {
		t.Error("a second listener for the same connections was handed out")
	}
	lis.Close()
	if _, err := lis.Accept(); err == nil {
		t.Error("a closed listener accepted a connection")
	}
}

func TestOnlyMembersGetIn(t *testing.T) {
	p := newPool(t)
	relay := p.relay()
	member := p.behind(relay)
	echo(t, member)
	ctx := context.Background()

	// A node the pool has not admitted, which admits everyone itself.
	stranger, err := New(Config{Identity: newIdentity(t), Via: relay.Addrs(), Allow: func(string) bool { return true }})
	if err != nil {
		t.Fatal(err)
	}
	defer stranger.Close()
	// The relay learns who is calling from the handshake, and hangs up.
	stranger.Connect(ctx, relay.Addrs()[0])
	waitFor(t, func() bool { return len(routes(stranger, relay)) == 0 && len(routes(relay, stranger)) == 0 })
	if _, err := stranger.Dial(ctx, member.ID(), echoProtocol); err == nil {
		t.Error("a node that is no member reached a member through the relay")
	}
	if stranger.Relayed() {
		t.Error("a node that is no member was given a place on the relay")
	}
	// And a member does not go looking for one that is not.
	if _, err := member.Dial(ctx, stranger.ID(), echoProtocol); err == nil || !strings.Contains(err.Error(), "reach node") {
		t.Errorf("a member dialling a stranger: %v", err)
	}

	// A relay carries nothing to a node it would not admit, even for a
	// node it would. The asker here is laxer than its pool.
	lax, err := New(Config{Identity: newIdentity(t), Via: relay.Addrs(), Allow: func(string) bool { return true }})
	if err != nil {
		t.Fatal(err)
	}
	defer lax.Close()
	p.admit(lax.ID())
	if _, err := lax.Dial(ctx, stranger.ID(), echoProtocol); err == nil {
		t.Error("the relay carried a connection to a node that is no member")
	}
}

func TestDialsThatFail(t *testing.T) {
	p := newPool(t)
	relay := p.relay()
	a, b := p.behind(relay), p.behind(relay)
	ctx := context.Background()

	if _, err := a.Dial(ctx, "not-a-node-id", echoProtocol); err == nil || !strings.Contains(err.Error(), "node ID") {
		t.Errorf("dialling something that is no node ID: %v", err)
	}
	// b is there but answers to no such protocol.
	conn, err := a.Dial(ctx, b.ID(), echoProtocol)
	if err == nil {
		// The refusal may come on first use rather than on opening.
		conn.SetReadDeadline(time.Now().Add(30 * time.Second))
		conn.Write([]byte("hello"))
		_, err = conn.Read(make([]byte, 1))
	}
	if err == nil {
		t.Error("a stream under a protocol nobody serves worked")
	}
	if err := a.Connect(ctx, "not an address"); err == nil || !strings.Contains(err.Error(), "address") {
		t.Errorf("connecting to something that is no address: %v", err)
	}
}

func TestAListenerThatClosesTurnsAwayWhatItHadNotTaken(t *testing.T) {
	p := newPool(t)
	relay := p.relay()
	a := p.behind(relay)
	// The relay listens but takes nothing.
	lis := relay.Listen(echoProtocol)
	if lis.Addr().String() != relay.ID() {
		t.Errorf("the listener's address is %v", lis.Addr())
	}
	conn, err := a.Dial(context.Background(), relay.ID(), echoProtocol)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.Write([]byte("anyone?"))
	// Give the stream time to arrive and wait to be taken.
	time.Sleep(200 * time.Millisecond)
	lis.Close()
	conn.SetReadDeadline(time.Now().Add(30 * time.Second))
	if _, err := conn.Read(make([]byte, 1)); err == nil {
		t.Error("a stream nobody took was answered")
	}
	if _, err := lis.Accept(); err != net.ErrClosed {
		t.Errorf("Accept on a closed listener: %v", err)
	}
}

func TestHostsThatCannotStart(t *testing.T) {
	ident := newIdentity(t)
	everyone := func(string) bool { return true }
	taken, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer taken.Close()
	for name, cfg := range map[string]Config{
		"a listen address that is none": {Listen: "nonsense"},
		"a listen address in use":       {Listen: taken.Addr().String()},
		"a relay address that is none":  {Via: []string{"/ip4/127.0.0.1/tcp/1"}},
	} {
		cfg.Identity, cfg.Allow = ident, everyone
		if h, err := New(cfg); err == nil {
			h.Close()
			t.Errorf("%s: the host started", name)
		}
	}
}

func TestListenAddressesAsMultiaddresses(t *testing.T) {
	for hostport, want := range map[string]string{
		"127.0.0.1:7700":     "/ip4/127.0.0.1/tcp/7700",
		":7700":              "/ip4/0.0.0.0/tcp/7700",
		"0.0.0.0:7700":       "/ip4/0.0.0.0/tcp/7700",
		"[::1]:7700":         "/ip6/::1/tcp/7700",
		"[2001:db8::1]:7700": "/ip6/2001:db8::1/tcp/7700",
	} {
		if got, err := multiaddrOf(hostport); err != nil || got != want {
			t.Errorf("multiaddrOf(%q) = %q, %v; want %q", hostport, got, err, want)
		}
	}
}

func TestANodeTakesItsPlaceOnTheRelayAgainAfterLosingIt(t *testing.T) {
	old := placeCheck
	placeCheck = 20 * time.Millisecond
	defer func() { placeCheck = old }()

	p := newPool(t)
	relay := p.relay()
	a, b := p.behind(relay), p.behind(relay)
	echo(t, b)

	// The relay drops b, as a restart or a break in the network would.
	if err := relay.host.Network().ClosePeer(b.host.ID()); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return !b.Relayed() })
	waitFor(t, b.Relayed)
	conn, err := a.Dial(context.Background(), b.ID(), echoProtocol)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	roundTrip(t, conn, 1<<16)
}

// knows reports whether a host knows of another and, if it must be, is
// connected to it.
func knows(h, other *Host, connected bool) bool {
	for _, peer := range h.Peers() {
		if peer.ID == other.ID() {
			return peer.Connected() || !connected
		}
	}
	return false
}

func TestNodesOnOneNetworkFindEachOther(t *testing.T) {
	p := newPool(t)
	a := p.start(Config{Discover: true})
	b := p.start(Config{Discover: true})
	// Neither was told of the other.
	waitFor(t, func() bool { return knows(a, b, true) && knows(b, a, true) })

	for _, peer := range a.Peers() {
		if peer.ID == a.ID() {
			t.Error("a host lists itself among its peers")
		}
		if peer.ID == b.ID() && (len(peer.Addrs) == 0 || len(peer.Conns) == 0) {
			t.Errorf("a knows b as %+v, want its addresses and a connection", peer)
		}
	}
	// A node that does not discover is not found, and finds nobody.
	hidden := p.start(Config{})
	time.Sleep(300 * time.Millisecond)
	if knows(a, hidden, false) || len(hidden.Peers()) != 0 {
		t.Errorf("a node that does not discover was found, or found others: %v", hidden.Peers())
	}
}

// withoutLAN stops hosts started during a test from announcing themselves
// on the network, so that what they find, they find another way.
func withoutLAN(t *testing.T, err error) {
	old := announce
	announce = func(host.Host, mdns.Notifee) (io.Closer, error) { return io.NopCloser(nil), err }
	t.Cleanup(func() { announce = old })
}

func TestANodeFindsOthersThroughTheNodesItKnows(t *testing.T) {
	withoutLAN(t, nil)
	p := newPool(t)
	a := p.start(Config{Discover: true})
	b := p.start(Config{Listen: "127.0.0.1:0", Discover: true})
	c := p.start(Config{Discover: true})
	echo(t, c)
	ctx := context.Background()

	// c knows b, and then a is told of b and nothing else.
	if n := c.Bootstrap(ctx, b.Addrs()); n != 1 {
		t.Fatalf("c reached %d of the one node it was given", n)
	}
	if n := a.Bootstrap(ctx, append(b.Addrs(), "/ip4/127.0.0.1/tcp/1/p2p/"+newIdentity(t).ID(), "not an address")); n != 1 {
		t.Fatalf("a reached %d of the nodes it was given, want only the one that is there", n)
	}
	// Each takes a moment to enter the others it has met in its table.
	waitFor(t, func() bool { return a.table.RoutingTable().Size() > 0 && b.table.RoutingTable().Size() == 2 })
	// a can reach c by its ID alone: b tells it where c is.
	conn, err := a.Dial(ctx, c.ID(), echoProtocol)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	roundTrip(t, conn, 1<<16)
	if !knows(a, c, true) {
		t.Errorf("a's peers after reaching c: %+v", a.Peers())
	}
}

func TestAStrangerMayConnectToANodeThatDiscoversButIsNotRelayedFor(t *testing.T) {
	withoutLAN(t, nil)
	p := newPool(t)
	relay := p.start(Config{Listen: "127.0.0.1:0", Relay: true, Discover: true})
	member := p.behind(relay)
	echo(t, member)
	ctx := context.Background()

	stranger, err := New(Config{Identity: newIdentity(t), Via: relay.Addrs(), Allow: func(string) bool { return true }, Log: quiet})
	if err != nil {
		t.Fatal(err)
	}
	defer stranger.Close()
	if err := stranger.Connect(ctx, relay.Addrs()[0]); err != nil {
		t.Fatalf("a stranger connecting to a node that discovers: %v", err)
	}
	waitFor(t, func() bool { return knows(relay, stranger, true) })
	// Being let in is not being carried for.
	if _, err := stranger.Dial(ctx, member.ID(), echoProtocol); err == nil {
		t.Error("the relay carried a stranger to a member")
	}
	if stranger.Relayed() {
		t.Error("a stranger was given a place on the relay")
	}
}

func TestANodeThatCannotAnnounceItselfStartsAllTheSame(t *testing.T) {
	withoutLAN(t, errors.New("no multicast here"))
	logs := new(bytes.Buffer)
	h, err := New(Config{Identity: newIdentity(t), Allow: func(string) bool { return true }, Discover: true, Log: slog.New(slog.NewTextHandler(logs, nil))})
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	if !strings.Contains(logs.String(), "cannot look for nodes on this network") || !strings.Contains(logs.String(), "no multicast here") {
		t.Errorf("logged:\n%s", logs)
	}
}

func TestACallThatFailsIsMadeAgain(t *testing.T) {
	withoutLAN(t, nil)
	p := newPool(t)
	a := p.start(Config{Discover: true})
	// A node that is not there yet when first called, at an address that
	// nothing is listening on.
	port := freePort(t)
	identity := newIdentity(t)
	p.admit(identity.ID())
	late := make(chan *Host, 1)
	go func() {
		time.Sleep(50 * time.Millisecond)
		h, err := New(Config{Identity: identity, Listen: "127.0.0.1:" + port, Allow: p.allow, Discover: true, Log: quiet})
		if err != nil {
			t.Error(err)
		}
		late <- h
	}()
	address := "/ip4/127.0.0.1/tcp/" + port + "/p2p/" + identity.ID()
	err := a.Connect(context.Background(), address)
	if h := <-late; h != nil {
		defer h.Close()
	}
	if err != nil {
		t.Errorf("a call to a node that came up a moment later: %v", err)
	}

	// A node that never comes is given up on, soon.
	nobody := "/ip4/127.0.0.1/tcp/" + freePort(t) + "/p2p/" + newIdentity(t).ID()
	started := time.Now()
	if err := a.Connect(context.Background(), nobody); err == nil {
		t.Error("a call to nobody succeeded")
	}
	if took := time.Since(started); took > 10*time.Second {
		t.Errorf("gave up on nobody after %v", took)
	}
	// And at once if the caller has stopped waiting.
	stopped, cancel := context.WithCancel(context.Background())
	cancel()
	if err := a.Connect(stopped, nobody); err == nil {
		t.Error("a call nobody was waiting for succeeded")
	}
}

// freePort returns a port nothing is listening on.
func freePort(t *testing.T) string {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer lis.Close()
	_, port, _ := net.SplitHostPort(lis.Addr().String())
	return port
}

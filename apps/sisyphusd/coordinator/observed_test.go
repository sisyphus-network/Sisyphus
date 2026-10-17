package coordinator

import (
	"context"
	"log/slog"
	"net"
	"net/netip"
	"testing"

	"google.golang.org/grpc/peer"

	"github.com/sisyphus-network/Sisyphus/packages/runtime"
	"github.com/sisyphus-network/Sisyphus/packages/storage"
)

// Where a call comes from is its connection's address, in full; a
// connection carried over something with no address has none.
func TestWhereACallComesFrom(t *testing.T) {
	for name, tt := range map[string]struct {
		ctx  context.Context
		want netip.Addr
	}{
		"over IPv4":            {from(&net.TCPAddr{IP: net.ParseIP("8.8.8.8"), Port: 51234}), netip.MustParseAddr("8.8.8.8")},
		"over IPv6, in full":   {from(&net.TCPAddr{IP: net.ParseIP("2001:67c:2e8:22::c100:68b"), Port: 1}), netip.MustParseAddr("2001:67c:2e8:22::c100:68b")},
		"IPv4 written as IPv6": {from(&net.TCPAddr{IP: net.ParseIP("::ffff:8.8.8.8"), Port: 1}), netip.MustParseAddr("8.8.8.8")},
		"over a relay":         {from(nodeAddr("12D3KooW")), netip.Addr{}},
		"from nowhere":         {context.Background(), netip.Addr{}},
	} {
		if got := callersSource(tt.ctx); got != tt.want {
			t.Errorf("%s: %v, want %v", name, got, tt.want)
		}
	}
}

func from(addr net.Addr) context.Context {
	return peer.NewContext(context.Background(), &peer.Peer{Addr: addr})
}

// nodeAddr stands for the address of a connection carried by libp2p, which
// is a node's ID and no network address.
type nodeAddr string

func (a nodeAddr) Network() string { return "libp2p" }
func (a nodeAddr) String() string  { return string(a) }

// What a worker is told of where it is, and what the coordinator takes
// from a worker, are global addresses or nothing.
func TestOnlyAGlobalAddressIsToldOrTaken(t *testing.T) {
	if got := seenAt(netip.MustParseAddr("::ffff:8.8.8.8")); got != "8.8.8.8" {
		t.Errorf("told %q", got)
	}
	for _, addr := range []netip.Addr{{}, netip.MustParseAddr("10.0.0.5"), netip.MustParseAddr("100.64.1.1"), netip.MustParseAddr("127.0.0.1")} {
		if got := seenAt(addr); got != "" {
			t.Errorf("a worker at %v is told it is at %q", addr, got)
		}
	}
	if got := global("::ffff:193.0.6.139"); got != netip.MustParseAddr("193.0.6.139") {
		t.Errorf("took %v", got)
	}
	for _, said := range []string{"", "not an address", "192.168.1.1", "100.64.0.1", "fd00::1", "2001:db8::1"} {
		if got := global(said); got.IsValid() {
			t.Errorf("took %q as %v", said, got)
		}
	}
}

// The coordinator is where most of its workers say they reached it, and
// nowhere when no address is said more than another.
func TestTheCoordinatorIsWhereMostOfItsWorkersReachedIt(t *testing.T) {
	c := New(Config{Workloads: runtime.Builtin(), Store: storage.NewMemory(), Log: slog.New(slog.DiscardHandler)})
	defer c.Close()
	at := func(dialled ...string) netip.Addr {
		c.mu.Lock()
		c.workers = make(map[string]*worker)
		for i, said := range dialled {
			c.workers[string(rune('a'+i))] = &worker{dialled: global(said)}
		}
		c.mu.Unlock()
		return c.Observed()
	}
	a, b := netip.MustParseAddr("193.0.6.139"), netip.MustParseAddr("8.8.4.4")
	for _, tt := range []struct {
		dialled []string
		want    netip.Addr
	}{
		{nil, netip.Addr{}},
		{[]string{""}, netip.Addr{}},
		{[]string{"193.0.6.139"}, a},
		{[]string{"193.0.6.139", "10.0.0.1", ""}, a},
		{[]string{"193.0.6.139", "8.8.4.4"}, netip.Addr{}},
		{[]string{"193.0.6.139", "8.8.4.4", "8.8.4.4"}, b},
		{[]string{"8.8.4.4", "193.0.6.139", "193.0.6.139", "8.8.4.4", "193.0.6.139"}, a},
	} {
		if got := at(tt.dialled...); got != tt.want {
			t.Errorf("workers that reached it at %v: %v, want %v", tt.dialled, got, tt.want)
		}
	}
}

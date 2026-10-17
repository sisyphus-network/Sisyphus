package worker

import (
	"context"
	"net"
	"testing"

	"google.golang.org/grpc/peer"
)

// The address a stream reached is its connection's; one carried over
// something with no address of its own gives none.
func TestTheAddressAStreamReached(t *testing.T) {
	for name, tt := range map[string]struct {
		ctx  context.Context
		want string
	}{
		"over TCP":     {peer.NewContext(context.Background(), &peer.Peer{Addr: &net.TCPAddr{IP: net.ParseIP("193.0.6.139"), Port: 7700}}), "193.0.6.139"},
		"over IPv6":    {peer.NewContext(context.Background(), &peer.Peer{Addr: &net.TCPAddr{IP: net.ParseIP("2001:67c:2e8::1"), Port: 7700}}), "2001:67c:2e8::1"},
		"over a relay": {peer.NewContext(context.Background(), &peer.Peer{Addr: relayed("12D3KooW")}), ""},
		"with no peer": {context.Background(), ""},
	} {
		if got := dialled(tt.ctx); got != tt.want {
			t.Errorf("%s: %q, want %q", name, got, tt.want)
		}
	}
}

// relayed stands for the address of a connection carried by libp2p, which
// is a node's ID and no network address.
type relayed string

func (r relayed) Network() string { return "libp2p" }
func (r relayed) String() string  { return string(r) }

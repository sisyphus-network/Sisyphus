package coordinator

import (
	"context"
	"net"
	"testing"

	"google.golang.org/grpc/peer"
)

// named is an address that is whatever text a test gives it.
type named string

func (n named) Network() string { return "test" }
func (n named) String() string  { return string(n) }

// Workers are told apart by where they connect from: an IPv4 address as it
// is, an IPv6 address by its /64, which is what one site is given.
func TestWhereACallerIs(t *testing.T) {
	tests := []struct {
		name string
		from net.Addr
		want string
	}{
		{"an IPv4 address", &net.TCPAddr{IP: net.IPv4(203, 0, 113, 5), Port: 40000}, "203.0.113.5"},
		{"the same address from another port", &net.TCPAddr{IP: net.IPv4(203, 0, 113, 5), Port: 40001}, "203.0.113.5"},
		{"an IPv4 address written as IPv6", named("[::ffff:203.0.113.5]:40000"), "203.0.113.5"},
		{"an IPv6 address", &net.TCPAddr{IP: net.ParseIP("2001:db8:1:2:3:4:5:6"), Port: 40000}, "2001:db8:1:2::/64"},
		{"another in the same /64", &net.TCPAddr{IP: net.ParseIP("2001:db8:1:2::9"), Port: 40000}, "2001:db8:1:2::/64"},
		{"one in the next /64", &net.TCPAddr{IP: net.ParseIP("2001:db8:1:3::9"), Port: 40000}, "2001:db8:1:3::/64"},
		{"an address with no port", named("203.0.113.5"), ""},
		{"a name rather than an address", named("worker.example:40000"), ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := peer.NewContext(context.Background(), &peer.Peer{Addr: tt.from})
			if got := callersAddress(ctx); got != tt.want {
				t.Errorf("callersAddress = %q, want %q", got, tt.want)
			}
		})
	}
	if got := callersAddress(context.Background()); got != "" {
		t.Errorf("a call from nowhere is at %q", got)
	}
}

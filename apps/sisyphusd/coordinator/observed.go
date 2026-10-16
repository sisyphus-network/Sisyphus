package coordinator

import (
	"context"
	"net/netip"

	"google.golang.org/grpc/peer"

	"github.com/sisyphus-network/Sisyphus/packages/geo"
)

// Where the pool sees its members.
//
// A node behind a NAT has only private addresses of its own, and cannot
// tell from them where it is. The node it is connected to can: the
// coordinator sees each worker's source address, and each worker the
// address it reached the coordinator at. Each tells the other, and each
// takes what it is told only if it is a global address, never one that is
// private, shared by a carrier's NAT, or carried over a tunnel or relay,
// which has no address of its own to give. What either learns is for
// showing where a node is, as the pool sees it; it decides nothing, and is
// never taken as an address the node can be reached at.
//
// A worker hears from one coordinator, which it already trusts with its
// work, and takes its word. A coordinator hears from many workers, any of
// which could be wrong, and takes the address most of them agree on.

// callersSource is the address a call comes from, as its connection has it,
// or the zero address if it has none.
func callersSource(ctx context.Context) netip.Addr {
	from, ok := peer.FromContext(ctx)
	if !ok {
		return netip.Addr{}
	}
	at, err := netip.ParseAddrPort(from.Addr.String())
	if err != nil {
		return netip.Addr{}
	}
	return at.Addr().Unmap()
}

// seenAt is what a worker is told of the address it connects from: the
// address, if it is a global one, and otherwise nothing.
func seenAt(addr netip.Addr) string {
	if !geo.Global(addr) {
		return ""
	}
	return addr.Unmap().String()
}

// global parses an address a worker gives, and keeps it only if it is a
// global one.
func global(s string) netip.Addr {
	addr, err := netip.ParseAddr(s)
	if err != nil || !geo.Global(addr) {
		return netip.Addr{}
	}
	return addr.Unmap()
}

// Observed returns this node's address as its workers reach it: the one
// most of those connected now say they reached it at. It is the zero
// address if none says, or if no address is said by more than any other.
func (c *Coordinator) Observed() netip.Addr {
	c.mu.Lock()
	defer c.mu.Unlock()
	said := make(map[netip.Addr]int)
	for _, w := range c.workers {
		if w.dialled.IsValid() {
			said[w.dialled]++
		}
	}
	var most netip.Addr
	tied := false
	for addr, n := range said {
		switch {
		case n > said[most]:
			most, tied = addr, false
		case n == said[most]:
			tied = true
		}
	}
	if tied {
		return netip.Addr{}
	}
	return most
}

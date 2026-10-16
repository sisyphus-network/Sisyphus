package main

import (
	"context"
	"net/netip"
	"sync/atomic"
	"testing"

	"github.com/sisyphus-network/Sisyphus/packages/runtime"
)

// Each side of a worker's connection tells the other where it sees it: a
// worker is told the address it connects from, if that is a global one,
// and the coordinator places itself where most of its workers say they
// reached it.
func TestThePoolSaysWhereItSeesEachNode(t *testing.T) {
	sources := []netip.Addr{netip.MustParseAddr("8.8.8.8"), netip.MustParseAddr("10.0.0.5"), netip.MustParseAddr("100.64.0.9"), netip.MustParseAddr("2001:67c:2e8::1")}
	var next atomic.Int32
	workersFrom = func(context.Context) netip.Addr { return sources[next.Add(1)-1] }
	defer func() { workersFrom = nil }()
	p := startPool(t, runtime.Builtin())

	join := func(name, dialled string) (*rawWorker, string) {
		hi := hello(name, 1, "primes")
		hi.GetHello().DialledAddress = dialled
		w := p.connectRaw(hi)
		msg, err := w.stream.Recv()
		if err != nil || msg.GetWelcome() == nil {
			t.Fatalf("%s was not welcomed: %v, %v", name, msg, err)
		}
		return w, msg.GetWelcome().GetObservedAddress()
	}
	// The first worker connects from a global address, and is told it.
	_, told := join("one", "193.0.6.139")
	if told != "8.8.8.8" {
		t.Errorf("a worker at 8.8.8.8 was told %q", told)
	}
	if got := p.coord.Observed(); got != netip.MustParseAddr("193.0.6.139") {
		t.Errorf("with one worker, the coordinator is at %v", got)
	}
	// One at a private address, or behind a carrier's NAT, is told nothing,
	// and what it says that is no global address counts for nothing.
	for _, name := range []string{"two", "three"} {
		if _, told := join(name, "203.0.113.4"); told != "" {
			t.Errorf("%s was told %q", name, told)
		}
	}
	if got := p.coord.Observed(); got != netip.MustParseAddr("193.0.6.139") {
		t.Errorf("the coordinator is at %v", got)
	}
	// Two that disagree, one each, place it nowhere.
	four, told := join("four", "8.8.4.4")
	if told != "2001:67c:2e8::1" {
		t.Errorf("a worker at an IPv6 address was told %q", told)
	}
	if got := p.coord.Observed(); got.IsValid() {
		t.Errorf("with two workers that disagree, the coordinator is at %v", got)
	}
	// Once the one that disagreed is gone, the other's word stands again.
	four.cancel()
	waitFor(t, func() bool { return p.coord.Observed() == netip.MustParseAddr("193.0.6.139") })
}

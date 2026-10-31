package runtime

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"sync"
	"time"
)

// A mining task reaches one address on the network and no other: its
// pool's, resolved once on the worker before the task starts. Its container
// has no network of its own (Docker's "none"), so the only way out is a
// Unix socket the worker gives it, behind which the worker connects to that
// one address and port and copies bytes both ways. A job names the pool,
// and so whoever submits one could otherwise point a miner at the worker
// owner's own network: the router's page, a printer, a NAS. An address on
// such a network is refused, and so is any a name resolves to there.

// poolRefused are the networks a pool's address may not be in: none of
// them is a public pool on the Internet. Loopback, private (RFC 1918 and
// IPv6's unique local fc00::/7), link-local, shared address space (CGNAT,
// 100.64.0.0/10), multicast and unspecified addresses, and the ranges kept
// for documentation, benchmarks and the future; and the IPv6 ranges that
// carry an IPv4 address inside them (NAT64, 6to4, Teredo, IPv4-compatible),
// through which an IPv4 address refused above could be reached.
var poolRefused = func() []netip.Prefix {
	var prefixes []netip.Prefix
	for _, p := range []string{
		"0.0.0.0/8",       // this network
		"10.0.0.0/8",      // private
		"100.64.0.0/10",   // shared address space, CGNAT
		"127.0.0.0/8",     // loopback
		"169.254.0.0/16",  // link-local
		"172.16.0.0/12",   // private
		"192.0.0.0/24",    // IETF protocol assignments
		"192.0.2.0/24",    // documentation
		"192.88.99.0/24",  // 6to4 relay anycast
		"192.168.0.0/16",  // private
		"198.18.0.0/15",   // benchmarking
		"198.51.100.0/24", // documentation
		"203.0.113.0/24",  // documentation
		"224.0.0.0/4",     // multicast
		"240.0.0.0/4",     // reserved, and the broadcast address
		"::/96",           // unspecified, loopback and IPv4-compatible
		"::ffff:0:0/96",   // IPv4-mapped, which are unmapped before this is asked
		"64:ff9b::/96",    // NAT64
		"64:ff9b:1::/48",  // local NAT64
		"100::/64",        // discard
		"2001::/32",       // Teredo
		"2001:2::/48",     // benchmarking
		"2001:db8::/32",   // documentation
		"2002::/16",       // 6to4
		"3fff::/20",       // documentation
		"5f00::/16",       // segment routing
		"fc00::/7",        // unique local
		"fe80::/10",       // link-local
		"fec0::/10",       // site-local, deprecated
		"ff00::/8",        // multicast
	} {
		prefixes = append(prefixes, netip.MustParsePrefix(p))
	}
	return prefixes
}()

// poolAddressRefused says why a pool may not be reached at addr, or nothing
// if it may: it must be an address on the public Internet.
func poolAddressRefused(addr netip.Addr) error {
	addr = addr.Unmap()
	if !addr.IsValid() {
		return errors.New("no address")
	}
	if addr.Zone() != "" {
		return fmt.Errorf("%s has a zone, which only an address on this machine's own links has", addr)
	}
	for _, p := range poolRefused {
		if p.Contains(addr) {
			return fmt.Errorf("%s is not an address on the public Internet (it is in %s), and a mining pool may only be on it", addr, p)
		}
	}
	return nil
}

// poolResolver is what resolves a pool's name: Go's own, or a test's.
type poolResolver interface {
	LookupNetIP(ctx context.Context, network, host string) ([]netip.Addr, error)
}

// resolvePool finds the one address a task may reach its pool at: the
// pool's address if it names one, or the first of those its name resolves
// to that is on the public Internet. It is resolved once, here, and the
// task reaches only this, so that a name that later resolves to somewhere
// else does not take the miner with it. refused says which addresses are
// not to be reached.
func resolvePool(ctx context.Context, resolver poolResolver, host string, port uint16, refused func(netip.Addr) error) (netip.AddrPort, error) {
	if addr, err := netip.ParseAddr(host); err == nil {
		if err := refused(addr); err != nil {
			return netip.AddrPort{}, fmt.Errorf("the pool: %w", err)
		}
		return netip.AddrPortFrom(addr.Unmap(), port), nil
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	addrs, err := resolver.LookupNetIP(ctx, "ip", host)
	if err != nil {
		return netip.AddrPort{}, fmt.Errorf("find the pool %s: %w", host, err)
	}
	var why []error
	for _, addr := range addrs {
		err := refused(addr)
		if err == nil {
			return netip.AddrPortFrom(addr.Unmap(), port), nil
		}
		why = append(why, err)
	}
	return netip.AddrPort{}, fmt.Errorf("the pool %s has no address a miner may reach: %w", host, errors.Join(why...))
}

// poolForwarder takes connections on a Unix socket and joins each to a
// new connection to one address and port, at most poolMostConnections at
// once: a miner keeps one connection to its pool, and makes another only
// when that one fails.
type poolForwarder struct {
	listener net.Listener
	to       netip.AddrPort
	dial     func(ctx context.Context, network, address string) (net.Conn, error)

	mu     sync.Mutex
	open   map[net.Conn]bool
	closed bool
	done   sync.WaitGroup
	// Slots counts the connections to the pool being made or open. A slot
	// is taken before the pool is dialed, so that no more than
	// poolMostConnections dials are ever made at once, and given back when
	// the dial fails or the connection it made is closed.
	slots int
	// Made counts the connections made to the pool, and Failed those that
	// could not be.
	made, failed int
}

// poolMostConnections is how many connections a task may have open to its
// pool at once.
const poolMostConnections = 4

// forwardPool starts a forwarder from a Unix socket at socket to to. It
// stops when ctx is done, or stop is called; stop waits for every
// connection to be closed.
func forwardPool(ctx context.Context, socket string, to netip.AddrPort, dial func(ctx context.Context, network, address string) (net.Conn, error)) (*poolForwarder, error) {
	listener, err := net.Listen("unix", socket)
	if err != nil {
		return nil, fmt.Errorf("make the socket the miner reaches its pool through: %w", err)
	}
	f := &poolForwarder{listener: listener, to: to, dial: dial, open: make(map[net.Conn]bool)}
	ctx, cancel := context.WithCancel(ctx)
	f.done.Add(2)
	go func() {
		defer f.done.Done()
		<-ctx.Done()
		f.close()
	}()
	go func() {
		defer f.done.Done()
		defer cancel()
		f.accept(ctx)
	}()
	return f, nil
}

// accept takes connections until the listener is closed.
func (f *poolForwarder) accept(ctx context.Context) {
	for {
		in, err := f.listener.Accept()
		if err != nil {
			return
		}
		if !f.hold(in) {
			in.Close()
			continue
		}
		f.done.Add(1)
		go func() {
			defer f.done.Done()
			f.join(ctx, in)
		}()
	}
}

// hold keeps a miner's connection among those open and takes a slot for
// the connection to the pool it will be joined to, unless every slot is
// taken or the forwarder is closed.
func (f *poolForwarder) hold(in net.Conn) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed || f.slots >= poolMostConnections {
		return false
	}
	f.slots++
	f.open[in] = true
	return true
}

// keep keeps a connection made to the pool among those open, unless the
// forwarder is closed. Its slot was taken when the miner's was held.
func (f *poolForwarder) keep(out net.Conn) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return false
	}
	f.open[out] = true
	return true
}

// free gives back the slot a held connection took.
func (f *poolForwarder) free() {
	f.mu.Lock()
	f.slots--
	f.mu.Unlock()
}

// let forgets an open connection, and closes it.
func (f *poolForwarder) let(c net.Conn) {
	f.mu.Lock()
	delete(f.open, c)
	f.mu.Unlock()
	c.Close()
}

// join connects to the pool and copies bytes between it and in, both
// ways, until either side closes. in holds a slot, which join gives back
// once the connection to the pool is closed or could not be made, and
// before in is closed.
func (f *poolForwarder) join(ctx context.Context, in net.Conn) {
	defer f.let(in)
	defer f.free()
	dialing, cancel := context.WithTimeout(ctx, 20*time.Second)
	out, err := f.dial(dialing, "tcp", f.to.String())
	cancel()
	f.mu.Lock()
	if err != nil {
		f.failed++
	} else {
		f.made++
	}
	f.mu.Unlock()
	if err != nil {
		return
	}
	if !f.keep(out) {
		out.Close()
		return
	}
	defer f.let(out)
	var copying sync.WaitGroup
	copying.Add(1)
	go func() {
		defer copying.Done()
		io.Copy(out, in)
		out.Close()
	}()
	io.Copy(in, out)
	in.Close()
	copying.Wait()
}

// close stops taking connections and closes those open.
func (f *poolForwarder) close() {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return
	}
	f.closed = true
	f.listener.Close()
	for c := range f.open {
		c.Close()
	}
}

// stop closes the forwarder and waits for it, and says how many
// connections it made to the pool and how many it could not.
func (f *poolForwarder) stop() (made, failed int) {
	f.close()
	f.done.Wait()
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.made, f.failed
}

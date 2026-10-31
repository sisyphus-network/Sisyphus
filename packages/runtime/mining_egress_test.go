package runtime

import (
	"context"
	"errors"
	"io"
	"net"
	"net/netip"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// Which addresses a miner may reach its pool at: only those on the public
// Internet, however they are written.
func TestOnlyPublicAddressesMayBeAPools(t *testing.T) {
	for _, tc := range []struct {
		addr string
		in   string // the refused network it is in, or "" for none
	}{
		// Public.
		{"1.1.1.1", ""},
		{"8.8.8.8", ""},
		{"100.63.255.255", ""},
		{"100.128.0.0", ""},
		{"172.15.255.255", ""},
		{"172.32.0.0", ""},
		{"2606:4700::1111", ""},
		{"2a00:1450:4001::1", ""},
		{"::ffff:8.8.8.8", ""},
		// Loopback.
		{"127.0.0.1", "127.0.0.0/8"},
		{"127.255.255.254", "127.0.0.0/8"},
		{"::1", "::/96"},
		{"::ffff:127.0.0.1", "127.0.0.0/8"},
		// Private.
		{"10.0.0.1", "10.0.0.0/8"},
		{"172.16.0.1", "172.16.0.0/12"},
		{"172.31.255.255", "172.16.0.0/12"},
		{"192.168.1.1", "192.168.0.0/16"},
		{"::ffff:192.168.1.1", "192.168.0.0/16"},
		{"fc00::1", "fc00::/7"},
		{"fd12:3456::1", "fc00::/7"},
		// Link-local.
		{"169.254.169.254", "169.254.0.0/16"},
		{"fe80::1", "fe80::/10"},
		// CGNAT.
		{"100.64.0.1", "100.64.0.0/10"},
		{"100.127.255.255", "100.64.0.0/10"},
		// Multicast, broadcast and unspecified.
		{"224.0.0.1", "224.0.0.0/4"},
		{"239.255.255.250", "224.0.0.0/4"},
		{"255.255.255.255", "240.0.0.0/4"},
		{"ff02::1", "ff00::/8"},
		{"0.0.0.0", "0.0.0.0/8"},
		{"::", "::/96"},
		// Kept for documentation, benchmarks and the future.
		{"192.0.2.1", "192.0.2.0/24"},
		{"198.51.100.7", "198.51.100.0/24"},
		{"203.0.113.9", "203.0.113.0/24"},
		{"198.18.0.1", "198.18.0.0/15"},
		{"240.0.0.1", "240.0.0.0/4"},
		{"2001:db8::1", "2001:db8::/32"},
		{"fec0::1", "fec0::/10"},
		// IPv4 inside IPv6, which could reach a refused IPv4 address.
		{"::10.0.0.1", "::/96"},
		{"64:ff9b::a00:1", "64:ff9b::/96"},
		{"2002:c0a8:101::1", "2002::/16"},
		{"2001:0:4136:e378::1", "2001::/32"},
	} {
		err := poolAddressRefused(netip.MustParseAddr(tc.addr))
		switch {
		case tc.in == "" && err != nil:
			t.Errorf("%s is refused: %v", tc.addr, err)
		case tc.in != "" && (err == nil || !strings.Contains(err.Error(), "(it is in "+tc.in+")")):
			t.Errorf("%s: %v, want it refused as in %s", tc.addr, err, tc.in)
		}
	}
	if err := poolAddressRefused(netip.Addr{}); err == nil {
		t.Error("no address is taken")
	}
	if err := poolAddressRefused(netip.MustParseAddr("2a00:1450::1%eth0")); err == nil || !strings.Contains(err.Error(), "has a zone") {
		t.Errorf("an address with a zone: %v", err)
	}
}

// A pool's name is resolved once, to the first of its addresses that may
// be reached; one named by its address is reached at that.
func TestAPoolIsResolvedToOneAddress(t *testing.T) {
	ctx := context.Background()
	public, private := netip.MustParseAddr("1.1.1.1"), netip.MustParseAddr("10.1.2.3")
	for _, tc := range []struct {
		host     string
		resolves staticResolver
		want     string
		err      string
	}{
		{"pool.example.com", staticResolver{private, public, netip.MustParseAddr("8.8.8.8")}, "1.1.1.1:3333", ""},
		{"pool.example.com", staticResolver{netip.MustParseAddr("::ffff:1.1.1.1")}, "1.1.1.1:3333", ""},
		{"pool.example.com", staticResolver{netip.MustParseAddr("2606:4700::1111")}, "[2606:4700::1111]:3333", ""},
		{"pool.example.com", staticResolver{private, netip.MustParseAddr("fe80::1")}, "", "has no address a miner may reach: 10.1.2.3 is not"},
		{"pool.example.com", nil, "", "find the pool pool.example.com: no such host"},
		{"1.1.1.1", nil, "1.1.1.1:3333", ""},
		{"::ffff:1.1.1.1", nil, "1.1.1.1:3333", ""},
		{"10.1.2.3", staticResolver{public}, "", "the pool: 10.1.2.3 is not an address on the public Internet"},
	} {
		got, err := resolvePool(ctx, tc.resolves, tc.host, 3333, poolAddressRefused)
		switch {
		case tc.err == "" && (err != nil || got.String() != tc.want):
			t.Errorf("%s resolving to %v: %v, %v", tc.host, tc.resolves, got, err)
		case tc.err != "" && (err == nil || !strings.Contains(err.Error(), tc.err)):
			t.Errorf("%s resolving to %v: %v, want %q", tc.host, tc.resolves, err, tc.err)
		}
	}
}

// echoServer answers each connection with what it is sent, with "echo: "
// before it.
func echoServer(t *testing.T) netip.AddrPort {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { listener.Close() })
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				conn.Write([]byte("echo: "))
				io.Copy(conn, conn)
			}()
		}
	}()
	return listener.Addr().(*net.TCPAddr).AddrPort()
}

// The forwarder connects only to the one address it was made for, however
// many connections it is given, and copies both ways.
func TestTheForwarderReachesOnlyItsAddress(t *testing.T) {
	ctx := context.Background()
	to := echoServer(t)
	var dialed []string
	var mu sync.Mutex
	dial := func(ctx context.Context, network, address string) (net.Conn, error) {
		mu.Lock()
		dialed = append(dialed, network+" "+address)
		mu.Unlock()
		return (&net.Dialer{}).DialContext(ctx, network, address)
	}
	socket := filepath.Join(t.TempDir(), "pool.sock")
	f, err := forwardPool(ctx, socket, to, dial)
	if err != nil {
		t.Fatal(err)
	}
	for range 3 {
		conn, err := net.Dial("unix", socket)
		if err != nil {
			t.Fatal(err)
		}
		conn.Write([]byte("login\n"))
		got := make([]byte, len("echo: login\n"))
		if _, err := io.ReadFull(conn, got); err != nil || string(got) != "echo: login\n" {
			t.Errorf("read %q, %v", got, err)
		}
		conn.Close()
	}
	made, failed := f.stop()
	if made != 3 || failed != 0 {
		t.Errorf("made %d, failed %d", made, failed)
	}
	for _, d := range dialed {
		if d != "tcp "+to.String() {
			t.Errorf("the forwarder dialed %s", d)
		}
	}
	if _, err := net.Dial("unix", socket); err == nil {
		t.Error("the stopped forwarder still takes connections")
	}
	if _, err := forwardPool(ctx, filepath.Join(t.TempDir(), "no", "such", "pool.sock"), to, dial); err == nil {
		t.Error("a socket was made where there is no directory")
	}
}

// A pool that cannot be reached is counted, and the miner's connection
// closed; one connection too many is closed at once; the forwarder stops
// with its context, closing what is open.
func TestTheForwarderHoldsOnlySoMuch(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	release := make(chan struct{})
	var dialing sync.WaitGroup
	refusing := true
	var mu sync.Mutex
	dial := func(ctx context.Context, network, address string) (net.Conn, error) {
		mu.Lock()
		refuse := refusing
		mu.Unlock()
		if refuse {
			return nil, errors.New("connection refused")
		}
		dialing.Done()
		select {
		case <-release:
		case <-ctx.Done():
		}
		return nil, errors.New("given up")
	}
	socket := filepath.Join(t.TempDir(), "pool.sock")
	f, err := forwardPool(ctx, socket, netip.MustParseAddrPort("1.1.1.1:3333"), dial)
	if err != nil {
		t.Fatal(err)
	}
	conn, _ := net.Dial("unix", socket)
	if n, err := conn.Read(make([]byte, 1)); n != 0 || err == nil {
		t.Errorf("a connection to a pool that refused it read %d bytes, %v", n, err)
	}
	conn.Close()

	mu.Lock()
	refusing = false
	mu.Unlock()
	most := poolMostConnections
	dialing.Add(most)
	var held []net.Conn
	for range most {
		conn, err := net.Dial("unix", socket)
		if err != nil {
			t.Fatal(err)
		}
		held = append(held, conn)
	}
	dialing.Wait()
	extra, _ := net.Dial("unix", socket)
	extra.SetReadDeadline(time.Now().Add(10 * time.Second))
	if _, err := extra.Read(make([]byte, 1)); !errors.Is(err, io.EOF) {
		t.Errorf("a connection past the most was not closed: %v", err)
	}
	extra.Close()
	// Ending the task's context closes every connection.
	cancel()
	for _, conn := range held {
		conn.SetReadDeadline(time.Now().Add(10 * time.Second))
		if _, err := conn.Read(make([]byte, 1)); !errors.Is(err, io.EOF) {
			t.Errorf("a held connection outlived its task: %v", err)
		}
		conn.Close()
	}
	close(release)
	if made, failed := f.stop(); made != 0 || failed != 1+most {
		t.Errorf("made %d, failed %d", made, failed)
	}
}

// A connection made to the pool as the forwarder stops is closed, not
// kept.
func TestAConnectionMadeAsTheForwarderStopsIsClosed(t *testing.T) {
	ctx := context.Background()
	to := echoServer(t)
	var f *poolForwarder
	ready := make(chan struct{})
	closed := make(chan net.Conn, 1)
	dial := func(ctx context.Context, network, address string) (net.Conn, error) {
		<-ready
		conn, err := (&net.Dialer{}).DialContext(ctx, network, address)
		f.close()
		closed <- conn
		return conn, err
	}
	socket := filepath.Join(t.TempDir(), "pool.sock")
	f, err := forwardPool(ctx, socket, to, dial)
	if err != nil {
		t.Fatal(err)
	}
	close(ready)
	conn, _ := net.Dial("unix", socket)
	defer conn.Close()
	out := <-closed
	// Once the forwarder has stopped, which waits for every connection's
	// copying to end, the one it made is closed.
	if made, _ := f.stop(); made != 1 {
		t.Errorf("made %d", made)
	}
	if _, err := out.Write([]byte("x")); err == nil {
		t.Error("the connection to the pool was kept")
	}
}

// countedConn is a connection to the pool that says when it is closed.
type countedConn struct {
	net.Conn
	once   sync.Once
	closed func()
}

func (c *countedConn) Close() error {
	c.once.Do(c.closed)
	return c.Conn.Close()
}

// No more than poolMostConnections connections to the pool are ever being
// made or open at once, however many miners connect at the same time: a
// slot is taken before the pool is dialed, and given back when the dial
// fails or the connection it made is closed.
func TestTheForwarderNeverDialsMoreThanItsMost(t *testing.T) {
	ctx := context.Background()
	var mu sync.Mutex
	busy, most := 0, 0 // dials being made and connections open, and the most there were
	started := make(chan struct{}, 64)
	outcome := make(chan bool) // whether a dial being made succeeds
	var pools []net.Conn
	dial := func(ctx context.Context, network, address string) (net.Conn, error) {
		mu.Lock()
		busy++
		most = max(most, busy)
		mu.Unlock()
		started <- struct{}{}
		if !<-outcome {
			mu.Lock()
			busy--
			mu.Unlock()
			return nil, errors.New("connection refused")
		}
		ours, pool := net.Pipe()
		mu.Lock()
		pools = append(pools, pool)
		mu.Unlock()
		return &countedConn{Conn: ours, closed: func() {
			mu.Lock()
			busy--
			mu.Unlock()
		}}, nil
	}
	socket := filepath.Join(t.TempDir(), "pool.sock")
	f, err := forwardPool(ctx, socket, netip.MustParseAddrPort("1.1.1.1:3333"), dial)
	if err != nil {
		t.Fatal(err)
	}
	// connect connects n miners at once, and says which of them were
	// closed at once, through closed.
	connect := func(n int) (conns []net.Conn, closed chan net.Conn) {
		closed = make(chan net.Conn, n)
		var connecting sync.WaitGroup
		var cmu sync.Mutex
		for range n {
			connecting.Add(1)
			go func() {
				defer connecting.Done()
				conn, err := net.Dial("unix", socket)
				if err != nil {
					t.Error(err)
					return
				}
				cmu.Lock()
				conns = append(conns, conn)
				cmu.Unlock()
				go func() {
					conn.SetReadDeadline(time.Now().Add(10 * time.Second))
					if _, err := conn.Read(make([]byte, 1)); errors.Is(err, io.EOF) {
						closed <- conn
					}
				}()
			}()
		}
		connecting.Wait()
		return conns, closed
	}
	wait := func(n int, c chan struct{}) {
		for range n {
			select {
			case <-c:
			case <-time.After(10 * time.Second):
				t.Fatal("a dial was not started")
			}
		}
	}
	waitClosed := func(n int, c chan net.Conn) {
		for range n {
			select {
			case <-c:
			case <-time.After(10 * time.Second):
				t.Fatal("a connection past the most was not closed")
			}
		}
	}
	slotsFree := func() {
		for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(time.Millisecond) {
			f.mu.Lock()
			slots := f.slots
			f.mu.Unlock()
			if slots == 0 {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("%d slots were not given back", slots)
			}
		}
	}

	// Three times as many miners as may dial connect at once; only the
	// most may dial, and the rest are closed while they still are.
	first, closed := connect(3 * poolMostConnections)
	wait(poolMostConnections, started)
	waitClosed(2*poolMostConnections, closed)
	// Dials that fail give back their slots.
	for range poolMostConnections {
		outcome <- false
	}
	waitClosed(poolMostConnections, closed)
	slotsFree()
	for _, conn := range first {
		conn.Close()
	}

	// So the most may dial again, and this time are connected; while they
	// are, one more is closed at once.
	second, _ := connect(poolMostConnections)
	wait(poolMostConnections, started)
	for range poolMostConnections {
		outcome <- true
	}
	_, closed = connect(1)
	waitClosed(1, closed)
	// Connections that close give back their slots.
	for _, conn := range second {
		conn.Close()
	}
	slotsFree()
	_, closed = connect(1)
	wait(1, started)
	outcome <- false
	waitClosed(1, closed)

	if made, failed := f.stop(); made != poolMostConnections || failed != poolMostConnections+1 {
		t.Errorf("made %d, failed %d", made, failed)
	}
	mu.Lock()
	defer mu.Unlock()
	if most > poolMostConnections {
		t.Errorf("%d connections to the pool were being made or open at once; the most is %d", most, poolMostConnections)
	}
	if busy != 0 {
		t.Errorf("%d connections to the pool were left open", busy)
	}
	for _, pool := range pools {
		pool.Close()
	}
}

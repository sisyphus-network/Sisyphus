package natpmp

import (
	"context"
	"encoding/binary"
	"errors"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeGateway answers NAT-PMP on a loopback address, with what answer
// says for each request, and records every request it is sent.
type fakeGateway struct {
	conn   *net.UDPConn
	mu     sync.Mutex
	asked  [][]byte
	answer func(request []byte, from *net.UDPAddr) [][]byte
}

func startGateway(t *testing.T, answer func(request []byte, from *net.UDPAddr) [][]byte) *fakeGateway {
	t.Helper()
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	old := port
	port = conn.LocalAddr().(*net.UDPAddr).Port
	t.Cleanup(func() { port = old })
	g := &fakeGateway{conn: conn, answer: answer}
	go func() {
		buf := make([]byte, 64)
		for {
			n, from, err := conn.ReadFromUDP(buf)
			if err != nil {
				return
			}
			request := append([]byte(nil), buf[:n]...)
			g.mu.Lock()
			g.asked = append(g.asked, request)
			g.mu.Unlock()
			for _, reply := range g.answer(request, from) {
				conn.WriteToUDP(reply, from)
			}
		}
	}()
	return g
}

// requests is what the gateway was sent.
func (g *fakeGateway) requests() [][]byte {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([][]byte(nil), g.asked...)
}

// externalAnswer is a gateway's answer to the external address request.
func externalAnswer(result uint16, addr [4]byte) []byte {
	answer := make([]byte, 12)
	answer[1] = 128
	binary.BigEndian.PutUint16(answer[2:4], result)
	binary.BigEndian.PutUint32(answer[4:8], 3600)
	copy(answer[8:], addr[:])
	return answer
}

var loopback = netip.MustParseAddr("127.0.0.1")

// The client asks for the external address, and nothing else: no request
// it sends asks for a mapping.
func TestTheGatewayIsAskedOnlyForItsExternalAddress(t *testing.T) {
	g := startGateway(t, func([]byte, *net.UDPAddr) [][]byte { return [][]byte{externalAnswer(0, [4]byte{193, 0, 6, 139})} })
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	got, err := ExternalAddress(ctx, loopback)
	if err != nil || got != netip.MustParseAddr("193.0.6.139") {
		t.Fatalf("external address %v, %v", got, err)
	}
	for _, request := range g.requests() {
		if len(request) != 2 || request[0] != 0 || request[1] != 0 {
			t.Errorf("the gateway was sent %v, which is not the external address request", request)
		}
	}
}

// What is not an answer to the request is ignored, and the request sent
// again; an answer that refuses is an error.
func TestOnlyAnAnswerToTheRequestIsTaken(t *testing.T) {
	sent := 0
	startGateway(t, func([]byte, *net.UDPAddr) [][]byte {
		sent++
		if sent == 1 {
			// The first time, nothing that answers the request: too short,
			// another version, another opcode.
			return [][]byte{{0, 128}, {1, 128, 0, 0, 0, 0, 0, 0, 1, 2, 3, 4}, append([]byte{0, 129}, make([]byte, 14)...)}
		}
		return [][]byte{externalAnswer(3, [4]byte{})} // network failure
	})
	old := firstWait
	firstWait = 20 * time.Millisecond
	defer func() { firstWait = old }()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := ExternalAddress(ctx, loopback); err == nil || !strings.Contains(err.Error(), "result 3") {
		t.Fatalf("a gateway that refuses: %v", err)
	}
}

// An answer that does not come from the gateway is not the gateway's,
// however right it looks.
func TestAnAnswerFromElsewhereIsIgnored(t *testing.T) {
	impostor, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 2)})
	if err != nil {
		t.Skip("no second loopback address here:", err)
	}
	defer impostor.Close()
	// The gateway says nothing itself, and has another address answer for
	// it.
	startGateway(t, func(_ []byte, from *net.UDPAddr) [][]byte {
		impostor.WriteToUDP(externalAnswer(0, [4]byte{6, 6, 6, 6}), from)
		return nil
	})
	old := firstWait
	firstWait = 20 * time.Millisecond
	defer func() { firstWait = old }()
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	if got, err := ExternalAddress(ctx, loopback); !errors.Is(err, ErrNoAnswer) {
		t.Fatalf("with only an impostor answering: %v, %v", got, err)
	}
}

// Only an IPv4 gateway can be asked, and only from a socket that opens.
func TestOnlyAnIPv4GatewayIsAsked(t *testing.T) {
	if _, err := ExternalAddress(context.Background(), netip.MustParseAddr("fe80::1")); err == nil {
		t.Error("an IPv6 gateway was asked")
	}
	old := listen
	listen = func() (*net.UDPConn, error) { return nil, errors.New("no sockets left") }
	defer func() { listen = old }()
	if _, err := ExternalAddress(context.Background(), loopback); err == nil || !strings.Contains(err.Error(), "no sockets left") {
		t.Errorf("with no socket: %v", err)
	}
}

// The default gateway is read from the routing table.
func TestTheGatewayIsTheDefaultRoutes(t *testing.T) {
	for table, want := range map[string]string{
		"Iface\tDestination\tGateway\nwlan0\t0010A8C0\t00000000\neth0\t00000000\t0189A8C0\n":   "192.168.137.1",
		"Iface\tDestination\tGateway\neth0\t00000000\tnothex!!\ndocker0\t000011AC\t00000000\n": "",
		"Iface\tDestination\tGateway\nwg0\t00000000\t00000000\n":                               "",
		"Iface\tDestination\tGateway\nshort\n":                                                 "",
		"Iface\tDestination\tGateway\neth0\t00000000\t01\n":                                    "",
	} {
		got, err := gatewayIn(strings.NewReader(table))
		switch {
		case want == "" && !errors.Is(err, ErrNoGateway):
			t.Errorf("%q: %v, %v; want no gateway", table, got, err)
		case want != "" && (err != nil || got != netip.MustParseAddr(want)):
			t.Errorf("%q: %v, %v; want %s", table, got, err, want)
		}
	}
	file := filepath.Join(t.TempDir(), "route")
	os.WriteFile(file, []byte("Iface\tDestination\tGateway\neth0\t00000000\t0101A8C0\n"), 0o600)
	old := routes
	defer func() { routes = old }()
	routes = file
	if got, err := Gateway(); err != nil || got != netip.MustParseAddr("192.168.1.1") {
		t.Errorf("Gateway() = %v, %v", got, err)
	}
	routes = filepath.Join(t.TempDir(), "missing")
	if _, err := Gateway(); err == nil {
		t.Error("a routing table that is not there gave a gateway")
	}
}

package stun

import (
	"context"
	"encoding/binary"
	"errors"
	"net"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeServer answers STUN on a loopback address, with what answer says
// for each request, and records every request it is sent.
type fakeServer struct {
	addr   string
	mu     sync.Mutex
	asked  [][]byte
	answer func(request []byte, from *net.UDPAddr) [][]byte
}

func startServer(t *testing.T, answer func(request []byte, from *net.UDPAddr) [][]byte) *fakeServer {
	t.Helper()
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	s := &fakeServer{addr: conn.LocalAddr().String(), answer: answer}
	go func() {
		buf := make([]byte, 1500)
		for {
			n, from, err := conn.ReadFromUDP(buf)
			if err != nil {
				return
			}
			request := append([]byte(nil), buf[:n]...)
			s.mu.Lock()
			s.asked = append(s.asked, request)
			s.mu.Unlock()
			for _, reply := range s.answer(request, from) {
				conn.WriteToUDP(reply, from)
			}
		}
	}()
	return s
}

// requests is what the server was sent.
func (s *fakeServer) requests() [][]byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([][]byte(nil), s.asked...)
}

// message is a STUN message of the given type answering request, carrying
// attrs, each a type and a value, padded as the RFC asks.
func message(kind uint16, request []byte, attrs ...any) []byte {
	var body []byte
	for i := 0; i+1 < len(attrs); i += 2 {
		value := attrs[i+1].([]byte)
		body = binary.BigEndian.AppendUint16(body, attrs[i].(uint16))
		body = binary.BigEndian.AppendUint16(body, uint16(len(value)))
		body = append(body, value...)
		for len(body)%4 != 0 {
			body = append(body, 0)
		}
	}
	m := binary.BigEndian.AppendUint16(nil, kind)
	m = binary.BigEndian.AppendUint16(m, uint16(len(body)))
	m = append(m, request[4:20]...) // the cookie and the transaction ID
	return append(m, body...)
}

// xored is an XOR-MAPPED-ADDRESS value for addr, as a server answering
// request writes it.
func xored(request []byte, addr netip.Addr) []byte {
	mask := request[4:20]
	raw := addr.AsSlice()
	value := []byte{0, 1, 0x12 ^ mask[0], 0x34 ^ mask[1]}
	if addr.Is6() {
		value[1] = 2
	}
	for i, b := range raw {
		value = append(value, b^mask[i])
	}
	return value
}

func ask(t *testing.T, server string) (netip.Addr, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return ReflexiveAddress(ctx, server)
}

// The client sends a Binding request, and nothing else, and reads the
// address the server says it came from.
func TestTheServerIsAskedOnlyWhereTheRequestCameFrom(t *testing.T) {
	want := netip.MustParseAddr("193.0.6.139")
	s := startServer(t, func(request []byte, _ *net.UDPAddr) [][]byte {
		return [][]byte{message(bindingSuccess, request, uint16(xorMappedAddress), xored(request, want))}
	})
	if got, err := ask(t, s.addr); err != nil || got != want {
		t.Fatalf("got %v, %v; want %v", got, err, want)
	}
	for _, r := range s.requests() {
		if len(r) != 20 || binary.BigEndian.Uint16(r[0:2]) != bindingRequest || binary.BigEndian.Uint16(r[2:4]) != 0 || binary.BigEndian.Uint32(r[4:8]) != magic {
			t.Errorf("sent %x, which is not a bare Binding request", r)
		}
	}
}

// An IPv6 address is unmasked with the transaction ID as well as the
// cookie, and an old server's plain MAPPED-ADDRESS is read too.
func TestEveryWayAnAddressIsSaidIsRead(t *testing.T) {
	six := netip.MustParseAddr("2001:67c:2e8:22::c100:68b")
	four := netip.MustParseAddr("193.0.6.139")
	for _, tt := range []struct {
		name   string
		answer func(request []byte) []byte
		want   netip.Addr
	}{
		{"xor ipv6", func(r []byte) []byte {
			return message(bindingSuccess, r, uint16(xorMappedAddress), xored(r, six))
		}, six},
		{"plain", func(r []byte) []byte {
			return message(bindingSuccess, r, uint16(mappedAddress), append([]byte{0, 1, 0x12, 0x34}, four.AsSlice()...))
		}, four},
		{"xor preferred", func(r []byte) []byte {
			return message(bindingSuccess, r, uint16(0x8022), []byte("software"), uint16(mappedAddress), append([]byte{0, 1, 0, 0}, 9, 9, 9, 9), uint16(xorMappedAddress), xored(r, four))
		}, four},
	} {
		t.Run(tt.name, func(t *testing.T) {
			s := startServer(t, func(request []byte, _ *net.UDPAddr) [][]byte { return [][]byte{tt.answer(request)} })
			if got, err := ask(t, s.addr); err != nil || got != tt.want {
				t.Errorf("got %v, %v; want %v", got, err, tt.want)
			}
		})
	}
}

// A server that refuses, or answers without an address, or with one it
// does not spell out properly, is not taken at its word.
func TestAnAnswerWithNoAddressIsAnError(t *testing.T) {
	for _, tt := range []struct {
		name   string
		answer func(request []byte) []byte
		want   string
	}{
		{"refused", func(r []byte) []byte {
			return message(bindingError, r, uint16(errorCode), []byte{0, 0, 4, 20, 'n', 'o'})
		}, "error 420"},
		{"refused without a code", func(r []byte) []byte { return message(bindingError, r) }, "error 0"},
		{"no address", func(r []byte) []byte { return message(bindingSuccess, r) }, "without an address"},
		{"short address", func(r []byte) []byte {
			return message(bindingSuccess, r, uint16(xorMappedAddress), []byte{0, 1, 0, 0, 1})
		}, "did not spell out"},
		{"unknown family", func(r []byte) []byte {
			return message(bindingSuccess, r, uint16(xorMappedAddress), []byte{0, 3, 0, 0, 1, 2, 3, 4})
		}, "did not spell out"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			s := startServer(t, func(request []byte, _ *net.UDPAddr) [][]byte { return [][]byte{tt.answer(request)} })
			if _, err := ask(t, s.addr); err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("got %v, want an error saying %q", err, tt.want)
			}
		})
	}
}

// What is not the answer to this request, whatever it says, is ignored,
// and the answer that follows it is taken.
func TestOnlyTheAnswerToTheRequestIsTaken(t *testing.T) {
	want := netip.MustParseAddr("193.0.6.139")
	s := startServer(t, func(request []byte, _ *net.UDPAddr) [][]byte {
		other := append([]byte(nil), request...)
		other[19] ^= 1 // another transaction
		noCookie := message(bindingSuccess, request, uint16(xorMappedAddress), xored(request, netip.MustParseAddr("9.9.9.9")))
		noCookie[4] ^= 1
		long := message(bindingSuccess, request)
		binary.BigEndian.PutUint16(long[2:4], 400) // says more than it has
		return [][]byte{
			[]byte("short"),
			message(bindingSuccess, other, uint16(xorMappedAddress), xored(other, netip.MustParseAddr("9.9.9.9"))),
			noCookie,
			long,
			message(bindingRequest, request),
			message(bindingSuccess, request, uint16(xorMappedAddress), xored(request, want)),
		}
	})
	if got, err := ask(t, s.addr); err != nil || got != want {
		t.Fatalf("got %v, %v; want %v", got, err, want)
	}
}

// Attributes that run past the end of the message are not read.
func TestAttributesPastTheEndAreNotRead(t *testing.T) {
	for _, attrs := range [][]byte{
		{0, 0x20, 0, 8, 1, 2},                      // longer than what follows
		{0, 0x80, 0, 3, 1, 2, 3},                   // padding that is not there
		{0, 0x80, 0, 0, 0, 0x20},                   // a header cut short
		{0, 0x80, 0, 2, 1, 2, 0, 0, 0, 0x20},       // one after, cut short
		{0, 0x80, 0, 1, 1, 0, 0, 0, 0, 0x20, 0, 9}, // ditto, longer than what follows
	} {
		if got := attribute(attrs, xorMappedAddress); got != nil {
			t.Errorf("attribute(%x) = %x, want nothing", attrs, got)
		}
	}
}

// A request that goes unanswered is sent again, and one that is never
// answered ends with the context.
func TestAnUnansweredRequestIsRepeatedUntilTheEnd(t *testing.T) {
	defer func(old time.Duration) { firstWait = old }(firstWait)
	firstWait = 20 * time.Millisecond
	want := netip.MustParseAddr("193.0.6.139")
	answered := startServer(t, func([]byte, *net.UDPAddr) [][]byte { return nil })
	var mu sync.Mutex
	asked := 0
	second := startServer(t, func(request []byte, _ *net.UDPAddr) [][]byte {
		mu.Lock()
		defer mu.Unlock()
		if asked++; asked != 2 {
			return nil // the first is lost, and later ones not needed
		}
		return [][]byte{message(bindingSuccess, request, uint16(xorMappedAddress), xored(request, want))}
	})
	if got, err := ask(t, second.addr); err != nil || got != want {
		t.Errorf("got %v, %v; want %v after asking again", got, err, want)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	if _, err := ReflexiveAddress(ctx, answered.addr); !errors.Is(err, ErrNoAnswer) {
		t.Errorf("a server that never answers: %v", err)
	}
	if n := len(answered.requests()); n < 2 {
		t.Errorf("asked %d times in all", n)
	}
	// With no deadline, the waits alone set when it asks again.
	ctx, cancel = context.WithCancel(context.Background())
	time.AfterFunc(100*time.Millisecond, cancel)
	if _, err := ReflexiveAddress(ctx, answered.addr); !errors.Is(err, ErrNoAnswer) {
		t.Errorf("a server that never answers, without a deadline: %v", err)
	}
}

// A server named without a port is asked at the usual one, and one that
// cannot be reached is an error.
func TestAServerIsNamedWithOrWithoutItsPort(t *testing.T) {
	defer func(old func(context.Context, string) (net.Conn, error)) { dial = old }(dial)
	var dialled []string
	dial = func(_ context.Context, server string) (net.Conn, error) {
		dialled = append(dialled, server)
		return nil, errors.New("unreachable")
	}
	for _, server := range []string{"stun.example.org", "stun.example.org:19302", "2001:db8::1", "[2001:db8::1]"} {
		if _, err := ReflexiveAddress(context.Background(), server); err == nil {
			t.Errorf("%s: no error from a server that cannot be reached", server)
		}
	}
	want := []string{"stun.example.org:3478", "stun.example.org:19302", "[2001:db8::1]:3478", "[2001:db8::1]:3478"}
	if strings.Join(dialled, " ") != strings.Join(want, " ") {
		t.Errorf("dialled %v, want %v", dialled, want)
	}
}

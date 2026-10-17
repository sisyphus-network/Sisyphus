// Package stun asks a STUN server which address a machine's traffic comes
// from, by the one Binding request STUN has for it (RFC 8489). It is read
// only as far as the network goes: nothing it sends opens a port or asks
// anything of the machine's gateway. It is not private: the server learns
// the address it answers with, which is why it is only ever sent to a
// server the user names.
package stun

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"strings"
	"time"
)

// DefaultPort is where a STUN server answers when it is named without one.
const DefaultPort = "3478"

// dial opens the socket the request is sent from, connected to the
// server, so that only what the server sends is read; tests put another
// here.
var dial = func(ctx context.Context, server string) (net.Conn, error) {
	var d net.Dialer
	return d.DialContext(ctx, "udp", server)
}

// firstWait is how long the first request is given before it is sent
// again; each wait after doubles, as the RFC asks.
var firstWait = 500 * time.Millisecond

// ErrNoAnswer is returned when the server says nothing of use before the
// context ends.
var ErrNoAnswer = errors.New("the STUN server did not answer")

// magic is the cookie every STUN message carries since RFC 5389.
const magic = 0x2112A442

const (
	bindingRequest = 0x0001
	bindingSuccess = 0x0101
	bindingError   = 0x0111

	mappedAddress    = 0x0001
	errorCode        = 0x0009
	xorMappedAddress = 0x0020
)

// ReflexiveAddress asks the STUN server at server, host:port or a host
// alone for the usual port, which address the request came to it from,
// and returns it. Only the server's answer to this request is taken. The
// request is sent again, at doubling intervals, until an answer comes or
// ctx ends.
func ReflexiveAddress(ctx context.Context, server string) (netip.Addr, error) {
	if _, _, err := net.SplitHostPort(server); err != nil {
		server = net.JoinHostPort(strings.Trim(server, "[]"), DefaultPort)
	}
	conn, err := dial(ctx, server)
	if err != nil {
		return netip.Addr{}, err
	}
	defer conn.Close()
	var id [12]byte
	rand.Read(id[:])
	request := binary.BigEndian.AppendUint16(nil, bindingRequest)
	request = binary.BigEndian.AppendUint16(request, 0) // no attributes
	request = binary.BigEndian.AppendUint32(request, magic)
	request = append(request, id[:]...)
	answer := make([]byte, 1500)
	for wait := firstWait; ; wait *= 2 {
		// A request that could not be sent goes unanswered like one that
		// was lost, and is sent again.
		conn.Write(request)
		deadline := time.Now().Add(wait)
		if end, ok := ctx.Deadline(); ok && end.Before(deadline) {
			deadline = end
		}
		conn.SetReadDeadline(deadline)
		for {
			n, err := conn.Read(answer)
			if err != nil {
				break // this wait is over; ask again, if there is time
			}
			if addr, err, ok := read(answer[:n], id); ok {
				return addr, err
			}
		}
		if ctx.Err() != nil {
			return netip.Addr{}, ErrNoAnswer
		}
	}
}

// read makes what an answer to the Binding request with transaction id
// says, and reports whether it was such an answer at all.
func read(answer []byte, id [12]byte) (addr netip.Addr, err error, ok bool) {
	if len(answer) < 20 || binary.BigEndian.Uint32(answer[4:8]) != magic || !bytes.Equal(answer[8:20], id[:]) {
		return netip.Addr{}, nil, false
	}
	kind, length := binary.BigEndian.Uint16(answer[0:2]), int(binary.BigEndian.Uint16(answer[2:4]))
	if kind != bindingSuccess && kind != bindingError || 20+length > len(answer) {
		return netip.Addr{}, nil, false
	}
	attributes := answer[20 : 20+length]
	if kind == bindingError {
		code := 0
		if value := attribute(attributes, errorCode); len(value) >= 4 {
			code = int(value[2]&7)*100 + int(value[3])
		}
		return netip.Addr{}, fmt.Errorf("the STUN server refused to say the address (error %d)", code), true
	}
	if value := attribute(attributes, xorMappedAddress); value != nil {
		return address(value, id, true)
	}
	if value := attribute(attributes, mappedAddress); value != nil {
		return address(value, id, false)
	}
	return netip.Addr{}, errors.New("the STUN server answered without an address"), true
}

// attribute returns the value of the first attribute of the given type, or
// nil if there is none.
func attribute(attributes []byte, want uint16) []byte {
	for len(attributes) >= 4 {
		kind, length := binary.BigEndian.Uint16(attributes[0:2]), int(binary.BigEndian.Uint16(attributes[2:4]))
		if 4+length > len(attributes) {
			return nil
		}
		if kind == want {
			return attributes[4 : 4+length]
		}
		// Values are padded to a multiple of four bytes.
		next := 4 + (length+3)&^3
		if next > len(attributes) {
			return nil
		}
		attributes = attributes[next:]
	}
	return nil
}

// address reads a (XOR-)MAPPED-ADDRESS value: a reserved byte, the family,
// the port and the address, the last two XORed with the cookie and the
// transaction ID if xored is set.
func address(value []byte, id [12]byte, xored bool) (netip.Addr, error, bool) {
	var mask [16]byte
	if xored {
		binary.BigEndian.PutUint32(mask[:4], magic)
		copy(mask[4:], id[:])
	}
	switch {
	case len(value) == 8 && value[1] == 1:
		var ip [4]byte
		for i := range ip {
			ip[i] = value[4+i] ^ mask[i]
		}
		return netip.AddrFrom4(ip), nil, true
	case len(value) == 20 && value[1] == 2:
		var ip [16]byte
		for i := range ip {
			ip[i] = value[4+i] ^ mask[i]
		}
		return netip.AddrFrom16(ip), nil, true
	}
	return netip.Addr{}, errors.New("the STUN server answered with an address it did not spell out properly"), true
}

// Package natpmp asks the gateway a machine sits behind for the address it
// shows the internet, by the one read-only request NAT-PMP has (RFC 6886,
// opcode 0). It never asks for a mapping: nothing it sends opens a port or
// changes the gateway's state.
package natpmp

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"time"
)

// port is where a gateway answers NAT-PMP; tests put another here.
var port = 5351

// listen opens the socket the request is sent from; tests put another
// here.
var listen = func() (*net.UDPConn, error) { return net.ListenUDP("udp4", nil) }

// firstWait is how long the first request is given before it is sent
// again; each wait after doubles, as the RFC asks.
var firstWait = 250 * time.Millisecond

// ErrNoAnswer is returned when the gateway says nothing of use before the
// context ends: it does not speak NAT-PMP, or does not let it be asked.
var ErrNoAnswer = errors.New("the gateway did not answer NAT-PMP")

// ExternalAddress asks the gateway for the IPv4 address it shows the
// internet. Only an answer from the gateway itself, to the request sent,
// is taken; anything else that arrives is ignored. The request is sent
// again, at doubling intervals, until an answer comes or ctx ends.
func ExternalAddress(ctx context.Context, gateway netip.Addr) (netip.Addr, error) {
	gateway = gateway.Unmap()
	if !gateway.Is4() {
		return netip.Addr{}, fmt.Errorf("NAT-PMP asks an IPv4 gateway, and %s is not one", gateway)
	}
	conn, err := listen()
	if err != nil {
		return netip.Addr{}, err
	}
	defer conn.Close()
	to := net.UDPAddrFromAddrPort(netip.AddrPortFrom(gateway, uint16(port)))
	// Version 0, opcode 0: what is your external address.
	request := []byte{0, 0}
	answer := make([]byte, 16)
	for wait := firstWait; ; wait *= 2 {
		// A request that could not be sent goes unanswered like one that
		// was lost, and is sent again.
		conn.WriteToUDP(request, to)
		deadline := time.Now().Add(wait)
		if end, ok := ctx.Deadline(); ok && end.Before(deadline) {
			deadline = end
		}
		conn.SetReadDeadline(deadline)
		for {
			n, from, err := conn.ReadFromUDPAddrPort(answer)
			if err != nil {
				break // this wait is over; ask again, if there is time
			}
			if from.Addr().Unmap() != gateway {
				continue // not the gateway's to answer
			}
			if addr, err, ok := read(answer[:n]); ok {
				return addr, err
			}
		}
		if ctx.Err() != nil {
			return netip.Addr{}, ErrNoAnswer
		}
	}
}

// read makes what an answer to the external address request says of it,
// and reports whether it was such an answer at all.
func read(answer []byte) (addr netip.Addr, err error, ok bool) {
	// Version 0, opcode 128 (0 answered), result, seconds since the
	// gateway's start, and the address.
	if len(answer) < 12 || answer[0] != 0 || answer[1] != 128 {
		return netip.Addr{}, nil, false
	}
	if result := binary.BigEndian.Uint16(answer[2:4]); result != 0 {
		return netip.Addr{}, fmt.Errorf("the gateway refused to say its external address (NAT-PMP result %d)", result), true
	}
	return netip.AddrFrom4([4]byte(answer[8:12])), nil, true
}

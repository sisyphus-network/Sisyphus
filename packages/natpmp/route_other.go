//go:build !linux

package natpmp

import (
	"errors"
	"net/netip"
)

// Gateway returns the machine's IPv4 default gateway. Only Linux's
// routing table is read so far; elsewhere it says so.
func Gateway() (netip.Addr, error) {
	return netip.Addr{}, errors.New("finding the default gateway is supported on Linux only, so far")
}

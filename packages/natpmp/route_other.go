//go:build !linux && !windows && !darwin && !freebsd && !openbsd && !netbsd && !dragonfly

package natpmp

import (
	"errors"
	"net/netip"
)

// Gateway returns the machine's IPv4 default gateway. Its routing table
// is read on Linux, macOS, the BSDs and Windows; elsewhere it says so.
func Gateway() (netip.Addr, error) {
	return netip.Addr{}, errors.New("finding the default gateway is not supported on this system")
}

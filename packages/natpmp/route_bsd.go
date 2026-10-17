//go:build darwin || freebsd || openbsd || netbsd || dragonfly

package natpmp

import (
	"bytes"
	"net/netip"
	"os/exec"
)

// Gateway returns the machine's IPv4 default gateway, as its routing table
// has it, read with netstat. Nothing is sent anywhere to find it.
func Gateway() (netip.Addr, error) {
	table, err := exec.Command("netstat", "-rn", "-f", "inet").Output()
	if err != nil {
		return netip.Addr{}, err
	}
	return gatewayInNetstat(bytes.NewReader(table))
}

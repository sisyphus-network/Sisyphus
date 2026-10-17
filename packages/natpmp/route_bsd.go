//go:build darwin || freebsd || openbsd || netbsd || dragonfly

package natpmp

import (
	"bytes"
	"net/netip"
	"os/exec"
)

// Gateway returns the machine's IPv4 default gateway: the one the kernel
// chooses, as `route -n get default` reports it. Nothing is sent anywhere
// to find it.
func Gateway() (netip.Addr, error) {
	answer, err := exec.Command("route", "-n", "get", "default").Output()
	if err != nil {
		return netip.Addr{}, err
	}
	return gatewayInRouteGet(bytes.NewReader(answer))
}

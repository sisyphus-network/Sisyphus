package natpmp

import (
	"bytes"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
)

// Gateway returns the machine's IPv4 default gateway, as its routing table
// has it, read with the system's route command. Nothing is sent anywhere
// to find it.
func Gateway() (netip.Addr, error) {
	route := "route"
	if root := os.Getenv("SystemRoot"); root != "" {
		route = filepath.Join(root, "System32", "route.exe")
	}
	table, err := exec.Command(route, "print", "-4", "0.0.0.0").Output()
	if err != nil {
		return netip.Addr{}, err
	}
	return gatewayInRoutePrint(bytes.NewReader(table))
}

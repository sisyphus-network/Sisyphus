package natpmp

import (
	"net/netip"
	"os"
)

// routes is where Linux lists its routes; tests put another here.
var routes = "/proc/net/route"

// Gateway returns the machine's IPv4 default gateway, as its routing table
// has it. Nothing is sent anywhere to find it.
func Gateway() (netip.Addr, error) {
	table, err := os.Open(routes)
	if err != nil {
		return netip.Addr{}, err
	}
	defer table.Close()
	return gatewayIn(table)
}

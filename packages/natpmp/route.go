package natpmp

import (
	"bufio"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"io"
	"net/netip"
	"strconv"
	"strings"
)

// ErrNoGateway is returned when the machine has no IPv4 default route.
var ErrNoGateway = errors.New("this machine has no IPv4 default route")

// Route flags, as Linux has them (RTF_UP and RTF_GATEWAY).
const (
	routeUp      = 0x1
	routeGateway = 0x2
)

// gatewayIn reads the IPv4 default gateway from a table laid out as
// Linux's /proc/net/route: a header, then one route a line, the
// destination, gateway and mask in hexadecimal, least significant byte
// first, and the flags and metric. It is the gateway of the default route
// the kernel would use: one that is up and goes through a gateway, and of
// those, the one of lowest metric, the first if they tie.
func gatewayIn(table io.Reader) (netip.Addr, error) {
	lines := bufio.NewScanner(table)
	lines.Scan() // the header
	var best netip.Addr
	var bestMetric uint64
	for lines.Scan() {
		// Iface Destination Gateway Flags RefCnt Use Metric Mask ...
		fields := strings.Fields(lines.Text())
		if len(fields) < 8 || fields[1] != "00000000" || fields[7] != "00000000" {
			continue // not a default route
		}
		flags, err := strconv.ParseUint(fields[3], 16, 32)
		if err != nil || flags&(routeUp|routeGateway) != routeUp|routeGateway {
			continue // down, or not through a gateway
		}
		metric, err := strconv.ParseUint(fields[6], 10, 32)
		if err != nil {
			continue
		}
		raw, err := hex.DecodeString(fields[2])
		if err != nil || len(raw) != 4 {
			continue
		}
		var addr [4]byte
		binary.LittleEndian.PutUint32(addr[:], binary.BigEndian.Uint32(raw))
		gateway := netip.AddrFrom4(addr)
		if gateway.IsUnspecified() || (best.IsValid() && metric >= bestMetric) {
			continue
		}
		best, bestMetric = gateway, metric
	}
	if !best.IsValid() {
		return netip.Addr{}, ErrNoGateway
	}
	return best, nil
}

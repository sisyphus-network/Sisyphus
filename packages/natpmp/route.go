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

// gatewayInNetstat reads the IPv4 default gateway from what
// `netstat -rn -f inet` prints on macOS and the BSDs: a row a route, the
// destination, gateway and flags first. It is the gateway of the first
// default route that is up (U) and goes through a gateway (G), which is
// the one the system prefers; a route through an interface rather than an
// address, such as a VPN's "link#12", has no gateway to ask.
func gatewayInNetstat(table io.Reader) (netip.Addr, error) {
	lines := bufio.NewScanner(table)
	for lines.Scan() {
		fields := strings.Fields(lines.Text())
		if len(fields) < 3 || fields[0] != "default" || !strings.Contains(fields[2], "U") || !strings.Contains(fields[2], "G") {
			continue
		}
		if gateway, err := netip.ParseAddr(fields[1]); err == nil && gateway.Is4() && !gateway.IsUnspecified() {
			return gateway, nil
		}
	}
	return netip.Addr{}, ErrNoGateway
}

// gatewayInRoutePrint reads the IPv4 default gateway from what Windows
// prints for `route print -4 0.0.0.0`. Its headers are in the system's
// language, so only the rows of active routes are read, five columns
// each: destination, mask, gateway, interface and metric. Persistent
// routes have four and are passed over, as is a route "On-link", which has
// no gateway. It is the gateway of the default route of lowest metric, the
// first if they tie.
func gatewayInRoutePrint(table io.Reader) (netip.Addr, error) {
	lines := bufio.NewScanner(table)
	var best netip.Addr
	var bestMetric uint64
	for lines.Scan() {
		fields := strings.Fields(lines.Text())
		if len(fields) != 5 || fields[0] != "0.0.0.0" || fields[1] != "0.0.0.0" {
			continue
		}
		gateway, err := netip.ParseAddr(fields[2])
		if err != nil || !gateway.Is4() || gateway.IsUnspecified() {
			continue
		}
		metric, err := strconv.ParseUint(fields[4], 10, 32)
		if err != nil || (best.IsValid() && metric >= bestMetric) {
			continue
		}
		best, bestMetric = gateway, metric
	}
	if !best.IsValid() {
		return netip.Addr{}, ErrNoGateway
	}
	return best, nil
}

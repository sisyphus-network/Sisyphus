package natpmp

import (
	"bufio"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"io"
	"net/netip"
	"strings"
)

// ErrNoGateway is returned when the machine has no IPv4 default route.
var ErrNoGateway = errors.New("this machine has no IPv4 default route")

// gatewayIn reads the IPv4 default gateway from a table laid out as
// Linux's /proc/net/route: a header, then one route a line, the
// destination and gateway in hexadecimal, least significant byte first.
func gatewayIn(table io.Reader) (netip.Addr, error) {
	lines := bufio.NewScanner(table)
	lines.Scan() // the header
	for lines.Scan() {
		fields := strings.Fields(lines.Text())
		if len(fields) < 3 || fields[1] != "00000000" {
			continue // not the default route
		}
		raw, err := hex.DecodeString(fields[2])
		if err != nil || len(raw) != 4 {
			continue
		}
		var addr [4]byte
		binary.LittleEndian.PutUint32(addr[:], binary.BigEndian.Uint32(raw))
		if gateway := netip.AddrFrom4(addr); !gateway.IsUnspecified() {
			return gateway, nil
		}
	}
	return netip.Addr{}, ErrNoGateway
}

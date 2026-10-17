package natpmp

import (
	"errors"
	"net/netip"
	"strings"
	"testing"
)

const header = "Iface\tDestination\tGateway \tFlags\tRefCnt\tUse\tMetric\tMask\t\tMTU\tWindow\tIRTT\n"

// route is one line of the table: an interface's route to a destination,
// through a gateway, with its flags and metric.
func route(iface, destination, gateway, flags, metric, mask string) string {
	return strings.Join([]string{iface, destination, gateway, flags, "0", "0", metric, mask, "0", "0", "0"}, "\t") + "\n"
}

// The gateway is that of the default route the kernel would use: up, and
// through a gateway, and of those the one of lowest metric.
func TestTheGatewayIsTheDefaultRouteInUse(t *testing.T) {
	for name, tt := range map[string]struct {
		table string
		want  string
	}{
		"one default route": {route("eth0", "00000000", "0189A8C0", "0003", "100", "00000000"), "192.168.137.1"},
		"a route elsewhere and the default": {
			route("wlan0", "0010A8C0", "00000000", "0001", "0", "00FFFFFF") + route("eth0", "00000000", "0101A8C0", "0003", "100", "00000000"),
			"192.168.1.1",
		},
		"two defaults, the lower metric wins": {
			route("wlan0", "00000000", "0101A8C0", "0003", "600", "00000000") + route("eth0", "00000000", "0102A8C0", "0003", "100", "00000000"),
			"192.168.2.1",
		},
		"two defaults of one metric, the first": {
			route("eth0", "00000000", "0101A8C0", "0003", "100", "00000000") + route("eth1", "00000000", "0102A8C0", "0003", "100", "00000000"),
			"192.168.1.1",
		},
		"a default that is down is passed over": {
			route("eth0", "00000000", "0101A8C0", "0002", "0", "00000000") + route("wlan0", "00000000", "0102A8C0", "0003", "600", "00000000"),
			"192.168.2.1",
		},
		"a default not through a gateway is passed over": {route("wg0", "00000000", "00000000", "0001", "0", "00000000"), ""},
		"a gateway of nothing":                           {route("eth0", "00000000", "00000000", "0003", "0", "00000000"), ""},
		"a destination with a mask is no default":        {route("eth0", "00000000", "0101A8C0", "0003", "0", "00FFFFFF"), ""},
		"flags that are not hexadecimal":                 {route("eth0", "00000000", "0101A8C0", "zz", "0", "00000000"), ""},
		"a metric that is not a number":                  {route("eth0", "00000000", "0101A8C0", "0003", "low", "00000000"), ""},
		"a gateway that is not hexadecimal":              {route("eth0", "00000000", "nothex!!", "0003", "0", "00000000"), ""},
		"a gateway that is too short":                    {route("eth0", "00000000", "01", "0003", "0", "00000000"), ""},
		"a line cut short":                               {"short\n", ""},
		"no routes":                                      {"", ""},
	} {
		got, err := gatewayIn(strings.NewReader(header + tt.table))
		switch {
		case tt.want == "" && !errors.Is(err, ErrNoGateway):
			t.Errorf("%s: %v, %v; want no gateway", name, got, err)
		case tt.want != "" && (err != nil || got != netip.MustParseAddr(tt.want)):
			t.Errorf("%s: %v, %v; want %s", name, got, err, tt.want)
		}
	}
}

// The default gateway is the one the kernel chose, as macOS and the BSDs
// print it for `route -n get default`.
func TestTheGatewayIsReadFromRouteGet(t *testing.T) {
	for _, tt := range []struct {
		name, answer string
		want         netip.Addr
	}{
		{"macOS", `   route to: default
destination: default
       mask: default
    gateway: 192.168.1.1
  interface: en0
      flags: <UP,GATEWAY,DONE,STATIC,PRCLONING,GLOBAL>
 recvpipe  sendpipe  ssthresh  rtt,msec    rttvar  hopcount      mtu     expire
       0         0         0         0         0         0      1500         0
`, netip.MustParseAddr("192.168.1.1")},
		{"FreeBSD", `   route to: default
destination: default
       mask: default
    gateway: 10.0.2.2
        fib: 0
  interface: em0
      flags: <UP,GATEWAY,DONE,STATIC>
`, netip.MustParseAddr("10.0.2.2")},
		{"through an interface", "   route to: default\n  interface: utun3\n      flags: <UP,DONE,STATIC>\n", netip.Addr{}},
		{"gateway that is no address", "    gateway: link#18\n", netip.Addr{}},
		{"unspecified", "    gateway: 0.0.0.0\n", netip.Addr{}},
		{"ipv6", "    gateway: fe80::1%en0\n", netip.Addr{}},
	} {
		got, err := gatewayInRouteGet(strings.NewReader(tt.answer))
		if tt.want.IsValid() && (err != nil || got != tt.want) || !tt.want.IsValid() && !errors.Is(err, ErrNoGateway) {
			t.Errorf("%s: got %v, %v; want %v", tt.name, got, err, tt.want)
		}
	}
}

// The default gateway is the next hop of the route Windows chose, as
// PowerShell prints it.
func TestTheGatewayIsReadFromTheNextHop(t *testing.T) {
	for _, tt := range []struct {
		name, answer string
		want         netip.Addr
	}{
		{"one", "192.168.178.1\r\n", netip.MustParseAddr("192.168.178.1")},
		{"on the link first", "0.0.0.0\r\n10.0.0.1\r\n", netip.MustParseAddr("10.0.0.1")},
		{"on the link only", "0.0.0.0\r\n", netip.Addr{}},
		{"ipv6", "fe80::1\r\n", netip.Addr{}},
		{"nothing", "", netip.Addr{}},
	} {
		got, err := gatewayInNextHop(strings.NewReader(tt.answer))
		if tt.want.IsValid() && (err != nil || got != tt.want) || !tt.want.IsValid() && !errors.Is(err, ErrNoGateway) {
			t.Errorf("%s: got %v, %v; want %v", tt.name, got, err, tt.want)
		}
	}
}

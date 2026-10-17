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

// The default gateway is found in netstat's table as macOS and the BSDs
// print it: the first default route that is up and through an address.
func TestTheGatewayIsReadFromNetstat(t *testing.T) {
	for _, tt := range []struct {
		name, table string
		want        netip.Addr
	}{
		{"macOS", `Routing tables

Internet:
Destination        Gateway            Flags               Netif Expire
default            link#18            UCSIg             utun3
default            192.168.1.1        UGScg               en0
default            10.0.0.1           UGScIg              en1
127                127.0.0.1          UCS                   lo0
192.168.1          link#6             UCS                   en0      !
`, netip.MustParseAddr("192.168.1.1")},
		{"FreeBSD", `Routing tables

Internet:
Destination        Gateway            Flags     Netif Expire
default            10.0.2.2           UGS         em0
10.0.2.0/24        link#1             U           em0
`, netip.MustParseAddr("10.0.2.2")},
		{"down", "default            10.0.2.2           GS         em0\n", netip.Addr{}},
		{"no gateway", "default            10.0.2.2           US         em0\n", netip.Addr{}},
		{"unspecified", "default            0.0.0.0           UGS         em0\n", netip.Addr{}},
		{"ipv6", "default            fe80::1%en0        UGcg        en0\n", netip.Addr{}},
		{"short", "default\n", netip.Addr{}},
	} {
		got, err := gatewayInNetstat(strings.NewReader(tt.table))
		if tt.want.IsValid() && (err != nil || got != tt.want) || !tt.want.IsValid() && !errors.Is(err, ErrNoGateway) {
			t.Errorf("%s: got %v, %v; want %v", tt.name, got, err, tt.want)
		}
	}
}

// The default gateway is found in what Windows prints, whatever language
// its headers are in: the active default route of lowest metric.
func TestTheGatewayIsReadFromRoutePrint(t *testing.T) {
	for _, tt := range []struct {
		name, table string
		want        netip.Addr
	}{
		{"English", `===========================================================================
Interface List
 12...00 15 5d 01 02 03 ......Hyper-V Virtual Ethernet Adapter
===========================================================================

IPv4 Route Table
===========================================================================
Active Routes:
Network Destination        Netmask          Gateway       Interface  Metric
          0.0.0.0          0.0.0.0      192.168.1.1    192.168.1.100     35
          0.0.0.0          0.0.0.0         10.0.0.1       10.0.0.20     25
          0.0.0.0          0.0.0.0         10.0.0.9       10.0.0.21     25
===========================================================================
Persistent Routes:
  Network Address          Netmask  Gateway Address  Metric
          0.0.0.0          0.0.0.0      172.16.0.1  Default
===========================================================================
`, netip.MustParseAddr("10.0.0.1")},
		{"German", `IPv4-Routentabelle
===========================================================================
Aktive Routen:
     Netzwerkziel    Netzwerkmaske          Gateway    Schnittstelle Metrik
          0.0.0.0          0.0.0.0     192.168.178.1   192.168.178.20     50
===========================================================================
Ständige Routen:
  Keine
`, netip.MustParseAddr("192.168.178.1")},
		{"on-link only", "          0.0.0.0          0.0.0.0         On-link       10.0.0.20     25\n", netip.Addr{}},
		{"unspecified", "          0.0.0.0          0.0.0.0         0.0.0.0       10.0.0.20     25\n", netip.Addr{}},
		{"bad metric", "          0.0.0.0          0.0.0.0         10.0.0.1       10.0.0.20     many\n", netip.Addr{}},
		{"not default", "        10.0.0.0    255.255.255.0         10.0.0.1       10.0.0.20     25\n", netip.Addr{}},
	} {
		got, err := gatewayInRoutePrint(strings.NewReader(tt.table))
		if tt.want.IsValid() && (err != nil || got != tt.want) || !tt.want.IsValid() && !errors.Is(err, ErrNoGateway) {
			t.Errorf("%s: got %v, %v; want %v", tt.name, got, err, tt.want)
		}
	}
}

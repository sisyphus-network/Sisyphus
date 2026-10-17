package natpmp

import (
	"net/netip"
	"os"
	"path/filepath"
	"testing"
)

// On Linux the routing table is read from where the kernel lists it.
func TestTheGatewayIsReadFromTheKernelsTable(t *testing.T) {
	file := filepath.Join(t.TempDir(), "route")
	os.WriteFile(file, []byte(header+route("eth0", "00000000", "0101A8C0", "0003", "0", "00000000")), 0o600)
	old := routes
	defer func() { routes = old }()
	routes = file
	if got, err := Gateway(); err != nil || got != netip.MustParseAddr("192.168.1.1") {
		t.Errorf("Gateway() = %v, %v", got, err)
	}
	routes = filepath.Join(t.TempDir(), "missing")
	if _, err := Gateway(); err == nil {
		t.Error("a routing table that is not there gave a gateway")
	}
}

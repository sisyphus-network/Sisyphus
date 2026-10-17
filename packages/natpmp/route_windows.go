package natpmp

import (
	"bytes"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
)

// towardInternet is an address beyond every local network, in a block set
// aside for documentation (RFC 5737), that Windows is asked how it would
// reach. Find-NetRoute only looks the route up; nothing is sent to it.
const towardInternet = "192.0.2.1"

// Gateway returns the machine's IPv4 default gateway: the next hop of the
// route Windows itself chooses for an address on the internet, weighing
// route and interface metrics as it does. Nothing is sent anywhere to find
// it.
func Gateway() (netip.Addr, error) {
	powershell := "powershell.exe"
	if root := os.Getenv("SystemRoot"); root != "" {
		powershell = filepath.Join(root, "System32", "WindowsPowerShell", "v1.0", "powershell.exe")
	}
	answer, err := exec.Command(powershell, "-NoProfile", "-NonInteractive", "-Command",
		"Find-NetRoute -RemoteIPAddress "+towardInternet+" | Where-Object NextHop | ForEach-Object NextHop").Output()
	if err != nil {
		return netip.Addr{}, err
	}
	return gatewayInNextHop(bytes.NewReader(answer))
}

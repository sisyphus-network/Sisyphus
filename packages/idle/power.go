package idle

import (
	"io/fs"
	"path"
	"strings"
)

// dischargingIn reports whether any battery that a tree laid out as
// Linux's /sys/class/power_supply lists is discharging: one directory a
// supply, whose type file says Battery and whose status file says
// Discharging. The battery of a wireless mouse or keyboard, whose scope
// file says Device, is not the machine's. A machine whose batteries are charging, full or absent runs
// on mains power, as far as can be told.
func dischargingIn(supplies fs.FS) bool {
	listed, _ := fs.ReadDir(supplies, ".")
	for _, supply := range listed {
		read := func(name string) string {
			said, _ := fs.ReadFile(supplies, path.Join(supply.Name(), name))
			return strings.TrimSpace(string(said))
		}
		if read("type") == "Battery" && read("scope") != "Device" && read("status") == "Discharging" {
			return true
		}
	}
	return false
}

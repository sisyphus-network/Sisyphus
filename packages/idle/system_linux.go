package idle

import (
	"os"
	"time"
)

// System returns the Detector of the system the node runs on: on Linux,
// what logind says of the sessions of the people logged in, or only of
// those of the given accounts if any are given.
func System(users []string) Detector { return logind{run: output, now: time.Now, users: users} }

// ByUser is whether System tells the sessions of one account from those
// of another, and so whether the users given it change anything.
const ByUser = true

// RunsOnBattery reports whether the machine runs on battery now.
func RunsOnBattery() bool { return dischargingIn(os.DirFS("/sys/class/power_supply")) }

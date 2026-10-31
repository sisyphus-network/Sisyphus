package idle

import (
	"os"
	"time"
)

// System returns the Detector of the system the node runs on: on Linux,
// what logind says of the sessions of the people logged in.
func System() Detector { return logind{run: output, now: time.Now} }

// RunsOnBattery reports whether the machine runs on battery now.
func RunsOnBattery() bool { return dischargingIn(os.DirFS("/sys/class/power_supply")) }

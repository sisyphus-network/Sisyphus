package idle

// System returns the Detector of the system the node runs on: on macOS,
// IOKit's HIDIdleTime, as ioreg lists it.
func System() Detector { return ioreg{run: output} }

// RunsOnBattery reports whether the machine runs on battery now. On macOS
// that is not yet told, and taken not to be.
func RunsOnBattery() bool { return false }

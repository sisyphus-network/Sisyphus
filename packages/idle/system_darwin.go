package idle

// System returns the Detector of the system the node runs on: on macOS,
// IOKit's HIDIdleTime, as ioreg lists it. That is the time since the last
// input at the machine's own keyboard, mouse or trackpad, by whoever is at
// the console, so users changes nothing.
func System(users []string) Detector { return ioreg{run: output} }

// ByUser is whether System tells the sessions of one account from those
// of another, and so whether the users given it change anything.
const ByUser = false

// RunsOnBattery reports whether the machine runs on battery now. On macOS
// that is not yet told, and taken not to be.
func RunsOnBattery() bool { return false }

//go:build !linux && !darwin && !windows

package idle

// System returns the Detector of the system the node runs on, which here
// cannot tell when its owner last used it.
func System(users []string) Detector { return Unknown{} }

// ByUser is whether System tells the sessions of one account from those
// of another, and so whether the users given it change anything.
const ByUser = false

// RunsOnBattery reports whether the machine runs on battery now, which here
// is not told, and taken not to be.
func RunsOnBattery() bool { return false }

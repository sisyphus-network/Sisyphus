//go:build !linux && !darwin && !windows

package idle

// System returns the Detector of the system the node runs on, which here
// cannot tell when its owner last used it.
func System() Detector { return Unknown{} }

// RunsOnBattery reports whether the machine runs on battery now, which here
// is not told, and taken not to be.
func RunsOnBattery() bool { return false }

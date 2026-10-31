// Package idle tells whether a machine's owner is using it, so that a
// worker set to share the machine only while they are away takes new tasks
// only then.
//
// What it can tell depends on the system: how long since the last
// keyboard or mouse input, from logind on Linux, from IOKit's HIDIdleTime
// on macOS and from GetLastInputInfo on Windows, and on Linux whether the
// machine runs on battery. Where it cannot tell, it says so, and a worker
// takes that as its owner being there.
package idle

import (
	"context"
	"fmt"
	"math"
	"os/exec"
	"time"
)

// Detector tells how long the machine's owner has been away.
type Detector interface {
	// Idle returns how long it has been since anyone last used the
	// machine's keyboard or mouse, and whether that is known at all.
	Idle(ctx context.Context) (since time.Duration, known bool)
}

// Forever is how long a machine that nobody is logged in to has been idle.
const Forever = time.Duration(math.MaxInt64)

// Unknown is the Detector of a system whose idleness cannot be told: it
// never knows.
type Unknown struct{}

func (Unknown) Idle(context.Context) (time.Duration, bool) { return 0, false }

// Mode is when a machine's owner lets it take new tasks.
type Mode int

const (
	// Always takes them whenever it has a free slot, as servers do.
	Always Mode = iota
	// WhenIdle takes them only while the owner is away.
	WhenIdle
	// Paused takes none.
	Paused
)

// modes are the modes by name, as an owner gives them.
var modes = map[string]Mode{"always": Always, "idle": WhenIdle, "paused": Paused}

// ParseMode returns the mode of the given name: always, idle or paused.
func ParseMode(name string) (Mode, error) {
	if mode, ok := modes[name]; ok {
		return mode, nil
	}
	return Always, fmt.Errorf("%q is not a way to share: give always, idle or paused", name)
}

func (m Mode) String() string {
	for name, mode := range modes {
		if mode == m {
			return name
		}
	}
	return fmt.Sprintf("Mode(%d)", int(m))
}

// Policy is when a machine's owner lets it take new tasks.
type Policy struct {
	Mode Mode
	// After is how long the owner must have been away, in WhenIdle mode,
	// before the machine takes new tasks.
	After time.Duration
	// Detector tells how long that has been. Nil never knows.
	Detector Detector
	// OnBattery, if set, says whether the machine runs on battery now, in
	// which case it takes no new tasks in WhenIdle mode.
	OnBattery func() bool
}

// Reason is why a machine takes new tasks now, or does not.
type Reason int

const (
	// Free means it takes them.
	Free Reason = iota
	// PausedByOwner means its owner has paused it.
	PausedByOwner
	// InUse means its owner is using it, or used it too lately.
	InUse
	// NotKnown means it cannot tell whether its owner is using it.
	NotKnown
	// OnBattery means it runs on battery.
	OnBattery
)

func (r Reason) String() string {
	switch r {
	case Free:
		return "its owner lets it take tasks"
	case PausedByOwner:
		return "its owner has paused it"
	case InUse:
		return "its owner is using it"
	case NotKnown:
		return "it cannot tell whether its owner is using it, and so takes them to be"
	case OnBattery:
		return "it is running on battery"
	}
	return fmt.Sprintf("Reason(%d)", int(r))
}

// Check says whether the machine takes new tasks now, and why.
func (p Policy) Check(ctx context.Context) Reason {
	switch p.Mode {
	case Always:
		return Free
	case Paused:
		return PausedByOwner
	}
	if p.OnBattery != nil && p.OnBattery() {
		return OnBattery
	}
	detector := p.Detector
	if detector == nil {
		detector = Unknown{}
	}
	since, known := detector.Idle(ctx)
	switch {
	case !known:
		return NotKnown
	case since < p.After:
		return InUse
	}
	return Free
}

// output runs a program and returns what it printed.
func output(ctx context.Context, name string, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, name, args...).Output()
}

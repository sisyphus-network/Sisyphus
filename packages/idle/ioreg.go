package idle

import (
	"bytes"
	"context"
	"strconv"
	"time"
)

// ioreg tells how long the machine's owner has been away from the
// HIDIdleTime macOS's IOHIDSystem keeps, by asking ioreg: the time since
// the last input from any keyboard, mouse or trackpad, whoever is logged in.
type ioreg struct {
	// run runs a program and returns what it printed.
	run func(ctx context.Context, name string, args ...string) ([]byte, error)
}

func (i ioreg) Idle(ctx context.Context) (time.Duration, bool) {
	listed, err := i.run(ctx, "ioreg", "-c", "IOHIDSystem", "-d", "4")
	if err != nil {
		return 0, false
	}
	return hidIdle(listed)
}

// hidIdle reads the first HIDIdleTime, in nanoseconds, from what ioreg
// lists: a line such as `| |   "HIDIdleTime" = 1234567890`.
func hidIdle(listed []byte) (time.Duration, bool) {
	key := []byte(`"HIDIdleTime" = `)
	for line := range bytes.Lines(listed) {
		_, value, found := bytes.Cut(line, key)
		if !found {
			continue
		}
		nanoseconds, err := strconv.ParseInt(string(bytes.TrimSpace(value)), 10, 64)
		if err != nil || nanoseconds < 0 {
			return 0, false
		}
		return time.Duration(nanoseconds), true
	}
	return 0, false
}

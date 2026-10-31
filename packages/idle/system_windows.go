package idle

import (
	"context"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	getLastInputInfo = windows.NewLazySystemDLL("user32.dll").NewProc("GetLastInputInfo")
	getTickCount     = windows.NewLazySystemDLL("kernel32.dll").NewProc("GetTickCount")
)

// lastInputInfo is Windows' LASTINPUTINFO.
type lastInputInfo struct {
	size uint32
	time uint32
}

// lastInput tells how long the machine's owner has been away from what
// GetLastInputInfo says of the session the node runs in. A node that runs
// as a service runs in session 0, where nobody types, so there it is not
// known.
type lastInput struct{}

func (lastInput) Idle(context.Context) (time.Duration, bool) {
	var session uint32
	if err := windows.ProcessIdToSessionId(windows.GetCurrentProcessId(), &session); err != nil || session == 0 {
		return 0, false
	}
	info := lastInputInfo{size: uint32(unsafe.Sizeof(lastInputInfo{}))}
	if ok, _, _ := getLastInputInfo.Call(uintptr(unsafe.Pointer(&info))); ok == 0 {
		return 0, false
	}
	// Both count milliseconds since the machine started, in 32 bits, which
	// wrap every 49.7 days: the difference is right across a wrap.
	now, _, _ := getTickCount.Call()
	return time.Duration(uint32(now)-info.time) * time.Millisecond, true
}

// System returns the Detector of the system the node runs on: on Windows,
// GetLastInputInfo.
func System() Detector { return lastInput{} }

// RunsOnBattery reports whether the machine runs on battery now. On
// Windows that is not yet told, and taken not to be.
func RunsOnBattery() bool { return false }

package runtime

import (
	"context"
	"io"
	"maps"
	"sync"
)

// The share of the processor a container is given while the machine's
// owner is using it, and the one it is given otherwise, which is Docker's
// own. A share counts only when the processor is wanted by more than can
// have it: a container with the low share then has 2 parts in 1026 of
// what it contends for with anything of the owner's, and when nothing else
// wants the processor it has as much as it likes.
const (
	lowShares   = "2"
	usualShares = "1024"
)

// engineFunc runs a container engine's command, as Container.Engine does.
type engineFunc = func(ctx context.Context, args []string, stdout, stderr io.Writer) (status int, err error)

// containers holds the containers running the pool's tasks on this machine,
// by name, with the engine each runs in.
var containers = runningContainers{engines: make(map[string]engineFunc)}

type runningContainers struct {
	mu      sync.Mutex
	low     bool
	engines map[string]engineFunc
}

// start notes a container about to be run, and reports whether it is to
// start with the low share.
func (r *runningContainers) start(name string, in engineFunc) (low bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.engines[name] = in
	return r.low
}

// stop notes that a container has ended.
func (r *runningContainers) stop(name string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.engines, name)
}

// Throttle gives the containers running the pool's tasks on this machine
// the low share of the processor while low is true, as when their worker's
// owner comes back to it, and their usual share once it is false again.
// Those that start in the meantime start with the low share. Each running
// one is changed as its engine allows: one that has just ended, or is still
// being started and so cannot yet be changed, keeps the share it has.
//
// It changes nothing a task runs other than in a container: work done in
// the daemon itself, or by a model server, or on a graphics card, goes on
// as before.
func Throttle(ctx context.Context, low bool) {
	containers.mu.Lock()
	if containers.low == low {
		containers.mu.Unlock()
		return
	}
	containers.low = low
	running := maps.Clone(containers.engines)
	containers.mu.Unlock()
	shares := usualShares
	if low {
		shares = lowShares
	}
	for name, in := range running {
		in(ctx, []string{"update", "--cpu-shares", shares, name}, io.Discard, io.Discard)
	}
}

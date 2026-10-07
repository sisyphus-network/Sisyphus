package worker

import (
	"context"
	"sync"
)

// Slots shares out a node's capacity among the pools it works for. A node
// runs one worker for each, and they would otherwise each think the whole
// machine was theirs.
//
// It is fair in this sense: when a slot comes free and tasks of several
// pools are waiting, it goes to the pool with the fewest tasks running. A
// pool that sends a hundred tasks does not keep another's one task waiting
// behind them.
type Slots struct {
	mu      sync.Mutex
	changed *sync.Cond
	free    int
	running map[string]int
	waiting map[string]int
}

// NewSlots returns room for n tasks at once.
func NewSlots(n int) *Slots {
	s := &Slots{free: n, running: make(map[string]int), waiting: make(map[string]int)}
	s.changed = sync.NewCond(&s.mu)
	return s
}

// Acquire waits for a slot for a task of the given pool and takes it. It
// returns false, having taken nothing, if ctx ends first.
func (s *Slots) Acquire(ctx context.Context, pool string) bool {
	// Waking everyone when ctx ends lets the wait below notice.
	stop := context.AfterFunc(ctx, func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.changed.Broadcast()
	})
	defer stop()

	s.mu.Lock()
	defer s.mu.Unlock()
	s.waiting[pool]++
	defer func() { s.waiting[pool]-- }()
	for s.free == 0 || !s.nextLocked(pool) {
		if ctx.Err() != nil {
			return false
		}
		s.changed.Wait()
	}
	s.free--
	s.running[pool]++
	return true
}

// nextLocked reports whether pool is next in line: no pool with a task
// waiting has fewer running.
func (s *Slots) nextLocked(pool string) bool {
	for other, waiting := range s.waiting {
		if waiting > 0 && s.running[other] < s.running[pool] {
			return false
		}
	}
	return true
}

// Release gives back a slot a task of the given pool had taken.
func (s *Slots) Release(pool string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.free++
	s.running[pool]--
	s.changed.Broadcast()
}

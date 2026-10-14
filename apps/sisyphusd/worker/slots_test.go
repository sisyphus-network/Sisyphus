package worker

import (
	"context"
	"testing"
	"time"
)

// taken reports whether an attempt to take a slot has succeeded yet.
func taken(done <-chan bool) bool {
	select {
	case <-done:
		return true
	case <-time.After(50 * time.Millisecond):
		return false
	}
}

func acquire(s *Slots, pool string) <-chan bool {
	done := make(chan bool, 1)
	go func() { done <- s.Acquire(context.Background(), pool) }()
	return done
}

// count returns how many more attempts on ran succeed within a moment.
func count(ran <-chan bool) (n int) {
	for {
		select {
		case <-ran:
			n++
		case <-time.After(50 * time.Millisecond):
			return n
		}
	}
}

func TestSlotsAreSharedOutFairlyBetweenPools(t *testing.T) {
	s := NewSlots(2)
	ctx := context.Background()
	// One pool fills the node and queues three more tasks behind them.
	if first, second := s.Acquire(ctx, "greedy"), s.Acquire(ctx, "greedy"); !first || !second {
		t.Fatal("a free slot could not be taken")
	}
	queued := make(chan bool, 3)
	for range 3 {
		go func() { queued <- s.Acquire(ctx, "greedy") }()
	}
	if n := count(queued); n != 0 {
		t.Fatalf("%d more tasks ran on a full node", n)
	}
	// Another pool's one task arrives last, and goes first.
	modest := acquire(s, "modest")
	if taken(modest) {
		t.Fatal("a task ran with no slot free")
	}
	s.Release("greedy")
	if !taken(modest) {
		t.Fatal("the pool with nothing running was kept waiting behind the one with a queue")
	}
	if n := count(queued); n != 0 {
		t.Fatalf("%d of the queued tasks ran in the slot that went to the other pool", n)
	}
	// The next slot goes back to the first pool, which now has no more
	// running than the other.
	s.Release("greedy")
	if n := count(queued); n != 1 {
		t.Fatalf("%d of the queued tasks ran when one slot came free, want 1", n)
	}
	// And as slots come back, the rest of the queue drains.
	s.Release("modest")
	s.Release("greedy")
	if n := count(queued); n != 2 {
		t.Fatalf("%d of the last two queued tasks ran once two slots were free", n)
	}
}

func TestWaitingForASlotEndsWhenTheWaiterGivesUp(t *testing.T) {
	s := NewSlots(1)
	s.Acquire(context.Background(), "a")
	ctx, cancel := context.WithCancel(context.Background())
	gaveUp := make(chan bool, 1)
	go func() { gaveUp <- s.Acquire(ctx, "b") }()
	time.Sleep(20 * time.Millisecond)
	cancel()
	select {
	case got := <-gaveUp:
		if got {
			t.Fatal("a slot was taken that nobody released")
		}
	case <-time.After(30 * time.Second):
		t.Fatal("a cancelled wait for a slot never returned")
	}
	// Giving up took nothing: the slot, once free, can be had.
	s.Release("a")
	if !s.Acquire(context.Background(), "b") {
		t.Error("the slot was lost")
	}
}

func TestWhoseTurnItIs(t *testing.T) {
	s := NewSlots(4)
	s.running["greedy"], s.running["modest"] = 2, 1
	// With nobody else waiting, anyone may go.
	if !s.nextLocked("greedy") || !s.nextLocked("modest") {
		t.Error("a pool was held back with nobody else waiting")
	}
	// With a task of the pool that has less running waiting, the other
	// gives way to it, and not the other way about.
	s.waiting["modest"], s.waiting["greedy"] = 1, 1
	if s.nextLocked("greedy") {
		t.Error("the pool with more running went ahead of one with less")
	}
	if !s.nextLocked("modest") {
		t.Error("the pool with less running was held back")
	}
}

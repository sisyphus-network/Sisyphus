package worker

import (
	"sync"
	"testing"
	"time"

	pb "github.com/sisyphus-network/Sisyphus/packages/protocol/sisyphus/v1"
)

func TestReportsWakeOnceForCoalescedProgressAndLogs(t *testing.T) {
	changed := make(chan struct{}, 1)
	r := &reports{changed: changed}
	r.Progress(0.25)
	r.Log("first")
	r.Progress(0.75)
	r.Log("second")
	select {
	case <-changed:
	default:
		t.Fatal("task report did not wake the sender")
	}
	select {
	case <-changed:
		t.Fatal("coalesced task reports scheduled redundant sends")
	default:
	}

	update := r.take(&pb.TaskAssignment{TaskId: "job/0", Attempt: 3})
	if update == nil || update.GetTaskId() != "job/0" || update.GetAttempt() != 3 || update.GetProgress() != 0.75 {
		t.Fatalf("task update = %v", update)
	}
	if got := update.GetLog(); len(got) != 2 || got[0] != "first" || got[1] != "second" {
		t.Fatalf("coalesced logs = %v", got)
	}
	if update := r.take(&pb.TaskAssignment{}); update != nil {
		t.Fatalf("an already-sent report was repeated: %v", update)
	}
}

func TestPrivateTaskLogsWakeOnlyForTheWithheldMarker(t *testing.T) {
	changed := make(chan struct{}, 1)
	r := &reports{private: true, changed: changed}
	r.Log("secret one")
	r.Log("secret two")
	select {
	case <-changed:
	default:
		t.Fatal("withheld marker did not wake the sender")
	}
	select {
	case <-changed:
		t.Fatal("private log contents caused repeated notifications")
	default:
	}
	update := r.take(&pb.TaskAssignment{TaskId: "private/0"})
	if update == nil || len(update.GetLog()) != 1 || update.GetLog()[0] != withheldLine {
		t.Fatalf("private task update = %v", update)
	}
}

// A change after a quiet spell is passed on at once. Changes that follow
// within the gap wait out the rest of it and go together, and none is lost
// when the task ends first.
func TestTaskUpdatesAreSentAtOnceButNoMoreOftenThanTheGap(t *testing.T) {
	const gap = 50 * time.Millisecond
	changed, done := make(chan struct{}, 1), make(chan struct{})
	var mu sync.Mutex
	var passed []time.Time
	sent := make(chan struct{}, 10)
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		passOn(changed, done, gap, func() {
			mu.Lock()
			passed = append(passed, time.Now())
			mu.Unlock()
			sent <- struct{}{}
		})
	}()
	start := time.Now()
	changed <- struct{}{}
	<-sent
	if waited := time.Since(start); waited >= gap {
		t.Errorf("the first change waited %v", waited)
	}
	for range 3 {
		changed <- struct{}{}
	}
	<-sent
	select {
	case <-sent:
		t.Error("changes within one gap were sent more than once")
	case <-time.After(2 * gap):
	}
	// After a quiet spell the next change goes at once again. One just after
	// it waits, and if the task ends first it is left for the pass made as
	// the task ends.
	changed <- struct{}{}
	<-sent
	changed <- struct{}{}
	close(done)
	<-stopped
	mu.Lock()
	defer mu.Unlock()
	if len(passed) != 3 || passed[1].Sub(passed[0]) < gap {
		t.Errorf("passed at %v, want three times, the first two at least %v apart", passed, gap)
	}
}

package replication

import (
	"context"
	"slices"
	"testing"
	"time"

	pb "github.com/sisyphus-network/Sisyphus/packages/protocol/sisyphus/v1"
	"github.com/sisyphus-network/Sisyphus/packages/storage"
)

// owedOf returns what the manager keeps to try again for a follower.
func (r *rig) owedOf(id string) (list *pb.KeepList, at time.Time, wait time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	f := r.Manager.followers[id]
	if f == nil {
		return nil, time.Time{}, 0
	}
	return f.owedList, f.owedAt, f.owedWait
}

func (r *rig) strandedOf(id string) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if f := r.Manager.followers[id]; f != nil {
		return slices.Clone(f.stranded)
	}
	return nil
}

// A follower whose list is shown before it has connected as a worker has
// nothing taken back at first. What its list names is tried again on the
// manager's own schedule, without the follower showing its list again, and
// once it comes back the follower is given a list for the store the node has
// now (#470).
func TestWhatCouldNotBeTakenBackIsTriedAgainWithoutTheFollowerAsking(t *testing.T) {
	graceOf(t, time.Minute)
	r := newRig(t, 1)
	theirs := storage.NewMemory()
	blob := put(t, theirs, "held for the store that was lost")
	expired := put(t, theirs, "whose old keep time has passed")
	list := signedBy(r.ident, r.old,
		listed(blob, time.Time{}),
		listed(expired, time.Now().Add(-time.Second)),
	)

	// Not yet connected as a worker, the follower serves nothing.
	told := r.show("a", list, blob, expired)
	if told.GetStore() != r.old || len(restoredPins(r.store)) != 0 {
		t.Fatalf("before the follower serves anything it is told %v, and the node has taken back %v", told, restoredPins(r.store))
	}
	owed, at, wait := r.owedOf("a")
	if owed != list || at.IsZero() || wait != min(2*TakeBackRetry, Interval) {
		t.Fatalf("what could not be taken back is kept as %v, to try at %v and then after %v", owed, at, wait)
	}

	// A report on the stream that shows no list leaves the try pending.
	reported := r.Replicate(ctx, "a", &pb.ReplicateRequest{Store: r.old, Holding: []string{blob, expired}, KeepsLists: true})
	if again, _, _ := r.owedOf("a"); again != list {
		t.Fatal("a report that showed no list dropped what was still to be taken back")
	}
	if slices.Contains(reported.GetHold(), expired) {
		t.Error("a blob whose signed keep time expired was left stranded while another blob is still owed")
	}

	// Not yet due, nothing is tried.
	r.followers["a"] = theirs
	r.retakeBack(ctx, at.Add(-time.Nanosecond))
	if len(restoredPins(r.store)) != 0 {
		t.Fatal("a list was tried again before it was due")
	}

	// Due, and now served, it all comes back, and the follower's next
	// assignment is for the store the node has now.
	r.retakeBack(ctx, at)
	if pins := restoredPins(r.store); len(pins) != 1 {
		t.Fatalf("tried again, the node took back %v, want the one blob", pins)
	}
	if owed, _, _ := r.owedOf("a"); owed != nil {
		t.Error("with everything taken back, the list is still kept to try again")
	}
	if next := r.changedAssignment("a", nil); next.GetStore() != r.now || len(next.GetBlobs()) != 1 {
		t.Errorf("after everything came back the follower would be pushed %v, want its blob for the store the node has now", next)
	}
	if status := r.Status(""); status.GetFromEarlierStore() != 0 || status.GetRestored() != 1 {
		t.Errorf("the node reports %v, want nothing left from the earlier store", status)
	}
}

// A stream report without its old signed list must not make a blob the
// coordinator already has look as if it is still owed by the follower.
func TestAStreamReportDoesNotTreatAnAlreadyPinnedBlobAsOwed(t *testing.T) {
	graceOf(t, time.Minute)
	r := newRig(t, 1)
	present := keep(t, r.store, "already pinned in the current store")
	theirs := storage.NewMemory()
	pending := put(t, theirs, "still to come from the old store")
	list := signedBy(r.ident, r.old, listed(present, time.Time{}), listed(pending, time.Time{}))
	r.show("a", list, present, pending)

	// This later stream report has no signed list, but the old list is still
	// retained while one of its blobs remains to be taken back.
	r.Replicate(ctx, "a", &pb.ReplicateRequest{Store: r.old, Holding: []string{present, pending}, KeepsLists: true})
	stranded := r.strandedOf("a")
	if slices.Contains(stranded, present) || !slices.Contains(stranded, pending) {
		t.Errorf("the report leaves %v stranded; want only the outstanding blob %q", stranded, pending)
	}
}

// A try that fails again waits twice as long before the next, up to the
// interval at which a follower would have asked.
func TestATakeBackThatFailsAgainBacksOffUpToTheInterval(t *testing.T) {
	graceOf(t, time.Minute)
	r := newRig(t, 1)
	blob := put(t, storage.NewMemory(), "never served")
	r.show("a", signedBy(r.ident, r.old, listed(blob, time.Time{})), blob)

	_, at, wait := r.owedOf("a")
	for range 10 {
		before := time.Now()
		r.retakeBack(ctx, at)
		var next time.Duration
		_, at, next = r.owedOf("a")
		if at.Before(before.Add(wait)) {
			t.Fatalf("the next try is at %v, sooner than %v after the last", at, wait)
		}
		if next != min(2*wait, Interval) {
			t.Fatalf("after a wait of %v the next is %v, want %v", wait, next, min(2*wait, Interval))
		}
		wait = next
	}
	if wait != Interval {
		t.Errorf("the wait settled at %v, want %v", wait, Interval)
	}
	if len(restoredPins(r.store)) != 0 {
		t.Error("something was taken back from a follower that serves nothing")
	}
}

// A follower that goes while its list is being tried again is left gone,
// and another tried in the same pass is not held up by it.
func TestATakeBackTriedAgainForAFollowerThatWentChangesNothing(t *testing.T) {
	graceOf(t, time.Minute)
	r := newRig(t, 1)
	theirs := storage.NewMemory()
	blob := put(t, theirs, "fetched as the follower goes")
	other := put(t, storage.NewMemory(), "held by a follower that serves nothing yet")
	r.show("a", signedBy(r.ident, r.old, listed(blob, time.Time{})), blob)
	r.show("b", signedBy(r.ident, r.old, listed(other, time.Time{})), other)

	r.followers["a"] = theirs
	r.fetching = func() {
		r.mu.Lock()
		delete(r.Manager.followers, "a")
		r.mu.Unlock()
	}
	r.retakeBack(ctx, time.Now().Add(time.Hour))
	if _, known := r.Manager.followers["a"]; known {
		t.Error("trying a list again brought back a follower that had gone")
	}
	if owed, _, wait := r.owedOf("b"); owed == nil || wait != min(4*TakeBackRetry, Interval) {
		t.Errorf("the other follower's list was not tried in the same pass: kept %v, next wait %v", owed, wait)
	}
}

// Run tries an owed list again when it comes due, with nothing else to wake
// it.
func TestRunTriesAnOwedListAgainWhenItComesDue(t *testing.T) {
	graceOf(t, time.Minute)
	r := newRig(t, 1)
	r.Manager.takeBackRetry = 10 * time.Millisecond
	theirs := storage.NewMemory()
	blob := put(t, theirs, "taken back by the manager's own schedule")
	r.show("a", signedBy(r.ident, r.old, listed(blob, time.Time{})), blob)
	r.followers["a"] = theirs

	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { r.Run(runCtx); close(done) }()
	defer func() { cancel(); <-done }()
	eventually(t, "the owed blob is taken back", func() bool {
		r.taking.Lock()
		defer r.taking.Unlock()
		return len(restoredPins(r.store)) == 1
	})
}

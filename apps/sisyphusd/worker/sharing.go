package worker

import (
	"context"
	"time"

	"github.com/sisyphus-network/Sisyphus/packages/idle"
)

// sharingPoll is how often a worker that takes tasks only while its owner
// is away asks whether they are. Tests shorten it.
var sharingPoll = 5 * time.Second

// detectWait is the longest the worker waits to be told whether its owner
// is away; one not told in time takes them to be there.
const detectWait = 2 * time.Second

// availability says whether the worker's owner lets it take new tasks now,
// and if not, why not.
func (w *Worker) availability(ctx context.Context) idle.Reason {
	if w.Sharing == nil {
		return idle.Free
	}
	asking, cancel := context.WithTimeout(ctx, detectWait)
	defer cancel()
	return w.Sharing.Check(asking)
}

// sharesSometimes reports whether the worker's owner may keep it from
// taking new tasks, and watchesOwner whether that can change while it runs.
func (w *Worker) sharesSometimes() bool { return w.Sharing != nil && w.Sharing.Mode != idle.Always }
func (w *Worker) watchesOwner() bool    { return w.Sharing != nil && w.Sharing.Mode == idle.WhenIdle }

// watchOwner asks every sharingPoll whether the worker's owner lets it
// take new tasks, until ctx ends. When the answer changes it says so in
// the log; when it changes between yes and no, it calls changed and, while
// the answer is no, has the tasks running give way to the owner's work.
func (w *Worker) watchOwner(ctx context.Context, was idle.Reason, running func() int32, changed func(free bool)) {
	ticker := time.NewTicker(sharingPoll)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
		case <-ctx.Done():
			return
		}
		now := w.availability(ctx)
		if ctx.Err() != nil || now == was {
			continue
		}
		w.sayAvailability(now, running())
		if (now == idle.Free) != (was == idle.Free) {
			changed(now == idle.Free)
			w.throttle(ctx, now != idle.Free)
		}
		was = now
	}
}

// sayAvailability says in the log whether the worker takes new tasks, and
// why.
func (w *Worker) sayAvailability(now idle.Reason, running int32) {
	switch now {
	case idle.Free:
		w.Log.Info("taking new tasks", "node", w.Name, "because", now.String())
	case idle.NotKnown:
		w.Log.Warn("taking no new tasks", "node", w.Name, "because", now.String(), "running", running)
	default:
		w.Log.Info("taking no new tasks", "node", w.Name, "because", now.String(), "running", running)
	}
}

// throttle has the tasks running give way to the owner's work while low is
// true, and stop giving way once it is false, if the worker knows how.
func (w *Worker) throttle(ctx context.Context, low bool) {
	if w.Throttle != nil {
		w.Throttle(ctx, low)
	}
}

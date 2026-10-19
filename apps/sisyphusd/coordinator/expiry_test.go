package coordinator

import (
	"context"
	"log/slog"
	"testing"
	"time"

	jobmodel "github.com/sisyphus-network/Sisyphus/packages/job-model"
	"github.com/sisyphus-network/Sisyphus/packages/runtime"
	"github.com/sisyphus-network/Sisyphus/packages/storage"
)

func TestNextAttemptExpiryFindsTheNearestTimedAttempt(t *testing.T) {
	start := time.Now()
	first := &jobmodel.Job{TaskTimeout: time.Minute}
	untimed := &jobmodel.Job{}
	c := &Coordinator{workers: map[string]*worker{
		"later":   {running: map[string]assignment{"later": {job: first, started: start.Add(time.Second)}}},
		"earlier": {running: map[string]assignment{"earlier": {job: first, started: start}}},
		"untimed": {running: map[string]assignment{"untimed": {job: untimed, started: start.Add(-time.Hour)}}},
	}}

	got, found := c.nextAttemptExpiry()
	if !found || !got.Equal(start.Add(time.Minute)) {
		t.Fatalf("nearest expiry = %v, found %v; want %v", got, found, start.Add(time.Minute))
	}
}

func TestRunningAttemptExpiresAtItsDeadline(t *testing.T) {
	c := New(Config{Workloads: runtime.Builtin(), Store: storage.NewMemory(), Log: slog.New(slog.DiscardHandler)})
	defer c.Close()

	job := jobmodel.New("expiry-test", "primes", nil, jobmodel.Distributed, 1, [][]byte{{1}}, time.Now())
	job.TaskTimeout = 40 * time.Millisecond
	job.Tasks[0].Failures = maxAttempts - 1
	job.Start(job.Tasks[0], "worker", "worker", time.Now())
	attempt := assignment{job: job, task: job.Tasks[0], attempt: job.Tasks[0].Attempt, started: job.Tasks[0].StartedAt}
	worker := &worker{id: "worker", name: "worker", send: newOutbox(), running: map[string]assignment{job.Tasks[0].ID: attempt}}

	c.mu.Lock()
	c.jobs[job.ID] = job
	c.changed[job.ID] = make(chan struct{})
	c.workers[worker.id] = worker
	c.notifyExpiryChangedLocked()
	c.mu.Unlock()

	deadline := time.Now().Add(eventualWait)
	for time.Now().Before(deadline) {
		c.mu.Lock()
		state := job.State
		c.mu.Unlock()
		if state == jobmodel.Failed {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("attempt was not expired at its deadline")
}

// The deadline watcher stops when the coordinator does, even while it waits
// for a deadline that is still far off.
func TestTheExpiryWatcherStopsWhileWaitingForADeadline(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	c := &Coordinator{ctx: ctx, expiryChanged: make(chan struct{}, 1), workers: map[string]*worker{
		"worker": {running: map[string]assignment{"task": {job: &jobmodel.Job{TaskTimeout: time.Hour}, started: time.Now()}}},
	}}
	done := make(chan struct{})
	go func() {
		defer close(done)
		c.watchAttemptExpirations()
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the watcher kept waiting after the coordinator stopped")
	}
}

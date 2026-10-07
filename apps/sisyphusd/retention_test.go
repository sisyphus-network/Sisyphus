package main

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/ipfs/go-cid"

	"github.com/sisyphus-network/Sisyphus/apps/sisyphusd/blobclient"
	"github.com/sisyphus-network/Sisyphus/apps/sisyphusd/coordinator"
	pb "github.com/sisyphus-network/Sisyphus/packages/protocol/sisyphus/v1"
	"github.com/sisyphus-network/Sisyphus/packages/runtime"
	"github.com/sisyphus-network/Sisyphus/packages/storage"
)

// These tests check how long a coordinator keeps a job's data.

// jobPins returns the pins a job holds in the coordinator's store, by CID.
func (p *pool) jobPins(jobID string) map[string]storage.Pin {
	pins := make(map[string]storage.Pin)
	for _, pin := range p.store.Pins() {
		if pin.Owner == "job:"+jobID {
			pins[pin.CID.String()] = pin
		}
	}
	return pins
}

func (p *pool) holds(id string) bool {
	p.t.Helper()
	has, err := p.store.Has(p.ctx, cid.MustParse(id))
	if err != nil {
		p.t.Fatal(err)
	}
	return has
}

// afterGrace is a time by which the hour every new blob is kept has passed.
func afterGrace() time.Time { return time.Now().Add(storage.GracePeriod + time.Minute) }

func (p *pool) upload(text string) string {
	p.t.Helper()
	c, err := blobclient.Upload(p.ctx, p.blobs, strings.NewReader(text))
	if err != nil {
		p.t.Fatal(err)
	}
	return c.String()
}

func TestFinishedJobKeepsItsInputsAndResultsForTheRetentionPeriod(t *testing.T) {
	p := startPool(t, runtime.Builtin())
	p.startWorker("a", 2)
	p.startWorker("b", 2)
	p.waitForWorkers(2)
	input := p.upload(sampleText())

	job := p.wait(p.submit(&pb.JobSpec{Workload: "wordcount", Params: []byte(`{"input":"` + input + `"}`), MaxTasks: 4}).GetJobId())
	if job.GetState() != pb.JobState_JOB_STATE_SUCCEEDED {
		t.Fatalf("job %v: %s", job.GetState(), job.GetError())
	}
	var result runtime.WordCountResult
	if err := json.Unmarshal(job.GetResult(), &result); err != nil {
		t.Fatal(err)
	}

	if got := job.GetInputBlobs(); len(got) != 1 || got[0] != input {
		t.Errorf("job lists inputs %v, want only %s", got, input)
	}
	if got := job.GetOutputBlobs(); len(got) != 1 || got[0] != result.Output {
		t.Errorf("job lists outputs %v, want only %s", got, result.Output)
	}

	// The job pins exactly its input and its result, until the retention
	// period after it finished.
	pins := p.jobPins(job.GetJobId())
	wantExpiry := job.GetFinishedAt().AsTime().Add(testRetain)
	for _, kept := range []string{input, result.Output} {
		pin, ok := pins[kept]
		if !ok {
			t.Errorf("%s is not pinned by the job", kept)
		} else if !pin.Expires.Equal(wantExpiry) {
			t.Errorf("%s is pinned until %v, want %v", kept, pin.Expires, wantExpiry)
		}
	}
	if len(pins) != 2 {
		t.Errorf("the job holds %d pins, want 2: %v", len(pins), pins)
	}

	// Once the grace period is over, a collection removes the four tables of
	// counts the tasks passed to the aggregation, and nothing else.
	done, err := p.store.GC(p.ctx, afterGrace())
	if err != nil {
		t.Fatal(err)
	}
	if done.Blocks != 4 {
		t.Errorf("collection removed %d blocks, want the 4 intermediate tables", done.Blocks)
	}
	if !p.holds(input) || !p.holds(result.Output) {
		t.Fatal("a collection within the retention period removed the job's input or result")
	}

	// After the retention period nothing keeps them.
	if _, err := p.store.GC(p.ctx, wantExpiry.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if p.holds(input) || p.holds(result.Output) {
		t.Error("the job's input or result outlived the retention period")
	}
}

// staged is a workload whose first task finishes at once and whose second
// waits to be released, so a test can look at a job mid-flight. Each task
// stores a blob; the aggregation stores the result. With fail set, the
// second task fails instead of finishing.
type staged struct {
	waiting chan struct{}
	release chan struct{}
	fail    bool
}

func (staged) Name() string { return "staged" }

func (staged) Split(ctx context.Context, blobs runtime.Blobs, params []byte, _ int) ([][]byte, error) {
	blob, err := blobs.Open(ctx, cid.MustParse(string(params)))
	if err != nil {
		return nil, err
	}
	blob.Close()
	return [][]byte{[]byte("quick"), []byte("slow")}, nil
}

func (s staged) Execute(ctx context.Context, blobs runtime.Blobs, payload []byte) ([]byte, error) {
	stored, err := blobs.Put(ctx, strings.NewReader("output of the "+string(payload)+" task"))
	if err != nil {
		return nil, err
	}
	if string(payload) == "slow" {
		s.waiting <- struct{}{}
		select {
		case <-s.release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		if s.fail {
			return nil, errors.New("disk on fire")
		}
	}
	return []byte(stored.String()), nil
}

func (staged) Aggregate(ctx context.Context, blobs runtime.Blobs, _ [][]byte) ([]byte, error) {
	stored, err := blobs.Put(ctx, strings.NewReader("the result"))
	if err != nil {
		return nil, err
	}
	return []byte(stored.String()), nil
}

func startStagedJob(t *testing.T, w staged) (p *pool, input string, job *pb.Job) {
	t.Helper()
	p = startPool(t, runtime.NewRegistry(w))
	p.startWorker("a", 2)
	p.waitForWorkers(1)
	input = p.upload("the input")
	job = p.submit(&pb.JobSpec{Workload: "staged", Params: []byte(input)})
	<-w.waiting
	// Wait for the quick task's result to have been recorded.
	for {
		got, err := p.client.GetJob(p.ctx, &pb.GetJobRequest{JobId: job.GetJobId()})
		if err != nil {
			t.Fatal(err)
		}
		if got.GetJob().GetTasks()[0].GetState() == pb.TaskState_TASK_STATE_SUCCEEDED {
			return p, input, job
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func quickOutput(t *testing.T) string {
	t.Helper()
	c, err := storage.CID(context.Background(), strings.NewReader("output of the quick task"))
	if err != nil {
		t.Fatal(err)
	}
	return c.String()
}

// A job can run for longer than the hour a new blob is kept, so what it
// needs must be held for as long as it runs.
func TestRunningJobHoldsItsInputsAndTaskOutputsUntilItEnds(t *testing.T) {
	w := staged{waiting: make(chan struct{}, 4), release: make(chan struct{})}
	p, input, job := startStagedJob(t, w)
	quick := quickOutput(t)

	pins := p.jobPins(job.GetJobId())
	for name, held := range map[string]string{"input": input, "finished task's output": quick} {
		pin, ok := pins[held]
		if !ok || !pin.Expires.IsZero() {
			t.Errorf("the %s is not pinned until released while the job runs: %+v", name, pin)
		}
	}
	if _, err := p.store.GC(p.ctx, afterGrace()); err != nil {
		t.Fatal(err)
	}
	if !p.holds(input) || !p.holds(quick) {
		t.Fatal("a collection removed data a running job still needs")
	}

	close(w.release)
	done := p.wait(job.GetJobId())
	if done.GetState() != pb.JobState_JOB_STATE_SUCCEEDED {
		t.Fatalf("job %v: %s", done.GetState(), done.GetError())
	}
	pins = p.jobPins(job.GetJobId())
	if _, held := pins[quick]; held {
		t.Error("a task's output is still pinned after the job finished")
	}
	if pin := pins[input]; pin.Expires.IsZero() {
		t.Error("the input is still pinned until released after the job finished")
	}
	if pin, ok := pins[string(done.GetResult())]; !ok || pin.Expires.IsZero() {
		t.Errorf("the result is not pinned for the retention period: %+v", pin)
	}
}

func TestFailedJobKeepsItsInputsAndReleasesTheRest(t *testing.T) {
	w := staged{waiting: make(chan struct{}, 4), release: make(chan struct{}), fail: true}
	p, input, job := startStagedJob(t, w)
	close(w.release)
	done := p.wait(job.GetJobId())
	if done.GetState() != pb.JobState_JOB_STATE_FAILED {
		t.Fatalf("job is %v, want failed", done.GetState())
	}

	pins := p.jobPins(job.GetJobId())
	if pin, ok := pins[input]; !ok || !pin.Expires.Equal(done.GetFinishedAt().AsTime().Add(testRetain)) {
		t.Errorf("a failed job's input is pinned as %+v, want it kept for the retention period", pin)
	}
	if len(pins) != 1 {
		t.Errorf("a failed job holds %d pins, want only its input: %v", len(pins), pins)
	}
	if got := done.GetOutputBlobs(); len(got) != 0 {
		t.Errorf("a failed job lists outputs %v", got)
	}
}

func TestJobSucceedsEvenIfAWorkerNamesBlobsThatCannotBePinned(t *testing.T) {
	p := startPool(t, runtime.Builtin())
	w := p.connectRaw(hello("careless", 1, "primes"))
	w.welcome()
	absent, err := storage.CID(p.ctx, strings.NewReader("never uploaded"))
	if err != nil {
		t.Fatal(err)
	}
	job := p.submit(primesJob(pb.ScheduleMode_SCHEDULE_MODE_DISTRIBUTED, 1))
	task := w.assignment()
	w.reply(&pb.TaskResult{
		TaskId: task.GetTaskId(), Attempt: task.GetAttempt(),
		Outcome:      &pb.TaskResult_Output{Output: []byte(`{"count":5}`)},
		WrittenBlobs: []string{"not a CID at all", absent.String()},
	})

	if done := p.wait(job.GetJobId()); string(done.GetResult()) != `{"count":5}` {
		t.Errorf("result %s (%s)", done.GetResult(), done.GetError())
	}
	if !strings.Contains(p.logs.String(), "could not pin a job's blobs") {
		t.Errorf("the failure to pin was not logged:\n%s", p.logs.String())
	}
	if len(p.jobPins(job.GetJobId())) != 0 {
		t.Errorf("the job holds pins: %v", p.jobPins(job.GetJobId()))
	}
}

// unpinFails is a store that cannot release pins.
type unpinFails struct{ *storage.Store }

func (unpinFails) Unpin(string, ...cid.Cid) error { return errors.New("pin file is read-only") }

func TestJobSucceedsEvenIfItsIntermediateBlobsCannotBeReleased(t *testing.T) {
	p := startPoolOver(t, runtime.Builtin(), func(store *storage.Store) coordinator.Store { return unpinFails{store} })
	p.startWorker("a", 2)
	p.waitForWorkers(1)
	input := p.upload("roll the boulder up the hill")

	job := p.wait(p.submit(&pb.JobSpec{Workload: "wordcount", Params: []byte(`{"input":"` + input + `"}`), MaxTasks: 2}).GetJobId())
	if job.GetState() != pb.JobState_JOB_STATE_SUCCEEDED {
		t.Fatalf("job %v: %s", job.GetState(), job.GetError())
	}
	if !strings.Contains(p.logs.String(), "could not release a job's intermediate blobs") {
		t.Errorf("the failure to unpin was not logged:\n%s", p.logs.String())
	}
}

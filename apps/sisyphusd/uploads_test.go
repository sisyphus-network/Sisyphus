package main

import (
	"slices"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/sisyphus-network/Sisyphus/apps/sisyphusd/access"
	"github.com/sisyphus-network/Sisyphus/apps/sisyphusd/blobclient"
	pb "github.com/sisyphus-network/Sisyphus/packages/protocol/sisyphus/v1"
	"github.com/sisyphus-network/Sisyphus/packages/runtime"
)

// uploadAs uploads text to the pool's coordinator as the worker w, for the
// attempt a if it is not nil.
func (p *pool) uploadAs(w *rawWorker, text string, a *pb.TaskAssignment) (string, error) {
	p.t.Helper()
	conn, err := grpc.NewClient(p.addr, grpc.WithTransportCredentials(p.credentialsFor(p.rawIdents[w])))
	if err != nil {
		p.t.Fatal(err)
	}
	defer conn.Close()
	ctx := p.ctx
	if a != nil {
		ctx = blobclient.ForAttempt(ctx, a.GetTaskId(), a.GetAttempt())
	}
	c, err := blobclient.Upload(ctx, pb.NewBlobServiceClient(conn), strings.NewReader(text))
	return c.String(), err
}

// heldFor lists the owners the coordinator's store holds a blob for.
func (p *pool) heldFor(blob string) []string {
	var owners []string
	for _, pin := range p.store.Pins() {
		if pin.CID.String() == blob {
			owners = append(owners, pin.Owner)
		}
	}
	return owners
}

// A worker uploads only for an attempt at a task that it holds, as the
// coordinator knows it, and what it uploads is kept for that attempt. When
// the attempt ends, what its result names is kept for the job, and the
// rest is let go of.
func TestAWorkerUploadsOnlyForAnAttemptItHolds(t *testing.T) {
	p := startPool(t, runtime.Builtin())
	w := p.connectRaw(hello("rig", 1, "primes"))
	w.welcome()

	if _, err := p.uploadAs(w, "for no task", nil); status.Code(err) != codes.FailedPrecondition || !strings.Contains(err.Error(), "names none") {
		t.Fatalf("an upload naming no attempt: %v", err)
	}
	id := p.submit(primesJob(pb.ScheduleMode_SCHEDULE_MODE_FULL_WORKER, 0)).GetJobId()
	a := w.assignment()
	// Another worker, connected only once the task is rig's.
	other := p.connectRaw(hello("other", 1, "primes"))
	other.welcome()
	if _, err := p.uploadAs(other, "for another's task", a); status.Code(err) != codes.FailedPrecondition || !strings.Contains(err.Error(), "holds no attempt 1") {
		t.Fatalf("an upload for another worker's attempt: %v", err)
	}
	wrong := &pb.TaskAssignment{TaskId: a.GetTaskId(), Attempt: a.GetAttempt() + 1}
	if _, err := p.uploadAs(w, "for an attempt not given", wrong); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("an upload for an attempt not handed out: %v", err)
	}

	named, err := p.uploadAs(w, "the task's output", a)
	if err != nil {
		t.Fatal(err)
	}
	unnamed, err := p.uploadAs(w, "something the task left aside", a)
	if err != nil {
		t.Fatal(err)
	}
	attempt := "job:" + id + "#0@1."
	if held := p.heldFor(unnamed); !slices.Contains(held, attempt) {
		t.Fatalf("an upload for an attempt is held for %v", held)
	}
	w.returns(a, `{"count":5}`, named)
	if done := p.wait(id); done.GetState() != pb.JobState_JOB_STATE_SUCCEEDED {
		t.Fatalf("the job: %v %s", done.GetState(), done.GetError())
	}
	for _, blob := range []string{named, unnamed} {
		if held := p.heldFor(blob); slices.Contains(held, attempt) {
			t.Errorf("still held for the attempt once it ended: %s %v", blob, held)
		}
	}
	// Nor is what the result did not name kept for a grace period, as an
	// upload of the node's own would be: it goes at the next collection.
	if held := p.heldFor(unnamed); len(held) != 0 {
		t.Errorf("what the result did not name is still held for %v", held)
	}
	if _, err := p.store.GC(p.ctx, time.Now()); err != nil {
		t.Fatal(err)
	}
	if has, _ := p.store.Has(p.ctx, mustCID(t, unnamed)); has {
		t.Error("what the result did not name was not collected")
	}

	// A worker that is admitted but not connected holds nothing.
	_, away := p.admit(access.Worker)
	conn, err := grpc.NewClient(p.addr, grpc.WithTransportCredentials(away))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := blobclient.Upload(blobclient.ForAttempt(p.ctx, a.GetTaskId(), a.GetAttempt()), pb.NewBlobServiceClient(conn), strings.NewReader("from afar")); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("an upload from a worker that is not connected: %v", err)
	}
}

// What an attempt uploaded is let go of however the attempt ends.
func TestWhatAnAttemptUploadedIsLetGoOfWhenItFails(t *testing.T) {
	p := startPool(t, runtime.Builtin())
	w := p.connectRaw(hello("rig", 1, "primes"))
	w.welcome()
	id := p.submit(primesJob(pb.ScheduleMode_SCHEDULE_MODE_FULL_WORKER, 0)).GetJobId()
	a := w.assignment()
	uploaded, err := p.uploadAs(w, "half of something", a)
	if err != nil {
		t.Fatal(err)
	}
	w.fails(a, "out of memory")
	again := w.assignment()
	if held := p.heldFor(uploaded); slices.Contains(held, "job:"+id+"#0@1.") {
		t.Errorf("held for the failed attempt: %v", held)
	}
	w.returns(again, `{"count":5}`)
	p.wait(id)
}

// What an attempt uploaded is let go of when the coordinator stops too: the
// attempt goes back to be handed out again, and no result of it is taken in.
func TestWhatAnAttemptUploadedIsLetGoOfWhenTheCoordinatorStops(t *testing.T) {
	p := startPool(t, runtime.Builtin())
	w := p.connectRaw(hello("rig", 1, "primes"))
	w.welcome()
	id := p.submit(primesJob(pb.ScheduleMode_SCHEDULE_MODE_FULL_WORKER, 0)).GetJobId()
	a := w.assignment()
	uploaded, err := p.uploadAs(w, "left half done", a)
	if err != nil {
		t.Fatal(err)
	}
	if held := p.heldFor(uploaded); !slices.Contains(held, "job:"+id+"#0@1.") {
		t.Fatalf("held for %v before the stop", held)
	}
	p.stopAndForget()
	if held := p.heldFor(uploaded); slices.Contains(held, "job:"+id+"#0@1.") {
		t.Errorf("still held for the attempt after the coordinator stopped: %v", held)
	}
}

// A worker may upload only so much for one attempt, counted across all its
// uploads as they arrive; the next attempt has an allowance of its own.
func TestAWorkerUploadsOnlySoMuchForOneAttempt(t *testing.T) {
	attemptAllowance = 1000
	defer func() { attemptAllowance = 0 }()
	p := startPool(t, runtime.Builtin())
	w := p.connectRaw(hello("rig", 1, "primes"))
	w.welcome()
	id := p.submit(primesJob(pb.ScheduleMode_SCHEDULE_MODE_FULL_WORKER, 0)).GetJobId()
	a := w.assignment()
	if _, err := p.uploadAs(w, strings.Repeat("a", 600), a); err != nil {
		t.Fatalf("an upload within the allowance: %v", err)
	}
	if _, err := p.uploadAs(w, strings.Repeat("b", 500), a); status.Code(err) != codes.ResourceExhausted || !strings.Contains(err.Error(), "has uploaded 600 bytes, and an attempt may upload 1000") {
		t.Fatalf("an upload past the allowance: %v", err)
	}
	if _, err := p.uploadAs(w, strings.Repeat("c", 300), a); err != nil {
		t.Fatalf("an upload that still fits: %v", err)
	}
	w.fails(a, "ran out of room")
	again := w.assignment()
	if _, err := p.uploadAs(w, strings.Repeat("d", 900), again); err != nil {
		t.Fatalf("the next attempt's allowance: %v", err)
	}
	w.returns(again, `{"count":5}`)
	p.wait(id)
}

// An upload begun while its attempt lasted may not go on once the attempt
// has ended: the next piece is refused, rather than counted against an
// allowance begun afresh.
func TestAnUploadMayNotGoOnOnceItsAttemptHasEnded(t *testing.T) {
	attemptAllowance = 1000
	defer func() { attemptAllowance = 0 }()
	p := startPool(t, runtime.Builtin())
	w := p.connectRaw(hello("rig", 1, "primes"))
	w.welcome()
	id := p.submit(primesJob(pb.ScheduleMode_SCHEDULE_MODE_FULL_WORKER, 0)).GetJobId()
	a := w.assignment()
	owner, ok := p.coord.Uploading(p.rawIdents[w].ID(), a.GetTaskId(), a.GetAttempt())
	if !ok {
		t.Fatal("the worker holds no attempt")
	}
	conn, err := grpc.NewClient(p.addr, grpc.WithTransportCredentials(p.credentialsFor(p.rawIdents[w])))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	stream, err := pb.NewBlobServiceClient(conn).Put(p.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := stream.Send(&pb.PutBlobRequest{TaskId: a.GetTaskId(), Attempt: a.GetAttempt(), Data: []byte(strings.Repeat("a", 600))}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { used, _ := p.coord.Uploaded(owner); return used == 600 })
	w.fails(a, "gave up")
	waitFor(t, func() bool { _, counted := p.coord.Uploaded(owner); return !counted })
	if err := stream.Send(&pb.PutBlobRequest{Data: []byte(strings.Repeat("b", 600))}); err != nil {
		t.Fatal(err)
	}
	if _, err := stream.CloseAndRecv(); status.Code(err) != codes.FailedPrecondition || !strings.Contains(err.Error(), "the attempt this upload is for has ended") {
		t.Fatalf("a piece sent after the attempt ended: %v", err)
	}
	if _, counted := p.coord.Uploaded(owner); counted {
		t.Error("an attempt that had ended is counted again")
	}
	w.returns(w.assignment(), `{"count":5}`)
	p.wait(id)
}

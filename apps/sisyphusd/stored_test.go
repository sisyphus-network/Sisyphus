package main

import (
	"context"
	"errors"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/sisyphus-network/Sisyphus/apps/sisyphusd/blobclient"
	"github.com/sisyphus-network/Sisyphus/apps/sisyphusd/coordinator"
	pb "github.com/sisyphus-network/Sisyphus/packages/protocol/sisyphus/v1"
	"github.com/sisyphus-network/Sisyphus/packages/runtime"
	"github.com/sisyphus-network/Sisyphus/packages/storage"
)

// storedBy is what each member keeps in the coordinator's store, as the
// pool's owner is told.
func (p *pool) storedBy() map[string]uint64 {
	p.t.Helper()
	listed, err := pb.NewPoolServiceClient(p.conn).ListMembers(p.ctx, &pb.ListMembersRequest{})
	if err != nil {
		p.t.Fatal(err)
	}
	stored := make(map[string]uint64)
	for _, m := range listed.GetMembers() {
		stored[m.GetNodeId()] = m.GetStoredBytes()
	}
	return stored
}

// The pool's owner can see what each client keeps: what it pinned, and
// what its jobs keep, a blob kept both ways counting once.
func TestTheOwnerSeesWhatEachClientKeeps(t *testing.T) {
	p := startPool(t, runtime.Builtin())
	p.startWorker("rig", 2)
	p.waitForWorkers(1)
	alice, bob := p.newClient(), p.newClient()

	text := sampleText()
	input, err := blobclient.Upload(p.ctx, alice.blobs, strings.NewReader(text))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := alice.blobs.Pin(p.ctx, &pb.PinBlobRequest{Cid: input.String()}); err != nil {
		t.Fatal(err)
	}
	pinned := p.storedBy()
	if pinned[alice.id] < uint64(len(text)) || pinned[bob.id] != 0 {
		t.Fatalf("with alice's input pinned, alice keeps %d bytes and bob %d", pinned[alice.id], pinned[bob.id])
	}

	// Her job keeps the input again, and what it produced.
	submitted, err := alice.jobs.SubmitJob(p.ctx, &pb.SubmitJobRequest{Spec: &pb.JobSpec{Workload: "wordcount", Params: []byte(`{"input":"` + input.String() + `"}`), MaxTasks: 2}})
	if err != nil {
		t.Fatal(err)
	}
	if job := p.wait(submitted.GetJob().GetJobId()); job.GetState() != pb.JobState_JOB_STATE_SUCCEEDED {
		t.Fatalf("job %v: %s", job.GetState(), job.GetError())
	}
	after := p.storedBy()
	if after[alice.id] <= pinned[alice.id] || after[alice.id] >= 2*pinned[alice.id] || after[bob.id] != 0 {
		t.Errorf("with alice's job done, alice keeps %d bytes (%d before) and bob %d", after[alice.id], pinned[alice.id], after[bob.id])
	}

}

// The table of members says what each client keeps; a worker keeps
// nothing there of its own.
func TestTheTableOfMembersSaysWhatEachClientKeeps(t *testing.T) {
	for _, tt := range []struct {
		member *pb.Member
		want   string
	}{
		{&pb.Member{Role: pb.Role_ROLE_CLIENT, StoredBytes: 3 << 20}, "3.0 MiB"},
		{&pb.Member{Role: pb.Role_ROLE_CLIENT}, "0 B"},
		{&pb.Member{Role: pb.Role_ROLE_WORKER}, "-"},
	} {
		if got := storedOf(tt.member); got != tt.want {
			t.Errorf("%v: %q, want %q", tt.member, got, tt.want)
		}
	}
}

type groupingFails struct{ *storage.Store }

func (g groupingFails) GroupPins(ctx context.Context, group func(string) string) error {
	g.Store.GroupPins(ctx, group)
	return errors.New("a pinned blob is missing")
}

// Pins that cannot be counted are left out, and said so in the log; the
// members are listed all the same, with what can be counted.
func TestMembersAreListedWhenSomePinsCannotBeCounted(t *testing.T) {
	p := startPoolOver(t, runtime.Builtin(), func(store *storage.Store) coordinator.Store { return groupingFails{store} })
	p.newClient()
	members, err := pb.NewPoolServiceClient(p.conn).ListMembers(p.ctx, &pb.ListMembersRequest{})
	if err != nil || len(members.GetMembers()) == 0 {
		t.Errorf("members %v, %v", members, err)
	}
}

// A client may keep no more than its share: what it uploads counts
// towards it at once, and an upload or a pin that would take it past its
// share is refused. Each client has a share of its own, and the node's own
// uploads are not held to one.
func TestAClientKeepsNoMoreThanItsShare(t *testing.T) {
	clientShare = 1000
	defer func() { clientShare = 0 }()
	p := startPool(t, runtime.Builtin())
	alice, bob := p.newClient(), p.newClient()
	// Each client uploads its own letter, so that alice's and bob's blobs
	// of one size are never the same blob. (Taking a letter from their IDs
	// made them the same whenever the IDs happened to end alike.)
	fill := map[*guest]string{alice: "a", bob: "b"}
	upload := func(g *guest, size int) (string, error) {
		c, err := blobclient.Upload(p.ctx, g.blobs, strings.NewReader(strings.Repeat(fill[g], size)))
		return c.String(), err
	}

	first, err := upload(alice, 600)
	if err != nil {
		t.Fatal(err)
	}
	if got := p.storedBy()[alice.id]; got != 600 {
		t.Fatalf("an upload not yet pinned has alice keep %d bytes", got)
	}
	if _, err := upload(alice, 500); status.Code(err) != codes.ResourceExhausted || !strings.Contains(err.Error(), "400 bytes left of this client's share") {
		t.Fatalf("an upload past alice's share: %v", err)
	}
	if _, err := upload(alice, 400); err != nil {
		t.Fatalf("an upload to alice's share exactly: %v", err)
	}
	if _, err := upload(alice, 1); status.Code(err) != codes.ResourceExhausted || !strings.Contains(err.Error(), "keeps 1000 bytes here") {
		t.Fatalf("an upload with alice's share used: %v", err)
	}

	// Pinning what she sent keeps nothing more; pinning another's blob
	// would, and is refused, and leaves no pin behind.
	if _, err := alice.blobs.Pin(p.ctx, &pb.PinBlobRequest{Cid: first}); err != nil {
		t.Fatalf("alice pinning her own upload: %v", err)
	}
	theirs, err := upload(bob, 600)
	if err != nil {
		t.Fatalf("bob's share is his own: %v", err)
	}
	if _, err := alice.blobs.Pin(p.ctx, &pb.PinBlobRequest{Cid: theirs}); status.Code(err) != codes.ResourceExhausted || !strings.Contains(err.Error(), "would have this client keep 1600 bytes") {
		t.Fatalf("alice pinning bob's blob past her share: %v", err)
	}
	pins, err := alice.blobs.ListPins(p.ctx, &pb.ListPinsRequest{})
	if err != nil {
		t.Fatal(err)
	}
	for _, pin := range pins.GetPins() {
		if pin.GetCid() == theirs {
			t.Errorf("a pin that was refused was left in place")
		}
	}

	// The node's own uploads are not held to a client's share.
	if _, err := blobclient.Upload(p.ctx, p.blobs, strings.NewReader(strings.Repeat("n", 5000))); err != nil {
		t.Errorf("the node's own upload: %v", err)
	}
}

// The store keeps a little more than was sent, for the links between the
// pieces of a large file, and a client's share is held to what the store
// keeps: an upload that fits its share as sent but not as stored is
// refused, and leaves nothing counted against the client.
func TestAClientsShareCountsWhatIsStored(t *testing.T) {
	const size = 300 << 10 // more than one 256 KiB piece
	clientShare = size
	defer func() { clientShare = 0 }()
	p := startPool(t, runtime.Builtin())
	alice := p.newClient()

	_, err := blobclient.Upload(p.ctx, alice.blobs, strings.NewReader(strings.Repeat("a", size)))
	if status.Code(err) != codes.ResourceExhausted || !strings.Contains(err.Error(), "uploading") || !strings.Contains(err.Error(), "may keep 307200") {
		t.Fatalf("an upload that fits as sent but not as stored: %v", err)
	}
	if got := p.storedBy()[alice.id]; got != 0 {
		t.Errorf("a refused upload has alice keep %d bytes", got)
	}
}

// What a client's job stores counts towards its share too. A task whose
// outputs would take the client past it fails, and its job with it, at
// once: the same task would store the same again, so it is not tried
// again, and none of what it stored is kept for the client.
func TestAClientsJobStoresNoMoreThanItsShare(t *testing.T) {
	clientShare = 1000
	defer func() { clientShare = 0 }()
	p := startPool(t, runtime.Builtin())
	w := p.connectRaw(hello("rig", 1, "primes"))
	w.welcome()
	alice := p.newClient()
	run := func(stored ...string) *pb.Job {
		t.Helper()
		submitted, err := alice.jobs.SubmitJob(p.ctx, &pb.SubmitJobRequest{Spec: primesJob(pb.ScheduleMode_SCHEDULE_MODE_FULL_WORKER, 0)})
		if err != nil {
			t.Fatal(err)
		}
		task := w.assignment()
		w.reply(&pb.TaskResult{TaskId: task.GetTaskId(), Attempt: task.GetAttempt(), Outcome: &pb.TaskResult_Output{Output: []byte(`{"count":5}`)}, WrittenBlobs: stored})
		return p.wait(submitted.GetJob().GetJobId())
	}

	if job := run(p.upload(strings.Repeat("s", 600))); job.GetState() != pb.JobState_JOB_STATE_SUCCEEDED {
		t.Fatalf("a job storing what fits alice's share: %v %s", job.GetState(), job.GetError())
	}
	large := p.upload(strings.Repeat("l", 1500))
	// Named beside what is no blob, it is refused all the same.
	if job := run("not a CID at all", large); job.GetState() != pb.JobState_JOB_STATE_FAILED || !strings.Contains(job.GetError(), "may keep 1000") {
		t.Fatalf("a job storing more than alice's share, beside what is no blob: %v %s", job.GetState(), job.GetError())
	}
	job := run(large)
	if job.GetState() != pb.JobState_JOB_STATE_FAILED || job.GetError() != "task 0 failed: its outputs would have this client keep 1500 bytes here, and it may keep 1000; free some and submit the job again" {
		t.Fatalf("a job storing more than alice's share: %v %s", job.GetState(), job.GetError())
	}
	if pins := p.jobPins(job.GetJobId()); len(pins) != 0 {
		t.Errorf("the job that was refused keeps %v", pins)
	}
	if got := p.storedBy()[alice.id]; got != 0 {
		t.Errorf("alice keeps %d bytes", got)
	}
	failed := 0
	for _, e := range p.events(job.GetJobId(), 0) {
		if e.GetKind() == "task-failed" {
			failed++
		}
	}
	if failed != 1 {
		t.Errorf("the task failed %d times", failed)
	}
}

// staged lists what is staged for a job's tasks: what their results stored,
// kept until each task is settled.
func (p *pool) staged(jobID string) []string {
	return p.stagedIn(p.store, jobID)
}

// What the copies of a verified task store counts towards its client's
// share only once the task is settled, and then only what the copies it was
// settled by stored: a copy that is outvoted costs the client nothing. What
// they stored is held to the share as a task's on one worker's word is.
func TestAVerifiedTaskIsHeldToItsClientsShareOnceSettled(t *testing.T) {
	clientShare = 1000
	defer func() { clientShare = 0 }()
	p := startPool(t, runtime.Builtin())
	crew := p.crew("alpha", "beta", "gamma")
	alice := p.newClient()
	small, large := p.upload(strings.Repeat("s", 600)), p.upload(strings.Repeat("l", 1500))
	submit := func() string {
		t.Helper()
		submitted, err := alice.jobs.SubmitJob(p.ctx, &pb.SubmitJobRequest{Spec: verifiedPrimes(1, 2)})
		if err != nil {
			t.Fatal(err)
		}
		return submitted.GetJob().GetJobId()
	}

	id := submit()
	first, second := crew[0].assignment(), crew[1].assignment()
	crew[0].returns(first, `{"count":7}`, large)
	crew[1].returns(second, `{"count":7}`, small)
	crew[2].returns(crew[2].assignment(), `{"count":7}`, small)
	if job := p.wait(id); job.GetState() != pb.JobState_JOB_STATE_SUCCEEDED {
		t.Fatalf("a job settled on what fits alice's share, outvoting more: %v %s", job.GetState(), job.GetError())
	}
	if staged := p.staged(id); len(staged) != 0 {
		t.Errorf("still staged: %v", staged)
	}

	id = submit()
	first, second = crew[0].assignment(), crew[1].assignment()
	crew[0].returns(first, `{"count":7}`, large)
	crew[1].returns(second, `{"count":7}`, large)
	job := p.wait(id)
	if job.GetState() != pb.JobState_JOB_STATE_FAILED || job.GetError() != "task 0 failed: its outputs would have this client keep 1500 bytes here, and it may keep 1000; free some and submit the job again" {
		t.Fatalf("a job settled on more than alice's share: %v %s", job.GetState(), job.GetError())
	}
	if staged, pins := p.staged(id), p.jobPins(id); len(staged) != 0 || len(pins) != 0 {
		t.Errorf("still staged: %v; the job's pins: %v", staged, pins)
	}
	if got := p.storedBy()[alice.id]; got != 0 {
		t.Errorf("alice keeps %d bytes", got)
	}
}

// What a worker's task stored while it is audited is staged too, and
// released when the audit settles the task: here with the coordinator's
// own result, which stored nothing.
func TestAnAuditedTaskStagesWhatItsWorkerStored(t *testing.T) {
	clientShare = 1000
	defer func() { clientShare = 0 }()
	p := startPool(t, runtime.Builtin())
	w := p.connectRaw(hello("only", 1, "primes"))
	w.welcome()
	alice := p.newClient()
	submitted, err := alice.jobs.SubmitJob(p.ctx, &pb.SubmitJobRequest{Spec: &pb.JobSpec{Workload: "primes", Params: []byte(hundred), MaxTasks: 1, AuditShare: 1}})
	if err != nil {
		t.Fatal(err)
	}
	id := submitted.GetJob().GetJobId()
	w.returns(w.assignment(), primesBelow100, p.upload(strings.Repeat("l", 1500)))
	if job := p.wait(id); job.GetState() != pb.JobState_JOB_STATE_SUCCEEDED || string(job.GetResult()) != primesBelow100 {
		t.Fatalf("the job: %v %s", job.GetState(), job.GetError())
	}
	if staged := p.staged(id); len(staged) != 0 {
		t.Errorf("still staged: %v", staged)
	}
	if got := p.storedBy()[alice.id]; got != 0 {
		t.Errorf("alice keeps %d bytes", got)
	}
}

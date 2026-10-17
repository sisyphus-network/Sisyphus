package main

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

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

type usageFails struct{ *storage.Store }

func (usageFails) Usage(context.Context, time.Time, func(string) string) (map[string]uint64, error) {
	return nil, errors.New("a pinned blob is missing")
}

// A store that cannot say what it keeps is said to have failed, rather
// than every client keeping nothing.
func TestMembersAreNotListedWithUsageThatCannotBeMeasured(t *testing.T) {
	p := startPoolOver(t, runtime.Builtin(), func(store *storage.Store) coordinator.Store { return usageFails{store} })
	_, err := pb.NewPoolServiceClient(p.conn).ListMembers(p.ctx, &pb.ListMembersRequest{})
	if status.Code(err) != codes.Internal || !strings.Contains(err.Error(), "a pinned blob is missing") {
		t.Errorf("members with usage that cannot be measured: %v", err)
	}
}

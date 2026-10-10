package main

import (
	"slices"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/sisyphus-network/Sisyphus/apps/sisyphusd/access"
	pb "github.com/sisyphus-network/Sisyphus/packages/protocol/sisyphus/v1"
	"github.com/sisyphus-network/Sisyphus/packages/runtime"
)

// guest is a node admitted to a pool as a client, and its connection.
type guest struct {
	id    string
	jobs  pb.NodeServiceClient
	blobs pb.BlobServiceClient
}

// client admits a new node to the pool as a client and connects as it.
func (p *pool) newClient() *guest {
	p.t.Helper()
	ident, creds := p.admit(access.Client)
	conn, err := grpc.NewClient(p.addr, grpc.WithTransportCredentials(creds))
	if err != nil {
		p.t.Fatal(err)
	}
	p.t.Cleanup(func() { conn.Close() })
	return &guest{id: ident.ID(), jobs: pb.NewNodeServiceClient(conn), blobs: pb.NewBlobServiceClient(conn)}
}

// A pool's clients are several, and each sees and stops the jobs it
// submitted. Another's job is, to a client, a job that is not there.
func TestAClientSeesAndStopsOnlyItsOwnJobs(t *testing.T) {
	g := gated{started: make(chan struct{}, 16), release: make(chan struct{})}
	p := startPool(t, runtime.NewRegistry(g, runtime.Primes{}))
	p.startWorker("rig", 1)
	p.waitForWorkers(1)
	ours, theirs := p.newClient(), p.newClient()

	submitted, err := ours.jobs.SubmitJob(p.ctx, &pb.SubmitJobRequest{Spec: &pb.JobSpec{Workload: "gated", MaxTasks: 1}})
	if err != nil {
		t.Fatal(err)
	}
	id := submitted.GetJob().GetJobId()
	<-g.started
	if submitted.GetJob().GetSubmitterId() != ours.id {
		t.Fatalf("the job is said to be submitted by %q", submitted.GetJob().GetSubmitterId())
	}

	// Each way a job is looked at or stopped, tried by the other client.
	notThere := func(what string, err error) {
		t.Helper()
		if status.Code(err) != codes.NotFound || status.Convert(err).Message() != `job "`+id+`" not found` {
			t.Errorf("another client's %s: %v, want what is said of a job that is not there", what, err)
		}
	}
	_, err = theirs.jobs.GetJob(p.ctx, &pb.GetJobRequest{JobId: id})
	notThere("GetJob", err)
	_, err = theirs.jobs.CancelJob(p.ctx, &pb.CancelJobRequest{JobId: id})
	notThere("CancelJob", err)
	_, err = theirs.jobs.GetJobRecord(p.ctx, &pb.GetJobRecordRequest{JobId: id})
	notThere("GetJobRecord", err)
	watching, err := theirs.jobs.WatchJob(p.ctx, &pb.WatchJobRequest{JobId: id})
	if err != nil {
		t.Fatal(err)
	}
	_, err = watching.Recv()
	notThere("WatchJob", err)
	following, err := theirs.jobs.WatchJobEvents(p.ctx, &pb.WatchJobEventsRequest{JobId: id})
	if err != nil {
		t.Fatal(err)
	}
	_, err = following.Recv()
	notThere("WatchJobEvents", err)
	// A job that really is not there is said of in the same words.
	if _, err := theirs.jobs.GetJob(p.ctx, &pb.GetJobRequest{JobId: "no-such-job"}); status.Code(err) != codes.NotFound {
		t.Errorf("a job nobody submitted: %v", err)
	}

	// The job ran on untouched: its own client sees it, as does the node.
	if got, err := ours.jobs.GetJob(p.ctx, &pb.GetJobRequest{JobId: id}); err != nil || got.GetJob().GetState() != pb.JobState_JOB_STATE_RUNNING {
		t.Fatalf("the job as its own client sees it: %v, %v", got.GetJob().GetState(), err)
	}
	if got := p.job(id); got.GetState() != pb.JobState_JOB_STATE_RUNNING {
		t.Errorf("the job as the node sees it: %v", got.GetState())
	}
	events, err := ours.jobs.WatchJobEvents(p.ctx, &pb.WatchJobEventsRequest{JobId: id})
	if err != nil {
		t.Fatal(err)
	}
	if first, err := events.Recv(); err != nil || first.GetKind() != "submitted" {
		t.Errorf("the job's events as its own client reads them: %v, %v", first, err)
	}
	if stopped, err := ours.jobs.CancelJob(p.ctx, &pb.CancelJobRequest{JobId: id}); err != nil || stopped.GetJob().GetState() != pb.JobState_JOB_STATE_CANCELLED {
		t.Errorf("the job stopped by its own client: %v, %v", stopped.GetJob().GetState(), err)
	}
	if _, err := ours.jobs.GetJobRecord(p.ctx, &pb.GetJobRecordRequest{JobId: id}); err != nil {
		t.Errorf("the job's record as its own client reads it: %v", err)
	}
}

// What a client pins is kept for that client: it lists and releases its
// own pins and nobody else's. The node itself sees and releases them all.
func TestAClientsPinsAreItsOwn(t *testing.T) {
	p := startPool(t, runtime.Builtin())
	ours, theirs := p.newClient(), p.newClient()
	blob := p.upload("what two clients both want kept")

	owners := func(as pb.BlobServiceClient) []string {
		t.Helper()
		listed, err := as.ListPins(p.ctx, &pb.ListPinsRequest{})
		if err != nil {
			t.Fatal(err)
		}
		var held []string
		for _, pin := range listed.GetPins() {
			if pin.GetCid() == blob && pin.GetOwner() != "recent" {
				held = append(held, pin.GetOwner())
			}
		}
		slices.Sort(held)
		return held
	}
	for _, as := range []pb.BlobServiceClient{ours.blobs, theirs.blobs, p.blobs} {
		if _, err := as.Pin(p.ctx, &pb.PinBlobRequest{Cid: blob}); err != nil {
			t.Fatal(err)
		}
	}
	if got := owners(ours.blobs); !slices.Equal(got, []string{"user:" + ours.id}) {
		t.Errorf("a client is shown the pins of %v", got)
	}
	want := []string{"user", "user:" + ours.id, "user:" + theirs.id}
	slices.Sort(want)
	if got := owners(p.blobs); !slices.Equal(got, want) {
		t.Errorf("the node is shown the pins of %v, want %v", got, want)
	}

	// One client's release leaves the other's pin, and the user's.
	if _, err := theirs.blobs.Unpin(p.ctx, &pb.UnpinBlobRequest{Cid: blob}); err != nil {
		t.Fatal(err)
	}
	if got := owners(p.blobs); !slices.Equal(got, []string{"user", "user:" + ours.id}) {
		t.Errorf("after one client let go the blob is pinned for %v", got)
	}
	// The node's own release lets go for everyone: the store is its own.
	if _, err := p.blobs.Unpin(p.ctx, &pb.UnpinBlobRequest{Cid: blob}); err != nil {
		t.Fatal(err)
	}
	if got := owners(p.blobs); len(got) != 0 {
		t.Errorf("after the node let go the blob is still pinned for %v", got)
	}
}

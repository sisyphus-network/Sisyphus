package main

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/sisyphus-network/Sisyphus/apps/sisyphusd/worker"
	nodepb "github.com/sisyphus-network/Sisyphus/packages/protocol/sisyphus/node/v1"
	pb "github.com/sisyphus-network/Sisyphus/packages/protocol/sisyphus/v1"
)

// A node that has joined another's pool, and holds the client role there,
// submits jobs to that pool through its own local API, lists and follows
// them, and sees the pool's workers. Without the client role it is refused,
// and while it is away from the pool it is told the pool cannot be reached
// (#474).
func TestAJoinedClientNodeSubmitsToItsPoolThroughItsOwnAPI(t *testing.T) {
	addr, stopCoordinator := startNode(t, "--name", "rig")
	workerDir, apiAddr := t.TempDir(), freeAddr(t)
	startDaemon(t, "--role", "worker", "--coordinator", addr, "--join", invite(t, addr, "worker"), "--data-dir", workerDir, "--name", "hand", "--slots", "1", "--api-listen", apiAddr)
	client := desktop(t, apiAddr)
	poolAsSeenBy(t, client)
	token, err := os.ReadFile(filepath.Join(workerDir, "api.token"))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	ctx = metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+strings.TrimSpace(string(token)))
	primes := &nodepb.SubmitJobRequest{Workload: "primes", Params: []byte(`{"from":0,"to":1000}`), Mode: nodepb.JobMode_JOB_MODE_DISTRIBUTED}

	// As a worker alone, it is told it is not the pool's client.
	waitFor(t, func() bool {
		pool, err := client.GetJoinedPool(ctx, &nodepb.GetJoinedPoolRequest{})
		return err == nil && pool.GetState() == nodepb.JoinedPoolState_JOINED_POOL_STATE_CONNECTED
	})
	if _, err := client.SubmitJob(ctx, primes); status.Code(err) != codes.FailedPrecondition || !strings.Contains(err.Error(), "not a client") {
		t.Fatalf("a worker alone submitting through its own API: %v, want FailedPrecondition", err)
	}

	// Given a client invitation, it may.
	if _, err := as(t, workerDir, "pool", "join", "--addr", addr, invite(t, addr, "client")); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool {
		pool, err := client.GetJoinedPool(ctx, &nodepb.GetJoinedPoolRequest{})
		return err == nil && slices.Contains(pool.GetRoles(), nodepb.PoolRole_POOL_ROLE_CLIENT)
	})
	watch, err := client.WatchJobs(ctx, &nodepb.WatchJobsRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if first, err := watch.Recv(); err != nil || len(first.GetJobs()) != 0 {
		t.Fatalf("the jobs before any was submitted: %v, %v", first, err)
	}
	submitted, err := client.SubmitJob(ctx, primes)
	if err != nil {
		t.Fatal(err)
	}
	id := submitted.GetJob().GetJobId()

	// The watch is told as the job goes on, until it has succeeded on the
	// pool's workers.
	for {
		now, err := watch.Recv()
		if err != nil {
			t.Fatalf("waiting for the job to succeed: %v", err)
		}
		if i := slices.IndexFunc(now.GetJobs(), func(job *nodepb.Job) bool { return job.GetJobId() == id }); i >= 0 && now.GetJobs()[i].GetState() == nodepb.JobState_JOB_STATE_SUCCEEDED {
			if result := string(now.GetJobs()[i].GetResult()); !strings.Contains(result, `"count":168`) {
				t.Errorf("the job's result: %s", result)
			}
			break
		}
	}
	if got, err := client.GetJob(ctx, &nodepb.GetJobRequest{JobId: id}); err != nil || got.GetJob().GetState() != nodepb.JobState_JOB_STATE_SUCCEEDED {
		t.Errorf("GetJob = %v, %v", got, err)
	}
	if listed, err := client.ListJobs(ctx, &nodepb.ListJobsRequest{}); err != nil || len(listed.GetJobs()) != 1 || listed.GetJobs()[0].GetJobId() != id {
		t.Errorf("ListJobs = %v, %v", listed, err)
	}
	events, err := client.WatchJobEvents(ctx, &nodepb.WatchJobEventsRequest{JobId: id})
	if err != nil {
		t.Fatal(err)
	}
	if event, err := events.Recv(); err != nil || event.GetSeq() == 0 {
		t.Errorf("the job's first event: %v, %v", event, err)
	}
	// A private job is sealed with the node's own key, which goes to the
	// pool with it, as the CLI's --key-file does.
	if sealed, err := client.SubmitJob(ctx, &nodepb.SubmitJobRequest{Workload: "primes", Params: primes.GetParams(), Mode: primes.GetMode(), Private: true}); err != nil || !sealed.GetJob().GetPrivate() {
		t.Errorf("a private job submitted through the node: %v, %v", sealed, err)
	}
	// A finished job cannot be cancelled; the pool says so, and the node
	// passes that on.
	if _, err := client.CancelJob(ctx, &nodepb.CancelJobRequest{JobId: id}); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("cancelling a finished job: %v, want FailedPrecondition", err)
	}
	// The pool's workers are listed, this node among them.
	if workers, err := client.ListWorkers(ctx, &nodepb.ListWorkersRequest{}); err != nil || !slices.ContainsFunc(workers.GetWorkers(), func(w *nodepb.Worker) bool { return w.GetName() == "hand" }) {
		t.Errorf("ListWorkers = %v, %v", workers, err)
	}

	// With the coordinator gone, the node says the pool cannot be reached.
	stopCoordinator()
	waitFor(t, func() bool {
		_, err := client.ListJobs(ctx, &nodepb.ListJobsRequest{})
		return status.Code(err) == codes.Unavailable
	})
}

// fakePool is a pool's coordinator as a joined client node's JobControl
// calls it, failing as told.
type fakePool struct {
	pb.NodeServiceClient
	submit, get, list, watch error
	events                   []*pb.JobEvent
	recvErr                  error
}

func (f *fakePool) SubmitJob(context.Context, *pb.SubmitJobRequest, ...grpc.CallOption) (*pb.SubmitJobResponse, error) {
	return &pb.SubmitJobResponse{Job: &pb.Job{JobId: "j"}}, f.submit
}

func (f *fakePool) GetJob(_ context.Context, req *pb.GetJobRequest, _ ...grpc.CallOption) (*pb.GetJobResponse, error) {
	if req.GetJobId() == "gone" {
		return nil, status.Error(codes.NotFound, "no such job")
	}
	return &pb.GetJobResponse{Job: &pb.Job{JobId: req.GetJobId()}}, f.get
}

func (f *fakePool) ListNodes(context.Context, *pb.ListNodesRequest, ...grpc.CallOption) (*pb.ListNodesResponse, error) {
	return &pb.ListNodesResponse{}, f.list
}

func (f *fakePool) WatchJob(context.Context, *pb.WatchJobRequest, ...grpc.CallOption) (grpc.ServerStreamingClient[pb.WatchJobResponse], error) {
	return nil, f.watch
}

func (f *fakePool) WatchJobEvents(context.Context, *pb.WatchJobEventsRequest, ...grpc.CallOption) (grpc.ServerStreamingClient[pb.JobEvent], error) {
	if f.watch != nil {
		return nil, f.watch
	}
	return &fakeEvents{events: f.events, err: f.recvErr}, nil
}

type fakeEvents struct {
	grpc.ServerStreamingClient[pb.JobEvent]
	events []*pb.JobEvent
	err    error
}

func (e *fakeEvents) Recv() (*pb.JobEvent, error) {
	if len(e.events) == 0 {
		return nil, e.err
	}
	event := e.events[0]
	e.events = e.events[1:]
	return event, nil
}

// fakeRecords keeps which jobs are the node's in memory, failing as told.
type fakeRecords struct {
	ids        []string
	load, save error
	dropped    []string
}

func (r *fakeRecords) PoolJobs(string) ([]string, error) { return r.ids, r.load }
func (r *fakeRecords) AddPoolJob(_, id string, _ time.Time) error {
	r.ids = append(r.ids, id)
	return r.save
}
func (r *fakeRecords) DropPoolJob(id string) error {
	r.dropped = append(r.dropped, id)
	return nil
}

// What a joined client node's JobControl does when the pool, or its own
// record of its jobs, fails it.
func TestAJoinedClientNodesJobsWhenThingsFail(t *testing.T) {
	broken := errors.New("broken")
	client := worker.JoinedPool{ID: "c", Roles: []pb.Role{pb.Role_ROLE_WORKER, pb.Role_ROLE_CLIENT}, Connected: true}
	newJobs := func(pool *fakePool, records *fakeRecords) *poolJobs {
		return newPoolJobs(context.Background(), pool, records, func() worker.JoinedPool { return client }, func() {})
	}

	// A submission the pool refuses is refused, and not noted.
	records := &fakeRecords{}
	if _, err := newJobs(&fakePool{submit: broken}, records).Submit(context.Background(), &pb.JobSpec{}); !errors.Is(err, broken) || len(records.ids) != 0 {
		t.Errorf("a refused submission: %v, noted %v", err, records.ids)
	}
	// One the node cannot note is reported, naming the job.
	if _, err := newJobs(&fakePool{}, &fakeRecords{save: broken}).Submit(context.Background(), &pb.JobSpec{}); status.Code(err) != codes.Internal || !strings.Contains(err.Error(), "job j") {
		t.Errorf("a submission that could not be noted: %v", err)
	}

	// A job the pool no longer has is forgotten; one it could not be asked
	// about is only left out.
	records = &fakeRecords{ids: []string{"kept", "gone"}}
	if jobs := newJobs(&fakePool{}, records).Jobs(); len(jobs) != 1 || jobs[0].GetJobId() != "kept" || !slices.Equal(records.dropped, []string{"gone"}) {
		t.Errorf("listed %v and forgot %v", jobs, records.dropped)
	}
	if jobs := newJobs(&fakePool{get: broken}, &fakeRecords{ids: []string{"kept"}}).Jobs(); len(jobs) != 0 {
		t.Errorf("with the pool failing, listed %v", jobs)
	}
	if jobs := newJobs(&fakePool{}, &fakeRecords{load: broken}).Jobs(); jobs != nil {
		t.Errorf("with the record unreadable, listed %v", jobs)
	}
	if nodes := newJobs(&fakePool{list: broken}, &fakeRecords{}).Nodes(); nodes != nil {
		t.Errorf("with the pool failing, its workers are %v", nodes)
	}

	// A watch passes on what goes wrong with it.
	nothing := func(*pb.JobEvent) error { return nil }
	if err := newJobs(&fakePool{watch: broken}, &fakeRecords{}).WatchEvents(context.Background(), "j", 0, nothing); !errors.Is(err, broken) {
		t.Errorf("a watch the pool refused: %v", err)
	}
	if err := newJobs(&fakePool{events: []*pb.JobEvent{{Seq: 1}}, recvErr: broken}, &fakeRecords{}).WatchEvents(context.Background(), "j", 0, nothing); !errors.Is(err, broken) {
		t.Errorf("a watch cut short: %v", err)
	}
	if err := newJobs(&fakePool{events: []*pb.JobEvent{{Seq: 1}}, recvErr: io.EOF}, &fakeRecords{}).WatchEvents(context.Background(), "j", 0, func(*pb.JobEvent) error { return broken }); !errors.Is(err, broken) {
		t.Errorf("a watch whose receiver gave up: %v", err)
	}

	if err := newJobs(&fakePool{watch: broken}, &fakeRecords{}).Watch(context.Background(), "j", func(*pb.Job) error { return nil }); !errors.Is(err, broken) {
		t.Errorf("a planner's watch the pool refused: %v", err)
	}

	// Resuming with the record unreadable follows nothing.
	jobs := newJobs(&fakePool{}, &fakeRecords{load: broken})
	jobs.Resume()
	if len(jobs.following)+len(jobs.over) != 0 {
		t.Errorf("following %v, over %v", jobs.following, jobs.over)
	}
}

// Resuming follows each job the node has recorded once: a job recorded twice
// is followed once, and one already followed to its end is not followed
// again. The end-to-end tests reach these only when a reconnect happens to
// meet a recorded job, so they are driven here (#642).
func TestAJoinedClientNodeFollowsEachRecordedJobOnce(t *testing.T) {
	client := worker.JoinedPool{ID: "c", Roles: []pb.Role{pb.Role_ROLE_CLIENT}, Connected: true}
	changed := make(chan struct{}, 8)
	jobs := newPoolJobs(context.Background(), &fakePool{events: []*pb.JobEvent{{Seq: 1}}, recvErr: io.EOF}, &fakeRecords{ids: []string{"j", "j"}},
		func() worker.JoinedPool { return client }, func() { changed <- struct{}{} })

	jobs.Resume()
	// A watch tells of its event, then of its end, which it notes first.
	for range 2 {
		select {
		case <-changed:
		case <-time.After(5 * time.Second):
			t.Fatal("the job was never followed to its end")
		}
	}
	jobs.mu.Lock()
	followed, over := len(jobs.following), jobs.over["j"]
	jobs.mu.Unlock()
	if followed != 0 || !over {
		t.Fatalf("after its watch ended: following %d, over %v", followed, over)
	}

	// Neither the job recorded twice nor resuming again starts a second watch.
	jobs.Resume()
	select {
	case <-changed:
		t.Error("a job already followed to its end was followed again")
	case <-time.After(100 * time.Millisecond):
	}
}

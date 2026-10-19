package main

import (
	"context"
	"errors"
	"io"
	"slices"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/sisyphus-network/Sisyphus/apps/sisyphusd/worker"
	pb "github.com/sisyphus-network/Sisyphus/packages/protocol/sisyphus/v1"
)

// poolCallTimeout bounds a call to the joined pool's coordinator that the
// local API makes without a deadline of its own.
const poolCallTimeout = 10 * time.Second

var (
	errNotAClient = status.Error(codes.FailedPrecondition, "this node is not a client of the pool it has joined, so it cannot submit jobs to it or read them")
	errPoolAway   = status.Error(codes.Unavailable, "this node is not connected to the pool it has joined; its jobs are there, and can be read again once it is")
)

// poolJobRecords is where a node keeps which of a pool's jobs are its own.
// A nodedb.DB is one.
type poolJobRecords interface {
	PoolJobs(coordinatorID string) ([]string, error)
	AddPoolJob(coordinatorID, jobID string, now time.Time) error
	DropPoolJob(jobID string) error
}

// poolJobs is the local API's JobControl on a node that has joined another's
// pool: what it is asked it asks the pool's coordinator, as a client, which
// checks on every call that the node may (#474). The coordinator keeps the
// jobs; the node keeps only which of them it submitted, so that it can list
// them, and follows each until it is over so that a watch of them is told
// when one changes.
type poolJobs struct {
	ctx     context.Context
	client  pb.NodeServiceClient
	records poolJobRecords
	pool    func() worker.JoinedPool
	changed func()

	mu sync.Mutex
	// following holds the jobs being followed now, and over those that have
	// been followed to their end.
	following, over map[string]bool
}

func newPoolJobs(ctx context.Context, client pb.NodeServiceClient, records poolJobRecords, pool func() worker.JoinedPool, changed func()) *poolJobs {
	return &poolJobs{ctx: ctx, client: client, records: records, pool: pool, changed: changed, following: make(map[string]bool), over: make(map[string]bool)}
}

// Available says whether the pool's jobs can be reached: the node must hold
// the client role there, and be connected.
func (p *poolJobs) Available() error {
	pool := p.pool()
	switch {
	case !slices.Contains(pool.Roles, pb.Role_ROLE_CLIENT):
		return errNotAClient
	case !pool.Connected:
		return errPoolAway
	}
	return nil
}

func (p *poolJobs) Submit(ctx context.Context, spec *pb.JobSpec) (*pb.Job, error) {
	res, err := p.client.SubmitJob(ctx, &pb.SubmitJobRequest{Spec: spec})
	if err != nil {
		return nil, err
	}
	job := res.GetJob()
	if err := p.records.AddPoolJob(p.pool().ID, job.GetJobId(), time.Now()); err != nil {
		return nil, status.Errorf(codes.Internal, "job %s was submitted to the pool, but this node could not note it as its own: %v", job.GetJobId(), err)
	}
	p.follow(job.GetJobId())
	p.changed()
	return job, nil
}

func (p *poolJobs) Cancel(jobID string) (*pb.Job, error) {
	ctx, cancel := context.WithTimeout(p.ctx, poolCallTimeout)
	defer cancel()
	res, err := p.client.CancelJob(ctx, &pb.CancelJobRequest{JobId: jobID})
	return res.GetJob(), err
}

func (p *poolJobs) Get(jobID string) (*pb.Job, error) {
	ctx, cancel := context.WithTimeout(p.ctx, poolCallTimeout)
	defer cancel()
	res, err := p.client.GetJob(ctx, &pb.GetJobRequest{JobId: jobID})
	return res.GetJob(), err
}

func (p *poolJobs) WatchEvents(ctx context.Context, jobID string, after uint64, fn func(*pb.JobEvent) error) error {
	stream, err := p.client.WatchJobEvents(ctx, &pb.WatchJobEventsRequest{JobId: jobID, AfterSeq: after})
	if err != nil {
		return err
	}
	return receive(stream, fn)
}

// Watch calls fn with the job as it stands, and again each time it changes,
// until it is over: what the node's planner waits on.
func (p *poolJobs) Watch(ctx context.Context, jobID string, fn func(*pb.Job) error) error {
	stream, err := p.client.WatchJob(ctx, &pb.WatchJobRequest{JobId: jobID})
	if err != nil {
		return err
	}
	return receive(stream, func(res *pb.WatchJobResponse) error { return fn(res.GetJob()) })
}

// receive calls fn with each message on a stream from the pool until the
// stream ends, and says how it ended.
func receive[T any](stream grpc.ServerStreamingClient[T], fn func(*T) error) error {
	for {
		msg, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		if err := fn(msg); err != nil {
			return err
		}
	}
}

// Jobs returns the node's own jobs in the pool, newest first, as the
// coordinator has them now. One the coordinator no longer has is forgotten;
// one it could not be asked about is left out this time.
func (p *poolJobs) Jobs() []*pb.Job {
	ids, err := p.records.PoolJobs(p.pool().ID)
	if err != nil {
		return nil
	}
	var jobs []*pb.Job
	for _, id := range ids {
		job, err := p.Get(id)
		switch status.Code(err) {
		case codes.OK:
			jobs = append(jobs, job)
		case codes.NotFound:
			p.records.DropPoolJob(id)
		}
	}
	return jobs
}

// Nodes returns the pool's workers, as a client of it may see them.
func (p *poolJobs) Nodes() []*pb.NodeInfo {
	ctx, cancel := context.WithTimeout(p.ctx, poolCallTimeout)
	defer cancel()
	res, err := p.client.ListNodes(ctx, &pb.ListNodesRequest{})
	if err != nil {
		return nil
	}
	return res.GetNodes()
}

// Resume follows each of the node's jobs not yet followed to its end, as
// when the node has just connected, or has just become a client.
func (p *poolJobs) Resume() {
	if p.Available() != nil {
		return
	}
	ids, err := p.records.PoolJobs(p.pool().ID)
	if err != nil {
		return
	}
	for _, id := range ids {
		p.follow(id)
	}
}

// follow watches a job's events until it is over, saying the node's jobs
// have changed at each. A watch cut short, as when the connection is lost,
// is taken up again by the next Resume.
func (p *poolJobs) follow(jobID string) {
	p.mu.Lock()
	if p.following[jobID] || p.over[jobID] {
		p.mu.Unlock()
		return
	}
	p.following[jobID] = true
	p.mu.Unlock()
	go func() {
		err := p.WatchEvents(p.ctx, jobID, 0, func(*pb.JobEvent) error {
			p.changed()
			return nil
		})
		p.mu.Lock()
		delete(p.following, jobID)
		if err == nil {
			p.over[jobID] = true
		}
		p.mu.Unlock()
		p.changed()
	}()
}

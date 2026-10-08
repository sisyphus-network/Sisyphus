package inference

import (
	"context"
	"errors"
	"io"
	"testing"

	"google.golang.org/grpc"

	pb "github.com/sisyphus-network/Sisyphus/packages/protocol/sisyphus/v1"
)

var errGone = errors.New("the coordinator has gone")

// coordinator is the node that coordinates a pool, as a member calls it.
// It fails at whatever is named in failing, and its streams give two
// things and then end, or break.
type coordinator struct {
	pb.NodeServiceClient
	failing string
	broken  bool
}

func (c *coordinator) fail(call string) error {
	if c.failing == call {
		return errGone
	}
	return nil
}

type flow[T any] struct {
	grpc.ClientStream
	left   []*T
	broken bool
}

func (f *flow[T]) Recv() (*T, error) {
	if len(f.left) == 0 {
		if f.broken {
			return nil, errGone
		}
		return nil, io.EOF
	}
	next := f.left[0]
	f.left = f.left[1:]
	return next, nil
}

func (c *coordinator) SubmitJob(context.Context, *pb.SubmitJobRequest, ...grpc.CallOption) (*pb.SubmitJobResponse, error) {
	return &pb.SubmitJobResponse{Job: &pb.Job{JobId: "job-1"}}, c.fail("SubmitJob")
}

func (c *coordinator) GetJob(context.Context, *pb.GetJobRequest, ...grpc.CallOption) (*pb.GetJobResponse, error) {
	return &pb.GetJobResponse{Job: &pb.Job{JobId: "job-1"}}, c.fail("GetJob")
}

func (c *coordinator) CancelJob(context.Context, *pb.CancelJobRequest, ...grpc.CallOption) (*pb.CancelJobResponse, error) {
	return &pb.CancelJobResponse{Job: &pb.Job{JobId: "job-1"}}, c.fail("CancelJob")
}

func (c *coordinator) ListNodes(context.Context, *pb.ListNodesRequest, ...grpc.CallOption) (*pb.ListNodesResponse, error) {
	if err := c.fail("ListNodes"); err != nil {
		return nil, err
	}
	return &pb.ListNodesResponse{Nodes: []*pb.NodeInfo{{Name: "rig"}}}, nil
}

func (c *coordinator) WatchJob(context.Context, *pb.WatchJobRequest, ...grpc.CallOption) (grpc.ServerStreamingClient[pb.WatchJobResponse], error) {
	if err := c.fail("WatchJob"); err != nil {
		return nil, err
	}
	return &flow[pb.WatchJobResponse]{broken: c.broken, left: []*pb.WatchJobResponse{{Job: &pb.Job{JobId: "job-1"}}, {Job: &pb.Job{JobId: "job-1", State: pb.JobState_JOB_STATE_SUCCEEDED}}}}, nil
}

func (c *coordinator) WatchJobEvents(context.Context, *pb.WatchJobEventsRequest, ...grpc.CallOption) (grpc.ServerStreamingClient[pb.JobEvent], error) {
	if err := c.fail("WatchJobEvents"); err != nil {
		return nil, err
	}
	return &flow[pb.JobEvent]{broken: c.broken, left: []*pb.JobEvent{{Seq: 1}, {Seq: 2}}}, nil
}

func TestAMemberReachesItsPoolThroughTheNodeThatCoordinatesIt(t *testing.T) {
	ctx := context.Background()
	pool := Remote{Node: &coordinator{}}
	if job, err := pool.Submit(ctx, &pb.JobSpec{Workload: "chat"}); err != nil || job.GetJobId() != "job-1" {
		t.Errorf("Submit = %v, %v", job, err)
	}
	if job, err := pool.Get("job-1"); err != nil || job.GetJobId() != "job-1" {
		t.Errorf("Get = %v, %v", job, err)
	}
	if job, err := pool.Cancel("job-1"); err != nil || job.GetJobId() != "job-1" {
		t.Errorf("Cancel = %v, %v", job, err)
	}
	if nodes := pool.Nodes(); len(nodes) != 1 || nodes[0].GetName() != "rig" {
		t.Errorf("Nodes = %v", nodes)
	}
	var last *pb.Job
	if err := pool.Watch(ctx, "job-1", func(job *pb.Job) error { last = job; return nil }); err != nil || last.GetState() != pb.JobState_JOB_STATE_SUCCEEDED {
		t.Errorf("Watch ended with %v, %v", last, err)
	}
	seen := 0
	if err := pool.WatchEvents(ctx, "job-1", 0, func(*pb.JobEvent) error { seen++; return nil }); err != nil || seen != 2 {
		t.Errorf("WatchEvents saw %d, %v", seen, err)
	}
	// Whoever is watching may stop.
	enough := errors.New("enough")
	if err := pool.Watch(ctx, "job-1", func(*pb.Job) error { return enough }); !errors.Is(err, enough) {
		t.Errorf("Watch, stopped: %v", err)
	}
	if err := pool.WatchEvents(ctx, "job-1", 0, func(*pb.JobEvent) error { return enough }); !errors.Is(err, enough) {
		t.Errorf("WatchEvents, stopped: %v", err)
	}

	// A coordinator that has gone is a failure, and no workers.
	quiet, heard := func(*pb.Job) error { return nil }, func(*pb.JobEvent) error { return nil }
	for call, failed := range map[string]func(Remote) error{
		"SubmitJob":      func(r Remote) error { _, err := r.Submit(ctx, nil); return err },
		"GetJob":         func(r Remote) error { _, err := r.Get("job-1"); return err },
		"CancelJob":      func(r Remote) error { _, err := r.Cancel("job-1"); return err },
		"WatchJob":       func(r Remote) error { return r.Watch(ctx, "job-1", quiet) },
		"WatchJobEvents": func(r Remote) error { return r.WatchEvents(ctx, "job-1", 0, heard) },
	} {
		if err := failed(Remote{Node: &coordinator{failing: call}}); !errors.Is(err, errGone) {
			t.Errorf("with %s failing: %v", call, err)
		}
	}
	if nodes := (Remote{Node: &coordinator{failing: "ListNodes"}}).Nodes(); len(nodes) != 0 {
		t.Errorf("the workers of a coordinator that has gone: %v", nodes)
	}
	lost := Remote{Node: &coordinator{broken: true}}
	if err := lost.Watch(ctx, "job-1", quiet); !errors.Is(err, errGone) {
		t.Errorf("Watch, cut off: %v", err)
	}
	if err := lost.WatchEvents(ctx, "job-1", 0, heard); !errors.Is(err, errGone) {
		t.Errorf("WatchEvents, cut off: %v", err)
	}
}

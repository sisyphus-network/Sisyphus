package inference

import (
	"context"
	"errors"
	"io"

	pb "github.com/sisyphus-network/Sisyphus/packages/protocol/sisyphus/v1"
)

// Remote is a pool as one of its members reaches it: through the node
// that coordinates it, over the connection the member already has to that
// node, which knows who it is talking to. With it a member offers the
// pool's models on its own machine, to its own programs, and nothing new
// is opened to the network.
type Remote struct {
	Node pb.NodeServiceClient
}

func (r Remote) Submit(ctx context.Context, spec *pb.JobSpec) (*pb.Job, error) {
	submitted, err := r.Node.SubmitJob(ctx, &pb.SubmitJobRequest{Spec: spec})
	return submitted.GetJob(), err
}

func (r Remote) Get(jobID string) (*pb.Job, error) {
	got, err := r.Node.GetJob(context.Background(), &pb.GetJobRequest{JobId: jobID})
	return got.GetJob(), err
}

func (r Remote) Cancel(jobID string) (*pb.Job, error) {
	stopped, err := r.Node.CancelJob(context.Background(), &pb.CancelJobRequest{JobId: jobID})
	return stopped.GetJob(), err
}

// Nodes lists the pool's connected workers, or none if the coordinator
// cannot be asked just now.
func (r Remote) Nodes() []*pb.NodeInfo {
	listed, _ := r.Node.ListNodes(context.Background(), &pb.ListNodesRequest{})
	return listed.GetNodes()
}

// Watch calls fn with the job as it changes and returns when it is over.
func (r Remote) Watch(ctx context.Context, jobID string, fn func(*pb.Job) error) error {
	stream, err := r.Node.WatchJob(ctx, &pb.WatchJobRequest{JobId: jobID})
	if err != nil {
		return err
	}
	for {
		next, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		if err := fn(next.GetJob()); err != nil {
			return err
		}
	}
}

// WatchEvents calls fn with each thing that happens to the job and returns
// when it is over.
func (r Remote) WatchEvents(ctx context.Context, jobID string, after uint64, fn func(*pb.JobEvent) error) error {
	stream, err := r.Node.WatchJobEvents(ctx, &pb.WatchJobEventsRequest{JobId: jobID, AfterSeq: after})
	if err != nil {
		return err
	}
	for {
		next, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		if err := fn(next); err != nil {
			return err
		}
	}
}

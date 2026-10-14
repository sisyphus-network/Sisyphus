package main

import (
	"context"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	"github.com/sisyphus-network/Sisyphus/apps/sisyphusd/access"
	pb "github.com/sisyphus-network/Sisyphus/packages/protocol/sisyphus/v1"
	"github.com/sisyphus-network/Sisyphus/packages/runtime"
)

// Verifying by agreement means something only if a worker cannot copy what
// another returned. Until a task is settled the coordinator is the only one
// that has its results: a second worker is handed what the first was, and
// nothing a worker may call tells it about a job.
func TestAWorkerIsToldNothingOfWhatAnotherReturnedForItsTask(t *testing.T) {
	p := startPool(t, runtime.Builtin())
	crew := p.crew("first", "second")
	id := p.submit(verifiedPrimes(1, 2)).GetJobId()

	first := crew[0].assignment()
	crew[0].returns(first, `{"count":25}`)
	// The first result is in, and the task is not settled. What the second
	// worker holds is the task and which attempt at it this is.
	second := crew[1].assignment()
	same := proto.Clone(second).(*pb.TaskAssignment)
	same.Attempt = first.GetAttempt()
	if !proto.Equal(first, same) {
		t.Errorf("the second worker was handed something other than the first was:\n%v\n%v", first, second)
	}

	// It cannot ask about the job either, by any of the ways a client can.
	_, creds := p.admit(access.Worker)
	conn, err := grpc.NewClient(p.addr, grpc.WithTransportCredentials(creds))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	node := pb.NewNodeServiceClient(conn)
	asks := map[string]func(context.Context) error{
		"GetJob": func(ctx context.Context) error {
			_, err := node.GetJob(ctx, &pb.GetJobRequest{JobId: id})
			return err
		},
		"GetJobRecord": func(ctx context.Context) error {
			_, err := node.GetJobRecord(ctx, &pb.GetJobRecordRequest{JobId: id})
			return err
		},
		"WatchJob": func(ctx context.Context) error {
			stream, err := node.WatchJob(ctx, &pb.WatchJobRequest{JobId: id})
			if err != nil {
				return err
			}
			_, err = stream.Recv()
			return err
		},
		"WatchJobEvents": func(ctx context.Context) error {
			stream, err := node.WatchJobEvents(ctx, &pb.WatchJobEventsRequest{JobId: id})
			if err != nil {
				return err
			}
			_, err = stream.Recv()
			return err
		},
	}
	for name, ask := range asks {
		if err := ask(p.ctx); status.Code(err) != codes.PermissionDenied {
			t.Errorf("a worker calling %s: %v", name, err)
		}
	}

	crew[1].returns(second, `{"count":25}`)
	if done := p.wait(id); done.GetState() != pb.JobState_JOB_STATE_SUCCEEDED {
		t.Errorf("the job: %v %s", done.GetState(), done.GetError())
	}
}

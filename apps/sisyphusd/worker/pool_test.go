package worker_test

import (
	"context"
	"slices"
	"testing"
	"time"

	"google.golang.org/grpc"

	"github.com/sisyphus-network/Sisyphus/apps/sisyphusd/worker"
	pb "github.com/sisyphus-network/Sisyphus/packages/protocol/sisyphus/v1"
	"github.com/sisyphus-network/Sisyphus/packages/runtime"
	"github.com/sisyphus-network/Sisyphus/packages/storage"
)

// countingCoordinator welcomes a worker as both worker and client, tells it
// how many workers the pool has, then takes its client role away.
type countingCoordinator struct {
	pb.UnimplementedCoordinatorServiceServer
}

func (countingCoordinator) Connect(stream grpc.BidiStreamingServer[pb.WorkerMessage, pb.CoordinatorMessage]) error {
	if _, err := stream.Recv(); err != nil {
		return err
	}
	stream.Send(&pb.CoordinatorMessage{Kind: &pb.CoordinatorMessage_Welcome{Welcome: &pb.Welcome{CoordinatorId: "counting", Roles: []pb.Role{pb.Role_ROLE_WORKER, pb.Role_ROLE_CLIENT}}}})
	stream.Send(&pb.CoordinatorMessage{Kind: &pb.CoordinatorMessage_WorkerCount{WorkerCount: &pb.WorkerCount{Count: 3}}})
	stream.Send(&pb.CoordinatorMessage{Kind: &pb.CoordinatorMessage_RolesChanged{RolesChanged: &pb.RolesChanged{Roles: []pb.Role{pb.Role_ROLE_WORKER}}}})
	for {
		if _, err := stream.Recv(); err != nil {
			return nil
		}
	}
}

// A worker keeps the count of the pool's workers it was told as a client,
// but once it is no longer a client, or no longer connected, that count is
// not live.
func TestAWorkerCountIsLiveOnlyForAConnectedClient(t *testing.T) {
	srv := newServer()
	pb.RegisterCoordinatorServiceServer(srv, countingCoordinator{})
	addr, _ := serve(t, srv)
	changed := make(chan struct{}, 1)
	w := &worker.Worker{Name: "w", Coordinator: addr, Credentials: workerCreds, Slots: 1, Workloads: runtime.Builtin(), Blobs: storage.NewMemory(), Log: quiet(),
		OnPool: func() {
			select {
			case changed <- struct{}{}:
			default:
			}
		}}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		w.Run(ctx)
	}()
	until := func(what string, want func(worker.JoinedPool) bool) worker.JoinedPool {
		t.Helper()
		timeout := time.After(30 * time.Second)
		for {
			if pool := w.JoinedPool(); want(pool) {
				return pool
			}
			select {
			case <-changed:
			case <-timeout:
				t.Fatalf("waiting for %s, the pool is %+v", what, w.JoinedPool())
			}
		}
	}

	demoted := until("the client role to be taken away", func(p worker.JoinedPool) bool {
		return p.Connected && slices.Equal(p.Roles, []pb.Role{pb.Role_ROLE_WORKER})
	})
	if demoted.Workers != 3 || demoted.Counted {
		t.Errorf("no longer a client, the worker has %d workers, live %v; want 3, not live", demoted.Workers, demoted.Counted)
	}

	cancel()
	<-done
	if gone := w.JoinedPool(); gone.Connected || gone.Counted {
		t.Errorf("gone from the pool, the worker has %+v", gone)
	}
}

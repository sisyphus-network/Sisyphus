package main

import (
	"context"
	"log/slog"
	"sync"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pb "github.com/sisyphus-network/Sisyphus/packages/protocol/sisyphus/v1"
)

// poolDeparture is how a node that works for another's pool leaves it (#509).
// The coordinator lets go of what it kept for the node first. Only once it
// has said so does the node forget the pool, so that a leave that cannot be
// carried out leaves everything as it was, to be asked again.
//
// The node was started to work for that pool, so once it has left, it
// stops: its worker, turned away on reconnecting, would otherwise stop it
// with an error.
type poolDeparture struct {
	pool pb.PoolServiceClient
	// forget forgets what the node kept about the pool the coordinator with
	// the given ID runs. A nodedb.DB's ForgetPool is one.
	forget func(coordinatorID string) error
	// coordinatorID and address are the coordinator's, and dataDir is where
	// the node remembers which node is at that address.
	coordinatorID, address, dataDir string
	log                             *slog.Logger

	// mu keeps one leave from overtaking another, and the worker's ending
	// from being taken for what it was before the leave was answered.
	mu sync.Mutex
	// asked is whether the coordinator has let the node go; done is closed
	// once the node has forgotten the pool too.
	asked bool
	done  chan struct{}
}

func newPoolDeparture(pool pb.PoolServiceClient, forget func(string) error, coordinatorID, address, dataDir string, log *slog.Logger) *poolDeparture {
	return &poolDeparture{pool: pool, forget: forget, coordinatorID: coordinatorID, address: address, dataDir: dataDir, log: log, done: make(chan struct{})}
}

// leave has the node leave the pool. Asked again, after a part of it
// failed, it carries out the rest: the coordinator answers a node that has
// already left as it did the first time.
func (l *poolDeparture) leave(ctx context.Context) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	select {
	case <-l.done:
		return nil
	default:
	}
	if _, bounded := ctx.Deadline(); !bounded {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, poolCallTimeout)
		defer cancel()
	}
	if _, err := l.pool.Leave(ctx, &pb.LeaveRequest{}); err != nil {
		return status.Errorf(status.Code(err), "the pool's coordinator could not be asked to let this node go, so it has not left the pool; ask again: %s", status.Convert(err).Message())
	}
	l.asked = true
	if err := l.forget(l.coordinatorID); err != nil {
		return status.Errorf(codes.Internal, "this node has left the pool, but could not forget it here; ask again: %v", err)
	}
	// Last, so that a node that stops before this still knows where the pool
	// is, and can be asked to leave it again.
	if err := forgetKnown(l.dataDir, l.address); err != nil {
		return status.Errorf(codes.Internal, "this node has left the pool, but could not forget it here; ask again: %v", err)
	}
	close(l.done)
	l.log.Info("left the pool; this node stops, and joins another when started with its --coordinator", "coordinator", l.coordinatorID)
	return nil
}

// workerStopped is what the node stops with once its worker has stopped
// with err. A worker turned away because the node has left is no failure;
// the node stops once it has forgotten the pool, or is stopped before.
func (l *poolDeparture) workerStopped(ctx context.Context, err error) error {
	l.mu.Lock()
	asked := l.asked
	l.mu.Unlock()
	if !asked {
		return err
	}
	select {
	case <-l.done:
	case <-ctx.Done():
	}
	return nil
}

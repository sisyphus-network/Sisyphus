// Package worker implements the worker role of sisyphusd: it dials a
// coordinator, advertises what this machine offers and executes the tasks it
// is assigned.
package worker

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	goruntime "runtime"
	"sync"
	"sync/atomic"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/status"

	pb "github.com/excho0/Sisyphus/packages/protocol/sisyphus/v1"
	"github.com/excho0/Sisyphus/packages/runtime"
)

const (
	defaultHeartbeatInterval = 5 * time.Second
	minBackoff               = time.Second
	maxBackoff               = 30 * time.Second
)

type Worker struct {
	NodeID string
	// Coordinator is the host:port of the coordinator to join.
	Coordinator string
	// Slots is how many tasks this node runs at once.
	Slots     int
	Workloads *runtime.Registry
	// Blobs is the stored data this node's tasks can read and write.
	Blobs runtime.Blobs
	// HeartbeatInterval is how often the coordinator hears from this node
	// while it is idle. Zero means five seconds.
	HeartbeatInterval time.Duration
	Log               *slog.Logger
}

// Run keeps the worker connected to its coordinator, reconnecting with
// back-off, until ctx is cancelled. It returns an error only if the
// coordinator rejects this node outright.
func (w *Worker) Run(ctx context.Context) error {
	backoff := minBackoff
	for {
		welcomed, err := w.session(ctx)
		if ctx.Err() != nil {
			return nil
		}
		switch status.Code(err) {
		case codes.AlreadyExists, codes.InvalidArgument:
			return fmt.Errorf("coordinator rejected node %q: %w", w.NodeID, err)
		}
		if welcomed {
			backoff = minBackoff
		}
		w.Log.Warn("lost coordinator, reconnecting", "node", w.NodeID, "in", backoff.String(), "error", err)
		select {
		case <-time.After(backoff):
		case <-ctx.Done():
			return nil
		}
		backoff = min(backoff*2, maxBackoff)
	}
}

// session runs one connection to the coordinator until it breaks. It reports
// whether the coordinator accepted this node.
func (w *Worker) session(ctx context.Context) (welcomed bool, err error) {
	conn, err := grpc.NewClient(w.Coordinator,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithKeepaliveParams(keepalive.ClientParameters{Time: 10 * time.Second, Timeout: 5 * time.Second, PermitWithoutStream: true}),
	)
	if err != nil {
		return false, err
	}
	defer conn.Close()

	// Deferred calls run last-in first-out: cancelling the session context
	// stops running tasks and the heartbeat before waiting for them.
	var tasks sync.WaitGroup
	defer tasks.Wait()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	stream, err := pb.NewCoordinatorServiceClient(conn).Connect(ctx)
	if err != nil {
		return false, err
	}
	// A gRPC stream allows only one sender at a time.
	var sendMu sync.Mutex
	send := func(msg *pb.WorkerMessage) error {
		sendMu.Lock()
		defer sendMu.Unlock()
		return stream.Send(msg)
	}

	// A send on a broken stream fails without saying why; the reason comes
	// from Recv below, so send errors are not checked here or in the
	// heartbeat.
	send(&pb.WorkerMessage{Kind: &pb.WorkerMessage_Hello{Hello: &pb.Hello{
		NodeId:       w.NodeID,
		Capabilities: w.capabilities(),
	}}})

	var running atomic.Int32
	tasks.Add(1)
	go func() {
		defer tasks.Done()
		interval := w.HeartbeatInterval
		if interval == 0 {
			interval = defaultHeartbeatInterval
		}
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				heartbeat := &pb.Heartbeat{RunningTasks: uint32(running.Load())}
				send(&pb.WorkerMessage{Kind: &pb.WorkerMessage_Heartbeat{Heartbeat: heartbeat}})
			case <-ctx.Done():
				return
			}
		}
	}()

	for {
		msg, err := stream.Recv()
		if err != nil {
			return welcomed, err
		}
		switch kind := msg.GetKind().(type) {
		case *pb.CoordinatorMessage_Welcome:
			welcomed = true
			w.Log.Info("joined pool", "node", w.NodeID, "coordinator", kind.Welcome.GetCoordinatorId(), "slots", w.Slots)
		case *pb.CoordinatorMessage_Assignment:
			running.Add(1)
			tasks.Add(1)
			go func() {
				defer tasks.Done()
				defer running.Add(-1)
				result := w.execute(ctx, kind.Assignment)
				// A result from a cancelled session is of no use: the
				// coordinator has already given the task to someone else.
				if ctx.Err() == nil {
					send(&pb.WorkerMessage{Kind: &pb.WorkerMessage_TaskResult{TaskResult: result}})
				}
			}()
		}
	}
}

func (w *Worker) execute(ctx context.Context, a *pb.TaskAssignment) (result *pb.TaskResult) {
	result = &pb.TaskResult{TaskId: a.GetTaskId(), Attempt: a.GetAttempt()}
	fail := func(err any) {
		result.Outcome = &pb.TaskResult_Error{Error: fmt.Sprint(err)}
	}
	// A workload that panics must cost one task, not the whole daemon.
	defer func() {
		if r := recover(); r != nil {
			fail(fmt.Sprintf("workload panicked: %v", r))
		}
	}()

	workload, err := w.Workloads.Get(a.GetWorkload())
	if err != nil {
		fail(err)
		return result
	}
	started := time.Now()
	touched := runtime.Record(w.Blobs)
	output, err := workload.Execute(ctx, touched, a.GetPayload())
	if err != nil {
		fail(err)
		return result
	}
	result.ReadBlobs, result.WrittenBlobs = touched.Read(), touched.Written()
	w.Log.Debug("task done", "node", w.NodeID, "task", a.GetTaskId(), "took", time.Since(started).String())
	result.Outcome = &pb.TaskResult_Output{Output: output}
	return result
}

func (w *Worker) capabilities() *pb.NodeCapabilities {
	hostname, _ := os.Hostname()
	return &pb.NodeCapabilities{
		Hostname:  hostname,
		Os:        goruntime.GOOS,
		Arch:      goruntime.GOARCH,
		CpuCores:  uint32(goruntime.NumCPU()),
		TaskSlots: uint32(w.Slots),
		Workloads: w.Workloads.Names(),
	}
}

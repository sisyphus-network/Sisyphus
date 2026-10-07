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
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/status"

	pb "github.com/sisyphus-network/Sisyphus/packages/protocol/sisyphus/v1"
	"github.com/sisyphus-network/Sisyphus/packages/runtime"
	"github.com/sisyphus-network/Sisyphus/packages/sealed"
)

const (
	defaultHeartbeatInterval = 5 * time.Second
	minBackoff               = time.Second
	maxBackoff               = 30 * time.Second
)

type Worker struct {
	// Name is a label for people to recognise this node by. The node's ID is
	// that of the key in Credentials.
	Name string
	// Coordinator is the host:port of the coordinator to connect to, and
	// Credentials how to prove who this node is and check who answers.
	Coordinator string
	Credentials credentials.TransportCredentials
	// Slots is how many tasks this node runs at once.
	Slots int
	// Limit, if set, is shared with the node's other workers: a task takes
	// a slot from it before it runs and gives it back after, so that between
	// them they run no more at once than the node has room for. Pool names
	// whose tasks this worker's are, when slots are shared out.
	Limit     *Slots
	Pool      string
	Workloads *runtime.Registry
	// Blobs is the stored data this node's tasks can read and write.
	Blobs runtime.Blobs
	// ServeAddress, if not empty, is where other workers of the pool can
	// fetch blobs from this one.
	ServeAddress string
	// RelayAddresses are where this node's libp2p host relays between the
	// pool's members, if it does, and Relayed, if set, returns how many
	// times it has joined two of them and how many bytes passed.
	RelayAddresses []string
	Relayed        func() (connections, bytes uint64)
	// OnSwarm, if set, is called with the fingerprint of the key of the
	// pool's private IPFS network whenever the coordinator states it: on
	// joining, and when the key changes. It must not block for long.
	OnSwarm func(fingerprint string)
	// connected is whether the coordinator has this node in its pool right
	// now.
	connected atomic.Bool
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
		case codes.AlreadyExists, codes.InvalidArgument, codes.PermissionDenied:
			return fmt.Errorf("coordinator rejected node %q: %w", w.Name, err)
		}
		if welcomed {
			backoff = minBackoff
		}
		w.Log.Warn("lost coordinator, reconnecting", "node", w.Name, "in", backoff.String(), "error", err)
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
		grpc.WithTransportCredentials(w.Credentials),
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
		Name:         w.Name,
		Capabilities: w.capabilities(),
		ServeAddress: w.ServeAddress, RelayAddresses: w.RelayAddresses,
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
				if w.Relayed != nil {
					heartbeat.RelayedConnections, heartbeat.RelayedBytes = w.Relayed()
				}
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
		case *pb.CoordinatorMessage_SwarmUpdate:
			w.swarmChanged(kind.SwarmUpdate.GetSwarmFingerprint())
		case *pb.CoordinatorMessage_Welcome:
			welcomed = true
			w.connected.Store(true)
			defer w.connected.Store(false)
			w.swarmChanged(kind.Welcome.GetSwarmFingerprint())
			w.Log.Info("joined pool", "node", w.Name, "coordinator", kind.Welcome.GetCoordinatorId(), "slots", w.Slots)
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

// Connected reports whether the worker is connected to its coordinator and
// has been welcomed into the pool.
func (w *Worker) Connected() bool {
	return w.connected.Load()
}

func (w *Worker) swarmChanged(fingerprint string) {
	if w.OnSwarm != nil {
		w.OnSwarm(fingerprint)
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
	if w.Limit != nil {
		if !w.Limit.Acquire(ctx, w.Pool) {
			fail(ctx.Err())
			return result
		}
		defer w.Limit.Release(w.Pool)
	}
	started := time.Now()
	touched := runtime.Record(w.Blobs)
	var blobs runtime.Blobs = touched
	switch len(a.GetKey()) {
	case 0:
	case sealed.KeySize:
		// A private job: what the task stores is sealed, and sealed inputs
		// are opened, with the job's key.
		blobs = runtime.Sealed(touched, sealed.Key(a.GetKey()))
	default:
		fail(fmt.Sprintf("the task came with a key of %d bytes, which is not a key", len(a.GetKey())))
		return result
	}
	output, err := workload.Execute(ctx, blobs, a.GetPayload())
	if err != nil {
		fail(err)
		return result
	}
	result.ReadBlobs, result.WrittenBlobs = touched.Read(), touched.Written()
	w.Log.Debug("task done", "node", w.Name, "task", a.GetTaskId(), "took", time.Since(started).String())
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

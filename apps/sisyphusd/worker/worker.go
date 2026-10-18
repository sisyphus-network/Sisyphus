// Package worker implements the worker role of sisyphusd: it dials a
// coordinator, advertises what this machine offers and executes the tasks it
// is assigned.
package worker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"os"
	goruntime "runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"

	"github.com/sisyphus-network/Sisyphus/apps/sisyphusd/blobclient"
	"github.com/sisyphus-network/Sisyphus/packages/geo"
	"github.com/sisyphus-network/Sisyphus/packages/hardware"
	pb "github.com/sisyphus-network/Sisyphus/packages/protocol/sisyphus/v1"
	"github.com/sisyphus-network/Sisyphus/packages/runtime"
	"github.com/sisyphus-network/Sisyphus/packages/sealed"
)

const defaultHeartbeatInterval = 5 * time.Second

// How long a worker waits before trying its coordinator again, doubling
// from the least to the most. Once it is trying as seldom as it will, it
// says so once, and then only every so often that it is still trying, so
// that a coordinator that is down for the night does not fill the log.
// Tests put shorter times in their place.
var (
	minBackoff  = time.Second
	maxBackoff  = 30 * time.Second
	stillTrying = 10 * time.Minute
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
	// Hardware is what the machine has that its owner offers to the pool.
	Hardware hardware.Info
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
	// observed is the worker's address as its coordinator sees it, while it
	// is connected and that is a global address.
	observed atomic.Pointer[netip.Addr]
	// offered is what the worker last told its coordinator it has.
	offered atomic.Pointer[[]string]
	// changed carries word that what the worker offers may have changed;
	// see OffersChanged.
	changedOnce sync.Once
	changed     chan struct{}
	// HeartbeatInterval is how often the coordinator hears from this node
	// while it is idle, which is how it tells a quiet worker from a lost
	// one. Zero means five seconds.
	HeartbeatInterval time.Duration
	Log               *slog.Logger
}

// Run keeps the worker connected to its coordinator, reconnecting with
// back-off, until ctx is cancelled. It returns an error only if the
// coordinator rejects this node outright.
func (w *Worker) Run(ctx context.Context) error {
	backoff := minBackoff
	// lost is when the coordinator was last there, and told when the log
	// last said that it is not.
	var lost, told time.Time
	for {
		welcomed, err := w.session(ctx)
		if ctx.Err() != nil {
			return nil
		}
		switch status.Code(err) {
		case codes.AlreadyExists, codes.InvalidArgument, codes.PermissionDenied:
			return fmt.Errorf("coordinator rejected node %q: %w", w.Name, err)
		}
		if welcomed || lost.IsZero() {
			backoff, lost = minBackoff, time.Now()
		}
		switch {
		case backoff < maxBackoff:
			w.Log.Warn("lost coordinator, reconnecting", "node", w.Name, "in", backoff.String(), "error", err)
		case time.Since(told) >= stillTrying:
			told = time.Now()
			w.Log.Warn("still cannot reach the coordinator; trying again quietly", "node", w.Name,
				"unreachable_for", time.Since(lost).Round(time.Second).String(), "every", backoff.String(), "error", err)
		}
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
	hello := w.capabilities()
	offered := hello.GetLabels()
	w.storeOffered(offered)
	send(&pb.WorkerMessage{Kind: &pb.WorkerMessage_Hello{Hello: &pb.Hello{
		Name:         w.Name,
		Capabilities: hello,
		ServeAddress: w.ServeAddress, RelayAddresses: w.RelayAddresses,
		DialledAddress: dialled(stream.Context()),
	}}})

	var running atomic.Int32
	// stops holds, by task ID, how to stop each task now running.
	var stops sync.Map
	// refresh asks the workloads again what the worker offers and, if that
	// has changed, tells the coordinator. It is not done on a timer: it is
	// done on word that something the worker serves changed (see
	// OffersChanged) and after every task that used a model, before that
	// task's result is sent. So the coordinator, when it hears that a
	// model was not there, already knows not to give the task back.
	//
	// The asking is bounded by offersWait and by the session, so a slow or
	// absent model server delays a task's result by no more than that. One
	// that has not answered in time changes nothing, and refresh says so.
	// After a task, it is then asked once more in the background, but only
	// once the task's result has been sent, so the result always goes
	// first and a late answer still corrects what the coordinator was told.
	// Until then a model found missing may be asked for here again. An
	// asking in the background that times out asks no more, so a server
	// that never answers is not asked on a timer. Answers are asked for outside the lock, so tasks that end
	// together do not wait in turn, and one asked for earlier never replaces
	// one asked for later.
	var offeredMu sync.Mutex
	var asked, applied uint64
	refresh := func() (late bool) {
		offeredMu.Lock()
		asked++
		mine := asked
		offeredMu.Unlock()
		within, done := context.WithTimeout(ctx, offersWait)
		defer done()
		now := w.labelsWithin(within)
		if within.Err() != nil {
			return true
		}
		offeredMu.Lock()
		defer offeredMu.Unlock()
		if mine < applied {
			return false
		}
		applied = mine
		if !slices.Equal(now, offered) {
			offered = now
			w.storeOffered(now)
			send(&pb.WorkerMessage{Kind: &pb.WorkerMessage_Heartbeat{Heartbeat: &pb.Heartbeat{
				RunningTasks: uint32(running.Load()), Labels: &pb.Labels{Labels: now},
			}}})
		}
		return false
	}
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
			case <-w.changes():
				refresh()
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
			// Only an address that is someone's on the internet says
			// where this node is.
			if seen, err := netip.ParseAddr(kind.Welcome.GetObservedAddress()); err == nil && geo.Global(seen) {
				w.observed.Store(&seen)
				defer w.observed.Store(nil)
			}
			w.Log.Info("joined pool", "node", w.Name, "coordinator", kind.Welcome.GetCoordinatorId(), "slots", w.Slots)
		case *pb.CoordinatorMessage_Cancel:
			// A task that has already finished is no longer there to stop.
			if stop, running := stops.Load(kind.Cancel.GetTaskId()); running {
				stop.(context.CancelFunc)()
			}
		case *pb.CoordinatorMessage_Assignment:
			assigned := kind.Assignment
			running.Add(1)
			tasks.Add(1)
			// Each task can be stopped by itself, on the coordinator's word.
			taskCtx, stop := context.WithCancel(ctx)
			stops.Store(assigned.GetTaskId(), stop)
			go func() {
				defer tasks.Done()
				defer running.Add(-1)
				defer stops.Delete(assigned.GetTaskId())
				// What the task reports of itself is passed on as it changes,
				// no more than a few times a second, and once more when it ends.
				said := &reports{private: private(assigned), changed: make(chan struct{}, 1)}
				pass := func() {
					if update := said.take(assigned); update != nil {
						send(&pb.WorkerMessage{Kind: &pb.WorkerMessage_TaskUpdate{TaskUpdate: update}})
					}
				}
				passing := make(chan struct{})
				go func() {
					defer close(passing)
					passOn(said.changed, taskCtx.Done(), updateGap, pass)
				}()
				result := w.execute(runtime.WithReporter(taskCtx, said), assigned)
				stop()
				<-passing
				// A result from a cancelled session is of no use: the
				// coordinator has already given the task to someone else.
				if ctx.Err() == nil {
					pass()
					// Running a model loads it, and failing to may mean it
					// is gone: what the worker offers is told first.
					late := runtime.ModelNeeded(assigned.GetNeeds()) != "" && refresh()
					send(&pb.WorkerMessage{Kind: &pb.WorkerMessage_TaskResult{TaskResult: result}})
					if late {
						w.OffersChanged()
					}
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

// Observed returns this node's address as its coordinator sees it, while it
// is connected and that is a global address, and otherwise the zero
// address. Behind a VPN it is the VPN's: it says where the node's traffic
// comes from, not where the machine is, and is for display only.
func (w *Worker) Observed() netip.Addr {
	if seen := w.observed.Load(); seen != nil {
		return *seen
	}
	return netip.Addr{}
}

// dialled is the address of the other end of a stream's connection, as
// this node reached it, or nothing if it has none: one carried over a
// tunnel or a relay has no address of its own that it could give.
func dialled(ctx context.Context) string {
	to, ok := peer.FromContext(ctx)
	if !ok {
		return ""
	}
	host, _, err := net.SplitHostPort(to.Addr.String())
	if err != nil {
		return ""
	}
	return host
}

func (w *Worker) swarmChanged(fingerprint string) {
	if w.OnSwarm != nil {
		w.OnSwarm(fingerprint)
	}
}

func (w *Worker) execute(ctx context.Context, a *pb.TaskAssignment) (result *pb.TaskResult) {
	result = &pb.TaskResult{TaskId: a.GetTaskId(), Attempt: a.GetAttempt()}
	began := time.Now()
	// Each task the worker is given shows in its own log as it ends, so
	// that its owner sees what their machine is doing for the pool.
	defer func() { w.logOutcome(ctx, a, result, time.Since(began)) }()
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
	// The coordinator may be anyone's: a task it gives this worker for
	// having something the worker does not have is refused, not run.
	if lacks := w.lacking(a.GetNeeds()); lacks != "" {
		fail(fmt.Sprintf("this worker does not have %s, which the task's job needs", lacks))
		return result
	}
	started := []any{"job", a.GetJobId(), "task", a.GetTaskId(), "workload", a.GetWorkload(), "attempt", a.GetAttempt()}
	// The model a task asks for is named, but not for a private job, whose
	// owner may not want even that in a worker's log.
	if needy, ok := workload.(runtime.Needy); ok && !private(a) {
		if model := runtime.ModelNeeded(needy.Needs(a.GetPayload())); model != "" {
			started = append(started, "model", model)
		}
	}
	w.Log.Info("task started", started...)
	if w.Limit != nil {
		if !w.Limit.Acquire(ctx, w.Pool) {
			fail(ctx.Err())
			return result
		}
		defer w.Limit.Release(w.Pool)
	}
	touched := runtime.Record(w.Blobs)
	// A job that is not private opens no sealed blob: it has no key to one.
	blobs := runtime.Open(touched)
	if private(a) {
		// A private job: what the task stores is sealed with the job's own
		// key, and sealed inputs are opened with the keys given for them.
		ring, own, err := keyring(a)
		if err != nil {
			fail(err)
			return result
		}
		blobs = runtime.Sealed(touched, ring, own)
	}
	// A model task says what it ran with before it asks its server
	// anything, so that goes with the result however the task ends.
	ctx = runtime.WithModelUse(ctx, func(use runtime.ModelUse) {
		result.Used = &pb.ModelUse{
			Server: use.Server, Version: use.Version,
			Model: use.Model, Digest: use.Digest, Quantization: use.Quantization,
			Options: use.Options,
		}
	})
	// What the task stores on the coordinator is stored for this attempt,
	// which the coordinator lets go of when the attempt ends unless the
	// result names it.
	ctx = blobclient.ForAttempt(ctx, a.GetTaskId(), a.GetAttempt())
	output, err := workload.Execute(ctx, blobs, a.GetPayload())
	if err != nil {
		fail(err)
		return result
	}
	result.ReadBlobs, result.WrittenBlobs = touched.Read(), touched.Written()
	result.Outcome = &pb.TaskResult_Output{Output: output}
	return result
}

// logOutcome says in the worker's log how a task it was given ended.
func (w *Worker) logOutcome(ctx context.Context, a *pb.TaskAssignment, result *pb.TaskResult, took time.Duration) {
	said := []any{"job", a.GetJobId(), "task", a.GetTaskId(), "took", took.Round(time.Millisecond).String()}
	switch {
	case result.GetError() == "":
		w.Log.Info("task done", append(said, "output_bytes", len(result.GetOutput()))...)
	case ctx.Err() != nil:
		w.Log.Info("task stopped", said...)
	default:
		w.Log.Warn("task failed", append(said, "error", result.GetError())...)
	}
}

func (w *Worker) capabilities() *pb.NodeCapabilities {
	hostname, _ := os.Hostname()
	capabilities := &pb.NodeCapabilities{
		Hostname:  hostname,
		Os:        goruntime.GOOS,
		Arch:      goruntime.GOARCH,
		CpuCores:  uint32(goruntime.NumCPU()),
		TaskSlots: uint32(w.Slots),
		Workloads: w.Workloads.Names(),

		CpuModel: w.Hardware.CPUModel, MemoryBytes: w.Hardware.MemoryBytes,
		Labels: w.labels(),
	}
	for _, gpu := range w.Hardware.GPUs {
		capabilities.Gpus = append(capabilities.Gpus, &pb.Gpu{Name: gpu.Name, MemoryBytes: gpu.MemoryBytes})
	}
	return capabilities
}

// private reports whether a task is of a private job: one that comes with
// keys, of either kind.
func private(a *pb.TaskAssignment) bool {
	return len(a.GetKeys()) > 0 || len(a.GetKey()) > 0
}

// keyring makes of the keys a task came with what opens its sealed inputs,
// and returns with it the job's own key, which is the first of them and
// seals what the task stores.
//
// A task that comes with a sealing key whole and no others is from a
// coordinator from before keys were derived, which expects what the task
// stores to be sealed with that key itself. That is no longer done, so the
// task is refused: run as though it had no key, it would store unsealed
// what it was to seal.
func keyring(a *pb.TaskAssignment) (*sealed.Ring, sealed.Grant, error) {
	if len(a.GetKeys()) == 0 {
		return nil, sealed.Grant{}, errors.New("the task came with a sealing key whole and no key of its job's own, as a coordinator from before each job had one gives it; this node does not seal with a whole key, so the coordinator must be brought up to date")
	}
	grants := make([]sealed.Grant, 0, len(a.GetKeys()))
	for _, given := range a.GetKeys() {
		if len(given.GetId()) != len(sealed.KeyID{}) || len(given.GetKey()) != sealed.KeySize {
			return nil, sealed.Grant{}, fmt.Errorf("the task came with a key of %d bytes whose ID is of %d, which is not a key", len(given.GetKey()), len(given.GetId()))
		}
		grants = append(grants, sealed.Grant{ID: sealed.KeyID(given.GetId()), Key: sealed.Key(given.GetKey())})
	}
	ring := sealed.NewRing(grants...)
	switch len(a.GetKey()) {
	case 0:
	case sealed.KeySize:
		whole := sealed.Key(a.GetKey())
		ring.Whole = &whole
	default:
		return nil, sealed.Grant{}, fmt.Errorf("the task came with a key of %d bytes, which is not a key", len(a.GetKey()))
	}
	return ring, grants[0], nil
}

// lacking returns the first of needs that the worker has not offered, or
// "" if it has them all. Its own name is the coordinator's to give it, and
// is not checked.
func (w *Worker) lacking(needs []string) string {
	offered := w.offered.Load()
	if offered == nil {
		labels := w.labels()
		offered = &labels
	}
	for _, need := range needs {
		if !strings.HasPrefix(need, runtime.WorkerLabel) && !slices.Contains(*offered, need) {
			return need
		}
	}
	return ""
}

// OffersChanged tells the worker that what it serves may have changed, as
// when its owner fetches or removes a model, or changes which are lent. It
// asks its workloads again and tells its coordinator if anything differs.
// It never waits, and word that comes while the worker is still acting on
// earlier word is acted on once.
func (w *Worker) OffersChanged() {
	select {
	case w.changes() <- struct{}{}:
	default:
	}
}

func (w *Worker) changes() chan struct{} {
	w.changedOnce.Do(func() { w.changed = make(chan struct{}, 1) })
	return w.changed
}

// storeOffered keeps a private snapshot. The session replaces its offered
// slice as labels change, while task execution may read it concurrently.
func (w *Worker) storeOffered(labels []string) {
	snapshot := slices.Clone(labels)
	w.offered.Store(&snapshot)
}

// labels is what the worker has that a job may ask for by name: itself,
// and whatever its workloads offer. Who it is the coordinator decides for
// itself, from the key the worker connected with; the name is sent for a
// coordinator from before it did.
func (w *Worker) labels() []string { return w.labelsWithin(context.Background()) }

func (w *Worker) labelsWithin(ctx context.Context) []string {
	return append([]string{runtime.WorkerLabel + w.Name, runtime.SealingLabel}, w.Workloads.Offers(ctx)...)
}

// offersWait is the longest the worker waits for its workloads to say what
// they offer, when asked on an event, before it gives up until the next.
// A local model server answers in milliseconds. Tests shorten it.
var offersWait = time.Second

// updateGap is the least time between two updates a worker sends of how a
// running task is getting on, so that a task that logs in a tight loop
// costs its coordinator no more than a few messages a second.
const updateGap = 250 * time.Millisecond

// passOn calls pass when changed says something has, until done ends. A
// change after a quiet spell is passed on at once; one sooner than gap after
// the last pass waits out the rest of it, and those that come in the
// meantime go with it.
func passOn(changed, done <-chan struct{}, gap time.Duration, pass func()) {
	var last time.Time
	var due <-chan time.Time
	for {
		select {
		case <-changed:
			if due != nil {
				continue
			}
			if wait := gap - time.Since(last); wait > 0 {
				due = time.After(wait)
				continue
			}
		case <-due:
			due = nil
		case <-done:
			return
		}
		pass()
		last = time.Now()
	}
}

// maxLogLine is the longest line of a task's log that is passed on whole.
const maxLogLine = 4 << 10

// reports collects what a running task says of itself until it is passed on
// to the coordinator. changed coalesces bursts without losing the latest
// progress or any non-private log lines.
type reports struct {
	mu       sync.Mutex
	progress float64
	lines    []string
	fresh    bool
	changed  chan struct{}
	// private says the task is of a private job. What such a task logs may
	// be the data the job seals, a container's every printed line for one,
	// and a log is passed on and kept unsealed: so it is not passed on.
	// withheld says that this has been said.
	private, withheld bool
}

// withheldLine is what stands in a private job's log for what its task
// would have logged.
const withheldLine = "this job is private, so what its task logs is not passed on"

func (r *reports) Progress(done float64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.progress, r.fresh = done, true
	r.notifyLocked()
}

func (r *reports) Log(line string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.private {
		if !r.withheld {
			r.withheld, r.fresh = true, true
			r.lines = append(r.lines, withheldLine)
			r.notifyLocked()
		}
		return
	}
	if len(line) > maxLogLine {
		line = line[:maxLogLine] + " [cut short]"
	}
	r.lines, r.fresh = append(r.lines, line), true
	r.notifyLocked()
}

func (r *reports) notifyLocked() {
	select {
	case r.changed <- struct{}{}:
	default:
	}
}

// take returns what has been said since it was last called, as an update for
// the coordinator, or nil if nothing has.
func (r *reports) take(a *pb.TaskAssignment) *pb.TaskUpdate {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.fresh {
		return nil
	}
	update := &pb.TaskUpdate{TaskId: a.GetTaskId(), Attempt: a.GetAttempt(), Progress: r.progress, Log: r.lines}
	r.lines, r.fresh = nil, false
	return update
}

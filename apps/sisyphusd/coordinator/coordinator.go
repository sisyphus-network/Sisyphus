// Package coordinator implements the coordinator role of sisyphusd: it
// accepts jobs, splits them into tasks, assigns tasks to connected workers,
// retries failures and aggregates results. All state is in memory.
package coordinator

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"log/slog"
	"slices"
	"sort"
	"sync"
	"time"

	"github.com/ipfs/go-cid"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/excho0/Sisyphus/apps/sisyphusd/access"
	jobmodel "github.com/excho0/Sisyphus/packages/job-model"
	pb "github.com/excho0/Sisyphus/packages/protocol/sisyphus/v1"
	"github.com/excho0/Sisyphus/packages/runtime"
)

const (
	// maxAttempts is how many times a task is handed out before its job fails.
	// Losing the worker mid-task counts as an attempt.
	maxAttempts = 3
	// maxTasksPerJob bounds how finely a submitter can ask to split a job.
	maxTasksPerJob = 10_000
)

// Store is the blob store a coordinator works against: workloads split from
// it and aggregate into it, and the coordinator pins a job's blobs in it for
// as long as they are needed. A storage.Store is one.
type Store interface {
	runtime.Blobs
	Pin(ctx context.Context, owner string, expires time.Time, cids ...cid.Cid) error
	Unpin(owner string, cids ...cid.Cid) error
}

type Config struct {
	// ID names this coordinator to its workers.
	ID        string
	Workloads *runtime.Registry
	Store     Store
	// Retain is how long a job's inputs and results stay pinned after it
	// finishes.
	Retain time.Duration
	Log    *slog.Logger
}

type Coordinator struct {
	pb.UnimplementedCoordinatorServiceServer

	id        string
	workloads *runtime.Registry
	store     Store
	retain    time.Duration
	log       *slog.Logger

	// ctx bounds the aggregations still running when the coordinator closes.
	ctx         context.Context
	cancel      context.CancelFunc
	aggregating sync.WaitGroup

	mu      sync.Mutex
	jobs    map[string]*jobmodel.Job
	active  []*jobmodel.Job // unfinished jobs in submission order
	workers map[string]*worker
	// changed holds, per job, a channel that is closed and replaced every
	// time the job changes. Watchers wait on it.
	changed map[string]chan struct{}
}

type worker struct {
	id string
	// name is the label the worker gave itself.
	name string
	// removed is closed, once, to make the worker's connection end.
	removed chan struct{}
	remove  sync.Once

	capabilities *pb.NodeCapabilities
	connectedAt  time.Time
	lastSeen     time.Time
	// send is drained onto the worker's stream by its Connect call. It has
	// room for a Welcome plus one assignment per slot, so sends made while
	// holding the coordinator lock never block.
	send    chan *pb.CoordinatorMessage
	running map[string]assignment // by task ID
}

type assignment struct {
	job  *jobmodel.Job
	task *jobmodel.Task
}

func New(cfg Config) *Coordinator {
	ctx, cancel := context.WithCancel(context.Background())
	return &Coordinator{
		id:        cfg.ID,
		workloads: cfg.Workloads,
		store:     cfg.Store,
		retain:    cfg.Retain,
		log:       cfg.Log,
		ctx:       ctx,
		cancel:    cancel,
		jobs:      make(map[string]*jobmodel.Job),
		workers:   make(map[string]*worker),
		changed:   make(map[string]chan struct{}),
	}
}

// Close stops work the coordinator is doing on its own account and waits for
// it to end. Call it before closing the blob store.
func (c *Coordinator) Close() {
	c.cancel()
	c.aggregating.Wait()
}

// Submit validates and splits a job, queues it and returns its initial state.
func (c *Coordinator) Submit(ctx context.Context, spec *pb.JobSpec) (*pb.Job, error) {
	workload, err := c.workloads.Get(spec.GetWorkload())
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	if spec.GetMaxTasks() > maxTasksPerJob {
		return nil, status.Errorf(codes.InvalidArgument, "max_tasks exceeds %d", maxTasksPerJob)
	}
	var mode jobmodel.Mode
	switch spec.GetMode() {
	case pb.ScheduleMode_SCHEDULE_MODE_UNSPECIFIED, pb.ScheduleMode_SCHEDULE_MODE_DISTRIBUTED:
		mode = jobmodel.Distributed
	case pb.ScheduleMode_SCHEDULE_MODE_FULL_WORKER:
		mode = jobmodel.FullWorker
	default:
		return nil, status.Errorf(codes.InvalidArgument, "unknown schedule mode %d", spec.GetMode())
	}

	parts := 1
	if mode == jobmodel.Distributed {
		parts = int(spec.GetMaxTasks())
		if parts == 0 {
			c.mu.Lock()
			parts = max(c.slotsLocked(workload.Name()), 1)
			c.mu.Unlock()
		}
	}
	// Splitting may read stored data, so it runs without the lock.
	touched := runtime.Record(c.store)
	payloads, err := workload.Split(ctx, touched, spec.GetParams(), parts)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	if len(payloads) == 0 {
		return nil, status.Error(codes.Internal, "workload split the job into no tasks")
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	job := jobmodel.New(newID(), workload.Name(), spec.GetParams(), mode, int(spec.GetMaxTasks()), payloads, time.Now())
	c.jobs[job.ID] = job
	c.active = append(c.active, job)
	c.changed[job.ID] = make(chan struct{})
	// Hold the job's inputs until it is over, however long that takes.
	job.NoteRead(touched.Read()...)
	c.pin(job, time.Time{}, touched.Read())
	c.log.Info("job submitted", "job", job.ID, "workload", job.Workload, "tasks", len(job.Tasks))

	c.scheduleLocked()
	return job.ToProto(), nil
}

func (c *Coordinator) Get(jobID string) (*pb.Job, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	job, ok := c.jobs[jobID]
	if !ok {
		return nil, status.Errorf(codes.NotFound, "job %q not found", jobID)
	}
	return job.ToProto(), nil
}

// Watch calls fn with the job's current state and again after every change,
// returning nil once it has delivered a terminal state.
func (c *Coordinator) Watch(ctx context.Context, jobID string, fn func(*pb.Job) error) error {
	for {
		c.mu.Lock()
		job, ok := c.jobs[jobID]
		if !ok {
			c.mu.Unlock()
			return status.Errorf(codes.NotFound, "job %q not found", jobID)
		}
		snapshot, terminal, changed := job.ToProto(), job.Terminal(), c.changed[jobID]
		c.mu.Unlock()

		if err := fn(snapshot); err != nil {
			return err
		}
		if terminal {
			return nil
		}
		select {
		case <-changed:
		case <-ctx.Done():
			return status.FromContextError(ctx.Err()).Err()
		}
	}
}

// Nodes lists connected workers, ordered by node ID.
func (c *Coordinator) Nodes() []*pb.NodeInfo {
	c.mu.Lock()
	defer c.mu.Unlock()
	nodes := make([]*pb.NodeInfo, 0, len(c.workers))
	for _, w := range c.workers {
		nodes = append(nodes, &pb.NodeInfo{
			NodeId:       w.id,
			Name:         w.name,
			Capabilities: w.capabilities,
			RunningTasks: uint32(len(w.running)),
			ConnectedAt:  timestamppb.New(w.connectedAt),
			LastSeenAt:   timestamppb.New(w.lastSeen),
		})
	}
	sort.Slice(nodes, func(i, j int) bool { return nodes[i].NodeId < nodes[j].NodeId })
	return nodes
}

// Connect serves one worker for as long as its stream stays open. The
// worker's ID is that of the key it connected with, as established by the
// access package; nothing the worker sends can change it.
func (c *Coordinator) Connect(stream grpc.BidiStreamingServer[pb.WorkerMessage, pb.CoordinatorMessage]) error {
	id := access.Caller(stream.Context())
	if id == "" {
		return status.Error(codes.Unauthenticated, "the caller has not been identified")
	}
	first, err := stream.Recv()
	if err != nil {
		return err
	}
	hello := first.GetHello()
	if hello == nil {
		return status.Error(codes.InvalidArgument, "first message must be a Hello")
	}
	capabilities := hello.GetCapabilities()
	if capabilities == nil {
		capabilities = &pb.NodeCapabilities{}
	}
	if capabilities.GetTaskSlots() > maxTasksPerJob {
		return status.Errorf(codes.InvalidArgument, "task_slots exceeds %d", maxTasksPerJob)
	}

	now := time.Now()
	w := &worker{
		id:           id,
		name:         hello.GetName(),
		capabilities: capabilities,
		connectedAt:  now,
		lastSeen:     now,
		send:         make(chan *pb.CoordinatorMessage, capabilities.GetTaskSlots()+1),
		running:      make(map[string]assignment),
		removed:      make(chan struct{}),
	}

	c.mu.Lock()
	if _, taken := c.workers[w.id]; taken {
		c.mu.Unlock()
		return status.Errorf(codes.AlreadyExists, "node %s is already connected", w.id)
	}
	c.workers[w.id] = w
	w.send <- &pb.CoordinatorMessage{Kind: &pb.CoordinatorMessage_Welcome{Welcome: &pb.Welcome{CoordinatorId: c.id}}}
	c.scheduleLocked()
	c.mu.Unlock()
	c.log.Info("worker connected", "node", w.id, "name", w.name, "slots", capabilities.GetTaskSlots())

	// One goroutine writes to the stream and one reads from it, so that this
	// one can also act on the worker being removed. A failed Send means the
	// stream is broken, which the reader also sees, so the writer needs no
	// way to report its error.
	stop, stopped := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(stopped)
		for {
			select {
			case msg := <-w.send:
				stream.Send(msg)
			case <-stop:
				return
			}
		}
	}()
	broken := make(chan error, 1)
	go func() {
		for {
			msg, err := stream.Recv()
			if err != nil {
				broken <- err
				return
			}
			switch kind := msg.GetKind().(type) {
			case *pb.WorkerMessage_Heartbeat:
				c.mu.Lock()
				w.lastSeen = time.Now()
				c.mu.Unlock()
			case *pb.WorkerMessage_TaskResult:
				c.handleResult(w, kind.TaskResult)
			}
		}
	}()
	defer func() {
		c.disconnect(w)
		close(stop)
		<-stopped
	}()

	select {
	case err := <-broken:
		if errors.Is(err, io.EOF) {
			return nil // the worker closed its side cleanly
		}
		return err
	case <-w.removed:
		return status.Errorf(codes.PermissionDenied, "node %s has been removed from the pool", w.id)
	}
}

// Remove disconnects the worker with the given ID, if it is connected. Its
// running tasks are handed to other workers.
func (c *Coordinator) Remove(id string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if w, ok := c.workers[id]; ok {
		w.remove.Do(func() { close(w.removed) })
	}
}

func (c *Coordinator) disconnect(w *worker) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.workers, w.id)
	now := time.Now()
	for _, a := range w.running {
		if a.job.Terminal() {
			continue
		}
		a.job.Fail(a.task, "worker "+w.id+" disconnected", maxAttempts, now)
		c.settleLocked(a.job)
		c.notifyLocked(a.job)
	}
	c.log.Info("worker disconnected", "node", w.id, "lost_tasks", len(w.running))
	// The reader may still deliver a result that was already on its way.
	// With nothing recorded as running, it is dropped like any other result
	// for a task the worker does not hold.
	w.running = make(map[string]assignment)
	c.scheduleLocked()
}

func (c *Coordinator) handleResult(w *worker, result *pb.TaskResult) {
	c.mu.Lock()
	defer c.mu.Unlock()
	w.lastSeen = time.Now()

	a, ok := w.running[result.GetTaskId()]
	if !ok || uint32(a.task.Attempt) != result.GetAttempt() {
		c.log.Warn("dropping result for a task this worker is not running", "node", w.id, "task", result.GetTaskId())
		return
	}
	delete(w.running, a.task.ID)
	defer c.scheduleLocked()

	// The job may already have failed because of another task.
	if a.job.Terminal() {
		return
	}
	now := time.Now()
	switch outcome := result.GetOutcome().(type) {
	case *pb.TaskResult_Output:
		// What the task stored must outlive the hour a new blob is kept
		// for, since the job may run longer than that.
		a.job.NoteRead(result.GetReadBlobs()...)
		a.job.NoteTaskOutput(result.GetWrittenBlobs()...)
		c.pin(a.job, time.Time{}, result.GetWrittenBlobs())
		if a.job.Succeed(a.task, outcome.Output) {
			// Succeed reports the last task only once, so each job is
			// aggregated once.
			c.aggregating.Add(1)
			go c.aggregate(a.job)
		}
	case *pb.TaskResult_Error:
		c.log.Warn("task attempt failed", "task", a.task.ID, "node", w.id, "attempt", a.task.Attempt, "error", outcome.Error)
		a.job.Fail(a.task, outcome.Error, maxAttempts, now)
	default:
		a.job.Fail(a.task, "worker reported no outcome", maxAttempts, now)
	}
	c.settleLocked(a.job)
	c.notifyLocked(a.job)
}

// aggregate combines a job's task outputs into its result and finishes it.
// Aggregating may read and write stored data, so it runs without the lock;
// until it is done the job stays running with every task succeeded.
func (c *Coordinator) aggregate(job *jobmodel.Job) {
	defer c.aggregating.Done()

	c.mu.Lock()
	outputs := job.Outputs()
	c.mu.Unlock()

	workload, err := c.workloads.Get(job.Workload)
	var result []byte
	touched := runtime.Record(c.store)
	if err == nil {
		result, err = workload.Aggregate(c.ctx, touched, outputs)
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now()
	job.NoteRead(touched.Read()...)
	job.NoteResult(touched.Written()...)
	job.Finish(result, err, now)
	c.settleLocked(job)
	c.log.Info("job finished", "job", job.ID, "state", job.State.String(), "took", now.Sub(job.CreatedAt).String())
	c.notifyLocked(job)
	c.scheduleLocked()
}

// settleLocked sets how long a job's blobs are kept once it is over: its
// inputs and results for the retention period, and what only passed between
// its tasks no longer. It does nothing while the job is still going.
func (c *Coordinator) settleLocked(job *jobmodel.Job) {
	if !job.Terminal() {
		return
	}
	keep := append(job.InputBlobs(), job.OutputBlobs()...)
	c.pin(job, job.FinishedAt.Add(c.retain), keep)
	if err := c.store.Unpin(owner(job), decode(job.IntermediateBlobs())...); err != nil {
		c.log.Warn("could not release a job's intermediate blobs", "job", job.ID, "error", err)
	}
}

// pin pins blobs on a job's behalf. Keeping data is best effort: a job is
// not failed because its blobs could not be pinned, which a worker naming a
// blob the coordinator does not hold is enough to cause.
func (c *Coordinator) pin(job *jobmodel.Job, expires time.Time, cids []string) {
	if err := c.store.Pin(c.ctx, owner(job), expires, decode(cids)...); err != nil {
		c.log.Warn("could not pin a job's blobs", "job", job.ID, "error", err)
	}
}

// owner is the name a job's pins are held under.
func owner(job *jobmodel.Job) string {
	return "job:" + job.ID
}

// decode parses CIDs, dropping any that are malformed. They can come from
// workers, which may send anything.
func decode(cids []string) []cid.Cid {
	decoded := make([]cid.Cid, 0, len(cids))
	for _, s := range cids {
		if c, err := cid.Decode(s); err == nil {
			decoded = append(decoded, c)
		}
	}
	return decoded
}

// scheduleLocked hands pending tasks to workers with free slots, oldest job
// first.
func (c *Coordinator) scheduleLocked() {
	c.active = slices.DeleteFunc(c.active, func(job *jobmodel.Job) bool { return job.Terminal() })
	for _, job := range c.active {
		for _, task := range job.Assignable() {
			w := c.pickWorkerLocked(job.Workload)
			if w == nil {
				break
			}
			job.Start(task, w.id, w.name)
			w.running[task.ID] = assignment{job: job, task: task}
			w.send <- &pb.CoordinatorMessage{Kind: &pb.CoordinatorMessage_Assignment{Assignment: &pb.TaskAssignment{
				TaskId:   task.ID,
				JobId:    job.ID,
				Attempt:  uint32(task.Attempt),
				Workload: job.Workload,
				Payload:  task.Payload,
			}}}
			c.notifyLocked(job)
		}
	}
}

// pickWorkerLocked returns the worker with the most free slots that supports
// the workload, or nil if none has a free slot.
func (c *Coordinator) pickWorkerLocked(workload string) *worker {
	var best *worker
	bestFree := 0
	for _, w := range c.workers {
		if !slices.Contains(w.capabilities.GetWorkloads(), workload) {
			continue
		}
		free := int(w.capabilities.GetTaskSlots()) - len(w.running)
		if free > bestFree || (free == bestFree && best != nil && w.id < best.id) {
			best, bestFree = w, free
		}
	}
	return best
}

// slotsLocked counts task slots across connected workers that support the
// workload.
func (c *Coordinator) slotsLocked(workload string) int {
	total := 0
	for _, w := range c.workers {
		if slices.Contains(w.capabilities.GetWorkloads(), workload) {
			total += int(w.capabilities.GetTaskSlots())
		}
	}
	return total
}

func (c *Coordinator) notifyLocked(job *jobmodel.Job) {
	close(c.changed[job.ID])
	c.changed[job.ID] = make(chan struct{})
}

func newID() string {
	var b [8]byte
	rand.Read(b[:]) // never fails; see crypto/rand
	return hex.EncodeToString(b[:])
}

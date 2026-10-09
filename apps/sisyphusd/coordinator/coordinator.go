// Package coordinator implements the coordinator role of sisyphusd: it
// accepts jobs, splits them into tasks, assigns tasks to connected workers,
// retries failures and aggregates results. All state is in memory.
package coordinator

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/ipfs/go-cid"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/sisyphus-network/Sisyphus/apps/sisyphusd/access"
	jobmodel "github.com/sisyphus-network/Sisyphus/packages/job-model"
	pb "github.com/sisyphus-network/Sisyphus/packages/protocol/sisyphus/v1"
	"github.com/sisyphus-network/Sisyphus/packages/runtime"
	"github.com/sisyphus-network/Sisyphus/packages/sealed"
)

const (
	// maxAttempts is how many times a task is handed out before its job fails.
	// Losing the worker mid-task counts as an attempt.
	maxAttempts = 3
	// maxTasksPerJob bounds how finely a submitter can ask to split a job.
	maxTasksPerJob = 10_000
)

// Store is the blob store a coordinator works against: workloads split from
// it and aggregate into it, the coordinator pins a job's blobs in it for as
// long as they are needed, and writes the job's record into it when the job
// is over. A storage.Store is one.
type Store interface {
	runtime.Blobs
	Pin(ctx context.Context, owner string, expires time.Time, cids ...cid.Cid) error
	Unpin(owner string, cids ...cid.Cid) error
	PutNodes(ctx context.Context, root []byte, linked ...[]byte) (cid.Cid, error)
	GetNode(ctx context.Context, c cid.Cid) ([]byte, error)
}

// Journal keeps jobs somewhere that outlasts the coordinator. A nodedb.DB is
// one.
type Journal interface {
	// SaveJob writes what has changed in a job since it was last saved.
	SaveJob(job *jobmodel.Job) error
	// LoadJobs returns every saved job, oldest first.
	LoadJobs() ([]*jobmodel.Job, error)
	// DeleteJobs forgets the jobs with the given IDs.
	DeleteJobs(ids []string) error
	// SaveEvent records one thing that happened to a job, and LoadEvents
	// returns all that has, in order.
	SaveEvent(jobID string, e jobmodel.Event) error
	LoadEvents(jobID string) ([]jobmodel.Event, error)
}

// noJournal is the journal of a coordinator that keeps its jobs in memory
// only.
type noJournal struct{}

func (noJournal) SaveJob(*jobmodel.Job) error            { return nil }
func (noJournal) LoadJobs() ([]*jobmodel.Job, error)     { return nil, nil }
func (noJournal) DeleteJobs([]string) error              { return nil }
func (noJournal) SaveEvent(string, jobmodel.Event) error { return nil }
func (noJournal) LoadEvents(string) ([]jobmodel.Event, error) {
	return nil, nil
}

type Config struct {
	// ID names this coordinator to its workers.
	ID        string
	Workloads *runtime.Registry
	Store     Store
	// Journal, if set, is where jobs are kept across restarts; see Recover.
	Journal Journal
	// Retain is how long a job's inputs and results stay pinned after it
	// finishes.
	Retain time.Duration
	// KeepJobs is how long a finished job stays on record; see Prune. Zero
	// keeps them for good.
	KeepJobs time.Duration
	Log      *slog.Logger
}

type Coordinator struct {
	pb.UnimplementedCoordinatorServiceServer

	id        string
	workloads *runtime.Registry
	store     Store
	journal   Journal
	retain    time.Duration
	keepJobs  time.Duration
	log       *slog.Logger

	// ctx bounds the aggregations still running when the coordinator closes.
	ctx         context.Context
	cancel      context.CancelFunc
	aggregating sync.WaitGroup

	mu sync.Mutex
	// swarmFingerprint identifies the current key of the pool's private
	// IPFS network, if it has one.
	swarmFingerprint string
	jobs             map[string]*jobmodel.Job
	active           []*jobmodel.Job // unfinished jobs in submission order
	workers          map[string]*worker
	// changed holds, per job, a channel that is closed and replaced every
	// time the job changes or something happens to it. Watchers wait on it.
	changed map[string]chan struct{}
	// events holds, per job, what has happened to it, and seqs the number
	// of the last event: more than the events held, if the oldest of a very
	// long list have been let go.
	events map[string][]jobmodel.Event
	seqs   map[string]uint64
}

// maxEvents is how many of a job's events are kept to hand.
const maxEvents = 5000

type worker struct {
	id string
	// name is the label the worker gave itself.
	name string
	// serveAddress is where other workers can fetch blobs from this one, if
	// it serves them, and holds the CIDs of the blobs its tasks have opened
	// or stored, which it is therefore likely to have.
	serveAddress string
	holds        map[string]struct{}
	// relayAddresses are where the worker relays at, if it does, and
	// relayed what it last said it had carried.
	relayAddresses                   []string
	relayedConnections, relayedBytes uint64
	// removed is closed, once, to make the worker's connection end.
	removed chan struct{}
	remove  sync.Once

	capabilities *pb.NodeCapabilities
	connectedAt  time.Time
	lastSeen     time.Time
	// send holds the messages waiting to go out on the worker's stream.
	send    *outbox
	running map[string]assignment // by task ID
}

// outbox is the queue of messages waiting to be written to a worker's
// stream. Adding to it never blocks, which lets the coordinator queue a
// message while holding its lock, and it never drops anything.
type outbox struct {
	mu      sync.Mutex
	waiting sync.Cond
	queue   []*pb.CoordinatorMessage
	closed  bool
}

func newOutbox() *outbox {
	o := &outbox{}
	o.waiting.L = &o.mu
	return o
}

func (o *outbox) add(msg *pb.CoordinatorMessage) {
	o.mu.Lock()
	o.queue = append(o.queue, msg)
	o.mu.Unlock()
	o.waiting.Signal()
}

// next returns the oldest message, waiting for one if need be. It reports
// false once the outbox has been closed.
func (o *outbox) next() (*pb.CoordinatorMessage, bool) {
	o.mu.Lock()
	defer o.mu.Unlock()
	for len(o.queue) == 0 && !o.closed {
		o.waiting.Wait()
	}
	if o.closed {
		return nil, false
	}
	msg := o.queue[0]
	o.queue = o.queue[1:]
	return msg, true
}

func (o *outbox) close() {
	o.mu.Lock()
	o.closed = true
	o.mu.Unlock()
	o.waiting.Signal()
}

type assignment struct {
	job  *jobmodel.Job
	task *jobmodel.Task
}

func New(cfg Config) *Coordinator {
	ctx, cancel := context.WithCancel(context.Background())
	if cfg.Journal == nil {
		cfg.Journal = noJournal{}
	}
	c := &Coordinator{
		id:        cfg.ID,
		workloads: cfg.Workloads,
		store:     cfg.Store,
		journal:   cfg.Journal,
		retain:    cfg.Retain,
		keepJobs:  cfg.KeepJobs,
		log:       cfg.Log,
		ctx:       ctx,
		cancel:    cancel,
		jobs:      make(map[string]*jobmodel.Job),
		workers:   make(map[string]*worker),
		changed:   make(map[string]chan struct{}),
		events:    make(map[string][]jobmodel.Event),
		seqs:      make(map[string]uint64),
	}
	// Attempts that have run past their time are looked for once a second.
	c.aggregating.Add(1)
	go func() {
		defer c.aggregating.Done()
		tick := time.NewTicker(time.Second)
		defer tick.Stop()
		for {
			select {
			case now := <-tick.C:
				c.Expire(now)
			case <-c.ctx.Done():
				return
			}
		}
	}()
	return c
}

// recordLocked notes something that happened to a job, for whoever is
// following it now and whoever asks later.
func (c *Coordinator) recordLocked(job *jobmodel.Job, kind string, task int, node, text string) {
	c.seqs[job.ID]++
	e := jobmodel.Event{Seq: c.seqs[job.ID], At: time.Now(), Kind: kind, Task: task, Node: node, Text: text}
	held := append(c.events[job.ID], e)
	c.events[job.ID] = held[max(len(held)-maxEvents, 0):]
	if err := c.journal.SaveEvent(job.ID, e); err != nil {
		c.log.Warn("could not save a job's event", "job", job.ID, "error", err)
	}
	c.wakeLocked(job)
}

// The kinds of thing that happen to a job.
const (
	eventSubmitted     = "submitted"
	eventResumed       = "resumed"
	eventTaskStarted   = "task-started"
	eventTaskSucceeded = "task-succeeded"
	eventTaskFailed    = "task-failed"
	eventTaskLost      = "task-lost"
	eventTaskTimedOut  = "task-timed-out"
	eventLog           = "log"
	whole              = -1 // the task index of an event about the job itself
)

// WatchEvents calls fn with each thing that has happened to a job after the
// event numbered after, in order, and goes on as more happens, returning nil
// once the job is over and everything has been delivered.
func (c *Coordinator) WatchEvents(ctx context.Context, jobID string, after uint64, fn func(*pb.JobEvent) error) error {
	for {
		c.mu.Lock()
		job, ok := c.jobs[jobID]
		if !ok {
			c.mu.Unlock()
			return status.Errorf(codes.NotFound, "job %q not found", jobID)
		}
		var fresh []jobmodel.Event
		for _, e := range c.events[jobID] {
			if e.Seq > after {
				fresh = append(fresh, e)
			}
		}
		over, changed := job.Terminal(), c.changed[jobID]
		c.mu.Unlock()

		for _, e := range fresh {
			if err := fn(e.ToProto()); err != nil {
				return err
			}
			after = e.Seq
		}
		if over {
			return nil
		}
		select {
		case <-changed:
		case <-ctx.Done():
			return status.FromContextError(ctx.Err()).Err()
		}
	}
}

// Cancel stops a job that is not over: its running tasks are told to stop
// and nothing more of it is handed out.
func (c *Coordinator) Cancel(jobID string) (*pb.Job, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	job, ok := c.jobs[jobID]
	if !ok {
		return nil, status.Errorf(codes.NotFound, "job %q not found", jobID)
	}
	if !job.Cancel(time.Now()) {
		return nil, status.Errorf(codes.FailedPrecondition, "job %q is already over: it %s", jobID, job.State)
	}
	c.settleLocked(job)
	c.log.Info("job cancelled", "job", job.ID)
	c.notifyLocked(job)
	c.scheduleLocked()
	return job.ToProto(), nil
}

// Expire fails the attempts that have, as of now, run longer than their
// job allows, and tells the workers running them to stop.
func (c *Coordinator) Expire(now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, w := range c.workers {
		for id, a := range w.running {
			if a.job.TaskTimeout <= 0 || now.Sub(a.task.StartedAt) <= a.job.TaskTimeout {
				continue
			}
			delete(w.running, id)
			w.send.add(cancelOf(a.task))
			reason := "timed out after " + a.job.TaskTimeout.String()
			c.recordLocked(a.job, eventTaskTimedOut, a.task.Index, w.name, reason)
			a.job.Fail(a.task, reason, maxAttempts, now)
			c.settleLocked(a.job)
			c.notifyLocked(a.job)
		}
	}
	c.scheduleLocked()
}

// cancelOf is the message that tells a worker to stop a task.
func cancelOf(task *jobmodel.Task) *pb.CoordinatorMessage {
	return &pb.CoordinatorMessage{Kind: &pb.CoordinatorMessage_Cancel{Cancel: &pb.TaskCancel{TaskId: task.ID, Attempt: uint32(task.Attempt)}}}
}

// stopTasksLocked tells every worker running a task of a job that is over to
// stop it, and takes the task off the worker's hands, so that its slot is
// free at once and whatever it still reports is ignored.
func (c *Coordinator) stopTasksLocked(job *jobmodel.Job) {
	for _, w := range c.workers {
		for id, a := range w.running {
			if a.job == job {
				delete(w.running, id)
				w.send.add(cancelOf(a.task))
			}
		}
	}
}

// handleUpdate takes in news of a task still running: how far along it is
// and what it has logged.
func (c *Coordinator) handleUpdate(w *worker, update *pb.TaskUpdate) {
	c.mu.Lock()
	defer c.mu.Unlock()
	a, ok := w.running[update.GetTaskId()]
	if !ok || uint32(a.task.Attempt) != update.GetAttempt() {
		return
	}
	a.task.Progress = min(max(update.GetProgress(), 0), 1)
	for _, line := range update.GetLog() {
		c.recordLocked(a.job, eventLog, a.task.Index, w.name, line)
	}
	c.wakeLocked(a.job)
}

// Recover takes up the jobs in the journal where an earlier coordinator
// left them, and returns how many of them were unfinished. Call it once,
// before the coordinator is given anything else to do.
//
// A task that was running goes back to wait for a worker: whoever had it
// has lost its connection, and anything it sends later is for an attempt
// this coordinator did not hand out. That does not count against the task.
func (c *Coordinator) Recover() (unfinished int, err error) {
	jobs, err := c.journal.LoadJobs()
	if err != nil {
		return 0, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, job := range jobs {
		job.Needs = c.workloads.Needs(job.Workload, job.Params)
		// What happened to it before comes back with it. A job whose events
		// cannot be read is still a job.
		events, err := c.journal.LoadEvents(job.ID)
		if err != nil {
			c.log.Warn("could not load a job's events", "job", job.ID, "error", err)
		}
		c.events[job.ID] = events[max(len(events)-maxEvents, 0):]
		if len(events) > 0 {
			c.seqs[job.ID] = events[len(events)-1].Seq
		}
		c.jobs[job.ID] = job
		c.changed[job.ID] = make(chan struct{})
	}
	// Every job is known before any is taken up, so that one which ends
	// here finds the jobs carrying out its steps.
	for _, job := range jobs {
		if job.Terminal() {
			continue
		}
		unfinished++
		c.recordLocked(job, eventResumed, whole, "", "the coordinator was restarted")
		if _, err := c.workloads.Get(job.Workload); err != nil {
			// Nobody is left who could split, run or combine it.
			job.Finish(nil, fmt.Errorf("the coordinator was restarted without this job's workload: %w", err), time.Now())
			c.settleLocked(job)
			c.saveLocked(job)
			continue
		}
		c.active = append(c.active, job)
		waiting := false
		for _, task := range job.Tasks {
			if task.State == jobmodel.Running {
				job.Requeue(task, "the coordinator was restarted")
			}
			waiting = waiting || task.State != jobmodel.Succeeded
		}
		c.saveLocked(job)
		if !waiting {
			// Every task had finished and only combining them was cut short.
			c.aggregating.Add(1)
			go c.aggregate(job)
		}
	}
	// A job whose tasks are jobs waits for no worker to take it up again.
	c.scheduleLocked()
	return unfinished, nil
}

// Prune forgets the jobs that finished more than the keeping period before
// now, and returns how many that was. Their stored data is not touched: how
// long that is kept is the retention period's business.
func (c *Coordinator) Prune(now time.Time) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	aged := func(job *jobmodel.Job) bool {
		return c.keepJobs > 0 && job.Terminal() && now.Sub(job.FinishedAt) > c.keepJobs
	}
	var old []string
	for id, job := range c.jobs {
		// A step is kept as long as the job it is a step of, whose record
		// links its own.
		parent, step := c.jobs[job.Parent]
		if aged(job) && (!step || aged(parent)) {
			old = append(old, id)
		}
	}
	if err := c.journal.DeleteJobs(old); err != nil {
		return 0, err
	}
	for _, id := range old {
		// A job's record is kept as long as the job is, and no longer.
		if err := c.store.Unpin(recordOwner(id), decode([]string{c.jobs[id].Record})...); err != nil {
			c.log.Warn("could not release a job's record", "job", id, "error", err)
		}
		delete(c.jobs, id)
		delete(c.changed, id)
		delete(c.events, id)
		delete(c.seqs, id)
	}
	return len(old), nil
}

// Holds reports whether owner is the name an unfinished job holds its pins
// under. Pins held open under any other job's name have nobody left to
// release them.
func (c *Coordinator) Holds(owner string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	job, ok := c.jobs[strings.TrimPrefix(owner, ownerPrefix)]
	return ok && strings.HasPrefix(owner, ownerPrefix) && !job.Terminal()
}

// saveLocked records a job's changes in the journal. A job is not stopped
// because it could not be recorded: it carries on in memory, and its unsaved
// changes are written with the next one.
func (c *Coordinator) saveLocked(job *jobmodel.Job) {
	if err := c.journal.SaveJob(job); err != nil {
		c.log.Warn("could not save a job; it will be lost if the coordinator stops now", "job", job.ID, "error", err)
	}
}

// Close stops work the coordinator is doing on its own account and waits for
// it to end. Call it before closing the blob store, and before cutting off
// the workers: a worker lost to a coordinator that has been closed is not
// held against the tasks it was running. Closing twice does no harm.
func (c *Coordinator) Close() {
	c.cancel()
	c.aggregating.Wait()
}

// Submit validates and splits a job, queues it and returns its initial state.
func (c *Coordinator) Submit(ctx context.Context, spec *pb.JobSpec) (*pb.Job, error) {
	return c.submit(ctx, spec, "", "", access.Caller(ctx))
}

// submit takes a job in. parent and step are set for a job that carries
// out a step of another, and submitter is the node the job came from, if
// it came from one.
func (c *Coordinator) submit(ctx context.Context, spec *pb.JobSpec, parent, step, submitter string) (*pb.Job, error) {
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
			parts = max(c.slotsLocked(workload.Name(), spec.GetMinMemoryBytes(), int(spec.GetMinGpus()), c.workloads.Needs(workload.Name(), spec.GetParams())), 1)
			c.mu.Unlock()
		}
	}
	if n := len(spec.GetKey()); n != 0 && n != sealed.KeySize {
		return nil, status.Errorf(codes.InvalidArgument, "a job's key must be %d bytes, not %d", sealed.KeySize, n)
	}
	// Splitting may read stored data, so it runs without the lock.
	touched := runtime.Record(c.store)
	payloads, err := workload.Split(ctx, withKey(touched, spec.GetKey()), spec.GetParams(), parts)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	if len(payloads) == 0 {
		return nil, status.Error(codes.Internal, "workload split the job into no tasks")
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	if of, is := c.jobs[parent]; is && of.Terminal() {
		// It ended while this step was being split. A step begun now
		// would be nobody's to stop, and no part of the job's record.
		return nil, status.Errorf(codes.Aborted, "job %q, which this would be a step of, is over", parent)
	}
	job := jobmodel.New(newID(), workload.Name(), spec.GetParams(), mode, int(spec.GetMaxTasks()), payloads, time.Now())
	job.Key, job.Private, job.Submitter = spec.GetKey(), len(spec.GetKey()) > 0, submitter
	job.TaskTimeout = time.Duration(spec.GetTaskTimeoutSeconds()) * time.Second
	job.MinMemory, job.MinGPUs = spec.GetMinMemoryBytes(), int(spec.GetMinGpus())
	job.Needs = c.workloads.Needs(job.Workload, job.Params)
	job.Parent, job.Step = parent, step
	job.NoteRead(touched.Read()...)
	// A job is accepted only once it is on record: its submitter is about
	// to be given an ID to ask after.
	if err := c.journal.SaveJob(job); err != nil {
		return nil, status.Errorf(codes.Internal, "could not record the job: %v", err)
	}
	c.jobs[job.ID] = job
	c.active = append(c.active, job)
	c.changed[job.ID] = make(chan struct{})
	// Hold the job's inputs until it is over, however long that takes.
	c.pin(job, time.Time{}, touched.Read())
	c.log.Info("job submitted", "job", job.ID, "workload", job.Workload, "tasks", len(job.Tasks))
	c.recordLocked(job, eventSubmitted, whole, "", fmt.Sprintf("%s, in %d tasks", job.Workload, len(job.Tasks)))

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

// Jobs returns every job the coordinator has on record, newest first.
func (c *Coordinator) Jobs() []*pb.Job {
	c.mu.Lock()
	defer c.mu.Unlock()
	jobs := make([]*pb.Job, 0, len(c.jobs))
	for _, job := range c.jobs {
		jobs = append(jobs, job.ToProto())
	}
	sort.Slice(jobs, func(a, b int) bool {
		if at, bt := jobs[a].GetCreatedAt().AsTime(), jobs[b].GetCreatedAt().AsTime(); !at.Equal(bt) {
			return at.After(bt)
		}
		return jobs[a].GetJobId() < jobs[b].GetJobId()
	})
	return jobs
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

			RelayAddresses: w.relayAddresses, RelayedConnections: w.relayedConnections, RelayedBytes: w.relayedBytes,
		})
	}
	sort.Slice(nodes, func(i, j int) bool { return nodes[i].NodeId < nodes[j].NodeId })
	return nodes
}

// Relays returns where the connected workers that relay can be reached,
// ordered by node.
func (c *Coordinator) Relays() []string {
	var addresses []string
	for _, node := range c.Nodes() {
		addresses = append(addresses, node.GetRelayAddresses()...)
	}
	return addresses
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
		id:             id,
		name:           hello.GetName(),
		serveAddress:   hello.GetServeAddress(),
		relayAddresses: hello.GetRelayAddresses(),
		holds:          make(map[string]struct{}),
		capabilities:   capabilities,
		connectedAt:    now,
		lastSeen:       now,
		send:           newOutbox(),
		running:        make(map[string]assignment),
		removed:        make(chan struct{}),
	}

	c.mu.Lock()
	if _, taken := c.workers[w.id]; taken {
		c.mu.Unlock()
		return status.Errorf(codes.AlreadyExists, "node %s is already connected", w.id)
	}
	c.workers[w.id] = w
	w.send.add(&pb.CoordinatorMessage{Kind: &pb.CoordinatorMessage_Welcome{Welcome: &pb.Welcome{
		CoordinatorId: c.id, SwarmFingerprint: c.swarmFingerprint,
	}}})
	c.scheduleLocked()
	c.mu.Unlock()
	c.log.Info("worker connected", "node", w.id, "name", w.name, "slots", capabilities.GetTaskSlots())

	// One goroutine writes to the stream and one reads from it, so that this
	// one can also act on the worker being removed. A failed Send means the
	// stream is broken, which the reader also sees, so the writer needs no
	// way to report its error.
	written := make(chan struct{})
	go func() {
		defer close(written)
		for {
			msg, ok := w.send.next()
			if !ok {
				return
			}
			stream.Send(msg)
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
				w.relayedConnections, w.relayedBytes = kind.Heartbeat.GetRelayedConnections(), kind.Heartbeat.GetRelayedBytes()
				if changed := kind.Heartbeat.GetLabels(); changed != nil {
					// What it has now may be what a waiting job needs.
					w.capabilities.Labels = changed.GetLabels()
					c.scheduleLocked()
				}
				c.mu.Unlock()
			case *pb.WorkerMessage_TaskResult:
				c.handleResult(w, kind.TaskResult)
			case *pb.WorkerMessage_TaskUpdate:
				c.handleUpdate(w, kind.TaskUpdate)
			}
		}
	}()
	defer func() {
		c.disconnect(w)
		w.send.close()
		<-written
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

// Holders returns the connected workers, other than asker, that serve blobs
// and are likely to hold the one named, ordered by node ID. It is a hint: a
// worker may have dropped a blob since its tasks used it.
func (c *Coordinator) Holders(blob, asker string) []*pb.BlobHolder {
	c.mu.Lock()
	defer c.mu.Unlock()
	var holders []*pb.BlobHolder
	for _, w := range c.workers {
		if _, has := w.holds[blob]; has && w.id != asker && w.serveAddress != "" {
			holders = append(holders, &pb.BlobHolder{NodeId: w.id, Address: w.serveAddress})
		}
	}
	sort.Slice(holders, func(i, j int) bool { return holders[i].NodeId < holders[j].NodeId })
	return holders
}

// AnnounceSwarm records the fingerprint of the key of the pool's private
// IPFS network and tells every connected worker. Workers that connect later
// are told as they are welcomed.
func (c *Coordinator) AnnounceSwarm(fingerprint string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.swarmFingerprint = fingerprint
	for _, w := range c.workers {
		w.send.add(&pb.CoordinatorMessage{Kind: &pb.CoordinatorMessage_SwarmUpdate{SwarmUpdate: &pb.SwarmUpdate{SwarmFingerprint: fingerprint}}})
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
		if c.ctx.Err() != nil {
			// It is the coordinator that is going away, not the worker.
			a.job.Requeue(a.task, "the coordinator stopped")
		} else {
			c.recordLocked(a.job, eventTaskLost, a.task.Index, w.name, "its worker disconnected")
			a.job.Fail(a.task, "worker "+w.id+" disconnected", maxAttempts, now)
		}
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
	if c.ctx.Err() != nil {
		// A stopping coordinator can no longer combine a job's outputs, so
		// it takes in no more of them. The task runs again after a restart.
		c.log.Warn("dropping a result that arrived while stopping", "node", w.id, "task", result.GetTaskId())
		return
	}
	delete(w.running, a.task.ID)
	defer c.scheduleLocked()

	// A job that is over has had its tasks taken off every worker's hands,
	// so a task found here belongs to one that is still going.
	now := time.Now()
	switch outcome := result.GetOutcome().(type) {
	case *pb.TaskResult_Output:
		// What the task stored must outlive the hour a new blob is kept
		// for, since the job may run longer than that.
		a.job.NoteRead(result.GetReadBlobs()...)
		a.job.NoteTaskOutput(result.GetWrittenBlobs()...)
		for _, held := range append(result.GetReadBlobs(), result.GetWrittenBlobs()...) {
			w.holds[held] = struct{}{}
		}
		c.pin(a.job, time.Time{}, result.GetWrittenBlobs())
		c.recordLocked(a.job, eventTaskSucceeded, a.task.Index, w.name, "")
		if a.job.Succeed(a.task, outcome.Output) {
			// Succeed reports the last task only once, so each job is
			// aggregated once.
			c.aggregating.Add(1)
			go c.aggregate(a.job)
		}
	case *pb.TaskResult_Error:
		c.log.Warn("task attempt failed", "task", a.task.ID, "node", w.id, "attempt", a.task.Attempt, "error", outcome.Error)
		c.recordLocked(a.job, eventTaskFailed, a.task.Index, w.name, outcome.Error)
		a.job.Fail(a.task, outcome.Error, maxAttempts, now)
	default:
		c.recordLocked(a.job, eventTaskFailed, a.task.Index, w.name, "worker reported no outcome")
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
	outputs, key := job.Outputs(), job.Key
	c.mu.Unlock()

	workload, err := c.workloads.Get(job.Workload)
	var result []byte
	touched := runtime.Record(c.store)
	if err == nil {
		result, err = workload.Aggregate(c.ctx, withKey(touched, key), outputs)
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	if c.ctx.Err() != nil {
		// Cut short by the coordinator stopping, which is no fault of the
		// job's. It stays as it was, to be combined after a restart.
		return
	}
	if job.Terminal() {
		// Cancelled while its outputs were being combined.
		return
	}
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
	// Whatever of it is still running is of no use to anyone now.
	c.stopTasksLocked(job)
	c.stopStepsLocked(job)
	c.recordLocked(job, job.State.String(), whole, "", job.Err)
	keep := append(job.InputBlobs(), job.OutputBlobs()...)
	c.pin(job, job.FinishedAt.Add(c.retain), keep)
	if err := c.store.Unpin(owner(job), decode(job.IntermediateBlobs())...); err != nil {
		c.log.Warn("could not release a job's intermediate blobs", "job", job.ID, "error", err)
	}
	c.recordJobLocked(job)
}

// pin pins blobs on a job's behalf. Keeping data is best effort: a job is
// not failed because its blobs could not be pinned, which a worker naming a
// blob the coordinator does not hold is enough to cause.
func (c *Coordinator) pin(job *jobmodel.Job, expires time.Time, cids []string) {
	if err := c.store.Pin(c.ctx, owner(job), expires, decode(cids)...); err != nil {
		c.log.Warn("could not pin a job's blobs", "job", job.ID, "error", err)
	}
}

// withKey returns blobs as a private job with the given key sees them:
// sealing what it stores and unsealing what it opens. With no key it is
// blobs itself.
func withKey(blobs runtime.Blobs, key []byte) runtime.Blobs {
	if len(key) == 0 {
		return blobs
	}
	return runtime.Sealed(blobs, sealed.Key(key))
}

// OwnsPin reports whether owner is a name some job, past or present, holds
// pins under.
func OwnsPin(owner string) bool {
	return strings.HasPrefix(owner, ownerPrefix)
}

const ownerPrefix = "job:"

// owner is the name a job's pins are held under.
func owner(job *jobmodel.Job) string {
	return ownerPrefix + job.ID
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
		if composite, is := c.composite(job); is {
			// Its tasks are jobs, which the coordinator submits itself.
			c.stepsLocked(job, composite)
			continue
		}
		for _, task := range job.Assignable() {
			w := c.pickWorkerLocked(job)
			if w == nil {
				break
			}
			job.Start(task, w.id, w.name, time.Now())
			c.recordLocked(job, eventTaskStarted, task.Index, w.name, fmt.Sprintf("attempt %d", task.Attempt))
			w.running[task.ID] = assignment{job: job, task: task}
			w.send.add(&pb.CoordinatorMessage{Kind: &pb.CoordinatorMessage_Assignment{Assignment: &pb.TaskAssignment{
				TaskId:   task.ID,
				JobId:    job.ID,
				Attempt:  uint32(task.Attempt),
				Workload: job.Workload,
				Payload:  task.Payload,
				Key:      job.Key,
			}}})
			c.notifyLocked(job)
		}
	}
}

// pickWorkerLocked returns the worker with the most free slots that supports
// the workload, or nil if none has a free slot.
func (c *Coordinator) pickWorkerLocked(job *jobmodel.Job) *worker {
	var best *worker
	bestFree := 0
	for _, w := range c.workers {
		if !suits(w.capabilities, job.Workload, job.MinMemory, job.MinGPUs, job.Needs) {
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
func (c *Coordinator) slotsLocked(workload string, minMemory uint64, minGPUs int, needs []string) int {
	total := 0
	for _, w := range c.workers {
		if suits(w.capabilities, workload, minMemory, minGPUs, needs) {
			total += int(w.capabilities.GetTaskSlots())
		}
	}
	return total
}

// suits reports whether a worker with the given capabilities can be given
// tasks of a workload that ask for so much memory, so many graphics cards,
// and whatever else is named in needs. A worker that does not say how much
// memory it has is taken to have none to speak of.
func suits(has *pb.NodeCapabilities, workload string, minMemory uint64, minGPUs int, needs []string) bool {
	for _, need := range needs {
		if !slices.Contains(has.GetLabels(), need) {
			return false
		}
	}
	return slices.Contains(has.GetWorkloads(), workload) && has.GetMemoryBytes() >= minMemory && len(has.GetGpus()) >= minGPUs
}

// notifyLocked is called after every change to a job: it records the change
// and wakes whoever is watching the job.
func (c *Coordinator) notifyLocked(job *jobmodel.Job) {
	c.saveLocked(job)
	c.wakeLocked(job)
}

// wakeLocked wakes whoever is watching a job or following its events.
func (c *Coordinator) wakeLocked(job *jobmodel.Job) {
	close(c.changed[job.ID])
	c.changed[job.ID] = make(chan struct{})
}

func newID() string {
	var b [8]byte
	rand.Read(b[:]) // never fails; see crypto/rand
	return hex.EncodeToString(b[:])
}

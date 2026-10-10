// Package coordinator implements the coordinator role of sisyphusd: it
// accepts jobs, splits them into tasks, assigns tasks to connected workers,
// retries failures and aggregates results. All state is in memory.
package coordinator

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
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
	"github.com/libp2p/go-libp2p/core/peer"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/sisyphus-network/Sisyphus/apps/sisyphusd/access"
	jobmodel "github.com/sisyphus-network/Sisyphus/packages/job-model"
	pb "github.com/sisyphus-network/Sisyphus/packages/protocol/sisyphus/v1"
	"github.com/sisyphus-network/Sisyphus/packages/runtime"
	"github.com/sisyphus-network/Sisyphus/packages/sealed"
	"github.com/sisyphus-network/Sisyphus/packages/storage"
)

const (
	// maxAttempts is how many times a task is handed out before its job fails.
	// Losing the worker mid-task counts as an attempt.
	maxAttempts = 3
	// maxTasksPerJob bounds how finely a submitter can ask to split a job.
	maxTasksPerJob = 10_000
	// maxVerify bounds how many workers a submitter can ask to have run
	// each task of a job; see verify.go.
	maxVerify = 10
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

// Standings keeps what has been seen of workers somewhere that outlasts the
// coordinator. A nodedb.DB is one.
type Standings interface {
	// SaveStanding writes one worker's standing, in place of what was
	// saved for it before.
	SaveStanding(s jobmodel.Standing) error
	// LoadStandings returns the standing of every worker that has one.
	LoadStandings() ([]jobmodel.Standing, error)
}

// noStandings is what keeps them for a coordinator that holds its workers'
// standing in memory only.
type noStandings struct{}

func (noStandings) SaveStanding(jobmodel.Standing) error        { return nil }
func (noStandings) LoadStandings() ([]jobmodel.Standing, error) { return nil, nil }

type Config struct {
	// ID names this coordinator to its workers.
	ID        string
	Workloads *runtime.Registry
	Store     Store
	// Journal, if set, is where jobs are kept across restarts; see Recover.
	Journal Journal
	// Standings, if set, is where the standing of workers is kept across
	// restarts; see Recover. With none it lasts as long as the coordinator.
	Standings Standings
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
	standings Standings
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
	// standing holds, per worker that has one, how its results for verified
	// tasks have come out; see verify.go. It is by node ID, and has workers
	// that are not connected now.
	standing map[string]jobmodel.Standing
	// changed holds, per job, a channel that is closed and replaced every
	// time the job changes or something happens to it. Watchers wait on it.
	changed map[string]chan struct{}
	// events holds, per job, what has happened to it, and seqs the number
	// of the last event: more than the events held, if the oldest of a very
	// long list have been let go.
	events map[string][]jobmodel.Event
	seqs   map[string]uint64
	// logged is how many lines each job's tasks have logged.
	logged map[string]int
	// keys is what the workers of each private job are given, once it has
	// been worked out.
	keys map[string]*pb.TaskAssignment
}

// maxEvents is how many of a job's events are kept to hand.
const maxEvents = 5000

// What a worker reports is kept by the coordinator, on its disk and in its
// memory, so there is only so much of it that a worker can have kept:
// maxLogLines lines logged by a job's tasks, of maxLogLine bytes each, which
// is what a worker cuts a line to itself, and maxTaskBlobs blobs named as
// read or stored by one task.
const (
	maxLogLines  = 20000
	maxLogLine   = 4<<10 + len(" [cut short]")
	maxTaskBlobs = 4096
)

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
	// attempt is the number of the attempt at the task that this is, and
	// started when it was handed out. A task of a job that is verified is
	// with several workers at once, each of them a different attempt.
	attempt int
	started time.Time
}

func New(cfg Config) *Coordinator {
	ctx, cancel := context.WithCancel(context.Background())
	if cfg.Journal == nil {
		cfg.Journal = noJournal{}
	}
	if cfg.Standings == nil {
		cfg.Standings = noStandings{}
	}
	c := &Coordinator{
		id:        cfg.ID,
		workloads: cfg.Workloads,
		store:     cfg.Store,
		journal:   cfg.Journal,
		standings: cfg.Standings,
		retain:    cfg.Retain,
		keepJobs:  cfg.KeepJobs,
		log:       cfg.Log,
		ctx:       ctx,
		cancel:    cancel,
		jobs:      make(map[string]*jobmodel.Job),
		workers:   make(map[string]*worker),
		standing:  make(map[string]jobmodel.Standing),
		changed:   make(map[string]chan struct{}),
		events:    make(map[string][]jobmodel.Event),
		seqs:      make(map[string]uint64),
		logged:    make(map[string]int),
		keys:      make(map[string]*pb.TaskAssignment),
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
	// In a job that is verified: a worker returned a result for a task,
	// and the results in for a task are not all the same.
	eventTaskResult    = "task-result"
	eventTaskDisagreed = "task-disagreed"
	eventTaskRechecked = "task-rechecked"
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
			if a.job.TaskTimeout <= 0 || now.Sub(a.started) <= a.job.TaskTimeout {
				continue
			}
			delete(w.running, id)
			w.send.add(cancelOf(a))
			reason := "timed out after " + a.job.TaskTimeout.String()
			c.recordLocked(a.job, eventTaskTimedOut, a.task.Index, w.name, reason)
			c.failLocked(a, reason, now)
			c.settleLocked(a.job)
			c.notifyLocked(a.job)
		}
	}
	c.scheduleLocked()
}

// cancelOf is the message that tells a worker to stop a task it was given.
func cancelOf(a assignment) *pb.CoordinatorMessage {
	return &pb.CoordinatorMessage{Kind: &pb.CoordinatorMessage_Cancel{Cancel: &pb.TaskCancel{TaskId: a.task.ID, Attempt: uint32(a.attempt)}}}
}

// stopTasksLocked tells every worker running a task of a job that is over to
// stop it, and takes the task off the worker's hands, so that its slot is
// free at once and whatever it still reports is ignored.
func (c *Coordinator) stopTasksLocked(job *jobmodel.Job) {
	for _, w := range c.workers {
		for id, a := range w.running {
			if a.job == job {
				delete(w.running, id)
				w.send.add(cancelOf(a))
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
	if !ok || uint32(a.attempt) != update.GetAttempt() {
		return
	}
	if progress := min(max(update.GetProgress(), 0), 1); c.replicated(a.job) {
		a.job.ReportCopy(a.task, progress)
	} else {
		a.task.Progress = progress
	}
	for _, line := range update.GetLog() {
		c.logLocked(a.job, a.task.Index, w.name, line)
	}
	c.wakeLocked(a.job)
}

// logLocked keeps a line one of a job's tasks logged, cut to the length a
// worker should have cut it to, until the job has logged as many as are
// kept: then it says so, once, in place of the next, and keeps no more.
func (c *Coordinator) logLocked(job *jobmodel.Job, task int, node, line string) {
	switch logged := c.logged[job.ID]; {
	case logged > maxLogLines:
		return
	case logged == maxLogLines:
		line = fmt.Sprintf("this job's tasks have logged %d lines, and what they log from here on is not kept", maxLogLines)
	case len(line) > maxLogLine:
		line = strings.ToValidUTF8(line[:maxLogLine], "") + " [cut short]"
	}
	c.logged[job.ID]++
	c.recordLocked(job, eventLog, task, node, line)
}

// Recover takes up the jobs in the journal where an earlier coordinator
// left them, and returns how many of them were unfinished. Call it once,
// before the coordinator is given anything else to do.
//
// A task that was running goes back to wait for a worker: whoever had it
// has lost its connection, and anything it sends later is for an attempt
// this coordinator did not hand out. That does not count against the task.
// The results a task of a verified job already had are kept, and only the
// workers that had not answered are asked again.
//
// The standing of workers comes back too, so that one outvoted before the
// restart is still on probation after it.
func (c *Coordinator) Recover() (unfinished int, err error) {
	jobs, err := c.journal.LoadJobs()
	if err != nil {
		return 0, err
	}
	standings, err := c.standings.LoadStandings()
	if err != nil {
		return 0, err
	}
	// A job's key is kept only while it is unfinished, so only such jobs
	// have keys to work out again.
	keys := make(map[string]*pb.TaskAssignment)
	for _, job := range jobs {
		keys[job.ID], _ = c.keysFor(job.ID, job.Key, job.Params)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, s := range standings {
		c.standing[s.NodeID] = s
	}
	for _, job := range jobs {
		job.Needs = c.needs(job.Workload, job.Params, len(job.Key) > 0)
		c.keys[job.ID] = keys[job.ID]
		// What happened to it before comes back with it. A job whose events
		// cannot be read is still a job.
		events, err := c.journal.LoadEvents(job.ID)
		if err != nil {
			c.log.Warn("could not load a job's events", "job", job.ID, "error", err)
		}
		for _, e := range events {
			if e.Kind == eventLog {
				c.logged[job.ID]++
			}
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
				c.takeBack(job, task, "the coordinator was restarted")
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
		delete(c.logged, id)
		delete(c.keys, id)
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

	verify := int(spec.GetVerify())
	if verify > maxVerify {
		return nil, status.Errorf(codes.InvalidArgument, "verify exceeds %d", maxVerify)
	}
	share := spec.GetVerifyShare()
	if share < 0 || share > 1 || share != share {
		return nil, status.Error(codes.InvalidArgument, "verify_share is the share of a job's tasks to verify, from 0 to 1")
	}
	if share > 0 && verify < 2 {
		return nil, status.Error(codes.InvalidArgument, "verify_share says how many of a job's tasks to verify, and needs verify to say by how many workers")
	}
	needs := c.needs(workload.Name(), spec.GetParams(), len(spec.GetKey()) > 0)

	parts := 1
	if mode == jobmodel.Distributed {
		parts = int(spec.GetMaxTasks())
		if parts == 0 {
			// A task of a verified job takes a slot on each worker it is
			// handed to.
			c.mu.Lock()
			parts = max(c.slotsLocked(workload.Name(), spec.GetMinMemoryBytes(), int(spec.GetMinGpus()), needs)/max(verify, 1), 1)
			c.mu.Unlock()
		}
	}
	if n := len(spec.GetKey()); n != 0 && n != sealed.KeySize {
		return nil, status.Errorf(codes.InvalidArgument, "a job's key must be %d bytes, not %d", sealed.KeySize, n)
	}
	// Splitting may read stored data, so it runs without the lock.
	// The job has its ID before it is split, since what splitting stores
	// is sealed, if the job is private, with the job's own key.
	id := newID()
	touched := runtime.Record(c.store)
	payloads, err := workload.Split(ctx, withKey(touched, spec.GetKey(), id), spec.GetParams(), parts)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	if len(payloads) == 0 {
		return nil, status.Error(codes.Internal, "workload split the job into no tasks")
	}
	keys, old := c.keysFor(id, spec.GetKey(), spec.GetParams())

	c.mu.Lock()
	defer c.mu.Unlock()
	if of, is := c.jobs[parent]; is && of.Terminal() {
		// It ended while this step was being split. A step begun now
		// would be nobody's to stop, and no part of the job's record.
		return nil, status.Errorf(codes.Aborted, "job %q, which this would be a step of, is over", parent)
	}
	// A job cannot be verified by fewer workers than it asks to agree. The
	// steps of a job whose tasks are jobs are held to that as each begins.
	_, steps := workload.(runtime.Composite)
	if able := c.ableLocked(workload.Name(), spec.GetMinMemoryBytes(), int(spec.GetMinGpus()), needs); verify >= 2 && !steps && able < verify {
		return nil, status.Errorf(codes.FailedPrecondition, "verify asks for %d different workers to run each task, and %d connected now could take this job", verify, able)
	}
	job := jobmodel.New(id, workload.Name(), spec.GetParams(), mode, int(spec.GetMaxTasks()), payloads, time.Now())
	switch {
	case verify >= 2 && steps:
		// Its tasks are jobs, which pick their own tasks to verify.
		job.Check(verify, share, nil)
	case verify >= 2:
		job.Check(verify, share, shuffled(len(job.Tasks)))
	}
	job.Key, job.Private, job.Submitter = spec.GetKey(), len(spec.GetKey()) > 0, submitter
	job.TaskTimeout = time.Duration(spec.GetTaskTimeoutSeconds()) * time.Second
	job.MinMemory, job.MinGPUs = spec.GetMinMemoryBytes(), int(spec.GetMinGpus())
	job.Needs = needs
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
	asked := fmt.Sprintf("%s, in %d tasks", job.Workload, len(job.Tasks))
	switch {
	case job.VerifyShare > 0 && steps:
		asked += fmt.Sprintf(", a share of %.3g of the tasks of each to be verified by %d workers", job.VerifyShare, verify)
	case job.VerifyShare > 0:
		asked += fmt.Sprintf(", a share of %.3g of them, picked at random, to be verified by %d workers", job.VerifyShare, verify)
	case verify >= 2:
		asked += fmt.Sprintf(", each to be verified by %d workers", verify)
	}
	c.recordLocked(job, eventSubmitted, whole, "", asked)
	c.keys[job.ID] = keys
	if old != nil {
		c.log.Warn("a private job names an input sealed with a sealing key itself, so its workers are given that key whole", "job", job.ID, "input", old.String())
		c.recordLocked(job, eventLog, whole, "", "input "+old.String()+" was sealed before each file had a key of its own, so this job's workers are given the whole sealing key; store the file again to give them only its own")
	}

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
		standing := c.standing[w.id]
		nodes = append(nodes, &pb.NodeInfo{
			NodeId:       w.id,
			Name:         w.name,
			Capabilities: w.capabilities,
			RunningTasks: uint32(len(w.running)),
			ConnectedAt:  timestamppb.New(w.connectedAt),
			LastSeenAt:   timestamppb.New(w.lastSeen),

			RelayAddresses: w.relayAddresses, RelayedConnections: w.relayedConnections, RelayedBytes: w.relayedBytes,

			VerifiedAgreed: standing.Agreed, VerifiedOutvoted: standing.Outvoted, Probation: uint32(standing.Probation),
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
	capabilities.Labels = labelsOf(id, hello.GetName(), capabilities.GetLabels())

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
					w.capabilities.Labels = labelsOf(w.id, w.name, changed.GetLabels())
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

// ServeAddress returns where the connected worker with the given ID serves
// blobs to its fellows, or "" if it is not connected or serves none.
func (c *Coordinator) ServeAddress(id string) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if w, ok := c.workers[id]; ok {
		return w.serveAddress
	}
	return ""
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
			c.requeueLocked(a, "the coordinator stopped")
		} else {
			c.recordLocked(a.job, eventTaskLost, a.task.Index, w.name, "its worker disconnected")
			c.failLocked(a, "worker "+w.id+" disconnected", now)
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
	if !ok || uint32(a.attempt) != result.GetAttempt() {
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
	if named := len(result.GetReadBlobs()) + len(result.GetWrittenBlobs()); named > maxTaskBlobs {
		// Every blob named is noted with the job and pinned for it.
		result = &pb.TaskResult{Outcome: &pb.TaskResult_Error{Error: fmt.Sprintf("the worker named %d blobs as read and stored by the task, and a task may have %d", named, maxTaskBlobs)}}
	}
	switch outcome := result.GetOutcome().(type) {
	case *pb.TaskResult_Output:
		// What the task stored must outlive the hour a new blob is kept
		// for, since the job may run longer than that. Only what the store
		// holds is taken as stored: the worker's word for it is not enough.
		read, written := wellFormed(result.GetReadBlobs()), c.keep(a.job, w, result.GetWrittenBlobs())
		a.job.NoteRead(read...)
		a.job.NoteTaskOutput(written...)
		// Who holds what is a worker's word for where a blob may be
		// fetched, and whoever fetches checks what comes against its CID.
		for _, held := range append(result.GetReadBlobs(), result.GetWrittenBlobs()...) {
			w.holds[held] = struct{}{}
		}
		done := false
		if c.replicated(a.job) {
			// One result among several, which settles the task only if
			// enough of them are the same.
			done = c.returnedLocked(w, a, outcome.Output, written)
		} else {
			c.recordLocked(a.job, eventTaskSucceeded, a.task.Index, w.name, "")
			done = a.job.Succeed(a.task, outcome.Output)
		}
		if done {
			// The last task is reported only once, so each job is
			// aggregated once.
			c.aggregating.Add(1)
			go c.aggregate(a.job)
		}
	case *pb.TaskResult_Error:
		c.log.Warn("task attempt failed", "task", a.task.ID, "node", w.id, "attempt", a.attempt, "error", outcome.Error)
		c.recordLocked(a.job, eventTaskFailed, a.task.Index, w.name, outcome.Error)
		c.failLocked(a, outcome.Error, now)
	default:
		c.recordLocked(a.job, eventTaskFailed, a.task.Index, w.name, "worker reported no outcome")
		c.failLocked(a, "worker reported no outcome", now)
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
	outputs, key, id := job.Outputs(), job.Key, job.ID
	c.mu.Unlock()

	workload, err := c.workloads.Get(job.Workload)
	var result []byte
	touched := runtime.Record(c.store)
	if err == nil {
		result, err = workload.Aggregate(c.ctx, withKey(touched, key, id), outputs)
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

// keep pins for a job the blobs a worker says one of its tasks stored, and
// returns those that are taken to have been stored. A blob the store does
// not hold was not stored, whatever the worker says, and what is no CID is
// no blob: neither is noted with the job. A pin that fails for another
// reason is the store's trouble and not the worker's, so the blob is still
// noted, as it was before the store was asked one blob at a time.
func (c *Coordinator) keep(job *jobmodel.Job, w *worker, cids []string) []string {
	if all := decode(cids); len(all) == len(cids) && c.store.Pin(c.ctx, owner(job), time.Time{}, all...) == nil {
		return cids
	}
	var stored []string
	for _, named := range cids {
		blob, err := cid.Decode(named)
		if err == nil {
			if err = c.store.Pin(c.ctx, owner(job), time.Time{}, blob); err != nil && !errors.Is(err, storage.ErrNotFound) {
				c.log.Warn("could not pin a job's blobs", "job", job.ID, "error", err)
				err = nil
			}
		}
		if err != nil {
			c.log.Warn("a worker named a blob as stored by its task that the pool does not hold", "job", job.ID, "node", w.id, "blob", named)
			continue
		}
		stored = append(stored, named)
	}
	return stored
}

// wellFormed returns those of cids that are CIDs.
func wellFormed(cids []string) []string {
	return slices.DeleteFunc(slices.Clone(cids), func(named string) bool {
		_, err := cid.Decode(named)
		return err != nil
	})
}

// withKey returns blobs as a private job with the given key sees them:
// sealing what it stores and unsealing what it opens. With no key it is
// blobs itself.
func withKey(blobs runtime.Blobs, key []byte, jobID string) runtime.Blobs {
	if len(key) == 0 {
		return blobs
	}
	// The coordinator holds the sealing key itself, which opens whatever
	// was sealed with any key derived from it. What it stores for the job
	// it seals as the job's workers do, with the job's own.
	return runtime.Sealed(blobs, sealed.Key(key), sealed.Key(key).ForJob(jobID))
}

// needs is what a worker must have to be given the tasks of a job: what
// its workload asks for, and, for a private job, that it takes keys as
// they are now given.
func (c *Coordinator) needs(workload string, params []byte, private bool) []string {
	needs := c.workloads.Needs(workload, params)
	if private {
		needs = append(needs, runtime.SealingLabel)
	}
	return needs
}

// keysFor works out the keys the workers of a private job are given, as
// the part of a task's assignment that carries them. They are the job's
// own key, which what its tasks store is sealed with, and the key of each
// sealed blob the job's parameters name. The sealing key the job was
// submitted with is not among them: a worker is given what opens this
// job's data, and with that opens nothing else the same key seals.
//
// An input sealed before keys were derived is opened by the sealing key
// and nothing less. A job that names one has its workers given that key
// whole, as every private job's once were, and the first such input is
// returned so that it can be said.
//
// It reads the store, which may take its time over a blob it does not
// hold, so it is not called with the lock held.
func (c *Coordinator) keysFor(jobID string, sealing, params []byte) (given *pb.TaskAssignment, old *cid.Cid) {
	if len(sealing) == 0 {
		return nil, nil
	}
	key := sealed.Key(sealing)
	own := key.ForJob(jobID)
	given = &pb.TaskAssignment{Keys: []*pb.SealingKey{{Id: own.ID[:], Key: own.Key[:]}}}
	held := map[sealed.KeyID]bool{own.ID: true}
	for _, named := range namedBlobs(params) {
		switch id, whole, is := c.sealedWith(named); {
		case !is || held[id]:
		case whole:
			if old == nil {
				given.Key, old = sealing, &named
			}
		default:
			held[id] = true
			derived := key.Grant(id)
			given.Keys = append(given.Keys, &pb.SealingKey{Id: derived.ID[:], Key: derived.Key[:]})
		}
	}
	return given, old
}

// sealedWith reads the head of a stored blob: whether it is sealed, and
// with which key. What the store does not hold, or cannot read, is taken
// not to be sealed: a worker that needs it will say so itself.
func (c *Coordinator) sealedWith(named cid.Cid) (id sealed.KeyID, whole, is bool) {
	blob, err := c.store.Open(c.ctx, named)
	if err != nil {
		return id, false, false
	}
	defer blob.Close()
	head := make([]byte, sealed.HeaderSize)
	n, _ := io.ReadFull(blob, head) // a short blob has a short head
	return sealed.Header(head[:n])
}

// namedBlobs returns the blobs that a job's parameters name: every string
// in them, however deep, that is a CID.
func namedBlobs(params []byte) []cid.Cid {
	var parsed any
	json.Unmarshal(params, &parsed) // parameters that are not JSON name nothing
	var named []cid.Cid
	var walk func(any)
	walk = func(v any) {
		switch v := v.(type) {
		case string:
			if c, err := cid.Decode(v); err == nil {
				named = append(named, c)
			}
		case []any:
			for _, inner := range v {
				walk(inner)
			}
		case map[string]any:
			for _, inner := range v {
				walk(inner)
			}
		}
	}
	walk(parsed)
	return named
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
		if c.replicated(job) {
			// Each of its tasks goes to several workers at once.
			c.copiesLocked(job)
			continue
		}
		for _, task := range job.Assignable() {
			w := c.pickWorkerLocked(job, task)
			if w == nil {
				break
			}
			now := time.Now()
			job.Start(task, w.id, w.name, now)
			c.handLocked(w, assignment{job: job, task: task, attempt: task.Attempt, started: now})
		}
	}
}

// handLocked gives a worker an attempt at a task.
func (c *Coordinator) handLocked(w *worker, a assignment) {
	c.recordLocked(a.job, eventTaskStarted, a.task.Index, w.name, fmt.Sprintf("attempt %d", a.attempt))
	w.running[a.task.ID] = a
	keys := c.keys[a.job.ID]
	w.send.add(&pb.CoordinatorMessage{Kind: &pb.CoordinatorMessage_Assignment{Assignment: &pb.TaskAssignment{
		TaskId:   a.task.ID,
		JobId:    a.job.ID,
		Attempt:  uint32(a.attempt),
		Workload: a.job.Workload,
		Payload:  a.task.Payload,
		Key:      keys.GetKey(),
		Keys:     keys.GetKeys(),
	}}})
	c.notifyLocked(a.job)
}

// pickWorkerLocked returns the worker with the most free slots that supports
// the workload, or nil if none has a free slot. A worker that has a copy of
// the task, or has returned a result for it, is passed over: only a task of
// a verified job has such workers while it waits for another.
func (c *Coordinator) pickWorkerLocked(job *jobmodel.Job, task *jobmodel.Task) *worker {
	var best *worker
	bestFree := 0
	for _, w := range c.workers {
		if !suits(w.capabilities, job.Workload, job.MinMemory, job.MinGPUs, job.Needs) || task.Asked(w.id) {
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

// labelsOf is what a worker is labelled with: what it says it offers, and
// who it is. Who it is the coordinator says for itself, whatever the worker
// said: its node ID, which is that of the key it connected with, and the
// name it gave. So a job that asks for a worker by ID is given to that node
// and no other, where one that asks by name is given to whichever worker
// has taken the name. A name that is itself a node ID labels nothing, or a
// worker could pass for the node it names.
func labelsOf(id, name string, said []string) []string {
	labels := []string{runtime.WorkerLabel + id}
	if _, err := peer.Decode(name); err != nil && name != "" {
		labels = append(labels, runtime.WorkerLabel+name)
	}
	for _, label := range said {
		if !strings.HasPrefix(label, runtime.WorkerLabel) {
			labels = append(labels, label)
		}
	}
	return labels
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

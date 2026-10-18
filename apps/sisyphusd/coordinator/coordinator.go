// Package coordinator implements the coordinator role of sisyphusd: it
// accepts jobs, splits them into tasks, assigns tasks to connected workers,
// retries failures and aggregates results. All state is in memory.
package coordinator

import (
	"cmp"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net/netip"
	"slices"
	"sort"
	"strconv"
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
	Has(ctx context.Context, c cid.Cid) (bool, error)
	Pin(ctx context.Context, owner string, expires time.Time, cids ...cid.Cid) error
	// PinWithin pins as Pin does, unless that would take the owner's group
	// past limit; see storage.Store.PinWithin.
	PinWithin(ctx context.Context, owner string, expires time.Time, limit uint64, cids ...cid.Cid) error
	Unpin(owner string, cids ...cid.Cid) error
	// UnpinPrefix releases every pin whose owner's name begins with prefix.
	UnpinPrefix(prefix string) error
	PutNodes(ctx context.Context, root []byte, linked ...[]byte) (cid.Cid, error)
	GetNode(ctx context.Context, c cid.Cid) ([]byte, error)
	// GroupPins has the store count what each group of owners keeps, and
	// Usage says it; see storage.Store.GroupPins.
	GroupPins(ctx context.Context, group func(owner string) string) error
	Usage(now time.Time) map[string]uint64
	// GroupUsage says what one group keeps, as Usage does.
	GroupUsage(group string, now time.Time) uint64
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
	ID string
	// Sign, if set, signs with the key of the node named ID. A job's
	// record is then kept with the coordinator's attestation of it.
	Sign      func([]byte) []byte
	Workloads *runtime.Registry
	// Audits is the workloads this node can run itself, and so can audit:
	// run a task again to check what a worker returned for it. Nil is none.
	Audits *runtime.Registry
	Store  Store
	// ClientOf names the client a pin owner other than a job holds pins
	// for, or nothing for one that holds them for none. It is how Stored
	// counts a client's own pins.
	ClientOf func(owner string) string
	// DistinctAddresses has a verified task's copies go only to workers at
	// different network addresses; see verify.go. AddressOf says where the
	// worker making a call is, and is the call's own address if left nil.
	DistinctAddresses bool
	AddressOf         func(ctx context.Context) string
	// SourceOf says, in full, the address the worker making a call connects
	// from, and is the call's own source address if left nil. A worker is
	// told it, if it is a global address; see observed.go.
	SourceOf func(ctx context.Context) netip.Addr
	// MaxJobsPerClient, if not zero, is how many unfinished jobs one client
	// may have at once, so that one cannot fill the coordinator for all
	// the rest. The steps of a job are not counted apart from it, and the
	// node's own jobs, which have no submitter, not at all.
	MaxJobsPerClient int
	// MaxTasksPerClient, if not zero, is how many unfinished tasks, waiting
	// or running, one client may have across all its jobs and their steps:
	// a job that would take it past that is refused whole, and so is a
	// step of a graph when it begins, which fails the graph. It bounds what
	// one client can queue, which a count of jobs does not when one job has
	// many tasks. The node's own jobs are not counted.
	MaxTasksPerClient int
	// MaxStoreBytesPerClient, if not zero, is the most one client may keep
	// in the store, as Stored counts it. A task of the client's job whose
	// outputs would take it past that fails, and its job with it, at once:
	// its outputs are not kept, the task is not tried again, and its
	// worker is not blamed. The node's own jobs are not counted.
	MaxStoreBytesPerClient uint64
	// MaxUploadBytesPerAttempt, if not zero, is the most a worker may
	// upload to the coordinator for one attempt at a task, counted as it
	// arrives across all the attempt's uploads. An upload that would take
	// the attempt past it is refused.
	MaxUploadBytesPerAttempt uint64
	// MaxTaskTimeout, if not zero, is the longest one attempt at a task of
	// a client's job may run: a job may ask for less, never more, and one
	// that names no timeout gets this. Without it, a task that never ends
	// holds its worker's slot until the worker goes. The node's own jobs
	// keep what they ask for.
	MaxTaskTimeout time.Duration
	// HardenedContainers has every job whose tasks run in containers go
	// only to workers that run them hardened: runtime.HardenedLabels. For a pool that
	// takes container work from people its workers' owners do not know.
	HardenedContainers bool
	// Journal, if set, is where jobs are kept across restarts; see Recover.
	Journal Journal
	// Standings, if set, is where the standing of workers is kept across
	// restarts; see Recover. With none it lasts as long as the coordinator.
	Standings Standings
	// Retain is how long a job's inputs and results stay pinned after it
	// finishes.
	Retain time.Duration
	// KeepJobs is how long a finished job stays on record; see Prune. Zero
	// keeps them for good. A client's job stays at least as long as Retain
	// too, since its record is what says whose its data is.
	KeepJobs time.Duration
	Log      *slog.Logger
	// WorkersChanged is called after a worker joins or leaves this pool.
	WorkersChanged func()
	// JobsChanged is called whenever a job's published snapshot changes.
	JobsChanged func()
	// PruneChanged is called when a job becomes terminal and may gain a retention deadline.
	PruneChanged func()
}

type Coordinator struct {
	pb.UnimplementedCoordinatorServiceServer

	id string
	// sign, if set, signs with the coordinator's node key; see Config.Sign.
	sign      func([]byte) []byte
	workloads *runtime.Registry
	audits    *runtime.Registry
	// distinct is whether a verified task's copies must go to workers at
	// different addresses, addressOf how a caller's is told, and addresses
	// where each worker that has connected was when it last did.
	distinct  bool
	addressOf func(ctx context.Context) string
	sourceOf  func(ctx context.Context) netip.Addr
	// hardened is whether container jobs need hardened workers.
	hardened bool
	// perClient is how many unfinished jobs one client may have, and
	// tasksPerClient how many unfinished tasks.
	perClient, tasksPerClient int
	// storePerClient is the most one client may keep in the store.
	storePerClient uint64
	// maxTaskTimeout is the longest an attempt at a client's task may run.
	maxTaskTimeout time.Duration
	addresses      map[string]string
	store          Store
	clientOf       func(owner string) string
	// submitters has the client each client job's pins are held for, by
	// the name they are held under. It is read with the store's pins
	// locked, so it has a lock of its own and not the coordinator's.
	submitters sync.Map
	// perAttempt is the most a worker may upload for one attempt, and
	// uploaded how much each attempt has while it lasts, by the name
	// Uploading gives it. inFlight is how much the attempts at each
	// client's jobs have uploaded between them, while they last, for a
	// coordinator that holds clients to a share.
	// They have a lock of their own, as uploads are counted piece by piece
	// as they arrive.
	perAttempt     uint64
	uploadMu       sync.Mutex
	uploaded       map[string]attemptUploads
	inFlight       map[string]uint64
	grouping       sync.Once
	journal        Journal
	standings      Standings
	retain         time.Duration
	keepJobs       time.Duration
	log            *slog.Logger
	workersChanged func()
	jobsChanged    func()
	pruneChanged   func()
	relayMu        sync.Mutex
	relaySubs      map[chan struct{}]struct{}

	// ctx bounds the aggregations still running when the coordinator closes.
	ctx         context.Context
	cancel      context.CancelFunc
	aggregating background

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
	// expiryChanged wakes the deadline watcher when running attempts change.
	expiryChanged chan struct{}
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
	// dialled is the coordinator's address as the worker reached it, if
	// that is a global address.
	dialled netip.Addr
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
		id:             cfg.ID,
		sign:           cfg.Sign,
		workloads:      cfg.Workloads,
		audits:         cfg.Audits,
		distinct:       cfg.DistinctAddresses,
		hardened:       cfg.HardenedContainers,
		perClient:      cfg.MaxJobsPerClient,
		tasksPerClient: cfg.MaxTasksPerClient,
		storePerClient: cfg.MaxStoreBytesPerClient,
		perAttempt:     cfg.MaxUploadBytesPerAttempt,
		uploaded:       make(map[string]attemptUploads),
		inFlight:       make(map[string]uint64),
		maxTaskTimeout: cfg.MaxTaskTimeout,
		addressOf:      cfg.AddressOf,
		sourceOf:       cfg.SourceOf,
		addresses:      make(map[string]string),
		store:          cfg.Store,
		clientOf:       cfg.ClientOf,
		journal:        cfg.Journal,
		standings:      cfg.Standings,
		retain:         cfg.Retain,
		keepJobs:       cfg.KeepJobs,
		log:            cfg.Log,
		workersChanged: cfg.WorkersChanged,
		jobsChanged:    cfg.JobsChanged,
		pruneChanged:   cfg.PruneChanged,
		relaySubs:      make(map[chan struct{}]struct{}),
		ctx:            ctx,
		cancel:         cancel,
		jobs:           make(map[string]*jobmodel.Job),
		workers:        make(map[string]*worker),
		standing:       make(map[string]jobmodel.Standing),
		changed:        make(map[string]chan struct{}),
		events:         make(map[string][]jobmodel.Event),
		seqs:           make(map[string]uint64),
		logged:         make(map[string]int),
		keys:           make(map[string]*pb.TaskAssignment),
		expiryChanged:  make(chan struct{}, 1),
	}
	if c.addressOf == nil {
		c.addressOf = callersAddress
	}
	if c.sourceOf == nil {
		c.sourceOf = callersSource
	}
	// Attempts are expired at their nearest deadline, not found by scanning
	// all running work once a second.
	c.aggregating.Add(1)
	go func() {
		defer c.aggregating.Done()
		c.watchAttemptExpirations()
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
	changed := false
	for _, w := range c.workers {
		for id, a := range w.running {
			if a.job.TaskTimeout <= 0 || now.Before(a.started.Add(a.job.TaskTimeout)) {
				continue
			}
			delete(w.running, id)
			changed = true
			c.releaseLocked(a)
			w.send.add(cancelOf(a))
			reason := "timed out after " + a.job.TaskTimeout.String()
			c.recordLocked(a.job, eventTaskTimedOut, a.task.Index, w.name, reason)
			c.failLocked(a, reason, now)
			c.settleLocked(a.job)
			c.notifyLocked(a.job)
		}
	}
	if changed {
		c.notifyExpiryChangedLocked()
	}
	c.scheduleLocked()
}

// watchAttemptExpirations waits for the nearest running-attempt deadline.
// Assignment changes interrupt the wait so an earlier deadline is not missed.
func (c *Coordinator) watchAttemptExpirations() {
	timer := time.NewTimer(time.Hour)
	timer.Stop()
	defer timer.Stop()
	for {
		deadline, found := c.nextAttemptExpiry()
		if !found {
			select {
			case <-c.expiryChanged:
				continue
			case <-c.ctx.Done():
				return
			}
		}
		// A deadline already past makes the timer fire at once.
		timer.Reset(time.Until(deadline))
		select {
		case <-timer.C:
			c.Expire(time.Now())
		case <-c.expiryChanged:
			// Since Go 1.23, the Reset above drops any tick the timer made
			// for an earlier deadline, so nothing needs draining here.
		case <-c.ctx.Done():
			return
		}
	}
}

func (c *Coordinator) nextAttemptExpiry() (time.Time, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	var earliest time.Time
	for _, w := range c.workers {
		for _, a := range w.running {
			if a.job.TaskTimeout <= 0 {
				continue
			}
			deadline := a.started.Add(a.job.TaskTimeout)
			if earliest.IsZero() || deadline.Before(earliest) {
				earliest = deadline
			}
		}
	}
	return earliest, !earliest.IsZero()
}

// notifyExpiryChangedLocked asks the deadline watcher to re-evaluate the
// running attempts. Changes coalesce; the current state is always re-read.
func (c *Coordinator) notifyExpiryChangedLocked() {
	select {
	case c.expiryChanged <- struct{}{}:
	default:
	}
}

// cancelOf is the message that tells a worker to stop a task it was given.
func cancelOf(a assignment) *pb.CoordinatorMessage {
	return &pb.CoordinatorMessage{Kind: &pb.CoordinatorMessage_Cancel{Cancel: &pb.TaskCancel{TaskId: a.task.ID, Attempt: uint32(a.attempt)}}}
}

// stopTasksLocked tells every worker running a task of a job that is over to
// stop it, and takes the task off the worker's hands, so that its slot is
// free at once and whatever it still reports is ignored.
func (c *Coordinator) stopTasksLocked(job *jobmodel.Job) {
	changed := false
	for _, w := range c.workers {
		for id, a := range w.running {
			if a.job == job {
				delete(w.running, id)
				changed = true
				c.releaseLocked(a)
				w.send.add(cancelOf(a))
			}
		}
	}
	if changed {
		c.notifyExpiryChangedLocked()
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
		c.noteSubmitter(job)
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
		kept := c.keepJobs
		if job.Submitter != "" {
			// What a client's job keeps counts towards the client for as
			// long as it is kept, and only the job's record says whose it
			// is: the record stays until its data has lapsed too.
			kept = max(kept, c.retain)
		}
		return c.keepJobs > 0 && job.Terminal() && now.Sub(job.FinishedAt) > kept
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
		c.submitters.Delete(owner(c.jobs[id]))
		delete(c.jobs, id)
		delete(c.changed, id)
		delete(c.events, id)
		delete(c.seqs, id)
		delete(c.logged, id)
		delete(c.keys, id)
	}
	return len(old), nil
}

// NextPruneAt returns the earliest time a finished job can be pruned, or
// zero when no finished job has a retention deadline. A child job's record
// remains while its parent record links it, so both deadlines must have
// passed before the child is eligible.
func (c *Coordinator) NextPruneAt() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.keepJobs <= 0 {
		return time.Time{}
	}
	due := func(job *jobmodel.Job) time.Time {
		if job == nil || !job.Terminal() {
			return time.Time{}
		}
		kept := c.keepJobs
		if job.Submitter != "" {
			kept = max(kept, c.retain)
		}
		if job.FinishedAt.IsZero() {
			return time.Now()
		}
		// Prune uses a strict greater-than age comparison.
		return job.FinishedAt.Add(kept).Add(time.Nanosecond)
	}
	var next time.Time
	for _, job := range c.jobs {
		at := due(job)
		if at.IsZero() {
			continue
		}
		if parent, step := c.jobs[job.Parent]; step {
			parentAt := due(parent)
			if parentAt.IsZero() {
				continue
			}
			if parentAt.After(at) {
				at = parentAt
			}
		}
		if next.IsZero() || at.Before(next) {
			next = at
		}
	}
	return next
}

// Holds reports whether owner is the name an unfinished job holds its pins
// under, or stages its tasks' results under. Pins held open under any other job's name have nobody left to
// release them.
func (c *Coordinator) Holds(owner string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	id, _, _ := strings.Cut(strings.TrimPrefix(owner, ownerPrefix), stagingSeparator)
	job, ok := c.jobs[id]
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
// background counts what the coordinator has set going on its own, so that
// Close can wait for it. Unlike a sync.WaitGroup it may be added to while
// Close is waiting, as a worker that disconnects as the coordinator closes
// may finish a job and store its record: what starts then is waited for
// too.
type background struct {
	mu   sync.Mutex
	n    int
	idle *sync.Cond
}

func (b *background) Add(n int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.n += n
}

func (b *background) Done() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.n--; b.n == 0 && b.idle != nil {
		b.idle.Broadcast()
	}
}

func (b *background) Wait() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.idle == nil {
		b.idle = sync.NewCond(&b.mu)
	}
	for b.n > 0 {
		b.idle.Wait()
	}
}

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
	audit := spec.GetAuditShare()
	switch _, composite := workload.(runtime.Composite); {
	case audit < 0 || audit > 1 || audit != audit:
		return nil, status.Error(codes.InvalidArgument, "audit_share is the share of a job's tasks for the coordinator to run again itself, from 0 to 1")
	case audit > 0 && verify >= 2:
		return nil, status.Error(codes.InvalidArgument, "audit_share has the coordinator check tasks by running them itself, and verify has other workers check them: a job asks for one or the other")
	case audit > 0 && !composite && !c.runsItself(workload.Name()):
		return nil, status.Errorf(codes.FailedPrecondition, "audit_share has this node run tasks of the job again itself, and it cannot run %s: it is not a worker for that workload", workload.Name())
	}
	if varying, ok := workload.(runtime.Varying); ok && varying.Varies() && (verify >= 2 || audit > 0) {
		return nil, status.Errorf(codes.InvalidArgument, "%s gives results that can differ from one run to the next, and verify and audit_share check a task by comparing what two runs returned, so honest workers would be taken for wrong ones: they are for work that gives the same result every time", workload.Name())
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
	composite, steps := workload.(runtime.Composite)
	if able := c.ableLocked(workload.Name(), spec.GetMinMemoryBytes(), int(spec.GetMinGpus()), needs); verify >= 2 && !steps && able < verify {
		return nil, status.Errorf(codes.FailedPrecondition, "verify asks for %d different workers to run each task, and %d connected now could take this job%s", verify, able, c.apart())
	}
	if submitter != "" {
		jobs, tasks := c.unfinishedOfLocked(submitter)
		if parent == "" && c.perClient > 0 && jobs >= c.perClient {
			return nil, status.Errorf(codes.ResourceExhausted, "this client has %d jobs unfinished, as many as this coordinator lets one client have; submit again once one is over", jobs)
		}
		switch {
		case c.tasksPerClient == 0:
		case steps:
			// The tasks of a job of jobs are its steps', counted as each
			// step begins and is split, like any job; the step then fails
			// the job if it does not fit. One that asks for more tasks
			// than a client may have could never fit.
			for _, payload := range payloads {
				if name, asks := composite.Asks(payload); asks > c.tasksPerClient {
					return nil, status.Errorf(codes.ResourceExhausted, "step %s asks for %d tasks, more than the %d this coordinator lets one client have; split it smaller", name, asks, c.tasksPerClient)
				}
			}
		case tasks+len(payloads) > c.tasksPerClient:
			return nil, status.Errorf(codes.ResourceExhausted, "this job has %d tasks and this client has %d unfinished, more between them than the %d this coordinator lets one client have; submit it once some are over, or split it smaller", len(payloads), tasks, c.tasksPerClient)
		}
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
	job.AuditShare = audit
	job.TaskTimeout = time.Duration(spec.GetTaskTimeoutSeconds()) * time.Second
	if submitter != "" && c.maxTaskTimeout > 0 && (job.TaskTimeout == 0 || job.TaskTimeout > c.maxTaskTimeout) {
		job.TaskTimeout = c.maxTaskTimeout
	}
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
	c.noteSubmitter(job)
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
	c.mu.Lock()
	c.addresses[id] = c.addressOf(stream.Context())
	c.mu.Unlock()
	w := &worker{
		id:             id,
		name:           hello.GetName(),
		serveAddress:   hello.GetServeAddress(),
		relayAddresses: hello.GetRelayAddresses(),
		dialled:        global(hello.GetDialledAddress()),
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
		ObservedAddress: seenAt(c.sourceOf(stream.Context())),
	}}})
	c.scheduleLocked()
	c.mu.Unlock()
	if c.workersChanged != nil {
		c.workersChanged()
	}
	c.notifyRelaysChanged()
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
				relayedConnections, relayedBytes := kind.Heartbeat.GetRelayedConnections(), kind.Heartbeat.GetRelayedBytes()
				workerStateChanged := w.relayedConnections != relayedConnections || w.relayedBytes != relayedBytes
				w.relayedConnections, w.relayedBytes = relayedConnections, relayedBytes
				if changed := kind.Heartbeat.GetLabels(); changed != nil {
					// What it has now may be what a waiting job needs.
					labels := labelsOf(w.id, w.name, changed.GetLabels())
					workerStateChanged = workerStateChanged || !slices.Equal(w.capabilities.GetLabels(), labels)
					w.capabilities.Labels = labels
					c.scheduleLocked()
				}
				c.mu.Unlock()
				if workerStateChanged && c.workersChanged != nil {
					c.workersChanged()
				}
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
		// Either way the attempt is over, and what it uploaded with it:
		// a stopping coordinator takes in no more results, so nothing it
		// uploaded can be named by one.
		c.releaseLocked(a)
		c.settleLocked(a.job)
		c.notifyLocked(a.job)
	}
	c.log.Info("worker disconnected", "node", w.id, "lost_tasks", len(w.running))
	// The reader may still deliver a result that was already on its way.
	// With nothing recorded as running, it is dropped like any other result
	// for a task the worker does not hold.
	w.running = make(map[string]assignment)
	c.notifyExpiryChangedLocked()
	c.scheduleLocked()
	c.mu.Unlock()
	if c.workersChanged != nil {
		c.workersChanged()
	}
	c.notifyRelaysChanged()
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
	c.notifyExpiryChangedLocked()
	defer c.scheduleLocked()
	// What the attempt uploaded is kept further only if its result names
	// it, which keep does below.
	defer c.releaseLocked(a)
	// What a model task says it ran with stays with its attempt however
	// the attempt ends. It is the worker's word, kept, not checked.
	if used := result.GetUsed(); used != nil {
		a.job.Used(a.task, a.attempt, jobmodel.ModelUse{
			Server: used.GetServer(), Version: used.GetVersion(),
			Model: used.GetModel(), Digest: used.GetDigest(), Quantization: used.GetQuantization(),
			Options: used.GetOptions(),
		})
	}

	// A job that is over has had its tasks taken off every worker's hands,
	// so a task found here belongs to one that is still going.
	now := time.Now()
	if named := len(result.GetReadBlobs()) + len(result.GetWrittenBlobs()); named > maxTaskBlobs {
		// Every blob named is noted with the job and pinned for it.
		result = &pb.TaskResult{Outcome: &pb.TaskResult_Error{Error: fmt.Sprintf("the worker named %d blobs as read and stored by the task, and a task may have %d", named, maxTaskBlobs)}}
	}
	switch outcome := result.GetOutcome().(type) {
	case *pb.TaskResult_Output:
		c.log.Info("task returned", "task", a.task.ID, "worker", w.name, "took", now.Sub(a.started).Round(time.Millisecond).String())
		// What the task stored must outlive the hour a new blob is kept
		// for, since the job may run longer than that. Only what the store
		// holds is taken as stored: the worker's word for it is not enough.
		// What a task stored is held to its client's share once it is the
		// task's result: at once if the worker's word is taken for it, and
		// otherwise when the task is settled. Until then it is staged: kept
		// for the task, and counted for nobody, so that a result that is
		// not taken costs the client nothing.
		replicated := c.replicated(a.job)
		picked := !replicated && c.pickedLocked(a.job)
		pinOwner, limit := owner(a.job), c.shareOf(a.job)
		if replicated || picked {
			pinOwner, limit = staging(a.job, a.task), 0
		}
		written, over := c.keep(a.job, w, pinOwner, result.GetWrittenBlobs(), limit)
		if over != nil {
			c.refuseLocked(a.job, a.task, w.name, over, now)
			break
		}
		read := wellFormed(result.GetReadBlobs())
		a.job.NoteRead(read...)
		a.job.NoteTaskOutput(written...)
		// Who holds what is a worker's word for where a blob may be
		// fetched, and whoever fetches checks what comes against its CID.
		for _, held := range append(result.GetReadBlobs(), result.GetWrittenBlobs()...) {
			w.holds[held] = struct{}{}
		}
		done := false
		if replicated {
			// One result among several, which settles the task only if
			// enough of them are the same.
			done = c.returnedLocked(w, a, outcome.Output, written)
		} else if picked {
			// Not taken yet: this node runs the task itself first, and
			// the task is over when it has.
			c.recordLocked(a.job, eventTaskResult, a.task.Index, w.name, "returned a result, which the coordinator checks by running the task itself")
			c.aggregating.Add(1)
			go c.audit(w.id, w.name, a, outcome.Output, written)
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
	if err == nil {
		if references, ok := workload.(runtime.ResultBlobs); ok {
			// A workload can only retain task blobs the coordinator accepted
			// and pinned; a worker's aggregate JSON is not proof a CID exists.
			written := make(map[string]struct{})
			for _, c := range job.IntermediateBlobs() {
				written[c] = struct{}{}
			}
			var retained []string
			for _, c := range references.ResultBlobs(result) {
				if _, exists := written[c]; exists {
					retained = append(retained, c)
				}
			}
			job.NoteResult(retained...)
		}
	}
	job.Finish(result, err, now)
	c.settleLocked(job)
	c.log.Info("job finished", "job", job.ID, "state", job.State.String(), "took", now.Sub(job.CreatedAt).String(), "by", workedBy(job))
	c.notifyLocked(job)
	c.scheduleLocked()
}

// workedBy says which workers did a job's tasks and how many each did, as
// "laptop×12 r620×20", most first.
func workedBy(job *jobmodel.Job) string {
	counts := map[string]int{}
	for _, task := range job.Tasks {
		if task.NodeName != "" {
			counts[task.NodeName]++
		}
	}
	names := slices.Collect(maps.Keys(counts))
	slices.SortFunc(names, func(a, b string) int {
		if c := cmp.Compare(counts[b], counts[a]); c != 0 {
			return c
		}
		return strings.Compare(a, b)
	})
	said := make([]string, len(names))
	for i, name := range names {
		said[i] = fmt.Sprintf("%s×%d", name, counts[name])
	}
	return strings.Join(said, " ")
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
	// Whatever its tasks' results staged, including any a stop left
	// behind before it was noted with a result, is of no use now.
	if err := c.store.UnpinPrefix(owner(job) + stagingSeparator); err != nil {
		c.log.Warn("could not release what a job's results staged", "job", job.ID, "error", err)
	}
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
// noted. They are pinned under pinOwner, and with a limit, only if that
// keeps the job's client within it; if not, none are, and keep returns why.
func (c *Coordinator) keep(job *jobmodel.Job, w *worker, pinOwner string, cids []string, limit uint64) ([]string, *storage.OverShareError) {
	if limit > 0 {
		c.count()
	}
	if all := decode(cids); len(all) == len(cids) {
		err := c.store.PinWithin(c.ctx, pinOwner, time.Time{}, limit, all...)
		if over := (*storage.OverShareError)(nil); errors.As(err, &over) {
			return nil, over
		}
		if err == nil {
			return cids, nil
		}
	}
	// Some are not stored blobs, or the store could not pin them: ask
	// which it holds, and pin those.
	var stored []string
	var held []cid.Cid
	for _, named := range cids {
		blob, err := cid.Decode(named)
		if err == nil {
			var has bool
			if has, err = c.store.Has(c.ctx, blob); err != nil {
				c.log.Warn("could not ask the store for a job's blob", "job", job.ID, "blob", named, "error", err)
				err = nil
			} else if !has {
				err = storage.ErrNotFound
			}
		}
		if err != nil {
			c.log.Warn("a worker named a blob as stored by its task that the pool does not hold", "job", job.ID, "node", w.id, "blob", named)
			continue
		}
		stored, held = append(stored, named), append(held, blob)
	}
	err := c.store.PinWithin(c.ctx, pinOwner, time.Time{}, limit, held...)
	if over := (*storage.OverShareError)(nil); errors.As(err, &over) {
		return nil, over
	}
	if err != nil {
		c.log.Warn("could not pin a job's blobs", "job", job.ID, "error", err)
	}
	return stored, nil
}

// refuseLocked fails a task whose outputs would take its job's client past
// its share, and the job with it. The same task would store the same again,
// so it is not tried again, and its workers did what they were asked.
func (c *Coordinator) refuseLocked(job *jobmodel.Job, task *jobmodel.Task, by string, over *storage.OverShareError, now time.Time) {
	reason := fmt.Sprintf("its outputs would have this client keep %d bytes here, and it may keep %d; free some and submit the job again", over.Keeps, over.May)
	c.recordLocked(job, eventTaskFailed, task.Index, by, reason)
	job.Refuse(task, reason, now)
}

// chargeLocked holds what a task was settled with to its job's client's
// share, as keep does what a worker's word is taken for, and releases what
// was staged for the task. It reports whether the client may keep it; if
// not, nothing more is kept for the client, and the task fails.
func (c *Coordinator) chargeLocked(job *jobmodel.Job, task *jobmodel.Task, taken, staged []string, now time.Time) bool {
	defer c.unstage(job, task, staged)
	limit := c.shareOf(job)
	if limit > 0 {
		c.count()
	}
	err := c.store.PinWithin(c.ctx, owner(job), time.Time{}, limit, decode(taken)...)
	if over := (*storage.OverShareError)(nil); errors.As(err, &over) {
		c.refuseLocked(job, task, "", over, now)
		return false
	}
	if err != nil {
		c.log.Warn("could not pin a job's blobs", "job", job.ID, "error", err)
	}
	return true
}

// unstage releases what was staged for a task.
func (c *Coordinator) unstage(job *jobmodel.Job, task *jobmodel.Task, cids []string) {
	if len(cids) == 0 {
		return
	}
	if err := c.store.Unpin(staging(job, task), decode(cids)...); err != nil {
		c.log.Warn("could not release what a task's results staged", "job", job.ID, "task", task.Index, "error", err)
	}
}

// staged is every blob the results a task took in stored.
func staged(task *jobmodel.Task) []string {
	var cids []string
	for _, r := range task.Results {
		cids = append(cids, r.Blobs...)
	}
	return cids
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
		return runtime.Open(blobs)
	}
	// The coordinator holds the sealing key itself, which opens whatever
	// was sealed with any key derived from it. What it stores for the job
	// it seals as the job's workers do, with the job's own.
	return runtime.Sealed(blobs, sealed.Key(key), sealed.Key(key).ForJob(jobID))
}

// unfinishedOfLocked is how many unfinished jobs, not counting steps, the
// client submitter has, and how many unfinished tasks, steps' included.
func (c *Coordinator) unfinishedOfLocked(submitter string) (jobs, tasks int) {
	for _, job := range c.active {
		if job.Submitter != submitter {
			continue
		}
		if job.Parent == "" {
			jobs++
		}
		// The tasks of a job of jobs are its steps, whose own tasks are
		// counted with them.
		if _, steps := c.composite(job); steps {
			continue
		}
		for _, task := range job.Tasks {
			if task.State == jobmodel.Pending || task.State == jobmodel.Running {
				tasks++
			}
		}
	}
	return jobs, tasks
}

// needs is what a worker must have to be given the tasks of a job: what
// its workload asks for; for a private job, that it takes keys as they are
// now given; and for a job whose tasks run in containers, on a coordinator
// that asks it, that it runs containers hardened.
func (c *Coordinator) needs(workload string, params []byte, private bool) []string {
	needs := c.workloads.Needs(workload, params)
	if private {
		needs = append(needs, runtime.SealingLabel)
	}
	if c.hardened && c.workloads.InContainers(workload) {
		needs = append(needs, runtime.HardenedLabels...)
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

// Uploading names the owner what a worker uploads for an attempt at a task
// is held under, if the worker holds that attempt now, and reports whether
// it does. It is the coordinator's own record of the attempt, not the
// worker's word, that a worker's upload is kept by.
func (c *Coordinator) Uploading(nodeID, taskID string, attempt uint32) (string, bool) {
	if c.storePerClient > 0 {
		// What the upload is held to counts what the client keeps.
		c.count()
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	w, ok := c.workers[nodeID]
	if !ok {
		return "", false
	}
	a, ok := w.running[taskID]
	if !ok || uint32(a.attempt) != attempt {
		return "", false
	}
	return uploads(a), true
}

// uploads is the name what a worker uploads for an attempt is held under
// while the attempt lasts. It is staged for the attempt's task, so that it
// counts for no client, a job that is still going holds it, and the job lets
// go of it when it ends. The name ends in a dot so that, as a prefix, it is
// no other attempt's.
func uploads(a assignment) string {
	return staging(a.job, a.task) + "@" + strconv.Itoa(a.attempt) + "."
}

// attemptUploads is what an attempt has uploaded while it lasts, and the
// client it is held to the share of: its job's submitter, or nobody for the
// node's own jobs or a coordinator that holds clients to no share.
type attemptUploads struct {
	used   uint64
	client string
	// copies is how many copies of the attempt's task are run to be
	// verified, each of which counts towards the client's share, or zero.
	copies int
}

// ChargeUpload counts n more bytes uploaded for the attempt Uploading named
// owner, and refuses them if they would take the attempt past what one may
// upload, or its job's client past its share. Until the attempt's task is
// settled what it uploads counts for no client in the store, so it is
// counted here instead, with what the client's other attempts have
// uploaded: those of one client's jobs that run at once share what is left
// of its share, and cannot each spend all of it. Each piece is counted as it
// arrives, so uploads made at the same time cannot together go past either.
// An attempt is counted from when it is handed out until it ends, so a
// piece that arrives after that, for an upload begun while it lasted, is
// refused rather than counted afresh.
//
// What a client keeps is as the store counts it, which Uploading has it
// start doing: it cannot start while an upload is being stored, as each
// piece of one is counted here.
func (c *Coordinator) ChargeUpload(owner string, n uint64) error {
	c.uploadMu.Lock()
	defer c.uploadMu.Unlock()
	up, ok := c.uploaded[owner]
	if !ok {
		return status.Error(codes.FailedPrecondition, "the attempt this upload is for has ended")
	}
	if c.perAttempt != 0 && up.used+n > c.perAttempt {
		return status.Errorf(codes.ResourceExhausted, "this attempt has uploaded %d bytes, and an attempt may upload %d", up.used, c.perAttempt)
	}
	if up.client != "" {
		kept, flying := c.store.GroupUsage(up.client, time.Now()), c.inFlight[up.client]
		if kept+flying+n > c.storePerClient {
			why := fmt.Sprintf("the client this job is for keeps %d bytes here and has %d more on their way, and may keep %d", kept, flying, c.storePerClient)
			if up.copies >= 2 {
				why += fmt.Sprintf("; each of this task's %d verified copies counts towards it", up.copies)
			}
			return status.Error(codes.ResourceExhausted, why)
		}
		c.inFlight[up.client] = flying + n
	}
	up.used += n
	c.uploaded[owner] = up
	return nil
}

// Uploaded reports how much the attempt Uploading named owner has uploaded,
// and whether it is still counted, that is, whether it has not ended.
func (c *Coordinator) Uploaded(owner string) (uint64, bool) {
	c.uploadMu.Lock()
	defer c.uploadMu.Unlock()
	up, ok := c.uploaded[owner]
	return up.used, ok
}

// releaseLocked lets go of what was uploaded for an attempt that has ended.
func (c *Coordinator) releaseLocked(a assignment) {
	c.uploadMu.Lock()
	// What it uploaded is no longer on its way for its client: it is let
	// go of, or kept for the job, and counted as the job's pins are.
	if up := c.uploaded[uploads(a)]; up.client != "" {
		if flying := c.inFlight[up.client]; flying > up.used {
			c.inFlight[up.client] = flying - up.used
		} else {
			delete(c.inFlight, up.client)
		}
	}
	delete(c.uploaded, uploads(a))
	c.uploadMu.Unlock()
	if err := c.store.UnpinPrefix(uploads(a)); err != nil {
		c.log.Warn("could not let go of what an attempt uploaded", "task", a.task.ID, "attempt", a.attempt, "error", err)
	}
}

// staging is the name what the results of one of a job's tasks stored is
// held under until the task is settled. It counts for no client.
func staging(job *jobmodel.Job, task *jobmodel.Task) string {
	return owner(job) + stagingSeparator + strconv.Itoa(task.Index)
}

const stagingSeparator = "#"

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

// scheduleLocked hands pending tasks to workers with free slots. A free slot
// goes to the task of whichever submitter has the fewest tasks running, of
// that submitter's oldest job, so that one client's job of many tasks does
// not hold every slot while another's waits; with only one submitter
// waiting, it has every slot there is. Each copy of a verified job's task
// takes a slot, and is shared out the same way.
func (c *Coordinator) scheduleLocked() {
	c.active = slices.DeleteFunc(c.active, func(job *jobmodel.Job) bool { return job.Terminal() })
	var waiting []*waitingJob
	for _, job := range c.active {
		if composite, is := c.composite(job); is {
			// Its tasks are jobs, which the coordinator submits itself.
			c.stepsLocked(job, composite)
			continue
		}
		next := &waitingJob{job: job, copies: c.replicated(job)}
		if next.copies {
			// Each of its tasks goes to several workers at once.
			next.tasks = wanted(job)
		} else {
			next.tasks = job.Assignable()
		}
		if len(next.tasks) > 0 {
			waiting = append(waiting, next)
		}
	}
	running := c.runningBySubmitterLocked()
	for len(waiting) > 0 {
		// The first of the fewest is the oldest job of that submitter.
		i := 0
		for j, w := range waiting {
			if running[w.job.Submitter] < running[waiting[i].job.Submitter] {
				i = j
			}
		}
		next := waiting[i]
		task := next.tasks[0]
		switch w := c.pickWorkerLocked(next.job, task); {
		case w == nil && next.copies:
			// Every free worker has this task already; another of the
			// job's tasks may still have one it has not asked.
			next.tasks = next.tasks[1:]
		case w == nil:
			// Nowhere to put it now; the rest of the job waits with it.
			next.tasks = nil
		case next.copies:
			running[next.job.Submitter]++
			if c.copyLocked(next.job, task, w) {
				// Tasks it had done enough of want more copies now.
				next.tasks = wanted(next.job)
			} else if next.job.Wanted(task) == 0 {
				next.tasks = next.tasks[1:]
			}
		default:
			now := time.Now()
			next.job.Start(task, w.id, w.name, now)
			c.handLocked(w, assignment{job: next.job, task: task, attempt: task.Attempt, started: now})
			running[next.job.Submitter]++
			next.tasks = next.tasks[1:]
		}
		if len(next.tasks) == 0 {
			waiting = slices.Delete(waiting, i, i+1)
		}
	}
}

// wanted is the tasks of a verified job that want more copies.
func wanted(job *jobmodel.Job) []*jobmodel.Task {
	var tasks []*jobmodel.Task
	for _, task := range job.Tasks {
		if job.Wanted(task) > 0 {
			tasks = append(tasks, task)
		}
	}
	return tasks
}

// waitingJob is a job with tasks to hand out, and those tasks. copies says
// that each of them goes to several workers, as long as it wants more.
type waitingJob struct {
	job    *jobmodel.Job
	tasks  []*jobmodel.Task
	copies bool
}

// runningBySubmitterLocked is how many tasks each submitter has running on
// workers now; the node's own jobs are counted under "".
func (c *Coordinator) runningBySubmitterLocked() map[string]int {
	running := make(map[string]int)
	for _, w := range c.workers {
		for _, a := range w.running {
			running[a.job.Submitter]++
		}
	}
	return running
}

// handLocked gives a worker an attempt at a task.
func (c *Coordinator) handLocked(w *worker, a assignment) {
	c.recordLocked(a.job, eventTaskStarted, a.task.Index, w.name, fmt.Sprintf("attempt %d", a.attempt))
	c.log.Info("task given", "task", a.task.ID, "worker", w.name, "attempt", a.attempt)
	w.running[a.task.ID] = a
	c.notifyExpiryChangedLocked()
	c.uploadMu.Lock()
	up := attemptUploads{}
	if c.shareOf(a.job) > 0 {
		up.client, up.copies = a.job.Submitter, a.task.Verify
	}
	c.uploaded[uploads(a)] = up
	c.uploadMu.Unlock()
	keys := c.keys[a.job.ID]
	w.send.add(&pb.CoordinatorMessage{Kind: &pb.CoordinatorMessage_Assignment{Assignment: &pb.TaskAssignment{
		TaskId:   a.task.ID,
		JobId:    a.job.ID,
		Attempt:  uint32(a.attempt),
		Workload: a.job.Workload,
		Payload:  a.task.Payload,
		Key:      keys.GetKey(),
		Keys:     keys.GetKeys(),
		Needs:    a.job.Needs,
	}}})
	c.notifyLocked(a.job)
}

// pickWorkerLocked returns the worker with the most free slots that supports
// the workload, or nil if none has a free slot. A worker that has a copy of
// the task, or has returned a result for it, is passed over: only a task of
// a verified job has such workers while it waits for another.
//
// A worker that an attempt at the task has already failed on, or been lost
// with, is the last to be given it again. While another that could run it
// is connected the task goes there, waiting for a slot if it must; it goes
// back to where it went wrong only when there is nowhere else.
//
// A task that needs a language model goes first to the worker with the
// fewest tasks on that model already: a model server answers one at a time,
// or nearly, however many slots its worker has. Among those, it goes to the
// one with the model in memory and most of it on a graphics card, where the
// reply comes soonest.
func (c *Coordinator) pickWorkerLocked(job *jobmodel.Job, task *jobmodel.Task) *worker {
	model := runtime.ModelNeeded(job.Needs)
	var best, again *worker
	var bestRank, againRank rank
	elsewhere := false
	for _, w := range c.workers {
		if !suits(w.capabilities, job.Workload, job.MinMemory, job.MinGPUs, job.Needs) || task.Asked(w.id) || c.besideLocked(task, w.id) {
			continue
		}
		r := rank{free: int(w.capabilities.GetTaskSlots()) - len(w.running), id: w.id}
		if model != "" {
			r.onModel, r.onCard = w.onModel(model), onCard(w.capabilities.GetLabels(), model)
		}
		if task.Dropped(w.id) {
			if r.free > 0 && (again == nil || r.before(againRank)) {
				again, againRank = w, r
			}
			continue
		}
		elsewhere = true
		if r.free > 0 && (best == nil || r.before(bestRank)) {
			best, bestRank = w, r
		}
	}
	if best == nil && !elsewhere {
		return again
	}
	return best
}

// rank is what decides between workers that could be given a task.
type rank struct {
	// onModel is how many tasks the worker is running on the model the task
	// needs, and onCard how much of that model it has on a graphics card: -1
	// if the model is not in memory. Both are zero for a task that needs no
	// model.
	onModel, onCard int
	free            int
	id              string
}

// before reports whether a worker ranked r is to be given the task before
// one ranked other.
func (r rank) before(other rank) bool {
	switch {
	case r.onModel != other.onModel:
		return r.onModel < other.onModel
	case r.onCard != other.onCard:
		return r.onCard > other.onCard
	case r.free != other.free:
		return r.free > other.free
	}
	return r.id < other.id
}

// onModel counts the tasks a worker is running that need a model.
func (w *worker) onModel(model string) int {
	n := 0
	for _, a := range w.running {
		if runtime.ModelNeeded(a.job.Needs) == model {
			n++
		}
	}
	return n
}

// onCard is how much of a model a worker's labels say is on its graphics
// cards, or -1 if they do not say it is in memory.
func onCard(labels []string, model string) int {
	for _, loaded := range runtime.LoadedModels(labels) {
		if loaded.Model == model {
			return loaded.OnCard
		}
	}
	return -1
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
	if job.Terminal() && c.pruneChanged != nil {
		c.pruneChanged()
	}
	c.wakeLocked(job)
}

// wakeLocked wakes whoever is watching a job or following its events.
func (c *Coordinator) wakeLocked(job *jobmodel.Job) {
	close(c.changed[job.ID])
	c.changed[job.ID] = make(chan struct{})
	if c.jobsChanged != nil {
		c.jobsChanged()
	}
}

func newID() string {
	var b [8]byte
	rand.Read(b[:]) // never fails; see crypto/rand
	return hex.EncodeToString(b[:])
}

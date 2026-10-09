package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ipfs/go-cid"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/status"

	"github.com/sisyphus-network/Sisyphus/apps/sisyphusd/access"
	"github.com/sisyphus-network/Sisyphus/apps/sisyphusd/api"
	"github.com/sisyphus-network/Sisyphus/apps/sisyphusd/blobclient"
	"github.com/sisyphus-network/Sisyphus/apps/sisyphusd/coordinator"
	"github.com/sisyphus-network/Sisyphus/apps/sisyphusd/replication"
	"github.com/sisyphus-network/Sisyphus/apps/sisyphusd/worker"
	"github.com/sisyphus-network/Sisyphus/packages/identity"
	pb "github.com/sisyphus-network/Sisyphus/packages/protocol/sisyphus/v1"
	"github.com/sisyphus-network/Sisyphus/packages/runtime"
	"github.com/sisyphus-network/Sisyphus/packages/storage"
)

// These tests run a real coordinator and real workers in one process, talking
// gRPC over loopback TCP.

const primesBelowTwoMillion = `{"count":148933}`

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

// testRetain is how long test coordinators keep a finished job's blobs.
const testRetain = 24 * time.Hour

type pool struct {
	t         *testing.T
	ctx       context.Context
	addr      string
	client    pb.NodeServiceClient
	blobs     pb.BlobServiceClient
	workloads *runtime.Registry
	// store is the coordinator's blob store; workerStores are the workers'.
	store        *storage.Store
	workerStores map[string]*storage.Store
	// blobGets counts downloads served by the coordinator.
	blobGets *atomic.Int32
	// coord is the coordinator itself, for calls that bypass the network,
	// and conn a connection to it for clients the helpers do not cover.
	coord *coordinator.Coordinator
	conn  *grpc.ClientConn
	// heartbeat, if set, is how often workers started later report in, and
	// tune, if set, changes each such worker before it starts.
	heartbeat time.Duration
	tune      func(*worker.Worker)
	// logs is everything the coordinator has logged.
	logs *syncBuffer
	// stop shuts the coordinator down as a node being stopped would, before
	// the test ends.
	stop func()
	// ident is the coordinator's identity and access its list of admitted
	// nodes; workerIdents are the identities of workers started by name.
	ident        *identity.Identity
	access       *access.List
	workerIdents map[string]*identity.Identity
	// rawIdents are the identities of hand-driven workers.
	rawIdents map[*rawWorker]*identity.Identity
	// names is where the coordinator keeps the records of names.
	names *nameShelf
	// copies are the stores in which the storage followers started by name
	// keep what they hold for the pool, lists the files in which each keeps
	// the list its coordinator signed, addresses where each serves blobs,
	// and unfollow how to stop each following while it goes on serving.
	copies    map[string]*storage.Store
	lists     map[string]string
	addresses map[string]string
	unfollow  map[string]func()
}

// newIdentity makes a node identity that lasts for the test.
func newIdentity(t *testing.T) *identity.Identity {
	t.Helper()
	ident, _, err := identity.LoadOrCreate(filepath.Join(t.TempDir(), "node.key"))
	if err != nil {
		t.Fatal(err)
	}
	return ident
}

// admit creates a new node identity, admits it to the pool in the given
// role, and returns it with the credentials it connects with.
func (p *pool) admit(role access.Role) (*identity.Identity, credentials.TransportCredentials) {
	p.t.Helper()
	ident := newIdentity(p.t)
	if err := p.access.Admit(ident.ID(), role, time.Now()); err != nil {
		p.t.Fatal(err)
	}
	return ident, p.credentialsFor(ident)
}

// credentialsFor returns what ident connects to the coordinator with.
func (p *pool) credentialsFor(ident *identity.Identity) credentials.TransportCredentials {
	return credentials.NewTLS(ident.ClientTLS(p.ident.ID()))
}

// syncBuffer is a bytes.Buffer safe to write from one goroutine while
// another reads.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func countBlobGets(n *atomic.Int32) grpc.StreamServerInterceptor {
	return func(srv any, stream grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		if info.FullMethod == pb.BlobService_Get_FullMethodName {
			n.Add(1)
		}
		return handler(srv, stream)
	}
}

func startPool(t *testing.T, workloads *runtime.Registry) *pool {
	t.Helper()
	return startPoolOver(t, workloads, func(store *storage.Store) coordinator.Store { return store })
}

// startPoolOver is startPool with the coordinator's view of its store
// wrapped, for tests that make the store misbehave.
func startPoolOver(t *testing.T, workloads *runtime.Registry, wrap func(*storage.Store) coordinator.Store) *pool {
	t.Helper()
	return startPoolWith(t, workloads, wrap, nil)
}

// startPoolWith starts a pool whose coordinator keeps its jobs in journal,
// if one is given, and takes up the jobs already there.
func startPoolWith(t *testing.T, workloads *runtime.Registry, wrap func(*storage.Store) coordinator.Store, journal coordinator.Journal) *pool {
	t.Helper()
	return startPoolKeeping(t, workloads, wrap, journal, 0)
}

// startPoolKeeping is startPoolWith for a pool whose coordinator has that
// many storage followers hold a copy of whatever it has pinned.
func startPoolKeeping(t *testing.T, workloads *runtime.Registry, wrap func(*storage.Store) coordinator.Store, journal coordinator.Journal, replicas int) *pool {
	t.Helper()
	return startPoolAs(t, workloads, wrap, journal, replication.Config{Identity: newIdentity(t), StoreID: "first", Replicas: replicas})
}

// startPoolAs is startPoolKeeping for a pool whose coordinator has the key,
// the name for its store, and the rest that copies says.
func startPoolAs(t *testing.T, workloads *runtime.Registry, wrap func(*storage.Store) coordinator.Store, journal coordinator.Journal, copies replication.Config) *pool {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	store := storage.NewMemory()
	logs := new(syncBuffer)
	ident := copies.Identity
	// Kept in memory: an access list with no file saves nothing.
	admitted, err := access.Open(access.InMemory(), ident.ID())
	if err != nil {
		t.Fatal(err)
	}
	coord := coordinator.New(coordinator.Config{
		ID: ident.ID(), Workloads: workloads, Store: wrap(store), Journal: journal, Retain: testRetain,
		Log: slog.New(slog.NewTextHandler(logs, nil)),
	})
	if _, err := coord.Recover(); err != nil {
		t.Fatal(err)
	}
	gets := new(atomic.Int32)
	kept := &nameShelf{records: make(map[string][]byte)}
	copies.Store, copies.Log, copies.Address = store, slog.New(slog.NewTextHandler(logs, nil)), coord.ServeAddress
	copies.Fetch = func(ctx context.Context, from *pb.BlobHolder, c cid.Cid, into blobclient.Putter) error {
		return worker.FetchFromPeer(ctx, from, c, into, func(nodeID string) credentials.TransportCredentials {
			return credentials.NewTLS(ident.ClientTLS(nodeID))
		}, nil)
	}
	replicated := replication.New(copies)
	srv := api.NewServer(
		api.Config{Identity: ident, Access: admitted, Coordinator: coord, Store: store, Replication: replicated, Names: api.NewNames(kept, func(id string) bool {
			_, admitted := admitted.Role(id)
			return admitted
		})},
		grpc.StreamInterceptor(countBlobGets(gets)),
	)
	go srv.Serve(lis)
	stop := sync.OnceFunc(func() {
		coord.Close()
		srv.Stop()
	})
	t.Cleanup(stop)

	// The helpers' own calls are made as the node's owner.
	conn, err := grpc.NewClient(lis.Addr().String(), grpc.WithTransportCredentials(credentials.NewTLS(ident.ClientTLS(ident.ID()))))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	return &pool{
		t: t, ctx: ctx, addr: lis.Addr().String(), workloads: workloads,
		client: pb.NewNodeServiceClient(conn), blobs: pb.NewBlobServiceClient(conn),
		store: store, blobGets: gets, workerStores: make(map[string]*storage.Store),
		coord: coord, conn: conn, logs: logs, stop: stop,
		ident: ident, access: admitted, workerIdents: make(map[string]*identity.Identity),
		rawIdents: make(map[*rawWorker]*identity.Identity),
		names:     kept,
		copies:    make(map[string]*storage.Store), lists: make(map[string]string),
		addresses: make(map[string]string), unfollow: make(map[string]func()),
	}
}

// startWorker runs a worker until the test ends or the returned stop function
// is called, whichever comes first. stop waits for the worker to exit.
func (p *pool) startWorker(id string, slots int) (stop func()) {
	p.t.Helper()
	return p.startMember(id, slots, nil, "")
}

// startFollower runs a worker that is also a storage follower, as one
// started with --replica-dir is.
func (p *pool) startFollower(id string) (stop func()) {
	p.t.Helper()
	return p.startMember(id, 1, storage.NewMemory(), filepath.Join(p.t.TempDir(), "kept.list"))
}

// startMember runs a worker which, given a store for them and a file for
// the list its coordinator signs, also holds copies of the pool's data as
// the coordinator directs.
func (p *pool) startMember(id string, slots int, copies *storage.Store, list string) (stop func()) {
	p.t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	// Like a worker-only node, each test worker has a store of its own.
	local := storage.NewMemory()
	p.workerStores[id] = local
	ident, creds := p.admit(access.Worker)
	p.workerIdents[id] = ident
	blobs, err := worker.DialBlobs(p.addr, creds, local, 0)
	if err != nil {
		p.t.Fatal(err)
	}
	p.t.Cleanup(func() { blobs.Close() })
	// Like a worker started with --serve, it lets its fellows fetch from it
	// and fetches from them.
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		p.t.Fatal(err)
	}
	var served api.Opener = local
	following := make(chan struct{})
	if copies == nil {
		close(following)
	} else {
		p.copies[id], p.lists[id], served = copies, list, api.Stores(copies, local)
		follower := &replication.Follower{Store: copies, ListFile: list, Coordinator: pb.NewBlobServiceClient(blobs.Conn()), Log: quiet}
		asking, unfollow := context.WithCancel(ctx)
		go func() {
			defer close(following)
			follower.Run(asking)
		}()
		p.unfollow[id] = func() {
			unfollow()
			<-following
		}
	}
	p.addresses[id] = lis.Addr().String()
	peers := api.NewPeerServer(ident, served, worker.NewMembers(blobs.Conn()).IsMember)
	go peers.Serve(lis)
	p.t.Cleanup(peers.Stop)
	blobs.PeerCredentials = func(nodeID string) credentials.TransportCredentials {
		return credentials.NewTLS(ident.ClientTLS(nodeID))
	}
	w := &worker.Worker{
		Name: id, Coordinator: p.addr, Credentials: creds, Slots: slots, Workloads: p.workloads, Blobs: blobs, Log: quiet,
		HeartbeatInterval: p.heartbeat, ServeAddress: lis.Addr().String(),
	}
	if p.tune != nil {
		p.tune(w)
	}
	go func() {
		defer close(done)
		w.Run(ctx)
	}()
	stop = func() {
		cancel()
		<-done
		<-following
	}
	p.t.Cleanup(stop)
	return stop
}

func (p *pool) waitForWorkers(n int) {
	p.t.Helper()
	for {
		listed, err := p.client.ListNodes(p.ctx, &pb.ListNodesRequest{})
		if err != nil {
			p.t.Fatalf("waiting for %d workers: %v", n, err)
		}
		if len(listed.GetNodes()) == n {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func (p *pool) submit(spec *pb.JobSpec) *pb.Job {
	p.t.Helper()
	submitted, err := p.client.SubmitJob(p.ctx, &pb.SubmitJobRequest{Spec: spec})
	if err != nil {
		p.t.Fatal(err)
	}
	return submitted.GetJob()
}

// wait follows a job to its terminal state.
func (p *pool) wait(jobID string) *pb.Job {
	p.t.Helper()
	stream, err := p.client.WatchJob(p.ctx, &pb.WatchJobRequest{JobId: jobID})
	if err != nil {
		p.t.Fatal(err)
	}
	var last *pb.Job
	for {
		event, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return last
		}
		if err != nil {
			p.t.Fatalf("watching job %s: %v", jobID, err)
		}
		last = event.GetJob()
	}
}

func primesJob(mode pb.ScheduleMode, tasks uint32) *pb.JobSpec {
	return &pb.JobSpec{Workload: "primes", Params: []byte(`{"from":0,"to":2000000}`), Mode: mode, MaxTasks: tasks}
}

func TestDistributedJobRunsAcrossWorkers(t *testing.T) {
	p := startPool(t, runtime.Builtin())
	for _, id := range []string{"a", "b", "c"} {
		p.startWorker(id, 2)
	}
	p.waitForWorkers(3)

	job := p.wait(p.submit(primesJob(pb.ScheduleMode_SCHEDULE_MODE_DISTRIBUTED, 12)).GetJobId())

	if job.GetState() != pb.JobState_JOB_STATE_SUCCEEDED {
		t.Fatalf("job %v: %s", job.GetState(), job.GetError())
	}
	if string(job.GetResult()) != primesBelowTwoMillion {
		t.Errorf("result = %s, want %s", job.GetResult(), primesBelowTwoMillion)
	}
	if len(job.GetTasks()) != 12 {
		t.Errorf("job ran as %d tasks, want 12", len(job.GetTasks()))
	}
	ranOn := make(map[string]int)
	for _, task := range job.GetTasks() {
		if task.GetState() != pb.TaskState_TASK_STATE_SUCCEEDED || task.GetAttempt() != 1 {
			t.Errorf("task %d: %v after %d attempt(s)", task.GetIndex(), task.GetState(), task.GetAttempt())
		}
		ranOn[task.GetNodeName()]++
	}
	if len(ranOn) != 3 {
		t.Errorf("tasks ran on %v, want all three workers used", ranOn)
	}
	if job.GetFinishedAt() == nil {
		t.Error("finished job has no finish time")
	}
}

func TestDistributedJobDefaultsToOneTaskPerSlot(t *testing.T) {
	p := startPool(t, runtime.Builtin())
	p.startWorker("a", 2)
	p.startWorker("b", 3)
	p.waitForWorkers(2)

	job := p.wait(p.submit(primesJob(pb.ScheduleMode_SCHEDULE_MODE_DISTRIBUTED, 0)).GetJobId())

	if len(job.GetTasks()) != 5 {
		t.Errorf("job ran as %d tasks, want 5", len(job.GetTasks()))
	}
	if string(job.GetResult()) != primesBelowTwoMillion {
		t.Errorf("result = %s, want %s", job.GetResult(), primesBelowTwoMillion)
	}
}

func TestFullWorkerJobRunsAsOneTask(t *testing.T) {
	p := startPool(t, runtime.Builtin())
	p.startWorker("a", 4)
	p.startWorker("b", 4)
	p.waitForWorkers(2)

	// max_tasks is ignored in full-worker mode.
	job := p.wait(p.submit(primesJob(pb.ScheduleMode_SCHEDULE_MODE_FULL_WORKER, 8)).GetJobId())

	if len(job.GetTasks()) != 1 {
		t.Fatalf("job ran as %d tasks, want 1", len(job.GetTasks()))
	}
	if string(job.GetResult()) != primesBelowTwoMillion {
		t.Errorf("result = %s, want %s", job.GetResult(), primesBelowTwoMillion)
	}
}

func TestJobWaitsForAWorker(t *testing.T) {
	p := startPool(t, runtime.Builtin())
	submitted := p.submit(primesJob(pb.ScheduleMode_SCHEDULE_MODE_DISTRIBUTED, 3))
	if submitted.GetState() != pb.JobState_JOB_STATE_PENDING {
		t.Fatalf("job with no workers is %v, want pending", submitted.GetState())
	}

	p.startWorker("late", 1)

	job := p.wait(submitted.GetJobId())
	if string(job.GetResult()) != primesBelowTwoMillion {
		t.Errorf("result = %s (%s), want %s", job.GetResult(), job.GetError(), primesBelowTwoMillion)
	}
}

// gated is a workload whose tasks block until released, so a test can act
// while they are running.
type gated struct {
	started chan struct{}
	release chan struct{}
}

func (gated) Name() string { return "gated" }

func (gated) Split(_ context.Context, _ runtime.Blobs, _ []byte, parts int) ([][]byte, error) {
	return make([][]byte, parts), nil
}

func (g gated) Execute(ctx context.Context, _ runtime.Blobs, _ []byte) ([]byte, error) {
	g.started <- struct{}{}
	select {
	case <-g.release:
		return []byte("ok"), nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (gated) Aggregate(_ context.Context, _ runtime.Blobs, outputs [][]byte) ([]byte, error) {
	return []byte(strconv.Itoa(len(outputs))), nil
}

func TestTasksMoveToAnotherWorkerWhenTheirsDies(t *testing.T) {
	g := gated{started: make(chan struct{}, 16), release: make(chan struct{})}
	p := startPool(t, runtime.NewRegistry(g))

	stopDoomed := p.startWorker("doomed", 2)
	p.waitForWorkers(1)
	submitted := p.submit(&pb.JobSpec{Workload: "gated", MaxTasks: 2})
	<-g.started
	<-g.started

	// Kill the worker while it holds both tasks, then bring up a replacement.
	stopDoomed()
	p.waitForWorkers(0)
	p.startWorker("survivor", 2)
	<-g.started
	<-g.started
	close(g.release)

	job := p.wait(submitted.GetJobId())
	if job.GetState() != pb.JobState_JOB_STATE_SUCCEEDED {
		t.Fatalf("job %v: %s", job.GetState(), job.GetError())
	}
	if string(job.GetResult()) != "2" {
		t.Errorf("result = %s, want 2", job.GetResult())
	}
	for _, task := range job.GetTasks() {
		if task.GetNodeName() != "survivor" || task.GetAttempt() != 2 {
			t.Errorf("task %d finished on %q at attempt %d, want survivor at attempt 2", task.GetIndex(), task.GetNodeName(), task.GetAttempt())
		}
	}
}

type broken struct{}

func (broken) Name() string { return "broken" }

func (broken) Split(_ context.Context, _ runtime.Blobs, _ []byte, parts int) ([][]byte, error) {
	return make([][]byte, parts), nil
}

func (broken) Execute(context.Context, runtime.Blobs, []byte) ([]byte, error) {
	return nil, errors.New("disk on fire")
}

func (broken) Aggregate(context.Context, runtime.Blobs, [][]byte) ([]byte, error) {
	return nil, errors.New("unreachable")
}

func TestJobFailsAfterATaskExhaustsItsAttempts(t *testing.T) {
	p := startPool(t, runtime.NewRegistry(broken{}))
	p.startWorker("a", 1)
	p.waitForWorkers(1)

	job := p.wait(p.submit(&pb.JobSpec{Workload: "broken", MaxTasks: 1}).GetJobId())

	if job.GetState() != pb.JobState_JOB_STATE_FAILED {
		t.Fatalf("job is %v, want failed", job.GetState())
	}
	if want := "task 0 failed after 3 attempts: disk on fire"; job.GetError() != want {
		t.Errorf("error = %q, want %q", job.GetError(), want)
	}
	if task := job.GetTasks()[0]; task.GetState() != pb.TaskState_TASK_STATE_FAILED || task.GetAttempt() != 3 {
		t.Errorf("task is %v after %d attempts", task.GetState(), task.GetAttempt())
	}
}

type panicky struct{ broken }

func (panicky) Name() string { return "panicky" }

func (panicky) Execute(context.Context, runtime.Blobs, []byte) ([]byte, error) { panic("oh no") }

func TestPanickingWorkloadFailsTheTaskNotTheWorker(t *testing.T) {
	p := startPool(t, runtime.NewRegistry(panicky{}))
	p.startWorker("a", 1)
	p.waitForWorkers(1)

	job := p.wait(p.submit(&pb.JobSpec{Workload: "panicky", MaxTasks: 1}).GetJobId())

	if want := "task 0 failed after 3 attempts: workload panicked: oh no"; job.GetError() != want {
		t.Errorf("error = %q, want %q", job.GetError(), want)
	}
	p.waitForWorkers(1)
}

func TestANodeCanOnlyBeConnectedOnce(t *testing.T) {
	p := startPool(t, runtime.Builtin())
	p.startWorker("original", 1)
	p.waitForWorkers(1)

	// A second worker using the same key, whatever it calls itself.
	twin := &worker.Worker{
		Name: "copy", Coordinator: p.addr, Credentials: p.credentialsFor(p.workerIdents["original"]),
		Slots: 1, Workloads: p.workloads, Blobs: storage.NewMemory(), Log: quiet,
	}
	err := twin.Run(p.ctx)
	if status.Code(errors.Unwrap(err)) != codes.AlreadyExists {
		t.Errorf("second worker returned %v, want an AlreadyExists rejection", err)
	}
}

func TestInvalidRequestsAreRejected(t *testing.T) {
	p := startPool(t, runtime.Builtin())
	tests := []struct {
		name string
		call func() error
		want codes.Code
	}{
		{"unknown workload", func() error {
			_, err := p.client.SubmitJob(p.ctx, &pb.SubmitJobRequest{Spec: &pb.JobSpec{Workload: "nope"}})
			return err
		}, codes.InvalidArgument},
		{"bad params", func() error {
			_, err := p.client.SubmitJob(p.ctx, &pb.SubmitJobRequest{Spec: &pb.JobSpec{Workload: "primes", Params: []byte(`{"from":9,"to":1}`)}})
			return err
		}, codes.InvalidArgument},
		{"too many tasks", func() error {
			_, err := p.client.SubmitJob(p.ctx, &pb.SubmitJobRequest{Spec: primesJob(pb.ScheduleMode_SCHEDULE_MODE_DISTRIBUTED, 1_000_000)})
			return err
		}, codes.InvalidArgument},
		{"get unknown job", func() error {
			_, err := p.client.GetJob(p.ctx, &pb.GetJobRequest{JobId: "missing"})
			return err
		}, codes.NotFound},
		{"watch unknown job", func() error {
			stream, err := p.client.WatchJob(p.ctx, &pb.WatchJobRequest{JobId: "missing"})
			if err != nil {
				return err
			}
			_, err = stream.Recv()
			return err
		}, codes.NotFound},
	}
	for _, tt := range tests {
		if got := status.Code(tt.call()); got != tt.want {
			t.Errorf("%s: code %v, want %v", tt.name, got, tt.want)
		}
	}
}

// sampleText is a few megabytes of varied words, enough to span many storage
// chunks.
func sampleText() string {
	var text strings.Builder
	for i := 0; text.Len() < 3_000_000; i++ {
		fmt.Fprintf(&text, "Line %d: the boulder rolls down hill %d, and Sisyphus walks after it. ", i, i%97)
		if i%7 == 0 {
			text.WriteString("One must imagine him happy!\n")
		}
	}
	return text.String()
}

// wordCountLocally runs the wordcount workload in this process as a single
// task, giving the result a distributed run must reproduce exactly.
func wordCountLocally(t *testing.T, text string) (result string, table string) {
	t.Helper()
	ctx := context.Background()
	store := storage.NewMemory()
	input, err := store.Put(ctx, strings.NewReader(text))
	if err != nil {
		t.Fatal(err)
	}
	w := runtime.WordCount{}
	payloads, err := w.Split(ctx, store, []byte(`{"input":"`+input.String()+`"}`), 1)
	if err != nil {
		t.Fatal(err)
	}
	output, err := w.Execute(ctx, store, payloads[0])
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := w.Aggregate(ctx, store, [][]byte{output})
	if err != nil {
		t.Fatal(err)
	}
	var parsed runtime.WordCountResult
	if err := json.Unmarshal(encoded, &parsed); err != nil {
		t.Fatal(err)
	}
	blob, err := store.Open(ctx, cid.MustParse(parsed.Output))
	if err != nil {
		t.Fatal(err)
	}
	defer blob.Close()
	data, err := io.ReadAll(blob)
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded), string(data)
}

func TestWordCountJobMovesItsDataThroughTheStore(t *testing.T) {
	p := startPool(t, runtime.Builtin())
	for _, id := range []string{"a", "b", "c"} {
		p.startWorker(id, 2)
	}
	p.waitForWorkers(3)

	text := sampleText()
	input, err := blobclient.Upload(p.ctx, p.blobs, strings.NewReader(text))
	if err != nil {
		t.Fatal(err)
	}
	job := p.wait(p.submit(&pb.JobSpec{
		Workload: "wordcount",
		Params:   []byte(`{"input":"` + input.String() + `"}`),
		MaxTasks: 12,
	}).GetJobId())
	if job.GetState() != pb.JobState_JOB_STATE_SUCCEEDED {
		t.Fatalf("job %v: %s", job.GetState(), job.GetError())
	}

	wantResult, wantTable := wordCountLocally(t, text)
	if string(job.GetResult()) != wantResult {
		t.Fatalf("result %s, but a single local task gives %s", job.GetResult(), wantResult)
	}
	var result runtime.WordCountResult
	if err := json.Unmarshal(job.GetResult(), &result); err != nil {
		t.Fatal(err)
	}
	var table strings.Builder
	if err := blobclient.Download(p.ctx, p.blobs, cid.MustParse(result.Output), &table); err != nil {
		t.Fatalf("fetching the result blob: %v", err)
	}
	if table.String() != wantTable {
		t.Error("the result table differs from the one a single local task produces")
	}

	// Every worker ran tasks, so each must have pulled the input into its
	// own store, and only once however many tasks it ran. The one further
	// download is this test fetching the table.
	for id, store := range p.workerStores {
		if has, err := store.Has(p.ctx, input); err != nil || !has {
			t.Errorf("worker %s does not hold the input: has=%v err=%v", id, has, err)
		}
	}
	if got := p.blobGets.Load(); got != 4 {
		t.Errorf("coordinator served %d downloads, want 3 input fetches and 1 result fetch", got)
	}
}

func TestJobWithAMissingInputIsRejectedAtSubmission(t *testing.T) {
	p := startPool(t, runtime.Builtin())
	absent, err := storage.CID(p.ctx, strings.NewReader("never stored"))
	if err != nil {
		t.Fatal(err)
	}
	_, err = p.client.SubmitJob(p.ctx, &pb.SubmitJobRequest{Spec: &pb.JobSpec{
		Workload: "wordcount",
		Params:   []byte(`{"input":"` + absent.String() + `"}`),
	}})
	if status.Code(err) != codes.InvalidArgument || !strings.Contains(err.Error(), "blob not found") {
		t.Errorf("error %v, want InvalidArgument naming the missing blob", err)
	}
}

// slowAggregate is a workload whose tasks finish at once but whose
// aggregation blocks until released.
type slowAggregate struct {
	broken
	aggregating chan struct{}
	release     chan struct{}
}

func (slowAggregate) Name() string { return "slow-aggregate" }

func (slowAggregate) Execute(context.Context, runtime.Blobs, []byte) ([]byte, error) {
	return []byte("ok"), nil
}

func (s slowAggregate) Aggregate(ctx context.Context, _ runtime.Blobs, _ [][]byte) ([]byte, error) {
	close(s.aggregating)
	select {
	case <-s.release:
		return []byte("done"), nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func TestCoordinatorStaysResponsiveWhileAJobAggregates(t *testing.T) {
	slow := slowAggregate{aggregating: make(chan struct{}), release: make(chan struct{})}
	p := startPool(t, runtime.NewRegistry(slow, runtime.Primes{}))
	p.startWorker("a", 2)
	p.waitForWorkers(1)

	stuck := p.submit(&pb.JobSpec{Workload: "slow-aggregate", MaxTasks: 2})
	<-slow.aggregating

	got, err := p.client.GetJob(p.ctx, &pb.GetJobRequest{JobId: stuck.GetJobId()})
	if err != nil {
		t.Fatal(err)
	}
	if got.GetJob().GetState() != pb.JobState_JOB_STATE_RUNNING {
		t.Errorf("job is %v while aggregating, want running", got.GetJob().GetState())
	}
	for _, task := range got.GetJob().GetTasks() {
		if task.GetState() != pb.TaskState_TASK_STATE_SUCCEEDED {
			t.Errorf("task %d is %v while the job aggregates", task.GetIndex(), task.GetState())
		}
	}
	// Another job runs start to finish in the meantime.
	other := p.wait(p.submit(primesJob(pb.ScheduleMode_SCHEDULE_MODE_DISTRIBUTED, 2)).GetJobId())
	if string(other.GetResult()) != primesBelowTwoMillion {
		t.Errorf("a job submitted during another's aggregation: %s (%s)", other.GetResult(), other.GetError())
	}

	close(slow.release)
	if job := p.wait(stuck.GetJobId()); string(job.GetResult()) != "done" {
		t.Errorf("after release: result %q, state %v, error %s", job.GetResult(), job.GetState(), job.GetError())
	}
}

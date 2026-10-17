package coordinator

import (
	"context"
	"fmt"
	"log/slog"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sisyphus-network/Sisyphus/packages/nodedb"
	pb "github.com/sisyphus-network/Sisyphus/packages/protocol/sisyphus/v1"
	"github.com/sisyphus-network/Sisyphus/packages/runtime"
	"github.com/sisyphus-network/Sisyphus/packages/storage"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// One client may have only so many jobs unfinished at once, so that it
// cannot fill the coordinator for everyone else. Others are not held to
// its count, nor is the node itself, nor are the steps of a job.
func TestAClientMayHaveOnlySoManyJobsUnfinished(t *testing.T) {
	c := New(Config{Workloads: runtime.Builtin(), Store: storage.NewMemory(), MaxJobsPerClient: 2, Log: slog.New(slog.DiscardHandler)})
	defer c.Close()
	ctx := context.Background()
	submit := func(parent, submitter string) (*pb.Job, error) {
		return c.submit(ctx, &pb.JobSpec{Workload: "primes", Params: []byte(`{"from":1,"to":10}`)}, parent, "", submitter)
	}
	// With no worker connected, every job stays unfinished.
	first, err := submit("", "alice")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := submit("", "alice"); err != nil {
		t.Fatal(err)
	}
	if _, err := submit("", "alice"); status.Code(err) != codes.ResourceExhausted || !strings.Contains(err.Error(), "this client has 2 jobs unfinished") {
		t.Fatalf("a third job from one client: %v", err)
	}
	for name, submitter := range map[string]string{"another client": "bob", "the node itself": ""} {
		if _, err := submit("", submitter); err != nil {
			t.Errorf("a job from %s: %v", name, err)
		}
	}
	if _, err := submit("a job of alice's", "alice"); err != nil {
		t.Errorf("a step of one of the client's jobs: %v", err)
	}
	// Once one of its jobs is over, the client may submit another.
	if _, err := c.Cancel(first.GetJobId()); err != nil {
		t.Fatal(err)
	}
	if _, err := submit("", "alice"); err != nil {
		t.Errorf("a job once one of the client's was over: %v", err)
	}
}

// One client may have only so many tasks unfinished across its jobs, since
// one job of many tasks would otherwise get past a count of jobs. A job that
// would take it over is refused whole.
func TestAClientMayHaveOnlySoManyTasksUnfinished(t *testing.T) {
	c := New(Config{Workloads: runtime.Builtin(), Store: storage.NewMemory(), MaxTasksPerClient: 10, Log: slog.New(slog.DiscardHandler)})
	defer c.Close()
	ctx := context.Background()
	submit := func(tasks uint32, submitter string) (*pb.Job, error) {
		return c.submit(ctx, &pb.JobSpec{Workload: "primes", Params: []byte(`{"from":1,"to":1000}`), Mode: pb.ScheduleMode_SCHEDULE_MODE_DISTRIBUTED, MaxTasks: tasks}, "", "", submitter)
	}
	first, err := submit(6, "alice")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := submit(5, "alice"); status.Code(err) != codes.ResourceExhausted || !strings.Contains(err.Error(), "this job has 5 tasks and this client has 6 unfinished") {
		t.Fatalf("a job taking a client past its tasks: %v", err)
	}
	if _, err := submit(4, "alice"); err != nil {
		t.Fatalf("a job taking a client to its tasks exactly: %v", err)
	}
	if _, err := submit(11, "bob"); status.Code(err) != codes.ResourceExhausted {
		t.Errorf("one job of more tasks than a client may have: %v", err)
	}
	if _, err := submit(11, ""); err != nil {
		t.Errorf("a job of the node's own: %v", err)
	}
	// Once one of its jobs is over, its tasks no longer count.
	if _, err := c.Cancel(first.GetJobId()); err != nil {
		t.Fatal(err)
	}
	if _, err := submit(6, "alice"); err != nil {
		t.Errorf("a job once the client's first was over: %v", err)
	}
}

// The tasks of a job of jobs are its steps, which are jobs of the same
// client's whose own tasks are counted: the steps are not counted twice.
func TestAClientsGraphCountsTheTasksOfItsStepsOnce(t *testing.T) {
	c := New(Config{Workloads: runtime.Builtin().With(runtime.Graph{}), Store: storage.NewMemory(), MaxTasksPerClient: 4, Log: slog.New(slog.DiscardHandler)})
	defer c.Close()
	ctx := context.Background()
	graph, err := c.submit(ctx, &pb.JobSpec{Workload: "graph", Params: []byte(`{"steps":[{"name":"low","workload":"primes","params":{"from":0,"to":1000},"tasks":3}]}`)}, "", "", "carol")
	if err != nil {
		t.Fatal(err)
	}
	// Its one step begins as a job of three tasks.
	deadline := time.Now().Add(10 * time.Second)
	for !slices.ContainsFunc(c.Jobs(), func(job *pb.Job) bool { return job.GetParentJobId() == graph.GetJobId() }) {
		if time.Now().After(deadline) {
			t.Fatal("the graph's step never began")
		}
		time.Sleep(10 * time.Millisecond)
	}
	primes := &pb.JobSpec{Workload: "primes", Params: []byte(`{"from":1,"to":10}`)}
	if _, err := c.submit(ctx, primes, "", "", "carol"); err != nil {
		t.Fatalf("a fourth task, with the graph's step taking three: %v", err)
	}
	if _, err := c.submit(ctx, primes, "", "", "carol"); status.Code(err) != codes.ResourceExhausted {
		t.Fatalf("a fifth task: %v", err)
	}
}

// A graph's steps are held to the client's tasks too: a graph with a step
// that asks for more than a client may have is refused whole, and a step
// that would take the client past it when it begins fails the graph.
func TestAClientsGraphIsHeldToItsTasks(t *testing.T) {
	c := New(Config{Workloads: runtime.Builtin().With(runtime.Graph{}), Store: storage.NewMemory(), MaxTasksPerClient: 4, Log: slog.New(slog.DiscardHandler)})
	defer c.Close()
	ctx := context.Background()
	graph := func(tasks int) *pb.JobSpec {
		return &pb.JobSpec{Workload: "graph", Params: []byte(fmt.Sprintf(`{"steps":[{"name":"low","workload":"primes","params":{"from":0,"to":1000},"tasks":%d}]}`, tasks))}
	}
	if _, err := c.submit(ctx, graph(5), "", "", "carol"); status.Code(err) != codes.ResourceExhausted || !strings.Contains(err.Error(), "step low asks for 5 tasks, more than the 4") {
		t.Fatalf("a graph with a step of more tasks than a client may have: %v", err)
	}
	if _, err := c.submit(ctx, graph(5), "", "", ""); err != nil {
		t.Fatalf("a graph of the node's own: %v", err)
	}

	// With two tasks of the client's waiting, a step of three does not fit
	// when it begins, and the graph fails for it.
	if _, err := c.submit(ctx, &pb.JobSpec{Workload: "primes", Params: []byte(`{"from":1,"to":10}`), MaxTasks: 2}, "", "", "carol"); err != nil {
		t.Fatal(err)
	}
	submitted, err := c.submit(ctx, graph(3), "", "", "carol")
	if err != nil {
		t.Fatalf("a graph whose step fits on its own: %v", err)
	}
	var ended *pb.Job
	if err := c.Watch(ctx, submitted.GetJobId(), func(now *pb.Job) error { ended = now; return nil }); err != nil {
		t.Fatal(err)
	}
	if ended.GetState() != pb.JobState_JOB_STATE_FAILED || !strings.Contains(ended.GetError(), "this job has 3 tasks and this client has 2 unfinished") {
		t.Fatalf("the graph ended %s: %s", ended.GetState(), ended.GetError())
	}
}

// Steps that run side by side share the client's tasks: when the second
// to begin does not fit beside the first, the graph fails, and the first,
// a step of a graph that is over, is cancelled and counts no more.
func TestAClientsStepsSideBySideShareItsTasks(t *testing.T) {
	c := New(Config{Workloads: runtime.Builtin().With(runtime.Graph{}), Store: storage.NewMemory(), MaxTasksPerClient: 5, Log: slog.New(slog.DiscardHandler)})
	defer c.Close()
	ctx := context.Background()
	graph, err := c.submit(ctx, &pb.JobSpec{Workload: "graph", Params: []byte(`{"steps":[` +
		`{"name":"low","workload":"primes","params":{"from":0,"to":1000},"tasks":3},` +
		`{"name":"high","workload":"primes","params":{"from":1000,"to":2000},"tasks":3}]}`)}, "", "", "carol")
	if err != nil {
		t.Fatalf("a graph each of whose steps fits on its own: %v", err)
	}
	var ended *pb.Job
	if err := c.Watch(ctx, graph.GetJobId(), func(now *pb.Job) error { ended = now; return nil }); err != nil {
		t.Fatal(err)
	}
	if ended.GetState() != pb.JobState_JOB_STATE_FAILED || !strings.Contains(ended.GetError(), "this job has 3 tasks and this client has 3 unfinished, more between them than the 5") {
		t.Fatalf("the graph ended %s: %s", ended.GetState(), ended.GetError())
	}
	steps := 0
	for _, job := range c.Jobs() {
		if job.GetParentJobId() != graph.GetJobId() {
			continue
		}
		steps++
		if job.GetState() != pb.JobState_JOB_STATE_CANCELLED {
			t.Errorf("step job %s is %s, with the graph over", job.GetJobId(), job.GetState())
		}
	}
	if steps != 1 {
		t.Errorf("%d steps began, want the one that fitted", steps)
	}
	if _, err := c.submit(ctx, &pb.JobSpec{Workload: "primes", Params: []byte(`{"from":1,"to":10}`), MaxTasks: 5}, "", "", "carol"); err != nil {
		t.Errorf("the client's tasks once the graph and its steps were over: %v", err)
	}
}

// An attempt at a client's task may run no longer than the coordinator
// allows: a job may ask for less, never more, and one that asks for no
// limit gets the coordinator's. The node's own jobs keep what they ask.
func TestAClientsTasksRunNoLongerThanTheCoordinatorAllows(t *testing.T) {
	c := New(Config{Workloads: runtime.Builtin(), Store: storage.NewMemory(), MaxTaskTimeout: time.Minute, Log: slog.New(slog.DiscardHandler)})
	defer c.Close()
	ctx := context.Background()
	for _, tt := range []struct {
		submitter string
		asks      uint32
		want      time.Duration
	}{
		{"alice", 0, time.Minute},
		{"alice", 30, 30 * time.Second},
		{"alice", 60, time.Minute},
		{"alice", 3600, time.Minute},
		{"", 0, 0},
		{"", 3600, time.Hour},
	} {
		submitted, err := c.submit(ctx, &pb.JobSpec{Workload: "primes", Params: []byte(`{"from":1,"to":10}`), TaskTimeoutSeconds: tt.asks}, "", "", tt.submitter)
		if err != nil {
			t.Fatal(err)
		}
		if got := time.Duration(submitted.GetSpec().GetTaskTimeoutSeconds()) * time.Second; got != tt.want {
			t.Errorf("a job of %q's asking for %ds has tasks run for at most %v, want %v", tt.submitter, tt.asks, got, tt.want)
		}
	}
}

// What a client's finished job keeps counts towards the client until it
// lapses, though the job's record would otherwise be forgotten sooner, and
// across a restart: the record that says whose the data is stays as long.
func TestAClientsJobCountsTowardsItUntilItsDataLapses(t *testing.T) {
	db, err := nodedb.Open(filepath.Join(t.TempDir(), "node.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	store := storage.NewMemory()
	start := func() *Coordinator {
		c := New(Config{Workloads: runtime.Builtin(), Store: store, Journal: db, KeepJobs: time.Hour, Retain: 24 * time.Hour, Log: slog.New(slog.DiscardHandler)})
		if _, err := c.Recover(); err != nil {
			t.Fatal(err)
		}
		return c
	}
	c := start()
	ctx := context.Background()
	spec := &pb.JobSpec{Workload: "primes", Params: []byte(`{"from":1,"to":10}`), MaxTasks: 1}
	theirs, err := c.submit(ctx, spec, "", "", "alice")
	if err != nil {
		t.Fatal(err)
	}
	ours, err := c.submit(ctx, spec, "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	// Alice's job has finished and keeps its data for the retention
	// period, as a finished job does.
	data, err := store.Put(ctx, strings.NewReader("what alice's job produced"))
	if err != nil {
		t.Fatal(err)
	}
	finished := time.Now()
	if err := store.Pin(ctx, ownerPrefix+theirs.GetJobId(), finished.Add(24*time.Hour), data); err != nil {
		t.Fatal(err)
	}
	c.Close()
	saved, _ := db.LoadJobs()
	for _, job := range saved {
		job.Finish([]byte("done"), nil, finished)
		if err := db.SaveJob(job); err != nil {
			t.Fatal(err)
		}
	}
	c = start()
	defer func() { c.Close() }()
	alices := func() uint64 {
		t.Helper()
		stored, err := c.Stored(ctx, func(string) string { return "" })
		if err != nil {
			t.Fatal(err)
		}
		return stored["alice"]
	}
	if got := alices(); got != uint64(len("what alice's job produced")) {
		t.Fatalf("alice keeps %d bytes", got)
	}

	// Past the keeping period the node's own job is forgotten, and hers is
	// not, since its data is still kept: it still counts, and still does
	// after a restart.
	if n, err := c.Prune(finished.Add(2 * time.Hour)); n != 1 || err != nil {
		t.Fatalf("pruned %d jobs two hours on, %v; want the node's own", n, err)
	}
	if _, err := c.Get(ours.GetJobId()); status.Code(err) != codes.NotFound {
		t.Errorf("the node's own job: %v, want it forgotten", err)
	}
	c.Close()
	c = start()
	if got := alices(); got != uint64(len("what alice's job produced")) {
		t.Errorf("after a restart, alice keeps %d bytes", got)
	}
	// Once her data has lapsed, her job goes too.
	if n, err := c.Prune(finished.Add(25 * time.Hour)); n != 1 || err != nil {
		t.Errorf("pruned %d jobs a day on, %v; want hers", n, err)
	}
}

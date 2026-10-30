package main

import (
	"context"
	"encoding/csv"
	"errors"
	"os"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/sisyphus-network/Sisyphus/packages/accounting"
	pb "github.com/sisyphus-network/Sisyphus/packages/protocol/sisyphus/v1"
	"github.com/sisyphus-network/Sisyphus/packages/runtime"
)

// A pool's accounting counts its jobs by machine and by workload. A
// graph counts as a job of its own and its steps as theirs, and the
// coordinator that carried the steps out is no machine of it. A client is
// told of its own jobs only.
func TestAPoolsAccountingCountsItsJobsAndAClientItsOwn(t *testing.T) {
	p := startPool(t, runtime.Builtin().With(runtime.Graph{}))
	p.startWorker("rig", 2)
	p.waitForWorkers(1)
	ours, theirs := p.newClient(), p.newClient()
	before := time.Now()

	p.wait(p.submit(&pb.JobSpec{Workload: "primes", Params: []byte(`{"from":0,"to":1000}`), MaxTasks: 2}).GetJobId())
	p.wait(p.submit(graphOf(`{"name":"low","workload":"primes","params":{"from":0,"to":100}}`)).GetJobId())
	p.wait(p.submitFor(ours, &pb.JobSpec{Workload: "primes", Params: []byte(`{"from":0,"to":10}`), MaxTasks: 1}))

	// The node itself is told of every job.
	all := p.coord.Accounting(accounting.Range{From: before}, "")
	if all.GetJobs() != 4 || len(all.GetMachines()) != 1 || len(all.GetWorkloads()) != 2 || !all.GetFrom().AsTime().Equal(before) || all.GetTo() != nil {
		t.Fatalf("the node's accounting: %v", all)
	}
	// Two tasks, two of the step and one of the client's, all on the rig.
	rig := all.GetMachines()[0]
	if rig.GetName() != "rig" || rig.GetJobs() != 3 || rig.GetAttempts() != 5 || rig.GetAccepted().GetUnchecked() != 5 {
		t.Errorf("the rig: %v", rig)
	}
	graph, primes := all.GetWorkloads()[0], all.GetWorkloads()[1]
	if graph.GetWorkload() != "graph" || graph.GetJobs() != 1 || graph.GetSucceeded() != 1 || graph.GetTasks() != 0 || graph.GetAccepted().GetUnchecked() != 0 {
		t.Errorf("the graph: %v", graph)
	}
	if primes.GetWorkload() != "primes" || primes.GetJobs() != 3 || primes.GetTasks() != 5 || primes.GetMachines() != 1 || primes.GetAccepted().GetUnchecked() != 5 {
		t.Errorf("primes: %v", primes)
	}

	// Each client, over the network, of its own: the one that submitted
	// the graph and the primes job, then the others.
	asked, err := p.client.GetAccounting(p.ctx, &pb.GetAccountingRequest{From: timestamppb.New(before)})
	if err != nil || asked.GetJobs() != 3 || !asked.GetFrom().AsTime().Equal(before) {
		t.Errorf("the first client's accounting: %v, %v", asked, err)
	}
	mine, err := ours.jobs.GetAccounting(p.ctx, &pb.GetAccountingRequest{})
	if err != nil || mine.GetJobs() != 1 || mine.GetMachines()[0].GetAttempts() != 1 {
		t.Errorf("the client's own accounting: %v, %v", mine, err)
	}
	if none, err := theirs.jobs.GetAccounting(p.ctx, &pb.GetAccountingRequest{}); err != nil || none.GetJobs() != 0 || none.GetMachines() != nil || none.GetWorkloads() != nil {
		t.Errorf("another client's accounting: %v, %v", none, err)
	}
	// A span that is over before it starts is refused.
	_, err = p.client.GetAccounting(p.ctx, &pb.GetAccountingRequest{From: timestamppb.New(before), To: timestamppb.New(before)})
	if status.Code(err) != codes.InvalidArgument || !strings.Contains(err.Error(), "not after it starts") {
		t.Errorf("an empty span: %v", err)
	}
}

func TestCLIPrintsAccountingAndWritesItAsCSV(t *testing.T) {
	addr, _ := startNode(t)
	mustCLI(t, "job", "submit", "--addr", addr, "--params", `{"from":0,"to":1000}`, "--tasks", "2")

	out := mustCLI(t, "accounting", "--addr", addr, "--last", "1h")
	for _, want := range []string{
		`(?m)^1 jobs ended since \d{4}-\d\d-\d\d \d\d:\d\d:\d\d$`,
		`(?m)^MACHINE +NODE_ID +JOBS +ATTEMPTS +FAILED_ATTEMPTS +ACCEPTED +AGREEMENT +AUDIT +UNCHECKED +CAUGHT +OUTVOTED$`,
		`(?m)^\S+ +\S+ +1 +2 +0 +2 +0 +0 +2 +0 +0$`,
		`(?m)^WORKLOAD +JOBS +SUCCEEDED +FAILED +CANCELLED +TASKS +ATTEMPTS +FAILED_ATTEMPTS +ACCEPTED +AGREEMENT +AUDIT +UNCHECKED +CAUGHT +MACHINES +WALL_SECONDS$`,
		`(?m)^primes +1 +1 +0 +0 +2 +2 +0 +2 +0 +0 +2 +0 +1 +\d+$`,
	} {
		if !regexp.MustCompile(want).MatchString(out) {
			t.Errorf("accounting printed no line matching %s:\n%s", want, out)
		}
	}

	rows := func(args ...string) [][]string {
		t.Helper()
		read, err := csv.NewReader(strings.NewReader(mustCLI(t, append([]string{"accounting", "--addr", addr, "--csv"}, args...)...))).ReadAll()
		if err != nil {
			t.Fatal(err)
		}
		return read
	}
	machines := rows()
	if len(machines) != 2 || len(machines[0]) != len(machineColumns) || machines[0][0] != "machine" || machines[1][5] != "2" {
		t.Errorf("accounting --csv: %q", machines)
	}
	workloads := rows("--by", "workload")
	if len(workloads) != 2 || workloads[0][0] != "workload" || workloads[1][0] != "primes" || workloads[1][len(workloadColumns)-1] == "" {
		t.Errorf("accounting --csv --by workload: %q", workloads)
	}
	// Before the job: nothing, and only what was asked for.
	if out := mustCLI(t, "accounting", "--addr", addr, "--to", "2000-01-01", "--by", "workload"); strings.Contains(out, "NODE_ID") || !strings.HasPrefix(out, "0 jobs ended before 2000-01-01 00:00:00\n\nWORKLOAD") {
		t.Errorf("accounting before 2000:\n%s", out)
	}
}

// recordedAccounting is a node that answers every request for accounting
// with the same totals, and keeps the requests.
type recordedAccounting struct {
	pb.UnimplementedNodeServiceServer
	mu       sync.Mutex
	requests []*pb.GetAccountingRequest
	totals   *pb.Accounting
	err      error
}

func (r *recordedAccounting) GetAccounting(_ context.Context, req *pb.GetAccountingRequest) (*pb.Accounting, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.requests = append(r.requests, req)
	return r.totals, r.err
}

func (r *recordedAccounting) last() *pb.GetAccountingRequest {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.requests[len(r.requests)-1]
}

func TestCLIAccountingAsksForTheSpanItIsGiven(t *testing.T) {
	present := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	now = func() time.Time { return present }
	t.Cleanup(func() { now = time.Now })
	node := &recordedAccounting{totals: &pb.Accounting{}}
	addr := serveFake(t, func(srv *grpc.Server) { pb.RegisterNodeServiceServer(srv, node) })
	midnight := time.Date(2026, 10, 1, 0, 0, 0, 0, time.Local)

	for _, c := range []struct {
		args     []string
		from, to time.Time
		says     string
	}{
		{nil, time.Time{}, time.Time{}, "in all the coordinator holds"},
		{[]string{"--last", "7d"}, present.Add(-7 * 24 * time.Hour), time.Time{}, "since"},
		{[]string{"--last", "36h", "--to", "2026-10-02T00:00:00Z"}, time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC), time.Date(2026, 10, 2, 0, 0, 0, 0, time.UTC), "from"},
		{[]string{"--from", "2026-10-01"}, midnight, time.Time{}, "since " + midnight.Format(time.DateTime)},
		{[]string{"--to", "2026-10-01"}, time.Time{}, midnight, "before " + midnight.Format(time.DateTime)},
	} {
		// The node says back the span it was asked for, as a coordinator does.
		node.totals = &pb.Accounting{}
		if !c.from.IsZero() {
			node.totals.From = timestamppb.New(c.from)
		}
		if !c.to.IsZero() {
			node.totals.To = timestamppb.New(c.to)
		}
		out := mustCLI(t, append([]string{"accounting", "--addr", addr}, c.args...)...)
		asked := node.last()
		if !c.from.Equal(at(asked.GetFrom())) || !c.to.Equal(at(asked.GetTo())) {
			t.Errorf("%v asked from %v to %v, want from %v to %v", c.args, at(asked.GetFrom()), at(asked.GetTo()), c.from, c.to)
		}
		if !strings.HasPrefix(out, "0 jobs ended "+c.says) {
			t.Errorf("%v printed %q, want it to begin with %q", c.args, out, c.says)
		}
	}
}

// at is the time a message holds, or none.
func at(t *timestamppb.Timestamp) time.Time {
	if t == nil {
		return time.Time{}
	}
	return t.AsTime()
}

// failingWriter fails every write.
type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errors.New("disk full") }

func TestCLIAccountingSaysWhatIsWrong(t *testing.T) {
	node := &recordedAccounting{totals: &pb.Accounting{}}
	addr := serveFake(t, func(srv *grpc.Server) { pb.RegisterNodeServiceServer(srv, node) })
	for _, c := range []struct {
		args []string
		want string
	}{
		{[]string{"extra"}, `unexpected argument "extra"`},
		{[]string{"--no-such-flag"}, "flag provided but not defined"},
		{[]string{"--by", "day"}, `--by "day": expected machine or workload`},
		{[]string{"--last", "7d", "--from", "2026-10-01"}, "--last and --from both say where the span starts"},
		{[]string{"--last", "0d"}, `--last "0d" is not a span of time`},
		{[]string{"--last", "-1h"}, `--last "-1h" is not a span of time`},
		{[]string{"--last", "week"}, `--last "week" is not a span of time`},
		{[]string{"--from", "yesterday"}, `"yesterday" is neither a date`},
		{[]string{"--to", "2026-13-01"}, `"2026-13-01" is neither a date`},
		{[]string{"--addr", badAddr}, "address"},
	} {
		args := append([]string{"accounting"}, c.args...)
		if !strings.HasPrefix(c.args[0], "--addr") {
			args = append(args, "--addr", addr)
		}
		if _, err := cli(t, args...); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%v: %v, want an error saying %q", c.args, err, c.want)
		}
	}

	// What the node says is wrong.
	node.err = status.Error(codes.InvalidArgument, "the range ends before it starts")
	if _, err := cli(t, "accounting", "--addr", addr); status.Code(err) != codes.InvalidArgument {
		t.Errorf("a refusal: %v", err)
	}

	// And a table that cannot be written.
	node.err = nil
	node.totals = &pb.Accounting{Machines: []*pb.MachineTotals{{Name: "rig"}}}
	stdout = failingWriter{}
	defer func() { stdout = os.Stdout }()
	err := run(context.Background(), []string{"accounting", "--addr", addr, "--data-dir", dataDirOf(addr), "--csv"})
	if err == nil || !strings.Contains(err.Error(), "disk full") {
		t.Errorf("writing to a full disk: %v", err)
	}
}

// dataDirOf is the data directory commands to a test node run from.
func dataDirOf(addr string) string {
	dir, _ := dataDirs.Load(addr)
	return dir.(string)
}

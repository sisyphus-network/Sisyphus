package main

import (
	"context"
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pb "github.com/sisyphus-network/Sisyphus/packages/protocol/sisyphus/v1"
	"github.com/sisyphus-network/Sisyphus/packages/runtime"
)

// These tests are about work whose honest results differ slightly from one
// machine to another, as a computation in floating point does between
// processors, and whose workload compares them within a tolerance of its
// own.

// numeric is such a workload: each task computes one number, which honest
// workers agree on to eight decimals and may not past that.
type numeric struct{}

// computed is what this node gets for a task of numeric.
const computed = "1.000000000000"

func (numeric) Name() string { return "numeric" }

func (numeric) Split(_ context.Context, _ runtime.Blobs, _ []byte, parts int) ([][]byte, error) {
	return make([][]byte, parts), nil
}

func (numeric) Execute(context.Context, runtime.Blobs, []byte) ([]byte, error) {
	return []byte(computed), nil
}

func (numeric) Aggregate(_ context.Context, _ runtime.Blobs, outputs [][]byte) ([]byte, error) {
	var all []string
	for _, o := range outputs {
		all = append(all, string(o))
	}
	return []byte(strings.Join(all, ",")), nil
}

func (numeric) Varies() bool { return true }

func (numeric) Compare(a, b []byte) (bool, string) {
	x, errA := strconv.ParseFloat(string(a), 64)
	y, errB := strconv.ParseFloat(string(b), 64)
	if errA != nil || errB != nil {
		return false, "one of them is not a number"
	}
	if d := math.Abs(x - y); d > 1e-8 {
		return false, fmt.Sprintf("the values differ by %.3g, more than 1e-08", d)
	}
	return true, ""
}

// drifting is work whose results vary and which does not compare them.
type drifting struct{ numeric }

func (drifting) Name() string { return "drifting" }

func (drifting) Compare() {} // not runtime.Comparing

// numericCrew connects hand-driven workers of numeric, each with a slot
// fewer than the one before, so that a task goes to them in this order.
func (p *pool) numericCrew(names ...string) []*rawWorker {
	p.t.Helper()
	var crew []*rawWorker
	for i, name := range names {
		w := p.connectRaw(hello(name, uint32(len(names)-i), "numeric"))
		w.welcome()
		crew = append(crew, w)
	}
	return crew
}

func TestWorkersThatDifferWithinTheWorkloadsToleranceAgree(t *testing.T) {
	p := startPool(t, runtime.NewRegistry(numeric{}))
	crew := p.numericCrew("alpha", "beta")
	id := p.submit(&pb.JobSpec{Workload: "numeric", MaxTasks: 1, Verify: 2}).GetJobId()
	// They differ in the tenth decimal.
	crew[0].returns(crew[0].assignment(), "1.000000000000")
	crew[1].returns(crew[1].assignment(), "1.000000000100")
	done := p.wait(id)
	if done.GetState() != pb.JobState_JOB_STATE_SUCCEEDED || string(done.GetResult()) != "1.000000000000" {
		t.Fatalf("the job: %v %s %s", done.GetState(), done.GetResult(), done.GetError())
	}
	told := story(p.events(id, 0))
	if strings.Contains(told, "task-disagreed") || !strings.Contains(told, "task-succeeded: alpha, beta returned the same result") {
		t.Errorf("the job's events:\n%s", told)
	}
	for _, w := range crew {
		if got := p.standing(w); got != "1 agreed, 0 outvoted, 0 owed" {
			t.Errorf("an honest worker's standing: %s", got)
		}
	}
	// Both are among those that settled the task, though their digests
	// differ.
	if record := p.recordText(id); strings.Count(record, `"agreed":true`) != 2 {
		t.Errorf("the job's record: %s", record)
	}
}

func TestAResultAlteredBeyondTheWorkloadsToleranceIsOutvoted(t *testing.T) {
	p := startPool(t, runtime.NewRegistry(numeric{}))
	crew := p.numericCrew("alpha", "liar", "gamma")
	alpha, liar, gamma := crew[0], crew[1], crew[2]
	id := p.submit(&pb.JobSpec{Workload: "numeric", MaxTasks: 1, Verify: 2}).GetJobId()
	alpha.returns(alpha.assignment(), "1.000000000000")
	liar.returns(liar.assignment(), "1.5")
	gamma.returns(gamma.assignment(), "0.999999999990")
	done := p.wait(id)
	if done.GetState() != pb.JobState_JOB_STATE_SUCCEEDED || string(done.GetResult()) != "1.000000000000" {
		t.Fatalf("the job: %v %s %s", done.GetState(), done.GetResult(), done.GetError())
	}
	// What differed is told where the disagreement is, and where the worker
	// is outvoted.
	told := story(p.events(id, 0))
	for _, want := range []string{
		"task-disagreed: alpha returned ",
		"; liar returned ",
		" (the values differ by 0.5, more than 1e-08)",
		"task-succeeded: alpha, gamma returned the same result; liar returned another (the values differ by 0.5, more than 1e-08)",
	} {
		if !strings.Contains(told, want) {
			t.Errorf("the job's events lack %q:\n%s", want, told)
		}
	}
	if logged := p.logs.String(); !strings.Contains(logged, "the values differ by 0.5") {
		t.Errorf("the coordinator's log: %s", logged)
	}
	if got := p.standing(liar); got != "0 agreed, 1 outvoted, 10 owed" {
		t.Errorf("the outvoted worker's standing: %s", got)
	}
	if record := p.recordText(id); strings.Count(record, `"agreed":true`) != 2 || strings.Count(record, `"agreed":false`) != 1 {
		t.Errorf("the job's record: %s", record)
	}

	// With nobody else to ask, the task cannot be verified, and the job
	// says what differed.
	p = startPool(t, runtime.NewRegistry(numeric{}))
	crew = p.numericCrew("alpha", "liar")
	id = p.submit(&pb.JobSpec{Workload: "numeric", MaxTasks: 1, Verify: 2}).GetJobId()
	crew[0].returns(crew[0].assignment(), "1.000000000000")
	crew[1].returns(crew[1].assignment(), "not a number")
	failed := p.wait(id)
	if failed.GetState() != pb.JobState_JOB_STATE_FAILED || !strings.Contains(failed.GetError(), "(one of them is not a number)") {
		t.Errorf("the job: %v %s", failed.GetState(), failed.GetError())
	}
}

func TestTheCoordinatorChecksAResultWithinTheWorkloadsTolerance(t *testing.T) {
	p := startPool(t, runtime.NewRegistry(numeric{}))
	w := p.connectRaw(hello("only", 1, "numeric"))
	w.welcome()
	run := func(returned string) (*pb.Job, string) {
		t.Helper()
		id := p.submit(&pb.JobSpec{Workload: "numeric", MaxTasks: 1, AuditShare: 1}).GetJobId()
		w.returns(w.assignment(), returned)
		done := p.wait(id)
		return done, story(p.events(id, 0))
	}

	// A worker on another processor, which differs in the eleventh
	// decimal: its own result is taken.
	honest, told := run("1.000000000030")
	if honest.GetState() != pb.JobState_JOB_STATE_SUCCEEDED || string(honest.GetResult()) != "1.000000000030" || !strings.Contains(told, audited) || strings.Contains(told, "task-disagreed") {
		t.Errorf("the job: %v %s\n%s", honest.GetState(), honest.GetResult(), told)
	}
	if got := p.standing(w); got != "1 agreed, 0 outvoted, 0 owed" {
		t.Errorf("an honest worker's standing: %s", got)
	}

	// One that made its number up is caught, and the events say by how
	// much it was off.
	lied, told := run("1.25")
	if lied.GetState() != pb.JobState_JOB_STATE_SUCCEEDED || string(lied.GetResult()) != computed {
		t.Errorf("the job: %v %s", lied.GetState(), lied.GetResult())
	}
	if !strings.Contains(told, auditedOtherwise) || !strings.Contains(told, "(the values differ by 0.25, more than 1e-08)") || !strings.Contains(told, "the coordinator's own result is taken, and not only's") {
		t.Errorf("the job's events:\n%s", told)
	}
	if got := p.standing(w); got != "1 agreed, 1 outvoted, 10 owed" {
		t.Errorf("the standing of a worker caught: %s", got)
	}
}

func TestWorkThatVariesAndDoesNotCompareItsResultsIsStillNotChecked(t *testing.T) {
	p := startPool(t, runtime.NewRegistry(numeric{}, drifting{}))
	for name, spec := range map[string]*pb.JobSpec{
		"verify":      {Workload: "drifting", Verify: 2},
		"audit_share": {Workload: "drifting", AuditShare: 0.5},
	} {
		_, err := p.client.SubmitJob(p.ctx, &pb.SubmitJobRequest{Spec: spec})
		if status.Code(err) != codes.InvalidArgument || !strings.Contains(err.Error(), "drifting gives results that can differ from one run to the next") {
			t.Errorf("drifting with %s: %v", name, err)
		}
	}
	// Work that compares its results is not refused for varying: with no
	// workers here, a job that asks for verify waits for two.
	if _, err := p.client.SubmitJob(p.ctx, &pb.SubmitJobRequest{Spec: &pb.JobSpec{Workload: "numeric", Verify: 2}}); status.Code(err) != codes.FailedPrecondition || !strings.Contains(err.Error(), "2 different workers") {
		t.Errorf("numeric with verify: %v", err)
	}
	if _, err := p.client.SubmitJob(p.ctx, &pb.SubmitJobRequest{Spec: &pb.JobSpec{Workload: "numeric", AuditShare: 0.5}}); err != nil {
		t.Errorf("numeric with audit_share: %v", err)
	}
}

// tiered is numeric with two tiers, as qchem has: honest results agree to
// twelve decimals on one kind of processor, and to eight across kinds. An
// output is the number, and may go on to claim a processor, which a worker
// could write whatever it likes into and the comparison must not believe.
// It notes every pair of origins it is given.
type tiered struct {
	numeric
	seen *originLog
}

type originLog struct {
	mu   sync.Mutex
	seen [][2]runtime.Origin
}

func (l *originLog) all() [][2]runtime.Origin {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.seen)
}

func (tiered) Name() string { return "tiered" }

func (t tiered) CompareFrom(a []byte, aFrom runtime.Origin, b []byte, bFrom runtime.Origin) (bool, string, string) {
	t.seen.mu.Lock()
	t.seen.seen = append(t.seen.seen, [2]runtime.Origin{aFrom, bFrom})
	t.seen.mu.Unlock()
	tier, within := runtime.TierCrossCPU, 1e-8
	if runtime.SameCPU(aFrom, bFrom) {
		tier, within = runtime.TierSameCPU, 1e-12
	}
	x, errA := strconv.ParseFloat(strings.Fields(string(a) + " ")[0], 64)
	y, errB := strconv.ParseFloat(strings.Fields(string(b) + " ")[0], 64)
	if errA != nil || errB != nil {
		return false, tier, "one of them is not a number"
	}
	if d := math.Abs(x - y); d > within {
		return false, tier, fmt.Sprintf("the values differ by %.3g, more than %g", d, within)
	}
	return true, tier, ""
}

// helloOn is hello from a worker that says its machine has this processor.
func helloOn(name string, slots uint32, cpu string, workloads ...string) *pb.WorkerMessage {
	msg := hello(name, slots, workloads...)
	msg.GetHello().GetCapabilities().CpuModel = cpu
	return msg
}

// Which tier a verified task is held to follows from the processors the
// coordinator recorded for the two workers when they joined, never from
// what their outputs claim: both outputs here claim the same processor.
func TestAVerifiedTaskIsHeldToTheTierOfTheProcessorsItsWorkersJoinedWith(t *testing.T) {
	const xeon, epyc = "Intel(R) Xeon(R) CPU E5-2670 0 @ 2.60GHz", "AMD EPYC 7763 64-Core Processor"
	for _, tt := range []struct {
		name       string
		cpus       [2]string
		returned   [2]string
		succeeded  bool
		tier, said string
	}{
		{"the same processor, the same bits", [2]string{xeon, xeon}, [2]string{"1.000000000000 " + xeon, "1.000000000000 " + xeon}, true, runtime.TierSameCPU, "alpha, beta returned the same result (compared in the same-cpu tier)"},
		{"the same processor, digits apart", [2]string{xeon, xeon}, [2]string{"1.000000000000 " + xeon, "1.000000000100 " + xeon}, false, runtime.TierSameCPU, "(the values differ by 1e-10, more than 1e-12)"},
		{"different processors", [2]string{xeon, epyc}, [2]string{"1.000000000000 " + xeon, "1.000000000100 " + xeon}, true, runtime.TierCrossCPU, "alpha, beta returned the same result (compared in the cross-cpu tier)"},
		{"one processor unknown", [2]string{xeon, ""}, [2]string{"1.000000000000 " + xeon, "1.000000000100 " + xeon}, true, runtime.TierCrossCPU, "(compared in the cross-cpu tier)"},
		{"both unknown", [2]string{"", ""}, [2]string{"1.000000000000 " + xeon, "1.000000000100 " + xeon}, true, runtime.TierCrossCPU, "(compared in the cross-cpu tier)"},
		{"different processors, too far apart for either tier", [2]string{xeon, epyc}, [2]string{"1.000000000000", "1.0001"}, false, runtime.TierCrossCPU, "more than 1e-08"},
	} {
		seen := new(originLog)
		p := startPool(t, runtime.NewRegistry(tiered{seen: seen}))
		alpha := p.connectRaw(helloOn("alpha", 2, tt.cpus[0], "tiered"))
		alpha.welcome()
		beta := p.connectRaw(helloOn("beta", 1, tt.cpus[1], "tiered"))
		beta.welcome()
		id := p.submit(&pb.JobSpec{Workload: "tiered", MaxTasks: 1, Verify: 2}).GetJobId()
		alpha.returns(alpha.assignment(), tt.returned[0])
		beta.returns(beta.assignment(), tt.returned[1])
		done := p.wait(id)
		if succeeded := done.GetState() == pb.JobState_JOB_STATE_SUCCEEDED; succeeded != tt.succeeded {
			t.Errorf("%s: the job %v %s", tt.name, done.GetState(), done.GetError())
		}
		if told := story(p.events(id, 0)) + "\n" + done.GetError(); !strings.Contains(told, tt.said) {
			t.Errorf("%s: the job's events lack %q:\n%s", tt.name, tt.said, told)
		}
		// Every comparison was given the processors the workers joined
		// with, in one order or the other.
		origins := seen.all()
		if len(origins) == 0 {
			t.Fatalf("%s: never compared", tt.name)
		}
		for _, o := range origins {
			got := []string{o[0].CPUModel, o[1].CPUModel}
			slices.Sort(got)
			want := []string{tt.cpus[0], tt.cpus[1]}
			slices.Sort(want)
			if !slices.Equal(got, want) {
				t.Errorf("%s: compared as from %q, want %q", tt.name, got, want)
			}
		}
	}
}

// Three workers that settle a task, two on one processor and one on
// another, settle it in both tiers, which is said as runtime.TierMixed.
func TestATaskSettledInBothTiersIsSaidToBeMixed(t *testing.T) {
	const xeon, epyc = "Intel(R) Xeon(R) CPU E5-2670 0 @ 2.60GHz", "AMD EPYC 7763 64-Core Processor"
	p := startPool(t, runtime.NewRegistry(tiered{seen: new(originLog)}))
	var crew []*rawWorker
	for i, w := range []struct{ name, cpu string }{{"alpha", xeon}, {"beta", xeon}, {"gamma", epyc}} {
		raw := p.connectRaw(helloOn(w.name, uint32(3-i), w.cpu, "tiered"))
		raw.welcome()
		crew = append(crew, raw)
	}
	id := p.submit(&pb.JobSpec{Workload: "tiered", MaxTasks: 1, Verify: 3}).GetJobId()
	for i, returned := range []string{"1.000000000000", "1.000000000000", "1.000000000100"} {
		crew[i].returns(crew[i].assignment(), returned)
	}
	done := p.wait(id)
	want := "alpha, beta, gamma returned the same result (compared in the " + runtime.TierMixed + " tier)"
	if told := story(p.events(id, 0)); done.GetState() != pb.JobState_JOB_STATE_SUCCEEDED || !strings.Contains(told, want) {
		t.Errorf("the job %v, its events lack %q:\n%s", done.GetState(), want, told)
	}
}

// An audit compares the worker's result, as from the processor it joined
// with, with the coordinator's own, as from the coordinator's processor.
func TestAnAuditIsHeldToTheTierOfTheWorkersAndTheCoordinatorsProcessors(t *testing.T) {
	for _, tt := range []struct {
		name, cpu, returned string
		matched             bool
		said                string
	}{
		{"the coordinator's processor, the same bits", poolCPU, computed, true, audited + " (compared in the same-cpu tier)"},
		{"the coordinator's processor, digits apart", poolCPU, "1.000000000100", false, "(the values differ by 1e-10, more than 1e-12)"},
		{"another processor, digits apart", "Another CPU", "1.000000000100", true, audited + " (compared in the cross-cpu tier)"},
		{"an unknown processor that claims the coordinator's", "", "1.000000000100 " + poolCPU, true, audited + " (compared in the cross-cpu tier)"},
	} {
		seen := new(originLog)
		p := startPool(t, runtime.NewRegistry(tiered{seen: seen}))
		w := p.connectRaw(helloOn("only", 1, tt.cpu, "tiered"))
		w.welcome()
		id := p.submit(&pb.JobSpec{Workload: "tiered", MaxTasks: 1, AuditShare: 1}).GetJobId()
		w.returns(w.assignment(), tt.returned)
		done := p.wait(id)
		told := story(p.events(id, 0))
		if done.GetState() != pb.JobState_JOB_STATE_SUCCEEDED || !strings.Contains(told, tt.said) || strings.Contains(told, "task-disagreed") == tt.matched {
			t.Errorf("%s: the job %v, its events:\n%s", tt.name, done.GetState(), told)
		}
		// Once to settle it, and again to say why where they differ.
		origins := seen.all()
		if len(origins) == 0 {
			t.Errorf("%s: never compared", tt.name)
		}
		for _, o := range origins {
			if o[0].CPUModel != poolCPU || o[1].CPUModel != tt.cpu {
				t.Errorf("%s: compared as from %v", tt.name, origins)
			}
		}
	}
}

package main

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	pb "github.com/sisyphus-network/Sisyphus/packages/protocol/sisyphus/v1"
	"github.com/sisyphus-network/Sisyphus/packages/runtime"
)

// stepsOf returns the jobs submitted to carry out a graph's steps, by step.
func (p *pool) stepsOf(graph string) map[string][]*pb.Job {
	p.t.Helper()
	found := make(map[string][]*pb.Job)
	for _, job := range p.coord.Jobs() {
		if job.GetParentJobId() == graph {
			found[job.GetStep()] = append(found[job.GetStep()], job)
		}
	}
	return found
}

func graphOf(steps string) *pb.JobSpec {
	return &pb.JobSpec{Workload: "graph", Params: []byte(`{"steps":[` + steps + `]}`)}
}

func TestAGraphRunsItsStepsAsJobsEachUsingWhatTheLastProduced(t *testing.T) {
	p := startPool(t, runtime.Builtin().With(runtime.Graph{}))
	p.startWorker("rig", 4)
	p.waitForWorkers(1)
	input := p.upload("the boulder rolls and the boulder rolls again")

	done := p.wait(p.submit(graphOf(`
		{"name":"count","workload":"wordcount","params":{"input":"` + input + `"}},
		{"name":"again","workload":"wordcount","params":{"input":"${count.result.output}"}},
		{"name":"low","workload":"primes","params":{"from":0,"to":1000},"tasks":2},
		{"name":"span","workload":"primes","params":{"from":0,"to":"${low.result.count}"},"after":["again"]}`)).GetJobId())
	if done.GetState() != pb.JobState_JOB_STATE_SUCCEEDED {
		t.Fatalf("the graph %v: %s", done.GetState(), done.GetError())
	}
	var result struct {
		Result struct{ Count int }
		Steps  []struct {
			Step, JobID string `json:"-"`
			Name        string `json:"step"`
			Job         string `json:"job_id"`
		}
	}
	if err := json.Unmarshal(done.GetResult(), &result); err != nil {
		t.Fatalf("%s: %v", done.GetResult(), err)
	}
	// There are 168 primes below 1000, and 39 below 168.
	if result.Result.Count != 39 || len(result.Steps) != 4 || result.Steps[3].Name != "span" {
		t.Errorf("the graph's result: %s", done.GetResult())
	}
	// Each step was a job of its own, known for a step of this one, and
	// the graph's tasks are its steps, carried out by the coordinator.
	steps := p.stepsOf(done.GetJobId())
	for i, name := range []string{"count", "again", "low", "span"} {
		if len(steps[name]) != 1 || steps[name][0].GetJobId() != result.Steps[i].Job || steps[name][0].GetState() != pb.JobState_JOB_STATE_SUCCEEDED {
			t.Errorf("step %s was carried out by %v", name, steps[name])
		}
		if task := done.GetTasks()[i]; task.GetNodeName() != "coordinator" || task.GetState() != pb.TaskState_TASK_STATE_SUCCEEDED {
			t.Errorf("the graph's task %d: %v", i, task)
		}
	}
	if low := steps["low"][0]; len(low.GetTasks()) != 2 {
		t.Errorf("the step asked for in two tasks ran in %d", len(low.GetTasks()))
	}
	var said []string
	for _, e := range p.eventsSoFar(done.GetJobId()) {
		said = append(said, e.GetKind()+" "+e.GetText())
	}
	if all := strings.Join(said, "\n"); !strings.Contains(all, "task-started step count is job "+result.Steps[0].Job) || !strings.Contains(all, "task-succeeded step span") {
		t.Errorf("the graph's events:\n%s", all)
	}
}

func TestAGraphFailsWithTheStepThatFailsAndStopsWithTheGraph(t *testing.T) {
	g := gated{started: make(chan struct{}, 16), release: make(chan struct{})}
	p := startPool(t, runtime.NewRegistry(g, runtime.Primes{}, runtime.WordCount{}, runtime.Graph{}))
	p.startWorker("rig", 4)
	p.waitForWorkers(1)

	// A step whose job is refused, one whose job fails, one that refers
	// to what was not produced: each fails the graph, and what waited is
	// not begun.
	for name, tt := range map[string]struct{ steps, want string }{
		"refused":      {`{"name":"a","workload":"no-such-workload"},{"name":"b","workload":"primes","params":{"from":0,"to":10},"after":["a"]}`, `step a: unknown workload "no-such-workload"`},
		"failed":       {`{"name":"a","workload":"wordcount","params":{"input":"bafkreigh2akiscaildcqabsyg3dfr6chu3fgpregiymsck7e7aqa4s52zy"}},{"name":"b","workload":"primes","after":["a"]}`, "step a"},
		"not produced": {`{"name":"a","workload":"primes","params":{"from":0,"to":10}},{"name":"b","workload":"primes","params":{"from":0,"to":"${a.result.total}"}}`, `step "b": ${a.result.total} refers to something step "a" did not produce`},
	} {
		failed := p.wait(p.submit(graphOf(tt.steps)).GetJobId())
		if failed.GetState() != pb.JobState_JOB_STATE_FAILED || !strings.Contains(failed.GetError(), tt.want) || strings.Contains(failed.GetError(), "rpc error") {
			t.Errorf("a graph with a step %s: %v, %s", name, failed.GetState(), failed.GetError())
		}
		if last := failed.GetTasks()[1]; name != "not produced" && last.GetState() != pb.TaskState_TASK_STATE_CANCELLED {
			t.Errorf("after a step %s, the step that waited is %v", name, last.GetState())
		}
	}

	// Stopping a graph stops the job its step is running as.
	graph := p.submit(graphOf(`{"name":"held","workload":"gated","tasks":1}`))
	<-g.started
	held := p.stepsOf(graph.GetJobId())["held"][0].GetJobId()
	if _, err := p.client.CancelJob(p.ctx, &pb.CancelJobRequest{JobId: graph.GetJobId()}); err != nil {
		t.Fatal(err)
	}
	if got := p.wait(held); got.GetState() != pb.JobState_JOB_STATE_CANCELLED {
		t.Errorf("the step's job after its graph was stopped: %v", got.GetState())
	}
	if got := p.wait(graph.GetJobId()); got.GetState() != pb.JobState_JOB_STATE_CANCELLED {
		t.Errorf("the graph after being stopped: %v", got.GetState())
	}
	// And stopping the step's job fails the graph.
	graph = p.submit(graphOf(`{"name":"held","workload":"gated","tasks":1}`))
	<-g.started
	held = p.stepsOf(graph.GetJobId())["held"][0].GetJobId()
	if _, err := p.client.CancelJob(p.ctx, &pb.CancelJobRequest{JobId: held}); err != nil {
		t.Fatal(err)
	}
	if got := p.wait(graph.GetJobId()); got.GetState() != pb.JobState_JOB_STATE_FAILED || !strings.Contains(got.GetError(), "step held (job "+held+") cancelled") {
		t.Errorf("the graph after its step's job was stopped: %v, %s", got.GetState(), got.GetError())
	}
}

func TestAGraphIsTakenUpWhereItWasAfterARestart(t *testing.T) {
	file := filepath.Join(t.TempDir(), "node.db")
	g := gated{started: make(chan struct{}, 16), release: make(chan struct{})}
	workloads := runtime.NewRegistry(g, runtime.Primes{}, runtime.Graph{})

	db := journalIn(t, file)
	before := startPoolWith(t, workloads, sameStore, db)
	stopFirst := before.startWorker("first", 2)
	before.waitForWorkers(1)
	graph := before.submit(graphOf(`
		{"name":"quick","workload":"primes","params":{"from":0,"to":100}},
		{"name":"held","workload":"gated","tasks":1,"after":["quick"]},
		{"name":"last","workload":"primes","params":{"from":0,"to":"${quick.result.count}"},"after":["held"]}`))
	<-g.started
	held := before.stepsOf(graph.GetJobId())["held"][0].GetJobId()

	before.stopAndForget()
	stopFirst()
	db.Close()

	db = journalIn(t, file)
	after := startPoolWith(t, workloads, sameStore, db)
	after.startWorker("second", 2)
	<-g.started
	close(g.release)
	done := after.wait(graph.GetJobId())
	// 25 primes below 100, and 9 below 25.
	if done.GetState() != pb.JobState_JOB_STATE_SUCCEEDED || !strings.Contains(string(done.GetResult()), `"result":{"count":9}`) {
		t.Fatalf("the graph, taken up again: %v, %s, %s", done.GetState(), done.GetResult(), done.GetError())
	}
	// The step that was running went on as the job it was, and the one
	// that had finished was not run again.
	steps := after.stepsOf(graph.GetJobId())
	if len(steps["held"]) != 1 || steps["held"][0].GetJobId() != held || len(steps["quick"]) != 1 || len(steps["last"]) != 1 {
		t.Errorf("the steps' jobs after a restart: %v", steps)
	}
}

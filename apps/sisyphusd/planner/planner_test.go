package planner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/sisyphus-network/Sisyphus/packages/ai"
	pb "github.com/sisyphus-network/Sisyphus/packages/protocol/sisyphus/v1"
)

// scripted is a model that says what it was told to, one reply for each time
// it is asked, and remembers what it was asked.
type scripted struct {
	replies []ai.Message
	err     error
	asked   []ai.Request
}

func (s *scripted) Chat(_ context.Context, req ai.Request, said func(string)) (ai.Message, error) {
	s.asked = append(s.asked, req)
	if len(s.asked) > len(s.replies) {
		return ai.Message{}, s.err
	}
	reply := s.replies[len(s.asked)-1]
	reply.Role = ai.Assistant
	// As a model does, a word at a time.
	for _, word := range strings.SplitAfter(reply.Content, " ") {
		if word != "" {
			said(word)
		}
	}
	return reply, nil
}

func (*scripted) Models(context.Context) ([]string, error) { return nil, nil }

// calling is a reply that asks for one tool to be run.
func calling(id, tool, arguments string) ai.Message {
	return ai.Message{Calls: []ai.Call{{ID: id, Name: tool, Arguments: json.RawMessage(arguments)}}}
}

// office is a pool that finishes every job at once with a given result.
type office struct {
	submitted []*pb.JobSpec
	jobs      map[string]*pb.Job
	result    string
	refuse    error
	lose      error
}

func (o *office) Submit(_ context.Context, spec *pb.JobSpec) (*pb.Job, error) {
	if o.refuse != nil {
		return nil, o.refuse
	}
	o.submitted = append(o.submitted, spec)
	at := time.Unix(1_700_000_000, 0)
	job := &pb.Job{
		JobId: fmt.Sprintf("job-%d", len(o.submitted)), Spec: spec, State: pb.JobState_JOB_STATE_SUCCEEDED,
		Result: []byte(o.result), Tasks: make([]*pb.Task, 2), CreatedAt: timestamppb.New(at), FinishedAt: timestamppb.New(at.Add(1500 * time.Millisecond)),
	}
	if o.jobs == nil {
		o.jobs = make(map[string]*pb.Job)
	}
	o.jobs[job.GetJobId()] = job
	return &pb.Job{JobId: job.GetJobId(), Spec: spec, State: pb.JobState_JOB_STATE_PENDING}, nil
}

func (o *office) Watch(_ context.Context, id string, fn func(*pb.Job) error) error {
	if o.lose != nil {
		return o.lose
	}
	return fn(o.jobs[id])
}

func (o *office) Get(id string) (*pb.Job, error) {
	if job, ok := o.jobs[id]; ok {
		return job, nil
	}
	return nil, fmt.Errorf("job %q not found", id)
}

// run has a planner answer a question and returns everything about it.
func run(t *testing.T, model *scripted, pool *office, question string) (said []ai.Message, events []Event, err error) {
	t.Helper()
	p := &Planner{Model: model, ModelName: "some-model", Pool: pool, Workloads: []Workload{{"primes", "Counts primes."}, {"wordcount", "Counts words."}}}
	said, err = p.Run(context.Background(), []ai.Message{{Role: ai.User, Content: question}}, func(e Event) { events = append(events, e) })
	return said, events, err
}

func kinds(events []Event) string {
	var out []string
	for _, e := range events {
		out = append(out, e.Kind)
	}
	return strings.Join(out, " ")
}

func TestAQuestionThatNeedsNoComputingIsJustAnswered(t *testing.T) {
	model := &scripted{replies: []ai.Message{{Content: "A boulder is a large rock."}}}
	pool := &office{}
	said, events, err := run(t, model, pool, "What is a boulder?")
	if err != nil {
		t.Fatal(err)
	}
	if len(said) != 1 || said[0].Role != ai.Assistant || said[0].Content != "A boulder is a large rock." || len(pool.submitted) != 0 {
		t.Errorf("said %+v, having submitted %d jobs", said, len(pool.submitted))
	}
	// What the model says is passed on as it says it.
	var heard string
	for _, e := range events {
		if e.Kind != Text {
			t.Errorf("an event of kind %q for a plain answer", e.Kind)
		}
		heard += e.Text
	}
	if heard != "A boulder is a large rock." || len(events) < 2 {
		t.Errorf("heard %q in %d pieces", heard, len(events))
	}
	// The model was told what it is, what the pool can run, and the tools.
	asked := model.asked[0]
	if asked.Model != "some-model" || asked.Messages[0].Role != ai.System || !strings.Contains(asked.Messages[0].Content, "- primes: Counts primes.") ||
		!strings.Contains(asked.Messages[0].Content, "- wordcount: Counts words.") || asked.Messages[1].Content != "What is a boulder?" || len(asked.Tools) != 2 {
		t.Errorf("the model was asked %+v", asked)
	}
}

func TestThePlannerComputesAndThenAnswersFromTheResult(t *testing.T) {
	model := &scripted{replies: []ai.Message{
		calling("call-1", "run_job", `{"workload":"primes","params":{"from":0,"to":100},"tasks":2}`),
		{Content: "There are 25 primes below 100."},
	}}
	pool := &office{result: `{"count":25}`}
	said, events, err := run(t, model, pool, "How many primes are there below 100?")
	if err != nil {
		t.Fatal(err)
	}
	// The pool was given the job the model asked for.
	if len(pool.submitted) != 1 || pool.submitted[0].GetWorkload() != "primes" || string(pool.submitted[0].GetParams()) != `{"from":0,"to":100}` || pool.submitted[0].GetMaxTasks() != 2 {
		t.Fatalf("the pool was given %v", pool.submitted)
	}
	// What was said: the model's request, what came back, and its answer.
	if len(said) != 3 || len(said[0].Calls) != 1 || said[1].Role != ai.ToolRole || said[1].CallID != "call-1" || said[1].Name != "run_job" || said[2].Content != "There are 25 primes below 100." {
		t.Fatalf("said %+v", said)
	}
	var result map[string]any
	if err := json.Unmarshal([]byte(said[1].Content), &result); err != nil {
		t.Fatal(err)
	}
	if result["job_id"] != "job-1" || result["state"] != "succeeded" || result["seconds"] != 1.5 || result["tasks"] != 2.0 || result["result"].(map[string]any)["count"] != 25.0 {
		t.Errorf("the model was told %v", result)
	}
	// The second time it was asked, it had the result in front of it.
	second := model.asked[1].Messages
	if len(second) != 4 || second[3].Role != ai.ToolRole || second[3].Content != said[1].Content {
		t.Errorf("the model was then asked %+v", second)
	}
	// And whoever was watching saw each step.
	if !strings.HasPrefix(kinds(events), "call job result text") || events[0].Tool != "run_job" || events[1].JobID != "job-1" || events[2].Tool != "run_job" {
		t.Errorf("events: %s", kinds(events))
	}
}

func TestToolsThatFailTellTheModelWhy(t *testing.T) {
	for name, tt := range map[string]struct {
		call ai.Message
		pool office
		want string
	}{
		"a tool there is none of":          {calling("c", "launch_rocket", `{}`), office{}, `there is no tool called \"launch_rocket\"`},
		"arguments that are not run_job's": {calling("c", "run_job", `["primes"]`), office{}, "not what run_job takes"},
		"a job the pool refuses":           {calling("c", "run_job", `{"workload":"nope","params":{}}`), office{refuse: errors.New(`unknown workload "nope"`)}, `unknown workload \"nope\"`},
		"a job that cannot be followed":    {calling("c", "run_job", `{"workload":"primes","params":{}}`), office{lose: errors.New("the coordinator stopped")}, "job-1 was started and could not be followed"},
		"arguments that are not get_job's": {calling("c", "get_job", `"job-1"`), office{}, "not what get_job takes"},
		"a job that is not there":          {calling("c", "get_job", `{"job_id":"job-9"}`), office{}, `job \"job-9\" not found`},
	} {
		model := &scripted{replies: []ai.Message{tt.call, {Content: "I could not do that."}}}
		said, _, err := run(t, model, &tt.pool, "Do something.")
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		// The failure goes to the model, which gets to answer.
		if len(said) != 3 || !strings.HasPrefix(said[1].Content, `{"error":`) || !strings.Contains(said[1].Content, tt.want) || said[2].Content != "I could not do that." {
			t.Errorf("%s: the model was told %s, want an error containing %s", name, said[1].Content, tt.want)
		}
	}
}

func TestLookingUpAJobRunEarlier(t *testing.T) {
	pool := &office{result: `{"count":25}`}
	model := &scripted{replies: []ai.Message{
		calling("c1", "run_job", `{"workload":"primes","params":{}}`),
		calling("c2", "get_job", `{"job_id":"job-1"}`),
		{Content: "Done."},
	}}
	said, _, err := run(t, model, pool, "Run it and check it.")
	if err != nil || len(said) != 5 || said[3].Content != said[1].Content {
		t.Errorf("said %+v, %v; want get_job to describe the job as run_job did", said, err)
	}
}

func TestWhatTheModelIsToldOfAJob(t *testing.T) {
	at := timestamppb.New(time.Unix(1_700_000_000, 0))
	for name, tt := range map[string]struct {
		job  *pb.Job
		want map[string]string
	}{
		"a result that is not JSON": {&pb.Job{JobId: "j", State: pb.JobState_JOB_STATE_SUCCEEDED, Result: []byte("forty-two"), CreatedAt: at, FinishedAt: at},
			map[string]string{"result": `"forty-two"`, "state": `"succeeded"`}},
		"a failed job": {&pb.Job{JobId: "j", State: pb.JobState_JOB_STATE_FAILED, Error: "task 0 failed after 3 attempts: boom", CreatedAt: at, FinishedAt: at},
			map[string]string{"state": `"failed"`, "error": `"task 0 failed after 3 attempts: boom"`}},
		"a job still running": {&pb.Job{JobId: "j", State: pb.JobState_JOB_STATE_RUNNING, CreatedAt: at},
			map[string]string{"state": `"running"`}},
		"a job with stored outputs": {&pb.Job{JobId: "j", State: pb.JobState_JOB_STATE_SUCCEEDED, Result: []byte(`{"words":9}`), OutputBlobs: []string{"bafy-table"}, CreatedAt: at, FinishedAt: at},
			map[string]string{"result": `{"words":9}`, "stored_outputs": `["bafy-table"]`}},
	} {
		encoded, _ := json.Marshal(describe(tt.job))
		var got map[string]json.RawMessage
		json.Unmarshal(encoded, &got)
		for field, want := range tt.want {
			if string(got[field]) != want {
				t.Errorf("%s: %s is %s, want %s", name, field, got[field], want)
			}
		}
		if _, timed := got["seconds"]; timed != (tt.job.GetFinishedAt() != nil) {
			t.Errorf("%s: seconds given: %v", name, timed)
		}
	}
	// A result too long to be worth reading is cut.
	long := describe(&pb.Job{JobId: "j", Result: []byte(strings.Repeat("x", 20_000)), CreatedAt: at})
	if text := long["result"].(string); len(text) > 8300 || !strings.HasSuffix(text, "[cut short]") {
		t.Errorf("a long result is passed on as %d bytes", len(text))
	}
	// And a job with no result has none to give.
	if _, has := describe(&pb.Job{JobId: "j", CreatedAt: at})["result"]; has {
		t.Error("a job with no result was given one")
	}
}

func TestAModelThatFailsOrNeverFinishes(t *testing.T) {
	// It fails after one step: what was said until then is kept.
	failing := &scripted{replies: []ai.Message{calling("c", "run_job", `{"workload":"primes","params":{}}`)}, err: errors.New("the model service is down")}
	said, _, err := run(t, failing, &office{result: "{}"}, "Count.")
	if err == nil || err.Error() != "the model service is down" || len(said) != 2 {
		t.Errorf("said %d messages, error %v", len(said), err)
	}

	// It keeps asking for more and never answers.
	var endless []ai.Message
	for range 20 {
		endless = append(endless, calling("c", "get_job", `{"job_id":"x"}`))
	}
	p := &Planner{Model: &scripted{replies: endless}, Pool: &office{}, MaxSteps: 3}
	said, err = p.Run(context.Background(), []ai.Message{{Role: ai.User, Content: "Loop."}}, func(Event) {})
	if !errors.Is(err, ErrTooManySteps) || len(said) != 6 {
		t.Errorf("after three steps: %d messages, %v", len(said), err)
	}
	// Left to itself it allows eight.
	model := &scripted{replies: endless}
	p = &Planner{Model: model, Pool: &office{}}
	if _, err := p.Run(context.Background(), nil, func(Event) {}); !errors.Is(err, ErrTooManySteps) || len(model.asked) != 8 {
		t.Errorf("with no limit given the model was asked %d times, %v", len(model.asked), err)
	}
}

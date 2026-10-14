package planner

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/sisyphus-network/Sisyphus/packages/ai"
)

// A small model sometimes writes the call it means to make as text. The
// planner takes it as the call: the job runs and the question is answered.
func TestACallAModelWroteOutAsTextIsMade(t *testing.T) {
	model := &scripted{replies: []ai.Message{
		{Content: `{"name": "run_job", "parameters": {"workload": "primes", "params": {"from": 0, "to": 100}, "tasks": None, "private": False}}`, Raw: []byte(`{"as":"the service gave it"}`)},
		{Content: "There are 25 primes below 100."},
	}}
	pool := &office{result: `{"count":25}`}
	said, events, err := run(t, model, pool, "How many primes are there below 100?")
	if err != nil {
		t.Fatal(err)
	}
	if len(pool.submitted) != 1 || pool.submitted[0].GetWorkload() != "primes" || string(pool.submitted[0].GetParams()) != `{"from": 0, "to": 100}` || pool.submitted[0].GetKey() != nil {
		t.Fatalf("the pool was given %v", pool.submitted)
	}
	// It goes into what was said as a call, not as the text it came as, so
	// the model reads back a conversation of the usual shape.
	if len(said) != 3 || said[0].Content != "" || said[0].Raw != nil || len(said[0].Calls) != 1 || said[0].Calls[0].ID != "written-0" ||
		said[1].Role != ai.ToolRole || said[1].CallID != "written-0" || said[1].Name != "run_job" || said[2].Content != "There are 25 primes below 100." {
		t.Fatalf("said %+v", said)
	}
	// The person is not shown the call as text: the first thing they hear
	// of it is that it is being made.
	if got := kinds(events); got != "call job result text text text text text text" {
		t.Errorf("events: %s", got)
	}
	// What the model was shown the second time has the call in it.
	if again := model.asked[1].Messages; len(again[2].Calls) != 1 || again[3].CallID != "written-0" {
		t.Errorf("the model was shown %+v", again)
	}
}

func TestWhichRepliesAreACallWrittenOut(t *testing.T) {
	tests := []struct {
		name, text string
		arguments  string // empty for a reply that is not a call
	}{
		{"arguments under their own name", `{"name":"get_job","arguments":{"job_id":"abc"}}`, `{"job_id":"abc"}`},
		{"arguments called parameters", ` {"name":"get_job","parameters":{"job_id":"abc"}} `, `{"job_id":"abc"}`},
		{"in a code fence", "```json\n{\"name\":\"get_job\",\"arguments\":{\"job_id\":\"abc\"}}\n```", `{"job_id":"abc"}`},
		{"in a fence with no language", "```\n{\"name\":\"get_job\",\"arguments\":{\"job_id\":\"abc\"}}\n```", `{"job_id":"abc"}`},
		{"after the mark some models put before a call", `<|python_tag|>{"name":"get_job","arguments":{"job_id":"abc"}}`, `{"job_id":"abc"}`},
		{"with Python's values", `{"name":"run_job","arguments":{"workload":"primes","private":True,"tasks":None,"params":{"odd":False}}}`, `{"workload":"primes","private":true,"tasks":null,"params":{"odd":false}}`},
		{"with those words inside text, which are left", `{"name":"run_job","arguments":{"workload":"None","note":"say \"True\"","private":False}}`, `{"workload":"None","note":"say \"True\"","private":false}`},

		{"an answer", "There are 25 primes below 100.", ""},
		{"an answer with a call in it", `I will run {"name":"get_job","arguments":{"job_id":"abc"}}`, ""},
		{"a call and then more", `{"name":"get_job","arguments":{"job_id":"abc"}} and then I will tell you`, ""},
		{"a fence that is not closed", "```json\n{\"name\":\"get_job\",\"arguments\":{\"job_id\":\"abc\"}}", ""},
		{"a tool there is not", `{"name":"delete_everything","arguments":{}}`, ""},
		{"some other object", `{"count":25}`, ""},
		{"arguments that are not an object", `{"name":"get_job","arguments":"abc"}`, ""},
		{"no arguments", `{"name":"get_job"}`, ""},
		{"what is not JSON either way", `{"name":"get_job","arguments":{"job_id":abc}}`, ""},
		{"text that ends in the middle of an escape", `{"name":"get_job","arguments":{"job_id":"abc\`, ""},
		{"nothing", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			call, written := writtenOut(tt.text)
			if written != (tt.arguments != "") || string(call.Arguments) != tt.arguments {
				t.Errorf("writtenOut = %s %s, %v; want %q", call.Name, call.Arguments, written, tt.arguments)
			}
		})
	}
}

// A reply that makes a call and also says something that reads like one is
// a reply that makes a call: the text is left alone.
func TestACallThatWasMadeIsNotLookedForInTheText(t *testing.T) {
	reply := calling("call-1", "get_job", `{"job_id":"job-0"}`)
	reply.Content = `{"name":"get_job","arguments":{"job_id":"other"}}`
	model := &scripted{replies: []ai.Message{reply, {Content: "It is not there."}}}
	said, _, err := run(t, model, &office{}, "What became of that job?")
	if err != nil {
		t.Fatal(err)
	}
	if len(said[0].Calls) != 1 || said[0].Calls[0].ID != "call-1" || said[0].Content == "" {
		t.Errorf("said %+v", said[0])
	}
}

// Text is held back only while it may be a call written out. What turns out
// to be an answer is shown, all of it, in the order it came.
func TestTextThatOpensLikeACallAndIsNotOneIsStillShown(t *testing.T) {
	tests := map[string]string{
		"an answer that is JSON":             `{"count": 25} is what you asked for.`,
		"an answer that opens with code":     "```go\nfmt.Println(25)\n``` prints it.",
		"an answer that opens with a space":  " There are 25.",
		"an answer that is a brace and more": "{ " + strings.Repeat("and more ", 600),
	}
	for name, answer := range tests {
		t.Run(name, func(t *testing.T) {
			_, events, err := run(t, &scripted{replies: []ai.Message{{Content: answer}}}, &office{}, "How many?")
			if err != nil {
				t.Fatal(err)
			}
			var heard string
			for _, e := range events {
				heard += e.Text
			}
			if heard != answer {
				t.Errorf("heard %q", heard)
			}
		})
	}
	// One that runs on is shown before it ends, not all at the end.
	_, events, _ := run(t, &scripted{replies: []ai.Message{{Content: tests["an answer that is a brace and more"]}}}, &office{}, "How many?")
	if len(events) < 2 {
		t.Errorf("a long answer came in %d pieces", len(events))
	}
}

// A model that fails part way through text that was being held back has
// what it said shown, as it would have been.
func TestHeldTextIsShownWhenTheModelFails(t *testing.T) {
	var heard string
	p := &Planner{Model: stopsShort{`{"name": "run_`}, Pool: &office{}}
	_, err := p.Run(context.Background(), []ai.Message{{Role: ai.User, Content: "How many?"}}, func(e Event) { heard += e.Text })
	if err == nil || heard != `{"name": "run_` {
		t.Errorf("heard %q, and the error was %v", heard, err)
	}
}

// stopsShort is a model that says something and then fails.
type stopsShort struct{ says string }

func (m stopsShort) Chat(_ context.Context, _ ai.Request, said func(string)) (ai.Message, error) {
	said(m.says)
	return ai.Message{}, errors.New("the model went away")
}

func (stopsShort) Models(context.Context) ([]ai.Model, error) { return nil, nil }

package planner

import (
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
	if got := kinds(events); !strings.HasSuffix(got, "text call job result text text text text text text") {
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

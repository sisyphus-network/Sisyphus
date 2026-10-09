package runtime

import (
	"encoding/json"
	"strings"
	"testing"
)

const pipeline = `{"steps":[
	{"name":"count","workload":"wordcount","params":{"input":"bafy-input"},"tasks":4,"timeout_seconds":30},
	{"name":"low","workload":"primes","params":{"from":0,"to":1000}},
	{"name":"report","workload":"container","after":["low"],"params":{
		"image":"alpine:3.20","input":"${count.result.output}","tasks":"${low.result.count}",
		"command":["echo","job ${count.job_id} counted ${count.result.words} words; first file ${count.outputs.0}; all of it ${low.result}"]}},
	{"name":"bare","workload":"primes","after":["report"]}]}`

func TestAGraphIsStepsThatWaitForWhatTheyReferTo(t *testing.T) {
	graph := Graph{}
	payloads, err := graph.Split(ctx, nil, []byte(pipeline), 9)
	if err != nil || len(payloads) != 4 {
		t.Fatalf("Split = %d payloads, %v", len(payloads), err)
	}
	for i, want := range [][]int{nil, nil, {0, 1}, {2}} {
		got := graph.After(payloads[i])
		if len(got) != len(want) {
			t.Fatalf("step %d waits for %v, want %v", i, got, want)
		}
		for j := range want {
			if got[j] != want[j] {
				t.Errorf("step %d waits for %v, want %v", i, got, want)
			}
		}
	}
	// A step with nothing to wait for is the job its step describes.
	first, err := graph.Child(payloads[0], make([][]byte, 4))
	if err != nil || first.Name != "count" || first.Workload != "wordcount" || first.Tasks != 4 || first.Timeout.Seconds() != 30 || string(first.Params) != `{"input":"bafy-input"}` {
		t.Fatalf("the first step is %+v, %v", first, err)
	}
	// What a step produced is put where a later one refers to it: whole
	// where the reference is all there is, as text where it is part.
	outputs := make([][]byte, 4)
	outputs[0] = graph.Output(payloads[0], "job-a", []byte(`{"output":"bafy-counts","words":339}`), []string{"bafy-counts"})
	outputs[1] = graph.Output(payloads[1], "job-b", []byte(`{"count":168}`), nil)
	report, err := graph.Child(payloads[2], outputs)
	if err != nil {
		t.Fatal(err)
	}
	var params struct {
		Input   string
		Tasks   int
		Command []string
	}
	if err := json.Unmarshal(report.Params, &params); err != nil {
		t.Fatalf("%s: %v", report.Params, err)
	}
	if params.Input != "bafy-counts" || params.Tasks != 168 || params.Command[1] != `job job-a counted 339 words; first file bafy-counts; all of it {"count":168}` {
		t.Errorf("the report step's parameters: %s", report.Params)
	}
	// A step with no parameters has none, and a result that is not JSON is kept as text.
	if bare, err := graph.Child(payloads[3], outputs); err != nil || bare.Params != nil {
		t.Errorf("a step with no parameters: %+v, %v", bare, err)
	}
	outputs[2] = graph.Output(payloads[2], "job-c", []byte("plain words"), nil)
	outputs[3] = graph.Output(payloads[3], "job-d", nil, nil)
	result, err := graph.Aggregate(ctx, nil, outputs)
	if err != nil || !strings.Contains(string(result), `"step":"report","job_id":"job-c","result":"plain words"`) || strings.Contains(string(result), `"result":null`) {
		t.Fatalf("Aggregate = %s, %v", result, err)
	}
	// The graph's own result is its last step's, when that has one.
	outputs[3] = graph.Output(payloads[3], "job-d", []byte(`{"count":25}`), nil)
	if result, _ := graph.Aggregate(ctx, nil, outputs); !strings.HasPrefix(string(result), `{"result":{"count":25},"steps":[`) {
		t.Errorf("Aggregate = %s", result)
	}
	if _, err := graph.Aggregate(ctx, nil, [][]byte{[]byte("not JSON")}); err == nil || !strings.Contains(err.Error(), "graph step 0 output") {
		t.Errorf("an output that is none: %v", err)
	}
	if _, err := graph.Execute(ctx, nil, payloads[0]); err == nil || !strings.Contains(err.Error(), "not tasks for a worker") {
		t.Errorf("a step given to a worker: %v", err)
	}
	registry := Builtin().With(graph)
	if graph.Name() != "graph" || !strings.Contains(graph.Describe(), "${name.result}") || !registry.Composite("graph") || registry.Composite("primes") {
		t.Errorf("graph says %q", graph.Describe())
	}
}

func TestAGraphThatCannotBeRunIsRefused(t *testing.T) {
	many := strings.Repeat(`{"name":"s","workload":"primes"},`, MaxGraphSteps+1)
	for params, want := range map[string]string{
		`not JSON`:     "graph parameters",
		`{"steps":[]}`: "must have steps",
		`{"steps":[` + strings.TrimSuffix(many, ",") + `]}`:                             "at most 64 steps, and this has 65",
		`{"steps":[{"name":"a b","workload":"primes"}]}`:                                `step 1 needs a name of letters, digits, - and _, not "a b"`,
		`{"steps":[{"name":"a","workload":"primes"},{"name":"a","workload":"primes"}]}`: `two steps are called "a"`,
		`{"steps":[{"name":"a"}]}`:                                                      `step "a" names no workload`,
		`{"steps":[{"name":"a","workload":"primes","after":["b"]}]}`:                    `waits for a step "b" that there is none of`,
		`{"steps":[{"name":"a","workload":"primes","params":{"to":"${a.result}"}}]}`:    `step "a" waits for itself`,
		`{"steps":[{"name":"a","workload":"primes","after":["c"]},{"name":"b","workload":"primes","after":["a"]},{"name":"c","workload":"primes","after":["b"]}]}`: "waits for a step that waits for it",
	} {
		if _, err := (Graph{}).Split(ctx, nil, []byte(params), 1); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%.60s: %v, want %q", params, err, want)
		}
	}
}

func TestAStepThatRefersToWhatWasNotProducedFails(t *testing.T) {
	graph := Graph{}
	steps := func(params string) [][]byte {
		payloads, err := graph.Split(ctx, nil, []byte(`{"steps":[{"name":"a","workload":"primes"},{"name":"b","workload":"primes","params":`+params+`}]}`), 1)
		if err != nil {
			t.Fatal(err)
		}
		return payloads
	}
	for params, want := range map[string]string{
		`{"to":"${a.result.missing}"}`:          "${a.result.missing} refers to something step \"a\" did not produce",
		`{"to":"up to ${a.result.list.9}"}`:     "did not produce",
		`{"to":["${a.result.list.x}"]}`:         "did not produce",
		`{"to":{"n":"${a.result.count.deep}"}}`: "did not produce",
		`{"to":"${a.outputs.0}"}`:               "did not produce",
	} {
		payloads := steps(params)
		outputs := [][]byte{graph.Output(payloads[0], "job-a", []byte(`{"count":25,"list":[1,2]}`), nil), nil}
		if _, err := graph.Child(payloads[1], outputs); err == nil || !strings.Contains(err.Error(), want) || !strings.Contains(err.Error(), `step "b"`) {
			t.Errorf("%s: %v, want %q", params, err, want)
		}
	}
	// What is there is found, in lists by position.
	payloads := steps(`{"to":"${a.result.list.1}","more":[true,7,"${a.result.count}"]}`)
	outputs := [][]byte{graph.Output(payloads[0], "job-a", []byte(`{"count":25,"list":[1,2]}`), nil), nil}
	if child, err := graph.Child(payloads[1], outputs); err != nil || string(child.Params) != `{"more":[true,7,25],"to":2}` {
		t.Errorf("found in a list: %s, %v", child.Params, err)
	}
}

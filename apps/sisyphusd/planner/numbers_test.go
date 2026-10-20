package planner

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"testing"

	"github.com/sisyphus-network/Sisyphus/packages/ai"
)

// The V0 run's follow-up, replayed (#532): the second job's result reaches
// the model with the comparison already worked out, and when the answer
// copies the count wrong anyway, a note under it gives the right one.
func TestAFollowUpIsHandedItsComparisonAndAMisquotedCountIsPointedOut(t *testing.T) {
	pool := &office{results: []string{`{"count":8007105059}`, `{"count":7573900598}`}}
	model := &scripted{replies: []ai.Message{
		calling("c1", "run_job", `{"workload":"primes","params":{"from":0,"to":200000000000}}`),
		{Content: "There are 8,007,105,059 primes below 200 billion."},
		calling("c2", "run_job", `{"workload":"primes","params":{"from":200000000000,"to":400000000000}}`),
		{Content: "There are 7,573,905,598 primes between 200 and 400 billion, 433,204,461 fewer than below 200 billion."},
	}}
	p := &Planner{Model: model, ModelName: "some-model", Pool: pool, Workloads: []Workload{{"primes", "Counts primes."}}}
	first := []ai.Message{{Role: ai.User, Content: "How many primes are there below 200 billion?"}}
	said, err := p.Run(context.Background(), first, func(Event) {})
	if err != nil {
		t.Fatal(err)
	}
	if got := said[len(said)-1].Content; got != "There are 8,007,105,059 primes below 200 billion." {
		t.Errorf("a correctly quoted answer was changed to %q", got)
	}
	history := append(first, said...)
	history = append(history, ai.Message{Role: ai.User, Content: "And between 200 and 400 billion? More or fewer, by how much?"})
	var events []Event
	said, err = p.Run(context.Background(), history, func(e Event) { events = append(events, e) })
	if err != nil {
		t.Fatal(err)
	}

	var tool struct {
		Compared []map[string]any `json:"compared_with"`
	}
	if err := json.Unmarshal([]byte(said[1].Content), &tool); err != nil || len(tool.Compared) != 1 {
		t.Fatalf("the second job's result as the model saw it: %s", said[1].Content)
	}
	if got := tool.Compared[0]["in_words"]; got != "433,204,461 fewer than job job-1's 8,007,105,059" {
		t.Errorf("the comparison in words: %v", got)
	}
	if got := tool.Compared[0]["difference"]; got != -433204461.0 {
		t.Errorf("the difference: %v", got)
	}

	const note = "\n\nNote: the answer says 7,573,905,598, but job job-2 returned 7,573,900,598."
	if got := said[len(said)-1].Content; !strings.HasSuffix(got, note) {
		t.Errorf("the answer as kept: %q", got)
	}
	if last := events[len(events)-1]; last.Kind != Text || last.Text != note {
		t.Errorf("the last event: %+v", last)
	}
}

func TestOnlyNumbersNearlyButNotQuiteAResultArePointedOut(t *testing.T) {
	figures := []figure{{job: "a", field: "count", value: 8007105059}, {job: "b", field: "count", value: 8007105099}, {job: "c", field: "count", value: 12345}}
	for _, tt := range []struct{ answer, want string }{
		{"8,007,105,059 primes, in 64.978 s, over 100 tasks", ""},
		{"exactly 8007105059", ""},
		{"about 9,999,999,999", ""},
		{"12,346 words", "\n\nNote: the answer says 12,346, but job c returned 12,345."},
		{"8 007 105 069 of them", "\n\nNote: the answer says 8 007 105 069, which is none of the results (8,007,105,059 from job a, 8,007,105,099 from job b)."},
		{"1234 is short", ""},
	} {
		if got := misquoted(tt.answer, figures); got != tt.want {
			t.Errorf("%q: %q, want %q", tt.answer, got, tt.want)
		}
	}
	if got := misquoted("12,346 of them", nil); got != "" {
		t.Errorf("with no results to check against: %q", got)
	}
}

func TestComparisonsAreWorkedOutOnlyForTheSameFieldOfOtherJobs(t *testing.T) {
	earlier := []figure{{job: "j1", field: "count", value: 10}, {job: "j1", field: "total", value: 7}, {job: "j2", field: "count", value: 3}}
	got := withComparisons(`{"job_id":"j2","result":{"count":10,"total":5,"note":"x"}}`, earlier)
	var out struct {
		Compared []struct {
			Job     string `json:"job_id"`
			Field   string `json:"field"`
			InWords string `json:"in_words"`
		} `json:"compared_with"`
	}
	if err := json.Unmarshal([]byte(got), &out); err != nil {
		t.Fatal(err)
	}
	want := []string{"count/the same as job j1's 10", "total/2 less than job j1's 7"}
	if len(out.Compared) != len(want) {
		t.Fatalf("compared: %s", got)
	}
	for i, c := range out.Compared {
		if c.Field+"/"+c.InWords != want[i] || c.Job != "j1" {
			t.Errorf("comparison %d: %+v, want %s", i, c, want[i])
		}
	}
	if got := inWords("total", 3, figure{job: "j", value: 1}); got != "3 more than job j's 1" {
		t.Errorf("a larger total: %q", got)
	}
	if got := inWords("count", 1234, figure{job: "j", value: 1}); got != "1,234 more than job j's 1" {
		t.Errorf("a larger count: %q", got)
	}
	for _, unchanged := range []string{`not json`, `{"error":"no such job"}`} {
		if got := withComparisons(unchanged, earlier); got != unchanged {
			t.Errorf("%s became %s", unchanged, got)
		}
	}
	if got := withComparisons(`{"job_id":"j3","result":{"words":1}}`, earlier); got != `{"job_id":"j3","result":{"words":1}}` {
		t.Errorf("a result with nothing to compare became %s", got)
	}
}

func TestFiguresAreReadOnlyFromJobResultsTheToolsReturned(t *testing.T) {
	messages := []ai.Message{
		{Role: ai.User, Content: `{"job_id":"u","result":{"count":1}}`},
		{Role: ai.ToolRole, Content: `not json`},
		{Role: ai.ToolRole, Content: `{"error":"no such job"}`},
		{Role: ai.ToolRole, Content: `{"job_id":"t","result":{"count":5,"ratio":0.5,"name":"x"},"compared_with":[{"job_id":"s","field":"count","difference":-2}]}`},
		{Role: ai.ToolRole, Content: `{"job_id":"v","result":"plain text"}`},
	}
	got := figuresIn(messages)
	want := []figure{{job: "t", field: "count", value: 5}, {job: "t", field: "difference from s", value: 2}}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("figures: %+v, want %+v", got, want)
	}
	if got := grouped(-1234567); got != "-1,234,567" {
		t.Errorf("grouped: %q", got)
	}
	if abs(3) != 3 {
		t.Error("abs of a positive number")
	}
}

// A result can be any whole number, so a difference too large or small for
// an int64 is left out, never wrapped round or quoted with the wrong sign.
func TestAComparisonThatWouldOverflowIsLeftOut(t *testing.T) {
	for _, tt := range []struct {
		name          string
		this, earlier int64
	}{
		{"above the largest", math.MaxInt64, -1},
		{"below the smallest", math.MinInt64, 1},
		{"exactly the smallest", -1, math.MaxInt64},
	} {
		result := fmt.Sprintf(`{"job_id":"j2","result":{"count":%d}}`, tt.this)
		if got := withComparisons(result, []figure{{job: "j1", field: "count", value: tt.earlier}}); got != result {
			t.Errorf("%s: %s", tt.name, got)
		}
	}
	if d, held := subtract(math.MaxInt64, 1); !held || d != math.MaxInt64-1 {
		t.Errorf("a difference that fits: %d, %v", d, held)
	}
}

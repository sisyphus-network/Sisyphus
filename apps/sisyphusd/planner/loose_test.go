package planner

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/sisyphus-network/Sisyphus/packages/sealed"
)

// A small model often puts true, false and numbers in quotes. The job it
// meant is run all the same.
func TestQuotedFlagsAndCountsAreTakenAtTheirWord(t *testing.T) {
	for name, tt := range map[string]struct {
		arguments string
		tasks     uint32
		private   bool
	}{
		"as JSON gives them":  {`{"workload":"primes","params":{},"tasks":100,"private":false}`, 100, false},
		"in quotes":           {`{"workload":"primes","params":{},"tasks":"100","private":"false"}`, 100, false},
		"quoted, with spaces": {`{"workload":"primes","params":{},"tasks":" 7 ","private":" TRUE "}`, 7, true},
		"quoted true":         {`{"workload":"primes","params":{},"private":"true"}`, 0, true},
		"left out":            {`{"workload":"primes","params":{}}`, 0, false},
	} {
		pool := &office{}
		p := &Planner{Pool: pool, SealingKey: func() (sealed.Key, error) { return sealed.Key{1}, nil }}
		if _, err := p.runJob(context.Background(), json.RawMessage(tt.arguments), func(Event) {}); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if len(pool.submitted) != 1 {
			t.Fatalf("%s: submitted %v", name, pool.submitted)
		}
		got := pool.submitted[0]
		if got.GetMaxTasks() != tt.tasks || (len(got.GetKey()) > 0) != tt.private {
			t.Errorf("%s: the pool was given %d tasks, private %v; want %d, %v", name, got.GetMaxTasks(), len(got.GetKey()) > 0, tt.tasks, tt.private)
		}
	}
}

// What is neither, quoted or not, is still refused, and the model is told
// what was wrong with it.
func TestFlagsAndCountsThatAreNeitherAreRefused(t *testing.T) {
	for name, tt := range map[string]struct {
		arguments string
		want      string
	}{
		"a word that is not true or false": {`{"workload":"primes","params":{},"private":"maybe"}`, `"maybe" is not true or false`},
		"a number for a flag":              {`{"workload":"primes","params":{},"private":2}`, `2 is not true or false`},
		"a word for a count":               {`{"workload":"primes","params":{},"tasks":"many"}`, `"many" is not a whole number from 0 to 4294967295`},
		"a negative count":                 {`{"workload":"primes","params":{},"tasks":-1}`, `-1 is not a whole number from 0 to 4294967295`},
		"a count too big, quoted":          {`{"workload":"primes","params":{},"tasks":"4294967296"}`, `"4294967296" is not a whole number`},
	} {
		pool := &office{}
		p := &Planner{Pool: pool}
		_, err := p.runJob(context.Background(), json.RawMessage(tt.arguments), func(Event) {})
		if err == nil || !strings.Contains(err.Error(), "not what run_job takes") || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("%s: got %v, want an error saying %s", name, err, tt.want)
		}
		if len(pool.submitted) != 0 {
			t.Errorf("%s: the pool was given %v", name, pool.submitted)
		}
	}
}

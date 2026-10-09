package main

import (
	"context"
	"strings"
	"testing"

	"github.com/sisyphus-network/Sisyphus/packages/runtime"
)

// These tests run verified jobs from the command line on real nodes, one of
// which does not do the work it is given.

// lying is primes as run by a worker that does no counting: whatever range
// it is given, it says it found one prime.
type lying struct{ runtime.Primes }

func (lying) Execute(context.Context, runtime.Blobs, []byte) ([]byte, error) {
	return []byte(`{"count":1}`), nil
}

// withALiar has any node started under the name "liar" before the test
// ends run primes that way.
func withALiar(t *testing.T) {
	t.Helper()
	honest := runsAs
	runsAs = func(name string, runs *runtime.Registry) *runtime.Registry {
		if name == "liar" {
			return runs.With(lying{})
		}
		return honest(name, runs)
	}
	// Put back once the nodes the test started have stopped.
	t.Cleanup(func() { runsAs = honest })
}

// startCrew starts a node that only coordinates, and a worker of one slot
// under each of the given names, and returns the coordinator's address.
func startCrew(t *testing.T, names ...string) string {
	t.Helper()
	addr := freeAddr(t)
	startDaemon(t, "--role", "coordinator", "--listen", addr, "--name", "boss")
	for _, name := range names {
		startDaemon(t, "--role", "worker", "--coordinator", addr, "--name", name, "--slots", "1")
	}
	for _, name := range names {
		waitForOutput(t, name, "nodes", "--addr", addr)
	}
	return addr
}

func TestCLIVerifiedJobSucceedsThoughOneOfItsThreeWorkersLies(t *testing.T) {
	withALiar(t)
	addr := startCrew(t, "one", "two", "liar")

	// Three tasks, each for two workers, and three workers of one slot:
	// every worker is given something at once, the liar too.
	out := mustCLI(t, "job", "submit", "--addr", addr, "--verify", "2", "--tasks", "3", "--params", `{"from":0,"to":1000000}`)
	for _, want := range []string{"3 task(s), each verified by 2 workers", "succeeded in", `{"count":78498}`} {
		if !strings.Contains(out, want) {
			t.Errorf("submit output lacks %q:\n%s", want, out)
		}
	}
	id := strings.TrimSuffix(strings.Fields(out)[1], ":")

	// The log names the worker that was outvoted, and whose results stood.
	logs := mustCLI(t, "job", "logs", "--addr", addr, id)
	if !strings.Contains(logs, "task-disagreed: ") || !strings.Contains(logs, "liar returned ") || !strings.Contains(logs, "returned the same result; liar returned another") {
		t.Errorf("job logs printed:\n%s", logs)
	}
	if strings.Contains(logs, "liar returned the same result") || strings.Contains(logs, ", liar returned the same") {
		t.Errorf("the liar is said to have agreed:\n%s", logs)
	}
	if got := mustCLI(t, "job", "get", "--addr", addr, id); !strings.Contains(got, "succeeded, workload primes, each verified by 2 workers") {
		t.Errorf("job get printed:\n%s", got)
	}
	// So does the record, which the job still gives.
	record := mustCLI(t, "job", "record", "--addr", addr, "--verify", id)
	if !strings.Contains(record, `"agreed": false`) || !strings.Contains(record, `"agreed": true`) || !strings.Contains(record, `"verify": 2`) ||
		!strings.Contains(record, "verified: the job as the node holds it gives this record") {
		t.Errorf("job record printed:\n%s", record)
	}
}

func TestCLIVerifiedJobFailsWhenItsWorkersDisagreeAndNobodyIsLeftToAsk(t *testing.T) {
	withALiar(t)
	addr := startCrew(t, "one", "liar")

	_, err := cli(t, "job", "submit", "--addr", addr, "--verify", "2", "--tasks", "1", "--params", `{"from":0,"to":1000000}`)
	if err == nil {
		t.Fatal("a job whose two workers returned different results succeeded")
	}
	for _, want := range []string{
		"task 0 could not be verified: its workers returned different results, no 2 of them the same, and no other connected worker can be asked",
		"liar returned ", "one returned ",
		"Verification is for work that gives the same result every time it is run",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error lacks %q: %v", want, err)
		}
	}

	// What cannot be verified at all is refused when it is submitted.
	if _, err := cli(t, "job", "submit", "--addr", addr, "--verify", "3", "--params", `{"from":0,"to":100}`); err == nil ||
		!strings.Contains(err.Error(), "verify asks for 3 different workers to run each task, and 2 connected now could take this job") {
		t.Errorf("a job to be verified by more workers than there are: %v", err)
	}
	if _, err := cli(t, "job", "submit", "--addr", addr, "--verify", "2", "--key-file", newKeyFile(t), "--params", `{"from":0,"to":100}`); err == nil ||
		!strings.Contains(err.Error(), "a private job cannot be verified") {
		t.Errorf("a private job to be verified: %v", err)
	}
}

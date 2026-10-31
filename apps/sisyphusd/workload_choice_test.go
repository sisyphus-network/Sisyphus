package main

import (
	"strings"
	"testing"
)

// workloadsOf returns the WORKLOADS column of a node's row in a `nodes`
// table, for a node that relays nothing: the last but one.
func workloadsOf(t *testing.T, table, name string) string {
	t.Helper()
	for _, line := range strings.Split(table, "\n") {
		if fields := strings.Fields(line); len(fields) > 2 && fields[0] == name {
			return fields[len(fields)-2]
		}
	}
	t.Fatalf("no row for %s in:\n%s", name, table)
	return ""
}

// A worker whose owner refuses a workload never says it runs it, so the
// pool never gives it one: a job of it waits for a worker that takes it.
func TestAWorkerIsNeverGivenAWorkloadItsOwnerRefuses(t *testing.T) {
	addr := freeAddr(t)
	startDaemon(t, "--listen", addr, "--name", "rig", "--deny-workloads", "primes,forecasts")
	nodes := waitForOutput(t, "rig", "nodes", "--addr", addr)
	if got := workloadsOf(t, nodes, "rig"); got != "forecast,wordcount" {
		t.Errorf("rig offers %s, want forecast,wordcount:\n%s", got, nodes)
	}

	id := strings.TrimSpace(mustCLI(t, "job", "submit", "--addr", addr, "--detach", "--workload", "primes", "--tasks", "2", "--params", `{"from":1,"to":1000}`))
	if out := mustCLI(t, "job", "get", "--addr", addr, id); !strings.Contains(out, "pending") {
		t.Errorf("a primes job with nobody who takes it:\n%s", out)
	}

	// A worker that takes primes runs it, and it alone.
	startDaemon(t, "--role", "worker", "--coordinator", addr, "--name", "helper", "--allow-workloads", "primes")
	nodes = waitForOutput(t, "helper", "nodes", "--addr", addr)
	if got := workloadsOf(t, nodes, "helper"); got != "primes" {
		t.Errorf("helper offers %s, want primes:\n%s", got, nodes)
	}
	out := waitForOutput(t, "succeeded in", "job", "get", "--addr", addr, id)
	if strings.Contains(out, "on rig") {
		t.Errorf("a primes task ran on the worker whose owner refuses them:\n%s", out)
	}
}

func TestTheWorkloadsAnOwnerNamesMustExist(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"--deny-workloads", "prmes"}, `--allow-workloads, --deny-workloads: "prmes" is neither a workload nor a group`},
		{[]string{"--allow-workloads", "default,mining"}, `"mining" is neither a workload nor a group`},
		{[]string{"--role", "coordinator", "--deny-workloads", "containers"}, "--allow-workloads and --deny-workloads say what a worker takes, and need the worker role"},
		{[]string{"--role", "coordinator", "--allow-workloads", "primes"}, "need the worker role"},
	} {
		_, err := cli(t, append([]string{"run", "--data-dir", t.TempDir(), "--listen", freeAddr(t)}, tc.args...)...)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%v: %v, want %q", tc.args, err, tc.want)
		}
	}
}

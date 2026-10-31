package main

import (
	"context"
	"strings"
	"testing"

	"github.com/sisyphus-network/Sisyphus/packages/runtime"
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
	if got := workloadsOf(t, nodes, "rig"); got != "forecast,vanity,wordcount" {
		t.Errorf("rig offers %s, want forecast,vanity,wordcount:\n%s", got, nodes)
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
		{[]string{"--allow-workloads", "default,minning"}, `"minning" is neither a workload nor a group`},
		{[]string{"--role", "coordinator", "--deny-workloads", "containers"}, "--allow-workloads and --deny-workloads say what a worker takes, and need the worker role"},
		{[]string{"--role", "coordinator", "--allow-workloads", "primes"}, "need the worker role"},
		// Nothing left to take.
		{[]string{"--deny-workloads", "default"}, "they leave this worker nothing to take of what it can run (forecast, forecasts, primes, vanity, wordcount)"},
		{[]string{"--allow-workloads", "transcode,models"}, "nothing to take"},
		{[]string{"--allow-workloads", "primes", "--deny-workloads", "primes"}, "nothing to take"},
	} {
		_, err := cli(t, append([]string{"run", "--data-dir", t.TempDir(), "--listen", freeAddr(t)}, tc.args...)...)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%v: %v, want %q", tc.args, err, tc.want)
		}
	}
}

// Allowing what the node was not started able to run takes nothing, and the
// node says so in its log.
func TestANodeWarnsOfWhatItIsAllowedButCannotRun(t *testing.T) {
	logs := captureLogs(t)
	addr := freeAddr(t)
	startDaemon(t, "--listen", addr, "--name", "rig", "--allow-workloads", "primes,transcode,models")
	nodes := waitForOutput(t, "rig", "nodes", "--addr", addr)
	if got := workloadsOf(t, nodes, "rig"); got != "primes" {
		t.Errorf("rig offers %s, want primes:\n%s", got, nodes)
	}
	if !strings.Contains(logs.String(), "names=transcode,models") || !strings.Contains(logs.String(), "not started able to run") {
		t.Errorf("no warning of what cannot run in:\n%s", logs.String())
	}
}

// Mining is opt-in: a worker that runs containers does not take it unless
// its owner names it, or a group of its own, on this machine. Allowing
// every container workload is not naming it. There is no flag of the
// coordinator's, and no job, that turns it on.
func TestOnlyAWorkerWhoseOwnerNamesMiningMines(t *testing.T) {
	old := checkContainers
	checkContainers = func(context.Context, runtime.Confinement) error { return nil }
	defer func() { checkContainers = old }()
	addr := freeAddr(t)
	startDaemon(t, "--listen", addr, "--name", "rig", "--containers", "--allow-workloads", "default,containers")
	nodes := waitForOutput(t, "rig", "nodes", "--addr", addr)
	if got := workloadsOf(t, nodes, "rig"); strings.Contains(got, "mining") || !strings.Contains(got, "images") {
		t.Errorf("rig, which runs containers and did not name mining, offers %s:\n%s", got, nodes)
	}
	startDaemon(t, "--role", "worker", "--coordinator", addr, "--name", "miner", "--containers", "--allow-workloads", "mining-cpu")
	nodes = waitForOutput(t, "miner", "nodes", "--addr", addr)
	if got := workloadsOf(t, nodes, "miner"); got != "mining" {
		t.Errorf("miner, whose owner named mining-cpu, offers %s:\n%s", got, nodes)
	}
}

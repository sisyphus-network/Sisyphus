package main

import (
	"bytes"
	"context"
	"net"
	"os"
	"strings"
	"testing"
	"time"
)

// These tests drive the same entry point as the command line, against
// daemons started the same way.

// cli runs one sisyphusd command and returns what it printed.
func cli(t *testing.T, args ...string) (string, error) {
	t.Helper()
	var out bytes.Buffer
	stdout = &out
	defer func() { stdout = os.Stdout }()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	err := run(ctx, args)
	return out.String(), err
}

func freeAddr(t *testing.T) string {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer lis.Close()
	return lis.Addr().String()
}

// startDaemon runs "sisyphusd run" with the given flags until the test ends.
func startDaemon(t *testing.T, args ...string) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- run(ctx, append([]string{"run"}, args...)) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("daemon %v exited with: %v", args, err)
		}
	})
}

// waitForOutput reruns a command until its output contains want.
func waitForOutput(t *testing.T, want string, args ...string) string {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		out, err := cli(t, args...)
		if err == nil && strings.Contains(out, want) {
			return out
		}
		if time.Now().After(deadline) {
			t.Fatalf("%v never printed %q; last output %q, error %v", args, want, out, err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestCLISubmitsJobsToACombinedNode(t *testing.T) {
	addr := freeAddr(t)
	startDaemon(t, "--listen", addr, "--node-id", "solo", "--slots", "3")
	nodes := waitForOutput(t, "solo", "nodes", "--addr", addr)
	if !strings.Contains(nodes, "0/3") || !strings.Contains(nodes, "primes") {
		t.Errorf("nodes output lacks slots or workloads:\n%s", nodes)
	}

	out, err := cli(t, "job", "submit", "--addr", addr, "--tasks", "4", "--params", `{"from":0,"to":1000000}`)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"4 task(s)", "task 3 succeeded on solo (attempt 1)", "succeeded in", `{"count":78498}`} {
		if !strings.Contains(out, want) {
			t.Errorf("submit output lacks %q:\n%s", want, out)
		}
	}

	out, err = cli(t, "job", "submit", "--addr", addr, "--mode", "full-worker", "--params", `{"from":0,"to":1000000}`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "1 task(s)") || !strings.Contains(out, `{"count":78498}`) {
		t.Errorf("full-worker output:\n%s", out)
	}
}

func TestCLIDetachedJobCanBeFetchedLater(t *testing.T) {
	addr := freeAddr(t)
	startDaemon(t, "--listen", addr, "--slots", "2")
	waitForOutput(t, "primes", "nodes", "--addr", addr)

	out, err := cli(t, "job", "submit", "--addr", addr, "--detach", "--params", `{"from":0,"to":100}`)
	if err != nil {
		t.Fatal(err)
	}
	jobID := strings.TrimSpace(out)
	if len(jobID) != 16 {
		t.Fatalf("detached submit printed %q, want only a job ID", out)
	}

	got := waitForOutput(t, "succeeded in", "job", "get", "--addr", addr, jobID)
	if !strings.Contains(got, "job "+jobID+": succeeded, workload primes") || !strings.Contains(got, `{"count":25}`) {
		t.Errorf("job get output:\n%s", got)
	}
}

func TestCLIWorkerOnlyNodeJoinsACoordinatorOnlyNode(t *testing.T) {
	addr := freeAddr(t)
	startDaemon(t, "--role", "coordinator", "--listen", addr, "--node-id", "boss")

	out, err := cli(t, "nodes", "--addr", addr)
	for err != nil { // the listener may not be up yet
		time.Sleep(20 * time.Millisecond)
		out, err = cli(t, "nodes", "--addr", addr)
	}
	if !strings.Contains(out, "no workers connected") {
		t.Errorf("coordinator-only node lists workers:\n%s", out)
	}

	startDaemon(t, "--role", "worker", "--coordinator", addr, "--node-id", "hand", "--slots", "1")
	nodes := waitForOutput(t, "hand", "nodes", "--addr", addr)
	if strings.Contains(nodes, "boss") {
		t.Errorf("coordinator-only node appears as a worker:\n%s", nodes)
	}
}

func TestCLIReportsAFailedSubmission(t *testing.T) {
	addr := freeAddr(t)
	startDaemon(t, "--listen", addr, "--slots", "1")
	waitForOutput(t, "primes", "nodes", "--addr", addr)

	if _, err := cli(t, "job", "submit", "--addr", addr, "--workload", "nope"); err == nil || !strings.Contains(err.Error(), `unknown workload "nope"`) {
		t.Errorf("unknown workload: error %v", err)
	}
	if _, err := cli(t, "job", "get", "--addr", addr, "missing"); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Errorf("unknown job: error %v", err)
	}
}

func TestCLIRejectsBadUsage(t *testing.T) {
	tests := []struct {
		args []string
		want string
	}{
		{[]string{"frobnicate"}, `unknown command "frobnicate"`},
		{[]string{"job"}, `expected "job submit" or "job get"`},
		{[]string{"job", "get"}, "expected exactly one job ID"},
		{[]string{"job", "submit", "--mode", "sideways"}, `unknown mode "sideways"`},
		{[]string{"job", "submit", "extra"}, `unexpected argument "extra"`},
		{[]string{"nodes", "extra"}, `unexpected argument "extra"`},
		{[]string{"run", "--role", "overlord"}, `unknown role "overlord"`},
		{[]string{"run", "--role", "worker"}, "needs --coordinator"},
		{[]string{"run", "--coordinator", "example:1"}, "worker-only nodes"},
		{[]string{"run", "--slots", "0"}, "at least 1"},
		{[]string{"run", "extra"}, `unexpected argument "extra"`},
	}
	for _, tt := range tests {
		_, err := cli(t, tt.args...)
		if err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("%v: error %v, want one containing %q", tt.args, err, tt.want)
		}
	}
}

func TestLoopbackRewritesOnlyWildcardAddresses(t *testing.T) {
	tests := []struct{ ip, want string }{
		{"0.0.0.0", "127.0.0.1:7700"},
		{"::", "127.0.0.1:7700"},
		{"192.168.1.5", "192.168.1.5:7700"},
	}
	for _, tt := range tests {
		if got := loopback(&net.TCPAddr{IP: net.ParseIP(tt.ip), Port: 7700}); got != tt.want {
			t.Errorf("loopback(%s) = %s, want %s", tt.ip, got, tt.want)
		}
	}
}

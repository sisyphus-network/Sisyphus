package main

import (
	"bytes"
	"context"
	"math/rand/v2"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// These tests drive the same entry point as the command line, against
// daemons started the same way.

// TestMain points the home directory at a scratch directory, so that a
// command run without --data-dir never touches the real ~/.sisyphus.
func TestMain(m *testing.M) {
	home, err := os.MkdirTemp("", "sisyphus-test-home")
	if err != nil {
		panic(err)
	}
	os.Setenv("HOME", home)
	// Nothing a test does may land in the real user's data directory.
	os.Unsetenv("XDG_DATA_HOME")
	code := m.Run()
	os.RemoveAll(home)
	os.Exit(code)
}

// dataDirs records, for each address a test node listens on, the data
// directory holding that node's key. Commands sent to the address are run
// from that directory, which makes them the node's owner.
var dataDirs sync.Map

// cli runs one sisyphusd command and returns what it printed. A command
// addressed to a test node is run as that node's owner unless it names a
// data directory of its own.
func cli(t *testing.T, args ...string) (string, error) {
	t.Helper()
	if !slices.Contains(args, "--data-dir") {
		if i := slices.Index(args, "--addr"); i >= 0 && i+1 < len(args) {
			if dir, ok := dataDirs.Load(args[i+1]); ok {
				args = slices.Insert(slices.Clone(args), i, "--data-dir", dir.(string))
			}
		}
	}
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
	// Not a port of the system's choosing: between this function returning
	// and the caller listening, the system may give that same port to
	// something else, and nodes and their Kubo daemons take such ports all
	// the time. Ports below the range it chooses from are left alone.
	for {
		addr := "127.0.0.1:" + strconv.Itoa(20000+rand.IntN(12000))
		lis, err := net.Listen("tcp", addr)
		if err != nil {
			continue
		}
		lis.Close()
		return addr
	}
}

// startDaemon runs "sisyphusd run" with the given flags until the test ends
// or the returned stop function is called, whichever comes first. It gives
// the node a data directory of its own unless the flags name one, and a
// worker-only node an invitation from its coordinator unless they hold one.
func startDaemon(t *testing.T, args ...string) (stop func()) {
	t.Helper()
	dataDir := t.TempDir()
	if i := slices.Index(args, "--data-dir"); i >= 0 {
		dataDir = args[i+1]
	} else {
		args = append([]string{"--data-dir", dataDir}, args...)
	}
	if i := slices.Index(args, "--listen"); i >= 0 {
		dataDirs.Store(args[i+1], dataDir)
	}
	// Test nodes keep to themselves unless a test is about their finding
	// each other: there may be many on this machine at once.
	if !slices.Contains(args, "--discovery") {
		args = append(args, "--discovery", "off")
	}
	if i := slices.Index(args, "--coordinator"); i >= 0 && !slices.Contains(args, "--join") && !joined(dataDir) {
		args = append(args, "--join", invite(t, args[i+1], "worker"))
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	args = append([]string{"run"}, args...)
	go func() { done <- run(ctx, args) }()
	stop = sync.OnceFunc(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("daemon %v exited with: %v", args, err)
		}
	})
	t.Cleanup(stop)
	return stop
}

// joined reports whether the node in dataDir has joined anything before.
func joined(dataDir string) bool {
	_, err := os.Stat(filepath.Join(dataDir, "known.json"))
	return err == nil
}

// invite gets an invitation from the test node at addr, waiting for the
// node to come up if it has only just been started.
func invite(t *testing.T, addr, role string) string {
	t.Helper()
	return strings.TrimSpace(waitForOutput(t, ":", "pool", "invite", "--addr", addr, "--role", role))
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
	startDaemon(t, "--listen", addr, "--name", "solo", "--slots", "3")
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
	startDaemon(t, "--role", "coordinator", "--listen", addr, "--name", "boss")

	out, err := cli(t, "nodes", "--addr", addr)
	for err != nil { // the listener may not be up yet
		time.Sleep(20 * time.Millisecond)
		out, err = cli(t, "nodes", "--addr", addr)
	}
	if !strings.Contains(out, "no workers connected") {
		t.Errorf("coordinator-only node lists workers:\n%s", out)
	}

	startDaemon(t, "--role", "worker", "--coordinator", addr, "--name", "hand", "--slots", "1")
	nodes := waitForOutput(t, "hand", "nodes", "--addr", addr)
	if strings.Contains(nodes, "boss") {
		t.Errorf("coordinator-only node appears as a worker:\n%s", nodes)
	}
}

func TestCLIWorkerRejoinsARestartedCoordinator(t *testing.T) {
	addr := freeAddr(t)
	// The coordinator keeps its data directory across the restart, and with
	// it its key, which the worker checks, and its list of admitted nodes.
	dataDir := t.TempDir()
	stopCoordinator := startDaemon(t, "--role", "coordinator", "--listen", addr, "--data-dir", dataDir)
	startDaemon(t, "--role", "worker", "--coordinator", addr, "--name", "loyal", "--slots", "1")
	waitForOutput(t, "loyal", "nodes", "--addr", addr)

	stopCoordinator()
	startDaemon(t, "--role", "coordinator", "--listen", addr, "--data-dir", dataDir)

	waitForOutput(t, "loyal", "nodes", "--addr", addr)
	out, err := cli(t, "job", "submit", "--addr", addr, "--params", `{"from":0,"to":100}`)
	if err != nil || !strings.Contains(out, `{"count":25}`) {
		t.Errorf("job after the worker rejoined: error %v, output:\n%s", err, out)
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

package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/timestamppb"

	pb "github.com/sisyphus-network/Sisyphus/packages/protocol/sisyphus/v1"
)

// badAddr is an address no gRPC client can be made for.
const badAddr = "bad\x00address"

// captureStderr collects what commands print to standard error.
func captureStderr(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	stderr = &buf
	t.Cleanup(func() { stderr = os.Stderr })
	return &buf
}

func TestEveryCommandRejectsAnUnknownFlag(t *testing.T) {
	for _, command := range [][]string{
		{"run"}, {"job", "submit"}, {"job", "get"}, {"nodes"}, {"blob", "put"}, {"blob", "get"}, {"blob", "stat"},
	} {
		_, err := cli(t, append(command, "--no-such-flag")...)
		if err == nil || !strings.Contains(err.Error(), "flag provided but not defined") {
			t.Errorf("%v --no-such-flag: error %v", command, err)
		}
	}
}

func TestEveryClientCommandReportsAnUnusableAddress(t *testing.T) {
	file := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(file, []byte("data"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, command := range [][]string{
		{"job", "submit", "--addr", badAddr},
		{"job", "get", "--addr", badAddr, "some-job"},
		{"nodes", "--addr", badAddr},
		{"blob", "put", "--addr", badAddr, file},
		{"blob", "get", "--addr", badAddr, helloCID},
		{"blob", "stat", "--addr", badAddr, helloCID},
	} {
		_, err := cli(t, command...)
		if err == nil || !strings.Contains(err.Error(), "invalid control character") {
			t.Errorf("%v: error %v", command[:2], err)
		}
	}
}

func TestUsage(t *testing.T) {
	errOut := captureStderr(t)

	out, err := cli(t, "help")
	if err != nil || !strings.Contains(out, "sisyphusd run [flags]") {
		t.Errorf("help printed %q, error %v", out, err)
	}

	out, err = cli(t)
	if !errors.Is(err, flag.ErrHelp) || out != "" || !strings.Contains(errOut.String(), "sisyphusd run [flags]") {
		t.Errorf("no arguments: stdout %q, stderr %q, error %v", out, errOut.String(), err)
	}
}

func TestVersion(t *testing.T) {
	if out, err := cli(t, "version"); err != nil || out != "sisyphusd dev\n" {
		t.Errorf("printed %q, error %v", out, err)
	}
}

func TestExitCode(t *testing.T) {
	errOut := captureStderr(t)
	if got := exitCode(nil); got != 0 || errOut.Len() != 0 {
		t.Errorf("success: code %d, printed %q", got, errOut.String())
	}
	if got := exitCode(errors.New("it broke")); got != 1 || errOut.String() != "sisyphusd: it broke\n" {
		t.Errorf("failure: code %d, printed %q", got, errOut.String())
	}
	// The flag package has already explained a usage error.
	errOut.Reset()
	if got := exitCode(flag.ErrHelp); got != 1 || errOut.Len() != 0 {
		t.Errorf("usage error: code %d, printed %q", got, errOut.String())
	}
}

func TestMainRunsTheCommandLineAndExitsWithItsStatus(t *testing.T) {
	captureStderr(t)
	var out bytes.Buffer
	stdout = &out
	oldArgs, oldExit := os.Args, exit
	defer func() { stdout, os.Args, exit = os.Stdout, oldArgs, oldExit }()

	code := -1
	exit = func(c int) { code = c }

	os.Args = []string{"sisyphusd", "help"}
	main()
	if code != 0 || !strings.Contains(out.String(), "Usage:") {
		t.Errorf("sisyphusd help: exit %d, output %q", code, out.String())
	}

	os.Args = []string{"sisyphusd", "frobnicate"}
	main()
	if code != 1 {
		t.Errorf("sisyphusd frobnicate: exit %d, want 1", code)
	}
}

func TestBlobGetFailsWhenTheOutputDirectoryIsMissing(t *testing.T) {
	addr, _ := startNode(t)
	target := filepath.Join(t.TempDir(), "no-such-dir", "out")
	if _, err := cli(t, "blob", "get", "--addr", addr, "-o", target, helloCID); err == nil || !strings.Contains(err.Error(), "no such file or directory") {
		t.Errorf("error %v, want the missing directory reported", err)
	}
}

func TestJobGetShowsAJobThatHasNotFinished(t *testing.T) {
	addr := freeAddr(t)
	startDaemon(t, "--role", "coordinator", "--listen", addr)
	// With no workers the job stays pending.
	id := strings.TrimSpace(waitForOutput(t, "", "job", "submit", "--addr", addr, "--detach", "--params", `{"from":0,"to":10}`))

	out, err := cli(t, "job", "get", "--addr", addr, id)
	if err != nil {
		t.Fatal(err)
	}
	if want := "job " + id + ": pending, workload primes\n  task 0 pending\n"; out != want {
		t.Errorf("printed %q, want %q", out, want)
	}
}

// scriptedJobs is a node client that replays a fixed sequence of job updates.
type scriptedJobs struct {
	pb.NodeServiceClient
	watchErr  error
	updates   []*pb.Job
	streamErr error
}

func (s *scriptedJobs) WatchJob(context.Context, *pb.WatchJobRequest, ...grpc.CallOption) (grpc.ServerStreamingClient[pb.WatchJobResponse], error) {
	if s.watchErr != nil {
		return nil, s.watchErr
	}
	return &scriptedWatch{updates: s.updates, err: s.streamErr}, nil
}

type scriptedWatch struct {
	grpc.ClientStream
	updates []*pb.Job
	err     error
}

func (w *scriptedWatch) Recv() (*pb.WatchJobResponse, error) {
	if len(w.updates) > 0 {
		job := w.updates[0]
		w.updates = w.updates[1:]
		return &pb.WatchJobResponse{Job: job}, nil
	}
	if w.err != nil {
		return nil, w.err
	}
	return nil, io.EOF
}

func follow(t *testing.T, client pb.NodeServiceClient) (string, error) {
	t.Helper()
	var out bytes.Buffer
	stdout = &out
	defer func() { stdout = os.Stdout }()
	err := followJob(context.Background(), client, &pb.Job{JobId: "j"})
	return out.String(), err
}

func TestFollowingAJobThatFails(t *testing.T) {
	started := time.Unix(1_700_000_000, 0)
	task := func(state pb.TaskState, attempt uint32, reason string) []*pb.Task {
		return []*pb.Task{{TaskId: "j/0", State: state, Attempt: attempt, NodeId: "12D3KooWexample", NodeName: "a", Error: reason}}
	}
	client := &scriptedJobs{updates: []*pb.Job{
		{JobId: "j", State: pb.JobState_JOB_STATE_RUNNING, Tasks: task(pb.TaskState_TASK_STATE_RUNNING, 1, "")},
		{JobId: "j", State: pb.JobState_JOB_STATE_RUNNING, Tasks: task(pb.TaskState_TASK_STATE_RUNNING, 2, "disk on fire")},
		// An update in which nothing about the task changed prints nothing.
		{JobId: "j", State: pb.JobState_JOB_STATE_RUNNING, Tasks: task(pb.TaskState_TASK_STATE_RUNNING, 2, "disk on fire")},
		{
			JobId: "j", State: pb.JobState_JOB_STATE_FAILED, Error: "task 0 failed after 2 attempts: disk on fire",
			Tasks:     task(pb.TaskState_TASK_STATE_FAILED, 2, "disk on fire"),
			CreatedAt: timestamppb.New(started), FinishedAt: timestamppb.New(started.Add(1500 * time.Millisecond)),
		},
	}}

	out, err := follow(t, client)
	wantOut := "  task 0 running on a (attempt 1)\n" +
		"  task 0 running on a (attempt 2): disk on fire\n" +
		"  task 0 failed on a (attempt 2): disk on fire\n"
	if out != wantOut {
		t.Errorf("printed:\n%s\nwant:\n%s", out, wantOut)
	}
	if want := "job j failed after 1.5s: task 0 failed after 2 attempts: disk on fire"; err == nil || err.Error() != want {
		t.Errorf("error %v, want %q", err, want)
	}
}

func TestFollowingAJobWhenTheNodeGoesAway(t *testing.T) {
	gone := errors.New("node went away")
	if _, err := follow(t, &scriptedJobs{watchErr: gone}); !errors.Is(err, gone) {
		t.Errorf("watch that cannot start: error %v", err)
	}
	running := &pb.Job{JobId: "j", State: pb.JobState_JOB_STATE_RUNNING}
	if _, err := follow(t, &scriptedJobs{updates: []*pb.Job{running}, streamErr: gone}); !errors.Is(err, gone) {
		t.Errorf("watch that breaks part-way: error %v", err)
	}
}

func TestDaemonStartupFailures(t *testing.T) {
	taken, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer taken.Close()

	// Data directories left in particular states by earlier runs.
	withFile := func(name, content string) string {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		return dir
	}
	joinedBadAddr := withFile("known.json", `{"bad\u0000address":"12D3KooWexample"}`)
	brokenKnown := withFile("known.json", "not json")
	brokenAccess := withFile("access.json", "not json")

	tests := []struct {
		name string
		args []string
		want string
	}{
		{"the listen address is in use", []string{"--listen", taken.Addr().String()}, "address already in use"},
		{"a worker has not joined its coordinator", []string{"--role", "worker", "--coordinator", "127.0.0.1:1"}, "has not joined a coordinator at 127.0.0.1:1"},
		{"the address to join is unusable", []string{"--role", "worker", "--coordinator", badAddr, "--join", "12D3KooWexample:token"}, "invalid control character"},
		{"an invitation is malformed", []string{"--role", "worker", "--coordinator", "127.0.0.1:1", "--join", "no-colon-here"}, "an invitation looks like"},
		{"a coordinator is given an invitation", []string{"--join", "12D3KooWexample:token"}, "--join is for worker-only nodes"},
		{"the joined coordinator's address is unusable", []string{"--role", "worker", "--coordinator", badAddr, "--data-dir", joinedBadAddr}, "invalid control character"},
		{"the record of joined nodes is unreadable", []string{"--role", "worker", "--coordinator", "127.0.0.1:1", "--data-dir", brokenKnown}, "known nodes"},
		{"the list of admitted nodes is unreadable", []string{"--listen", freeAddr(t), "--data-dir", brokenAccess}, "access list"},
	}
	for _, tt := range tests {
		args := append([]string{"run", "--data-dir", t.TempDir()}, tt.args...)
		if _, err := cli(t, args...); err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("%s: error %v, want one containing %q", tt.name, err, tt.want)
		}
	}
}

func TestDaemonStopsWhenItsCoordinatorRemovesIt(t *testing.T) {
	addr := freeAddr(t)
	startDaemon(t, "--role", "coordinator", "--listen", addr)
	workerDir := t.TempDir()
	invitation := invite(t, addr, "worker")

	// Run the worker here rather than through startDaemon, to see why it stops.
	stopped := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		stopped <- run(ctx, []string{"run", "--data-dir", workerDir, "--role", "worker", "--coordinator", addr, "--join", invitation, "--name", "outcast"})
	}()
	waitForOutput(t, "outcast", "nodes", "--addr", addr)

	workerID := strings.TrimSpace(mustCLI(t, "id", "--data-dir", workerDir))
	mustCLI(t, "pool", "remove", "--addr", addr, workerID)

	err := <-stopped
	if err == nil || !strings.Contains(err.Error(), `coordinator rejected node "outcast"`) || !strings.Contains(err.Error(), "removed from the pool") {
		t.Errorf("worker stopped with %v, want it to say it was removed", err)
	}
	if out := mustCLI(t, "nodes", "--addr", addr); out != "no workers connected\n" {
		t.Errorf("the removed worker is still listed:\n%s", out)
	}
}

func TestVerboseDaemonLogsEachTask(t *testing.T) {
	logs := captureLogs(t)
	addr := freeAddr(t)
	stop := startDaemon(t, "-v", "--listen", addr, "--slots", "1")
	waitForOutput(t, "primes", "nodes", "--addr", addr)
	if _, err := cli(t, "job", "submit", "--addr", addr, "--params", `{"from":0,"to":100}`); err != nil {
		t.Fatal(err)
	}
	stop()

	if !strings.Contains(logs.String(), `level=DEBUG msg="task done"`) {
		t.Errorf("no per-task debug line in the log:\n%s", logs)
	}
}

func TestDefaultName(t *testing.T) {
	realHostname := hostname
	defer func() { hostname = realHostname }()

	hostname = func() (string, error) { return "rig", nil }
	if got := defaultName(); got != "rig" {
		t.Errorf("default name %q, want the host name", got)
	}
	hostname = func() (string, error) { return "", errors.New("no hostname") }
	if got := defaultName(); got != "node" {
		t.Errorf("default name without a host name is %q", got)
	}
}

func TestDefaultDataDir(t *testing.T) {
	t.Setenv("HOME", "/home/camus")
	if got := defaultDataDir(); got != "/home/camus/.sisyphus" {
		t.Errorf("data directory %q", got)
	}
	t.Setenv("HOME", "")
	if got := defaultDataDir(); got != ".sisyphus" {
		t.Errorf("data directory with no home is %q", got)
	}
}

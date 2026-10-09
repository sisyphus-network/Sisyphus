package main

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pb "github.com/sisyphus-network/Sisyphus/packages/protocol/sisyphus/v1"
	"github.com/sisyphus-network/Sisyphus/packages/runtime"
)

// nodeID returns the ID of the node whose key is in dataDir.
func nodeID(t *testing.T, dataDir string) string {
	t.Helper()
	return strings.TrimSpace(mustCLI(t, "id", "--data-dir", dataDir))
}

// as runs a command against addr from another machine's point of view: with
// the key and the joined nodes found in dataDir.
func as(t *testing.T, dataDir string, command ...string) (string, error) {
	t.Helper()
	// Flags must come before a command's arguments, so put this one where
	// the address is given.
	i := slices.Index(command, "--addr")
	return cli(t, slices.Insert(slices.Clone(command), i, "--data-dir", dataDir)...)
}

func TestAClientJoinsWithAnInvitationAndCanThenUseTheNode(t *testing.T) {
	addr, _ := startNode(t, "--name", "rig")
	laptop := t.TempDir()

	// Before joining, the laptop does not even know which node to expect at
	// that address, so it will not talk to it.
	if _, err := as(t, laptop, "nodes", "--addr", addr); err == nil || !strings.Contains(err.Error(), "authentication handshake failed") {
		t.Errorf("an unjoined node's call: %v, want the connection refused", err)
	}
	if out := mustCLI(t, "pool", "members", "--addr", addr); out != "no nodes have been admitted\n" {
		t.Errorf("members of a new node: %q", out)
	}

	invitation := invite(t, addr, "client")
	out, err := as(t, laptop, "pool", "join", "--addr", addr, invitation)
	if err != nil || out != "joined "+addr+" as a client\n" {
		t.Fatalf("pool join printed %q, error %v", out, err)
	}

	// It can now do what a client may.
	if out, err := as(t, laptop, "nodes", "--addr", addr); err != nil || !strings.Contains(out, "rig") {
		t.Errorf("a client listing nodes: %q, %v", out, err)
	}
	if out, err := as(t, laptop, "job", "submit", "--addr", addr, "--params", `{"from":0,"to":100}`); err != nil || !strings.Contains(out, `{"count":25}`) {
		t.Errorf("a client submitting a job: %q, %v", out, err)
	}
	// And not what only the owner may.
	if _, err := as(t, laptop, "pool", "invite", "--addr", addr); err == nil || !strings.Contains(err.Error(), "admitted as a client, which may not call Invite") {
		t.Errorf("a client issuing an invitation: %v", err)
	}

	members := mustCLI(t, "pool", "members", "--addr", addr)
	if !strings.Contains(members, nodeID(t, laptop)) || !strings.Contains(members, "client") {
		t.Errorf("members:\n%s", members)
	}

	// Removed, it is refused again.
	mustCLI(t, "pool", "remove", "--addr", addr, nodeID(t, laptop))
	if _, err := as(t, laptop, "nodes", "--addr", addr); err == nil || !strings.Contains(err.Error(), "has not been admitted") {
		t.Errorf("a removed client's call: %v, want it refused", err)
	}
	if _, err := cli(t, "pool", "remove", "--addr", addr, nodeID(t, laptop)); err == nil || !strings.Contains(err.Error(), "is not a member") {
		t.Errorf("removing a node twice: %v", err)
	}
}

func TestAnInvitationWorksOnceAndOnlyForTheNodeThatIssuedIt(t *testing.T) {
	addr, _ := startNode(t)
	invitation := invite(t, addr, "client")

	if _, err := as(t, t.TempDir(), "pool", "join", "--addr", addr, invitation); err != nil {
		t.Fatal(err)
	}
	second := t.TempDir()
	if _, err := as(t, second, "pool", "join", "--addr", addr, invitation); err == nil || !strings.Contains(err.Error(), "already used or expired") {
		t.Errorf("a second node using the same invitation: %v", err)
	}
	// The failed join left no record of the node as joined.
	if _, err := os.Stat(filepath.Join(second, "known.json")); !os.IsNotExist(err) {
		t.Errorf("a node that failed to join remembers the node anyway (stat: %v)", err)
	}

	// An invitation naming another node as its issuer is not honoured here:
	// the joiner refuses to talk to anyone but the node named in it.
	other, _ := startNode(t)
	fromOther := invite(t, other, "client")
	if _, err := as(t, t.TempDir(), "pool", "join", "--addr", addr, fromOther); err == nil || !strings.Contains(err.Error(), "authentication handshake failed") {
		t.Errorf("joining one node with another's invitation: %v, want the connection refused", err)
	}
}

func TestAWorkerMayNotActAsAClient(t *testing.T) {
	addr := freeAddr(t)
	startDaemon(t, "--role", "coordinator", "--listen", addr)
	workerDir := t.TempDir()
	startDaemon(t, "--role", "worker", "--coordinator", addr, "--data-dir", workerDir, "--name", "hand")
	waitForOutput(t, "hand", "nodes", "--addr", addr)

	if _, err := as(t, workerDir, "nodes", "--addr", addr); err == nil || !strings.Contains(err.Error(), "admitted as a worker, which may not call ListNodes") {
		t.Errorf("a worker's key listing nodes: %v", err)
	}
	members := mustCLI(t, "pool", "members", "--addr", addr)
	if !strings.Contains(members, nodeID(t, workerDir)) || !strings.Contains(members, "worker") {
		t.Errorf("members:\n%s", members)
	}
}

func TestANodeThatKnowsTheCoordinatorButWasNeverAdmittedIsRefused(t *testing.T) {
	addr := freeAddr(t)
	coordinatorDir := t.TempDir()
	startDaemon(t, "--role", "coordinator", "--listen", addr, "--data-dir", coordinatorDir)
	waitForOutput(t, "no workers", "nodes", "--addr", addr)

	// It has the right ID on record, as if it had joined some other time,
	// but is on nobody's list.
	intruder := t.TempDir()
	known := `{"` + addr + `":"` + nodeID(t, coordinatorDir) + `"}`
	if err := os.WriteFile(filepath.Join(intruder, "known.json"), []byte(known), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := cli(t, "run", "--data-dir", intruder, "--role", "worker", "--coordinator", addr)
	if err == nil || !strings.Contains(err.Error(), "has not been admitted") {
		t.Errorf("an unadmitted worker stopped with %v, want it refused", err)
	}
}

func TestJoiningFailsIfTheNodeCannotRememberWhatItJoined(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root can write to a read-only directory")
	}
	addr, _ := startNode(t)

	// The record of joined nodes cannot be read.
	broken := t.TempDir()
	nodeID(t, broken)
	if err := os.WriteFile(filepath.Join(broken, "known.json"), []byte("not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := as(t, broken, "pool", "join", "--addr", addr, invite(t, addr, "client")); err == nil || !strings.Contains(err.Error(), "known nodes") {
		t.Errorf("joining with an unreadable record: %v", err)
	}
	if _, err := as(t, broken, "nodes", "--addr", addr); err == nil || !strings.Contains(err.Error(), "known nodes") {
		t.Errorf("a command with an unreadable record: %v", err)
	}

	// It cannot be written.
	readOnly := t.TempDir()
	nodeID(t, readOnly)
	if err := os.Chmod(readOnly, 0o500); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(readOnly, 0o700)
	if _, err := as(t, readOnly, "pool", "join", "--addr", addr, invite(t, addr, "client")); err == nil || !strings.Contains(err.Error(), "remember the node joined") {
		t.Errorf("joining with an unwritable record: %v", err)
	}
}

func TestPoolCommandUsage(t *testing.T) {
	badKey := t.TempDir()
	if err := os.WriteFile(filepath.Join(badKey, "node.key"), []byte("not a key"), 0o600); err != nil {
		t.Fatal(err)
	}
	knownIsDir := t.TempDir()
	if err := os.Mkdir(filepath.Join(knownIsDir, "known.json"), 0o700); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		args []string
		want string
	}{
		{[]string{"pool"}, "expected pool invite, join, members, remove, work-for, rekey or cluster"},
		{[]string{"pool", "invite", "extra"}, `unexpected argument "extra"`},
		{[]string{"pool", "invite", "--role", "admin"}, `unknown role "admin"`},
		{[]string{"pool", "invite", "--ttl", "10ms"}, "--ttl must be at least a second"},
		{[]string{"pool", "join"}, "expected exactly one invitation"},
		{[]string{"pool", "join", "no-colon-here"}, "an invitation looks like"},
		{[]string{"pool", "join", ":token"}, "an invitation looks like"},
		{[]string{"pool", "join", "12D3KooWexample:"}, "an invitation looks like"},
		{[]string{"pool", "join", "--data-dir", badKey, "12D3KooWexample:token"}, "is not PEM"},
		{[]string{"pool", "members", "extra"}, `unexpected argument "extra"`},
		{[]string{"pool", "remove"}, "expected exactly one node ID"},
		{[]string{"pool", "invite", "--no-such-flag"}, "flag provided but not defined"},
		{[]string{"pool", "join", "--no-such-flag"}, "flag provided but not defined"},
		{[]string{"pool", "members", "--no-such-flag"}, "flag provided but not defined"},
		{[]string{"pool", "remove", "--no-such-flag"}, "flag provided but not defined"},
		{[]string{"pool", "invite", "--addr", badAddr}, "invalid control character"},
		{[]string{"pool", "members", "--addr", badAddr}, "invalid control character"},
		{[]string{"pool", "remove", "--addr", badAddr, "12D3KooWexample"}, "invalid control character"},
		{[]string{"nodes", "--data-dir", badKey}, "is not PEM"},
		{[]string{"nodes", "--data-dir", knownIsDir}, "read known nodes"},
	}
	for _, tt := range tests {
		_, err := cli(t, tt.args...)
		if err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("%v: error %v, want one containing %q", tt.args, err, tt.want)
		}
	}
}

func TestPoolCommandsReportANodeThatCannotManageItsPool(t *testing.T) {
	// A stand-in node offering no pool service at all.
	addr := serveFake(t, func(*grpc.Server) {})
	for _, command := range [][]string{{"pool", "invite"}, {"pool", "members"}} {
		if _, err := cli(t, append(command, "--addr", addr)...); err == nil || !strings.Contains(err.Error(), "Unimplemented") {
			t.Errorf("%v: error %v, want the node's refusal", command, err)
		}
	}
}

// noStream is a worker stream on a server that did not identify its caller.
type noStream struct {
	grpc.ServerStream
}

func (noStream) Context() context.Context { return context.Background() }

func (noStream) Recv() (*pb.WorkerMessage, error) { return nil, nil }

func (noStream) Send(*pb.CoordinatorMessage) error { return nil }

func TestCoordinatorRefusesAWorkerNobodyIdentified(t *testing.T) {
	p := startPool(t, runtime.Builtin())
	if err := p.coord.Connect(noStream{}); status.Code(err) != codes.Unauthenticated {
		t.Errorf("error %v, want Unauthenticated", err)
	}
}

func TestRemovedWorkerIsDisconnectedAndItsTasksMove(t *testing.T) {
	g := gated{started: make(chan struct{}, 16), release: make(chan struct{})}
	p := startPool(t, runtime.NewRegistry(g))
	p.startWorker("doomed", 1)
	p.waitForWorkers(1)
	job := p.submit(&pb.JobSpec{Workload: "gated", MaxTasks: 1})
	<-g.started

	doomed := p.workerIdents["doomed"].ID()
	p.coord.Remove("12D3KooWnobody") // not connected: nothing happens
	p.coord.Remove(doomed)
	p.coord.Remove(doomed) // already going: nothing more happens
	p.waitForWorkers(0)

	p.startWorker("heir", 1)
	<-g.started
	close(g.release)
	done := p.wait(job.GetJobId())
	if task := done.GetTasks()[0]; done.GetState() != pb.JobState_JOB_STATE_SUCCEEDED || task.GetNodeName() != "heir" || task.GetAttempt() != 2 {
		t.Errorf("job %v; its task finished on %q at attempt %d, want heir at attempt 2", done.GetState(), task.GetNodeName(), task.GetAttempt())
	}
}

// A worker is usually restarted with the command it was first started with,
// invitation and all, by a person or by a service manager.
func TestAWorkerRestartsWithTheCommandThatFirstStartedIt(t *testing.T) {
	addr := freeAddr(t)
	startDaemon(t, "--role", "coordinator", "--listen", addr)
	workerDir := t.TempDir()
	command := []string{"--role", "worker", "--coordinator", addr, "--join", invite(t, addr, "worker"), "--data-dir", workerDir, "--name", "steady"}

	stop := startDaemon(t, command...)
	waitForOutput(t, "steady", "nodes", "--addr", addr)
	stop()
	waitForOutput(t, "no workers connected", "nodes", "--addr", addr)

	startDaemon(t, command...)
	waitForOutput(t, "steady", "nodes", "--addr", addr)
}

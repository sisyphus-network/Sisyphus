package main

import (
	"testing"

	"github.com/sisyphus-network/Sisyphus/apps/sisyphusd/access"
	pb "github.com/sisyphus-network/Sisyphus/packages/protocol/sisyphus/v1"
	"github.com/sisyphus-network/Sisyphus/packages/runtime"
	"google.golang.org/grpc"
)

// Free slots go to whichever client has the fewest tasks running, so that
// one client's job of many tasks, submitted first, does not take every slot
// while another's waits. With only one client waiting, it has them all.
func TestFreeSlotsAreSharedBetweenClients(t *testing.T) {
	p := startPool(t, runtime.Builtin())
	_, creds := p.admit(access.Client)
	conn, err := grpc.NewClient(p.addr, grpc.WithTransportCredentials(creds))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	other := pb.NewNodeServiceClient(conn)

	spec := func(tasks uint32) *pb.JobSpec {
		return &pb.JobSpec{Workload: "primes", Params: []byte(`{"from":0,"to":1000}`), Mode: pb.ScheduleMode_SCHEDULE_MODE_DISTRIBUTED, MaxTasks: tasks}
	}
	first := p.submit(spec(6))
	second, err := other.SubmitJob(p.ctx, &pb.SubmitJobRequest{Spec: spec(4)})
	if err != nil {
		t.Fatal(err)
	}

	// A worker with two slots is given one task of each, not two of the
	// first job.
	w := p.connectRaw(hello("pair", 2, "primes"))
	w.welcome()
	got := map[string]int{}
	for range 2 {
		got[w.assignment().GetJobId()]++
	}
	if got[first.GetJobId()] != 1 || got[second.GetJob().GetJobId()] != 1 {
		t.Errorf("two slots went %v, want one to each client's job", got)
	}

	// With only one client's work waiting, all of a worker's slots go to it.
	if _, err := p.client.CancelJob(p.ctx, &pb.CancelJobRequest{JobId: second.GetJob().GetJobId()}); err != nil {
		t.Fatal(err)
	}
	alone := p.connectRaw(hello("alone", 3, "primes"))
	alone.welcome()
	for range 3 {
		if job := alone.assignment().GetJobId(); job != first.GetJobId() {
			t.Errorf("a slot went to %s, want the only job still waiting", job)
		}
	}
}

// Each copy of a verified job's task takes a slot, and is shared out like
// any task: a client with a verified job does not take a second slot for a
// second copy while another client has nothing running.
func TestCopiesOfVerifiedTasksAreSharedBetweenClients(t *testing.T) {
	p := startPool(t, runtime.Builtin())
	_, creds := p.admit(access.Client)
	conn, err := grpc.NewClient(p.addr, grpc.WithTransportCredentials(creds))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	other := pb.NewNodeServiceClient(conn)

	// Two workers of a slot each, both busy with a job of the node's own.
	one, two := p.connectRaw(hello("one", 1, "primes")), p.connectRaw(hello("two", 1, "primes"))
	one.welcome()
	two.welcome()
	spec := func(tasks, verify uint32) *pb.JobSpec {
		return &pb.JobSpec{Workload: "primes", Params: []byte(`{"from":0,"to":1000}`), Mode: pb.ScheduleMode_SCHEDULE_MODE_DISTRIBUTED, MaxTasks: tasks, Verify: verify}
	}
	p.submit(spec(2, 0))
	busy := []*pb.TaskAssignment{one.assignment(), two.assignment()}

	// The node then asks for a job verified by both, and the client for
	// one of its own.
	verified := p.submit(spec(2, 2))
	plain, err := other.SubmitJob(p.ctx, &pb.SubmitJobRequest{Spec: spec(2, 0)})
	if err != nil {
		t.Fatal(err)
	}

	// The first slot to free goes to the client, which has nothing
	// running, where the node has a task still on the other worker.
	one.reply(&pb.TaskResult{TaskId: busy[0].GetTaskId(), Attempt: busy[0].GetAttempt(), Outcome: &pb.TaskResult_Output{Output: []byte(`{"count":5}`)}})
	if got := one.assignment().GetJobId(); got != plain.GetJob().GetJobId() {
		t.Errorf("the first slot to free went to %s, want the client's job", got)
	}
	// The next goes to a copy of the node's verified task, the node now
	// having nothing running.
	two.reply(&pb.TaskResult{TaskId: busy[1].GetTaskId(), Attempt: busy[1].GetAttempt(), Outcome: &pb.TaskResult_Output{Output: []byte(`{"count":5}`)}})
	if got := two.assignment().GetJobId(); got != verified.GetJobId() {
		t.Errorf("the second slot to free went to %s, want the verified job", got)
	}
}

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

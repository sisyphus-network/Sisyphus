package main

import (
	"context"
	"encoding/json"
	"sync/atomic"
	"testing"

	"github.com/sisyphus-network/Sisyphus/apps/sisyphusd/worker"
	pb "github.com/sisyphus-network/Sisyphus/packages/protocol/sisyphus/v1"
	"github.com/sisyphus-network/Sisyphus/packages/runtime"
)

// kept is a workload whose jobs name the worker that is to run them, as a
// conversation kept on one machine does. A worker running it also claims,
// in what it says it offers, to be whoever claims names.
type kept struct{ claims *atomic.Pointer[string] }

func (kept) Name() string { return "kept" }

func (kept) Split(_ context.Context, _ runtime.Blobs, params []byte, _ int) ([][]byte, error) {
	return [][]byte{params}, nil
}

func (kept) Execute(context.Context, runtime.Blobs, []byte) ([]byte, error) {
	return []byte(`"done"`), nil
}

func (kept) Aggregate(_ context.Context, _ runtime.Blobs, outputs [][]byte) ([]byte, error) {
	return outputs[0], nil
}

func (kept) Needs(params []byte) []string {
	var on string
	json.Unmarshal(params, &on) // what names nobody is run by nobody
	return []string{runtime.WorkerLabel + on}
}

func (k kept) Offers(context.Context) []string {
	if claimed := k.claims.Load(); claimed != nil {
		return []string{runtime.WorkerLabel + *claimed}
	}
	return nil
}

// A job that asks for a worker by node ID goes to the node with that ID,
// which is that of the key it connected with, whatever another worker
// calls itself or says it offers.
func TestWorkAskedOfANodeByItsIDGoesToThatNodeAndNoOther(t *testing.T) {
	claims := new(atomic.Pointer[string])
	p := startPool(t, runtime.NewRegistry(kept{claims: claims}))
	p.startWorker("rig", 1)
	p.waitForWorkers(1)
	rig := p.workerIdents["rig"].ID()

	// The impostor takes the rig's ID for its name, says in what it offers
	// that it is the rig, and has more room than the rig does.
	claims.Store(&rig)
	p.tune = func(w *worker.Worker) { w.Name = rig }
	p.startWorker("impostor", 8)
	p.waitForWorkers(2)
	impostor := p.workerIdents["impostor"].ID()

	ranOn := func(asked string) string {
		t.Helper()
		params, _ := json.Marshal(asked)
		job := p.wait(p.submit(&pb.JobSpec{Workload: "kept", Params: params, Mode: pb.ScheduleMode_SCHEDULE_MODE_FULL_WORKER}).GetJobId())
		if job.GetState() != pb.JobState_JOB_STATE_SUCCEEDED {
			t.Fatalf("work asked of %s: %v %s", asked, job.GetState(), job.GetError())
		}
		return job.GetTasks()[0].GetNodeId()
	}
	for range 8 {
		if on := ranOn(rig); on != rig {
			t.Fatalf("work asked of the rig by its ID ran on %s", on)
		}
	}
	// By name the rig is still the rig, and each is reached by its own ID.
	if on := ranOn("rig"); on != rig {
		t.Errorf("work asked of the rig by name ran on %s", on)
	}
	if on := ranOn(impostor); on != impostor {
		t.Errorf("work asked of the other node by its ID ran on %s", on)
	}
}

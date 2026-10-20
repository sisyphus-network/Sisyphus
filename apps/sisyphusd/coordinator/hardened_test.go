package coordinator

import (
	"slices"
	"testing"

	jobmodel "github.com/sisyphus-network/Sisyphus/packages/job-model"
	pb "github.com/sisyphus-network/Sisyphus/packages/protocol/sisyphus/v1"
	"github.com/sisyphus-network/Sisyphus/packages/runtime"
)

func TestAHardenedCoordinatorGivesContainerWorkOnlyToHardenedWorkers(t *testing.T) {
	workloads := runtime.WithContainers()
	plain := &Coordinator{workloads: workloads}
	hardened := &Coordinator{workloads: workloads, hardened: true}
	image := []byte(`{"image":"alpine:3.20","network":true}`)

	// A coordinator that does not ask it needs nothing more than the job does.
	if needs := plain.needs("container", image, false); !slices.Equal(needs, []string{runtime.NetworkLabel}) {
		t.Errorf("a plain coordinator's container job needs %v", needs)
	}
	// One that does asks it of every workload whose tasks run in
	// containers, as well as what the job asks, and of no other.
	if needs := hardened.needs("container", image, true); !slices.Equal(needs, []string{runtime.NetworkLabel, runtime.SealingLabel, runtime.BoundedTmpLabel, runtime.SandboxedLabel}) {
		t.Errorf("a hardened coordinator's container job needs %v", needs)
	}
	if needs := hardened.needs("transcode", []byte(`{"input":"bafy"}`), false); !slices.Equal(needs, runtime.HardenedLabels) {
		t.Errorf("a hardened coordinator's transcode job needs %v", needs)
	}
	if needs := hardened.needs("primes", []byte(`{}`), false); len(needs) != 0 {
		t.Errorf("a hardened coordinator's primes job needs %v", needs)
	}

	// So a worker that is only sandboxed is passed over for one that is
	// hardened in both ways.
	job := &jobmodel.Job{Workload: "container", Needs: hardened.needs("container", []byte(`{"image":"alpine:3.20"}`), false)}
	newWorker := func(id string, labels ...string) *worker {
		return &worker{id: id, running: make(map[string]assignment),
			capabilities: &pb.NodeCapabilities{TaskSlots: 1, Workloads: []string{"container"}, Labels: labels}}
	}
	half := newWorker("half", runtime.SandboxedLabel)
	hardened.workers = map[string]*worker{"half": half}
	if got, _ := hardened.pickWorkerLocked(job, &jobmodel.Task{}); got != nil {
		t.Errorf("a worker that is only sandboxed was picked")
	}
	whole := newWorker("whole", runtime.SandboxedLabel, runtime.BoundedTmpLabel)
	hardened.workers["whole"] = whole
	if got, _ := hardened.pickWorkerLocked(job, &jobmodel.Task{}); got != whole {
		t.Errorf("picked %v, want the hardened worker", got)
	}
}

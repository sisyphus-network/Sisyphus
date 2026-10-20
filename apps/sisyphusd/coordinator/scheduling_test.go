package coordinator

import (
	"testing"

	jobmodel "github.com/sisyphus-network/Sisyphus/packages/job-model"
	pb "github.com/sisyphus-network/Sisyphus/packages/protocol/sisyphus/v1"
)

func TestPickWorkerSpreadsModelTasksBeforePreferringGPU(t *testing.T) {
	const model = "llama3.1:8b"
	job := &jobmodel.Job{Needs: []string{"model:" + model}, Workload: "prompts"}
	task := &jobmodel.Task{}
	newWorker := func(id string, labels ...string) *worker {
		return &worker{
			id:           id,
			capabilities: &pb.NodeCapabilities{TaskSlots: 4, Workloads: []string{"prompts"}, Labels: append([]string{"model:" + model}, labels...)},
			running:      make(map[string]assignment),
		}
	}

	// A worker already handling a request for this model must lose to an idle
	// worker, even when it has the model on a graphics card.
	gpu := newWorker("gpu", "loaded:"+model+"=100")
	gpu.running["gpu-task"] = assignment{job: &jobmodel.Job{Needs: job.Needs}, task: &jobmodel.Task{ID: "gpu-task"}}
	cpu := newWorker("cpu", "loaded:"+model+"=0")
	c := &Coordinator{workers: map[string]*worker{"gpu": gpu, "cpu": cpu}}
	task = &jobmodel.Task{History: []jobmodel.Attempt{{NodeID: "gpu", State: jobmodel.Running}}}
	if got, _ := c.pickWorkerLocked(job, task); got != cpu {
		if got == nil {
			t.Fatal("picked no worker, want the idle worker so model requests are spread")
		}
		t.Fatalf("picked %q, want the idle worker so model requests are spread", got.id)
	}

	// Once both servers have equal model load, prefer the model already on a card.
	cpu.running["cpu-task"] = assignment{job: &jobmodel.Job{Needs: job.Needs}, task: &jobmodel.Task{ID: "cpu-task"}}
	task = &jobmodel.Task{}
	if got, _ := c.pickWorkerLocked(job, task); got != gpu {
		if got == nil {
			t.Fatal("picked no worker, want the equally loaded GPU worker")
		}
		t.Fatalf("picked %q, want the equally loaded GPU worker", got.id)
	}

	// A worker with no loaded-model report ranks below one that reports the
	// model resident, all else being equal.
	cpu.capabilities.Labels = []string{"model:" + model}
	if got, _ := c.pickWorkerLocked(job, task); got != gpu {
		if got == nil {
			t.Fatal("picked no worker, want the worker reporting the model loaded")
		}
		t.Fatalf("picked %q, want the worker reporting the model loaded", got.id)
	}
}

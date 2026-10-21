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

// A worker gives tasks that hold graphics cards no more than it has cards
// free, whatever its slots, and counts its cards as such tasks hold them.
func TestTasksOnGraphicsCardsAreHeldToTheCards(t *testing.T) {
	onOne, onTwo, onNone := &jobmodel.Job{MinGPUs: 1}, &jobmodel.Job{MinGPUs: 2}, &jobmodel.Job{}
	w := &worker{
		id:           "rig",
		capabilities: &pb.NodeCapabilities{TaskSlots: 6, Workloads: []string{"md"}, Gpus: make([]*pb.Gpu, 3)},
		running:      map[string]assignment{"a": {job: onOne}, "b": {job: onNone}},
	}
	for gpus, want := range map[int]int{0: 4, 1: 2, 2: 1, 3: 0} {
		if got := w.freeFor(gpus); got != want {
			t.Errorf("with one card held, room for %d tasks of %d cards, want %d", got, gpus, want)
		}
	}
	w.running["c"] = assignment{job: onTwo}
	if got, cpu := w.freeFor(1), w.freeFor(0); got != 0 || cpu != 3 {
		t.Errorf("with every card held, room for %d tasks on cards and %d on the processor, want 0 and 3", got, cpu)
	}

	idle := &worker{id: "idle", capabilities: &pb.NodeCapabilities{TaskSlots: 6, Workloads: []string{"md"}, Gpus: make([]*pb.Gpu, 3)}}
	c := &Coordinator{workers: map[string]*worker{"rig": w, "idle": idle}}
	for gpus, want := range map[int]int{0: 12, 1: 6, 2: 2} {
		if got := c.slotsLocked("md", 0, gpus, nil); got != want {
			t.Errorf("%d tasks of %d cards can run at once, want %d", got, gpus, want)
		}
	}
	if got, _ := c.pickWorkerLocked(&jobmodel.Job{Workload: "md", MinGPUs: 1}, &jobmodel.Task{}); got != idle {
		t.Errorf("a task on a card went to %v, want the worker with cards free", got)
	}
	idle.running = map[string]assignment{"d": {job: onTwo}, "e": {job: onOne}}
	if got, _ := c.pickWorkerLocked(&jobmodel.Job{Workload: "md", MinGPUs: 1}, &jobmodel.Task{}); got != nil {
		t.Errorf("a task on a card went to %s, with every card held", got.id)
	}
	if got, _ := c.pickWorkerLocked(&jobmodel.Job{Workload: "md"}, &jobmodel.Task{}); got == nil {
		t.Error("a task on the processor waits, with slots free")
	}
}

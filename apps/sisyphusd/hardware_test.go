package main

import (
	"context"
	"encoding/json"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sisyphus-network/Sisyphus/apps/sisyphusd/worker"
	"github.com/sisyphus-network/Sisyphus/packages/hardware"
	nodepb "github.com/sisyphus-network/Sisyphus/packages/protocol/sisyphus/node/v1"
	pb "github.com/sisyphus-network/Sisyphus/packages/protocol/sisyphus/v1"
	"github.com/sisyphus-network/Sisyphus/packages/runtime"
)

// startMachine starts a worker that says it has the given hardware.
func (p *pool) startMachine(name string, slots int, has hardware.Info) {
	p.t.Helper()
	p.tune = func(w *worker.Worker) { w.Hardware = has }
	p.startWorker(name, slots)
	p.tune = nil
}

func TestTasksGoOnlyToWorkersWithWhatTheJobAsksFor(t *testing.T) {
	p := startPool(t, runtime.Builtin())
	p.startMachine("laptop", 4, hardware.Info{CPUModel: "a small one", MemoryBytes: 8 << 30})
	p.startMachine("rig", 2, hardware.Info{CPUModel: "a big one", MemoryBytes: 256 << 30, GPUs: []hardware.GPU{{Name: "RTX 4090", MemoryBytes: 24 << 30}, {Name: "RTX 4090", MemoryBytes: 24 << 30}}})
	p.startWorker("unknown", 4) // says nothing of what it has
	p.waitForWorkers(3)

	// The coordinator knows what each has.
	for _, node := range p.coord.Nodes() {
		c := node.GetCapabilities()
		switch node.GetName() {
		case "rig":
			if c.GetCpuModel() != "a big one" || c.GetMemoryBytes() != 256<<30 || len(c.GetGpus()) != 2 || c.GetGpus()[0].GetName() != "RTX 4090" || c.GetGpus()[0].GetMemoryBytes() != 24<<30 {
				t.Errorf("the rig is known as %v", c)
			}
		case "unknown":
			if c.GetMemoryBytes() != 0 || len(c.GetGpus()) != 0 {
				t.Errorf("a worker that said nothing is known as %v", c)
			}
		}
	}

	ranOn := func(spec *pb.JobSpec) map[string]int {
		t.Helper()
		job := p.wait(p.submit(spec).GetJobId())
		if job.GetState() != pb.JobState_JOB_STATE_SUCCEEDED {
			t.Fatalf("job %v: %s", job.GetState(), job.GetError())
		}
		on := make(map[string]int)
		for _, task := range job.GetTasks() {
			on[task.GetNodeName()]++
		}
		return on
	}
	primes := func(change func(*pb.JobSpec)) *pb.JobSpec {
		spec := primesJob(pb.ScheduleMode_SCHEDULE_MODE_DISTRIBUTED, 0)
		change(spec)
		return spec
	}
	// Asking nothing, a job is split across every slot there is.
	if on := ranOn(primes(func(*pb.JobSpec) {})); on["laptop"]+on["rig"]+on["unknown"] != 10 {
		t.Errorf("a job that asks nothing ran as %v, want ten tasks across all three", on)
	}
	// Asking for memory, it goes to those that have it and is split to fit.
	if on := ranOn(primes(func(s *pb.JobSpec) { s.MinMemoryBytes = 4 << 30 })); on["unknown"] != 0 || on["laptop"] != 4 || on["rig"] != 2 {
		t.Errorf("a job that asks for 4 GiB ran as %v", on)
	}
	if on := ranOn(primes(func(s *pb.JobSpec) { s.MinMemoryBytes = 64 << 30 })); len(on) != 1 || on["rig"] != 2 {
		t.Errorf("a job that asks for 64 GiB ran as %v", on)
	}
	// Asking for graphics cards, likewise, however many tasks it is in.
	if on := ranOn(primes(func(s *pb.JobSpec) { s.MinGpus, s.MaxTasks = 2, 5 })); len(on) != 1 || on["rig"] != 5 {
		t.Errorf("a job that asks for two graphics cards ran as %v", on)
	}

	// What nobody has, waits, and says so in what it asks for.
	waiting := p.submit(primes(func(s *pb.JobSpec) { s.MinGpus = 3 }))
	time.Sleep(100 * time.Millisecond)
	if job := p.job(waiting.GetJobId()); job.GetState() != pb.JobState_JOB_STATE_PENDING || job.GetSpec().GetMinGpus() != 3 || len(job.GetTasks()) != 1 {
		t.Errorf("a job nobody can run: %v", job)
	}
	// Until someone who has it turns up.
	p.startMachine("monster", 1, hardware.Info{MemoryBytes: 1 << 40, GPUs: make([]hardware.GPU, 4)})
	if job := p.wait(waiting.GetJobId()); job.GetTasks()[0].GetNodeName() != "monster" {
		t.Errorf("the job that was waiting ran on %s", job.GetTasks()[0].GetNodeName())
	}
}

func TestANodeSaysWhatItHasAndTheOwnerCanOfferLess(t *testing.T) {
	dataDir := t.TempDir()
	addr, apiAddr := freeAddr(t), freeAddr(t)
	startDaemon(t, "--data-dir", dataDir, "--listen", addr, "--name", "rig", "--slots", "1", "--api-listen", apiAddr, "--offer-memory-mb", "2048", "--offer-gpus", "0")
	client := desktop(t, apiAddr)
	poolAsSeenBy(t, client)
	out := waitForOutput(t, "rig", "nodes", "--addr", addr)
	if !strings.Contains(out, "MEMORY") || !strings.Contains(out, "GPUS") || !strings.Contains(out, "2.0 GiB") {
		t.Errorf("nodes output:\n%s", out)
	}
	workers, err := client.ListWorkers(context.Background(), &nodepb.ListWorkersRequest{})
	if err != nil || len(workers.GetWorkers()) != 1 || workers.GetWorkers()[0].GetMemoryBytes() != 2048<<20 || len(workers.GetWorkers()[0].GetGpus()) != 0 {
		t.Fatalf("ListWorkers = %v, %v", workers, err)
	}

	// A job within what was offered runs; one beyond it waits.
	if out := mustCLI(t, "job", "submit", "--addr", addr, "--min-memory-mb", "1024", "--params", `{"from":0,"to":100}`); !strings.Contains(out, `{"count":25}`) {
		t.Errorf("a job asking for 1 GiB:\n%s", out)
	}
	id := strings.TrimSpace(mustCLI(t, "job", "submit", "--addr", addr, "--detach", "--min-memory-mb", "4096", "--gpus", "1", "--params", `{"from":0,"to":100}`))
	time.Sleep(100 * time.Millisecond)
	submitted, err := client.GetJob(tokenOf(t, dataDir), &nodepb.GetJobRequest{JobId: id})
	if err != nil || submitted.GetJob().GetState() != nodepb.JobState_JOB_STATE_PENDING || submitted.GetJob().GetMinMemoryBytes() != 4096<<20 || submitted.GetJob().GetMinGpus() != 1 {
		t.Errorf("a job asking for more than was offered: %v, %v", submitted, err)
	}
	// The desktop asks for the same things in the same way.
	ctx := tokenOf(t, dataDir)
	viaAPI, err := client.SubmitJob(ctx, &nodepb.SubmitJobRequest{Workload: "primes", Params: []byte(`{"from":0,"to":100}`), MinMemoryBytes: 8 << 30, MinGpus: 2})
	if err != nil || viaAPI.GetJob().GetMinMemoryBytes() != 8<<30 || viaAPI.GetJob().GetMinGpus() != 2 {
		t.Errorf("SubmitJob = %v, %v", viaAPI, err)
	}
}

// carded is a gated workload whose tasks each hold as many graphics cards
// as its parameters say.
type carded struct{ gated }

func (carded) Name() string { return "carded" }

func (carded) GPUsNeeded(params []byte) int {
	var p struct {
		GPUs int `json:"gpus"`
	}
	json.Unmarshal(params, &p)
	return p.GPUs
}

func TestAGraphicsCardRunsOneTaskAtATimeAndTheOtherSlotsTakeTheRest(t *testing.T) {
	cards := carded{gated{started: make(chan struct{}, 16), release: make(chan struct{})}}
	cpu := gated{started: make(chan struct{}, 16), release: make(chan struct{})}
	p := startPool(t, runtime.NewRegistry(cards, cpu))
	p.startMachine("laptop", 4, hardware.Info{MemoryBytes: 16 << 30, GPUs: []hardware.GPU{{Name: "RTX 4050", MemoryBytes: 6 << 30}}})
	p.startMachine("r620", 4, hardware.Info{MemoryBytes: 64 << 30})
	p.waitForWorkers(2)
	quiet := func(started chan struct{}, what string) {
		t.Helper()
		select {
		case <-started:
			t.Fatalf("%s started", what)
		case <-time.After(150 * time.Millisecond):
		}
	}

	// The job asks for no card itself; its workload says each task holds
	// one, so it goes only to the laptop, and to its one card one at a
	// time, though the laptop has slots for four.
	onCard := p.submit(&pb.JobSpec{Workload: "carded", Params: []byte(`{"gpus":1}`), MaxTasks: 3})
	<-cards.started
	quiet(cards.started, "a second task on the laptop's one graphics card")
	if job := p.job(onCard.GetJobId()); job.GetSpec().GetMinGpus() != 1 {
		t.Errorf("the job asks for %d graphics cards, want the 1 its workload needs", job.GetSpec().GetMinGpus())
	}
	// Tasks that need no card fill the laptop's other slots and the r620's.
	offCard := p.submit(&pb.JobSpec{Workload: "gated", MaxTasks: 7})
	for range 7 {
		<-cpu.started
	}
	quiet(cards.started, "a second task on the laptop's one graphics card, beside the processor's tasks")

	// As each task on the card ends, the next takes it.
	cards.release <- struct{}{}
	<-cards.started
	close(cards.release)
	<-cards.started
	close(cpu.release)
	gpuJob, cpuJob := p.wait(onCard.GetJobId()), p.wait(offCard.GetJobId())
	if gpuJob.GetState() != pb.JobState_JOB_STATE_SUCCEEDED || cpuJob.GetState() != pb.JobState_JOB_STATE_SUCCEEDED {
		t.Fatalf("the jobs: %v (%s), %v (%s)", gpuJob.GetState(), gpuJob.GetError(), cpuJob.GetState(), cpuJob.GetError())
	}
	for _, task := range gpuJob.GetTasks() {
		if task.GetNodeName() != "laptop" {
			t.Errorf("a task that needs a graphics card ran on %s", task.GetNodeName())
		}
	}
	on := map[string]int{}
	for _, task := range cpuJob.GetTasks() {
		on[task.GetNodeName()]++
	}
	if on["laptop"] != 3 || on["r620"] != 4 {
		t.Errorf("the processor's tasks ran as %v, want three on the laptop beside its card's task and four on the r620", on)
	}

	// A job left to the pool is split into as many tasks as there are
	// cards to run it, not slots. One that needs more cards than anyone
	// has waits, as a job that asks for them does.
	if job := p.wait(p.submit(&pb.JobSpec{Workload: "carded", Params: []byte(`{"gpus":1}`)}).GetJobId()); len(job.GetTasks()) != 1 || job.GetState() != pb.JobState_JOB_STATE_SUCCEEDED {
		t.Errorf("a job on the card left to the pool: %d tasks, %v", len(job.GetTasks()), job.GetState())
	}
	waiting := p.submit(&pb.JobSpec{Workload: "carded", Params: []byte(`{"gpus":2}`), MinGpus: 1})
	time.Sleep(100 * time.Millisecond)
	if job := p.job(waiting.GetJobId()); job.GetState() != pb.JobState_JOB_STATE_PENDING || job.GetSpec().GetMinGpus() != 2 {
		t.Errorf("a job that needs two graphics cards: %v, asking for %d", job.GetState(), job.GetSpec().GetMinGpus())
	}
	p.startMachine("rig", 4, hardware.Info{MemoryBytes: 64 << 30, GPUs: make([]hardware.GPU, 2)})
	if job := p.wait(waiting.GetJobId()); job.GetTasks()[0].GetNodeName() != "rig" {
		t.Errorf("the job that needs two graphics cards ran on %s", job.GetTasks()[0].GetNodeName())
	}
}

// asking is a gated workload of another name, whose tasks hold no cards.
type asking struct{ gated }

func (asking) Name() string { return "asking" }

func TestAJobThatOnlyAsksForCardsTakesEverySlotOfAMachineWithThem(t *testing.T) {
	cards := carded{gated{started: make(chan struct{}, 16), release: make(chan struct{})}}
	plain := asking{gated{started: make(chan struct{}, 16), release: make(chan struct{})}}
	p := startPool(t, runtime.NewRegistry(cards, plain))
	p.startMachine("laptop", 4, hardware.Info{MemoryBytes: 16 << 30, GPUs: []hardware.GPU{{Name: "RTX 4050", MemoryBytes: 6 << 30}}})
	p.startMachine("r620", 4, hardware.Info{MemoryBytes: 64 << 30})
	p.waitForWorkers(2)

	// The laptop's one card is held by a task that needs it.
	onCard := p.submit(&pb.JobSpec{Workload: "carded", Params: []byte(`{"gpus":1}`), MaxTasks: 1})
	<-cards.started
	// A job that asks for a machine with a card, for work that needs none,
	// goes only to the laptop, is split across all its slots, and takes
	// every one the card's task leaves free.
	picky := p.submit(&pb.JobSpec{Workload: "asking", MinGpus: 1})
	for range 3 {
		<-plain.started
	}
	if job := p.job(picky.GetJobId()); len(job.GetTasks()) != 4 || job.GetSpec().GetMinGpus() != 1 {
		t.Errorf("a job that only asks for a card: %d tasks, asking for %d cards", len(job.GetTasks()), job.GetSpec().GetMinGpus())
	}
	close(cards.release)
	close(plain.release)
	p.wait(onCard.GetJobId())
	job := p.wait(picky.GetJobId())
	if job.GetState() != pb.JobState_JOB_STATE_SUCCEEDED {
		t.Fatalf("job %v: %s", job.GetState(), job.GetError())
	}
	for _, task := range job.GetTasks() {
		if task.GetNodeName() != "laptop" {
			t.Errorf("a task of a job that asks for a card ran on %s", task.GetNodeName())
		}
	}
}

func TestAWorkerSaysWhatAcceleratorsItHas(t *testing.T) {
	p := startPool(t, runtime.Builtin())
	npu := hardware.Accelerator{Kind: hardware.KindNPU, Vendor: "Intel", Model: "Intel NPU (Meteor Lake)", Driver: "intel_vpu"}
	tpu := hardware.Accelerator{Kind: hardware.KindEdgeTPU, Vendor: "Google", Model: "Coral USB Accelerator"}
	p.startMachine("laptop", 2, hardware.Info{Accelerators: []hardware.Accelerator{npu, tpu}})
	p.startWorker("plain", 2)
	p.waitForWorkers(2)
	for _, node := range p.coord.Nodes() {
		c := node.GetCapabilities()
		switch node.GetName() {
		case "laptop":
			got := c.GetAccelerators()
			if len(got) != 2 || got[0].GetKind() != "npu" || got[0].GetVendor() != "Intel" || got[0].GetModel() != npu.Model || got[0].GetDriver() != "intel_vpu" || got[1].GetKind() != "edgetpu" {
				t.Errorf("the laptop's accelerators are known as %v", got)
			}
			for _, label := range []string{"npu", "npu-intel", "edgetpu", "edgetpu-google"} {
				if !slices.Contains(c.GetLabels(), label) {
					t.Errorf("the laptop's labels %v lack %s", c.GetLabels(), label)
				}
			}
		case "plain":
			if len(c.GetAccelerators()) != 0 || slices.Contains(c.GetLabels(), "npu") {
				t.Errorf("a worker with no accelerators is known as %v", c)
			}
		}
	}
	// `sisyphusd nodes` says what each has, below its table.
	listed, err := p.client.ListNodes(p.ctx, &pb.ListNodesRequest{})
	if err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	stdout = &out
	t.Cleanup(func() { stdout = os.Stdout })
	if err := printNodes(listed.GetNodes()); err != nil {
		t.Fatal(err)
	}
	if said := out.String(); !strings.Contains(said, "\nlaptop: accelerators: npu Intel NPU (Meteor Lake) [intel_vpu]; edgetpu Google Coral USB Accelerator\n") || strings.Contains(said, "plain: accelerators") {
		t.Errorf("nodes says:\n%s", said)
	}
}

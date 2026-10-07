package main

import (
	"context"
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
	submitted, err := client.GetJob(context.Background(), &nodepb.GetJobRequest{JobId: id})
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

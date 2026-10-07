package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sisyphus-network/Sisyphus/packages/runtime"
)

// requireDocker skips a test that needs Docker where there is none, unless
// that is not to be let pass.
func requireDocker(t *testing.T) {
	t.Helper()
	if err := exec.Command("docker", "version").Run(); err != nil {
		if os.Getenv("SISYPHUS_REQUIRE_DOCKER") != "" {
			t.Fatal("docker cannot be used, and SISYPHUS_REQUIRE_DOCKER is set")
		}
		t.Skip("docker cannot be used")
	}
}

func TestAPoolRunsAContainerOnEachTask(t *testing.T) {
	requireDocker(t)
	addr := freeAddr(t)
	startDaemon(t, "--role", "coordinator", "--listen", addr)
	startDaemon(t, "--role", "worker", "--coordinator", addr, "--name", "plain", "--slots", "2")
	waitForOutput(t, "plain", "nodes", "--addr", addr)

	// With no worker that runs containers, such a job is taken in and waits.
	params := `{"image":"alpine:3.20","command":["sh","-c","echo I am task $SISYPHUS_TASK_INDEX of $SISYPHUS_TASK_COUNT; echo $SISYPHUS_TASK_INDEX > /output/index"]}`
	id := strings.TrimSpace(mustCLI(t, "job", "submit", "--addr", addr, "--detach", "--workload", "container", "--tasks", "3", "--params", params))
	if out := mustCLI(t, "job", "get", "--addr", addr, id); !strings.Contains(out, "pending") {
		t.Errorf("a container job with nobody to run it:\n%s", out)
	}
	if nodes := mustCLI(t, "nodes", "--addr", addr); strings.Contains(nodes, "container") {
		t.Errorf("a worker that was not told to offers containers:\n%s", nodes)
	}

	// A worker whose owner allows it takes the job.
	startDaemon(t, "--role", "worker", "--coordinator", addr, "--name", "docker-host", "--slots", "3", "--containers")
	waitForOutput(t, "docker-host", "nodes", "--addr", addr)
	out := waitForOutput(t, "succeeded in", "job", "get", "--addr", addr, id)
	if strings.Contains(out, "on plain") {
		t.Errorf("a container task ran on the worker that does not run them:\n%s", out)
	}
	var result runtime.ContainerResult
	if err := json.Unmarshal([]byte(out[strings.LastIndex(out, "\n{")+1:]), &result); err != nil || len(result.Tasks) != 3 {
		t.Fatalf("the job's result, in:\n%s\n%v", out, err)
	}
	// Each task's output is stored like any other data.
	dir := t.TempDir()
	for i, task := range result.Tasks {
		printed, index := filepath.Join(dir, "printed"), filepath.Join(dir, "index")
		mustCLI(t, "blob", "get", "--addr", addr, "-o", printed, task.Stdout)
		mustCLI(t, "blob", "get", "--addr", addr, "-o", index, task.Files["index"])
		gotPrinted, _ := os.ReadFile(printed)
		gotIndex, _ := os.ReadFile(index)
		if want := "I am task " + string(rune('0'+i)) + " of 3\n"; string(gotPrinted) != want || strings.TrimSpace(string(gotIndex)) != string(rune('0'+i)) {
			t.Errorf("task %d printed %q and wrote %q", i, gotPrinted, gotIndex)
		}
	}
	// And what it printed is in the job's log.
	if logs := mustCLI(t, "job", "logs", "--addr", addr, id); !strings.Contains(logs, "on docker-host log: I am task 1 of 3") || !strings.Contains(logs, "log: running alpine:3.20 sh -c") {
		t.Errorf("the job's log:\n%s", logs)
	}
}

func TestANodeToldToRunContainersChecksThatItCan(t *testing.T) {
	old := checkContainers
	checkContainers = func(context.Context) error { return errors.New("Docker cannot be used here: permission denied") }
	defer func() { checkContainers = old }()
	_, err := cli(t, "run", "--data-dir", t.TempDir(), "--listen", freeAddr(t), "--containers")
	if err == nil || !strings.Contains(err.Error(), "--containers: Docker cannot be used here") {
		t.Errorf("error %v, want the node not started", err)
	}
}

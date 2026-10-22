package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/sisyphus-network/Sisyphus/packages/runtime"
)

const containerTestImage = "alpine:3.20"

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

// dockerEventsWithin bounds the failure diagnostic's `docker events` call.
// The command already returns at --until; this guards a stalled daemon.
const dockerEventsWithin = 10 * time.Second

// dockerEventsSince lists what Docker itself recorded for the test image's
// containers from start until now. Tasks run with --rm, so a container that
// was created, started and exited leaves nothing for `docker ps -a`; its
// events remain. It runs docker as the worker does, with the test's
// environment and so its Docker context. What docker says on stderr appears
// among the events, as unparsed lines.
func dockerEventsSince(start time.Time) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), dockerEventsWithin)
	defer cancel()
	out, err := exec.CommandContext(ctx, "docker", "events",
		"--since", dockerTimestamp(start), "--until", dockerTimestamp(time.Now()),
		"--filter", "type=container", "--filter", "image="+containerTestImage,
		"--format", "{{json .}}").CombinedOutput()
	return summarizeDockerEvents(out), err
}

// dockerNetworkEventsSince lists the network events Docker recorded from
// start until now, every network's and not only the test's: a container
// on Docker's bridge network takes a lock that can hold up every other
// container's start, the test's included, while it is set up (#314).
func dockerNetworkEventsSince(start time.Time) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), dockerEventsWithin)
	defer cancel()
	out, err := exec.CommandContext(ctx, "docker", "events",
		"--since", dockerTimestamp(start), "--until", dockerTimestamp(time.Now()),
		"--filter", "type=network", "--format", "{{json .}}").CombinedOutput()
	return summarizeDockerEvents(out), err
}

// dockerProbeWithin bounds the failure diagnostic's probe container, and
// dockerProbeKeeps how much of what docker -D says of it is kept: the end,
// where a stalled request is.
const (
	dockerProbeWithin = 15 * time.Second
	dockerProbeKeeps  = 4 << 10
)

// dockerProbe starts a container as the worker would start a task's, with
// no network, through the Docker client's debug log, and says how long it
// took and the end of that log. Run while the test's job is stalled, it
// shows whether a new container can be created at all, and if not, which
// request the client is waiting on.
func dockerProbe() string {
	ctx, cancel := context.WithTimeout(context.Background(), dockerProbeWithin)
	defer cancel()
	began := time.Now()
	out, err := exec.CommandContext(ctx, "docker", "-D", "run", "--rm", "--network", "none", containerTestImage, "true").CombinedOutput()
	return fmt.Sprintf("took %s (error %v): %s", time.Since(began).Round(time.Millisecond), err, dockerDebugTail(out))
}

// lastBytes is the last n bytes of b, marked as cut where they are.
func lastBytes(b []byte, n int) []byte {
	if len(b) <= n {
		return b
	}
	return append([]byte("…"), b[len(b)-n:]...)
}

// What of Docker's debug log could carry a credential: an authorization
// or registry-auth header, whose value may be several words and is cut to
// the end of its line, or a token, password or secret given as a value.
var (
	headerInDockerDebug = regexp.MustCompile(`(?i)((?:authorization|x-registry-auth)["']?\s*[:=]\s*)[^\n]*`)
	secretInDockerDebug = regexp.MustCompile(`(?i)((?:token|password|secret)["']?\s*[:=]\s*)("[^"]*"|\S+)`)
)

// redactDockerDebug replaces any such value with <redacted>.
func redactDockerDebug(b []byte) string {
	return secretInDockerDebug.ReplaceAllString(headerInDockerDebug.ReplaceAllString(string(b), "${1}<redacted>"), "${1}<redacted>")
}

// dockerDebugTail redacts before cutting: otherwise the cut could remove the
// start of a sensitive header and leave the credential in the retained tail.
func dockerDebugTail(b []byte) string {
	return string(lastBytes([]byte(redactDockerDebug(b)), dockerProbeKeeps))
}

// dockerTimestamp writes a time as docker events' --since and --until take
// it: Unix seconds with a fraction.
func dockerTimestamp(at time.Time) string {
	return fmt.Sprintf("%d.%09d", at.Unix(), at.Nanosecond())
}

// summarizeDockerEvents turns `docker events --format '{{json .}}'` output
// into one line per event: its time, action, container name and short ID,
// and the exit code of a container that died. A line that is not an event
// is kept as it was, marked unparsed.
func summarizeDockerEvents(out []byte) string {
	var lines []string
	scanner := bufio.NewScanner(bytes.NewReader(out))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var event struct {
			Action   string
			TimeNano int64 `json:"timeNano"`
			Actor    struct {
				ID         string
				Attributes map[string]string
			}
		}
		if err := json.Unmarshal([]byte(line), &event); err != nil || event.Action == "" {
			lines = append(lines, "unparsed: "+line)
			continue
		}
		id := event.Actor.ID
		if len(id) > 12 {
			id = id[:12]
		}
		summary := time.Unix(0, event.TimeNano).UTC().Format("15:04:05.000") + " " + event.Action + " " + event.Actor.Attributes["name"] + " " + id
		if code, ok := event.Actor.Attributes["exitCode"]; ok {
			summary += " exitCode=" + code
		}
		// A network event names the container it connects or disconnects.
		if container := event.Actor.Attributes["container"]; container != "" {
			summary += " container=" + container[:min(12, len(container))]
		}
		lines = append(lines, summary)
	}
	if len(lines) == 0 {
		return "no events"
	}
	return strings.Join(lines, "; ")
}

func TestSummarizeDockerEvents(t *testing.T) {
	out := []byte(`{"status":"create","id":"0123456789abcdef0123","from":"alpine:3.20","Type":"container","Action":"create","Actor":{"ID":"0123456789abcdef0123","Attributes":{"image":"alpine:3.20","name":"sisyphus-a"}},"scope":"local","time":1792570000,"timeNano":1792570000123000000}
{"status":"start","id":"0123456789abcdef0123","from":"alpine:3.20","Type":"container","Action":"start","Actor":{"ID":"0123456789abcdef0123","Attributes":{"image":"alpine:3.20","name":"sisyphus-a"}},"scope":"local","time":1792570000,"timeNano":1792570000456000000}

{"status":"die","id":"0123456789abcdef0123","from":"alpine:3.20","Type":"container","Action":"die","Actor":{"ID":"0123456789abcdef0123","Attributes":{"execDuration":"0","exitCode":"137","image":"alpine:3.20","name":"sisyphus-a"}},"scope":"local","time":1792570001,"timeNano":1792570001789000000}
{"status":"destroy","id":"0123456789abcdef0123","from":"alpine:3.20","Type":"container","Action":"destroy","Actor":{"ID":"short","Attributes":{"image":"alpine:3.20","name":"sisyphus-a"}},"scope":"local","time":1792570002,"timeNano":1792570002000000000}
{"Type":"network","Action":"connect","Actor":{"ID":"fedcba9876543210fedc","Attributes":{"container":"0123456789abcdef0123","name":"bridge","type":"bridge"}},"scope":"local","time":1792570003,"timeNano":1792570003000000000}
Error response from daemon: context deadline exceeded
{"Type":"container"}
`)
	want := "08:06:40.123 create sisyphus-a 0123456789ab; " +
		"08:06:40.456 start sisyphus-a 0123456789ab; " +
		"08:06:41.789 die sisyphus-a 0123456789ab exitCode=137; " +
		"08:06:42.000 destroy sisyphus-a short; " +
		"08:06:43.000 connect bridge fedcba987654 container=0123456789ab; " +
		"unparsed: Error response from daemon: context deadline exceeded; " +
		`unparsed: {"Type":"container"}`
	if got := summarizeDockerEvents(out); got != want {
		t.Errorf("summary:\n%s\nwant:\n%s", got, want)
	}
	if got := summarizeDockerEvents(nil); got != "no events" {
		t.Errorf("no output: %q", got)
	}
	if got := dockerTimestamp(time.Unix(1792570000, 5)); got != "1792570000.000000005" {
		t.Errorf("timestamp %q", got)
	}
}

func TestDockerDebugIsCutAndRedacted(t *testing.T) {
	if got := string(lastBytes([]byte("abcdef"), 3)); got != "…def" {
		t.Errorf("cut to %q", got)
	}
	if got := string(lastBytes([]byte("abc"), 3)); got != "abc" {
		t.Errorf("kept %q", got)
	}
	log := `DEBU[0000] POST /v1.47/containers/create Authorization: Bearer abc.def
DEBU[0000] {"identitytoken":"xyz", "password": "hunter2", "auth": "ok"} X-Registry-Auth=eyJh token=t0k secret: s3cret`
	want := `DEBU[0000] POST /v1.47/containers/create Authorization: <redacted>
DEBU[0000] {"identitytoken":<redacted>, "password": <redacted>, "auth": "ok"} X-Registry-Auth=<redacted>`
	if got := redactDockerDebug([]byte(log)); got != want {
		t.Errorf("redacted:\n%s\nwant:\n%s", got, want)
	}

	// Put the start of the authorization header just before the retained tail.
	// Truncating first would strip "Authorization" and leak the remaining
	// bearer credential because the redactor could no longer recognize it.
	authorization := []byte("Authorization: Bearer sensitive-tail-token")
	longLog := append(bytes.Repeat([]byte("x"), dockerProbeKeeps-7), authorization...)
	if got := dockerDebugTail(longLog); strings.Contains(got, "sensitive-tail-token") {
		t.Errorf("truncated debug log leaked a bearer credential: %q", got)
	}
}

func TestTheDockerProbesAreBounded(t *testing.T) {
	requireDocker(t)
	began := time.Now()
	if summary, err := dockerNetworkEventsSince(time.Now().Add(-time.Second)); err != nil || summary == "" {
		t.Errorf("docker network events: %q, %v", summary, err)
	}
	if probe := dockerProbe(); !strings.HasPrefix(probe, "took ") {
		t.Errorf("probe: %q", probe)
	}
	if took := time.Since(began); took > dockerEventsWithin+dockerProbeWithin {
		t.Errorf("the probes took %s", took)
	}
}

func TestDockerEventsSinceIsBounded(t *testing.T) {
	requireDocker(t)
	began := time.Now()
	summary, err := dockerEventsSince(time.Now().Add(-time.Second))
	if err != nil || summary == "" {
		t.Errorf("docker events: %q, %v", summary, err)
	}
	if took := time.Since(began); took > dockerEventsWithin {
		t.Errorf("docker events took %s", took)
	}
}

// waitForContainerJob waits for the container integration test's job to finish.
// If its deadline expires, capture the engine and task logs before cancelling
// the job and stopping the worker, so the failed run shows whether Docker ever
// created the containers and where their task output stopped. Docker's own
// events since submitted show whether containers were created, started and
// exited, which --rm otherwise hides.
func waitForContainerJob(t *testing.T, within time.Duration, addr, id string, submitted time.Time) string {
	t.Helper()
	deadline := time.Now().Add(within)
	var last string
	var lastErr error
	for {
		callWithin := min(30*time.Second, time.Until(deadline))
		if callWithin <= 0 {
			callWithin = time.Nanosecond
		}
		last, lastErr = cliWithin(t, callWithin, "job", "get", "--addr", addr, id)
		if lastErr == nil && strings.Contains(last, "succeeded in") {
			return last
		}
		if time.Now().After(deadline) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			containers, dockerErr := exec.CommandContext(ctx, "docker", "ps", "-a", "--no-trunc", "--filter", "name=sisyphus-", "--format", "{{.ID}} {{.Names}} {{.Status}} {{.Image}}").CombinedOutput()
			cancel()
			ctx, cancel = context.WithTimeout(context.Background(), 5*time.Second)
			image, imageErr := exec.CommandContext(ctx, "docker", "image", "inspect", "--format", "{{.Id}}", containerTestImage).CombinedOutput()
			cancel()
			events, eventsErr := dockerEventsSince(submitted)
			networks, networksErr := dockerNetworkEventsSince(submitted)
			probe := dockerProbe()
			_, cancelErr := cliWithin(t, 5*time.Second, "job", "cancel", "--addr", addr, id)
			logs, logsErr := cliWithin(t, 5*time.Second, "job", "logs", "--addr", addr, id)
			t.Fatalf("job did not finish within %s; last job get %q (error %v); matching Docker containers %q (error %v); alpine:3.20 image %q (error %v); alpine:3.20 Docker events since submission %q (error %v); network events since submission %q (error %v); a probe container %s; cancel error %v; job logs %q (error %v)", within, last, lastErr, containers, dockerErr, image, imageErr, events, eventsErr, networks, networksErr, probe, cancelErr, logs, logsErr)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestAPoolRunsAContainerOnEachTask(t *testing.T) {
	requireDocker(t)
	addr := freeAddr(t)
	startDaemon(t, "--role", "coordinator", "--listen", addr)
	startDaemon(t, "--role", "worker", "--coordinator", addr, "--name", "plain", "--slots", "2")
	waitForOutput(t, "plain", "nodes", "--addr", addr)

	// With no worker that runs containers, such a job is taken in and waits.
	submitted := time.Now()
	params := `{"image":"` + containerTestImage + `","command":["sh","-c","echo I am task $SISYPHUS_TASK_INDEX of $SISYPHUS_TASK_COUNT; echo $SISYPHUS_TASK_INDEX > /output/index"]}`
	id := strings.TrimSpace(mustCLI(t, "job", "submit", "--addr", addr, "--detach", "--workload", "container", "--tasks", "3", "--params", params))
	if out := mustCLI(t, "job", "get", "--addr", addr, id); !strings.Contains(out, "pending") {
		t.Errorf("a container job with nobody to run it:\n%s", out)
	}
	if nodes := mustCLI(t, "nodes", "--addr", addr); strings.Contains(nodes, "container") {
		t.Errorf("a worker that was not told to offers containers:\n%s", nodes)
	}

	// A worker whose owner allows it takes the job, and holds its tasks in
	// as its owner asked: by the runtime named, with a root they cannot
	// write to.
	stopWorker := startDaemon(t, "--role", "worker", "--coordinator", addr, "--name", "docker-host", "--slots", "3", "--containers", "--container-runtime", "runc", "--containers-read-only", "--containers-tmp-mb", "64")
	// And the pool is told how it holds them in.
	waitForOutput(t, "docker-host runs containers with a read-only root and a sized /tmp", "nodes", "--addr", addr)
	// Three containers started at once are slow to start on a machine busy
	// with the rest of the tests: alone this takes seconds, but with the
	// whole suite running it has taken over twenty.
	out := waitForContainerJob(t, time.Minute, addr, id, submitted)
	if strings.Contains(out, "on plain") {
		t.Errorf("a container task ran on the worker that does not run them:\n%s", out)
	}
	var result runtime.ContainerResult
	if err := json.Unmarshal([]byte(out[strings.LastIndex(out, "\n{")+1:]), &result); err != nil || len(result.Tasks) != 3 {
		t.Fatalf("the job's result, in:\n%s\n%v", out, err)
	}
	// The task artifacts remain available from the coordinator, not just from
	// the workers whose local stores first held them.
	stopWorker()
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
	checkContainers = func(context.Context, runtime.Confinement) error {
		return errors.New("Docker cannot be used here: permission denied")
	}
	defer func() { checkContainers = old }()
	_, err := cli(t, "run", "--data-dir", t.TempDir(), "--listen", freeAddr(t), "--containers")
	if err == nil || !strings.Contains(err.Error(), "--containers: Docker cannot be used here") {
		t.Errorf("error %v, want the node not started", err)
	}
	// What it is told of how to run them reaches the check, and means
	// nothing on a node that runs none.
	var asked runtime.Confinement
	checkContainers = func(_ context.Context, confined runtime.Confinement) error {
		asked = confined
		return errors.New("Docker here has no runtime called runsc")
	}
	taskDir := t.TempDir()
	_, err = cli(t, "run", "--data-dir", t.TempDir(), "--listen", freeAddr(t), "--containers", "--task-dir", taskDir, "--container-runtime", "runsc", "--containers-read-only", "--containers-tmp-mb", "32", "--containers-network", "--containers-memory-mb", "512", "--containers-cpus", "1.5", "--containers-timeout", "1h")
	if err == nil || !strings.Contains(err.Error(), "--containers: Docker here has no runtime called runsc") || asked != (runtime.Confinement{TaskDir: taskDir, Runtime: "runsc", ReadOnly: true, TmpMB: 32, Network: true, MemoryMB: 512, CPUs: 1.5, Timeout: time.Hour}) {
		t.Errorf("error %v, asked for %+v, want the node not started for want of the runtime", err, asked)
	}
	workingDir, err := filepath.Abs(".")
	if err != nil {
		t.Fatal(err)
	}
	relativeTaskDir, err := filepath.Rel(workingDir, taskDir)
	if err != nil {
		t.Fatal(err)
	}
	_, err = cli(t, "run", "--data-dir", t.TempDir(), "--listen", freeAddr(t), "--containers", "--task-dir", relativeTaskDir)
	if err == nil || !strings.Contains(err.Error(), "Docker here has no runtime called runsc") || asked.TaskDir != taskDir {
		t.Errorf("relative --task-dir passed to worker as %q; want absolute %q (error %v)", asked.TaskDir, taskDir, err)
	}
	for _, flags := range [][]string{{"--task-dir", t.TempDir()}, {"--container-runtime", "runsc"}, {"--containers-read-only"}, {"--containers-memory-mb", "512"}, {"--containers-cpus", "2"}, {"--containers-timeout", "1h"}, {"--containers-tmp-mb", "64"}, {"--containers-network"}} {
		if _, err := cli(t, append([]string{"run", "--data-dir", t.TempDir(), "--listen", freeAddr(t)}, flags...)...); err == nil || !strings.Contains(err.Error(), "need --containers") {
			t.Errorf("%v without --containers: %v", flags, err)
		}
	}
	missing := filepath.Join(t.TempDir(), "missing")
	file := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{missing, file} {
		if _, err := cli(t, "run", "--data-dir", t.TempDir(), "--listen", freeAddr(t), "--containers", "--task-dir", path); err == nil || !strings.Contains(err.Error(), "--task-dir must name an existing") {
			t.Errorf("--task-dir %q: %v", path, err)
		}
	}
	// What a task may use is at most something.
	for _, flags := range [][]string{{"--containers-memory-mb", "-1"}, {"--containers-cpus", "-0.5"}, {"--containers-timeout", "-1s"}, {"--containers-tmp-mb", "-1"}} {
		if _, err := cli(t, append([]string{"run", "--data-dir", t.TempDir(), "--listen", freeAddr(t), "--containers"}, flags...)...); err == nil || !strings.Contains(err.Error(), "cannot be less than nothing") {
			t.Errorf("%v: %v", flags, err)
		}
	}
	for _, timeout := range []string{"-1s", "-1m"} {
		if _, err := cli(t, "run", "--data-dir", t.TempDir(), "--listen", freeAddr(t), "--max-task-timeout", timeout); err == nil || !strings.Contains(err.Error(), "--max-task-timeout cannot be negative") {
			t.Errorf("--max-task-timeout %s: %v", timeout, err)
		}
	}
	// A sized /tmp means something only where the root cannot be written.
	if _, err := cli(t, "run", "--data-dir", t.TempDir(), "--listen", freeAddr(t), "--containers", "--containers-tmp-mb", "64"); err == nil || !strings.Contains(err.Error(), "needs it") {
		t.Errorf("--containers-tmp-mb without --containers-read-only: %v", err)
	}
}

func TestCheckTaskDirReportsAnUnwritableVolume(t *testing.T) {
	dir := t.TempDir()
	want := errors.New("no space")
	err := checkTaskDirWith(dir, os.Stat, func(string, string) (*os.File, error) {
		return nil, want
	})
	if !errors.Is(err, want) || !strings.Contains(err.Error(), "existing, writable directory") {
		t.Errorf("checkTaskDir error = %v", err)
	}
}

func TestResolveTaskDirReportsAnUnresolvablePath(t *testing.T) {
	want := errors.New("working directory is unavailable")
	_, err := resolveTaskDir("tasks", func(string) (string, error) {
		return "", want
	})
	if !errors.Is(err, want) || !strings.Contains(err.Error(), "resolve --task-dir") {
		t.Errorf("resolveTaskDir error = %v", err)
	}
}

func TestDaemonReportsAnUnresolvableTaskDir(t *testing.T) {
	old := absoluteTaskDir
	absoluteTaskDir = func(string) (string, error) {
		return "", errors.New("working directory is unavailable")
	}
	defer func() { absoluteTaskDir = old }()

	if _, err := cli(t, "run", "--data-dir", t.TempDir(), "--listen", freeAddr(t), "--containers", "--task-dir", "tasks"); err == nil || !strings.Contains(err.Error(), "resolve --task-dir: working directory is unavailable") {
		t.Errorf("unresolvable --task-dir error = %v", err)
	}
}

// What a worker's owner allows of the pool's containers is said beside the
// table of nodes, so that whoever runs the pool can see which take what.
func TestTheNodesSayHowEachRunsContainers(t *testing.T) {
	for labels, want := range map[string]string{
		"":                   "",
		runtime.SealingLabel: "",
		runtime.NetworkLabel: "laptop runs containers with the network allowed",
		runtime.SandboxedLabel + " " + runtime.BoundedTmpLabel + " " + runtime.NetworkLabel: "laptop runs containers with the network allowed, sandboxed, with a read-only root and a sized /tmp",
	} {
		if got := containersOf("laptop", strings.Fields(labels)); got != want {
			t.Errorf("%q: %q, want %q", labels, got, want)
		}
	}
}

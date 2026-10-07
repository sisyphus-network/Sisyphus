package runtime

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/ipfs/go-cid"
)

// Container is the workload that runs anything: a container image, with a
// command, once on each of however many tasks the job is split into. Each
// task is told which one it is, and that is all the splitting there is: a
// program that wants to do a share of some larger work takes its share from
// its index.
//
// It is how real programs run on a pool without being built into the
// daemon, and it is also, on any worker that offers it, leave for whoever
// may submit jobs to run what they like there. A worker offers it only if
// its owner says so.
type Container struct {
	// Engine runs a container engine's command to its end, giving what it
	// prints to stdout and stderr, and returns the status it exited with.
	// Nil means Docker.
	Engine func(ctx context.Context, args []string, stdout, stderr io.Writer) (status int, err error)
}

// ContainerParams are a container job's parameters.
type ContainerParams struct {
	// Image is the image to run, and Command what to run in it. An empty
	// command runs whatever the image runs by default.
	Image   string   `json:"image"`
	Command []string `json:"command,omitempty"`
	// Tasks is how many copies to run. Zero leaves it to the pool.
	Tasks int `json:"tasks,omitempty"`
	// Input, if set, is the CID of a stored file every task is given, at
	// /input/data.
	Input string `json:"input,omitempty"`
	// Env is extra environment for the command.
	Env map[string]string `json:"env,omitempty"`
	// MemoryMB and CPUs, if not zero, are the most each task may use.
	MemoryMB int     `json:"memory_mb,omitempty"`
	CPUs     float64 `json:"cpus,omitempty"`
	// Network lets the task reach the network. Without it, it cannot.
	Network bool `json:"network,omitempty"`
}

// containerTask is one task's payload: the job's parameters, and which of
// how many tasks this is.
type containerTask struct {
	ContainerParams
	Index int `json:"index"`
	Count int `json:"count"`
}

// ContainerOutput is what one task of a container job produced.
type ContainerOutput struct {
	Index int `json:"index"`
	// Stdout is the CID of what the command printed, and Files the CIDs of
	// the files it left in /output, by name.
	Stdout string            `json:"stdout"`
	Files  map[string]string `json:"files,omitempty"`
}

// ContainerResult is a container job's result: its tasks' outputs, in order.
type ContainerResult struct {
	Tasks []ContainerOutput `json:"tasks"`
}

func (Container) Name() string { return "container" }

func (Container) Describe() string {
	return `Runs a container image, once for each task, with no network unless asked. Parameters: {"image": "<image>", "command": ["<program>", "<argument>", ...], "tasks": <how many copies>, "input": "<optional CID of a stored file, given to each task at /input/data>", "network": <true to allow network access>}. Each task has SISYPHUS_TASK_INDEX (from 0) and SISYPHUS_TASK_COUNT in its environment. Result: {"tasks": [{"index": <n>, "stdout": "<CID of what it printed>", "files": {"<name>": "<CID of a file it wrote to /output>"}}]}.`
}

func (Container) Split(_ context.Context, _ Blobs, params []byte, parts int) ([][]byte, error) {
	var p ContainerParams
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, fmt.Errorf("container parameters: %w", err)
	}
	if p.Image == "" {
		return nil, errors.New("container parameters: no image is named")
	}
	if p.Input != "" {
		if _, err := cid.Decode(p.Input); err != nil {
			return nil, fmt.Errorf("container parameters: input is not a CID: %w", err)
		}
	}
	count := parts
	if p.Tasks > 0 {
		count = p.Tasks
	}
	payloads := make([][]byte, count)
	for i := range payloads {
		payloads[i] = mustJSON(containerTask{ContainerParams: p, Index: i, Count: count})
	}
	return payloads, nil
}

// maxStdout is how much of what a task prints is kept.
const maxStdout = 16 << 20

func (c Container) Execute(ctx context.Context, blobs Blobs, payload []byte) ([]byte, error) {
	var task containerTask
	if err := json.Unmarshal(payload, &task); err != nil {
		return nil, fmt.Errorf("container payload: %w", err)
	}
	engine := c.Engine
	if engine == nil {
		engine = docker
	}
	// The task's files live in a directory of its own for as long as it runs.
	dir, err := os.MkdirTemp("", "sisyphus-task-")
	if err != nil {
		return nil, fmt.Errorf("make the task's directory: %w", err)
	}
	defer os.RemoveAll(dir)
	input, output := filepath.Join(dir, "input"), filepath.Join(dir, "output")
	os.Mkdir(input, 0o755)  // in a directory just made, these cannot fail
	os.Mkdir(output, 0o777) // the command may run as anyone
	os.Chmod(output, 0o777)
	if task.Input != "" {
		if err := fetchTo(ctx, blobs, task.Input, filepath.Join(input, "data")); err != nil {
			return nil, err
		}
	}

	var suffix [6]byte
	rand.Read(suffix[:]) // never fails; see crypto/rand
	name := "sisyphus-" + hex.EncodeToString(suffix[:])
	args := []string{"run", "--rm", "--name", name,
		"--volume", input + ":/input:ro", "--volume", output + ":/output",
		"--env", "SISYPHUS_TASK_INDEX=" + strconv.Itoa(task.Index), "--env", "SISYPHUS_TASK_COUNT=" + strconv.Itoa(task.Count),
	}
	// The command runs as whoever runs this node, so that what it leaves
	// behind is this node's to read and to clear away. Where there is no
	// such thing as a user number, it runs as the image has it.
	if uid := os.Getuid(); uid >= 0 {
		args = append(args, "--user", strconv.Itoa(uid)+":"+strconv.Itoa(os.Getgid()))
	}
	if !task.Network {
		args = append(args, "--network", "none")
	}
	if task.MemoryMB > 0 {
		args = append(args, "--memory", strconv.Itoa(task.MemoryMB)+"m")
	}
	if task.CPUs > 0 {
		args = append(args, "--cpus", strconv.FormatFloat(task.CPUs, 'f', -1, 64))
	}
	for _, key := range sortedKeys(task.Env) {
		args = append(args, "--env", key+"="+task.Env[key])
	}
	args = append(append(args, task.Image), task.Command...)

	// What the command prints is the task's log as it goes, and stdout is
	// also kept as its output.
	report := Report(ctx)
	report.Log("running " + task.Image + " " + strings.Join(task.Command, " "))
	var printed capped
	stdout := &lineWriter{each: report.Log, also: &printed}
	stderr := &lineWriter{each: report.Log}
	status, err := engine(ctx, args, stdout, stderr)
	stdout.flush()
	stderr.flush()
	if ctx.Err() != nil {
		// Stopping the engine's command does not stop the container.
		engine(context.WithoutCancel(ctx), []string{"kill", name}, io.Discard, io.Discard)
		return nil, ctx.Err()
	}
	if err != nil {
		return nil, fmt.Errorf("run the container: %w", err)
	}
	if status != 0 {
		return nil, fmt.Errorf("the command exited with status %d: %s", status, stderr.last)
	}

	out := ContainerOutput{Index: task.Index}
	stored, err := blobs.Put(ctx, bytes.NewReader(printed.Bytes()))
	if err != nil {
		return nil, fmt.Errorf("store what the command printed: %w", err)
	}
	out.Stdout = stored.String()
	left, _ := os.ReadDir(output) // the directory is this task's own
	for _, entry := range left {
		if !entry.Type().IsRegular() {
			continue
		}
		file, err := os.Open(filepath.Join(output, entry.Name()))
		if err != nil {
			return nil, fmt.Errorf("read the task's output %s: %w", entry.Name(), err)
		}
		stored, err := blobs.Put(ctx, file)
		file.Close()
		if err != nil {
			return nil, fmt.Errorf("store the task's output %s: %w", entry.Name(), err)
		}
		if out.Files == nil {
			out.Files = make(map[string]string)
		}
		out.Files[entry.Name()] = stored.String()
	}
	return mustJSON(out), nil
}

func (Container) Aggregate(_ context.Context, _ Blobs, outputs [][]byte) ([]byte, error) {
	result := ContainerResult{Tasks: make([]ContainerOutput, len(outputs))}
	for i, output := range outputs {
		if err := json.Unmarshal(output, &result.Tasks[i]); err != nil {
			return nil, fmt.Errorf("task %d output: %w", i, err)
		}
	}
	return mustJSON(result), nil
}

// CheckContainers reports, as an error, a machine on which container tasks
// cannot be run: one where Docker is not installed, not running, or not
// this user's to use.
func CheckContainers(ctx context.Context) error {
	var said bytes.Buffer
	status, err := docker(ctx, []string{"version", "--format", "{{.Server.Version}}"}, io.Discard, &said)
	if err == nil && status != 0 {
		err = errors.New(strings.TrimSpace(said.String()))
	}
	if err != nil {
		return fmt.Errorf("Docker cannot be used here: %w", err)
	}
	return nil
}

// docker runs the docker command.
func docker(ctx context.Context, args []string, stdout, stderr io.Writer) (int, error) {
	cmd := exec.CommandContext(ctx, "docker", args...)
	cmd.Stdout, cmd.Stderr = stdout, stderr
	err := cmd.Run()
	var exited *exec.ExitError
	if errors.As(err, &exited) {
		return exited.ExitCode(), nil
	}
	return 0, err
}

// fetchTo writes the stored blob with the given CID to a file.
func fetchTo(ctx context.Context, blobs Blobs, id, path string) error {
	c, _ := cid.Decode(id) // checked when the job was split
	blob, err := blobs.Open(ctx, c)
	if err != nil {
		return fmt.Errorf("container input %s: %w", id, err)
	}
	defer blob.Close()
	// In a directory just made for the task this cannot fail, and if it
	// somehow did the copy below would say so.
	file, _ := os.Create(path)
	defer file.Close()
	if _, err := io.Copy(file, blob); err != nil {
		return fmt.Errorf("container input %s: %w", id, err)
	}
	return nil
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// capped is a buffer that keeps the first maxStdout bytes it is given.
type capped struct{ bytes.Buffer }

func (c *capped) Write(p []byte) (int, error) {
	if room := maxStdout - c.Len(); room < len(p) {
		c.Buffer.Write(p[:max(room, 0)])
		return len(p), nil
	}
	return c.Buffer.Write(p)
}

// lineWriter passes what is written to it on a line at a time, and to
// another writer as it comes.
type lineWriter struct {
	each func(line string)
	also io.Writer
	part []byte
	// last is the last line passed on.
	last string
}

func (w *lineWriter) Write(p []byte) (int, error) {
	if w.also != nil {
		w.also.Write(p)
	}
	w.part = append(w.part, p...)
	for {
		i := bytes.IndexByte(w.part, '\n')
		if i < 0 {
			return len(p), nil
		}
		w.line(string(w.part[:i]))
		w.part = w.part[i+1:]
	}
}

// flush passes on a last line that had no end.
func (w *lineWriter) flush() {
	if len(w.part) > 0 {
		w.line(string(w.part))
		w.part = nil
	}
}

func (w *lineWriter) line(line string) {
	w.last = strings.TrimRight(line, "\r")
	w.each(w.last)
}

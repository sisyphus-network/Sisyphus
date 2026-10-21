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
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

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
	Engine      func(ctx context.Context, args []string, stdout, stderr io.Writer) (status int, err error)
	prepareDirs func(input, output string) error
	// MaxOutput is how many bytes of files a task may leave in /output to
	// be stored. Zero means maxOutput.
	MaxOutput int64
	// Confined is how much further than the worker's usual the containers
	// are held in.
	Confined Confinement
}

// Confinement is what a worker's owner asks of the containers that run the
// pool's tasks, beyond what every one of them is held to.
type Confinement struct {
	// TaskDir is an existing directory under which task workspaces and
	// staged inputs are created. Operators can put it on a dedicated,
	// quota-backed filesystem to contain disk exhaustion. Empty uses the
	// operating system's temporary directory.
	TaskDir string
	// Runtime is the runtime the engine is to run them with, by the name
	// the engine knows it under, in place of its usual: a sandboxed one
	// such as gVisor's runsc, which stands between the task and the
	// machine's kernel, or Kata's, which gives each a small machine of its
	// own. Empty is the engine's usual.
	Runtime string
	// ReadOnly gives a task nowhere to write but /output and /tmp, which
	// is then held in memory: nothing of the image can be changed, and
	// nothing written anywhere else.
	ReadOnly bool
	// TmpMB, if not zero, is how many megabytes /tmp may hold where the
	// root is read-only. Without it /tmp may grow until the task runs out
	// of memory, or the machine does where the task's memory has no limit.
	TmpMB int
	// Network lets a task reach the network when its job asks to. Without
	// it no task can, whatever its job asks: whoever submits a job does
	// not decide what this machine's network is used for.
	Network bool
	// MemoryMB and CPUs, if not zero, are the most memory and processors a
	// task may use. A task whose job asks for less is held to what it asks;
	// one whose job asks for more, or for no limit, is held to these.
	MemoryMB int
	CPUs     float64
	// Timeout, if not zero, is the longest a task may run before the worker
	// stops it, whatever its job allows: a coordinator's timeout is the
	// job's to set, and this one is the owner's.
	Timeout time.Duration
}

// heldTo is what a task asking for asked of something is given where its
// worker's owner allows at most most, zero for either being no limit.
func heldTo[N int | float64](asked, most N) N {
	if most > 0 && (asked <= 0 || asked > most) {
		return most
	}
	return asked
}

// NetworkLabel marks a worker whose owner lets the pool's containers reach
// the network. A container job that asks for the network is given only to
// workers that have it.
const NetworkLabel = "container-network"

// BoundedTmpLabel marks a worker whose containers have a read-only root and
// a sized /tmp. It does not bound writes to /output while a task is running.
// SandboxedLabel marks one that runs them with a runtime that stands
// between a task and the machine's kernel, gVisor's or Kata's, which the
// worker found Docker has before it started.
const (
	BoundedTmpLabel = "container-bounded-tmp"
	SandboxedLabel  = "container-sandboxed"
)

// HardenedLabels are what a coordinator that hands container work only to
// hardened workers needs of each.
var HardenedLabels = []string{BoundedTmpLabel, SandboxedLabel}

// sandboxed reports whether the runtime asked for is one that sandboxes:
// gVisor's runsc, under any of the names it is installed as, or Kata's.
func (c Confinement) sandboxed() bool {
	return strings.HasPrefix(c.Runtime, "runsc") || strings.HasPrefix(c.Runtime, "io.containerd.runsc.") || strings.Contains(c.Runtime, "kata")
}

// args is what Confinement adds to the engine's command.
func (c Confinement) args() []string {
	var args []string
	if c.Runtime != "" {
		args = append(args, "--runtime", c.Runtime)
	}
	if c.ReadOnly {
		tmp := "/tmp"
		if c.TmpMB > 0 {
			tmp += ":size=" + strconv.Itoa(c.TmpMB) + "m"
		}
		args = append(args, "--read-only", "--tmpfs", tmp)
	}
	return args
}

// What a task leaves in /output is stored in the pool and kept by its
// coordinator, so there is only so much of it a task may leave: maxOutput
// bytes unless the worker's owner says otherwise, in maxOutputFiles files.
const (
	maxOutput      = 4 << 30
	maxOutputFiles = 1024
)

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
	// Inputs, if set, are more stored files every task is given, by name:
	// each at /input/<name>, beside Input's /input/data. A name is made
	// only of lower-case letters, digits, dots, dashes and underscores,
	// and is neither "data", ".", nor "..": it is a file's name in /input
	// and nothing else.
	Inputs map[string]string `json:"inputs,omitempty"`
	// Env is extra environment for the command.
	Env map[string]string `json:"env,omitempty"`
	// MemoryMB and CPUs, if not zero, are the most each task may use.
	MemoryMB int     `json:"memory_mb,omitempty"`
	CPUs     float64 `json:"cpus,omitempty"`
	// GPUs, if not zero, is how many of the machine's graphics cards each
	// task is given. The job should also ask for workers that have them.
	GPUs int `json:"gpus,omitempty"`
	// Network lets the task reach the network. Without it, it cannot.
	Network bool `json:"network,omitempty"`
}

// containerTask is one task's payload: the job's parameters, and which of
// how many tasks this is.
type containerTask struct {
	ContainerParams
	// Job is the same in every task of a job and in no other job's. Tasks
	// of a job that run on one machine share one copy of its input.
	Job   string `json:"job"`
	Index int    `json:"index"`
	Count int    `json:"count"`
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

// Needs says that a job that asks for the network needs a worker whose
// owner allows it.
func (Container) Needs(params []byte) []string {
	var p ContainerParams
	if json.Unmarshal(params, &p) != nil || !p.Network {
		return nil
	}
	return []string{NetworkLabel}
}

// GPUsNeeded is the graphics cards each task asks for.
func (Container) GPUsNeeded(params []byte) int {
	var p ContainerParams
	if json.Unmarshal(params, &p) != nil {
		return 0
	}
	return max(p.GPUs, 0)
}

// Offers is the network, where the worker's owner allows it.
func (c Container) Offers(context.Context) []string {
	var offers []string
	if c.Confined.Network {
		offers = append(offers, NetworkLabel)
	}
	if c.Confined.ReadOnly && c.Confined.TmpMB > 0 {
		offers = append(offers, BoundedTmpLabel)
	}
	if c.Confined.sandboxed() {
		offers = append(offers, SandboxedLabel)
	}
	return offers
}

// Varies says that Container results can differ between runs.
func (Container) Varies() bool { return true }

func (Container) Name() string { return "container" }

func (Container) Describe() string {
	return `Runs a container image, once for each task, with no network unless asked. Parameters: {"image": "<image>", "command": ["<program>", "<argument>", ...], "tasks": <how many copies, at most 10000>, "input": "<optional CID of a stored file, given to each task at /input/data>", "inputs": {"<name>": "<CID of a stored file, given to each task at /input/<name>>"}, "network": <true to allow network access, which only workers whose owner allows it give>, "gpus": <how many graphics cards each task needs, if any>}. Each task has SISYPHUS_TASK_INDEX (from 0) and SISYPHUS_TASK_COUNT in its environment. Result: {"tasks": [{"index": <n>, "stdout": "<CID of what it printed>", "files": {"<name>": "<CID of a file it wrote to /output>"}}]}.`
}

func (Container) Split(ctx context.Context, blobs Blobs, params []byte, parts int) ([][]byte, error) {
	var p ContainerParams
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, fmt.Errorf("container parameters: %w", err)
	}
	if err := checkImage(p.Image); err != nil {
		return nil, fmt.Errorf("container parameters: %w", err)
	}
	// The count is the client's, and a payload is made for each copy before
	// any other limit is checked.
	if p.Tasks > MaxTasks {
		return nil, fmt.Errorf("container parameters: tasks is %d; a job can have at most %d", p.Tasks, MaxTasks)
	}
	if err := checkInputs(p.Inputs); err != nil {
		return nil, fmt.Errorf("container parameters: %w", err)
	}
	for _, id := range taskFiles(p) {
		input, err := cid.Decode(id)
		if err != nil {
			return nil, fmt.Errorf("container parameters: input is not a CID: %w", err)
		}
		// Opening it is what tells the coordinator the job reads it, and so
		// keeps it while the job runs: only tasks read it after this.
		blob, err := blobs.Open(ctx, input)
		if err != nil {
			return nil, fmt.Errorf("container input %s: %w", input, err)
		}
		blob.Close()
	}
	count := parts
	if p.Tasks > 0 {
		count = p.Tasks
	}
	var job [12]byte
	rand.Read(job[:]) // never fails; see crypto/rand
	payloads := make([][]byte, count)
	for i := range payloads {
		payloads[i] = mustJSON(containerTask{ContainerParams: p, Job: hex.EncodeToString(job[:]), Index: i, Count: count})
	}
	return payloads, nil
}

// checkImage refuses what cannot be the name of an image. The name is
// handed to the engine among its options, so one that began with a dash
// would be read as an option: a mount of the worker's disk, say, or leave
// to run privileged. No image's name begins with one, or has a space or a
// control character in it.
func checkImage(image string) error {
	switch {
	case image == "":
		return errors.New("no image is named")
	case strings.HasPrefix(image, "-"):
		return fmt.Errorf("%q is not the name of an image: it begins with a dash", image)
	case strings.ContainsFunc(image, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }):
		return fmt.Errorf("%q is not the name of an image: it has a space or a control character in it", image)
	}
	return nil
}

// maxInputs is how many named inputs a job may give its tasks.
const maxInputs = 16

// checkInputs refuses named inputs whose names could be anything but a
// file's name in /input, or are not CIDs. The names come from whoever
// submitted the job, and each is joined to a directory of the worker's to
// write the input to: a slash, or a name that is only dots, would have it
// written somewhere else. "data" is Input's.
func checkInputs(named map[string]string) error {
	if len(named) > maxInputs {
		return fmt.Errorf("%d inputs are named; a job can name at most %d", len(named), maxInputs)
	}
	for name, id := range named {
		if !inputName(name) {
			return fmt.Errorf("%q cannot name an input: a name is 1 to 64 lower-case letters, digits, dots, dashes and underscores, and not data, . or ..", name)
		}
		if _, err := cid.Decode(id); err != nil {
			return fmt.Errorf("input %s is not a CID: %w", name, err)
		}
	}
	return nil
}

// inputName reports whether name may name an input.
func inputName(name string) bool {
	if name == "" || len(name) > 64 || name == "data" || name == "." || name == ".." {
		return false
	}
	return !strings.ContainsFunc(name, func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '.' || r == '-' || r == '_')
	})
}

// taskFiles are the files a task is given in /input, by name, as the CIDs
// of what they hold.
func taskFiles(p ContainerParams) map[string]string {
	files := make(map[string]string, len(p.Inputs)+1)
	for name, id := range p.Inputs {
		files[name] = id
	}
	if p.Input != "" {
		files["data"] = p.Input
	}
	return files
}

// maxStdout is how much of what a task prints is kept.
const maxStdout = 16 << 20

func (c Container) Execute(ctx context.Context, blobs Blobs, payload []byte) ([]byte, error) {
	var task containerTask
	if err := json.Unmarshal(payload, &task); err != nil {
		return nil, fmt.Errorf("container payload: %w", err)
	}
	// The worker checks for itself: what it is handed came over the
	// network, and the image is put among the engine's own arguments.
	if err := checkImage(task.Image); err != nil {
		return nil, fmt.Errorf("container payload: %w", err)
	}
	// It checks the names of its inputs too, each of which is joined to a
	// directory of its own.
	if err := checkInputs(task.Inputs); err != nil {
		return nil, fmt.Errorf("container payload: %w", err)
	}
	// A coordinator gives a job that asks for the network only to workers
	// that offer it, but this worker does not rely on that.
	if task.Network && !c.Confined.Network {
		return nil, errors.New("the job asks for the network, and this worker's owner does not let the pool's containers reach it")
	}
	engine := c.Engine
	if engine == nil {
		engine = docker
	}
	// The task's files live in a directory of its own for as long as it runs.
	dir, err := os.MkdirTemp(c.Confined.TaskDir, "sisyphus-task-")
	if err != nil {
		return nil, fmt.Errorf("make the task's directory: %w", err)
	}
	defer os.RemoveAll(dir)
	input, output := filepath.Join(dir, "input"), filepath.Join(dir, "output")
	if err := c.prepareTaskDirs(input, output); err != nil {
		return nil, err
	}
	if files := taskFiles(task.ContainerParams); len(files) > 0 {
		shared, release, err := inputs.take(ctx, blobs, c.Confined.TaskDir, task.Job, files)
		if err != nil {
			return nil, err
		}
		defer release()
		input = shared
	}

	var suffix [6]byte
	rand.Read(suffix[:]) // never fails; see crypto/rand
	name := "sisyphus-" + hex.EncodeToString(suffix[:])
	// The command is somebody else's program. It can gain no privilege it
	// did not start with, whatever the image has in it that would give one,
	// and can start only so many processes.
	args := []string{"run", "--rm", "--name", name,
		"--security-opt", "no-new-privileges", "--pids-limit", "4096",
		"--volume", input + ":/input:ro", "--volume", output + ":/output",
		"--env", "SISYPHUS_TASK_INDEX=" + strconv.Itoa(task.Index), "--env", "SISYPHUS_TASK_COUNT=" + strconv.Itoa(task.Count),
	}
	// The command runs as whoever runs this node, so that what it leaves
	// behind is this node's to read and to clear away, and with none of
	// the capabilities Docker gives a container, which that user could not
	// use anyway. Where there is no such thing as a user number, it runs
	// as the image has it, which may be as root inside the container, and
	// keeps Docker's usual capabilities: an image that installs what it
	// needs as root would not run without them.
	//
	// That user has no home in the image, and a home that cannot be written
	// to stops much that a command may do first, installing a package for
	// itself among it. So its home is /tmp, which it can write to, unless
	// the job's environment gives another.
	if uid := os.Getuid(); uid >= 0 {
		args = append(args, "--user", strconv.Itoa(uid)+":"+strconv.Itoa(os.Getgid()), "--cap-drop", "ALL", "--env", "HOME=/tmp")
	}
	args = append(args, c.Confined.args()...)
	if !task.Network {
		args = append(args, "--network", "none")
	}
	// The job may ask for less than the worker's owner allows, never more.
	if memory := heldTo(task.MemoryMB, c.Confined.MemoryMB); memory > 0 {
		args = append(args, "--memory", strconv.Itoa(memory)+"m")
	}
	if cpus := heldTo(task.CPUs, c.Confined.CPUs); cpus > 0 {
		args = append(args, "--cpus", strconv.FormatFloat(cpus, 'f', -1, 64))
	}
	if task.GPUs > 0 {
		args = append(args, "--gpus", strconv.Itoa(task.GPUs))
	}
	for _, key := range sortedKeys(task.Env) {
		args = append(args, "--env", key+"="+task.Env[key])
	}
	args = append(append(args, task.Image), task.Command...)

	// What the command prints is the task's log as it goes, and stdout is
	// also kept as its output.
	report := Report(ctx)
	report.Log("running " + task.Image + " " + firstLine(strings.Join(task.Command, " ")))
	var printed capped
	stdout := &lineWriter{each: report.Log, also: &printed}
	stderr := &lineWriter{each: report.Log}
	run := ctx
	if c.Confined.Timeout > 0 {
		var cancel context.CancelFunc
		run, cancel = context.WithTimeout(ctx, c.Confined.Timeout)
		defer cancel()
	}
	status, err := engine(run, args, stdout, stderr)
	stdout.flush()
	stderr.flush()
	if run.Err() != nil {
		// Stopping the engine's command does not stop the container.
		engine(context.WithoutCancel(ctx), []string{"kill", name}, io.Discard, io.Discard)
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("the task ran for longer than the %s this worker's owner allows a task, and was stopped", c.Confined.Timeout)
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
	limit := c.MaxOutput
	if limit == 0 {
		limit = maxOutput
	}
	room := limit
	left, _ := os.ReadDir(output) // the directory is this task's own
	for _, entry := range left {
		if !entry.Type().IsRegular() {
			continue
		}
		if len(out.Files) == maxOutputFiles {
			return nil, fmt.Errorf("the task left more than %d files in /output", maxOutputFiles)
		}
		file, err := os.Open(filepath.Join(output, entry.Name()))
		if err != nil {
			return nil, fmt.Errorf("read the task's output %s: %w", entry.Name(), err)
		}
		// One byte more than there is room for is read, so that a file
		// too long is known as such however long it is.
		within := &io.LimitedReader{R: file, N: room + 1}
		stored, err := blobs.Put(ctx, within)
		file.Close()
		if err != nil {
			return nil, fmt.Errorf("store the task's output %s: %w", entry.Name(), err)
		}
		if within.N == 0 {
			return nil, fmt.Errorf("the task left more in /output than the %d bytes a task may: %s", limit, entry.Name())
		}
		room = within.N - 1
		if out.Files == nil {
			out.Files = make(map[string]string)
		}
		out.Files[entry.Name()] = stored.String()
	}
	return mustJSON(out), nil
}

func (c Container) prepareTaskDirs(input, output string) error {
	if c.prepareDirs != nil {
		return c.prepareDirs(input, output)
	}
	return makeTaskDirs(input, output, os.Mkdir, os.Chmod)
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

// ResultBlobs are the task outputs the container result refers to by CID.
func (Container) ResultBlobs(raw []byte) []string {
	var result ContainerResult
	if json.Unmarshal(raw, &result) != nil {
		return nil
	}
	var cids []string
	for _, task := range result.Tasks {
		if task.Stdout != "" {
			cids = append(cids, task.Stdout)
		}
		for _, c := range task.Files {
			if c != "" {
				cids = append(cids, c)
			}
		}
	}
	return cids
}

// CheckContainers reports, as an error, a machine on which container tasks
// cannot be run: one where Docker is not installed, not running, or not
// this user's to use.
func CheckContainers(ctx context.Context, confined Confinement) error {
	var said bytes.Buffer
	status, err := docker(ctx, []string{"version", "--format", "{{.Server.Version}}"}, io.Discard, &said)
	if err == nil && status != 0 {
		err = errors.New(strings.TrimSpace(said.String()))
	}
	if err != nil {
		return fmt.Errorf("Docker cannot be used here: %w", err)
	}
	if confined.Runtime == "" {
		return nil
	}
	// A runtime is named to the engine as an argument, and must not be
	// read as one of its options.
	if strings.ContainsFunc(confined.Runtime, func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '.' || r == '-' || r == '_')
	}) || strings.HasPrefix(confined.Runtime, "-") {
		return fmt.Errorf("%q is not the name of a container runtime", confined.Runtime)
	}
	var listed bytes.Buffer
	if status, err := docker(ctx, []string{"info", "--format", "{{range $name, $_ := .Runtimes}}{{$name}} {{end}}"}, &listed, io.Discard); err != nil || status != 0 {
		return fmt.Errorf("Docker did not say which runtimes it has, so %s cannot be asked for", confined.Runtime)
	}
	has := strings.Fields(listed.String())
	if !slices.Contains(has, confined.Runtime) {
		return fmt.Errorf("Docker here has no runtime called %s: it has %s. A sandboxed runtime is installed and made known to Docker before a node can use it", confined.Runtime, strings.Join(has, ", "))
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

// firstLine returns text up to its first line break, with a mark that
// there was more if there was.
func firstLine(text string) string {
	line, _, more := strings.Cut(text, "\n")
	if more {
		line += " ..."
	}
	return line
}

// inputs holds the inputs of the container tasks running on this machine.
var inputs = sharedInputs{held: make(map[string]*sharedInput)}

// sharedInputs keeps one copy of a job's input for all of the job's tasks
// that are running here, for as long as any is. A large input is then
// fetched and written once however many tasks read it. Tasks of different
// jobs share nothing, though they name the same input: a private job's
// input is unsealed, and is not another job's to read.
type sharedInputs struct {
	mu   sync.Mutex
	held map[string]*sharedInput
}

type sharedInput struct {
	users int
	// fetching is held by the task that is fetching the input; dir is
	// where it is, once it has been.
	fetching sync.Mutex
	dir      string
}

// take returns a directory holding the files given, each under its name
// with what the CID it is given holds, and what to call when the task is
// done with it. The names are checked before: none has a slash in it or
// is only dots.
func (s *sharedInputs) take(ctx context.Context, blobs Blobs, base, job string, files map[string]string) (dir string, release func(), err error) {
	key := job
	for _, name := range sortedKeys(files) {
		key += "/" + name + "=" + files[name]
	}
	s.mu.Lock()
	in := s.held[key]
	if in == nil {
		in = new(sharedInput)
		s.held[key] = in
	}
	in.users++
	s.mu.Unlock()
	release = func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		if in.users--; in.users == 0 {
			delete(s.held, key)
			os.RemoveAll(in.dir)
		}
	}

	// The first task to come fetches it. If that fails, the failure is that
	// task's, and the next to come tries for itself.
	in.fetching.Lock()
	defer in.fetching.Unlock()
	if in.dir == "" {
		made, err := os.MkdirTemp(base, "sisyphus-input-")
		if err == nil {
			for _, name := range sortedKeys(files) {
				if err = fetchTo(ctx, blobs, files[name], filepath.Join(made, name)); err != nil {
					os.RemoveAll(made)
					break
				}
			}
		}
		if err != nil {
			release()
			return "", nil, err
		}
		in.dir = made
	}
	return in.dir, release, nil
}

// fetchTo writes the stored blob with the given CID to a file.
func fetchTo(ctx context.Context, blobs Blobs, id, path string) error {
	c, _ := cid.Decode(id) // checked when the job was split
	blob, err := blobs.Open(ctx, c)
	if err != nil {
		return fmt.Errorf("container input %s: %w", id, err)
	}
	defer blob.Close()
	return writeStagedInput(id, path, blob, func(path string) (io.WriteCloser, error) {
		return os.Create(path)
	})
}

func makeTaskDirs(input, output string, mkdir func(string, os.FileMode) error, chmod func(string, os.FileMode) error) error {
	if err := mkdir(input, 0o755); err != nil {
		return fmt.Errorf("make the task's input directory: %w", err)
	}
	if err := mkdir(output, 0o777); err != nil {
		return fmt.Errorf("make the task's output directory: %w", err)
	}
	if err := chmod(output, 0o777); err != nil {
		return fmt.Errorf("make the task's output directory writable: %w", err)
	}
	return nil
}

func writeStagedInput(id, path string, source io.Reader, create func(string) (io.WriteCloser, error)) error {
	file, err := create(path)
	if err != nil {
		return fmt.Errorf("container input %s: make staged file: %w", id, err)
	}
	if _, err := io.Copy(file, source); err != nil {
		file.Close()
		return fmt.Errorf("container input %s: %w", id, err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("container input %s: finish staged file: %w", id, err)
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

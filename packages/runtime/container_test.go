package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ipfs/go-cid"

	"github.com/sisyphus-network/Sisyphus/packages/storage"
)

// volume returns the directory on this machine that a container's
// arguments mount at the given place inside it.
func volume(args []string, inside string) string {
	for i, arg := range args {
		if arg == "--volume" {
			if host, mounted, _ := strings.Cut(args[i+1], ":"); strings.HasPrefix(mounted, inside) {
				return host
			}
		}
	}
	return ""
}

// pretend is a container engine that does what a test tells it to in place
// of running anything, and remembers what it was asked.
type pretend struct {
	asked [][]string
	do    func(args []string, stdout, stderr io.Writer) (int, error)
}

func (p *pretend) engine(ctx context.Context, args []string, stdout, stderr io.Writer) (int, error) {
	p.asked = append(p.asked, args)
	if args[0] == "kill" {
		return 0, nil
	}
	return p.do(args, stdout, stderr)
}

func TestAContainerJobsInputIsReadWhenItIsSplit(t *testing.T) {
	ctx := context.Background()
	store := storage.NewMemory()
	input, err := store.Put(ctx, strings.NewReader("what the tasks are given"))
	if err != nil {
		t.Fatal(err)
	}
	// Only tasks read the input after this, so splitting is where the
	// coordinator learns of it and keeps it for as long as the job runs.
	touched := Record(store)
	if _, err := (Container{}).Split(ctx, touched, []byte(`{"image":"alpine:3.20","input":"`+input.String()+`"}`), 2); err != nil {
		t.Fatal(err)
	}
	if read := touched.Read(); !slices.Equal(read, []string{input.String()}) {
		t.Errorf("splitting read %v, want the input %s", read, input)
	}
	// An input that is not here is refused before any task is given out.
	absent, _ := storage.NewMemory().Put(ctx, strings.NewReader("held elsewhere"))
	if _, err := (Container{}).Split(ctx, store, []byte(`{"image":"alpine:3.20","input":"`+absent.String()+`"}`), 1); err == nil || !strings.Contains(err.Error(), "container input "+absent.String()) {
		t.Errorf("an input that is not held: %v", err)
	}
}

func TestAContainerJobIsSplitIntoCopiesThatKnowWhichTheyAre(t *testing.T) {
	c := Container{}
	params := `{"image":"alpine:3.20","command":["echo","hi"],"env":{"B":"2","A":"1"},"memory_mb":256,"cpus":1.5}`
	payloads, err := c.Split(context.Background(), nil, []byte(params), 3)
	if err != nil || len(payloads) != 3 {
		t.Fatalf("Split = %d payloads, %v", len(payloads), err)
	}
	var second containerTask
	json.Unmarshal(payloads[1], &second)
	if second.Index != 1 || second.Count != 3 || second.Image != "alpine:3.20" || !slices.Equal(second.Command, []string{"echo", "hi"}) {
		t.Errorf("the second task's payload: %+v", second)
	}
	// A job that says how many copies it wants gets that many.
	if payloads, _ := c.Split(context.Background(), nil, []byte(`{"image":"alpine:3.20","tasks":5}`), 2); len(payloads) != 5 {
		t.Errorf("a job asking for five tasks was split into %d", len(payloads))
	}
	for name, bad := range map[string]string{
		"not JSON":              `image: alpine`,
		"no image":              `{"command":["echo"]}`,
		"an input that is none": `{"image":"alpine:3.20","input":"not-a-cid"}`,
		"too many copies":       `{"image":"alpine:3.20","tasks":10001}`,
	} {
		if _, err := c.Split(context.Background(), nil, []byte(bad), 1); err == nil || !strings.Contains(err.Error(), "container parameters") {
			t.Errorf("%s: %v", name, err)
		}
	}
	if c.Name() != "container" || !strings.Contains(c.Describe(), "SISYPHUS_TASK_INDEX") {
		t.Errorf("the workload is %q, described as %q", c.Name(), c.Describe())
	}
}

func TestAContainerTaskRunsWithItsInputAndItsOutputIsKept(t *testing.T) {
	store := storage.NewMemory()
	ctx := context.Background()
	input, err := store.Put(ctx, strings.NewReader("what the task is given"))
	if err != nil {
		t.Fatal(err)
	}
	said := new(heard)
	engine := &pretend{do: func(args []string, stdout, stderr io.Writer) (int, error) {
		// As a command would: read the input, print, complain, and leave
		// files behind, one of them in a directory, which is not kept.
		given, _ := os.ReadFile(filepath.Join(volume(args, "/input"), "data"))
		io.WriteString(stdout, "read: "+string(given)+"\nand a line with no end")
		io.WriteString(stderr, "a warning\r\n")
		out := volume(args, "/output")
		os.WriteFile(filepath.Join(out, "result.txt"), []byte("the answer"), 0o644)
		os.Mkdir(filepath.Join(out, "scratch"), 0o755)
		return 0, nil
	}}
	c := Container{Engine: engine.engine, Confined: Confinement{Runtime: "runsc", ReadOnly: true}}
	payloads, _ := c.Split(ctx, store, []byte(`{"image":"alpine:3.20","command":["sh","-c","work"],"input":"`+input.String()+`","env":{"B":"2","A":"1"},"memory_mb":256,"cpus":1.5,"gpus":2}`), 2)
	raw, err := c.Execute(WithReporter(ctx, said), store, payloads[1])
	if err != nil {
		t.Fatal(err)
	}
	var out ContainerOutput
	json.Unmarshal(raw, &out)
	if out.Index != 1 || readBlob(t, store, out.Stdout) != "read: what the task is given\nand a line with no end" ||
		len(out.Files) != 1 || readBlob(t, store, out.Files["result.txt"]) != "the answer" {
		t.Errorf("the task's output: %+v", out)
	}
	// What it printed was logged as it went, a line at a time.
	if !slices.Equal(said.lines, []string{"running alpine:3.20 sh -c work", "read: what the task is given", "a warning", "and a line with no end"}) {
		t.Errorf("logged %q", said.lines)
	}

	// The engine was asked for a container with no network, the limits and
	// environment the job gave, and the task's place in the job.
	args := strings.Join(engine.asked[0], " ")
	for _, want := range []string{"run --rm --name sisyphus-", ":/input:ro", ":/output", "--env SISYPHUS_TASK_INDEX=1", "--env SISYPHUS_TASK_COUNT=2",
		"--security-opt no-new-privileges", "--pids-limit 4096", "--user " + strconv.Itoa(os.Getuid()) + ":" + strconv.Itoa(os.Getgid()) + " --cap-drop ALL --env HOME=/tmp", "--runtime runsc --read-only --tmpfs /tmp --network none", "--memory 256m", "--cpus 1.5", "--gpus 2", "--env A=1 --env B=2", "alpine:3.20 sh -c work"} {
		if !strings.Contains(args, want) {
			t.Errorf("the engine was asked %q, without %q", args, want)
		}
	}
	// And the task's directory is gone, and its input with it.
	for _, inside := range []string{"/output", "/input"} {
		if _, err := os.Stat(volume(engine.asked[0], inside)); !os.IsNotExist(err) {
			t.Errorf("what the task had at %s was left behind: %v", inside, err)
		}
	}

	// A job that asks for the network is refused it by a worker whose owner
	// does not allow it, and gets it from one that does; one with no limits
	// has none.
	engine.do = func([]string, io.Writer, io.Writer) (int, error) { return 0, nil }
	open, _ := c.Split(ctx, store, []byte(`{"image":"alpine:3.20","network":true}`), 1)
	if _, err := c.Execute(ctx, store, open[0]); err == nil || !strings.Contains(err.Error(), "does not let the pool's containers reach it") || len(engine.asked) != 1 {
		t.Fatalf("a job asking for the network on a worker that does not allow it: %v, %d asked", err, len(engine.asked))
	}
	c.Confined.Network = true
	if _, err := c.Execute(ctx, store, open[0]); err != nil {
		t.Fatal(err)
	}
	if args := strings.Join(engine.asked[1], " "); strings.Contains(args, "--network") || strings.Contains(args, "--memory") || strings.Contains(args, "--cpus") || strings.Contains(args, "--gpus") {
		t.Errorf("a job with the network and no limits was run as %q", args)
	}
}

func TestTheResultOfAContainerJobIsItsTasksOutputsInOrder(t *testing.T) {
	c := Container{}
	outputs := [][]byte{mustJSON(ContainerOutput{Index: 0, Stdout: "bafy-a"}), mustJSON(ContainerOutput{Index: 1, Stdout: "bafy-b", Files: map[string]string{"f": "bafy-f"}})}
	raw, err := c.Aggregate(context.Background(), nil, outputs)
	if err != nil {
		t.Fatal(err)
	}
	var result ContainerResult
	json.Unmarshal(raw, &result)
	if len(result.Tasks) != 2 || result.Tasks[1].Stdout != "bafy-b" || result.Tasks[1].Files["f"] != "bafy-f" {
		t.Errorf("result %+v", result)
	}
	if _, err := c.Aggregate(context.Background(), nil, [][]byte{[]byte("not json")}); err == nil || !strings.Contains(err.Error(), "task 0 output") {
		t.Errorf("an output that is not JSON: %v", err)
	}
}

func TestAContainerResultKeepsTheBlobsItReferences(t *testing.T) {
	workload := Container{}
	result := mustJSON(ContainerResult{Tasks: []ContainerOutput{
		{Stdout: "stdout-0", Files: map[string]string{"index": "index-0"}},
		{Stdout: "stdout-1"},
	}})
	got := workload.ResultBlobs(result)
	sort.Strings(got)
	want := []string{"index-0", "stdout-0", "stdout-1"}
	if !slices.Equal(got, want) {
		t.Errorf("ResultBlobs() = %v, want %v", got, want)
	}
	if got := workload.ResultBlobs([]byte("not JSON")); len(got) != 0 {
		t.Errorf("ResultBlobs(invalid JSON) = %v, want none", got)
	}
}

// awkward is a blob store that fails in the ways a test asks.
type awkward struct {
	Blobs
	puts     int
	failPut  int // which call to Put fails, counting from one
	badReads bool
}

var errAwkward = errors.New("the store is full")

func (a *awkward) Put(ctx context.Context, r io.Reader) (cid.Cid, error) {
	if a.puts++; a.puts == a.failPut {
		return cid.Undef, errAwkward
	}
	return a.Blobs.Put(ctx, r)
}

func (a *awkward) Open(ctx context.Context, c cid.Cid) (storage.Blob, error) {
	blob, err := a.Blobs.Open(ctx, c)
	if err == nil && a.badReads {
		return unreadable{blob}, nil
	}
	return blob, err
}

type unreadable struct{ storage.Blob }

func (unreadable) Read([]byte) (int, error) { return 0, errAwkward }

func TestAnImageThatWouldBeReadAsAnOptionIsRefused(t *testing.T) {
	ctx := context.Background()
	ran := false
	c := Container{Engine: func(context.Context, []string, io.Writer, io.Writer) (int, error) { ran = true; return 0, nil }}
	for image, want := range map[string]string{
		"":                     "no image is named",
		"--privileged":         "it begins with a dash",
		"--volume=/:/host":     "it begins with a dash",
		"-v":                   "it begins with a dash",
		"alpine --privileged":  "it has a space or a control character in it",
		"alpine\n--privileged": "it has a space or a control character in it",
		"alpine\u0000":         "it has a space or a control character in it",
	} {
		params := mustJSON(ContainerParams{Image: image, Command: []string{"alpine", "id"}})
		if _, err := c.Split(ctx, nil, params, 1); err == nil || !strings.Contains(err.Error(), "container parameters: ") || !strings.Contains(err.Error(), want) {
			t.Errorf("a job with the image %q: %v", image, err)
		}
		// A worker handed such a task by a coordinator that did not check
		// refuses it too, and starts nothing.
		payload := mustJSON(containerTask{ContainerParams: ContainerParams{Image: image, Command: []string{"alpine", "id"}}, Count: 1})
		if _, err := c.Execute(ctx, storage.NewMemory(), payload); err == nil || !strings.Contains(err.Error(), "container payload: ") || !strings.Contains(err.Error(), want) {
			t.Errorf("a task with the image %q: %v", image, err)
		}
	}
	if ran {
		t.Error("the engine was started for a task whose image is no image")
	}
	// Names that are images, in every form they take.
	for _, image := range []string{"alpine", "alpine:3.20", "ghcr.io/owner/name:tag", "localhost:5000/a/b", "alpine@sha256:" + strings.Repeat("a", 64)} {
		if _, err := c.Split(ctx, nil, mustJSON(ContainerParams{Image: image}), 1); err != nil {
			t.Errorf("a job with the image %q: %v", image, err)
		}
	}
}

func TestContainerTasksThatFail(t *testing.T) {
	store := storage.NewMemory()
	ctx := context.Background()
	input, _ := store.Put(ctx, strings.NewReader("input"))
	missing := "bafkreigh2akiscaildcqabsyg3dfr6chu3fgpregiymsck7e7aqa4s52zy"
	payload := func(params string) []byte {
		payloads, err := Container{}.Split(ctx, store, []byte(params), 1)
		if err != nil {
			t.Fatal(err)
		}
		return payloads[0]
	}
	plain := payload(`{"image":"alpine:3.20"}`)
	leaves := func(args []string, _, _ io.Writer) (int, error) {
		os.WriteFile(filepath.Join(volume(args, "/output"), "f"), []byte("x"), 0o644)
		return 0, nil
	}
	quiet := func([]string, io.Writer, io.Writer) (int, error) { return 0, nil }

	for name, tt := range map[string]struct {
		payload []byte
		blobs   Blobs
		do      func([]string, io.Writer, io.Writer) (int, error)
		want    string
	}{
		"a payload that is not one": {[]byte("not json"), store, quiet, "container payload"},
		// Split refuses an input it cannot find, so this is one that went
		// after the job was split.
		"an input that is not stored":  {mustJSON(containerTask{ContainerParams: ContainerParams{Image: "alpine:3.20", Input: missing}, Count: 1}), store, quiet, "container input " + missing},
		"an input that cannot be read": {payload(`{"image":"alpine:3.20","input":"` + input.String() + `"}`), &awkward{Blobs: store, badReads: true}, quiet, "the store is full"},
		"an engine that cannot be run": {plain, store, func([]string, io.Writer, io.Writer) (int, error) { return 0, errors.New("docker: not found") }, "run the container: docker: not found"},
		"a command that fails": {plain, store, func(_ []string, _, stderr io.Writer) (int, error) {
			io.WriteString(stderr, "first\nno such file\n")
			return 3, nil
		}, "exited with status 3: no such file"},
		"output that cannot be stored": {plain, &awkward{Blobs: store, failPut: 1}, quiet, "store what the command printed"},
		"a file that cannot be stored": {plain, &awkward{Blobs: store, failPut: 2}, leaves, "store the task's output f"},
		"a file that cannot be read": {plain, store, func(args []string, _, _ io.Writer) (int, error) {
			os.WriteFile(filepath.Join(volume(args, "/output"), "locked"), []byte("x"), 0o000)
			return 0, nil
		}, "read the task's output locked"},
	} {
		c := Container{Engine: (&pretend{do: tt.do}).engine}
		if _, err := c.Execute(ctx, tt.blobs, tt.payload); err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("%s: %v, want an error containing %q", name, err, tt.want)
		}
	}

	// Nowhere to put the task's files.
	t.Setenv("TMPDIR", filepath.Join(t.TempDir(), "no-such-directory"))
	if _, err := (Container{Engine: (&pretend{do: quiet}).engine}).Execute(ctx, store, plain); err == nil || !strings.Contains(err.Error(), "make the task's directory") {
		t.Errorf("with no temporary directory: %v", err)
	}
}

func TestAContainerTaskKeepsItsWorkspaceAndStagedInputUnderTaskDir(t *testing.T) {
	ctx := context.Background()
	store := storage.NewMemory()
	input, err := store.Put(ctx, strings.NewReader("staged input"))
	if err != nil {
		t.Fatal(err)
	}
	base := t.TempDir()
	var taskDir, inputDir string
	engine := &pretend{do: func(args []string, _, _ io.Writer) (int, error) {
		taskDir = filepath.Dir(volume(args, "/output"))
		inputDir = volume(args, "/input")
		return 0, nil
	}}
	payloads, err := (Container{}).Split(ctx, store, mustJSON(ContainerParams{Image: "alpine:3.20", Input: input.String()}), 1)
	if err != nil {
		t.Fatal(err)
	}
	confined := Confinement{TaskDir: base}
	if _, err := (Container{Engine: engine.engine, Confined: confined}).Execute(ctx, store, payloads[0]); err != nil {
		t.Fatal(err)
	}
	for name, path := range map[string]string{"task workspace": taskDir, "staged input": inputDir} {
		rel, err := filepath.Rel(base, path)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
			t.Errorf("%s path %q is not under task directory %q (relative %q, error %v)", name, path, base, rel, err)
		}
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s path %q remains after the task: %v", name, path, err)
		}
	}
}

func TestTaskWorkspaceDirectoryErrorsAreReturned(t *testing.T) {
	for name, tt := range map[string]struct {
		mkdir func(string, os.FileMode) error
		chmod func(string, os.FileMode) error
		want  string
	}{
		"input directory": {
			mkdir: func(string, os.FileMode) error { return errors.New("full") },
			chmod: func(string, os.FileMode) error { return nil },
			want:  "make the task's input directory: full",
		},
		"output directory": {
			mkdir: func(path string, _ os.FileMode) error {
				if path == "output" {
					return errors.New("full")
				}
				return nil
			},
			chmod: func(string, os.FileMode) error { return nil },
			want:  "make the task's output directory: full",
		},
		"output permissions": {
			mkdir: func(string, os.FileMode) error { return nil },
			chmod: func(string, os.FileMode) error { return errors.New("denied") },
			want:  "make the task's output directory writable: denied",
		},
	} {
		err := makeTaskDirs("input", "output", tt.mkdir, tt.chmod)
		if err == nil || err.Error() != tt.want {
			t.Errorf("%s: %v, want %q", name, err, tt.want)
		}
	}
}

func TestContainerTaskReturnsWhenItsDirectoriesCannotBeMade(t *testing.T) {
	payloads, err := (Container{}).Split(context.Background(), nil, []byte(`{"image":"alpine:3.20"}`), 1)
	if err != nil {
		t.Fatal(err)
	}
	want := errors.New("volume is full")
	c := Container{
		Confined:    Confinement{TaskDir: t.TempDir()},
		prepareDirs: func(string, string) error { return want },
		Engine: func(context.Context, []string, io.Writer, io.Writer) (int, error) {
			t.Error("the engine ran without task directories")
			return 0, nil
		},
	}
	if _, err := c.Execute(context.Background(), storage.NewMemory(), payloads[0]); !errors.Is(err, want) {
		t.Errorf("execute error = %v, want %v", err, want)
	}
}

type closeFailWriter struct {
	bytes.Buffer
	err error
}

func (w *closeFailWriter) Close() error { return w.err }

func TestStagingInputReportsCreateAndCloseFailures(t *testing.T) {
	t.Run("create", func(t *testing.T) {
		want := errors.New("full")
		err := writeStagedInput("input-cid", "data", strings.NewReader("input"), func(string) (io.WriteCloser, error) {
			return nil, want
		})
		if !errors.Is(err, want) || !strings.Contains(err.Error(), "make staged file") {
			t.Errorf("staging error = %v", err)
		}
	})
	t.Run("close", func(t *testing.T) {
		want := errors.New("full")
		err := writeStagedInput("input-cid", "data", strings.NewReader("input"), func(string) (io.WriteCloser, error) {
			return &closeFailWriter{err: want}, nil
		})
		if !errors.Is(err, want) || !strings.Contains(err.Error(), "finish staged file") {
			t.Errorf("staging error = %v", err)
		}
	})
}

func TestAContainerTaskThatIsStoppedHasItsContainerKilled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	engine := &pretend{}
	engine.do = func([]string, io.Writer, io.Writer) (int, error) {
		cancel()
		return 0, context.Canceled
	}
	payloads, _ := Container{}.Split(ctx, nil, []byte(`{"image":"alpine:3.20"}`), 1)
	if _, err := (Container{Engine: engine.engine}).Execute(ctx, storage.NewMemory(), payloads[0]); !errors.Is(err, context.Canceled) {
		t.Errorf("a stopped task: %v", err)
	}
	// The container is told to stop by name, since stopping the command
	// that started it does not.
	if len(engine.asked) != 2 || engine.asked[1][0] != "kill" || !strings.HasPrefix(engine.asked[1][1], "sisyphus-") || !slices.Contains(engine.asked[0], engine.asked[1][1]) {
		t.Errorf("the engine was asked %v", engine.asked)
	}
}

// What a task leaves in /output is stored and kept, so a task may leave
// only so much.
func TestATaskMayLeaveOnlySoMuchInItsOutput(t *testing.T) {
	store := storage.NewMemory()
	ctx := context.Background()
	leaving := func(files map[string]int) *pretend {
		return &pretend{do: func(args []string, _, _ io.Writer) (int, error) {
			for name, size := range files {
				os.WriteFile(filepath.Join(volume(args, "/output"), name), bytes.Repeat([]byte("x"), size), 0o644)
			}
			return 0, nil
		}}
	}
	run := func(c Container) (ContainerOutput, error) {
		payloads, _ := c.Split(ctx, store, []byte(`{"image":"alpine:3.20"}`), 1)
		raw, err := c.Execute(ctx, store, payloads[0])
		var out ContainerOutput
		json.Unmarshal(raw, &out)
		return out, err
	}
	// Up to the limit, between however many files.
	if out, err := run(Container{Engine: leaving(map[string]int{"a": 60, "b": 40}).engine, MaxOutput: 100}); err != nil || len(out.Files) != 2 {
		t.Errorf("a task that left as much as it may: %+v, %v", out, err)
	}
	for name, files := range map[string]map[string]int{"one file too long": {"a": 101}, "two that are together": {"a": 60, "b": 41}} {
		if _, err := run(Container{Engine: leaving(files).engine, MaxOutput: 100}); err == nil || !strings.Contains(err.Error(), "left more in /output than the 100 bytes a task may") {
			t.Errorf("%s: %v", name, err)
		}
	}
	// With no limit set there is the usual one, which these are well within.
	many := map[string]int{}
	for i := range maxOutputFiles {
		many[strconv.Itoa(i)] = 1
	}
	if out, err := run(Container{Engine: leaving(many).engine}); err != nil || len(out.Files) != maxOutputFiles {
		t.Errorf("a task that left as many files as it may: %d, %v", len(out.Files), err)
	}
	many["one more"] = 1
	if _, err := run(Container{Engine: leaving(many).engine}); err == nil || !strings.Contains(err.Error(), "left more than 1024 files in /output") {
		t.Errorf("a task that left too many files: %v", err)
	}
}

func TestOnlySoMuchOfWhatATaskPrintsIsKept(t *testing.T) {
	var kept capped
	kept.Write(bytes.Repeat([]byte("x"), maxStdout-10))
	if n, err := kept.Write(bytes.Repeat([]byte("y"), 100)); n != 100 || err != nil || kept.Len() != maxStdout {
		t.Errorf("writing past the limit: %d, %v, %d kept", n, err, kept.Len())
	}
	kept.Write([]byte("more"))
	if kept.Len() != maxStdout {
		t.Errorf("%d bytes kept, want %d", kept.Len(), maxStdout)
	}
}

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

func TestARealContainerRuns(t *testing.T) {
	requireDocker(t)
	// A temporary directory of this test's own, so that what it finds
	// left behind is its own doing and not that of another package's
	// tests, which run at the same time and make task directories too.
	// Under the usual one, which Docker can always reach.
	own, err := os.MkdirTemp(os.TempDir(), "sisyphus-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(own) })
	t.Setenv("TMPDIR", own)
	store := storage.NewMemory()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	input, _ := store.Put(ctx, strings.NewReader("stone\n"))
	c := Container{}
	payloads, err := c.Split(ctx, store, []byte(`{"image":"alpine:3.20","input":"`+input.String()+`","command":["sh","-c","echo task $SISYPHUS_TASK_INDEX of $SISYPHUS_TASK_COUNT pushes the $(cat /input/data); echo uphill > /output/where; wget -q -T 2 -O- http://1.1.1.1 >/dev/null 2>&1 || echo no network"]}`), 3)
	if err != nil {
		t.Fatal(err)
	}
	said := new(heard)
	raw, err := c.Execute(WithReporter(ctx, said), store, payloads[2])
	if err != nil {
		t.Fatal(err)
	}
	var out ContainerOutput
	json.Unmarshal(raw, &out)
	if got := readBlob(t, store, out.Stdout); got != "task 2 of 3 pushes the stone\nno network\n" {
		t.Errorf("the container printed %q", got)
	}
	if got := readBlob(t, store, out.Files["where"]); got != "uphill\n" {
		t.Errorf("the file it left: %q", got)
	}
	// Nothing of the task is left on this machine.
	if left, _ := filepath.Glob(filepath.Join(os.TempDir(), "sisyphus-task-*")); len(left) != 0 {
		t.Errorf("task directories left behind: %v", left)
	}

	// Named inputs are beside the input, and can no more be written to.
	second, _ := store.Put(ctx, strings.NewReader("hill\n"))
	named, err := c.Split(ctx, store, []byte(`{"image":"alpine:3.20","input":"`+input.String()+`","inputs":{"where.txt":"`+second.String()+`"},"command":["sh","-c","ls /input; cat /input/data /input/where.txt; echo no > /input/where.txt 2>/dev/null || echo read-only"]}`), 1)
	if err != nil {
		t.Fatal(err)
	}
	if raw, err = c.Execute(ctx, store, named[0]); err != nil {
		t.Fatal(err)
	}
	json.Unmarshal(raw, &out)
	if got := readBlob(t, store, out.Stdout); got != "data\nwhere.txt\nstone\nhill\nread-only\n" {
		t.Errorf("a task with a named input printed %q", got)
	}

	// Held in further at its owner's asking: run by the runtime named, with
	// nowhere to write but /output and /tmp.
	held := Container{Confined: Confinement{Runtime: "runc", ReadOnly: true}}
	writes, _ := held.Split(ctx, store, []byte(`{"image":"alpine:3.20","command":["sh","-c","echo kept > /output/kept; echo scratch > /tmp/scratch && echo tmp is writable; echo no > /etc/changed 2>/dev/null || echo the image is not"]}`), 1)
	raw, err = held.Execute(ctx, store, writes[0])
	if err != nil {
		t.Fatal(err)
	}
	json.Unmarshal(raw, &out)
	if got := readBlob(t, store, out.Stdout); got != "tmp is writable\nthe image is not\n" || readBlob(t, store, out.Files["kept"]) != "kept\n" {
		t.Errorf("a task with a read-only root printed %q and left %v", got, out.Files)
	}

	// The command can gain no privilege, and has none of a container's
	// usual capabilities to begin with.
	confined, _ := c.Split(ctx, store, []byte(`{"image":"alpine:3.20","command":["sh","-c","grep -E '^(CapBnd|NoNewPrivs)' /proc/self/status"]}`), 1)
	raw, err = c.Execute(ctx, store, confined[0])
	if err != nil {
		t.Fatal(err)
	}
	json.Unmarshal(raw, &out)
	if got := readBlob(t, store, out.Stdout); got != "CapBnd:\t0000000000000000\nNoNewPrivs:\t1\n" {
		t.Errorf("the container's privileges: %q", got)
	}
	// It has a home it can write to, which the node's user has none of in
	// the image, unless the job gives it another.
	for params, want := range map[string]string{
		`{"image":"alpine:3.20","command":["sh","-c","echo kept > ~/note && echo $HOME"]}`:                          "/tmp\n",
		`{"image":"alpine:3.20","env":{"HOME":"/output"},"command":["sh","-c","echo kept > ~/note && echo $HOME"]}`: "/output\n",
	} {
		homed, _ := c.Split(ctx, store, []byte(params), 1)
		raw, err = c.Execute(ctx, store, homed[0])
		if err != nil {
			t.Fatal(err)
		}
		json.Unmarshal(raw, &out)
		if got := readBlob(t, store, out.Stdout); got != want {
			t.Errorf("the home of %s: %q", params, got)
		}
	}

	// A command that fails says how, and one that cannot be started says so.
	failing, _ := c.Split(ctx, store, []byte(`{"image":"alpine:3.20","command":["sh","-c","echo it went wrong >&2; exit 7"]}`), 1)
	if _, err := c.Execute(ctx, store, failing[0]); err == nil || !strings.Contains(err.Error(), "exited with status 7: it went wrong") {
		t.Errorf("a failing command: %v", err)
	}
	t.Setenv("PATH", t.TempDir())
	if _, err := c.Execute(ctx, store, failing[0]); err == nil || !strings.Contains(err.Error(), "run the container") {
		t.Errorf("with no docker to run: %v", err)
	}
}

func TestFindingOutWhetherContainersCanBeRun(t *testing.T) {
	requireDocker(t)
	if err := CheckContainers(context.Background(), Confinement{}); err != nil {
		t.Errorf("on a machine with Docker: %v", err)
	}
	// A runtime Docker has may be asked for, and one it has not may not:
	// a node told to hold its tasks in further does not run them less
	// held in than it was told.
	if err := CheckContainers(context.Background(), Confinement{Runtime: "runc"}); err != nil {
		t.Errorf("asking for a runtime Docker has: %v", err)
	}
	if err := CheckContainers(context.Background(), Confinement{Runtime: "no-such-runtime"}); err == nil || !strings.Contains(err.Error(), "Docker here has no runtime called no-such-runtime: it has ") || !strings.Contains(err.Error(), "runc") {
		t.Errorf("asking for a runtime Docker has not: %v", err)
	}
	for _, name := range []string{"--privileged", "run sc", "runsc;id", "-v"} {
		if err := CheckContainers(context.Background(), Confinement{Runtime: name}); err == nil || !strings.Contains(err.Error(), "is not the name of a container runtime") {
			t.Errorf("a runtime called %q: %v", name, err)
		}
	}
	// A docker that answers for its version and not for its runtimes.
	real, _ := exec.LookPath("docker")
	partial := filepath.Join(t.TempDir(), "docker")
	os.WriteFile(partial, []byte("#!/bin/sh\n[ \"$1\" = version ] && exec "+real+" \"$@\"\nexit 1\n"), 0o700)
	func() {
		t.Setenv("PATH", filepath.Dir(partial))
		defer t.Setenv("PATH", os.Getenv("PATH"))
		if err := CheckContainers(context.Background(), Confinement{Runtime: "runc"}); err == nil || !strings.Contains(err.Error(), "did not say which runtimes it has") {
			t.Errorf("with a Docker that does not list its runtimes: %v", err)
		}
	}()
	// Docker installed, with no daemon behind it to talk to.
	t.Setenv("DOCKER_HOST", "tcp://127.0.0.1:1")
	if err := CheckContainers(context.Background(), Confinement{}); err == nil || !strings.Contains(err.Error(), "Docker cannot be used here") {
		t.Errorf("with no Docker daemon to reach: %v", err)
	}
	// No Docker at all.
	t.Setenv("PATH", t.TempDir())
	if err := CheckContainers(context.Background(), Confinement{}); err == nil || !strings.Contains(err.Error(), "Docker cannot be used here") {
		t.Errorf("with no docker command: %v", err)
	}
	if got := WithContainers().Names(); !slices.Contains(got, "container") || slices.Contains(Builtin().Names(), "container") {
		t.Errorf("the registries: %v with containers, %v without", got, Builtin().Names())
	}
}

// counting is a store that counts how often it is opened.
type counting struct {
	Blobs
	mu    sync.Mutex
	opens int
}

func (c *counting) Open(ctx context.Context, id cid.Cid) (storage.Blob, error) {
	c.mu.Lock()
	c.opens++
	c.mu.Unlock()
	return c.Blobs.Open(ctx, id)
}

func TestTasksOfAJobOnOneMachineShareOneCopyOfItsInput(t *testing.T) {
	store := &counting{Blobs: storage.NewMemory()}
	ctx := context.Background()
	input, _ := store.Put(ctx, strings.NewReader("a large input"))
	params := []byte(`{"image":"alpine:3.20","input":"` + input.String() + `"}`)

	// Two tasks of one job run at once: neither finishes until both have
	// started, and each says where its input was.
	var mu sync.Mutex
	var seen []string
	both := make(chan struct{})
	engine := &pretend{do: func(args []string, _, _ io.Writer) (int, error) {
		dir := volume(args, "/input")
		given, _ := os.ReadFile(filepath.Join(dir, "data"))
		mu.Lock()
		seen = append(seen, dir+": "+string(given))
		if len(seen) == 2 {
			close(both)
		}
		mu.Unlock()
		<-both
		return 0, nil
	}}
	// The engine is asked by two tasks at once, so what it was asked is
	// not kept.
	c := Container{Engine: func(ctx context.Context, args []string, stdout, stderr io.Writer) (int, error) {
		return engine.do(args, stdout, stderr)
	}}
	payloads, _ := c.Split(ctx, store.Blobs, params, 2) // on the coordinator, which is not counted
	var tasks sync.WaitGroup
	for _, payload := range payloads {
		tasks.Add(1)
		go func() {
			defer tasks.Done()
			if _, err := c.Execute(ctx, store, payload); err != nil {
				t.Error(err)
			}
		}()
	}
	tasks.Wait()
	if len(seen) != 2 || seen[0] != seen[1] || !strings.HasSuffix(seen[0], ": a large input") || store.opens != 1 {
		t.Errorf("two tasks of one job saw %q, the input having been fetched %d times", seen, store.opens)
	}
	shared, _, _ := strings.Cut(seen[0], ": ")
	if _, err := os.Stat(shared); !os.IsNotExist(err) {
		t.Errorf("the input was left behind when its last task ended: %v", err)
	}

	// A task of another job is not given that copy, though it names the
	// same input: it fetches its own.
	seen, both = nil, make(chan struct{})
	close(both)
	first, _ := c.Split(ctx, store.Blobs, params, 1)
	second, _ := c.Split(ctx, store.Blobs, params, 1)
	engine.do = func(args []string, _, _ io.Writer) (int, error) {
		seen = append(seen, volume(args, "/input"))
		if len(seen) == 1 {
			// While the first job's task runs, the second job's does.
			if _, err := c.Execute(ctx, store, second[0]); err != nil {
				t.Error(err)
			}
		}
		return 0, nil
	}
	if _, err := c.Execute(ctx, store, first[0]); err != nil {
		t.Fatal(err)
	}
	if len(seen) != 2 || seen[0] == seen[1] || store.opens != 3 {
		t.Errorf("tasks of two jobs saw %q, the input having been fetched %d times", seen, store.opens)
	}
	if len(inputs.held) != 0 {
		t.Errorf("inputs still held with no task running: %v", inputs.held)
	}

	// With nowhere to put it, the input is not had.
	t.Setenv("TMPDIR", filepath.Join(t.TempDir(), "no-such-directory"))
	if _, _, err := inputs.take(ctx, store, "", "job", map[string]string{"data": input.String()}); err == nil || len(inputs.held) != 0 {
		t.Errorf("with no temporary directory: %v, holding %v", err, inputs.held)
	}
}

// A job may give its tasks more files than its input, each by a name of its
// own, and they are found beside the input in /input.
func TestAContainerTaskIsGivenItsNamedInputs(t *testing.T) {
	store := storage.NewMemory()
	ctx := context.Background()
	data, _ := store.Put(ctx, strings.NewReader("the input"))
	topology, _ := store.Put(ctx, strings.NewReader("a topology"))
	coordinates, _ := store.Put(ctx, strings.NewReader("coordinates"))
	params := `{"image":"alpine:3.20","input":"` + data.String() + `","inputs":{"system.prmtop":"` + topology.String() + `","coords_1-a":"` + coordinates.String() + `"}}`

	// Splitting reads each of them, so the coordinator keeps them all.
	touched := Record(store)
	payloads, err := (Container{}).Split(ctx, touched, []byte(params), 2)
	if err != nil {
		t.Fatal(err)
	}
	read := touched.Read()
	sort.Strings(read)
	want := []string{data.String(), topology.String(), coordinates.String()}
	sort.Strings(want)
	if !slices.Equal(read, want) {
		t.Errorf("splitting read %v, want %v", read, want)
	}

	var given map[string]string
	var dir string
	engine := &pretend{do: func(args []string, _, _ io.Writer) (int, error) {
		dir = volume(args, "/input")
		given = map[string]string{}
		entries, _ := os.ReadDir(dir)
		for _, e := range entries {
			text, _ := os.ReadFile(filepath.Join(dir, e.Name()))
			given[e.Name()] = string(text)
		}
		return 0, nil
	}}
	c := Container{Engine: engine.engine}
	if _, err := c.Execute(ctx, store, payloads[1]); err != nil {
		t.Fatal(err)
	}
	if len(given) != 3 || given["data"] != "the input" || given["system.prmtop"] != "a topology" || given["coords_1-a"] != "coordinates" {
		t.Errorf("the task was given %v", given)
	}
	// They are one read-only mount, as the input alone is.
	if args := strings.Join(engine.asked[0], " "); strings.Count(args, "--volume") != 2 || !strings.Contains(args, dir+":/input:ro") {
		t.Errorf("the engine was asked %q", args)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) || len(inputs.held) != 0 {
		t.Errorf("the inputs were left behind: %v, holding %v", err, inputs.held)
	}

	// Named inputs need no unnamed one.
	only, err := (Container{}).Split(ctx, store, []byte(`{"image":"alpine:3.20","inputs":{"one":"`+topology.String()+`"}}`), 1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Execute(ctx, store, only[0]); err != nil || len(given) != 1 || given["one"] != "a topology" {
		t.Errorf("a task given only a named input: %v, %v", given, err)
	}

	// One the worker cannot fetch fails the task, and nothing of what it
	// had fetched is left behind.
	elsewhere := storage.NewMemory()
	elsewhere.Put(ctx, strings.NewReader("the input"))
	own := t.TempDir()
	c.Confined.TaskDir = own
	if _, err := c.Execute(ctx, elsewhere, payloads[0]); err == nil || !strings.Contains(err.Error(), "container input") || len(inputs.held) != 0 {
		t.Errorf("a named input the worker cannot fetch: %v, holding %v", err, inputs.held)
	}
	if left, _ := os.ReadDir(own); len(left) != 0 {
		t.Errorf("left behind after a failed fetch: %v", left)
	}
	if len(engine.asked) != 2 {
		t.Errorf("the engine was asked to run %d tasks", len(engine.asked))
	}
}

// The names of a job's inputs are its submitter's, and each is joined to a
// directory of the worker's: none may lead out of it, or over the input,
// whether the coordinator splits the job or a worker is handed a payload
// made by hand.
func TestAnInputNameThatCouldLeadOutOfInputIsRefused(t *testing.T) {
	store := storage.NewMemory()
	ctx := context.Background()
	stored, _ := store.Put(ctx, strings.NewReader("anything"))
	id := stored.String()
	many := map[string]string{}
	for i := range maxInputs + 1 {
		many["in"+strconv.Itoa(i)] = id
	}
	bad := map[string]map[string]string{
		"empty":               {"": id},
		"the input's own":     {"data": id},
		"this directory":      {".": id},
		"the one above":       {"..": id},
		"up and out":          {"../escaped": id},
		"further up":          {"../../../../tmp/escaped": id},
		"a slash":             {"a/b": id},
		"absolute":            {"/etc/passwd": id},
		"a backslash":         {`..\escaped`: id},
		"upper case":          {"Topology": id},
		"a space":             {"a b": id},
		"a control character": {"a\nb": id},
		"a null":              {"a\u0000b": id},
		"not ASCII":           {"caf\u00e9": id},
		"too long":            {strings.Repeat("a", 65): id},
		"not a CID":           {"fine": "not-a-cid"},
		"too many":            many,
	}
	engine := &pretend{do: func([]string, io.Writer, io.Writer) (int, error) { return 0, nil }}
	c := Container{Engine: engine.engine, Confined: Confinement{TaskDir: t.TempDir()}}
	for why, named := range bad {
		params := mustJSON(map[string]any{"image": "alpine:3.20", "inputs": named})
		if _, err := c.Split(ctx, store, params, 1); err == nil || !strings.Contains(err.Error(), "container parameters") {
			t.Errorf("%s: split %v", why, err)
		}
		forged := mustJSON(map[string]any{"image": "alpine:3.20", "inputs": named, "job": "forged", "index": 0, "count": 1})
		if _, err := c.Execute(ctx, store, forged); err == nil || !strings.Contains(err.Error(), "container payload") {
			t.Errorf("%s: a worker handed it ran it: %v", why, err)
		}
	}
	if len(engine.asked) != 0 {
		t.Errorf("the engine was asked %v", engine.asked)
	}
	// Nothing was written anywhere but where the worker keeps its tasks'.
	if _, err := os.Stat(filepath.Join(c.Confined.TaskDir, "..", "escaped")); !os.IsNotExist(err) {
		t.Errorf("an input was written out of the task directory: %v", err)
	}
	// The longest name, and every character a name may have, are taken.
	for _, name := range []string{strings.Repeat("z", 64), "abc-xyz_0.9", "..a", "-"} {
		if err := checkInputs(map[string]string{name: id}); err != nil {
			t.Errorf("%q: %v", name, err)
		}
	}
}

// A job that asks for the network needs a worker that offers it, and only a
// worker whose owner allows it offers it.
func TestTheNetworkIsTheWorkerOwnersToOffer(t *testing.T) {
	ctx := context.Background()
	if needs := (Container{}).Needs([]byte(`{"image":"alpine:3.20","network":true}`)); !slices.Equal(needs, []string{NetworkLabel}) {
		t.Errorf("a job asking for the network needs %v", needs)
	}
	for _, params := range []string{`{"image":"alpine:3.20"}`, `not json`} {
		if needs := (Container{}).Needs([]byte(params)); needs != nil {
			t.Errorf("%s needs %v", params, needs)
		}
	}
	if offers := (Container{}).Offers(ctx); offers != nil {
		t.Errorf("a worker whose owner said nothing offers %v", offers)
	}
	if offers := ContainersConfined(Confinement{Network: true}).Offers(ctx); !slices.Contains(offers, NetworkLabel) {
		t.Errorf("a worker whose owner allows the network offers %v", offers)
	}
}

// A worker's owner sets the most memory and processors a task may use. A job
// may ask for less, and gets it; asking for more, or for nothing, gets the
// owner's.
func TestAWorkersOwnerSetsTheMostATaskMayUse(t *testing.T) {
	store := storage.NewMemory()
	ctx := context.Background()
	engine := &pretend{do: func([]string, io.Writer, io.Writer) (int, error) { return 0, nil }}
	c := Container{Engine: engine.engine, Confined: Confinement{MemoryMB: 512, CPUs: 2}}
	for params, want := range map[string]string{
		`{"image":"alpine:3.20","memory_mb":256,"cpus":1.5}`: "--memory 256m --cpus 1.5",
		`{"image":"alpine:3.20","memory_mb":4096,"cpus":8}`:  "--memory 512m --cpus 2",
		`{"image":"alpine:3.20"}`:                            "--memory 512m --cpus 2",
		`{"image":"alpine:3.20","memory_mb":-1,"cpus":-1}`:   "--memory 512m --cpus 2",
	} {
		payloads, _ := c.Split(ctx, store, []byte(params), 1)
		if _, err := c.Execute(ctx, store, payloads[0]); err != nil {
			t.Fatal(err)
		}
		if args := strings.Join(engine.asked[len(engine.asked)-1], " "); !strings.Contains(args, want) {
			t.Errorf("%s was run as %q, without %q", params, args, want)
		}
	}

	// And how much a read-only task may write to /tmp.
	c.Confined.ReadOnly, c.Confined.TmpMB = true, 64
	payloads, _ := c.Split(ctx, store, []byte(`{"image":"alpine:3.20"}`), 1)
	if _, err := c.Execute(ctx, store, payloads[0]); err != nil {
		t.Fatal(err)
	}
	if args := strings.Join(engine.asked[len(engine.asked)-1], " "); !strings.Contains(args, "--read-only --tmpfs /tmp:size=64m") {
		t.Errorf("a task with a sized /tmp was run as %q", args)
	}
}

// A worker's owner sets the longest a task may run, and the worker stops a
// task that runs longer, container and all, whatever its job allows.
func TestAWorkersOwnerSetsTheLongestATaskMayRun(t *testing.T) {
	store := storage.NewMemory()
	ctx := context.Background()
	var killed []string
	c := Container{Confined: Confinement{Timeout: 50 * time.Millisecond}, Engine: func(ctx context.Context, args []string, _, _ io.Writer) (int, error) {
		if args[0] == "kill" {
			killed = append(killed, args[1])
			return 0, nil
		}
		<-ctx.Done()
		return -1, ctx.Err()
	}}
	payloads, _ := c.Split(ctx, store, []byte(`{"image":"alpine:3.20","command":["sleep","3600"]}`), 1)
	if _, err := c.Execute(ctx, store, payloads[0]); err == nil || !strings.Contains(err.Error(), "longer than the 50ms this worker's owner allows a task") || len(killed) != 1 {
		t.Fatalf("a task that ran too long: %v, killed %v", err, killed)
	}

	// A task stopped for its job's own sake is stopped for that reason.
	stopped, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := c.Execute(stopped, store, payloads[0]); !errors.Is(err, context.Canceled) || len(killed) != 2 {
		t.Fatalf("a task whose job was cancelled: %v, killed %v", err, killed)
	}
}

// A worker says its containers are hardened only where they are: written
// to nowhere but a sized /tmp and /output, and run by a runtime that
// sandboxes.
func TestAWorkerSaysItsContainersAreHardenedOnlyWhereTheyAre(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		confined Confinement
		want     []string
	}{
		{Confinement{}, nil},
		{Confinement{ReadOnly: true}, nil},
		{Confinement{Runtime: "runc", ReadOnly: true, TmpMB: 64}, []string{BoundedTmpLabel}},
		{Confinement{Runtime: "runsc"}, []string{SandboxedLabel}},
		{Confinement{Runtime: "runsc-kvm"}, []string{SandboxedLabel}},
		{Confinement{Runtime: "io.containerd.runsc.v1"}, []string{SandboxedLabel}},
		{Confinement{Runtime: "io.containerd.kata.v2", ReadOnly: true, TmpMB: 64, Network: true}, []string{NetworkLabel, BoundedTmpLabel, SandboxedLabel}},
	} {
		if got := (Container{Confined: tc.confined}).Offers(ctx); !slices.Equal(got, tc.want) {
			t.Errorf("%+v offers %v, want %v", tc.confined, got, tc.want)
		}
	}
	// Every workload that runs in a container is known as one, and is held
	// in as its worker's owner said.
	owners := Confinement{Runtime: "runsc", ReadOnly: true, TmpMB: 64}
	for _, name := range []string{"container", "transcode", "md", "render"} {
		if got := ContainersConfined(owners).workloads[name].(contained).confinement(); got != owners {
			t.Errorf("%s is held in as %+v", name, got)
		}
	}
	r := WithContainers()
	for name, want := range map[string]bool{"container": true, "transcode": true, "md": true, "render": true, "primes": false, "chat": false, "no such": false} {
		if r.InContainers(name) != want {
			t.Errorf("%s runs in containers: %v", name, !want)
		}
	}
}

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
	"strings"
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
	c := Container{Engine: engine.engine}
	payloads, _ := c.Split(ctx, store, []byte(`{"image":"alpine:3.20","command":["sh","-c","work"],"input":"`+input.String()+`","env":{"B":"2","A":"1"},"memory_mb":256,"cpus":1.5}`), 2)
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
		"--network none", "--memory 256m", "--cpus 1.5", "--env A=1 --env B=2", "alpine:3.20 sh -c work"} {
		if !strings.Contains(args, want) {
			t.Errorf("the engine was asked %q, without %q", args, want)
		}
	}
	// And the task's directory is gone.
	if _, err := os.Stat(volume(engine.asked[0], "/output")); !os.IsNotExist(err) {
		t.Errorf("the task's directory was left behind: %v", err)
	}

	// A job that asks for the network gets it, and one with no limits has none.
	engine.do = func([]string, io.Writer, io.Writer) (int, error) { return 0, nil }
	open, _ := c.Split(ctx, store, []byte(`{"image":"alpine:3.20","network":true}`), 1)
	if _, err := c.Execute(ctx, store, open[0]); err != nil {
		t.Fatal(err)
	}
	if args := strings.Join(engine.asked[1], " "); strings.Contains(args, "--network") || strings.Contains(args, "--memory") || strings.Contains(args, "--cpus") {
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
		"a payload that is not one":    {[]byte("not json"), store, quiet, "container payload"},
		"an input that is not stored":  {payload(`{"image":"alpine:3.20","input":"` + missing + `"}`), store, quiet, "container input " + missing},
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
	if err := CheckContainers(context.Background()); err != nil {
		t.Errorf("on a machine with Docker: %v", err)
	}
	// Docker installed, with no daemon behind it to talk to.
	t.Setenv("DOCKER_HOST", "tcp://127.0.0.1:1")
	if err := CheckContainers(context.Background()); err == nil || !strings.Contains(err.Error(), "Docker cannot be used here") {
		t.Errorf("with no Docker daemon to reach: %v", err)
	}
	// No Docker at all.
	t.Setenv("PATH", t.TempDir())
	if err := CheckContainers(context.Background()); err == nil || !strings.Contains(err.Error(), "Docker cannot be used here") {
		t.Errorf("with no docker command: %v", err)
	}
	if got := WithContainers().Names(); !slices.Contains(got, "container") || slices.Contains(Builtin().Names(), "container") {
		t.Errorf("the registries: %v with containers, %v without", got, Builtin().Names())
	}
}

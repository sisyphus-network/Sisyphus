package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sisyphus-network/Sisyphus/packages/storage"
)

func TestAnMDJobIsAContainerJobWithTheImageAndScriptFixed(t *testing.T) {
	store := storage.NewMemory()
	ctx := context.Background()

	payloads, err := MD{}.Split(ctx, store, []byte(`{}`), 4)
	if err != nil || len(payloads) != 4 {
		t.Fatalf("split: %d payloads, %v", len(payloads), err)
	}
	var task containerTask
	json.Unmarshal(payloads[2], &task)
	if task.Image != MDImage || !strings.HasPrefix(MDImage, "sisyphus-md:") || task.Input != "" || task.Index != 2 || task.Count != 4 || task.Network {
		t.Errorf("the third task: %+v", task)
	}
	if len(task.Command) != 3 || task.Command[0] != "python" || !strings.Contains(task.Command[2], "from openmm import") {
		t.Errorf("its command: %q", task.Command)
	}
	// Unless said otherwise: ten picoseconds at room temperature, fifty frames.
	if task.Env["PICOSECONDS"] != "10" || task.Env["TEMPERATURE"] != "300" || task.Env["SEED"] != "1" || task.Env["FRAMES"] != "50" {
		t.Errorf("its settings: %v", task.Env)
	}

	molecule, _ := store.Put(ctx, strings.NewReader("ATOM ..."))
	payloads, err = MD{}.Split(ctx, store, []byte(`{"input":"`+molecule.String()+`","picoseconds":0.5,"temperature":310.5,"seed":7,"frames":5,"replicas":3}`), 8)
	if err != nil || len(payloads) != 3 {
		t.Fatalf("split as asked: %d payloads, %v", len(payloads), err)
	}
	json.Unmarshal(payloads[0], &task)
	if task.Input != molecule.String() || task.Env["PICOSECONDS"] != "0.5" || task.Env["TEMPERATURE"] != "310.5" || task.Env["SEED"] != "7" || task.Env["FRAMES"] != "5" {
		t.Errorf("settings as asked: %s %v", task.Input, task.Env)
	}

	// An Amber system is given as its two files, by name, and no PDB file.
	topology, _ := store.Put(ctx, strings.NewReader("%VERSION ..."))
	coordinates, _ := store.Put(ctx, strings.NewReader("ACE\n    22"))
	amber := `"prmtop":"` + topology.String() + `","inpcrd":"` + coordinates.String() + `"`
	payloads, err = MD{}.Split(ctx, store, []byte(`{`+amber+`,"replicas":2}`), 8)
	if err != nil || len(payloads) != 2 {
		t.Fatalf("split an Amber system: %d payloads, %v", len(payloads), err)
	}
	task = containerTask{}
	json.Unmarshal(payloads[1], &task)
	if task.Input != "" || len(task.Inputs) != 2 || task.Inputs["prmtop"] != topology.String() || task.Inputs["inpcrd"] != coordinates.String() || task.Image != MDImage {
		t.Errorf("an Amber system's task: %+v", task)
	}

	for params, want := range map[string]string{
		`not json`: "md parameters",
		`{"input":"` + molecule.String() + `",` + amber + `}`:                           "in place of input, not with it",
		`{"input":"` + molecule.String() + `","prmtop":"` + topology.String() + `"}`:    "in place of input, not with it",
		`{"inpcrd":"` + coordinates.String() + `","input":"` + molecule.String() + `"}`: "in place of input, not with it",
		`{"prmtop":"` + topology.String() + `"}`:                                        "needs both its prmtop and its inpcrd",
		`{"inpcrd":"` + coordinates.String() + `"}`:                                     "needs both its prmtop and its inpcrd",
		`{"prmtop":"somewhere","inpcrd":"` + coordinates.String() + `"}`:                "prmtop is not the CID of a stored file",
		`{"prmtop":"` + topology.String() + `","inpcrd":"elsewhere"}`:                   "inpcrd is not the CID of a stored file",
		`{"input":"somewhere"}`:   "input is not the CID of a stored PDB file",
		`{"picoseconds":-1}`:      "picoseconds is -1, and must be more than 0 and at most 1e+06",
		`{"picoseconds":1000001}`: "picoseconds is 1.000001e+06",
		`{"temperature":-5}`:      "temperature is -5, and must be more than 0 and at most 1000 kelvin",
		`{"temperature":1001}`:    "temperature is 1001",
		`{"seed":-1}`:             "seed is -1, and must be from 1 to 1073741823",
		`{"seed":1073741824}`:     "seed is 1073741824",
		`{"frames":-1}`:           "frames is -1, and must be from 1 to 100000",
		`{"frames":100001}`:       "frames is 100001",
		`{"replicas":-1}`:         "replicas is -1, and must be at least 1",
	} {
		if _, err := (MD{}).Split(ctx, store, []byte(params), 2); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v, want an error containing %q", params, err, want)
		}
	}
	if w, err := WithContainers().Get("md"); err != nil || w.Name() != "md" || !strings.Contains(WithContainers().Describe("md"), "OpenMM") {
		t.Errorf("the workload as registered: %v, %v", w, err)
	}
	if !WithContainers().InContainers("md") {
		t.Error("md is not said to run in containers")
	}
	if _, err := Builtin().Get("md"); err == nil {
		t.Error("a node that runs no containers offers molecular dynamics")
	}
	if !(MD{}).Varies() {
		t.Error("md says two honest runs give the same result")
	}
}

func TestTheMDImageIsBuiltOnlyWhereItIsMissing(t *testing.T) {
	store := storage.NewMemory()
	ctx := context.Background()
	payloads, _ := MD{}.Split(ctx, store, []byte(`{}`), 1)
	simulates := func(args []string, _, _ io.Writer) (int, error) {
		os.WriteFile(filepath.Join(volume(args, "/output"), "summary.json"), []byte(`{}`), 0o644)
		return 0, nil
	}
	has := &pretend{do: func(args []string, stdout, stderr io.Writer) (int, error) {
		if args[0] == "image" {
			return 0, nil
		}
		return simulates(args, stdout, stderr)
	}}
	if _, err := (MD{Engine: has.engine}).Execute(ctx, store, payloads[0]); err != nil {
		t.Fatal(err)
	}
	if len(has.asked) != 2 || strings.Join(has.asked[0], " ") != "image inspect "+MDImage || has.asked[1][0] != "run" {
		t.Errorf("with the image there, the engine was asked %v", has.asked)
	}

	var recipe string
	lacks := &pretend{do: func(args []string, stdout, stderr io.Writer) (int, error) {
		switch args[0] {
		case "image":
			return 1, nil
		case "build":
			text, _ := os.ReadFile(filepath.Join(args[len(args)-1], "Dockerfile"))
			recipe = string(text)
			return 0, nil
		}
		return simulates(args, stdout, stderr)
	}}
	said := new(heard)
	if _, err := (MD{Engine: lacks.engine}).Execute(WithReporter(ctx, said), store, payloads[0]); err != nil {
		t.Fatal(err)
	}
	if len(lacks.asked) != 3 || strings.Join(lacks.asked[1][:3], " ") != "build --tag "+MDImage || !strings.Contains(recipe, "pip install --no-cache-dir openmm==") {
		t.Errorf("with the image missing, the engine was asked %v with the recipe %q", lacks.asked, recipe)
	}
	if log := strings.Join(said.lines, "\n"); !strings.Contains(log, "building the image molecular dynamics runs in") {
		t.Errorf("the task's log: %q", said.lines)
	}

	broken := &pretend{do: func(args []string, _, stderr io.Writer) (int, error) {
		if args[0] == "image" {
			return 1, nil
		}
		io.WriteString(stderr, "ERROR: no matching distribution found for openmm\n")
		return 1, nil
	}}
	if _, err := (MD{Engine: broken.engine}).Execute(ctx, store, payloads[0]); err == nil || !strings.Contains(err.Error(), "build the image molecular dynamics runs in: ERROR: no matching distribution") {
		t.Errorf("a build that fails: %v", err)
	}
	none := &pretend{do: func([]string, io.Writer, io.Writer) (int, error) { return 0, errors.New("docker: not found") }}
	if _, err := (MD{Engine: none.engine}).Execute(ctx, store, payloads[0]); err == nil || !strings.Contains(err.Error(), "docker: not found") {
		t.Errorf("no engine: %v", err)
	}
}

func TestTheReplicasAreSummedUp(t *testing.T) {
	store := storage.NewMemory()
	ctx := context.Background()
	replica := func(index int, summary string) []byte {
		s, _ := store.Put(ctx, strings.NewReader(summary))
		files := map[string]string{"summary.json": s.String()}
		for _, name := range []string{"final.pdb", "energies.csv", "trajectory.dcd"} {
			c, _ := store.Put(ctx, strings.NewReader(name+" of "+summary))
			files[name] = c.String()
		}
		return mustJSON(ContainerOutput{Index: index, Files: files})
	}
	outputs := [][]byte{
		replica(0, `{"seed":3,"platform":"CPU","atoms":8867,"picoseconds":10,"ns_per_day":5.5,"potential_energy_kj_mol":-100}`),
		replica(1, `{"seed":4,"platform":"CUDA","atoms":8867,"picoseconds":10,"ns_per_day":120,"potential_energy_kj_mol":-110}`),
	}
	raw, err := MD{}.Aggregate(ctx, store, outputs)
	if err != nil {
		t.Fatal(err)
	}
	var result MDResult
	json.Unmarshal(raw, &result)
	if result.Replicas != 2 || result.Atoms != 8867 || result.Picoseconds != 10 || math.Abs(result.Nanoseconds-0.02) > 1e-12 || result.MeanEnergy != -105 || result.EnergySpread != 5 {
		t.Errorf("the summary: %+v", result)
	}
	second := result.ReplicaResults[1]
	if len(result.ReplicaResults) != 2 || second.Index != 1 || second.Seed != 4 || second.Platform != "CUDA" || second.NsPerDay != 120 || second.PotentialEnergy != -110 {
		t.Errorf("the second replica: %+v", second)
	}
	if got := readBlob(t, store, second.Trajectory); !strings.HasPrefix(got, "trajectory.dcd of ") || !strings.HasPrefix(readBlob(t, store, second.Final), "final.pdb") || !strings.HasPrefix(readBlob(t, store, second.Energies), "energies.csv") {
		t.Errorf("the second replica's files: %+v", second)
	}
	if mean, spread := meanAndSpread(nil); mean != 0 || spread != 0 {
		t.Errorf("nothing to sum: %v, %v", mean, spread)
	}

	missing := "bafkreigh2akiscaildcqabsyg3dfr6chu3fgpregiymsck7e7aqa4s52zy"
	garbled, _ := store.Put(ctx, strings.NewReader("not json"))
	for name, tt := range map[string]struct {
		outputs [][]byte
		want    string
	}{
		"an output that is not one":   {[][]byte{outputs[0], []byte("not json")}, "task 1 output"},
		"a replica with no summary":   {[][]byte{mustJSON(ContainerOutput{})}, "replica 0 left no summary"},
		"a summary that is not there": {[][]byte{mustJSON(ContainerOutput{Files: map[string]string{"summary.json": missing}})}, "replica 0's summary"},
		"a summary that is garbled":   {[][]byte{mustJSON(ContainerOutput{Files: map[string]string{"summary.json": garbled.String()}})}, "replica 0's summary: invalid character"},
	} {
		if _, err := (MD{}).Aggregate(ctx, store, tt.outputs); err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("%s: %v, want an error containing %q", name, err, tt.want)
		}
	}
}

// Two short replicas of the villin headpiece in water, simulated by OpenMM
// in real containers, held in as far as an owner can ask without another
// runtime, and summed up.
func TestRealReplicasAreSimulated(t *testing.T) {
	requireRealImages(t)
	requireDocker(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	// The image is built here if this machine has not got it, which takes
	// the network and a minute or two.
	if err := haveImage(ctx, docker, MDImage, mdDockerfile, "molecular dynamics"); err != nil {
		t.Fatal(err)
	}
	store := storage.NewMemory()
	w := MD{Confined: Confinement{ReadOnly: true}}
	payloads, err := w.Split(ctx, store, []byte(`{"picoseconds":0.2,"frames":4,"replicas":2,"seed":11}`), 2)
	if err != nil {
		t.Fatal(err)
	}
	outputs := make([][]byte, len(payloads))
	said := new(heard)
	for i, payload := range payloads {
		if outputs[i], err = w.Execute(WithReporter(ctx, said), store, payload); err != nil {
			t.Fatalf("replica %d: %v\n%s", i, err, strings.Join(said.lines, "\n"))
		}
	}
	if log := strings.Join(said.lines, "\n"); !strings.Contains(log, "replica 1: 8867 atoms, 100 steps on CPU") || !strings.Contains(log, "replica 0: done") {
		t.Errorf("the replicas' log:\n%s", log)
	}
	raw, err := w.Aggregate(ctx, store, outputs)
	if err != nil {
		t.Fatal(err)
	}
	var result MDResult
	json.Unmarshal(raw, &result)
	if result.Replicas != 2 || result.Atoms != 8867 || math.Abs(result.Picoseconds-0.2) > 1e-9 || result.MeanEnergy >= 0 {
		t.Errorf("the summary: %+v", result)
	}
	for _, r := range result.ReplicaResults {
		if r.Seed != 11+r.Index || r.Platform != "CPU" || r.NsPerDay <= 0 {
			t.Errorf("replica %d: %+v", r.Index, r)
		}
		if final := readBlob(t, store, r.Final); !strings.Contains(final, "ATOM") {
			t.Errorf("replica %d's last frame is not a PDB file: %.80q", r.Index, final)
		}
		// A header, then one line for each of the four frames.
		if lines := strings.Split(strings.TrimSpace(readBlob(t, store, r.Energies)), "\n"); len(lines) != 5 || !strings.Contains(lines[0], "Potential Energy") {
			t.Errorf("replica %d's energies: %q", r.Index, lines)
		}
		if trajectory := readBlob(t, store, r.Trajectory); len(trajectory) < 4*8867*12 {
			t.Errorf("replica %d's trajectory is %d bytes, too few for four frames", r.Index, len(trajectory))
		}
	}
	// The two started from different random states, so they end apart.
	if result.ReplicaResults[0].PotentialEnergy == result.ReplicaResults[1].PotentialEnergy {
		t.Errorf("two replicas ended with the same energy: %+v", result.ReplicaResults)
	}

	// Something that is not a molecule is the task's failure.
	nonsense, _ := store.Put(ctx, strings.NewReader("this is not a molecule"))
	bad, _ := w.Split(ctx, store, []byte(`{"input":"`+nonsense.String()+`","picoseconds":0.01}`), 1)
	if _, err := w.Execute(ctx, store, bad[0]); err == nil || !strings.Contains(err.Error(), "exited with status") {
		t.Errorf("simulating what is not a molecule: %v", err)
	}

	// A replica that asked for a graphics card and finds none fails, and
	// says why, rather than run on the processor. This image has no CUDA
	// platform, as a worker whose card is not passed through has none.
	var task containerTask
	json.Unmarshal(payloads[0], &task)
	task.Env["NEED_CUDA"] = "1"
	said = new(heard)
	if _, err := w.Execute(WithReporter(ctx, said), store, mustJSON(task)); err == nil || !strings.Contains(err.Error(), "exited with status") {
		t.Errorf("a replica that finds no graphics card: %v", err)
	}
	if log := strings.Join(said.lines, "\n"); !strings.Contains(log, "replica 0: the job asked for a graphics card, and OpenMM found no CUDA platform") || strings.Contains(log, "steps on CPU") {
		t.Errorf("the log of a replica that finds no graphics card:\n%s", log)
	}
}

// Two short replicas of an Amber system, alanine dipeptide in a vacuum,
// simulated as its topology says. The files are OpenMM's own test system,
// copied from OpenMM 8.2.0's wrappers/python/tests/systems under its MIT
// License, which is beside them in testdata/openmm.
func TestRealReplicasOfAnAmberSystemAreSimulated(t *testing.T) {
	requireDocker(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	if err := haveImage(ctx, docker, MDImage, mdDockerfile, "molecular dynamics"); err != nil {
		t.Fatal(err)
	}
	store := storage.NewMemory()
	stored := map[string]string{}
	for _, name := range []string{"prmtop", "inpcrd"} {
		file, err := os.Open(filepath.Join("testdata", "openmm", "alanine-dipeptide-implicit."+name))
		if err != nil {
			t.Fatal(err)
		}
		c, err := store.Put(ctx, file)
		file.Close()
		if err != nil {
			t.Fatal(err)
		}
		stored[name] = c.String()
	}
	w := MD{Confined: Confinement{ReadOnly: true}}
	payloads, err := w.Split(ctx, store, mustJSON(map[string]any{"prmtop": stored["prmtop"], "inpcrd": stored["inpcrd"], "picoseconds": 0.1, "frames": 5, "replicas": 2, "seed": 3}), 8)
	if err != nil || len(payloads) != 2 {
		t.Fatalf("split: %d payloads, %v", len(payloads), err)
	}
	outputs := make([][]byte, len(payloads))
	said := new(heard)
	for i, payload := range payloads {
		if outputs[i], err = w.Execute(WithReporter(ctx, said), store, payload); err != nil {
			t.Fatalf("replica %d: %v\n%s", i, err, strings.Join(said.lines, "\n"))
		}
	}
	// 22 atoms, as the topology has them: nothing was added to them.
	if log := strings.Join(said.lines, "\n"); !strings.Contains(log, "replica 1: 22 atoms, 50 steps on CPU") || !strings.Contains(log, "replica 0: done") {
		t.Errorf("the replicas' log:\n%s", log)
	}
	raw, err := w.Aggregate(ctx, store, outputs)
	if err != nil {
		t.Fatal(err)
	}
	var result MDResult
	json.Unmarshal(raw, &result)
	if result.Replicas != 2 || result.Atoms != 22 || math.Abs(result.Picoseconds-0.1) > 1e-9 || math.Abs(result.Nanoseconds-0.0002) > 1e-12 {
		t.Errorf("the summary: %+v", result)
	}
	for _, r := range result.ReplicaResults {
		if r.Seed != 3+r.Index || r.Platform != "CPU" || r.NsPerDay <= 0 || math.IsNaN(r.PotentialEnergy) {
			t.Errorf("replica %d: %+v", r.Index, r)
		}
		// The last frame has the topology's residues and its 22 atoms.
		final := readBlob(t, store, r.Final)
		atoms := 0
		for _, line := range strings.Split(final, "\n") {
			if strings.HasPrefix(line, "ATOM") || strings.HasPrefix(line, "HETATM") {
				atoms++
			}
		}
		if atoms != 22 || !strings.Contains(final, "ALA") || !strings.Contains(final, "ACE") || !strings.Contains(final, "NME") {
			t.Errorf("replica %d's last frame has %d atoms: %.200q", r.Index, atoms, final)
		}
		if lines := strings.Split(strings.TrimSpace(readBlob(t, store, r.Energies)), "\n"); len(lines) != 6 || !strings.Contains(lines[0], "Potential Energy") {
			t.Errorf("replica %d's energies: %q", r.Index, lines)
		}
		if trajectory := readBlob(t, store, r.Trajectory); len(trajectory) < 5*22*12 {
			t.Errorf("replica %d's trajectory is %d bytes, too few for five frames", r.Index, len(trajectory))
		}
	}
}

func TestAnMDJobOnAGraphicsCardRunsInTheCUDAImageWithACard(t *testing.T) {
	store := storage.NewMemory()
	ctx := context.Background()

	payloads, err := MD{}.Split(ctx, store, []byte(`{"gpu":true,"replicas":2}`), 8)
	if err != nil || len(payloads) != 2 {
		t.Fatalf("split: %d payloads, %v", len(payloads), err)
	}
	var task containerTask
	json.Unmarshal(payloads[1], &task)
	if task.Image != MDCUDAImage || MDCUDAImage == MDImage || !strings.HasPrefix(MDCUDAImage, "sisyphus-md:") || task.GPUs != 1 || task.Env["NEED_CUDA"] != "1" {
		t.Errorf("a task on a graphics card: %+v", task)
	}
	// On the processor, as before: no card, and nothing that asks for one.
	payloads, _ = MD{}.Split(ctx, store, []byte(`{"gpu":false}`), 1)
	task = containerTask{}
	json.Unmarshal(payloads[0], &task)
	if _, asked := task.Env["NEED_CUDA"]; task.Image != MDImage || task.GPUs != 0 || asked {
		t.Errorf("a task on the processor: %+v", task)
	}

	// The coordinator is told each task holds one card.
	for params, want := range map[string]int{`{"gpu":true}`: 1, `{"gpu":false}`: 0, `{}`: 0, `not json`: 0} {
		if got := WithContainers().GPUsNeeded("md", []byte(params)); got != want {
			t.Errorf("md %s needs %d graphics cards, want %d", params, got, want)
		}
	}
	if got := WithContainers().GPUsNeeded("primes", []byte(`{"gpu":true}`)); got != 0 {
		t.Errorf("primes needs %d graphics cards", got)
	}
	if !slices.ContainsFunc(MD{}.Form(), func(f Field) bool { return f.Name == "gpu" && f.Kind == KindToggle && f.Default == false }) {
		t.Errorf("the form has no graphics card toggle: %+v", MD{}.Form())
	}

	// The worker builds the CUDA image, once, and runs the task with a card.
	var recipe string
	lacks := &pretend{do: func(args []string, _, _ io.Writer) (int, error) {
		switch args[0] {
		case "image":
			return 1, nil
		case "build":
			text, _ := os.ReadFile(filepath.Join(args[len(args)-1], "Dockerfile"))
			recipe = string(text)
			return 0, nil
		}
		os.WriteFile(filepath.Join(volume(args, "/output"), "summary.json"), []byte(`{"platform":"CUDA"}`), 0o644)
		return 0, nil
	}}
	payloads, _ = MD{}.Split(ctx, store, []byte(`{"gpu":true}`), 1)
	said := new(heard)
	if _, err := (MD{Engine: lacks.engine}).Execute(WithReporter(ctx, said), store, payloads[0]); err != nil {
		t.Fatal(err)
	}
	if len(lacks.asked) != 3 || strings.Join(lacks.asked[0], " ") != "image inspect "+MDCUDAImage || strings.Join(lacks.asked[1][:3], " ") != "build --tag "+MDCUDAImage {
		t.Errorf("with the CUDA image missing, the engine was asked %v", lacks.asked)
	}
	if !strings.Contains(recipe, "FROM python:3.12-slim") || !strings.Contains(recipe, "openmm[cuda12]==8.2.0") || !strings.Contains(recipe, "OpenMM-CUDA-12==8.2.0") {
		t.Errorf("the CUDA image's recipe: %q", recipe)
	}
	run := strings.Join(lacks.asked[2], " ")
	if !strings.Contains(run, "--gpus 1") || !strings.Contains(run, "--env NEED_CUDA=1") || !strings.Contains(run, " "+MDCUDAImage+" python -c") {
		t.Errorf("the task was run as %q", run)
	}
	if log := strings.Join(said.lines, "\n"); !strings.Contains(log, "building the image molecular dynamics on a graphics card runs in") {
		t.Errorf("the task's log: %q", said.lines)
	}
}

func TestAContainerJobNeedsTheGraphicsCardsItAsksFor(t *testing.T) {
	for params, want := range map[string]int{`{"image":"a","gpus":2}`: 2, `{"image":"a"}`: 0, `{"image":"a","gpus":-1}`: 0, `not json`: 0} {
		if got := WithContainers().GPUsNeeded("container", []byte(params)); got != want {
			t.Errorf("container %s needs %d graphics cards, want %d", params, got, want)
		}
	}
}

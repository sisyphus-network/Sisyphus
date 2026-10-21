package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"os"
	"path/filepath"
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

	for params, want := range map[string]string{
		`not json`:                "md parameters",
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
}

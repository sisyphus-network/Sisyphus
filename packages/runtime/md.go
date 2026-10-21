package runtime

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"strconv"

	"github.com/ipfs/go-cid"
)

// MD runs molecular dynamics with OpenMM: one simulation of the same
// molecule for each task, each a replica of its own, from its own random
// start, and the replicas summed up at the end. Replicas are how a long
// simulation is shared out: many short, independent runs sample more of
// what a molecule does than one long one in the same time.
//
// Like Transcode, it is the container workload with the image and the
// command fixed, so it runs only on workers that run containers. The image
// is built on a worker the first time it is needed.
type MD struct {
	// Engine runs the container engine, as Container's does.
	Engine func(ctx context.Context, args []string, stdout, stderr io.Writer) (status int, err error)
	// Confined holds its containers in as Container's does.
	Confined Confinement
}

// MDParams are an md job's parameters.
type MDParams struct {
	// Input is the CID of a stored PDB file of the molecule. Empty means
	// the villin headpiece in water that OpenMM comes with, 8,867 atoms.
	// A molecule without water is put in a box of it, with salt to make
	// it neutral; hydrogens it lacks are added.
	Input string `json:"input,omitempty"`
	// Prmtop and Inpcrd are the CIDs of a stored Amber topology and the
	// coordinates that go with it, in place of Input: a system prepared
	// with Amber's tools, run as it is, with the force field its topology
	// holds. Both are given or neither, and not with Input. A system with
	// a box in its coordinates is simulated with periodic boundaries and
	// PME; one without, in a vacuum or implicit solvent, with no cutoff.
	Prmtop string `json:"prmtop,omitempty"`
	Inpcrd string `json:"inpcrd,omitempty"`
	// Picoseconds is how long each replica is simulated. Zero means 10.
	Picoseconds float64 `json:"picoseconds,omitempty"`
	// Temperature is in kelvin. Zero means 300.
	Temperature float64 `json:"temperature,omitempty"`
	// Seed is the first replica's random seed; the others follow from it.
	// Zero means 1.
	Seed int `json:"seed,omitempty"`
	// Frames is how many snapshots of each replica to keep. Zero means 50.
	Frames int `json:"frames,omitempty"`
	// Replicas is how many simulations to run. Zero leaves it to the pool.
	Replicas int `json:"replicas,omitempty"`
	// GPU runs each replica on a graphics card, with OpenMM's CUDA
	// platform, in an image that has it. Only workers with a card are
	// given such a job, and a replica that finds no card fails rather
	// than run on the processor.
	GPU bool `json:"gpu,omitempty"`
}

// MDReplica is what one replica came to.
type MDReplica struct {
	Index int `json:"index"`
	Seed  int `json:"seed"`
	// Platform is where OpenMM ran it, CPU or CUDA.
	Platform string `json:"platform"`
	// NsPerDay is how fast it ran, in nanoseconds simulated a day.
	NsPerDay float64 `json:"ns_per_day"`
	// PotentialEnergy is the molecule's at the end, in kJ/mol.
	PotentialEnergy float64 `json:"potential_energy_kj_mol"`
	// Final, Energies and Trajectory are CIDs: the last frame as PDB, the
	// energies and temperature as it went as CSV, and its frames as DCD.
	Final      string `json:"final"`
	Energies   string `json:"energies"`
	Trajectory string `json:"trajectory"`
}

// MDResult is an md job's result.
type MDResult struct {
	Replicas       int         `json:"replicas"`
	Atoms          int         `json:"atoms"`
	Picoseconds    float64     `json:"picoseconds_each"`
	Nanoseconds    float64     `json:"total_nanoseconds"`
	MeanEnergy     float64     `json:"mean_potential_energy_kj_mol"`
	EnergySpread   float64     `json:"potential_energy_stdev_kj_mol"`
	ReplicaResults []MDReplica `json:"runs"`
}

// mdDockerfile is the whole of the image molecular dynamics runs in.
const mdDockerfile = "FROM python:3.12-slim\nRUN pip install --no-cache-dir openmm==8.2.0\n"

// mdCUDADockerfile is the image molecular dynamics runs in on a graphics
// card: the same, with OpenMM's CUDA platform. The platform is a package of
// its own, which the extra pulls in; it is pinned too, since the extra
// leaves its version open and a platform of another version than OpenMM's
// does not load.
const mdCUDADockerfile = "FROM python:3.12-slim\nRUN pip install --no-cache-dir openmm[cuda12]==8.2.0 OpenMM-CUDA-12==8.2.0\n"

// MDImage and MDCUDAImage name those images, by what they are made from,
// as TranscodeImage does.
var (
	MDImage     = mdImage(mdDockerfile)
	MDCUDAImage = mdImage(mdCUDADockerfile)
)

func mdImage(dockerfile string) string {
	sum := sha256.Sum256([]byte(dockerfile + mdScript))
	return "sisyphus-md:" + hex.EncodeToString(sum[:6])
}

// mdScript is what each task runs: it prepares the molecule, settles it,
// and simulates it with the Amber force field and TIP3P water, two
// femtoseconds a step, recording as it goes. An Amber system, given as
// /input/prmtop and /input/inpcrd, is already prepared, and is simulated
// as its topology says.
const mdScript = `import json, os, time
from openmm import app, unit, LangevinMiddleIntegrator, Platform
env = os.environ
index = int(env["SISYPHUS_TASK_INDEX"])
seed = int(env["SEED"]) + index
temperature = float(env["TEMPERATURE"]) * unit.kelvin
steps = max(1, round(float(env["PICOSECONDS"]) / 0.002))
interval = max(1, steps // int(env["FRAMES"]))
box = None
if os.path.exists("/input/prmtop"):
    prmtop = app.AmberPrmtopFile("/input/prmtop")
    inpcrd = app.AmberInpcrdFile("/input/inpcrd")
    topology, positions, box = prmtop.topology, inpcrd.positions, inpcrd.boxVectors
    if box is not None:
        topology.setPeriodicBoxVectors(box)
    system = prmtop.createSystem(nonbondedMethod=app.NoCutoff if box is None else app.PME, nonbondedCutoff=1 * unit.nanometer, constraints=app.HBonds)
else:
    path = "/input/data" if os.path.exists("/input/data") else os.path.join(os.path.dirname(app.__file__), "data", "test.pdb")
    pdb = app.PDBFile(path)
    forcefield = app.ForceField("amber14-all.xml", "amber14/tip3p.xml")
    model = app.Modeller(pdb.topology, pdb.positions)
    model.addHydrogens(forcefield)
    if not any(r.name == "HOH" for r in model.topology.residues()):
        model.addSolvent(forcefield, padding=1 * unit.nanometer, neutralize=True)
    topology, positions = model.topology, model.positions
    system = forcefield.createSystem(topology, nonbondedMethod=app.PME, nonbondedCutoff=1 * unit.nanometer, constraints=app.HBonds)
integrator = LangevinMiddleIntegrator(temperature, 1 / unit.picosecond, 0.002 * unit.picoseconds)
integrator.setRandomNumberSeed(seed)
names = [Platform.getPlatform(i).getName() for i in range(Platform.getNumPlatforms())]
if env.get("NEED_CUDA") and "CUDA" not in names:
    raise SystemExit("replica %d: the job asked for a graphics card, and OpenMM found no CUDA platform: %s" % (index, "; ".join(Platform.getPluginLoadFailures()) or "no plugin for it"))
platform = Platform.getPlatformByName("CUDA" if "CUDA" in names else "CPU")
simulation = app.Simulation(topology, system, integrator, platform)
if box is not None:
    simulation.context.setPeriodicBoxVectors(*box)
simulation.context.setPositions(positions)
print(f"replica {index}: {topology.getNumAtoms()} atoms, {steps} steps on {platform.getName()}", flush=True)
simulation.minimizeEnergy()
simulation.context.setVelocitiesToTemperature(temperature, seed)
simulation.reporters.append(app.DCDReporter("/output/trajectory.dcd", interval))
simulation.reporters.append(app.StateDataReporter("/output/energies.csv", interval, step=True, time=True, potentialEnergy=True, kineticEnergy=True, temperature=True))
started = time.time()
simulation.step(steps)
took = max(time.time() - started, 1e-9)
state = simulation.context.getState(getEnergy=True, getPositions=True)
with open("/output/final.pdb", "w") as out:
    app.PDBFile.writeFile(topology, state.getPositions(), out)
energy = state.getPotentialEnergy().value_in_unit(unit.kilojoule_per_mole)
summary = {"seed": seed, "platform": platform.getName(), "atoms": topology.getNumAtoms(), "picoseconds": steps * 0.002,
           "ns_per_day": steps * 0.002e-3 / took * 86400, "potential_energy_kj_mol": energy}
with open("/output/summary.json", "w") as out:
    json.dump(summary, out)
print(f"replica {index}: done, {summary['ns_per_day']:.1f} ns/day, {energy:.1f} kJ/mol", flush=True)
`

// mdLimit is the longest a replica may be simulated, in picoseconds: a
// microsecond, which is weeks on most machines.
const mdLimit = 1e6

func (MD) Name() string { return "md" }

func (MD) Describe() string {
	return `Simulates a molecule with OpenMM molecular dynamics, as replicas from different random starts run on different workers, and sums them up. Parameters: {"input": "<optional CID of a stored PDB file; without it, the villin headpiece in water>", "prmtop": "<CID of a stored Amber topology, with inpcrd, in place of input>", "inpcrd": "<CID of the stored Amber coordinates that go with prmtop>", "picoseconds": <how long each replica is simulated, default 10>, "temperature": <kelvin, default 300>, "seed": <optional first random seed>, "frames": <snapshots kept of each replica, default 50>, "replicas": <optional number of replicas>, "gpu": <true to run each replica on a graphics card, on workers that have one>}. Result: {"replicas": <n>, "atoms": <n>, "picoseconds_each": <ps>, "total_nanoseconds": <ns>, "mean_potential_energy_kj_mol": <e>, "potential_energy_stdev_kj_mol": <e>, "runs": [{"index", "seed", "platform", "ns_per_day", "potential_energy_kj_mol", "final": "<CID of the last frame, PDB>", "energies": "<CID, CSV>", "trajectory": "<CID, DCD>"}]}.`
}

func (MD) Form() []Field {
	return []Field{
		{Name: "input", Kind: KindFile, Label: "Molecule", Help: "A stored PDB file of the molecule, or none for the villin headpiece in water."},
		{Name: "prmtop", Kind: KindFile, Label: "Amber topology (prmtop)", Help: "A system prepared with Amber's tools, in place of a molecule: its prmtop file, given together with its inpcrd."},
		{Name: "inpcrd", Kind: KindFile, Label: "Amber coordinates (inpcrd)", Help: "The inpcrd file of the coordinates that go with the Amber topology, given together with it."},
		{Name: "picoseconds", Kind: KindNumber, Label: "Length", Help: "How long to simulate each replica, in picoseconds.", Default: 10, Minimum: bound(0.002), Maximum: bound(mdLimit)},
		{Name: "temperature", Kind: KindNumber, Label: "Temperature", Help: "The temperature to hold the molecule at, in kelvin.", Default: 300, Minimum: bound(1), Maximum: bound(1000)},
		{Name: "replicas", Kind: KindInteger, Label: "Replicas", Help: "How many simulations to run side by side, or 0 to leave it to the pool.", Default: 0, Minimum: bound(0), Maximum: bound(MaxTasks)},
		{Name: "frames", Kind: KindInteger, Label: "Frames", Help: "How many snapshots of each replica to keep.", Default: 50, Minimum: bound(1), Maximum: bound(100000)},
		{Name: "seed", Kind: KindInteger, Label: "Seed", Help: "The first replica's random seed; the others follow from it.", Default: 1, Minimum: bound(1), Maximum: bound(math.MaxInt32 / 2)},
		{Name: "gpu", Kind: KindToggle, Label: "Use a graphics card", Help: "Run each replica on a worker's graphics card, one at a time on each card, many times faster than on its processor.", Default: false},
	}
}

func (MD) Split(ctx context.Context, blobs Blobs, params []byte, parts int) ([][]byte, error) {
	var p MDParams
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, fmt.Errorf("md parameters: %w", err)
	}
	if p.Input != "" {
		if _, err := cid.Decode(p.Input); err != nil {
			return nil, fmt.Errorf("md parameters: input is not the CID of a stored PDB file: %w", err)
		}
	}
	var amber map[string]string
	switch {
	case p.Input != "" && (p.Prmtop != "" || p.Inpcrd != ""):
		return nil, errors.New("md parameters: an Amber system's prmtop and inpcrd are given in place of input, not with it")
	case (p.Prmtop == "") != (p.Inpcrd == ""):
		return nil, errors.New("md parameters: an Amber system needs both its prmtop and its inpcrd")
	case p.Prmtop != "":
		for name, id := range map[string]string{"prmtop": p.Prmtop, "inpcrd": p.Inpcrd} {
			if _, err := cid.Decode(id); err != nil {
				return nil, fmt.Errorf("md parameters: %s is not the CID of a stored file: %w", name, err)
			}
		}
		amber = map[string]string{"prmtop": p.Prmtop, "inpcrd": p.Inpcrd}
	}
	if p.Picoseconds == 0 {
		p.Picoseconds = 10
	}
	if p.Temperature == 0 {
		p.Temperature = 300
	}
	if p.Seed == 0 {
		p.Seed = 1
	}
	if p.Frames == 0 {
		p.Frames = 50
	}
	switch {
	case !(p.Picoseconds > 0 && p.Picoseconds <= mdLimit):
		return nil, fmt.Errorf("md parameters: picoseconds is %g, and must be more than 0 and at most %g", p.Picoseconds, float64(mdLimit))
	case !(p.Temperature > 0 && p.Temperature <= 1000):
		return nil, fmt.Errorf("md parameters: temperature is %g, and must be more than 0 and at most 1000 kelvin", p.Temperature)
	case p.Seed < 0 || p.Seed > math.MaxInt32/2:
		return nil, fmt.Errorf("md parameters: seed is %d, and must be from 1 to %d", p.Seed, math.MaxInt32/2)
	case p.Frames < 0 || p.Frames > 100000:
		return nil, fmt.Errorf("md parameters: frames is %d, and must be from 1 to 100000", p.Frames)
	case p.Replicas < 0:
		return nil, fmt.Errorf("md parameters: replicas is %d, and must be at least 1, or 0 to leave it to the pool", p.Replicas)
	}
	c := ContainerParams{
		Image: MDImage, Command: []string{"python", "-c", mdScript},
		Input: p.Input, Inputs: amber, Tasks: p.Replicas,
		Env: map[string]string{
			"PICOSECONDS": strconv.FormatFloat(p.Picoseconds, 'g', -1, 64),
			"TEMPERATURE": strconv.FormatFloat(p.Temperature, 'g', -1, 64),
			"SEED":        strconv.Itoa(p.Seed), "FRAMES": strconv.Itoa(p.Frames),
		},
	}
	if p.GPU {
		c.Image, c.GPUs, c.Env["NEED_CUDA"] = MDCUDAImage, 1, "1"
	}
	return Container{}.Split(ctx, blobs, mustJSON(c), parts)
}

// GPUsNeeded is one graphics card for each replica of a job that asks to
// run on one.
func (MD) GPUsNeeded(params []byte) int {
	var p MDParams
	if json.Unmarshal(params, &p) != nil || !p.GPU {
		return 0
	}
	return 1
}

func (m MD) Execute(ctx context.Context, blobs Blobs, payload []byte) ([]byte, error) {
	engine := m.Engine
	if engine == nil {
		engine = docker
	}
	// The task says which image it runs in. One that cannot be read is
	// Container's to refuse, after the image is had.
	var task containerTask
	json.Unmarshal(payload, &task)
	tag, dockerfile, what := MDImage, mdDockerfile, "molecular dynamics"
	if task.GPUs > 0 {
		tag, dockerfile, what = MDCUDAImage, mdCUDADockerfile, "molecular dynamics on a graphics card"
	}
	if err := haveImage(ctx, engine, tag, dockerfile, what); err != nil {
		return nil, err
	}
	return Container{Engine: engine, Confined: m.Confined}.Execute(ctx, blobs, payload)
}

// Varies says that replicas can differ between runs: OpenMM's results
// depend on the machine it runs on and how many threads it uses.
func (MD) Varies() bool { return true }

func (MD) Aggregate(ctx context.Context, blobs Blobs, outputs [][]byte) ([]byte, error) {
	var result MDResult
	var energies []float64
	for i, output := range outputs {
		var out ContainerOutput
		if err := json.Unmarshal(output, &out); err != nil {
			return nil, fmt.Errorf("task %d output: %w", i, err)
		}
		c, err := cid.Decode(out.Files["summary.json"])
		if err != nil {
			return nil, fmt.Errorf("replica %d left no summary", i)
		}
		blob, err := blobs.Open(ctx, c)
		if err != nil {
			return nil, fmt.Errorf("replica %d's summary: %w", i, err)
		}
		var summary struct {
			MDReplica
			Atoms       int     `json:"atoms"`
			Picoseconds float64 `json:"picoseconds"`
		}
		err = json.NewDecoder(io.LimitReader(blob, 1<<20)).Decode(&summary)
		blob.Close()
		if err != nil {
			return nil, fmt.Errorf("replica %d's summary: %w", i, err)
		}
		replica := summary.MDReplica
		replica.Index = out.Index
		replica.Final, replica.Energies, replica.Trajectory = out.Files["final.pdb"], out.Files["energies.csv"], out.Files["trajectory.dcd"]
		result.ReplicaResults = append(result.ReplicaResults, replica)
		result.Atoms, result.Picoseconds = summary.Atoms, summary.Picoseconds
		result.Nanoseconds += summary.Picoseconds / 1000
		energies = append(energies, replica.PotentialEnergy)
	}
	result.Replicas = len(outputs)
	result.MeanEnergy, result.EnergySpread = meanAndSpread(energies)
	return mustJSON(result), nil
}

// meanAndSpread returns the mean of values and their standard deviation,
// or zeros for none.
func meanAndSpread(values []float64) (mean, spread float64) {
	if len(values) == 0 {
		return 0, 0
	}
	for _, v := range values {
		mean += v
	}
	mean /= float64(len(values))
	for _, v := range values {
		spread += (v - mean) * (v - mean)
	}
	return mean, math.Sqrt(spread / float64(len(values)))
}

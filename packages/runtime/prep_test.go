package runtime

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ipfs/go-cid"

	"github.com/sisyphus-network/Sisyphus/packages/storage"
)

// prepMolecule is a PDB file of alanine dipeptide's heavy atoms, as a
// crystal structure would have it, without its hydrogens.
const prepMolecule = `REMARK   made by Sisyphus's prep test
ATOM      1  CH3 ACE     1       2.000   2.090   0.000  1.00  0.00
ATOM      2  C   ACE     1       3.427   2.641  -0.000  1.00  0.00
ATOM      3  O   ACE     1       4.391   1.877  -0.000  1.00  0.00
ATOM      4  N   ALA     2       3.555   3.970  -0.000  1.00  0.00
ATOM      5  CA  ALA     2       4.853   4.614  -0.000  1.00  0.00
ATOM      6  CB  ALA     2       5.661   4.221  -1.232  1.00  0.00
ATOM      7  C   ALA     2       4.713   6.129   0.000  1.00  0.00
ATOM      8  O   ALA     2       3.601   6.653   0.000  1.00  0.00
ATOM      9  N   NME     3       5.846   6.835   0.000  1.00  0.00
ATOM     10  C   NME     3       5.846   8.284   0.000  1.00  0.00
TER
END
`

func TestAPrepJobIsAContainerJobWithTheImageAndScriptFixed(t *testing.T) {
	store := storage.NewMemory()
	ctx := context.Background()
	molecule := storedCID(t, store, prepMolecule)
	other := storedCID(t, store, "HETATM    1  O   HOH A   1       0.000   0.000   0.000\r\n")

	// One file is one system, in one task, given it as system-000.pdb.
	payloads, err := Prep{}.Split(ctx, store, []byte(`{"input":"`+molecule+`"}`), 4)
	if err != nil || len(payloads) != 1 {
		t.Fatalf("split: %d payloads, %v", len(payloads), err)
	}
	task := trajTaskOf(payloads[0])
	if task.Image != PrepImage || !strings.HasPrefix(PrepImage, "sisyphus-prep:") || task.Index != 0 || task.Count != 1 || task.Network || task.Input != "" {
		t.Errorf("the task: %+v", task)
	}
	if !equalEnv(task.Inputs, map[string]string{"system-000.pdb": molecule}) {
		t.Errorf("its inputs: %v", task.Inputs)
	}
	if len(task.Command) != 3 || task.Command[0] != "bash" || task.Command[2] != prepScript || !strings.Contains(prepScript, "loadpdb $pdb") || strings.Contains(prepScript, "source $pdb") {
		t.Errorf("its command: %q", task.Command)
	}
	// Unless said otherwise: ff19SB, in OPC water, in a truncated
	// octahedron 10 ångströms past the molecule, made neutral.
	want := map[string]string{
		"SYSTEMS": "1", "FORCEFIELD": "ff19SB", "WATER": "opc", "BOX": "10", "SHAPE": "octahedron", "SALT_MM": "0",
		"SOURCES": "source leaprc.protein.ff19SB\nsource leaprc.DNA.OL21\nsource leaprc.RNA.OL3\nsource leaprc.water.opc",
		"SOLVATE": "solvateoct mol OPCBOX 10",
	}
	if !equalEnv(task.Env, want) {
		t.Errorf("its settings: %q", task.Env)
	}

	// Several files are shared out in runs, each task given only its own.
	payloads, err = Prep{}.Split(ctx, store, []byte(`{"inputs":["`+molecule+`","`+other+`","`+molecule+`"],"forcefield":"ff14SB","box":12.5,"box_shape":"box","salt_mm":150}`), 2)
	if err != nil || len(payloads) != 2 {
		t.Fatalf("several: %d payloads, %v", len(payloads), err)
	}
	for i, want := range []map[string]string{
		{"system-000.pdb": molecule},
		{"system-001.pdb": other, "system-002.pdb": molecule},
	} {
		task = trajTaskOf(payloads[i])
		if !equalEnv(task.Inputs, want) || task.Env["SYSTEMS"] != "3" || task.Count != 2 {
			t.Errorf("task %d of several: %v, %v", i, task.Inputs, task.Env)
		}
	}
	// ff14SB is in TIP3P unless said otherwise.
	if task.Env["WATER"] != "tip3p" || task.Env["SOLVATE"] != "solvatebox mol TIP3PBOX 12.5" || task.Env["SALT_MM"] != "150" || !strings.HasPrefix(task.Env["SOURCES"], "source leaprc.protein.ff14SB\n") {
		t.Errorf("ff14SB's settings: %q", task.Env)
	}
	// No water is no box and no water's commands.
	payloads, _ = Prep{}.Split(ctx, store, []byte(`{"input":"`+molecule+`","water":"none"}`), 1)
	if task = trajTaskOf(payloads[0]); task.Env["SOLVATE"] != "" || strings.Contains(task.Env["SOURCES"], "water") {
		t.Errorf("no water: %q", task.Env)
	}
	// No more tasks than systems, and enough that none is given more than
	// a task may have.
	many := make([]string, 40)
	for i := range many {
		many[i] = molecule
	}
	for tasks, want := range map[int]int{0: 3, 2: 3, 8: 8, 100: 40} {
		payloads, err = Prep{}.Split(ctx, store, mustJSON(map[string]any{"inputs": many, "tasks": tasks}), 1)
		if err != nil || len(payloads) != want {
			t.Fatalf("forty systems in %d tasks: %d payloads, %v", tasks, len(payloads), err)
		}
		given := 0
		for i, payload := range payloads {
			task = trajTaskOf(payload)
			given += len(task.Inputs)
			if len(task.Inputs) == 0 || checkInputs(task.Inputs) != nil {
				t.Errorf("task %d of forty systems in %d is given %d inputs", i, want, len(task.Inputs))
			}
		}
		if given != 40 {
			t.Errorf("forty systems in %d tasks are given out as %d", want, given)
		}
	}

	missing := "bafkreigh2akiscaildcqabsyg3dfr6chu3fgpregiymsck7e7aqa4s52zy"
	one := `"input":"` + molecule + `"`
	tooMany := make([]string, prepMostSystems+1)
	for i := range tooMany {
		tooMany[i] = molecule
	}
	for params, want := range map[string]string{
		`not json`: "prep parameters",
		`{}`:       "no PDB file is given",
		`{` + one + `,"inputs":["` + molecule + `"]}`:       "inputs are given in place of input, not with it",
		string(mustJSON(map[string]any{"inputs": tooMany})): "65 PDB files are given; a job can have at most 64",
		`{"input":"x"}`:                                           "input 0 is not the CID of a stored PDB file",
		`{"inputs":["` + molecule + `","x"]}`:                     "input 1 is not the CID",
		`{"input":"` + missing + `"}`:                             "prep input 0, " + missing,
		`{` + one + `,"forcefield":"ff99"}`:                       `forcefield is "ff99", and must be one of ff19SB, ff14SB`,
		`{` + one + `,"forcefield":"ff19sb"}`:                     `forcefield is "ff19sb"`,
		`{` + one + `,"water":"spce"}`:                            `water is "spce", and must be one of opc, tip3p, none`,
		`{` + one + `,"water":"source leaprc.gaff"}`:              `water is "source leaprc.gaff"`,
		`{` + one + `,"box_shape":"sphere"}`:                      `box_shape is "sphere", and must be one of octahedron, box`,
		`{` + one + `,"box":2.9}`:                                 "box is 2.9, and must be from 3 to 50 ångströms",
		`{` + one + `,"box":-10}`:                                 "box is -10",
		`{` + one + `,"box":50.5}`:                                "box is 50.5",
		`{` + one + `,"salt_mm":-1}`:                              "salt_mm is -1, and must be from 0 to 1000",
		`{` + one + `,"salt_mm":1001}`:                            "salt_mm is 1001",
		`{` + one + `,"water":"none","salt_mm":150}`:              "salt_mm is 150, and a molecule in no water is given no salt",
		`{` + one + `,"box":"10; system rm"}`:                     "prep parameters",
		`{` + one + `,"tasks":-1}`:                                "tasks is -1, and must be from 1 to 10000",
		`{` + one + `,"tasks":10001}`:                             "tasks is 10001",
		`{` + one + `,"water":"tip3p","box":3,"salt_mm":1000}`:    "",
		`{` + one + `,"forcefield":"ff14SB","water":"opc"}`:       "",
		`{` + one + `,"water":"none","box_shape":"box","box":50}`: "",
	} {
		_, err := (Prep{}).Split(ctx, store, []byte(params), 2)
		if want == "" && err != nil || want != "" && (err == nil || !strings.Contains(err.Error(), want)) {
			t.Errorf("%s: %v, want an error containing %q", params, err, want)
		}
	}

	if w, err := WithContainers().Get("prep"); err != nil || w.Name() != "prep" || !strings.Contains(WithContainers().Describe("prep"), "tleap") {
		t.Errorf("the workload as registered: %v, %v", w, err)
	}
	if !WithContainers().InContainers("prep") {
		t.Error("prep is not said to run in containers")
	}
	if _, err := Builtin().Get("prep"); err == nil {
		t.Error("a node that runs no containers offers to prepare systems")
	}
	if !(Prep{}).Varies() {
		t.Error("prep says two honest runs give the same result, though its image is built from conda-forge as it is each day")
	}
}

// sized is a blob store whose blobs say they are of the size it says.
type sized struct {
	Blobs
	size uint64
}

func (s sized) Open(ctx context.Context, c cid.Cid) (storage.Blob, error) {
	blob, err := s.Blobs.Open(ctx, c)
	return sizedBlob{blob, s.size}, err
}

type sizedBlob struct {
	storage.Blob
	size uint64
}

func (s sizedBlob) Size() uint64 { return s.size }

// A PDB file is data, and only a PDB file of one model is taken: text,
// of lines of a sane length, with atoms in it.
func TestOnlyPDBFilesArePrepared(t *testing.T) {
	store := storage.NewMemory()
	ctx := context.Background()
	for content, want := range map[string]string{
		prepMolecule: "",
		"ATOM      1  N   ALA A   1       0.000   0.000   0.000  1.00  0.00\n":      "",
		"HETATM    1 NA    NA A   1       0.000   0.000   0.000  1.00  0.00\r\nEND": "",
		"MODEL        1\nATOM      1  N   ALA A   1\nENDMDL\n":                      "",
		"REMARK nothing\nEND\n": "it has no ATOM or HETATM records",
		"":                      "it has no ATOM or HETATM records",
		"ATOMS of no kind\n":    "it has no ATOM or HETATM records",
		"ATOM      1  N   ALA A   1\nsource leaprc.gaff\x00\n":             "line 2 has characters a PDB file does not",
		"ATOM      1  N   ALÄ A   1\n":                                     "line 1 has characters",
		"\x89PNG\r\n\x1a\n":                                                "line 1 has characters",
		"ATOM      1  N   ALA A   1\n" + strings.Repeat("x", 2000):         "line 2 is longer than the 1024 characters a line of a PDB file may be",
		"MODEL        1\nATOM  \nENDMDL\nMODEL        2\nATOM  \nENDMDL\n": "it has 2 models, and a system is one",
	} {
		err := checkPDB(ctx, store, cid.MustParse(storedCID(t, store, content)))
		if want == "" && err != nil || want != "" && (err == nil || !strings.Contains(err.Error(), want)) {
			t.Errorf("%q: %v, want an error containing %q", content, err, want)
		}
	}
	molecule := cid.MustParse(storedCID(t, store, prepMolecule))
	if err := checkPDB(ctx, sized{store, prepMostInput + 1}, molecule); err == nil || !strings.Contains(err.Error(), "it has 134217729 bytes, and a PDB file given to prep may have at most 134217728") {
		t.Errorf("too long a file: %v", err)
	}
	if err := checkPDB(ctx, &awkward{Blobs: store, badReads: true}, molecule); err != errAwkward {
		t.Errorf("a file that cannot be read: %v", err)
	}
}

func TestThePrepImageIsBuiltOnlyWhereItIsMissing(t *testing.T) {
	store := storage.NewMemory()
	ctx := context.Background()
	molecule := storedCID(t, store, prepMolecule)
	payloads, _ := Prep{}.Split(ctx, store, []byte(`{"inputs":["`+molecule+`","`+molecule+`"],"tasks":2}`), 2)
	var ran []string
	prepares := func(args []string, _, _ io.Writer) (int, error) {
		ran = args
		os.WriteFile(filepath.Join(volume(args, "/output"), "summary.json"), []byte(`{}`), 0o644)
		return 0, nil
	}
	has := &pretend{do: func(args []string, stdout, stderr io.Writer) (int, error) {
		if args[0] == "image" {
			return 0, nil
		}
		return prepares(args, stdout, stderr)
	}}
	if _, err := (Prep{Engine: has.engine}).Execute(ctx, store, payloads[1]); err != nil {
		t.Fatal(err)
	}
	if len(has.asked) != 2 || strings.Join(has.asked[0], " ") != "image inspect "+PrepImage || has.asked[1][0] != "run" {
		t.Errorf("with the image there, the engine was asked %v", has.asked)
	}
	if !slices.Contains(ran, "SOLVATE=solvateoct mol OPCBOX 10") || !slices.Contains(ran, "SYSTEMS=2") || ran[len(ran)-1] != prepScript || ran[len(ran)-4] != PrepImage {
		t.Errorf("the task ran %q", ran)
	}
	if staged, _ := os.ReadDir(volume(ran, "/input")); len(staged) != 0 {
		t.Errorf("the task's inputs were left behind: %v", staged)
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
		return prepares(args, stdout, stderr)
	}}
	said := new(heard)
	if _, err := (Prep{Engine: lacks.engine}).Execute(WithReporter(ctx, said), store, payloads[0]); err != nil {
		t.Fatal(err)
	}
	if len(lacks.asked) != 3 || strings.Join(lacks.asked[1][:3], " ") != "build --tag "+PrepImage || !strings.Contains(recipe, "ambertools=25.3=cuda_None_nompi_") ||
		!strings.Contains(recipe, "FROM debian:12.12-slim") || !strings.Contains(recipe, "libfaketime=0.9.10-2.1") || !strings.Contains(trajDockerfile, strings.SplitN(recipe, "\n", 4)[2]) {
		t.Errorf("with the image missing, the engine was asked %v with the recipe %q", lacks.asked, recipe)
	}
	if log := strings.Join(said.lines, "\n"); !strings.Contains(log, "building the image preparing systems runs in") {
		t.Errorf("the task's log: %q", said.lines)
	}

	broken := &pretend{do: func(args []string, _, stderr io.Writer) (int, error) {
		if args[0] == "image" {
			return 1, nil
		}
		io.WriteString(stderr, "critical libmamba could not solve\n")
		return 1, nil
	}}
	if _, err := (Prep{Engine: broken.engine}).Execute(ctx, store, payloads[0]); err == nil || !strings.Contains(err.Error(), "build the image preparing systems runs in: critical libmamba could not solve") {
		t.Errorf("a build that fails: %v", err)
	}

	// The worker makes tleap's commands itself, from settings it checks for
	// itself, runs only its own image and script, and takes no input but
	// the job's PDB files: what a payload says of them is not run.
	task := trajTaskOf(payloads[1])
	tampered := func(change func(*containerTask)) []byte {
		copied := task
		copied.Env, copied.Inputs = map[string]string{}, map[string]string{}
		for k, v := range task.Env {
			copied.Env[k] = v
		}
		for k, v := range task.Inputs {
			copied.Inputs[k] = v
		}
		change(&copied)
		return mustJSON(copied)
	}
	ran = nil
	if _, err := (Prep{Engine: has.engine}).Execute(ctx, store, tampered(func(c *containerTask) {
		c.Env["SOURCES"] = "source /input/system-001.pdb"
		c.Env["SOLVATE"] = "system rm -rf /output"
		c.Env["LD_PRELOAD"] = "/input/evil.so"
		c.Command = []string{"sh", "-c", "anything"}
		c.Input, c.Network, c.GPUs = molecule, true, 1
	})); err != nil {
		t.Fatal(err)
	}
	if slices.Contains(ran, "SOLVATE=system rm -rf /output") || !slices.Contains(ran, "SOLVATE=solvateoct mol OPCBOX 10") || slices.Contains(ran, "LD_PRELOAD=/input/evil.so") ||
		ran[len(ran)-1] != prepScript || slices.Contains(ran, "anything") || slices.Contains(ran, "--gpus") || !slices.Contains(ran, "none") {
		t.Errorf("a payload with commands of its own ran %q", ran)
	}
	for name, tt := range map[string]struct {
		change func(*containerTask)
		want   string
	}{
		"another image":            {func(c *containerTask) { c.Image = "alpine:3.20" }, `prep payload: "alpine:3.20" is not the image systems are prepared in`},
		"a force field not on it":  {func(c *containerTask) { c.Env["FORCEFIELD"] = "leaprc.gaff" }, `prep payload: forcefield is "leaprc.gaff"`},
		"a water with a command":   {func(c *containerTask) { c.Env["WATER"] = "opc\nsystem ls" }, `prep payload: water is "opc\nsystem ls"`},
		"a box with a command":     {func(c *containerTask) { c.Env["BOX"] = "10 ; quit" }, `prep payload: box is "10 ; quit", which is not a number as a task is given one`},
		"a box written otherwise":  {func(c *containerTask) { c.Env["BOX"] = "1e1" }, `prep payload: box is "1e1"`},
		"a box of nothing":         {func(c *containerTask) { c.Env["BOX"] = "NaN" }, `prep payload: box is NaN`},
		"too much salt":            {func(c *containerTask) { c.Env["SALT_MM"] = "5000" }, `prep payload: salt_mm is 5000`},
		"a shape not on it":        {func(c *containerTask) { c.Env["SHAPE"] = "sphere" }, `prep payload: box_shape is "sphere"`},
		"no settings":              {func(c *containerTask) { c.Env = nil }, `prep payload: box is ""`},
		"no systems":               {func(c *containerTask) { c.Env["SYSTEMS"] = "0" }, `prep payload: "0" systems`},
		"too many systems":         {func(c *containerTask) { c.Env["SYSTEMS"] = "65" }, `prep payload: "65" systems`},
		"a leaprc among its files": {func(c *containerTask) { c.Inputs["leaprc"] = molecule }, `prep payload: "leaprc" is not the PDB file of one of the job's 2 systems`},
		"a system past the last":   {func(c *containerTask) { c.Inputs["system-002.pdb"] = molecule }, `"system-002.pdb" is not the PDB file`},
		"a system named otherwise": {func(c *containerTask) { c.Inputs["system-1.pdb"] = molecule }, `"system-1.pdb" is not the PDB file`},
		"a system of no number":    {func(c *containerTask) { c.Inputs["system--01.pdb"] = molecule }, `"system--01.pdb" is not the PDB file`},
	} {
		if _, err := (Prep{Engine: has.engine}).Execute(ctx, store, tampered(tt.change)); err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("%s: %v, want an error containing %q", name, err, tt.want)
		}
	}
	if _, err := (Prep{Engine: has.engine}).Execute(ctx, store, []byte("not json")); err == nil || !strings.Contains(err.Error(), "prep payload") {
		t.Errorf("a payload that is not one: %v", err)
	}
}

// prepFilesOf are the files a task leaves for a system of atoms atoms.
func prepFilesOf(index, atoms int) map[string]string {
	name := prepName(index)
	return map[string]string{
		name + ".prmtop": "%VERSION  VERSION_STAMP = V0001.000  DATE = 01/01/00  00:00:00\n%FLAG TITLE\n%FORMAT(20a4)\ndefault_name\n",
		name + ".inpcrd": fmt.Sprintf("default_name\n%6d\n   1.0000000   2.0000000   3.0000000\n", atoms),
		name + ".pdb":    "CRYST1   44.576   44.576   44.576 109.47 109.47 109.47 P 1           1\nATOM      1  N   LYS     1\nEND\n",
	}
}

// prepReportOf is what a task says of a system of atoms atoms, in OPC in a
// truncated octahedron, made neutral from charge.
func prepReportOf(index, atoms int, charge float64) prepReport {
	before, after := charge, 0.0
	sodium, chloride := max(0, int(-charge)), max(0, int(charge))
	return prepReport{Index: index, Atoms: atoms, Residues: atoms / 4, Waters: atoms/4 - 20, Ions: map[string]int{"Na+": sodium, "Cl-": chloride},
		Box: []float64{44.5757152, 44.5757152, 44.5757152, 109.4712190, 109.4712190, 109.4712190}, ChargeBefore: &before, ChargeAfter: &after}
}

// prepFailedOf is what a task says of a system it could not prepare.
func prepFailedOf(index int, why string, unknown ...string) prepReport {
	return prepReport{Index: index, Failed: why, Unknown: unknown}
}

// prepSummaryOf is what a task that prepared systems first to last of
// systems says, with the usual settings.
func prepSummaryOf(first, systems int, reports ...prepReport) prepSummary {
	return prepSummary{Systems: systems, First: first, Last: first + len(reports) - 1, Forcefield: "ff19SB", Water: "opc", Box: "10", Shape: "octahedron", SaltMM: "0", Prepared: reports}
}

// prepTask is the output of a task that said summary and left the files of
// each system it prepared, and the log of each it did not.
func prepTask(t *testing.T, store *storage.Store, index int, summary prepSummary) []byte {
	t.Helper()
	files := map[string]string{}
	for _, report := range summary.Prepared {
		if report.Failed != "" {
			files[prepName(report.Index)+".log"] = "Unknown residue: LIG   number: 3   type: Terminal/last\n"
			continue
		}
		for name, content := range prepFilesOf(report.Index, report.Atoms) {
			files[name] = content
		}
	}
	return ocrTask(t, store, index, mustJSONString(summary), files)
}

func TestPreparedSystemsArePutTogetherInOrder(t *testing.T) {
	store := storage.NewMemory()
	ctx := context.Background()
	kaekak, dipeptide := prepReportOf(0, 7728, 2), prepReportOf(1, 2806, 0)
	ligand := prepFailedOf(2, "unknown_residue", "LIG", "ZN")
	kaekak.Ions, kaekak.SaltPairs, kaekak.ChargeAfter = map[string]int{"Na+": 5, "Cl-": 7}, 5, new(float64)
	*kaekak.ChargeAfter = math.Copysign(0, -1)
	// Three systems among three tasks, the first of which was left none,
	// as the script shares them: none, 0, 1 to 2.
	outputs := [][]byte{
		prepTask(t, store, 0, prepSummaryOf(0, 3)),
		prepTask(t, store, 1, prepSummaryOf(0, 3, kaekak)),
		prepTask(t, store, 2, prepSummaryOf(1, 3, dipeptide, ligand)),
	}
	raw, err := Prep{}.Aggregate(ctx, store, outputs)
	if err != nil {
		t.Fatal(err)
	}
	var result PrepResult
	json.Unmarshal(raw, &result)
	if result.Forcefield != "ff19SB" || result.Water != "opc" || result.Box != 10 || result.BoxShape != "octahedron" || result.SaltMM != 0 ||
		len(result.Systems) != 3 || result.Prepared != 2 || result.Failed != 1 || result.Tasks != 3 {
		t.Fatalf("the result: %s", raw)
	}
	first := result.Systems[0]
	if first.Index != 0 || first.Atoms != 7728 || first.Residues != 1932 || first.Waters != 1912 || first.Ions["Na+"] != 5 || first.Ions["Cl-"] != 7 || first.SaltPairs != 5 ||
		*first.ChargeBefore != 2 || *first.ChargeAfter != 0 || first.Box == nil || first.Box.A != 44.5757152 || first.Box.Gamma != 109.471219 || first.Error != "" || first.Log != "" {
		t.Errorf("the first system: %+v", first)
	}
	files := prepFilesOf(0, 7728)
	if readBlob(t, store, first.Prmtop) != files["system-000.prmtop"] || readBlob(t, store, first.Inpcrd) != files["system-000.inpcrd"] || readBlob(t, store, first.PDB) != files["system-000.pdb"] {
		t.Errorf("the first system's files: %+v", first)
	}
	failed := result.Systems[2]
	if failed.Index != 2 || failed.Prmtop != "" || failed.Atoms != 0 || failed.ChargeBefore != nil ||
		failed.Error != "tleap does not know the residues LIG, ZN: prep makes systems of standard amino acids and nucleotides, water and common ions only, and a ligand, a modified residue or another molecule needs parameters of its own, made with antechamber and GAFF, which prep does not make" ||
		!strings.HasPrefix(readBlob(t, store, failed.Log), "Unknown residue: LIG") {
		t.Errorf("the system that failed: %+v", failed)
	}
	if !strings.Contains(string(raw), `"charge_before":2,"charge_after":0}`) || !strings.Contains(string(raw), `"box":{"a":44.5757152,`) {
		t.Errorf("the result as JSON: %s", raw)
	}

	// The archive has each system's files by fixed names, in order.
	zipped := []byte(readBlob(t, store, result.Archive))
	archive, err := zip.NewReader(bytes.NewReader(zipped), int64(len(zipped)))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, entry := range archive.File {
		names = append(names, entry.Name)
		if entry.Method != zip.Deflate {
			t.Errorf("%s is not compressed", entry.Name)
		}
	}
	if want := []string{"system-000.prmtop", "system-000.inpcrd", "system-000.pdb", "system-001.prmtop", "system-001.inpcrd", "system-001.pdb", "system-002.log"}; !slices.Equal(names, want) {
		t.Errorf("the archive has %v, want %v", names, want)
	}
	entry, _ := archive.File[4].Open()
	if got, _ := io.ReadAll(entry); string(got) != prepFilesOf(1, 2806)["system-001.inpcrd"] {
		t.Errorf("the archive's system-001.inpcrd: %q", got)
	}
	if got := (Prep{}).ResultBlobs(raw); !slices.Equal(got, []string{first.Prmtop, first.Inpcrd, first.PDB, result.Systems[1].Prmtop, result.Systems[1].Inpcrd, result.Systems[1].PDB, failed.Log}) || slices.Contains(got, result.Archive) {
		t.Errorf("the result's blobs: %v", got)
	}
	if got := (Prep{}).ResultBlobs([]byte("not JSON")); len(got) != 0 {
		t.Errorf("the blobs of no result: %v", got)
	}

	// The same systems, prepared in one task, come to the same result, and
	// the same archive.
	whole, err := Prep{}.Aggregate(ctx, store, [][]byte{prepTask(t, store, 0, prepSummaryOf(0, 3, kaekak, dipeptide, ligand))})
	var one PrepResult
	json.Unmarshal(whole, &one)
	if err != nil || one.Archive != result.Archive || one.Tasks != 1 || !strings.Contains(string(whole), string(mustJSON(result.Systems))) {
		t.Errorf("in one task: %s, %v", whole, err)
	}

	// A rectangular box of TIP3P, with salt, and a molecule in no water.
	box := prepReportOf(0, 400, -3)
	box.Box, box.Ions, box.SaltPairs = []float64{30, 31.5, 32, 90, 90, 90}, map[string]int{"Na+": 8, "Cl-": 5}, 5
	rectangular := prepSummaryOf(0, 1, box)
	rectangular.Water, rectangular.Shape, rectangular.SaltMM, rectangular.Box = "tip3p", "box", "150", "12.5"
	raw, err = Prep{}.Aggregate(ctx, store, [][]byte{prepTask(t, store, 0, rectangular)})
	result = PrepResult{}
	json.Unmarshal(raw, &result)
	if err != nil || result.Water != "tip3p" || result.BoxShape != "box" || result.Box != 12.5 || result.SaltMM != 150 || *result.Systems[0].Box != (PrepBox{30, 31.5, 32, 90, 90, 90}) {
		t.Errorf("a rectangular box: %s, %v", raw, err)
	}
	alone := prepReportOf(0, 22, 0)
	charge := 1.0
	alone.Box, alone.Waters, alone.Ions, alone.ChargeBefore, alone.ChargeAfter = nil, 0, map[string]int{"Na+": 0, "Cl-": 0}, &charge, &charge
	vacuum := prepSummaryOf(0, 2, alone, prepFailedOf(1, "unknown_residue", "HOH"))
	vacuum.Water = "none"
	raw, err = Prep{}.Aggregate(ctx, store, [][]byte{prepTask(t, store, 0, vacuum)})
	result = PrepResult{}
	json.Unmarshal(raw, &result)
	if err != nil || result.Systems[0].Box != nil || *result.Systems[0].ChargeAfter != 1 || !strings.HasSuffix(result.Systems[1].Error, "which prep does not make; with no water, the file's waters and ions are not known either") {
		t.Errorf("no water: %s, %v", raw, err)
	}
	// Every other way a system fails is said plainly.
	for why, want := range map[string]string{
		"not_pdb":    "the file has no ATOM or HETATM records",
		"not_loaded": "tleap could not read the file as a PDB file; its log says why",
		"not_saved":  "tleap could not make a topology of it",
	} {
		raw, err := Prep{}.Aggregate(ctx, store, [][]byte{prepTask(t, store, 0, prepSummaryOf(0, 2, prepFailedOf(0, why), dipeptide))})
		result = PrepResult{}
		json.Unmarshal(raw, &result)
		if err != nil || !strings.HasPrefix(result.Systems[0].Error, want) || result.Failed != 1 {
			t.Errorf("%s: %s, %v", why, raw, err)
		}
	}
}

// Whatever a task says is checked: every system is prepared once, in
// order, all with the same settings, what is said of each is plain numbers
// within bounds, and only the files the script leaves are taken, by their
// names.
func TestWhatPrepTasksSayIsChecked(t *testing.T) {
	store := storage.NewMemory()
	ctx := context.Background()
	missing := "bafkreigh2akiscaildcqabsyg3dfr6chu3fgpregiymsck7e7aqa4s52zy"
	garbled := storedCID(t, store, "not json")
	first := prepTask(t, store, 0, prepSummaryOf(0, 2, prepReportOf(0, 400, -1)))
	second := func(summary prepSummary) [][]byte {
		return [][]byte{first, prepTask(t, store, 1, summary)}
	}
	alone := func(summary prepSummary) [][]byte {
		return [][]byte{prepTask(t, store, 0, summary)}
	}
	with := func(change func(*prepSummary)) prepSummary {
		s := prepSummaryOf(0, 1, prepReportOf(0, 400, -1))
		change(&s)
		return s
	}
	report := func(change func(*prepReport)) [][]byte {
		r := prepReportOf(0, 400, -1)
		change(&r)
		return alone(prepSummaryOf(0, 1, r))
	}
	// The files a task left for system 0, as the summary says, changed.
	files := func(change func(map[string]string)) [][]byte {
		f := prepFilesOf(0, 400)
		change(f)
		stored := map[string]string{"summary.json": storedCID(t, store, mustJSONString(prepSummaryOf(0, 1, prepReportOf(0, 400, -1))))}
		for name, content := range f {
			stored[name] = storedCID(t, store, content)
		}
		return [][]byte{mustJSON(ContainerOutput{Files: stored})}
	}
	failed := func(r prepReport, log string) [][]byte {
		stored := map[string]string{"summary.json": storedCID(t, store, mustJSONString(prepSummaryOf(0, 1, r)))}
		if log != "" {
			stored["system-000.log"] = log
		}
		return [][]byte{mustJSON(ContainerOutput{Files: stored})}
	}
	number := func(v float64) *float64 { return &v }
	for name, tt := range map[string]struct {
		outputs [][]byte
		blobs   Blobs
		want    string
	}{
		"an output that is not one":   {[][]byte{first, []byte("not json")}, store, "task 1 output"},
		"a task with no summary":      {[][]byte{ocrTask(t, store, 0, "", nil)}, store, "task 0's summary: the task left none"},
		"a summary that is not there": {[][]byte{mustJSON(ContainerOutput{Files: map[string]string{"summary.json": missing}})}, store, "task 0's summary: "},
		"a summary that is garbled":   {[][]byte{mustJSON(ContainerOutput{Files: map[string]string{"summary.json": garbled}})}, store, "task 0's summary: invalid character"},
		"a force field not on offer":  {alone(with(func(s *prepSummary) { s.Forcefield = "gaff" })), store, `task 0 says it prepared 1 systems, with settings no job asks for: forcefield is "gaff"`},
		"a box written otherwise":     {alone(with(func(s *prepSummary) { s.Box = "10.0" })), store, `box is "10.0", which is not a number`},
		"salt written otherwise":      {alone(with(func(s *prepSummary) { s.SaltMM = "" })), store, `salt_mm is "", which is not a number`},
		"no systems":                  {alone(with(func(s *prepSummary) { s.Systems = 0 })), store, "task 0 says it prepared 0 systems"},
		"too many systems":            {alone(with(func(s *prepSummary) { s.Systems = 65 })), store, "task 0 says it prepared 65 systems"},
		"another force field": {second(func() prepSummary {
			s := prepSummaryOf(1, 2, prepReportOf(1, 400, 0))
			s.Forcefield = "ff14SB"
			return s
		}()), store, "task 1 prepared 2 systems with ff14SB, water opc, box 10, octahedron and salt 0, and task 0 2 with ff19SB"},
		"another water":                    {second(func() prepSummary { s := prepSummaryOf(1, 2, prepReportOf(1, 400, 0)); s.Water = "tip3p"; return s }()), store, "task 1 prepared"},
		"another box":                      {second(func() prepSummary { s := prepSummaryOf(1, 2, prepReportOf(1, 400, 0)); s.Box = "12"; return s }()), store, "task 1 prepared"},
		"another shape":                    {second(func() prepSummary { s := prepSummaryOf(1, 2, prepReportOf(1, 400, 0)); s.Shape = "box"; return s }()), store, "task 1 prepared"},
		"other salt":                       {second(func() prepSummary { s := prepSummaryOf(1, 2, prepReportOf(1, 400, 0)); s.SaltMM = "150"; return s }()), store, "task 1 prepared"},
		"another number of systems":        {second(prepSummaryOf(1, 3, prepReportOf(1, 400, 0))), store, "task 1 prepared 3 systems"},
		"a system prepared twice":          {second(prepSummaryOf(0, 2, prepReportOf(0, 400, 0))), store, "task 1 says it prepared systems 0 to 0 of 2, after systems up to 0, and reports 1"},
		"a system skipped":                 {alone(prepSummaryOf(1, 2, prepReportOf(1, 400, 0))), store, "task 0 says it prepared systems 1 to 1 of 2, after systems up to -1"},
		"systems past the last":            {second(prepSummaryOf(1, 2, prepReportOf(1, 400, 0), prepReportOf(2, 400, 0))), store, "task 1 says it prepared systems 1 to 2 of 2"},
		"systems that run backwards":       {alone(with(func(s *prepSummary) { s.Last = -2 })), store, "task 0 says it prepared systems 0 to -2"},
		"a report too few":                 {alone(with(func(s *prepSummary) { s.Last = 1; s.Systems = 2 })), store, "task 0 says it prepared systems 0 to 1 of 2, after systems up to -1, and reports 1"},
		"a report of another system":       {report(func(r *prepReport) { r.Index = 1 }), store, "task 0 reports system 1 where system 0 was to be"},
		"systems left out":                 {[][]byte{first}, store, "the tasks prepared 1 of the 2 systems"},
		"no system prepared":               {failed(prepFailedOf(0, "unknown_residue", "LIG"), storedCID(t, store, "Unknown residue: LIG")), store, "no system was prepared: system 0: tleap does not know the residues LIG: prep makes systems of standard amino acids"},
		"a failure not known":              {failed(prepFailedOf(0, "rm -rf"), storedCID(t, store, "x")), store, `task 0's system 0: it says it failed for "rm -rf", of residues 0`},
		"unknown residues of nothing":      {failed(prepFailedOf(0, "unknown_residue"), storedCID(t, store, "x")), store, `it says it failed for "unknown_residue", of residues 0`},
		"residues unknown otherwise":       {failed(prepFailedOf(0, "not_saved", "LIG"), storedCID(t, store, "x")), store, `it says it failed for "not_saved", of residues 1`},
		"too many unknown residues":        {failed(prepFailedOf(0, "unknown_residue", strings.Split("A B C D E F G H I J K L M N O P Q", " ")...), storedCID(t, store, "x")), store, `of residues 17`},
		"a residue of a formula":           {failed(prepFailedOf(0, "unknown_residue", "=1+1"), storedCID(t, store, "x")), store, `"=1+1" is not a residue's name`},
		"a residue with a quote":           {failed(prepFailedOf(0, "unknown_residue", `L"G`), storedCID(t, store, "x")), store, `is not a residue's name`},
		"a failure with no log":            {failed(prepFailedOf(0, "not_saved"), ""), store, "task 0's system 0: the task left no system-000.log"},
		"a log that is not there":          {failed(prepFailedOf(0, "not_saved"), missing), store, "task 0's system 0: system-000.log: "},
		"a log too long":                   {failed(prepFailedOf(0, "not_saved"), storedCID(t, store, strings.Repeat("x", prepMostLog+1))), store, "system-000.log has 65537 bytes, and must have from 1 to 65536"},
		"an empty log":                     {failed(prepFailedOf(0, "not_saved"), storedCID(t, store, "")), store, "system-000.log has 0 bytes"},
		"no atoms":                         {report(func(r *prepReport) { r.Atoms = 0 }), store, "task 0's system 0: it says it has 0 atoms in 100 residues"},
		"too many atoms":                   {report(func(r *prepReport) { r.Atoms = prepMostAtoms + 1 }), store, "it says it has 10000001 atoms"},
		"no residues":                      {report(func(r *prepReport) { r.Residues = 0 }), store, "it says it has 400 atoms in 0 residues"},
		"more residues than atoms":         {report(func(r *prepReport) { r.Residues = 401 }), store, "it says it has 400 atoms in 401 residues"},
		"fewer than no waters":             {report(func(r *prepReport) { r.Waters = -1 }), store, "it says it has -1 waters"},
		"more waters than residues":        {report(func(r *prepReport) { r.Waters = 100 }), store, "it says it has 100 waters and ions map[Cl-:0 Na+:1], with 0 pairs of salt, among 100 residues"},
		"fewer than no ions":               {report(func(r *prepReport) { r.Ions["Cl-"] = -1 }), store, "it says it has 80 waters and ions"},
		"no chloride":                      {report(func(r *prepReport) { delete(r.Ions, "Cl-"); r.Ions["K+"] = 0 }), store, "it says it has 80 waters and ions map[K+:0 Na+:1]"},
		"ions of another kind":             {report(func(r *prepReport) { r.Ions["K+"] = 1 }), store, "it says it has 80 waters and ions"},
		"no ions":                          {report(func(r *prepReport) { r.Ions = nil }), store, "it says it has 80 waters and ions map[]"},
		"more salt than ions":              {report(func(r *prepReport) { r.SaltPairs = 1 }), store, "with 1 pairs of salt"},
		"fewer than no salt":               {report(func(r *prepReport) { r.SaltPairs = -1 }), store, "with -1 pairs of salt"},
		"no charge before":                 {report(func(r *prepReport) { r.ChargeBefore = nil }), store, "it does not say its charge, or says one past any molecule's"},
		"no charge after":                  {report(func(r *prepReport) { r.ChargeAfter = nil }), store, "it does not say its charge"},
		"too great a charge":               {report(func(r *prepReport) { r.ChargeBefore = number(-1e6) }), store, "it does not say its charge"},
		"a charge left":                    {report(func(r *prepReport) { r.ChargeAfter = number(0.5) }), store, "it says it has a charge of 0.5 after it was made neutral"},
		"a box of five":                    {report(func(r *prepReport) { r.Box = r.Box[:5] }), store, "it says its box is [44.5757152 44.5757152 44.5757152 109.471219 109.471219], which is no octahedron"},
		"no box":                           {report(func(r *prepReport) { r.Box = nil }), store, "it says its box is [], which is no octahedron"},
		"a box of no size":                 {report(func(r *prepReport) { r.Box[1] = 0 }), store, "which is no octahedron"},
		"too great a box":                  {report(func(r *prepReport) { r.Box[2] = prepMostEdge + 1 }), store, "which is no octahedron"},
		"a rectangular box":                {report(func(r *prepReport) { r.Box[3], r.Box[4], r.Box[5] = 90, 90, 90 }), store, "which is no octahedron"},
		"no prmtop":                        {files(func(f map[string]string) { delete(f, "system-000.prmtop") }), store, "task 0's system 0: the task left no system-000.prmtop"},
		"a prmtop of something else":       {files(func(f map[string]string) { f["system-000.prmtop"] = "ATOM  " }), store, "system-000.prmtop is not an Amber topology"},
		"an empty prmtop":                  {files(func(f map[string]string) { f["system-000.prmtop"] = "" }), store, "system-000.prmtop has 0 bytes, and must have from 1 to 2147483648"},
		"no pdb":                           {files(func(f map[string]string) { delete(f, "system-000.pdb") }), store, "the task left no system-000.pdb"},
		"a pdb of something else":          {files(func(f map[string]string) { f["system-000.pdb"] = "%VERSION" }), store, "system-000.pdb is not a PDB file"},
		"no inpcrd":                        {files(func(f map[string]string) { delete(f, "system-000.inpcrd") }), store, "the task left no system-000.inpcrd"},
		"an inpcrd of other atoms":         {files(func(f map[string]string) { f["system-000.inpcrd"] = "default_name\n   401\n" }), store, "system-000.inpcrd is not the coordinates of 400 atoms"},
		"an inpcrd of one line":            {files(func(f map[string]string) { f["system-000.inpcrd"] = "default_name" }), store, "system-000.inpcrd is not the coordinates of 400 atoms"},
		"an inpcrd with no count":          {files(func(f map[string]string) { f["system-000.inpcrd"] = "default_name\n\n1.0\n" }), store, "system-000.inpcrd is not the coordinates"},
		"a file that is not there":         {[][]byte{mustJSON(ContainerOutput{Files: map[string]string{"summary.json": storedCID(t, store, mustJSONString(prepSummaryOf(0, 1, prepReportOf(0, 400, -1)))), "system-000.prmtop": missing}})}, store, "task 0's system 0: system-000.prmtop: "},
		"an archive that cannot be stored": {alone(prepSummaryOf(0, 1, prepReportOf(0, 400, -1))), &awkward{Blobs: store, failPut: 1}, "store the systems as one zip file: the store is full"},
		"files that cannot be read":        {alone(prepSummaryOf(0, 1, prepReportOf(0, 400, -1))), &awkward{Blobs: store, badReads: true}, "task 0's summary: the store is full"},
	} {
		if _, err := (Prep{}).Aggregate(ctx, tt.blobs, tt.outputs); err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("%s: %v, want an error containing %q", name, err, tt.want)
		}
	}
	// A molecule in no water is changed by its hydrogens alone.
	vacuum := func(change func(*prepReport)) [][]byte {
		r := prepReportOf(0, 22, 0)
		r.Box, r.Waters = nil, 0
		change(&r)
		s := prepSummaryOf(0, 1, r)
		s.Water = "none"
		return alone(s)
	}
	for name, tt := range map[string]struct {
		change func(*prepReport)
		want   string
	}{
		"its charge changed": {func(r *prepReport) { r.ChargeAfter = number(1) }, "it says it was changed, in no water, by more than its hydrogens"},
		"a box":              {func(r *prepReport) { r.Box = []float64{1, 1, 1, 90, 90, 90} }, "it says it was changed, in no water"},
		"salt":               {func(r *prepReport) { r.SaltPairs, r.Ions["Na+"], r.Ions["Cl-"] = 1, 1, 1 }, "it says it was changed, in no water"},
		"nothing changed":    {func(*prepReport) {}, ""},
	} {
		_, err := (Prep{}).Aggregate(ctx, store, vacuum(tt.change))
		if tt.want == "" && err != nil || tt.want != "" && (err == nil || !strings.Contains(err.Error(), tt.want)) {
			t.Errorf("in no water, %s: %v, want an error containing %q", name, err, tt.want)
		}
	}
	// A file past the most one may have is refused.
	if _, _, err := prepFile(ctx, sized{store, prepMostFile + 1}, map[string]string{"system-000.prmtop": garbled}, "system-000.prmtop", prepMostFile); err == nil || !strings.Contains(err.Error(), "system-000.prmtop has 2147483649 bytes, and must have from 1 to 2147483648") {
		t.Errorf("too large a file: %v", err)
	}
	// The archive is not made of files that are not there.
	if err := zipText(ctx, store, io.Discard, []string{"system-000.prmtop"}, []string{missing}); err == nil || !strings.Contains(err.Error(), "system-000.prmtop: ") {
		t.Errorf("an archive of a file that is not there: %v", err)
	}
	if err := zipText(ctx, &awkward{Blobs: store, badReads: true}, io.Discard, []string{"system-000.prmtop"}, []string{garbled}); err == nil || !strings.Contains(err.Error(), "system-000.prmtop: the store is full") {
		t.Errorf("an archive of a file that cannot be read: %v", err)
	}

	// Files other than the script's are not looked at, whatever their names.
	odd := files(func(f map[string]string) {
		f["../../escape.prmtop"] = "x"
		f["system-001.prmtop"] = "y"
		f["summary.txt"] = "z"
	})
	raw, err := Prep{}.Aggregate(ctx, store, odd)
	var result PrepResult
	json.Unmarshal(raw, &result)
	if err != nil || len(result.Systems) != 1 || len(Prep{}.ResultBlobs(raw)) != 3 {
		t.Errorf("a task that left other files: %s, %v", raw, err)
	}
	zipped := []byte(readBlob(t, store, result.Archive))
	archive, _ := zip.NewReader(bytes.NewReader(zipped), int64(len(zipped)))
	for _, entry := range archive.File {
		if !slices.Contains([]string{"system-000.prmtop", "system-000.inpcrd", "system-000.pdb"}, entry.Name) {
			t.Errorf("the archive has %s", entry.Name)
		}
	}
}

// Two peptides, KAKEAK, of charge +2, and alanine dipeptide, of none,
// both without their hydrogens, are prepared by real tleap in real
// containers: in one task and in two, which come to the same bytes, in
// OPC with salt, in TIP3P in a rectangular box, and in no water. A ligand
// tleap does not know fails, saying so. A prepared system is then
// simulated by md and its trajectory analysed by traj, as a user would
// chain them.
func TestRealSystemsArePrepared(t *testing.T) {
	requireRealImages(t)
	requireDocker(t)
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Minute)
	defer cancel()
	// The image is built here if this machine has not got it, which takes
	// the network and a few minutes.
	if err := haveImage(ctx, docker, PrepImage, prepDockerfile, "preparing systems"); err != nil {
		t.Fatal(err)
	}
	store := storage.NewMemory()
	peptide, err := os.ReadFile(filepath.Join("testdata", "amber", "kakeak.pdb"))
	if err != nil {
		t.Fatal(err)
	}
	kakeak, dipeptide := storedCID(t, store, string(peptide)), storedCID(t, store, prepMolecule)
	ligand := storedCID(t, store, strings.Replace(prepMolecule, "TER\n", "HETATM   11  C1  LIG A   4       9.000   9.000   9.000  1.00  0.00           C\nHETATM   12  O1  LIG A   4      10.000   9.000   9.000  1.00  0.00           O\nTER\n", 1))

	w := Prep{Confined: Confinement{ReadOnly: true}}
	run := func(params map[string]any, parts int) (PrepResult, error) {
		t.Helper()
		payloads, err := w.Split(ctx, store, mustJSON(params), parts)
		if err != nil {
			t.Fatal(err)
		}
		outputs := make([][]byte, len(payloads))
		said := new(heard)
		started := time.Now()
		for i, payload := range payloads {
			if outputs[i], err = w.Execute(WithReporter(ctx, said), store, payload); err != nil {
				t.Fatalf("task %d: %v\n%s", i, err, strings.Join(said.lines, "\n"))
			}
		}
		t.Logf("prepared in %s:\n%s", time.Since(started).Round(time.Millisecond), strings.Join(said.lines, "\n"))
		raw, err := w.Aggregate(ctx, store, outputs)
		var result PrepResult
		json.Unmarshal(raw, &result)
		t.Logf("result: %s", raw)
		return result, err
	}

	// In one task and in two, the files are the same bytes: each system is
	// made by a tleap of its own, its clock held still.
	both := map[string]any{"inputs": []string{kakeak, dipeptide}, "salt_mm": 150}
	whole, err := run(mustJSONAny(both, "tasks", 1), 2)
	if err != nil {
		t.Fatal(err)
	}
	shared, err := run(mustJSONAny(both, "tasks", 2), 2)
	if err != nil {
		t.Fatal(err)
	}
	if whole.Tasks != 1 || shared.Tasks != 2 || whole.Prepared != 2 || whole.Archive != shared.Archive || string(mustJSON(whole.Systems)) != string(mustJSON(shared.Systems)) {
		t.Fatalf("in one task %+v, in two %+v", whole, shared)
	}
	k, d := whole.Systems[0], whole.Systems[1]
	// KAKEAK's 47 heavy atoms are given their 57 hydrogens, and its charge
	// of +2 two more chlorides than sodiums.
	if *k.ChargeBefore != 2 || math.Abs(*k.ChargeAfter) > 1e-6 || k.Ions["Cl-"]-k.Ions["Na+"] != 2 || k.SaltPairs < 1 || k.Ions["Na+"] != k.SaltPairs || k.Waters < 1000 ||
		k.Atoms != 104+4*k.Waters+k.Ions["Na+"]+k.Ions["Cl-"] || k.Residues != 6+k.Waters+k.Ions["Na+"]+k.Ions["Cl-"] {
		t.Errorf("KAKEAK: %+v", k)
	}
	if b := k.Box; b == nil || b.Alpha != 109.471219 || b.A != b.B || b.A < 30 || b.A > 80 {
		t.Errorf("KAKEAK's box: %+v", k.Box)
	}
	if *d.ChargeBefore != 0 || *d.ChargeAfter != 0 || d.Ions["Na+"] != d.Ions["Cl-"] || d.Atoms != 22+4*d.Waters+2*d.Ions["Na+"] {
		t.Errorf("alanine dipeptide: %+v", d)
	}
	prmtop := readBlob(t, store, d.Prmtop)
	if !strings.HasPrefix(prmtop, "%VERSION  VERSION_STAMP = V0001.000  DATE = 01/01/00  00:00:00") || !strings.Contains(prmtop, "%FLAG BOX_DIMENSIONS") {
		t.Errorf("alanine dipeptide's topology begins %q", firstLine(prmtop))
	}
	if pdb := readBlob(t, store, d.PDB); !strings.HasPrefix(pdb, "CRYST1") || strings.Count(pdb, "\nATOM  ")+1 < d.Atoms-d.Waters {
		t.Errorf("alanine dipeptide's PDB file begins %q", firstLine(pdb))
	}

	// TIP3P in a rectangular box, and no water at all.
	tip3p, err := run(map[string]any{"input": dipeptide, "forcefield": "ff14SB", "box_shape": "box", "box": 8}, 1)
	if s := tip3p.Systems[0]; err != nil || tip3p.Water != "tip3p" || s.Box == nil || s.Box.Alpha != 90 || s.Atoms != 22+3*s.Waters {
		t.Errorf("in TIP3P in a rectangular box: %+v, %v", tip3p, err)
	}
	vacuum, err := run(map[string]any{"input": kakeak, "water": "none"}, 1)
	if s := vacuum.Systems[0]; err != nil || s.Box != nil || s.Atoms != 104 || s.Residues != 6 || *s.ChargeAfter != 2 {
		t.Errorf("in no water: %+v, %v", vacuum, err)
	}

	// A ligand tleap does not know fails the system, saying which, and the
	// job if it is the only one.
	mixed, err := run(map[string]any{"inputs": []string{dipeptide, ligand}, "water": "tip3p"}, 1)
	if err != nil || mixed.Prepared != 1 || mixed.Failed != 1 || !strings.HasPrefix(mixed.Systems[1].Error, "tleap does not know the residues LIG: ") ||
		!strings.Contains(readBlob(t, store, mixed.Systems[1].Log), "Unknown residue: LIG") {
		t.Errorf("a ligand among systems: %+v, %v", mixed, err)
	}
	if _, err := run(map[string]any{"input": ligand}, 1); err == nil || !strings.Contains(err.Error(), "no system was prepared: system 0: tleap does not know the residues LIG: prep makes systems of standard amino acids and nucleotides") {
		t.Errorf("a ligand alone: %v", err)
	} else {
		t.Logf("a ligand alone: %v", err)
	}

	// The prepared system is what md simulates, as it is, and its
	// trajectory, with the same topology, is what traj analyses.
	md := MD{Confined: Confinement{ReadOnly: true}}
	payloads, err := md.Split(ctx, store, mustJSON(map[string]any{"prmtop": d.Prmtop, "inpcrd": d.Inpcrd, "picoseconds": 0.1, "frames": 5, "replicas": 1}), 1)
	if err != nil {
		t.Fatal(err)
	}
	ran, err := md.Execute(ctx, store, payloads[0])
	if err != nil {
		t.Fatalf("md of the prepared system: %v", err)
	}
	raw, err := md.Aggregate(ctx, store, [][]byte{ran})
	var simulated MDResult
	json.Unmarshal(raw, &simulated)
	if err != nil || simulated.Atoms != d.Atoms || len(simulated.ReplicaResults) != 1 {
		t.Fatalf("md of the prepared system: %s, %v", raw, err)
	}
	payloads, err = Traj{}.Split(ctx, store, mustJSON(map[string]any{"topology": d.Prmtop, "trajectory": simulated.ReplicaResults[0].Trajectory, "analyses": "rmsd,radgyr", "mask": ":1-3&!@H="}), 1)
	if err != nil {
		t.Fatal(err)
	}
	ran, err = Traj{Confined: Confinement{ReadOnly: true}}.Execute(ctx, store, payloads[0])
	if err != nil {
		t.Fatalf("traj of md's trajectory: %v", err)
	}
	raw, err = Traj{}.Aggregate(ctx, store, [][]byte{ran})
	var analysed TrajResult
	json.Unmarshal(raw, &analysed)
	if err != nil || analysed.Frames != 5 || analysed.RMSD == nil || analysed.RMSD.Max > 2 {
		t.Errorf("traj of md's trajectory: %s, %v", raw, err)
	}
	t.Logf("prepared, simulated and analysed: %s", raw)
}

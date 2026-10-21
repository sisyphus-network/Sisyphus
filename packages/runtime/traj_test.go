package runtime

import (
	"bufio"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sisyphus-network/Sisyphus/packages/storage"
)

// trajFiles are a topology and trajectories, stored, as their CIDs: an
// Amber topology, a PDB file, and a DCD, a NetCDF and an mdcrd trajectory,
// each only as much as tells what it is.
func trajFiles(t *testing.T, store *storage.Store) (parm7, pdb, dcd, nc, mdcrd string) {
	t.Helper()
	return storedCID(t, store, "%VERSION  VERSION_STAMP = V0001.000\n%FLAG TITLE\n"),
		storedCID(t, store, "REMARK   1 CREATED WITH OPENMM\nATOM      1  N   ALA A   1\n"),
		storedCID(t, store, "T\x00\x00\x00CORD\x05\x00\x00\x00"),
		storedCID(t, store, "CDF\x02\x00\x00\x00\x05"),
		storedCID(t, store, "Cpptraj Generated trajectory\n   1.000   2.000\n")
}

func TestATrajJobIsAContainerJobWithTheImageAndScriptFixed(t *testing.T) {
	store := storage.NewMemory()
	ctx := context.Background()
	parm7, pdb, dcd, nc, mdcrd := trajFiles(t, store)
	one := `"topology":"` + parm7 + `","trajectory":"` + dcd + `"`

	// One trajectory is shared out by its frames: every task is given it
	// and the topology, and counts the frames for itself.
	payloads, err := Traj{}.Split(ctx, store, []byte(`{`+one+`}`), 4)
	if err != nil || len(payloads) != 4 {
		t.Fatalf("split: %d payloads, %v", len(payloads), err)
	}
	task := trajTaskOf(payloads[3])
	if task.Image != TrajImage || !strings.HasPrefix(TrajImage, "sisyphus-traj:") || task.Index != 3 || task.Count != 4 || task.Network || task.Input != "" {
		t.Errorf("the last task: %+v", task)
	}
	if want := map[string]string{"topology.parm7": parm7, "traj-000.dcd": dcd}; !equalEnv(task.Inputs, want) {
		t.Errorf("its inputs: %v", task.Inputs)
	}
	if len(task.Command) != 3 || task.Command[0] != "bash" || task.Command[2] != trajScript || !strings.Contains(trajScript, "cpptraj -i") {
		t.Errorf("its command: %q", task.Command)
	}
	// Unless said otherwise: RMSD and the radius of gyration of the alpha
	// carbons.
	want := map[string]string{
		"SPLIT": "frames", "FILES": "1", "ANALYSES": "rmsd,radgyr", "MASK": "@CA", "MASK1": "", "MASK2": "", "REGION": "*", "REFERENCE": "1",
		"ACTIONS": "rms RMSD ref [first] @CA nomod out /output/rmsd.dat\nradgyr RADGYR @CA nomax out /output/radgyr.dat\nprecision /output/rmsd.dat 16 6\nprecision /output/radgyr.dat 16 6",
		"OUTPUTS": "rmsd.dat radgyr.dat",
	}
	if !equalEnv(task.Env, want) {
		t.Errorf("its settings: %q", task.Env)
	}

	// Every analysis, given in any order, is run in the menu's.
	payloads, err = Traj{}.Split(ctx, store, []byte(`{"topology":"`+pdb+`","trajectory":"`+nc+`","analyses":"dssp,distance,radgyr,hbond,rmsd","mask":"!@H=","mask1":":1@CA",`+
		`"mask2":":20-22@CA,C,N","region":":1-30","tasks":3}`), 8)
	if err != nil || len(payloads) != 3 {
		t.Fatalf("split as asked: %d payloads, %v", len(payloads), err)
	}
	task = trajTaskOf(payloads[0])
	if task.Env["ANALYSES"] != "rmsd,radgyr,distance,hbond,dssp" || task.Inputs["topology.pdb"] != pdb || task.Inputs["traj-000.nc"] != nc || task.Count != 3 {
		t.Errorf("settings as asked: %v, %v, %d tasks", task.Env, task.Inputs, task.Count)
	}
	if got, want := task.Env["ACTIONS"], `rms RMSD ref [first] !@H= nomod out /output/rmsd.dat
radgyr RADGYR !@H= nomax out /output/radgyr.dat
distance DISTANCE :1@CA :20-22@CA,C,N out /output/distance.dat
hbond HBOND :1-30 out /output/hbonds.dat avgout /output/hbonds-avg.dat
secstruct DSSP :1-30 out /output/dssp.dat sumout /output/.work/dssp-sum.dat
precision /output/rmsd.dat 16 6
precision /output/radgyr.dat 16 6
precision /output/distance.dat 16 6`; got != want {
		t.Errorf("the commands:\n%s\nwant\n%s", got, want)
	}
	if task.Env["OUTPUTS"] != "rmsd.dat radgyr.dat distance.dat hbonds.dat hbonds-avg.dat dssp.dat" {
		t.Errorf("the outputs: %q", task.Env["OUTPUTS"])
	}

	// RMSF is found in one task, however many are asked for, after the
	// frames are fitted onto the first.
	payloads, err = Traj{}.Split(ctx, store, []byte(`{`+one+`,"analyses":"rmsf","tasks":5}`), 8)
	if err != nil || len(payloads) != 1 {
		t.Fatalf("rmsf: %d payloads, %v", len(payloads), err)
	}
	task = trajTaskOf(payloads[0])
	if task.Env["ACTIONS"] != "rms FIT ref [first] @CA\natomicfluct RMSF @CA byres out /output/rmsf.dat\nprecision /output/rmsf.dat 16 6" || task.Env["REFERENCE"] != "1" {
		t.Errorf("rmsf: %q", task.Env)
	}

	// Several trajectories are shared out whole, each task given only its
	// own and, where it fits frames onto it, the first.
	several := `"topology":"` + parm7 + `","trajectories":["` + dcd + `","` + nc + `","` + mdcrd + `","` + dcd + `","` + nc + `"]`
	payloads, err = Traj{}.Split(ctx, store, []byte(`{`+several+`}`), 2)
	if err != nil || len(payloads) != 2 {
		t.Fatalf("several: %d payloads, %v", len(payloads), err)
	}
	for i, want := range []map[string]string{
		{"topology.parm7": parm7, "traj-000.dcd": dcd, "traj-001.nc": nc},
		{"topology.parm7": parm7, "traj-000.dcd": dcd, "traj-002.mdcrd": mdcrd, "traj-003.dcd": dcd, "traj-004.nc": nc},
	} {
		task = trajTaskOf(payloads[i])
		if !equalEnv(task.Inputs, want) || task.Env["SPLIT"] != "files" || task.Env["FILES"] != "5" {
			t.Errorf("task %d of several: %v, %v", i, task.Inputs, task.Env)
		}
	}
	// No more tasks than trajectories, and none given the first that does
	// not need it.
	payloads, err = Traj{}.Split(ctx, store, []byte(`{`+several+`,"analyses":"hbond","region":":1-5"}`), 8)
	if err != nil || len(payloads) != 5 {
		t.Fatalf("several in more tasks: %d payloads, %v", len(payloads), err)
	}
	task = trajTaskOf(payloads[4])
	if !equalEnv(task.Inputs, map[string]string{"topology.parm7": parm7, "traj-004.nc": nc}) || task.Env["REFERENCE"] != "0" {
		t.Errorf("the last of five: %v, %v", task.Inputs, task.Env)
	}
	// Enough tasks that none is given more than it can be.
	many := make([]string, 40)
	for i := range many {
		many[i] = dcd
	}
	payloads, err = Traj{}.Split(ctx, store, mustJSON(map[string]any{"topology": parm7, "trajectories": many}), 1)
	if err != nil || len(payloads) != 3 {
		t.Fatalf("forty trajectories: %d payloads, %v", len(payloads), err)
	}
	for i, payload := range payloads {
		task = trajTaskOf(payload)
		if len(task.Inputs) > maxInputs || checkInputs(task.Inputs) != nil {
			t.Errorf("task %d of forty trajectories is given %d inputs", i, len(task.Inputs))
		}
	}

	nonsense := storedCID(t, store, "\x00\x01\x02 not a file of either")
	missing := "bafkreigh2akiscaildcqabsyg3dfr6chu3fgpregiymsck7e7aqa4s52zy"
	for params, want := range map[string]string{
		`not json`: "traj parameters",
		`{"topology":"somewhere","trajectory":"` + dcd + `"}`:                                       "topology is not the CID of a stored topology",
		`{"topology":"` + parm7 + `"}`:                                                              "no trajectory is given",
		`{"topology":"` + parm7 + `","trajectory":"x"}`:                                             "trajectory 0 is not the CID of a stored trajectory",
		`{"topology":"` + parm7 + `","trajectories":["` + dcd + `","x"]}`:                           "trajectory 1 is not the CID",
		`{` + one + `,"trajectories":["` + dcd + `"]}`:                                              "in place of trajectory, not with it",
		`{` + one + `,"analyses":"cluster"}`:                                                        `analyses is "cluster", and must be some of rmsd, radgyr, distance, hbond, dssp, rmsf`,
		`{` + one + `,"analyses":"rmsd,rmsd"}`:                                                      `analyses is "rmsd,rmsd"`,
		`{` + one + `,"analyses":"rmsd,"}`:                                                          `analyses is "rmsd,"`,
		`{` + one + `,"analyses":"rmsd, radgyr"}`:                                                   `analyses is "rmsd, radgyr"`,
		`{` + one + `,"analyses":"RMSD"}`:                                                           `analyses is "RMSD"`,
		`{` + one + `,"mask":"@CA\nrun"}`:                                                           `mask is "@CA\nrun", and must be an Amber mask`,
		`{` + one + `,"mask":"@CA out x"}`:                                                          `mask is "@CA out x"`,
		`{` + one + `,"mask":"out"}`:                                                                `mask is "out"`,
		`{` + one + `,"mask":"@CA;system"}`:                                                         `mask is "@CA;system"`,
		`{` + one + `,"region":"$HOME"}`:                                                            `region is "$HOME"`,
		`{` + one + `,"analyses":"distance"}`:                                                       `mask1 is "", and must be an Amber mask`,
		`{` + one + `,"analyses":"distance","mask1":":1"}`:                                          `mask2 is ""`,
		`{` + one + `,"analyses":"distance","mask1":":1","mask2":":2 #"}`:                           `mask2 is ":2 #"`,
		`{` + one + `,"mask1":":1","mask2":":2"}`:                                                   "analyses does not ask for one",
		`{` + one + `,"tasks":-1}`:                                                                  "tasks is -1, and must be from 1 to 10000",
		`{` + one + `,"tasks":10001}`:                                                               "tasks is 10001",
		`{"topology":"` + nonsense + `","trajectory":"` + dcd + `"}`:                                "topology is neither an Amber topology (prmtop or parm7) nor a PDB file",
		`{"topology":"` + dcd + `","trajectory":"` + dcd + `"}`:                                     "topology is neither",
		`{"topology":"` + parm7 + `","trajectory":"` + nonsense + `"}`:                              "trajectory 0 is neither DCD, NetCDF nor Amber's mdcrd",
		`{"topology":"` + parm7 + `","trajectories":["` + dcd + `","` + parm7 + `x"]}`:              "trajectory 1 is not the CID",
		`{"topology":"` + missing + `","trajectory":"` + dcd + `"}`:                                 "traj topology " + missing,
		`{"topology":"` + parm7 + `","trajectory":"` + missing + `"}`:                               "traj trajectory 0, " + missing,
		`{` + one + `,"analyses":"rmsd,radgyr,distance,hbond,dssp,rmsf","mask1":":1","mask2":":2"}`: "",
		`{` + one + `,"mask":"(:1-10<:5.0)&!@H=","region":"^1"}`:                                    "",
		`{` + one + `,"mask":"@/C,%CT","region":":ALA,GLY@N?,C*"}`:                                  "",
		`{"topology":"` + parm7 + `","trajectory":"` + mdcrd + `"}`:                                 "",
	} {
		_, err := (Traj{}).Split(ctx, store, []byte(params), 2)
		if want == "" && err != nil || want != "" && (err == nil || !strings.Contains(err.Error(), want)) {
			t.Errorf("%s: %v, want an error containing %q", params, err, want)
		}
	}
	tooMany := make([]string, trajMostTrajectories+1)
	for i := range tooMany {
		tooMany[i] = dcd
	}
	if _, err := (Traj{}).Split(ctx, store, mustJSON(map[string]any{"topology": parm7, "trajectories": tooMany}), 2); err == nil || !strings.Contains(err.Error(), "257 trajectories are given; a job can have at most 256") {
		t.Errorf("too many trajectories: %v", err)
	}
	if _, err := (Traj{}).Split(ctx, store, mustJSON(map[string]any{"topology": parm7, "trajectories": many[:16], "analyses": "rmsf"}), 2); err == nil || !strings.Contains(err.Error(), "rmsf is found in one task, and one task can be given at most 15 trajectories, not 16") {
		t.Errorf("rmsf of too many trajectories: %v", err)
	}

	if w, err := WithContainers().Get("traj"); err != nil || w.Name() != "traj" || !strings.Contains(WithContainers().Describe("traj"), "cpptraj") {
		t.Errorf("the workload as registered: %v, %v", w, err)
	}
	if !WithContainers().InContainers("traj") {
		t.Error("traj is not said to run in containers")
	}
	if _, err := Builtin().Get("traj"); err == nil {
		t.Error("a node that runs no containers offers to analyse trajectories")
	}
	if !(Traj{}).Varies() {
		t.Error("traj says two honest runs give the same result, though its image is built from conda-forge as it is each day")
	}
}

// A mask is one argument to cpptraj, and can be nothing more.
func TestOnlyAmberMasksAreTaken(t *testing.T) {
	for mask, want := range map[string]bool{
		"@CA": true, ":1-20@CA,C,N": true, "!:WAT,Na+,Cl-": true, "*": true, "^1": true, "(:1-10<:5.0)&!@H=": true, ":ALA|:GLY": true,
		"@/C": true, "@%CT": true, ":1@H?": true, "@" + strings.Repeat("C", trajMostMask-1): true,
		"": false, "CA": false, "out": false, "nomod": false, "1": false, "-@CA": false,
		"@CA out": false, "@CA\nrun": false, "@CA\rrun": false, "@CA\tx": false, "@CA;ls": false, `@"CA"`: false, "@'CA'": false,
		"@CA#": false, "@$CA": false, `@CA\`: false, "@CA`ls`": false, "@C\x00": false, "@ÇA": false,
		"@" + strings.Repeat("C", trajMostMask): false,
	} {
		if got := checkTrajMask("mask", mask) == nil; got != want {
			t.Errorf("%q is taken: %v", mask, got)
		}
	}
	for head, want := range map[string]string{
		"%VERSION  VERSION_STAMP": "parm7", "%FLAG TITLE": "parm7", "REMARK   1 CREATED WITH OPENMM": "pdb", "CRYST1   30.000": "pdb",
		"ATOM      1  N": "pdb", "HETATM    1": "pdb", "MODEL        1": "pdb", "HEADER    PROTEIN": "pdb",
		"%VERSIO": "", "ATOMS": "", "remark": "", "": "", "3\nwater\n": "",
	} {
		if got := topologyKind([]byte(head)); got != want {
			t.Errorf("a topology that begins %q is %q, want %q", head, got, want)
		}
	}
	for head, want := range map[string]string{
		"T\x00\x00\x00CORD": "dcd", "\x00\x00\x00T\x00\x00\x00\x00CORD": "dcd", "CDF\x01": "nc", "CDF\x02": "nc", "\x89HDF\r\n\x1a\n": "nc",
		"Cpptraj Generated trajectory\n   1.000": "mdcrd", "title\r\n  1.0\t2.0": "mdcrd",
		"": "", "CDF\x03": "", "\x00\x01": "", "title\x00": "", "\x89PNG": "", "tïtle": "",
	} {
		if got := trajectoryKind([]byte(head)); got != want {
			t.Errorf("a trajectory that begins %q is %q, want %q", head, got, want)
		}
	}
}

func TestTheTrajImageIsBuiltOnlyWhereItIsMissing(t *testing.T) {
	store := storage.NewMemory()
	ctx := context.Background()
	parm7, _, dcd, _, _ := trajFiles(t, store)
	payloads, _ := Traj{}.Split(ctx, store, []byte(`{"topology":"`+parm7+`","trajectory":"`+dcd+`"}`), 1)
	var ran []string
	analyses := func(args []string, _, _ io.Writer) (int, error) {
		ran = args
		os.WriteFile(filepath.Join(volume(args, "/output"), "summary.json"), []byte(`{}`), 0o644)
		return 0, nil
	}
	has := &pretend{do: func(args []string, stdout, stderr io.Writer) (int, error) {
		if args[0] == "image" {
			return 0, nil
		}
		return analyses(args, stdout, stderr)
	}}
	if _, err := (Traj{Engine: has.engine}).Execute(ctx, store, payloads[0]); err != nil {
		t.Fatal(err)
	}
	if len(has.asked) != 2 || strings.Join(has.asked[0], " ") != "image inspect "+TrajImage || has.asked[1][0] != "run" {
		t.Errorf("with the image there, the engine was asked %v", has.asked)
	}
	if !slices.Contains(ran, "ACTIONS="+trajSettingsFor("rmsd,radgyr", "@CA", "", "", "*").actions()) || ran[len(ran)-1] != trajScript || ran[len(ran)-4] != TrajImage {
		t.Errorf("the task ran %q", ran)
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
		return analyses(args, stdout, stderr)
	}}
	said := new(heard)
	if _, err := (Traj{Engine: lacks.engine}).Execute(WithReporter(ctx, said), store, payloads[0]); err != nil {
		t.Fatal(err)
	}
	if len(lacks.asked) != 3 || strings.Join(lacks.asked[1][:3], " ") != "build --tag "+TrajImage || !strings.Contains(recipe, "ambertools=25.3=cuda_None_nompi_") || !strings.Contains(recipe, "FROM debian:12.12-slim") {
		t.Errorf("with the image missing, the engine was asked %v with the recipe %q", lacks.asked, recipe)
	}
	if log := strings.Join(said.lines, "\n"); !strings.Contains(log, "building the image analysing trajectories runs in") {
		t.Errorf("the task's log: %q", said.lines)
	}

	broken := &pretend{do: func(args []string, _, stderr io.Writer) (int, error) {
		if args[0] == "image" {
			return 1, nil
		}
		io.WriteString(stderr, "critical libmamba could not solve\n")
		return 1, nil
	}}
	if _, err := (Traj{Engine: broken.engine}).Execute(ctx, store, payloads[0]); err == nil || !strings.Contains(err.Error(), "build the image analysing trajectories runs in: critical libmamba could not solve") {
		t.Errorf("a build that fails: %v", err)
	}

	// The worker makes cpptraj's commands itself, from settings it checks
	// for itself, and runs only its own image and script: what a payload
	// says of them is not run.
	task := trajTaskOf(payloads[0])
	tampered := func(change func(*containerTask)) []byte {
		copied := task
		copied.Env = map[string]string{}
		for k, v := range task.Env {
			copied.Env[k] = v
		}
		change(&copied)
		return mustJSON(copied)
	}
	ran = nil
	if _, err := (Traj{Engine: has.engine}).Execute(ctx, store, tampered(func(c *containerTask) {
		c.Env["ACTIONS"] = "system rm -rf /output"
		c.Env["OUTPUTS"] = "../escape"
		c.Command = []string{"sh", "-c", "anything"}
	})); err != nil {
		t.Fatal(err)
	}
	if slices.Contains(ran, "ACTIONS=system rm -rf /output") || !slices.Contains(ran, "OUTPUTS=rmsd.dat radgyr.dat") || ran[len(ran)-1] != trajScript || slices.Contains(ran, "anything") {
		t.Errorf("a payload with commands of its own ran %q", ran)
	}
	for name, tt := range map[string]struct {
		change func(*containerTask)
		want   string
	}{
		"another image":            {func(c *containerTask) { c.Image = "alpine:3.20" }, `traj payload: "alpine:3.20" is not the image trajectories are analysed in`},
		"a mask with a command":    {func(c *containerTask) { c.Env["MASK"] = "@CA\nsystem ls" }, `traj payload: mask is "@CA\nsystem ls"`},
		"a region with a command":  {func(c *containerTask) { c.Env["REGION"] = "* out /x" }, `traj payload: region is "* out /x"`},
		"an analysis not on offer": {func(c *containerTask) { c.Env["ANALYSES"] = "rmsd,cluster" }, `traj payload: analyses is "rmsd,cluster"`},
		"analyses out of order":    {func(c *containerTask) { c.Env["ANALYSES"] = "radgyr,rmsd" }, "a task is given them in the order"},
		"no analyses":              {func(c *containerTask) { c.Env = nil }, `traj payload: analyses is ""`},
		"a distance with no masks": {func(c *containerTask) { c.Env["ANALYSES"] = "distance" }, `traj payload: mask1 is ""`},
		"another way to share":     {func(c *containerTask) { c.Env["SPLIT"] = "bytes" }, `shared out by "bytes"`},
		"no trajectories":          {func(c *containerTask) { c.Env["FILES"] = "0" }, `traj payload: "0" trajectories`},
		"too many trajectories":    {func(c *containerTask) { c.Env["FILES"] = "257" }, `traj payload: "257" trajectories`},
	} {
		if _, err := (Traj{Engine: has.engine}).Execute(ctx, store, tampered(tt.change)); err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("%s: %v, want an error containing %q", name, err, tt.want)
		}
	}
	if _, err := (Traj{Engine: has.engine}).Execute(ctx, store, []byte("not json")); err == nil || !strings.Contains(err.Error(), "traj payload") {
		t.Errorf("a payload that is not one: %v", err)
	}
}

// trajTaskOf reads a task's payload.
func trajTaskOf(payload []byte) (task containerTask) {
	json.Unmarshal(payload, &task)
	return task
}

// trajSettingsFor are the settings with these analyses and masks, which
// must be ones a job may ask for.
func trajSettingsFor(analyses, mask, mask1, mask2, region string) trajSettings {
	s, err := trajSettingsOf(analyses, mask, mask1, mask2, region)
	if err != nil {
		panic(err)
	}
	return s
}

// trajSummaryOf is what a task that analysed frames first to last of one
// trajectory of frames frames says, with these analyses.
func trajSummaryOf(first, last, frames int, analyses string) string {
	return mustJSONString(trajSummary{Split: "frames", Files: 1, Counts: []int{frames}, First: first, Last: last, Analyses: analyses, Mask: "@CA", Region: "*"})
}

// trajFilesSummaryOf is what a task that analysed trajectories firstFile on
// of files, of counts frames each, says, with these analyses.
func trajFilesSummaryOf(firstFile, files int, analyses string, counts ...int) string {
	total := 0
	for _, count := range counts {
		total += count
	}
	return mustJSONString(trajSummary{Split: "files", Files: files, FirstFile: firstFile, LastFile: firstFile + len(counts) - 1, Counts: counts, First: 1, Last: total, Analyses: analyses, Mask: "@CA", Region: "*"})
}

// trajSeriesTable is a table of one number for each frame, as cpptraj
// writes one.
func trajSeriesTable(name string, values ...string) string {
	var table strings.Builder
	fmt.Fprintf(&table, "#Frame %14s\n", name)
	for i, value := range values {
		fmt.Fprintf(&table, "%8d %14s\n", i+1, value)
	}
	return table.String()
}

// trajDSSPTable is a table of secondary structure, as cpptraj writes one,
// with a row of kinds for each frame.
func trajDSSPTable(residues []string, frames ...string) string {
	var table strings.Builder
	table.WriteString("#Frame")
	for _, residue := range residues {
		fmt.Fprintf(&table, " %12s", residue)
	}
	table.WriteByte('\n')
	for i, kinds := range frames {
		fmt.Fprintf(&table, "%8d", i+1)
		for _, kind := range kinds {
			fmt.Fprintf(&table, " %12c", kind)
		}
		table.WriteByte('\n')
	}
	return table.String()
}

// trajHBondTable is a table of hydrogen bonds, as cpptraj writes one: each
// a line of acceptor, hydrogen, donor and the frames it was made in.
func trajHBondTable(bonds ...string) string {
	var table strings.Builder
	table.WriteString("#Acceptor           DonorH         Donor   Frames         Frac      AvgDist       AvgAng\n")
	for _, bond := range bonds {
		table.WriteString(bond + "       0.5000       2.9000     155.0000\n")
	}
	return table.String()
}

// trajTables are the files a task analysing these frames, with every analysis
// but RMSF, would leave: rmsd, radius of gyration, distance and hydrogen
// bonds as each frame's number, and secondary structure.
func trajTables(rmsd, radgyr, distance, hbonds []string, dssp []string, bonds ...string) map[string]string {
	return map[string]string{
		"rmsd.dat": trajSeriesTable("RMSD", rmsd...), "radgyr.dat": trajSeriesTable("RADGYR", radgyr...),
		"distance.dat": trajSeriesTable("DISTANCE", distance...), "hbonds.dat": trajSeriesTable("HBOND[UU]", hbonds...),
		"hbonds-avg.dat": trajHBondTable(bonds...), "dssp.dat": trajDSSPTable([]string{"ALA:2", "GLU:3", "LYS:4"}, dssp...),
	}
}

const trajEvery = "rmsd,radgyr,distance,hbond,dssp"

func TestAnalysedFramesArePutTogetherInOrder(t *testing.T) {
	store := storage.NewMemory()
	ctx := context.Background()
	summary := func(first, last int) string {
		var s trajSummary
		json.Unmarshal([]byte(trajSummaryOf(first, last, 5, trajEvery)), &s)
		s.Mask1, s.Mask2 = ":1", ":3"
		return mustJSONString(s)
	}
	rmsd := []string{"0.000000", "0.512345", "0.733000", "1.100000", "0.950000"}
	radgyr := []string{"5.333000", "5.306000", "5.303000", "5.301000", "5.310000"}
	distance := []string{"15.802800", "15.655400", "15.543200", "15.400000", "15.500000"}
	hbonds := []string{"4", "4", "3", "2", "4"}
	dssp := []string{"044", "040", "444", "067", "123"}
	// Five frames among four tasks, the first of which was left none, as
	// the script shares them: none, 1 to 2, 3, 4 to 5.
	outputs := [][]byte{
		ocrTask(t, store, 0, summary(1, 0), nil),
		ocrTask(t, store, 1, summary(1, 2), trajTables(rmsd[:2], radgyr[:2], distance[:2], hbonds[:2], dssp[:2],
			"ALA_2@O          LYS_4@H      LYS_4@N       2", "GLU_3@O     LYS_4@H   LYS_4@N  1")),
		ocrTask(t, store, 2, summary(3, 3), trajTables(rmsd[2:3], radgyr[2:3], distance[2:3], hbonds[2:3], dssp[2:3],
			"GLU_3@O     LYS_4@H   LYS_4@N  1")),
		ocrTask(t, store, 3, summary(4, 5), trajTables(rmsd[3:], radgyr[3:], distance[3:], hbonds[3:], dssp[3:],
			"ALA_2@O          LYS_4@H      LYS_4@N       1", "LYS_4@O   ALA_2@H   ALA_2@N   1", "ALA_2@OXT LYS_4@HZ1 LYS_4@NZ 1")),
	}
	raw, err := Traj{}.Aggregate(ctx, store, outputs)
	if err != nil {
		t.Fatal(err)
	}
	var result TrajResult
	json.Unmarshal(raw, &result)
	if result.Frames != 5 || result.Trajectories != 1 || !slices.Equal(result.TrajectoryFrames, []int{5}) || result.Tasks != 4 ||
		!slices.Equal(result.Analyses, []string{"rmsd", "radgyr", "distance", "hbond", "dssp"}) || result.RMSF != "" {
		t.Fatalf("the result: %+v", result)
	}
	wantSeries := `frame,trajectory,rmsd,radgyr,distance,hbonds
1,0,0.000000,5.333000,15.802800,4
2,0,0.512345,5.306000,15.655400,4
3,0,0.733000,5.303000,15.543200,3
4,0,1.100000,5.301000,15.400000,2
5,0,0.950000,5.310000,15.500000,4
`
	if got := readBlob(t, store, result.Series); got != wantSeries {
		t.Errorf("the series:\n%s\nwant\n%s", got, wantSeries)
	}
	if r := result.RMSD; r == nil || math.Abs(r.Mean-0.659069) > 1e-6 || r.Min != 0 || r.Max != 1.1 || r.Stdev <= 0 {
		t.Errorf("RMSD sums up as %+v", r)
	}
	if h := result.HBondsPerFrame; h == nil || h.Mean != 3.4 || h.Min != 2 || h.Max != 4 || result.RadGyr == nil || result.Distance == nil || result.Distance.Max != 15.8028 {
		t.Errorf("the rest sum up as %+v, %+v, %+v", result.HBondsPerFrame, result.RadGyr, result.Distance)
	}
	wantBonds := `acceptor,donor_hydrogen,donor,frames,fraction
ALA_2@O,LYS_4@H,LYS_4@N,3,0.6000
GLU_3@O,LYS_4@H,LYS_4@N,2,0.4000
ALA_2@OXT,LYS_4@HZ1,LYS_4@NZ,1,0.2000
LYS_4@O,ALA_2@H,ALA_2@N,1,0.2000
`
	if got := readBlob(t, store, result.HBonds); got != wantBonds {
		t.Errorf("the hydrogen bonds:\n%s\nwant\n%s", got, wantBonds)
	}
	wantDSSP := `frame,trajectory,ALA:2,GLU:3,LYS:4
1,0,-,H,H
2,0,-,H,-
3,0,H,H,H
4,0,-,T,S
5,0,E,B,G
`
	if got := readBlob(t, store, result.DSSP); got != wantDSSP {
		t.Errorf("the secondary structure:\n%s\nwant\n%s", got, wantDSSP)
	}
	wantResidues := `residue,none,extended,bridge,3-10,alpha,pi,turn,bend
ALA:2,0.6000,0.2000,0.0000,0.0000,0.2000,0.0000,0.0000,0.0000
GLU:3,0.0000,0.0000,0.2000,0.0000,0.6000,0.0000,0.2000,0.0000
LYS:4,0.2000,0.0000,0.0000,0.2000,0.4000,0.0000,0.0000,0.2000
`
	if got := readBlob(t, store, result.DSSPResidues); got != wantResidues {
		t.Errorf("each residue's secondary structure:\n%s\nwant\n%s", got, wantResidues)
	}
	if result.Structure["alpha"] != 0.4 || result.Structure["none"] != 0.2667 || len(result.Structure) != 8 {
		t.Errorf("the structure in all: %v", result.Structure)
	}

	// The same frames, analysed in one task, come to the same tables.
	whole, err := Traj{}.Aggregate(ctx, store, [][]byte{ocrTask(t, store, 0, summary(1, 5), trajTables(rmsd, radgyr, distance, hbonds, dssp,
		"ALA_2@O LYS_4@H LYS_4@N 3", "GLU_3@O LYS_4@H LYS_4@N 2", "LYS_4@O ALA_2@H ALA_2@N 1", "ALA_2@OXT LYS_4@HZ1 LYS_4@NZ 1"))})
	var one TrajResult
	json.Unmarshal(whole, &one)
	if err != nil || one.Series != result.Series || one.HBonds != result.HBonds || one.DSSP != result.DSSP || one.DSSPResidues != result.DSSPResidues || *one.RMSD != *result.RMSD || one.Tasks != 1 {
		t.Errorf("in one task: %+v, %v", one, err)
	}

	// Several trajectories, shared out whole: each frame is numbered among
	// them all, and said which it is from.
	several, err := Traj{}.Aggregate(ctx, store, [][]byte{
		ocrTask(t, store, 0, trajFilesSummaryOf(0, 3, "rmsd", 2, 1), map[string]string{"rmsd.dat": trajSeriesTable("RMSD", "0.0", "0.5", "0.7")}),
		ocrTask(t, store, 1, trajFilesSummaryOf(2, 3, "rmsd", 0), nil),
	})
	result = TrajResult{}
	json.Unmarshal(several, &result)
	if err != nil || result.Frames != 3 || result.Trajectories != 3 || !slices.Equal(result.TrajectoryFrames, []int{2, 1, 0}) || readBlob(t, store, result.Series) != "frame,trajectory,rmsd\n1,0,0.0\n2,0,0.5\n3,1,0.7\n" || result.RadGyr != nil || result.HBonds != "" {
		t.Errorf("several trajectories: %+v, %v", result, err)
	}

	// RMSF, found in one task, is each residue's.
	fluct, err := Traj{}.Aggregate(ctx, store, [][]byte{ocrTask(t, store, 0, trajSummaryOf(1, 3, 3, "rmsd,rmsf"), map[string]string{
		"rmsd.dat": trajSeriesTable("RMSD", "0.0", "0.1", "0.2"), "rmsf.dat": "#Res RMSF\n   2.000   0.262900\n   3.000000   0.211700\n  12.000   0.140400\n",
	})})
	result = TrajResult{}
	json.Unmarshal(fluct, &result)
	if err != nil || readBlob(t, store, result.RMSF) != "residue,rmsf\n2,0.262900\n3,0.211700\n12,0.140400\n" || result.Series == "" {
		t.Errorf("rmsf: %+v, %v", result, err)
	}

	// A trajectory of no frames is analysed, as nothing.
	nothing, err := Traj{}.Aggregate(ctx, store, [][]byte{ocrTask(t, store, 0, trajSummaryOf(1, 0, 0, "rmsd"), nil)})
	result = TrajResult{}
	json.Unmarshal(nothing, &result)
	if err != nil || result.Frames != 0 || result.Series != "" || result.RMSD != nil {
		t.Errorf("no frames: %+v, %v", result, err)
	}
}

// Whatever a task says is checked: every frame is analysed once, in order,
// all with the same settings, the tables have a row for each frame and
// only numbers and names in them, and only the files the script leaves
// are taken, by their names.
func TestWhatTrajTasksSayIsChecked(t *testing.T) {
	store := storage.NewMemory()
	ctx := context.Background()
	missing := "bafkreigh2akiscaildcqabsyg3dfr6chu3fgpregiymsck7e7aqa4s52zy"
	garbled := storedCID(t, store, "not json")
	rmsd := func(values ...string) map[string]string {
		return map[string]string{"rmsd.dat": trajSeriesTable("RMSD", values...)}
	}
	first := ocrTask(t, store, 0, trajSummaryOf(1, 1, 2, "rmsd"), rmsd("0.0"))
	second := func(summary string, files map[string]string) [][]byte {
		return [][]byte{first, ocrTask(t, store, 1, summary, files)}
	}
	with := func(change func(*trajSummary)) string {
		var s trajSummary
		json.Unmarshal([]byte(trajSummaryOf(2, 2, 2, "rmsd")), &s)
		change(&s)
		return mustJSONString(s)
	}
	alone := func(summary string, files map[string]string) [][]byte {
		return [][]byte{ocrTask(t, store, 0, summary, files)}
	}
	for name, tt := range map[string]struct {
		outputs [][]byte
		blobs   Blobs
		want    string
	}{
		"an output that is not one":        {[][]byte{first, []byte("not json")}, store, "task 1 output"},
		"a task with no summary":           {alone("", rmsd("0.0")), store, "task 0's summary: the task left none"},
		"a summary that is not there":      {[][]byte{mustJSON(ContainerOutput{Files: map[string]string{"summary.json": missing}})}, store, "task 0's summary: "},
		"a summary that is garbled":        {[][]byte{mustJSON(ContainerOutput{Files: map[string]string{"summary.json": garbled}})}, store, "task 0's summary: invalid character"},
		"an analysis not on offer":         {alone(trajSummaryOf(1, 1, 1, "cluster"), nil), store, `task 0 says it analysed 1 trajectories, shared out by "frames", with settings no job asks for: analyses is "cluster"`},
		"a mask with a command":            {alone(with(func(s *trajSummary) { s.First, s.Mask = 1, "@CA\nrun" }), nil), store, `with settings no job asks for: mask is "@CA\nrun"`},
		"another way to share":             {alone(with(func(s *trajSummary) { s.First, s.Split = 1, "bytes" }), nil), store, `shared out by "bytes"`},
		"no trajectories":                  {alone(with(func(s *trajSummary) { s.First, s.Split, s.Files = 1, "files", 0 }), nil), store, "says it analysed 0 trajectories"},
		"frames of several":                {alone(with(func(s *trajSummary) { s.First, s.Files = 1, 2 }), nil), store, "says it analysed 2 trajectories"},
		"rmsf in several tasks":            {[][]byte{ocrTask(t, store, 0, trajSummaryOf(1, 1, 2, "rmsf"), nil), ocrTask(t, store, 1, trajSummaryOf(2, 2, 2, "rmsf"), nil)}, store, "rmsf is found in one task, and 2 tasks say they found it"},
		"other analyses":                   {second(trajSummaryOf(2, 2, 2, "rmsd,radgyr"), rmsd("0.1")), store, `task 1 analysed 1 trajectories, shared out by "frames", with "rmsd,radgyr", and task 0 1, by "frames", with "rmsd"`},
		"another mask":                     {second(with(func(s *trajSummary) { s.Mask = "@N" }), rmsd("0.1")), store, "task 1 analysed"},
		"another region":                   {second(with(func(s *trajSummary) { s.Region = ":1" }), rmsd("0.1")), store, "task 1 analysed"},
		"another trajectory":               {second(trajSummaryOf(2, 3, 3, "rmsd"), rmsd("0.1", "0.2")), store, "task 1 analysed a trajectory of 3 frames, and task 0 one of 2"},
		"a frame analysed twice":           {second(trajSummaryOf(1, 2, 2, "rmsd"), rmsd("0.0", "0.1")), store, "task 1 says it analysed frames 1 to 2 of 2, after frames up to 1"},
		"a frame skipped":                  {alone(trajSummaryOf(2, 2, 2, "rmsd"), rmsd("0.1")), store, "task 0 says it analysed frames 2 to 2 of 2, after frames up to 0"},
		"frames that run backwards":        {second(trajSummaryOf(2, 0, 2, "rmsd"), nil), store, "task 1 says it analysed frames 2 to 0"},
		"frames past the last":             {second(trajSummaryOf(2, 3, 2, "rmsd"), rmsd("0.1", "0.2")), store, "task 1 says it analysed frames 2 to 3 of 2"},
		"frames left out":                  {[][]byte{first}, store, "the tasks analysed 1 of the 1 trajectories, and 1 of the frames"},
		"fewer than no frames":             {alone(trajSummaryOf(1, -1, -1, "rmsd"), nil), store, "task 0 says it analysed a trajectory of [-1] frames"},
		"too many frames":                  {alone(trajSummaryOf(1, 0, trajMostFrames+1, "rmsd"), nil), store, "a trajectory of [10000001] frames"},
		"frames of another file":           {alone(with(func(s *trajSummary) { s.First, s.LastFile = 1, 1 }), nil), store, "task 0 says it analysed a trajectory of [2] frames"},
		"a trajectory skipped":             {alone(trajFilesSummaryOf(1, 2, "rmsd", 1), rmsd("0.0")), store, "task 0 says it analysed trajectories 1 to 1 of 2, after trajectories up to -1"},
		"a trajectory analysed twice":      {[][]byte{ocrTask(t, store, 0, trajFilesSummaryOf(0, 2, "rmsd", 1), rmsd("0.0")), ocrTask(t, store, 1, trajFilesSummaryOf(0, 2, "rmsd", 1, 1), rmsd("0.0", "0.1"))}, store, "task 1 says it analysed trajectories 0 to 1 of 2, after trajectories up to 0"},
		"trajectories past the last":       {alone(trajFilesSummaryOf(0, 1, "rmsd", 1, 1), rmsd("0.0", "0.1")), store, "task 0 says it analysed trajectories 0 to 1 of 1"},
		"trajectories left out":            {alone(trajFilesSummaryOf(0, 2, "rmsd", 1), rmsd("0.0")), store, "the tasks analysed 1 of the 2 trajectories"},
		"counts that do not add up":        {alone(with(func(s *trajSummary) { s.Split, s.Files, s.First, s.Last, s.Counts = "files", 1, 1, 3, []int{2} }), rmsd("0.0", "0.1", "0.2")), store, "task 0 says it analysed frames 1 to 3 of trajectories with 2"},
		"counts for too few files":         {alone(with(func(s *trajSummary) { s.Split, s.Files, s.LastFile, s.First, s.Counts = "files", 2, 1, 1, []int{2} }), nil), store, "task 0 says it analysed trajectories 0 to 1 of 2"},
		"a trajectory of fewer than no":    {alone(trajFilesSummaryOf(0, 1, "rmsd", -1), nil), store, "task 0 says trajectory 0 has -1 frames"},
		"no table":                         {alone(trajSummaryOf(1, 1, 1, "rmsd"), nil), store, "task 0's rmsd.dat: the task left none"},
		"a table that is not there":        {[][]byte{mustJSON(ContainerOutput{Files: map[string]string{"summary.json": storedCID(t, store, trajSummaryOf(1, 1, 1, "rmsd")), "rmsd.dat": missing}})}, store, "task 0's rmsd.dat: "},
		"an empty table":                   {alone(trajSummaryOf(1, 1, 1, "rmsd"), map[string]string{"rmsd.dat": ""}), store, "task 0's rmsd.dat: it is empty"},
		"a table with no header":           {alone(trajSummaryOf(1, 1, 1, "rmsd"), map[string]string{"rmsd.dat": "1 0.0\n"}), store, "task 0's rmsd.dat: it has no header"},
		"a frame too few":                  {alone(trajSummaryOf(1, 2, 2, "rmsd"), rmsd("0.0")), store, "task 0's rmsd.dat: it has 1 frames, and the task says it analysed 2"},
		"a frame too many":                 {alone(trajSummaryOf(1, 1, 1, "rmsd"), rmsd("0.0", "0.1")), store, "task 0's rmsd.dat: it has 2 frames, and the task says it analysed 1"},
		"frames out of order":              {alone(trajSummaryOf(1, 2, 2, "rmsd"), map[string]string{"rmsd.dat": "#Frame RMSD\n2 0.0\n1 0.1\n"}), store, `task 0's rmsd.dat: row 1 is of frame "2"`},
		"a word for a number":              {alone(trajSummaryOf(1, 1, 1, "rmsd"), rmsd("=cmd()")), store, `task 0's rmsd.dat: frame 1 is "=cmd()", not a number`},
		"a number that is not":             {alone(trajSummaryOf(1, 1, 1, "rmsd"), rmsd("nan")), store, `frame 1 is "nan", not a number`},
		"two numbers for one":              {alone(trajSummaryOf(1, 1, 1, "rmsd"), map[string]string{"rmsd.dat": "#Frame RMSD\n1 0.0 0.1\n"}), store, `frame 1 is "0.0 0.1", not a number`},
		"part of a hydrogen bond":          {alone(trajSummaryOf(1, 1, 1, "hbond"), map[string]string{"hbonds.dat": trajSeriesTable("HB", "1.5"), "hbonds-avg.dat": trajHBondTable()}), store, `task 0's hbonds.dat: frame 1 is "1.5", not a number`},
		"a hydrogen bond of a formula":     {alone(trajSummaryOf(1, 1, 1, "hbond"), map[string]string{"hbonds.dat": trajSeriesTable("HB", "1"), "hbonds-avg.dat": trajHBondTable("=HYPERLINK(1)@O ALA_2@H ALA_2@N 1")}), store, `task 0's hbonds-avg.dat: "=HYPERLINK(1)@O ALA_2@H ALA_2@N 1 0.5000 2.9000 155.0000" is not a hydrogen bond`},
		"a hydrogen bond with a comma":     {alone(trajSummaryOf(1, 1, 1, "hbond"), map[string]string{"hbonds.dat": trajSeriesTable("HB", "1"), "hbonds-avg.dat": trajHBondTable("ALA_2@O,x ALA_2@H ALA_2@N 1")}), store, "is not a hydrogen bond"},
		"a hydrogen bond made too often":   {alone(trajSummaryOf(1, 1, 1, "hbond"), map[string]string{"hbonds.dat": trajSeriesTable("HB", "1"), "hbonds-avg.dat": trajHBondTable("ALA_2@O LYS_4@H LYS_4@N 2")}), store, `the hydrogen bond ALA_2@O LYS_4@H LYS_4@N says it was made in "2" of the task's 1 frames`},
		"a hydrogen bond never made":       {alone(trajSummaryOf(1, 1, 1, "hbond"), map[string]string{"hbonds.dat": trajSeriesTable("HB", "1"), "hbonds-avg.dat": trajHBondTable("ALA_2@O LYS_4@H LYS_4@N 0")}), store, `made in "0" of the task's 1 frames`},
		"a hydrogen bond listed twice":     {alone(trajSummaryOf(1, 2, 2, "hbond"), map[string]string{"hbonds.dat": trajSeriesTable("HB", "1", "1"), "hbonds-avg.dat": trajHBondTable("ALA_2@O LYS_4@H LYS_4@N 1", "ALA_2@O LYS_4@H LYS_4@N 1")}), store, "the hydrogen bond ALA_2@O LYS_4@H LYS_4@N is listed twice"},
		"no hydrogen bond table":           {alone(trajSummaryOf(1, 1, 1, "hbond"), map[string]string{"hbonds.dat": trajSeriesTable("HB", "1")}), store, "task 0's hbonds-avg.dat: the task left none"},
		"a kind of structure not known":    {alone(trajSummaryOf(1, 1, 1, "dssp"), map[string]string{"dssp.dat": trajDSSPTable([]string{"ALA:2"}, "8")}), store, `task 0's dssp.dat: frame 1 says residue ALA:2 is "8", which is no kind of structure`},
		"a residue too few":                {alone(trajSummaryOf(1, 1, 1, "dssp"), map[string]string{"dssp.dat": "#Frame ALA:2 GLU:3\n1 4\n"}), store, "task 0's dssp.dat: frame 1 has 1 residues, not 2"},
		"a residue of a formula":           {alone(trajSummaryOf(1, 1, 1, "dssp"), map[string]string{"dssp.dat": trajDSSPTable([]string{"=1+1"}, "0")}), store, `task 0's dssp.dat: "=1+1" is not a residue`},
		"no residues":                      {alone(trajSummaryOf(1, 1, 1, "dssp"), map[string]string{"dssp.dat": "#Frame\n1\n"}), store, "task 0's dssp.dat: it has 0 residues, and must have from 1 to 100000"},
		"other residues":                   {[][]byte{ocrTask(t, store, 0, trajSummaryOf(1, 1, 2, "dssp"), map[string]string{"dssp.dat": trajDSSPTable([]string{"ALA:2"}, "4")}), ocrTask(t, store, 1, trajSummaryOf(2, 2, 2, "dssp"), map[string]string{"dssp.dat": trajDSSPTable([]string{"ALA:3"}, "4")})}, store, "task 1's dssp.dat: it has residues ALA:3, and the tasks before had ALA:2"},
		"an RMSF of no residues":           {alone(trajSummaryOf(1, 1, 1, "rmsf"), map[string]string{"rmsf.dat": "#Res RMSF\n"}), store, "task 0's rmsf.dat: it has no residues"},
		"an RMSF of residues out of order": {alone(trajSummaryOf(1, 1, 1, "rmsf"), map[string]string{"rmsf.dat": "#Res RMSF\n3.000 0.1\n2.000 0.2\n"}), store, `task 0's rmsf.dat: "2.000 0.2" is not a residue's RMSF, after residue 3`},
		"an RMSF of part of a residue":     {alone(trajSummaryOf(1, 1, 1, "rmsf"), map[string]string{"rmsf.dat": "#Res RMSF\n2.500 0.1\n"}), store, `"2.500 0.1" is not a residue's RMSF`},
		"an RMSF of a formula":             {alone(trajSummaryOf(1, 1, 1, "rmsf"), map[string]string{"rmsf.dat": "#Res RMSF\n2.000 =1+1\n"}), store, `"2.000 =1+1" is not a residue's RMSF`},
		"no secondary structure table":     {alone(trajSummaryOf(1, 1, 1, "dssp"), nil), store, "task 0's dssp.dat: the task left none"},
		"no RMSF table":                    {alone(trajSummaryOf(1, 1, 1, "rmsf"), nil), store, "task 0's rmsf.dat: the task left none"},
		"a series that cannot be stored":   {alone(trajSummaryOf(1, 1, 1, "rmsd"), rmsd("0.0")), &awkward{Blobs: store, failPut: 1}, "store the series: the store is full"},
	} {
		if _, err := (Traj{}).Aggregate(ctx, tt.blobs, tt.outputs); err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("%s: %v, want an error containing %q", name, err, tt.want)
		}
	}

	// Each table that cannot be stored fails the job, by its name.
	var all trajSummary
	json.Unmarshal([]byte(trajSummaryOf(1, 1, 1, "rmsd,hbond,dssp,rmsf")), &all)
	every := ocrTask(t, store, 0, mustJSONString(all), map[string]string{
		"rmsd.dat": trajSeriesTable("RMSD", "0.0"), "hbonds.dat": trajSeriesTable("HB", "1"), "hbonds-avg.dat": trajHBondTable("ALA_2@O LYS_4@H LYS_4@N 1"),
		"dssp.dat": trajDSSPTable([]string{"ALA:2"}, "4"), "rmsf.dat": "#Res RMSF\n2.000 0.1\n",
	})
	if _, err := (Traj{}).Aggregate(ctx, store, [][]byte{every}); err != nil {
		t.Errorf("every table: %v", err)
	}
	for put, want := range map[int]string{2: "store the hydrogen bonds: ", 3: "store the secondary structure: ", 4: "store the secondary structure of each residue: ", 5: "store the RMSF: "} {
		if _, err := (Traj{}).Aggregate(ctx, &awkward{Blobs: store, failPut: put}, [][]byte{every}); err == nil || !strings.Contains(err.Error(), want+"the store is full") {
			t.Errorf("table %d cannot be stored: %v, want an error containing %q", put, err, want)
		}
	}
	if _, _, err := readTrajTable(ctx, &awkward{Blobs: store, badReads: true}, storedCID(t, store, "#Frame RMSD\n"), -1); err != errAwkward {
		t.Errorf("a table that cannot be read: %v", err)
	}

	// No more residues, nor hydrogen bonds, than a job may have.
	residues := make([]string, trajMostResidues+1)
	for i := range residues {
		residues[i] = "ALA:" + strconv.Itoa(i+1)
	}
	crowded := ocrTask(t, store, 0, trajSummaryOf(1, 1, 1, "dssp"), map[string]string{"dssp.dat": trajDSSPTable(residues, strings.Repeat("0", len(residues)))})
	if _, err := (Traj{}).Aggregate(ctx, store, [][]byte{crowded}); err == nil || !strings.Contains(err.Error(), "task 0's dssp.dat: it has 100001 residues, and must have from 1 to 100000") {
		t.Errorf("too many residues: %v", err)
	}
	bonds := make([]string, trajMostHBonds+1)
	for i := range bonds {
		bonds[i] = fmt.Sprintf("ALA_%d@O LYS_1@H LYS_1@N 1", i+1)
	}
	bonded := ocrTask(t, store, 0, trajSummaryOf(1, 1, 1, "hbond"), map[string]string{"hbonds.dat": trajSeriesTable("HB", "1"), "hbonds-avg.dat": trajHBondTable(bonds...)})
	if _, err := (Traj{}).Aggregate(ctx, store, [][]byte{bonded}); err == nil || !strings.Contains(err.Error(), "task 0's hbonds-avg.dat: more than 100000 hydrogen bonds are listed") {
		t.Errorf("too many hydrogen bonds: %v", err)
	}

	// Files other than the script's are not looked at, whatever their names.
	odd := ocrTask(t, store, 0, trajSummaryOf(1, 1, 1, "rmsd"), map[string]string{"rmsd.dat": trajSeriesTable("RMSD", "0.0"), "../../escape.csv": "x", "series.csv": "y", "radgyr.dat": "z"})
	raw, err := Traj{}.Aggregate(ctx, store, [][]byte{odd})
	var result TrajResult
	json.Unmarshal(raw, &result)
	if err != nil || readBlob(t, store, result.Series) != "frame,trajectory,rmsd\n1,0,0.0\n" {
		t.Errorf("a task that left other files: %+v, %v", result, err)
	}

	// A table past the most one may have is refused, from one task or
	// added up from several.
	huge := ocrTask(t, store, 0, trajSummaryOf(1, 1, 1, "rmsd"), map[string]string{"rmsd.dat": "#Frame RMSD\n" + strings.Repeat(" ", trajMostText) + "\n1 0.0\n"})
	if _, err := (Traj{}).Aggregate(ctx, store, [][]byte{huge}); err == nil || !strings.Contains(err.Error(), "task 0's rmsd.dat: ") {
		t.Errorf("too long a table: %v", err)
	}
	// The same table, stored once, stands for three series of each of
	// two tasks, each task's half the frames.
	half := trajMostText / 2 / 48
	var table strings.Builder
	table.WriteString("#Frame RMSD\n")
	for i := range half {
		fmt.Fprintf(&table, "%d 0.000000000000000\n", i+1)
	}
	values := storedCID(t, store, table.String())
	three := func(index int, summary string) []byte {
		return mustJSON(ContainerOutput{Index: index, Files: map[string]string{
			"summary.json": storedCID(t, store, summary), "rmsd.dat": values, "radgyr.dat": values, "distance.dat": values,
		}})
	}
	var both trajSummary
	json.Unmarshal([]byte(trajSummaryOf(1, half, 2*half, "rmsd,radgyr,distance")), &both)
	both.Mask1, both.Mask2 = ":1", ":2"
	firstHalf := mustJSONString(both)
	both.First, both.Last = half+1, 2*half
	halves := [][]byte{three(0, firstHalf), three(1, mustJSONString(both))}
	if _, err := (Traj{}).Aggregate(ctx, store, halves); err == nil || !strings.Contains(err.Error(), "task 1's frames take the tables past the 67108864 bytes a table may have") {
		t.Errorf("too long a series in all: %v", err)
	}
}

// readInpcrd reads the coordinates of an Amber inpcrd file.
func readInpcrd(t *testing.T, path string) [][3]float64 {
	t.Helper()
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	lines := bufio.NewScanner(file)
	lines.Scan() // the title
	lines.Scan()
	atoms, err := strconv.Atoi(strings.Fields(lines.Text())[0])
	if err != nil {
		t.Fatal(err)
	}
	var numbers []float64
	for lines.Scan() && len(numbers) < 3*atoms {
		text := lines.Text()
		for at := 0; at+12 <= len(text); at += 12 {
			v, err := strconv.ParseFloat(strings.TrimSpace(text[at:at+12]), 64)
			if err != nil {
				t.Fatal(err)
			}
			numbers = append(numbers, v)
		}
	}
	coordinates := make([][3]float64, atoms)
	for i := range coordinates {
		copy(coordinates[i][:], numbers[3*i:3*i+3])
	}
	return coordinates
}

// writeDCD writes frames of coordinates as a CHARMM DCD file of 32-bit
// records, as OpenMM's DCDReporter does, with no unit cell.
func writeDCD(t *testing.T, path string, frames [][][3]float64) {
	t.Helper()
	var out []byte
	record := func(body []byte) {
		out = binary.LittleEndian.AppendUint32(out, uint32(len(body)))
		out = append(out, body...)
		out = binary.LittleEndian.AppendUint32(out, uint32(len(body)))
	}
	header := []byte("CORD")
	control := make([]uint32, 20)
	control[0], control[2], control[3], control[19] = uint32(len(frames)), 1, uint32(len(frames)), 24
	for _, v := range control {
		header = binary.LittleEndian.AppendUint32(header, v)
	}
	binary.LittleEndian.PutUint32(header[4+9*4:], math.Float32bits(0.002))
	record(header)
	title := binary.LittleEndian.AppendUint32(nil, 1)
	record(append(title, fmt.Sprintf("%-80s", "made by Sisyphus's traj test")...))
	record(binary.LittleEndian.AppendUint32(nil, uint32(len(frames[0]))))
	for _, frame := range frames {
		for axis := range 3 {
			var body []byte
			for _, atom := range frame {
				body = binary.LittleEndian.AppendUint32(body, math.Float32bits(float32(atom[axis])))
			}
			record(body)
		}
	}
	if err := os.WriteFile(path, out, 0o644); err != nil {
		t.Fatal(err)
	}
}

// trajIn runs cpptraj in the image with a directory of this machine's at
// /work, as ocrIn does, given its commands.
func trajIn(t *testing.T, dir, commands string) {
	t.Helper()
	os.WriteFile(filepath.Join(dir, "commands.in"), []byte(commands), 0o644)
	args := []string{"run", "--rm", "--network", "none", "--user", strconv.Itoa(os.Getuid()) + ":" + strconv.Itoa(os.Getgid()), "--volume", dir + ":/work", "--workdir", "/work", TrajImage, "cpptraj", "-i", "/work/commands.in"}
	if out, err := exec.Command("docker", args...).CombinedOutput(); err != nil {
		t.Fatalf("%s: %v\n%s", commands, err, out)
	}
}

// Alanine dipeptide, OpenMM's test system, as an Amber topology and a
// trajectory of 60 frames made here: the molecule turning and drifting as
// a whole, and each atom trembling about its place. The trajectory is
// analysed by real cpptraj in real containers, in one task and shared out,
// as DCD, as NetCDF and as mdcrd, and with the topology as a PDB file.
func TestARealTrajectoryIsAnalysed(t *testing.T) {
	requireRealImages(t)
	requireDocker(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	// The image is built here if this machine has not got it, which takes
	// the network and a few minutes.
	if err := haveImage(ctx, docker, TrajImage, trajDockerfile, "analysing trajectories"); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	os.Chmod(dir, 0o777)
	base := readInpcrd(t, filepath.Join("testdata", "openmm", "alanine-dipeptide-implicit.inpcrd"))
	frames := make([][][3]float64, 60)
	for f := range frames {
		turn := 0.07 * float64(f)
		frames[f] = make([][3]float64, len(base))
		for a, atom := range base {
			x := atom[0] + 0.15*math.Sin(float64(7*f+3*a))
			y := atom[1] + 0.15*math.Sin(float64(5*f+11*a+1))
			z := atom[2] + 0.15*math.Sin(float64(3*f+13*a+2))
			if f == 0 {
				x, y, z = atom[0], atom[1], atom[2]
			}
			frames[f][a] = [3]float64{x*math.Cos(turn) - y*math.Sin(turn) + 0.3*float64(f), x*math.Sin(turn) + y*math.Cos(turn), z - 0.1*float64(f)}
		}
	}
	writeDCD(t, filepath.Join(dir, "walk.dcd"), frames)
	topology, err := os.ReadFile(filepath.Join("testdata", "openmm", "alanine-dipeptide-implicit.prmtop"))
	if err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(dir, "dipeptide.parm7"), topology, 0o644)
	// The same frames as NetCDF and as mdcrd, and the molecule as a PDB
	// file, made by cpptraj.
	trajIn(t, dir, "parm /work/dipeptide.parm7\ntrajin /work/walk.dcd\ntrajout /work/walk.nc netcdf\ntrajout /work/walk.mdcrd mdcrd\nrun\nclear trajin\ntrajin /work/walk.dcd 1 1\ntrajout /work/dipeptide.pdb pdb\nrun\n")
	store := storage.NewMemory()
	put := func(name string) string {
		t.Helper()
		file, err := os.Open(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		defer file.Close()
		c, err := store.Put(ctx, file)
		if err != nil {
			t.Fatal(err)
		}
		return c.String()
	}
	parm7, pdb, dcd, nc, mdcrd := put("dipeptide.parm7"), put("dipeptide.pdb"), put("walk.dcd"), put("walk.nc"), put("walk.mdcrd")

	w := Traj{Confined: Confinement{ReadOnly: true}}
	run := func(params map[string]any, parts int) TrajResult {
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
		t.Logf("%v analysed in %s:\n%s", params["analyses"], time.Since(started).Round(time.Millisecond), strings.Join(said.lines, "\n"))
		raw, err := w.Aggregate(ctx, store, outputs)
		if err != nil {
			t.Fatal(err)
		}
		var result TrajResult
		json.Unmarshal(raw, &result)
		t.Logf("result: %s", raw)
		return result
	}
	every := map[string]any{"topology": parm7, "trajectory": dcd, "analyses": "rmsd,radgyr,distance,hbond,dssp", "mask": "!@H=", "mask1": ":1", "mask2": ":3"}

	// In one task, and in four, the tables are the same bytes: each frame
	// is fitted onto the first frame of the trajectory, not of its task.
	whole := run(mustJSONAny(every, "tasks", 1), 4)
	shared := run(mustJSONAny(every, "tasks", 4), 4)
	if whole.Frames != 60 || whole.Tasks != 1 || shared.Tasks != 4 || shared.Frames != 60 || !slices.Equal(shared.TrajectoryFrames, []int{60}) {
		t.Fatalf("in one task %+v, in four %+v", whole, shared)
	}
	if whole.Series != shared.Series || whole.HBonds != shared.HBonds || whole.DSSP != shared.DSSP || whole.DSSPResidues != shared.DSSPResidues || *whole.RMSD != *shared.RMSD {
		t.Errorf("shared out, the tables differ:\n%s\n%s", readBlob(t, store, whole.Series), readBlob(t, store, shared.Series))
	}
	series := strings.Split(strings.TrimSpace(readBlob(t, store, shared.Series)), "\n")
	if len(series) != 61 || series[0] != "frame,trajectory,rmsd,radgyr,distance,hbonds" || !strings.HasPrefix(series[1], "1,0,0.000000,") {
		t.Errorf("the series: %q", series[:2])
	}
	// The molecule turned and drifted, which a fit takes away, and its
	// atoms trembled by up to a quarter of an ångström.
	if r := shared.RMSD; r.Min != 0 || r.Max > 0.4 || r.Mean < 0.05 {
		t.Errorf("RMSD sums up as %+v", r)
	}
	if g := shared.RadGyr; g.Max-g.Min > 0.3 || g.Mean < 1 || g.Mean > 4 {
		t.Errorf("the radius of gyration sums up as %+v", g)
	}
	if d := shared.Distance; d.Mean < 2 || d.Mean > 8 {
		t.Errorf("the distance sums up as %+v", d)
	}
	if !strings.HasPrefix(readBlob(t, store, shared.DSSP), "frame,trajectory,ACE:1,ALA:2,NME:3\n") || shared.Structure["none"]+shared.Structure["bend"]+shared.Structure["turn"] < 0.99 {
		t.Errorf("the secondary structure: %q, %v", firstLine(readBlob(t, store, shared.DSSP)), shared.Structure)
	}

	// The same frames as NetCDF and as mdcrd, shared out whole: those of
	// the NetCDF file are the DCD's to the bit, and the mdcrd's to its
	// three decimal places.
	several := run(map[string]any{"topology": parm7, "trajectories": []string{dcd, nc, mdcrd}, "analyses": "rmsd", "mask": "!@H=", "tasks": 2}, 8)
	if several.Frames != 180 || several.Trajectories != 3 || !slices.Equal(several.TrajectoryFrames, []int{60, 60, 60}) || several.Tasks != 2 {
		t.Fatalf("three trajectories: %+v", several)
	}
	rows := strings.Split(strings.TrimSpace(readBlob(t, store, several.Series)), "\n")[1:]
	for f := range 60 {
		dcdRow, ncRow, mdcrdRow := strings.Split(rows[f], ","), strings.Split(rows[60+f], ","), strings.Split(rows[120+f], ",")
		a, _ := strconv.ParseFloat(dcdRow[2], 64)
		c, _ := strconv.ParseFloat(mdcrdRow[2], 64)
		if dcdRow[0] != strconv.Itoa(f+1) || ncRow[0] != strconv.Itoa(61+f) || ncRow[1] != "1" || mdcrdRow[1] != "2" || ncRow[2] != dcdRow[2] || math.Abs(a-c) > 0.005 {
			t.Errorf("frame %d: %q, %q, %q", f+1, rows[f], rows[60+f], rows[120+f])
		}
	}

	// RMSF, by residue, with the topology as a PDB file: one task, though
	// four are asked for.
	fluct := run(map[string]any{"topology": pdb, "trajectory": dcd, "analyses": "rmsf", "mask": "!@H=", "tasks": 4}, 4)
	table := strings.Split(strings.TrimSpace(readBlob(t, store, fluct.RMSF)), "\n")
	if fluct.Tasks != 1 || len(table) != 4 || table[0] != "residue,rmsf" || !strings.HasPrefix(table[1], "1,") || !strings.HasPrefix(table[3], "3,") {
		t.Errorf("rmsf: %+v, %q", fluct, table)
	}
	for _, row := range table[1:] {
		if v, err := strconv.ParseFloat(strings.Split(row, ",")[1], 64); err != nil || v <= 0 || v > 0.4 {
			t.Errorf("a residue's RMSF: %q", row)
		}
	}

	// A mask that selects nothing is the task's failure, said plainly.
	nothing, _ := w.Split(ctx, store, mustJSON(map[string]any{"topology": parm7, "trajectory": dcd, "mask": ":LYS@CA", "tasks": 1}), 1)
	if _, err := w.Execute(ctx, store, nothing[0]); err == nil || !strings.Contains(err.Error(), "exited with status 1: cpptraj wrote no rmsd.dat, for the reason it warns of above") {
		t.Errorf("a mask that selects nothing: %v", err)
	} else {
		t.Logf("a mask that selects nothing: %v", err)
	}
	// So is a trajectory of another molecule.
	other := storedCID(t, store, "%VERSION  VERSION_STAMP = V0001.000\n%FLAG TITLE\n%FORMAT(20a4)\n\n")
	wrong, _ := w.Split(ctx, store, mustJSON(map[string]any{"topology": other, "trajectory": dcd, "tasks": 1}), 1)
	if _, err := w.Execute(ctx, store, wrong[0]); err == nil || !strings.Contains(err.Error(), "exited with status 1: trajectory 0 cannot be read with this topology") {
		t.Errorf("a topology that is not the trajectory's: %v", err)
	}
}

// mustJSONAny is params with one more key.
func mustJSONAny(params map[string]any, key string, value any) map[string]any {
	more := map[string]any{key: value}
	for k, v := range params {
		more[k] = v
	}
	return more
}

func TestOnlyNumbersAreTakenAsNumbers(t *testing.T) {
	for field, want := range map[string]bool{
		"0": true, "4": true, "0.000000": true, "-12.5": true, "123456789012345.123456789012345": true,
		"": false, "-": false, ".": false, "1.": false, ".5": false, "--1": false, "+1": false, "1e5": false, "nan": false, "inf": false,
		"1.2.3": false, "1,5": false, "=1+1": false, " 1": false, "1234567890123456": false, "1.1234567890123456": false, "١": false,
	} {
		if got := trajNumber(field); got != want {
			t.Errorf("%q is a number: %v", field, got)
		}
	}
}

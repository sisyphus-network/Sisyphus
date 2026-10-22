package runtime

import (
	"context"
	"crypto/sha256"
	"encoding/csv"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sisyphus-network/Sisyphus/packages/storage"
)

// threeWaters is three structures of water as XYZ, the way files are found:
// with a blank line between two of them, a line ending in a carriage
// return, and more columns than an atom needs.
const threeWaters = "3\nwater one\nO 0.0 0.0 0.1173\nH 0.0 0.7572 -0.4692\nH 0.0 -0.7572 -0.4692\n\n" +
	"3\r\nwater two\r\nO 0 0 0.12 0.5\nH 0 .76 -.47\nH\t0 -0.76 -4.7e-1\n" +
	"3\n\n8 0 0 0.1\n1 0 0.75 -0.46\n1 0 -0.75 -0.46\n"

func TestAQchemJobIsAContainerJobWithTheImageAndCommandFixed(t *testing.T) {
	store := storage.NewMemory()
	ctx := context.Background()
	waters, _ := store.Put(ctx, strings.NewReader(threeWaters))

	payloads, err := Qchem{}.Split(ctx, store, []byte(`{"input":"`+waters.String()+`"}`), 2)
	if err != nil || len(payloads) != 2 {
		t.Fatalf("split: %d payloads, %v", len(payloads), err)
	}
	var task containerTask
	json.Unmarshal(payloads[1], &task)
	if task.Image != QchemImage || !regexp.MustCompile(`^sisyphus-qchem:[0-9a-f]{12}$`).MatchString(task.Image) || task.Input != waters.String() || task.Index != 1 || task.Count != 2 || task.Network {
		t.Errorf("the last task: %+v", task)
	}
	if len(task.Command) != 3 || task.Command[0] != "sh" || !strings.Contains(task.Command[2], "xtb ") || !strings.Contains(task.Command[2], "OMP_NUM_THREADS=1") {
		t.Errorf("its command: %q", task.Command)
	}
	// Unless said otherwise: GFN2-xTB, optimised, neutral and closed
	// shell, in the gas phase. The second of two tasks has the second and
	// third waters.
	want := map[string]string{"FORMAT": "xyz", "STRUCTURES": "3", "METHOD": "gfn2", "JOB": "opt", "XTB_ARGS": "--gfn 2 --chrg 0 --uhf 0 --opt normal", "FIRST": "1", "LAST": "3"}
	if fmt.Sprint(task.Env) != fmt.Sprint(want) {
		t.Errorf("its settings: %v, want %v", task.Env, want)
	}
	json.Unmarshal(payloads[0], &task)
	if task.Env["FIRST"] != "0" || task.Env["LAST"] != "1" {
		t.Errorf("the first task's structures: %v", task.Env)
	}

	for params, want := range map[string]string{
		`"method":"gfn1","job":"sp","solvent":"water","charge":-1,"uhf":1`:        "--gfn 1 --chrg -1 --uhf 1 --alpb water",
		`"method":"gfn0","job":"hess","solvent":"none"`:                           "--gfn 0 --chrg 0 --uhf 0 --hess",
		`"method":"gfnff","job":"ohess","convergence":"tight","solvent":"chcl3"`:  "--gfnff --chrg 0 --uhf 0 --ohess tight --alpb chcl3",
		`"method":"gfn2","job":"opt","convergence":"extreme","solvent":"toluene"`: "--gfn 2 --chrg 0 --uhf 0 --opt extreme --alpb toluene",
	} {
		payloads, err := Qchem{}.Split(ctx, store, []byte(`{"input":"`+waters.String()+`",`+params+`}`), 1)
		if err != nil || len(payloads) != 1 {
			t.Fatalf("%s: %d payloads, %v", params, len(payloads), err)
		}
		json.Unmarshal(payloads[0], &task)
		if task.Env["XTB_ARGS"] != want || task.Env["FIRST"] != "0" || task.Env["LAST"] != "3" {
			t.Errorf("%s: %v, want %s", params, task.Env, want)
		}
	}
	// There are never more tasks than structures, however many are asked
	// for, by the job or by the pool; and the runs are as even as can be.
	for asked, want := range map[string][]string{`"tasks":2`: {"0", "1", "3"}, `"tasks":9`: {"0", "1", "2", "3"}, `"tasks":0`: {"0", "1", "2", "3"}} {
		payloads, err := Qchem{}.Split(ctx, store, []byte(`{"input":"`+waters.String()+`",`+asked+`}`), 50)
		if err != nil || len(payloads) != len(want)-1 {
			t.Fatalf("%s: %d payloads, %v", asked, len(payloads), err)
		}
		for i, payload := range payloads {
			json.Unmarshal(payload, &task)
			if task.Env["FIRST"] != want[i] || task.Env["LAST"] != want[i+1] || task.Count != len(payloads) {
				t.Errorf("%s: task %d has %s to %s of %d", asked, i, task.Env["FIRST"], task.Env["LAST"], task.Count)
			}
		}
	}

	missing := "bafkreigh2akiscaildcqabsyg3dfr6chu3fgpregiymsck7e7aqa4s52zy"
	nonsense, _ := store.Put(ctx, strings.NewReader("the boulder and the hill"))
	for params, want := range map[string]string{
		`not json`:                                          "qchem parameters",
		`{}`:                                                "input is not the CID of a stored file of structures",
		`{"input":"` + missing + `"}`:                       "qchem input " + missing,
		`{"input":"` + nonsense.String() + `"}`:             `qchem input: line 1: "the boulder and the hill" is not a number of atoms`,
		`{"input":"CID","method":"gfn3"}`:                   `method is "gfn3", and must be one of gfn2, gfn1, gfn0, gfnff`,
		`{"input":"CID","method":"GFN2"}`:                   `method is "GFN2"`,
		`{"input":"CID","job":"md"}`:                        `job is "md", and must be one of sp, opt, hess, ohess`,
		`{"input":"CID","solvent":"seawater"}`:              `solvent is "seawater", and must be none or one of acetone, `,
		`{"input":"CID","solvent":"h2o"}`:                   `solvent is "h2o"`,
		`{"input":"CID","method":"gfn0","solvent":"water"}`: "xtb has no solvent parameters for GFN0-xTB",
		`{"input":"CID","convergence":"verytight"}`:         `convergence is "verytight", and must be one of crude, sloppy, loose, lax, normal, tight, vtight, extreme`,
		`{"input":"CID","charge":51}`:                       "charge is 51, and must be from -50 to 50",
		`{"input":"CID","charge":-51}`:                      "charge is -51",
		`{"input":"CID","uhf":-1}`:                          "uhf is -1, and must be from 0 to 50",
		`{"input":"CID","uhf":51}`:                          "uhf is 51",
		`{"input":"CID","tasks":-1}`:                        "tasks is -1",
		`{"input":"CID","tasks":10001}`:                     "tasks is 10001, and must be from 1 to 10000",
	} {
		params = strings.Replace(params, "CID", waters.String(), 1)
		if _, err := (Qchem{}).Split(ctx, store, []byte(params), 2); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v, want an error containing %q", params, err, want)
		}
	}
	if w, err := WithContainers().Get("qchem"); err != nil || w.Name() != "qchem" || !strings.Contains(WithContainers().Describe("qchem"), "xtb") || !strings.Contains(WithContainers().Describe("qchem"), "woctanol") {
		t.Errorf("the workload as registered: %v, %v", w, err)
	}
	if !WithContainers().InContainers("qchem") {
		t.Error("qchem is not said to run in containers")
	}
	if _, err := Builtin().Get("qchem"); err == nil {
		t.Error("a node that runs no containers offers quantum chemistry")
	}
	if !(Qchem{}).Varies() {
		t.Error("qchem says two honest workers give the same result, and on different processors they do not")
	}
}

func TestTheQchemImageIsBuiltOnlyWhereItIsMissing(t *testing.T) {
	store := storage.NewMemory()
	ctx := context.Background()
	waters, _ := store.Put(ctx, strings.NewReader(threeWaters))
	payloads, _ := Qchem{}.Split(ctx, store, []byte(`{"input":"`+waters.String()+`"}`), 1)
	computes := func(args []string, _, _ io.Writer) (int, error) {
		os.WriteFile(filepath.Join(volume(args, "/output"), "summary.json"), []byte(`{}`), 0o644)
		return 0, nil
	}

	has := &pretend{do: func(args []string, stdout, stderr io.Writer) (int, error) {
		if args[0] == "image" {
			return 0, nil
		}
		return computes(args, stdout, stderr)
	}}
	raw, err := Qchem{Engine: has.engine, Confined: Confinement{ReadOnly: true}}.Execute(ctx, store, payloads[0])
	if err != nil {
		t.Fatal(err)
	}
	var out ContainerOutput
	json.Unmarshal(raw, &out)
	if got := readBlob(t, store, out.Files["summary.json"]); got != `{}` {
		t.Errorf("the task's output: %q", got)
	}
	if len(has.asked) != 2 || strings.Join(has.asked[0], " ") != "image inspect "+QchemImage || has.asked[1][0] != "run" || !slices.Contains(has.asked[1], "--read-only") {
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
		return computes(args, stdout, stderr)
	}}
	said := new(heard)
	if _, err := (Qchem{Engine: lacks.engine}).Execute(WithReporter(ctx, said), store, payloads[0]); err != nil {
		t.Fatal(err)
	}
	if len(lacks.asked) != 3 || strings.Join(lacks.asked[1][:3], " ") != "build --tag "+QchemImage {
		t.Errorf("with the image missing, the engine was asked %v", lacks.asked)
	}
	// xtb's release, checked against its digest, and its licence, from
	// the commit the release was made from.
	for _, want := range []string{"/releases/download/v6.7.1/xtb-6.7.1-linux-x86_64.tar.xz", xtbSum + "  /xtb.tar.xz\" | sha256sum -c -", "/" + xtbCommit + "/COPYING.LESSER", xtbCopying, xtbCopyingLess, "XTBPATH=/usr/local/share/xtb", "apk add --no-cache jq", "uname -m"} {
		if !strings.Contains(recipe, want) {
			t.Errorf("the recipe lacks %q:\n%s", want, recipe)
		}
	}
	if sum := sha256.Sum256([]byte(recipe)); !strings.HasSuffix(QchemImage, ":"+hex.EncodeToString(sum[:6])) {
		t.Errorf("the image %s is not named by its recipe", QchemImage)
	}
	if log := strings.Join(said.lines, "\n"); !strings.Contains(log, "building the image quantum chemistry runs in") {
		t.Errorf("the task's log: %q", said.lines)
	}

	broken := &pretend{do: func(args []string, _, stderr io.Writer) (int, error) {
		if args[0] == "image" {
			return 1, nil
		}
		io.WriteString(stderr, "fetching\nxtb is released for x86-64 alone, and this machine is aarch64\n")
		return 1, nil
	}}
	if _, err := (Qchem{Engine: broken.engine}).Execute(ctx, store, payloads[0]); err == nil || !strings.Contains(err.Error(), "build the image quantum chemistry runs in: xtb is released for x86-64 alone") {
		t.Errorf("a build that fails: %v", err)
	}

	// A worker builds and runs only the image of its own making.
	nothing := &pretend{do: func([]string, io.Writer, io.Writer) (int, error) { return 0, nil }}
	var task containerTask
	json.Unmarshal(payloads[0], &task)
	task.Image = "alpine:3.20"
	for payload, want := range map[string]string{
		"not json":             "qchem payload",
		string(mustJSON(task)): `qchem payload: "alpine:3.20" is not the image quantum chemistry runs in`,
	} {
		if _, err := (Qchem{Engine: nothing.engine}).Execute(ctx, store, []byte(payload)); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%.40s: %v, want an error containing %q", payload, err, want)
		}
	}
	if len(nothing.asked) != 0 {
		t.Errorf("the engine was asked %v", nothing.asked)
	}
}

// molfile writes a V2000 molfile of the atoms given, each an element and
// three coordinates, as the columns of its atom block have them.
func molfile(title string, atoms ...[4]string) string {
	text := title + "\n  sisyphus\n\n" + fmt.Sprintf("%3d%3d  0  0  0  0  0  0  0  0999 V2000\n", len(atoms), 0)
	for _, a := range atoms {
		text += fmt.Sprintf("%10s%10s%10s %-3s 0  0  0  0  0  0  0  0  0  0  0  0\n", a[1], a[2], a[3], a[0])
	}
	return text + "M  END\n"
}

func TestStructuresAreReadAsTheTasksReadThem(t *testing.T) {
	read := func(text string) (string, []string, error) {
		format, n, err := countStructures(strings.NewReader(text))
		var seen []string
		if err == nil {
			eachStructure(strings.NewReader(text), format, func(title string, atoms []string) error {
				seen = append(seen, title+": "+strings.Join(atoms, ", "))
				return nil
			})
			if len(seen) != n {
				t.Errorf("%d structures counted, and %d read", n, len(seen))
			}
		}
		return format, seen, err
	}
	format, seen, err := read(threeWaters)
	want := []string{
		"water one: O 0.0 0.0 0.1173, H 0.0 0.7572 -0.4692, H 0.0 -0.7572 -0.4692",
		"water two: O 0 0 0.12, H 0 .76 -.47, H 0 -0.76 -4.7e-1",
		": 8 0 0 0.1, 1 0 0.75 -0.46, 1 0 -0.75 -0.46",
	}
	if format != "xyz" || !slices.Equal(seen, want) || err != nil {
		t.Errorf("the waters: %s %q, %v", format, seen, err)
	}

	water := molfile("water", [4]string{"O", "0.0000", "0.0000", "0.1173"}, [4]string{"H", "0.0000", "0.7572", "-0.4692"}, [4]string{"H", "0.0000", "-0.7572", "-0.4692"})
	big := molfile("chloride far away", [4]string{"Cl", "-1000.0000", "1000.0000", "-999.9999"})
	for _, sdf := range []string{
		water + "> <ID>\n1\n\n$$$$\n" + big + "$$$$\n",
		water + "$$$$\n" + big + "$$$$\n\n\n\n\n\n",
		water + "$$$$\n$$$$\n" + big,
	} {
		format, seen, err := read(sdf)
		want := []string{"water: O 0.0000 0.0000 0.1173, H 0.0000 0.7572 -0.4692, H 0.0000 -0.7572 -0.4692", "chloride far away: Cl -1000.0000 1000.0000 -999.9999"}
		if format != "sdf" || !slices.Equal(seen, want) || err != nil {
			t.Errorf("an SDF file: %s %q, %v\n%s", format, seen, err, sdf)
		}
	}

	defer func(structures int, input int64) { qchemMostStructures, qchemMostInput = structures, input }(qchemMostStructures, qchemMostInput)
	qchemMostStructures, qchemMostInput = 2, 4096
	for text, want := range map[string]string{
		"":                          "it holds no structures",
		"\n \n\t\n":                 "it holds no structures",
		"three\nwater\n":            `line 1: "three" is not a number of atoms from 1 to 20000`,
		"0\nnothing\n":              `line 1: "0" is not a number of atoms`,
		"20001\nmany\n":             `"20001" is not a number of atoms`,
		"3 atoms\nwater\n":          `"3 atoms" is not a number of atoms`,
		"1\nan atom\nH 0 0\n":       `line 3: "H 0 0" is not an atom, an element and three coordinates`,
		"1\nan atom\nH 0 0 x\n":     `"H 0 0 x" is not an atom`,
		"1\nan atom\nH 0 0 1e\n":    `"H 0 0 1e" is not an atom`,
		"1\nan atom\nH 0 0 inf\n":   `"H 0 0 inf" is not an atom`,
		"1\nan atom\nH 0 0 0x1p3\n": `"H 0 0 0x1p3" is not an atom`,
		"1\nan atom\nHe2 0 0 0\n":   `"He2 0 0 0" is not an atom`,
		"1\nan atom\n-H 0 0 0\n":    `"-H 0 0 0" is not an atom`,
		"2\ntwo atoms\nH 0 0 0\n":   "the file ends within a structure of 2 atoms",
		"2\n":                       "the file ends within a structure of 2 atoms",
		"1\na\nH 0 0 0\n1\nb\nH 0 0 0\n1\nc\nH 0 0 0\n":                                                                   "it holds more than the 2 structures a job may have",
		"1\n" + strings.Repeat("x", 5000) + "\nH 0 0 0\n":                                                                 "it is larger than the 4096 bytes a job's structures may be",
		"title\n\n\n  0  0  0  0  0  0  0  0  0  0999 V3000\n":                                                            "it is an SDF file of V3000 molfiles, and only V2000 ones are read",
		"title\n\n\n  x  0  0  0  0  0  0  0  0  0999 V2000\n$$$$\n":                                                      `molfile "title": its counts line "  x  0  0  0  0  0  0  0  0  0999 V2000" does not begin with a number of atoms`,
		"title\n\n\n  0  0  0  0  0  0  0  0  0  0999 V2000\n":                                                            "does not begin with a number of atoms from 1 to 20000",
		"title\n\n\n  2  0  0  0  0  0  0  0  0  0999 V2000\n    0.0000    0.0000    0.0000 H\n$$$$\n":                    `molfile "title" ends within its 2 atoms`,
		"title\n\n\n  1  0  0  0  0  0  0  0  0  0999 V2000\n    0.0000    0.0000    0.0000\n$$$$\n":                      `molfile "title": "    0.0000    0.0000    0.0000" is not an atom, as a molfile has one`,
		"title\n\n\n  1  0  0  0  0  0  0  0  0  0999 V2000\n    0.0000    0.0000    0.0000 H\n$$$$\nnext\n\n$$$$\n":      "line 9: a molfile ends before its counts line",
		strings.Repeat("title\n\n\n  1  0  0  0  0  0  0  0  0  0999 V2000\n    0.0000    0.0000    0.0000 H\n$$$$\n", 3): "it holds more than the 2 structures a job may have",
	} {
		if _, _, err := read(text); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%.60q: %v, want an error containing %q", text, err, want)
		}
	}
	if err := eachStructure(strings.NewReader("1\nx\nH 0 0 0\n"), "xyz", func(string, []string) error { return errAwkward }); err != errAwkward {
		t.Errorf("a structure that is refused: %v", err)
	}
	if err := eachStructure(strings.NewReader("1\n"+strings.Repeat("x", 1<<20)+"\nH 0 0 0\n"), "xyz", func(string, []string) error { return nil }); err == nil || err.Error() != "it has a line longer than 1 MiB" {
		t.Errorf("a line too long: %v", err)
	}
	if err := eachStructure(iotestReader{}, "xyz", func(string, []string) error { return nil }); err != errAwkward {
		t.Errorf("a file that cannot be read: %v", err)
	}
	if column("short", 10, 10) != "" || column("ab cd", 1, 10) != "bcd" {
		t.Error("columns past a line's end")
	}
}

// iotestReader is a file that cannot be read.
type iotestReader struct{}

func (iotestReader) Read([]byte) (int, error) { return 0, errAwkward }

// qchemTask is a task's output as the script would leave it, with the
// summary and the geometries given stored.
func qchemTask(t *testing.T, store Blobs, summary any, geometries string) []byte {
	t.Helper()
	ctx := context.Background()
	files := map[string]string{}
	stored, err := store.Put(ctx, strings.NewReader(string(mustJSON(summary))))
	if err != nil {
		t.Fatal(err)
	}
	files["summary.json"] = stored.String()
	if geometries != "" {
		stored, _ = store.Put(ctx, strings.NewReader(geometries))
		files["structures.xyz"] = stored.String()
	}
	// Whatever else a worker says it left is never read.
	files["../../etc/passwd"] = "bafkreigh2akiscaildcqabsyg3dfr6chu3fgpregiymsck7e7aqa4s52zy"
	return mustJSON(ContainerOutput{Files: files})
}

const (
	h2Opt    = "2\n energy: -1.0 gnorm: 0.0001 xtb: 6.7.1 (edcfbbe)\nH 0.0 0.0 0.0\nH 0.0 0.0 0.74\n"
	h2Opt2   = "2\n energy: -1.0\nH 0.0 0.0 0.0\nH 0.0 0.0 0.75\n"
	h2OptBad = "2\n energy: -1.0\nH 0.0 0.0 0.0\nH 0.0 0.0 0.76\n"
)

func TestQchemResultsArePutTogetherInOrder(t *testing.T) {
	store := storage.NewMemory()
	ctx := context.Background()
	s := func(index int, title, energy string, converged, geometry bool) qchemTaskStructure {
		return qchemTaskStructure{Index: index, Title: title, Energy: energy, Gap: "13.8", Dipole: "0.1 0.2 -0.2", Converged: converged, Geometry: geometry}
	}
	failed := qchemTaskStructure{Index: 2, Title: "clash\r\nof atoms", Status: 174, Converged: false, Geometry: false}
	unfinished := s(3, "slow", "-0.99", false, true)
	unfinished.Gap, unfinished.Dipole = "", ""
	outputs := [][]byte{
		qchemTask(t, store, qchemSummary{Method: "gfn2", Job: "opt", Structures: []qchemTaskStructure{s(0, "anti", "-13.665127724012", true, true), s(1, "gauche", "-13.664176232897", true, true)}}, h2Opt+h2Opt2),
		qchemTask(t, store, qchemSummary{Method: "gfn2", Job: "opt", Structures: []qchemTaskStructure{failed, unfinished}}, h2OptBad),
	}
	raw, err := Qchem{}.Aggregate(ctx, store, outputs)
	if err != nil {
		t.Fatal(err)
	}
	var result QchemResult
	json.Unmarshal(raw, &result)
	if result.Method != "gfn2" || result.Job != "opt" || result.Structures != 4 || result.Converged != 2 || result.Lowest == nil || *result.Lowest != 0 || *result.LowestEnergy != -13.665127724012 || len(result.Results) != 4 {
		t.Fatalf("the result: %s", raw)
	}
	gauche := result.Results[1]
	if math.Abs(*gauche.Relative-0.597) > 0.001 || *gauche.Energy != -13.664176232897 || *gauche.Gap != 13.8 || math.Abs(*gauche.Dipole-0.3*auDebye) > 1e-12 || !gauche.Converged || gauche.Error != "" || gauche.Frequencies != "" || gauche.Imaginary != nil {
		t.Errorf("the gauche conformer: %+v", gauche)
	}
	if got := readBlob(t, store, gauche.Geometry); got != "2\ngauche | energy -13.664176232897 Eh\nH 0.0 0.0 0.0\nH 0.0 0.0 0.75\n" {
		t.Errorf("its geometry:\n%s", got)
	}
	clash := result.Results[2]
	if clash.Title != "clash  of atoms" || clash.Energy != nil || clash.Relative != nil || clash.Converged || clash.Error != "xtb exited with status 174" || clash.Geometry != "" {
		t.Errorf("a structure xtb failed on: %+v", clash)
	}
	slow := result.Results[3]
	if slow.Converged || *slow.Relative < 0 || slow.Gap != nil || slow.Dipole != nil || slow.Geometry == "" {
		t.Errorf("a structure whose optimisation did not converge: %+v", slow)
	}
	if got, want := readBlob(t, store, result.XYZ), "2\nanti | energy -13.665127724012 Eh\nH 0.0 0.0 0.0\nH 0.0 0.0 0.74\n"+
		"2\ngauche | energy -13.664176232897 Eh\nH 0.0 0.0 0.0\nH 0.0 0.0 0.75\n"+
		"2\nslow | energy -0.99 Eh\nH 0.0 0.0 0.0\nH 0.0 0.0 0.76\n"; got != want {
		t.Errorf("the geometries:\n%s\nwant:\n%s", got, want)
	}
	rows, err := csv.NewReader(strings.NewReader(readBlob(t, store, result.CSV))).ReadAll()
	if err != nil || len(rows) != 5 || strings.Join(rows[0], ",") != "index,title,energy_hartree,relative_kcal_mol,gap_ev,dipole_debye,free_energy_hartree,imaginary_frequencies,converged,error" ||
		strings.Join(rows[1], ",") != "0,anti,-13.665127724012,0,13.8,"+number(result.Results[0].Dipole)+",,,true," || rows[2][3] != number(gauche.Relative) || strings.Join(rows[3], ",") != "2,clash  of atoms,,,,,,,false,xtb exited with status 174" {
		t.Errorf("the table: %q, %v", rows, err)
	}

	// Frequencies, of a job that finds them.
	hess := s(0, "water", "-5.07", true, false)
	hess.FreeEnergy, hess.Frequencies = "-5.06", [][]string{{"-41.20", "0.5"}, {"-0.00", "0.0"}, {"1601.10", "75.3"}, {"3640.0", "2.1"}}
	plain := s(1, "no modes", "-5.08", true, false)
	raw, err = Qchem{}.Aggregate(ctx, store, [][]byte{qchemTask(t, store, qchemSummary{Method: "gfnff", Job: "hess", Structures: []qchemTaskStructure{hess, plain}}, "")})
	if err != nil {
		t.Fatal(err)
	}
	result = QchemResult{}
	json.Unmarshal(raw, &result)
	water := result.Results[0]
	if *water.FreeEnergy != -5.06 || water.Imaginary == nil || *water.Imaginary != 1 || result.XYZ != "" || water.Geometry != "" || *result.Lowest != 1 || result.Results[1].Frequencies != "" {
		t.Errorf("frequencies: %s", raw)
	}
	if got := readBlob(t, store, water.Frequencies); got != "# wave number (cm-1), infrared intensity (km/mol); imaginary frequencies are negative\n-41.20 0.5\n-0.00 0.0\n1601.10 75.3\n3640.0 2.1\n" {
		t.Errorf("the frequencies:\n%s", got)
	}

	// None converged: none is lowest, and none is measured from it.
	raw, err = Qchem{}.Aggregate(ctx, store, [][]byte{qchemTask(t, store, qchemSummary{Method: "gfn1", Job: "sp", Structures: []qchemTaskStructure{s(0, "a", "-1", false, false)}}, "")})
	result = QchemResult{}
	json.Unmarshal(raw, &result)
	if err != nil || result.Lowest != nil || result.LowestEnergy != nil || result.Results[0].Relative != nil || result.Converged != 0 || result.XYZ != "" {
		t.Errorf("none converged: %s, %v", raw, err)
	}

	missing := "bafkreigh2akiscaildcqabsyg3dfr6chu3fgpregiymsck7e7aqa4s52zy"
	garbled, _ := store.Put(ctx, strings.NewReader("not json"))
	one := func(job string, structure qchemTaskStructure, geometries string) [][]byte {
		return [][]byte{qchemTask(t, store, qchemSummary{Method: "gfn2", Job: job, Structures: []qchemTaskStructure{structure}}, geometries)}
	}
	with := func(change func(*qchemTaskStructure)) qchemTaskStructure {
		structure := s(0, "x", "-1", true, false)
		change(&structure)
		return structure
	}
	good := one("opt", s(0, "x", "-1", true, true), h2Opt)
	for name, tt := range map[string]struct {
		outputs [][]byte
		blobs   Blobs
		want    string
	}{
		"an output that is not one":         {[][]byte{good[0], []byte("not json")}, store, "task 1 output"},
		"a task that left no summary":       {[][]byte{mustJSON(ContainerOutput{})}, store, "task 0's summary: the task left none"},
		"a summary that is not stored":      {[][]byte{mustJSON(ContainerOutput{Files: map[string]string{"summary.json": missing}})}, store, "task 0's summary: "},
		"a summary that is garbled":         {[][]byte{mustJSON(ContainerOutput{Files: map[string]string{"summary.json": garbled.String()}})}, store, "task 0's summary: invalid character"},
		"a method that is none":             {[][]byte{qchemTask(t, store, qchemSummary{Method: "b3lyp", Job: "sp"}, "")}, store, `task 0 says it ran "b3lyp" "sp", which is no method and job of xtb's`},
		"a job that is none":                {[][]byte{qchemTask(t, store, qchemSummary{Method: "gfn2", Job: "md"}, "")}, store, `task 0 says it ran "gfn2" "md"`},
		"tasks that disagree":               {append(one("sp", s(0, "x", "-1", true, false), ""), one("opt", s(1, "y", "-1", true, false), "")...), store, "task 1 ran gfn2 opt, and task 0 gfn2 sp"},
		"a task with no structures":         {[][]byte{qchemTask(t, store, qchemSummary{Method: "gfn2", Job: "sp"}, "")}, store, "task 0 computed no structures"},
		"a structure out of turn":           {one("sp", s(1, "x", "-1", true, false), ""), store, "task 0 gives structure 1 where structure 0 was due"},
		"a structure given twice":           {append(slices.Clone(good), good...), store, "task 1 gives structure 0 where structure 1 was due"},
		"an energy that is not a number":    {one("sp", with(func(s *qchemTaskStructure) { s.Energy = "NaN" }), ""), store, `task 0, structure 0: the energy "NaN" is not a number`},
		"a gap that is not a number":        {one("sp", with(func(s *qchemTaskStructure) { s.Gap = "1e999" }), ""), store, `the gap "1e999" is not a number`},
		"a free energy that is none":        {one("sp", with(func(s *qchemTaskStructure) { s.FreeEnergy = "0x10" }), ""), store, `the free energy "0x10" is not a number`},
		"a dipole of two numbers":           {one("sp", with(func(s *qchemTaskStructure) { s.Dipole = "0.1 0.2" }), ""), store, `the dipole "0.1 0.2" is not three numbers`},
		"a dipole of words":                 {one("sp", with(func(s *qchemTaskStructure) { s.Dipole = "0.1 0.2 north" }), ""), store, `the dipole "0.1 0.2 north" is not three numbers`},
		"converged with no energy":          {one("sp", with(func(s *qchemTaskStructure) { s.Energy = "" }), ""), store, "it is said to have converged, with no energy"},
		"a frequency with no intensity":     {one("hess", with(func(s *qchemTaskStructure) { s.Frequencies = [][]string{{"100.0"}} }), ""), store, `the frequency ["100.0"] is not a wave number and an intensity`},
		"a frequency that is a word":        {one("ohess", with(func(s *qchemTaskStructure) { s.Frequencies = [][]string{{"100.0", "lots"}} }), "\n"), store, "is not a wave number and an intensity"},
		"a geometry of a single point":      {one("sp", with(func(s *qchemTaskStructure) { s.Geometry = true }), ""), store, "task 0, structure 0: a geometry is said to be left that is not there"},
		"a geometry that is not there":      {one("opt", with(func(s *qchemTaskStructure) { s.Geometry = true }), h2Opt[:0]+"\n"), store, "a geometry is said to be left that is not there"},
		"geometries that were not stored":   {one("opt", with(func(s *qchemTaskStructure) { s.Geometry = true }), ""), store, "task 0's geometries: the task left none"},
		"geometries more than structures":   {one("opt", s(0, "x", "-1", true, true), h2Opt+h2Opt), store, "task 0 left 1 geometries more than its structures"},
		"geometries that are garbled":       {one("opt", s(0, "x", "-1", true, true), "two\n"), store, `task 0's geometries: line 1: "two" is not a number of atoms`},
		"geometries that cannot be read":    {good, &awkward{Blobs: store, badReads: true}, "task 0's summary: the store is full"},
		"a geometry that cannot be stored":  {good, &awkward{Blobs: store, failPut: 1}, "store structure 0's geometry: the store is full"},
		"frequencies that cannot be stored": {[][]byte{qchemTask(t, store, qchemSummary{Method: "gfn2", Job: "hess", Structures: []qchemTaskStructure{hess}}, "")}, &awkward{Blobs: store, failPut: 1}, "store structure 0's frequencies: the store is full"},
		"a table that cannot be stored":     {good, &awkward{Blobs: store, failPut: 2}, "store the table of results: the store is full"},
		"geometries that cannot be stored":  {good, &awkward{Blobs: store, failPut: 3}, "store the optimised geometries: the store is full"},
	} {
		if _, err := (Qchem{}).Aggregate(ctx, tt.blobs, tt.outputs); err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("%s: %v, want an error containing %q", name, err, tt.want)
		}
	}
	// A geometries file that is not stored.
	var out ContainerOutput
	json.Unmarshal(good[0], &out)
	out.Files["structures.xyz"] = missing
	if _, err := (Qchem{}).Aggregate(ctx, store, [][]byte{mustJSON(out)}); err == nil || !strings.Contains(err.Error(), "task 0's geometries: ") {
		t.Errorf("geometries that are not stored: %v", err)
	}
}

// qchemRun runs a qchem job's tasks in real containers, one after
// another, and puts their outputs together.
func qchemRun(t *testing.T, ctx context.Context, store Blobs, params string) (QchemResult, []byte, [][]byte) {
	t.Helper()
	w := Qchem{Confined: Confinement{ReadOnly: true}}
	payloads, err := w.Split(ctx, store, []byte(params), 1)
	if err != nil {
		t.Fatal(err)
	}
	outputs := make([][]byte, len(payloads))
	said := new(heard)
	for i, payload := range payloads {
		if outputs[i], err = w.Execute(WithReporter(ctx, said), store, payload); err != nil {
			t.Fatalf("task %d: %v\n%s", i, err, strings.Join(said.lines, "\n"))
		}
	}
	raw, err := w.Aggregate(ctx, store, outputs)
	if err != nil {
		t.Fatal(err)
	}
	var result QchemResult
	json.Unmarshal(raw, &result)
	return result, raw, outputs
}

// Three conformers of butane, anti and the two gauche, optimised with
// GFN2-xTB in two tasks, in real containers: each converges, to a plausible
// energy, the anti conformer is the lowest, and the gauche lie the 0.6
// kcal/mol or so above it that GFN2-xTB puts them at. Run again on this
// machine, and shared out otherwise, the job gives the same result to the
// bit, and so does each task.
func TestARealConformerSetIsOptimised(t *testing.T) {
	requireRealImages(t)
	requireDocker(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()
	// The image is built here if this machine has not got it, which takes
	// the network and a minute.
	if err := haveImage(ctx, docker, QchemImage, qchemDockerfile, "quantum chemistry"); err != nil {
		t.Fatal(err)
	}
	fixture, err := os.ReadFile("testdata/butane-conformers.xyz")
	if err != nil {
		t.Fatal(err)
	}
	store := storage.NewMemory()
	butane, _ := store.Put(ctx, strings.NewReader(string(fixture)))

	started := time.Now()
	result, raw, outputs := qchemRun(t, ctx, store, `{"input":"`+butane.String()+`","tasks":2}`)
	t.Logf("optimised in %s: %s", time.Since(started).Round(time.Millisecond), raw)
	if len(outputs) != 2 || result.Structures != 3 || result.Converged != 3 || result.Lowest == nil || *result.Lowest != 0 || result.Method != "gfn2" || result.Job != "opt" {
		t.Fatalf("the result: %s", raw)
	}
	for i, s := range result.Results {
		// GFN2-xTB puts butane near -13.66 hartree.
		if !s.Converged || s.Energy == nil || *s.Energy > -13.6 || *s.Energy < -13.7 || s.Gap == nil || *s.Gap < 5 || s.Dipole == nil || *s.Dipole > 0.5 || s.Geometry == "" || s.Error != "" {
			t.Errorf("conformer %d: %+v", i, s)
		}
	}
	if anti := result.Results[0]; anti.Title != "butane anti" || *anti.Relative != 0 {
		t.Errorf("the anti conformer: %+v", anti)
	}
	for _, gauche := range result.Results[1:] {
		if !strings.HasPrefix(gauche.Title, "butane gauche") || *gauche.Relative < 0.3 || *gauche.Relative > 1.2 {
			t.Errorf("a gauche conformer: %+v", gauche)
		}
	}
	geometries := readBlob(t, store, result.XYZ)
	if strings.Count(geometries, "\n14\n")+1 != 3 || !strings.HasPrefix(geometries, "14\nbutane anti | energy -13.6") {
		t.Errorf("the geometries:\n%s", geometries)
	}
	if _, n, err := countStructures(strings.NewReader(geometries)); n != 3 || err != nil {
		t.Errorf("the geometries are %d structures of XYZ: %v", n, err)
	}
	rows, _ := csv.NewReader(strings.NewReader(readBlob(t, store, result.CSV))).ReadAll()
	if len(rows) != 4 || rows[1][1] != "butane anti" || rows[1][8] != "true" {
		t.Errorf("the table: %q", rows)
	}

	// The same job again, and shared out as one task and as three: the
	// same result, byte for byte, and the same output from each task.
	_, again, outputsAgain := qchemRun(t, ctx, store, `{"input":"`+butane.String()+`","tasks":2}`)
	if string(again) != string(raw) || !slices.EqualFunc(outputs, outputsAgain, func(a, b []byte) bool { return string(a) == string(b) }) {
		t.Errorf("run again, the job gave\n%s\nand\n%s", raw, again)
	}
	for _, tasks := range []string{"1", "3"} {
		if _, other, _ := qchemRun(t, ctx, store, `{"input":"`+butane.String()+`","tasks":`+tasks+`}`); string(other) != string(raw) {
			t.Errorf("in %s tasks, the job gave\n%s\nnot\n%s", tasks, other, raw)
		}
	}

	// As SDF, with the same coordinates, the conformers come to the same
	// energies to the bit; and a structure xtb cannot optimise, two atoms
	// in one place, fails alone.
	var sdf strings.Builder
	eachStructure(strings.NewReader(string(fixture)), "xyz", func(title string, atoms []string) error {
		var block [][4]string
		for _, atom := range atoms {
			f := strings.Fields(atom)
			block = append(block, [4]string{f[0], f[1], f[2], f[3]})
		}
		sdf.WriteString(molfile(title, block...) + "$$$$\n")
		return nil
	})
	asSDF, _ := store.Put(ctx, strings.NewReader(sdf.String()))
	asXYZ, _ := store.Put(ctx, strings.NewReader(string(fixture)+"2\ntwo atoms in one place\nC 0 0 0\nC 0 0 0.0001\n"))
	fromSDF, rawSDF, _ := qchemRun(t, ctx, store, `{"input":"`+asSDF.String()+`","tasks":2}`)
	fromXYZ, rawXYZ, _ := qchemRun(t, ctx, store, `{"input":"`+asXYZ.String()+`","tasks":2}`)
	for i := range 3 {
		if a, b := fromSDF.Results[i], fromXYZ.Results[i]; a.Energy == nil || b.Energy == nil || *a.Energy != *b.Energy || a.Title != b.Title || !a.Converged {
			t.Errorf("structure %d from SDF and from XYZ:\n%s\n%s", i, rawSDF, rawXYZ)
		}
	}
	// Whether xtb got far enough to leave a last geometry depends on where
	// it gave up, and a result may name one when it did not converge, so
	// the test asks only that it failed and says why.
	if clash := fromXYZ.Results[3]; clash.Converged || clash.Error == "" || fromXYZ.Converged != 3 || fromXYZ.Structures != 4 || *fromXYZ.Lowest != 0 {
		t.Errorf("two atoms in one place: %s", rawXYZ)
	}

	// Frequencies in water, at the optimised geometry: a minimum, with
	// no imaginary frequency, and a free energy above the energy.
	solvated, rawSolvated, _ := qchemRun(t, ctx, store, `{"input":"`+butane.String()+`","job":"ohess","solvent":"water","tasks":3}`)
	for i, s := range solvated.Results {
		if !s.Converged || s.Imaginary == nil || *s.Imaginary != 0 || s.FreeEnergy == nil || *s.FreeEnergy <= *s.Energy || s.Frequencies == "" || s.Geometry == "" {
			t.Errorf("conformer %d with frequencies: %s", i, rawSolvated)
		}
	}
	if modes := strings.Count(readBlob(t, store, solvated.Results[0].Frequencies), "\n") - 1; modes != 3*14-6 {
		t.Errorf("butane has %d modes", modes)
	}
}

package runtime

import (
	"bufio"
	"bytes"
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/csv"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/ipfs/go-cid"
	"github.com/sisyphus-network/Sisyphus/packages/storage"
)

// Qchem computes molecules with xtb, the Grimme group's semiempirical
// tight-binding quantum chemistry: the energy of each of the structures in
// a stored file, its optimised geometry, or its vibrational frequencies.
// The structures, the conformers of a molecule or a library of molecules,
// are shared out in runs, one run to each task, and each task computes its
// structures one after another; the results are put back together in the
// file's order, ranked by energy.
//
// Like Transcode, it is the container workload with the image and the
// command fixed, so it runs only on workers that run containers. The image
// is built on a worker the first time it is needed, from xtb's statically
// linked release for Linux, checked against its SHA-256.
type Qchem struct {
	// Engine runs the container engine, as Container's does.
	Engine func(ctx context.Context, args []string, stdout, stderr io.Writer) (status int, err error)
	// Confined holds its containers in as Container's does.
	Confined Confinement
}

// QchemParams are a qchem job's parameters.
type QchemParams struct {
	// Input is the CID of the stored structures: an XYZ file of one or
	// more structures one after another, or an SDF file (V2000 molfiles)
	// of one or more molecules.
	Input string `json:"input"`
	// Method is one of qchemMethods. Empty means gfn2.
	Method string `json:"method,omitempty"`
	// Job is sp, a single point; opt, an optimised geometry; hess, the
	// frequencies at the geometry given; or ohess, the geometry optimised
	// and its frequencies. Empty means opt.
	Job string `json:"job,omitempty"`
	// Charge is each structure's total charge, and UHF how many of its
	// electrons are unpaired.
	Charge int `json:"charge,omitempty"`
	UHF    int `json:"uhf,omitempty"`
	// Solvent is one of qchemSolvents, for xtb's ALPB implicit solvation,
	// or none or empty for the gas phase.
	Solvent string `json:"solvent,omitempty"`
	// Convergence is how tightly a geometry is optimised, one of
	// qchemLevels. Empty means normal.
	Convergence string `json:"convergence,omitempty"`
	// Tasks is how many runs of structures to share the file out as. Zero
	// leaves it to the pool; there are never more runs than structures.
	Tasks int `json:"tasks,omitempty"`
}

// QchemResult is a qchem job's result.
type QchemResult struct {
	Method string `json:"method"`
	Job    string `json:"job"`
	// Structures is how many structures there were, and Converged how
	// many of them xtb finished: an optimisation that converged, or an
	// energy that it found.
	Structures int `json:"structures"`
	Converged  int `json:"converged"`
	// Lowest is the index of the structure of lowest energy among those
	// that converged, and LowestEnergy that energy in hartree. Neither is
	// there when none did.
	Lowest       *int     `json:"lowest,omitempty"`
	LowestEnergy *float64 `json:"lowest_energy_hartree,omitempty"`
	// CSV is the CID of a table of the results, a row for each structure;
	// XYZ is that of the optimised geometries, one after another, for a
	// job that optimises them.
	CSV     string           `json:"csv"`
	XYZ     string           `json:"xyz,omitempty"`
	Results []QchemStructure `json:"results"`
}

// QchemStructure is what one structure came to. A number xtb did not give
// is left out.
type QchemStructure struct {
	// Index is where the structure is in the file, from 0, and Title its
	// title line.
	Index int    `json:"index"`
	Title string `json:"title"`
	// Energy is the total energy, in hartree, and Relative how far above
	// the lowest it is, in kcal/mol.
	Energy   *float64 `json:"energy_hartree,omitempty"`
	Relative *float64 `json:"relative_kcal_mol,omitempty"`
	// Gap is the HOMO-LUMO gap in eV, and Dipole the dipole moment's size
	// in debye. GFN-FF, a force field, gives neither.
	Gap    *float64 `json:"gap_ev,omitempty"`
	Dipole *float64 `json:"dipole_debye,omitempty"`
	// FreeEnergy is the Gibbs free energy at 298.15 K, in hartree, which
	// a job that finds frequencies gives, and Imaginary how many of the
	// frequencies are imaginary: none at a minimum.
	FreeEnergy *float64 `json:"free_energy_hartree,omitempty"`
	Imaginary  *int     `json:"imaginary_frequencies,omitempty"`
	Converged  bool     `json:"converged"`
	// Error says how xtb failed, if it did.
	Error string `json:"error,omitempty"`
	// Geometry is the CID of the optimised geometry as XYZ, or the last
	// one reached where the optimisation did not converge; Frequencies
	// that of the vibrational frequencies, in cm⁻¹, with their infrared
	// intensities.
	Geometry    string `json:"geometry,omitempty"`
	Frequencies string `json:"frequencies,omitempty"`
}

// xtbRelease is the release of xtb the image is made from, and the SHA-256
// of its statically linked build for Linux on x86-64.
const xtbRelease, xtbSum = "6.7.1", "62a8d18778286e815292ee53d76ce447daf460a4dea3782c0f25cbac7019b5df"

// xtbCommit is the commit that release was made from, whose licence texts
// go into the image with xtb, with their SHA-256s.
const (
	xtbCommit      = "26b28010e805f7d1aeeef39813feb473e69cc4be"
	xtbCopying     = "3972dc9744f6499f0f9b2dbf76696f2ae7ad8af9b23dde66d6af86c9dfb36986"
	xtbCopyingLess = "e3a994d82e644b03a792a930f574002658412f62407f5fee083f2555c5f23118"
)

// qchemDockerfile is the whole of the image quantum chemistry runs in: xtb
// as its authors release it, checked against its digest, its parameters,
// and its licence; and jq, which the tasks write their summaries with.
// xtb's release is built for x86-64 alone, so the image is refused
// elsewhere, by name, rather than failing on every task.
const qchemDockerfile = `FROM alpine:3.20 AS fetch
RUN [ "$(uname -m)" = x86_64 ] || { echo "xtb is released for x86-64 alone, and this machine is $(uname -m)"; exit 1; }
RUN apk add --no-cache curl xz && curl -fsSL -o /xtb.tar.xz https://github.com/grimme-lab/xtb/releases/download/v` + xtbRelease + `/xtb-` + xtbRelease + `-linux-x86_64.tar.xz && echo "` + xtbSum + `  /xtb.tar.xz" | sha256sum -c - && tar -xJf /xtb.tar.xz -C /
RUN curl -fsSL -o /COPYING https://raw.githubusercontent.com/grimme-lab/xtb/` + xtbCommit + `/COPYING && curl -fsSL -o /COPYING.LESSER https://raw.githubusercontent.com/grimme-lab/xtb/` + xtbCommit + `/COPYING.LESSER && printf '%s  /COPYING\n%s  /COPYING.LESSER\n' ` + xtbCopying + ` ` + xtbCopyingLess + ` | sha256sum -c -
FROM alpine:3.20
RUN apk add --no-cache jq
COPY --from=fetch /xtb-dist/bin/xtb /usr/local/bin/xtb
COPY --from=fetch /xtb-dist/share/xtb /usr/local/share/xtb
COPY --from=fetch /COPYING /COPYING.LESSER /usr/local/share/licenses/xtb/
ENV XTBPATH=/usr/local/share/xtb
`

// QchemImage names that image, by what it is made from, as TranscodeImage
// does.
var QchemImage = func() string {
	sum := sha256.Sum256([]byte(qchemDockerfile))
	return "sisyphus-qchem:" + hex.EncodeToString(sum[:6])
}()

// qchemMethods are the methods a job may choose, with the arguments that
// ask xtb for each: GFN2-xTB, GFN1-xTB, GFN0-xTB and the GFN-FF force field.
var qchemMethods = map[string]string{"gfn2": "--gfn 2", "gfn1": "--gfn 1", "gfn0": "--gfn 0", "gfnff": "--gfnff"}

// qchemMethodNames and qchemJobs are the methods and the jobs, in the
// order a form offers them.
var (
	qchemMethodNames = []string{"gfn2", "gfn1", "gfn0", "gfnff"}
	qchemJobs        = []string{"sp", "opt", "hess", "ohess"}
)

// qchemSolvents are the solvents xtb 6.7.1 has ALPB parameters for, for
// each of GFN2-xTB, GFN1-xTB and GFN-FF, under the names it takes. GFN0-xTB
// has none of its own.
var qchemSolvents = []string{"acetone", "acetonitrile", "aniline", "benzaldehyde", "benzene", "ch2cl2", "chcl3", "cs2", "dioxane", "dmf", "dmso", "ethanol", "ether", "ethylacetate", "furane", "hexadecane", "hexane", "methanol", "nitromethane", "octanol", "phenol", "thf", "toluene", "water", "woctanol"}

// qchemLevels are how tightly xtb may optimise a geometry, loosest first.
var qchemLevels = []string{"crude", "sloppy", "loose", "lax", "normal", "tight", "vtight", "extreme"}

// Charges and unpaired electrons are held to what a molecule xtb can
// compute could have.
const qchemMostCharge, qchemMostUnpaired = 50, 50

// A file may hold at most qchemMostStructures structures, each of at most
// qchemMostAtoms atoms, in at most qchemMostInput bytes.
const qchemMostAtoms = 20_000

var (
	qchemMostStructures       = 1_000_000
	qchemMostInput      int64 = 1 << 30
)

// hartreeKcal is a hartree in kcal/mol, and auDebye an atomic unit of
// dipole moment in debye, as CODATA 2018 has them.
const hartreeKcal, auDebye = 627.5094740631, 2.541746473

// qchemScript is what each task runs. It takes its run of structures, from
// FIRST to before LAST, out of the file, each as XYZ; runs xtb on each in
// a directory of its own, on one thread, so that the same structure gives
// the same bits on the same machine however the work is shared out; and
// leaves what it found in summary.json, the optimised geometries in
// structures.xyz. A structure xtb fails on is noted as failed, and the
// task goes on to the next. Only fixed names are left in /output, and
// nothing is printed to stdout, so that two runs of a task on one machine
// leave the same output byte for byte: what xtb says, timings among it,
// goes to the task's log only where it failed. A number xtb gives that is
// not one, where it failed, is left out.
const qchemScript = `set -eu
export OMP_NUM_THREADS=1 MKL_NUM_THREADS=1 MKL_CBWR=COMPATIBLE
ulimit -s unlimited 2>/dev/null || true
work=$(mktemp -d)
awk -v format="$FORMAT" -v first="$FIRST" -v last="$LAST" -v dir="$work" '
BEGIN { k = 0 }
function keep(  name) {
	if (k >= first && k < last) { name = dir "/" k ".xyz"; printf "%d\n%s\n%s", n, title, atoms > name; close(name) }
	k++; atoms = ""; n = 0; line = 0
}
function field(from, width,  s) { s = substr($0, from, width); gsub(/[ \t]/, "", s); return s }
{ sub(/\r$/, "") }
format == "xyz" && state == 0 { if (NF > 0) { n = $1 + 0; state = 1 }; next }
format == "xyz" && state == 1 { title = $0; state = 2; got = 0; next }
format == "xyz" { atoms = atoms $1 " " $2 " " $3 " " $4 "\n"; if (++got == n) { keep(); state = 0 }; next }
NF == 1 && $1 == "$$$$" { if (line > 0) keep(); line = 0; next }
{ line++ }
line == 1 { title = $0 }
line == 4 { n = substr($0, 1, 3) + 0 }
line > 4 && line <= 4 + n { atoms = atoms field(32, 3) " " field(1, 10) " " field(11, 10) " " field(21, 10) "\n" }
END { if (format == "sdf" && n > 0 && line >= 4 + n) keep(); print k > (dir "/count") }
' /input/data
if [ "$(cat "$work/count")" != "$STRUCTURES" ]; then
	echo "the input has $(cat "$work/count") structures here, and $STRUCTURES when the job was split" >&2
	exit 1
fi
case "$JOB" in opt|ohess) optimise=yes ;; *) optimise=no ;; esac
: > "$work/summary.jsonl"
: > "$work/structures.xyz"
echo "computing structures $FIRST to $((LAST - 1)) of $STRUCTURES: xtb $XTB_ARGS" >&2
k=$FIRST
while [ "$k" -lt "$LAST" ]; do
	mkdir "$work/run$k"
	cd "$work/run$k"
	status=0
	xtb "$work/$k.xyz" $XTB_ARGS --json > out.txt 2> err.txt || status=$?
	value() { awk -v key="| $1 " 'index($0, key) { v = $(NF - 2) } END { if (v ~ /^-?[0-9]+\.[0-9]+$/) print v }' out.txt; }
	energy=$(value "TOTAL ENERGY")
	gap=$(value "HOMO-LUMO GAP")
	free=$(value "TOTAL FREE ENERGY")
	dipole=
	if [ -f xtbout.json ]; then dipole=$(jq -r '."dipole / a.u." // [] | map(tostring) | join(" ")' xtbout.json 2> /dev/null) || dipole=; fi
	frequencies=
	if [ -f vibspectrum ]; then frequencies=$(awk '$1 ~ /^[0-9]+$/ && $NF != "-" && $(NF - 2) $(NF - 1) ~ /^-?[0-9]+\.[0-9]+-?[0-9]+\.[0-9]+$/ { print $(NF - 2), $(NF - 1) }' vibspectrum); fi
	converged=no
	if [ "$status" = 0 ] && [ -n "$energy" ]; then
		if [ "$optimise" = no ] || [ -f .xtboptok ]; then converged=yes; fi
	fi
	geometry=no
	if [ "$optimise" = yes ]; then
		for left in xtbopt.xyz xtblast.xyz; do
			if [ -f "$left" ]; then cat "$left" >> "$work/structures.xyz"; geometry=yes; break; fi
		done
	fi
	jq -n -c --argjson index "$k" --arg title "$(sed -n 2p "$work/$k.xyz")" --argjson status "$status" \
		--arg energy "$energy" --arg gap "$gap" --arg free "$free" --arg dipole "$dipole" --arg frequencies "$frequencies" \
		--arg converged "$converged" --arg geometry "$geometry" \
		'{index: $index, title: $title, status: $status, energy: $energy, gap: $gap, free_energy: $free, dipole: $dipole,
		frequencies: ($frequencies | split("\n") | map(select(. != "") | split(" "))),
		converged: ($converged == "yes"), geometry: ($geometry == "yes")}' >> "$work/summary.jsonl"
	echo "structure $k: ${energy:-no} energy, converged: $converged" >&2
	if [ "$status" != 0 ]; then
		echo "xtb exited with status $status on structure $k:" >&2
		tail -n 5 out.txt err.txt >&2
	fi
	k=$((k + 1))
done
jq -s -c --arg method "$METHOD" --arg job "$JOB" '{method: $method, job: $job, structures: .}' "$work/summary.jsonl" > /output/summary.json
if [ "$optimise" = yes ]; then cp "$work/structures.xyz" /output/structures.xyz; fi
`

func (Qchem) Name() string { return "qchem" }

func (Qchem) Describe() string {
	return `Computes molecules with xtb's semiempirical quantum chemistry (GFN2-xTB, GFN1-xTB, GFN0-xTB or the GFN-FF force field): the energy, optimised geometry or vibrational frequencies of each structure in a stored file of conformers or molecules, the structures shared out in runs across the pool and ranked by energy. Parameters: {"input": "<CID of a stored XYZ file of one or more structures one after another, or an SDF file of V2000 molfiles>", "method": "<gfn2 (the default), gfn1, gfn0 or gfnff>", "job": "<sp for the energy, opt (the default) to optimise the geometry, hess for frequencies at the geometry given, ohess to optimise and then find frequencies>", "charge": <total charge of each structure, default 0>, "uhf": <unpaired electrons, default 0>, "solvent": "<none (the default), or an ALPB solvent: ` + strings.Join(qchemSolvents, ", ") + `>", "convergence": "<how tightly to optimise: ` + strings.Join(qchemLevels, ", ") + `; default normal>", "tasks": <optional number of runs of structures>}. Result: {"method", "job", "structures": <n>, "converged": <n>, "lowest": <index of the lowest-energy converged structure>, "lowest_energy_hartree": <E>, "csv": "<CID of a CSV table of the results>", "xyz": "<CID of the optimised geometries, one after another>", "results": [{"index", "title", "energy_hartree", "relative_kcal_mol", "gap_ev", "dipole_debye", "free_energy_hartree", "imaginary_frequencies", "converged", "error", "geometry": "<CID, XYZ>", "frequencies": "<CID, text>"}]}.`
}

func (Qchem) Form() []Field {
	return []Field{
		{Name: "input", Kind: KindFile, Label: "Structures", Help: "A stored XYZ file of one or more structures one after another, such as a molecule's conformers, or an SDF file of one or more molecules.", Required: true},
		{Name: "method", Kind: KindChoice, Label: "Method", Help: "GFN2-xTB for most work, GFN1-xTB or GFN0-xTB, or the GFN-FF force field for large systems.", Default: "gfn2", Choices: slices.Clone(qchemMethodNames)},
		{Name: "job", Kind: KindChoice, Label: "Job", Help: "sp finds the energy, opt optimises the geometry, hess finds the frequencies at the geometry given, and ohess optimises and then finds them.", Default: "opt", Choices: slices.Clone(qchemJobs)},
		{Name: "charge", Kind: KindInteger, Label: "Charge", Help: "The total charge of each structure.", Default: 0, Minimum: bound(-qchemMostCharge), Maximum: bound(qchemMostCharge)},
		{Name: "uhf", Kind: KindInteger, Label: "Unpaired electrons", Help: "How many of each structure's electrons are unpaired: 0 for a closed shell, 1 for a radical.", Default: 0, Minimum: bound(0), Maximum: bound(qchemMostUnpaired)},
		{Name: "solvent", Kind: KindChoice, Label: "Solvent", Help: "A solvent to surround each structure with, by xtb's ALPB model, or none for the gas phase.", Default: "none", Choices: append([]string{"none"}, qchemSolvents...)},
		{Name: "convergence", Kind: KindChoice, Label: "Convergence", Help: "How tightly to optimise a geometry, from crude to extreme.", Default: "normal", Choices: slices.Clone(qchemLevels)},
		{Name: "tasks", Kind: KindInteger, Label: "Runs", Help: "How many runs of structures to share the file out as, or 0 to leave it to the pool.", Default: 0, Minimum: bound(0), Maximum: bound(MaxTasks)},
	}
}

func (Qchem) Split(ctx context.Context, blobs Blobs, params []byte, parts int) ([][]byte, error) {
	var p QchemParams
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, fmt.Errorf("qchem parameters: %w", err)
	}
	input, err := cid.Decode(p.Input)
	if err != nil {
		return nil, fmt.Errorf("qchem parameters: input is not the CID of a stored file of structures: %w", err)
	}
	p.Method, p.Job, p.Convergence = cmp.Or(p.Method, "gfn2"), cmp.Or(p.Job, "opt"), cmp.Or(p.Convergence, "normal")
	if p.Solvent == "" {
		p.Solvent = "none"
	}
	switch {
	case qchemMethods[p.Method] == "":
		return nil, fmt.Errorf("qchem parameters: method is %q, and must be one of %s", p.Method, strings.Join(qchemMethodNames, ", "))
	case !slices.Contains(qchemJobs, p.Job):
		return nil, fmt.Errorf("qchem parameters: job is %q, and must be one of %s", p.Job, strings.Join(qchemJobs, ", "))
	case p.Solvent != "none" && !slices.Contains(qchemSolvents, p.Solvent):
		return nil, fmt.Errorf("qchem parameters: solvent is %q, and must be none or one of %s", p.Solvent, strings.Join(qchemSolvents, ", "))
	case p.Solvent != "none" && p.Method == "gfn0":
		return nil, errors.New("qchem parameters: xtb has no solvent parameters for GFN0-xTB; choose another method, or no solvent")
	case !slices.Contains(qchemLevels, p.Convergence):
		return nil, fmt.Errorf("qchem parameters: convergence is %q, and must be one of %s", p.Convergence, strings.Join(qchemLevels, ", "))
	case p.Charge < -qchemMostCharge || p.Charge > qchemMostCharge:
		return nil, fmt.Errorf("qchem parameters: charge is %d, and must be from %d to %d", p.Charge, -qchemMostCharge, qchemMostCharge)
	case p.UHF < 0 || p.UHF > qchemMostUnpaired:
		return nil, fmt.Errorf("qchem parameters: uhf is %d, and must be from 0 to %d", p.UHF, qchemMostUnpaired)
	case p.Tasks < 0 || p.Tasks > MaxTasks:
		return nil, fmt.Errorf("qchem parameters: tasks is %d, and must be from 1 to %d, or 0 to leave it to the pool", p.Tasks, MaxTasks)
	}
	blob, err := blobs.Open(ctx, input)
	if err != nil {
		return nil, fmt.Errorf("qchem input %s: %w", input, err)
	}
	format, sizes, err := countStructures(blob)
	blob.Close()
	if err != nil {
		return nil, fmt.Errorf("qchem input: %w", err)
	}
	structures := len(sizes)
	args := qchemMethods[p.Method] + " --chrg " + strconv.Itoa(p.Charge) + " --uhf " + strconv.Itoa(p.UHF)
	switch p.Job {
	case "opt", "ohess":
		args += " --" + p.Job + " " + p.Convergence
	case "hess":
		args += " --hess"
	}
	if p.Solvent != "none" {
		args += " --alpb " + p.Solvent
	}
	tasks, err := qchemTasks(sizes, p.Job, min(cmp.Or(p.Tasks, parts), structures))
	if err != nil {
		return nil, fmt.Errorf("qchem input: %w", err)
	}
	payloads, err := Container{}.Split(ctx, blobs, mustJSON(ContainerParams{
		Image: QchemImage, Command: []string{"sh", "-c", qchemScript},
		Input: p.Input, Tasks: tasks,
		Env: map[string]string{
			"FORMAT": format, "STRUCTURES": strconv.Itoa(structures),
			"METHOD": p.Method, "JOB": p.Job, "XTB_ARGS": args,
		},
	}), parts)
	// Each task is given its own run of structures, so that which ones are
	// whose is the coordinator's to say, in the task's payload.
	for i := range payloads {
		var task containerTask
		json.Unmarshal(payloads[i], &task) // made just now
		task.Env["FIRST"], task.Env["LAST"] = strconv.Itoa(structures*i/tasks), strconv.Itoa(structures*(i+1)/tasks)
		payloads[i] = mustJSON(task)
	}
	return payloads, err
}

func (q Qchem) Execute(ctx context.Context, blobs Blobs, payload []byte) ([]byte, error) {
	var task containerTask
	if err := json.Unmarshal(payload, &task); err != nil {
		return nil, fmt.Errorf("qchem payload: %w", err)
	}
	// The worker runs only the image of its own making.
	if task.Image != QchemImage {
		return nil, fmt.Errorf("qchem payload: %q is not the image quantum chemistry runs in", task.Image)
	}
	engine := q.Engine
	if engine == nil {
		engine = docker
	}
	if err := haveImage(ctx, engine, QchemImage, qchemDockerfile, "quantum chemistry"); err != nil {
		return nil, err
	}
	// What the container leaves is read here and said inline, not stored:
	// honest workers' numbers differ in their last digits from one kind
	// of processor to another, so files of them would be named
	// differently, and results that store different blobs are never the
	// same. Inline, Compare can hold them to a tolerance instead.
	held := &heldBlobs{Blobs: blobs, held: storage.NewMemory()}
	raw, err := Container{Engine: engine, Confined: q.Confined, MaxOutput: qchemMostOutput}.Execute(ctx, held, payload)
	if err != nil {
		return nil, err
	}
	var out ContainerOutput
	json.Unmarshal(raw, &out) // made just now
	var summary qchemSummary
	if err := held.left(ctx, out, "summary.json", func(r io.Reader) error {
		return json.NewDecoder(io.LimitReader(r, qchemMostOutput)).Decode(&summary)
	}); err != nil {
		return nil, fmt.Errorf("the task's summary: %w", err)
	}
	if summary.Job == "opt" || summary.Job == "ohess" {
		var blocks []string
		if err := held.left(ctx, out, "structures.xyz", func(r io.Reader) error {
			return eachStructure(r, "xyz", func(_ string, atoms []string) error {
				blocks = append(blocks, strings.Join(atoms, "\n"))
				return nil
			})
		}); err != nil {
			return nil, fmt.Errorf("the task's geometries: %w", err)
		}
		for i := range summary.Structures {
			if s := &summary.Structures[i]; s.Geometry && len(blocks) > 0 {
				s.Atoms, blocks = strings.Split(blocks[0], "\n"), blocks[1:]
			}
		}
	}
	output := mustJSON(summary)
	if len(output) > qchemMostOutput {
		return nil, fmt.Errorf("the task came to %d bytes, more than the %d it may say", len(output), qchemMostOutput)
	}
	return output, nil
}

// heldBlobs stores what is put in it in memory, apart from the worker's
// store, and opens from the worker's store.
type heldBlobs struct {
	Blobs
	held *storage.Store
}

func (h *heldBlobs) Put(ctx context.Context, r io.Reader) (cid.Cid, error) {
	return h.held.Put(ctx, r)
}

// left reads the file of the name given that a task left, which is held
// here, with read.
func (h *heldBlobs) left(ctx context.Context, out ContainerOutput, name string, read func(io.Reader) error) error {
	c, err := cid.Decode(out.Files[name])
	if err != nil {
		return errors.New("the task left none")
	}
	blob, err := h.held.Open(ctx, c)
	if err == nil {
		defer blob.Close()
		err = read(blob)
	}
	return err
}

// Varies says that two honest workers can give different bytes for a task.
// On one kind of processor they do not: xtb is one statically linked build,
// run on one thread with MKL held to the same arithmetic whatever the
// processor, and a task leaves only what xtb found, with no timing, so two
// runs of it, or of the job shared out otherwise, agree to the bit. But the
// build also picks its own arithmetic by the processor it finds, and run on
// processors with AVX2 and FMA, or on AMD's, the last digits xtb prints
// differ: a gap in its eleventh decimal, a coordinate in its tenth. So its
// results are compared within a tolerance, by Compare.
func (Qchem) Varies() bool { return true }

// qchemTolerances are how far apart two runs of a task may be and still be
// the same result.
type qchemTolerances struct {
	energy    float64 // hartree, for the energy and free energy
	gap       float64 // eV
	dipole    float64 // atomic units, each component
	rmsd      float64 // ångström, the geometries aligned
	wave      float64 // cm⁻¹
	intensity float64 // km/mol, and as much again a percent
}

// The tolerances Compare holds two runs of a task to, in two tiers.
//
// Across processors: measured on one machine with MKL's code paths for
// SSE4.2, AVX and its own compatible arithmetic, which differ from one
// another as processors do, the same butane optimisation and frequencies
// differed by 1e-12 hartree in the energy, 1e-10 in the free energy, 5e-8
// eV in the gap and 2e-9 ångström in a coordinate, not at all in a
// frequency as xtb prints it, and 2e-5 km/mol in an intensity. The
// tolerances are far wider than that, for processors that differ more and
// optimisations that end a little apart, and far narrower than any
// difference that would matter to a chemist: a millionth of a hartree is
// 0.0006 kcal/mol.
//
// On one kind of processor: xtb runs on one thread with MKL's arithmetic
// pinned, and gives the same bits run after run however a job is split, so
// two honest runs there are byte-identical. The tolerances are a hundred
// times tighter in energy and a thousand in the gap, ten in frequency and a
// hundred in geometry, which leaves a dishonest worker far less room, and
// are still wider than every difference measured across code paths above.
var (
	qchemCrossCPU = qchemTolerances{energy: 1e-6, gap: 1e-3, dipole: 1e-3, rmsd: 1e-3, wave: 1.0, intensity: 0.01}
	qchemSameCPU  = qchemTolerances{energy: 1e-8, gap: 1e-6, dipole: 1e-5, rmsd: 1e-5, wave: 0.1, intensity: 0.001}
)

// Compare says whether two outputs of one task are the same result, to
// within the tolerances across processors, since it does not know where
// they were computed; see CompareFrom.
func (Qchem) Compare(a, b []byte) (bool, string) {
	return compareQchem(a, b, qchemCrossCPU)
}

// CompareFrom compares two outputs of one task within the tolerances for
// one kind of processor when the coordinator knows both were computed on
// the same kind, and within those across processors otherwise: when they
// were computed on different kinds, or either kind is unknown.
func (Qchem) CompareFrom(a []byte, aFrom Origin, b []byte, bFrom Origin) (bool, string, string) {
	tier, within := TierCrossCPU, qchemCrossCPU
	if SameCPU(aFrom, bFrom) {
		tier, within = TierSameCPU, qchemSameCPU
	}
	same, why := compareQchem(a, b, within)
	return same, tier, why
}

// compareQchem says whether two outputs of one task are the same result,
// to within the given tolerances: the same structures, each with the same
// title, status and outcome, its numbers within their tolerances, its
// optimised geometry within the RMSD tolerance of the other once the two
// are laid over each other, which honest optimisations that drift apart
// as a whole do not hold against them, and its frequencies, in order,
// each within its tolerance. The outputs are taken in the order of their
// bytes, so that which is given first never changes the answer.
func compareQchem(a, b []byte, within qchemTolerances) (bool, string) {
	if bytes.Compare(a, b) > 0 {
		a, b = b, a
	}
	var x, y qchemSummary
	if json.Unmarshal(a, &x) != nil || json.Unmarshal(b, &y) != nil {
		return false, "an output is not a qchem task's"
	}
	switch {
	case x.Method != y.Method || x.Job != y.Job:
		return false, fmt.Sprintf("one ran %s %s, the other %s %s", x.Method, x.Job, y.Method, y.Job)
	case len(x.Structures) != len(y.Structures):
		return false, fmt.Sprintf("one computed %d structures, the other %d", len(x.Structures), len(y.Structures))
	}
	for i := range x.Structures {
		if why := compareStructures(x.Structures[i], y.Structures[i], within); why != "" {
			return false, fmt.Sprintf("structure %d: %s", x.Structures[i].Index, why)
		}
	}
	return true, ""
}

// compareStructures says how what two runs said of one structure differs
// by more than the tolerances, or nothing where it does not.
func compareStructures(x, y qchemTaskStructure, within qchemTolerances) string {
	switch {
	case x.Index != y.Index || x.Title != y.Title:
		return "they are not the same structure"
	case x.Status != y.Status || x.Converged != y.Converged || x.Geometry != y.Geometry:
		return fmt.Sprintf("one ended with status %d, converged %t, the other with status %d, converged %t", x.Status, x.Converged, y.Status, y.Converged)
	}
	for _, n := range []struct {
		what      string
		x, y      string
		tolerance float64
	}{
		{"energy", x.Energy, y.Energy, within.energy},
		{"free energy", x.FreeEnergy, y.FreeEnergy, within.energy},
		{"gap", x.Gap, y.Gap, within.gap},
	} {
		if why := qchemWithin(n.what, n.x, n.y, n.tolerance); why != "" {
			return why
		}
	}
	if why := compareFrequencies(x.Frequencies, y.Frequencies, within); why != "" {
		return why
	}
	turn, why := compareGeometries(x.Atoms, y.Atoms, within.rmsd)
	if why != "" {
		return why
	}
	return compareDipoles(x.Dipole, y.Dipole, turn, within.dipole)
}

// compareDipoles compares two runs' dipoles, each component within the
// tolerance, once the first is turned as its geometry turns to
// lie over the other's: a dipole turns with its molecule, so two runs whose
// optimisations end turned apart give the same dipole in different axes.
func compareDipoles(x, y string, turn [3][3]float64, tolerance float64) string {
	dx, dy := strings.Fields(x), strings.Fields(y)
	switch {
	case len(dx) != len(dy):
		return "one gave a dipole and the other none"
	case len(dx) == 0:
		return ""
	case len(dx) != 3:
		return fmt.Sprintf("the dipole %q or %q is not three numbers", x, y)
	}
	var a, b [3]float64
	for i := range 3 {
		var errA, errB error
		a[i], errA = strconv.ParseFloat(dx[i], 64)
		b[i], errB = strconv.ParseFloat(dy[i], 64)
		if errA != nil || errB != nil || !decimal.MatchString(dx[i]) || !decimal.MatchString(dy[i]) {
			return fmt.Sprintf("the dipole %q or %q is not a number", dx[i], dy[i])
		}
	}
	for i := range 3 {
		turned := turn[i][0]*a[0] + turn[i][1]*a[1] + turn[i][2]*a[2]
		if gap := math.Abs(turned - b[i]); !(gap <= tolerance) {
			return fmt.Sprintf("the dipole is %q in one and %q in the other, %.3g apart in one direction once the geometries are laid over each other, more than %g", x, y, gap, tolerance)
		}
	}
	return ""
}

// qchemWithin says how two numbers as a task gave them differ by more than the
// tolerance, or are not both numbers, or nothing where they agree. Two
// that are both empty, none given, agree.
func qchemWithin(what, x, y string, tolerance float64) string {
	if x == "" || y == "" {
		if x != y {
			return "one gave the " + what + " and the other none"
		}
		return ""
	}
	a, errA := strconv.ParseFloat(x, 64)
	b, errB := strconv.ParseFloat(y, 64)
	switch {
	case errA != nil || errB != nil || !decimal.MatchString(x) || !decimal.MatchString(y):
		return fmt.Sprintf("the %s %q or %q is not a number", what, x, y)
	case math.Abs(a-b) > tolerance:
		return fmt.Sprintf("the %s is %s in one and %s in the other, %.3g apart, more than %g", what, x, y, math.Abs(a-b), tolerance)
	}
	return ""
}

// compareFrequencies compares two runs' frequencies, each a wave number
// and an intensity, in order of wave number, where near-degenerate modes
// may come in either order.
func compareFrequencies(x, y [][]string, within qchemTolerances) string {
	if len(x) != len(y) {
		return fmt.Sprintf("one gave %d frequencies, the other %d", len(x), len(y))
	}
	sorted := func(modes [][]string) ([][2]float64, bool) {
		out := make([][2]float64, len(modes))
		for i, mode := range modes {
			if len(mode) != 2 || !decimal.MatchString(mode[0]) || !decimal.MatchString(mode[1]) {
				return nil, false
			}
			out[i][0], _ = strconv.ParseFloat(mode[0], 64)
			out[i][1], _ = strconv.ParseFloat(mode[1], 64)
		}
		slices.SortFunc(out, func(a, b [2]float64) int { return cmp.Compare(a[0], b[0]) })
		return out, true
	}
	a, okA := sorted(x)
	b, okB := sorted(y)
	if !okA || !okB {
		return "a frequency is not a wave number and an intensity"
	}
	for i := range a {
		wave, intensity := math.Abs(a[i][0]-b[i][0]), math.Abs(a[i][1]-b[i][1])
		if wave > within.wave || intensity > within.intensity*(1+math.Max(math.Abs(a[i][1]), math.Abs(b[i][1]))) {
			return fmt.Sprintf("mode %d is %g cm-1 at %g km/mol in one and %g cm-1 at %g km/mol in the other", i+1, a[i][0], a[i][1], b[i][0], b[i][1])
		}
	}
	return ""
}

// compareGeometries compares two runs' optimised geometries: the same
// elements in the same order, and the atoms, once the geometries are laid
// over each other as closely as they go, at most the tolerance apart in
// root mean square. It gives the turn that lays the first over the
// second, which is no turn where there are no geometries.
func compareGeometries(x, y []string, tolerance float64) ([3][3]float64, string) {
	none := [3][3]float64{{1, 0, 0}, {0, 1, 0}, {0, 0, 1}}
	if len(x) != len(y) {
		return none, fmt.Sprintf("one gave a geometry of %d atoms, the other of %d", len(x), len(y))
	}
	if len(x) == 0 {
		return none, ""
	}
	p, q := make([][3]float64, len(x)), make([][3]float64, len(y))
	for i := range x {
		a, b := blankFields(x[i]), blankFields(y[i])
		if len(a) != 4 || len(b) != 4 || a[0] != b[0] {
			return none, fmt.Sprintf("atom %d is %q in one and %q in the other", i+1, x[i], y[i])
		}
		for k := range 3 {
			var errA, errB error
			p[i][k], errA = strconv.ParseFloat(a[k+1], 64)
			q[i][k], errB = strconv.ParseFloat(b[k+1], 64)
			if errA != nil || errB != nil || !decimal.MatchString(a[k+1]) || !decimal.MatchString(b[k+1]) {
				return none, fmt.Sprintf("atom %d is %q in one and %q in the other", i+1, x[i], y[i])
			}
		}
	}
	rmsd, turn := alignment(p, q)
	if !(rmsd <= tolerance) {
		return none, fmt.Sprintf("the geometries are %.3g ångström apart in root mean square, laid over each other, more than %g", rmsd, tolerance)
	}
	return turn, ""
}

// alignment is the root mean square distance between the atoms of two
// geometries of the same atoms once the first is moved and turned to lie
// over the second as closely as it goes, and the turn that does it, found
// as Horn's quaternion method finds them: from the largest eigenvalue of a
// 4×4 matrix made from the two, and its eigenvector, the turn's quaternion.
func alignment(p, q [][3]float64) (float64, [3][3]float64) {
	n := float64(len(p))
	var cp, cq [3]float64
	for i := range p {
		for k := range 3 {
			cp[k] += p[i][k] / n
			cq[k] += q[i][k] / n
		}
	}
	var s [3][3]float64
	var squares float64
	for i := range p {
		var a, b [3]float64
		for k := range 3 {
			a[k], b[k] = p[i][k]-cp[k], q[i][k]-cq[k]
			squares += a[k]*a[k] + b[k]*b[k]
		}
		for j := range 3 {
			for k := range 3 {
				s[j][k] += a[j] * b[k]
			}
		}
	}
	m := [4][4]float64{
		{s[0][0] + s[1][1] + s[2][2], s[1][2] - s[2][1], s[2][0] - s[0][2], s[0][1] - s[1][0]},
		{s[1][2] - s[2][1], s[0][0] - s[1][1] - s[2][2], s[0][1] + s[1][0], s[2][0] + s[0][2]},
		{s[2][0] - s[0][2], s[0][1] + s[1][0], -s[0][0] + s[1][1] - s[2][2], s[1][2] + s[2][1]},
		{s[0][1] - s[1][0], s[2][0] + s[0][2], s[1][2] + s[2][1], -s[0][0] - s[1][1] + s[2][2]},
	}
	value, v := largestEigen(m)
	w, x, y, z := v[0], v[1], v[2], v[3]
	turn := [3][3]float64{
		{w*w + x*x - y*y - z*z, 2 * (x*y - w*z), 2 * (x*z + w*y)},
		{2 * (y*x + w*z), w*w - x*x + y*y - z*z, 2 * (y*z - w*x)},
		{2 * (z*x - w*y), 2 * (z*y + w*x), w*w - x*x - y*y + z*z},
	}
	return math.Sqrt(math.Max(0, (squares-2*value)/n)), turn
}

// largestEigen finds the largest eigenvalue of a symmetric 4×4 matrix, and
// its eigenvector of unit length, by Jacobi's method: turning it, a pair of
// rows and columns at a time, until what is off its diagonal is gone, and
// keeping the turns, whose columns are then the eigenvectors.
func largestEigen(m [4][4]float64) (float64, [4]float64) {
	v := [4][4]float64{{1, 0, 0, 0}, {0, 1, 0, 0}, {0, 0, 1, 0}, {0, 0, 0, 1}}
	for range 64 {
		var off float64
		for i := range 4 {
			for j := i + 1; j < 4; j++ {
				off += m[i][j] * m[i][j]
			}
		}
		if off < 1e-300 {
			break
		}
		for i := range 4 {
			for j := i + 1; j < 4; j++ {
				if m[i][j] == 0 {
					continue
				}
				theta := (m[j][j] - m[i][i]) / (2 * m[i][j])
				t := 1 / (math.Abs(theta) + math.Sqrt(theta*theta+1))
				if theta < 0 {
					t = -t
				}
				c := 1 / math.Sqrt(t*t+1)
				s := t * c
				for k := range 4 {
					m[k][i], m[k][j] = c*m[k][i]-s*m[k][j], s*m[k][i]+c*m[k][j]
				}
				for k := range 4 {
					m[i][k], m[j][k] = c*m[i][k]-s*m[j][k], s*m[i][k]+c*m[j][k]
				}
				for k := range 4 {
					v[k][i], v[k][j] = c*v[k][i]-s*v[k][j], s*v[k][i]+c*v[k][j]
				}
			}
		}
	}
	best := 0
	for i := 1; i < 4; i++ {
		if m[i][i] > m[best][best] {
			best = i
		}
	}
	return m[best][best], [4]float64{v[0][best], v[1][best], v[2][best], v[3][best]}
}

// qchemTaskStructure is what a task says of one structure, its numbers as
// xtb printed them.
type qchemTaskStructure struct {
	Index       int        `json:"index"`
	Title       string     `json:"title"`
	Status      int        `json:"status"`
	Energy      string     `json:"energy"`
	Gap         string     `json:"gap"`
	FreeEnergy  string     `json:"free_energy"`
	Dipole      string     `json:"dipole"`
	Frequencies [][]string `json:"frequencies"`
	Converged   bool       `json:"converged"`
	Geometry    bool       `json:"geometry"`
	// Atoms are the optimised geometry's atoms, each an element and three
	// coordinates in ångström, which Execute adds where Geometry says
	// there is one.
	Atoms []string `json:"atoms,omitempty"`
}

// qchemSummary is what a task leaves in summary.json.
type qchemSummary struct {
	Method     string               `json:"method"`
	Job        string               `json:"job"`
	Structures []qchemTaskStructure `json:"structures"`
}

func (Qchem) Aggregate(ctx context.Context, blobs Blobs, outputs [][]byte) ([]byte, error) {
	var result QchemResult
	var table bytes.Buffer
	rows := csv.NewWriter(&table)
	rows.Write([]string{"index", "title", "energy_hartree", "relative_kcal_mol", "gap_ev", "dipole_debye", "free_energy_hartree", "imaginary_frequencies", "converged", "error"})
	var geometries []string
	var combined bytes.Buffer
	for i, output := range outputs {
		var summary qchemSummary
		if err := json.Unmarshal(output, &summary); err != nil {
			return nil, fmt.Errorf("task %d output: %w", i, err)
		}
		if i == 0 {
			result.Method, result.Job = summary.Method, summary.Job
		}
		switch {
		case qchemMethods[summary.Method] == "" || !slices.Contains(qchemJobs, summary.Job):
			return nil, fmt.Errorf("task %d says it ran %q %q, which is no method and job of xtb's", i, summary.Method, summary.Job)
		case summary.Method != result.Method || summary.Job != result.Job:
			return nil, fmt.Errorf("task %d ran %s %s, and task 0 %s %s", i, summary.Method, summary.Job, result.Method, result.Job)
		case len(summary.Structures) == 0:
			return nil, fmt.Errorf("task %d computed no structures", i)
		}
		optimised := summary.Job == "opt" || summary.Job == "ohess"
		for _, s := range summary.Structures {
			// The tasks' runs follow one another, from the first
			// structure, with none left out or twice.
			if s.Index != len(result.Results) {
				return nil, fmt.Errorf("task %d gives structure %d where structure %d was due", i, s.Index, len(result.Results))
			}
			done, err := qchemStructure(s, summary.Job)
			if err != nil {
				return nil, fmt.Errorf("task %d, structure %d: %w", i, s.Index, err)
			}
			switch {
			case s.Geometry != (len(s.Atoms) > 0) || s.Geometry && !optimised:
				return nil, fmt.Errorf("task %d, structure %d: a geometry is said to be left that is not there, or one is there that is not said to be", i, s.Index)
			case s.Geometry:
				if err := qchemAtoms(s.Atoms); err != nil {
					return nil, fmt.Errorf("task %d, structure %d: %w", i, s.Index, err)
				}
				geometries = append(geometries, xyzBlock(strings.Join(s.Atoms, "\n")+"\n", done))
			default:
				geometries = append(geometries, "")
			}
			result.Results = append(result.Results, done)
		}
	}
	// The lowest energy among the structures xtb finished, which the
	// others are measured from.
	for _, s := range result.Results {
		if s.Converged && (result.LowestEnergy == nil || *s.Energy < *result.LowestEnergy) {
			result.Lowest, result.LowestEnergy = &s.Index, s.Energy
		}
	}
	for i := range result.Results {
		s := &result.Results[i]
		if s.Converged {
			result.Converged++
		}
		if s.Energy != nil && result.LowestEnergy != nil {
			relative := (*s.Energy - *result.LowestEnergy) * hartreeKcal
			s.Relative = &relative
		}
		if geometries[i] != "" {
			stored, err := blobs.Put(ctx, strings.NewReader(geometries[i]))
			if err != nil {
				return nil, fmt.Errorf("store structure %d's geometry: %w", s.Index, err)
			}
			s.Geometry = stored.String()
			combined.WriteString(geometries[i])
		}
		rows.Write([]string{strconv.Itoa(s.Index), s.Title, number(s.Energy), number(s.Relative), number(s.Gap), number(s.Dipole), number(s.FreeEnergy), countOf(s.Imaginary), strconv.FormatBool(s.Converged), s.Error})
	}
	result.Structures = len(result.Results)
	// The frequencies were kept as text in Frequencies until now.
	for i := range result.Results {
		s := &result.Results[i]
		if s.Frequencies == "" {
			continue
		}
		stored, err := blobs.Put(ctx, strings.NewReader(s.Frequencies))
		if err != nil {
			return nil, fmt.Errorf("store structure %d's frequencies: %w", s.Index, err)
		}
		s.Frequencies = stored.String()
	}
	rows.Flush()
	stored, err := blobs.Put(ctx, &table)
	if err != nil {
		return nil, fmt.Errorf("store the table of results: %w", err)
	}
	result.CSV = stored.String()
	if combined.Len() > 0 {
		if stored, err = blobs.Put(ctx, &combined); err != nil {
			return nil, fmt.Errorf("store the optimised geometries: %w", err)
		}
		result.XYZ = stored.String()
	}
	return mustJSON(result), nil
}

// qchemStructure checks what a task says of one structure, and turns it
// into the structure's result: numbers as numbers, and the frequencies as
// the text of the file they are stored as.
func qchemStructure(s qchemTaskStructure, job string) (QchemStructure, error) {
	done := QchemStructure{Index: s.Index, Title: oneLine(s.Title), Converged: s.Converged}
	var err error
	if done.Energy, err = maybeNumber("energy", s.Energy); err != nil {
		return done, err
	}
	if done.Gap, err = maybeNumber("gap", s.Gap); err != nil {
		return done, err
	}
	if done.FreeEnergy, err = maybeNumber("free energy", s.FreeEnergy); err != nil {
		return done, err
	}
	if s.Dipole != "" {
		var size float64
		parts := strings.Fields(s.Dipole)
		for _, part := range parts {
			v, err := strconv.ParseFloat(part, 64)
			if err != nil || !decimal.MatchString(part) {
				return done, fmt.Errorf("the dipole %q is not three numbers", s.Dipole)
			}
			size += v * v
		}
		if len(parts) != 3 {
			return done, fmt.Errorf("the dipole %q is not three numbers", s.Dipole)
		}
		debye := math.Sqrt(size) * auDebye
		done.Dipole = &debye
	}
	if s.Converged && done.Energy == nil {
		return done, errors.New("it is said to have converged, with no energy")
	}
	if s.Status != 0 {
		done.Error = "xtb exited with status " + strconv.Itoa(s.Status)
	}
	if job == "hess" || job == "ohess" {
		imaginary := 0
		var text strings.Builder
		for _, f := range s.Frequencies {
			if len(f) != 2 || !decimal.MatchString(f[0]) || !decimal.MatchString(f[1]) {
				return done, fmt.Errorf("the frequency %q is not a wave number and an intensity", f)
			}
			if strings.HasPrefix(f[0], "-") && strings.Trim(f[0], "-0.") != "" {
				imaginary++
			}
			fmt.Fprintf(&text, "%s %s\n", f[0], f[1])
		}
		if len(s.Frequencies) > 0 {
			done.Frequencies = "# wave number (cm-1), infrared intensity (km/mol); imaginary frequencies are negative\n" + text.String()
			done.Imaginary = &imaginary
		}
	}
	return done, nil
}

// decimal is what a number in xtb's output, or in a structure's
// coordinates, looks like: digits, a point, and an exponent, but nothing
// ParseFloat would also take, such as Inf or hexadecimal.
var decimal = regexp.MustCompile(`^[+-]?([0-9]+\.?[0-9]*|\.[0-9]+)([eE][+-]?[0-9]+)?$`)

// maybeNumber reads a number a task gave as text, where empty is none.
func maybeNumber(what, text string) (*float64, error) {
	if text == "" {
		return nil, nil
	}
	v, err := strconv.ParseFloat(text, 64)
	if err != nil || !decimal.MatchString(text) {
		return nil, fmt.Errorf("the %s %q is not a number", what, text)
	}
	return &v, nil
}

// number and countOf write a number of the table, or nothing for none.
func number(v *float64) string {
	if v == nil {
		return ""
	}
	return strconv.FormatFloat(*v, 'f', -1, 64)
}

func countOf(n *int) string {
	if n == nil {
		return ""
	}
	return strconv.Itoa(*n)
}

// oneLine makes a title one line, with no control characters, so that it
// can be an XYZ file's title line.
func oneLine(title string) string {
	return strings.Map(func(r rune) rune {
		if r < ' ' || r == 0x7f {
			return ' '
		}
		return r
	}, title)
}

// qchemAtoms checks a geometry a task gave: each atom an element and three
// coordinates, as an XYZ file has them.
func qchemAtoms(atoms []string) error {
	for _, atom := range atoms {
		fields := blankFields(atom)
		if len(fields) != 4 || !element.MatchString(fields[0]) || !decimal.MatchString(fields[1]) || !decimal.MatchString(fields[2]) || !decimal.MatchString(fields[3]) {
			return fmt.Errorf("%q is not an atom of a geometry, an element and three coordinates", atom)
		}
	}
	return nil
}

// xyzBlock writes an optimised geometry, whose atoms lines are given, as
// one structure of an XYZ file, with the structure's title and energy as
// its title line.
func xyzBlock(atoms string, s QchemStructure) string {
	title := s.Title
	if s.Energy != nil {
		title += " | energy " + number(s.Energy) + " Eh"
	}
	return strconv.Itoa(strings.Count(atoms, "\n")) + "\n" + title + "\n" + atoms
}

// countStructures finds what format a file of structures is in, and the
// size of each structure it holds, checking each as the tasks will read
// it. A file whose fourth line is a molfile's counts line is SDF; any
// other, XYZ.
func countStructures(r io.Reader) (format string, sizes []qchemSize, err error) {
	in := bufio.NewReader(io.LimitReader(r, qchemMostInput+1))
	head, _ := in.Peek(4096)
	format = "xyz"
	if lines := strings.SplitN(string(head), "\n", 5); len(lines) > 3 {
		switch counts := lines[3]; {
		case strings.Contains(counts, "V3000"):
			return "", nil, errors.New("it is an SDF file of V3000 molfiles, and only V2000 ones are read: save it as V2000 or XYZ")
		case strings.Contains(counts, "V2000"):
			format = "sdf"
		}
	}
	counted := &counted{r: in}
	err = eachStructure(counted, format, func(title string, atoms []string) error {
		sizes = append(sizes, qchemSize{atoms: len(atoms), title: len(title)})
		if len(sizes) > qchemMostStructures {
			return fmt.Errorf("it holds more than the %d structures a job may have", qchemMostStructures)
		}
		return nil
	})
	switch {
	case counted.n > uint64(qchemMostInput):
		return "", nil, fmt.Errorf("it is larger than the %d bytes a job's structures may be", qchemMostInput)
	case err != nil:
		return "", nil, err
	case len(sizes) == 0:
		return "", nil, errors.New("it holds no structures")
	}
	return format, sizes, nil
}

// qchemSize is how many atoms a structure has, and how long its title is,
// which is what decides how much a task says of it.
type qchemSize struct{ atoms, title int }

// qchemMostOutput is how much a task may say, inline, of its run of
// structures: well within the 4 MiB a message between nodes may be.
const qchemMostOutput = 3 << 20

// output is at most how many bytes a task says of a structure of this
// size, in a job of this kind: its numbers, its title, which JSON may
// write at six bytes a character, and, where the job finds them, the
// optimised geometry, at most 96 bytes an atom as xtb writes it, and the
// frequencies, three an atom, at most 32 bytes each.
func (s qchemSize) output(job string) int {
	n := 512 + 6*s.title
	if job == "opt" || job == "ohess" {
		n += 96 * s.atoms
	}
	if job == "hess" || job == "ohess" {
		n += 3 * 32 * s.atoms
	}
	return n
}

// qchemTasks is how many runs to share structures of these sizes out as:
// as many as were asked for, or more, as few more as there must be for no
// task to say more of its run than qchemMostOutput. Runs are as even in
// number as qchem's Split makes them.
func qchemTasks(sizes []qchemSize, job string, asked int) (int, error) {
	total := make([]int, len(sizes)+1)
	for i, size := range sizes {
		if n := size.output(job); n > qchemMostOutput {
			return 0, fmt.Errorf("structure %d, of %d atoms, is more than a task can say of in a %s job: what it computes would come to %d bytes, more than %d", i, size.atoms, job, n, qchemMostOutput)
		}
		total[i+1] = total[i] + size.output(job)
	}
	fits := func(tasks int) bool {
		for i := range tasks {
			if total[len(sizes)*(i+1)/tasks]-total[len(sizes)*i/tasks] > qchemMostOutput {
				return false
			}
		}
		return true
	}
	// No fewer runs than the whole divided by what one may say, and with
	// one structure a run, every run fits, so the search ends.
	tasks := max(asked, (total[len(sizes)]+qchemMostOutput-1)/qchemMostOutput)
	for ; !fits(tasks); tasks++ {
		if tasks >= MaxTasks {
			return 0, fmt.Errorf("its %d structures need more than the %d tasks a job may have for no task to say more than %d bytes of its run", len(sizes), MaxTasks, qchemMostOutput)
		}
	}
	return tasks, nil
}

// eachStructure reads the structures of an XYZ or SDF file, calling each
// with every one's title and its atoms, each as an element and three
// coordinates. It reads them as the tasks' script does, and refuses what
// the script might read otherwise.
func eachStructure(r io.Reader, format string, each func(title string, atoms []string) error) error {
	lines := bufio.NewScanner(r)
	lines.Buffer(nil, 1<<20)
	at := 0
	next := func() (string, bool) {
		if !lines.Scan() {
			return "", false
		}
		at++
		return lines.Text(), true
	}
	if format == "sdf" {
		return eachMolfile(next, &at, each)
	}
	for {
		line, ok := next()
		if !ok {
			break
		}
		fields := blankFields(line)
		if len(fields) == 0 {
			continue
		}
		atoms, err := strconv.Atoi(fields[0])
		if len(fields) != 1 || err != nil || atoms < 1 || atoms > qchemMostAtoms {
			return fmt.Errorf("line %d: %q is not a number of atoms from 1 to %d, which begins each structure of an XYZ file", at, line, qchemMostAtoms)
		}
		title, ok := next()
		structure := make([]string, 0, atoms)
		for ok && len(structure) < atoms {
			var line string
			if line, ok = next(); ok {
				fields := blankFields(line)
				if len(fields) < 4 || !element.MatchString(fields[0]) || !decimal.MatchString(fields[1]) || !decimal.MatchString(fields[2]) || !decimal.MatchString(fields[3]) {
					return fmt.Errorf("line %d: %q is not an atom, an element and three coordinates", at, line)
				}
				structure = append(structure, strings.Join(fields[:4], " "))
			}
		}
		if !ok {
			if err := lineError(lines.Err()); err != nil {
				return err
			}
			return fmt.Errorf("the file ends within a structure of %d atoms", atoms)
		}
		if err := each(title, structure); err != nil {
			return err
		}
	}
	return lineError(lines.Err())
}

// eachMolfile reads the molfiles of an SDF file, each ending at a line of
// $$$$, or at the end of the file. Of each it reads the title, the counts
// line, and the atoms' elements and coordinates, from the columns they are
// in, and skips the rest.
func eachMolfile(next func() (string, bool), number *int, each func(title string, atoms []string) error) error {
	var record []string
	finish := func() error {
		if len(record) < 4 {
			return fmt.Errorf("line %d: a molfile ends before its counts line", *number)
		}
		atoms, err := strconv.Atoi(strings.TrimSpace(column(record[3], 0, 3)))
		if err != nil || atoms < 1 || atoms > qchemMostAtoms {
			return fmt.Errorf("molfile %q: its counts line %q does not begin with a number of atoms from 1 to %d", record[0], record[3], qchemMostAtoms)
		}
		if len(record) < 4+atoms {
			return fmt.Errorf("molfile %q ends within its %d atoms", record[0], atoms)
		}
		structure := make([]string, 0, atoms)
		for _, line := range record[4 : 4+atoms] {
			x, y, z, symbol := column(line, 0, 10), column(line, 10, 10), column(line, 20, 10), column(line, 31, 3)
			if !element.MatchString(symbol) || !decimal.MatchString(x) || !decimal.MatchString(y) || !decimal.MatchString(z) {
				return fmt.Errorf("molfile %q: %q is not an atom, as a molfile has one", record[0], line)
			}
			structure = append(structure, symbol+" "+x+" "+y+" "+z)
		}
		return each(record[0], structure)
	}
	for {
		line, ok := next()
		if !ok {
			break
		}
		if fields := blankFields(line); len(fields) == 1 && fields[0] == "$$$$" {
			if len(record) > 0 {
				if err := finish(); err != nil {
					return err
				}
			}
			record = nil
			continue
		}
		record = append(record, line)
	}
	// What follows the last $$$$ is a molfile of its own, unless it is
	// only blank lines.
	if slices.ContainsFunc(record, func(line string) bool { return len(blankFields(line)) > 0 }) {
		return finish()
	}
	return nil
}

// lineError says what went wrong reading lines, if anything did.
func lineError(err error) error {
	if errors.Is(err, bufio.ErrTooLong) {
		return errors.New("it has a line longer than 1 MiB")
	}
	return err
}

// element is what an atom's element may be: its symbol, or its number.
var element = regexp.MustCompile(`^([A-Za-z]{1,3}|[0-9]{1,3})$`)

// column is width bytes of line from from, without blanks, as awk's substr
// gives them: what there is of them, where the line is shorter.
func column(line string, from, width int) string {
	if from >= len(line) {
		return ""
	}
	return strings.NewReplacer(" ", "", "\t", "").Replace(line[from:min(from+width, len(line))])
}

// blankFields splits a line at spaces and tabs, as awk splits its fields.
func blankFields(line string) []string {
	return strings.FieldsFunc(line, func(r rune) bool { return r == ' ' || r == '\t' })
}

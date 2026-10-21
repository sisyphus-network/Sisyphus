package runtime

import (
	"archive/zip"
	"bufio"
	"bytes"
	"cmp"
	"context"
	"crypto/sha256"
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
)

// Prep prepares molecular systems for Amber's force fields with tleap,
// AmberTools' program for building them: each of one or more PDB files of
// proteins or nucleic acids is given its missing hydrogens, put in a box
// of water, made neutral with sodium or chloride ions and, if asked, given
// salt, and saved as an Amber topology and coordinates (prmtop and inpcrd),
// the files the md workload simulates, with a PDB file of the whole system.
// The systems are shared out across the pool, each task preparing a run of
// them one after another.
//
// The force field, the water, the box and the salt are chosen from a fixed
// menu, and the tleap commands are made by this code from those choices: a
// job cannot hand tleap commands of its own, which can read and write files
// wherever the task may and run other programs. The PDB files are read by
// tleap's loadpdb as coordinates, never sourced as commands.
//
// Only what tleap's own libraries hold can be prepared: the standard amino
// acids and nucleotides, water and the common ions. A ligand needs
// parameters of its own, made with antechamber and GAFF, which prep does
// not make; a system with one fails, saying which residues tleap did not
// know.
//
// Like Transcode, it is the container workload with the image and the
// command fixed, so it runs only on workers that run containers. The image
// is built on a worker the first time it is needed, from AmberTools as
// conda-forge builds it.
type Prep struct {
	// Engine runs the container engine, as Container's does.
	Engine func(ctx context.Context, args []string, stdout, stderr io.Writer) (status int, err error)
	// Confined holds its containers in as Container's does.
	Confined Confinement
}

// PrepParams are a prep job's parameters.
type PrepParams struct {
	// Input is the CID of one stored PDB file, a system to prepare;
	// Inputs, in its place, those of several, each a system of its own.
	Input  string   `json:"input,omitempty"`
	Inputs []string `json:"inputs,omitempty"`
	// Forcefield is the force field of the proteins, one of
	// prepForcefields. Empty means ff19SB. Nucleic acids are given OL21's
	// for DNA and OL3's for RNA whichever it is.
	Forcefield string `json:"forcefield,omitempty"`
	// Water is the water model the system is put in, one of prepWaters, or
	// none for the molecule alone, in a vacuum, as implicit solvent wants
	// it. Empty means the one the force field is made for: opc for ff19SB,
	// tip3p for ff14SB.
	Water string `json:"water,omitempty"`
	// Box is how far the box's walls are from the molecule, at least, in
	// ångströms. Zero means 10. BoxShape is octahedron, a truncated
	// octahedron, which holds the same molecule in less water, or box, a
	// rectangular box. Empty means octahedron.
	Box      float64 `json:"box,omitempty"`
	BoxShape string  `json:"box_shape,omitempty"`
	// SaltMM is how much salt, sodium chloride, to add beyond the ions
	// that make the system neutral, in millimoles a litre. Zero adds none.
	SaltMM float64 `json:"salt_mm,omitempty"`
	// Tasks is how many runs of systems to share them out as. Zero leaves
	// it to the pool; there are never more runs than systems.
	Tasks int `json:"tasks,omitempty"`
}

// PrepBox is the size of a prepared system's periodic box: the lengths of
// its edges in ångströms and the angles between them in degrees.
type PrepBox struct {
	A     float64 `json:"a"`
	B     float64 `json:"b"`
	C     float64 `json:"c"`
	Alpha float64 `json:"alpha"`
	Beta  float64 `json:"beta"`
	Gamma float64 `json:"gamma"`
}

// PrepSystem is what one system came to.
type PrepSystem struct {
	// Index is which of the job's PDB files it is, from 0.
	Index int `json:"index"`
	// Prmtop, Inpcrd and PDB are the CIDs of its Amber topology, its
	// coordinates, and the whole system as a PDB file.
	Prmtop string `json:"prmtop,omitempty"`
	Inpcrd string `json:"inpcrd,omitempty"`
	PDB    string `json:"pdb,omitempty"`
	// Atoms and Residues are how many the prepared system has, among them
	// Waters water molecules and Ions ions of sodium and chloride, of
	// which SaltPairs pairs are the salt asked for. A count of none is
	// left out, as is each of these for a system that was not prepared.
	Atoms     int            `json:"atoms,omitempty"`
	Residues  int            `json:"residues,omitempty"`
	Waters    int            `json:"waters,omitempty"`
	Ions      map[string]int `json:"ions,omitempty"`
	SaltPairs int            `json:"salt_pairs,omitempty"`
	// Box is the periodic box it is in, where it is in water.
	Box *PrepBox `json:"box,omitempty"`
	// ChargeBefore is the molecule's charge as the PDB file has it, and
	// ChargeAfter the system's, in elementary charges.
	ChargeBefore *float64 `json:"charge_before,omitempty"`
	ChargeAfter  *float64 `json:"charge_after,omitempty"`
	// Error says why a system was not prepared, and Log is the CID of the
	// lines of tleap's log that say more.
	Error string `json:"error,omitempty"`
	Log   string `json:"log,omitempty"`
}

// PrepResult is a prep job's result.
type PrepResult struct {
	Forcefield string  `json:"forcefield"`
	Water      string  `json:"water"`
	Box        float64 `json:"box_angstrom"`
	BoxShape   string  `json:"box_shape"`
	SaltMM     float64 `json:"salt_mm"`
	// Systems are what each system came to, in the order of the files.
	// Prepared and Failed count them.
	Systems  []PrepSystem `json:"systems"`
	Prepared int          `json:"prepared"`
	Failed   int          `json:"failed"`
	// Archive is the CID of a zip file of every prepared system's files,
	// named system-000.prmtop, system-000.inpcrd and system-000.pdb by the
	// system's index, and the log of each that failed, system-000.log.
	Archive string `json:"archive"`
	// Tasks is how many tasks the job was shared out as.
	Tasks int `json:"tasks"`
}

// The menu a job chooses from: the force fields for proteins, each with
// tleap's file of commands that loads it and the water model it is made
// for; the water models, each with the file that loads it and the box of
// it tleap fills a system with; and the shapes of box.
var (
	prepForcefields = []string{"ff19SB", "ff14SB"}
	prepWaters      = []string{"opc", "tip3p", "none"}
	prepShapes      = []string{"octahedron", "box"}
	prepOwnWater    = map[string]string{"ff19SB": "opc", "ff14SB": "tip3p"}
	prepWaterBoxes  = map[string]string{"opc": "OPCBOX", "tip3p": "TIP3PBOX"}
	prepSolvate     = map[string]string{"octahedron": "solvateoct", "box": "solvatebox"}
)

// Bounds of a prep job: how many systems it may have; the nearest and
// farthest the box's walls may be from the molecule, in ångströms; the
// most salt, in millimoles a litre; the most bytes a PDB file may have,
// enough for a million atoms; and the longest a line of one may be.
const (
	prepMostSystems = 64
	prepLeastBox    = 3
	prepMostBox     = 50
	prepMostSalt    = 1000
	prepMostInput   = 128 << 20
	prepMostLine    = 1024
)

// prepSettings are what a job asks of each task.
type prepSettings struct {
	Forcefield, Water, Shape string
	Box, Salt                float64
}

// check refuses settings off the menu or out of bounds.
func (s prepSettings) check() error {
	switch {
	case !slices.Contains(prepForcefields, s.Forcefield):
		return fmt.Errorf("forcefield is %q, and must be one of %s", s.Forcefield, strings.Join(prepForcefields, ", "))
	case !slices.Contains(prepWaters, s.Water):
		return fmt.Errorf("water is %q, and must be one of %s", s.Water, strings.Join(prepWaters, ", "))
	case !slices.Contains(prepShapes, s.Shape):
		return fmt.Errorf("box_shape is %q, and must be one of %s", s.Shape, strings.Join(prepShapes, ", "))
	case !(s.Box >= prepLeastBox && s.Box <= prepMostBox):
		return fmt.Errorf("box is %g, and must be from %d to %d ångströms", s.Box, prepLeastBox, prepMostBox)
	case !(s.Salt >= 0 && s.Salt <= prepMostSalt):
		return fmt.Errorf("salt_mm is %g, and must be from 0 to %d", s.Salt, prepMostSalt)
	case s.Water == "none" && s.Salt > 0:
		return fmt.Errorf("salt_mm is %g, and a molecule in no water is given no salt", s.Salt)
	}
	return nil
}

// sources are tleap's commands that load the force fields: the proteins',
// the nucleic acids', and the water's with its ions, each a file of
// AmberTools' own.
func (s prepSettings) sources() string {
	lines := []string{"source leaprc.protein." + s.Forcefield, "source leaprc.DNA.OL21", "source leaprc.RNA.OL3"}
	if s.Water != "none" {
		lines = append(lines, "source leaprc.water."+s.Water)
	}
	return strings.Join(lines, "\n")
}

// solvate is tleap's command that puts the molecule, mol, in its box of
// water, or nothing where it is to have none.
func (s prepSettings) solvate() string {
	if s.Water == "none" {
		return ""
	}
	return prepSolvate[s.Shape] + " mol " + prepWaterBoxes[s.Water] + " " + prepNumber(s.Box)
}

// env is the settings as a task's environment.
func (s prepSettings) env() map[string]string {
	return map[string]string{
		"FORCEFIELD": s.Forcefield, "WATER": s.Water, "BOX": prepNumber(s.Box), "SHAPE": s.Shape, "SALT_MM": prepNumber(s.Salt),
		"SOURCES": s.sources(), "SOLVATE": s.solvate(),
	}
}

// prepNumber writes a number as the settings carry it: as few digits as
// say it, with no exponent.
func prepNumber(v float64) string { return strconv.FormatFloat(v, 'f', -1, 64) }

// prepSettingsOf reads and checks the settings a task's environment, or a
// task's summary, says it ran with. Each number is as prepNumber writes
// it, and nothing else.
func prepSettingsOf(forcefield, water, box, shape, salt string) (prepSettings, error) {
	s := prepSettings{Forcefield: forcefield, Water: water, Shape: shape}
	for _, number := range []struct {
		name, text string
		to         *float64
	}{{"box", box, &s.Box}, {"salt_mm", salt, &s.Salt}} {
		v, err := strconv.ParseFloat(number.text, 64)
		if err != nil || prepNumber(v) != number.text {
			return prepSettings{}, fmt.Errorf("%s is %q, which is not a number as a task is given one", number.name, number.text)
		}
		*number.to = v
	}
	return s, s.check()
}

// prepDockerfile is the whole of the image systems are prepared in: tleap
// from AmberTools 25, as conda-forge builds it, the same build as traj's,
// with the force fields, libraries and commands it loads and the libraries
// the program loads, and AmberTools' licence, on Debian. AmberTools is
// installed whole in a first stage, as traj's image installs it, so that a
// worker that has built one has that stage for the other, and only tleap's
// program, teLeap, and its files are kept. libfaketime holds tleap's clock
// still: tleap seeds the random choice of which waters to make ions of
// from it, and stamps each topology with it, so held still the same files
// come to the same bytes on every run.
const prepDockerfile = `FROM mambaorg/micromamba:2.0.5-debian12-slim AS build
USER root
RUN micromamba create -y -p /opt/amber -c conda-forge --override-channels ambertools=25.3=cuda_None_nompi_py312hd652fd9_100
RUN mkdir -p /opt/leap/bin /opt/leap/lib /opt/leap/dat/leap /opt/leap/share/licenses && cp /opt/amber/bin/teLeap /opt/leap/bin/ \
 && cp -r /opt/amber/dat/leap/cmd /opt/amber/dat/leap/lib /opt/amber/dat/leap/parm /opt/amber/dat/leap/prep /opt/leap/dat/leap/ \
 && cp /opt/conda/pkgs/ambertools-25.3-*/info/licenses/AmberTools/LICENSE /opt/leap/share/licenses/AmberTools \
 && for pass in 1 2 3 4 5 6 7 8; do ldd /opt/leap/bin/teLeap | awk '/not found/ { print "/opt/amber/lib/" $1 }' | xargs -r cp -L -t /opt/leap/lib/; done \
 && ! ldd /opt/leap/bin/teLeap | grep 'not found'
FROM debian:12.12-slim
RUN apt-get update && apt-get install -y --no-install-recommends libfaketime=0.9.10-2.1 && rm -rf /var/lib/apt/lists/*
COPY --from=build /opt/leap /opt/leap
ENV PATH=/opt/leap/bin:$PATH
RUN printf 'source leaprc.protein.ff19SB\nsource leaprc.protein.ff14SB\nsource leaprc.DNA.OL21\nsource leaprc.RNA.OL3\nsource leaprc.water.opc\nsource leaprc.water.tip3p\nquit\n' > /tmp/check.in \
 && cd /tmp && teLeap -s -I/opt/leap/dat/leap/prep -I/opt/leap/dat/leap/lib -I/opt/leap/dat/leap/parm -I/opt/leap/dat/leap/cmd -f /tmp/check.in > /tmp/check.log \
 && grep -q 'Errors = 0' /tmp/check.log && rm /tmp/check.in /tmp/check.log /tmp/leap.log
`

// PrepImage names that image. The name carries what the image is made
// from, as TranscodeImage's does.
var PrepImage = func() string {
	sum := sha256.Sum256([]byte(prepDockerfile))
	return "sisyphus-prep:" + hex.EncodeToString(sum[:6])
}()

// prepScript is what each task runs. The systems are /input/system-<i>.pdb,
// numbered from 000, of which the task takes the contiguous run that is
// its share, and it is given only those. For each, it looks again that
// the file is text with atoms in it, and runs tleap twice, with tleap's
// own files and no others and the clock held still: first to learn the
// molecule's charge and how much water fills its box, then to make the
// system, with the ions that make it neutral and the salt asked for, and
// save it to /output/system-<i>.prmtop, .inpcrd and .pdb. tleap's commands
// are made here from the settings the worker gave in SOURCES and SOLVATE;
// the PDB file is loaded with loadpdb, as coordinates. A system tleap
// cannot make is left with the lines of its log that say why, as
// /output/system-<i>.log, and the next is made. The task says what each
// came to in summary.json, as numbers it reads from tleap's log and from
// the topology and coordinates themselves: the atoms, residues, waters and
// ions, the box, and the charge before and after.
//
// The salt is as many pairs of ions as there are of salt in that much
// water at the concentration asked: 55.5 moles of water make a litre.
const prepScript = `set -euo pipefail
work=/output/.work
mkdir -p "$work"
cd "$work"
leap() {
	LD_PRELOAD=/usr/lib/x86_64-linux-gnu/faketime/libfaketime.so.1 FAKETIME="2000-01-01 00:00:00" \
		teLeap -s -I/opt/leap/dat/leap/prep -I/opt/leap/dat/leap/lib -I/opt/leap/dat/leap/parm -I/opt/leap/dat/leap/cmd -f "$1" > "$2" 2>&1 || true
}
number() { [[ $1 =~ ^-?[0-9]{1,12}(\.[0-9]{1,12})?$ ]]; }
first_charge() { awk '/^Total unperturbed charge:/ { print $4; exit }' "$1"; }
last_charge() { awk '/^Total unperturbed charge:/ { q = $4 } END { print q }' "$1"; }
excerpt() {
	{
		grep -E 'Unknown residue|Created a new atom|FATAL|Error|Could not|not saved|does not have' "$1" || true
		echo ...
		tail -n 10 "$1"
	} | awk 'NR <= 40 { print substr($0, 1, 200) }'
}
fail() {
	unknown=
	if [ "$1" = unknown_residue ]; then
		unknown=$(awk '/^Unknown residue:/ && $3 ~ /^[A-Za-z0-9+_*-]+$/ && length($3) <= 4 && !seen[$3]++ && n < 16 { printf "%s\"%s\"", (n++ ? "," : ""), $3 }' "$2")
	fi
	if [ -n "$2" ]; then
		excerpt "$2" > "$out.log"
	else
		echo "$3" > "$out.log"
	fi
	rm -f "$out.prmtop" "$out.inpcrd" "$out.pdb"
	entry=$(printf '{"index":%d,"failed":"%s","unknown":[%s]}' "$j" "$1" "$unknown")
	echo "system $j was not prepared: $1" >&2
}
prepare() {
	j=$1
	name=$(printf system-%03d "$j")
	pdb=/input/$name.pdb out=/output/$name
	if ! LC_ALL=C grep -qE '^(ATOM  |HETATM)' "$pdb" || LC_ALL=C grep -q '[^[:print:][:space:]]' "$pdb"; then
		fail not_pdb "" "the file has no ATOM or HETATM records, or has characters a PDB file does not"
		return
	fi
	{
		printf '%s\n' "$SOURCES" "mol = loadpdb $pdb" "charge mol"
		if [ -n "$SOLVATE" ]; then printf '%s\n' "$SOLVATE"; fi
		echo quit
	} > survey.in
	leap survey.in survey.log
	if grep -q '^Unknown residue:' survey.log; then
		fail unknown_residue survey.log
		return
	fi
	charge=$(first_charge survey.log)
	if ! number "$charge"; then
		fail not_loaded survey.log
		return
	fi
	ions= pairs=0
	if [ -n "$SOLVATE" ]; then
		waters=$(awk '/^ *Added [0-9]+ residues\.$/ { n = $2 } END { print n + 0 }' survey.log)
		ion=$(awk -v q="$charge" 'BEGIN { if (q > 0.5) print "Cl-"; else if (q < -0.5) print "Na+" }')
		if [ -n "$ion" ]; then ions="addionsrand mol $ion 0"; fi
		pairs=$(awk -v s="$SALT_MM" -v w="$waters" 'BEGIN { printf "%d", s * w / 55500 + 0.5 }')
		if [ "$pairs" -gt 0 ]; then ions="$ions${ions:+
}addionsrand mol Na+ $pairs Cl- $pairs"; fi
	fi
	{
		printf '%s\n' "$SOURCES" "mol = loadpdb $pdb" "charge mol"
		if [ -n "$SOLVATE" ]; then printf '%s\n' "$SOLVATE"; fi
		if [ -n "$ions" ]; then printf '%s\n' "$ions"; fi
		printf '%s\n' "charge mol" "saveamberparm mol $out.prmtop $out.inpcrd" "savepdb mol $out.pdb" quit
	} > build.in
	leap build.in build.log
	if grep -q '^Unknown residue:' build.log; then
		fail unknown_residue build.log
		return
	fi
	before=$(first_charge build.log)
	after=$(last_charge build.log)
	if [ ! -s "$out.prmtop" ] || [ ! -s "$out.inpcrd" ] || [ ! -s "$out.pdb" ] || grep -qE '^FATAL|not saved|Failed to generate' build.log || ! number "$before" || ! number "$after"; then
		fail not_saved build.log
		return
	fi
	read -r atoms residues waters sodium chloride < <(awk '
		/^%FLAG/ { flag = $2; next }
		/^%FORMAT/ { next }
		flag == "POINTERS" { for (c = 1; c <= length($0); c += 8) p[++n] = substr($0, c, 8) + 0 }
		flag == "RESIDUE_LABEL" { for (c = 1; c <= length($0); c += 4) { r = substr($0, c, 4); gsub(/ /, "", r); if (r == "WAT") w++; else if (r == "Na+") na++; else if (r == "Cl-") cl++ } }
		END { printf "%d %d %d %d %d\n", p[1], p[12], w, na, cl }' "$out.prmtop")
	box=
	if [ -n "$SOLVATE" ]; then
		box=$(tail -n 1 "$out.inpcrd" | awk '{ for (c = 1; c <= 61; c += 12) { v = substr($0, c, 12); gsub(/ /, "", v); printf "%s%s", (c > 1 ? "," : ""), v } }')
	fi
	entry=$(printf '{"index":%d,"atoms":%d,"residues":%d,"waters":%d,"ions":{"Na+":%d,"Cl-":%d},"salt_pairs":%d,"box":[%s],"charge_before":%s,"charge_after":%s}' \
		"$j" "$atoms" "$residues" "$waters" "$sodium" "$chloride" "$pairs" "$box" "$before" "$after")
	echo "system $j: $atoms atoms, $residues residues, $waters waters, $sodium Na+ and $chloride Cl-, charge $before before and $after after" >&2
}
i=$SISYPHUS_TASK_INDEX
k=$SISYPHUS_TASK_COUNT
first=$(( SYSTEMS * i / k ))
last=$(( SYSTEMS * (i + 1) / k - 1 ))
echo "preparing systems $first to $last of $SYSTEMS with $FORCEFIELD and water $WATER" >&2
prepared=
for j in $(seq "$first" "$last"); do
	prepare "$j"
	prepared=${prepared:+$prepared,}$entry
done
printf '{"systems":%d,"first":%d,"last":%d,"forcefield":"%s","water":"%s","box":"%s","shape":"%s","salt_mm":"%s","prepared":[%s]}\n' \
	"$SYSTEMS" "$first" "$last" "$FORCEFIELD" "$WATER" "$BOX" "$SHAPE" "$SALT_MM" "$prepared" > /output/summary.json
echo "prepared systems $first to $last" >&2
`

func (Prep) Name() string { return "prep" }

func (Prep) Describe() string {
	return `Prepares molecular systems for Amber force fields with AmberTools' tleap: each PDB file of proteins or nucleic acids is given its hydrogens, put in a box of water, made neutral with ions and given salt if asked, and saved as an Amber topology and coordinates (prmtop and inpcrd), the files md simulates, with a PDB file of the whole system. Several files are shared out across the pool, a run of them to each task. Only standard amino acids and nucleotides, water and common ions are known: a ligand needs parameters (antechamber, GAFF) that prep does not make, and fails, saying which residues were not known. No tleap commands are taken. Parameters: {"input": "<CID of a stored PDB file>", "inputs": ["<CIDs of several, in place of input, each a system>", ...], "forcefield": "<ff19SB (the default) or ff14SB>", "water": "<opc, tip3p or none; default opc for ff19SB, tip3p for ff14SB>", "box": <ångströms from the molecule to the box's walls, 3 to 50, default 10>, "box_shape": "<octahedron (the default) or box>", "salt_mm": <sodium chloride beyond neutralising, millimolar, 0 to 1000, default 0>, "tasks": <optional number of runs of systems>}. Result: {"forcefield", "water", "box_angstrom", "box_shape", "salt_mm", "systems": [{"index": <n>, "prmtop": "<CID of the Amber topology>", "inpcrd": "<CID of its coordinates>", "pdb": "<CID of the whole system as PDB>", "atoms": <n>, "residues": <n>, "waters": <n>, "ions": {"Na+": <n>, "Cl-": <n>}, "salt_pairs": <n>, "box": {"a", "b", "c", "alpha", "beta", "gamma"}, "charge_before": <e>, "charge_after": <e>, "error": "<why, if it was not prepared>", "log": "<CID of tleap's log lines that say more, if not>"}], "prepared": <n>, "failed": <n>, "archive": "<CID of a zip of every system's files>", "tasks": <n>}. Give a system's prmtop and inpcrd to md, and md's trajectory with that prmtop to traj.`
}

func (Prep) Form() []Field {
	return []Field{
		{Name: "input", Kind: KindFile, Label: "Molecule (PDB)", Help: "A stored PDB file of proteins or nucleic acids, of standard residues only: a ligand needs parameters that are not made here.", Required: true},
		{Name: "forcefield", Kind: KindChoice, Label: "Force field", Help: "ff19SB, Amber's newest for proteins, made for OPC water, or ff14SB, made for TIP3P.", Default: "ff19SB", Choices: slices.Clone(prepForcefields)},
		{Name: "water", Kind: KindChoice, Label: "Water", Help: "The water to put the molecule in: OPC for ff19SB, TIP3P for ff14SB, or none for the molecule alone, in a vacuum.", Default: "opc", Choices: slices.Clone(prepWaters)},
		{Name: "box", Kind: KindNumber, Label: "Padding", Help: "How far the box's walls are from the molecule, at least, in ångströms.", Default: 10, Minimum: bound(prepLeastBox), Maximum: bound(prepMostBox)},
		{Name: "box_shape", Kind: KindChoice, Label: "Box shape", Help: "A truncated octahedron, which needs less water than a rectangular box for the same padding, or a rectangular box.", Default: "octahedron", Choices: slices.Clone(prepShapes)},
		{Name: "salt_mm", Kind: KindNumber, Label: "Salt", Help: "Sodium chloride to add beyond the ions that make the system neutral, in millimoles a litre: 150 is about the body's.", Default: 0, Minimum: bound(0), Maximum: bound(prepMostSalt)},
		{Name: "tasks", Kind: KindInteger, Label: "Tasks", Help: "How many runs of systems to share them out as, or 0 to leave it to the pool.", Default: 0, Minimum: bound(0), Maximum: bound(MaxTasks)},
	}
}

// checkPDB refuses a stored file that is not a PDB file of one model:
// one too long, or with a line too long or of anything but printable
// ASCII, or with no atoms, or with more than one model. tleap reads it as
// data, and would make nothing of such a file; refusing it here says why.
func checkPDB(ctx context.Context, blobs Blobs, id cid.Cid) error {
	blob, err := blobs.Open(ctx, id)
	if err != nil {
		return err
	}
	defer blob.Close()
	if blob.Size() > prepMostInput {
		return fmt.Errorf("it has %d bytes, and a PDB file given to prep may have at most %d", blob.Size(), prepMostInput)
	}
	lines := bufio.NewScanner(blob)
	lines.Buffer(nil, prepMostLine)
	atoms, models, n := 0, 0, 0
	for lines.Scan() {
		n++
		line := lines.Bytes()
		if bytes.ContainsFunc(line, func(r rune) bool { return r != '\t' && r != '\r' && (r < ' ' || r > '~') }) {
			return fmt.Errorf("line %d has characters a PDB file does not", n)
		}
		switch {
		case bytes.HasPrefix(line, []byte("ATOM  ")), bytes.HasPrefix(line, []byte("HETATM")):
			atoms++
		case bytes.HasPrefix(line, []byte("MODEL ")):
			models++
		}
	}
	switch err := lines.Err(); {
	case errors.Is(err, bufio.ErrTooLong):
		return fmt.Errorf("line %d is longer than the %d characters a line of a PDB file may be", n+1, prepMostLine)
	case err != nil:
		return err
	case atoms == 0:
		return errors.New("it has no ATOM or HETATM records, so it is no PDB file of a molecule")
	case models > 1:
		return fmt.Errorf("it has %d models, and a system is one: give each model as a file of its own", models)
	}
	return nil
}

func (Prep) Split(ctx context.Context, blobs Blobs, params []byte, parts int) ([][]byte, error) {
	var p PrepParams
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, fmt.Errorf("prep parameters: %w", err)
	}
	inputs := p.Inputs
	switch {
	case p.Input != "" && len(inputs) > 0:
		return nil, errors.New("prep parameters: inputs are given in place of input, not with it")
	case p.Input != "":
		inputs = []string{p.Input}
	case len(inputs) == 0:
		return nil, errors.New("prep parameters: no PDB file is given")
	case len(inputs) > prepMostSystems:
		return nil, fmt.Errorf("prep parameters: %d PDB files are given; a job can have at most %d", len(inputs), prepMostSystems)
	}
	forcefield := cmp.Or(p.Forcefield, "ff19SB")
	settings := prepSettings{
		Forcefield: forcefield, Water: cmp.Or(p.Water, prepOwnWater[forcefield]),
		Box: cmp.Or(p.Box, 10), Shape: cmp.Or(p.BoxShape, "octahedron"), Salt: p.SaltMM,
	}
	if err := settings.check(); err != nil {
		return nil, fmt.Errorf("prep parameters: %w", err)
	}
	if p.Tasks < 0 || p.Tasks > MaxTasks {
		return nil, fmt.Errorf("prep parameters: tasks is %d, and must be from 1 to %d, or 0 to leave it to the pool", p.Tasks, MaxTasks)
	}
	// Each file is read through here, which is also what tells the
	// coordinator the job reads it, so that it keeps it while the job runs.
	for i, input := range inputs {
		id, err := cid.Decode(input)
		if err != nil {
			return nil, fmt.Errorf("prep parameters: input %d is not the CID of a stored PDB file: %w", i, err)
		}
		if err := checkPDB(ctx, blobs, id); err != nil {
			return nil, fmt.Errorf("prep input %d, %s: %w", i, id, err)
		}
	}
	// No more tasks than systems, and enough that none is given more than
	// a task may have.
	n := len(inputs)
	tasks := max(min(cmp.Or(p.Tasks, parts, 1), n), (n+maxInputs-1)/maxInputs)
	env := settings.env()
	env["SYSTEMS"] = strconv.Itoa(n)
	// The image, the count of tasks and the lack of inputs are made here,
	// and are what Container takes: this cannot fail.
	payloads, _ := Container{}.Split(ctx, blobs, mustJSON(ContainerParams{
		Image: PrepImage, Command: []string{"bash", "-c", prepScript}, Tasks: tasks, Env: env,
	}), parts)
	// Each task is given only its own run of systems, as the script reckons
	// its share, by their checked CIDs and names made here, which the
	// worker checks again.
	for i := range payloads {
		var task containerTask
		json.Unmarshal(payloads[i], &task) // made just now
		task.Inputs = map[string]string{}
		for j := n * i / tasks; j < n*(i+1)/tasks; j++ {
			task.Inputs[prepName(j)+".pdb"] = inputs[j]
		}
		payloads[i] = mustJSON(task)
	}
	return payloads, nil
}

// prepName is what system i's files are called, before their extension.
func prepName(i int) string { return fmt.Sprintf("system-%03d", i) }

func (p Prep) Execute(ctx context.Context, blobs Blobs, payload []byte) ([]byte, error) {
	var task containerTask
	if err := json.Unmarshal(payload, &task); err != nil {
		return nil, fmt.Errorf("prep payload: %w", err)
	}
	// The worker runs only the image of its own making, and its own
	// script, and makes tleap's commands itself, from settings it checks
	// for itself: whatever commands the payload holds are not run. It
	// takes no input but the PDB files of systems of the job, by the
	// names the script reads them by.
	if task.Image != PrepImage {
		return nil, fmt.Errorf("prep payload: %q is not the image systems are prepared in", task.Image)
	}
	settings, err := prepSettingsOf(task.Env["FORCEFIELD"], task.Env["WATER"], task.Env["BOX"], task.Env["SHAPE"], task.Env["SALT_MM"])
	if err != nil {
		return nil, fmt.Errorf("prep payload: %w", err)
	}
	systems, err := strconv.Atoi(task.Env["SYSTEMS"])
	if err != nil || systems < 1 || systems > prepMostSystems {
		return nil, fmt.Errorf("prep payload: %q systems", task.Env["SYSTEMS"])
	}
	for name := range task.Inputs {
		index, err := strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(name, "system-"), ".pdb"))
		if err != nil || index < 0 || index >= systems || name != prepName(index)+".pdb" {
			return nil, fmt.Errorf("prep payload: %q is not the PDB file of one of the job's %d systems", name, systems)
		}
	}
	task.Command, task.Input, task.Network, task.GPUs = []string{"bash", "-c", prepScript}, "", false, 0
	task.Env = settings.env()
	task.Env["SYSTEMS"] = strconv.Itoa(systems)
	engine := p.Engine
	if engine == nil {
		engine = docker
	}
	if err := haveImage(ctx, engine, PrepImage, prepDockerfile, "preparing systems"); err != nil {
		return nil, err
	}
	return Container{Engine: engine, Confined: p.Confined}.Execute(ctx, blobs, mustJSON(task))
}

// Varies says that two honest workers may give different files for a
// task. On one machine tleap gives the same bytes every time, and however
// the job is shared out: each system is made by a tleap of its own, whose
// clock is held still, so that it places the ions the same way and stamps
// the topology with the same date. But each worker builds the image from
// conda-forge as it is that day, so two workers may run different builds
// of the libraries tleap loads, and the C library's mathematics picks its
// code by the processor it finds. Whether that moves the seventh decimal
// place of a coordinate has not been measured across machines; comparing
// two runs' bytes, as verification does, would take honest workers for
// wrong ones if it did.
func (Prep) Varies() bool { return true }

// prepSummary is what a task says it prepared: systems First to Last of
// Systems, with the settings it ran with, and what each came to.
type prepSummary struct {
	Systems    int          `json:"systems"`
	First      int          `json:"first"`
	Last       int          `json:"last"`
	Forcefield string       `json:"forcefield"`
	Water      string       `json:"water"`
	Box        string       `json:"box"`
	Shape      string       `json:"shape"`
	SaltMM     string       `json:"salt_mm"`
	Prepared   []prepReport `json:"prepared"`
}

// prepReport is what a task says one system came to: why it failed and,
// for an unknown residue, which; or what it is made of.
type prepReport struct {
	Index        int            `json:"index"`
	Failed       string         `json:"failed"`
	Unknown      []string       `json:"unknown"`
	Atoms        int            `json:"atoms"`
	Residues     int            `json:"residues"`
	Waters       int            `json:"waters"`
	Ions         map[string]int `json:"ions"`
	SaltPairs    int            `json:"salt_pairs"`
	Box          []float64      `json:"box"`
	ChargeBefore *float64       `json:"charge_before"`
	ChargeAfter  *float64       `json:"charge_after"`
}

// Bounds of what a prep job's tasks may say: the most atoms a system may
// have; the longest a box's edge may be, in ångströms; the greatest charge
// a molecule may have; the most a neutral system's charge may be off
// nought, which is tleap's rounding; the most bytes each of a system's
// files may have, and its log; and the most unknown residues named.
const (
	prepMostAtoms   = 10_000_000
	prepMostEdge    = 10_000
	prepMostCharge  = 100_000
	prepNeutral     = 0.01
	prepMostFile    = 2 << 30
	prepMostLog     = 64 << 10
	prepMostUnknown = 16
)

// prepFailures are why a task may say a system was not prepared, each with
// what the job's result says of it.
var prepFailures = map[string]string{
	"not_pdb":         "the file has no ATOM or HETATM records, or has characters a PDB file does not",
	"not_loaded":      "tleap could not read the file as a PDB file; its log says why",
	"unknown_residue": "tleap does not know the residues %s: prep makes systems of standard amino acids and nucleotides, water and common ions only, and a ligand, a modified residue or another molecule needs parameters of its own, made with antechamber and GAFF, which prep does not make",
	"not_saved":       "tleap could not make a topology of it, most often for atoms its templates of the residues do not have, or residues it cannot join; its log says why",
}

// prepResidue is a residue's name as tleap writes it.
var prepResidue = regexp.MustCompile(`^[A-Za-z0-9+_*-]{1,4}$`)

// Aggregate takes from each task only its summary and the files it is meant
// to leave, by the fixed names of the systems it says it prepared, and
// checks that the tasks prepared every system once, in order, with the same
// settings, and that what each says of a system is plain numbers within
// bounds, before anything they said is believed.
func (Prep) Aggregate(ctx context.Context, blobs Blobs, outputs [][]byte) ([]byte, error) {
	result := PrepResult{Tasks: len(outputs), Systems: []PrepSystem{}}
	var first prepSummary
	var settings prepSettings
	read := 0
	for i, raw := range outputs {
		var out ContainerOutput
		if err := json.Unmarshal(raw, &out); err != nil {
			return nil, fmt.Errorf("task %d output: %w", i, err)
		}
		var summary prepSummary
		if err := decodePrepSummary(ctx, blobs, out.Files["summary.json"], &summary); err != nil {
			return nil, fmt.Errorf("task %d's summary: %w", i, err)
		}
		if i == 0 {
			first = summary
			var err error
			settings, err = prepSettingsOf(summary.Forcefield, summary.Water, summary.Box, summary.Shape, summary.SaltMM)
			if err != nil || summary.Systems < 1 || summary.Systems > prepMostSystems {
				return nil, fmt.Errorf("task %d says it prepared %d systems, with settings no job asks for: %v", i, summary.Systems, err)
			}
			result.Forcefield, result.Water, result.Box, result.BoxShape, result.SaltMM = settings.Forcefield, settings.Water, settings.Box, settings.Shape, settings.Salt
		}
		if summary.Systems != first.Systems || summary.Forcefield != first.Forcefield || summary.Water != first.Water ||
			summary.Box != first.Box || summary.Shape != first.Shape || summary.SaltMM != first.SaltMM {
			return nil, fmt.Errorf("task %d prepared %d systems with %s, water %s, box %s, %s and salt %s, and task 0 %d with %s, water %s, box %s, %s and salt %s", i,
				summary.Systems, summary.Forcefield, summary.Water, summary.Box, summary.Shape, summary.SaltMM,
				first.Systems, first.Forcefield, first.Water, first.Box, first.Shape, first.SaltMM)
		}
		if summary.First != read || summary.Last < summary.First-1 || summary.Last >= summary.Systems || len(summary.Prepared) != summary.Last-summary.First+1 {
			return nil, fmt.Errorf("task %d says it prepared systems %d to %d of %d, after systems up to %d, and reports %d", i, summary.First, summary.Last, summary.Systems, read-1, len(summary.Prepared))
		}
		for _, report := range summary.Prepared {
			if report.Index != read {
				return nil, fmt.Errorf("task %d reports system %d where system %d was to be", i, report.Index, read)
			}
			system, err := checkPrepReport(ctx, blobs, settings, report, out.Files)
			if err != nil {
				return nil, fmt.Errorf("task %d's system %d: %w", i, read, err)
			}
			if system.Error == "" {
				result.Prepared++
			} else {
				result.Failed++
			}
			result.Systems = append(result.Systems, system)
			read++
		}
	}
	if read != first.Systems {
		return nil, fmt.Errorf("the tasks prepared %d of the %d systems", read, first.Systems)
	}
	if result.Prepared == 0 {
		return nil, fmt.Errorf("no system was prepared: system 0: %s", result.Systems[0].Error)
	}
	// Each file is named by its system's index and its kind: names the
	// coordinator makes from numbers it has checked, and none a worker
	// gave.
	var names, ids []string
	for _, system := range result.Systems {
		name := prepName(system.Index)
		if system.Error != "" {
			names, ids = append(names, name+".log"), append(ids, system.Log)
			continue
		}
		names = append(names, name+".prmtop", name+".inpcrd", name+".pdb")
		ids = append(ids, system.Prmtop, system.Inpcrd, system.PDB)
	}
	reading, writing := io.Pipe()
	go func() { writing.CloseWithError(zipText(ctx, blobs, writing, names, ids)) }()
	archive, err := blobs.Put(ctx, reading)
	reading.CloseWithError(errors.New("the archive is no longer being stored"))
	if err != nil {
		return nil, fmt.Errorf("store the systems as one zip file: %w", err)
	}
	result.Archive = archive.String()
	return mustJSON(result), nil
}

// checkPrepReport checks what a task says of one system, and the files it
// left for it, and makes what the result says of it.
func checkPrepReport(ctx context.Context, blobs Blobs, settings prepSettings, report prepReport, files map[string]string) (PrepSystem, error) {
	name := prepName(report.Index)
	system := PrepSystem{Index: report.Index}
	if report.Failed != "" {
		why, known := prepFailures[report.Failed]
		if !known || len(report.Unknown) > prepMostUnknown || (report.Failed == "unknown_residue") != (len(report.Unknown) > 0) {
			return PrepSystem{}, fmt.Errorf("it says it failed for %q, of residues %d", report.Failed, len(report.Unknown))
		}
		for _, residue := range report.Unknown {
			if !prepResidue.MatchString(residue) {
				return PrepSystem{}, fmt.Errorf("%q is not a residue's name", residue)
			}
		}
		if report.Failed == "unknown_residue" {
			why = fmt.Sprintf(why, strings.Join(report.Unknown, ", "))
			if settings.Water == "none" {
				why += "; with no water, the file's waters and ions are not known either"
			}
		}
		var err error
		if system.Log, _, err = prepFile(ctx, blobs, files, name+".log", prepMostLog); err != nil {
			return PrepSystem{}, err
		}
		system.Error = why
		return system, nil
	}

	// The counts are whole numbers, as JSON decodes them into ints, and
	// the box and the charges plain ones: JSON has nothing else.
	solvated := settings.Water != "none"
	sodium, hasSodium := report.Ions["Na+"]
	chloride, hasChloride := report.Ions["Cl-"]
	switch {
	case report.Atoms < 1 || report.Atoms > prepMostAtoms || report.Residues < 1 || report.Residues > report.Atoms:
		return PrepSystem{}, fmt.Errorf("it says it has %d atoms in %d residues", report.Atoms, report.Residues)
	case len(report.Ions) != 2 || !hasSodium || !hasChloride || sodium < 0 || chloride < 0 || report.Waters < 0 || report.Waters+sodium+chloride > report.Residues || report.SaltPairs < 0 || report.SaltPairs > min(sodium, chloride):
		return PrepSystem{}, fmt.Errorf("it says it has %d waters and ions %v, with %d pairs of salt, among %d residues", report.Waters, report.Ions, report.SaltPairs, report.Residues)
	case report.ChargeBefore == nil || report.ChargeAfter == nil || math.Abs(*report.ChargeBefore) > prepMostCharge:
		return PrepSystem{}, errors.New("it does not say its charge, or says one past any molecule's")
	case solvated && math.Abs(*report.ChargeAfter) > prepNeutral:
		return PrepSystem{}, fmt.Errorf("it says it has a charge of %g after it was made neutral", *report.ChargeAfter)
	case !solvated && (*report.ChargeAfter != *report.ChargeBefore || len(report.Box) != 0 || report.SaltPairs != 0):
		return PrepSystem{}, errors.New("it says it was changed, in no water, by more than its hydrogens")
	case solvated && !prepBoxOf(settings.Shape, report.Box):
		return PrepSystem{}, fmt.Errorf("it says its box is %v, which is no %s", report.Box, settings.Shape)
	}
	system.Atoms, system.Residues, system.Waters, system.SaltPairs = report.Atoms, report.Residues, report.Waters, report.SaltPairs
	system.Ions = map[string]int{"Na+": sodium, "Cl-": chloride}
	// tleap writes a charge of nought as -0.000000 as often as not, which
	// adding nought makes plain nought.
	before, after := *report.ChargeBefore+0, *report.ChargeAfter+0
	system.ChargeBefore, system.ChargeAfter = &before, &after
	if solvated {
		b := report.Box
		system.Box = &PrepBox{A: b[0], B: b[1], C: b[2], Alpha: b[3], Beta: b[4], Gamma: b[5]}
	}

	// Each file is there, of a size within bounds, and begins as its kind
	// does: the coordinates with as many atoms as the summary says.
	var head []byte
	var err error
	if system.Prmtop, head, err = prepFile(ctx, blobs, files, name+".prmtop", prepMostFile); err != nil {
		return PrepSystem{}, err
	}
	if topologyKind(head) != "parm7" {
		return PrepSystem{}, fmt.Errorf("%s.prmtop is not an Amber topology", name)
	}
	if system.PDB, head, err = prepFile(ctx, blobs, files, name+".pdb", prepMostFile); err != nil {
		return PrepSystem{}, err
	}
	if topologyKind(head) != "pdb" {
		return PrepSystem{}, fmt.Errorf("%s.pdb is not a PDB file", name)
	}
	if system.Inpcrd, head, err = prepFile(ctx, blobs, files, name+".inpcrd", prepMostFile); err != nil {
		return PrepSystem{}, err
	}
	if lines := strings.SplitN(string(head), "\n", 3); len(lines) < 3 || len(strings.Fields(lines[1])) == 0 || strings.Fields(lines[1])[0] != strconv.Itoa(report.Atoms) {
		return PrepSystem{}, fmt.Errorf("%s.inpcrd is not the coordinates of %d atoms", name, report.Atoms)
	}
	return system, nil
}

// prepBoxOf reports whether a box is of the shape asked for: edges of
// ångströms, at most prepMostEdge, and the angles a truncated octahedron's
// or a rectangular box's, as tleap writes them.
func prepBoxOf(shape string, box []float64) bool {
	angle := 90.0
	if shape == "octahedron" {
		angle = 109.4712190
	}
	if len(box) != 6 {
		return false
	}
	for i, v := range box {
		if i < 3 && !(v > 0 && v <= prepMostEdge) || i >= 3 && math.Abs(v-angle) > 1e-3 {
			return false
		}
	}
	return true
}

// prepFile finds the file of the given name a task left, and returns its
// CID and first bytes, refusing one that is not there, is empty, or has
// more than most bytes.
func prepFile(ctx context.Context, blobs Blobs, files map[string]string, name string, most uint64) (string, []byte, error) {
	id, err := cid.Decode(files[name])
	if err != nil {
		return "", nil, fmt.Errorf("the task left no %s", name)
	}
	blob, err := blobs.Open(ctx, id)
	if err != nil {
		return "", nil, fmt.Errorf("%s: %w", name, err)
	}
	defer blob.Close()
	if blob.Size() == 0 || blob.Size() > most {
		return "", nil, fmt.Errorf("%s has %d bytes, and must have from 1 to %d", name, blob.Size(), most)
	}
	head := make([]byte, 512)
	n, _ := io.ReadFull(blob, head) // a short file has a short head
	return id.String(), head[:n], nil
}

// decodePrepSummary reads a task's summary.
func decodePrepSummary(ctx context.Context, blobs Blobs, id string, summary *prepSummary) error {
	c, err := cid.Decode(id)
	if err != nil {
		return errors.New("the task left none")
	}
	blob, err := blobs.Open(ctx, c)
	if err != nil {
		return err
	}
	defer blob.Close()
	return json.NewDecoder(io.LimitReader(blob, 1<<20)).Decode(summary)
}

// zipText writes a zip file of the files with the given names and CIDs,
// compressed, as text files are worth: a prep job's topologies,
// coordinates and logs.
func zipText(ctx context.Context, blobs Blobs, w io.Writer, names, cids []string) error {
	archive := zip.NewWriter(w)
	for i, name := range names {
		c, _ := cid.Decode(cids[i]) // checked as the tasks' outputs were
		blob, err := blobs.Open(ctx, c)
		if err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		entry, err := archive.CreateHeader(&zip.FileHeader{Name: name, Method: zip.Deflate})
		if err == nil {
			_, err = io.Copy(entry, blob)
		}
		blob.Close()
		if err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
	}
	return archive.Close()
}

// ResultBlobs are the systems' files the result refers to, which the tasks
// stored. The archive is stored by Aggregate, and kept with the result as
// everything it stores is.
func (Prep) ResultBlobs(raw []byte) []string {
	var result PrepResult
	json.Unmarshal(raw, &result)
	var ids []string
	for _, system := range result.Systems {
		for _, id := range []string{system.Prmtop, system.Inpcrd, system.PDB, system.Log} {
			if id != "" {
				ids = append(ids, id)
			}
		}
	}
	return ids
}

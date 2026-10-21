package runtime

import (
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
	"sort"
	"strconv"
	"strings"

	"github.com/ipfs/go-cid"
)

// Traj analyses molecular dynamics trajectories with cpptraj, AmberTools'
// trajectory program: how far a molecule strays from where it began, how
// compact it is, how far apart two of its parts are, which hydrogen bonds
// it makes and how often, its secondary structure, and how much each of
// its residues moves. One long trajectory is shared out across the pool by
// its frames, each task analysing a contiguous run of them; several are
// shared out whole, each task analysing a run of them. The frames' results
// are put back together in order.
//
// Which analyses are run is chosen from a fixed menu, and the commands
// cpptraj is given are made by this code from the choices and from atom
// masks it has checked: a job cannot hand cpptraj commands of its own,
// which can read and write files wherever the task may.
//
// Like Transcode, it is the container workload with the image and the
// command fixed, so it runs only on workers that run containers. The image
// is built on a worker the first time it is needed, from AmberTools as
// conda-forge builds it.
type Traj struct {
	// Engine runs the container engine, as Container's does.
	Engine func(ctx context.Context, args []string, stdout, stderr io.Writer) (status int, err error)
	// Confined holds its containers in as Container's does.
	Confined Confinement
}

// TrajParams are a traj job's parameters.
type TrajParams struct {
	// Topology is the CID of the stored topology the trajectories are of:
	// an Amber topology (prmtop or parm7), or a PDB file of the system,
	// its atoms in the trajectories' order.
	Topology string `json:"topology"`
	// Trajectory is the CID of one stored trajectory, as DCD, NetCDF or
	// Amber's text mdcrd; Trajectories, in its place, those of several, in
	// the order their frames are to be put together.
	Trajectory   string   `json:"trajectory,omitempty"`
	Trajectories []string `json:"trajectories,omitempty"`
	// Analyses are what to find, as some of trajAnalyses joined with
	// commas, each once. Empty means rmsd,radgyr.
	Analyses string `json:"analyses,omitempty"`
	// Mask is the Amber mask of the atoms RMSD, RMSF and the radius of
	// gyration are taken over. Empty means @CA, the alpha carbons.
	Mask string `json:"mask,omitempty"`
	// Mask1 and Mask2 are the masks of the two groups of atoms whose
	// distance apart, centre of mass to centre of mass, is measured, and
	// are given only when it is asked for.
	Mask1 string `json:"mask1,omitempty"`
	Mask2 string `json:"mask2,omitempty"`
	// Region is the mask of the atoms in which hydrogen bonds and
	// secondary structure are looked for. Empty means *, every atom of
	// the solute.
	Region string `json:"region,omitempty"`
	// Tasks is how many tasks to share the frames, or the trajectories,
	// out as. Zero leaves it to the pool.
	Tasks int `json:"tasks,omitempty"`
}

// TrajStats sum up one number found for each frame: its mean, standard
// deviation, least and greatest.
type TrajStats struct {
	Mean  float64 `json:"mean"`
	Stdev float64 `json:"stdev"`
	Min   float64 `json:"min"`
	Max   float64 `json:"max"`
}

// TrajResult is a traj job's result.
type TrajResult struct {
	// Frames is how many frames were analysed, Trajectories how many
	// trajectories they came from, and TrajectoryFrames how many frames
	// each had, in order.
	Frames           int   `json:"frames"`
	Trajectories     int   `json:"trajectories"`
	TrajectoryFrames []int `json:"trajectory_frames"`
	// Analyses are what was found, in trajAnalyses' order.
	Analyses []string `json:"analyses"`
	// Series is the CID of a CSV table with a row for each frame: its
	// number from 1 among all the frames, the trajectory it is from, and
	// what was found for it, of RMSD, radius of gyration, distance and
	// the number of hydrogen bonds, as were asked for.
	Series string `json:"series,omitempty"`
	// RMSD, RadGyr and Distance, in ångströms, and HBondsPerFrame sum up
	// those columns.
	RMSD           *TrajStats `json:"rmsd_angstrom,omitempty"`
	RadGyr         *TrajStats `json:"radgyr_angstrom,omitempty"`
	Distance       *TrajStats `json:"distance_angstrom,omitempty"`
	HBondsPerFrame *TrajStats `json:"hbonds_per_frame,omitempty"`
	// HBonds is the CID of a CSV table of each hydrogen bond found, its
	// acceptor, hydrogen and donor, with how many frames it was made in
	// and in what fraction of them, most often made first.
	HBonds string `json:"hbonds,omitempty"`
	// RMSF is the CID of a CSV table of each residue's fluctuation about
	// its mean position, in ångströms, over every frame.
	RMSF string `json:"rmsf,omitempty"`
	// DSSP is the CID of a CSV table of each residue's secondary structure
	// in each frame, a letter of DSSP's; DSSPResidues that of the fraction
	// of the frames each residue spent in each kind; and Structure the
	// fraction of all residues' frames spent in each kind.
	DSSP         string             `json:"dssp,omitempty"`
	DSSPResidues string             `json:"dssp_residues,omitempty"`
	Structure    map[string]float64 `json:"structure,omitempty"`
	// Tasks is how many tasks the job was shared out as.
	Tasks int `json:"tasks"`
}

// trajAnalyses are the analyses a job may ask for, in the order they are
// run and listed:
//
//   - rmsd: the root-mean-square deviation of Mask's atoms from where they
//     are in the first frame of the first trajectory, fitted onto them;
//   - radgyr: the radius of gyration of Mask's atoms;
//   - distance: the distance between the centres of mass of Mask1's atoms
//     and Mask2's;
//   - hbond: the hydrogen bonds among Region's atoms, how many in each
//     frame and how often each is made;
//   - dssp: the secondary structure of Region's residues, by DSSP;
//   - rmsf: each residue's root-mean-square fluctuation, of Mask's atoms,
//     after fitting every frame onto the first.
//
// Clustering is not among them: it compares every frame with every other,
// or with centres found from all of them, so it does not share out by
// frames, and run in one task it would be the one task a pool gains
// nothing from.
var trajAnalyses = []string{"rmsd", "radgyr", "distance", "hbond", "dssp", "rmsf"}

// trajSeries are the analyses that find one number for each frame, with
// the name of their column in the series table and the file cpptraj
// writes them to.
var trajSeries = []struct{ analysis, column, file string }{
	{"rmsd", "rmsd", "rmsd.dat"},
	{"radgyr", "radgyr", "radgyr.dat"},
	{"distance", "distance", "distance.dat"},
	{"hbond", "hbonds", "hbonds.dat"},
}

// dsspKinds are the kinds of secondary structure cpptraj tells apart, as it
// numbers them from 0, with the letter DSSP gives each.
var dsspKinds = []struct{ name, letter string }{
	{"none", "-"}, {"extended", "E"}, {"bridge", "B"}, {"3-10", "G"},
	{"alpha", "H"}, {"pi", "I"}, {"turn", "T"}, {"bend", "S"},
}

// Bounds of a traj job: how many trajectories it may have; how many of
// them one task may be given, which, with the topology and the first
// trajectory where a task needs its first frame, is all the named inputs
// a task may have; the longest a mask may be; and the most text any one
// table may have, which the coordinator holds in memory as it puts the
// frames together.
const (
	trajMostTrajectories = 256
	trajMostEach         = maxInputs - 2
	trajMostMask         = 256
	trajMostText         = 64 << 20
)

// trajMask is what an Amber mask may be made of here. It begins as a mask
// does, with a residue (:), an atom (@), a molecule (^), a negation (!),
// a group or everything, so that it is never taken for one of cpptraj's
// keywords, and has in it only what masks are written with: names and
// numbers, ranges and lists, wildcards, element (/) and type (%)
// selections, and, or, and distance (<, >) operators. It has no space, so
// it is one argument, no line break, so it is no more than that, and none
// of the quotes, #, $ and \ that cpptraj reads otherwise.
var trajMask = regexp.MustCompile(`^[:@^!(*][A-Za-z0-9:@^!()*.,\-+_=?<>&|%/]*$`)

// checkTrajMask refuses a mask that is empty, too long, or has in it
// anything trajMask does not allow.
func checkTrajMask(name, mask string) error {
	if len(mask) > trajMostMask || !trajMask.MatchString(mask) {
		return fmt.Errorf("%s is %q, and must be an Amber mask of at most %d characters, beginning with :, @, ^, !, ( or *, with no spaces, quotes or line breaks", name, mask, trajMostMask)
	}
	return nil
}

// trajSettings are what a job asks of each task: the analyses, in
// trajAnalyses' order, and the masks they are run with.
type trajSettings struct {
	Analyses                   []string
	Mask, Mask1, Mask2, Region string
}

// parseTrajAnalyses reads analyses joined with commas, each one of
// trajAnalyses and each once, into trajAnalyses' order.
func parseTrajAnalyses(analyses string) ([]string, error) {
	parts := strings.Split(analyses, ",")
	for i, part := range parts {
		if !slices.Contains(trajAnalyses, part) || slices.Contains(parts[:i], part) {
			return nil, fmt.Errorf("analyses is %q, and must be some of %s, joined with commas, each once", analyses, strings.Join(trajAnalyses, ", "))
		}
	}
	var ordered []string
	for _, analysis := range trajAnalyses {
		if slices.Contains(parts, analysis) {
			ordered = append(ordered, analysis)
		}
	}
	return ordered, nil
}

// check refuses settings with a mask that is not one, or with the masks of
// a distance where none is asked for or none where one is.
func (s trajSettings) check() error {
	for _, mask := range []struct{ name, mask string }{{"mask", s.Mask}, {"region", s.Region}} {
		if err := checkTrajMask(mask.name, mask.mask); err != nil {
			return err
		}
	}
	if !s.has("distance") {
		if s.Mask1 != "" || s.Mask2 != "" {
			return errors.New("mask1 and mask2 are the two groups a distance is measured between, and analyses does not ask for one")
		}
		return nil
	}
	for _, mask := range []struct{ name, mask string }{{"mask1", s.Mask1}, {"mask2", s.Mask2}} {
		if err := checkTrajMask(mask.name, mask.mask); err != nil {
			return fmt.Errorf("%w: a distance is measured between the atoms of mask1 and those of mask2", err)
		}
	}
	return nil
}

func (s trajSettings) has(analysis string) bool { return slices.Contains(s.Analyses, analysis) }

// needsReference says whether the analyses fit frames onto the first frame
// of the first trajectory, which every task is then given.
func (s trajSettings) needsReference() bool { return s.has("rmsd") || s.has("rmsf") }

// actions are the commands cpptraj is given for the analyses, after the
// topology, the first frame and the frames are loaded: one line each, of
// cpptraj's own keywords and the checked masks, with every file it writes
// at a fixed name in /output. RMSD is found without moving the frames, so
// that what follows sees them as they were; the fit RMSF needs is made
// last. Numbers are written to six decimal places.
func (s trajSettings) actions() string {
	var lines, precise []string
	if s.has("rmsd") {
		lines = append(lines, "rms RMSD ref [first] "+s.Mask+" nomod out /output/rmsd.dat")
		precise = append(precise, "rmsd.dat")
	}
	if s.has("radgyr") {
		lines = append(lines, "radgyr RADGYR "+s.Mask+" nomax out /output/radgyr.dat")
		precise = append(precise, "radgyr.dat")
	}
	if s.has("distance") {
		lines = append(lines, "distance DISTANCE "+s.Mask1+" "+s.Mask2+" out /output/distance.dat")
		precise = append(precise, "distance.dat")
	}
	if s.has("hbond") {
		lines = append(lines, "hbond HBOND "+s.Region+" out /output/hbonds.dat avgout /output/hbonds-avg.dat")
	}
	if s.has("dssp") {
		lines = append(lines, "secstruct DSSP "+s.Region+" out /output/dssp.dat sumout /output/.work/dssp-sum.dat")
	}
	if s.has("rmsf") {
		lines = append(lines, "rms FIT ref [first] "+s.Mask, "atomicfluct RMSF "+s.Mask+" byres out /output/rmsf.dat")
		precise = append(precise, "rmsf.dat")
	}
	for _, file := range precise {
		lines = append(lines, "precision /output/"+file+" 16 6")
	}
	return strings.Join(lines, "\n")
}

// outputs are the files the analyses leave in /output, by name.
func (s trajSettings) outputs() []string {
	var files []string
	for _, series := range trajSeries {
		if s.has(series.analysis) {
			files = append(files, series.file)
		}
	}
	if s.has("hbond") {
		files = append(files, "hbonds-avg.dat")
	}
	if s.has("dssp") {
		files = append(files, "dssp.dat")
	}
	if s.has("rmsf") {
		files = append(files, "rmsf.dat")
	}
	return files
}

// env is the settings as a task's environment.
func (s trajSettings) env() map[string]string {
	reference := "0"
	if s.needsReference() {
		reference = "1"
	}
	return map[string]string{
		"ANALYSES": strings.Join(s.Analyses, ","), "MASK": s.Mask, "MASK1": s.Mask1, "MASK2": s.Mask2, "REGION": s.Region,
		"REFERENCE": reference, "ACTIONS": s.actions(), "OUTPUTS": strings.Join(s.outputs(), " "),
	}
}

// trajSettingsOf reads and checks the settings a task's environment, or a
// task's summary, says it ran with.
func trajSettingsOf(analyses, mask, mask1, mask2, region string) (trajSettings, error) {
	parsed, err := parseTrajAnalyses(analyses)
	if err != nil {
		return trajSettings{}, err
	}
	if strings.Join(parsed, ",") != analyses {
		return trajSettings{}, fmt.Errorf("analyses is %q, and a task is given them in the order %s", analyses, strings.Join(trajAnalyses, ", "))
	}
	s := trajSettings{Analyses: parsed, Mask: mask, Mask1: mask1, Mask2: mask2, Region: region}
	return s, s.check()
}

// trajDockerfile is the whole of the image trajectories are analysed in:
// cpptraj from AmberTools 25, as conda-forge builds it, without MPI or
// CUDA, and the libraries it loads, and AmberTools' licence, on Debian.
// AmberTools is installed whole in a first stage, a few gigabytes of it,
// and only cpptraj kept: each library it cannot find is copied in from
// that installation until it finds them all.
const trajDockerfile = `FROM mambaorg/micromamba:2.0.5-debian12-slim AS build
USER root
RUN micromamba create -y -p /opt/amber -c conda-forge --override-channels ambertools=25.3=cuda_None_nompi_py312hd652fd9_100
RUN mkdir -p /opt/cpptraj/bin /opt/cpptraj/lib /opt/cpptraj/share/licenses && cp /opt/amber/bin/cpptraj /opt/cpptraj/bin/ \
 && cp /opt/conda/pkgs/ambertools-25.3-*/info/licenses/AmberTools/LICENSE /opt/cpptraj/share/licenses/AmberTools \
 && for pass in 1 2 3 4 5 6 7 8; do ldd /opt/cpptraj/bin/cpptraj | awk '/not found/ { print "/opt/amber/lib/" $1 }' | xargs -r cp -L -t /opt/cpptraj/lib/; done \
 && ! ldd /opt/cpptraj/bin/cpptraj | grep 'not found'
FROM debian:12.12-slim
COPY --from=build /opt/cpptraj /opt/cpptraj
ENV PATH=/opt/cpptraj/bin:$PATH
RUN cpptraj --version
`

// TrajImage names that image. The name carries what the image is made
// from, as TranscodeImage's does.
var TrajImage = func() string {
	sum := sha256.Sum256([]byte(trajDockerfile))
	return "sisyphus-traj:" + hex.EncodeToString(sum[:6])
}()

// trajScript is what each task runs. The topology is /input/topology.parm7
// or /input/topology.pdb, and trajectory i /input/traj-<i>.<dcd, nc or
// mdcrd>, numbered from 000. Where one trajectory is shared out by its
// frames, the task counts them with cpptraj and takes the contiguous run
// that is its share; where several are shared out whole, it takes the run
// of them that is its share, which are all it is given, and counts each's
// frames. Either way it says which it took in summary.json before anything
// else, and then has cpptraj read them, with the first frame of the first
// trajectory as the reference to fit onto, and run the analyses the worker
// gave it in ACTIONS, which leave their results in /output. cpptraj runs
// on one thread, since the pool runs as many tasks on a worker as it has
// processors.
const trajScript = `set -euo pipefail
export OMP_NUM_THREADS=1
work=/output/.work
mkdir -p "$work"
cd "$work"
topology=$(echo /input/topology.*)
trajectory() { echo /input/traj-$(printf %03d "$1").*; }
frames() {
	if ! said=$(cpptraj -p "$topology" -y "$1" -tl 2>&1); then
		printf '%s\n' "$said" | grep '^Error' >&2 || true
		echo "trajectory $2 cannot be read with this topology" >&2
		exit 1
	fi
	n=$(printf '%s\n' "$said" | awk '/^Frames:/ { print $2 }')
	case "$n" in ''|*[!0-9]*) echo "cpptraj did not say how many frames trajectory $2 has" >&2; exit 1 ;; esac
	echo "$n"
}
i=$SISYPHUS_TASK_INDEX
k=$SISYPHUS_TASK_COUNT
if [ "$SPLIT" = frames ]; then
	file=$(trajectory 0)
	n=$(frames "$file" 0)
	first_file=0 last_file=0 counts=$n
	first=$(( n * i / k + 1 ))
	last=$(( n * (i + 1) / k ))
	trajin="trajin $file $first $last"
else
	first_file=$(( FILES * i / k ))
	last_file=$(( FILES * (i + 1) / k - 1 ))
	counts= trajin= total=0
	for j in $(seq "$first_file" "$last_file"); do
		file=$(trajectory "$j")
		n=$(frames "$file" "$j")
		counts=${counts:+$counts,}$n
		trajin="$trajin
trajin $file"
		total=$(( total + n ))
	done
	first=1 last=$total
fi
printf '{"split":"%s","files":%d,"first_file":%d,"last_file":%d,"counts":[%s],"first":%d,"last":%d,"analyses":"%s","mask":"%s","mask1":"%s","mask2":"%s","region":"%s"}\n' \
	"$SPLIT" "$FILES" "$first_file" "$last_file" "$counts" "$first" "$last" "$ANALYSES" "$MASK" "$MASK1" "$MASK2" "$REGION" > /output/summary.json
if [ "$first" -gt "$last" ]; then
	echo "no frames are left to this task" >&2
	exit 0
fi
{
	echo "parm $topology"
	if [ "$REFERENCE" = 1 ]; then echo "reference $(trajectory 0) 1 [first]"; fi
	printf '%s\n' "$trajin" "$ACTIONS" run
} > "$work/analysis.in"
echo "analysing $(( last - first + 1 )) frames of trajectories $first_file to $last_file: $ANALYSES" >&2
if ! cpptraj -i "$work/analysis.in" > "$work/cpptraj.log" 2>&1; then
	grep -E '^(Error|Warning)' "$work/cpptraj.log" | tail -n 5 >&2 || true
	echo "cpptraj failed" >&2
	exit 1
fi
for output in $OUTPUTS; do
	if [ ! -s "/output/$output" ]; then
		grep '^Warning' "$work/cpptraj.log" | tail -n 5 >&2 || true
		echo "cpptraj wrote no $output, for the reason it warns of above: most often a mask that selects too few of this topology's atoms" >&2
		exit 1
	fi
done
echo "analysed $(( last - first + 1 )) frames" >&2
`

func (Traj) Name() string { return "traj" }

func (Traj) Describe() string {
	return `Analyses molecular dynamics trajectories with AmberTools' cpptraj, one long trajectory shared out across the pool by runs of its frames, or several shared out whole, and puts the frames' results back together in order. The analyses are chosen from a fixed menu; no cpptraj commands are taken. Parameters: {"topology": "<CID of the stored Amber topology (prmtop or parm7), or a PDB file of the system, such as an md job's final PDB>", "trajectory": "<CID of a stored trajectory, DCD, NetCDF or mdcrd>", "trajectories": ["<CIDs of several, in place of trajectory, in order>", ...], "analyses": "<some of rmsd, radgyr, distance, hbond, dssp and rmsf joined with commas; default rmsd,radgyr>", "mask": "<Amber mask of the atoms RMSD, RMSF and the radius of gyration are taken over, default @CA>", "mask1": "<mask of one group, for distance>", "mask2": "<mask of the other>", "region": "<mask of the atoms hydrogen bonds and secondary structure are looked for in, default *>", "tasks": <optional number of tasks; rmsf is found in one>}. Result: {"frames": <n>, "trajectories": <n>, "trajectory_frames": [<frames of each>], "analyses": [...], "series": "<CID of a CSV table, a row for each frame: frame, trajectory, rmsd, radgyr, distance, hbonds>", "rmsd_angstrom": {"mean", "stdev", "min", "max"}, "radgyr_angstrom": {...}, "distance_angstrom": {...}, "hbonds_per_frame": {...}, "hbonds": "<CID of a CSV table of each hydrogen bond and how often it was made>", "rmsf": "<CID of a CSV table of each residue's RMSF>", "dssp": "<CID of a CSV table of each residue's secondary structure in each frame>", "dssp_residues": "<CID of a CSV table of the fraction of frames each residue spent in each kind>", "structure": {"alpha": <fraction>, ...}, "tasks": <n>}.`
}

func (Traj) Form() []Field {
	return []Field{
		{Name: "topology", Kind: KindFile, Label: "Topology", Help: "The system's Amber topology (prmtop or parm7), or a PDB file of it with its atoms in the trajectory's order: for an md job's trajectory, its final PDB.", Required: true},
		{Name: "trajectory", Kind: KindFile, Label: "Trajectory", Help: "The stored trajectory, as DCD, NetCDF or Amber's text mdcrd.", Required: true},
		{Name: "analyses", Kind: KindText, Label: "Analyses", Help: "Some of rmsd, radgyr, distance, hbond, dssp and rmsf, joined with commas, such as rmsd,radgyr,hbond.", Default: "rmsd,radgyr"},
		{Name: "mask", Kind: KindText, Label: "Atoms", Help: "The Amber mask of the atoms RMSD, RMSF and the radius of gyration are taken over: @CA for the alpha carbons, or :1-20@CA,C,N for a backbone.", Default: "@CA"},
		{Name: "mask1", Kind: KindText, Label: "Distance from", Help: "For distance, the Amber mask of one group of atoms, such as :5@CA.", Default: ""},
		{Name: "mask2", Kind: KindText, Label: "Distance to", Help: "For distance, the Amber mask of the other group of atoms, such as :20@CA.", Default: ""},
		{Name: "region", Kind: KindText, Label: "Region", Help: "The Amber mask of the atoms hydrogen bonds and secondary structure are looked for in: * for the whole solute.", Default: "*"},
		{Name: "tasks", Kind: KindInteger, Label: "Tasks", Help: "How many runs of frames to share the trajectory out as, or 0 to leave it to the pool. RMSF is found in one.", Default: 0, Minimum: bound(0), Maximum: bound(MaxTasks)},
	}
}

// topologyKind says what the start of a stored file says it is: an Amber
// topology, a PDB file, or neither.
func topologyKind(head []byte) string {
	switch {
	case bytes.HasPrefix(head, []byte("%VERSION")), bytes.HasPrefix(head, []byte("%FLAG")):
		return "parm7"
	}
	record, _, _ := bytes.Cut(head, []byte("\n"))
	if len(record) >= 6 && slices.Contains([]string{"HEADER", "TITLE ", "REMARK", "CRYST1", "MODEL ", "ATOM  ", "HETATM", "COMPND", "AUTHOR", "EXPDTA", "SEQRES"}, string(record[:6])) {
		return "pdb"
	}
	return ""
}

// trajectoryKind says what the start of a stored file says it is: a DCD
// file, of 32-bit records or 64-bit ones; NetCDF, classic, 64-bit offset
// or version 4, which is HDF5; Amber's mdcrd, which is text; or none of
// them.
func trajectoryKind(head []byte) string {
	switch {
	case len(head) >= 8 && string(head[4:8]) == "CORD", len(head) >= 12 && string(head[8:12]) == "CORD":
		return "dcd"
	case bytes.HasPrefix(head, []byte("CDF\x01")), bytes.HasPrefix(head, []byte("CDF\x02")), bytes.HasPrefix(head, []byte("\x89HDF\r\n\x1a\n")):
		return "nc"
	case len(head) > 0 && !bytes.ContainsFunc(head, func(r rune) bool { return r != '\n' && r != '\r' && r != '\t' && (r < ' ' || r > '~') }):
		return "mdcrd"
	}
	return ""
}

// readHead reads the first bytes of a stored file, to tell what it is.
func readHead(ctx context.Context, blobs Blobs, id cid.Cid) ([]byte, error) {
	blob, err := blobs.Open(ctx, id)
	if err != nil {
		return nil, err
	}
	defer blob.Close()
	head := make([]byte, 512)
	n, _ := io.ReadFull(blob, head) // a short file has a short head
	return head[:n], nil
}

func (Traj) Split(ctx context.Context, blobs Blobs, params []byte, parts int) ([][]byte, error) {
	var p TrajParams
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, fmt.Errorf("traj parameters: %w", err)
	}
	topology, err := cid.Decode(p.Topology)
	if err != nil {
		return nil, fmt.Errorf("traj parameters: topology is not the CID of a stored topology: %w", err)
	}
	trajectories := p.Trajectories
	switch {
	case p.Trajectory != "" && len(trajectories) > 0:
		return nil, errors.New("traj parameters: trajectories are given in place of trajectory, not with it")
	case p.Trajectory != "":
		trajectories = []string{p.Trajectory}
	case len(trajectories) == 0:
		return nil, errors.New("traj parameters: no trajectory is given")
	case len(trajectories) > trajMostTrajectories:
		return nil, fmt.Errorf("traj parameters: %d trajectories are given; a job can have at most %d", len(trajectories), trajMostTrajectories)
	}
	ids := make([]cid.Cid, len(trajectories))
	for i, trajectory := range trajectories {
		if ids[i], err = cid.Decode(trajectory); err != nil {
			return nil, fmt.Errorf("traj parameters: trajectory %d is not the CID of a stored trajectory: %w", i, err)
		}
	}
	analyses, err := parseTrajAnalyses(cmp.Or(p.Analyses, "rmsd,radgyr"))
	if err != nil {
		return nil, fmt.Errorf("traj parameters: %w", err)
	}
	settings := trajSettings{Analyses: analyses, Mask: cmp.Or(p.Mask, "@CA"), Mask1: p.Mask1, Mask2: p.Mask2, Region: cmp.Or(p.Region, "*")}
	if err := settings.check(); err != nil {
		return nil, fmt.Errorf("traj parameters: %w", err)
	}
	if p.Tasks < 0 || p.Tasks > MaxTasks {
		return nil, fmt.Errorf("traj parameters: tasks is %d, and must be from 1 to %d, or 0 to leave it to the pool", p.Tasks, MaxTasks)
	}
	// RMSF is a mean over every frame of how far each atom is from its
	// mean position, which no task can find from its own frames alone, so
	// a job that asks for it is one task.
	if settings.has("rmsf") && len(trajectories) > trajMostEach+1 {
		return nil, fmt.Errorf("traj parameters: rmsf is found in one task, and one task can be given at most %d trajectories, not %d", trajMostEach+1, len(trajectories))
	}

	// What each file is decides what it is called in a task, which is how
	// cpptraj knows it, so the first bytes of each are read here; the
	// frames are counted by the tasks.
	head, err := readHead(ctx, blobs, topology)
	if err != nil {
		return nil, fmt.Errorf("traj topology %s: %w", topology, err)
	}
	kind := topologyKind(head)
	if kind == "" {
		return nil, errors.New("traj parameters: topology is neither an Amber topology (prmtop or parm7) nor a PDB file")
	}
	inputs := map[string]string{"topology." + kind: p.Topology}
	names := make([]string, len(ids))
	for i, id := range ids {
		head, err := readHead(ctx, blobs, id)
		if err != nil {
			return nil, fmt.Errorf("traj trajectory %d, %s: %w", i, id, err)
		}
		kind := trajectoryKind(head)
		if kind == "" {
			return nil, fmt.Errorf("traj parameters: trajectory %d is neither DCD, NetCDF nor Amber's mdcrd", i)
		}
		names[i] = fmt.Sprintf("traj-%03d.%s", i, kind)
	}

	split, tasks := "frames", cmp.Or(p.Tasks, parts, 1)
	switch {
	case settings.has("rmsf"):
		tasks = 1
	case len(ids) > 1:
		// Several trajectories are shared out whole, no more tasks than
		// there are of them, and enough that none is given too many.
		tasks = max(min(tasks, len(ids)), (len(ids)+trajMostEach-1)/trajMostEach)
	}
	if len(ids) > 1 {
		split = "files"
	}
	// Every task is given the first trajectory where it needs its first
	// frame, and where it is the one shared out by its frames.
	if split == "frames" || settings.needsReference() {
		inputs[names[0]] = trajectories[0]
	}
	env := settings.env()
	env["SPLIT"], env["FILES"] = split, strconv.Itoa(len(ids))
	payloads, err := Container{}.Split(ctx, blobs, mustJSON(ContainerParams{
		Image: TrajImage, Command: []string{"bash", "-c", trajScript},
		Inputs: inputs, Tasks: tasks, Env: env,
	}), parts)
	if err != nil || split == "frames" {
		return payloads, err
	}
	// Each task is given only its own run of trajectories, as the script
	// reckons its share, so that no worker fetches what it does not read:
	// at most trajMostEach of them, by their checked CIDs and names made
	// here, which the worker checks again.
	for i := range payloads {
		var task containerTask
		json.Unmarshal(payloads[i], &task) // made just now
		for j := len(ids) * i / tasks; j < len(ids)*(i+1)/tasks; j++ {
			task.Inputs[names[j]] = trajectories[j]
		}
		payloads[i] = mustJSON(task)
	}
	return payloads, nil
}

func (t Traj) Execute(ctx context.Context, blobs Blobs, payload []byte) ([]byte, error) {
	var task containerTask
	if err := json.Unmarshal(payload, &task); err != nil {
		return nil, fmt.Errorf("traj payload: %w", err)
	}
	// The worker runs only the image of its own making, and its own
	// script, and makes cpptraj's commands itself, from settings it checks
	// for itself: whatever commands the payload holds are not run.
	if task.Image != TrajImage {
		return nil, fmt.Errorf("traj payload: %q is not the image trajectories are analysed in", task.Image)
	}
	settings, err := trajSettingsOf(task.Env["ANALYSES"], task.Env["MASK"], task.Env["MASK1"], task.Env["MASK2"], task.Env["REGION"])
	if err != nil {
		return nil, fmt.Errorf("traj payload: %w", err)
	}
	if split := task.Env["SPLIT"]; split != "frames" && split != "files" {
		return nil, fmt.Errorf("traj payload: the trajectories are shared out by %q, which is neither frames nor files", split)
	}
	if files, err := strconv.Atoi(task.Env["FILES"]); err != nil || files < 1 || files > trajMostTrajectories {
		return nil, fmt.Errorf("traj payload: %q trajectories", task.Env["FILES"])
	}
	task.Command = []string{"bash", "-c", trajScript}
	for key, value := range settings.env() {
		task.Env[key] = value
	}
	engine := t.Engine
	if engine == nil {
		engine = docker
	}
	if err := haveImage(ctx, engine, TrajImage, trajDockerfile, "analysing trajectories"); err != nil {
		return nil, err
	}
	return Container{Engine: engine, Confined: t.Confined}.Execute(ctx, blobs, mustJSON(task))
}

// Varies says that two honest workers may give different numbers for a
// task. On one machine cpptraj gives the same bytes every time, and however
// the job is shared out, since each frame's numbers are found from that
// frame and the first alone. But each worker builds the image from
// conda-forge as it is that day, so two workers may run different builds of
// the libraries cpptraj loads, and OpenBLAS among them picks its arithmetic
// by the processor it finds. Whether that moves the sixth decimal place of
// these analyses has not been measured; comparing two runs' bytes, as
// verification does, would take honest workers for wrong ones if it did.
func (Traj) Varies() bool { return true }

// trajSummary is what a task says it analysed. Shared out by frames, it
// is frames First to Last of the one trajectory, of Counts[0]; shared out
// whole, trajectories FirstFile to LastFile of Files, of Counts frames
// each, and First is 1 and Last how many frames that is. It also says the
// settings it ran with.
type trajSummary struct {
	Split     string `json:"split"`
	Files     int    `json:"files"`
	FirstFile int    `json:"first_file"`
	LastFile  int    `json:"last_file"`
	Counts    []int  `json:"counts"`
	First     int    `json:"first"`
	Last      int    `json:"last"`
	Analyses  string `json:"analyses"`
	Mask      string `json:"mask"`
	Mask1     string `json:"mask1"`
	Mask2     string `json:"mask2"`
	Region    string `json:"region"`
}

// Bounds of what a traj job's tasks may say: the most frames a job may
// have in all, ten million, more than a table of 64 MiB holds a row for;
// the most residues secondary structure may be found for; and the most
// hydrogen bonds that may be listed. Each bounds what the coordinator holds
// in memory as it puts the tasks' tables together.
const (
	trajMostFrames   = 10_000_000
	trajMostResidues = 100_000
	trajMostHBonds   = 100_000
)

// trajRun is a run of a task's frames, all from one trajectory.
type trajRun struct{ trajectory, frames int }

// trajFrom says which trajectory each of a task's frames is from, as runs
// of them in order.
type trajFrom []trajRun

func (f trajFrom) frames() (n int) {
	for _, run := range f {
		n += run.frames
	}
	return n
}

// trajectory is which trajectory the task's frame r, from 0, is from: r
// is one of its frames.
func (f trajFrom) trajectory(r int) int {
	run := 0
	for r >= f[run].frames {
		r -= f[run].frames
		run++
	}
	return f[run].trajectory
}

// trajNumber reports whether a field is a number as cpptraj writes one:
// up to 15 digits, perhaps with a minus sign before them and a point and
// up to 15 more after. Nothing else is put in a table. It is the regular
// expression -?[0-9]{1,15}(\.[0-9]{1,15})?, by hand, since a table may have
// millions of them.
func trajNumber(field string) bool {
	field = strings.TrimPrefix(field, "-")
	whole, fraction, pointed := strings.Cut(field, ".")
	digits := func(s string) bool {
		return len(s) >= 1 && len(s) <= 15 && !strings.ContainsFunc(s, func(r rune) bool { return r < '0' || r > '9' })
	}
	return digits(whole) && (!pointed || digits(fraction))
}

// trajWhole is a whole number as cpptraj writes one where it writes
// numbers with fractions.
var trajWhole = regexp.MustCompile(`^[0-9]{1,8}(\.0{1,15})?$`)

// trajName is an atom's or a residue's name as cpptraj writes it in a
// table: the residue's name and number, and the atom's name.
var trajName = regexp.MustCompile(`^[A-Za-z0-9+\-_'*]{1,8}[_:][0-9]{1,7}(@[A-Za-z0-9+\-_'*]{1,8})?$`)

// trajCombined is what the tasks' tables come to, as they are put
// together.
type trajCombined struct {
	settings trajSettings
	frames   int // put together so far
	// series are the rows of the series table, and values the numbers in
	// each of its columns.
	series bytes.Buffer
	values [][]float64
	row    []byte // the row being written
	// hbonds are how many frames each hydrogen bond was made in.
	hbonds map[[3]string]int
	// residues name the columns of DSSP's table, dssp holds its rows, and
	// kinds how many frames each residue spent in each kind.
	residues []string
	dssp     bytes.Buffer
	kinds    [][8]int
	// rmsf is the table of RMSF.
	rmsf bytes.Buffer
}

// Aggregate takes from each task only the files it is meant to leave, by
// their fixed names, and checks that the tasks analysed every frame once,
// in order, with the same settings, and that each table has a row for
// each of its frames, before anything they said is believed.
func (Traj) Aggregate(ctx context.Context, blobs Blobs, outputs [][]byte) ([]byte, error) {
	result := TrajResult{Tasks: len(outputs)}
	var c trajCombined
	var first trajSummary
	read, files := 0, 0
	for i, raw := range outputs {
		var out ContainerOutput
		if err := json.Unmarshal(raw, &out); err != nil {
			return nil, fmt.Errorf("task %d output: %w", i, err)
		}
		var summary trajSummary
		if err := decodeTrajSummary(ctx, blobs, out.Files["summary.json"], &summary); err != nil {
			return nil, fmt.Errorf("task %d's summary: %w", i, err)
		}
		if i == 0 {
			first = summary
			settings, err := trajSettingsOf(summary.Analyses, summary.Mask, summary.Mask1, summary.Mask2, summary.Region)
			if err != nil || summary.Split != "frames" && summary.Split != "files" || summary.Files < 1 || summary.Files > trajMostTrajectories || summary.Split == "frames" && summary.Files != 1 {
				return nil, fmt.Errorf("task %d says it analysed %d trajectories, shared out by %q, with settings no job asks for: %v", i, summary.Files, summary.Split, err)
			}
			if settings.has("rmsf") && len(outputs) != 1 {
				return nil, fmt.Errorf("rmsf is found in one task, and %d tasks say they found it", len(outputs))
			}
			c = trajCombined{settings: settings, hbonds: map[[3]string]int{}}
			result.Analyses, result.Trajectories = settings.Analyses, summary.Files
			c.values = make([][]float64, len(trajSeries))
		}
		if summary.Split != first.Split || summary.Files != first.Files || summary.Analyses != first.Analyses ||
			summary.Mask != first.Mask || summary.Mask1 != first.Mask1 || summary.Mask2 != first.Mask2 || summary.Region != first.Region {
			return nil, fmt.Errorf("task %d analysed %d trajectories, shared out by %q, with %q, and task 0 %d, by %q, with %q", i, summary.Files, summary.Split, summary.Analyses, first.Files, first.Split, first.Analyses)
		}
		// Which trajectory each of this task's frames is from.
		var from trajFrom
		if summary.Split == "frames" {
			switch {
			case summary.FirstFile != 0 || summary.LastFile != 0 || len(summary.Counts) != 1 || summary.Counts[0] < 0 || summary.Counts[0] > trajMostFrames:
				return nil, fmt.Errorf("task %d says it analysed a trajectory of %v frames", i, summary.Counts)
			case summary.Counts[0] != first.Counts[0]:
				return nil, fmt.Errorf("task %d analysed a trajectory of %d frames, and task 0 one of %d", i, summary.Counts[0], first.Counts[0])
			case summary.First != read+1 || summary.Last < read || summary.Last > summary.Counts[0]:
				return nil, fmt.Errorf("task %d says it analysed frames %d to %d of %d, after frames up to %d", i, summary.First, summary.Last, summary.Counts[0], read)
			}
			from = trajFrom{{0, summary.Last - read}}
			read, files = summary.Last, 1
		} else {
			if summary.FirstFile != files || summary.LastFile < summary.FirstFile || summary.LastFile >= summary.Files || len(summary.Counts) != summary.LastFile-summary.FirstFile+1 {
				return nil, fmt.Errorf("task %d says it analysed trajectories %d to %d of %d, after trajectories up to %d", i, summary.FirstFile, summary.LastFile, summary.Files, files-1)
			}
			for j, count := range summary.Counts {
				if count < 0 || count > trajMostFrames-result.Frames-from.frames() {
					return nil, fmt.Errorf("task %d says trajectory %d has %d frames, past the %d a job may have", i, summary.FirstFile+j, count, trajMostFrames)
				}
				from = append(from, trajRun{summary.FirstFile + j, count})
			}
			if summary.First != 1 || summary.Last != from.frames() {
				return nil, fmt.Errorf("task %d says it analysed frames %d to %d of trajectories with %d", i, summary.First, summary.Last, from.frames())
			}
			files = summary.LastFile + 1
		}
		if summary.Split == "files" || i == 0 {
			result.TrajectoryFrames = append(result.TrajectoryFrames, summary.Counts...)
		}
		if from.frames() == 0 {
			continue // more tasks than frames, or trajectories of none
		}
		if err := c.add(ctx, blobs, i, out.Files, from); err != nil {
			return nil, err
		}
		result.Frames = c.frames
	}
	if first.Split == "frames" && read != first.Counts[0] || files != first.Files {
		return nil, fmt.Errorf("the tasks analysed %d of the %d trajectories, and %d of the frames", files, first.Files, read)
	}
	return c.result(ctx, blobs, result)
}

// add puts together the tables a task left for its frames, each from which
// trajectory, with the tables of the tasks before it.
func (c *trajCombined) add(ctx context.Context, blobs Blobs, task int, files map[string]string, from trajFrom) error {
	frames := from.frames()
	// The numbers for each frame, of each series asked for, in its order.
	columns := make([][]string, len(trajSeries))
	for s, series := range trajSeries {
		if !c.settings.has(series.analysis) {
			continue
		}
		_, rows, err := readTrajTable(ctx, blobs, files[series.file], frames)
		if err != nil {
			return fmt.Errorf("task %d's %s: %w", task, series.file, err)
		}
		for r, row := range rows {
			if len(row) != 2 || !trajNumber(row[1]) || series.analysis == "hbond" && strings.Contains(row[1], ".") {
				return fmt.Errorf("task %d's %s: frame %d is %q, not a number", task, series.file, r+1, strings.Join(row[1:], " "))
			}
			value, _ := strconv.ParseFloat(row[1], 64)
			columns[s] = append(columns[s], row[1])
			c.values[s] = append(c.values[s], value)
		}
	}
	for r := range frames {
		if !slices.ContainsFunc(columns, func(column []string) bool { return column != nil }) {
			break // no series is asked for
		}
		row := strconv.AppendInt(c.row[:0], int64(c.frames+r+1), 10)
		row = strconv.AppendInt(append(row, ','), int64(from.trajectory(r)), 10)
		for _, column := range columns {
			if column != nil {
				row = append(append(row, ','), column[r]...)
			}
		}
		c.row = append(row, '\n')
		c.series.Write(c.row)
	}
	if c.settings.has("hbond") {
		if err := c.addHBonds(ctx, blobs, task, files["hbonds-avg.dat"], frames); err != nil {
			return fmt.Errorf("task %d's hbonds-avg.dat: %w", task, err)
		}
	}
	if c.settings.has("dssp") {
		if err := c.addDSSP(ctx, blobs, task, files["dssp.dat"], from); err != nil {
			return fmt.Errorf("task %d's dssp.dat: %w", task, err)
		}
	}
	if c.settings.has("rmsf") {
		if err := c.addRMSF(ctx, blobs, files["rmsf.dat"]); err != nil {
			return fmt.Errorf("task %d's rmsf.dat: %w", task, err)
		}
	}
	if c.series.Len() > trajMostText || c.dssp.Len() > trajMostText {
		return fmt.Errorf("task %d's frames take the tables past the %d bytes a table may have", task, trajMostText)
	}
	c.frames += frames
	return nil
}

// addHBonds counts the frames each hydrogen bond a task found was made in.
func (c *trajCombined) addHBonds(ctx context.Context, blobs Blobs, task int, id string, frames int) error {
	_, rows, err := readTrajTable(ctx, blobs, id, -1)
	if err != nil {
		return err
	}
	seen := map[[3]string]bool{}
	for _, row := range rows {
		if len(row) != 7 || !trajName.MatchString(row[0]) || !trajName.MatchString(row[1]) || !trajName.MatchString(row[2]) {
			return fmt.Errorf("%q is not a hydrogen bond", strings.Join(row, " "))
		}
		made, err := strconv.Atoi(row[3])
		bond := [3]string{row[0], row[1], row[2]}
		switch {
		case err != nil || made < 1 || made > frames:
			return fmt.Errorf("the hydrogen bond %s says it was made in %q of the task's %d frames", strings.Join(row[:3], " "), row[3], frames)
		case seen[bond]:
			return fmt.Errorf("the hydrogen bond %s is listed twice", strings.Join(row[:3], " "))
		case len(c.hbonds) >= trajMostHBonds && c.hbonds[bond] == 0:
			return fmt.Errorf("more than %d hydrogen bonds are listed", trajMostHBonds)
		}
		seen[bond] = true
		c.hbonds[bond] += made
	}
	return nil
}

// addDSSP adds a task's frames to DSSP's table, and counts what each
// residue was in them.
func (c *trajCombined) addDSSP(ctx context.Context, blobs Blobs, task int, id string, from trajFrom) error {
	header, rows, err := readTrajTable(ctx, blobs, id, from.frames())
	if err != nil {
		return err
	}
	header = header[1:] // the frame's column
	if c.residues == nil {
		if len(header) == 0 || len(header) > trajMostResidues {
			return fmt.Errorf("it has %d residues, and must have from 1 to %d", len(header), trajMostResidues)
		}
		for _, residue := range header {
			if !trajName.MatchString(residue) {
				return fmt.Errorf("%q is not a residue", residue)
			}
		}
		c.residues = header
		c.kinds = make([][8]int, len(header))
	} else if !slices.Equal(header, c.residues) {
		return fmt.Errorf("it has residues %s, and the tasks before had %s", firstLine(strings.Join(header, " ")), firstLine(strings.Join(c.residues, " ")))
	}
	for r, row := range rows {
		if len(row) != len(c.residues)+1 {
			return fmt.Errorf("frame %d has %d residues, not %d", r+1, len(row)-1, len(c.residues))
		}
		fmt.Fprintf(&c.dssp, "%d,%d", c.frames+r+1, from.trajectory(r))
		for j, kind := range row[1:] {
			if len(kind) != 1 || kind[0] < '0' || kind[0] > '7' {
				return fmt.Errorf("frame %d says residue %s is %q, which is no kind of structure", r+1, c.residues[j], kind)
			}
			c.kinds[j][kind[0]-'0']++
			c.dssp.WriteString("," + dsspKinds[kind[0]-'0'].letter)
		}
		c.dssp.WriteByte('\n')
	}
	return nil
}

// addRMSF makes RMSF's table, which one task finds for every frame.
func (c *trajCombined) addRMSF(ctx context.Context, blobs Blobs, id string) error {
	_, rows, err := readTrajTable(ctx, blobs, id, -1)
	if err != nil {
		return err
	}
	last := 0
	for _, row := range rows {
		// cpptraj writes the residue's number as a number with a
		// fraction, which is nought: 12.000.
		number, _, _ := strings.Cut(row[0], ".")
		residue, err := strconv.Atoi(number)
		if len(row) != 2 || !trajWhole.MatchString(row[0]) || err != nil || residue <= last || residue > 10_000_000 || !trajNumber(row[1]) {
			return fmt.Errorf("%q is not a residue's RMSF, after residue %d", strings.Join(row, " "), last)
		}
		fmt.Fprintf(&c.rmsf, "%d,%s\n", residue, row[1])
		last = residue
	}
	if last == 0 {
		return errors.New("it has no residues")
	}
	return nil
}

// result stores the tables put together, and sums them up.
func (c *trajCombined) result(ctx context.Context, blobs Blobs, result TrajResult) ([]byte, error) {
	store := func(what, header string, body []byte) (string, error) {
		stored, err := blobs.Put(ctx, io.MultiReader(strings.NewReader(header+"\n"), bytes.NewReader(body)))
		if err != nil {
			return "", fmt.Errorf("store the %s: %w", what, err)
		}
		return stored.String(), nil
	}
	if result.Frames == 0 {
		return mustJSON(result), nil
	}
	var err error
	if c.series.Len() > 0 {
		header := "frame,trajectory"
		for s, series := range trajSeries {
			if c.settings.has(series.analysis) {
				header += "," + series.column
				stats := trajStatsOf(c.values[s])
				switch series.analysis {
				case "rmsd":
					result.RMSD = stats
				case "radgyr":
					result.RadGyr = stats
				case "distance":
					result.Distance = stats
				case "hbond":
					result.HBondsPerFrame = stats
				}
			}
		}
		if result.Series, err = store("series", header, c.series.Bytes()); err != nil {
			return nil, err
		}
	}
	if c.settings.has("hbond") {
		// Most often made first, and in the order of their names where
		// they were made as often.
		bonds := make([][3]string, 0, len(c.hbonds))
		for bond := range c.hbonds {
			bonds = append(bonds, bond)
		}
		sort.Slice(bonds, func(i, j int) bool {
			if a, b := c.hbonds[bonds[i]], c.hbonds[bonds[j]]; a != b {
				return a > b
			}
			return slices.Compare(bonds[i][:], bonds[j][:]) < 0
		})
		var table bytes.Buffer
		for _, bond := range bonds {
			// No task says a bond was made in more of its frames than it
			// had, so none was made in more than there were.
			made := c.hbonds[bond]
			fmt.Fprintf(&table, "%s,%s,%s,%d,%.4f\n", bond[0], bond[1], bond[2], made, float64(made)/float64(result.Frames))
		}
		if result.HBonds, err = store("hydrogen bonds", "acceptor,donor_hydrogen,donor,frames,fraction", table.Bytes()); err != nil {
			return nil, err
		}
	}
	if c.settings.has("dssp") {
		if result.DSSP, err = store("secondary structure", "frame,trajectory,"+strings.Join(c.residues, ","), c.dssp.Bytes()); err != nil {
			return nil, err
		}
		header := "residue"
		var all [8]int
		for _, kind := range dsspKinds {
			header += "," + kind.name
		}
		var table bytes.Buffer
		for j, residue := range c.residues {
			table.WriteString(residue)
			for k, frames := range c.kinds[j] {
				fmt.Fprintf(&table, ",%.4f", float64(frames)/float64(result.Frames))
				all[k] += frames
			}
			table.WriteByte('\n')
		}
		if result.DSSPResidues, err = store("secondary structure of each residue", header, table.Bytes()); err != nil {
			return nil, err
		}
		result.Structure = map[string]float64{}
		for k, kind := range dsspKinds {
			result.Structure[kind.name] = math.Round(float64(all[k])/float64(result.Frames*len(c.residues))*1e4) / 1e4
		}
	}
	if c.settings.has("rmsf") {
		if result.RMSF, err = store("RMSF", "residue,rmsf", c.rmsf.Bytes()); err != nil {
			return nil, err
		}
	}
	return mustJSON(result), nil
}

// trajStatsOf sums up a column of numbers.
func trajStatsOf(values []float64) *TrajStats {
	mean, spread := meanAndSpread(values)
	return &TrajStats{Mean: mean, Stdev: spread, Min: slices.Min(values), Max: slices.Max(values)}
}

// decodeTrajSummary reads a task's summary.
func decodeTrajSummary(ctx context.Context, blobs Blobs, id string, summary *trajSummary) error {
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

// readTrajTable reads a table cpptraj wrote, of at most trajMostText bytes:
// its header, the fields of its first line without the # it begins with,
// and its rows, each split into its fields. A table of frames has a row
// for each, numbered from 1, and frames is how many; a table of anything
// else, frames -1, has any number of rows.
func readTrajTable(ctx context.Context, blobs Blobs, id string, frames int) (header []string, rows [][]string, err error) {
	c, err := cid.Decode(id)
	if err != nil {
		return nil, nil, errors.New("the task left none")
	}
	blob, err := blobs.Open(ctx, c)
	if err != nil {
		return nil, nil, err
	}
	defer blob.Close()
	within := &io.LimitedReader{R: blob, N: trajMostText + 1}
	lines := bufio.NewScanner(within)
	lines.Buffer(nil, trajMostText+1)
	for lines.Scan() {
		row := strings.Fields(lines.Text())
		switch {
		case header == nil:
			if len(row) == 0 || !strings.HasPrefix(row[0], "#") {
				return nil, nil, errors.New("it has no header")
			}
			header = append([]string{strings.TrimPrefix(row[0], "#")}, row[1:]...)
		case len(row) == 0:
		case frames >= 0 && row[0] != strconv.Itoa(len(rows)+1):
			return nil, nil, fmt.Errorf("row %d is of frame %q", len(rows)+1, row[0])
		default:
			rows = append(rows, row)
		}
	}
	if err := lines.Err(); err != nil {
		return nil, nil, err
	}
	switch {
	case within.N == 0:
		return nil, nil, fmt.Errorf("it is longer than the %d bytes a table may be", trajMostText)
	case header == nil:
		return nil, nil, errors.New("it is empty")
	case frames >= 0 && len(rows) != frames:
		return nil, nil, fmt.Errorf("it has %d frames, and the task says it analysed %d", len(rows), frames)
	}
	return header, rows, nil
}

package jobmodel

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"slices"
	"strings"
	"time"
)

// A job that is verified has each of its tasks run by several workers, and
// takes a result only once enough of them have returned the same one. Each
// such run is a copy of the task: an attempt like any other, except that
// several are out at once. What a copy returns is a result, and the task
// keeps every result it is given until they settle it.
//
// The methods here are for the tasks of such a job. They are the
// counterparts of Start, Succeed, Fail and Requeue, which are for a task
// that one worker at a time has.

// Result is what one worker returned for a task of a job that is verified.
type Result struct {
	TaskIndex int
	// Attempt is the number of the attempt that returned it.
	Attempt int
	// NodeID is the worker that returned it, and NodeName the label that
	// worker gave itself.
	NodeID   string
	NodeName string
	Output   []byte
	// Blobs are the CIDs of the blobs the attempt stored, in order.
	Blobs []string
	// CPUModel is the processor of the worker that returned it, as the
	// coordinator knew that worker when it handed out the attempt: what the
	// worker said of its machine when it joined, never anything read from
	// the output. Empty if unknown.
	CPUModel string
}

// same reports whether two results are the same: the same output, byte for
// byte, and the same stored blobs.
func (r Result) same(other Result) bool {
	return bytes.Equal(r.Output, other.Output) && slices.Equal(r.Blobs, other.Blobs)
}

// Comparer reports whether the outputs of two results of one task are the
// same result, and if they are not, why: it is a workload's own comparison,
// for work whose honest results agree only to some precision. It is given
// the whole results, so that it may hold them to a tolerance that depends
// on where they were computed, and says which tier of its tolerances it
// compared them in, or nothing for a workload that has one. It is
// symmetric and holds for an output compared with itself, but need not be
// transitive: a is within a tolerance of b, and b of c, without a being
// within it of c. Blobs are not its to compare; see Task.Same.
type Comparer func(a, b Result) (same bool, tier, why string)

// CompareBy has the results of the job's tasks compared by compare instead
// of byte for byte; nil goes back to bytes. Like Needs, it follows from the
// job's workload and is not saved: whoever holds the job sets it again.
func (j *Job) CompareBy(compare Comparer) {
	for _, t := range j.Tasks {
		t.compare = compare
	}
}

// Same reports whether two results for the task are the same, and if they
// are not and the task's results are compared by its workload, why not.
// Results compared that way must still have stored the same blobs: the
// workload compares outputs, and blobs are named by what they hold.
// Otherwise they are the same only if their outputs are the same bytes and
// they stored the same blobs, and why is empty.
func (t *Task) Same(a, b Result) (same bool, why string) {
	same, _, why = t.Compare(a, b)
	return same, why
}

// Compare is Same, and says as well which tier of its tolerances the task's
// workload compared the two results in: nothing for results compared byte
// for byte, for a workload with one tolerance, or for results that stored
// different blobs, which the workload is not asked about.
func (t *Task) Compare(a, b Result) (same bool, tier, why string) {
	switch {
	case t.compare == nil:
		return a.same(b), "", ""
	case !slices.Equal(a.Blobs, b.Blobs):
		return false, "", "they stored different blobs"
	}
	return t.compare(a, b)
}

// settledTier is the tier of the workload's tolerances that the results of
// a side were held to: the tier the first of them, the result a task it
// settles is given, was compared with each of the others in. Where those
// comparisons were made in different tiers, which takes three or more
// results, it is each tier, once, in sorted order and joined by "+": the
// side was held to each of them, and to the loosest at least. Nothing for
// a side whose results were compared in no tier.
func (t *Task) settledTier(side []Result) string {
	var tiers []string
	for _, other := range side[1:] {
		if _, tier, _ := t.Compare(side[0], other); tier != "" {
			tiers = append(tiers, tier)
		}
	}
	slices.Sort(tiers)
	return strings.Join(slices.Compact(tiers), "+")
}

// Digest is a name for what a result holds, by which results that differ
// can be told apart without printing them: the SHA-256, in hex, of the
// output and of the name of each blob, each preceded by its length as eight
// bytes, big-endian.
func (r Result) Digest() string {
	hash := sha256.New()
	for _, part := range append([]string{string(r.Output)}, r.Blobs...) {
		hash.Write(binary.BigEndian.AppendUint64(nil, uint64(len(part))))
		hash.Write([]byte(part))
	}
	return hex.EncodeToString(hash.Sum(nil))
}

// Check has the job verified: each of its tasks succeeds only once that
// many different workers have returned the same result for it. With a share
// between zero and one, only that share of the tasks is held to it, rounded
// up to a whole task, and the rest need one result: they are run once. The
// tasks held to it are the first so many of order, which lists the indexes
// of the job's tasks, each once, and which its caller shuffles so that
// nobody can tell beforehand which they will be. A job whose tasks are jobs
// gives no order: it hands what it was asked on to them, and its own tasks
// are held to nothing.
func (j *Job) Check(verify int, share float64, order []int) {
	j.Verify = verify
	checked := len(j.Tasks)
	if share > 0 && share < 1 {
		j.VerifyShare = share
		checked = int(math.Ceil(share * float64(len(j.Tasks))))
	}
	for at, index := range order {
		j.Tasks[index].Verify = 1
		if at < checked {
			j.Tasks[index].Verify = verify
		}
	}
}

// Outvoted reports whether a worker has returned, for a task of the job
// that has succeeded, a result other than the one the task succeeded with.
func (j *Job) Outvoted(nodeID string) bool {
	if j.outvoted == nil {
		j.outvoted = make(map[string]bool)
		for _, t := range j.Tasks {
			j.noteOutvoted(t)
		}
	}
	return j.outvoted[nodeID]
}

// noteOutvoted adds the workers whose results a task succeeded without to
// those the job knows to have been outvoted.
func (j *Job) noteOutvoted(t *Task) {
	if j.outvoted == nil || t.State != Succeeded {
		return
	}
	for _, side := range t.Sides()[min(1, len(t.Results)):] {
		for _, r := range side {
			j.outvoted[r.NodeID] = true
		}
	}
}

// Recheck has verified after all every task the job's spot checks passed
// over that the given worker has a copy of or has returned a result for,
// and returns those tasks. One of them that had succeeded on that worker's
// word alone has not any more: its result stands as one of those that must
// agree, and the task waits for the rest. Recheck is for a worker that has
// been outvoted, and does nothing to a job that is over.
func (j *Job) Recheck(nodeID string) []*Task {
	var again []*Task
	if j.Terminal() {
		return nil
	}
	for _, t := range j.Tasks {
		if t.Verify >= j.Verify || !t.Asked(nodeID) {
			continue
		}
		t.Verify = j.Verify
		if t.State == Succeeded {
			t.Output, t.Tier = nil, ""
			t.rest()
		}
		t.Progress = float64(t.lead()) / float64(t.Verify)
		j.mark(t)
		again = append(again, t)
	}
	return again
}

// MaxResults is how many results a task of a verified job takes in before
// it is given up as one its workers cannot agree on: one fewer than twice
// the number that must agree, which is as many as two different answers can
// draw out before one of them has enough.
func (t *Task) MaxResults() int {
	return 2*t.Verify - 1
}

// Sides returns a task's results sorted into sides: the results that are
// the same as each other. The side with the most results comes first, and
// of two as large the one heard from first. A task whose results all agree
// has one side, and one with none has none.
//
// Where results are compared by their workload, being the same need not be
// transitive, and results do not fall into sides of their own accord. So a
// side is drawn around one result, its anchor: it is the anchor and every
// result left that is the same as the anchor. The first side is drawn
// around whichever result the most others are the same as, the first heard
// from of those that tie; its results are taken out, and the next side is
// drawn from those left in the same way, until none are. The anchor comes
// first in its side, and the rest follow in the order of their attempts.
// Every result of a side is then the same as the first, which is the one a
// task that side settles is given: the task's result agrees with each
// result it was settled by, though those need not all agree with each
// other.
//
// For results compared byte for byte, the sides are the results that are
// identical, each in the order of their attempts, since the first heard
// from of identical results is the one drawn around.
func (t *Task) Sides() [][]Result {
	n := len(t.Results)
	// Each pair is compared once: a comparison may take some work.
	same := make([][]bool, n)
	for i := range same {
		same[i] = make([]bool, n)
		same[i][i] = true
		for j := range i {
			same[i][j], _ = t.Same(t.Results[j], t.Results[i])
			same[j][i] = same[i][j]
		}
	}
	var sides [][]Result
	left := make([]int, n)
	for i := range left {
		left[i] = i
	}
	for len(left) > 0 {
		var side []int
		for _, anchor := range left {
			around := []int{anchor}
			for _, i := range left {
				if i != anchor && same[anchor][i] {
					around = append(around, i)
				}
			}
			if len(around) > len(side) {
				side = around
			}
		}
		results := make([]Result, 0, len(side))
		for _, i := range side {
			results = append(results, t.Results[i])
		}
		sides = append(sides, results)
		left = slices.DeleteFunc(left, func(i int) bool { return slices.Contains(side, i) })
	}
	return sides
}

// Agreed returns the results a verified task was settled by: those of the
// side that had enough. A task that has not succeeded has none.
func (t *Task) Agreed() []Result {
	if t.State != Succeeded || len(t.Results) == 0 {
		return nil
	}
	return t.Sides()[0]
}

// lead is how many results the largest side has.
func (t *Task) lead() int {
	if sides := t.Sides(); len(sides) > 0 {
		return len(sides[0])
	}
	return 0
}

// out is how many copies of the task are with workers now.
func (t *Task) out() int {
	n := 0
	for _, a := range t.History {
		if a.State == Running {
			n++
		}
	}
	return n
}

// rest sets the state of a task that is not settled: running while any
// copy of it is out, and otherwise waiting.
func (t *Task) rest() {
	t.State = Pending
	if t.out() > 0 {
		t.State = Running
	}
}

// drop gives up the copies of the task that are out.
func (t *Task) drop() {
	for i := range t.History {
		if t.History[i].State == Running {
			t.History[i].State = Cancelled
		}
	}
}

// Wanted returns how many more workers a task should be handed to now: as
// many as, with the copies already out, would give its largest side enough
// results if they all agreed with it.
func (j *Job) Wanted(t *Task) int {
	if j.Terminal() || (t.State != Pending && t.State != Running) {
		return 0
	}
	return t.Verify - t.lead() - t.out()
}

// Dropped reports whether an attempt at the task by a worker ended without
// a result, because it failed or was lost. Its next attempt goes to another
// worker if there is one: whatever went wrong there may still be wrong.
func (t *Task) Dropped(nodeID string) bool {
	for _, a := range t.History {
		if a.NodeID == nodeID && (a.State == Pending || a.State == Failed) {
			return true
		}
	}
	return false
}

// Asked reports whether a worker has a copy of the task or has returned a
// result for it. Such a worker is not given the task again: the results
// that settle a task come from different workers.
func (t *Task) Asked(nodeID string) bool {
	for _, r := range t.Results {
		if r.NodeID == nodeID {
			return true
		}
	}
	for _, a := range t.History {
		if a.State == Running && a.NodeID == nodeID {
			return true
		}
	}
	return false
}

// StartCopy records that a copy of a task was handed to a worker, and
// returns the number of that attempt.
func (j *Job) StartCopy(t *Task, nodeID, nodeName string, now time.Time) int {
	t.State, t.StartedAt = Running, now
	t.Attempt++
	t.NodeID, t.NodeName = nodeID, nodeName
	t.History = append(t.History, Attempt{TaskIndex: t.Index, Number: t.Attempt, NodeID: nodeID, NodeName: nodeName, State: Running})
	j.mark(t)
	if j.State == Pending {
		j.State = Running
	}
	return t.Attempt
}

// ReportCopy notes how far along a copy of a task says it is, from 0 to 1.
// The task is as far along as the results it has and the furthest of its
// copies make it: a copy counts for as much as one of the results that
// must agree.
func (j *Job) ReportCopy(t *Task, progress float64) {
	t.Progress = max(t.Progress, (float64(t.lead())+progress)/float64(t.Verify))
}

// ReturnCopy takes in what the copy handed out as the given attempt
// returned: its output, and the blobs it stored. If enough results are now
// the same the task has succeeded, with that result as its output and the
// first worker to return it as its worker, and with the tier of its
// workload's tolerances they were held to as its Tier. ReturnCopy reports
// whether every task is then done, meaning the job is ready to be
// aggregated.
func (j *Job) ReturnCopy(t *Task, attempt int, output []byte, blobs []string) (allDone bool) {
	return j.ReturnCopyOn(t, attempt, "", output, blobs)
}

// ReturnCopyOn is ReturnCopy for a copy whose worker the coordinator knew
// to run on the named processor when it handed the copy out; see
// Result.CPUModel.
func (j *Job) ReturnCopyOn(t *Task, attempt int, cpuModel string, output []byte, blobs []string) (allDone bool) {
	run := &t.History[attempt-1]
	run.State, run.Err = Succeeded, ""
	// In the order of their attempts, which is how they are loaded again.
	at, _ := slices.BinarySearchFunc(t.Results, attempt, func(r Result, n int) int { return r.Attempt - n })
	t.Results = slices.Insert(t.Results, at, Result{TaskIndex: t.Index, Attempt: attempt, NodeID: run.NodeID, NodeName: run.NodeName, Output: output, Blobs: blobs, CPUModel: cpuModel})
	j.mark(t)

	agreed := t.Sides()[0]
	if len(agreed) < t.Verify {
		t.rest()
		t.Progress = float64(len(agreed)) / float64(t.Verify)
		return false
	}
	t.State, t.Progress, t.Output, t.Err = Succeeded, 1, agreed[0].Output, ""
	t.NodeID, t.NodeName = agreed[0].NodeID, agreed[0].NodeName
	t.Tier = t.settledTier(agreed)
	j.noteOutvoted(t)
	for _, other := range j.Tasks {
		if other.State != Succeeded {
			return false
		}
	}
	return true
}

// FailCopy records that the copy handed out as the given attempt failed. A
// failed copy is not a result: another is handed out in its place, unless
// maxAttempts of the task's attempts have now failed, in which case the
// task fails and takes the job with it.
func (j *Job) FailCopy(t *Task, attempt int, reason string, maxAttempts int, now time.Time) {
	run := &t.History[attempt-1]
	run.State, run.Err = Pending, reason
	t.Err = reason
	t.Failures++
	j.mark(t)
	if t.Failures < maxAttempts {
		t.rest()
		return
	}
	run.State, t.State = Failed, Failed
	t.drop()
	j.Finish(nil, fmt.Errorf("task %d failed after %d attempts: %s", t.Index, t.Failures, reason), now)
}

// RequeueCopy takes back the copy handed out as the given attempt without
// counting it against the task, for when it was lost through no fault of
// the task or its worker.
func (j *Job) RequeueCopy(t *Task, attempt int, reason string) {
	run := &t.History[attempt-1]
	run.State, run.Err = Pending, reason
	t.Err = reason
	t.rest()
	j.mark(t)
}

// Dispute fails a task whose workers returned results that differ and that
// nothing more can settle, and the job with it.
func (j *Job) Dispute(t *Task, reason string, now time.Time) {
	t.State, t.Err = Failed, reason
	t.drop()
	j.mark(t)
	j.Finish(nil, errors.New(reason), now)
}

// Probation is how many results a worker that has been outvoted must then
// return that agree, before its word alone is taken again.
const Probation = 10

// Standing is what a coordinator has seen of a worker across jobs: how the
// results it returned for verified tasks came out once those tasks were
// settled. It is kept by the worker's node ID, and counts results, not
// tasks run: an attempt that failed returned none, and a task that was not
// verified, or never settled, says nothing about its workers.
type Standing struct {
	NodeID string
	// Agreed counts the results that were among those a task was settled
	// by, and Outvoted those a task was settled without.
	Agreed, Outvoted uint64
	// Probation is how many agreeing results the worker still owes, having
	// been outvoted: zero for one that never was, or has since returned as
	// many as were asked.
	Probation int
}

// Agree counts a result that was one of those its task was settled by.
func (s *Standing) Agree() {
	s.Agreed++
	s.Probation = max(s.Probation-1, 0)
}

// Outvote counts a result its task was settled without, which puts the
// worker on probation from the start, however far along one it was.
func (s *Standing) Outvote() {
	s.Outvoted++
	s.Probation = Probation
}

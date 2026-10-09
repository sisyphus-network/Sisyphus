package coordinator

import (
	"cmp"
	crand "crypto/rand"
	"fmt"
	mrand "math/rand/v2"
	"slices"
	"strings"
	"time"

	jobmodel "github.com/sisyphus-network/Sisyphus/packages/job-model"
)

// Verification by replication: a job that asks for it has each of its tasks
// run by that many different workers, and a task succeeds only once that
// many of them have returned the same result. The rules:
//
//   - Two results are the same if their outputs are the same bytes and the
//     blobs their attempts stored have the same CIDs.
//     That holds for a private job too: sealing gives the same blob for
//     the same key and data, so the same work stores the same CIDs.
//   - A task is handed to as many workers at once as must agree. No worker
//     is asked twice for a result: one that has returned a result for a
//     task, or has a copy of it, is not given it again.
//   - If the results differ, the task is handed to one more worker for each
//     result its largest side still lacks, until some result has been
//     returned by enough workers. A task takes in at most one fewer than
//     twice that many results; if no side can reach enough within that, or
//     no connected worker is left that has not been asked, the task fails
//     and the job with it, saying which workers returned what.
//   - An attempt that fails, times out or is lost with its worker is not a
//     result. It is tried again as any failed attempt is, by the same
//     worker or another, and counts towards the same limit.
//   - A task that needs another worker when none it has not asked is
//     connected, and whose results so far do not differ, waits for one, as
//     a task of any job waits for a worker.
//
// Spot checks: a job can ask that only a share of its tasks be verified.
// Then:
//
//   - That share of the tasks, rounded up to a whole task, is picked at
//     random when the job is submitted. The others need one result: each is
//     run once and what its worker returns is taken. Nothing a worker is
//     sent says which kind of task it has been handed.
//   - A worker is outvoted when a task succeeds with a result other than
//     the one it returned. Every task of the job that such a worker ran
//     unverified, or is running, is then verified after all: what the
//     worker returned for it stands as one result, and the task goes to as
//     many more workers as it now lacks. So does any task of the job that
//     is handed to that worker afterwards.
//
// Standing: what verified tasks show of a worker is kept from one job to
// the next, by the worker's node ID. Then:
//
//   - When a task that two or more workers had to agree on succeeds, each
//     worker whose result was one of those that settled it has one more
//     result that agreed, and each worker that returned another has one
//     more that was outvoted. A task counts once, when it is settled: one
//     that fails because its workers cannot agree counts for nobody, and
//     neither does one that was run once and taken as it was, unless it is
//     verified after all and settled then. An attempt that fails, times
//     out or is lost is not a result and counts for nothing.
//   - A worker that is outvoted is on probation until it has returned
//     jobmodel.Probation results that agreed, counted from the last time
//     it was outvoted.
//   - A worker on probation is, in a job that asked for spot checks, taken
//     as one outvoted in that job: a task handed to it while it is on
//     probation is verified after all. A job that verifies every task
//     holds it to that already, and a job that is not verified still takes
//     its word: nothing here verifies what a job did not ask to have
//     verified.
//
// Beyond that, nothing here judges a worker: it is not scored, charged or
// shut out for what it returned, and is handed tasks like any other.

// replicated reports whether a job's tasks are each handed to several
// workers: it asked to be verified, and its tasks are not jobs, which are
// verified themselves.
func (c *Coordinator) replicated(job *jobmodel.Job) bool {
	_, steps := c.composite(job)
	return job.Verify >= 2 && !steps
}

// copiesLocked hands each task of a verified job to as many more workers as
// it wants now, as far as there are workers free that it has not asked.
func (c *Coordinator) copiesLocked(job *jobmodel.Job) {
	for _, task := range job.Tasks {
		for job.Wanted(task) > 0 {
			w := c.pickWorkerLocked(job, task)
			if w == nil {
				// Another task may still have a worker it can ask.
				break
			}
			now := time.Now()
			c.handLocked(w, assignment{job: job, task: task, attempt: job.StartCopy(task, w.id, w.name, now), started: now})
			switch owed := c.standing[w.id].Probation; {
			case job.Outvoted(w.id):
				// Its word alone is not taken again in this job.
				c.recheckLocked(job, w.id, w.name, outvotedHere)
			case owed > 0:
				// Nor is that of a worker another job found wrong, yet.
				c.recheckLocked(job, w.id, w.name, fmt.Sprintf("was outvoted in another job and has %d more %s to agree on before its word is taken alone", owed, plural(owed, "result")))
			}
		}
	}
}

// recheckLocked has the tasks a worker ran unverified, or is running so,
// verified after all, for a worker whose word alone is not taken: one that
// has been outvoted in the job, or is on probation. why says which, and is
// what the job's events say of each such task.
func (c *Coordinator) recheckLocked(job *jobmodel.Job, id, name, why string) (reopened bool) {
	for _, task := range job.Recheck(id) {
		reopened = true
		c.recordLocked(job, eventTaskRechecked, task.Index, cmp.Or(name, id),
			fmt.Sprintf("%s, so this task is now to be verified by %d workers", why, task.Verify))
	}
	return reopened
}

// outvotedHere is why a task is verified after all for a worker the job
// itself has found wrong.
const outvotedHere = "was outvoted elsewhere in the job"

// plural is a word for so many of something: itself for one, and with an s
// for any other number.
func plural(n int, word string) string {
	if n == 1 {
		return word
	}
	return word + "s"
}

// countLocked adds a task that has just succeeded to the standing of the
// workers that returned results for it: those on the side that settled it
// agreed, and the rest were outvoted. A task that one result was enough for
// was not verified, and says nothing of its worker.
func (c *Coordinator) countLocked(task *jobmodel.Task, sides [][]jobmodel.Result) {
	if task.Verify < 2 {
		return
	}
	for at, side := range sides {
		for _, r := range side {
			standing := c.standing[r.NodeID]
			standing.NodeID = r.NodeID
			if at == 0 {
				standing.Agree()
			} else {
				standing.Outvote()
			}
			c.standing[r.NodeID] = standing
			// A count that could not be saved is still held here, and is
			// written with the worker's next.
			if err := c.standings.SaveStanding(standing); err != nil {
				c.log.Warn("could not save a worker's standing; it will be lost if the coordinator stops now", "node", r.NodeID, "error", err)
			}
		}
	}
}

// shuffled returns the numbers from 0 up to n in an order nobody could have
// foretold: it is drawn from the system's source of randomness.
func shuffled(n int) []int {
	var seed [32]byte
	crand.Read(seed[:]) // never fails; see crypto/rand
	return mrand.New(mrand.NewChaCha8(seed)).Perm(n)
}

// failLocked counts an attempt as failed.
func (c *Coordinator) failLocked(a assignment, reason string, now time.Time) {
	if c.replicated(a.job) {
		a.job.FailCopy(a.task, a.attempt, reason, maxAttempts, now)
		return
	}
	a.job.Fail(a.task, reason, maxAttempts, now)
}

// requeueLocked takes an attempt back without counting it against its task.
func (c *Coordinator) requeueLocked(a assignment, reason string) {
	if c.replicated(a.job) {
		a.job.RequeueCopy(a.task, a.attempt, reason)
		return
	}
	a.job.Requeue(a.task, reason)
}

// takeBack takes back every attempt at a task that was out when an earlier
// coordinator stopped, without counting any against the task.
func (c *Coordinator) takeBack(job *jobmodel.Job, task *jobmodel.Task, reason string) {
	if !c.replicated(job) {
		job.Requeue(task, reason)
		return
	}
	for _, run := range task.History {
		if run.State == jobmodel.Running {
			job.RequeueCopy(task, run.Number, reason)
		}
	}
}

// returnedLocked takes in the result a worker returned for a task of a
// verified job, and reports whether that was the last the job was waiting
// for, so that it is ready to be combined.
func (c *Coordinator) returnedLocked(w *worker, a assignment, output []byte, stored []string) (allDone bool) {
	job, task := a.job, a.task
	// The blobs in order and each once, however the worker listed them.
	blobs := slices.Compact(slices.Sorted(slices.Values(stored)))
	allDone = job.ReturnCopy(task, a.attempt, output, blobs)
	sides := task.Sides()
	returned := jobmodel.Result{Output: output, Blobs: blobs}
	c.recordLocked(job, eventTaskResult, task.Index, w.name, "result "+short(returned))

	switch {
	case task.State == jobmodel.Succeeded:
		said := names(sides[0]) + " returned the same result"
		for _, other := range sides[1:] {
			said += "; " + names(other) + " returned another"
		}
		c.recordLocked(job, eventTaskSucceeded, task.Index, "", said)
		c.countLocked(task, sides)
		for _, other := range sides[1:] {
			for _, r := range other {
				// What else it returned in this job is no longer taken on
				// its word, which may undo tasks that had succeeded.
				if c.recheckLocked(job, r.NodeID, r.NodeName, outvotedHere) {
					allDone = false
				}
			}
		}
	case len(sides) > 1:
		c.log.Warn("workers returned different results for a task", "task", task.ID, "results", describe(sides))
		c.recordLocked(job, eventTaskDisagreed, task.Index, "", describe(sides))
		if why := c.unsettledLocked(job, task, sides); why != "" {
			job.Dispute(task, why, time.Now())
		}
	}
	return allDone
}

// unsettledLocked says why a task whose results differ can no longer be
// settled, or nothing if it still can be.
func (c *Coordinator) unsettledLocked(job *jobmodel.Job, task *jobmodel.Task, sides [][]jobmodel.Result) string {
	var why string
	switch {
	case len(sides[0])+task.MaxResults()-len(task.Results) < task.Verify:
		why = fmt.Sprintf("after %d results", len(task.Results))
	case c.unaskedLocked(job, task) < job.Wanted(task):
		why = "and no other connected worker can be asked"
	default:
		return ""
	}
	return fmt.Sprintf("task %d could not be verified: its workers returned different results, no %d of them the same, %s (%s). "+
		"Verification is for work that gives the same result every time it is run: either this workload does not, or a worker returned a wrong result",
		task.Index, task.Verify, why, describe(sides))
}

// unaskedLocked counts the connected workers that could be given a task of
// a verified job and have not been.
func (c *Coordinator) unaskedLocked(job *jobmodel.Job, task *jobmodel.Task) int {
	n := 0
	for _, w := range c.workers {
		if w.capabilities.GetTaskSlots() > 0 && suits(w.capabilities, job.Workload, job.MinMemory, job.MinGPUs, job.Needs) && !task.Asked(w.id) {
			n++
		}
	}
	return n
}

// ableLocked counts the connected workers that could be given tasks of a
// workload that ask for so much, whether or not they are busy now.
func (c *Coordinator) ableLocked(workload string, minMemory uint64, minGPUs int, needs []string) int {
	n := 0
	for _, w := range c.workers {
		if w.capabilities.GetTaskSlots() > 0 && suits(w.capabilities, workload, minMemory, minGPUs, needs) {
			n++
		}
	}
	return n
}

// short is the beginning of a result's digest: enough to tell the results
// of one task apart.
func short(r jobmodel.Result) string {
	return r.Digest()[:12]
}

// names lists the workers that returned the results of one side.
func names(side []jobmodel.Result) string {
	workers := make([]string, 0, len(side))
	for _, r := range side {
		// A worker that gave itself no label goes by its ID.
		workers = append(workers, cmp.Or(r.NodeName, r.NodeID))
	}
	return strings.Join(workers, ", ")
}

// describe says which workers returned which result, side by side.
func describe(sides [][]jobmodel.Result) string {
	said := make([]string, 0, len(sides))
	for _, side := range sides {
		said = append(said, names(side)+" returned "+short(side[0]))
	}
	return strings.Join(said, "; ")
}

// Package accounting totals what a pool's machines did and what its
// workloads cost, over a span of time, from the jobs a coordinator holds.
//
// Its unit is the accepted task (see docs/research/token-economics.md): a
// task the coordinator took a result for, and the basis it took it on. Each
// basis is something the coordinator saw for itself, never a worker's own
// word:
//
//   - Agreement: two or more workers returned the same result;
//   - Audit: the coordinator ran the task again itself and got the same;
//   - Unchecked: one worker's result was taken as it came, because the job
//     asked for no check, or the audit could not be run;
//   - Caught: the coordinator ran the task again, got another result, and
//     took its own. No worker is credited with the task.
//
// Every figure here is one a job's record (packages/jobrecord) also holds:
// the workload, each task's attempts and who made them, which results
// agreed, what audits found, and when the job was submitted and ended. So
// the totals can be checked against the records, and they hold no weight,
// price or measured duration: a weighting is a separate policy, applied to
// these counts afterwards. The one span of time here, a workload's wall
// time, runs from a job's submission to its end on the coordinator's
// clock, queueing included; it says how long the pool took, not how hard
// any machine worked.
//
// Figures that do not come from jobs, such as the time a machine spent
// mining and what its power is estimated to have cost (#776), belong to
// Machine beside those that do, and come in through Input beside Jobs, so
// that adding them changes no caller.
package accounting

import (
	"cmp"
	"slices"
	"strings"
	"time"

	jobmodel "github.com/sisyphus-network/Sisyphus/packages/job-model"
)

// Basis is how the coordinator came to accept a task's result.
type Basis string

const (
	Agreement Basis = "agreement"
	Audit     Basis = "audit"
	Unchecked Basis = "unchecked"
	Caught    Basis = "caught"
)

// Worker is a machine, by its node ID and the name it gave itself.
type Worker struct {
	ID, Name string
}

// Accepted is a task the coordinator accepted a result for: the accounting
// event.
type Accepted struct {
	Job      string
	Workload string
	Task     int
	Basis    Basis
	// Workers are those whose result was accepted: every worker that
	// agreed, for Agreement; the one worker, for Audit and Unchecked; none,
	// for Caught.
	Workers []Worker
	// At is when the job ended.
	At time.Time
}

// Events returns the accepted tasks of a job that is over, in task order.
// A job that is not over has none yet, and nor does a task that did not
// succeed.
func Events(job *jobmodel.Job) []Accepted {
	if !job.Terminal() {
		return nil
	}
	var events []Accepted
	for _, task := range job.Tasks {
		if task.State != jobmodel.Succeeded {
			continue
		}
		event := Accepted{Job: job.ID, Workload: job.Workload, Task: task.Index, At: job.FinishedAt}
		agreed := task.Agreed()
		switch {
		case task.Audit == jobmodel.AuditCaught:
			event.Basis = Caught
		case len(agreed) >= 2:
			event.Basis = Agreement
			for _, r := range agreed {
				event.Workers = append(event.Workers, Worker{r.NodeID, r.NodeName})
			}
		case task.Audit == jobmodel.AuditMatched:
			event.Basis, event.Workers = Audit, []Worker{{task.NodeID, task.NodeName}}
		default:
			event.Basis, event.Workers = Unchecked, []Worker{{task.NodeID, task.NodeName}}
		}
		events = append(events, event)
	}
	return events
}

// Range is a span of time: from From, and before To. A bound that is zero
// is no bound.
type Range struct {
	From, To time.Time
}

// Contains reports whether t is in the range.
func (r Range) Contains(t time.Time) bool {
	return (r.From.IsZero() || !t.Before(r.From)) && (r.To.IsZero() || t.Before(r.To))
}

// Counts are a machine's or a workload's accepted tasks, by basis.
type Counts struct {
	Agreement, Audit, Unchecked, Caught int
}

// Accepted is how many tasks were accepted with a worker's result: every
// basis but Caught.
func (c Counts) Accepted() int {
	return c.Agreement + c.Audit + c.Unchecked
}

func (c *Counts) add(basis Basis) {
	switch basis {
	case Agreement:
		c.Agreement++
	case Audit:
		c.Audit++
	case Unchecked:
		c.Unchecked++
	case Caught:
		c.Caught++
	}
}

// Machine is what one machine did.
type Machine struct {
	Worker
	// Jobs is how many jobs it was handed a task of.
	Jobs int
	// Attempts is how many times it was handed a task, and FailedAttempts
	// how many of those it failed.
	Attempts, FailedAttempts int
	// Counts are the tasks accepted with its result, by basis; an accepted
	// task that several machines agreed on counts for each of them. Caught
	// counts the tasks an audit took another result for than its own.
	Counts
	// Outvoted is how many results it returned for a verified task that
	// were not among those the task was accepted with.
	Outvoted int
}

// Workload is what was done of one workload.
type Workload struct {
	Name string
	// Jobs is how many of its jobs ended, and Succeeded, Failed and
	// Cancelled how each of those ended.
	Jobs, Succeeded, Failed, Cancelled int
	// Tasks is how many tasks those jobs had, and Attempts and
	// FailedAttempts how many times one was handed to a worker and how many
	// of those failed.
	Tasks, Attempts, FailedAttempts int
	// Counts are its accepted tasks, by basis, each counted once however
	// many machines agreed on it.
	Counts
	// Machines is how many machines were handed a task of it.
	Machines int
	// WallTime is the sum, over its jobs, of the time from submission to
	// end.
	WallTime time.Duration
}

// Input is what a summary is made from.
type Input struct {
	// Jobs are the jobs a coordinator holds. Only those that are over and
	// ended in Range are counted.
	Jobs  []*jobmodel.Job
	Range Range
	// Composite reports whether a job's tasks are other jobs, carried out
	// by the coordinator. Such a job counts as a job, but its tasks do not:
	// the jobs that carried them out are counted for themselves. Nil means
	// no job is.
	Composite func(*jobmodel.Job) bool
}

// Summary is the totals over a range.
type Summary struct {
	Range Range
	// Jobs is how many jobs ended in the range.
	Jobs int
	// Machines are by accepted tasks, most first, then by name and ID.
	Machines []Machine
	// Workloads are by name.
	Workloads []Workload
}

// Summarize totals the jobs of in that ended in its range. It reads them
// and changes nothing, and the same jobs always give the same summary.
func Summarize(in Input) Summary {
	summary := Summary{Range: in.Range}
	machines := map[string]*Machine{}
	workloads := map[string]*Workload{}
	// The machines handed a task of each workload, by node ID, so that one
	// handed tasks of it in several jobs counts once.
	ran := map[string]map[string]bool{}
	// The name a machine last gave itself is the one it goes by, so the
	// jobs are taken in the order they ended.
	jobs := slices.Clone(in.Jobs)
	slices.SortStableFunc(jobs, func(a, b *jobmodel.Job) int {
		return cmp.Or(a.FinishedAt.Compare(b.FinishedAt), strings.Compare(a.ID, b.ID))
	})
	machine := func(id, name string) *Machine {
		m, ok := machines[id]
		if !ok {
			m = &Machine{Worker: Worker{ID: id}}
			machines[id] = m
		}
		if name != "" {
			m.Name = name
		}
		return m
	}
	for _, job := range jobs {
		if !job.Terminal() || !in.Range.Contains(job.FinishedAt) {
			continue
		}
		summary.Jobs++
		w, ok := workloads[job.Workload]
		if !ok {
			w = &Workload{Name: job.Workload}
			workloads[job.Workload] = w
			ran[job.Workload] = map[string]bool{}
		}
		w.Jobs++
		switch job.State {
		case jobmodel.Succeeded:
			w.Succeeded++
		case jobmodel.Failed:
			w.Failed++
		default:
			w.Cancelled++
		}
		w.WallTime += max(job.FinishedAt.Sub(job.CreatedAt), 0)
		if in.Composite != nil && in.Composite(job) {
			continue
		}
		w.Tasks += len(job.Tasks)
		touched := map[string]bool{}
		for _, task := range job.Tasks {
			for _, attempt := range task.History {
				// An attempt restored with no worker named, from before
				// attempts were kept one by one, is no machine's.
				if attempt.NodeID == "" {
					continue
				}
				m := machine(attempt.NodeID, attempt.NodeName)
				m.Attempts++
				w.Attempts++
				if attempt.State == jobmodel.Failed {
					m.FailedAttempts++
					w.FailedAttempts++
				}
				touched[attempt.NodeID] = true
				ran[job.Workload][attempt.NodeID] = true
			}
			// The worker whose result an audit took another for is the one
			// the task was with.
			if task.State == jobmodel.Succeeded && task.Audit == jobmodel.AuditCaught {
				machine(task.NodeID, task.NodeName).Caught++
			}
			if agreed := task.Agreed(); len(agreed) > 0 {
				for _, r := range task.Results {
					if !slices.ContainsFunc(agreed, func(a jobmodel.Result) bool { return a.Attempt == r.Attempt }) {
						machine(r.NodeID, r.NodeName).Outvoted++
					}
				}
			}
		}
		for id := range touched {
			machines[id].Jobs++
		}
		for _, event := range Events(job) {
			w.add(event.Basis)
			for _, worker := range event.Workers {
				machine(worker.ID, worker.Name).add(event.Basis)
			}
		}
	}
	for _, m := range machines {
		summary.Machines = append(summary.Machines, *m)
	}
	slices.SortFunc(summary.Machines, func(a, b Machine) int {
		return cmp.Or(cmp.Compare(b.Accepted(), a.Accepted()), strings.Compare(a.Name, b.Name), strings.Compare(a.ID, b.ID))
	})
	for name, w := range workloads {
		w.Machines = len(ran[name])
		summary.Workloads = append(summary.Workloads, *w)
	}
	slices.SortFunc(summary.Workloads, func(a, b Workload) int { return strings.Compare(a.Name, b.Name) })
	return summary
}

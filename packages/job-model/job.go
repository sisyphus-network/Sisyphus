// Package jobmodel holds the job and task state machines. It does no I/O and
// no locking; the coordinator owns concurrency.
package jobmodel

import (
	"fmt"
	"time"
)

type State int

const (
	Pending State = iota
	Running
	Succeeded
	Failed
	// Cancelled is a job stopped on request, or a task given up because its
	// job ended without it.
	Cancelled
)

func (s State) String() string {
	switch s {
	case Pending:
		return "pending"
	case Running:
		return "running"
	case Succeeded:
		return "succeeded"
	case Failed:
		return "failed"
	case Cancelled:
		return "cancelled"
	}
	return fmt.Sprintf("State(%d)", int(s))
}

type Mode int

const (
	// Distributed splits a job into tasks spread across workers.
	Distributed Mode = iota
	// FullWorker sends the whole job to one worker as a single task.
	FullWorker
)

type Task struct {
	ID      string
	Index   int
	Payload []byte
	State   State
	// Attempt counts how many times the task has been handed to a worker,
	// and Failures how many of those attempts failed. They differ by the
	// attempt in progress and by any that were lost; see Requeue.
	Attempt  int
	Failures int
	// NodeID is the worker currently or most recently running the task, and
	// NodeName the label that worker gave itself.
	NodeID   string
	NodeName string
	Output   []byte
	// Err is the reason for the most recent failed attempt.
	Err string
	// History is every attempt at the task so far, in order, each as it
	// stood when the task moved on from it.
	History []Attempt
	// Results are what workers have returned for the task so far, in the
	// order of their attempts, if its job is verified; see Job.Verify. The
	// task's output is the one enough of them agree on.
	Results []Result
	// Verify is how many of those results must be the same for the task to
	// have succeeded: as many as its job asks for, or one for a task the
	// job's spot checks passed over; see Job.Check. It is zero in a job
	// that is not verified.
	Verify int
	// Progress is how far along the current attempt says it is, from 0 to
	// 1, and StartedAt when that attempt was handed out. Neither is kept
	// across a restart: an attempt in progress then is lost anyway.
	Progress  float64
	StartedAt time.Time
}

type Job struct {
	ID       string
	Workload string
	Params   []byte
	Mode     Mode
	// Key, if set, is the key the job's blobs are sealed with. It is a
	// secret and is not part of the job's public state.
	Key []byte
	// Private says the job was submitted with a key. It outlasts the key,
	// which is dropped when the job ends.
	Private bool
	// Commitments are keyed hashes of the values a private job's record
	// leaves out, by where in the record each is. They are worked out when
	// the job ends, while it still has its key, and kept with it; see
	// packages/jobrecord.
	Commitments map[string][]byte
	// MaxTasks is the split the submitter asked for; zero means unspecified.
	MaxTasks int
	// TaskTimeout, if not zero, is how long one attempt at a task may run.
	TaskTimeout time.Duration
	// Verify, if two or more, is how many different workers must return
	// the same result for each task before the task has succeeded. Each
	// task is then handed to that many at once, and to more if they
	// differ; see verify.go. A job whose tasks are jobs hands it on to
	// them instead.
	Verify int
	// VerifyShare, if between zero and one, is the share of the job's tasks
	// that were picked to be verified when it was submitted. The others are
	// run once, unless what a worker returns elsewhere gives cause to
	// verify them after all; see Check and Recheck.
	VerifyShare float64
	// outvoted is the workers that have returned, for a task of the job
	// that has succeeded, a result other than the one it succeeded with. It
	// is worked out from the tasks when first asked for.
	outvoted map[string]bool
	// MinMemory and MinGPUs are what a worker must have to be given the
	// job's tasks: bytes of memory, and graphics cards. Zero asks nothing.
	MinMemory uint64
	MinGPUs   int
	// Parent and Step are set on a job that is a step of another: the ID
	// of the job it is a step of, and the step's name.
	Parent, Step string
	// Submitter is the ID of the node whose key the job was submitted
	// with. It is empty for a job the node gave itself, through its local
	// API or its planner.
	Submitter string
	// Record is the CID of the job's record, set once the job is over and
	// its record has been worked out; see packages/jobrecord.
	Record string
	// Needs is what else a worker must have, as labels. It follows from
	// the workload and the parameters and is worked out by whoever holds
	// the job, not saved with it.
	Needs      []string
	State      State
	Tasks      []*Task
	Result     []byte
	Err        string
	CreatedAt  time.Time
	FinishedAt time.Time

	// The stored blobs the job has touched so far, by CID.
	read         map[string]struct{} // opened by the split, a task or the aggregation
	intermediate map[string]struct{} // stored by tasks
	outputs      map[string]struct{} // stored by the aggregation

	// What has changed since the job was last saved; see Unsaved.
	unsaved Changes
	touched map[int]struct{} // indexes of the tasks in unsaved
}

// New creates a pending job with one pending task per payload.
func New(id, workload string, params []byte, mode Mode, maxTasks int, payloads [][]byte, now time.Time) *Job {
	j := &Job{ID: id, Workload: workload, Params: params, Mode: mode, MaxTasks: maxTasks, CreatedAt: now}
	j.init()
	j.unsaved.New = true
	for i, payload := range payloads {
		t := &Task{ID: TaskID(id, i), Index: i, Payload: payload}
		j.Tasks = append(j.Tasks, t)
		j.touch(t)
	}
	return j
}

// TaskID is the ID of the task at index in the job with the given ID.
func TaskID(jobID string, index int) string {
	return fmt.Sprintf("%s/%d", jobID, index)
}

func (j *Job) init() {
	j.read, j.intermediate, j.outputs = make(map[string]struct{}), make(map[string]struct{}), make(map[string]struct{})
	j.touched = make(map[int]struct{})
}

// Terminal reports whether the job is over: succeeded, failed or cancelled.
func (j *Job) Terminal() bool {
	return j.State == Succeeded || j.State == Failed || j.State == Cancelled
}

// Assignable returns the tasks waiting for a worker. A finished job has none.
func (j *Job) Assignable() []*Task {
	if j.Terminal() {
		return nil
	}
	var tasks []*Task
	for _, t := range j.Tasks {
		if t.State == Pending {
			tasks = append(tasks, t)
		}
	}
	return tasks
}

// Start records that a pending task was handed to a worker.
func (j *Job) Start(t *Task, nodeID, nodeName string, now time.Time) {
	t.State = Running
	t.Progress, t.StartedAt = 0, now
	t.Attempt++
	t.NodeID = nodeID
	t.NodeName = nodeName
	j.touch(t)
	if j.State == Pending {
		j.State = Running
	}
}

// Succeed records a task's output and reports whether every task is now done,
// meaning the job is ready to be aggregated and finished.
func (j *Job) Succeed(t *Task, output []byte) (allDone bool) {
	t.State = Succeeded
	t.Progress = 1
	t.Output = output
	t.Err = ""
	j.touch(t)
	for _, other := range j.Tasks {
		if other.State != Succeeded {
			return false
		}
	}
	return true
}

// Fail records a failed attempt. The task goes back to pending unless
// maxAttempts of its attempts have now failed, in which case it fails and
// takes the job with it.
func (j *Job) Fail(t *Task, reason string, maxAttempts int, now time.Time) {
	t.Err = reason
	t.Failures++
	if t.Failures < maxAttempts {
		t.State = Pending
		j.touch(t)
		return
	}
	t.State = Failed
	j.touch(t)
	j.Finish(nil, fmt.Errorf("task %d failed after %d attempts: %s", t.Index, t.Failures, reason), now)
}

// Requeue puts a running task back to pending without counting the attempt
// against it, for when the attempt was lost through no fault of the task or
// its worker.
func (j *Job) Requeue(t *Task, reason string) {
	t.State = Pending
	t.Err = reason
	j.touch(t)
}

// Cancel stops a job that is not over, and reports whether it did.
func (j *Job) Cancel(now time.Time) bool {
	if j.Terminal() {
		return false
	}
	j.State, j.FinishedAt, j.Err = Cancelled, now, "cancelled"
	j.abandon()
	return true
}

// abandon gives up the tasks of a job that has ended without them.
func (j *Job) abandon() {
	for _, t := range j.Tasks {
		if t.State != Pending && t.State != Running {
			continue
		}
		t.State = Cancelled
		if j.Verify >= 2 {
			// Each of its copies that is out, not only the latest.
			t.drop()
			j.mark(t)
			continue
		}
		j.touch(t)
	}
}

// Finish moves the job to a terminal state. A nil err means success.
func (j *Job) Finish(result []byte, err error, now time.Time) {
	j.FinishedAt = now
	if err != nil {
		j.State = Failed
		j.Err = err.Error()
		j.abandon()
		return
	}
	j.State = Succeeded
	j.Result = result
}

// Outputs returns task outputs in task order.
func (j *Job) Outputs() [][]byte {
	outputs := make([][]byte, len(j.Tasks))
	for i, t := range j.Tasks {
		outputs[i] = t.Output
	}
	return outputs
}

// Attempt is one time a task was handed to a worker.
type Attempt struct {
	TaskIndex int
	// Number counts a task's attempts from one.
	Number   int
	NodeID   string
	NodeName string
	// State is how the attempt stands: running, succeeded, pending if it
	// was lost or failed and the task went back to wait for another, failed
	// if it was the task's last, or cancelled if the job ended without it.
	State State
	Err   string
}

// Event is one thing that happened to a job: a step in its life, or a line
// one of its tasks logged.
type Event struct {
	// Seq numbers a job's events from one.
	Seq  uint64
	At   time.Time
	Kind string
	// Task is the index of the task concerned, or -1 for the job itself.
	Task int
	// Node is the label of the worker concerned, if one was.
	Node string
	Text string
}

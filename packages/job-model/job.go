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
	// MaxTasks is the split the submitter asked for; zero means unspecified.
	MaxTasks int
	// TaskTimeout, if not zero, is how long one attempt at a task may run.
	TaskTimeout time.Duration
	// MinMemory and MinGPUs are what a worker must have to be given the
	// job's tasks: bytes of memory, and graphics cards. Zero asks nothing.
	MinMemory  uint64
	MinGPUs    int
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
	j.touch(t)
	if t.Failures < maxAttempts {
		t.State = Pending
		return
	}
	t.State = Failed
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
		if t.State == Pending || t.State == Running {
			t.State = Cancelled
			j.touch(t)
		}
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

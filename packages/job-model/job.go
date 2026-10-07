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
	// Attempt counts how many times the task has been handed to a worker.
	Attempt int
	// NodeID is the worker currently or most recently running the task.
	NodeID string
	Output []byte
	// Err is the reason for the most recent failed attempt.
	Err string
}

type Job struct {
	ID       string
	Workload string
	Params   []byte
	Mode     Mode
	// MaxTasks is the split the submitter asked for; zero means unspecified.
	MaxTasks   int
	State      State
	Tasks      []*Task
	Result     []byte
	Err        string
	CreatedAt  time.Time
	FinishedAt time.Time
}

// New creates a pending job with one pending task per payload.
func New(id, workload string, params []byte, mode Mode, maxTasks int, payloads [][]byte, now time.Time) *Job {
	j := &Job{ID: id, Workload: workload, Params: params, Mode: mode, MaxTasks: maxTasks, CreatedAt: now}
	for i, payload := range payloads {
		j.Tasks = append(j.Tasks, &Task{ID: fmt.Sprintf("%s/%d", id, i), Index: i, Payload: payload})
	}
	return j
}

// Terminal reports whether the job has finished, successfully or not.
func (j *Job) Terminal() bool {
	return j.State == Succeeded || j.State == Failed
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
func (j *Job) Start(t *Task, nodeID string) {
	t.State = Running
	t.Attempt++
	t.NodeID = nodeID
	if j.State == Pending {
		j.State = Running
	}
}

// Succeed records a task's output and reports whether every task is now done,
// meaning the job is ready to be aggregated and finished.
func (j *Job) Succeed(t *Task, output []byte) (allDone bool) {
	t.State = Succeeded
	t.Output = output
	t.Err = ""
	for _, other := range j.Tasks {
		if other.State != Succeeded {
			return false
		}
	}
	return true
}

// Fail records a failed attempt. The task goes back to pending unless it has
// used up maxAttempts, in which case it fails and takes the job with it.
func (j *Job) Fail(t *Task, reason string, maxAttempts int, now time.Time) {
	t.Err = reason
	if t.Attempt < maxAttempts {
		t.State = Pending
		return
	}
	t.State = Failed
	j.Finish(nil, fmt.Errorf("task %d failed after %d attempts: %s", t.Index, t.Attempt, reason), now)
}

// Finish moves the job to a terminal state. A nil err means success.
func (j *Job) Finish(result []byte, err error, now time.Time) {
	j.FinishedAt = now
	if err != nil {
		j.State = Failed
		j.Err = err.Error()
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

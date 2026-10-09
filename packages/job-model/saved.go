package jobmodel

import "sort"

// A job keeps count of what has changed since it was last saved, so that
// whoever keeps jobs on disk can write a change as small as the change was
// rather than the whole job, which may have thousands of tasks.

// Changes is what a job has that its saved copy lacks. The job's own fields,
// such as its state and result, are few and are not tracked: assume they
// have changed.
type Changes struct {
	// New is set if the job has never been saved.
	New bool
	// Tasks are the tasks that have changed, in index order.
	Tasks []*Task
	// Read, TaskOutputs and Results are the blobs noted since, as NoteRead,
	// NoteTaskOutput and NoteResult were told of them.
	Read, TaskOutputs, Results []string
}

// Unsaved returns what has changed since the job was last saved.
func (j *Job) Unsaved() Changes {
	changes := j.unsaved
	changes.Tasks = make([]*Task, 0, len(j.touched))
	for index := range j.touched {
		changes.Tasks = append(changes.Tasks, j.Tasks[index])
	}
	sort.Slice(changes.Tasks, func(a, b int) bool { return changes.Tasks[a].Index < changes.Tasks[b].Index })
	return changes
}

// MarkSaved records that everything Unsaved last returned has been saved.
// Nothing may change the job between the two calls.
func (j *Job) MarkSaved() {
	j.unsaved = Changes{}
	clear(j.touched)
}

// touch notes that a task has changed, and brings the last entry of its
// history up to date with it.
func (j *Job) touch(t *Task) {
	j.mark(t)
	if t.Attempt == 0 {
		return // never handed to anyone yet
	}
	for len(t.History) < t.Attempt {
		t.History = append(t.History, Attempt{TaskIndex: t.Index, Number: len(t.History) + 1})
	}
	t.History[t.Attempt-1] = Attempt{TaskIndex: t.Index, Number: t.Attempt, NodeID: t.NodeID, NodeName: t.NodeName, State: t.State, Err: t.Err}
}

// mark notes that a task has changed, and leaves its history as it is: the
// history of a task that several workers have at once is kept attempt by
// attempt; see verify.go.
func (j *Job) mark(t *Task) {
	j.touched[t.Index] = struct{}{}
}

// Restore rebuilds a job from its saved copy: its fields and tasks as they
// were, and the blobs it had noted. The job it returns has nothing unsaved.
func Restore(saved Job, read, taskOutputs, results []string) *Job {
	j := &saved
	j.init()
	// Who was outvoted is worked out again from the tasks.
	j.outvoted = nil
	// Progress is not saved, but a task that succeeded is known to be done.
	for _, t := range j.Tasks {
		if t.State == Succeeded {
			t.Progress = 1
		}
	}
	j.NoteRead(read...)
	j.NoteTaskOutput(taskOutputs...)
	j.NoteResult(results...)
	j.MarkSaved()
	return j
}

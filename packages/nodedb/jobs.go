package nodedb

import (
	"database/sql"
	"fmt"
	"time"

	jobmodel "github.com/sisyphus-network/Sisyphus/packages/job-model"
)

// The roles a blob can have in a job, as job_blobs records them.
const (
	blobRead = iota + 1
	blobTaskOutput
	blobResult
)

// SaveJob writes what has changed in a job since it was last saved, all of
// it or none. A job's arrival and its finish are synced to disk before
// SaveJob returns; the steps in between are not, and the last of them may be
// lost if the machine fails, in which case those tasks run again.
func (db *DB) SaveJob(job *jobmodel.Job) error {
	changes := job.Unsaved()
	// A finished job has no more use for its key, so it is not kept.
	key := job.Key
	if job.Terminal() {
		key = nil
	}
	err := db.write(func(b *batch) {
		b.exec(`INSERT INTO jobs (job_id, workload, params, mode, max_tasks, state, result, error, sealing_key, created_at_ns, finished_at_ns, task_timeout_ns, min_memory_bytes, min_gpus)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT (job_id) DO UPDATE SET state = excluded.state, result = excluded.result, error = excluded.error,
				sealing_key = excluded.sealing_key, finished_at_ns = excluded.finished_at_ns`,
			job.ID, job.Workload, blob(job.Params), int(job.Mode), job.MaxTasks, int(job.State), blob(job.Result), job.Err,
			key, nanos(job.CreatedAt), nanos(job.FinishedAt), int64(job.TaskTimeout), int64(job.MinMemory), job.MinGPUs)
		for _, t := range changes.Tasks {
			b.exec(`INSERT INTO tasks (job_id, task_index, payload, state, attempt, failures, node_id, node_name, output, error)
				VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
				ON CONFLICT (job_id, task_index) DO UPDATE SET state = excluded.state, attempt = excluded.attempt,
					failures = excluded.failures, node_id = excluded.node_id, node_name = excluded.node_name,
					output = excluded.output, error = excluded.error`,
				job.ID, t.Index, blob(t.Payload), int(t.State), t.Attempt, t.Failures, t.NodeID, t.NodeName, blob(t.Output), t.Err)
			if t.Attempt == 0 {
				continue // never handed to anyone yet
			}
			b.exec(`INSERT INTO task_attempts (job_id, task_index, attempt, node_id, node_name, state, error)
				VALUES (?, ?, ?, ?, ?, ?, ?)
				ON CONFLICT (job_id, task_index, attempt) DO UPDATE SET state = excluded.state, error = excluded.error`,
				job.ID, t.Index, t.Attempt, t.NodeID, t.NodeName, int(t.State), t.Err)
		}
		for role, cids := range map[int][]string{blobRead: changes.Read, blobTaskOutput: changes.TaskOutputs, blobResult: changes.Results} {
			for _, c := range cids {
				b.exec(`INSERT OR IGNORE INTO job_blobs (job_id, role, cid) VALUES (?, ?, ?)`, job.ID, role, c)
			}
		}
	})
	if err != nil {
		return fmt.Errorf("save job %s: %w", job.ID, err)
	}
	job.MarkSaved()
	if changes.New || job.Terminal() {
		return db.sync()
	}
	return nil
}

// LoadJobs returns every saved job, in the order they were first saved.
func (db *DB) LoadJobs() ([]*jobmodel.Job, error) {
	var saved []*jobmodel.Job
	byID := make(map[string]*jobmodel.Job)
	err := db.read(func(rows *sql.Rows) error {
		job := new(jobmodel.Job)
		var created, finished int64
		err := rows.Scan(&job.ID, &job.Workload, &job.Params, &job.Mode, &job.MaxTasks, &job.State, &job.Result, &job.Err,
			&job.Key, &created, &finished, &job.TaskTimeout, &job.MinMemory, &job.MinGPUs)
		if err != nil {
			return err
		}
		job.CreatedAt, job.FinishedAt = moment(created), moment(finished)
		saved, byID[job.ID] = append(saved, job), job
		return nil
	}, `SELECT job_id, workload, params, mode, max_tasks, state, result, error, sealing_key, created_at_ns, finished_at_ns, task_timeout_ns, min_memory_bytes, min_gpus
		FROM jobs ORDER BY seq`)
	if err != nil {
		return nil, fmt.Errorf("load jobs: %w", err)
	}

	err = db.read(func(rows *sql.Rows) error {
		var jobID string
		t := new(jobmodel.Task)
		if err := rows.Scan(&jobID, &t.Index, &t.Payload, &t.State, &t.Attempt, &t.Failures, &t.NodeID, &t.NodeName, &t.Output, &t.Err); err != nil {
			return err
		}
		t.ID = jobmodel.TaskID(jobID, t.Index)
		byID[jobID].Tasks = append(byID[jobID].Tasks, t)
		return nil
	}, `SELECT job_id, task_index, payload, state, attempt, failures, node_id, node_name, output, error
		FROM tasks WHERE job_id IN (SELECT job_id FROM jobs) ORDER BY job_id, task_index`)
	if err != nil {
		return nil, fmt.Errorf("load tasks: %w", err)
	}

	blobs := make(map[string]map[int][]string)
	err = db.read(func(rows *sql.Rows) error {
		var jobID, c string
		var role int
		if err := rows.Scan(&jobID, &role, &c); err != nil {
			return err
		}
		if blobs[jobID] == nil {
			blobs[jobID] = make(map[int][]string)
		}
		blobs[jobID][role] = append(blobs[jobID][role], c)
		return nil
	}, `SELECT job_id, role, cid FROM job_blobs`)
	if err != nil {
		return nil, fmt.Errorf("load job blobs: %w", err)
	}

	jobs := make([]*jobmodel.Job, 0, len(saved))
	for _, job := range saved {
		touched := blobs[job.ID]
		jobs = append(jobs, jobmodel.Restore(*job, touched[blobRead], touched[blobTaskOutput], touched[blobResult]))
	}
	return jobs, nil
}

// SaveEvent records one thing that happened to a job. Events are not synced
// to disk one by one; they are safe once the job's next step is.
func (db *DB) SaveEvent(jobID string, e jobmodel.Event) error {
	err := db.write(func(b *batch) {
		b.exec(`INSERT OR REPLACE INTO job_events (job_id, seq, at_ns, kind, task_index, node_name, text) VALUES (?, ?, ?, ?, ?, ?, ?)`,
			jobID, int64(e.Seq), nanos(e.At), e.Kind, e.Task, e.Node, e.Text)
	})
	if err != nil {
		return fmt.Errorf("save job event: %w", err)
	}
	return nil
}

// LoadEvents returns what has happened to a job, in order.
func (db *DB) LoadEvents(jobID string) ([]jobmodel.Event, error) {
	var events []jobmodel.Event
	err := db.read(func(rows *sql.Rows) error {
		var e jobmodel.Event
		var at int64
		if err := rows.Scan(&e.Seq, &at, &e.Kind, &e.Task, &e.Node, &e.Text); err != nil {
			return err
		}
		e.At = moment(at)
		events = append(events, e)
		return nil
	}, `SELECT seq, at_ns, kind, task_index, node_name, text FROM job_events WHERE job_id = ? ORDER BY seq`, jobID)
	if err != nil {
		return nil, fmt.Errorf("load job events: %w", err)
	}
	return events, nil
}

// DeleteJobs forgets jobs, with their tasks, the attempts at them and the
// record of the blobs they touched.
func (db *DB) DeleteJobs(ids []string) error {
	err := db.write(func(b *batch) {
		for _, id := range ids {
			b.exec(`DELETE FROM jobs WHERE job_id = ?`, id)
		}
	})
	if err != nil {
		return fmt.Errorf("delete jobs: %w", err)
	}
	return nil
}

// Attempt is one time a task was handed to a worker.
type Attempt struct {
	TaskIndex int
	// Number counts a task's attempts from one.
	Number   int
	NodeID   string
	NodeName string
	// State is how the attempt stands: running, succeeded, or pending if it
	// was lost or failed and the task went back to wait for another.
	State jobmodel.State
	Err   string
}

// Attempts returns every attempt at every task of a job, by task and then in
// the order they were made.
func (db *DB) Attempts(jobID string) ([]Attempt, error) {
	var attempts []Attempt
	err := db.read(func(rows *sql.Rows) error {
		var a Attempt
		if err := rows.Scan(&a.TaskIndex, &a.Number, &a.NodeID, &a.NodeName, &a.State, &a.Err); err != nil {
			return err
		}
		attempts = append(attempts, a)
		return nil
	}, `SELECT task_index, attempt, node_id, node_name, state, error FROM task_attempts WHERE job_id = ? ORDER BY task_index, attempt`, jobID)
	if err != nil {
		return nil, fmt.Errorf("load attempts: %w", err)
	}
	return attempts, nil
}

// blob is b as a column that is never NULL.
func blob(b []byte) []byte {
	if b == nil {
		return []byte{}
	}
	return b
}

// nanos is t as a column: nanoseconds since the Unix epoch, or zero for no
// time at all.
func nanos(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UnixNano()
}

func moment(nanos int64) time.Time {
	if nanos == 0 {
		return time.Time{}
	}
	return time.Unix(0, nanos)
}

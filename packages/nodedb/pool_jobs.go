package nodedb

import (
	"database/sql"
	"fmt"
	"time"
)

// PoolJobs returns the IDs of the jobs this node submitted to the pool the
// coordinator with the given ID runs, newest first.
func (db *DB) PoolJobs(coordinatorID string) ([]string, error) {
	var ids []string
	err := db.read(func(rows *sql.Rows) error {
		var id string
		err := rows.Scan(&id)
		ids = append(ids, id)
		return err
	}, `SELECT job_id FROM pool_jobs WHERE coordinator_id = ? ORDER BY submitted_ns DESC, job_id`, coordinatorID)
	if err != nil {
		return nil, fmt.Errorf("load the jobs submitted to the pool: %w", err)
	}
	return ids, nil
}

// AddPoolJob records that this node submitted the job with the given ID to
// the pool the coordinator with the given ID runs.
func (db *DB) AddPoolJob(coordinatorID, jobID string, now time.Time) error {
	return db.durably("save a job submitted to the pool", func(b *batch) {
		b.exec(`INSERT OR IGNORE INTO pool_jobs (job_id, coordinator_id, submitted_ns) VALUES (?, ?, ?)`, jobID, coordinatorID, nanos(now))
	})
}

// DropPoolJob forgets a job submitted to a pool, as when the pool no
// longer has it.
func (db *DB) DropPoolJob(jobID string) error {
	return db.durably("forget a job submitted to the pool", func(b *batch) {
		b.exec(`DELETE FROM pool_jobs WHERE job_id = ?`, jobID)
	})
}

package nodedb

import (
	"database/sql"
	"errors"
	"fmt"
	"math"
	"time"
)

// PoolLimits are what the node, as a coordinator, lets one member use. 0
// means no limit in each field.
type PoolLimits struct {
	MaxJobsPerClient  int
	MaxTasksPerClient int
	// MaxTaskTimeout is kept in whole seconds; less than a second is
	// dropped.
	MaxTaskTimeout           time.Duration
	MaxStoreBytesPerClient   uint64
	MaxUploadBytesPerAttempt uint64
}

// PoolLimits returns saved limits and whether the owner has saved any. No
// row means the daemon's startup flags provide the defaults.
func (db *DB) PoolLimits() (PoolLimits, bool, error) {
	var limits PoolLimits
	var timeout, store, upload int64
	err := db.sql.QueryRow(`SELECT max_jobs_per_client, max_tasks_per_client, max_task_timeout_seconds,
			max_store_bytes_per_client, max_upload_bytes_per_attempt FROM pool_limits WHERE singleton_id = 1`).
		Scan(&limits.MaxJobsPerClient, &limits.MaxTasksPerClient, &timeout, &store, &upload)
	if errors.Is(err, sql.ErrNoRows) {
		return PoolLimits{}, false, nil
	}
	if err != nil {
		return PoolLimits{}, false, fmt.Errorf("load pool limits: %w", err)
	}
	limits.MaxTaskTimeout = time.Duration(timeout) * time.Second
	limits.MaxStoreBytesPerClient, limits.MaxUploadBytesPerAttempt = uint64(store), uint64(upload)
	return limits, true, nil
}

// SetPoolLimits replaces the node's saved pool limits. SQLite keeps signed
// integers, so a byte limit past math.MaxInt64 cannot be saved.
func (db *DB) SetPoolLimits(limits PoolLimits) error {
	if limits.MaxStoreBytesPerClient > math.MaxInt64 || limits.MaxUploadBytesPerAttempt > math.MaxInt64 {
		return fmt.Errorf("save pool limits: a byte limit may be at most %d", int64(math.MaxInt64))
	}
	return db.durably("save pool limits", func(b *batch) {
		b.exec(`INSERT INTO pool_limits (singleton_id, max_jobs_per_client, max_tasks_per_client, max_task_timeout_seconds,
				max_store_bytes_per_client, max_upload_bytes_per_attempt) VALUES (1, ?, ?, ?, ?, ?)
			ON CONFLICT (singleton_id) DO UPDATE SET max_jobs_per_client = excluded.max_jobs_per_client,
				max_tasks_per_client = excluded.max_tasks_per_client, max_task_timeout_seconds = excluded.max_task_timeout_seconds,
				max_store_bytes_per_client = excluded.max_store_bytes_per_client,
				max_upload_bytes_per_attempt = excluded.max_upload_bytes_per_attempt`,
			limits.MaxJobsPerClient, limits.MaxTasksPerClient, int64(limits.MaxTaskTimeout/time.Second),
			int64(limits.MaxStoreBytesPerClient), int64(limits.MaxUploadBytesPerAttempt))
	})
}

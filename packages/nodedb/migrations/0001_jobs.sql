-- Jobs a coordinator has accepted, in the order it accepted them.
CREATE TABLE jobs (
    seq INTEGER PRIMARY KEY AUTOINCREMENT,
    job_id TEXT NOT NULL UNIQUE,
    workload TEXT NOT NULL,
    params BLOB NOT NULL,
    mode INTEGER NOT NULL,
    max_tasks INTEGER NOT NULL,
    state INTEGER NOT NULL,
    result BLOB NOT NULL,
    error TEXT NOT NULL,
    -- The key a private job's blobs are sealed with. Kept only until the job
    -- finishes, since its tasks cannot be handed out again without it.
    sealing_key BLOB,
    -- Nanoseconds since the Unix epoch; zero for a job not yet finished.
    created_at_ns INTEGER NOT NULL,
    finished_at_ns INTEGER NOT NULL
) STRICT;

CREATE TABLE tasks (
    job_id TEXT NOT NULL REFERENCES jobs (job_id) ON DELETE CASCADE,
    task_index INTEGER NOT NULL,
    payload BLOB NOT NULL,
    state INTEGER NOT NULL,
    attempt INTEGER NOT NULL,
    failures INTEGER NOT NULL,
    node_id TEXT NOT NULL,
    node_name TEXT NOT NULL,
    output BLOB NOT NULL,
    error TEXT NOT NULL,
    PRIMARY KEY (job_id, task_index)
) STRICT, WITHOUT ROWID;

-- Every time a task was handed to a worker, and how that went. A task's row
-- in tasks holds only its latest attempt.
CREATE TABLE task_attempts (
    job_id TEXT NOT NULL,
    task_index INTEGER NOT NULL,
    attempt INTEGER NOT NULL,
    node_id TEXT NOT NULL,
    node_name TEXT NOT NULL,
    state INTEGER NOT NULL,
    error TEXT NOT NULL,
    started_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (job_id, task_index, attempt),
    FOREIGN KEY (job_id, task_index) REFERENCES tasks (job_id, task_index) ON DELETE CASCADE
) STRICT, WITHOUT ROWID;

-- The stored blobs a job has touched: 1 for one it read, 2 for one a task
-- stored, 3 for one that is part of its result.
CREATE TABLE job_blobs (
    job_id TEXT NOT NULL REFERENCES jobs (job_id) ON DELETE CASCADE,
    role INTEGER NOT NULL CHECK (role IN (1, 2, 3)),
    cid TEXT NOT NULL,
    PRIMARY KEY (job_id, role, cid)
) STRICT, WITHOUT ROWID;

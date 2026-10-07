-- How long one attempt at a task of the job may run, in nanoseconds; zero
-- for no limit.
ALTER TABLE jobs ADD COLUMN task_timeout_ns INTEGER NOT NULL DEFAULT 0;

-- What has happened to each job: the steps of its life and the lines its
-- tasks logged, numbered from one within the job.
CREATE TABLE job_events (
    job_id TEXT NOT NULL REFERENCES jobs (job_id) ON DELETE CASCADE,
    seq INTEGER NOT NULL,
    at_ns INTEGER NOT NULL,
    kind TEXT NOT NULL,
    -- The task concerned, or -1 for the job as a whole.
    task_index INTEGER NOT NULL,
    node_name TEXT NOT NULL,
    text TEXT NOT NULL,
    PRIMARY KEY (job_id, seq)
) STRICT, WITHOUT ROWID;

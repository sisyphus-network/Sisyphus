-- Verification by replication. How many different workers must return the
-- same result for each of a job's tasks: zero or one for a job that is not
-- verified.
ALTER TABLE jobs ADD COLUMN verify INTEGER NOT NULL DEFAULT 0;

-- What each worker returned for a task of a verified job: the output, and
-- the CIDs of the blobs the attempt stored, one to a line. Every result is
-- kept, the ones that did not agree too, so that a coordinator restarted
-- while a task is unsettled knows who has answered and what they said, and
-- so that the job's record can say who agreed.
CREATE TABLE task_results (
    job_id TEXT NOT NULL,
    task_index INTEGER NOT NULL,
    attempt INTEGER NOT NULL,
    node_id TEXT NOT NULL,
    node_name TEXT NOT NULL,
    output BLOB NOT NULL,
    blobs TEXT NOT NULL,
    PRIMARY KEY (job_id, task_index, attempt),
    FOREIGN KEY (job_id, task_index) REFERENCES tasks (job_id, task_index) ON DELETE CASCADE
) STRICT, WITHOUT ROWID;

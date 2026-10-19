-- The jobs a node that has joined another's pool submitted to it as a
-- client. The pool's coordinator keeps the jobs; this is only which of them
-- are this node's, so its own app can list them.
CREATE TABLE pool_jobs (
    job_id TEXT PRIMARY KEY NOT NULL,
    coordinator_id TEXT NOT NULL,
    submitted_ns INTEGER NOT NULL
) STRICT;

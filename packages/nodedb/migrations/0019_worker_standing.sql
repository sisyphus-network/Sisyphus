-- What a coordinator has seen of each worker across jobs, by node ID: how
-- many of the results it returned for verified tasks were among those the
-- task was settled by, how many were not, and how many agreeing results it
-- still owes after being outvoted before its word alone is taken again.
-- Nothing here goes when a job does, or when a member is removed.
CREATE TABLE worker_standing (
    node_id TEXT PRIMARY KEY NOT NULL,
    agreed INTEGER NOT NULL,
    outvoted INTEGER NOT NULL,
    probation INTEGER NOT NULL
) STRICT, WITHOUT ROWID;

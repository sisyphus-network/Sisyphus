-- The nodes this one is willing to take tasks from. Whether it gives a
-- node tasks is that node's place in members; the two are separate.
CREATE TABLE works_for (
    node_id TEXT PRIMARY KEY NOT NULL,
    -- Nanoseconds since the Unix epoch.
    since_ns INTEGER NOT NULL
) STRICT, WITHOUT ROWID;

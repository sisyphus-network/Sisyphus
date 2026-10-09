-- The latest signed record of each name this node answers for: its own, and
-- those of the nodes it has admitted, who hand theirs over. A name is a
-- node's ID. The record is kept as it was signed, so that it can be passed
-- on and checked by whoever receives it.
CREATE TABLE names (
    name TEXT PRIMARY KEY NOT NULL,
    record BLOB NOT NULL,
    -- Nanoseconds since the Unix epoch.
    received_ns INTEGER NOT NULL
) STRICT, WITHOUT ROWID;

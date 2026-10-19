-- A member may hold both roles: a worker that redeems a client invitation
-- keeps working and may also submit jobs. Both are kept as "worker,client".
-- SQLite cannot change a CHECK constraint in place, so the table is rebuilt.
CREATE TABLE members_with_roles (
    node_id TEXT PRIMARY KEY NOT NULL,
    role TEXT NOT NULL CHECK (role IN ('worker', 'client', 'worker,client')),
    -- Nanoseconds since the Unix epoch.
    joined_at_ns INTEGER NOT NULL
) STRICT, WITHOUT ROWID;

INSERT INTO members_with_roles (node_id, role, joined_at_ns)
    SELECT node_id, role, joined_at_ns FROM members;

DROP TABLE members;

ALTER TABLE members_with_roles RENAME TO members;

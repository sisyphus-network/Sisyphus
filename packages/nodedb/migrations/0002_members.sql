-- Nodes admitted to this one, other than itself.
CREATE TABLE members (
    node_id TEXT PRIMARY KEY NOT NULL,
    role TEXT NOT NULL CHECK (role IN ('worker', 'client')),
    -- Nanoseconds since the Unix epoch.
    joined_at_ns INTEGER NOT NULL
) STRICT, WITHOUT ROWID;

-- Invitations issued and not yet used. The token itself is not kept, only
-- its SHA-256, so that reading this table admits nobody.
CREATE TABLE invitations (
    token_hash TEXT PRIMARY KEY NOT NULL,
    role TEXT NOT NULL CHECK (role IN ('worker', 'client')),
    expires_at_ns INTEGER NOT NULL
) STRICT, WITHOUT ROWID;

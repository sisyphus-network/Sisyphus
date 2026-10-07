-- The files the node's owner has stored in the pool, by the names they were
-- given. The content is in the store, under the CID; this is the list of
-- what to keep there and what to call it.
CREATE TABLE files (
    cid TEXT PRIMARY KEY NOT NULL,
    name TEXT NOT NULL,
    size_bytes INTEGER NOT NULL,
    stored_at_ns INTEGER NOT NULL
) STRICT, WITHOUT ROWID;

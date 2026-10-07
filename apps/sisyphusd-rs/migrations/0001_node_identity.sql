CREATE TABLE node_identity (
    singleton_id INTEGER PRIMARY KEY NOT NULL CHECK (singleton_id = 1),
    encoding_version INTEGER NOT NULL,
    private_key BLOB NOT NULL,
    created_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP
);

-- A single short-lived pairing code may be outstanding at a time. The
-- verifier is stored, never the code shown to the owner.
CREATE TABLE browser_pairings (
    singleton_id INTEGER PRIMARY KEY NOT NULL CHECK (singleton_id = 1),
    code_hash BLOB NOT NULL CHECK (length(code_hash) = 32),
    expires_at_ns INTEGER NOT NULL
) STRICT;

-- Each paired browser has its own revocable random credential. Only its
-- SHA-256 verifier is kept here; the credential itself is returned once.
CREATE TABLE browser_devices (
    device_id TEXT PRIMARY KEY NOT NULL,
    name TEXT NOT NULL,
    token_hash BLOB NOT NULL CHECK (length(token_hash) = 32),
    created_at_ns INTEGER NOT NULL,
    expires_at_ns INTEGER NOT NULL
) STRICT, WITHOUT ROWID;

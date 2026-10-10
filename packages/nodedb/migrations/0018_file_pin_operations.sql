-- SQLite ownership and blob-store pins are separate durable systems.
-- Replay in sequence and acknowledge only after the store succeeds.
CREATE TABLE file_pin_operations (
    seq INTEGER PRIMARY KEY AUTOINCREMENT,
    owner TEXT NOT NULL,
    cid TEXT NOT NULL,
    keep INTEGER NOT NULL CHECK (keep IN (0, 1))
);

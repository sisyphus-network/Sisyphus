CREATE TABLE trusted_compute_peers (
    peer_id TEXT PRIMARY KEY,
    trusted_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP
);

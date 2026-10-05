CREATE TABLE bootstrap_peers (
    peer_id TEXT NOT NULL,
    address TEXT NOT NULL,
    created_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (peer_id, address)
);

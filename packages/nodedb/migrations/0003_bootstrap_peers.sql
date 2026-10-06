-- The node's address book: nodes to connect to when it starts, so that it
-- can find the rest. The table is the Rust daemon's, kept as it was.
CREATE TABLE bootstrap_peers (
    peer_id TEXT NOT NULL,
    address TEXT NOT NULL,
    created_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (peer_id, address)
) STRICT, WITHOUT ROWID;

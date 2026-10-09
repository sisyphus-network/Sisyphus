-- The requests made of this node as a pinning service, through the IPFS
-- Pinning Service API: what was asked to be kept, under what name, and how
-- far the node has got with it. The data is in the store, pinned for the
-- request; this is the record the API answers from.
CREATE TABLE pin_requests (
    request_id TEXT PRIMARY KEY NOT NULL,
    cid TEXT NOT NULL,
    name TEXT NOT NULL,
    -- JSON: a list of addresses, and a map of strings to strings.
    origins TEXT NOT NULL,
    meta TEXT NOT NULL,
    status TEXT NOT NULL,
    info TEXT NOT NULL,
    created_at_ns INTEGER NOT NULL,
    -- JSON: the CIDs of the requests this one replaced, which are kept until
    -- this one is settled.
    holds TEXT NOT NULL
) STRICT, WITHOUT ROWID;

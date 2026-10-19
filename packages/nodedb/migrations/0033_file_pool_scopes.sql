-- A node can use its own pool and join other pools over time. Keep the
-- catalogue metadata per pool while sharing the content row by CID.
CREATE TABLE file_pool_scopes (
    coordinator_id TEXT NOT NULL,
    cid TEXT NOT NULL REFERENCES files(cid) ON DELETE CASCADE,
    name TEXT NOT NULL,
    size_bytes INTEGER NOT NULL,
    stored_at_ns INTEGER NOT NULL,
    private INTEGER NOT NULL,
    PRIMARY KEY (coordinator_id, cid)
) STRICT, WITHOUT ROWID;

-- Recover the scope of joined-pool entries from the durable holds introduced
-- with joined-pool file storage. Files without such a hold are legacy entries
-- in the node's own pool.
INSERT INTO file_pool_scopes (coordinator_id, cid, name, size_bytes, stored_at_ns, private)
SELECT DISTINCT h.coordinator_id, f.cid, f.name, f.size_bytes, f.stored_at_ns, f.private
FROM files f JOIN pool_file_holds h ON h.cid = f.cid;

INSERT INTO file_pool_scopes (coordinator_id, cid, name, size_bytes, stored_at_ns, private)
SELECT '', f.cid, f.name, f.size_bytes, f.stored_at_ns, f.private
FROM files f
WHERE NOT EXISTS (SELECT 1 FROM pool_file_holds h WHERE h.cid = f.cid);

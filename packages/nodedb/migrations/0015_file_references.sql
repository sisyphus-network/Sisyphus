-- Ownership is separate from a content-addressed file: identical uploads
-- share the CID but must be released independently.
CREATE TABLE file_references (
    owner TEXT NOT NULL CHECK (length(owner) > 0),
    cid TEXT NOT NULL REFERENCES files(cid) ON DELETE RESTRICT,
    PRIMARY KEY (owner, cid)
);
CREATE INDEX file_references_by_cid ON file_references(cid);

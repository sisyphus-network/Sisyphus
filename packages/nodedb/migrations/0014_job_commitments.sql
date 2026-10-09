-- What the record of a private job holds in place of each value it leaves
-- out: a keyed hash of the value, by the path to it in the record. They are
-- made when the job ends, with a key derived from the job's sealing key,
-- and kept so that the record can be worked out again once that key is
-- gone. The key itself is not here.
CREATE TABLE job_commitments (
    job_id TEXT NOT NULL REFERENCES jobs (job_id) ON DELETE CASCADE,
    name TEXT NOT NULL,
    commitment BLOB NOT NULL,
    PRIMARY KEY (job_id, name)
) STRICT, WITHOUT ROWID;

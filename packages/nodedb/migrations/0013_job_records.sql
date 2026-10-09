-- The CID of a finished job's record, the linked data that holds its
-- history; empty until the job is over. The node that submitted the job,
-- if it came from another node. And whether the job is private, which
-- used to be told by its key, and the key goes when the job ends.
ALTER TABLE jobs ADD COLUMN record_cid TEXT NOT NULL DEFAULT '';
ALTER TABLE jobs ADD COLUMN submitter_id TEXT NOT NULL DEFAULT '';
ALTER TABLE jobs ADD COLUMN private INTEGER NOT NULL DEFAULT 0;
UPDATE jobs SET private = 1 WHERE length(sealing_key) > 0;

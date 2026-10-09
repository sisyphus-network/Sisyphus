-- A job that is a step of another: the job it is a step of, and which
-- step. Both are empty for a job submitted by itself.
ALTER TABLE jobs ADD COLUMN parent_job_id TEXT NOT NULL DEFAULT '';
ALTER TABLE jobs ADD COLUMN step TEXT NOT NULL DEFAULT '';

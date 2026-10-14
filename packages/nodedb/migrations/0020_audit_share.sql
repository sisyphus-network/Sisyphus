-- The share of a job's tasks that its coordinator runs again itself before
-- taking the result. Zero for a job that is not audited.
ALTER TABLE jobs ADD COLUMN audit_share REAL NOT NULL DEFAULT 0;

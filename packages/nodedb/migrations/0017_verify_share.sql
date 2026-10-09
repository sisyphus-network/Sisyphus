-- Spot checks. The share of a verified job's tasks that was picked to be
-- verified when it was submitted: zero for a job that verifies them all.
ALTER TABLE jobs ADD COLUMN verify_share REAL NOT NULL DEFAULT 0;

-- How many different workers must return the same result for a task: as
-- many as its job asks for, or one for a task the job's spot checks passed
-- over. It rises to the job's number if such a task is verified after all.
-- Every task saved before there were spot checks is held to its job's.
ALTER TABLE tasks ADD COLUMN verify INTEGER NOT NULL DEFAULT 0;
UPDATE tasks SET verify = (SELECT verify FROM jobs WHERE jobs.job_id = tasks.job_id);

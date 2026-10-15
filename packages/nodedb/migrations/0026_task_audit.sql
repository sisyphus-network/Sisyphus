-- What came of the coordinator running a task again itself to check the
-- worker's result: matched, caught or inconclusive, or empty if the task
-- was not audited.
ALTER TABLE tasks ADD COLUMN audit TEXT NOT NULL DEFAULT '';

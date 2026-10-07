-- What a worker must have to be given a job's tasks: bytes of memory, and
-- graphics cards. Zero asks nothing.
ALTER TABLE jobs ADD COLUMN min_memory_bytes INTEGER NOT NULL DEFAULT 0;
ALTER TABLE jobs ADD COLUMN min_gpus INTEGER NOT NULL DEFAULT 0;

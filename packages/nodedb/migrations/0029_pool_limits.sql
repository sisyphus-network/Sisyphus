-- The limits a coordinator holds each member to belong to the node, so any
-- authorized client can configure them. 0 means no limit. The task timeout
-- is in whole seconds, and no more than a time.Duration holds.
CREATE TABLE pool_limits (
    singleton_id INTEGER PRIMARY KEY NOT NULL CHECK (singleton_id = 1),
    max_jobs_per_client INTEGER NOT NULL CHECK (max_jobs_per_client >= 0),
    max_tasks_per_client INTEGER NOT NULL CHECK (max_tasks_per_client >= 0),
    max_task_timeout_seconds INTEGER NOT NULL CHECK (max_task_timeout_seconds BETWEEN 0 AND 9223372036),
    max_store_bytes_per_client INTEGER NOT NULL CHECK (max_store_bytes_per_client >= 0),
    max_upload_bytes_per_attempt INTEGER NOT NULL CHECK (max_upload_bytes_per_attempt >= 0)
) STRICT;

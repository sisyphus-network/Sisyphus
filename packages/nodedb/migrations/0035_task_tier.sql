-- Which tier of its workload's tolerances settled a task, for a workload
-- whose tolerance depends on where its results were computed: same-cpu or
-- cross-cpu, or empty for a task settled byte for byte or in no tier.
ALTER TABLE tasks ADD COLUMN tier TEXT NOT NULL DEFAULT '';
-- The processor of the worker that returned a result, as the coordinator
-- knew that worker when it handed out the attempt, never as the result
-- said. Empty if unknown, and for every result saved before it was kept.
ALTER TABLE task_results ADD COLUMN cpu_model TEXT NOT NULL DEFAULT '';

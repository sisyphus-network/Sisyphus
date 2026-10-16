-- What a model task's worker says an attempt ran with: the model server,
-- its version, the model, its digest and quantisation, and the options
-- sent. All empty for work that asks no model server.
ALTER TABLE task_attempts ADD COLUMN used_server TEXT NOT NULL DEFAULT '';
ALTER TABLE task_attempts ADD COLUMN used_version TEXT NOT NULL DEFAULT '';
ALTER TABLE task_attempts ADD COLUMN used_model TEXT NOT NULL DEFAULT '';
ALTER TABLE task_attempts ADD COLUMN used_digest TEXT NOT NULL DEFAULT '';
ALTER TABLE task_attempts ADD COLUMN used_quantization TEXT NOT NULL DEFAULT '';
ALTER TABLE task_attempts ADD COLUMN used_options BLOB;

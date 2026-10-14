-- How the node's owner lends its Ollama models to the pool, as set in the
-- desktop app. Flags given when the node starts take its place.
CREATE TABLE model_lending (
    singleton_id INTEGER PRIMARY KEY NOT NULL CHECK (singleton_id = 1),
    enabled INTEGER NOT NULL CHECK (enabled IN (0, 1)),
    ollama_url TEXT NOT NULL,
    only_models TEXT NOT NULL,
    gpu_layers INTEGER NOT NULL,
    threads INTEGER NOT NULL,
    context INTEGER NOT NULL,
    keep_alive TEXT NOT NULL
) STRICT;

-- Which language model this node plans with, and how to reach it. The key,
-- if the service wants one, is kept here because the database is the one
-- place on the node that only its owner can read.
CREATE TABLE model_config (
    singleton_id INTEGER PRIMARY KEY NOT NULL CHECK (singleton_id = 1),
    provider TEXT NOT NULL,
    base_url TEXT NOT NULL,
    model TEXT NOT NULL,
    api_key TEXT NOT NULL
) STRICT;

-- Conversations with the planner, and what was said in each, in order. A
-- message is kept as the JSON it travels as.
CREATE TABLE chats (
    chat_id TEXT PRIMARY KEY NOT NULL,
    title TEXT NOT NULL,
    created_at_ns INTEGER NOT NULL
) STRICT, WITHOUT ROWID;

CREATE TABLE chat_messages (
    chat_id TEXT NOT NULL REFERENCES chats (chat_id) ON DELETE CASCADE,
    seq INTEGER NOT NULL,
    at_ns INTEGER NOT NULL,
    message TEXT NOT NULL,
    PRIMARY KEY (chat_id, seq)
) STRICT, WITHOUT ROWID;

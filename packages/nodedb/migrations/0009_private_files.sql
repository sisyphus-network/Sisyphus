-- Whether a stored file was sealed with the node's key before it was
-- stored. Its size is of what was sealed, not of what the store holds.
ALTER TABLE files ADD COLUMN private INTEGER NOT NULL DEFAULT 0;

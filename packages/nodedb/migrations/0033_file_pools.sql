-- Which store each file is kept in: the ID of the coordinator of a pool this
-- node has joined, or empty for the node's own store. A node that has kept
-- files in several shows each only its own (#509).
ALTER TABLE files ADD COLUMN pool TEXT NOT NULL DEFAULT '';

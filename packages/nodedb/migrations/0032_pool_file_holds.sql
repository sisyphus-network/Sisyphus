-- What a node that has joined another's pool as a client keeps there, and
-- for whom on this node: the user, a chat, a chat's draft. The pool's
-- coordinator keeps one pin of each file for this node as a client; this
-- is what that pin stands for, so that it lasts as long as the longest of
-- them and goes when the last of them does. An expiry of 0 is none.
CREATE TABLE pool_file_holds (
    coordinator_id TEXT NOT NULL,
    cid TEXT NOT NULL,
    owner TEXT NOT NULL,
    expires_ns INTEGER NOT NULL,
    PRIMARY KEY (coordinator_id, cid, owner)
) STRICT;

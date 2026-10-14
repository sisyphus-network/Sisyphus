-- Remote browser access is opt-in. No row means disabled.
CREATE TABLE web_access_config (
    singleton_id INTEGER PRIMARY KEY NOT NULL CHECK (singleton_id = 1),
    enabled INTEGER NOT NULL CHECK (enabled IN (0, 1)),
    listen_address TEXT NOT NULL DEFAULT '',
    public_url TEXT NOT NULL DEFAULT ''
) STRICT;

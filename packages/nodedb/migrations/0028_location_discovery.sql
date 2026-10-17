-- Location discovery is opt-in and belongs to the node, not the desktop
-- installation, so any authorized client can configure it.
CREATE TABLE location_discovery (
    singleton_id INTEGER PRIMARY KEY NOT NULL CHECK (singleton_id = 1),
    locate_gateway INTEGER NOT NULL CHECK (locate_gateway IN (0, 1)),
    locate_country INTEGER NOT NULL CHECK (locate_country IN (0, 1)),
    stun_server TEXT NOT NULL DEFAULT ''
) STRICT;

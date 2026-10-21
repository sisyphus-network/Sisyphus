-- Where the node's owner says the node is, and how much of it other members
-- of a pool may see. It belongs to the node, not the desktop installation,
-- so any authorized client can set it. No row means no location was given.
CREATE TABLE owner_location (
    singleton_id INTEGER PRIMARY KEY NOT NULL CHECK (singleton_id = 1),
    latitude REAL NOT NULL CHECK (latitude BETWEEN -90 AND 90),
    longitude REAL NOT NULL CHECK (longitude BETWEEN -180 AND 180),
    share TEXT NOT NULL CHECK (share IN ('exact', 'about-1km', 'city', 'country', 'nothing'))
) STRICT;

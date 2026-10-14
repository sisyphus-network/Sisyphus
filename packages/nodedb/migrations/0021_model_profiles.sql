CREATE TABLE model_profiles (
    profile_id TEXT PRIMARY KEY NOT NULL,
    name TEXT NOT NULL,
    provider TEXT NOT NULL,
    base_url TEXT NOT NULL,
    model TEXT NOT NULL,
    api_key TEXT NOT NULL,
    is_active INTEGER NOT NULL CHECK (is_active IN (0, 1))
) STRICT, WITHOUT ROWID;

-- Preserve the existing single configured provider as the first profile.
INSERT INTO model_profiles (profile_id, name, provider, base_url, model, api_key, is_active)
SELECT 'default', provider, provider, base_url, model, api_key, 1
FROM model_config WHERE singleton_id = 1;

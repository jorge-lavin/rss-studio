CREATE TABLE IF NOT EXISTS feature_flags (
    name TEXT PRIMARY KEY,
    enabled INTEGER NOT NULL CHECK (enabled IN (0, 1)),
    updated_at TEXT NOT NULL,
    updated_by INTEGER
);

INSERT INTO feature_flags (name, enabled, updated_at, updated_by)
VALUES
    ('billing.enabled', 0, strftime('%Y-%m-%dT%H:%M:%fZ', 'now'), NULL),
    ('billing.stripe', 0, strftime('%Y-%m-%dT%H:%M:%fZ', 'now'), NULL),
    ('billing.bitcoin', 0, strftime('%Y-%m-%dT%H:%M:%fZ', 'now'), NULL),
    ('billing.enforcement', 0, strftime('%Y-%m-%dT%H:%M:%fZ', 'now'), NULL)
ON CONFLICT(name) DO NOTHING;

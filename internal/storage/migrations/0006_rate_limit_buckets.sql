CREATE TABLE IF NOT EXISTS rate_limit_buckets (
    bucket_key TEXT NOT NULL,
    operation TEXT NOT NULL,
    window_started TEXT NOT NULL,
    count INTEGER NOT NULL,
    PRIMARY KEY (bucket_key, operation)
);

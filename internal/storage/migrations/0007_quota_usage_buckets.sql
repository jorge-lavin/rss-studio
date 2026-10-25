CREATE TABLE IF NOT EXISTS quota_usage_buckets (
    user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    bucket TEXT NOT NULL,
    period_start TEXT NOT NULL,
    count INTEGER NOT NULL CHECK (count >= 0),
    PRIMARY KEY (user_id, bucket, period_start)
);

CREATE INDEX IF NOT EXISTS quota_usage_buckets_user_bucket
    ON quota_usage_buckets(user_id, bucket, period_start);

INSERT INTO quota_usage_buckets (user_id, bucket, period_start, count)
SELECT user_id, 'published_feeds', 'permanent', COUNT(*)
FROM feeds
GROUP BY user_id
ON CONFLICT(user_id, bucket, period_start) DO NOTHING;

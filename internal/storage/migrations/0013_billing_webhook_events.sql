CREATE TABLE IF NOT EXISTS billing_webhook_events (
    provider TEXT NOT NULL,
    event_id TEXT NOT NULL,
    account_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    event_created_at TEXT NOT NULL,
    received_at TEXT NOT NULL,
    PRIMARY KEY (provider, event_id)
);

CREATE INDEX IF NOT EXISTS billing_webhook_events_account_time
    ON billing_webhook_events(provider, account_id, event_created_at);

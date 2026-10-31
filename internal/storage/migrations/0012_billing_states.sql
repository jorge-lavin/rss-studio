CREATE TABLE IF NOT EXISTS billing_states (
    user_id INTEGER PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
    product_key TEXT NOT NULL,
    status TEXT NOT NULL,
    trial_started_at TEXT,
    trial_ends_at TEXT,
    current_period_start TEXT,
    current_period_end TEXT,
    provider TEXT NOT NULL DEFAULT '',
    provider_customer_ref TEXT,
    provider_subscription_ref TEXT,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS billing_states_provider_subscription
    ON billing_states(provider, provider_subscription_ref)
    WHERE provider_subscription_ref IS NOT NULL;

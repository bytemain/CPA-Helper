-- +goose Up
-- The manual quota-reset counter carried no business value (it counted local
-- cooldown clears, not real credit redemptions). Drop it now that resetKeeperQuota
-- redeems a real OpenAI reset credit instead of incrementing this table.
DROP TABLE IF EXISTS codex_keeper_quota_resets;

-- +goose Down
-- Recreate the empty table schema so a rollback restores the shape (the historical
-- counts are intentionally not restored — they were worthless cooldown-clear tallies).
CREATE TABLE IF NOT EXISTS codex_keeper_quota_resets (
	auth_name VARCHAR(500) PRIMARY KEY,
	reset_count INTEGER NOT NULL DEFAULT 0,
	last_reset_at TIMESTAMP NULL
);

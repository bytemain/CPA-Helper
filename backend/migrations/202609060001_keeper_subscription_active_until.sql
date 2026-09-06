-- +goose Up
ALTER TABLE codex_keeper_auth_states ADD COLUMN subscription_active_until TIMESTAMP NULL;

-- +goose Down
ALTER TABLE codex_keeper_auth_states DROP COLUMN subscription_active_until;

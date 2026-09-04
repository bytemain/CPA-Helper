-- +goose Up
ALTER TABLE codex_keeper_auth_states ADD COLUMN reset_credit_count INTEGER;
ALTER TABLE codex_keeper_auth_states ADD COLUMN reset_credits TEXT;

-- +goose Down
ALTER TABLE codex_keeper_auth_states DROP COLUMN reset_credits;
ALTER TABLE codex_keeper_auth_states DROP COLUMN reset_credit_count;

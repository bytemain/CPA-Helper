-- +goose Up
-- The Codex-Keeper now inspects more than Codex accounts. `provider` records which upstream a
-- row belongs to (codex | antigravity); existing rows predate this and are treated as codex when
-- NULL. `antigravity_quota` stores the parsed Antigravity quota summary (groups -> buckets) as a
-- JSON blob, mirroring how reset_credits is stored; it stays NULL for codex rows.
ALTER TABLE codex_keeper_auth_states ADD COLUMN provider TEXT;
ALTER TABLE codex_keeper_auth_states ADD COLUMN antigravity_quota TEXT;

-- +goose Down
ALTER TABLE codex_keeper_auth_states DROP COLUMN antigravity_quota;
ALTER TABLE codex_keeper_auth_states DROP COLUMN provider;

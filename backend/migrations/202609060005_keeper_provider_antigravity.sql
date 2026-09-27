-- +goose Up
-- The Codex-Keeper now inspects more than Codex accounts. `provider` records which upstream a
-- row belongs to (codex | antigravity | kimi | xai | devin); existing rows predate this and are
-- treated as codex when NULL. `antigravity_quota` stores the parsed quota summary (groups ->
-- buckets) as a JSON blob, mirroring how reset_credits is stored. Despite the legacy column
-- name it is now a GENERIC per-provider quota snapshot: antigravity, kimi, xai, devin — and
-- codex — all persist their projected quota here.
ALTER TABLE codex_keeper_auth_states ADD COLUMN provider TEXT;
ALTER TABLE codex_keeper_auth_states ADD COLUMN antigravity_quota TEXT;
-- antigravity_identity_digest is a stable, versioned one-way digest of the account identity
-- (provider + account anchor + normalized email; the anchor is the Google project id for
-- antigravity, auth_index for the generic quota providers, account_id for codex). Only the
-- digest is stored — never the raw identifiers — so a later inspection can detect an identity
-- SWAP (digest changed) and clear the stale quota without persisting them.
ALTER TABLE codex_keeper_auth_states ADD COLUMN antigravity_identity_digest TEXT;

-- +goose Down
ALTER TABLE codex_keeper_auth_states DROP COLUMN antigravity_identity_digest;
ALTER TABLE codex_keeper_auth_states DROP COLUMN antigravity_quota;
ALTER TABLE codex_keeper_auth_states DROP COLUMN provider;

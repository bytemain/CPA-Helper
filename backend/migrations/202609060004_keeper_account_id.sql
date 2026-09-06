-- +goose Up
-- Store the account_id (ChatGPT account identity) alongside each keeper auth state so the
-- subscription renewal snapshot can be scoped to the ACCOUNT, not just the auth_index.
-- CPA's EnsureIndex() hashes file auth by provider + file path, so swapping the same
-- filename to a different OpenAI account keeps auth_index unchanged; without account_id a
-- preserve-on-unknown subscription write would inherit the previous account's renewal date.
ALTER TABLE codex_keeper_auth_states ADD COLUMN account_id TEXT NULL;

-- +goose Down
ALTER TABLE codex_keeper_auth_states DROP COLUMN account_id;

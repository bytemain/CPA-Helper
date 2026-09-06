-- +goose Up
-- Persistent redeem ledger for real reset-credit consumption. It holds at most one
-- row per auth_name, bound to the identity (auth_index + account_id) the redeem was
-- issued against. A pending row is an in-flight/unknown-outcome redeem whose
-- redeem_request_id must be reused idempotently on the next attempt for the SAME
-- identity, so a lost response can never burn a second credit. A pending row is only
-- reused when auth_index AND account_id still match — an auth_name rebuilt onto a
-- different account never inherits the old account's request_id.
CREATE TABLE IF NOT EXISTS codex_keeper_reset_redeems (
	auth_name VARCHAR(500) PRIMARY KEY,
	auth_index TEXT NOT NULL,
	account_id TEXT NOT NULL,
	redeem_request_id TEXT NOT NULL,
	status TEXT NOT NULL,
	updated_at TIMESTAMP NOT NULL
);

-- +goose Down
DROP TABLE IF EXISTS codex_keeper_reset_redeems;

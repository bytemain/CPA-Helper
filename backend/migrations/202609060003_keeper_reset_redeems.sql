-- +goose Up
-- Persistent redeem ledger for real reset-credit consumption. It is keyed by the full
-- identity (auth_name + auth_index + account_id), so distinct identities that share an
-- auth_name each keep their OWN row: a redeem issued against one identity is never
-- overwritten when the auth_name is later rebuilt onto a different account. A pending row
-- is an in-flight/unknown-outcome redeem whose redeem_request_id must be reused
-- idempotently on the next attempt for the SAME identity (so a lost response can never
-- burn a second credit); if the original account returns, its pending key is still here.
CREATE TABLE IF NOT EXISTS codex_keeper_reset_redeems (
	auth_name VARCHAR(500) NOT NULL,
	auth_index TEXT NOT NULL,
	account_id TEXT NOT NULL,
	redeem_request_id TEXT NOT NULL,
	status TEXT NOT NULL,
	updated_at TIMESTAMP NOT NULL,
	PRIMARY KEY (auth_name, auth_index, account_id)
);

-- +goose Down
DROP TABLE IF EXISTS codex_keeper_reset_redeems;

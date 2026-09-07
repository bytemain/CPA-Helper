-- +goose Up
-- Persistent redeem ledger for real reset-credit consumption, keyed by the STABLE OpenAI
-- account_id (the resource identity). auth_name (a filename) and auth_index (a CPA hash of
-- the file path) are only routing selectors — both can change for the same account (file
-- rename / move / reorder) — so the idempotency key must NOT depend on them. A pending row
-- is an in-flight/unknown-outcome redeem whose redeem_request_id must be reused
-- idempotently on the next attempt for the SAME account (however it is now routed), so a
-- lost response can never burn a second credit.
CREATE TABLE IF NOT EXISTS codex_keeper_reset_redeems (
	account_id TEXT PRIMARY KEY,
	redeem_request_id TEXT NOT NULL,
	status TEXT NOT NULL,
	updated_at TIMESTAMP NOT NULL
);

-- +goose Down
DROP TABLE IF EXISTS codex_keeper_reset_redeems;

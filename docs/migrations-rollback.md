# Migration rollback runbook

The database schema version is tracked by goose in `goose_db_version`. On startup the
app runs migrations up to `migrations.LatestVersion` and **refuses to start when the DB
version is newer than the binary** (goose reports
`database migration version is newer than this application`). A binary rollback
therefore always requires migrating the schema **down first**.

## Rolling back the reset-credit-consume release (migrations 202609060001–202609060003)

This release added, on top of `202609040002`:

- `202609060001` — `codex_keeper_auth_states.subscription_active_until` column.
- `202609060002` — **DROP** of the obsolete `codex_keeper_quota_resets` table.
- `202609060003` — `codex_keeper_reset_redeems` (redeem ledger) table.

The previous binary (`a996697`, target version `202609040002`) both refuses to start
against a newer version **and** still `SELECT`s `codex_keeper_quota_resets` in
`/accounts`. So you cannot just swap the binary back.

**Rollback procedure (do this in order):**

1. With the **current** (this-release) binary still deployed, migrate the schema down to
   the previous binary's target version:

   ```
   cpa-helper migrate down-to 202609040002
   ```

   (or the equivalent goose `down-to 202609040002`). This drops
   `codex_keeper_reset_redeems`, drops `subscription_active_until`, and **recreates an
   empty `codex_keeper_quota_resets`** so the old binary's `/accounts` query works.

2. Deploy the previous binary (`a996697`).

**Data semantics:** the Down of `202609060002` restores the `codex_keeper_quota_resets`
**schema only** — it is intentionally empty. The historical manual-reset counts are not
recovered (they carried no business value and were deliberately discarded). No other
data is lost by the rollback; auth states, credit snapshots, etc. are untouched by these
Down migrations.

This contract is covered by `TestRollbackToPreConsumeRestoresCompatSchema`, which
asserts that Down to `202609040002` restores the compat table and removes the new
objects.

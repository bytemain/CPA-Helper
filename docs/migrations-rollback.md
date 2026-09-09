# Migration rollback runbook

The database schema version is tracked by goose in `goose_db_version`. On startup the
app runs migrations up to `migrations.LatestVersion` and **refuses to start when the DB
version is newer than the binary** (goose reports
`database migration version is newer than this application`). A binary rollback
therefore always requires migrating the schema **down first**.

## Rolling back the keeper releases (migrations 202609060001–202609060005)

This release added, on top of `202609040002`:

- `202609060001` — `codex_keeper_auth_states.subscription_active_until` column.
- `202609060002` — **DROP** of the obsolete `codex_keeper_quota_resets` table.
- `202609060003` — `codex_keeper_reset_redeems` (redeem ledger) table.
- `202609060004` — `codex_keeper_auth_states.account_id` column (subscription identity scope).
- `202609060005` — `codex_keeper_auth_states.provider` + `antigravity_quota` columns (multi-provider inspection: Antigravity accounts). Its Down drops both columns; no data beyond the Antigravity quota snapshot / provider tag is lost.

The previous binary (`a996697`, target version `202609040002`) both refuses to start
against a newer version **and** still `SELECT`s `codex_keeper_quota_resets` in
`/accounts`. So you cannot just swap the binary back.

**Rollback procedure (do this in order):**

0. **Stop the CPA-Helper service and back up the SQLite database.** The downgrade is an
   **offline** operation: `migrate down-to` must run against a quiescent database. Its
   pending-redeem pre-flight check (step 1) is a safety net, **not** atomic protection —
   a still-running service could create a pending redeem between the check and the drop.
   Ensure no CPA-Helper process is running before proceeding.

1. Migrate the schema down to the previous binary's target version using the real
   `migrate down-to` subcommand (run it from the **current**, this-release binary):

   ```
   cpa-helper migrate down-to 202609040002
   ```

   **Pending redeems:** the command **refuses** to run when the redeem ledger holds any
   `status='pending'` row, because dropping the ledger loses that redeem's unique
   idempotency key (a later re-upgrade + reset could then double-consume). Reconcile
   those redeems first; only after that (and a backup) re-run with `--allow-pending`:

   ```
   cpa-helper migrate down-to 202609040002 --allow-pending
   ```

   This runs the Down migrations for `202609060005`, `202609060004`, `202609060003`,
   `202609060002`, and `202609060001`: it drops the `provider` + `antigravity_quota` columns,
   drops the `account_id` column, drops `codex_keeper_reset_redeems`,
   drops the `subscription_active_until` column, and **recreates an empty
   `codex_keeper_quota_resets`** so the old binary's `/accounts` query works.
   `202609040002` is the only allowlisted rollback target; the command refuses any other
   version and refuses to run when the DB is not newer than the target.

   > Do NOT expect a bare `cpa-helper migrate` to downgrade — it only runs migrations
   > **Up**. The dedicated `migrate down-to` subcommand is required.

2. Deploy the previous binary (`a996697`).

**Data semantics — what the rollback loses:**

- `codex_keeper_quota_resets` is restored as an **empty** table. The historical
  manual-reset counts are not recovered (they carried no business value and were
  deliberately discarded).
- `subscription_active_until` (the column **and every collected subscription-renewal
  timestamp**) is dropped and **not** recovered. If you need it, restore from the backup
  taken in step 0; otherwise accept the loss.
- `account_id` (the column **and every stored account identity**) is dropped and **not**
  recovered. It is re-derived on the next inspection after re-upgrading, so the loss is
  transient; restore from the step-0 backup only if you need it before re-upgrading.
- `codex_keeper_reset_redeems` (the in-flight redeem ledger) is dropped. Any unresolved
  pending redeem is lost; after rollback the old binary cannot consume credits at all, so
  this is acceptable.
- Other data — auth states, credit snapshots, usage records — is untouched by these Down
  migrations.

This contract is covered by `TestRollbackToPreConsumeRestoresCompatSchema` (Down restores
the compat table and removes the new objects) and by
`TestMigrateDownToRollsBackToTarget` (the `migrate down-to` CLI really moves the version
from the head to `202609040002` and rejects unlisted targets).

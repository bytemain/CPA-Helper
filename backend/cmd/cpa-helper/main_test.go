package main

import (
	"bytes"
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
)

func TestRunHelpListsOperationalSubcommands(t *testing.T) {
	var output bytes.Buffer
	if err := run(context.Background(), []string{"--help"}, &output); err != nil {
		t.Fatalf("run help failed: %v", err)
	}
	text := output.String()
	for _, want := range []string{"migrate", "serve", "doctor"} {
		if !strings.Contains(text, want) {
			t.Fatalf("help output missing %q: %s", want, text)
		}
	}
}

func TestBackendAddrRejectsBarePort(t *testing.T) {
	t.Setenv("CPA_HELPER_ADDR", "18317")
	if _, err := backendAddr(); err == nil {
		t.Fatal("backendAddr accepted a bare port")
	}
}

// TestMigrateDownToRollsBackToTarget proves the real `migrate down-to <version>`
// subcommand actually downgrades the schema to an allowlisted target (unlike a bare
// `migrate`, which only runs Up), and refuses unlisted targets / a missing version.
func TestMigrateDownToRollsBackToTarget(t *testing.T) {
	t.Setenv("CPA_HELPER_DATA_DIR", t.TempDir())
	ctx := context.Background()

	var up bytes.Buffer
	if err := run(ctx, []string{"migrate"}, &up); err != nil {
		t.Fatalf("migrate up: %v", err)
	}

	var down bytes.Buffer
	if err := run(ctx, []string{"migrate", "down-to", "202609040002"}, &down); err != nil {
		t.Fatalf("migrate down-to: %v", err)
	}
	out := down.String()
	if !strings.Contains(out, "current_version=202609040002") {
		t.Fatalf("rollback did not reach 202609040002: %s", out)
	}
	if !strings.Contains(out, "previous_version=202609060004") {
		t.Fatalf("rollback did not start from head 202609060004: %s", out)
	}

	// A non-allowlisted target is refused.
	if err := run(ctx, []string{"migrate", "down-to", "202609040001"}, &bytes.Buffer{}); err == nil {
		t.Fatal("down-to accepted a non-allowlisted target")
	}
	// A missing version errors rather than silently no-op'ing.
	if err := run(ctx, []string{"migrate", "down-to"}, &bytes.Buffer{}); err == nil {
		t.Fatal("down-to accepted a missing version")
	}
	// A destructive subcommand rejects unknown flags/subcommands instead of silently
	// ignoring a typo or falling back to Up.
	if err := run(ctx, []string{"migrate", "down-to", "202609040002", "--typo"}, &bytes.Buffer{}); err == nil {
		t.Fatal("down-to accepted an unknown flag")
	}
	if err := run(ctx, []string{"migrate", "nonsense"}, &bytes.Buffer{}); err == nil {
		t.Fatal("migrate accepted an unknown subcommand (must not fall back to Up)")
	}
}

// TestMigrateDownToRefusesPendingRedeems proves a rollback is refused by default when the
// redeem ledger holds a pending (unresolved) redeem — dropping it would lose the unique
// idempotency key — and proceeds only with the explicit --allow-pending override.
func TestMigrateDownToRefusesPendingRedeems(t *testing.T) {
	dataDir := t.TempDir()
	t.Setenv("CPA_HELPER_DATA_DIR", dataDir)
	ctx := context.Background()

	if err := run(ctx, []string{"migrate"}, &bytes.Buffer{}); err != nil {
		t.Fatalf("migrate up: %v", err)
	}

	// Inject a pending redeem directly into the ledger.
	dbPath := filepath.Join(dataDir, "db", "cpa_helper.sqlite3")
	db, err := sql.Open("sqlite", dbPath+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO codex_keeper_reset_redeems (account_id, redeem_request_id, status, updated_at) VALUES ('acct-p','rid','pending','2026-01-01 00:00:00')`); err != nil {
		_ = db.Close()
		t.Fatalf("insert pending: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close db: %v", err)
	}

	// Default rollback is refused while a pending redeem exists.
	if err := run(ctx, []string{"migrate", "down-to", "202609040002"}, &bytes.Buffer{}); err == nil {
		t.Fatal("rollback should be refused while a pending redeem exists")
	}
	if v := currentVersionForTest(t, dbPath); v != 202609060004 {
		t.Fatalf("refused rollback still changed version to %d", v)
	}

	// The explicit override proceeds.
	var out bytes.Buffer
	if err := run(ctx, []string{"migrate", "down-to", "202609040002", "--allow-pending"}, &out); err != nil {
		t.Fatalf("override rollback: %v", err)
	}
	if !strings.Contains(out.String(), "current_version=202609040002") {
		t.Fatalf("override rollback did not reach target: %s", out.String())
	}
}

func currentVersionForTest(t *testing.T, dbPath string) int64 {
	t.Helper()
	db, err := sql.Open("sqlite", dbPath+"?mode=ro&_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer db.Close()
	var v int64
	if err := db.QueryRow(`SELECT MAX(version_id) FROM goose_db_version`).Scan(&v); err != nil {
		t.Fatalf("query version: %v", err)
	}
	return v
}

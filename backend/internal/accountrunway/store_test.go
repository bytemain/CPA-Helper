package accountrunway

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	backendApp "cpa-helper/backend/internal/app"

	_ "modernc.org/sqlite"
)

// newFixtureDB creates a writable database with the minimal schema the report
// queries touch, then returns its path. Rows must be inserted through the
// writable handle (via insertAccount / insertUsage) before OpenReadOnly reads.
func newFixtureDB(t *testing.T) (*sql.DB, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "cpa_helper.sqlite3")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open writable db: %v", err)
	}
	_, err = db.Exec(`
		CREATE TABLE codex_keeper_auth_states (
			auth_name TEXT PRIMARY KEY,
			auth_index TEXT,
			email TEXT,
			disabled BOOLEAN NOT NULL DEFAULT 0,
			provider TEXT,
			primary_used_percent INTEGER,
			secondary_used_percent INTEGER,
			primary_reset_at TEXT,
			secondary_reset_at TEXT,
			primary_window_seconds INTEGER,
			secondary_window_seconds INTEGER,
			antigravity_quota TEXT
		);
		CREATE TABLE usage_records (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			timestamp DATETIME,
			source_account TEXT,
			source TEXT,
			auth_index TEXT,
			total_tokens INTEGER NOT NULL DEFAULT 0,
			failed BOOLEAN NOT NULL DEFAULT 0
		)`)
	if err != nil {
		db.Close()
		t.Fatalf("create schema: %v", err)
	}
	return db, path
}

type accountFixture struct {
	name          string
	authIndex     string
	email         string
	disabled      bool
	provider      string
	primaryUsed   *int64
	secondaryUsed *int64
	primaryReset  *time.Time
	secondaryRst  *time.Time
	primaryWin    *int64
	secondaryWin  *int64
	antiQuota     string
}

func insertAccount(t *testing.T, db *sql.DB, a accountFixture) {
	t.Helper()
	var primaryReset, secondaryReset interface{}
	if a.primaryReset != nil {
		primaryReset = backendApp.UsageDBTime(*a.primaryReset)
	}
	if a.secondaryRst != nil {
		secondaryReset = backendApp.UsageDBTime(*a.secondaryRst)
	}
	var provider interface{}
	if a.provider != "" {
		provider = a.provider
	}
	var authIndex interface{}
	if a.authIndex != "" {
		authIndex = a.authIndex
	}
	var quota interface{}
	if a.antiQuota != "" {
		quota = a.antiQuota
	}
	_, err := db.Exec(`INSERT INTO codex_keeper_auth_states
		(auth_name, auth_index, email, disabled, provider, primary_used_percent, secondary_used_percent,
		 primary_reset_at, secondary_reset_at, primary_window_seconds, secondary_window_seconds,
		 antigravity_quota)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`,
		a.name, authIndex, a.email, a.disabled, provider, a.primaryUsed, a.secondaryUsed,
		primaryReset, secondaryReset, a.primaryWin, a.secondaryWin, quota)
	if err != nil {
		t.Fatalf("insert account %s: %v", a.name, err)
	}
}

type usageFixture struct {
	at            time.Time
	sourceAccount string
	source        string
	authIndex     string
	tokens        int64
	failed        bool
}

func insertUsage(t *testing.T, db *sql.DB, u usageFixture) {
	t.Helper()
	var sourceAccount, source, authIndex interface{}
	if u.sourceAccount != "" {
		sourceAccount = u.sourceAccount
	}
	if u.source != "" {
		source = u.source
	}
	if u.authIndex != "" {
		authIndex = u.authIndex
	}
	_, err := db.Exec(`INSERT INTO usage_records
		(timestamp, source_account, source, auth_index, total_tokens, failed)
		VALUES (?,?,?,?,?,?)`,
		backendApp.UsageDBTime(u.at), sourceAccount, source, authIndex, u.tokens, u.failed)
	if err != nil {
		t.Fatalf("insert usage: %v", err)
	}
}

func i64(v int64) *int64 { return &v }

func TestOpenReadOnlyEnforcesQueryOnly(t *testing.T) {
	writable, path := newFixtureDB(t)
	if err := writable.Close(); err != nil {
		t.Fatalf("close writable: %v", err)
	}

	db, err := OpenReadOnly(context.Background(), path)
	if err != nil {
		t.Fatalf("OpenReadOnly: %v", err)
	}
	defer db.Close()

	if _, err := db.Exec(`INSERT INTO codex_keeper_auth_states (auth_name) VALUES ('x')`); err == nil {
		t.Fatal("write succeeded on read-only connection")
	}
}

func TestOpenReadOnlyMissingDB(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing.sqlite3")
	if _, err := OpenReadOnly(context.Background(), path); err == nil {
		t.Fatal("OpenReadOnly accepted a missing database file")
	}
}

// TestLoadUsageRowsSinceBoundIsDbTime is the mutation-teeth test for the
// timestamp comparison. The trap row stores the production '+08:00' layout with
// the same UTC calendar date as the bound but an instant two hours older: a
// buggy time.Time bound serialises as 'YYYY-MM-DD HH:MM:SS +0000 UTC', and
// ' ' < 'T' makes the lexicographic >= wrongly include that row. The fixed
// UsageDBTime bind ('T' separator) excludes it.
func TestLoadUsageRowsSinceBoundIsDbTime(t *testing.T) {
	writable, path := newFixtureDB(t)
	now := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)
	since := now.Add(-24 * time.Hour)
	// Stored literal: bound instant minus 2h, formatted at +08:00 so its UTC
	// calendar date matches the bound's -- the ' ' < 'T' same-date trap.
	stored := since.Add(-2 * time.Hour).In(time.FixedZone("Asia/Shanghai", 8*60*60)).Format("2006-01-02T15:04:05-07:00")
	if _, err := writable.Exec(`INSERT INTO usage_records (timestamp, source_account, total_tokens) VALUES ('` + stored + `','a@x.com',111)`); err != nil {
		t.Fatalf("insert trap row: %v", err)
	}
	insertUsage(t, writable, usageFixture{at: now.Add(-12 * time.Hour), sourceAccount: "a@x.com", tokens: 222})
	if err := writable.Close(); err != nil {
		t.Fatalf("close writable: %v", err)
	}

	db, err := OpenReadOnly(context.Background(), path)
	if err != nil {
		t.Fatalf("OpenReadOnly: %v", err)
	}
	defer db.Close()

	rows, err := LoadUsageRows(context.Background(), db, since)
	if err != nil {
		t.Fatalf("LoadUsageRows: %v", err)
	}
	if len(rows) != 1 || rows[0].TotalTokens != 222 {
		t.Fatalf("expected only the in-window row (222 tokens), got %+v", rows)
	}
}

func TestLoadAccountsNormalisesProviderAndParsesResets(t *testing.T) {
	writable, path := newFixtureDB(t)
	reset := time.Date(2026, 9, 21, 0, 0, 0, 0, time.FixedZone("Asia/Shanghai", 8*60*60))
	insertAccount(t, writable, accountFixture{
		name:         "codex-a@x.com",
		email:        "a@x.com",
		primaryUsed:  i64(40),
		primaryReset: &reset,
		primaryWin:   i64(18000),
	})
	insertAccount(t, writable, accountFixture{
		name:      "anti-b@x.com",
		email:     "b@x.com",
		provider:  "antigravity",
		antiQuota: `[{"display_name":"Gemini","buckets":[{"bucket_id":"b1","display_name":"Pro","window":"weekly","remaining_fraction":0.5,"reset_at":"2026-09-22T00:00:00+08:00"}]}]`,
	})
	if err := writable.Close(); err != nil {
		t.Fatalf("close writable: %v", err)
	}

	db, err := OpenReadOnly(context.Background(), path)
	if err != nil {
		t.Fatalf("OpenReadOnly: %v", err)
	}
	defer db.Close()

	accounts, err := LoadAccounts(context.Background(), db)
	if err != nil {
		t.Fatalf("LoadAccounts: %v", err)
	}
	if len(accounts) != 2 {
		t.Fatalf("expected 2 accounts, got %d", len(accounts))
	}
	var codex, anti Account
	for _, a := range accounts {
		switch a.Name {
		case "codex-a@x.com":
			codex = a
		case "anti-b@x.com":
			anti = a
		}
	}
	if codex.Provider != "codex" {
		t.Fatalf("NULL provider should normalise to codex, got %q", codex.Provider)
	}
	if codex.PrimaryResetAt == nil || !codex.PrimaryResetAt.Equal(reset) {
		t.Fatalf("primary reset not parsed: %+v", codex.PrimaryResetAt)
	}
	if anti.Provider != "antigravity" || len(anti.AntigravityGroups) != 1 {
		t.Fatalf("antigravity account not decoded: %+v", anti)
	}
	if got := anti.AntigravityGroups[0].Buckets[0].RemainingFraction; got != 0.5 {
		t.Fatalf("antigravity fraction = %v, want 0.5", got)
	}
}

func TestLoadAccountsMalformedQuotaYieldsNoGroups(t *testing.T) {
	writable, path := newFixtureDB(t)
	insertAccount(t, writable, accountFixture{
		name:      "anti-bad@x.com",
		provider:  "antigravity",
		antiQuota: `{not json`,
	})
	if err := writable.Close(); err != nil {
		t.Fatalf("close writable: %v", err)
	}
	db, err := OpenReadOnly(context.Background(), path)
	if err != nil {
		t.Fatalf("OpenReadOnly: %v", err)
	}
	defer db.Close()
	accounts, err := LoadAccounts(context.Background(), db)
	if err != nil {
		t.Fatalf("LoadAccounts: %v", err)
	}
	if len(accounts) != 1 || len(accounts[0].AntigravityGroups) != 0 {
		t.Fatalf("malformed quota should yield no groups: %+v", accounts)
	}
}

func TestParseArgsValidation(t *testing.T) {
	if _, err := ParseArgs([]string{"--since", "0"}, time.Now()); err == nil {
		t.Fatal("--since 0 accepted")
	}
	if _, err := ParseArgs([]string{"--provider", "claude"}, time.Now()); err == nil {
		t.Fatal("unknown provider accepted")
	}
	opts, err := ParseArgs([]string{"--since", "7", "--provider", "codex", "--json"}, time.Now())
	if err != nil {
		t.Fatalf("valid args rejected: %v", err)
	}
	if opts.Since != 7 || opts.Provider != "codex" || !opts.JSON {
		t.Fatalf("parsed options wrong: %+v", opts)
	}
}

// Sanity check that UsageDBTime emits the 'T'-separated layout the bound relies on.
func TestDbTimeLayout(t *testing.T) {
	ts := backendApp.UsageDBTime(time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC))
	want := fmt.Sprintf("2026-09-20T%02d:00:00", 12+8) // UTC+8
	if ts[:len(want)] != want {
		t.Fatalf("UsageDBTime layout unexpected: %q", ts)
	}
}

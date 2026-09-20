package accountrunway

import (
	"bytes"
	"context"
	"encoding/json"
	"math"
	"testing"
	"time"

	backendApp "cpa-helper/backend/internal/app"
)

var fixedNow = time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)

func codexAccount(name, email string, primaryUsed int64, reset time.Time, window int64) Account {
	return Account{
		Name:                 name,
		Email:                email,
		Provider:             "codex",
		PrimaryUsedPercent:   i64(primaryUsed),
		PrimaryResetAt:       &reset,
		PrimaryWindowSeconds: i64(window),
	}
}

// TestComputeBurnAttribution covers the production attribution order:
// source_account email wins, then email extracted from source, then auth_index;
// unmatched and ambiguous rows are counted as unattributed.
func TestComputeBurnAttribution(t *testing.T) {
	idx := codexAccount("idx-7", "b@x.com", 50, fixedNow.Add(2*time.Hour), 18000)
	idx.AuthIndex = "hash-zzz"
	accounts := []Account{
		codexAccount("acct-a@x.com", "a@x.com", 50, fixedNow.Add(2*time.Hour), 18000),
		idx,
	}
	rows := []UsageRow{
		{SourceAccount: "a@x.com", TotalTokens: 100},       // by source_account
		{Source: "cli user b@x.com tail", TotalTokens: 40}, // email from source
		{AuthIndex: "hash-zzz", TotalTokens: 20},           // via stored auth_index alias
		{SourceAccount: "ghost@x.com", TotalTokens: 999},   // no match
		{TotalTokens: 5}, // nothing to match on
		{SourceAccount: "A@X.COM", TotalTokens: 10, Failed: true}, // case + failed still counts
	}
	report := Compute(accounts, rows, "all", 1, fixedNow)

	byName := map[string]AccountReport{}
	for _, a := range report.Accounts {
		byName[a.Name] = a
	}
	if got := byName["acct-a@x.com"].BurnTokens; got != 110 {
		t.Fatalf("acct-a burn = %d, want 110 (100 + failed 10)", got)
	}
	if got := byName["idx-7"].BurnTokens; got != 60 {
		t.Fatalf("idx-7 burn = %d, want 60 (40 via source email + 20 via stored auth_index alias)", got)
	}
	if report.UnattributedRows != 2 || report.UnattributedTokens != 1004 {
		t.Fatalf("unattributed = %d rows / %d tokens, want 2 / 1004",
			report.UnattributedRows, report.UnattributedTokens)
	}
}

// TestComputeAmbiguousEmailUnattributed: two accounts sharing an email alias
// must not silently absorb burn — rows keyed to the shared email are
// unattributed.
func TestComputeAmbiguousEmailUnattributed(t *testing.T) {
	accounts := []Account{
		codexAccount("one", "shared@x.com", 50, fixedNow.Add(time.Hour), 18000),
		codexAccount("two", "shared@x.com", 50, fixedNow.Add(time.Hour), 18000),
	}
	rows := []UsageRow{{SourceAccount: "shared@x.com", TotalTokens: 77}}
	report := Compute(accounts, rows, "all", 1, fixedNow)
	if report.UnattributedRows != 1 || report.UnattributedTokens != 77 {
		t.Fatalf("ambiguous email should be unattributed: %+v", report)
	}
}

// TestComputeFractionOneIsCapUnknown guards the fraction==1 division: a bucket
// at 0% used has an indeterminate cap — UNKNOWN with cap_unknown, no Inf/NaN.
func TestComputeFractionOneIsCapUnknown(t *testing.T) {
	accounts := []Account{
		codexAccount("fresh@x.com", "fresh@x.com", 0, fixedNow.Add(3*time.Hour), 18000),
	}
	rows := []UsageRow{{SourceAccount: "fresh@x.com", TotalTokens: 500}}
	report := Compute(accounts, rows, "all", 1, fixedNow)

	b := report.Accounts[0].Buckets[0]
	if b.Status != StatusUnknown {
		t.Fatalf("status = %q, want UNKNOWN", b.Status)
	}
	if b.Cap != nil || b.RemainingTokens != nil || b.RunwayHours != nil {
		t.Fatalf("fraction==1 must not produce cap/runway numbers: %+v", b)
	}
	found := false
	for _, n := range b.Notes {
		if n == "cap_unknown" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected cap_unknown note, got %v", b.Notes)
	}
	if math.IsInf(b.RemainingFraction, 0) || math.IsNaN(b.RemainingFraction) {
		t.Fatal("fraction became Inf/NaN")
	}
	if report.Accounts[0].Status != StatusUnknown {
		t.Fatalf("account status = %q, want UNKNOWN", report.Accounts[0].Status)
	}
}

// TestComputeHealthyAndCritical: remaining tokens vs burn-until-reset decides
// status. Account "tight" burns so fast its bucket empties before reset.
func TestComputeHealthyAndCritical(t *testing.T) {
	reset := fixedNow.Add(4 * time.Hour)
	accounts := []Account{
		codexAccount("rich@x.com", "rich@x.com", 10, reset, 18000),   // 90% left
		codexAccount("tight@x.com", "tight@x.com", 95, reset, 18000), // 5% left
	}
	// 1-day window, both accounts burning the same absolute rate; the tight
	// account's remaining sliver is what turns CRITICAL.
	rows := []UsageRow{
		{SourceAccount: "rich@x.com", TotalTokens: 2400},
		{SourceAccount: "tight@x.com", TotalTokens: 2400},
	}
	report := Compute(accounts, rows, "all", 1, fixedNow)
	byName := map[string]AccountReport{}
	for _, a := range report.Accounts {
		byName[a.Name] = a
	}
	if byName["rich@x.com"].Status != StatusHealthy {
		t.Fatalf("rich should be HEALTHY, got %q (%+v)", byName["rich@x.com"].Status, byName["rich@x.com"].Buckets[0])
	}
	if byName["tight@x.com"].Status != StatusCritical {
		t.Fatalf("tight should be CRITICAL, got %q (%+v)", byName["tight@x.com"].Status, byName["tight@x.com"].Buckets[0])
	}
	// Sorted: critical first.
	if report.Accounts[0].Name != "tight@x.com" {
		t.Fatalf("critical account should sort first, got %q", report.Accounts[0].Name)
	}
	// Only the enabled critical account lands in pool.critical_accounts.
	found := false
	for _, n := range report.Pool.CriticalAccounts {
		if n == "tight@x.com" {
			found = true
		}
	}
	if !found {
		t.Fatalf("pool critical_accounts missing tight: %v", report.Pool.CriticalAccounts)
	}
	if report.Pool.GapTokens <= 0 {
		t.Fatalf("expected positive gap, got %v", report.Pool.GapTokens)
	}
}

// TestComputeDisabledExcludedFromGap: disabled accounts are counted but never
// contribute to the pool gap or additional-accounts recommendation.
func TestComputeDisabledExcludedFromGap(t *testing.T) {
	reset := fixedNow.Add(4 * time.Hour)
	disabled := codexAccount("dead@x.com", "dead@x.com", 95, reset, 18000)
	disabled.Disabled = true
	accounts := []Account{
		codexAccount("rich@x.com", "rich@x.com", 10, reset, 18000),
		disabled,
	}
	rows := []UsageRow{
		{SourceAccount: "rich@x.com", TotalTokens: 100},
		{SourceAccount: "dead@x.com", TotalTokens: 99999}, // huge burn, must not widen gap
	}
	report := Compute(accounts, rows, "all", 1, fixedNow)

	if report.Pool.DisabledAccounts != 1 || report.Pool.EnabledAccounts != 1 {
		t.Fatalf("pool counts wrong: %+v", report.Pool)
	}
	for _, n := range report.Pool.CriticalAccounts {
		if n == "dead@x.com" {
			t.Fatal("disabled account listed as critical")
		}
	}
	if report.Pool.AdditionalAccounts != 0 {
		t.Fatalf("disabled account influenced additional_accounts: %+v", report.Pool)
	}
	if report.Pool.GapTokens != 0 {
		t.Fatalf("disabled account contributed to gap: %v", report.Pool.GapTokens)
	}
}

// TestComputeProviderFilter keeps only the requested provider's accounts.
func TestComputeProviderFilter(t *testing.T) {
	accounts := []Account{
		codexAccount("c@x.com", "c@x.com", 50, fixedNow.Add(time.Hour), 18000),
		{Name: "a@x.com", Email: "a@x.com", Provider: "antigravity"},
	}
	report := Compute(accounts, nil, "codex", 1, fixedNow)
	if len(report.Accounts) != 1 || report.Accounts[0].Provider != "codex" {
		t.Fatalf("provider filter leaked accounts: %+v", report.Accounts)
	}
}

// TestComputeAntigravityBuckets: groups/buckets decode to named buckets;
// an unparseable window label is shown but excluded from runway math.
func TestComputeAntigravityBuckets(t *testing.T) {
	reset := fixedNow.Add(24 * time.Hour)
	account := Account{
		Name:     "anti@x.com",
		Email:    "anti@x.com",
		Provider: "antigravity",
		AntigravityGroups: []antigravityGroup{{
			DisplayName: "Gemini",
			Buckets: []antigravityBucket{
				{BucketID: "w", DisplayName: "Weekly", Window: "weekly", RemainingFraction: 0.8, ResetAt: &reset},
				{BucketID: "x", DisplayName: "Odd", Window: "fortnightly-ish", RemainingFraction: 0.5},
			},
		}},
	}
	report := Compute([]Account{account}, nil, "all", 1, fixedNow)
	buckets := report.Accounts[0].Buckets
	if len(buckets) != 2 {
		t.Fatalf("expected 2 buckets, got %+v", buckets)
	}
	if buckets[0].Name != "Gemini/Weekly" || buckets[0].WindowSeconds != 604800 {
		t.Fatalf("weekly bucket wrong: %+v", buckets[0])
	}
	if buckets[1].Status != StatusUnknown {
		t.Fatalf("unparseable window should be UNKNOWN, got %+v", buckets[1])
	}
	hasWindowNote := false
	for _, n := range buckets[1].Notes {
		if n == "window_unknown" {
			hasWindowNote = true
		}
	}
	if !hasWindowNote {
		t.Fatalf("expected window_unknown note, got %v", buckets[1].Notes)
	}
}

// TestRunAtGoldenJSON drives the full subcommand against a fixture DB with a
// fixed clock and asserts the decoded report fields.
func TestRunAtGoldenJSON(t *testing.T) {
	writable, path := newFixtureDB(t)
	reset := fixedNow.Add(4 * time.Hour)
	insertAccount(t, writable, accountFixture{
		name:         "solo@x.com",
		email:        "solo@x.com",
		provider:     "codex",
		primaryUsed:  i64(50),
		primaryReset: &reset,
		primaryWin:   i64(18000),
	})
	insertUsage(t, writable, usageFixture{at: fixedNow.Add(-time.Hour), sourceAccount: "solo@x.com", tokens: 1000})
	insertUsage(t, writable, usageFixture{at: fixedNow.Add(-30 * time.Hour), sourceAccount: "solo@x.com", tokens: 9999})
	if err := writable.Close(); err != nil {
		t.Fatalf("close writable: %v", err)
	}

	var out bytes.Buffer
	err := RunAt(context.Background(), []string{"--db", path, "--since", "1", "--json"}, &out, fixedNow)
	if err != nil {
		t.Fatalf("RunAt: %v", err)
	}
	var report Report
	if err := json.Unmarshal(out.Bytes(), &report); err != nil {
		t.Fatalf("output is not valid JSON: %v\n%s", err, out.String())
	}
	if report.GeneratedAt != backendApp.UsageDBTime(fixedNow) {
		t.Fatalf("generated_at = %q, want %q", report.GeneratedAt, backendApp.UsageDBTime(fixedNow))
	}
	if report.SinceDays != 1 || report.Provider != "all" {
		t.Fatalf("report meta wrong: %+v", report)
	}
	if len(report.Accounts) != 1 {
		t.Fatalf("expected 1 account, got %+v", report.Accounts)
	}
	acct := report.Accounts[0]
	if acct.Name != "solo@x.com" || acct.BurnTokens != 1000 {
		t.Fatalf("account wrong: %+v", acct)
	}
	if len(acct.Buckets) != 1 || acct.Buckets[0].Name != "primary" {
		t.Fatalf("buckets wrong: %+v", acct.Buckets)
	}
	if acct.Buckets[0].Cap == nil {
		t.Fatalf("expected a cap estimate: %+v", acct.Buckets[0])
	}
	if report.UnattributedRows != 0 {
		t.Fatalf("unexpected unattributed rows: %d", report.UnattributedRows)
	}
}

// TestRunAtTextOutput exercises the human-readable path end to end.
func TestRunAtTextOutput(t *testing.T) {
	writable, path := newFixtureDB(t)
	reset := fixedNow.Add(4 * time.Hour)
	insertAccount(t, writable, accountFixture{
		name:         "solo@x.com",
		email:        "solo@x.com",
		primaryUsed:  i64(50),
		primaryReset: &reset,
	})
	insertUsage(t, writable, usageFixture{at: fixedNow.Add(-time.Hour), sourceAccount: "solo@x.com", tokens: 100})
	if err := writable.Close(); err != nil {
		t.Fatalf("close writable: %v", err)
	}
	var out bytes.Buffer
	if err := RunAt(context.Background(), []string{"--db", path}, &out, fixedNow); err != nil {
		t.Fatalf("RunAt: %v", err)
	}
	text := out.String()
	for _, want := range []string{"ACCOUNT", "solo@x.com", "primary", "pool:"} {
		if !bytes.Contains(out.Bytes(), []byte(want)) {
			t.Fatalf("text output missing %q:\n%s", want, text)
		}
	}
}

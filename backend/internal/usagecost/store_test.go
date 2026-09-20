package usagecost

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
	"time"

	backendApp "cpa-helper/backend/internal/app"
)

// newFixtureDB writes a database with the columns this tool reads, using the
// same names and types as CPA-Helper's schema
// (backend/migrations/202605160001_initial_schema.sql).
func newFixtureDB(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "cpa_helper.sqlite3")
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`
		CREATE TABLE usage_records (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			created_at DATETIME NOT NULL,
			timestamp DATETIME NOT NULL,
			provider VARCHAR(120), model VARCHAR(180), endpoint VARCHAR(240),
			source_account VARCHAR(320),
			failed BOOLEAN NOT NULL DEFAULT 0,
			input_tokens INTEGER NOT NULL DEFAULT 0,
			output_tokens INTEGER NOT NULL DEFAULT 0,
			cached_tokens INTEGER NOT NULL DEFAULT 0,
			cache_read_tokens INTEGER NOT NULL DEFAULT 0,
			cache_creation_tokens INTEGER NOT NULL DEFAULT 0,
			reasoning_tokens INTEGER NOT NULL DEFAULT 0,
			total_tokens INTEGER NOT NULL DEFAULT 0,
			dedupe_key VARCHAR(80) NOT NULL UNIQUE,
			raw_json TEXT NOT NULL
		);
		CREATE TABLE model_prices (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			provider VARCHAR(120) NOT NULL, model VARCHAR(180) NOT NULL,
			input_usd_per_million REAL NOT NULL DEFAULT 0,
			output_usd_per_million REAL NOT NULL DEFAULT 0,
			cache_read_usd_per_million REAL NOT NULL DEFAULT 0,
			cache_creation_usd_per_million REAL NOT NULL DEFAULT 0,
			request_usd REAL,
			source VARCHAR(40) NOT NULL DEFAULT 'manual',
			updated_at DATETIME NOT NULL
		);`); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestOpenReadOnlyRefusesWrites(t *testing.T) {
	path := newFixtureDB(t)
	ctx := context.Background()
	db, err := OpenReadOnly(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	// This points at production data in real use, so the guard is asserted, not
	// assumed: a DSN change that drops mode=ro would otherwise go unnoticed
	// until the first write succeeded.
	_, err = db.ExecContext(ctx,
		`INSERT INTO model_prices (provider, model, updated_at) VALUES ('p','m','2026-09-20')`)
	if err == nil {
		t.Fatal("a write succeeded on a read-only connection")
	}
}

func TestLoadRecordsHonoursTheWindowAndNullDimensions(t *testing.T) {
	path := newFixtureDB(t)
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 9, 20, 4, 0, 0, 0, time.UTC)
	insert := func(key string, ts time.Time, provider, model, endpoint any, total int) {
		// Write timestamps via the SAME serialisation production uses. Binding
		// time.Time here would reproduce the reader's own byte shape, not the
		// database's -- which is exactly how the space-separator bound bug was
		// invisible to this test.
		dbTs := backendApp.UsageDBTime(ts)
		if _, err := db.Exec(`INSERT INTO usage_records
			(created_at, timestamp, provider, model, endpoint, source_account, failed, total_tokens, dedupe_key, raw_json)
			VALUES (?,?,?,?,?,?,0,?,?,'{}')`, dbTs, dbTs, provider, model, endpoint, "acct-"+key, total, key); err != nil {
			t.Fatal(err)
		}
	}
	insert("inside", now.Add(-1*time.Hour), "antigravity", "gemini", "/v1/chat", 10)
	insert("edge", now.Add(-48*time.Hour), "xai", "grok", "/v1/chat", 20)
	insert("outside", now.Add(-72*time.Hour), "devin", "swe", "/v1/chat", 40)
	insert("nulls", now.Add(-2*time.Hour), nil, nil, nil, 5)
	db.Close()

	ctx := context.Background()
	ro, err := OpenReadOnly(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer ro.Close()
	records, err := LoadRecords(ctx, ro, now.Add(-48*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	// The boundary row must be INCLUDED (>= since) and the older one excluded --
	// an off-by-one here shifts every number in the report with nothing to show
	// for it.
	if len(records) != 3 {
		t.Fatalf("records = %d, want 3: %+v", len(records), records)
	}
	var sawNulls bool
	for _, record := range records {
		if record.Provider == nil && record.Model == nil && record.Endpoint == nil {
			sawNulls = true
		}
	}
	if !sawNulls {
		t.Fatal("a row with NULL provider/model/endpoint must survive the load, not be dropped")
	}
}

// A record older than `since` in real time must be excluded even when its
// stored dbTime() string starts with the same UTC calendar date as the bound.
// This is the exact escape the space-separator bound allowed: the driver writes
// `since` as "2026-09-19 20:00:00 +0000 UTC" while production writes the record
// as "2026-09-20T13:00:00+08:00", and ' ' < 'T' keeps it in the window.
func TestLoadRecordsExcludesPreSinceRowStoredInDbTimeShape(t *testing.T) {
	path := newFixtureDB(t)
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	// 2026-09-20 20:00 +08:00 -- stored as "2026-09-20T20:00:00+08:00".
	since := time.Date(2026, 9, 20, 20, 0, 0, 0, time.FixedZone("Asia/Shanghai", 8*60*60))
	// 7h earlier in real time, same UTC calendar date as the buggy UTC bound.
	old := since.Add(-7 * time.Hour)
	for key, ts := range map[string]time.Time{"old": old, "new": since.Add(1 * time.Hour)} {
		dbTs := backendApp.UsageDBTime(ts)
		if _, err := db.Exec(`INSERT INTO usage_records
			(created_at, timestamp, provider, model, endpoint, failed, total_tokens, dedupe_key, raw_json)
			VALUES (?,?,?,?,?,0,1,?,'{}')`, dbTs, dbTs, "p", "m", "e", key); err != nil {
			t.Fatal(err)
		}
	}
	db.Close()

	ctx := context.Background()
	ro, err := OpenReadOnly(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer ro.Close()
	records, err := LoadRecords(ctx, ro, since)
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 {
		t.Fatalf("records = %d, want 1 -- the pre-since row leaked through the bound", len(records))
	}
}

func TestLoadPricesNormalisesKeysLikeCPAHelperDoes(t *testing.T) {
	path := newFixtureDB(t)
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO model_prices
		(provider, model, input_usd_per_million, request_usd, updated_at)
		VALUES ('  Antigravity ', ' Gemini-3.8-Flash ', 1.5, NULL, '2026-09-20'),
		       ('xai', 'grok-4.6', 0, 0.002, '2026-09-20')`); err != nil {
		t.Fatal(err)
	}
	db.Close()

	ctx := context.Background()
	ro, err := OpenReadOnly(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer ro.Close()
	prices, err := LoadPrices(ctx, ro)
	if err != nil {
		t.Fatal(err)
	}
	price, ok := prices[PriceKey{"antigravity", "gemini-3.8-flash"}]
	if !ok {
		t.Fatalf("key was not lowercased/trimmed: %+v", prices)
	}
	if price.InputUSDPerMillion != 1.5 {
		t.Fatalf("input price = %v", price.InputUSDPerMillion)
	}
	// NULL request_usd and 0 request_usd are different states: the first means
	// "not configured", the second means "configured as free". Collapsing them
	// changes which billing branch a record takes.
	if price.RequestUSD != nil {
		t.Fatalf("NULL request_usd must stay nil, got %v", *price.RequestUSD)
	}
	grok := prices[PriceKey{"xai", "grok-4.6"}]
	if grok.RequestUSD == nil || *grok.RequestUSD != 0.002 {
		t.Fatalf("request_usd = %v", grok.RequestUSD)
	}
}

func TestReportRefusesWhenNoPricingIsWired(t *testing.T) {
	// End to end through the real store: with no cost derivation available the
	// tool must fail, not print a table of zeros that reads like a $0 bill.
	path := newFixtureDB(t)
	ctx := context.Background()
	db, err := OpenReadOnly(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	prices, err := LoadPrices(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	records, err := LoadRecords(ctx, db, time.Unix(0, 0))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Aggregate(records, prices, GroupByModel, nil); err == nil ||
		!strings.Contains(err.Error(), "pricing implementation") {
		t.Fatalf("err = %v, want the no-pricing refusal", err)
	}
}

func TestLoadRecordsCarriesSourceAccountForAttribution(t *testing.T) {
	// artin asked for load per UNDERLYING account, which none of
	// model/provider/endpoint can answer: one model is served by several
	// accounts. The column has to survive the load for the dimension to exist.
	path := newFixtureDB(t)
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 9, 20, 4, 0, 0, 0, time.UTC)
	dbNow := backendApp.UsageDBTime(now)
	if _, err := db.Exec(`INSERT INTO usage_records
		(created_at, timestamp, provider, model, endpoint, source_account, failed, total_tokens, dedupe_key, raw_json)
		VALUES (?,?,'antigravity','gemini','/v1/chat','acct-a',0,10,'a','{}'),
		       (?,?,'antigravity','gemini','/v1/chat','acct-b',0,20,'b','{}'),
		       (?,?,'antigravity','gemini','/v1/chat',NULL,0,30,'c','{}')`,
		dbNow, dbNow, dbNow, dbNow, dbNow, dbNow); err != nil {
		t.Fatal(err)
	}
	db.Close()

	ctx := context.Background()
	ro, err := OpenReadOnly(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer ro.Close()
	records, err := LoadRecords(ctx, ro, now.Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	groups, err := Aggregate(records, map[PriceKey]ModelPrice{}, GroupBySourceAccount, fixedCost)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]int64{}
	for _, group := range groups {
		seen[group.Key] = group.TotalTokens
	}
	// Three rows that are identical on every other dimension must still split
	// three ways here -- otherwise the new flag is decorative.
	if seen["acct-a"] != 10 || seen["acct-b"] != 20 || seen[unattributed] != 30 {
		t.Fatalf("source-account buckets = %+v", seen)
	}
	// And the same rows must collapse to ONE group on a dimension they share,
	// which is what proves the split above came from source_account and not
	// from the rows differing somewhere else.
	byModel, err := Aggregate(records, map[PriceKey]ModelPrice{}, GroupByModel, fixedCost)
	if err != nil {
		t.Fatal(err)
	}
	if len(byModel) != 1 || byModel[0].Requests != 3 {
		t.Fatalf("by model = %+v, want one group of 3", byModel)
	}
}

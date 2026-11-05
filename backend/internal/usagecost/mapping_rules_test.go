package usagecost

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"math"
	"testing"
	"time"

	backendApp "cpa-helper/backend/internal/app"
)

// TestReportAppliesPriceMappingRules runs the subcommand against a database
// migrated by CPA-Helper itself. A devin/claude-* record has no price of its
// own; production prices it through the operator's rule onto Anthropic's
// price, so the report must too, instead of calling it unpriced.
func TestReportAppliesPriceMappingRules(t *testing.T) {
	t.Setenv("CPA_HELPER_DATA_DIR", t.TempDir())
	app, err := backendApp.New()
	if err != nil {
		t.Fatalf("New() failed: %v", err)
	}
	app.Close()
	path, err := backendApp.UsageDBPath()
	if err != nil {
		t.Fatal(err)
	}

	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	dbNow := backendApp.UsageDBTime(now)
	if _, err := db.Exec(`UPDATE app_settings SET model_price_mapping_rules = ? WHERE id = 1`,
		`[{"source_provider":"devin","source_model":"devin/claude-*","target_provider":"anthropic","target_model":"claude-*"}]`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO model_prices
		(provider, model, input_usd_per_million, output_usd_per_million, source, updated_at)
		VALUES ('anthropic', 'claude-opus-5-5', 4, 20, 'litellm', ?)`, dbNow); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO usage_records
		(created_at, timestamp, provider, model, failed, input_tokens, output_tokens, total_tokens, dedupe_key, raw_json)
		VALUES (?, ?, 'devin', 'devin/claude-opus-5-5', 0, 1000000, 1000000, 2000000, 'mapped', '{}'),
		       (?, ?, 'devin', 'devin/swe-2-flash', 0, 1000000, 0, 1000000, 'unmapped', '{}')`,
		dbNow, dbNow, dbNow, dbNow); err != nil {
		t.Fatal(err)
	}
	db.Close()

	var out bytes.Buffer
	if err := Run(context.Background(), []string{"--db", path, "--group-by", "model", "--since", "1", "--json"}, &out); err != nil {
		t.Fatalf("Run: %v", err)
	}
	var report Report
	if err := json.Unmarshal(out.Bytes(), &report); err != nil {
		t.Fatalf("decode report: %v\n%s", err, out.String())
	}
	groups := map[string]Group{}
	for _, group := range report.Groups {
		groups[group.Key] = group
	}
	mapped := groups["devin/claude-opus-5-5"]
	if mapped.UnpricedRequests != 0 || math.Abs(mapped.CostUSD-24) > 1e-9 {
		t.Fatalf("mapped model = %+v, want priced at Anthropic's $4 in + $20 out = $24", mapped)
	}
	// A model no rule covers stays unpriced: the rules must not leak onto it.
	if unmapped := groups["devin/swe-2-flash"]; unmapped.UnpricedRequests != 1 || unmapped.CostUSD != 0 {
		t.Fatalf("unmapped model = %+v, want unpriced", unmapped)
	}
}

func TestLoadMappingRulesWithoutSettingsRowIsEmpty(t *testing.T) {
	path := newFixtureDB(t)
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE app_settings (id INTEGER PRIMARY KEY, model_price_mapping_rules TEXT)`); err != nil {
		t.Fatal(err)
	}
	db.Close()

	ctx := context.Background()
	ro, err := OpenReadOnly(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer ro.Close()
	rules, err := LoadMappingRules(ctx, ro)
	if err != nil || len(rules) != 0 {
		t.Fatalf("rules = %+v, err = %v, want none", rules, err)
	}
}

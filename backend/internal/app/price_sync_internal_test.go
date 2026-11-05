package app

import (
	"context"
	"testing"
	"time"
)

func TestLiteLLMPriceSyncDue(t *testing.T) {
	t.Setenv("CPA_HELPER_DATA_DIR", t.TempDir())
	app, err := New()
	if err != nil {
		t.Fatalf("New() failed: %v", err)
	}
	defer app.Close()
	ctx := context.Background()
	now := time.Now().In(appTimeLocation)

	if count, due, err := app.liteLLMPriceSyncDue(ctx, now); err != nil || count != 0 || !due {
		t.Fatalf("empty table: count %d due %v err %v, want 0 true nil", count, due, err)
	}

	// Manual rows are not LiteLLM rows: they neither count nor make the table fresh.
	if _, err := app.db.Exec(`
		INSERT INTO model_prices (
			provider, model, input_usd_per_million, output_usd_per_million,
			cache_read_usd_per_million, cache_creation_usd_per_million, source, auto_synced, updated_at
		) VALUES ('kimi', 'manual-model', 4, 20, 0.2, 0, 'manual', 0, ?)
	`, dbTime(now)); err != nil {
		t.Fatalf("seed manual price: %v", err)
	}
	if count, due, err := app.liteLLMPriceSyncDue(ctx, now); err != nil || count != 0 || !due {
		t.Fatalf("manual only: count %d due %v err %v, want 0 true nil", count, due, err)
	}

	seed := func(model string, syncedAt time.Time) {
		t.Helper()
		if _, err := app.db.Exec(`
			INSERT INTO model_prices (
				provider, model, input_usd_per_million, output_usd_per_million,
				cache_read_usd_per_million, cache_creation_usd_per_million, source, auto_synced,
				last_synced_at, updated_at
			) VALUES ('openai', ?, 1, 5, 0.05, 0, 'litellm', 1, ?, ?)
		`, model, dbTime(syncedAt), dbTime(syncedAt)); err != nil {
			t.Fatalf("seed litellm price: %v", err)
		}
	}
	seed("old-model", now.Add(-48*time.Hour))
	if count, due, err := app.liteLLMPriceSyncDue(ctx, now); err != nil || count != 1 || !due {
		t.Fatalf("stale rows: count %d due %v err %v, want 1 true nil", count, due, err)
	}

	// The newest LiteLLM row decides freshness.
	seed("fresh-model", now.Add(-time.Hour))
	if count, due, err := app.liteLLMPriceSyncDue(ctx, now); err != nil || count != 2 || due {
		t.Fatalf("fresh rows: count %d due %v err %v, want 2 false nil", count, due, err)
	}
	if _, due, _ := app.liteLLMPriceSyncDue(ctx, now.Add(priceSyncInterval)); !due {
		t.Fatalf("rows synced a full interval ago should be due")
	}
}

func TestSyncLiteLLMPricesRejectsShrunkDownload(t *testing.T) {
	t.Setenv("CPA_HELPER_DATA_DIR", t.TempDir())
	app, err := New()
	if err != nil {
		t.Fatalf("New() failed: %v", err)
	}
	defer app.Close()

	now := dbTime(time.Now().In(appTimeLocation))
	if _, err := app.db.Exec(`
		INSERT INTO model_prices (
			provider, model, input_usd_per_million, output_usd_per_million,
			cache_read_usd_per_million, cache_creation_usd_per_million, source, auto_synced, updated_at
		) VALUES
			('openai', 'model-a', 1, 1, 0, 0, 'litellm', 1, ?),
			('openai', 'model-b', 1, 1, 0, 0, 'litellm', 1, ?),
			('openai', 'model-c', 1, 1, 0, 0, 'litellm', 1, ?),
			('openai', 'model-d', 1, 1, 0, 0, 'litellm', 1, ?)
	`, now, now, now, now); err != nil {
		t.Fatalf("seed prices: %v", err)
	}

	// One valid entry against a floor of two: the download looks broken, so nothing is written.
	rawData := map[string]any{
		"gpt-only-model": map[string]any{
			"litellm_provider":      "openai",
			"input_cost_per_token":  0.000001,
			"output_cost_per_token": 0.000005,
		},
		"not-a-price": "garbage",
	}
	if _, err := app.syncLiteLLMPrices(context.Background(), "https://example.com/prices.json", rawData, 2); err == nil {
		t.Fatalf("syncLiteLLMPrices accepted 1 valid row with minRows 2")
	}
	var count int
	if err := app.db.QueryRow(`SELECT COUNT(*) FROM model_prices WHERE source = 'litellm'`).Scan(&count); err != nil {
		t.Fatalf("count litellm rows: %v", err)
	}
	if count != 4 {
		t.Fatalf("litellm rows after rejected sync = %d, want 4 kept", count)
	}

	// The manual sync passes no floor, so an operator can still replace the table on purpose.
	if _, err := app.syncLiteLLMPrices(context.Background(), "https://example.com/prices.json", rawData, 0); err != nil {
		t.Fatalf("syncLiteLLMPrices with no floor failed: %v", err)
	}
	if err := app.db.QueryRow(`SELECT COUNT(*) FROM model_prices WHERE source = 'litellm'`).Scan(&count); err != nil {
		t.Fatalf("count litellm rows: %v", err)
	}
	if count != 1 {
		t.Fatalf("litellm rows after unguarded sync = %d, want 1", count)
	}
}

func TestPriceSyncRunnerHonorsDisableEnv(t *testing.T) {
	t.Setenv(priceSyncDisabledEnv, "1")
	runner := NewPriceSyncRunner(nil)
	runner.Start()
	if runner.done != nil {
		t.Fatalf("disabled runner started a loop")
	}
	runner.Stop()
}

// The price table also drives quota charges, so a sync changes what NEW usage deducts from a
// limited user's balance. Charges already recorded are not re-applied, and a manual price still
// wins over the synced LiteLLM price for the same model.
func TestPriceSyncChangesNewQuotaChargesOnly(t *testing.T) {
	t.Setenv("CPA_HELPER_DATA_DIR", t.TempDir())
	app, err := New()
	if err != nil {
		t.Fatalf("New() failed: %v", err)
	}
	defer app.Close()

	ctx := context.Background()
	userID := seedQuotaTestUser(t, app, "member")
	apiKey := "sk-quota-price-sync"
	seedQuotaTestAPIKey(t, app, userID, apiKey)
	seedQuotaTestPrice(t, app, "openai", "gpt-manual", 1)
	lifetime := 100.0
	if _, err := app.updateUserQuota(ctx, userID, userQuotaPayload{LifetimeQuotaUSD: &lifetime}); err != nil {
		t.Fatalf("update quota: %v", err)
	}
	usage := func(model, requestID string) {
		t.Helper()
		raw := `{"api_key":"` + apiKey + `","provider":"openai","model":"` + model + `","input_tokens":1000000,"request_id":"` + requestID + `"}`
		if _, created, err := app.saveUsageMessage(ctx, []byte(raw)); err != nil || !created {
			t.Fatalf("usage %s created=%v err=%v", requestID, created, err)
		}
	}
	charge := func(requestID string) (float64, bool) {
		t.Helper()
		var amount float64
		var unpriced bool
		if err := app.db.QueryRow(`
			SELECT c.amount_usd, c.unpriced FROM user_quota_charges c
			JOIN usage_records r ON r.id = c.usage_record_id WHERE r.request_id = ?
		`, requestID).Scan(&amount, &unpriced); err != nil {
			t.Fatalf("charge for %s: %v", requestID, err)
		}
		return amount, unpriced
	}

	usage("gpt-synced", "before-sync")
	if amount, unpriced := charge("before-sync"); amount != 0 || !unpriced {
		t.Fatalf("before sync: amount %v unpriced %v, want 0 true", amount, unpriced)
	}

	rawData := map[string]any{
		"gpt-synced": map[string]any{"litellm_provider": "openai", "input_cost_per_token": 0.000002},
		"gpt-manual": map[string]any{"litellm_provider": "openai", "input_cost_per_token": 0.000005},
	}
	if _, err := app.syncLiteLLMPrices(ctx, "https://example.com/prices.json", rawData, 0); err != nil {
		t.Fatalf("sync: %v", err)
	}

	usage("gpt-synced", "after-sync")
	usage("gpt-manual", "manual-after-sync")
	if amount, unpriced := charge("after-sync"); amount != 2 || unpriced {
		t.Fatalf("after sync: amount %v unpriced %v, want 2 false", amount, unpriced)
	}
	if amount, _ := charge("manual-after-sync"); amount != 1 {
		t.Fatalf("manual price: amount %v, want 1 (manual row, not the synced 5)", amount)
	}
	if amount, unpriced := charge("before-sync"); amount != 0 || !unpriced {
		t.Fatalf("recorded charge changed after sync: amount %v unpriced %v, want 0 true", amount, unpriced)
	}
	user, err := app.getUser(ctx, userID)
	if err != nil {
		t.Fatal(err)
	}
	if user.QuotaLifetimeUSD == nil || *user.QuotaLifetimeUSD != 97 {
		t.Fatalf("lifetime balance = %v, want 97 (100 - 2 - 1)", user.QuotaLifetimeUSD)
	}
}

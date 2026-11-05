package app

import (
	"context"
	"errors"
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

	if guard, due, err := app.liteLLMPriceSyncDue(ctx, now); err != nil || guard.minRows != 0 || guard.lastSynced != "" || !due {
		t.Fatalf("empty table: guard %+v due %v err %v, want floor 0, no last sync, due", guard, due, err)
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
	if guard, due, err := app.liteLLMPriceSyncDue(ctx, now); err != nil || guard.minRows != 0 || !due {
		t.Fatalf("manual only: guard %+v due %v err %v, want floor 0 and due", guard, due, err)
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
	if _, due, err := app.liteLLMPriceSyncDue(ctx, now); err != nil || !due {
		t.Fatalf("stale rows: due %v err %v, want due", due, err)
	}

	// The newest LiteLLM row decides freshness.
	seed("fresh-model", now.Add(-time.Hour))
	guard, due, err := app.liteLLMPriceSyncDue(ctx, now)
	if err != nil || due {
		t.Fatalf("fresh rows: due %v err %v, want not due", due, err)
	}
	if guard.minRows != 1 || guard.lastSynced != dbTime(now.Add(-time.Hour)) {
		t.Fatalf("fresh rows guard = %+v, want floor 1 (2 rows / 2) and the newest row's sync time", guard)
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
	// The seeded rows have no last_synced_at, so the guard's lastSynced is "".
	if _, err := app.syncLiteLLMPrices(context.Background(), "https://example.com/prices.json", rawData, &liteLLMAutoSyncGuard{minRows: 2}); err == nil {
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
	if _, err := app.syncLiteLLMPrices(context.Background(), "https://example.com/prices.json", rawData, nil); err != nil {
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
	if _, err := app.syncLiteLLMPrices(ctx, "https://example.com/prices.json", rawData, nil); err != nil {
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

// The daily sync decides to sync, then downloads. If a manual sync commits newer prices in the
// meantime, the daily sync's older download must not replace them.
func TestDailyPriceSyncYieldsToSyncDuringDownload(t *testing.T) {
	t.Setenv("CPA_HELPER_DATA_DIR", t.TempDir())
	app, err := New()
	if err != nil {
		t.Fatalf("New() failed: %v", err)
	}
	defer app.Close()
	ctx := context.Background()
	prices := func(inputPerToken float64) map[string]any {
		return map[string]any{"gpt-race": map[string]any{"litellm_provider": "openai", "input_cost_per_token": inputPerToken}}
	}
	inputPrice := func() float64 {
		t.Helper()
		var input float64
		if err := app.db.QueryRow(`SELECT input_usd_per_million FROM model_prices WHERE source = 'litellm' AND model = 'gpt-race'`).Scan(&input); err != nil {
			t.Fatalf("read price: %v", err)
		}
		return input
	}

	if _, err := app.syncLiteLLMPrices(ctx, "https://example.com/prices.json", prices(0.000001), nil); err != nil {
		t.Fatalf("initial sync: %v", err)
	}
	// The daily sync checks a day later and is due; its guard records what it saw.
	guard, due, err := app.liteLLMPriceSyncDue(ctx, time.Now().Add(priceSyncInterval+time.Minute))
	if err != nil || !due {
		t.Fatalf("daily check: due %v err %v, want due", due, err)
	}
	// While it downloads, an operator syncs newer prices by hand.
	time.Sleep(2 * time.Millisecond) // last_synced_at has microsecond precision; make it move
	if _, err := app.syncLiteLLMPrices(ctx, "https://example.com/prices.json", prices(0.000002), nil); err != nil {
		t.Fatalf("manual sync: %v", err)
	}
	// The daily sync now writes its older download with the guard from before.
	if _, err := app.syncLiteLLMPrices(ctx, "https://example.com/prices.json", prices(0.000001), guard); !errors.Is(err, errLiteLLMSyncSuperseded) {
		t.Fatalf("stale daily write err = %v, want errLiteLLMSyncSuperseded", err)
	}
	if got := inputPrice(); got != 2 {
		t.Fatalf("price after stale daily write = %v, want the manual sync's 2", got)
	}
	// Without an intervening sync the same guard still writes.
	guard, _, err = app.liteLLMPriceSyncDue(ctx, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := app.syncLiteLLMPrices(ctx, "https://example.com/prices.json", prices(0.000003), guard); err != nil {
		t.Fatalf("unraced daily write: %v", err)
	}
	if got := inputPrice(); got != 3 {
		t.Fatalf("price after unraced daily write = %v, want 3", got)
	}
}

package app

import (
	"context"
	"database/sql"
	"log"
	"os"
	"strings"
	"sync"
	"time"
)

const (
	// priceSyncInterval is how stale the LiteLLM price rows may get before they are refreshed.
	priceSyncInterval = 24 * time.Hour
	// priceSyncCheckInterval is how often the runner checks staleness; a failed sync is retried
	// at the next check.
	priceSyncCheckInterval = time.Hour
	// priceSyncStartDelay keeps the first check off the startup path (and out of short-lived
	// test apps, which also start background runners).
	priceSyncStartDelay = 5 * time.Minute
	// priceSyncDisabledEnv turns the daily sync off; the manual sync button keeps working.
	priceSyncDisabledEnv = "CPA_HELPER_PRICE_SYNC_DISABLED"
)

// PriceSyncRunner refreshes the LiteLLM-sourced rows of model_prices once a day, so models added
// to LiteLLM get priced without someone pressing the sync button.
type PriceSyncRunner struct {
	app  *App
	mu   sync.Mutex
	stop chan struct{}
	done chan struct{}
}

func NewPriceSyncRunner(app *App) *PriceSyncRunner {
	return &PriceSyncRunner{app: app}
}

func (r *PriceSyncRunner) Start() {
	if priceSyncDisabled() {
		log.Printf("daily LiteLLM price sync disabled by %s", priceSyncDisabledEnv)
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.done != nil {
		select {
		case <-r.done:
		default:
			return
		}
	}
	r.stop = make(chan struct{})
	r.done = make(chan struct{})
	go r.loop()
}

func (r *PriceSyncRunner) Stop() {
	r.mu.Lock()
	stop := r.stop
	done := r.done
	if stop == nil || done == nil {
		r.mu.Unlock()
		return
	}
	select {
	case <-stop:
	default:
		close(stop)
	}
	r.mu.Unlock()
	<-done
}

func (r *PriceSyncRunner) loop() {
	defer func() {
		r.mu.Lock()
		close(r.done)
		r.mu.Unlock()
	}()
	delay := priceSyncStartDelay
	for {
		timer := time.NewTimer(delay)
		select {
		case <-r.stop:
			timer.Stop()
			return
		case <-timer.C:
		}
		delay = priceSyncCheckInterval

		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		result, err := r.app.syncLiteLLMPricesIfDue(ctx, time.Now())
		cancel()
		if err != nil {
			log.Printf("daily LiteLLM price sync failed, retrying in %s: %v", priceSyncCheckInterval, err)
			continue
		}
		if result != nil {
			log.Printf("daily LiteLLM price sync imported %v prices (skipped %v manual, %v invalid)", result["imported"], result["skipped_manual"], result["skipped_invalid"])
		}
	}
}

func priceSyncDisabled() bool {
	value := strings.ToLower(strings.TrimSpace(os.Getenv(priceSyncDisabledEnv)))
	return value == "1" || value == "true" || value == "yes"
}

// syncLiteLLMPricesIfDue syncs from the default LiteLLM source when liteLLMPriceSyncDue says so.
// It returns nil, nil when the rows are still fresh.
func (a *App) syncLiteLLMPricesIfDue(ctx context.Context, now time.Time) (map[string]any, error) {
	count, due, err := a.liteLLMPriceSyncDue(ctx, now)
	if err != nil || !due {
		return nil, err
	}
	rawData, err := a.fetchLiteLLMPrices(ctx, defaultLiteLLMPricingURL)
	if err != nil {
		return nil, err
	}
	// Require at least half as many valid rows as are stored now, so a truncated or broken
	// download is rejected instead of replacing the table.
	return a.syncLiteLLMPrices(ctx, defaultLiteLLMPricingURL, rawData, count/2)
}

// liteLLMPriceSyncDue reports how many LiteLLM price rows are stored and whether they are due for
// a refresh: there are none, or the newest was synced at least priceSyncInterval ago.
func (a *App) liteLLMPriceSyncDue(ctx context.Context, now time.Time) (int, bool, error) {
	var count int
	var lastSynced sql.NullString
	if err := a.db.QueryRowContext(ctx, `
		SELECT COUNT(*), MAX(CAST(last_synced_at AS TEXT)) FROM model_prices WHERE source = 'litellm'
	`).Scan(&count, &lastSynced); err != nil {
		return 0, false, err
	}
	if count == 0 {
		return 0, true, nil
	}
	last, ok := parseDBTime(lastSynced.String)
	if !ok {
		return count, true, nil
	}
	return count, now.Sub(last) >= priceSyncInterval, nil
}

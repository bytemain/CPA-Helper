package app

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

// keeperTestIsResetCreditsCall reports whether an api-call proxies the
// rate-limit-reset-credits endpoint. It peeks the body and restores it so the
// handler can still decode the request afterward.
func keeperTestIsResetCreditsCall(r *http.Request) bool {
	if r.Body == nil {
		return false
	}
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		return false
	}
	r.Body = io.NopCloser(bytes.NewReader(raw))
	var payload struct {
		URL string `json:"url"`
	}
	_ = json.Unmarshal(raw, &payload)
	return strings.Contains(payload.URL, "rate-limit-reset-credits")
}

// keeperTestEmptyResetCreditsPayload is a valid, empty reset-credits api-call
// response (available_count 0, no credits).
func keeperTestEmptyResetCreditsPayload() map[string]any {
	return map[string]any{"status_code": 200, "body": map[string]any{"available_count": 0, "credits": []any{}}}
}

func TestKeeperUsageTimeoutDefaultIsThirtyButExistingValueIsPreserved(t *testing.T) {
	cfg, err := defaultConfig()
	if err != nil {
		t.Fatalf("defaultConfig: %v", err)
	}
	if cfg.CodexKeeper.UsageTimeoutSeconds != 30 {
		t.Fatalf("default usage_timeout_seconds = %d, want 30", cfg.CodexKeeper.UsageTimeoutSeconds)
	}
	normalized := normalizeKeeperConfig(KeeperConfig{UsageTimeoutSeconds: 15})
	if normalized.UsageTimeoutSeconds != 15 {
		t.Fatalf("normalized existing usage_timeout_seconds = %d, want 15", normalized.UsageTimeoutSeconds)
	}
}

func TestKeeperRequestRetriesTransientManagementFailures(t *testing.T) {
	attempts := 0
	cpa := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		w.Header().Set("Content-Type", "application/json")
		if attempts <= 2 {
			http.Error(w, "temporary failure", http.StatusBadGateway)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]bool{"ok": true})
	}))
	defer cpa.Close()

	cfg := AppConfig{
		Collector: CollectorConfig{
			CLIProxyURL:   cpa.URL,
			ManagementKey: "test-management-key",
		},
		CodexKeeper: KeeperConfig{MaxRetries: 2},
	}
	_, payload, err := (&App{}).keeperRequest(context.Background(), cfg, http.MethodGet, "/v0/management/auth-files", nil, nil, time.Second)
	if err != nil {
		t.Fatalf("keeperRequest: %v", err)
	}
	if attempts != 3 {
		t.Fatalf("attempts = %d, want 3", attempts)
	}
	if !strings.Contains(string(payload), `"ok":true`) {
		t.Fatalf("payload = %s, want ok response", payload)
	}
}

func TestKeeperRequestDoesNotRetryManagementClientErrors(t *testing.T) {
	attempts := 0
	cpa := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		http.Error(w, "bad key", http.StatusUnauthorized)
	}))
	defer cpa.Close()

	cfg := AppConfig{
		Collector: CollectorConfig{
			CLIProxyURL:   cpa.URL,
			ManagementKey: "test-management-key",
		},
		CodexKeeper: KeeperConfig{MaxRetries: 2},
	}
	_, _, err := (&App{}).keeperRequest(context.Background(), cfg, http.MethodGet, "/v0/management/auth-files", nil, nil, time.Second)
	if err == nil {
		t.Fatal("keeperRequest error is nil, want HTTP 401 error")
	}
	if attempts != 1 {
		t.Fatalf("attempts = %d, want 1", attempts)
	}
}

func TestConditionalKeeperRefreshCandidatesUseUsageQuotaAndCache(t *testing.T) {
	t.Setenv("CPA_HELPER_DATA_DIR", t.TempDir())
	app, err := New()
	if err != nil {
		t.Fatalf("New() failed: %v", err)
	}
	defer app.Close()

	ctx := context.Background()
	cfg, err := app.loadConfig(ctx)
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	cfg.CodexKeeper.AccountRefreshCacheMinutes = 10
	remoteDetails := map[string]map[string]any{
		"remote-detail-email.json": {
			"name":  "remote-detail-email.json",
			"type":  "codex",
			"email": "remote@example.com",
		},
		"remote-short-index.json": {
			"name":       "remote-short-index.json",
			"type":       "codex",
			"auth_index": "short-auth-index",
		},
		"remote-list-email.json": {
			"name":  "remote-list-email.json",
			"type":  "codex",
			"email": "list@example.com",
		},
		"remote-disabled-detail-email.json": {
			"name":     "remote-disabled-detail-email.json",
			"type":     "codex",
			"email":    "disabled-detail@example.com",
			"disabled": true,
		},
	}
	cpa := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v0/management/auth-files":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"files": []map[string]any{
					{"name": "remote-detail-email.json", "type": "codex"},
					{"name": "remote-short-index.json", "type": "codex"},
					{"name": "remote-list-email.json", "type": "codex", "email": "list@example.com"},
					{"name": "remote-disabled-list-email.json", "type": "codex", "email": "disabled-list@example.com", "disabled": true},
					{"name": "remote-disabled-detail-email.json", "type": "codex"},
					{"name": "email-match.json", "type": "codex"},
					{"name": "source-match.json", "type": "codex"},
					{"name": "cached-request.json", "type": "codex"},
					{"name": "cached-email.json", "type": "codex"},
					{"name": "quota-due.json", "type": "codex"},
					{"name": "quota-future.json", "type": "codex"},
					{"name": "quota-cached.json", "type": "codex"},
					{"name": "error-due.json", "type": "codex"},
					{"name": "error-cached.json", "type": "codex"},
					{"name": "normal-local.json", "type": "codex"},
				},
			})
		case r.Method == http.MethodGet && r.URL.Path == "/v0/management/auth-files/download":
			detail, ok := remoteDetails[r.URL.Query().Get("name")]
			if !ok {
				http.NotFound(w, r)
				return
			}
			_ = json.NewEncoder(w).Encode(detail)
		default:
			http.NotFound(w, r)
		}
	}))
	defer cpa.Close()
	cfg.Collector.CLIProxyURL = cpa.URL
	cfg.Collector.ManagementKey = "test-management-key"
	now := time.Now().In(appTimeLocation)

	insertKeeperUsageRecord(t, app, "active-request", now.Add(-time.Minute), `{"auth_index":"active-request.json","failed":true}`)
	insertKeeperUsageRecord(t, app, "email-request", now.Add(-time.Minute), `{"auth_index":"person@example.com"}`)
	insertKeeperUsageRecord(t, app, "source-email-request", now.Add(-time.Minute), `{"source":"source@example.com","auth_index":"source-short-index"}`)
	insertKeeperUsageRecord(t, app, "remote-detail-email-request", now.Add(-time.Minute), `{"auth_index":"remote@example.com"}`)
	insertKeeperUsageRecord(t, app, "remote-list-email-request", now.Add(-time.Minute), `{"auth_index":"list@example.com"}`)
	insertKeeperUsageRecord(t, app, "remote-short-index-request", now.Add(-time.Minute), `{"auth_index":"short-auth-index"}`)
	insertKeeperUsageRecord(t, app, "remote-disabled-list-email-request", now.Add(-time.Minute), `{"auth_index":"disabled-list@example.com"}`)
	insertKeeperUsageRecord(t, app, "remote-disabled-detail-email-request", now.Add(-time.Minute), `{"auth_index":"disabled-detail@example.com"}`)
	insertKeeperUsageRecord(t, app, "unknown-email-request", now.Add(-time.Minute), `{"auth_index":"unknown@example.com"}`)
	insertKeeperUsageRecord(t, app, "old-request", now.Add(-20*time.Minute), `{"auth_index":"old-request.json"}`)
	insertKeeperUsageRecord(t, app, "cached-request", now.Add(-time.Minute), `{"auth_index":"cached-request.json"}`)
	insertKeeperUsageRecord(t, app, "cached-email-request", now.Add(-time.Minute), `{"auth_index":"cached@example.com"}`)
	insertKeeperUsageRecord(t, app, "no-auth-index", now.Add(-time.Minute), `{"request_id":"missing-auth-index"}`)

	insertKeeperStateForCandidateWithEmail(t, app, "email-match.json", stringPtr("person@example.com"), nil, nil)
	insertKeeperStateForCandidateWithEmail(t, app, "source-match.json", stringPtr("source@example.com"), nil, nil)
	insertKeeperStateForCandidate(t, app, "cached-request.json", nil, timePtrValue(now.Add(-2*time.Minute)))
	insertKeeperStateForCandidateWithEmail(t, app, "cached-email.json", stringPtr("cached@example.com"), nil, timePtrValue(now.Add(-2*time.Minute)))
	insertKeeperStateForCandidate(t, app, "quota-due.json", timePtrValue(now.Add(-time.Minute)), nil)
	insertKeeperStateForCandidate(t, app, "quota-future.json", timePtrValue(now.Add(time.Minute)), nil)
	insertKeeperStateForCandidate(t, app, "quota-cached.json", timePtrValue(now.Add(-time.Minute)), timePtrValue(now.Add(-2*time.Minute)))
	insertKeeperStateForCandidateWithError(t, app, "error-due.json", "network check failed", timePtrValue(now.Add(-20*time.Minute)))
	insertKeeperStateForCandidateWithError(t, app, "error-cached.json", "network check failed", timePtrValue(now.Add(-2*time.Minute)))
	insertKeeperStateForCandidate(t, app, "normal-local.json", nil, timePtrValue(now.Add(-20*time.Minute)))

	names, err := app.conditionalKeeperRefreshCandidates(ctx, cfg)
	if err != nil {
		t.Fatalf("conditionalKeeperRefreshCandidates: %v", err)
	}
	assertStringSet(t, names, []string{
		"active-request.json",
		"email-match.json",
		"source-match.json",
		"remote-detail-email.json",
		"remote-list-email.json",
		"remote-short-index.json",
		"quota-due.json",
		"error-due.json",
	})
}

func TestConditionalKeeperRefreshCandidatesSkipDisabledLocalAccounts(t *testing.T) {
	t.Setenv("CPA_HELPER_DATA_DIR", t.TempDir())
	app, err := New()
	if err != nil {
		t.Fatalf("New() failed: %v", err)
	}
	defer app.Close()

	ctx := context.Background()
	cfg, err := app.loadConfig(ctx)
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	cfg.Collector.CLIProxyURL = ""
	cfg.CodexKeeper.AccountRefreshCacheMinutes = 10
	now := time.Now().In(appTimeLocation)

	insertKeeperUsageRecord(t, app, "enabled-request", now.Add(-time.Minute), `{"auth_index":"enabled-request.json","failed":true}`)
	insertKeeperUsageRecord(t, app, "disabled-request", now.Add(-time.Minute), `{"auth_index":"disabled-request.json","failed":true}`)
	insertKeeperUsageRecord(t, app, "disabled-email-request", now.Add(-time.Minute), `{"auth_index":"disabled@example.com","failed":true}`)

	insertKeeperStateForCandidate(t, app, "disabled-request.json", nil, nil)
	markKeeperStateDisabled(t, app, "disabled-request.json")
	insertKeeperStateForCandidateWithEmail(t, app, "disabled-email.json", stringPtr("disabled@example.com"), nil, nil)
	markKeeperStateDisabled(t, app, "disabled-email.json")
	insertKeeperStateForCandidate(t, app, "enabled-quota.json", timePtrValue(now.Add(-time.Minute)), nil)
	insertKeeperStateForCandidate(t, app, "disabled-quota.json", timePtrValue(now.Add(-time.Minute)), nil)
	markKeeperStateDisabled(t, app, "disabled-quota.json")
	insertKeeperStateForCandidateWithError(t, app, "enabled-error.json", "network check failed", timePtrValue(now.Add(-20*time.Minute)))
	insertKeeperStateForCandidateWithError(t, app, "disabled-error.json", "network check failed", timePtrValue(now.Add(-20*time.Minute)))
	markKeeperStateDisabled(t, app, "disabled-error.json")

	names, err := app.conditionalKeeperRefreshCandidates(ctx, cfg)
	if err != nil {
		t.Fatalf("conditionalKeeperRefreshCandidates: %v", err)
	}
	assertStringSet(t, names, []string{
		"enabled-request.json",
		"enabled-quota.json",
		"enabled-error.json",
	})
}

func TestConditionalKeeperRefreshCandidatesReconcileRemoteAuthStates(t *testing.T) {
	t.Setenv("CPA_HELPER_DATA_DIR", t.TempDir())

	cpa := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v0/management/auth-files":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"files": []map[string]any{
					{"name": "kept.json", "type": "codex"},
					{"name": "new-remote.json", "type": "codex"},
					{"name": "disabled-remote.json", "type": "codex", "disabled": true},
					{"name": "not-codex.json", "type": "other"},
				},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer cpa.Close()

	app, err := New()
	if err != nil {
		t.Fatalf("New() failed: %v", err)
	}
	defer app.Close()

	ctx := context.Background()
	cfg, err := app.loadConfig(ctx)
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	cfg.Collector.CLIProxyURL = cpa.URL
	cfg.Collector.ManagementKey = "test-management-key"
	cfg.CodexKeeper.AccountRefreshCacheMinutes = 10

	insertKeeperStateForCandidate(t, app, "kept.json", nil, nil)
	insertKeeperStateForCandidate(t, app, "stale-local.json", nil, nil)

	names, err := app.conditionalKeeperRefreshCandidates(ctx, cfg)
	if err != nil {
		t.Fatalf("conditionalKeeperRefreshCandidates: %v", err)
	}
	assertStringSet(t, names, []string{"new-remote.json"})
	if got := countKeeperRows(t, app, `SELECT COUNT(*) FROM codex_keeper_auth_states WHERE auth_name = 'kept.json'`); got != 1 {
		t.Fatalf("kept state rows = %d, want 1", got)
	}
	if got := countKeeperRows(t, app, `SELECT COUNT(*) FROM codex_keeper_auth_states WHERE auth_name = 'stale-local.json'`); got != 0 {
		t.Fatalf("stale state rows = %d, want 0", got)
	}
}

func TestKeeperQuotaWindowUsageAttributionPrefersSourceAccount(t *testing.T) {
	t.Setenv("CPA_HELPER_DATA_DIR", t.TempDir())
	app, err := New()
	if err != nil {
		t.Fatalf("New() failed: %v", err)
	}
	defer app.Close()

	now := time.Date(2026, 5, 18, 12, 30, 0, 0, appTimeLocation)
	resetAt := now.Add(30 * time.Minute)
	windowSeconds := 3600
	accounts := []keeperAccount{
		{
			Name:                 "source.json",
			Email:                stringPtr("source@example.com"),
			AccountType:          stringPtr("plus"),
			PrimaryResetAt:       timePtrValue(resetAt),
			PrimaryWindowSeconds: intPtrValue(windowSeconds),
		},
		{
			Name:                 "auth.json",
			Email:                stringPtr("auth@example.com"),
			AccountType:          stringPtr("plus"),
			PrimaryResetAt:       timePtrValue(resetAt),
			PrimaryWindowSeconds: intPtrValue(windowSeconds),
		},
	}
	insertKeeperWindowUsageRecord(t, app, keeperWindowUsageSeed{
		Dedupe:       "source-wins",
		Timestamp:    now.Add(-10 * time.Minute),
		Source:       "source@example.com",
		AuthIndex:    "auth.json",
		InputTokens:  11,
		OutputTokens: 7,
		RawJSON:      `{"source":"source@example.com","auth_index":"auth.json"}`,
	})
	insertKeeperWindowUsageRecord(t, app, keeperWindowUsageSeed{
		Dedupe:       "auth-fallback",
		Timestamp:    now.Add(-5 * time.Minute),
		Source:       "queue",
		AuthIndex:    "auth.json",
		InputTokens:  13,
		OutputTokens: 9,
		RawJSON:      `{"auth_index":"auth.json"}`,
	})
	insertKeeperWindowUsageRecord(t, app, keeperWindowUsageSeed{
		Dedupe:       "unknown",
		Timestamp:    now.Add(-4 * time.Minute),
		Source:       "unknown@example.com",
		AuthIndex:    "auth.json",
		InputTokens:  17,
		OutputTokens: 3,
		RawJSON:      `{"source":"unknown@example.com","auth_index":"auth.json"}`,
	})

	usages, err := app.computeKeeperQuotaWindowUsages(context.Background(), accounts, now)
	if err != nil {
		t.Fatalf("compute window usages: %v", err)
	}
	if got := usages["source.json"].Primary.Records; got != 1 {
		t.Fatalf("source account records = %d, want 1", got)
	}
	if got := usages["source.json"].Primary.TotalTokens; got != 18 {
		t.Fatalf("source account tokens = %d, want 18", got)
	}
	if got := usages["auth.json"].Primary.Records; got != 1 {
		t.Fatalf("auth fallback records = %d, want 1", got)
	}
}

func TestKeeperQuotaWindowUsageAttributionUsesAuthIndexWhenSourceAccountIsShared(t *testing.T) {
	t.Setenv("CPA_HELPER_DATA_DIR", t.TempDir())
	app, err := New()
	if err != nil {
		t.Fatalf("New() failed: %v", err)
	}
	defer app.Close()

	now := time.Date(2026, 5, 18, 12, 30, 0, 0, appTimeLocation)
	resetAt := now.Add(time.Hour)
	windowSeconds := keeperFiveHourWindowSeconds
	accounts := []keeperAccount{
		{
			Name:                 "shared-one.json",
			Email:                stringPtr("shared@example.com"),
			AuthIndex:            stringPtr("auth-one"),
			AccountType:          stringPtr("k12"),
			PrimaryResetAt:       timePtrValue(resetAt),
			PrimaryWindowSeconds: intPtrValue(windowSeconds),
		},
		{
			Name:                 "shared-two.json",
			Email:                stringPtr("shared@example.com"),
			AuthIndex:            stringPtr("auth-two"),
			AccountType:          stringPtr("k12"),
			PrimaryResetAt:       timePtrValue(resetAt),
			PrimaryWindowSeconds: intPtrValue(windowSeconds),
		},
	}
	insertKeeperWindowUsageRecord(t, app, keeperWindowUsageSeed{
		Dedupe:       "shared-one",
		Timestamp:    now.Add(-10 * time.Minute),
		Source:       "shared@example.com",
		AuthIndex:    "auth-one",
		InputTokens:  11,
		OutputTokens: 7,
		RawJSON:      `{"source":"shared@example.com","auth_index":"auth-one"}`,
	})
	insertKeeperWindowUsageRecord(t, app, keeperWindowUsageSeed{
		Dedupe:       "shared-two",
		Timestamp:    now.Add(-5 * time.Minute),
		Source:       "shared@example.com",
		AuthIndex:    "auth-two",
		InputTokens:  13,
		OutputTokens: 9,
		RawJSON:      `{"source":"shared@example.com","auth_index":"auth-two"}`,
	})

	usages, err := app.computeKeeperQuotaWindowUsages(context.Background(), accounts, now)
	if err != nil {
		t.Fatalf("compute window usages: %v", err)
	}
	if got := usages["shared-one.json"].Primary.Records; got != 1 {
		t.Fatalf("shared-one records = %d, want 1", got)
	}
	if got := usages["shared-one.json"].Primary.TotalTokens; got != 18 {
		t.Fatalf("shared-one tokens = %d, want 18", got)
	}
	if got := usages["shared-two.json"].Primary.Records; got != 1 {
		t.Fatalf("shared-two records = %d, want 1", got)
	}
	if got := usages["shared-two.json"].Primary.TotalTokens; got != 22 {
		t.Fatalf("shared-two tokens = %d, want 22", got)
	}
}

func TestAccountTypeFromKeeperDetailNormalizesCodexProPlans(t *testing.T) {
	tests := []struct {
		name   string
		detail map[string]any
		usage  *keeperUsageInfo
		want   string
	}{
		{
			name:  "usage pro is pro 20x",
			usage: &keeperUsageInfo{PlanType: "pro"},
			want:  "pro_20x",
		},
		{
			name:  "usage prolite is pro 5x",
			usage: &keeperUsageInfo{PlanType: "prolite"},
			want:  "pro_5x",
		},
		{
			name:   "top level pro-lite is pro 5x",
			detail: map[string]any{"plan_type": "pro-lite"},
			want:   "pro_5x",
		},
		{
			name:   "nested id token plan is used",
			detail: map[string]any{"id_token": map[string]any{"plan_type": "pro_lite"}},
			want:   "pro_5x",
		},
		{
			name:  "usage k12 is k12",
			usage: &keeperUsageInfo{PlanType: "k12"},
			want:  "k12",
		},
		{
			name:   "nested id token k12 is used",
			detail: map[string]any{"id_token": map[string]any{"plan_type": "k12"}},
			want:   "k12",
		},
		{
			name:   "attributes plan is used",
			detail: map[string]any{"attributes": map[string]any{"plan_type": "pro"}},
			want:   "pro_20x",
		},
		{
			name:   "jwt chatgpt plan is used",
			detail: map[string]any{"metadata": map[string]any{"id_token": keeperTestCodexJWT(t, "prolite")}},
			want:   "pro_5x",
		},
		{
			name:   "pro file name suffix is fallback",
			detail: map[string]any{"name": "codex-user@example.com-pro.json"},
			want:   "pro_20x",
		},
		{
			name:   "prolite file name suffix is fallback",
			detail: map[string]any{"name": "codex-user@example.com-prolite.json"},
			want:   "pro_5x",
		},
		{
			name:   "legacy pro_20x remains supported",
			detail: map[string]any{"account_type": "pro_20x"},
			want:   "pro_20x",
		},
		{
			name:   "legacy pro_5x remains supported",
			detail: map[string]any{"account_type": "pro_5x"},
			want:   "pro_5x",
		},
		{
			name:   "plus remains supported",
			detail: map[string]any{"plan_type": "plus"},
			want:   "plus",
		},
		{
			name:   "unknown becomes unknown",
			detail: map[string]any{"name": "codex-user@example.com.json"},
			want:   "unknown",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := accountTypeFromKeeperDetail(tt.detail, tt.usage)
			if got == nil || *got != tt.want {
				t.Fatalf("accountTypeFromKeeperDetail() = %v, want %q", got, tt.want)
			}
		})
	}
}

func TestDefaultKeeperPriorityRulesIncludeK12AndUnknown(t *testing.T) {
	rules := normalizePriorityRules(nil)
	if rules["k12"] != 2 {
		t.Fatalf("k12 priority = %d, want 2", rules["k12"])
	}
	if rules["unknown"] != 1 {
		t.Fatalf("unknown priority = %d, want 1", rules["unknown"])
	}
}

func TestKeeperRunStoresRemoteAuthIndex(t *testing.T) {
	t.Setenv("CPA_HELPER_DATA_DIR", t.TempDir())

	cpa := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v0/management/auth-files":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"files": []map[string]any{{"name": "stored.json", "type": "codex", "auth_index": "stored-auth"}},
			})
		case r.Method == http.MethodGet && r.URL.Path == "/v0/management/auth-files/download":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"name":         "stored.json",
				"type":         "codex",
				"email":        "stored@example.com",
				"auth_index":   "stored-auth",
				"account_type": "free",
				"disabled":     false,
				"priority":     0,
				"access_token": "test-token",
			})
		case r.Method == http.MethodPost && r.URL.Path == "/v0/management/api-call":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"status_code": 200,
				"body": map[string]any{
					"plan_type": "free",
					"rate_limit": map[string]any{
						"primary_window": map[string]any{
							"used_percent":        10,
							"reset_after_seconds": 3600,
						},
					},
				},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer cpa.Close()

	app, err := New()
	if err != nil {
		t.Fatalf("New() failed: %v", err)
	}
	defer app.Close()
	configureKeeperTestCPA(t, app, cpa.URL, nil)

	if _, _, err := app.executeKeeperRunForAccounts(context.Background(), "manual", []string{"stored.json"}, func(string) {}); err != nil {
		t.Fatalf("keeper run: %v", err)
	}
	state, err := app.getKeeperState(context.Background(), "stored.json")
	if err != nil {
		t.Fatalf("get keeper state: %v", err)
	}
	if state.AuthIndex == nil || *state.AuthIndex != "stored-auth" {
		t.Fatalf("auth_index = %v, want stored-auth", state.AuthIndex)
	}
}

func keeperTestCodexJWT(t *testing.T, planType string) string {
	t.Helper()
	payload, err := json.Marshal(map[string]any{
		"https://api.openai.com/auth": map[string]any{
			"chatgpt_plan_type": planType,
		},
	})
	if err != nil {
		t.Fatalf("marshal test jwt payload: %v", err)
	}
	return "header." + base64.RawURLEncoding.EncodeToString(payload) + ".signature"
}

func TestKeeperQuotaWindowUsageInfersAccountWindows(t *testing.T) {
	now := time.Date(2026, 5, 18, 12, 30, 0, 0, appTimeLocation)
	resetAt := now.Add(30 * time.Minute)

	freePair := keeperQuotaWindowPairForAccount(keeperAccount{
		Name:           "free.json",
		AccountType:    stringPtr("free"),
		PrimaryResetAt: timePtrValue(resetAt),
	}, now)
	if freePair.Primary == nil {
		t.Fatal("free primary window is nil, want monthly window")
	}
	if freePair.Primary.WindowSeconds != keeperMonthWindowSeconds || freePair.Primary.WindowSource != "inferred" {
		t.Fatalf("free window = %d/%s, want inferred monthly", freePair.Primary.WindowSeconds, freePair.Primary.WindowSource)
	}
	if freePair.Secondary != nil {
		t.Fatal("free secondary window is not nil, want single monthly window")
	}

	plusPair := keeperQuotaWindowPairForAccount(keeperAccount{
		Name:             "plus.json",
		AccountType:      stringPtr("plus"),
		PrimaryResetAt:   timePtrValue(resetAt),
		SecondaryResetAt: timePtrValue(resetAt.Add(2 * time.Hour)),
	}, now)
	if plusPair.Primary == nil || plusPair.Primary.WindowSeconds != keeperFiveHourWindowSeconds {
		t.Fatalf("plus primary window = %#v, want inferred 5h", plusPair.Primary)
	}
	if plusPair.Secondary == nil || plusPair.Secondary.WindowSeconds != keeperWeekWindowSeconds {
		t.Fatalf("plus secondary window = %#v, want inferred weekly", plusPair.Secondary)
	}

	k12Pair := keeperQuotaWindowPairForAccount(keeperAccount{
		Name:             "k12.json",
		AccountType:      stringPtr("k12"),
		PrimaryResetAt:   timePtrValue(resetAt),
		SecondaryResetAt: timePtrValue(resetAt.Add(2 * time.Hour)),
	}, now)
	if k12Pair.Primary == nil || k12Pair.Primary.WindowSeconds != keeperFiveHourWindowSeconds {
		t.Fatalf("k12 primary window = %#v, want inferred 5h", k12Pair.Primary)
	}
	if k12Pair.Secondary == nil || k12Pair.Secondary.WindowSeconds != keeperWeekWindowSeconds {
		t.Fatalf("k12 secondary window = %#v, want inferred weekly", k12Pair.Secondary)
	}

	usage := parseKeeperUsageInfo(map[string]any{
		"plan_type": "plus",
		"rate_limit": map[string]any{
			"primary_window": map[string]any{
				"used_percent":         20,
				"limit_window_seconds": float64(1234),
			},
			"secondary_window": map[string]any{
				"used_percent":         40,
				"limit_window_seconds": float64(5678),
			},
		},
	})
	if usage.PrimaryWindowSeconds == nil || *usage.PrimaryWindowSeconds != 1234 {
		t.Fatalf("primary limit_window_seconds = %v, want 1234", usage.PrimaryWindowSeconds)
	}
	if usage.SecondaryWindowSeconds == nil || *usage.SecondaryWindowSeconds != 5678 {
		t.Fatalf("secondary limit_window_seconds = %v, want 5678", usage.SecondaryWindowSeconds)
	}
	camelUsage := parseKeeperUsageInfo(map[string]any{"planType": "pro"})
	if camelUsage.PlanType != "pro" {
		t.Fatalf("camel planType = %q, want pro", camelUsage.PlanType)
	}
}

func TestKeeperQuotaWindowUsageUsesCurrentWindowBoundariesAndPricing(t *testing.T) {
	t.Setenv("CPA_HELPER_DATA_DIR", t.TempDir())
	app, err := New()
	if err != nil {
		t.Fatalf("New() failed: %v", err)
	}
	defer app.Close()

	insertKeeperTestPrice(t, app)
	now := time.Date(2026, 5, 18, 12, 30, 0, 0, appTimeLocation)
	resetAt := time.Date(2026, 5, 18, 13, 0, 0, 0, appTimeLocation)
	windowSeconds := 3600
	windowStart := resetAt.Add(-time.Duration(windowSeconds) * time.Second)
	accounts := []keeperAccount{
		{
			Name:                 "priced.json",
			Email:                stringPtr("priced@example.com"),
			AccountType:          stringPtr("plus"),
			PrimaryUsedPercent:   intPtrValue(100),
			PrimaryResetAt:       timePtrValue(resetAt),
			PrimaryWindowSeconds: intPtrValue(windowSeconds),
		},
	}
	insertKeeperWindowUsageRecord(t, app, keeperWindowUsageSeed{
		Dedupe:       "at-start",
		Timestamp:    windowStart,
		Source:       "priced@example.com",
		InputTokens:  10,
		OutputTokens: 5,
		RawJSON:      `{"source":"priced@example.com"}`,
	})
	insertKeeperWindowUsageRecord(t, app, keeperWindowUsageSeed{
		Dedupe:       "near-before-start",
		Timestamp:    windowStart.Add(-3 * time.Second),
		Source:       "priced@example.com",
		InputTokens:  4,
		OutputTokens: 1,
		RawJSON:      `{"source":"priced@example.com"}`,
	})
	insertKeeperWindowUsageRecord(t, app, keeperWindowUsageSeed{
		Dedupe:       "before-end",
		Timestamp:    resetAt.Add(-time.Second),
		Source:       "priced@example.com",
		Failed:       true,
		InputTokens:  20,
		OutputTokens: 10,
		RawJSON:      `{"source":"priced@example.com"}`,
	})
	insertKeeperWindowUsageRecord(t, app, keeperWindowUsageSeed{
		Dedupe:       "at-end",
		Timestamp:    resetAt,
		Source:       "priced@example.com",
		InputTokens:  100,
		OutputTokens: 100,
		RawJSON:      `{"source":"priced@example.com"}`,
	})
	insertKeeperWindowUsageRecord(t, app, keeperWindowUsageSeed{
		Dedupe:       "before-start",
		Timestamp:    windowStart.Add(-time.Minute),
		Source:       "priced@example.com",
		InputTokens:  100,
		OutputTokens: 100,
		RawJSON:      `{"source":"priced@example.com"}`,
	})

	usages, err := app.computeKeeperQuotaWindowUsages(context.Background(), accounts, now)
	if err != nil {
		t.Fatalf("compute window usages: %v", err)
	}
	usage := usages["priced.json"].Primary
	if usage == nil {
		t.Fatal("primary window usage is nil")
	}
	if usage.Records != 2 || usage.SuccessRecords != 1 || usage.FailedRecords != 1 {
		t.Fatalf("records = %d/%d/%d, want 2/1/1", usage.Records, usage.SuccessRecords, usage.FailedRecords)
	}
	if usage.InputTokens != 30 || usage.OutputTokens != 15 || usage.TotalTokens != 45 {
		t.Fatalf("tokens = input %d output %d total %d, want 30/15/45", usage.InputTokens, usage.OutputTokens, usage.TotalTokens)
	}
	if math.Abs(usage.EstimatedCostUSD-0.00006) > 0.00000001 {
		t.Fatalf("estimated cost = %.8f, want 0.00006000", usage.EstimatedCostUSD)
	}
	if usage.UnpricedRecords != 0 {
		t.Fatalf("unpriced records = %d, want 0", usage.UnpricedRecords)
	}
}

func TestKeeperQuotaWindowUsageUsesFreeMonthlyWindowBoundaries(t *testing.T) {
	t.Setenv("CPA_HELPER_DATA_DIR", t.TempDir())
	app, err := New()
	if err != nil {
		t.Fatalf("New() failed: %v", err)
	}
	defer app.Close()

	now := time.Date(2026, 5, 18, 12, 30, 0, 0, appTimeLocation)
	resetAt := now.Add(time.Hour)
	windowStart := resetAt.Add(-time.Duration(keeperMonthWindowSeconds) * time.Second)
	accounts := []keeperAccount{
		{
			Name:               "free-month.json",
			Email:              stringPtr("free-month@example.com"),
			AccountType:        stringPtr("free"),
			PrimaryUsedPercent: intPtrValue(0),
			PrimaryResetAt:     timePtrValue(resetAt),
		},
	}
	insertKeeperWindowUsageRecord(t, app, keeperWindowUsageSeed{
		Dedupe:       "inside-month-outside-week",
		Timestamp:    now.Add(-8 * 24 * time.Hour),
		Source:       "free-month@example.com",
		InputTokens:  20,
		OutputTokens: 10,
		RawJSON:      `{"source":"free-month@example.com"}`,
	})
	insertKeeperWindowUsageRecord(t, app, keeperWindowUsageSeed{
		Dedupe:       "previous-cycle-boundary",
		Timestamp:    windowStart.Add(-3 * time.Second),
		Source:       "free-month@example.com",
		InputTokens:  100,
		OutputTokens: 50,
		RawJSON:      `{"source":"free-month@example.com"}`,
	})

	usages, err := app.computeKeeperQuotaWindowUsages(context.Background(), accounts, now)
	if err != nil {
		t.Fatalf("compute window usages: %v", err)
	}
	usage := usages["free-month.json"].Primary
	if usage == nil {
		t.Fatal("primary window usage is nil")
	}
	if usage.WindowSeconds != keeperMonthWindowSeconds {
		t.Fatalf("window seconds = %d, want monthly", usage.WindowSeconds)
	}
	if usage.Records != 1 || usage.TotalTokens != 30 {
		t.Fatalf("usage = records %d tokens %d, want monthly-window record only", usage.Records, usage.TotalTokens)
	}
}

func TestAutomaticKeeperRunsRespectCacheButManualRefreshBypasses(t *testing.T) {
	t.Setenv("CPA_HELPER_DATA_DIR", t.TempDir())

	usageCalls := 0
	cpa := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v0/management/auth-files":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"files": []map[string]any{{"name": "cached.json", "type": "codex"}},
			})
		case r.Method == http.MethodGet && r.URL.Path == "/v0/management/auth-files/download":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"name":         "cached.json",
				"type":         "codex",
				"account_type": "free",
				"disabled":     false,
				"priority":     0,
				"access_token": "test-token",
			})
		case r.Method == http.MethodPost && r.URL.Path == "/v0/management/api-call":
			if keeperTestIsResetCreditsCall(r) {
				_ = json.NewEncoder(w).Encode(keeperTestEmptyResetCreditsPayload())
				return
			}
			usageCalls++
			_ = json.NewEncoder(w).Encode(map[string]any{
				"status_code": 200,
				"body": map[string]any{
					"plan_type": "free",
					"rate_limit": map[string]any{
						"primary_window": map[string]any{
							"used_percent":        10,
							"reset_after_seconds": 3600,
						},
					},
				},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer cpa.Close()

	app, err := New()
	if err != nil {
		t.Fatalf("New() failed: %v", err)
	}
	defer app.Close()
	configureKeeperTestCPA(t, app, cpa.URL, func(cfg *AppConfig) {
		cfg.CodexKeeper.AccountRefreshCacheMinutes = 10
	})
	insertKeeperStateForCandidate(t, app, "cached.json", nil, timePtrValue(time.Now().In(appTimeLocation).Add(-time.Minute)))

	stats, _, err := app.executeKeeperRunForAccounts(context.Background(), "daemon", nil, func(string) {})
	if err != nil {
		t.Fatalf("daemon run: %v", err)
	}
	if stats.Skipped != 1 {
		t.Fatalf("daemon skipped = %d, want 1", stats.Skipped)
	}
	if stats.Healthy != 1 {
		t.Fatalf("daemon healthy = %d, want clean cached checked state to count", stats.Healthy)
	}
	if usageCalls != 0 {
		t.Fatalf("daemon usage calls = %d, want 0", usageCalls)
	}

	_, _, err = app.executeKeeperRunForAccounts(context.Background(), "accounts", []string{"cached.json"}, func(string) {})
	if err != nil {
		t.Fatalf("manual account refresh: %v", err)
	}
	if usageCalls != 1 {
		t.Fatalf("manual usage calls = %d, want 1", usageCalls)
	}
	if got := countKeeperRows(t, app, `SELECT COUNT(*) FROM codex_keeper_runs`); got != 1 {
		t.Fatalf("keeper run rows = %d, want 1 because account refresh is not persisted", got)
	}
	if got := countKeeperRows(t, app, `SELECT COUNT(*) FROM codex_keeper_run_accounts`); got != 0 {
		t.Fatalf("keeper run account rows = %d, want 0 because skipped daemon and manual refresh are not persisted", got)
	}
}

func TestKeeperCredentialWebsocketsAppliesToRefreshModes(t *testing.T) {
	modes := []struct {
		name      string
		mode      string
		authNames []string
	}{
		{name: "daemon", mode: "daemon"},
		{name: "run-once", mode: "once"},
		{name: "accounts", mode: "accounts", authNames: []string{"accounts-auth.json"}},
		{name: "conditional", mode: "conditional", authNames: []string{"conditional-auth.json"}},
	}

	for _, tc := range modes {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("CPA_HELPER_DATA_DIR", t.TempDir())

			authName := tc.name + "-auth.json"
			if len(tc.authNames) > 0 {
				authName = tc.authNames[0]
			}
			websocketPatches := 0
			priorityPatches := 0
			authDetail := map[string]any{
				"name":         authName,
				"type":         "codex",
				"email":        "ws@example.com",
				"account_type": "free",
				"disabled":     false,
				"priority":     0,
				"access_token": "test-token",
				"websockets":   false,
			}
			cpa := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch {
				case r.Method == http.MethodGet && r.URL.Path == "/v0/management/auth-files":
					_ = json.NewEncoder(w).Encode(map[string]any{
						"files": []map[string]any{{"name": authName, "type": "codex", "websockets": false}},
					})
				case r.Method == http.MethodGet && r.URL.Path == "/v0/management/auth-files/download":
					if r.URL.Query().Get("name") != authName {
						http.NotFound(w, r)
						return
					}
					_ = json.NewEncoder(w).Encode(authDetail)
				case r.Method == http.MethodPost && r.URL.Path == "/v0/management/api-call":
					_ = json.NewEncoder(w).Encode(keeperWebsocketUsageSuccessPayload(10))
				case r.Method == http.MethodPatch && r.URL.Path == "/v0/management/auth-files/fields":
					var payload map[string]any
					if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
						http.Error(w, err.Error(), http.StatusBadRequest)
						return
					}
					if payload["name"] != authName {
						http.Error(w, "unexpected auth name", http.StatusBadRequest)
						return
					}
					if value, ok := payload["websockets"].(bool); ok && value {
						websocketPatches++
						authDetail["websockets"] = true
						_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
						return
					}
					if _, ok := payload["priority"]; ok {
						priorityPatches++
						_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
						return
					}
					http.Error(w, "missing supported field", http.StatusBadRequest)
				default:
					http.NotFound(w, r)
				}
			}))
			defer cpa.Close()

			app, err := New()
			if err != nil {
				t.Fatalf("New() failed: %v", err)
			}
			defer app.Close()
			configureKeeperTestCPA(t, app, cpa.URL, func(cfg *AppConfig) {
				cfg.CodexKeeper.DryRun = false
				cfg.CodexKeeper.EnableCredentialWebsockets = true
				cfg.CodexKeeper.WorkerThreads = 1
			})

			stats, _, err := app.executeKeeperRunForAccounts(context.Background(), tc.mode, tc.authNames, func(string) {})
			if err != nil {
				t.Fatalf("%s run: %v", tc.mode, err)
			}
			if websocketPatches != 1 {
				t.Fatalf("websocket patches = %d, want 1", websocketPatches)
			}
			if priorityPatches != 0 {
				t.Fatalf("priority patches = %d, want 0", priorityPatches)
			}
			if stats.Total != 1 || stats.Healthy != 1 || stats.NetworkError != 0 {
				t.Fatalf("stats = %#v, want one healthy account", stats)
			}
		})
	}
}

func TestKeeperCredentialWebsocketsRespectsDryRun(t *testing.T) {
	t.Setenv("CPA_HELPER_DATA_DIR", t.TempDir())

	websocketPatches := 0
	authName := "dry-run-auth.json"
	cpa := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v0/management/auth-files":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"files": []map[string]any{{"name": authName, "type": "codex", "websockets": false}},
			})
		case r.Method == http.MethodGet && r.URL.Path == "/v0/management/auth-files/download":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"name":         authName,
				"type":         "codex",
				"account_type": "free",
				"disabled":     false,
				"priority":     0,
				"access_token": "test-token",
				"websockets":   false,
			})
		case r.Method == http.MethodPost && r.URL.Path == "/v0/management/api-call":
			_ = json.NewEncoder(w).Encode(keeperWebsocketUsageSuccessPayload(10))
		case r.Method == http.MethodPatch && r.URL.Path == "/v0/management/auth-files/fields":
			websocketPatches++
			_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
		default:
			http.NotFound(w, r)
		}
	}))
	defer cpa.Close()

	app, err := New()
	if err != nil {
		t.Fatalf("New() failed: %v", err)
	}
	defer app.Close()
	configureKeeperTestCPA(t, app, cpa.URL, func(cfg *AppConfig) {
		cfg.CodexKeeper.DryRun = true
		cfg.CodexKeeper.EnableCredentialWebsockets = true
	})

	stats, _, err := app.executeKeeperRunForAccounts(context.Background(), "once", nil, func(string) {})
	if err != nil {
		t.Fatalf("once run: %v", err)
	}
	if websocketPatches != 0 {
		t.Fatalf("websocket patches = %d, want 0 in dry run", websocketPatches)
	}
	if stats.Total != 1 || stats.Healthy != 1 {
		t.Fatalf("stats = %#v, want one healthy account", stats)
	}
}

func TestAutomaticKeeperRunCountsCachedBadCredentialState(t *testing.T) {
	t.Setenv("CPA_HELPER_DATA_DIR", t.TempDir())

	downloadCalls := 0
	cpa := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v0/management/auth-files":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"files": []map[string]any{{"name": "cached-bad.json", "type": "codex"}},
			})
		case r.Method == http.MethodGet && r.URL.Path == "/v0/management/auth-files/download":
			downloadCalls++
			_ = json.NewEncoder(w).Encode(map[string]any{})
		default:
			http.NotFound(w, r)
		}
	}))
	defer cpa.Close()

	app, err := New()
	if err != nil {
		t.Fatalf("New() failed: %v", err)
	}
	defer app.Close()
	configureKeeperTestCPA(t, app, cpa.URL, func(cfg *AppConfig) {
		cfg.CodexKeeper.AccountRefreshCacheMinutes = 10
	})
	checkedAt := time.Now().In(appTimeLocation).Add(-time.Minute)
	insertKeeperStateForCandidate(t, app, "cached-bad.json", nil, timePtrValue(checkedAt))
	if _, err := app.db.Exec(`
		UPDATE codex_keeper_auth_states
		SET disabled = 1, last_status_code = 401, last_error = ?, latest_action = ?
		WHERE auth_name = ?
	`, "凭证不可用：HTTP 401", "禁用凭证：凭证不可用：HTTP 401", "cached-bad.json"); err != nil {
		t.Fatalf("mark cached bad credential state: %v", err)
	}

	stats, _, err := app.executeKeeperRunForAccounts(context.Background(), "daemon", nil, func(string) {})
	if err != nil {
		t.Fatalf("daemon run: %v", err)
	}
	if stats.Skipped != 1 {
		t.Fatalf("daemon skipped = %d, want 1", stats.Skipped)
	}
	if stats.StatusDisabled != 1 {
		t.Fatalf("daemon status_disabled = %d, want cached bad credential to count", stats.StatusDisabled)
	}
	if stats.Healthy != 0 {
		t.Fatalf("daemon healthy = %d, want 0", stats.Healthy)
	}
	if downloadCalls != 0 {
		t.Fatalf("download calls = %d, want 0", downloadCalls)
	}
}

func TestAutomaticKeeperRunReenablesRecoverableUnauthorizedDisabledAccount(t *testing.T) {
	t.Setenv("CPA_HELPER_DATA_DIR", t.TempDir())

	const authName = "recoverable-auto.json"
	cpa := newKeeperRecoveryTestCPA(t, map[string]map[string]any{
		authName: keeperRecoveryAuthDetail(authName, true),
	}, map[string]int{authName: http.StatusOK})
	defer cpa.Close()

	app, err := New()
	if err != nil {
		t.Fatalf("New() failed: %v", err)
	}
	defer app.Close()
	configureKeeperTestCPA(t, app, cpa.URL(), func(cfg *AppConfig) {
		cfg.CodexKeeper.DryRun = false
		cfg.CodexKeeper.AccountRefreshCacheMinutes = 10
	})
	markKeeperStateRecoverableUnauthorizedDisabled(t, app, authName, timePtrValue(time.Now().In(appTimeLocation).Add(-20*time.Minute)))

	stats, detail, err := app.executeKeeperRunForAccounts(context.Background(), "daemon", nil, func(string) {})
	if err != nil {
		t.Fatalf("daemon run: %v", err)
	}
	if stats.StatusEnabled != 1 {
		t.Fatalf("status_enabled = %d, want 1", stats.StatusEnabled)
	}
	if !strings.Contains(detail, "恢复启用 1") {
		t.Fatalf("detail = %q, want recovery count", detail)
	}
	assertKeeperRecoveredState(t, app, authName)
	if got := cpa.statusPatchCount(authName, false); got != 1 {
		t.Fatalf("enable status patch count = %d, want 1", got)
	}
	if got := cpa.usageCallCount(authName); got != 1 {
		t.Fatalf("usage calls = %d, want 1", got)
	}
}

func TestManualKeeperRefreshReenablesRecoverableUnauthorizedDisabledAccount(t *testing.T) {
	t.Setenv("CPA_HELPER_DATA_DIR", t.TempDir())

	const authName = "recoverable-manual.json"
	cpa := newKeeperRecoveryTestCPA(t, map[string]map[string]any{
		authName: keeperRecoveryAuthDetail(authName, true),
	}, map[string]int{authName: http.StatusOK})
	defer cpa.Close()

	app, err := New()
	if err != nil {
		t.Fatalf("New() failed: %v", err)
	}
	defer app.Close()
	configureKeeperTestCPA(t, app, cpa.URL(), func(cfg *AppConfig) {
		cfg.CodexKeeper.DryRun = false
		cfg.CodexKeeper.AccountRefreshCacheMinutes = 10
	})
	markKeeperStateRecoverableUnauthorizedDisabled(t, app, authName, timePtrValue(time.Now().In(appTimeLocation)))

	stats, _, err := app.executeKeeperRunForAccounts(context.Background(), "accounts", []string{authName}, func(string) {})
	if err != nil {
		t.Fatalf("manual refresh: %v", err)
	}
	if stats.StatusEnabled != 1 {
		t.Fatalf("status_enabled = %d, want 1", stats.StatusEnabled)
	}
	assertKeeperRecoveredState(t, app, authName)
	if got := cpa.statusPatchCount(authName, false); got != 1 {
		t.Fatalf("enable status patch count = %d, want 1", got)
	}
}

func TestConditionalKeeperRefreshSkipsDisabledAccounts(t *testing.T) {
	t.Setenv("CPA_HELPER_DATA_DIR", t.TempDir())

	const oldRecoverable = "recoverable-conditional.json"
	const recentRecoverable = "recoverable-cached.json"
	const manualDisabled = "manual-disabled.json"
	cpa := newKeeperRecoveryTestCPA(t, map[string]map[string]any{
		oldRecoverable:    keeperRecoveryAuthDetail(oldRecoverable, true),
		recentRecoverable: keeperRecoveryAuthDetail(recentRecoverable, true),
		manualDisabled:    keeperRecoveryAuthDetail(manualDisabled, true),
	}, map[string]int{
		oldRecoverable:    http.StatusOK,
		recentRecoverable: http.StatusOK,
		manualDisabled:    http.StatusOK,
	})
	defer cpa.Close()

	app, err := New()
	if err != nil {
		t.Fatalf("New() failed: %v", err)
	}
	defer app.Close()
	configureKeeperTestCPA(t, app, cpa.URL(), func(cfg *AppConfig) {
		cfg.CodexKeeper.DryRun = false
		cfg.CodexKeeper.AccountRefreshCacheMinutes = 10
	})
	now := time.Now().In(appTimeLocation)
	markKeeperStateRecoverableUnauthorizedDisabled(t, app, oldRecoverable, timePtrValue(now.Add(-20*time.Minute)))
	markKeeperStateRecoverableUnauthorizedDisabled(t, app, recentRecoverable, timePtrValue(now.Add(-time.Minute)))
	insertKeeperStateForCandidate(t, app, manualDisabled, nil, timePtrValue(now.Add(-20*time.Minute)))
	markKeeperStateDisabled(t, app, manualDisabled)

	cfg, err := app.loadConfig(context.Background())
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	names, err := app.conditionalKeeperRefreshCandidates(context.Background(), cfg)
	if err != nil {
		t.Fatalf("conditionalKeeperRefreshCandidates: %v", err)
	}
	assertStringSet(t, names, nil)
	assertKeeperStillDisabled(t, app, oldRecoverable)
	if got := cpa.usageCallCount(recentRecoverable); got != 0 {
		t.Fatalf("recent recoverable usage calls = %d, want 0 because cache skipped", got)
	}
	if got := cpa.usageCallCount(oldRecoverable); got != 0 {
		t.Fatalf("old recoverable usage calls = %d, want 0 because conditional refresh skips disabled accounts", got)
	}
	if got := cpa.usageCallCount(manualDisabled); got != 0 {
		t.Fatalf("manual disabled usage calls = %d, want 0", got)
	}
	if got := cpa.statusPatchCount(oldRecoverable, false); got != 0 {
		t.Fatalf("enable status patch count = %d, want 0", got)
	}
}

func TestManualKeeperRefreshDoesNotReenableNonRecoverableDisabledAccounts(t *testing.T) {
	t.Setenv("CPA_HELPER_DATA_DIR", t.TempDir())

	const manualDisabled = "manual-disabled-refresh.json"
	const paymentDisabled = "payment-disabled-refresh.json"
	cpa := newKeeperRecoveryTestCPA(t, map[string]map[string]any{
		manualDisabled:  keeperRecoveryAuthDetail(manualDisabled, true),
		paymentDisabled: keeperRecoveryAuthDetail(paymentDisabled, true),
	}, map[string]int{
		manualDisabled:  http.StatusOK,
		paymentDisabled: http.StatusOK,
	})
	defer cpa.Close()

	app, err := New()
	if err != nil {
		t.Fatalf("New() failed: %v", err)
	}
	defer app.Close()
	configureKeeperTestCPA(t, app, cpa.URL(), func(cfg *AppConfig) {
		cfg.CodexKeeper.DryRun = false
	})
	insertKeeperStateForCandidate(t, app, manualDisabled, nil, nil)
	markKeeperStateDisabled(t, app, manualDisabled)
	insertKeeperStateForCandidate(t, app, paymentDisabled, nil, nil)
	markKeeperStatePaymentRequiredDisabled(t, app, paymentDisabled)

	stats, _, err := app.executeKeeperRunForAccounts(context.Background(), "accounts", []string{manualDisabled, paymentDisabled}, func(string) {})
	if err != nil {
		t.Fatalf("manual refresh: %v", err)
	}
	if stats.StatusEnabled != 0 {
		t.Fatalf("status_enabled = %d, want 0", stats.StatusEnabled)
	}
	assertKeeperStillDisabled(t, app, manualDisabled)
	assertKeeperStillDisabled(t, app, paymentDisabled)
	if got := cpa.statusPatchCount(manualDisabled, false) + cpa.statusPatchCount(paymentDisabled, false); got != 0 {
		t.Fatalf("enable status patch count = %d, want 0", got)
	}
	if got := cpa.usageCallCount(manualDisabled); got != 1 {
		t.Fatalf("manual disabled usage calls = %d, want 1", got)
	}
	if got := cpa.usageCallCount(paymentDisabled); got != 1 {
		t.Fatalf("payment disabled usage calls = %d, want 1", got)
	}
}

func TestKeeperRunKeepsRecoverableUnauthorizedAccountDisabledWhenStillUnauthorized(t *testing.T) {
	t.Setenv("CPA_HELPER_DATA_DIR", t.TempDir())

	const authName = "still-unauthorized.json"
	cpa := newKeeperRecoveryTestCPA(t, map[string]map[string]any{
		authName: keeperRecoveryAuthDetail(authName, true),
	}, map[string]int{authName: http.StatusUnauthorized})
	defer cpa.Close()

	app, err := New()
	if err != nil {
		t.Fatalf("New() failed: %v", err)
	}
	defer app.Close()
	configureKeeperTestCPA(t, app, cpa.URL(), func(cfg *AppConfig) {
		cfg.CodexKeeper.DryRun = false
	})
	markKeeperStateRecoverableUnauthorizedDisabled(t, app, authName, timePtrValue(time.Now().In(appTimeLocation).Add(-20*time.Minute)))

	stats, _, err := app.executeKeeperRunForAccounts(context.Background(), "accounts", []string{authName}, func(string) {})
	if err != nil {
		t.Fatalf("manual refresh: %v", err)
	}
	if stats.StatusDisabled != 1 {
		t.Fatalf("status_disabled = %d, want 1", stats.StatusDisabled)
	}
	assertKeeperStillDisabled(t, app, authName)
	state, err := app.getKeeperState(context.Background(), authName)
	if err != nil {
		t.Fatalf("get keeper state: %v", err)
	}
	if state.LastStatusCode == nil || *state.LastStatusCode != http.StatusUnauthorized {
		t.Fatalf("last_status_code = %v, want 401", state.LastStatusCode)
	}
	if state.LastError == nil || !strings.Contains(*state.LastError, "凭证不可用") {
		t.Fatalf("last_error = %v, want credential error", state.LastError)
	}
	if state.LatestAction == nil || !strings.Contains(*state.LatestAction, "禁用凭证") {
		t.Fatalf("latest_action = %v, want keeper disable action", state.LatestAction)
	}
	if got := cpa.statusPatchCount(authName, false); got != 0 {
		t.Fatalf("enable status patch count = %d, want 0", got)
	}
}

func TestKeeperAuthDetailRequestFailureCountsAsNetworkError(t *testing.T) {
	t.Setenv("CPA_HELPER_DATA_DIR", t.TempDir())

	usageCalls := 0
	cpa := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v0/management/auth-files":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"files": []map[string]any{{"name": "download-fails.json", "type": "codex"}},
			})
		case r.Method == http.MethodGet && r.URL.Path == "/v0/management/auth-files/download":
			http.Error(w, "temporary management failure", http.StatusBadGateway)
		case r.Method == http.MethodPost && r.URL.Path == "/v0/management/api-call":
			if keeperTestIsResetCreditsCall(r) {
				_ = json.NewEncoder(w).Encode(keeperTestEmptyResetCreditsPayload())
				return
			}
			usageCalls++
			_ = json.NewEncoder(w).Encode(map[string]any{"status_code": 200, "body": map[string]any{}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer cpa.Close()

	app, err := New()
	if err != nil {
		t.Fatalf("New() failed: %v", err)
	}
	defer app.Close()
	configureKeeperTestCPA(t, app, cpa.URL, nil)

	stats, _, err := app.executeKeeperRunForAccounts(context.Background(), "daemon", nil, func(string) {})
	if err != nil {
		t.Fatalf("daemon run: %v", err)
	}
	if stats.NetworkError != 1 {
		t.Fatalf("network_error = %d, want 1", stats.NetworkError)
	}
	if stats.Skipped != 0 {
		t.Fatalf("skipped = %d, want 0", stats.Skipped)
	}
	if stats.Healthy != 0 || stats.StatusDisabled != 0 {
		t.Fatalf("stats = %#v, want only network error", stats)
	}
	if usageCalls != 0 {
		t.Fatalf("usage calls = %d, want 0", usageCalls)
	}
	state, err := app.getKeeperState(context.Background(), "download-fails.json")
	if err != nil {
		t.Fatalf("get keeper state: %v", err)
	}
	if state.LastError == nil || !strings.Contains(*state.LastError, "读取 auth file 详情失败") {
		t.Fatalf("last_error = %v, want auth detail failure", state.LastError)
	}
}

func TestKeeperRunSkipsInFlightAuthBeforeProcessing(t *testing.T) {
	t.Setenv("CPA_HELPER_DATA_DIR", t.TempDir())

	downloadCalls := 0
	usageCalls := 0
	cpa := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v0/management/auth-files":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"files": []map[string]any{{"name": "busy.json", "type": "codex"}},
			})
		case r.Method == http.MethodGet && r.URL.Path == "/v0/management/auth-files/download":
			downloadCalls++
			_ = json.NewEncoder(w).Encode(map[string]any{
				"name":         "busy.json",
				"type":         "codex",
				"account_type": "free",
				"disabled":     false,
				"priority":     0,
				"access_token": "test-token",
			})
		case r.Method == http.MethodPost && r.URL.Path == "/v0/management/api-call":
			if keeperTestIsResetCreditsCall(r) {
				_ = json.NewEncoder(w).Encode(keeperTestEmptyResetCreditsPayload())
				return
			}
			usageCalls++
			_ = json.NewEncoder(w).Encode(map[string]any{"status_code": 200, "body": map[string]any{}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer cpa.Close()

	app, err := New()
	if err != nil {
		t.Fatalf("New() failed: %v", err)
	}
	defer app.Close()
	configureKeeperTestCPA(t, app, cpa.URL, nil)

	stats, _, err := app.executeKeeperRunWithOptions(context.Background(), keeperRunOptions{
		Mode:            "accounts",
		AuthNames:       []string{"busy.json"},
		ManualRefresh:   true,
		UseRefreshCache: false,
		PersistRun:      false,
		TryLockAuthName: func(mode string, name string) bool {
			if mode != "accounts" || name != "busy.json" {
				t.Fatalf("TryLockAuthName(%q, %q), want accounts/busy.json", mode, name)
			}
			return false
		},
		UnlockAuthName: func(name string) {
			t.Fatalf("UnlockAuthName(%q) called after a failed lock", name)
		},
	}, func(string) {})
	if err != nil {
		t.Fatalf("account refresh: %v", err)
	}
	if stats.Skipped != 1 {
		t.Fatalf("skipped = %d, want 1", stats.Skipped)
	}
	if downloadCalls != 0 {
		t.Fatalf("download calls = %d, want 0", downloadCalls)
	}
	if usageCalls != 0 {
		t.Fatalf("usage calls = %d, want 0", usageCalls)
	}
}

func TestConditionalKeeperRunUsesAutomaticPriorityPolicy(t *testing.T) {
	t.Setenv("CPA_HELPER_DATA_DIR", t.TempDir())

	priorityPatches := []int{}
	cpa := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v0/management/auth-files":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"files": []map[string]any{{"name": "quota.json", "type": "codex"}},
			})
		case r.Method == http.MethodGet && r.URL.Path == "/v0/management/auth-files/download":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"name":         "quota.json",
				"type":         "codex",
				"account_type": "free",
				"disabled":     false,
				"priority":     0,
				"access_token": "test-token",
			})
		case r.Method == http.MethodPost && r.URL.Path == "/v0/management/api-call":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"status_code": 200,
				"body": map[string]any{
					"plan_type": "free",
					"rate_limit": map[string]any{
						"primary_window": map[string]any{
							"used_percent":        100,
							"reset_after_seconds": 3600,
						},
					},
				},
			})
		case r.Method == http.MethodPatch && r.URL.Path == "/v0/management/auth-files/fields":
			var payload struct {
				Priority *int `json:"priority"`
			}
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			if payload.Priority != nil {
				priorityPatches = append(priorityPatches, *payload.Priority)
			}
			_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
		default:
			http.NotFound(w, r)
		}
	}))
	defer cpa.Close()

	app, err := New()
	if err != nil {
		t.Fatalf("New() failed: %v", err)
	}
	defer app.Close()
	configureKeeperTestCPA(t, app, cpa.URL, func(cfg *AppConfig) {
		cfg.CodexKeeper.DryRun = false
		cfg.CodexKeeper.QuotaThreshold = 50
	})

	stats, _, err := app.executeKeeperRunForAccounts(context.Background(), "conditional", []string{"quota.json"}, func(string) {})
	if err != nil {
		t.Fatalf("conditional run: %v", err)
	}
	if stats.PriorityDegraded != 1 {
		t.Fatalf("priority_degraded = %d, want 1", stats.PriorityDegraded)
	}
	if len(priorityPatches) != 1 || priorityPatches[0] != -1 {
		t.Fatalf("priority patches = %#v, want [-1]", priorityPatches)
	}
	if got := countKeeperRows(t, app, `SELECT COUNT(*) FROM codex_keeper_runs`); got != 0 {
		t.Fatalf("keeper run rows = %d, want 0 because conditional refresh is not persisted", got)
	}
}

func TestManualKeeperRefreshUsesAutomaticPriorityPolicy(t *testing.T) {
	t.Setenv("CPA_HELPER_DATA_DIR", t.TempDir())

	authDetails := map[string]map[string]any{
		"quota.json": {
			"name":         "quota.json",
			"type":         "codex",
			"account_type": "free",
			"disabled":     false,
			"priority":     0,
			"access_token": "test-token",
		},
		"default.json": {
			"name":         "default.json",
			"type":         "codex",
			"account_type": "plus",
			"disabled":     false,
			"priority":     0,
			"access_token": "test-token",
		},
		"restore.json": {
			"name":         "restore.json",
			"type":         "codex",
			"account_type": "plus",
			"disabled":     false,
			"priority":     -1,
			"access_token": "test-token",
		},
	}
	usagePercents := map[string]int{
		"quota.json":   100,
		"default.json": 10,
		"restore.json": 10,
	}
	priorityPatches := map[string][]int{}
	cpa := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v0/management/auth-files":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"files": []map[string]any{
					{"name": "quota.json", "type": "codex"},
					{"name": "default.json", "type": "codex"},
					{"name": "restore.json", "type": "codex"},
				},
			})
		case r.Method == http.MethodGet && r.URL.Path == "/v0/management/auth-files/download":
			detail, ok := authDetails[r.URL.Query().Get("name")]
			if !ok {
				http.NotFound(w, r)
				return
			}
			_ = json.NewEncoder(w).Encode(detail)
		case r.Method == http.MethodPost && r.URL.Path == "/v0/management/api-call":
			var payload struct {
				AuthIndex string `json:"auth_index"`
			}
			_ = json.NewDecoder(r.Body).Decode(&payload)
			usedPercent := usagePercents[payload.AuthIndex]
			planType, _ := authDetails[payload.AuthIndex]["account_type"].(string)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"status_code": 200,
				"body": map[string]any{
					"plan_type": planType,
					"rate_limit": map[string]any{
						"primary_window": map[string]any{
							"used_percent":        usedPercent,
							"reset_after_seconds": 3600,
						},
					},
				},
			})
		case r.Method == http.MethodPatch && r.URL.Path == "/v0/management/auth-files/fields":
			var payload struct {
				Name     string `json:"name"`
				Priority *int   `json:"priority"`
			}
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			if payload.Priority == nil {
				http.Error(w, "priority is required", http.StatusBadRequest)
				return
			}
			priorityPatches[payload.Name] = append(priorityPatches[payload.Name], *payload.Priority)
			authDetails[payload.Name]["priority"] = *payload.Priority
			_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
		default:
			http.NotFound(w, r)
		}
	}))
	defer cpa.Close()

	app, err := New()
	if err != nil {
		t.Fatalf("New() failed: %v", err)
	}
	defer app.Close()
	configureKeeperTestCPA(t, app, cpa.URL, func(cfg *AppConfig) {
		cfg.CodexKeeper.DryRun = false
		cfg.CodexKeeper.QuotaThreshold = 50
	})
	insertKeeperStateForCandidate(t, app, "restore.json", nil, nil)
	_, err = app.db.Exec(`
		UPDATE codex_keeper_auth_states
		SET restore_priority = ?, updated_at = ?
		WHERE auth_name = ?
	`, 21, dbTime(time.Now().In(appTimeLocation)), "restore.json")
	if err != nil {
		t.Fatalf("seed restore priority: %v", err)
	}

	stats, _, err := app.executeKeeperRunForAccounts(context.Background(), "accounts", []string{"quota.json", "default.json", "restore.json"}, func(string) {})
	if err != nil {
		t.Fatalf("manual refresh: %v", err)
	}
	if stats.PriorityDegraded != 1 {
		t.Fatalf("priority_degraded = %d, want 1", stats.PriorityDegraded)
	}
	if stats.PriorityRestored != 2 {
		t.Fatalf("priority_restored = %d, want 2", stats.PriorityRestored)
	}
	expectedPatches := map[string][]int{
		"quota.json":   {-1},
		"default.json": {4},
		"restore.json": {21},
	}
	if !reflect.DeepEqual(priorityPatches, expectedPatches) {
		t.Fatalf("priority patches = %#v, want %#v", priorityPatches, expectedPatches)
	}
	if got := countKeeperRows(t, app, `SELECT COUNT(*) FROM codex_keeper_runs`); got != 0 {
		t.Fatalf("keeper run rows = %d, want 0 because account refresh is not persisted", got)
	}
}

func TestFullKeeperRunPrunesLocalStatesMissingFromCPA(t *testing.T) {
	t.Setenv("CPA_HELPER_DATA_DIR", t.TempDir())

	cpa := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v0/management/auth-files":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"files": []map[string]any{{"name": "kept.json", "type": "codex"}},
			})
		case r.Method == http.MethodGet && r.URL.Path == "/v0/management/auth-files/download":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"name":         "kept.json",
				"type":         "codex",
				"account_type": "free",
				"disabled":     false,
				"priority":     0,
				"access_token": "test-token",
			})
		case r.Method == http.MethodPost && r.URL.Path == "/v0/management/api-call":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"status_code": 200,
				"body": map[string]any{
					"plan_type": "free",
					"rate_limit": map[string]any{
						"primary_window": map[string]any{"used_percent": 10, "reset_after_seconds": 3600},
					},
				},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer cpa.Close()

	app, err := New()
	if err != nil {
		t.Fatalf("New() failed: %v", err)
	}
	defer app.Close()
	configureKeeperTestCPA(t, app, cpa.URL, nil)
	insertKeeperStateForCandidate(t, app, "kept.json", nil, nil)
	insertKeeperStateForCandidate(t, app, "stale.json", nil, nil)

	if _, _, err := app.executeKeeperRunForAccounts(context.Background(), "daemon", nil, func(string) {}); err != nil {
		t.Fatalf("daemon run: %v", err)
	}
	if got := countKeeperRows(t, app, `SELECT COUNT(*) FROM codex_keeper_auth_states WHERE auth_name = 'kept.json'`); got != 1 {
		t.Fatalf("kept state rows = %d, want 1", got)
	}
	if got := countKeeperRows(t, app, `SELECT COUNT(*) FROM codex_keeper_auth_states WHERE auth_name = 'stale.json'`); got != 0 {
		t.Fatalf("stale state rows = %d, want 0", got)
	}
}

func TestKeeperStatusStatsUseLatestDaemonRunOnly(t *testing.T) {
	t.Setenv("CPA_HELPER_DATA_DIR", t.TempDir())

	app, err := New()
	if err != nil {
		t.Fatalf("New() failed: %v", err)
	}
	defer app.Close()

	ctx := context.Background()
	daemonRunID, err := app.createKeeperRun(ctx, "daemon")
	if err != nil {
		t.Fatalf("create daemon run: %v", err)
	}
	if err := app.finishKeeperRun(ctx, daemonRunID, "completed", "daemon", keeperStats{
		Total:          7,
		Healthy:        6,
		StatusDisabled: 1,
	}); err != nil {
		t.Fatalf("finish daemon run: %v", err)
	}
	onceRunID, err := app.createKeeperRun(ctx, "once")
	if err != nil {
		t.Fatalf("create once run: %v", err)
	}
	if err := app.finishKeeperRun(ctx, onceRunID, "completed", "once", keeperStats{
		Total:            2,
		Healthy:          1,
		NetworkError:     1,
		PriorityRestored: 1,
	}); err != nil {
		t.Fatalf("finish once run: %v", err)
	}

	app.keeper.LoadPersistedState(ctx)
	status := app.keeper.Status()
	if status.Stats.Total != 7 || status.Stats.Healthy != 6 || status.Stats.StatusDisabled != 1 || status.Stats.NetworkError != 0 {
		t.Fatalf("status stats = %#v, want latest daemon stats only", status.Stats)
	}
}

func configureKeeperTestCPA(t *testing.T, app *App, url string, mutate func(*AppConfig)) {
	t.Helper()
	ctx := context.Background()
	cfg, err := app.loadConfig(ctx)
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	cfg.Collector.CLIProxyURL = url
	cfg.Collector.ManagementKey = "test-management-key"
	cfg.Collector.Enabled = false
	cfg.CodexKeeper.ScheduleCron = "0 0 29 2 *"
	cfg.CodexKeeper.CPATimeoutSeconds = 1
	cfg.CodexKeeper.UsageTimeoutSeconds = 1
	if mutate != nil {
		mutate(&cfg)
	}
	if err := app.saveConfig(ctx, cfg); err != nil {
		t.Fatalf("saveConfig: %v", err)
	}
}

func insertKeeperUsageRecord(t *testing.T, app *App, dedupe string, timestamp time.Time, rawJSON string) {
	t.Helper()
	now := dbTime(time.Now().In(appTimeLocation))
	source := "test"
	if value := rawJSONStringField(rawJSON, "source"); value != nil {
		source = *value
	}
	_, err := app.db.Exec(`
		INSERT INTO usage_records (
			created_at, timestamp, usage_username, api_key_description, provider,
			model, endpoint, source, request_id, auth, latency_ms, failed,
			input_tokens, output_tokens, cached_tokens, reasoning_tokens,
			total_tokens, dedupe_key, raw_json
		) VALUES (?, ?, NULL, NULL, 'codex', 'gpt-test', '/v1/responses',
			?, ?, 'api_key', 10, 1, 1, 1, 0, 0, 2, ?, ?)
	`, now, dbTime(timestamp), source, dedupe, "conditional-"+dedupe, rawJSON)
	if err != nil {
		t.Fatalf("insert usage record %s: %v", dedupe, err)
	}
}

type keeperWindowUsageSeed struct {
	Dedupe       string
	Timestamp    time.Time
	Source       string
	AuthIndex    string
	Failed       bool
	InputTokens  int
	OutputTokens int
	RawJSON      string
}

func insertKeeperWindowUsageRecord(t *testing.T, app *App, seed keeperWindowUsageSeed) {
	t.Helper()
	now := dbTime(time.Now().In(appTimeLocation))
	source := seed.Source
	if strings.TrimSpace(source) == "" {
		source = "test"
	}
	rawJSON := seed.RawJSON
	if strings.TrimSpace(rawJSON) == "" {
		rawJSON = `{}`
	}
	authIndex := strings.TrimSpace(seed.AuthIndex)
	sourceAccount := sourceAccountFromUsageSource(&source)
	inputTokens := seed.InputTokens
	outputTokens := seed.OutputTokens
	totalTokens := inputTokens + outputTokens
	_, err := app.db.Exec(`
		INSERT INTO usage_records (
			created_at, timestamp, usage_username, api_key_description, provider,
			model, endpoint, source, source_account, request_id, auth, auth_index, latency_ms,
			failed, input_tokens, output_tokens, cached_tokens, reasoning_tokens,
			total_tokens, dedupe_key, raw_json
		) VALUES (?, ?, NULL, NULL, 'codex', 'gpt-test', '/v1/responses',
			?, ?, ?, 'api_key', ?, 10, ?, ?, ?, 0, 0, ?, ?, ?)
	`, now, dbTime(seed.Timestamp), source, nullableTestString(sourceAccount), seed.Dedupe, nullableBlankTestString(authIndex), seed.Failed, inputTokens, outputTokens, totalTokens, "quota-"+seed.Dedupe, rawJSON)
	if err != nil {
		t.Fatalf("insert quota usage record %s: %v", seed.Dedupe, err)
	}
}

func insertKeeperTestPrice(t *testing.T, app *App) {
	t.Helper()
	_, err := app.db.Exec(`
		INSERT INTO model_prices (
			provider, model, input_usd_per_million, output_usd_per_million,
			cache_read_usd_per_million, cache_creation_usd_per_million, source, updated_at
		) VALUES ('codex', 'gpt-test', 1, 2, 0, 0, 'manual', ?)
		ON CONFLICT(provider, model) DO UPDATE SET
			input_usd_per_million = excluded.input_usd_per_million,
			output_usd_per_million = excluded.output_usd_per_million,
			updated_at = excluded.updated_at
	`, dbTime(time.Now().In(appTimeLocation)))
	if err != nil {
		t.Fatalf("insert test price: %v", err)
	}
}

func nullableTestString(value *string) any {
	if value == nil || strings.TrimSpace(*value) == "" {
		return nil
	}
	return *value
}

func nullableBlankTestString(value string) any {
	normalized := strings.TrimSpace(value)
	if normalized == "" {
		return nil
	}
	return normalized
}

func insertKeeperStateForCandidate(t *testing.T, app *App, name string, primaryResetAt *time.Time, lastCheckedAt *time.Time) {
	t.Helper()
	insertKeeperStateForCandidateWithEmail(t, app, name, nil, primaryResetAt, lastCheckedAt)
}

func insertKeeperStateForCandidateWithEmail(t *testing.T, app *App, name string, email *string, primaryResetAt *time.Time, lastCheckedAt *time.Time) {
	t.Helper()
	now := dbTime(time.Now().In(appTimeLocation))
	_, err := app.db.Exec(`
		INSERT INTO codex_keeper_auth_states (
			auth_name, email, disabled, primary_reset_at, last_checked_at, created_at, updated_at
		) VALUES (?, ?, 0, ?, ?, ?, ?)
		ON CONFLICT(auth_name) DO UPDATE SET
			email = excluded.email,
			primary_reset_at = excluded.primary_reset_at,
			last_checked_at = excluded.last_checked_at,
			updated_at = excluded.updated_at
	`, name, email, dbTimePtr(primaryResetAt), dbTimePtr(lastCheckedAt), now, now)
	if err != nil {
		t.Fatalf("insert keeper state %s: %v", name, err)
	}
}

func insertKeeperStateForCandidateWithError(t *testing.T, app *App, name string, lastError string, lastCheckedAt *time.Time) {
	t.Helper()
	insertKeeperStateForCandidate(t, app, name, nil, lastCheckedAt)
	_, err := app.db.Exec(`
		UPDATE codex_keeper_auth_states
		SET last_error = ?, updated_at = ?
		WHERE auth_name = ?
	`, lastError, dbTime(time.Now().In(appTimeLocation)), name)
	if err != nil {
		t.Fatalf("mark keeper state %s error: %v", name, err)
	}
}

func markKeeperStateDisabled(t *testing.T, app *App, name string) {
	t.Helper()
	_, err := app.db.Exec(`
		UPDATE codex_keeper_auth_states
		SET disabled = 1, updated_at = ?
		WHERE auth_name = ?
	`, dbTime(time.Now().In(appTimeLocation)), name)
	if err != nil {
		t.Fatalf("mark keeper state %s disabled: %v", name, err)
	}
}

func markKeeperStateRecoverableUnauthorizedDisabled(t *testing.T, app *App, name string, lastCheckedAt *time.Time) {
	t.Helper()
	insertKeeperStateForCandidate(t, app, name, nil, lastCheckedAt)
	_, err := app.db.Exec(`
		UPDATE codex_keeper_auth_states
		SET disabled = 1,
		    last_status_code = ?,
		    last_error = ?,
		    latest_action = ?,
		    last_checked_at = ?,
		    updated_at = ?
		WHERE auth_name = ?
	`, http.StatusUnauthorized, "凭证不可用：HTTP 401", "禁用凭证：凭证不可用：HTTP 401", dbTimePtr(lastCheckedAt), dbTime(time.Now().In(appTimeLocation)), name)
	if err != nil {
		t.Fatalf("mark keeper state %s recoverable unauthorized disabled: %v", name, err)
	}
}

func markKeeperStatePaymentRequiredDisabled(t *testing.T, app *App, name string) {
	t.Helper()
	_, err := app.db.Exec(`
		UPDATE codex_keeper_auth_states
		SET disabled = 1,
		    last_status_code = ?,
		    last_error = ?,
		    latest_action = ?,
		    updated_at = ?
		WHERE auth_name = ?
	`, http.StatusPaymentRequired, "凭证不可用：HTTP 402", "禁用凭证：凭证不可用：HTTP 402", dbTime(time.Now().In(appTimeLocation)), name)
	if err != nil {
		t.Fatalf("mark keeper state %s payment required disabled: %v", name, err)
	}
}

type keeperRecoveryStatusPatch struct {
	Name     string
	Disabled bool
}

type keeperRecoveryTestCPA struct {
	server          *httptest.Server
	mu              sync.Mutex
	authDetails     map[string]map[string]any
	usageStatuses   map[string]int
	usageCalls      map[string]int
	statusPatches   []keeperRecoveryStatusPatch
	priorityPatches map[string]int
}

func newKeeperRecoveryTestCPA(t *testing.T, authDetails map[string]map[string]any, usageStatuses map[string]int) *keeperRecoveryTestCPA {
	t.Helper()
	cpa := &keeperRecoveryTestCPA{
		authDetails:     authDetails,
		usageStatuses:   usageStatuses,
		usageCalls:      map[string]int{},
		priorityPatches: map[string]int{},
	}
	cpa.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v0/management/auth-files":
			cpa.mu.Lock()
			files := make([]map[string]any, 0, len(cpa.authDetails))
			for name := range cpa.authDetails {
				files = append(files, map[string]any{"name": name, "type": "codex"})
			}
			cpa.mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]any{"files": files})
		case r.Method == http.MethodGet && r.URL.Path == "/v0/management/auth-files/download":
			name := r.URL.Query().Get("name")
			cpa.mu.Lock()
			detail, ok := cpa.authDetails[name]
			if !ok {
				cpa.mu.Unlock()
				http.NotFound(w, r)
				return
			}
			copied := map[string]any{}
			for key, value := range detail {
				copied[key] = value
			}
			cpa.mu.Unlock()
			_ = json.NewEncoder(w).Encode(copied)
		case r.Method == http.MethodPost && r.URL.Path == "/v0/management/api-call":
			var payload struct {
				AuthIndex string `json:"auth_index"`
				URL       string `json:"url"`
			}
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			if strings.Contains(payload.URL, "rate-limit-reset-credits") {
				_ = json.NewEncoder(w).Encode(keeperTestEmptyResetCreditsPayload())
				return
			}
			cpa.mu.Lock()
			cpa.usageCalls[payload.AuthIndex]++
			status := cpa.usageStatuses[payload.AuthIndex]
			if status == 0 {
				status = http.StatusOK
			}
			planType := "free"
			if detail, ok := cpa.authDetails[payload.AuthIndex]; ok {
				if value, ok := detail["account_type"].(string); ok && strings.TrimSpace(value) != "" {
					planType = value
				}
			}
			cpa.mu.Unlock()
			if status >= 200 && status < 300 {
				_ = json.NewEncoder(w).Encode(map[string]any{
					"status_code": status,
					"body": map[string]any{
						"plan_type": planType,
						"rate_limit": map[string]any{
							"primary_window": map[string]any{
								"used_percent":        10,
								"reset_after_seconds": 3600,
							},
						},
					},
				})
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"status_code": status,
				"body": map[string]any{
					"error": map[string]any{
						"message": "test credential error",
						"code":    "test_credential_error",
					},
					"status": status,
				},
			})
		case r.Method == http.MethodPatch && r.URL.Path == "/v0/management/auth-files/status":
			var payload struct {
				Name     string `json:"name"`
				Disabled bool   `json:"disabled"`
			}
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			cpa.mu.Lock()
			cpa.statusPatches = append(cpa.statusPatches, keeperRecoveryStatusPatch{Name: payload.Name, Disabled: payload.Disabled})
			if detail, ok := cpa.authDetails[payload.Name]; ok {
				detail["disabled"] = payload.Disabled
			}
			cpa.mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
		case r.Method == http.MethodPatch && r.URL.Path == "/v0/management/auth-files/fields":
			var payload struct {
				Name     string `json:"name"`
				Priority *int   `json:"priority"`
			}
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			cpa.mu.Lock()
			cpa.priorityPatches[payload.Name]++
			if detail, ok := cpa.authDetails[payload.Name]; ok {
				if payload.Priority == nil {
					delete(detail, "priority")
				} else {
					detail["priority"] = *payload.Priority
				}
			}
			cpa.mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
		default:
			http.NotFound(w, r)
		}
	}))
	return cpa
}

func keeperRecoveryAuthDetail(name string, disabled bool) map[string]any {
	return map[string]any{
		"name":         name,
		"type":         "codex",
		"email":        name + "@example.com",
		"account_type": "free",
		"disabled":     disabled,
		"priority":     4,
		"access_token": "test-token",
		"auth_index":   name,
	}
}

func (c *keeperRecoveryTestCPA) URL() string {
	return c.server.URL
}

func (c *keeperRecoveryTestCPA) Close() {
	c.server.Close()
}

func (c *keeperRecoveryTestCPA) usageCallCount(name string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.usageCalls[name]
}

func (c *keeperRecoveryTestCPA) statusPatchCount(name string, disabled bool) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	count := 0
	for _, patch := range c.statusPatches {
		if patch.Name == name && patch.Disabled == disabled {
			count++
		}
	}
	return count
}

func assertKeeperRecoveredState(t *testing.T, app *App, name string) {
	t.Helper()
	state, err := app.getKeeperState(context.Background(), name)
	if err != nil {
		t.Fatalf("get keeper state %s: %v", name, err)
	}
	if state.Disabled {
		t.Fatalf("%s disabled = true, want false", name)
	}
	if state.LastStatusCode == nil || *state.LastStatusCode != http.StatusOK {
		t.Fatalf("%s last_status_code = %v, want 200", name, state.LastStatusCode)
	}
	if state.LastError != nil {
		t.Fatalf("%s last_error = %v, want nil", name, state.LastError)
	}
	if state.LatestAction == nil || !strings.Contains(*state.LatestAction, "恢复启用") {
		t.Fatalf("%s latest_action = %v, want recovery action", name, state.LatestAction)
	}
	if state.LastHealthyAt == nil {
		t.Fatalf("%s last_healthy_at is nil, want recovery to mark healthy", name)
	}
}

func assertKeeperStillDisabled(t *testing.T, app *App, name string) {
	t.Helper()
	state, err := app.getKeeperState(context.Background(), name)
	if err != nil {
		t.Fatalf("get keeper state %s: %v", name, err)
	}
	if !state.Disabled {
		t.Fatalf("%s disabled = false, want true", name)
	}
}

func stringPtr(value string) *string {
	return &value
}

func timePtrValue(value time.Time) *time.Time {
	return &value
}

func intPtrValue(value int) *int {
	return &value
}

func countKeeperRows(t *testing.T, app *App, query string) int {
	t.Helper()
	var count int
	if err := app.db.QueryRow(query).Scan(&count); err != nil {
		t.Fatalf("count rows with %q: %v", query, err)
	}
	return count
}

func assertStringSet(t *testing.T, got []string, want []string) {
	t.Helper()
	gotSet := map[string]bool{}
	for _, item := range got {
		gotSet[item] = true
	}
	if len(gotSet) != len(want) {
		t.Fatalf("names = %#v, want set %#v", got, want)
	}
	for _, item := range want {
		if !gotSet[item] {
			t.Fatalf("names = %#v, want set %#v", got, want)
		}
	}
}

func keeperWebsocketUsageSuccessPayload(usedPercent int) map[string]any {
	return map[string]any{
		"status_code": 200,
		"body": map[string]any{
			"plan_type": "free",
			"rate_limit": map[string]any{
				"primary_window": map[string]any{
					"used_percent":        usedPercent,
					"reset_after_seconds": 3600,
				},
			},
		},
	}
}

// resetCreditSnapshotJSON is a single valid projected reset credit for identity tests.
const resetCreditSnapshotJSON = `[{"id":"c1","reset_type":"codex_rate_limits","status":"available","granted_at":"2026-08-22T00:08:46.146320Z","expires_at":"2026-09-21T00:08:46.146320Z"}]`

func healthyResetResult(name, authIndex, accountID string, count *int, credits *string) keeperAccountResult {
	r := keeperAccountResult{
		Name:             name,
		Result:           "healthy",
		AuthIndex:        stringPtr(authIndex),
		CheckedAt:        time.Now().In(appTimeLocation),
		ResetCreditCount: count,
		ResetCredits:     credits,
	}
	if accountID != "" {
		r.AccountID = stringPtr(accountID)
	}
	return r
}

// TestUpsertKeeperStateClearsResetCreditsOnIdentityChange pins the identity
// boundary: when an auth_name is rebound to a different ACCOUNT (a new account_id)
// and the new account's reset-credit fetch fails (nil count/credits), the previous
// account's snapshot must NOT be preserved by COALESCE — it must be cleared so the
// wrong account's schedule never surfaces on the new identity's row.
func TestUpsertKeeperStateClearsResetCreditsOnIdentityChange(t *testing.T) {
	t.Setenv("CPA_HELPER_DATA_DIR", t.TempDir())
	app, err := New()
	if err != nil {
		t.Fatalf("New() failed: %v", err)
	}
	defer app.Close()
	ctx := context.Background()

	// idx-1 inspects healthy with a populated reset-credit snapshot.
	two := 2
	if err := app.upsertKeeperState(ctx, healthyResetResult("reused.json", "idx-1", "acct-1", &two, stringPtr(resetCreditSnapshotJSON))); err != nil {
		t.Fatalf("upsert idx-1: %v", err)
	}
	state, err := app.getKeeperState(ctx, "reused.json")
	if err != nil {
		t.Fatalf("get after idx-1: %v", err)
	}
	if state.ResetCreditCount == nil || *state.ResetCreditCount != 2 || len(state.ResetCredits) != 1 {
		t.Fatalf("idx-1 snapshot not stored: count=%v credits=%d", state.ResetCreditCount, len(state.ResetCredits))
	}

	// Same auth_name reassigned to idx-2; the new identity's fetch failed (nil).
	if err := app.upsertKeeperState(ctx, healthyResetResult("reused.json", "idx-2", "acct-2", nil, nil)); err != nil {
		t.Fatalf("upsert idx-2: %v", err)
	}
	state, err = app.getKeeperState(ctx, "reused.json")
	if err != nil {
		t.Fatalf("get after idx-2: %v", err)
	}
	if state.AuthIndex == nil || *state.AuthIndex != "idx-2" {
		t.Fatalf("auth_index = %v, want idx-2", state.AuthIndex)
	}
	if state.ResetCreditCount != nil {
		t.Fatalf("reset_credit_count = %d, want nil (stale snapshot must be cleared on identity change)", *state.ResetCreditCount)
	}
	if len(state.ResetCredits) != 0 {
		t.Fatalf("reset_credits = %+v, want empty (must not show old account's schedule)", state.ResetCredits)
	}
}

// TestUpsertKeeperStatePreservesResetCreditsOnSameIdentity is the companion: a
// failed fetch on the SAME auth_index keeps the last good snapshot (the intended
// preserve-on-failure semantics), so the boundary fix does not over-clear.
func TestUpsertKeeperStatePreservesResetCreditsOnSameIdentity(t *testing.T) {
	t.Setenv("CPA_HELPER_DATA_DIR", t.TempDir())
	app, err := New()
	if err != nil {
		t.Fatalf("New() failed: %v", err)
	}
	defer app.Close()
	ctx := context.Background()

	two := 2
	if err := app.upsertKeeperState(ctx, healthyResetResult("stable.json", "idx-1", "acct-1", &two, stringPtr(resetCreditSnapshotJSON))); err != nil {
		t.Fatalf("upsert first: %v", err)
	}
	// Same identity, failed fetch (nil count/credits).
	if err := app.upsertKeeperState(ctx, healthyResetResult("stable.json", "idx-1", "acct-1", nil, nil)); err != nil {
		t.Fatalf("upsert second: %v", err)
	}
	state, err := app.getKeeperState(ctx, "stable.json")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if state.ResetCreditCount == nil || *state.ResetCreditCount != 2 || len(state.ResetCredits) != 1 {
		t.Fatalf("same-identity failed fetch must preserve snapshot: count=%v credits=%d", state.ResetCreditCount, len(state.ResetCredits))
	}
}

// TestUpsertKeeperStatePreservesResetCreditsOnUnknownIdentity covers the
// transient-failure path: when getKeeperRemoteAuthFile fails (network_error /
// 404) the result carries a nil AuthIndex. That unknown identity must NOT clear
// the previous snapshot — a momentary auth-file read failure on the same account
// should preserve the schedule, not drop it. Only a KNOWN, different auth_index
// (a real reassignment) clears it.
func TestUpsertKeeperStatePreservesResetCreditsOnUnknownIdentity(t *testing.T) {
	t.Setenv("CPA_HELPER_DATA_DIR", t.TempDir())
	app, err := New()
	if err != nil {
		t.Fatalf("New() failed: %v", err)
	}
	defer app.Close()
	ctx := context.Background()

	two := 2
	if err := app.upsertKeeperState(ctx, healthyResetResult("same.json", "idx-1", "acct-1", &two, stringPtr(resetCreditSnapshotJSON))); err != nil {
		t.Fatalf("upsert idx-1: %v", err)
	}
	// A transport/404 failure on the auth-file read: nil AuthIndex, network_error.
	failed := keeperAccountResult{
		Name:      "same.json",
		Result:    "network_error",
		AuthIndex: nil,
		CheckedAt: time.Now().In(appTimeLocation),
	}
	if err := app.upsertKeeperState(ctx, failed); err != nil {
		t.Fatalf("upsert network_error: %v", err)
	}
	state, err := app.getKeeperState(ctx, "same.json")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if state.ResetCreditCount == nil || *state.ResetCreditCount != 2 || len(state.ResetCredits) != 1 {
		t.Fatalf("unknown identity (nil auth_index) must preserve snapshot, not clear: count=%v credits=%d", state.ResetCreditCount, len(state.ResetCredits))
	}
}

// TestKeeperResetInspectHonorsPerAuthLock proves the fix for the concurrency
// blocker: the reset-triggered inspection goes through InspectAccountsLocked, which
// wires the runner's per-auth lock. When a background run already holds the lock
// for the account, the sync inspect must SKIP it (no concurrent usage request),
// instead of the old direct executeKeeperRunForAccounts call that bypassed the
// lock and issued a second in-flight usage request for the same account.
func TestKeeperResetInspectHonorsPerAuthLock(t *testing.T) {
	t.Setenv("CPA_HELPER_DATA_DIR", t.TempDir())

	const authName = "locked.json"
	var mu sync.Mutex
	usageCalls := 0
	cpa := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v0/management/auth-files":
			_ = json.NewEncoder(w).Encode(map[string]any{"files": []map[string]any{{"name": authName, "type": "codex"}}})
		case r.Method == http.MethodGet && r.URL.Path == "/v0/management/auth-files/download":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"name": authName, "type": "codex", "auth_index": "idx-lock",
				"email": "lock@example.com", "account_type": "pro", "disabled": false,
				"priority": 1, "access_token": "test-token",
			})
		case r.Method == http.MethodPost && r.URL.Path == "/v0/management/api-call":
			mu.Lock()
			usageCalls++
			mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]any{"status_code": 200, "body": map[string]any{
				"rate_limit": map[string]any{"primary_window": map[string]any{"used_percent": 10, "reset_after_seconds": 3600}},
			}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer cpa.Close()

	app, err := New()
	if err != nil {
		t.Fatalf("New() failed: %v", err)
	}
	defer app.Close()
	configureKeeperTestCPA(t, app, cpa.URL, nil)

	// A background run holds the per-auth lock for this account.
	if !app.keeper.tryLockAuthName("daemon", authName) {
		t.Fatal("could not acquire per-auth lock for the simulated background run")
	}

	// The reset-triggered sync inspect must skip the locked account — the per-auth
	// lock is the guard that prevents a concurrent inspection of the same account.
	if _, err := app.keeper.InspectAccountsLocked([]string{authName}); err != nil {
		t.Fatalf("InspectAccountsLocked returned %v; expected it to run and skip the locked account", err)
	}
	mu.Lock()
	got := usageCalls
	mu.Unlock()
	if got != 0 {
		t.Fatalf("usage calls = %d, want 0 — the sync inspect bypassed the per-auth lock and inspected a locked account", got)
	}

	// Once the background run releases the lock, a fresh sync inspect proceeds.
	app.keeper.unlockAuthName(authName)
	if _, err := app.keeper.InspectAccountsLocked([]string{authName}); err != nil {
		t.Fatalf("InspectAccountsLocked after unlock: %v", err)
	}
	mu.Lock()
	got = usageCalls
	mu.Unlock()
	if got == 0 {
		t.Fatal("usage calls still 0 after unlock — the sync inspect never ran even when the lock was free")
	}
}

// TestKeeperResetInspectNotBlockedByUnrelatedRun proves a targeted reset refresh
// is NOT blocked by an unrelated run occupying the global "accounts" mode: the
// account is still inspected (per-auth lock only, no global mode gate). Under the
// old RunAccountsSync (markRunning "accounts") this returned conflict and left the
// target's post-reset snapshot stale — this test would then show 0 usage calls.
func TestKeeperResetInspectNotBlockedByUnrelatedRun(t *testing.T) {
	t.Setenv("CPA_HELPER_DATA_DIR", t.TempDir())

	const target = "target.json"
	var mu sync.Mutex
	usageCalls := 0
	cpa := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v0/management/auth-files":
			_ = json.NewEncoder(w).Encode(map[string]any{"files": []map[string]any{{"name": target, "type": "codex"}}})
		case r.Method == http.MethodGet && r.URL.Path == "/v0/management/auth-files/download":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"name": target, "type": "codex", "auth_index": "idx-target",
				"email": "t@example.com", "account_type": "pro", "disabled": false,
				"priority": 1, "access_token": "test-token",
			})
		case r.Method == http.MethodPost && r.URL.Path == "/v0/management/api-call":
			mu.Lock()
			usageCalls++
			mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]any{"status_code": 200, "body": map[string]any{
				"rate_limit": map[string]any{"primary_window": map[string]any{"used_percent": 10, "reset_after_seconds": 3600}},
			}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer cpa.Close()

	app, err := New()
	if err != nil {
		t.Fatalf("New() failed: %v", err)
	}
	defer app.Close()
	configureKeeperTestCPA(t, app, cpa.URL, nil)

	// An unrelated manual refresh already occupies the global "accounts" mode.
	if !app.keeper.markRunning("accounts") {
		t.Fatal("could not mark accounts running")
	}
	// The targeted reset refresh of a DIFFERENT account must still run.
	if _, err := app.keeper.InspectAccountsLocked([]string{target}); err != nil {
		t.Fatalf("InspectAccountsLocked: %v", err)
	}
	mu.Lock()
	got := usageCalls
	mu.Unlock()
	if got == 0 {
		t.Fatal("usage calls = 0 — the reset refresh was blocked by an unrelated accounts run")
	}
}

// TestKeeperSafeReason proves audit reasons are stable machine codes and never
// leak a raw error message (which could carry a URL/token/upstream detail).
func TestKeeperSafeReason(t *testing.T) {
	if got := keeperSafeReason(nil); got != "ok" {
		t.Fatalf("nil -> %q, want ok", got)
	}
	if got := keeperSafeReason(validationError("auth_index missing")); got != "validation_error" {
		t.Fatalf("validationError -> %q, want validation_error", got)
	}
	if got := keeperSafeReason(notFoundError("gone")); got != "not_found" {
		t.Fatalf("notFoundError -> %q, want not_found", got)
	}
	if got := keeperSafeReason(conflictError("busy")); got != "conflict" {
		t.Fatalf("conflictError -> %q, want conflict", got)
	}
	raw := errors.New("dial https://cpa.internal:8317 failed: token=sk-secret")
	got := keeperSafeReason(raw)
	if got != "internal_error" {
		t.Fatalf("opaque error -> %q, want internal_error", got)
	}
	if strings.Contains(got, "token") || strings.Contains(got, "cpa.internal") || strings.Contains(got, "sk-secret") {
		t.Fatalf("safe reason leaked raw error content: %q", got)
	}
}

// TestKeeperRefreshAuditOutcome pins the post-reset refresh audit classification so
// the reconciliation log never misreports: a mode conflict is skipped (not error),
// a real run error is error, a vanished target (Total==0) is skipped/not_inspected
// (not ok), and only a genuine inspection is ok.
func TestKeeperRefreshAuditOutcome(t *testing.T) {
	cases := []struct {
		name       string
		stats      keeperStats
		err        error
		wantResult string
		wantReason string
	}{
		{"conflict is skipped", keeperStats{}, conflictError("busy"), "skipped", "conflict"},
		{"validation is error", keeperStats{}, validationError("bad"), "error", "validation_error"},
		{"opaque run error", keeperStats{}, errors.New("dial cpa.internal token=sk"), "error", "internal_error"},
		{"network error", keeperStats{Total: 1, NetworkError: 1}, nil, "error", "network_error"},
		{"status disabled (bad creds)", keeperStats{Total: 1, StatusDisabled: 1}, nil, "error", "status_disabled"},
		{"healthy but state write failed", keeperStats{Total: 1, Healthy: 1, StateWriteError: 1}, nil, "error", "state_write_error"},
		{"per-auth lock skip", keeperStats{Total: 1, Skipped: 1}, nil, "skipped", "account_busy"},
		{"vanished target not inspected", keeperStats{Total: 0}, nil, "skipped", "not_inspected"},
		{"inspected ok", keeperStats{Total: 1, Healthy: 1}, nil, "ok", ""},
		{"healthy but reset-credits unavailable is partial", keeperStats{Total: 1, Healthy: 1, ResetCreditsUnavailable: 1}, nil, "partial", "reset_credits_unavailable"},
		{"recovered enabled ok", keeperStats{Total: 1, StatusEnabled: 1}, nil, "ok", ""},
		{"priority degraded ok", keeperStats{Total: 1, PriorityDegraded: 1}, nil, "ok", ""},
		{"priority restored ok", keeperStats{Total: 1, PriorityRestored: 1}, nil, "ok", ""},
		{"total>0 no outcome not ok", keeperStats{Total: 1}, nil, "skipped", "not_inspected"},
		{"partial batch not ok", keeperStats{Total: 2, Healthy: 1}, nil, "skipped", "not_inspected"},
		{"disabled prioritized over skipped", keeperStats{Total: 2, StatusDisabled: 1, Skipped: 1}, nil, "error", "status_disabled"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			result, reason := keeperRefreshAuditOutcome(tc.stats, tc.err)
			if result != tc.wantResult || reason != tc.wantReason {
				t.Fatalf("got (%q,%q), want (%q,%q)", result, reason, tc.wantResult, tc.wantReason)
			}
		})
	}
}

// TestKeeperResetCreditsFetchFailureFlagged proves that when usage succeeds but the
// reset-credit fetch fails (inner 401/malformed/transport), the account stays
// healthy but the run reports ResetCreditsUnavailable — so a post-reset refresh is
// audited partial (reset_credits_unavailable), not a false ok.
func TestKeeperResetCreditsFetchFailureFlagged(t *testing.T) {
	t.Setenv("CPA_HELPER_DATA_DIR", t.TempDir())

	const authName = "creds-fail.json"
	cpa := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v0/management/auth-files":
			_ = json.NewEncoder(w).Encode(map[string]any{"files": []map[string]any{{"name": authName, "type": "codex"}}})
		case r.Method == http.MethodGet && r.URL.Path == "/v0/management/auth-files/download":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"name": authName, "type": "codex", "auth_index": "idx-cf",
				"email": "cf@example.com", "account_type": "pro", "disabled": false,
				"priority": 1, "access_token": "test-token",
			})
		case r.Method == http.MethodPost && r.URL.Path == "/v0/management/api-call":
			var payload struct {
				URL string `json:"url"`
			}
			_ = json.NewDecoder(r.Body).Decode(&payload)
			if strings.Contains(payload.URL, "rate-limit-reset-credits") {
				// Reset-credit fetch fails at the inner layer.
				_ = json.NewEncoder(w).Encode(map[string]any{"status_code": 401, "body": map[string]any{"detail": "unauth"}})
				return
			}
			// Usage check succeeds -> account healthy.
			_ = json.NewEncoder(w).Encode(map[string]any{"status_code": 200, "body": map[string]any{
				"rate_limit": map[string]any{"primary_window": map[string]any{"used_percent": 10, "reset_after_seconds": 3600}},
			}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer cpa.Close()

	app, err := New()
	if err != nil {
		t.Fatalf("New() failed: %v", err)
	}
	defer app.Close()
	configureKeeperTestCPA(t, app, cpa.URL, nil)

	stats, err := app.keeper.InspectAccountsLocked([]string{authName})
	if err != nil {
		t.Fatalf("InspectAccountsLocked: %v", err)
	}
	// Usage succeeded, so the account reaches an ok-class outcome (healthy or a
	// priority action), never disabled/network — the point is the account is fine.
	okCount := stats.Healthy + stats.StatusEnabled + stats.PriorityDegraded + stats.PriorityRestored
	if okCount != 1 || stats.NetworkError != 0 || stats.StatusDisabled != 0 {
		t.Fatalf("expected one healthy-class outcome, stats=%+v", stats)
	}
	if stats.ResetCreditsUnavailable != 1 {
		t.Fatalf("ResetCreditsUnavailable = %d, want 1 (reset-credit fetch failed)", stats.ResetCreditsUnavailable)
	}
	if result, reason := keeperRefreshAuditOutcome(stats, nil); result != "partial" || reason != "reset_credits_unavailable" {
		t.Fatalf("audit outcome = (%q,%q), want (partial, reset_credits_unavailable)", result, reason)
	}
}

// TestKeeperStatsAddSumsEveryField guards keeperStats.add so a newly added counter
// (e.g. ResetCreditsUnavailable) is not silently dropped by the generalized
// aggregation. Every field is given a distinct value and must sum.
func TestKeeperStatsAddSumsEveryField(t *testing.T) {
	base := keeperStats{Total: 1, Healthy: 2, StatusDisabled: 3, StatusEnabled: 4, PriorityDegraded: 5, PriorityRestored: 6, Skipped: 7, NetworkError: 8, IdentityError: 12, ResetCreditsUnavailable: 9, StateWriteError: 11}
	delta := keeperStats{Total: 10, Healthy: 20, StatusDisabled: 30, StatusEnabled: 40, PriorityDegraded: 50, PriorityRestored: 60, Skipped: 70, NetworkError: 80, IdentityError: 120, ResetCreditsUnavailable: 90, StateWriteError: 110}
	base.add(delta)
	want := keeperStats{Total: 11, Healthy: 22, StatusDisabled: 33, StatusEnabled: 44, PriorityDegraded: 55, PriorityRestored: 66, Skipped: 77, NetworkError: 88, IdentityError: 132, ResetCreditsUnavailable: 99, StateWriteError: 121}
	if base != want {
		t.Fatalf("add sum = %+v, want %+v", base, want)
	}
}

// TestKeeperResetInspectStateWriteFailure proves a DB write-back failure is not
// swallowed: with a BEFORE UPDATE trigger aborting the upsert, the account still
// inspects healthy, but the run reports StateWriteError, the stored snapshot is
// unchanged, and the audit outcome is error/state_write_error (never ok).
func TestKeeperResetInspectStateWriteFailure(t *testing.T) {
	t.Setenv("CPA_HELPER_DATA_DIR", t.TempDir())

	const authName = "writefail.json"
	credit := map[string]any{"id": "c1", "reset_type": "codex_rate_limits", "status": "available", "granted_at": "2026-08-22T00:08:46.146320Z", "expires_at": "2026-09-21T00:08:46.146320Z"}
	cpa := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v0/management/auth-files":
			_ = json.NewEncoder(w).Encode(map[string]any{"files": []map[string]any{{"name": authName, "type": "codex"}}})
		case r.Method == http.MethodGet && r.URL.Path == "/v0/management/auth-files/download":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"name": authName, "type": "codex", "auth_index": "idx-wf",
				"email": "wf@example.com", "account_type": "pro", "disabled": false,
				"priority": 1, "access_token": "test-token",
			})
		case r.Method == http.MethodPost && r.URL.Path == "/v0/management/api-call":
			var payload struct {
				URL string `json:"url"`
			}
			_ = json.NewDecoder(r.Body).Decode(&payload)
			if strings.Contains(payload.URL, "rate-limit-reset-credits") {
				_ = json.NewEncoder(w).Encode(map[string]any{"status_code": 200, "body": map[string]any{"available_count": 1, "credits": []map[string]any{credit}}})
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"status_code": 200, "body": map[string]any{
				"rate_limit": map[string]any{"primary_window": map[string]any{"used_percent": 10, "reset_after_seconds": 3600}},
			}})
		case r.Method == http.MethodPatch && r.URL.Path == "/v0/management/auth-files/fields":
			_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
		default:
			http.NotFound(w, r)
		}
	}))
	defer cpa.Close()

	app, err := New()
	if err != nil {
		t.Fatalf("New() failed: %v", err)
	}
	defer app.Close()
	configureKeeperTestCPA(t, app, cpa.URL, nil)

	// First inspection creates the row (INSERT, before the trigger exists).
	if _, err := app.keeper.InspectAccountsLocked([]string{authName}); err != nil {
		t.Fatalf("first inspect: %v", err)
	}
	before, err := app.getKeeperState(context.Background(), authName)
	if err != nil {
		t.Fatalf("get before: %v", err)
	}

	// Make every subsequent UPDATE fail, as the review probe did.
	if _, err := app.db.ExecContext(context.Background(),
		`CREATE TRIGGER block_keeper_update BEFORE UPDATE ON codex_keeper_auth_states BEGIN SELECT RAISE(ABORT, 'blocked'); END;`); err != nil {
		t.Fatalf("create trigger: %v", err)
	}

	// Second inspection: usage/reset-credits succeed, but the upsert (now an UPDATE)
	// aborts. The failure must surface, not be swallowed.
	stats, err := app.keeper.InspectAccountsLocked([]string{authName})
	if err != nil {
		t.Fatalf("second inspect: %v", err)
	}
	okCount := stats.Healthy + stats.StatusEnabled + stats.PriorityDegraded + stats.PriorityRestored
	if okCount < 1 {
		t.Fatalf("expected a healthy-class outcome, stats=%+v", stats)
	}
	if stats.StateWriteError != 1 {
		t.Fatalf("StateWriteError = %d, want 1 (write-back failure must be recorded)", stats.StateWriteError)
	}
	if result, reason := keeperRefreshAuditOutcome(stats, nil); result != "error" || reason != "state_write_error" {
		t.Fatalf("audit outcome = (%q,%q), want (error, state_write_error)", result, reason)
	}

	// The stored snapshot must be unchanged (the aborted UPDATE wrote nothing).
	after, err := app.getKeeperState(context.Background(), authName)
	if err != nil {
		t.Fatalf("get after: %v", err)
	}
	if before.LastCheckedAt == nil || after.LastCheckedAt == nil || !before.LastCheckedAt.Equal(*after.LastCheckedAt) {
		t.Fatalf("last_checked_at changed despite a failed write: before=%v after=%v", before.LastCheckedAt, after.LastCheckedAt)
	}

	// The raw DB error (the trigger's RAISE text) must NOT reach the Keeper UI log;
	// only the stable marker is user-visible.
	lines, err := app.loadKeeperLogLines(500)
	if err != nil {
		t.Fatalf("load keeper log lines: %v", err)
	}
	sawMarker := false
	for _, line := range lines {
		if strings.Contains(line, "blocked") {
			t.Fatalf("raw DB error leaked into Keeper log: %q", line)
		}
		if strings.Contains(line, "state_write_error") {
			sawMarker = true
		}
	}
	if !sawMarker {
		t.Fatal("expected a stable state_write_error marker in the Keeper log")
	}
}

// TestKeeperSubscriptionActiveUntil pins the tri-state contract: a parsed value
// is known, a confirmed-absent claim is known-and-nil (clears the stored value),
// and an unreadable/malformed claim is unknown (preserves the stored value).
func TestKeeperSubscriptionActiveUntil(t *testing.T) {
	idToken := func(m map[string]any) map[string]any {
		return map[string]any{"id_token": m}
	}
	want := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)

	cases := []struct {
		name      string
		authInfo  map[string]any
		wantTime  *time.Time
		wantKnown bool
	}{
		{"unix-seconds", idToken(map[string]any{"chatgpt_subscription_active_until": float64(want.Unix())}), &want, true},
		{"rfc3339", idToken(map[string]any{"chatgpt_subscription_active_until": "2026-10-01T00:00:00Z"}), &want, true},
		{"date-only", idToken(map[string]any{"chatgpt_subscription_active_until": "2026-10-01"}), &want, true},
		{"unix-string", idToken(map[string]any{"chatgpt_subscription_active_until": "1790812800"}), &want, true},
		// Confirmed absent: id_token readable but no claim -> known, nil (clear).
		{"claim-absent", idToken(map[string]any{"plan_type": "pro"}), nil, true},
		{"claim-null", idToken(map[string]any{"chatgpt_subscription_active_until": nil}), nil, true},
		// Unknown: unreadable id_token or malformed claim -> preserve.
		{"no-id-token", map[string]any{"name": "x"}, nil, false},
		{"id-token-not-map", map[string]any{"id_token": "raw.jwt.string"}, nil, false},
		{"empty-string", idToken(map[string]any{"chatgpt_subscription_active_until": ""}), nil, false},
		{"non-positive", idToken(map[string]any{"chatgpt_subscription_active_until": float64(0)}), nil, false},
		{"garbage-string", idToken(map[string]any{"chatgpt_subscription_active_until": "not-a-time"}), nil, false},
		{"wrong-type", idToken(map[string]any{"chatgpt_subscription_active_until": true}), nil, false},
		// Strict numeric: fractional, non-finite, and out-of-range epochs are rejected.
		{"fractional", idToken(map[string]any{"chatgpt_subscription_active_until": float64(want.Unix()) + 0.5}), nil, false},
		{"nan", idToken(map[string]any{"chatgpt_subscription_active_until": math.NaN()}), nil, false},
		{"positive-inf", idToken(map[string]any{"chatgpt_subscription_active_until": math.Inf(1)}), nil, false},
		{"negative-inf", idToken(map[string]any{"chatgpt_subscription_active_until": math.Inf(-1)}), nil, false},
		{"below-range", idToken(map[string]any{"chatgpt_subscription_active_until": float64(100)}), nil, false},        // ~1970
		{"above-range", idToken(map[string]any{"chatgpt_subscription_active_until": float64(5000000000)}), nil, false}, // ~2128
		{"max-int64", idToken(map[string]any{"chatgpt_subscription_active_until": float64(math.MaxInt64)}), nil, false},
		{"negative", idToken(map[string]any{"chatgpt_subscription_active_until": float64(-1)}), nil, false},
		{"fractional-explicit", idToken(map[string]any{"chatgpt_subscription_active_until": float64(1700000000.5)}), nil, false},
		{"unix-string-out-of-range", idToken(map[string]any{"chatgpt_subscription_active_until": "100"}), nil, false},
		// String date forms are range-gated too, not just the numeric path.
		{"rfc3339-year-0001", idToken(map[string]any{"chatgpt_subscription_active_until": "0001-01-01T00:00:00Z"}), nil, false},
		{"rfc3339-year-9999", idToken(map[string]any{"chatgpt_subscription_active_until": "9999-12-31T23:59:59Z"}), nil, false},
		{"date-year-1000", idToken(map[string]any{"chatgpt_subscription_active_until": "1000-01-01"}), nil, false},
		{"date-year-2500", idToken(map[string]any{"chatgpt_subscription_active_until": "2500-01-01"}), nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, known := keeperSubscriptionActiveUntil(tc.authInfo)
			if known != tc.wantKnown {
				t.Fatalf("known = %v, want %v", known, tc.wantKnown)
			}
			switch {
			case tc.wantTime == nil && got != nil:
				t.Fatalf("time = %v, want nil", got)
			case tc.wantTime != nil && (got == nil || !got.Equal(*tc.wantTime)):
				t.Fatalf("time = %v, want %v", got, tc.wantTime)
			}
		})
	}
}

// TestUpsertSubscriptionPreservedWhenIdentityUnconfirmed proves the subscription write is
// gated on a confirmed identity: a later inspection whose detail read failed (AuthIndex
// nil) must NOT overwrite/clear the stored renewal time even though SubscriptionKnown was
// set early from the list claim.
func TestUpsertSubscriptionPreservedWhenIdentityUnconfirmed(t *testing.T) {
	t.Setenv("CPA_HELPER_DATA_DIR", t.TempDir())
	app, err := New()
	if err != nil {
		t.Fatalf("New(): %v", err)
	}
	defer app.Close()
	ctx := context.Background()
	idx := "idx-sub"
	known := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)

	// A confirmed inspection stores a known subscription renewal time.
	if err := app.upsertKeeperState(ctx, keeperAccountResult{
		Name: "sub.json", Result: "healthy", CheckedAt: time.Now(),
		AuthIndex: &idx, SubscriptionActiveUntil: &known, SubscriptionKnown: true,
	}); err != nil {
		t.Fatalf("first upsert: %v", err)
	}

	// A later inspection whose DETAIL read failed: identity is unconfirmed (AuthIndex
	// nil) but SubscriptionKnown is still true. The stored value must be preserved.
	if err := app.upsertKeeperState(ctx, keeperAccountResult{
		Name: "sub.json", Result: "error", CheckedAt: time.Now(),
		AuthIndex: nil, SubscriptionActiveUntil: nil, SubscriptionKnown: true,
	}); err != nil {
		t.Fatalf("second upsert: %v", err)
	}

	st, err := app.getKeeperState(ctx, "sub.json")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if st.SubscriptionActiveUntil == nil || !st.SubscriptionActiveUntil.Equal(known) {
		t.Fatalf("subscription not preserved on unconfirmed identity: got %v, want %v", st.SubscriptionActiveUntil, known)
	}
}

// TestCreateKeeperRedeemConvergesAcrossApps proves the DB-atomic claim is truly
// cross-process, not just in-process: two App instances with independent DB handles on
// the SAME SQLite file concurrently claim a fresh redeem for the same identity and both
// converge on ONE winner request_id (so OpenAI dedups a single logical key).
func TestCreateKeeperRedeemConvergesAcrossApps(t *testing.T) {
	t.Setenv("CPA_HELPER_DATA_DIR", t.TempDir())
	app1, err := New()
	if err != nil {
		t.Fatalf("New() app1: %v", err)
	}
	defer app1.Close()
	// Same data dir → same SQLite file, but a distinct *sql.DB handle (a second process).
	app2, err := New()
	if err != nil {
		t.Fatalf("New() app2: %v", err)
	}
	defer app2.Close()

	ctx := context.Background()
	var wg sync.WaitGroup
	ids := make([]string, 2)
	errs := make([]error, 2)
	apps := []*App{app1, app2}
	wg.Add(2)
	for i := range apps {
		go func(i int) {
			defer wg.Done()
			ids[i], errs[i] = apps[i].createKeeperRedeem(ctx, "acct-shared")
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("app%d claim: %v", i+1, err)
		}
	}
	if ids[0] == "" || ids[0] != ids[1] {
		t.Fatalf("cross-process claims did not converge on one request_id: %v", ids)
	}
}

// keeperInsertPendingRedeem seeds a pending redeem ledger row (keyed by account_id).
func keeperInsertPendingRedeem(t *testing.T, app *App, accountID, id string) {
	t.Helper()
	if _, err := app.db.ExecContext(context.Background(),
		`INSERT INTO codex_keeper_reset_redeems (account_id, redeem_request_id, status, updated_at) VALUES (?, ?, 'pending', '2026-01-01 00:00:00')`,
		accountID, id); err != nil {
		t.Fatalf("seed pending redeem: %v", err)
	}
}

func keeperInsertStateRow(t *testing.T, app *App, authName string) {
	t.Helper()
	if err := app.upsertKeeperState(context.Background(), keeperAccountResult{
		Name: authName, Result: "healthy", CheckedAt: time.Now(),
	}); err != nil {
		t.Fatalf("seed state row: %v", err)
	}
}

// TestPruneKeepsAccountLedger proves prune deletes an absent account's STATE row but never
// touches the account_id-keyed redeem ledger, so a pending idempotency key survives a
// transient/empty remote list and the account can replay it after re-import.
func TestPruneKeepsAccountLedger(t *testing.T) {
	t.Setenv("CPA_HELPER_DATA_DIR", t.TempDir())
	app, err := New()
	if err != nil {
		t.Fatalf("New(): %v", err)
	}
	defer app.Close()
	ctx := context.Background()

	keeperInsertStateRow(t, app, "gone.json")
	keeperInsertPendingRedeem(t, app, "acct-gone", "rid-gone")

	// A transient/empty remote list marks the account stale.
	pruned, err := app.pruneKeeperMissingAuthStates(ctx, map[string]bool{})
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	if pruned != 1 {
		t.Fatalf("pruned = %d, want 1", pruned)
	}
	if st, _ := app.getKeeperState(ctx, "gone.json"); st != nil {
		t.Fatal("stale account state should have been pruned")
	}
	// The account_id-keyed ledger row must survive the prune.
	if id, ok, _ := app.lookupPendingKeeperRedeem(ctx, "acct-gone"); !ok || id != "rid-gone" {
		t.Fatalf("prune dropped the account's ledger key: got (%q,%v), want (rid-gone,true)", id, ok)
	}
}

// TestPruneFailsClosedWithoutRunner proves prune deletes nothing when there is no runner
// (no per-auth fence), rather than proceeding unlocked.
func TestPruneFailsClosedWithoutRunner(t *testing.T) {
	t.Setenv("CPA_HELPER_DATA_DIR", t.TempDir())
	app, err := New()
	if err != nil {
		t.Fatalf("New(): %v", err)
	}
	defer app.Close()
	ctx := context.Background()
	keeperInsertStateRow(t, app, "orphan.json")
	app.keeper = nil // simulate a maintenance/test variant without a runner

	pruned, err := app.pruneKeeperMissingAuthStates(ctx, map[string]bool{})
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	if pruned != 0 {
		t.Fatalf("prune without a runner deleted %d rows; must fail closed", pruned)
	}
	if st, _ := app.getKeeperState(ctx, "orphan.json"); st == nil {
		t.Fatal("prune without a fence deleted state; must skip")
	}
}

// TestDeleteStateRowKeepsLedger proves deleting a file's state row never drops the
// account_id-keyed redeem ledger, so a re-import (any filename) still replays the pending
// key rather than minting a new one.
func TestDeleteStateRowKeepsLedger(t *testing.T) {
	t.Setenv("CPA_HELPER_DATA_DIR", t.TempDir())
	app, err := New()
	if err != nil {
		t.Fatalf("New(): %v", err)
	}
	defer app.Close()
	ctx := context.Background()

	keeperInsertStateRow(t, app, "del.json")
	keeperInsertPendingRedeem(t, app, "acct-del", "rid-del")

	if _, err := app.deleteKeeperStateRow(ctx, "del.json"); err != nil {
		t.Fatalf("delete state row: %v", err)
	}
	if st, _ := app.getKeeperState(ctx, "del.json"); st != nil {
		t.Fatal("state row was not deleted")
	}
	if id, ok, _ := app.lookupPendingKeeperRedeem(ctx, "acct-del"); !ok || id != "rid-del" {
		t.Fatalf("delete dropped the account's ledger key: got (%q,%v), want (rid-del,true)", id, ok)
	}
}

// TestUpsertSubscriptionClearedOnAccountSwapSameIndex proves the subscription snapshot is
// scoped to the ACCOUNT, not just auth_index: when the same auth_name+auth_index is swapped
// to a different account_id and the new claim is unknown, the previous account's renewal
// date is NOT inherited (it is cleared); the same-account unknown case still preserves.
func TestUpsertSubscriptionClearedOnAccountSwapSameIndex(t *testing.T) {
	t.Setenv("CPA_HELPER_DATA_DIR", t.TempDir())
	app, err := New()
	if err != nil {
		t.Fatalf("New(): %v", err)
	}
	defer app.Close()
	ctx := context.Background()
	idx := "idx-fixed"
	acctA := "acct-A"
	acctB := "acct-B"
	known := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)

	// Account A stores a known renewal date.
	if err := app.upsertKeeperState(ctx, keeperAccountResult{
		Name: "swap.json", Result: "healthy", CheckedAt: time.Now(),
		AuthIndex: &idx, AccountID: &acctA, SubscriptionActiveUntil: &known, SubscriptionKnown: true,
	}); err != nil {
		t.Fatalf("seed A: %v", err)
	}

	// Same auth_index, same account, unknown claim → preserve.
	if err := app.upsertKeeperState(ctx, keeperAccountResult{
		Name: "swap.json", Result: "error", CheckedAt: time.Now(),
		AuthIndex: &idx, AccountID: &acctA, SubscriptionActiveUntil: nil, SubscriptionKnown: false,
	}); err != nil {
		t.Fatalf("same-account unknown: %v", err)
	}
	if st, _ := app.getKeeperState(ctx, "swap.json"); st.SubscriptionActiveUntil == nil || !st.SubscriptionActiveUntil.Equal(known) {
		t.Fatalf("same-account unknown must preserve; got %v", st.SubscriptionActiveUntil)
	}

	// Same auth_index but the file now backs a DIFFERENT account, unknown claim → clear
	// (must not inherit A's renewal date).
	if err := app.upsertKeeperState(ctx, keeperAccountResult{
		Name: "swap.json", Result: "error", CheckedAt: time.Now(),
		AuthIndex: &idx, AccountID: &acctB, SubscriptionActiveUntil: nil, SubscriptionKnown: false,
	}); err != nil {
		t.Fatalf("swap unknown: %v", err)
	}
	if st, _ := app.getKeeperState(ctx, "swap.json"); st.SubscriptionActiveUntil != nil {
		t.Fatalf("account swap must clear the inherited renewal date; got %v", st.SubscriptionActiveUntil)
	}
}

// TestUpsertResetCreditClearedOnAccountSwapSameIndex proves the reset-credit snapshot is
// scoped to the ACCOUNT (account_id), not just auth_index: when the same auth_name+index is
// swapped to a different account and the new fetch failed, the old account's count/credits
// are cleared (not inherited); the same-account failed fetch still preserves.
func TestUpsertResetCreditClearedOnAccountSwapSameIndex(t *testing.T) {
	t.Setenv("CPA_HELPER_DATA_DIR", t.TempDir())
	app, err := New()
	if err != nil {
		t.Fatalf("New(): %v", err)
	}
	defer app.Close()
	ctx := context.Background()
	idx := "idx-rc"
	acctA := "acct-A"
	acctB := "acct-B"
	count := 2
	credits := `[{"id":"c1","reset_type":"codex_rate_limits","status":"available"}]`

	// Account A stores a reset-credit snapshot.
	if err := app.upsertKeeperState(ctx, keeperAccountResult{
		Name: "rc.json", Result: "healthy", CheckedAt: time.Now(),
		AuthIndex: &idx, AccountID: &acctA, ResetCreditCount: &count, ResetCredits: &credits,
	}); err != nil {
		t.Fatalf("seed A: %v", err)
	}

	// Same account, fetch failed (nil snapshot) → preserve.
	if err := app.upsertKeeperState(ctx, keeperAccountResult{
		Name: "rc.json", Result: "healthy", CheckedAt: time.Now(),
		AuthIndex: &idx, AccountID: &acctA, ResetCreditCount: nil, ResetCredits: nil,
	}); err != nil {
		t.Fatalf("same-account failed fetch: %v", err)
	}
	if st, _ := app.getKeeperState(ctx, "rc.json"); st.ResetCreditCount == nil || *st.ResetCreditCount != 2 {
		t.Fatalf("same-account failed fetch must preserve count; got %v", st.ResetCreditCount)
	}

	// Same auth_index, DIFFERENT account, fetch failed → clear (do not inherit).
	if err := app.upsertKeeperState(ctx, keeperAccountResult{
		Name: "rc.json", Result: "healthy", CheckedAt: time.Now(),
		AuthIndex: &idx, AccountID: &acctB, ResetCreditCount: nil, ResetCredits: nil,
	}); err != nil {
		t.Fatalf("swap failed fetch: %v", err)
	}
	st, _ := app.getKeeperState(ctx, "rc.json")
	if st.ResetCreditCount != nil {
		t.Fatalf("account swap must clear the inherited reset-credit count; got %v", *st.ResetCreditCount)
	}
	if len(st.ResetCredits) != 0 {
		t.Fatalf("account swap must clear the inherited reset-credit list; got %v", st.ResetCredits)
	}
}

// TestKeeperReconcileInspectionAccountID pins the cross-source identity reconciliation:
// the list id_token.chatgpt_account_id and the download account_id must AGREE (or only one
// present) to be trusted; a conflict — or a single source that is self-contradictory —
// yields (·, false) so the caller treats the identity as unknown.
func TestKeeperReconcileInspectionIdentity(t *testing.T) {
	idTokenAcct := func(id string) map[string]any {
		return map[string]any{"id_token": map[string]any{"chatgpt_account_id": id}}
	}
	cases := []struct {
		name     string
		authInfo map[string]any
		detail   map[string]any
		wantID   string
		wantOK   bool
	}{
		{"agree", idTokenAcct("acct-A"), map[string]any{"account_id": "acct-A"}, "acct-A", true},
		{"list-only", idTokenAcct("acct-A"), map[string]any{}, "acct-A", true},
		{"detail-only", map[string]any{}, map[string]any{"account_id": "acct-B"}, "acct-B", true},
		{"neither", map[string]any{}, map[string]any{}, "", true},
		{"account-conflict", idTokenAcct("acct-A"), map[string]any{"account_id": "acct-B"}, "", false},
		// A single source self-contradicting (top-level vs id_token claim) is also untrusted.
		{"detail-self-conflict", map[string]any{}, map[string]any{"account_id": "acct-B", "id_token": map[string]any{"chatgpt_account_id": "acct-C"}}, "", false},
		// auth_index conflict alone (accounts agree) is also untrusted.
		{"authindex-conflict", map[string]any{"auth_index": "idx-A", "id_token": map[string]any{"chatgpt_account_id": "acct-A"}}, map[string]any{"auth_index": "idx-B", "account_id": "acct-A"}, "", false},
		// account AND auth_index agree.
		{"both-agree", map[string]any{"auth_index": "idx-A", "id_token": map[string]any{"chatgpt_account_id": "acct-A"}}, map[string]any{"auth_index": "idx-A", "account_id": "acct-A"}, "acct-A", true},
		// A raw JWT id_token string (CLIProxyAPI's real download form) is decoded and its
		// chatgpt_account_id claim cross-checked against the top-level account_id.
		{"rawjwt-agree", map[string]any{"account_id": "acct-A"}, map[string]any{"account_id": "acct-A", "id_token": keeperTestJWT(t, map[string]any{"chatgpt_account_id": "acct-A"})}, "acct-A", true},
		// The deceptive case: top-level account_id A but a raw JWT claim B → conflict, untrusted.
		{"rawjwt-conflict", map[string]any{"account_id": "acct-A"}, map[string]any{"account_id": "acct-A", "id_token": keeperTestJWT(t, map[string]any{"chatgpt_account_id": "acct-B"})}, "", false},
		// The claim nested under the OpenAI auth namespace is also cross-checked.
		{"rawjwt-namespace-conflict", map[string]any{"account_id": "acct-A"}, map[string]any{"account_id": "acct-A", "id_token": keeperTestJWT(t, map[string]any{"https://api.openai.com/auth": map[string]any{"chatgpt_account_id": "acct-B"}})}, "", false},
		// An id_token that is present but unparseable leaves the identity indeterminate → fail closed.
		{"idtoken-unparseable", map[string]any{}, map[string]any{"account_id": "acct-A", "id_token": "not-a-jwt"}, "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := keeperReconcileInspectionIdentity(tc.authInfo, tc.detail)
			if ok != tc.wantOK || got != tc.wantID {
				t.Fatalf("reconcile = (%q,%v), want (%q,%v)", got, ok, tc.wantID, tc.wantOK)
			}
		})
	}
}

// keeperTestJWT builds a raw JWT string (header.payload.sig, base64url) carrying the given
// claims — the shape CLIProxyAPI's real download auth JSON uses for id_token, so tests can
// exercise the raw-JWT identity cross-check. The signature is a placeholder (identity parsing
// reads the payload, it does not verify the signature).
func keeperTestJWT(t *testing.T, claims map[string]any) string {
	t.Helper()
	enc := func(v any) string {
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatalf("marshal jwt part: %v", err)
		}
		return base64.RawURLEncoding.EncodeToString(b)
	}
	return enc(map[string]any{"alg": "none", "typ": "JWT"}) + "." + enc(claims) + ".sig"
}

// TestKeeperLedgerPerAccountAndRouteAgnostic proves the redeem ledger keys on the stable
// account_id: distinct accounts keep separate rows, the SAME account converges on one key
// regardless of how it is routed (a claim reusing the same account_id returns the existing
// pending id), and a resolved (terminal) row lets the next claim mint a fresh id.
func TestKeeperLedgerPerAccountAndRouteAgnostic(t *testing.T) {
	t.Setenv("CPA_HELPER_DATA_DIR", t.TempDir())
	app, err := New()
	if err != nil {
		t.Fatalf("New(): %v", err)
	}
	defer app.Close()
	ctx := context.Background()

	// Two distinct accounts get distinct pending rows.
	idA, err := app.createKeeperRedeem(ctx, "acct-A")
	if err != nil {
		t.Fatalf("claim A: %v", err)
	}
	idB, err := app.createKeeperRedeem(ctx, "acct-B")
	if err != nil {
		t.Fatalf("claim B: %v", err)
	}
	if idA == idB {
		t.Fatalf("distinct accounts share a request_id %q; each must get its own", idA)
	}

	// The SAME account, however it is now routed, converges on its existing pending id
	// (route-agnostic) rather than minting a new one.
	idAAgain, err := app.createKeeperRedeem(ctx, "acct-A")
	if err != nil {
		t.Fatalf("re-claim A: %v", err)
	}
	if idAAgain != idA {
		t.Fatalf("re-claim of account A minted a new id %q (want existing %q)", idAAgain, idA)
	}
	if got, ok, _ := app.lookupPendingKeeperRedeem(ctx, "acct-A"); !ok || got != idA {
		t.Fatalf("account A pending lookup = (%q,%v), want (%q,true)", got, ok, idA)
	}

	// After A resolves (terminal), the next claim mints a fresh id.
	if err := app.finishKeeperRedeem(ctx, "acct-A", idA, keeperResetCreditCodeReset); err != nil {
		t.Fatalf("finish A: %v", err)
	}
	idAFresh, err := app.createKeeperRedeem(ctx, "acct-A")
	if err != nil {
		t.Fatalf("fresh claim A: %v", err)
	}
	if idAFresh == idA {
		t.Fatalf("claim after a resolved redeem reused the terminal id %q; must mint fresh", idA)
	}
}

// TestKeeperInspectIdentityConflictPreservesSnapshot proves an inspection whose list and
// download identities conflict (list account_id A, detail account_id B) bails out with an
// identity_error: it does NOT fetch the reset credits, preserves the previous snapshot, and
// the refresh audit reports error (never a healthy/ok refresh).
func TestKeeperInspectIdentityConflictPreservesSnapshot(t *testing.T) {
	t.Setenv("CPA_HELPER_DATA_DIR", t.TempDir())
	const authName = "conflict.json"
	var mu sync.Mutex
	creditFetches := 0
	cpa := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v0/management/auth-files":
			// List identity: account A.
			_ = json.NewEncoder(w).Encode(map[string]any{"files": []map[string]any{
				{"name": authName, "type": "codex", "id_token": map[string]any{"chatgpt_account_id": "acct-LIST-A"}},
			}})
		case r.Method == http.MethodGet && r.URL.Path == "/v0/management/auth-files/download":
			// Download identity: account B (conflicts with the list).
			_ = json.NewEncoder(w).Encode(map[string]any{
				"name": authName, "type": "codex", "auth_index": "idx-1", "account_id": "acct-DETAIL-B",
				"email": "c@example.com", "account_type": "pro", "disabled": false, "priority": 1, "access_token": "test-token",
			})
		case r.Method == http.MethodPost && r.URL.Path == "/v0/management/api-call":
			var p struct {
				URL string `json:"url"`
			}
			_ = json.NewDecoder(r.Body).Decode(&p)
			if strings.Contains(p.URL, "rate-limit-reset-credits") {
				mu.Lock()
				creditFetches++
				mu.Unlock()
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"status_code": 200, "body": map[string]any{"rate_limit": map[string]any{"primary_window": map[string]any{"used_percent": 10, "reset_after_seconds": 3600}}}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer cpa.Close()

	app, err := New()
	if err != nil {
		t.Fatalf("New(): %v", err)
	}
	defer app.Close()
	configureKeeperTestCPA(t, app, cpa.URL, nil)
	ctx := context.Background()

	// Seed a FULL good prior snapshot for account B: every business column set, so we can
	// prove the identity-conflict write preserves all of them, not just reset credits.
	idx, acctB, count := "idx-1", "acct-DETAIL-B", 5
	email, acctType := "b@example.com", "pro"
	prio, prim, sec, qt := 1, 40, 20, 80
	dis := false
	healthyAt := time.Now().Add(-time.Hour).Truncate(time.Second)
	sub := time.Now().Add(720 * time.Hour).Truncate(time.Second)
	if err := app.upsertKeeperState(ctx, keeperAccountResult{
		Name: authName, Result: "healthy", CheckedAt: healthyAt, Email: &email, AuthIndex: &idx,
		AccountID: &acctB, AccountType: &acctType, Disabled: &dis, Priority: &prio,
		PrimaryUsedPercent: &prim, SecondaryUsedPercent: &sec, QuotaThreshold: &qt,
		SubscriptionActiveUntil: &sub, ResetCreditCount: &count, ResetCredits: stringPtr(resetCreditSnapshotJSON),
	}); err != nil {
		t.Fatalf("seed snapshot: %v", err)
	}
	before, err := app.getKeeperState(ctx, authName)
	if err != nil {
		t.Fatalf("read seeded state: %v", err)
	}

	stats, err := app.keeper.InspectAccountsLocked([]string{authName})
	if err != nil {
		t.Fatalf("InspectAccountsLocked: %v", err)
	}
	if stats.IdentityError != 1 || stats.Healthy != 0 {
		t.Fatalf("stats = %+v, want IdentityError=1, Healthy=0", stats)
	}
	if result, reason := keeperRefreshAuditOutcome(stats, nil); result != "error" || reason != "identity_error" {
		t.Fatalf("audit outcome = (%q,%q), want (error, identity_error)", result, reason)
	}
	mu.Lock()
	fetches := creditFetches
	mu.Unlock()
	if fetches != 0 {
		t.Fatalf("reset-credit fetched %d times on identity conflict; must not fetch from a mixed detail", fetches)
	}
	// EVERY business column of the prior snapshot must survive intact — an identity
	// conflict must not clobber email/auth_index/account_id/account_type/disabled/priority/
	// usage/quota/reset-credit/subscription to NULL (a cleared auth_index would also block a
	// later reset). Only last_error/latest_action/last_checked_at may change.
	st, err := app.getKeeperState(ctx, authName)
	if err != nil {
		t.Fatalf("get state: %v", err)
	}
	if st.Email == nil || *st.Email != "b@example.com" || st.AuthIndex == nil || *st.AuthIndex != "idx-1" ||
		st.AccountID == nil || *st.AccountID != "acct-DETAIL-B" || st.AccountType == nil || *st.AccountType != "pro" ||
		st.Disabled != false || st.Priority == nil || *st.Priority != 1 {
		t.Fatalf("identity fields not preserved on conflict: %+v", st)
	}
	if st.PrimaryUsedPercent == nil || *st.PrimaryUsedPercent != 40 ||
		st.SecondaryUsedPercent == nil || *st.SecondaryUsedPercent != 20 ||
		st.QuotaThreshold == nil || *st.QuotaThreshold != 80 {
		t.Fatalf("usage/quota not preserved on conflict: %+v", st)
	}
	if st.ResetCreditCount == nil || *st.ResetCreditCount != 5 || len(st.ResetCredits) != 1 {
		t.Fatalf("reset credits not preserved on conflict: count=%v credits=%d", st.ResetCreditCount, len(st.ResetCredits))
	}
	if st.SubscriptionActiveUntil == nil || !st.SubscriptionActiveUntil.Equal(*before.SubscriptionActiveUntil) {
		t.Fatalf("subscription not preserved on conflict: got=%v want=%v", st.SubscriptionActiveUntil, before.SubscriptionActiveUntil)
	}
	// last_healthy_at must NOT advance (a conflict is not a healthy refresh).
	if st.LastHealthyAt == nil || before.LastHealthyAt == nil || !st.LastHealthyAt.Equal(*before.LastHealthyAt) {
		t.Fatalf("last_healthy_at changed on conflict: got=%v want=%v", st.LastHealthyAt, before.LastHealthyAt)
	}
	// The error/latest_action IS updated, and the check time advances.
	if st.LastError == nil {
		t.Fatal("identity conflict did not record an error on the account")
	}
	if st.LastCheckedAt == nil || !st.LastCheckedAt.After(healthyAt) {
		t.Fatalf("last_checked_at not advanced on conflict: got=%v seed=%v", st.LastCheckedAt, healthyAt)
	}
}

// TestKeeperInspectNeitherAccountIDSkipsAccountScopedWrites proves that when neither the list
// nor the download detail carries an account_id (a legacy auth_index-only credential), the
// inspection does NOT fetch a reset-credit snapshot (it cannot attribute it to a resource) and
// does NOT bind the list's subscription claim; it preserves the prior account-scoped snapshot
// (reset credits, subscription, the previously-known account_id) and reports the refresh as
// partial/reset_credits_unavailable — never a falsely-healthy ok that overwrote account state.
func TestKeeperInspectNeitherAccountIDSkipsAccountScopedWrites(t *testing.T) {
	t.Setenv("CPA_HELPER_DATA_DIR", t.TempDir())
	const authName = "legacy.json"
	// The list carries a PARSEABLE renewal claim that DIFFERS from the seeded snapshot but
	// still no account_id — so this pins the subscription guard: with an unknown account_id
	// the list's renewal must NOT be bound; the old snapshot value must be preserved.
	listSub := time.Now().Add(1000 * time.Hour).UTC().Truncate(time.Second)
	var mu sync.Mutex
	creditFetches := 0
	cpa := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v0/management/auth-files":
			// List entry: auth_index + a subscription claim, but NO account_id (the id_token
			// has chatgpt_subscription_active_until yet no chatgpt_account_id).
			_ = json.NewEncoder(w).Encode(map[string]any{"files": []map[string]any{
				{"name": authName, "type": "codex", "auth_index": "idx-1",
					"id_token": map[string]any{"chatgpt_subscription_active_until": listSub.Format(time.RFC3339)}},
			}})
		case r.Method == http.MethodGet && r.URL.Path == "/v0/management/auth-files/download":
			// Download detail: auth_index only, NO account_id.
			_ = json.NewEncoder(w).Encode(map[string]any{
				"name": authName, "type": "codex", "auth_index": "idx-1",
				"email": "legacy@example.com", "account_type": "pro", "disabled": false, "priority": 1, "access_token": "test-token",
			})
		case r.Method == http.MethodPost && r.URL.Path == "/v0/management/api-call":
			var p struct {
				URL string `json:"url"`
			}
			_ = json.NewDecoder(r.Body).Decode(&p)
			if strings.Contains(p.URL, "rate-limit-reset-credits") {
				mu.Lock()
				creditFetches++
				mu.Unlock()
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"status_code": 200, "body": map[string]any{"rate_limit": map[string]any{"primary_window": map[string]any{"used_percent": 10, "reset_after_seconds": 3600}}}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer cpa.Close()

	app, err := New()
	if err != nil {
		t.Fatalf("New(): %v", err)
	}
	defer app.Close()
	configureKeeperTestCPA(t, app, cpa.URL, nil)
	ctx := context.Background()

	// Seed a prior account-scoped snapshot bound to a KNOWN account_id, with reset credits
	// and a subscription renewal date — none of which this inspection may overwrite.
	idx, priorAcct, count := "idx-1", "acct-KNOWN-X", 5
	sub := time.Now().Add(720 * time.Hour).Truncate(time.Second)
	if err := app.upsertKeeperState(ctx, keeperAccountResult{
		Name: authName, Result: "healthy", CheckedAt: time.Now(), AuthIndex: &idx, AccountID: &priorAcct,
		ResetCreditCount: &count, ResetCredits: stringPtr(resetCreditSnapshotJSON),
		SubscriptionActiveUntil: &sub, SubscriptionKnown: true,
	}); err != nil {
		t.Fatalf("seed snapshot: %v", err)
	}

	stats, err := app.keeper.InspectAccountsLocked([]string{authName})
	if err != nil {
		t.Fatalf("InspectAccountsLocked: %v", err)
	}
	// Usage ran (an ok-family outcome) but the reset-credit snapshot could not be refreshed,
	// and this is never an identity/network error.
	okFamily := stats.Healthy + stats.StatusEnabled + stats.PriorityDegraded + stats.PriorityRestored
	if okFamily != 1 || stats.ResetCreditsUnavailable != 1 || stats.IdentityError != 0 || stats.NetworkError != 0 {
		t.Fatalf("stats = %+v, want ok-family=1, ResetCreditsUnavailable=1, IdentityError=0, NetworkError=0", stats)
	}
	if result, reason := keeperRefreshAuditOutcome(stats, nil); result != "partial" || reason != "reset_credits_unavailable" {
		t.Fatalf("audit outcome = (%q,%q), want (partial, reset_credits_unavailable)", result, reason)
	}
	mu.Lock()
	fetches := creditFetches
	mu.Unlock()
	if fetches != 0 {
		t.Fatalf("reset-credit fetched %d times with no account_id; must not attribute a snapshot to an unknown resource", fetches)
	}
	// The prior account-scoped snapshot must survive: reset credits, subscription, AND the
	// previously-known account_id (COALESCE preserves it when this inspection had none).
	st, err := app.getKeeperState(ctx, authName)
	if err != nil {
		t.Fatalf("get state: %v", err)
	}
	if st.ResetCreditCount == nil || *st.ResetCreditCount != 5 || len(st.ResetCredits) != 1 {
		t.Fatalf("reset credits not preserved with unknown account_id: count=%v credits=%d", st.ResetCreditCount, len(st.ResetCredits))
	}
	if st.SubscriptionActiveUntil == nil || !st.SubscriptionActiveUntil.Equal(sub) {
		t.Fatalf("subscription not preserved with unknown account_id: got=%v want=%v", st.SubscriptionActiveUntil, sub)
	}
	if st.SubscriptionActiveUntil.Equal(listSub) {
		t.Fatalf("list renewal claim was bound despite unknown account_id: got=%v (list=%v)", st.SubscriptionActiveUntil, listSub)
	}
	if st.AccountID == nil || *st.AccountID != "acct-KNOWN-X" {
		t.Fatalf("prior account_id not preserved with unknown inspection identity: %v", st.AccountID)
	}
}

// TestKeeperInspectRawJWTAccountConflictPreservesSnapshot proves the inspection path also
// catches a deceptive raw-JWT identity: the download detail has top-level account_id=A but a
// raw JWT id_token whose account claim (nested under the OpenAI auth namespace — the real
// on-wire location) is B. The detail is self-contradictory, so the inspection bails as
// identity_error: NO reset-credit fetch, NO usage/credit snapshot write, the prior snapshot
// preserved, and the refresh audited as an error.
func TestKeeperInspectRawJWTAccountConflictPreservesSnapshot(t *testing.T) {
	t.Setenv("CPA_HELPER_DATA_DIR", t.TempDir())
	const authName = "jwt-conflict.json"
	var mu sync.Mutex
	creditFetches := 0
	deceptiveJWT := keeperTestJWT(t, map[string]any{"https://api.openai.com/auth": map[string]any{"chatgpt_account_id": "acct-B"}})
	cpa := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v0/management/auth-files":
			_ = json.NewEncoder(w).Encode(map[string]any{"files": []map[string]any{
				{"name": authName, "type": "codex", "auth_index": "idx-1", "id_token": map[string]any{"chatgpt_account_id": "acct-A"}},
			}})
		case r.Method == http.MethodGet && r.URL.Path == "/v0/management/auth-files/download":
			// Top-level account_id=A, but the raw JWT's own claim is B → self-contradictory.
			_ = json.NewEncoder(w).Encode(map[string]any{
				"name": authName, "type": "codex", "auth_index": "idx-1", "account_id": "acct-A",
				"id_token": deceptiveJWT, "email": "j@example.com", "account_type": "pro",
				"disabled": false, "priority": 1, "access_token": "test-token",
			})
		case r.Method == http.MethodPost && r.URL.Path == "/v0/management/api-call":
			var p struct {
				URL string `json:"url"`
			}
			_ = json.NewDecoder(r.Body).Decode(&p)
			if strings.Contains(p.URL, "rate-limit-reset-credits") {
				mu.Lock()
				creditFetches++
				mu.Unlock()
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"status_code": 200, "body": map[string]any{"rate_limit": map[string]any{"primary_window": map[string]any{"used_percent": 99, "reset_after_seconds": 3600}}}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer cpa.Close()

	app, err := New()
	if err != nil {
		t.Fatalf("New(): %v", err)
	}
	defer app.Close()
	configureKeeperTestCPA(t, app, cpa.URL, nil)
	ctx := context.Background()

	// Seed a prior snapshot with a KNOWN usage percent + reset credits to prove they survive.
	idx, acct, count, used := "idx-1", "acct-A", 5, 42
	if err := app.upsertKeeperState(ctx, keeperAccountResult{
		Name: authName, Result: "healthy", CheckedAt: time.Now(), AuthIndex: &idx, AccountID: &acct,
		PrimaryUsedPercent: &used, ResetCreditCount: &count, ResetCredits: stringPtr(resetCreditSnapshotJSON),
	}); err != nil {
		t.Fatalf("seed snapshot: %v", err)
	}

	stats, err := app.keeper.InspectAccountsLocked([]string{authName})
	if err != nil {
		t.Fatalf("InspectAccountsLocked: %v", err)
	}
	if stats.IdentityError != 1 || stats.Healthy != 0 {
		t.Fatalf("stats = %+v, want IdentityError=1, Healthy=0", stats)
	}
	if result, reason := keeperRefreshAuditOutcome(stats, nil); result != "error" || reason != "identity_error" {
		t.Fatalf("audit outcome = (%q,%q), want (error, identity_error)", result, reason)
	}
	mu.Lock()
	fetches := creditFetches
	mu.Unlock()
	if fetches != 0 {
		t.Fatalf("reset-credit fetched %d times on a raw-JWT identity conflict; must not fetch", fetches)
	}
	// The usage snapshot must NOT be overwritten by the deceptive inspection's fresh 99%.
	st, err := app.getKeeperState(ctx, authName)
	if err != nil {
		t.Fatalf("get state: %v", err)
	}
	if st.PrimaryUsedPercent == nil || *st.PrimaryUsedPercent != 42 {
		t.Fatalf("usage snapshot overwritten on raw-JWT conflict: got=%v want=42", st.PrimaryUsedPercent)
	}
	if st.ResetCreditCount == nil || *st.ResetCreditCount != 5 {
		t.Fatalf("reset credits not preserved on raw-JWT conflict: %v", st.ResetCreditCount)
	}
	if st.LastError == nil {
		t.Fatal("raw-JWT identity conflict did not record an error")
	}
}

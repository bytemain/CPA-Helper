package app_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	backendApp "cpa-helper/backend/internal/app"
)

type keeperResetResponse struct {
	Status  string `json:"status"`
	Account struct {
		Name             string  `json:"name"`
		QuotaResetCount  int     `json:"quota_reset_count"`
		LastQuotaResetAt *string `json:"last_quota_reset_at"`
	} `json:"account"`
}

type keeperResetAccountsResponse struct {
	Items []struct {
		Name             string  `json:"name"`
		QuotaResetCount  int     `json:"quota_reset_count"`
		LastQuotaResetAt *string `json:"last_quota_reset_at"`
	} `json:"items"`
}

// TestKeeperQuotaReset drives the real /api/codex-keeper/reset-quota route:
// a successful CLIProxyAPI reset-quota call increments the per-auth counter
// (visible via /accounts), a CLIProxyAPI failure surfaces the error WITHOUT
// incrementing, and bad requests are rejected before any CLIProxyAPI call.
func accountsResetCount(t *testing.T, handler http.Handler, cookies []*http.Cookie) int {
	t.Helper()
	accounts := keeperResetAccountsResponse{}
	requestJSON(t, handler, http.MethodGet, "/api/codex-keeper/accounts", nil, cookies, &accounts)
	if len(accounts.Items) != 1 {
		t.Fatalf("accounts listing has %d items, want 1", len(accounts.Items))
	}
	return accounts.Items[0].QuotaResetCount
}

func TestKeeperQuotaReset(t *testing.T) {
	t.Setenv("CPA_HELPER_DATA_DIR", t.TempDir())

	authName := "reset-me.json"
	authDetail := map[string]any{
		"name":         authName,
		"type":         "codex",
		"auth_index":   "idx-7",
		"email":        "reset@example.com",
		"account_type": "plus",
		"disabled":     false,
		"priority":     1,
		"access_token": "test-token",
	}

	var mu sync.Mutex
	resetCalls := []string{}
	resetMode := "ok" // ok | http-fail | empty-body | wrong-index | bad-status

	cpa := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v0/management/auth-files":
			_ = json.NewEncoder(w).Encode(map[string]any{"files": []map[string]any{{"name": authName, "type": "codex"}}})
		case r.Method == http.MethodGet && r.URL.Path == "/v0/management/auth-files/download":
			_ = json.NewEncoder(w).Encode(authDetail)
		case r.Method == http.MethodPost && r.URL.Path == "/v0/management/api-call":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"status_code": 200,
				"body": map[string]any{
					"rate_limit": map[string]any{
						"primary_window": map[string]any{"used_percent": 10, "reset_after_seconds": 3600},
					},
				},
			})
		case r.Method == http.MethodPatch && r.URL.Path == "/v0/management/auth-files/fields":
			_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
		case r.Method == http.MethodPost && r.URL.Path == "/v0/management/reset-quota":
			var payload struct {
				AuthIndex string `json:"auth_index"`
			}
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil || payload.AuthIndex == "" {
				http.Error(w, "auth_index is required", http.StatusBadRequest)
				return
			}
			mu.Lock()
			mode := resetMode
			if mode == "ok" {
				resetCalls = append(resetCalls, payload.AuthIndex)
			}
			mu.Unlock()
			switch mode {
			case "http-fail":
				http.Error(w, "boom", http.StatusBadRequest)
			case "empty-body":
				_, _ = w.Write([]byte(`{}`))
			case "wrong-index":
				_ = json.NewEncoder(w).Encode(map[string]any{"status": "ok", "auth_index": "someone-else", "models": []string{}})
			case "padded-index":
				_ = json.NewEncoder(w).Encode(map[string]any{"status": "ok", "auth_index": "  " + payload.AuthIndex + "  ", "models": []string{}})
			case "bad-status":
				_ = json.NewEncoder(w).Encode(map[string]any{"status": "error", "auth_index": payload.AuthIndex})
			default:
				_ = json.NewEncoder(w).Encode(map[string]any{"status": "ok", "auth_index": payload.AuthIndex, "models": []string{}})
			}
		default:
			http.NotFound(w, r)
		}
	}))
	defer cpa.Close()

	app, err := backendApp.New()
	if err != nil {
		t.Fatalf("New() failed: %v", err)
	}
	defer app.Close()
	handler := app.Routes()

	cookies := requestJSON(t, handler, http.MethodPost, "/api/auth/setup", map[string]any{
		"username": "admin",
		"password": "test-password",
		"nickname": "Admin",
	}, nil, nil)
	requestJSON(t, handler, http.MethodPut, "/api/settings", map[string]any{
		"cliaproxy_url":     cpa.URL,
		"management_key":    "test-management-key",
		"collector_enabled": false,
	}, cookies, nil)
	requestJSON(t, handler, http.MethodPut, "/api/codex-keeper/settings", map[string]any{
		"schedule_cron":       "0 0 29 2 *",
		"dry_run":             false,
		"quota_threshold":     100,
		"worker_threads":      1,
		"cpa_timeout_seconds": 1,
	}, cookies, nil)
	requestJSON(t, handler, http.MethodPost, "/api/codex-keeper/run-once", nil, cookies, nil)
	waitForKeeperAccounts(t, handler, cookies, 1)

	// Happy path: reset succeeds, counter becomes 1 and carries a timestamp.
	reset := keeperResetResponse{}
	requestJSON(t, handler, http.MethodPost, "/api/codex-keeper/reset-quota", map[string]any{"auth_name": authName}, cookies, &reset)
	if reset.Status != "ok" || reset.Account.Name != authName {
		t.Fatalf("reset response = %+v, want ok for %s", reset, authName)
	}
	if reset.Account.QuotaResetCount != 1 || reset.Account.LastQuotaResetAt == nil {
		t.Fatalf("first reset count = %d (lastAt=%v), want 1 with timestamp", reset.Account.QuotaResetCount, reset.Account.LastQuotaResetAt)
	}
	mu.Lock()
	if len(resetCalls) != 1 || resetCalls[0] != "idx-7" {
		mu.Unlock()
		t.Fatalf("CLIProxyAPI reset calls = %v, want exactly one for auth_index %q", resetCalls, "idx-7")
	}
	mu.Unlock()

	// Wire minimalism: the reset response must not leak internal account fields.
	raw := map[string]json.RawMessage{}
	requestJSON(t, handler, http.MethodPost, "/api/codex-keeper/reset-quota", map[string]any{"auth_name": authName}, cookies, &raw)
	var accountFields map[string]any
	if err := json.Unmarshal(raw["account"], &accountFields); err != nil {
		t.Fatalf("decode reset account payload: %v", err)
	}
	for key := range accountFields {
		switch key {
		case "name", "quota_reset_count", "last_quota_reset_at":
		default:
			t.Fatalf("reset response leaks internal field %q (payload %v)", key, accountFields)
		}
	}

	// Third reset (after the wire-minimalism reset above) increments to 3.
	reset = keeperResetResponse{}
	requestJSON(t, handler, http.MethodPost, "/api/codex-keeper/reset-quota", map[string]any{"auth_name": authName}, cookies, &reset)
	if reset.Account.QuotaResetCount != 3 {
		t.Fatalf("third reset count = %d, want 3", reset.Account.QuotaResetCount)
	}

	// The accounts listing carries the counter (3 successful resets so far).
	accounts := keeperResetAccountsResponse{}
	requestJSON(t, handler, http.MethodGet, "/api/codex-keeper/accounts", nil, cookies, &accounts)
	if len(accounts.Items) != 1 || accounts.Items[0].QuotaResetCount != 3 || accounts.Items[0].LastQuotaResetAt == nil {
		t.Fatalf("accounts listing = %+v, want quota_reset_count 3 with timestamp", accounts.Items)
	}

	// Any CLIProxyAPI outcome short of a confirmed reset (HTTP failure, or a
	// deceptive 2xx whose body lacks status=ok for our exact auth_index) must
	// surface an error and must NOT increment the counter.
	baseline := accountsResetCount(t, handler, cookies)
	for _, mode := range []string{"http-fail", "empty-body", "wrong-index", "bad-status", "padded-index"} {
		mu.Lock()
		resetMode = mode
		mu.Unlock()
		requestJSONExpectStatus(t, handler, http.MethodPost, "/api/codex-keeper/reset-quota", map[string]any{"auth_name": authName}, cookies, http.StatusUnprocessableEntity)
		if got := accountsResetCount(t, handler, cookies); got != baseline {
			t.Fatalf("count after %s reset = %d, want unchanged %d", mode, got, baseline)
		}
	}
	mu.Lock()
	resetMode = "ok"
	mu.Unlock()

	// Bad requests are rejected before any CLIProxyAPI call.
	requestJSONExpectStatus(t, handler, http.MethodPost, "/api/codex-keeper/reset-quota", map[string]any{"auth_name": ""}, cookies, http.StatusUnprocessableEntity)
	requestJSONExpectStatus(t, handler, http.MethodPost, "/api/codex-keeper/reset-quota", map[string]any{"auth_name": "nope.json"}, cookies, http.StatusNotFound)
}

// keeperResetInspectAccountsResponse reads the reset_credit fields from the
// accounts endpoint so a test can assert the post-reset inspection refreshed them.
type keeperResetInspectAccountsResponse struct {
	Items []struct {
		Name             string `json:"name"`
		QuotaResetCount  int    `json:"quota_reset_count"`
		ResetCreditCount *int   `json:"reset_credit_count"`
	} `json:"items"`
}

// TestKeeperResetQuotaTriggersInspection proves that a successful reset-quota
// synchronously re-inspects just that account: the reset-credit snapshot in the DB
// is refreshed from a live fetch (not left at the pre-reset value), so the
// frontend's follow-up accounts reload shows the post-reset state.
func TestKeeperResetQuotaTriggersInspection(t *testing.T) {
	t.Setenv("CPA_HELPER_DATA_DIR", t.TempDir())

	const authName = "inspect-me.json"
	authDetail := map[string]any{
		"name": authName, "type": "codex", "auth_index": "idx-9",
		"email": "inspect@example.com", "account_type": "pro", "disabled": false,
		"priority": 1, "access_token": "test-token",
	}

	credit := func(id, expires string) map[string]any {
		return map[string]any{
			"id": id, "reset_type": "codex_rate_limits", "status": "available",
			"granted_at": "2026-08-22T00:08:46.146320Z", "expires_at": expires,
		}
	}
	var mu sync.Mutex
	// Before any reset: 2 available credits. After a reset consumes one: 1.
	availableCount := 2
	resetCreditFetches := 0

	cpa := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v0/management/auth-files":
			_ = json.NewEncoder(w).Encode(map[string]any{"files": []map[string]any{{"name": authName, "type": "codex"}}})
		case r.Method == http.MethodGet && r.URL.Path == "/v0/management/auth-files/download":
			_ = json.NewEncoder(w).Encode(authDetail)
		case r.Method == http.MethodPost && r.URL.Path == "/v0/management/api-call":
			var payload struct {
				URL string `json:"url"`
			}
			_ = json.NewDecoder(r.Body).Decode(&payload)
			if strings.Contains(payload.URL, "rate-limit-reset-credits") {
				mu.Lock()
				resetCreditFetches++
				n := availableCount
				mu.Unlock()
				credits := []map[string]any{credit("RateLimitResetCredit_A", "2026-09-21T00:08:46.146320Z")}
				if n >= 2 {
					credits = append(credits, credit("RateLimitResetCredit_B", "2026-10-04T02:24:33.736521Z"))
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"status_code": 200, "body": map[string]any{"available_count": n, "credits": credits}})
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"status_code": 200, "body": map[string]any{
				"rate_limit": map[string]any{"primary_window": map[string]any{"used_percent": 10, "reset_after_seconds": 3600}},
			}})
		case r.Method == http.MethodPost && r.URL.Path == "/v0/management/reset-quota":
			var payload struct {
				AuthIndex string `json:"auth_index"`
			}
			_ = json.NewDecoder(r.Body).Decode(&payload)
			// A reset consumes one credit, so a fresh fetch afterward returns 1.
			mu.Lock()
			availableCount = 1
			mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]any{"status": "ok", "auth_index": payload.AuthIndex, "models": []string{}})
		case r.Method == http.MethodPatch && r.URL.Path == "/v0/management/auth-files/fields":
			_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
		default:
			http.NotFound(w, r)
		}
	}))
	defer cpa.Close()

	app, err := backendApp.New()
	if err != nil {
		t.Fatalf("New() failed: %v", err)
	}
	defer app.Close()
	handler := app.Routes()

	cookies := requestJSON(t, handler, http.MethodPost, "/api/auth/setup", map[string]any{
		"username": "admin", "password": "test-password", "nickname": "Admin",
	}, nil, nil)
	requestJSON(t, handler, http.MethodPut, "/api/settings", map[string]any{
		"cliaproxy_url": cpa.URL, "management_key": "test-management-key", "collector_enabled": false,
	}, cookies, nil)
	requestJSON(t, handler, http.MethodPut, "/api/codex-keeper/settings", map[string]any{
		"schedule_cron": "0 0 29 2 *", "dry_run": false, "quota_threshold": 100,
		"worker_threads": 1, "cpa_timeout_seconds": 1,
	}, cookies, nil)
	requestJSON(t, handler, http.MethodPost, "/api/codex-keeper/run-once", nil, cookies, nil)
	waitForKeeperAccounts(t, handler, cookies, 1)

	// After the initial inspection the snapshot shows 2 available credits.
	before := keeperResetInspectAccountsResponse{}
	requestJSON(t, handler, http.MethodGet, "/api/codex-keeper/accounts", nil, cookies, &before)
	if len(before.Items) != 1 || before.Items[0].ResetCreditCount == nil || *before.Items[0].ResetCreditCount != 2 {
		t.Fatalf("pre-reset reset_credit_count = %+v, want 2", before.Items)
	}
	mu.Lock()
	fetchesBeforeReset := resetCreditFetches
	mu.Unlock()

	// Reset succeeds; the handler must synchronously re-inspect this account.
	reset := keeperResetResponse{}
	requestJSON(t, handler, http.MethodPost, "/api/codex-keeper/reset-quota", map[string]any{"auth_name": authName}, cookies, &reset)
	if reset.Status != "ok" {
		t.Fatalf("reset status = %q, want ok", reset.Status)
	}

	mu.Lock()
	fetchesAfterReset := resetCreditFetches
	mu.Unlock()
	if fetchesAfterReset <= fetchesBeforeReset {
		t.Fatalf("reset-credit fetches did not increase after reset (%d -> %d): no post-reset inspection ran", fetchesBeforeReset, fetchesAfterReset)
	}

	// The accounts readback now reflects the post-reset live fetch (1 credit left).
	after := keeperResetInspectAccountsResponse{}
	requestJSON(t, handler, http.MethodGet, "/api/codex-keeper/accounts", nil, cookies, &after)
	if len(after.Items) != 1 || after.Items[0].ResetCreditCount == nil || *after.Items[0].ResetCreditCount != 1 {
		t.Fatalf("post-reset reset_credit_count = %+v, want 1 (refreshed by the chained inspection)", after.Items)
	}
}

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

// keeperResetResponse is the minimal wire shape the reset route returns: only
// the account name plus whether a real reset credit was consumed this operation.
type keeperResetResponse struct {
	Status  string `json:"status"`
	Account struct {
		Name     string `json:"name"`
		Consumed bool   `json:"consumed"`
	} `json:"account"`
}

// keeperResetControl is the shared, mutex-guarded mock state that lets each
// sub-case steer the fake CLIProxyAPI: the authoritative available credit count,
// how the consume endpoint replies, and how /reset-quota replies. It also records
// call counts so a test can assert fail-closed ordering (e.g. a failed consume
// must never reach /reset-quota).
type keeperResetControl struct {
	mu               sync.Mutex
	availableCount   int    // authoritative available_count returned by the fresh fetch
	fetchMode        string // ok | fail (fresh reset-credit GET)
	consumeMode      string // ok | http-fail | unknown-code | no-credit
	resetMode        string // ok | http-fail | empty-body | wrong-index | bad-status | padded-index
	consumeCalls     int
	resetCreditFetch int
	resetQuotaCalls  []string
}

func newKeeperResetCPA(t *testing.T, authName string, authDetail map[string]any, ctrl *keeperResetControl) *httptest.Server {
	t.Helper()
	credit := func(id, expires string) map[string]any {
		return map[string]any{
			"id": id, "reset_type": "codex_rate_limits", "status": "available",
			"granted_at": "2026-08-22T00:08:46.146320Z", "expires_at": expires,
		}
	}
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v0/management/auth-files":
			_ = json.NewEncoder(w).Encode(map[string]any{"files": []map[string]any{{"name": authName, "type": "codex"}}})
		case r.Method == http.MethodGet && r.URL.Path == "/v0/management/auth-files/download":
			_ = json.NewEncoder(w).Encode(authDetail)
		case r.Method == http.MethodPost && r.URL.Path == "/v0/management/api-call":
			var p struct {
				URL string `json:"url"`
			}
			_ = json.NewDecoder(r.Body).Decode(&p)
			switch {
			// The consume URL also contains "rate-limit-reset-credits", so match the
			// more specific "/consume" suffix first.
			case strings.Contains(p.URL, "rate-limit-reset-credits/consume"):
				ctrl.mu.Lock()
				ctrl.consumeCalls++
				mode := ctrl.consumeMode
				// A real consume decrements the authoritative count.
				if mode == "ok" && ctrl.availableCount > 0 {
					ctrl.availableCount--
				}
				ctrl.mu.Unlock()
				switch mode {
				case "http-fail":
					_ = json.NewEncoder(w).Encode(map[string]any{"status_code": 403, "body": map[string]any{"error": map[string]any{"message": "forbidden"}}})
				case "unknown-code":
					_ = json.NewEncoder(w).Encode(map[string]any{"status_code": 200, "body": map[string]any{"code": "surprise"}})
				case "no-credit":
					_ = json.NewEncoder(w).Encode(map[string]any{"status_code": 200, "body": map[string]any{"code": "no_credit"}})
				default:
					_ = json.NewEncoder(w).Encode(map[string]any{"status_code": 200, "body": map[string]any{"code": "reset", "windows_reset": []any{}}})
				}
			case strings.Contains(p.URL, "rate-limit-reset-credits"):
				ctrl.mu.Lock()
				ctrl.resetCreditFetch++
				n := ctrl.availableCount
				mode := ctrl.fetchMode
				ctrl.mu.Unlock()
				if mode == "fail" {
					http.Error(w, "boom", http.StatusInternalServerError)
					return
				}
				credits := []map[string]any{}
				if n >= 1 {
					credits = append(credits, credit("RateLimitResetCredit_A", "2026-09-21T00:08:46.146320Z"))
				}
				if n >= 2 {
					credits = append(credits, credit("RateLimitResetCredit_B", "2026-10-04T02:24:33.736521Z"))
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"status_code": 200, "body": map[string]any{"available_count": n, "credits": credits}})
			default:
				_ = json.NewEncoder(w).Encode(map[string]any{"status_code": 200, "body": map[string]any{
					"rate_limit": map[string]any{"primary_window": map[string]any{"used_percent": 10, "reset_after_seconds": 3600}},
				}})
			}
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
			ctrl.mu.Lock()
			mode := ctrl.resetMode
			ctrl.resetQuotaCalls = append(ctrl.resetQuotaCalls, payload.AuthIndex)
			ctrl.mu.Unlock()
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
}

func setupKeeperResetApp(t *testing.T, cpaURL string) (http.Handler, []*http.Cookie, func()) {
	t.Helper()
	app, err := backendApp.New()
	if err != nil {
		t.Fatalf("New() failed: %v", err)
	}
	handler := app.Routes()
	cookies := requestJSON(t, handler, http.MethodPost, "/api/auth/setup", map[string]any{
		"username": "admin", "password": "test-password", "nickname": "Admin",
	}, nil, nil)
	requestJSON(t, handler, http.MethodPut, "/api/settings", map[string]any{
		"cliaproxy_url": cpaURL, "management_key": "test-management-key", "collector_enabled": false,
	}, cookies, nil)
	requestJSON(t, handler, http.MethodPut, "/api/codex-keeper/settings", map[string]any{
		"schedule_cron": "0 0 29 2 *", "dry_run": false, "quota_threshold": 100,
		"worker_threads": 1, "cpa_timeout_seconds": 1,
	}, cookies, nil)
	requestJSON(t, handler, http.MethodPost, "/api/codex-keeper/run-once", nil, cookies, nil)
	waitForKeeperAccounts(t, handler, cookies, 1)
	return handler, cookies, func() { app.Close() }
}

// TestKeeperReset drives the real reset route through its new contract: when a
// credit is available it redeems one (consume) and reports consumed=true; when
// none is available it clears only the local cooldown (consumed=false); a failed
// consume fails closed WITHOUT clearing the cooldown; an unconfirmed CLIProxyAPI
// reset surfaces an error; and the response wire shape stays minimal.
func TestKeeperReset(t *testing.T) {
	t.Setenv("CPA_HELPER_DATA_DIR", t.TempDir())

	authName := "reset-me.json"
	authDetail := map[string]any{
		"name": authName, "type": "codex", "auth_index": "idx-7",
		"email": "reset@example.com", "account_type": "plus", "disabled": false,
		"priority": 1, "access_token": "test-token", "account_id": "acct-123",
	}
	ctrl := &keeperResetControl{availableCount: 2, fetchMode: "ok", consumeMode: "ok", resetMode: "ok"}
	cpa := newKeeperResetCPA(t, authName, authDetail, ctrl)
	defer cpa.Close()
	handler, cookies, cleanup := setupKeeperResetApp(t, cpa.URL)
	defer cleanup()

	// Happy path: a credit is available, so one is redeemed (consumed=true) and the
	// cooldown is cleared exactly once.
	reset := keeperResetResponse{}
	requestJSON(t, handler, http.MethodPost, "/api/codex-keeper/reset-quota", map[string]any{"auth_name": authName}, cookies, &reset)
	if reset.Status != "ok" || reset.Account.Name != authName || !reset.Account.Consumed {
		t.Fatalf("reset response = %+v, want ok/consumed for %s", reset, authName)
	}
	ctrl.mu.Lock()
	if ctrl.consumeCalls != 1 {
		ctrl.mu.Unlock()
		t.Fatalf("consume calls = %d, want exactly 1", ctrl.consumeCalls)
	}
	if len(ctrl.resetQuotaCalls) != 1 || ctrl.resetQuotaCalls[0] != "idx-7" {
		ctrl.mu.Unlock()
		t.Fatalf("reset-quota calls = %v, want one for idx-7", ctrl.resetQuotaCalls)
	}
	ctrl.mu.Unlock()

	// Wire minimalism: the reset response must not leak internal account fields.
	raw := map[string]json.RawMessage{}
	requestJSON(t, handler, http.MethodPost, "/api/codex-keeper/reset-quota", map[string]any{"auth_name": authName}, cookies, &raw)
	var accountFields map[string]any
	if err := json.Unmarshal(raw["account"], &accountFields); err != nil {
		t.Fatalf("decode reset account payload: %v", err)
	}
	for key := range accountFields {
		switch key {
		case "name", "consumed":
		default:
			t.Fatalf("reset response leaks internal field %q (payload %v)", key, accountFields)
		}
	}

	// No-credit path: the authoritative count is 0, so consume is skipped entirely
	// and only the local cooldown is cleared (consumed=false).
	ctrl.mu.Lock()
	ctrl.availableCount = 0
	ctrl.consumeCalls = 0
	ctrl.resetQuotaCalls = nil
	ctrl.mu.Unlock()
	reset = keeperResetResponse{}
	requestJSON(t, handler, http.MethodPost, "/api/codex-keeper/reset-quota", map[string]any{"auth_name": authName}, cookies, &reset)
	if reset.Status != "ok" || reset.Account.Consumed {
		t.Fatalf("no-credit reset = %+v, want ok with consumed=false", reset)
	}
	ctrl.mu.Lock()
	if ctrl.consumeCalls != 0 {
		ctrl.mu.Unlock()
		t.Fatalf("no-credit path made %d consume calls, want 0", ctrl.consumeCalls)
	}
	if len(ctrl.resetQuotaCalls) != 1 {
		ctrl.mu.Unlock()
		t.Fatalf("no-credit path reset-quota calls = %v, want exactly one", ctrl.resetQuotaCalls)
	}
	ctrl.mu.Unlock()

	// Fail-closed: when a credit is available but the consume fails (inner non-2xx
	// or an unrecognized code, or the fresh count is unknown), the whole operation
	// errors and NEVER reaches /reset-quota — the cooldown is not cleared on a
	// half-done redemption.
	for _, mode := range []string{"http-fail", "unknown-code"} {
		ctrl.mu.Lock()
		ctrl.availableCount = 2
		ctrl.consumeMode = mode
		ctrl.resetQuotaCalls = nil
		ctrl.mu.Unlock()
		requestJSONExpectStatus(t, handler, http.MethodPost, "/api/codex-keeper/reset-quota", map[string]any{"auth_name": authName}, cookies, http.StatusUnprocessableEntity)
		ctrl.mu.Lock()
		calls := ctrl.resetQuotaCalls
		ctrl.mu.Unlock()
		if len(calls) != 0 {
			t.Fatalf("consume mode %s reached /reset-quota (%v); it must fail closed first", mode, calls)
		}
	}
	ctrl.mu.Lock()
	ctrl.consumeMode = "ok"
	ctrl.mu.Unlock()

	// Unknown available count (fresh fetch failed) blocks rather than degrading to
	// a cooldown-only clear.
	ctrl.mu.Lock()
	ctrl.fetchMode = "fail"
	ctrl.resetQuotaCalls = nil
	ctrl.mu.Unlock()
	requestJSONExpectStatus(t, handler, http.MethodPost, "/api/codex-keeper/reset-quota", map[string]any{"auth_name": authName}, cookies, http.StatusUnprocessableEntity)
	ctrl.mu.Lock()
	if len(ctrl.resetQuotaCalls) != 0 {
		ctrl.mu.Unlock()
		t.Fatalf("unknown-count path reached /reset-quota; it must block")
	}
	ctrl.fetchMode = "ok"
	ctrl.mu.Unlock()

	// Any CLIProxyAPI outcome short of a confirmed reset must surface an error.
	for _, mode := range []string{"http-fail", "empty-body", "wrong-index", "bad-status", "padded-index"} {
		ctrl.mu.Lock()
		ctrl.availableCount = 2
		ctrl.resetMode = mode
		ctrl.mu.Unlock()
		requestJSONExpectStatus(t, handler, http.MethodPost, "/api/codex-keeper/reset-quota", map[string]any{"auth_name": authName}, cookies, http.StatusUnprocessableEntity)
	}
	ctrl.mu.Lock()
	ctrl.resetMode = "ok"
	ctrl.mu.Unlock()

	// Bad requests are rejected before any CLIProxyAPI reset call.
	requestJSONExpectStatus(t, handler, http.MethodPost, "/api/codex-keeper/reset-quota", map[string]any{"auth_name": ""}, cookies, http.StatusUnprocessableEntity)
	requestJSONExpectStatus(t, handler, http.MethodPost, "/api/codex-keeper/reset-quota", map[string]any{"auth_name": "nope.json"}, cookies, http.StatusNotFound)
}

// keeperResetInspectAccountsResponse reads the reset_credit count from the
// accounts endpoint so a test can assert the post-reset inspection refreshed it.
type keeperResetInspectAccountsResponse struct {
	Items []struct {
		Name             string `json:"name"`
		ResetCreditCount *int   `json:"reset_credit_count"`
	} `json:"items"`
}

// TestKeeperResetQuotaTriggersInspection proves a successful reset synchronously
// re-inspects just that account: after redeeming one of two credits the DB
// snapshot is refreshed from a live fetch (2 -> 1), so the frontend's follow-up
// accounts reload shows the post-reset state rather than the stale pre-reset one.
func TestKeeperResetQuotaTriggersInspection(t *testing.T) {
	t.Setenv("CPA_HELPER_DATA_DIR", t.TempDir())

	const authName = "inspect-me.json"
	authDetail := map[string]any{
		"name": authName, "type": "codex", "auth_index": "idx-9",
		"email": "inspect@example.com", "account_type": "pro", "disabled": false,
		"priority": 1, "access_token": "test-token", "account_id": "acct-9",
	}
	ctrl := &keeperResetControl{availableCount: 2, fetchMode: "ok", consumeMode: "ok", resetMode: "ok"}
	cpa := newKeeperResetCPA(t, authName, authDetail, ctrl)
	defer cpa.Close()
	handler, cookies, cleanup := setupKeeperResetApp(t, cpa.URL)
	defer cleanup()

	// After the initial inspection the snapshot shows 2 available credits.
	before := keeperResetInspectAccountsResponse{}
	requestJSON(t, handler, http.MethodGet, "/api/codex-keeper/accounts", nil, cookies, &before)
	if len(before.Items) != 1 || before.Items[0].ResetCreditCount == nil || *before.Items[0].ResetCreditCount != 2 {
		t.Fatalf("pre-reset reset_credit_count = %+v, want 2", before.Items)
	}
	ctrl.mu.Lock()
	fetchesBeforeReset := ctrl.resetCreditFetch
	ctrl.mu.Unlock()

	// Reset succeeds; it redeems one credit and then re-inspects this account.
	reset := keeperResetResponse{}
	requestJSON(t, handler, http.MethodPost, "/api/codex-keeper/reset-quota", map[string]any{"auth_name": authName}, cookies, &reset)
	if reset.Status != "ok" || !reset.Account.Consumed {
		t.Fatalf("reset = %+v, want ok/consumed", reset)
	}

	ctrl.mu.Lock()
	fetchesAfterReset := ctrl.resetCreditFetch
	ctrl.mu.Unlock()
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

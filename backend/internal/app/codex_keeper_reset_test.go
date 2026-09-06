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
		Name    string `json:"name"`
		Outcome string `json:"outcome"`
	} `json:"account"`
}

// keeperResetControl is the shared, mutex-guarded mock state that lets each
// sub-case steer the fake CLIProxyAPI: the authoritative available credit count,
// how the consume endpoint replies, and how /reset-quota replies. It also records
// call counts so a test can assert fail-closed ordering (e.g. a failed consume
// must never reach /reset-quota).
// keeperConsumeSensitiveSentinel is a fake sensitive token embedded in the mock's
// inner consume error body; the redaction canary asserts it NEVER reaches the log.
const keeperConsumeSensitiveSentinel = "SENSITIVE-sk-consume-secret-zzz"

type keeperResetControl struct {
	mu                 sync.Mutex
	availableCount     int    // authoritative available_count returned by the fresh fetch
	fetchMode          string // ok | fail (fresh reset-credit GET)
	consumeMode        string // ok | http-fail | unknown-code | no-credit | lost
	consumeSuccessCode string // terminal code returned on a successful consume (default "reset")
	resetMode          string // ok | http-fail | empty-body | wrong-index | bad-status | padded-index
	consumeCalls       int
	consumeRequestIDs  []string // redeem_request_id seen on each consume call, in order
	resetCreditFetch   int
	resetQuotaCalls    []string
	// Concurrency gate: when armed, the first fresh reset-credit fetch to run under
	// the per-auth lock signals gateReached (once) then blocks on gateRelease, so a
	// test can deterministically hold the winner in-lock while contending requests hit
	// tryLock and 409.
	gateReached chan struct{}
	gateRelease chan struct{}
	gateOnce    *sync.Once
	// authIndexOverride, when non-empty, replaces the downloaded detail's auth_index
	// so a test can simulate a reassignment (fresh auth_index != stored DB row).
	authIndexOverride string
	// accountIDOverride, when non-empty, replaces the downloaded detail's account_id
	// so a test can simulate an account_id change on the same auth_index.
	accountIDOverride string
	// omitAccountID drops account_id from the downloaded detail entirely.
	omitAccountID bool
	// listAccountIDClaim, when non-empty, adds id_token.chatgpt_account_id to the
	// list entry so a test can exercise the list-vs-download account_id cross-check.
	listAccountIDClaim string
	// listTypeOverride, when non-empty, replaces the list entry's "type" (default
	// "codex") so a test can simulate a non-Codex / drifted provider.
	listTypeOverride string
	// omitAccessToken drops access_token from the downloaded detail.
	omitAccessToken bool
	// duplicateListName emits a second list entry with the same name (different
	// auth_index) to simulate a malformed/corrupt remote list.
	duplicateListName bool
	// detailAliasConflict adds a conflicting authIndex alias to the download detail
	// (auth_index != authIndex) to simulate a deceptive intra-object identity.
	detailAliasConflict bool
	// detailNameOverride, when non-empty, replaces the download detail's "name" to
	// simulate a proxy misroute binding another credential's detail to this target.
	detailNameOverride string
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
			entry := map[string]any{"name": authName, "type": "codex"}
			ctrl.mu.Lock()
			claim := ctrl.listAccountIDClaim
			typeOvr := ctrl.listTypeOverride
			dup := ctrl.duplicateListName
			ctrl.mu.Unlock()
			if claim != "" {
				entry["id_token"] = map[string]any{"chatgpt_account_id": claim}
			}
			if typeOvr != "" {
				entry["type"] = typeOvr
			}
			files := []map[string]any{entry}
			if dup {
				files = append(files, map[string]any{"name": authName, "type": "codex", "auth_index": "idx-duplicate"})
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"files": files})
		case r.Method == http.MethodGet && r.URL.Path == "/v0/management/auth-files/download":
			detail := map[string]any{}
			for k, v := range authDetail {
				detail[k] = v
			}
			ctrl.mu.Lock()
			ovr := ctrl.authIndexOverride
			acctOvr := ctrl.accountIDOverride
			omitAcct := ctrl.omitAccountID
			omitToken := ctrl.omitAccessToken
			aliasConflict := ctrl.detailAliasConflict
			nameOvr := ctrl.detailNameOverride
			ctrl.mu.Unlock()
			if nameOvr != "" {
				detail["name"] = nameOvr
			}
			if ovr != "" {
				detail["auth_index"] = ovr
			}
			if acctOvr != "" {
				detail["account_id"] = acctOvr
			}
			if omitAcct {
				delete(detail, "account_id")
			}
			if omitToken {
				delete(detail, "access_token")
			}
			if aliasConflict {
				detail["authIndex"] = "idx-conflict"
			}
			_ = json.NewEncoder(w).Encode(detail)
		case r.Method == http.MethodPost && r.URL.Path == "/v0/management/api-call":
			var p struct {
				URL  string `json:"url"`
				Data string `json:"data"`
			}
			_ = json.NewDecoder(r.Body).Decode(&p)
			switch {
			// The consume URL also contains "rate-limit-reset-credits", so match the
			// more specific "/consume" suffix first.
			case strings.Contains(p.URL, "rate-limit-reset-credits/consume"):
				var d struct {
					RedeemRequestID string `json:"redeem_request_id"`
				}
				_ = json.Unmarshal([]byte(p.Data), &d)
				ctrl.mu.Lock()
				ctrl.consumeCalls++
				ctrl.consumeRequestIDs = append(ctrl.consumeRequestIDs, d.RedeemRequestID)
				mode := ctrl.consumeMode
				successCode := ctrl.consumeSuccessCode
				// A real reset consumes one credit.
				if mode == "ok" && (successCode == "" || successCode == "reset") && ctrl.availableCount > 0 {
					ctrl.availableCount--
				}
				ctrl.mu.Unlock()
				if mode == "lost" {
					// Every attempt of this operation loses its response (outer 5xx):
					// even keeperRequest's idempotent retries fail, so the whole operation
					// is unknown and the ledger stays pending for the next operation.
					http.Error(w, "gateway", http.StatusBadGateway)
					return
				}
				switch mode {
				case "http-fail":
					// Inner non-2xx carrying a sensitive sentinel: the canary asserts it
					// never reaches the log.
					_ = json.NewEncoder(w).Encode(map[string]any{"status_code": 403, "body": map[string]any{"error": map[string]any{"message": keeperConsumeSensitiveSentinel}}})
				case "unknown-code":
					_ = json.NewEncoder(w).Encode(map[string]any{"status_code": 200, "body": map[string]any{"code": "surprise", "detail": keeperConsumeSensitiveSentinel}})
				case "no-credit":
					_ = json.NewEncoder(w).Encode(map[string]any{"status_code": 200, "body": map[string]any{"code": "no_credit"}})
				default:
					code := successCode
					if code == "" {
						code = "reset"
					}
					_ = json.NewEncoder(w).Encode(map[string]any{"status_code": 200, "body": map[string]any{"code": code, "windows_reset": []any{}}})
				}
			case strings.Contains(p.URL, "rate-limit-reset-credits"):
				ctrl.mu.Lock()
				ctrl.resetCreditFetch++
				n := ctrl.availableCount
				mode := ctrl.fetchMode
				gReached, gRelease, gOnce := ctrl.gateReached, ctrl.gateRelease, ctrl.gateOnce
				ctrl.mu.Unlock()
				if gRelease != nil {
					gOnce.Do(func() { close(gReached) })
					<-gRelease
				}
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
	if reset.Status != "ok" || reset.Account.Name != authName || reset.Account.Outcome != "reset" {
		t.Fatalf("reset response = %+v, want outcome=reset for %s", reset, authName)
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
		case "name", "outcome":
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
	if reset.Status != "ok" || reset.Account.Outcome != "cooldown_only" {
		t.Fatalf("no-credit reset = %+v, want ok with outcome=cooldown_only", reset)
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

	// Unknown available count (fresh fetch failed) blocks rather than degrading to a
	// cooldown-only clear. This runs BEFORE any consume-failure leaves a replayable
	// pending redeem (which would legitimately bypass the fresh-count gate).
	ctrl.mu.Lock()
	ctrl.availableCount = 2
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

	// Fail-closed: when a credit is available but the consume fails (inner non-2xx
	// or an unrecognized code), the whole operation errors and NEVER reaches
	// /reset-quota — the cooldown is not cleared on a half-done redemption. This leaves
	// a pending redeem (unknown outcome), which is resolved right after.
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
	// Resolve the pending left by the fail-closed loop with a clean successful reset so
	// the following assertions start without a replayable pending.
	ctrl.mu.Lock()
	ctrl.consumeMode = "ok"
	ctrl.availableCount = 2
	ctrl.mu.Unlock()
	requestJSON(t, handler, http.MethodPost, "/api/codex-keeper/reset-quota", map[string]any{"auth_name": authName}, cookies, &keeperResetResponse{})

	// A credit is available, so consume succeeds; any CLIProxyAPI outcome short of a
	// confirmed cooldown clear is then an irreversible PARTIAL (409), not a plain error.
	for _, mode := range []string{"http-fail", "empty-body", "wrong-index", "bad-status", "padded-index"} {
		ctrl.mu.Lock()
		ctrl.availableCount = 2
		ctrl.resetMode = mode
		ctrl.mu.Unlock()
		requestJSONExpectStatus(t, handler, http.MethodPost, "/api/codex-keeper/reset-quota", map[string]any{"auth_name": authName}, cookies, http.StatusConflict)
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
	if reset.Status != "ok" || reset.Account.Outcome != "reset" {
		t.Fatalf("reset = %+v, want outcome=reset", reset)
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

// postKeeperReset drives the reset route and returns only the HTTP status, so it
// is safe to call from goroutines (no t.Fatal off the test goroutine).
func postKeeperReset(handler http.Handler, cookies []*http.Cookie, authName string) int {
	req := httptest.NewRequest(http.MethodPost, "/api/codex-keeper/reset-quota", strings.NewReader(`{"auth_name":"`+authName+`"}`))
	req.Header.Set("Content-Type", "application/json")
	for _, c := range cookies {
		req.AddCookie(c)
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec.Code
}

// TestKeeperResetConcurrentSingleConsume proves the per-auth lock makes concurrent
// resets of the SAME account redeem at most one credit: while one request holds the
// lock (blocked in its fresh reset-credit fetch), every contending request returns
// 409 without reaching consume.
func TestKeeperResetConcurrentSingleConsume(t *testing.T) {
	t.Setenv("CPA_HELPER_DATA_DIR", t.TempDir())
	authName := "race-me.json"
	authDetail := map[string]any{
		"name": authName, "type": "codex", "auth_index": "idx-race",
		"email": "race@example.com", "account_type": "pro", "disabled": false,
		"priority": 1, "access_token": "test-token", "account_id": "acct-race",
	}
	ctrl := &keeperResetControl{availableCount: 3, fetchMode: "ok", consumeMode: "ok", resetMode: "ok"}
	cpa := newKeeperResetCPA(t, authName, authDetail, ctrl)
	defer cpa.Close()
	handler, cookies, cleanup := setupKeeperResetApp(t, cpa.URL)
	defer cleanup()

	// Arm the gate only AFTER setup, so the run-once inspection's fetch is not held.
	ctrl.mu.Lock()
	ctrl.gateReached = make(chan struct{})
	ctrl.gateRelease = make(chan struct{})
	ctrl.gateOnce = &sync.Once{}
	ctrl.mu.Unlock()

	// Winner: acquires the per-auth lock and blocks inside its fresh fetch.
	winnerCh := make(chan int, 1)
	go func() { winnerCh <- postKeeperReset(handler, cookies, authName) }()
	<-ctrl.gateReached // the winner now holds the per-auth lock

	// Contenders: fire concurrently while the winner holds the lock; all must 409.
	const contenders = 4
	var wg sync.WaitGroup
	statuses := make([]int, contenders)
	for i := 0; i < contenders; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			statuses[idx] = postKeeperReset(handler, cookies, authName)
		}(i)
	}
	wg.Wait()
	for i, s := range statuses {
		if s != http.StatusConflict {
			t.Fatalf("contender %d status = %d, want 409 (per-auth lock held)", i, s)
		}
	}

	close(ctrl.gateRelease) // let the winner finish
	if s := <-winnerCh; s != http.StatusOK {
		t.Fatalf("winner status = %d, want 200", s)
	}
	ctrl.mu.Lock()
	defer ctrl.mu.Unlock()
	if ctrl.consumeCalls != 1 {
		t.Fatalf("consume calls under concurrency = %d, want exactly 1", ctrl.consumeCalls)
	}
}

// TestKeeperResetLostResponseReusesRedeemID proves the persistent redeem ledger
// closes the cross-operation double-consume window: when a consume's response is
// lost (outer 5xx = unknown), the redeem stays pending and the NEXT reset reuses the
// SAME redeem_request_id (OpenAI then returns already_redeemed idempotently) instead
// of minting a new key that could burn a second credit.
func TestKeeperResetLostResponseReusesRedeemID(t *testing.T) {
	t.Setenv("CPA_HELPER_DATA_DIR", t.TempDir())
	authName := "lost-me.json"
	authDetail := map[string]any{
		"name": authName, "type": "codex", "auth_index": "idx-lost",
		"email": "lost@example.com", "account_type": "pro", "disabled": false,
		"priority": 1, "access_token": "test-token", "account_id": "acct-lost",
	}
	ctrl := &keeperResetControl{
		availableCount: 2, fetchMode: "ok", consumeMode: "lost", resetMode: "ok",
	}
	cpa := newKeeperResetCPA(t, authName, authDetail, ctrl)
	defer cpa.Close()
	handler, cookies, cleanup := setupKeeperResetApp(t, cpa.URL)
	defer cleanup()

	// First attempt: every consume attempt loses its response (unknown) → fail closed,
	// ledger left pending.
	requestJSONExpectStatus(t, handler, http.MethodPost, "/api/codex-keeper/reset-quota", map[string]any{"auth_name": authName}, cookies, http.StatusUnprocessableEntity)

	// The account recovers; the next attempt must reuse the pending redeem_request_id
	// so OpenAI resolves it idempotently as already_redeemed instead of burning a
	// second credit.
	ctrl.mu.Lock()
	ctrl.consumeMode = "ok"
	ctrl.consumeSuccessCode = "already_redeemed"
	ctrl.mu.Unlock()

	reset := keeperResetResponse{}
	requestJSON(t, handler, http.MethodPost, "/api/codex-keeper/reset-quota", map[string]any{"auth_name": authName}, cookies, &reset)
	if reset.Status != "ok" || reset.Account.Outcome != "already_redeemed" {
		t.Fatalf("retry reset = %+v, want outcome=already_redeemed", reset)
	}

	ctrl.mu.Lock()
	ids := append([]string{}, ctrl.consumeRequestIDs...)
	ctrl.mu.Unlock()
	if len(ids) < 2 {
		t.Fatalf("consume calls = %d, want >= 2 (lost + retry)", len(ids))
	}
	// Every consume attempt (the lost operation's retries AND the recovery operation)
	// must carry the exact same redeem_request_id.
	for i, id := range ids {
		if id == "" || id != ids[0] {
			t.Fatalf("redeem_request_id not reused (call %d = %q, first = %q); all=%v", i, id, ids[0], ids)
		}
	}
}

// TestKeeperResetConsumeLogRedaction is the canary: a failed consume whose inner
// body carries a sensitive sentinel must never write that raw body to the Keeper
// log; only a stable classification (reset-consume … inner_status) is recorded.
func TestKeeperResetConsumeLogRedaction(t *testing.T) {
	t.Setenv("CPA_HELPER_DATA_DIR", t.TempDir())
	authName := "redact-me.json"
	authDetail := map[string]any{
		"name": authName, "type": "codex", "auth_index": "idx-redact",
		"email": "redact@example.com", "account_type": "pro", "disabled": false,
		"priority": 1, "access_token": "test-token", "account_id": "acct-redact",
	}
	ctrl := &keeperResetControl{availableCount: 2, fetchMode: "ok", consumeMode: "http-fail", resetMode: "ok"}
	cpa := newKeeperResetCPA(t, authName, authDetail, ctrl)
	defer cpa.Close()
	handler, cookies, cleanup := setupKeeperResetApp(t, cpa.URL)
	defer cleanup()

	requestJSONExpectStatus(t, handler, http.MethodPost, "/api/codex-keeper/reset-quota", map[string]any{"auth_name": authName}, cookies, http.StatusUnprocessableEntity)

	var status struct {
		Logs []string `json:"logs"`
	}
	requestJSON(t, handler, http.MethodGet, "/api/codex-keeper/status", nil, cookies, &status)
	joined := strings.Join(status.Logs, "\n")
	if strings.Contains(joined, keeperConsumeSensitiveSentinel) {
		t.Fatalf("sensitive inner body leaked into Keeper log")
	}
	if !strings.Contains(joined, "reset-consume") || !strings.Contains(joined, "inner_status") {
		t.Fatalf("expected stable reset-consume/inner_status classification in log; logs=%v", status.Logs)
	}
}

// TestKeeperResetAuthIndexMismatch proves the exact same-row binding: when the fresh
// merged detail's auth_index no longer matches the stored DB row (a reassignment),
// the reset fails closed and never fetches credits, consumes, or clears the cooldown.
func TestKeeperResetAuthIndexMismatch(t *testing.T) {
	t.Setenv("CPA_HELPER_DATA_DIR", t.TempDir())
	authName := "moved-me.json"
	authDetail := map[string]any{
		"name": authName, "type": "codex", "auth_index": "idx-orig",
		"email": "moved@example.com", "account_type": "pro", "disabled": false,
		"priority": 1, "access_token": "test-token", "account_id": "acct-moved",
	}
	ctrl := &keeperResetControl{availableCount: 2, fetchMode: "ok", consumeMode: "ok", resetMode: "ok"}
	cpa := newKeeperResetCPA(t, authName, authDetail, ctrl)
	defer cpa.Close()
	handler, cookies, cleanup := setupKeeperResetApp(t, cpa.URL)
	defer cleanup()

	// The account was inspected with idx-orig; now the fresh detail reports a
	// different auth_index (reassignment).
	ctrl.mu.Lock()
	ctrl.authIndexOverride = "idx-different"
	ctrl.consumeCalls = 0
	ctrl.resetQuotaCalls = nil
	ctrl.mu.Unlock()

	requestJSONExpectStatus(t, handler, http.MethodPost, "/api/codex-keeper/reset-quota", map[string]any{"auth_name": authName}, cookies, http.StatusUnprocessableEntity)

	ctrl.mu.Lock()
	defer ctrl.mu.Unlock()
	if ctrl.consumeCalls != 0 {
		t.Fatalf("consume happened on auth_index mismatch (%d calls); must fail closed", ctrl.consumeCalls)
	}
	if len(ctrl.resetQuotaCalls) != 0 {
		t.Fatalf("cooldown cleared on auth_index mismatch; must fail closed")
	}
}

// TestKeeperResetMissingAccountID proves the reset fails closed when no account_id can
// be resolved (the consume header requires it) — no consume, no cooldown clear.
func TestKeeperResetMissingAccountID(t *testing.T) {
	t.Setenv("CPA_HELPER_DATA_DIR", t.TempDir())
	authName := "noacct-me.json"
	authDetail := map[string]any{
		"name": authName, "type": "codex", "auth_index": "idx-noacct",
		"email": "noacct@example.com", "account_type": "pro", "disabled": false,
		"priority": 1, "access_token": "test-token", "account_id": "acct-noacct",
	}
	ctrl := &keeperResetControl{availableCount: 2, fetchMode: "ok", consumeMode: "ok", resetMode: "ok"}
	cpa := newKeeperResetCPA(t, authName, authDetail, ctrl)
	defer cpa.Close()
	handler, cookies, cleanup := setupKeeperResetApp(t, cpa.URL)
	defer cleanup()

	// Now the fresh detail drops account_id (the list entry never had one).
	ctrl.mu.Lock()
	ctrl.omitAccountID = true
	ctrl.consumeCalls = 0
	ctrl.resetQuotaCalls = nil
	ctrl.mu.Unlock()

	requestJSONExpectStatus(t, handler, http.MethodPost, "/api/codex-keeper/reset-quota", map[string]any{"auth_name": authName}, cookies, http.StatusUnprocessableEntity)

	ctrl.mu.Lock()
	defer ctrl.mu.Unlock()
	if ctrl.consumeCalls != 0 || len(ctrl.resetQuotaCalls) != 0 {
		t.Fatalf("missing account_id must fail closed (consume=%d, reset-quota=%v)", ctrl.consumeCalls, ctrl.resetQuotaCalls)
	}
}

// TestKeeperResetLedgerIdentityChangeMintsFresh proves the ledger never reuses a
// pending redeem_request_id across an identity change: after a lost consume leaves a
// pending row bound to one account_id, a later reset whose account_id has changed (same
// auth_index) mints a FRESH id rather than replaying the previous account's key.
func TestKeeperResetLedgerIdentityChangeMintsFresh(t *testing.T) {
	t.Setenv("CPA_HELPER_DATA_DIR", t.TempDir())
	authName := "switch-me.json"
	authDetail := map[string]any{
		"name": authName, "type": "codex", "auth_index": "idx-switch",
		"email": "switch@example.com", "account_type": "pro", "disabled": false,
		"priority": 1, "access_token": "test-token", "account_id": "acct-A",
	}
	ctrl := &keeperResetControl{availableCount: 2, fetchMode: "ok", consumeMode: "lost", resetMode: "ok"}
	cpa := newKeeperResetCPA(t, authName, authDetail, ctrl)
	defer cpa.Close()
	handler, cookies, cleanup := setupKeeperResetApp(t, cpa.URL)
	defer cleanup()

	// First reset: consume is lost → pending row bound to account_id acct-A.
	requestJSONExpectStatus(t, handler, http.MethodPost, "/api/codex-keeper/reset-quota", map[string]any{"auth_name": authName}, cookies, http.StatusUnprocessableEntity)

	// The auth_name is now backed by a different account (same auth_index) and the
	// consume succeeds.
	ctrl.mu.Lock()
	firstIDs := append([]string{}, ctrl.consumeRequestIDs...)
	ctrl.accountIDOverride = "acct-B"
	ctrl.consumeMode = "ok"
	ctrl.mu.Unlock()

	reset := keeperResetResponse{}
	requestJSON(t, handler, http.MethodPost, "/api/codex-keeper/reset-quota", map[string]any{"auth_name": authName}, cookies, &reset)
	if reset.Status != "ok" || reset.Account.Outcome != "reset" {
		t.Fatalf("post-switch reset = %+v, want outcome=reset", reset)
	}

	ctrl.mu.Lock()
	defer ctrl.mu.Unlock()
	if len(firstIDs) == 0 {
		t.Fatal("no consume recorded for the lost first attempt")
	}
	newID := ctrl.consumeRequestIDs[len(ctrl.consumeRequestIDs)-1]
	if newID == firstIDs[0] {
		t.Fatalf("identity change reused the previous account's redeem_request_id %q", newID)
	}
}

// TestKeeperResetPendingReplayedWhenCountZero pins the control-flow rule that an
// identity-matched pending redeem is replayed with its original key even when the
// fresh available_count is 0: a lost first response that actually consumed the LAST
// credit is recovered as already_redeemed (consumed=true), never short-circuited into
// a cooldown-only clear.
func TestKeeperResetPendingReplayedWhenCountZero(t *testing.T) {
	t.Setenv("CPA_HELPER_DATA_DIR", t.TempDir())
	authName := "zero-me.json"
	authDetail := map[string]any{
		"name": authName, "type": "codex", "auth_index": "idx-zero",
		"email": "zero@example.com", "account_type": "pro", "disabled": false,
		"priority": 1, "access_token": "test-token", "account_id": "acct-zero",
	}
	ctrl := &keeperResetControl{availableCount: 1, fetchMode: "ok", consumeMode: "lost", resetMode: "ok"}
	cpa := newKeeperResetCPA(t, authName, authDetail, ctrl)
	defer cpa.Close()
	handler, cookies, cleanup := setupKeeperResetApp(t, cpa.URL)
	defer cleanup()

	// First reset: the only credit's consume response is lost → pending, fail closed.
	requestJSONExpectStatus(t, handler, http.MethodPost, "/api/codex-keeper/reset-quota", map[string]any{"auth_name": authName}, cookies, http.StatusUnprocessableEntity)
	ctrl.mu.Lock()
	lostIDs := append([]string{}, ctrl.consumeRequestIDs...)
	consumesBefore := ctrl.consumeCalls
	// The lost attempt actually consumed the last credit, so the fresh count is now 0
	// and a replay resolves it idempotently.
	ctrl.availableCount = 0
	ctrl.consumeMode = "ok"
	ctrl.consumeSuccessCode = "already_redeemed"
	ctrl.mu.Unlock()

	// Second reset: count is 0, but the pending redeem MUST still be replayed.
	reset := keeperResetResponse{}
	requestJSON(t, handler, http.MethodPost, "/api/codex-keeper/reset-quota", map[string]any{"auth_name": authName}, cookies, &reset)
	if reset.Status != "ok" || reset.Account.Outcome != "already_redeemed" {
		t.Fatalf("count-zero replay = %+v, want outcome=already_redeemed (not short-circuited to cooldown-only)", reset)
	}

	ctrl.mu.Lock()
	defer ctrl.mu.Unlock()
	if ctrl.consumeCalls <= consumesBefore {
		t.Fatalf("pending redeem was NOT replayed at count=0 (consume calls %d -> %d)", consumesBefore, ctrl.consumeCalls)
	}
	if len(lostIDs) == 0 || ctrl.consumeRequestIDs[len(ctrl.consumeRequestIDs)-1] != lostIDs[0] {
		t.Fatalf("replay used a different redeem_request_id than the pending one: pending=%v all=%v", lostIDs, ctrl.consumeRequestIDs)
	}
}

// TestKeeperResetAccountIDCrossCheck proves the list id_token.chatgpt_account_id is an
// OPTIONAL second cross-check: a matching claim passes, a conflicting claim fails
// closed, and (via the other reset tests where the list omits it) an absent claim never
// blocks a legitimate credential whose download account_id is present.
func TestKeeperResetAccountIDCrossCheck(t *testing.T) {
	t.Setenv("CPA_HELPER_DATA_DIR", t.TempDir())
	authName := "xcheck-me.json"
	authDetail := map[string]any{
		"name": authName, "type": "codex", "auth_index": "idx-xcheck",
		"email": "xcheck@example.com", "account_type": "pro", "disabled": false,
		"priority": 1, "access_token": "test-token", "account_id": "acct-xcheck",
	}
	ctrl := &keeperResetControl{availableCount: 2, fetchMode: "ok", consumeMode: "ok", resetMode: "ok"}
	cpa := newKeeperResetCPA(t, authName, authDetail, ctrl)
	defer cpa.Close()
	handler, cookies, cleanup := setupKeeperResetApp(t, cpa.URL)
	defer cleanup()

	// Matching list claim: reset proceeds and consumes.
	ctrl.mu.Lock()
	ctrl.listAccountIDClaim = "acct-xcheck"
	ctrl.mu.Unlock()
	reset := keeperResetResponse{}
	requestJSON(t, handler, http.MethodPost, "/api/codex-keeper/reset-quota", map[string]any{"auth_name": authName}, cookies, &reset)
	if reset.Status != "ok" || reset.Account.Outcome != "reset" {
		t.Fatalf("matching list account_id claim reset = %+v, want outcome=reset", reset)
	}

	// Conflicting list claim: fail closed, no consume, no cooldown clear.
	ctrl.mu.Lock()
	ctrl.listAccountIDClaim = "acct-DIFFERENT"
	ctrl.consumeCalls = 0
	ctrl.resetQuotaCalls = nil
	ctrl.mu.Unlock()
	requestJSONExpectStatus(t, handler, http.MethodPost, "/api/codex-keeper/reset-quota", map[string]any{"auth_name": authName}, cookies, http.StatusUnprocessableEntity)
	ctrl.mu.Lock()
	defer ctrl.mu.Unlock()
	if ctrl.consumeCalls != 0 || len(ctrl.resetQuotaCalls) != 0 {
		t.Fatalf("account_id conflict must fail closed (consume=%d, reset-quota=%v)", ctrl.consumeCalls, ctrl.resetQuotaCalls)
	}
}

// TestKeeperResetPendingReplayedWhenFetchUnavailable proves an identity-matched pending
// redeem is replayed with its original key even when the fresh reset-credit GET is
// temporarily unavailable (ok=false): the count gate applies only to a NEW operation, so
// a lost-response pending is not stranded while the count endpoint is down.
func TestKeeperResetPendingReplayedWhenFetchUnavailable(t *testing.T) {
	t.Setenv("CPA_HELPER_DATA_DIR", t.TempDir())
	authName := "fetchdown-me.json"
	authDetail := map[string]any{
		"name": authName, "type": "codex", "auth_index": "idx-fetchdown",
		"email": "fetchdown@example.com", "account_type": "pro", "disabled": false,
		"priority": 1, "access_token": "test-token", "account_id": "acct-fetchdown",
	}
	ctrl := &keeperResetControl{availableCount: 1, fetchMode: "ok", consumeMode: "lost", resetMode: "ok"}
	cpa := newKeeperResetCPA(t, authName, authDetail, ctrl)
	defer cpa.Close()
	handler, cookies, cleanup := setupKeeperResetApp(t, cpa.URL)
	defer cleanup()

	// First reset: the consume response is lost → pending, fail closed.
	requestJSONExpectStatus(t, handler, http.MethodPost, "/api/codex-keeper/reset-quota", map[string]any{"auth_name": authName}, cookies, http.StatusUnprocessableEntity)
	ctrl.mu.Lock()
	lostIDs := append([]string{}, ctrl.consumeRequestIDs...)
	// The count endpoint is now temporarily unavailable, but the account recovers.
	ctrl.fetchMode = "fail"
	ctrl.consumeMode = "ok"
	ctrl.consumeSuccessCode = "already_redeemed"
	ctrl.mu.Unlock()

	// Second reset: fresh GET would fail, but the pending redeem MUST still be replayed.
	reset := keeperResetResponse{}
	requestJSON(t, handler, http.MethodPost, "/api/codex-keeper/reset-quota", map[string]any{"auth_name": authName}, cookies, &reset)
	if reset.Status != "ok" || reset.Account.Outcome != "already_redeemed" {
		t.Fatalf("pending replay with fetch down = %+v, want outcome=already_redeemed", reset)
	}
	ctrl.mu.Lock()
	defer ctrl.mu.Unlock()
	if len(lostIDs) == 0 || ctrl.consumeRequestIDs[len(ctrl.consumeRequestIDs)-1] != lostIDs[0] {
		t.Fatalf("replay used a different redeem_request_id: pending=%v all=%v", lostIDs, ctrl.consumeRequestIDs)
	}
}

// TestKeeperResetNonCodexFailsClosed proves the reset resolver only acts on a Codex
// list entry: if the auth_name's remote type has drifted to another provider, the reset
// fails closed and never sends a non-Codex token/index to the OpenAI consume.
func TestKeeperResetNonCodexFailsClosed(t *testing.T) {
	t.Setenv("CPA_HELPER_DATA_DIR", t.TempDir())
	authName := "drift-me.json"
	authDetail := map[string]any{
		"name": authName, "type": "codex", "auth_index": "idx-drift",
		"email": "drift@example.com", "account_type": "pro", "disabled": false,
		"priority": 1, "access_token": "test-token", "account_id": "acct-drift",
	}
	ctrl := &keeperResetControl{availableCount: 2, fetchMode: "ok", consumeMode: "ok", resetMode: "ok"}
	cpa := newKeeperResetCPA(t, authName, authDetail, ctrl)
	defer cpa.Close()
	handler, cookies, cleanup := setupKeeperResetApp(t, cpa.URL)
	defer cleanup()

	// The account was inspected as Codex; now its remote list type has drifted.
	ctrl.mu.Lock()
	ctrl.listTypeOverride = "gemini"
	ctrl.consumeCalls = 0
	ctrl.resetQuotaCalls = nil
	ctrl.mu.Unlock()

	requestJSONExpectStatus(t, handler, http.MethodPost, "/api/codex-keeper/reset-quota", map[string]any{"auth_name": authName}, cookies, http.StatusUnprocessableEntity)
	ctrl.mu.Lock()
	defer ctrl.mu.Unlock()
	if ctrl.consumeCalls != 0 || len(ctrl.resetQuotaCalls) != 0 {
		t.Fatalf("non-Codex reset must fail closed (consume=%d, reset-quota=%v)", ctrl.consumeCalls, ctrl.resetQuotaCalls)
	}
}

// TestKeeperResetOutcomeCodes proves the reset DTO returns a distinct outcome per real
// business result — reset, already_redeemed, no_credit, nothing_to_reset are NOT
// collapsed into one another, and cooldown_only is distinct from no_credit.
func TestKeeperResetOutcomeCodes(t *testing.T) {
	t.Setenv("CPA_HELPER_DATA_DIR", t.TempDir())
	authName := "outcome-me.json"
	authDetail := map[string]any{
		"name": authName, "type": "codex", "auth_index": "idx-outcome",
		"email": "outcome@example.com", "account_type": "pro", "disabled": false,
		"priority": 1, "access_token": "test-token", "account_id": "acct-outcome",
	}
	ctrl := &keeperResetControl{availableCount: 5, fetchMode: "ok", consumeMode: "ok", resetMode: "ok"}
	cpa := newKeeperResetCPA(t, authName, authDetail, ctrl)
	defer cpa.Close()
	handler, cookies, cleanup := setupKeeperResetApp(t, cpa.URL)
	defer cleanup()

	cases := []struct {
		name        string
		consumeMode string
		successCode string
		available   int
		want        string
	}{
		{"reset", "ok", "reset", 5, "reset"},
		{"already_redeemed", "ok", "already_redeemed", 5, "already_redeemed"},
		{"no_credit", "no-credit", "", 5, "no_credit"},
		{"nothing_to_reset", "ok", "nothing_to_reset", 5, "nothing_to_reset"},
		{"cooldown_only", "ok", "reset", 0, "cooldown_only"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctrl.mu.Lock()
			ctrl.availableCount = tc.available
			ctrl.consumeMode = tc.consumeMode
			ctrl.consumeSuccessCode = tc.successCode
			ctrl.mu.Unlock()
			reset := keeperResetResponse{}
			requestJSON(t, handler, http.MethodPost, "/api/codex-keeper/reset-quota", map[string]any{"auth_name": authName}, cookies, &reset)
			if reset.Status != "ok" || reset.Account.Outcome != tc.want {
				t.Fatalf("outcome = %+v, want %q", reset, tc.want)
			}
		})
	}
}

// TestKeeperResetDryRunFailsClosed proves a manual reset is blocked under dry-run so an
// admin testing the keeper never silently burns a paid credit.
func TestKeeperResetDryRunFailsClosed(t *testing.T) {
	t.Setenv("CPA_HELPER_DATA_DIR", t.TempDir())
	authName := "dryrun-me.json"
	authDetail := map[string]any{
		"name": authName, "type": "codex", "auth_index": "idx-dryrun",
		"email": "dryrun@example.com", "account_type": "pro", "disabled": false,
		"priority": 1, "access_token": "test-token", "account_id": "acct-dryrun",
	}
	ctrl := &keeperResetControl{availableCount: 2, fetchMode: "ok", consumeMode: "ok", resetMode: "ok"}
	cpa := newKeeperResetCPA(t, authName, authDetail, ctrl)
	defer cpa.Close()
	handler, cookies, cleanup := setupKeeperResetApp(t, cpa.URL)
	defer cleanup()

	// Turn dry-run on.
	requestJSON(t, handler, http.MethodPut, "/api/codex-keeper/settings", map[string]any{
		"schedule_cron": "0 0 29 2 *", "dry_run": true, "quota_threshold": 100,
		"worker_threads": 1, "cpa_timeout_seconds": 1,
	}, cookies, nil)
	ctrl.mu.Lock()
	ctrl.consumeCalls = 0
	ctrl.resetQuotaCalls = nil
	ctrl.mu.Unlock()

	requestJSONExpectStatus(t, handler, http.MethodPost, "/api/codex-keeper/reset-quota", map[string]any{"auth_name": authName}, cookies, http.StatusUnprocessableEntity)
	ctrl.mu.Lock()
	defer ctrl.mu.Unlock()
	if ctrl.consumeCalls != 0 || len(ctrl.resetQuotaCalls) != 0 {
		t.Fatalf("dry-run reset must fail closed (consume=%d, reset-quota=%v)", ctrl.consumeCalls, ctrl.resetQuotaCalls)
	}
}

// TestKeeperResetResolverGuardsFailClosed proves the identity resolver fails closed on an
// ambiguous/deceptive remote response: a duplicate name, a self-conflicting auth_index
// alias, or a missing access_token must never reach the consume.
func TestKeeperResetResolverGuardsFailClosed(t *testing.T) {
	cases := []struct {
		name  string
		apply func(*keeperResetControl)
	}{
		{"duplicate-name", func(c *keeperResetControl) { c.duplicateListName = true }},
		{"alias-conflict", func(c *keeperResetControl) { c.detailAliasConflict = true }},
		{"missing-access-token", func(c *keeperResetControl) { c.omitAccessToken = true }},
		{"detail-name-mismatch", func(c *keeperResetControl) { c.detailNameOverride = "other-account.json" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("CPA_HELPER_DATA_DIR", t.TempDir())
			authName := "guard-me.json"
			authDetail := map[string]any{
				"name": authName, "type": "codex", "auth_index": "idx-guard",
				"email": "guard@example.com", "account_type": "pro", "disabled": false,
				"priority": 1, "access_token": "test-token", "account_id": "acct-guard",
			}
			ctrl := &keeperResetControl{availableCount: 2, fetchMode: "ok", consumeMode: "ok", resetMode: "ok"}
			cpa := newKeeperResetCPA(t, authName, authDetail, ctrl)
			defer cpa.Close()
			handler, cookies, cleanup := setupKeeperResetApp(t, cpa.URL)
			defer cleanup()

			ctrl.mu.Lock()
			tc.apply(ctrl)
			ctrl.consumeCalls = 0
			ctrl.resetQuotaCalls = nil
			ctrl.mu.Unlock()

			requestJSONExpectStatus(t, handler, http.MethodPost, "/api/codex-keeper/reset-quota", map[string]any{"auth_name": authName}, cookies, http.StatusUnprocessableEntity)
			ctrl.mu.Lock()
			defer ctrl.mu.Unlock()
			if ctrl.consumeCalls != 0 || len(ctrl.resetQuotaCalls) != 0 {
				t.Fatalf("%s must fail closed (consume=%d, reset-quota=%v)", tc.name, ctrl.consumeCalls, ctrl.resetQuotaCalls)
			}
		})
	}
}

// TestKeeperResetPartialAuditOnCooldownFailure proves that when a credit was consumed but
// the local cooldown clear then failed, the audit records an irreversible PARTIAL (not a
// plain error), so the money-affecting half-completion is not masked.
func TestKeeperResetPartialAuditOnCooldownFailure(t *testing.T) {
	t.Setenv("CPA_HELPER_DATA_DIR", t.TempDir())
	authName := "partial-me.json"
	authDetail := map[string]any{
		"name": authName, "type": "codex", "auth_index": "idx-partial",
		"email": "partial@example.com", "account_type": "pro", "disabled": false,
		"priority": 1, "access_token": "test-token", "account_id": "acct-partial",
	}
	ctrl := &keeperResetControl{availableCount: 2, fetchMode: "ok", consumeMode: "ok", resetMode: "http-fail"}
	cpa := newKeeperResetCPA(t, authName, authDetail, ctrl)
	defer cpa.Close()
	handler, cookies, cleanup := setupKeeperResetApp(t, cpa.URL)
	defer cleanup()

	// Consume succeeds (reset) but the cooldown clear fails → 409 partial to the caller.
	requestJSONExpectStatus(t, handler, http.MethodPost, "/api/codex-keeper/reset-quota", map[string]any{"auth_name": authName}, cookies, http.StatusConflict)

	var status struct {
		Logs []string `json:"logs"`
	}
	requestJSON(t, handler, http.MethodGet, "/api/codex-keeper/status", nil, cookies, &status)
	joined := strings.Join(status.Logs, "\n")
	if !strings.Contains(joined, "cooldown_failed_after_consume") || !strings.Contains(joined, "result=partial") {
		t.Fatalf("cooldown-after-consume must audit an irreversible partial; logs=%v", status.Logs)
	}
	// The handler must NOT overwrite the partial with a generic result=error line.
	if strings.Contains(joined, "reset-quota") && strings.Contains(joined, "result=error reason=validation_error") {
		t.Fatalf("partial was masked by a generic result=error audit; logs=%v", status.Logs)
	}
}

func deleteKeeperAccountReq(handler http.Handler, cookies []*http.Cookie, authName string) int {
	req := httptest.NewRequest(http.MethodDelete, "/api/codex-keeper/accounts/"+authName, nil)
	for _, c := range cookies {
		req.AddCookie(c)
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec.Code
}

// TestKeeperDeleteConflictsWithInFlightReset proves delete shares the per-auth fence with
// reset: while a reset holds the lock mid-consume, a concurrent delete of the same account
// is refused (409) so it can never drop the in-flight redeem ledger key; after the reset
// finishes the lock is free again.
func TestKeeperDeleteConflictsWithInFlightReset(t *testing.T) {
	t.Setenv("CPA_HELPER_DATA_DIR", t.TempDir())
	authName := "fence-me.json"
	authDetail := map[string]any{
		"name": authName, "type": "codex", "auth_index": "idx-fence",
		"email": "fence@example.com", "account_type": "pro", "disabled": false,
		"priority": 1, "access_token": "test-token", "account_id": "acct-fence",
	}
	ctrl := &keeperResetControl{availableCount: 2, fetchMode: "ok", consumeMode: "ok", resetMode: "ok"}
	cpa := newKeeperResetCPA(t, authName, authDetail, ctrl)
	defer cpa.Close()
	handler, cookies, cleanup := setupKeeperResetApp(t, cpa.URL)
	defer cleanup()

	// Arm the gate so a reset holds the per-auth lock while blocked in its fresh fetch.
	ctrl.mu.Lock()
	ctrl.gateReached = make(chan struct{})
	ctrl.gateRelease = make(chan struct{})
	ctrl.gateOnce = &sync.Once{}
	ctrl.mu.Unlock()

	winnerCh := make(chan int, 1)
	go func() { winnerCh <- postKeeperReset(handler, cookies, authName) }()
	<-ctrl.gateReached // the reset now holds the per-auth lock

	if s := deleteKeeperAccountReq(handler, cookies, authName); s != http.StatusConflict {
		t.Fatalf("delete during in-flight reset = %d, want 409 (per-auth fence)", s)
	}

	close(ctrl.gateRelease)
	if s := <-winnerCh; s != http.StatusOK {
		t.Fatalf("reset winner = %d, want 200", s)
	}
}

// TestKeeperResetPreservesOtherIdentityPendingKey proves the multi-row ledger keeps each
// identity's key: after an unknown-outcome redeem leaves identity A pending, a reset under
// a DIFFERENT identity B (same auth_name) does NOT overwrite A's key, and when the original
// account (A) returns, its ORIGINAL redeem_request_id is replayed (idempotent) rather than a
// fresh key that could double-consume.
func TestKeeperResetPreservesOtherIdentityPendingKey(t *testing.T) {
	t.Setenv("CPA_HELPER_DATA_DIR", t.TempDir())
	authName := "multi-me.json"
	authDetail := map[string]any{
		"name": authName, "type": "codex", "auth_index": "idx-multi",
		"email": "multi@example.com", "account_type": "pro", "disabled": false,
		"priority": 1, "access_token": "test-token", "account_id": "acct-A",
	}
	ctrl := &keeperResetControl{availableCount: 3, fetchMode: "ok", consumeMode: "lost", resetMode: "ok"}
	cpa := newKeeperResetCPA(t, authName, authDetail, ctrl)
	defer cpa.Close()
	handler, cookies, cleanup := setupKeeperResetApp(t, cpa.URL)
	defer cleanup()

	// Identity A: consume response lost → A pending.
	requestJSONExpectStatus(t, handler, http.MethodPost, "/api/codex-keeper/reset-quota", map[string]any{"auth_name": authName}, cookies, http.StatusUnprocessableEntity)
	ctrl.mu.Lock()
	idA := ctrl.consumeRequestIDs[len(ctrl.consumeRequestIDs)-1]
	// The auth_name is now backed by identity B; its consume succeeds.
	ctrl.accountIDOverride = "acct-B"
	ctrl.consumeMode = "ok"
	ctrl.consumeSuccessCode = "reset"
	ctrl.mu.Unlock()
	reset := keeperResetResponse{}
	requestJSON(t, handler, http.MethodPost, "/api/codex-keeper/reset-quota", map[string]any{"auth_name": authName}, cookies, &reset)
	if reset.Account.Outcome != "reset" {
		t.Fatalf("identity B reset = %+v, want reset", reset)
	}
	ctrl.mu.Lock()
	idB := ctrl.consumeRequestIDs[len(ctrl.consumeRequestIDs)-1]
	// The original account (A) returns; its pending redeem must still be here to replay.
	ctrl.accountIDOverride = ""
	ctrl.consumeSuccessCode = "already_redeemed"
	ctrl.mu.Unlock()
	reset = keeperResetResponse{}
	requestJSON(t, handler, http.MethodPost, "/api/codex-keeper/reset-quota", map[string]any{"auth_name": authName}, cookies, &reset)
	if reset.Account.Outcome != "already_redeemed" {
		t.Fatalf("returned identity A reset = %+v, want already_redeemed (replay of A's key)", reset)
	}
	ctrl.mu.Lock()
	idAReplay := ctrl.consumeRequestIDs[len(ctrl.consumeRequestIDs)-1]
	ctrl.mu.Unlock()
	if idB == idA {
		t.Fatalf("identity B reused A's key %q; identities must have separate keys", idA)
	}
	if idAReplay != idA {
		t.Fatalf("returned identity A did not replay its original key: original %q, replay %q (key was overwritten by B)", idA, idAReplay)
	}
}

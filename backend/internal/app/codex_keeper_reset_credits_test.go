package app

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// realResetCreditsBody is Friday's authoritative rate-limit-reset-credits response:
// two available codex_rate_limits credits expiring 2026-09-21 and 2026-10-04.
const realResetCreditsBody = `{
  "credits": [
    {"id":"RateLimitResetCredit_A","reset_type":"codex_rate_limits","is_supported_by_plan":true,"status":"available","granted_at":"2026-08-22T00:08:46.146320Z","expires_at":"2026-09-21T00:08:46.146320Z","redeem_started_at":null,"redeemed_at":null,"profile_image_url":"https://example.com/a.png","profile_user_id":"user_a","title":"Full reset","description":"secret description a"},
    {"id":"RateLimitResetCredit_B","reset_type":"codex_rate_limits","is_supported_by_plan":true,"status":"available","granted_at":"2026-09-04T02:24:33.736521Z","expires_at":"2026-10-04T02:24:33.736521Z","redeem_started_at":null,"redeemed_at":null,"profile_image_url":"https://example.com/b.png","profile_user_id":"user_b","title":"Full reset","description":"secret description b"}
  ],
  "available_count": 2,
  "total_earned_count": 2,
  "immediate_reset_purchase_eligible": false,
  "history_enabled": false
}`

func mustBody(t *testing.T, raw string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		t.Fatalf("unmarshal fixture: %v", err)
	}
	return m
}

func TestParseKeeperResetCreditsRealFixture(t *testing.T) {
	count, credits, ok := parseKeeperResetCredits(mustBody(t, realResetCreditsBody))
	if !ok {
		t.Fatal("expected ok for real fixture")
	}
	if count != 2 {
		t.Fatalf("count=%d, want 2", count)
	}
	if len(credits) != 2 {
		t.Fatalf("credits=%d, want 2", len(credits))
	}
	// Ascending by expires_at: A (09/21) before B (10/04).
	if credits[0].ID != "RateLimitResetCredit_A" || credits[1].ID != "RateLimitResetCredit_B" {
		t.Fatalf("order = %s,%s want A,B", credits[0].ID, credits[1].ID)
	}
	want, _ := time.Parse(time.RFC3339Nano, "2026-09-21T00:08:46.146320Z")
	if credits[0].ExpiresAt == nil || !credits[0].ExpiresAt.Equal(want) {
		t.Fatalf("expires_at microsecond parse mismatch: %v want %v", credits[0].ExpiresAt, want)
	}
	if credits[0].GrantedAt == nil {
		t.Fatal("granted_at should parse")
	}
	if credits[0].Title != "Full reset" {
		t.Fatalf("title=%q want Full reset", credits[0].Title)
	}
	// Safe projection: the struct has no field for profile URL / description, so
	// re-marshalling must not carry them.
	encoded, _ := json.Marshal(credits[0])
	for _, leaked := range []string{"profile_image_url", "description", "secret description"} {
		if strings.Contains(string(encoded), leaked) {
			t.Fatalf("projected credit leaked %q: %s", leaked, encoded)
		}
	}
}

func TestParseKeeperResetCreditsFilters(t *testing.T) {
	body := mustBody(t, `{
	  "available_count": 1,
	  "credits": [
	    {"id":"keep","reset_type":"codex_rate_limits","status":"available","granted_at":"2026-08-22T00:08:46.146320Z"},
	    {"id":"wrong-type","reset_type":"gpt4_rate_limits","status":"available","granted_at":"2026-08-22T00:08:46.146320Z"},
	    {"id":"redeemed","reset_type":"codex_rate_limits","status":"redeemed","granted_at":"2026-08-22T00:08:46.146320Z"},
	    {"id":"unsupported","reset_type":"codex_rate_limits","status":"available","is_supported_by_plan":false,"granted_at":"2026-08-22T00:08:46.146320Z"}
	  ]
	}`)
	count, credits, ok := parseKeeperResetCredits(body)
	if !ok {
		t.Fatal("expected ok")
	}
	if count != 1 {
		t.Fatalf("count=%d want 1 (authoritative available_count)", count)
	}
	if len(credits) != 1 || credits[0].ID != "keep" {
		t.Fatalf("filtered credits = %+v, want only 'keep'", credits)
	}
}

func TestParseKeeperResetCreditsNullExpirySortedLast(t *testing.T) {
	body := mustBody(t, `{
	  "available_count": 3,
	  "credits": [
	    {"id":"never","reset_type":"codex_rate_limits","status":"available","granted_at":"2026-08-22T00:08:46.146320Z","expires_at":null},
	    {"id":"later","reset_type":"codex_rate_limits","status":"available","granted_at":"2026-08-22T00:08:46.146320Z","expires_at":"2026-10-04T02:24:33.736521Z"},
	    {"id":"sooner","reset_type":"codex_rate_limits","status":"available","granted_at":"2026-08-22T00:08:46.146320Z","expires_at":"2026-09-21T00:08:46.146320Z"}
	  ]
	}`)
	_, credits, ok := parseKeeperResetCredits(body)
	if !ok {
		t.Fatal("expected ok")
	}
	if len(credits) != 3 {
		t.Fatalf("credits=%d want 3 (null-expiry entry must be kept)", len(credits))
	}
	if credits[0].ID != "sooner" || credits[1].ID != "later" || credits[2].ID != "never" {
		t.Fatalf("order = %s,%s,%s want sooner,later,never", credits[0].ID, credits[1].ID, credits[2].ID)
	}
	if credits[2].ExpiresAt != nil {
		t.Fatal("never-expiring credit must keep nil ExpiresAt")
	}
}

func TestParseKeeperResetCreditsTruncatedDetail(t *testing.T) {
	// Upstream may truncate the detail list while available_count stays authoritative.
	body := mustBody(t, `{
	  "available_count": 5,
	  "credits": [
	    {"id":"a","reset_type":"codex_rate_limits","status":"available","granted_at":"2026-08-22T00:08:46.146320Z","expires_at":"2026-09-21T00:08:46.146320Z"},
	    {"id":"b","reset_type":"codex_rate_limits","status":"available","granted_at":"2026-08-22T00:08:46.146320Z","expires_at":"2026-10-04T02:24:33.736521Z"}
	  ]
	}`)
	count, credits, ok := parseKeeperResetCredits(body)
	if !ok {
		t.Fatal("expected ok")
	}
	if count != 5 {
		t.Fatalf("count=%d want 5 (must not be overwritten by len)", count)
	}
	if len(credits) != 2 {
		t.Fatalf("credits=%d want 2", len(credits))
	}
}

func TestParseKeeperResetCreditsEmptyArrayClearsToZero(t *testing.T) {
	count, credits, ok := parseKeeperResetCredits(mustBody(t, `{"available_count":0,"credits":[]}`))
	if !ok {
		t.Fatal("expected ok for a successful empty result")
	}
	if count != 0 {
		t.Fatalf("count=%d want 0", count)
	}
	if len(credits) != 0 {
		t.Fatalf("credits=%d want 0", len(credits))
	}
}

func TestParseKeeperResetCreditsMalformed(t *testing.T) {
	cases := map[string]string{
		"missing available_count":    `{"credits":[]}`,
		"non-int available_count":    `{"available_count":"lots","credits":[]}`,
		"fractional available_count": `{"available_count":2.5,"credits":[]}`,
		"negative available_count":   `{"available_count":-1,"credits":[]}`,
		"oversized available_count":  `{"available_count":1e19,"credits":[]}`,
		"credits not array":          `{"available_count":1,"credits":{}}`,
		"missing credits":            `{"available_count":1}`,
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			if _, _, ok := parseKeeperResetCredits(mustBody(t, raw)); ok {
				t.Fatalf("expected ok=false for %s", name)
			}
		})
	}
	if _, _, ok := parseKeeperResetCredits(nil); ok {
		t.Fatal("expected ok=false for nil body")
	}
}

// TestParseKeeperResetCreditsDropsMalformedEntries pins the strict per-entry
// validation: an available/codex entry is dropped (not kept with silently nil'd
// fields) when it has an empty id, a missing/unparseable granted_at, or a non-null
// but unparseable expires_at. The authoritative available_count is unaffected, and
// a fully valid entry (including a null "never expires") still survives.
func TestParseKeeperResetCreditsDropsMalformedEntries(t *testing.T) {
	body := mustBody(t, `{
	  "available_count": 5,
	  "credits": [
	    {"id":"","reset_type":"codex_rate_limits","status":"available","granted_at":"2026-08-22T00:08:46.146320Z","expires_at":"2026-09-21T00:08:46.146320Z"},
	    {"id":"no-granted","reset_type":"codex_rate_limits","status":"available","expires_at":"2026-09-21T00:08:46.146320Z"},
	    {"id":"bad-expiry","reset_type":"codex_rate_limits","status":"available","granted_at":"2026-08-22T00:08:46.146320Z","expires_at":"not-a-date"},
	    {"id":"good","reset_type":"codex_rate_limits","status":"available","granted_at":"2026-08-22T00:08:46.146320Z","expires_at":null}
	  ]
	}`)
	count, credits, ok := parseKeeperResetCredits(body)
	if !ok {
		t.Fatal("expected ok (malformed entries are dropped, not fail-closed)")
	}
	if count != 5 {
		t.Fatalf("count=%d want 5 (authoritative, unaffected by dropped entries)", count)
	}
	if len(credits) != 1 || credits[0].ID != "good" {
		t.Fatalf("kept credits = %+v, want only the valid 'good' entry", credits)
	}
	if credits[0].GrantedAt == nil {
		t.Fatal("kept entry must carry a parsed granted_at, never nil")
	}
	if credits[0].ExpiresAt != nil {
		t.Fatal("the valid null-expiry entry must keep nil ExpiresAt (never expires)")
	}
}

// TestParseKeeperResetCreditsExpiryPresentVsNull pins the strict present-vs-null
// distinction for expires_at: only JSON null or an absent field mean "never
// expires" and are kept; a present empty string or a non-string value is malformed
// and the entry is dropped (not silently treated as never-expiring).
func TestParseKeeperResetCreditsExpiryPresentVsNull(t *testing.T) {
	const g = `"granted_at":"2026-08-22T00:08:46.146320Z"`
	body := mustBody(t, `{
	  "available_count": 6,
	  "credits": [
	    {"id":"null-expiry","reset_type":"codex_rate_limits","status":"available",`+g+`,"expires_at":null},
	    {"id":"absent-expiry","reset_type":"codex_rate_limits","status":"available",`+g+`},
	    {"id":"empty-string","reset_type":"codex_rate_limits","status":"available",`+g+`,"expires_at":""},
	    {"id":"numeric","reset_type":"codex_rate_limits","status":"available",`+g+`,"expires_at":123},
	    {"id":"valid","reset_type":"codex_rate_limits","status":"available",`+g+`,"expires_at":"2026-09-21T00:08:46.146320Z"}
	  ]
	}`)
	_, credits, ok := parseKeeperResetCredits(body)
	if !ok {
		t.Fatal("expected ok")
	}
	kept := map[string]*keeperResetCredit{}
	for i := range credits {
		kept[credits[i].ID] = &credits[i]
	}
	if len(credits) != 3 {
		t.Fatalf("kept %d credits, want 3 (null-expiry, absent-expiry, valid); got %v", len(credits), keysOf(kept))
	}
	if c, present := kept["null-expiry"]; !present || c.ExpiresAt != nil {
		t.Fatal("null expires_at must be kept as never-expiring")
	}
	if c, present := kept["absent-expiry"]; !present || c.ExpiresAt != nil {
		t.Fatal("absent expires_at must be kept as never-expiring")
	}
	if _, present := kept["empty-string"]; present {
		t.Fatal(`expires_at:"" must be dropped, not treated as never-expiring`)
	}
	if _, present := kept["numeric"]; present {
		t.Fatal("expires_at:123 (non-string) must be dropped, not treated as never-expiring")
	}
	if c, present := kept["valid"]; !present || c.ExpiresAt == nil {
		t.Fatal("valid RFC3339 expires_at must be kept with a parsed time")
	}
}

func keysOf(m map[string]*keeperResetCredit) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// fetchResetCreditsCPA builds a fake CLIProxyAPI whose api-call proxy returns the
// given inner status code and body (an object, or a raw string for malformed cases).
func fetchResetCreditsCPA(t *testing.T, innerStatus int, innerBody any, outerStatus int) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path != "/v0/management/api-call" {
			http.Error(w, "unexpected path "+r.URL.Path, http.StatusNotFound)
			return
		}
		if outerStatus != 0 && outerStatus != http.StatusOK {
			http.Error(w, "outer failure", outerStatus)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status_code": innerStatus,
			"body":        innerBody,
		})
	}))
}

func fetchResetCreditsCfg(url string) AppConfig {
	return AppConfig{
		Collector:   CollectorConfig{CLIProxyURL: url, ManagementKey: "test-key"},
		CodexKeeper: KeeperConfig{UsageTimeoutSeconds: 5},
	}
}

func TestFetchKeeperResetCreditsSuccess(t *testing.T) {
	var body map[string]any
	_ = json.Unmarshal([]byte(realResetCreditsBody), &body)
	cpa := fetchResetCreditsCPA(t, http.StatusOK, body, http.StatusOK)
	defer cpa.Close()

	count, credits, ok := (&App{}).fetchKeeperResetCredits(context.Background(), fetchResetCreditsCfg(cpa.URL), map[string]any{"auth_index": "idx-1"})
	if !ok {
		t.Fatal("expected ok")
	}
	if count != 2 || len(credits) != 2 {
		t.Fatalf("count=%d credits=%d want 2,2", count, len(credits))
	}
}

func TestFetchKeeperResetCreditsInnerErrorsPreserve(t *testing.T) {
	var body map[string]any
	_ = json.Unmarshal([]byte(realResetCreditsBody), &body)
	cases := []struct {
		name        string
		innerStatus int
		innerBody   any
		outerStatus int
	}{
		{"inner 401", http.StatusUnauthorized, map[string]any{"detail": "unauth"}, http.StatusOK},
		{"inner 500", http.StatusInternalServerError, map[string]any{"detail": "boom"}, http.StatusOK},
		{"outer 502", http.StatusOK, body, http.StatusBadGateway},
		{"malformed inner body", http.StatusOK, "not-json-object", http.StatusOK},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cpa := fetchResetCreditsCPA(t, tc.innerStatus, tc.innerBody, tc.outerStatus)
			defer cpa.Close()
			_, _, ok := (&App{}).fetchKeeperResetCredits(context.Background(), fetchResetCreditsCfg(cpa.URL), map[string]any{"auth_index": "idx-1"})
			if ok {
				t.Fatalf("%s: expected ok=false so the previous snapshot is preserved", tc.name)
			}
		})
	}
}

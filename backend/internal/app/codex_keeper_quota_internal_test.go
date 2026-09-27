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

// --- parser tests ------------------------------------------------------------

func TestParseKimiQuotaGroups(t *testing.T) {
	reset := time.Unix(1780000000, 0)
	body := map[string]any{"usages": []any{
		map[string]any{"limit_5h": float64(100), "remaining_5h": float64(40), "reset_at": float64(1780000000)},
		map[string]any{"limit_weekly": float64(500), "used_weekly": float64(250)},
	}}
	groups, ok := parseKimiQuotaGroups(body)
	if !ok || len(groups) != 1 {
		t.Fatalf("parse = (%v, %+v), want one group", ok, groups)
	}
	g := groups[0]
	if g.DisplayName != "Kimi" || len(g.Buckets) != 2 {
		t.Fatalf("group = %+v, want Kimi with 2 buckets", g)
	}
	var fiveH, weekly *keeperAntigravityBucket
	for i := range g.Buckets {
		switch g.Buckets[i].Window {
		case "5h":
			fiveH = &g.Buckets[i]
		case "weekly":
			weekly = &g.Buckets[i]
		}
	}
	if fiveH == nil || fiveH.RemainingFraction != 0.4 {
		t.Fatalf("5h bucket = %+v, want remaining 0.4", fiveH)
	}
	if fiveH.ResetAt == nil || !fiveH.ResetAt.Equal(reset) {
		t.Fatalf("5h resetAt = %v, want %v", fiveH.ResetAt, reset)
	}
	if weekly == nil || weekly.RemainingFraction != 0.5 {
		t.Fatalf("weekly bucket = %+v, want remaining 0.5", weekly)
	}
}

func TestParseKimiQuotaGroupsMalformed(t *testing.T) {
	if _, ok := parseKimiQuotaGroups(nil); ok {
		t.Fatal("nil body must be ok=false")
	}
	if _, ok := parseKimiQuotaGroups(map[string]any{}); ok {
		t.Fatal("no usages must be ok=false")
	}
	if _, ok := parseKimiQuotaGroups(map[string]any{"usages": []any{}}); ok {
		t.Fatal("empty usages must be ok=false")
	}
	// A limit entry with no resolvable remaining/used fraction yields no bucket.
	bad := map[string]any{"usages": []any{
		map[string]any{"limit_5h": "not-a-number"},
		map[string]any{"limit_weekly": float64(500)},
	}}
	if _, ok := parseKimiQuotaGroups(bad); ok {
		t.Fatal("non-numeric/unresolvable usages must be ok=false")
	}
}

func TestParseXAIQuotaGroups(t *testing.T) {
	reset := time.Unix(1780000000, 0)
	// Live sample shape: type WEEKLY, creditUsagePercent is the USED percent.
	body := map[string]any{
		"config": map[string]any{"creditUsagePercent": float64(30)},
		"currentPeriod": map[string]any{
			"type": "USAGE_PERIOD_TYPE_WEEKLY",
			"end":  float64(1780000000),
		},
	}
	groups, ok := parseXAIQuotaGroups(body)
	if !ok || len(groups) != 1 {
		t.Fatalf("parse = (%v, %+v), want one group", ok, groups)
	}
	g := groups[0]
	if g.DisplayName != "xAI" || len(g.Buckets) != 1 {
		t.Fatalf("group = %+v, want xAI with 1 bucket", g)
	}
	b := g.Buckets[0]
	if b.BucketID != "weekly" || b.Window != "weekly" {
		t.Fatalf("bucket = %+v, want weekly", b)
	}
	if b.RemainingFraction != 0.7 {
		t.Fatalf("remaining = %v, want 0.7", b.RemainingFraction)
	}
	if b.ResetAt == nil || !b.ResetAt.Equal(reset) {
		t.Fatalf("resetAt = %v, want %v", b.ResetAt, reset)
	}
	// Missing type defaults to weekly.
	bodyNoType := map[string]any{
		"config":        map[string]any{"creditUsagePercent": float64(30)},
		"currentPeriod": map[string]any{"end": float64(1780000000)},
	}
	gNoType, ok := parseXAIQuotaGroups(bodyNoType)
	if !ok || gNoType[0].Buckets[0].Window != "weekly" {
		t.Fatalf("missing type must default to weekly: (%v, %+v)", ok, gNoType)
	}
	// An explicit MONTHLY type yields a monthly bucket.
	bodyMonthly := map[string]any{
		"config": map[string]any{"creditUsagePercent": float64(30)},
		"currentPeriod": map[string]any{
			"type": "USAGE_PERIOD_TYPE_MONTHLY",
			"end":  float64(1780000000),
		},
	}
	gMonthly, ok := parseXAIQuotaGroups(bodyMonthly)
	if !ok || gMonthly[0].Buckets[0].Window != "monthly" {
		t.Fatalf("MONTHLY type must yield monthly bucket: (%v, %+v)", ok, gMonthly)
	}
	// RFC3339 period end also parses.
	body2 := map[string]any{
		"config": map[string]any{"creditUsagePercent": float64(30)},
		"currentPeriod": map[string]any{
			"type": "USAGE_PERIOD_TYPE_WEEKLY",
			"end":  "2026-10-01T00:00:00Z",
		},
	}
	g2, ok := parseXAIQuotaGroups(body2)
	if !ok || g2[0].Buckets[0].ResetAt == nil {
		t.Fatalf("RFC3339 period end not parsed: (%v, %+v)", ok, g2)
	}
}

func TestParseXAIQuotaGroupsMalformed(t *testing.T) {
	if _, ok := parseXAIQuotaGroups(nil); ok {
		t.Fatal("nil body must be ok=false")
	}
	if _, ok := parseXAIQuotaGroups(map[string]any{"config": map[string]any{}}); ok {
		t.Fatal("missing usage percent must be ok=false")
	}
}

func TestParseDevinQuotaGroups(t *testing.T) {
	reset := time.Unix(1780000000, 0)
	body := map[string]any{
		"userStatus": map[string]any{
			"planStatus": map[string]any{
				"dailyQuotaRemainingPercent":  float64(60),
				"weeklyQuotaRemainingPercent": float64(25),
				"dailyResetAtUnix":            float64(1780000000),
			},
		},
	}
	groups, ok := parseDevinQuotaGroups(body)
	if !ok || len(groups) != 1 {
		t.Fatalf("parse = (%v, %+v), want one group", ok, groups)
	}
	g := groups[0]
	if g.DisplayName != "Devin" || len(g.Buckets) != 2 {
		t.Fatalf("group = %+v, want Devin with 2 buckets", g)
	}
	var daily, weekly *keeperAntigravityBucket
	for i := range g.Buckets {
		switch g.Buckets[i].BucketID {
		case "daily":
			daily = &g.Buckets[i]
		case "weekly":
			weekly = &g.Buckets[i]
		}
	}
	if daily == nil || daily.RemainingFraction != 0.6 {
		t.Fatalf("daily bucket = %+v, want remaining 0.6", daily)
	}
	if daily.ResetAt == nil || !daily.ResetAt.Equal(reset) {
		t.Fatalf("daily resetAt = %v, want %v", daily.ResetAt, reset)
	}
	if weekly == nil || weekly.RemainingFraction != 0.25 {
		t.Fatalf("weekly bucket = %+v, want remaining 0.25", weekly)
	}

	// Pro-tier shape seen in production: only weekly percent + dailyQuotaResetAtUnix.
	proBody := map[string]any{
		"userStatus": map[string]any{
			"planStatus": map[string]any{
				"weeklyQuotaRemainingPercent": float64(49),
				"dailyQuotaResetAtUnix":       float64(1780000000),
			},
		},
	}
	groups, ok = parseDevinQuotaGroups(proBody)
	if !ok || len(groups) != 1 || len(groups[0].Buckets) != 2 {
		t.Fatalf("pro parse = (%v, %+v), want one group with 2 buckets", ok, groups)
	}
	daily, weekly = nil, nil
	for i := range groups[0].Buckets {
		switch groups[0].Buckets[i].BucketID {
		case "daily":
			daily = &groups[0].Buckets[i]
		case "weekly":
			weekly = &groups[0].Buckets[i]
		}
	}
	if daily == nil || daily.RemainingFraction != 0 {
		t.Fatalf("pro daily bucket = %+v, want remaining 0", daily)
	}
	if daily.ResetAt == nil || !daily.ResetAt.Equal(reset) {
		t.Fatalf("pro daily resetAt = %v, want %v", daily.ResetAt, reset)
	}
	if weekly == nil || weekly.RemainingFraction != 0.49 {
		t.Fatalf("pro weekly bucket = %+v, want remaining 0.49", weekly)
	}
}

func TestParseDevinQuotaGroupsMalformed(t *testing.T) {
	if _, ok := parseDevinQuotaGroups(nil); ok {
		t.Fatal("nil body must be ok=false")
	}
	// planStatus present but with no recognizable fields → no buckets.
	if _, ok := parseDevinQuotaGroups(map[string]any{
		"userStatus": map[string]any{"planStatus": map[string]any{"foo": "bar"}},
	}); ok {
		t.Fatal("unrecognizable planStatus must be ok=false")
	}
}

func TestParseCodexQuotaGroups(t *testing.T) {
	payload := map[string]any{
		"rate_limit": map[string]any{
			"primary_window":   map[string]any{"used_percent": float64(25), "limit_window_seconds": float64(18000), "reset_after_seconds": float64(3600)},
			"secondary_window": map[string]any{"used_percent": float64(80), "limit_window_seconds": float64(604800), "reset_at": float64(1780000000)},
		},
	}
	groups, ok := parseCodexQuotaGroups(payload)
	if !ok || len(groups) != 1 {
		t.Fatalf("parse = (%v, %+v), want one group", ok, groups)
	}
	g := groups[0]
	if g.DisplayName != "Codex" || len(g.Buckets) != 2 {
		t.Fatalf("group = %+v, want Codex with 2 buckets", g)
	}
	primary := g.Buckets[0]
	if primary.BucketID != "primary" || primary.RemainingFraction != 0.75 {
		t.Fatalf("primary bucket = %+v, want remaining 0.75", primary)
	}
	if primary.Window != "5h0m0s" {
		t.Fatalf("primary window = %q, want 5h0m0s", primary.Window)
	}
	if primary.ResetAt == nil {
		t.Fatal("primary resetAt must be derived from reset_after_seconds")
	}
	secondary := g.Buckets[1]
	if secondary.BucketID != "secondary" || secondary.RemainingFraction < 0.19 || secondary.RemainingFraction > 0.21 {
		t.Fatalf("secondary bucket = %+v, want remaining 0.2", secondary)
	}
	if secondary.ResetAt == nil || !secondary.ResetAt.Equal(time.Unix(1780000000, 0)) {
		t.Fatalf("secondary resetAt = %v", secondary.ResetAt)
	}
}

func TestParseCodexQuotaGroupsMalformed(t *testing.T) {
	if _, ok := parseCodexQuotaGroups(map[string]any{}); ok {
		t.Fatal("missing rate_limit must be ok=false")
	}
	if _, ok := parseCodexQuotaGroups(map[string]any{"rate_limit": map[string]any{}}); ok {
		t.Fatal("rate_limit without usable windows must be ok=false")
	}
}

// --- identity helpers --------------------------------------------------------

func TestKeeperQuotaIdentityDigest(t *testing.T) {
	d1 := keeperQuotaIdentityDigest("kimi", "idx-1", "User@Example.com")
	d2 := keeperQuotaIdentityDigest("kimi", "idx-1", "user@example.com")
	if d1 != d2 {
		t.Fatal("digest must lowercase email")
	}
	if !strings.HasPrefix(d1, "v1:") || len(d1) != len("v1:")+64 {
		t.Fatalf("digest format = %q", d1)
	}
	if keeperQuotaIdentityDigest("xai", "idx-1", "user@example.com") == d1 {
		t.Fatal("digest must differ per provider")
	}
	if keeperQuotaIdentityDigest("kimi", "idx-2", "user@example.com") == d1 {
		t.Fatal("digest must differ per auth_index")
	}
	if keeperQuotaIdentityDigest("kimi", "idx-1", "user@example.com") != d1 {
		t.Fatal("digest must be deterministic")
	}
}

func TestKeeperReconcileQuotaIdentity(t *testing.T) {
	name := "kimi-1.json"
	happy := func() (map[string]any, map[string]any) {
		return map[string]any{"name": name, "type": "kimi", "auth_index": "idx-k", "email": "a@x.com"},
			map[string]any{"name": name, "type": "kimi", "auth_index": "idx-k", "email": "a@x.com", "access_token": "t"}
	}
	t.Run("happy", func(t *testing.T) {
		list, detail := happy()
		id, ok := keeperReconcileQuotaIdentity(list, detail, "kimi", name)
		if !ok || id.authIndex != "idx-k" || id.email != "a@x.com" {
			t.Fatalf("identity = %+v, ok=%v", id, ok)
		}
	})
	t.Run("email-optional", func(t *testing.T) {
		list, detail := happy()
		delete(list, "email")
		delete(detail, "email")
		id, ok := keeperReconcileQuotaIdentity(list, detail, "kimi", name)
		if !ok || id.authIndex != "idx-k" || id.email != "" {
			t.Fatalf("identity = %+v, ok=%v", id, ok)
		}
	})
	t.Run("list-type-mismatch", func(t *testing.T) {
		list, detail := happy()
		list["type"] = "codex"
		if _, ok := keeperReconcileQuotaIdentity(list, detail, "kimi", name); ok {
			t.Fatal("list type≠provider must fail")
		}
	})
	t.Run("detail-type-mismatch", func(t *testing.T) {
		list, detail := happy()
		detail["type"] = "xai"
		if _, ok := keeperReconcileQuotaIdentity(list, detail, "kimi", name); ok {
			t.Fatal("detail type≠provider must fail")
		}
	})
	t.Run("auth-index-missing-both", func(t *testing.T) {
		list, detail := happy()
		delete(list, "auth_index")
		delete(detail, "auth_index")
		if _, ok := keeperReconcileQuotaIdentity(list, detail, "kimi", name); ok {
			t.Fatal("missing auth_index on both sources must fail")
		}
	})
	t.Run("auth-index-disagree", func(t *testing.T) {
		list, detail := happy()
		detail["auth_index"] = "idx-other"
		if _, ok := keeperReconcileQuotaIdentity(list, detail, "kimi", name); ok {
			t.Fatal("conflicting auth_index must fail")
		}
	})
}

// --- end-to-end inspection ---------------------------------------------------

// TestKeeperInspectKimiAccount mirrors TestKeeperInspectAntigravityAccount for the generic
// quota-provider path: a kimi auth file is routed to processKeeperQuotaAuth, its quota is
// fetched via api-call and stored in the antigravity_quota column, the identity digest is
// bound, and codex-only columns stay NULL.
func TestKeeperInspectKimiAccount(t *testing.T) {
	t.Setenv("CPA_HELPER_DATA_DIR", t.TempDir())
	const authName = "kimi-1.json"
	kimiBody := map[string]any{"usages": []any{
		map[string]any{"limit_5h": float64(100), "remaining_5h": float64(40), "reset_at": float64(1780000000)},
		map[string]any{"limit_weekly": float64(500), "used_weekly": float64(250)},
	}}
	var quotaCalls int
	cpa := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v0/management/auth-files":
			_ = json.NewEncoder(w).Encode(map[string]any{"files": []map[string]any{
				{"name": authName, "type": "kimi", "provider": "kimi", "auth_index": "idx-kimi", "email": "Kimi@Example.com"},
			}})
		case r.Method == http.MethodGet && r.URL.Path == "/v0/management/auth-files/download":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"name": authName, "type": "kimi", "provider": "kimi", "auth_index": "idx-kimi",
				"email": "Kimi@Example.com", "disabled": false, "priority": 1, "access_token": "tok",
			})
		case r.Method == http.MethodPost && r.URL.Path == "/v0/management/api-call":
			var p struct {
				AuthIndex string `json:"auth_index"`
				URL       string `json:"url"`
			}
			_ = json.NewDecoder(r.Body).Decode(&p)
			if p.AuthIndex != "idx-kimi" || p.URL != kimiQuotaURL {
				http.Error(w, "unexpected api-call", http.StatusBadRequest)
				return
			}
			quotaCalls++
			_ = json.NewEncoder(w).Encode(map[string]any{"status_code": 200, "body": kimiBody})
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

	stats, err := app.keeper.InspectAccountsLocked([]string{authName})
	if err != nil {
		t.Fatalf("InspectAccountsLocked: %v", err)
	}
	if stats.Healthy != 1 {
		t.Fatalf("stats = %+v, want Healthy=1", stats)
	}
	if quotaCalls != 1 {
		t.Fatalf("quota api-calls = %d, want 1", quotaCalls)
	}
	st, err := app.getKeeperState(ctx, authName)
	if err != nil {
		t.Fatalf("get state: %v", err)
	}
	if st.Provider == nil || *st.Provider != "kimi" {
		t.Fatalf("stored provider = %v, want kimi", st.Provider)
	}
	if len(st.AntigravityQuota) != 1 || st.AntigravityQuota[0].DisplayName != "Kimi" || len(st.AntigravityQuota[0].Buckets) != 2 {
		t.Fatalf("stored quota not persisted: %+v", st.AntigravityQuota)
	}
	wantDigest := keeperQuotaIdentityDigest("kimi", "idx-kimi", "Kimi@Example.com")
	if st.AntigravityIdentityDigest == nil || *st.AntigravityIdentityDigest != wantDigest {
		t.Fatalf("identity digest = %v, want %q", st.AntigravityIdentityDigest, wantDigest)
	}
	// Codex-only fields stay empty for a generic quota provider.
	if st.AccountID != nil || st.PrimaryUsedPercent != nil || st.ResetCreditCount != nil || st.ResetCredits != nil || st.SubscriptionActiveUntil != nil {
		t.Fatalf("codex-only fields must be nil for kimi: %+v", st)
	}
	// The account list the frontend consumes carries provider + quota.
	accounts, err := app.listKeeperAccounts(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	var found *keeperAccount
	for i := range accounts {
		if accounts[i].Name == authName {
			found = &accounts[i]
		}
	}
	if found == nil || found.Provider == nil || *found.Provider != "kimi" || len(found.AntigravityQuota) != 1 {
		t.Fatalf("kimi account not visible with quota in list: %+v", found)
	}
	raw, err := json.Marshal(keeperAccountResponses(accounts, nil))
	if err != nil {
		t.Fatalf("marshal responses: %v", err)
	}
	js := string(raw)
	if !strings.Contains(js, `"provider":"kimi"`) || !strings.Contains(js, `"antigravity_quota":[`) {
		t.Fatalf("API response drops provider/quota: %s", js)
	}
}

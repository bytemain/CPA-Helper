package app

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// antigravityGoldenBody is the real retrieveUserQuotaSummary response captured from a production
// Antigravity account (values from @Friday's read-back), used as the golden parse fixture.
const antigravityGoldenBody = `{
  "groups": [
    {
      "buckets": [
        {"bucketId":"gemini-weekly","displayName":"Weekly Limit Remaining","window":"weekly","resetTime":"2026-09-13T14:43:19Z","description":"You have used some of your weekly limit, it will fully refresh in 4 days, 6 hours.","remainingFraction":0.8308332},
        {"bucketId":"gemini-5h","displayName":"Five Hour Limit Remaining","window":"5h","resetTime":"2026-09-09T13:34:49Z","description":"You have used some of your 5-hour limit, it will fully refresh in 4 hours, 56 minutes.","remainingFraction":0.9927133}
      ],
      "displayName":"Gemini Models",
      "description":"Models within this group: Gemini Flash, Gemini Pro"
    },
    {
      "buckets": [
        {"bucketId":"3p-weekly","displayName":"Weekly Limit Remaining","window":"weekly","resetTime":"2026-09-14T06:16:39Z","remainingFraction":1.0},
        {"bucketId":"3p-5h","displayName":"Five Hour Limit Remaining","window":"5h","resetTime":"2026-09-09T13:34:56Z","remainingFraction":1.0}
      ],
      "displayName":"Claude and GPT models",
      "description":"Models within this group: Claude Opus, Claude Sonnet, GPT-OSS"
    }
  ],
  "description":"Within each group, models share a weekly limit and a 5-hour limit."
}`

func antigravityGoldenMap(t *testing.T) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(antigravityGoldenBody), &m); err != nil {
		t.Fatalf("unmarshal golden body: %v", err)
	}
	return m
}

func TestParseAntigravityQuotaGroupsGolden(t *testing.T) {
	groups, ok := parseAntigravityQuotaGroups(antigravityGoldenMap(t))
	if !ok {
		t.Fatal("golden body must parse")
	}
	if len(groups) != 2 {
		t.Fatalf("groups = %d, want 2", len(groups))
	}
	g0 := groups[0]
	if g0.DisplayName != "Gemini Models" || len(g0.Buckets) != 2 {
		t.Fatalf("group0 = %+v, want Gemini Models with 2 buckets", g0)
	}
	weekly := g0.Buckets[0]
	if weekly.BucketID != "gemini-weekly" || weekly.Window != "weekly" || weekly.RemainingFraction != 0.8308332 {
		t.Fatalf("gemini weekly bucket = %+v", weekly)
	}
	if weekly.ResetAt == nil || weekly.ResetAt.UTC().Format("2006-01-02T15:04:05Z") != "2026-09-13T14:43:19Z" {
		t.Fatalf("gemini weekly resetAt = %v, want 2026-09-13T14:43:19Z", weekly.ResetAt)
	}
	if g0.Buckets[1].Window != "5h" || g0.Buckets[1].RemainingFraction != 0.9927133 {
		t.Fatalf("gemini 5h bucket = %+v", g0.Buckets[1])
	}
	// Second group: buckets without a description still parse; fraction 1.0 kept.
	if groups[1].DisplayName != "Claude and GPT models" || len(groups[1].Buckets) != 2 || groups[1].Buckets[0].RemainingFraction != 1.0 {
		t.Fatalf("group1 = %+v", groups[1])
	}
}

func TestParseAntigravityQuotaGroupsEdgeCases(t *testing.T) {
	// No groups → not ok.
	if _, ok := parseAntigravityQuotaGroups(map[string]any{}); ok {
		t.Fatal("missing groups must be ok=false")
	}
	if _, ok := parseAntigravityQuotaGroups(map[string]any{"groups": []any{}}); ok {
		t.Fatal("empty groups must be ok=false")
	}
	// A group whose only bucket has an out-of-range fraction is dropped → no usable groups.
	bad := map[string]any{"groups": []any{map[string]any{
		"displayName": "G", "buckets": []any{map[string]any{"window": "5h", "remainingFraction": 1.5}},
	}}}
	if _, ok := parseAntigravityQuotaGroups(bad); ok {
		t.Fatal("out-of-range fraction must drop the bucket and the empty group")
	}
	// A present-but-unparseable resetTime drops just that bucket.
	badReset := map[string]any{"groups": []any{map[string]any{
		"displayName": "G", "buckets": []any{
			map[string]any{"window": "5h", "remainingFraction": 0.5, "resetTime": "not-a-time"},
			map[string]any{"window": "weekly", "remainingFraction": 0.9},
		},
	}}}
	groups, ok := parseAntigravityQuotaGroups(badReset)
	if !ok || len(groups) != 1 || len(groups[0].Buckets) != 1 || groups[0].Buckets[0].Window != "weekly" {
		t.Fatalf("bad-reset case = (%v, %+v), want the weekly bucket only", ok, groups)
	}
	// snake_case aliases are accepted.
	snake := map[string]any{"groups": []any{map[string]any{
		"display_name": "G", "buckets": []any{map[string]any{"bucket_id": "b", "display_name": "B", "window": "5h", "remaining_fraction": 0.25, "reset_time": "2026-01-01T00:00:00Z"}},
	}}}
	sg, ok := parseAntigravityQuotaGroups(snake)
	if !ok || sg[0].Buckets[0].BucketID != "b" || sg[0].Buckets[0].RemainingFraction != 0.25 {
		t.Fatalf("snake_case aliases not accepted: %v %+v", ok, sg)
	}
}

func TestKeeperExplicitAntigravityProjectID(t *testing.T) {
	cases := []struct {
		name    string
		detail  map[string]any
		want    string
		wantErr bool
	}{
		{"top-level", map[string]any{"project_id": "aicode-consumers"}, "aicode-consumers", false},
		{"camel", map[string]any{"projectId": "p2"}, "p2", false},
		{"metadata", map[string]any{"metadata": map[string]any{"project_id": "p3"}}, "p3", false},
		{"attributes-virtual", map[string]any{"attributes": map[string]any{"gemini_virtual_project": "p4"}}, "p4", false},
		{"installed", map[string]any{"installed": map[string]any{"project_id": "p5"}}, "p5", false},
		{"web", map[string]any{"web": map[string]any{"project_id": "p6"}}, "p6", false},
		{"none", map[string]any{"email": "x@y.com"}, "", false},
		// All present aliases must agree and be strings.
		{"top-level-agree", map[string]any{"project_id": "p", "projectId": "p"}, "p", false},
		{"top-level-conflict", map[string]any{"project_id": "p", "projectId": "other"}, "", true},
		{"top-level-wrong-type", map[string]any{"project_id": float64(1)}, "", true},
		{"nested-wrong-type", map[string]any{"metadata": map[string]any{"project_id": float64(2)}}, "", true},
		{"nested-vs-top-conflict", map[string]any{"projectId": "other", "metadata": map[string]any{"project_id": "p"}}, "", true},
		// A nested container that is PRESENT but not an object is present-invalid, not absent —
		// even when the top-level project id is valid, a wrong-type container must fail closed
		// (the whole detail is untrustworthy) rather than being silently skipped.
		{"metadata-non-object", map[string]any{"project_id": "p", "metadata": float64(7)}, "", true},
		{"attributes-non-object", map[string]any{"project_id": "p", "attributes": float64(7)}, "", true},
		{"installed-non-object", map[string]any{"project_id": "p", "installed": float64(7)}, "", true},
		{"web-non-object", map[string]any{"project_id": "p", "web": float64(7)}, "", true},
		// A null (or absent) container is genuinely absent and is skipped, not an error.
		{"nested-null-skipped", map[string]any{"project_id": "p", "metadata": nil}, "p", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := keeperExplicitAntigravityProjectID(tc.detail)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error (conflict/wrong-type), got %q", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Fatalf("projectID = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestFetchAntigravityQuota exercises the api-call egress: it requires a project id, tries the
// candidate hosts in order (first non-2xx is skipped), and parses the inner body. It also
// asserts the outgoing api-call carries the antigravity UA + {"project":...} data.
func TestFetchAntigravityQuota(t *testing.T) {
	t.Setenv("CPA_HELPER_DATA_DIR", t.TempDir())
	var mu sync.Mutex
	var seenUA, seenData string
	callCount := 0
	cpa := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method != http.MethodPost || r.URL.Path != "/v0/management/api-call" {
			http.NotFound(w, r)
			return
		}
		var p struct {
			URL    string            `json:"url"`
			Header map[string]string `json:"header"`
			Data   string            `json:"data"`
		}
		_ = json.NewDecoder(r.Body).Decode(&p)
		mu.Lock()
		callCount++
		n := callCount
		seenUA = p.Header["User-Agent"]
		seenData = p.Data
		mu.Unlock()
		// First candidate host fails (404) to exercise the fallback; second succeeds.
		if n == 1 {
			_ = json.NewEncoder(w).Encode(map[string]any{"status_code": 404, "body": map[string]any{"error": "not found"}})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"status_code": 200, "body": antigravityGoldenMap(t)})
	}))
	defer cpa.Close()

	app, err := New()
	if err != nil {
		t.Fatalf("New(): %v", err)
	}
	defer app.Close()
	configureKeeperTestCPA(t, app, cpa.URL, nil)
	ctx := context.Background()

	cfg, err := app.loadConfig(ctx)
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	groups, ok := app.fetchAntigravityQuota(ctx, cfg, "idx-ag", "aicode-consumers")
	if !ok || len(groups) != 2 {
		t.Fatalf("fetch = (%v, %d groups), want ok with 2 groups", ok, len(groups))
	}
	mu.Lock()
	defer mu.Unlock()
	if callCount != 2 {
		t.Fatalf("call count = %d, want 2 (first host 404 → fallback)", callCount)
	}
	if seenUA != antigravityQuotaUserAgent {
		t.Fatalf("outgoing UA = %q, want antigravity UA", seenUA)
	}
	if seenData != `{"project":"aicode-consumers"}` {
		t.Fatalf("outgoing data = %q, want the project body", seenData)
	}
}

// TestKeeperInspectAntigravityAccount is the end-to-end path: an antigravity account is now
// INCLUDED in the inspection (not skipped), routed to the antigravity provider path, and stored
// with provider=antigravity + its quota groups — visible via listKeeperAccounts.
func TestKeeperInspectAntigravityAccount(t *testing.T) {
	t.Setenv("CPA_HELPER_DATA_DIR", t.TempDir())
	const authName = "antigravity-1.json"
	cpa := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v0/management/auth-files":
			_ = json.NewEncoder(w).Encode(map[string]any{"files": []map[string]any{
				{"name": authName, "type": "antigravity", "auth_index": "idx-ag"},
			}})
		case r.Method == http.MethodGet && r.URL.Path == "/v0/management/auth-files/download":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"name": authName, "type": "antigravity", "auth_index": "idx-ag",
				"project_id": "aicode-consumers", "email": "eyo@example.com", "disabled": false,
				"priority": 1, "access_token": "tok",
			})
		case r.Method == http.MethodPost && r.URL.Path == "/v0/management/api-call":
			_ = json.NewEncoder(w).Encode(map[string]any{"status_code": 200, "body": antigravityGoldenMap(t)})
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
		t.Fatalf("stats = %+v, want Healthy=1 (antigravity inspected, not skipped)", stats)
	}
	st, err := app.getKeeperState(ctx, authName)
	if err != nil {
		t.Fatalf("get state: %v", err)
	}
	if st.Provider == nil || *st.Provider != "antigravity" {
		t.Fatalf("stored provider = %v, want antigravity", st.Provider)
	}
	if len(st.AntigravityQuota) != 2 || st.AntigravityQuota[0].DisplayName != "Gemini Models" {
		t.Fatalf("stored antigravity quota not persisted: %+v", st.AntigravityQuota)
	}
	// Codex-only fields stay empty for an antigravity account.
	if st.PrimaryUsedPercent != nil || st.ResetCreditCount != nil || st.SubscriptionActiveUntil != nil {
		t.Fatalf("codex-only fields must be nil for antigravity: %+v", st)
	}
	// It appears in the account list the frontend consumes.
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
	if found == nil || found.Provider == nil || *found.Provider != "antigravity" || len(found.AntigravityQuota) != 2 {
		t.Fatalf("antigravity account not visible with quota in list: %+v", found)
	}
	// The API projection (the exact path the /accounts handler serializes) must carry both
	// fields — otherwise the frontend never sees the provider or quota.
	raw, err := json.Marshal(keeperAccountResponses(accounts, nil))
	if err != nil {
		t.Fatalf("marshal responses: %v", err)
	}
	js := string(raw)
	if !strings.Contains(js, `"provider":"antigravity"`) {
		t.Fatalf("API response drops provider: %s", js)
	}
	if !strings.Contains(js, `"antigravity_quota":[`) || !strings.Contains(js, `"remaining_fraction"`) || !strings.Contains(js, `"reset_at"`) {
		t.Fatalf("API response drops antigravity_quota buckets: %s", js)
	}
}

// TestKeeperInspectAntigravityIdentityConflictFailsClosed reproduces the reverse probes: when the
// download detail disagrees with the list entry on name/index, has drifted to type=codex, or when
// neither source carries an explicit auth_index, the inspection must fail closed — NO quota
// api-call, identity_error, and the prior quota snapshot preserved.
func TestKeeperInspectAntigravityIdentityConflictFailsClosed(t *testing.T) {
	const authName = "antigravity-x.json"
	cases := []struct {
		name      string
		listEntry map[string]any
		download  map[string]any
	}{
		{"name-only-mismatch",
			map[string]any{"name": authName, "type": "antigravity", "auth_index": "idx-ag", "project_id": "p"},
			map[string]any{"name": "other.json", "type": "antigravity", "auth_index": "idx-ag", "project_id": "p", "access_token": "t"}},
		{"index-only-mismatch",
			map[string]any{"name": authName, "type": "antigravity", "auth_index": "idx-ag", "project_id": "p"},
			map[string]any{"name": authName, "type": "antigravity", "auth_index": "idx-other", "project_id": "p", "access_token": "t"}},
		{"type-drift-to-codex",
			map[string]any{"name": authName, "type": "antigravity", "auth_index": "idx-ag"},
			map[string]any{"name": authName, "type": "codex", "auth_index": "idx-ag", "project_id": "p", "access_token": "t"}},
		{"no-explicit-auth-index",
			map[string]any{"name": authName, "type": "antigravity"},
			map[string]any{"name": authName, "type": "antigravity", "project_id": "p", "access_token": "t"}},
		{"project-id-conflict",
			map[string]any{"name": authName, "type": "antigravity", "auth_index": "idx-ag", "project_id": "project-A"},
			map[string]any{"name": authName, "type": "antigravity", "auth_index": "idx-ag", "project_id": "project-B", "access_token": "t"}},
		// present-but-wrong-type detail fields must fail closed (not be treated as absent + backfilled).
		{"detail-name-wrong-type",
			map[string]any{"name": authName, "type": "antigravity", "auth_index": "idx-ag", "project_id": "p"},
			map[string]any{"name": float64(123), "type": "antigravity", "auth_index": "idx-ag", "project_id": "p", "access_token": "t"}},
		{"detail-type-wrong-type",
			map[string]any{"name": authName, "type": "antigravity", "auth_index": "idx-ag", "project_id": "p"},
			map[string]any{"name": authName, "type": float64(1), "auth_index": "idx-ag", "project_id": "p", "access_token": "t"}},
		{"detail-project-wrong-type",
			map[string]any{"name": authName, "type": "antigravity", "auth_index": "idx-ag", "project_id": "p"},
			map[string]any{"name": authName, "type": "antigravity", "auth_index": "idx-ag", "project_id": float64(7), "access_token": "t"}},
		// project-id aliases (top-level + nested) must all agree and be strings.
		{"detail-project-alias-conflict",
			map[string]any{"name": authName, "type": "antigravity", "auth_index": "idx-ag", "project_id": "p"},
			map[string]any{"name": authName, "type": "antigravity", "auth_index": "idx-ag", "project_id": "p", "projectId": "other", "access_token": "t"}},
		{"detail-nested-project-wrong-type",
			map[string]any{"name": authName, "type": "antigravity", "auth_index": "idx-ag", "project_id": "p"},
			map[string]any{"name": authName, "type": "antigravity", "auth_index": "idx-ag", "metadata": map[string]any{"project_id": float64(5)}, "access_token": "t"}},
		{"detail-nested-vs-top-conflict",
			map[string]any{"name": authName, "type": "antigravity", "auth_index": "idx-ag", "project_id": "p"},
			map[string]any{"name": authName, "type": "antigravity", "auth_index": "idx-ag", "projectId": "other", "metadata": map[string]any{"project_id": "p"}, "access_token": "t"}},
		// email aliases (email/account_email/user_email) must agree and be strings, across sources too.
		{"email-alias-cross-conflict",
			map[string]any{"name": authName, "type": "antigravity", "auth_index": "idx-ag", "project_id": "p", "email": "a@x.com"},
			map[string]any{"name": authName, "type": "antigravity", "auth_index": "idx-ag", "project_id": "p", "account_email": "b@x.com", "access_token": "t"}},
		{"detail-email-alias-self-conflict",
			map[string]any{"name": authName, "type": "antigravity", "auth_index": "idx-ag", "project_id": "p"},
			map[string]any{"name": authName, "type": "antigravity", "auth_index": "idx-ag", "project_id": "p", "email": "a@x.com", "account_email": "b@x.com", "access_token": "t"}},
		{"detail-email-wrong-type",
			map[string]any{"name": authName, "type": "antigravity", "auth_index": "idx-ag", "project_id": "p"},
			map[string]any{"name": authName, "type": "antigravity", "auth_index": "idx-ag", "project_id": "p", "email": float64(9), "access_token": "t"}},
		// Missing email on BOTH sides: the resource key is provider+project+email and a project can be
		// shared, so an unbound (email-less) identity is unprovable and must fail closed — not degrade
		// to a project-only digest that a second email-less credential could inherit. Everything else
		// (name/type/index/project) is consistent, so the ONLY reason this fails is the missing email.
		{"missing-email-both-sides",
			map[string]any{"name": authName, "type": "antigravity", "auth_index": "idx-ag", "project_id": "p"},
			map[string]any{"name": authName, "type": "antigravity", "auth_index": "idx-ag", "project_id": "p", "access_token": "t"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("CPA_HELPER_DATA_DIR", t.TempDir())
			var mu sync.Mutex
			quotaCalls := 0
			cpa := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch {
				case r.Method == http.MethodGet && r.URL.Path == "/v0/management/auth-files":
					_ = json.NewEncoder(w).Encode(map[string]any{"files": []map[string]any{tc.listEntry}})
				case r.Method == http.MethodGet && r.URL.Path == "/v0/management/auth-files/download":
					_ = json.NewEncoder(w).Encode(tc.download)
				case r.Method == http.MethodPost && r.URL.Path == "/v0/management/api-call":
					var p struct {
						URL string `json:"url"`
					}
					_ = json.NewDecoder(r.Body).Decode(&p)
					if strings.Contains(p.URL, "retrieveUserQuotaSummary") {
						mu.Lock()
						quotaCalls++
						mu.Unlock()
					}
					_ = json.NewEncoder(w).Encode(map[string]any{"status_code": 200, "body": antigravityGoldenMap(t)})
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

			// Seed a prior good antigravity snapshot to prove it is preserved on the conflict.
			seed, _ := parseAntigravityQuotaGroups(antigravityGoldenMap(t))
			encoded, _ := json.Marshal(seed)
			blob := string(encoded)
			ag, idx := keeperProviderAntigravity, "idx-ag"
			if err := app.upsertKeeperState(ctx, keeperAccountResult{
				Name: authName, Result: "healthy", CheckedAt: time.Now(), Provider: &ag, AuthIndex: &idx, AntigravityQuota: &blob,
			}); err != nil {
				t.Fatalf("seed: %v", err)
			}

			stats, err := app.keeper.InspectAccountsLocked([]string{authName})
			if err != nil {
				t.Fatalf("InspectAccountsLocked: %v", err)
			}
			if stats.IdentityError != 1 || stats.Healthy != 0 {
				t.Fatalf("stats = %+v, want IdentityError=1, Healthy=0", stats)
			}
			mu.Lock()
			calls := quotaCalls
			mu.Unlock()
			if calls != 0 {
				t.Fatalf("identity conflict made %d quota api-calls; must be 0", calls)
			}
			st, err := app.getKeeperState(ctx, authName)
			if err != nil {
				t.Fatalf("get: %v", err)
			}
			if len(st.AntigravityQuota) != 2 {
				t.Fatalf("prior snapshot not preserved on conflict: %+v", st.AntigravityQuota)
			}
			if st.LastError == nil {
				t.Fatal("identity conflict did not record an error")
			}
		})
	}
}

// TestKeeperUpsertProviderSwitchClearsStaleFields proves the upsert enforces the provider
// invariant: switching a row codex→antigravity clears the stale Codex-only columns (account_id,
// reset credits, subscription, usage), and switching back antigravity→codex clears the stale
// antigravity_quota. So a filename reused for a different provider never shows mixed data.
func TestKeeperUpsertProviderSwitchClearsStaleFields(t *testing.T) {
	t.Setenv("CPA_HELPER_DATA_DIR", t.TempDir())
	app, err := New()
	if err != nil {
		t.Fatalf("New(): %v", err)
	}
	defer app.Close()
	ctx := context.Background()
	const authName = "switcher.json"

	// Seed a fully-populated Codex row.
	codex := keeperProviderCodex
	idx, used, count, acct, restore := "idx-1", 40, 3, "acct-A", 9
	sub := timeMust(t, "2026-10-01T00:00:00Z")
	if err := app.upsertKeeperState(ctx, keeperAccountResult{
		Name: authName, Result: "healthy", CheckedAt: timeMust(t, "2026-09-09T00:00:00Z"), Provider: &codex,
		AuthIndex: &idx, PrimaryUsedPercent: &used, ResetCreditCount: &count, ResetCredits: stringPtr(resetCreditSnapshotJSON),
		SubscriptionActiveUntil: &sub, SubscriptionKnown: true, AccountID: &acct, RestorePriority: &restore,
	}); err != nil {
		t.Fatalf("seed codex: %v", err)
	}
	if seeded, _ := app.getKeeperState(ctx, authName); seeded.RestorePriority == nil || *seeded.RestorePriority != 9 {
		t.Fatalf("seed restore_priority not stored: %v", seeded.RestorePriority)
	}

	// Same filename now inspected as antigravity.
	antigravity := keeperProviderAntigravity
	quota, _ := parseAntigravityQuotaGroups(antigravityGoldenMap(t))
	encoded, _ := json.Marshal(quota)
	blob := string(encoded)
	if err := app.upsertKeeperState(ctx, keeperAccountResult{
		Name: authName, Result: "healthy", CheckedAt: timeMust(t, "2026-09-09T01:00:00Z"), Provider: &antigravity,
		AuthIndex: &idx, AntigravityQuota: &blob,
	}); err != nil {
		t.Fatalf("upsert antigravity: %v", err)
	}
	st, err := app.getKeeperState(ctx, authName)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if st.Provider == nil || *st.Provider != "antigravity" || len(st.AntigravityQuota) != 2 {
		t.Fatalf("switch to antigravity: provider/quota = %v/%d", st.Provider, len(st.AntigravityQuota))
	}
	if st.PrimaryUsedPercent != nil || st.ResetCreditCount != nil || st.ResetCredits != nil || st.SubscriptionActiveUntil != nil || st.AccountID != nil {
		t.Fatalf("codex-only fields not cleared on provider switch: %+v", st.keeperAccount)
	}
	if st.RestorePriority != nil {
		t.Fatalf("codex restore_priority not cleared on provider switch: %v", st.RestorePriority)
	}

	// Switch back to codex → the antigravity quota must be cleared.
	if err := app.upsertKeeperState(ctx, keeperAccountResult{
		Name: authName, Result: "healthy", CheckedAt: timeMust(t, "2026-09-09T02:00:00Z"), Provider: &codex,
		AuthIndex: &idx, PrimaryUsedPercent: &used,
	}); err != nil {
		t.Fatalf("upsert codex again: %v", err)
	}
	st, err = app.getKeeperState(ctx, authName)
	if err != nil {
		t.Fatalf("get2: %v", err)
	}
	if st.Provider == nil || *st.Provider != "codex" || len(st.AntigravityQuota) != 0 {
		t.Fatalf("switch back to codex: provider=%v antigravity_quota len=%d (want cleared)", st.Provider, len(st.AntigravityQuota))
	}
}

// TestKeeperConditionalReconcilePreservesAntigravity proves the conditional reconcile's prune
// existence-set includes Antigravity: a stored Antigravity row that the remote list still
// returns must NOT be pruned (the bug deleted it because the set was codex-only).
func TestKeeperConditionalReconcilePreservesAntigravity(t *testing.T) {
	t.Setenv("CPA_HELPER_DATA_DIR", t.TempDir())
	const authName = "antigravity-keep.json"
	cpa := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet && r.URL.Path == "/v0/management/auth-files" {
			_ = json.NewEncoder(w).Encode(map[string]any{"files": []map[string]any{
				{"name": authName, "type": "antigravity", "auth_index": "idx-ag"},
			}})
			return
		}
		http.NotFound(w, r)
	}))
	defer cpa.Close()

	app, err := New()
	if err != nil {
		t.Fatalf("New(): %v", err)
	}
	defer app.Close()
	configureKeeperTestCPA(t, app, cpa.URL, nil)
	ctx := context.Background()

	ag, idx := keeperProviderAntigravity, "idx-ag"
	if err := app.upsertKeeperState(ctx, keeperAccountResult{Name: authName, Result: "healthy", CheckedAt: time.Now(), Provider: &ag, AuthIndex: &idx}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	cfg, err := app.loadConfig(ctx)
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if err := app.reconcileKeeperConditionalRemoteAuthStates(ctx, cfg, func(string) {}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if _, err := app.getKeeperState(ctx, authName); err != nil {
		t.Fatalf("conditional reconcile pruned a still-present antigravity row: %v", err)
	}
}

// TestKeeperWebsocketFailureClearsStaleAntigravityProvider reproduces the provider-switch probe on
// the credential-websocket failure path: a row stored as Antigravity whose remote file is now Codex
// and whose websocket-enable PATCH fails must still be re-tagged provider=codex (clearing the stale
// antigravity_quota), not left showing as Antigravity.
func TestKeeperWebsocketFailureClearsStaleAntigravityProvider(t *testing.T) {
	t.Setenv("CPA_HELPER_DATA_DIR", t.TempDir())
	const authName = "switched.json"
	cpa := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v0/management/auth-files":
			// The file is now a Codex account (websockets not yet enabled).
			_ = json.NewEncoder(w).Encode(map[string]any{"files": []map[string]any{
				{"name": authName, "type": "codex", "auth_index": "idx-c", "websockets": false},
			}})
		case r.Method == http.MethodPatch && r.URL.Path == "/v0/management/auth-files/fields":
			http.Error(w, "boom", http.StatusBadGateway) // enabling websockets fails
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
	configureKeeperTestCPA(t, app, cpa.URL, func(cfg *AppConfig) {
		cfg.CodexKeeper.EnableCredentialWebsockets = true
		cfg.CodexKeeper.DryRun = false
	})
	ctx := context.Background()

	// Seed a prior Antigravity row with quota (the file used to be antigravity).
	seed, _ := parseAntigravityQuotaGroups(antigravityGoldenMap(t))
	encoded, _ := json.Marshal(seed)
	blob := string(encoded)
	ag, idx := keeperProviderAntigravity, "idx-c"
	if err := app.upsertKeeperState(ctx, keeperAccountResult{
		Name: authName, Result: "healthy", CheckedAt: time.Now(), Provider: &ag, AuthIndex: &idx, AntigravityQuota: &blob,
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	stats, err := app.keeper.InspectAccountsLocked([]string{authName})
	if err != nil {
		t.Fatalf("InspectAccountsLocked: %v", err)
	}
	if stats.NetworkError != 1 {
		t.Fatalf("stats = %+v, want NetworkError=1 (websocket enable failed)", stats)
	}
	st, err := app.getKeeperState(ctx, authName)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if st.Provider == nil || *st.Provider != "codex" {
		t.Fatalf("stale provider not switched to codex on websocket-failure path: %v", st.Provider)
	}
	if len(st.AntigravityQuota) != 0 {
		t.Fatalf("stale antigravity_quota not cleared on provider switch: %+v", st.AntigravityQuota)
	}
}

// TestKeeperInspectAntigravityProjectSwapClearsStaleQuotaOnFailure reproduces the cross-inspection
// project-swap probe: a row stored for project-A whose list+detail now consistently say project-B
// (name/index unchanged) and whose project-B quota fetch FAILS must clear A's quota (a confirmed
// project swap), not keep showing A's quota against B.
func TestKeeperInspectAntigravityProjectSwapClearsStaleQuotaOnFailure(t *testing.T) {
	t.Setenv("CPA_HELPER_DATA_DIR", t.TempDir())
	const authName = "proj-swap.json"
	cpa := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v0/management/auth-files":
			_ = json.NewEncoder(w).Encode(map[string]any{"files": []map[string]any{
				{"name": authName, "type": "antigravity", "auth_index": "idx-ag", "project_id": "project-B", "email": "sw@example.com"},
			}})
		case r.Method == http.MethodGet && r.URL.Path == "/v0/management/auth-files/download":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"name": authName, "type": "antigravity", "auth_index": "idx-ag", "project_id": "project-B", "email": "sw@example.com", "access_token": "t",
			})
		case r.Method == http.MethodPost && r.URL.Path == "/v0/management/api-call":
			http.Error(w, "quota boom", http.StatusBadGateway) // project-B quota fetch fails
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

	// Seed project-A with its quota bound to project-A.
	seed, _ := parseAntigravityQuotaGroups(antigravityGoldenMap(t))
	encoded, _ := json.Marshal(seed)
	blob := string(encoded)
	ag, idx, projectADigest := keeperProviderAntigravity, "idx-ag", keeperAntigravityIdentityDigest("project-A", "sw@example.com")
	if err := app.upsertKeeperState(ctx, keeperAccountResult{
		Name: authName, Result: "healthy", CheckedAt: time.Now(), Provider: &ag, AuthIndex: &idx,
		AntigravityQuota: &blob, AntigravityIdentityDigest: &projectADigest,
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	if _, err := app.keeper.InspectAccountsLocked([]string{authName}); err != nil {
		t.Fatalf("InspectAccountsLocked: %v", err)
	}
	st, err := app.getKeeperState(ctx, authName)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if st.AntigravityIdentityDigest == nil || *st.AntigravityIdentityDigest != keeperAntigravityIdentityDigest("project-B", "sw@example.com") {
		t.Fatalf("project digest not rebound to project-B: %v", st.AntigravityIdentityDigest)
	}
	// The stored digest must NOT be the raw project id (privacy contract).
	if st.AntigravityIdentityDigest != nil && *st.AntigravityIdentityDigest == "project-B" {
		t.Fatalf("raw project id leaked into storage: %v", st.AntigravityIdentityDigest)
	}
	if len(st.AntigravityQuota) != 0 {
		t.Fatalf("stale project-A quota not cleared on project swap + failed fetch: %+v", st.AntigravityQuota)
	}
}

// TestKeeperInspectAntigravityEmailSwapClearsStaleQuotaOnFailure proves the identity binding also
// covers the account email: a shared Google project reused by a different email (name/index/project
// unchanged) whose new fetch fails must clear the old account's quota, not label it under the new
// email.
func TestKeeperInspectAntigravityEmailSwapClearsStaleQuotaOnFailure(t *testing.T) {
	t.Setenv("CPA_HELPER_DATA_DIR", t.TempDir())
	const authName = "email-swap.json"
	cpa := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v0/management/auth-files":
			_ = json.NewEncoder(w).Encode(map[string]any{"files": []map[string]any{
				{"name": authName, "type": "antigravity", "auth_index": "idx-ag", "project_id": "shared-proj", "email": "b@example.com"},
			}})
		case r.Method == http.MethodGet && r.URL.Path == "/v0/management/auth-files/download":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"name": authName, "type": "antigravity", "auth_index": "idx-ag", "project_id": "shared-proj", "email": "b@example.com", "access_token": "t",
			})
		case r.Method == http.MethodPost && r.URL.Path == "/v0/management/api-call":
			http.Error(w, "quota boom", http.StatusBadGateway) // new account's fetch fails
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

	// Seed the SAME project but a DIFFERENT email (a@example.com) with its quota.
	seed, _ := parseAntigravityQuotaGroups(antigravityGoldenMap(t))
	encoded, _ := json.Marshal(seed)
	blob := string(encoded)
	ag, idx, digestA := keeperProviderAntigravity, "idx-ag", keeperAntigravityIdentityDigest("shared-proj", "a@example.com")
	if err := app.upsertKeeperState(ctx, keeperAccountResult{
		Name: authName, Result: "healthy", CheckedAt: time.Now(), Provider: &ag, AuthIndex: &idx,
		AntigravityQuota: &blob, AntigravityIdentityDigest: &digestA,
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	if _, err := app.keeper.InspectAccountsLocked([]string{authName}); err != nil {
		t.Fatalf("InspectAccountsLocked: %v", err)
	}
	st, err := app.getKeeperState(ctx, authName)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if st.AntigravityIdentityDigest == nil || *st.AntigravityIdentityDigest != keeperAntigravityIdentityDigest("shared-proj", "b@example.com") {
		t.Fatalf("identity digest not rebound to the new email: %v", st.AntigravityIdentityDigest)
	}
	if len(st.AntigravityQuota) != 0 {
		t.Fatalf("stale quota kept under a different email (shared project): %+v", st.AntigravityQuota)
	}
}

// TestKeeperInspectAntigravityDetailFailurePreservesIdentityDigest reproduces the 3-phase probe: a
// transient detail-read failure (identity UNKNOWN) must NOT wipe the stored identity digest, or the
// next inspection loses its swap anchor and keeps the old account's quota. Phase 1 stores project-A
// + quota; phase 2 fails the download (list still A) — digest+quota preserved; phase 3 resolves
// project-B but the quota fetch fails — the A digest is still present so the swap is detected and
// A's quota is cleared (not shown under B).
func TestKeeperInspectAntigravityDetailFailurePreservesIdentityDigest(t *testing.T) {
	t.Setenv("CPA_HELPER_DATA_DIR", t.TempDir())
	const authName = "digest-preserve.json"
	const email = "a@x.com"
	var mu sync.Mutex
	project := "project-A"
	downloadFail := false
	quotaFail := false
	cpa := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		mu.Lock()
		curProject, dFail, qFail := project, downloadFail, quotaFail
		mu.Unlock()
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v0/management/auth-files":
			_ = json.NewEncoder(w).Encode(map[string]any{"files": []map[string]any{
				{"name": authName, "type": "antigravity", "auth_index": "idx-ag", "project_id": curProject, "email": email},
			}})
		case r.Method == http.MethodGet && r.URL.Path == "/v0/management/auth-files/download":
			if dFail {
				http.Error(w, "download boom", http.StatusBadGateway)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"name": authName, "type": "antigravity", "auth_index": "idx-ag", "project_id": curProject, "email": email, "access_token": "t",
			})
		case r.Method == http.MethodPost && r.URL.Path == "/v0/management/api-call":
			if qFail {
				http.Error(w, "quota boom", http.StatusBadGateway)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"status_code": 200, "body": antigravityGoldenMap(t)})
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

	inspect := func() {
		if _, err := app.keeper.InspectAccountsLocked([]string{authName}); err != nil {
			t.Fatalf("InspectAccountsLocked: %v", err)
		}
	}

	// Phase 1: project-A resolves + quota fetched.
	inspect()
	st, _ := app.getKeeperState(ctx, authName)
	if st.AntigravityIdentityDigest == nil || *st.AntigravityIdentityDigest != keeperAntigravityIdentityDigest("project-A", email) || len(st.AntigravityQuota) != 2 {
		t.Fatalf("phase1: digest/quota not stored: %v %d", st.AntigravityIdentityDigest, len(st.AntigravityQuota))
	}

	// Phase 2: download fails (identity unknown) — digest AND quota must be preserved.
	mu.Lock()
	downloadFail = true
	mu.Unlock()
	inspect()
	st, _ = app.getKeeperState(ctx, authName)
	if st.AntigravityIdentityDigest == nil || *st.AntigravityIdentityDigest != keeperAntigravityIdentityDigest("project-A", email) {
		t.Fatalf("phase2: identity digest was wiped on a transient detail failure: %v", st.AntigravityIdentityDigest)
	}
	if len(st.AntigravityQuota) != 2 {
		t.Fatalf("phase2: quota not preserved on transient failure: %d", len(st.AntigravityQuota))
	}

	// Phase 3: detail recovers as project-B but the quota fetch fails — swap detected, A quota cleared.
	mu.Lock()
	downloadFail = false
	project = "project-B"
	quotaFail = true
	mu.Unlock()
	inspect()
	st, _ = app.getKeeperState(ctx, authName)
	if st.AntigravityIdentityDigest == nil || *st.AntigravityIdentityDigest != keeperAntigravityIdentityDigest("project-B", email) {
		t.Fatalf("phase3: digest not rebound to project-B: %v", st.AntigravityIdentityDigest)
	}
	if len(st.AntigravityQuota) != 0 {
		t.Fatalf("phase3: stale project-A quota shown under project-B: %+v", st.AntigravityQuota)
	}
}

// TestKeeperUpsertLegacyUnboundQuotaClearedWhenIdentityKnown proves the digest-CASE fail-closed
// branch for a legacy/unbound snapshot: a stored antigravity_quota whose identity digest is NULL
// (a pre-digest row, or a snapshot never bound to an identity) has no provable owner. When a new
// inspection resolves a KNOWN identity but its quota fetch FAILS (incoming quota NULL), the unbound
// quota must be CLEARED rather than COALESCE-preserved and inherited by the now-known identity. The
// same-identity transient failure (preserve-on-unknown) is covered by the 3-phase test above.
func TestKeeperUpsertLegacyUnboundQuotaClearedWhenIdentityKnown(t *testing.T) {
	t.Setenv("CPA_HELPER_DATA_DIR", t.TempDir())
	app, err := New()
	if err != nil {
		t.Fatalf("New(): %v", err)
	}
	defer app.Close()
	ctx := context.Background()
	const authName = "legacy-unbound.json"

	// Seed a legacy row: quota present, identity digest NULL (never bound to an identity).
	seed, _ := parseAntigravityQuotaGroups(antigravityGoldenMap(t))
	encoded, _ := json.Marshal(seed)
	blob := string(encoded)
	ag, idx := keeperProviderAntigravity, "idx-ag"
	if err := app.upsertKeeperState(ctx, keeperAccountResult{
		Name: authName, Result: "healthy", CheckedAt: time.Now(), Provider: &ag, AuthIndex: &idx,
		AntigravityQuota: &blob, // AntigravityIdentityDigest left nil (unbound)
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if seeded, _ := app.getKeeperState(ctx, authName); seeded.AntigravityIdentityDigest != nil || len(seeded.AntigravityQuota) != 2 {
		t.Fatalf("seed precondition: digest=%v quota=%d (want NULL digest, 2 groups)", seeded.AntigravityIdentityDigest, len(seeded.AntigravityQuota))
	}

	// New inspection: identity now KNOWN, but the quota fetch failed (incoming quota NULL).
	digest := keeperAntigravityIdentityDigest("proj", "e@x.com")
	if err := app.upsertKeeperState(ctx, keeperAccountResult{
		Name: authName, Result: "network_error", CheckedAt: time.Now(), Provider: &ag, AuthIndex: &idx,
		AntigravityIdentityDigest: &digest, // AntigravityQuota nil (fetch failed)
	}); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	st, err := app.getKeeperState(ctx, authName)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if st.AntigravityIdentityDigest == nil || *st.AntigravityIdentityDigest != digest {
		t.Fatalf("identity digest not bound: %v", st.AntigravityIdentityDigest)
	}
	if len(st.AntigravityQuota) != 0 {
		t.Fatalf("unbound legacy quota not cleared once identity known + fetch failed: %+v", st.AntigravityQuota)
	}
}

// TestKeeperCachedAntigravityPriorityNotDegraded proves the cache-audit stat entry
// (keeperCachedAuthStats → mergeCachedState, used by conditional-refresh / cache-skip accounting)
// does NOT count an Antigravity account at priority -1 as PriorityDegraded: the Codex
// quota-usage→priority=-1 "degraded" semantic does not apply to Antigravity, so it falls through to
// healthy. A Codex control row at priority -1 IS still counted as degraded, proving the provider
// guard is load-bearing rather than blanket-suppressing the branch.
func TestKeeperCachedAntigravityPriorityNotDegraded(t *testing.T) {
	t.Setenv("CPA_HELPER_DATA_DIR", t.TempDir())
	app, err := New()
	if err != nil {
		t.Fatalf("New(): %v", err)
	}
	defer app.Close()
	ctx := context.Background()

	minusOne := -1
	ag, codex := keeperProviderAntigravity, keeperProviderCodex
	agIdx, cIdx := "idx-ag", "idx-c"
	if err := app.upsertKeeperState(ctx, keeperAccountResult{
		Name: "ag.json", Result: "healthy", CheckedAt: time.Now(), Provider: &ag, AuthIndex: &agIdx, Priority: &minusOne,
	}); err != nil {
		t.Fatalf("seed antigravity: %v", err)
	}
	if err := app.upsertKeeperState(ctx, keeperAccountResult{
		Name: "codex.json", Result: "healthy", CheckedAt: time.Now(), Provider: &codex, AuthIndex: &cIdx, Priority: &minusOne,
	}); err != nil {
		t.Fatalf("seed codex: %v", err)
	}

	// Antigravity at priority -1: falls through to healthy, NOT degraded.
	agStats, err := app.keeperCachedAuthStats(ctx, []string{"ag.json"})
	if err != nil {
		t.Fatalf("ag stats: %v", err)
	}
	if agStats.PriorityDegraded != 0 || agStats.Healthy != 1 {
		t.Fatalf("antigravity priority=-1 miscounted: %+v (want PriorityDegraded=0, Healthy=1)", agStats)
	}

	// Codex control at priority -1: still counted as degraded (the guard is provider-specific).
	cStats, err := app.keeperCachedAuthStats(ctx, []string{"codex.json"})
	if err != nil {
		t.Fatalf("codex stats: %v", err)
	}
	if cStats.PriorityDegraded != 1 || cStats.Healthy != 0 {
		t.Fatalf("codex priority=-1 should be degraded: %+v (want PriorityDegraded=1, Healthy=0)", cStats)
	}
}

func timeMust(t *testing.T, s string) time.Time {
	t.Helper()
	parsed, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatalf("parse time %q: %v", s, err)
	}
	return parsed
}

// TestFetchAntigravityQuotaNoProject proves a missing project id fails closed with no remote call.
func TestFetchAntigravityQuotaNoProject(t *testing.T) {
	t.Setenv("CPA_HELPER_DATA_DIR", t.TempDir())
	var mu sync.Mutex
	calls := 0
	cpa := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls++
		mu.Unlock()
		http.NotFound(w, r)
	}))
	defer cpa.Close()
	app, err := New()
	if err != nil {
		t.Fatalf("New(): %v", err)
	}
	defer app.Close()
	configureKeeperTestCPA(t, app, cpa.URL, nil)
	cfg, err := app.loadConfig(context.Background())
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if _, ok := app.fetchAntigravityQuota(context.Background(), cfg, "idx", ""); ok {
		t.Fatal("missing project_id must fail closed")
	}
	mu.Lock()
	defer mu.Unlock()
	if calls != 0 {
		t.Fatalf("no project_id made %d remote calls; want 0", calls)
	}
}

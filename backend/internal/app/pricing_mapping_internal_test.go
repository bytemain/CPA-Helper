package app

import (
	"context"
	"testing"
)

// mappingRule is a terser literal for the table tests below.
func mappingRule(sourceProvider, sourceModel, targetProvider, targetModel string) ModelPriceMappingRule {
	return ModelPriceMappingRule{
		SourceProvider: sourceProvider,
		SourceModel:    sourceModel,
		TargetProvider: targetProvider,
		TargetModel:    targetModel,
	}
}

func mappedInput(t *testing.T, prices map[[2]string]ModelPrice, rules []ModelPriceMappingRule, provider, model string) (float64, bool) {
	t.Helper()
	book := priceBook{prices: prices, rules: rules}
	price := book.find(&provider, &model)
	if price == nil {
		return 0, false
	}
	return price.InputUSDPerMillion, true
}

func TestPriceBookExactPriceBeatsMappingRule(t *testing.T) {
	// The dictionary knows the reported model itself AND a rule would map it elsewhere; the
	// manually created exact row must win so an operator can always override a rule.
	prices := priceTable(
		[3]any{"devin", "swe-2", 7.0},
		[3]any{"moonshot", "moonshot/kimi-k3", 1.0},
	)
	rules := []ModelPriceMappingRule{mappingRule("devin", "swe-2", "moonshot", "moonshot/kimi-k3")}
	if got, ok := mappedInput(t, prices, rules, "devin", "swe-2"); !ok || got != 7 {
		t.Fatalf("exact price must win over the mapping rule, got %v,%v", got, ok)
	}
	// Remove the exact row and the rule takes over.
	delete(prices, priceKey("devin", "swe-2"))
	if got, ok := mappedInput(t, prices, rules, "devin", "swe-2"); !ok || got != 1 {
		t.Fatalf("mapping rule should price the record, got %v,%v", got, ok)
	}
}

func TestPriceBookMapsExactAndWildcardSources(t *testing.T) {
	prices := priceTable(
		[3]any{"moonshot", "moonshot/kimi-k3", 1.0},
		[3]any{"cognition", "swe-2", 2.0},
		[3]any{"cognition", "swe-9", 3.0},
	)
	cases := []struct {
		name     string
		rules    []ModelPriceMappingRule
		provider string
		model    string
		want     float64
		wantOK   bool
	}{
		{
			name:     "exact source maps to the litellm-keyed target",
			rules:    []ModelPriceMappingRule{mappingRule("devin", "swe-2", "moonshot", "moonshot/kimi-k3")},
			provider: "devin", model: "swe-2", want: 1, wantOK: true,
		},
		{
			name:     "provider-less rule matches any provider",
			rules:    []ModelPriceMappingRule{mappingRule("", "swe-2", "moonshot", "moonshot/kimi-k3")},
			provider: "someproxy", model: "swe-2", want: 1, wantOK: true,
		},
		{
			name:     "source provider that does not match leaves the record unpriced",
			rules:    []ModelPriceMappingRule{mappingRule("devin", "swe-2", "moonshot", "moonshot/kimi-k3")},
			provider: "someproxy", model: "swe-2", wantOK: false,
		},
		{
			name:     "wildcard capture is substituted into the target",
			rules:    []ModelPriceMappingRule{mappingRule("devin", "swe-*", "cognition", "swe-*")},
			provider: "devin", model: "swe-9", want: 3, wantOK: true,
		},
		{
			name:     "wildcard target without a capture stays literal when source is exact",
			rules:    []ModelPriceMappingRule{mappingRule("devin", "swe-x", "cognition", "swe-2")},
			provider: "devin", model: "swe-x", want: 2, wantOK: true,
		},
		{
			name:     "wildcard does not match an empty capture",
			rules:    []ModelPriceMappingRule{mappingRule("devin", "swe-*", "cognition", "swe-2")},
			provider: "devin", model: "swe-", wantOK: false,
		},
		{
			name:     "casing and surrounding whitespace are normalized on both sides",
			rules:    []ModelPriceMappingRule{mappingRule(" Devin ", " SWE-2 ", " MoonShot ", " MoonShot/Kimi-K3 ")},
			provider: " DEVIN ", model: " Swe-2 ", want: 1, wantOK: true,
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			rules := sanitizeModelPriceMappingRules(testCase.rules)
			got, ok := mappedInput(t, prices, rules, testCase.provider, testCase.model)
			if ok != testCase.wantOK || (testCase.wantOK && got != testCase.want) {
				t.Fatalf("got %v,%v; want %v,%v", got, ok, testCase.want, testCase.wantOK)
			}
		})
	}
}

func TestMatchModelPriceMappingRulePrecedence(t *testing.T) {
	cases := []struct {
		name           string
		rules          []ModelPriceMappingRule
		provider       string
		model          string
		wantProvider   string
		wantModel      string
		wantNoMatching bool
	}{
		{
			name: "exact source beats a wildcard rule declared first",
			rules: []ModelPriceMappingRule{
				mappingRule("", "swe-*", "wildcard", "wildcard-model"),
				mappingRule("", "swe-2", "exact", "exact-model"),
			},
			provider: "devin", model: "swe-2",
			wantProvider: "exact", wantModel: "exact-model",
		},
		{
			name: "longer literal wins between two wildcard rules",
			rules: []ModelPriceMappingRule{
				mappingRule("", "s*", "short", "short-model"),
				mappingRule("", "swe-2-*", "long", "long-model"),
			},
			provider: "devin", model: "swe-2-pro",
			wantProvider: "long", wantModel: "long-model",
		},
		{
			name: "provider-qualified beats an otherwise equal provider-less rule",
			rules: []ModelPriceMappingRule{
				mappingRule("", "swe-*", "anyprovider", "any-model"),
				mappingRule("devin", "swe-*", "devinonly", "devin-model"),
			},
			provider: "devin", model: "swe-2",
			wantProvider: "devinonly", wantModel: "devin-model",
		},
		{
			name: "otherwise equal rules fall back to list order",
			rules: []ModelPriceMappingRule{
				mappingRule("", "swe-*", "first", "first-model"),
				mappingRule("", "sw*2", "second", "second-model"),
			},
			provider: "devin", model: "swe-2",
			wantProvider: "first", wantModel: "first-model",
		},
		{
			name: "capture is taken from the winning rule",
			rules: []ModelPriceMappingRule{
				mappingRule("", "swe-*", "cognition", "swe-*"),
			},
			provider: "devin", model: "swe-2-pro",
			wantProvider: "cognition", wantModel: "swe-2-pro",
		},
		{
			name:           "no rule matches",
			rules:          []ModelPriceMappingRule{mappingRule("", "swe-*", "cognition", "swe-*")},
			provider:       "devin",
			model:          "kimi-k3",
			wantNoMatching: true,
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			rules, err := validateModelPriceMappingRules(testCase.rules)
			if err != nil {
				t.Fatalf("rules must be valid: %v", err)
			}
			gotProvider, gotModel, ok := matchModelPriceMappingRule(rules, testCase.provider, testCase.model)
			if testCase.wantNoMatching {
				if ok {
					t.Fatalf("expected no match, got %s/%s", gotProvider, gotModel)
				}
				return
			}
			if !ok || gotProvider != testCase.wantProvider || gotModel != testCase.wantModel {
				t.Fatalf("mapped to %s/%s (ok=%v); want %s/%s", gotProvider, gotModel, ok, testCase.wantProvider, testCase.wantModel)
			}
		})
	}
}

func TestPriceBookMappingNeverChains(t *testing.T) {
	// A→B and B→C: an A record must be priced by B, never by C.
	prices := priceTable(
		[3]any{"bprovider", "b-model", 2.0},
		[3]any{"cprovider", "c-model", 3.0},
	)
	rules, err := validateModelPriceMappingRules([]ModelPriceMappingRule{
		mappingRule("aprovider", "a-model", "bprovider", "b-model"),
		mappingRule("bprovider", "b-model", "cprovider", "c-model"),
	})
	if err != nil {
		t.Fatalf("rules must be valid: %v", err)
	}
	if got, ok := mappedInput(t, prices, rules, "aprovider", "a-model"); !ok || got != 2 {
		t.Fatalf("A must be priced by B (2.0), got %v,%v", got, ok)
	}
	// Even when B itself has no price, A does NOT fall through to C.
	delete(prices, priceKey("bprovider", "b-model"))
	if got, ok := mappedInput(t, prices, rules, "aprovider", "a-model"); ok {
		t.Fatalf("A must stay unpriced when B has no price, got %v", got)
	}
}

func TestPriceBookKeepsAssociationFallbacksAndEmptyRulesBehaviour(t *testing.T) {
	// Regression guard: with no rules the resolver must behave exactly like findMatchingPrice,
	// including the PR #16 provider/model association fallbacks.
	prices := priceTable(
		[3]any{"gemini", "gemini/gemini-3.8-flash", 0.3},
		[3]any{"openai", "gpt-5.2", 1.25},
	)
	for _, testCase := range []struct {
		provider string
		model    string
	}{
		{"antigravity", "gemini-3.8-flash-high"},
		{"gemini-cli", "gemini-3.8-flash"},
		{"codex", "gpt-5.2"},
		{"openai", "gpt-5.2"},
		{"kimi", "gpt-5.2"},
		{"unknown", "mystery"},
	} {
		provider, model := testCase.provider, testCase.model
		want := findMatchingPrice(prices, &provider, &model)
		got := priceBook{prices: prices}.find(&provider, &model)
		if (want == nil) != (got == nil) || (want != nil && want.InputUSDPerMillion != got.InputUSDPerMillion) {
			t.Fatalf("%s/%s: empty rules changed the result (%#v vs %#v)", provider, model, got, want)
		}
	}
	// The association fallbacks also apply to the MAPPED target: the rule points at the vendor
	// provider while the dictionary stores the LiteLLM-style `gemini/...` key.
	rules := []ModelPriceMappingRule{mappingRule("someproxy", "flash-fast", "gemini", "gemini-3.8-flash")}
	if got, ok := mappedInput(t, prices, rules, "someproxy", "flash-fast"); !ok || got != 0.3 {
		t.Fatalf("mapped target must use the existing lookup path, got %v,%v", got, ok)
	}
}

func TestPriceBookFindIgnoresBlankInputs(t *testing.T) {
	book := priceBook{
		prices: priceTable([3]any{"moonshot", "moonshot/kimi-k3", 1.0}),
		rules:  []ModelPriceMappingRule{mappingRule("", "*", "moonshot", "moonshot/kimi-k3")},
	}
	blank := "   "
	model := "swe-2"
	if price := book.find(nil, &model); price != nil {
		t.Fatal("nil provider must not match")
	}
	if price := book.find(&blank, &model); price != nil {
		t.Fatal("blank provider must not match")
	}
	if price := book.find(&model, &blank); price != nil {
		t.Fatal("blank model must not match")
	}
}

func TestValidateModelPriceMappingRules(t *testing.T) {
	long := ""
	for i := 0; i < maxModelPriceMappingFieldLength+1; i++ {
		long += "a"
	}
	cases := []struct {
		name  string
		rules []ModelPriceMappingRule
		want  string
	}{
		{
			name:  "empty source model",
			rules: []ModelPriceMappingRule{mappingRule("devin", "  ", "moonshot", "moonshot/kimi-k3")},
			want:  "模型价格映射规则 #1 的 source_model 不能为空",
		},
		{
			name: "empty target provider",
			rules: []ModelPriceMappingRule{
				mappingRule("devin", "swe-2", "moonshot", "moonshot/kimi-k3"),
				mappingRule("devin", "swe-3", "", "moonshot/kimi-k3"),
			},
			want: "模型价格映射规则 #2 的 target_provider 不能为空",
		},
		{
			name:  "empty target model",
			rules: []ModelPriceMappingRule{mappingRule("devin", "swe-2", "moonshot", "")},
			want:  "模型价格映射规则 #1 的 target_model 不能为空",
		},
		{
			name:  "two wildcards in source",
			rules: []ModelPriceMappingRule{mappingRule("devin", "swe-*-*", "moonshot", "moonshot/kimi-k3")},
			want:  "模型价格映射规则 #1 的 source_model 最多只能包含一个 *",
		},
		{
			name:  "two wildcards in target",
			rules: []ModelPriceMappingRule{mappingRule("devin", "swe-*", "moonshot", "kimi-*-*")},
			want:  "模型价格映射规则 #1 的 target_model 最多只能包含一个 *",
		},
		{
			name:  "wildcard in target without one in source",
			rules: []ModelPriceMappingRule{mappingRule("devin", "swe-2", "moonshot", "kimi-*")},
			want:  "模型价格映射规则 #1 的 target_model 含有 *，但 source_model 没有 *",
		},
		{
			name:  "wildcard in source provider",
			rules: []ModelPriceMappingRule{mappingRule("dev*", "swe-2", "moonshot", "moonshot/kimi-k3")},
			want:  "模型价格映射规则 #1 的 source_provider 不能包含 *",
		},
		{
			name:  "wildcard in target provider",
			rules: []ModelPriceMappingRule{mappingRule("devin", "swe-2", "moon*", "moonshot/kimi-k3")},
			want:  "模型价格映射规则 #1 的 target_provider 不能包含 *",
		},
		{
			name: "duplicate source pair",
			rules: []ModelPriceMappingRule{
				mappingRule("devin", "swe-2", "moonshot", "moonshot/kimi-k3"),
				mappingRule("DEVIN", " swe-2 ", "cognition", "swe-2"),
			},
			want: "模型价格映射规则 #2 与前面的规则重复（source_provider + source_model 相同）",
		},
		{
			name:  "field too long",
			rules: []ModelPriceMappingRule{mappingRule("devin", long, "moonshot", "moonshot/kimi-k3")},
			want:  "模型价格映射规则 #1 的 source_model 超出最大长度 180",
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if _, err := validateModelPriceMappingRules(testCase.rules); err == nil || err.Error() != testCase.want {
				t.Fatalf("error = %v, want %q", err, testCase.want)
			}
		})
	}

	tooMany := make([]ModelPriceMappingRule, 0, maxModelPriceMappingRules+1)
	for i := 0; i <= maxModelPriceMappingRules; i++ {
		tooMany = append(tooMany, mappingRule("devin", "swe-"+string(rune('a'+i%26))+string(rune('a'+i/26)), "moonshot", "moonshot/kimi-k3"))
	}
	if _, err := validateModelPriceMappingRules(tooMany); err == nil || err.Error() != "模型价格映射规则最多 50 条" {
		t.Fatalf("error = %v, want the 50-rule limit", err)
	}

	// The same source model with different providers is NOT a duplicate.
	valid, err := validateModelPriceMappingRules([]ModelPriceMappingRule{
		mappingRule("devin", "swe-2", "moonshot", "moonshot/kimi-k3"),
		mappingRule("", "swe-2", "cognition", "swe-2"),
	})
	if err != nil || len(valid) != 2 {
		t.Fatalf("provider-distinct rules must be accepted: %v (%d rules)", err, len(valid))
	}
	empty, err := validateModelPriceMappingRules(nil)
	if err != nil || empty == nil || len(empty) != 0 {
		t.Fatalf("nil rules must normalize to an empty (non-nil) list: %#v, %v", empty, err)
	}
}

func TestSanitizeModelPriceMappingRulesDropsMalformedStoredRules(t *testing.T) {
	got := sanitizeModelPriceMappingRules([]ModelPriceMappingRule{
		mappingRule("Devin", " SWE-2 ", "MoonShot", "MoonShot/Kimi-K3"),
		mappingRule("devin", "bad-*-*", "moonshot", "moonshot/kimi-k3"), // two wildcards
		mappingRule("devin", "swe-3", "", "moonshot/kimi-k3"),           // no target provider
		mappingRule("devin", "swe-2", "cognition", "swe-2"),             // duplicate source pair
	})
	if len(got) != 1 {
		t.Fatalf("sanitized rules = %#v, want only the first rule", got)
	}
	if got[0] != mappingRule("devin", "swe-2", "moonshot", "moonshot/kimi-k3") {
		t.Fatalf("sanitized rule = %#v, want normalized lower-cased fields", got[0])
	}
}

// TestMappedRecordWithoutTargetPriceStaysUnpriced is the money-path guarantee: a rule that maps to
// a target with no price row must leave the record UNPRICED (counted in unpriced_records), never a
// silent $0 charge that would deduct nothing while looking priced.
func TestMappedRecordWithoutTargetPriceStaysUnpriced(t *testing.T) {
	t.Setenv("CPA_HELPER_DATA_DIR", t.TempDir())
	app, err := New()
	if err != nil {
		t.Fatalf("New() failed: %v", err)
	}
	defer app.Close()

	ctx := context.Background()
	userID := seedQuotaTestUser(t, app, "member")
	apiKey := "sk-mapping-unpriced"
	seedQuotaTestAPIKey(t, app, userID, apiKey)
	lifetime := 5.0
	if _, err := app.updateUserQuota(ctx, userID, userQuotaPayload{LifetimeQuotaUSD: &lifetime}); err != nil {
		t.Fatalf("update quota: %v", err)
	}
	seedMappingRulesForTest(t, app, mappingRule("devin", "swe-2", "moonshot", "moonshot/kimi-k3"))

	raw := `{"api_key":"` + apiKey + `","provider":"devin","model":"swe-2","input_tokens":1000000,"request_id":"mapping-unpriced"}`
	if _, created, err := app.saveUsageMessage(ctx, []byte(raw)); err != nil || !created {
		t.Fatalf("usage created=%v err=%v", created, err)
	}
	user, err := app.getUser(ctx, userID)
	if err != nil {
		t.Fatal(err)
	}
	if user.QuotaUnpricedRecords != 1 {
		t.Fatalf("unpriced records = %d, want 1", user.QuotaUnpricedRecords)
	}
	if user.QuotaLifetimeUSD == nil || *user.QuotaLifetimeUSD != 5 {
		t.Fatalf("balance = %v, want 5 (nothing deducted)", user.QuotaLifetimeUSD)
	}
	var amount float64
	var unpriced bool
	if err := app.db.QueryRow(`SELECT amount_usd, unpriced FROM user_quota_charges`).Scan(&amount, &unpriced); err != nil {
		t.Fatal(err)
	}
	if amount != 0 || !unpriced {
		t.Fatalf("charge amount=%v unpriced=%v, want 0 true (never a silent $0 priced charge)", amount, unpriced)
	}

	// Once the target price exists the very same rule prices the next record.
	seedQuotaTestPrice(t, app, "moonshot", "moonshot/kimi-k3", 2)
	raw2 := `{"api_key":"` + apiKey + `","provider":"devin","model":"swe-2","input_tokens":1000000,"request_id":"mapping-priced"}`
	if _, created, err := app.saveUsageMessage(ctx, []byte(raw2)); err != nil || !created {
		t.Fatalf("second usage created=%v err=%v", created, err)
	}
	user, err = app.getUser(ctx, userID)
	if err != nil {
		t.Fatal(err)
	}
	if user.QuotaUnpricedRecords != 1 {
		t.Fatalf("unpriced records = %d, want 1 (the second record is priced)", user.QuotaUnpricedRecords)
	}
	if user.QuotaLifetimeUSD == nil || *user.QuotaLifetimeUSD != 3 {
		t.Fatalf("balance = %v, want 3 (2.00 charged through the mapping rule)", user.QuotaLifetimeUSD)
	}
}

// TestMappedRecordChargesThroughRule pins the end-to-end happy path through saveUsageMessage.
func TestMappedRecordChargesThroughRule(t *testing.T) {
	t.Setenv("CPA_HELPER_DATA_DIR", t.TempDir())
	app, err := New()
	if err != nil {
		t.Fatalf("New() failed: %v", err)
	}
	defer app.Close()

	ctx := context.Background()
	userID := seedQuotaTestUser(t, app, "member")
	apiKey := "sk-mapping-wildcard"
	seedQuotaTestAPIKey(t, app, userID, apiKey)
	lifetime := 10.0
	if _, err := app.updateUserQuota(ctx, userID, userQuotaPayload{LifetimeQuotaUSD: &lifetime}); err != nil {
		t.Fatalf("update quota: %v", err)
	}
	seedQuotaTestPrice(t, app, "cognition", "swe-9", 4)
	seedMappingRulesForTest(t, app, mappingRule("devin", "swe-*", "cognition", "swe-*"))

	raw := `{"api_key":"` + apiKey + `","provider":"devin","model":"swe-9","input_tokens":1000000,"request_id":"mapping-wildcard"}`
	if _, created, err := app.saveUsageMessage(ctx, []byte(raw)); err != nil || !created {
		t.Fatalf("usage created=%v err=%v", created, err)
	}
	user, err := app.getUser(ctx, userID)
	if err != nil {
		t.Fatal(err)
	}
	if user.QuotaUnpricedRecords != 0 || user.QuotaLifetimeUSD == nil || *user.QuotaLifetimeUSD != 6 {
		t.Fatalf("balance = %v unpriced = %d, want 6 and 0", user.QuotaLifetimeUSD, user.QuotaUnpricedRecords)
	}
}

func seedMappingRulesForTest(t *testing.T, app *App, rules ...ModelPriceMappingRule) {
	t.Helper()
	ctx := context.Background()
	cfg, err := app.loadConfig(ctx)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	cfg.ModelPriceMappingRules = rules
	if err := app.saveConfig(ctx, cfg); err != nil {
		t.Fatalf("save config: %v", err)
	}
}

// TestPriceBookRuleSourceIsTheStoredModelLiteral pins the production shape of the reported
// model, which is the trap this feature exists to walk into: for the reverse-proxied Devin
// traffic the stored usage row is provider "devin" with model "devin/swe-2" — the provider
// prefix is part of the model literal, not something the engine strips or infers. A rule
// written against the bare suffix therefore matches NOTHING, which would ship as a feature
// that silently prices nothing at all. Both halves are load-bearing: drop the prefix from the
// rule and the record must stay unpriced rather than quietly resolving.
func TestPriceBookRuleSourceIsTheStoredModelLiteral(t *testing.T) {
	prices := priceTable(
		[3]any{"moonshot", "moonshot/kimi-k3", 3.0},
		[3]any{"cognition", "cognition/swe-1.7", 5.0},
	)
	const storedModel = "devin/swe-2" // exactly what prod's usage_records holds

	// The rule must carry the same literal the row carries.
	withPrefix := []ModelPriceMappingRule{mappingRule("devin", storedModel, "moonshot", "moonshot/kimi-k3")}
	if got, ok := mappedInput(t, prices, withPrefix, "devin", storedModel); !ok || got != 3 {
		t.Fatalf("rule written with the stored literal must price the record, got %v,%v", got, ok)
	}

	// The same rule written against the bare suffix must NOT match: the engine matches the
	// stored value and never guesses a provider prefix.
	bare := []ModelPriceMappingRule{mappingRule("devin", "swe-2", "moonshot", "moonshot/kimi-k3")}
	if got, ok := mappedInput(t, prices, bare, "devin", storedModel); ok {
		t.Fatalf("a rule missing the %q prefix must not match the stored model, got %v", "devin/", got)
	}

	// Wildcards carry the prefix too, and an exact rule still beats the wildcard so the
	// wildcard's dead target (cognition/swe-2 is absent from the dictionary) cannot capture it.
	both := []ModelPriceMappingRule{
		mappingRule("devin", "devin/swe-*", "cognition", "cognition/swe-*"),
		mappingRule("devin", storedModel, "moonshot", "moonshot/kimi-k3"),
	}
	if got, ok := mappedInput(t, prices, both, "devin", storedModel); !ok || got != 3 {
		t.Fatalf("exact rule must beat the wildcard whose target does not exist, got %v,%v", got, ok)
	}
	// And the wildcard alone leaves it unpriced rather than inventing a price.
	wildcardOnly := []ModelPriceMappingRule{mappingRule("devin", "devin/swe-*", "cognition", "cognition/swe-*")}
	if got, ok := mappedInput(t, prices, wildcardOnly, "devin", storedModel); ok {
		t.Fatalf("wildcard mapping to an absent target must stay unpriced, got %v", got)
	}
	// The same wildcard does price a model whose mapped target exists.
	if got, ok := mappedInput(t, prices, wildcardOnly, "devin", "devin/swe-1.7"); !ok || got != 5 {
		t.Fatalf("wildcard must price when the mapped target exists, got %v,%v", got, ok)
	}
}

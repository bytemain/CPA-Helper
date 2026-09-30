package app_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	backendApp "cpa-helper/backend/internal/app"
)

type settingsMappingRule struct {
	SourceProvider string `json:"source_provider"`
	SourceModel    string `json:"source_model"`
	TargetProvider string `json:"target_provider"`
	TargetModel    string `json:"target_model"`
}

type settingsMappingResponse struct {
	ModelPriceMappingRules []settingsMappingRule `json:"model_price_mapping_rules"`
}

func newMappingSettingsTestApp(t *testing.T) (http.Handler, []*http.Cookie, func()) {
	t.Helper()
	t.Setenv("CPA_HELPER_DATA_DIR", t.TempDir())
	app, err := backendApp.New()
	if err != nil {
		t.Fatalf("New() failed: %v", err)
	}
	handler := app.Routes()
	cookies := requestJSON(t, handler, http.MethodPost, "/api/auth/setup", map[string]any{
		"username": "admin", "password": "test-password", "nickname": "Admin",
	}, nil, nil)
	return handler, cookies, app.Close
}

func storedMappingRules(t *testing.T, handler http.Handler, cookies []*http.Cookie) []settingsMappingRule {
	t.Helper()
	var settings settingsMappingResponse
	requestJSON(t, handler, http.MethodGet, "/api/settings", nil, cookies, &settings)
	if settings.ModelPriceMappingRules == nil {
		t.Fatal("model_price_mapping_rules decoded as nil; want a JSON array")
	}
	return settings.ModelPriceMappingRules
}

// putSettingsExpectStatus returns the response body so a rejection message can be asserted.
func putSettingsExpectStatus(t *testing.T, handler http.Handler, cookies []*http.Cookie, body any, expected int) string {
	t.Helper()
	encoded, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal request body: %v", err)
	}
	request := httptest.NewRequest(http.MethodPut, "/api/settings", bytes.NewReader(encoded))
	request.Header.Set("Content-Type", "application/json")
	for _, cookie := range cookies {
		request.AddCookie(cookie)
	}
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != expected {
		t.Fatalf("PUT /api/settings returned %d, want %d: %s", recorder.Code, expected, recorder.Body.String())
	}
	return recorder.Body.String()
}

func TestSettingsModelPriceMappingRulesRoundTrip(t *testing.T) {
	handler, cookies, cleanup := newMappingSettingsTestApp(t)
	defer cleanup()

	// Ships empty: deploying the release changes nothing until an operator configures rules.
	if rules := storedMappingRules(t, handler, cookies); len(rules) != 0 {
		t.Fatalf("fresh install has %d mapping rules, want 0", len(rules))
	}

	var saved settingsMappingResponse
	requestJSON(t, handler, http.MethodPut, "/api/settings", map[string]any{
		"model_price_mapping_rules": []map[string]any{
			{"source_provider": " Devin ", "source_model": " SWE-2 ", "target_provider": " MoonShot ", "target_model": " MoonShot/Kimi-K3 "},
			{"source_provider": "", "source_model": "swe-*", "target_provider": "cognition", "target_model": "swe-*"},
		},
	}, cookies, &saved)
	want := []settingsMappingRule{
		{SourceProvider: "devin", SourceModel: "swe-2", TargetProvider: "moonshot", TargetModel: "moonshot/kimi-k3"},
		{SourceProvider: "", SourceModel: "swe-*", TargetProvider: "cognition", TargetModel: "swe-*"},
	}
	if len(saved.ModelPriceMappingRules) != len(want) {
		t.Fatalf("PUT returned %#v, want %#v", saved.ModelPriceMappingRules, want)
	}
	for i, rule := range saved.ModelPriceMappingRules {
		if rule != want[i] {
			t.Fatalf("rule %d = %#v, want %#v (trimmed and lower-cased)", i, rule, want[i])
		}
	}
	stored := storedMappingRules(t, handler, cookies)
	if len(stored) != len(want) || stored[0] != want[0] || stored[1] != want[1] {
		t.Fatalf("GET returned %#v, want %#v", stored, want)
	}

	// Another PUT that omits the field must not clear the rules.
	requestJSON(t, handler, http.MethodPut, "/api/settings", map[string]any{"queue_name": "usage"}, cookies, nil)
	if stored := storedMappingRules(t, handler, cookies); len(stored) != 2 {
		t.Fatalf("an unrelated settings save dropped the rules: %#v", stored)
	}

	// An explicit empty list clears them.
	requestJSON(t, handler, http.MethodPut, "/api/settings", map[string]any{"model_price_mapping_rules": []map[string]any{}}, cookies, nil)
	if stored := storedMappingRules(t, handler, cookies); len(stored) != 0 {
		t.Fatalf("empty list did not clear the rules: %#v", stored)
	}
}

func TestSettingsModelPriceMappingRulesRejectsInvalidRules(t *testing.T) {
	handler, cookies, cleanup := newMappingSettingsTestApp(t)
	defer cleanup()

	good := []map[string]any{
		{"source_provider": "devin", "source_model": "swe-2", "target_provider": "moonshot", "target_model": "moonshot/kimi-k3"},
	}
	requestJSON(t, handler, http.MethodPut, "/api/settings", map[string]any{"model_price_mapping_rules": good}, cookies, nil)

	tooMany := make([]map[string]any, 0, 51)
	for i := 0; i < 51; i++ {
		tooMany = append(tooMany, map[string]any{
			"source_provider": "devin",
			"source_model":    "swe-" + string(rune('a'+i%26)) + string(rune('a'+i/26)),
			"target_provider": "moonshot",
			"target_model":    "moonshot/kimi-k3",
		})
	}
	longField := make([]byte, 181)
	for i := range longField {
		longField[i] = 'a'
	}

	cases := []struct {
		name    string
		rules   any
		message string
	}{
		{
			name:    "empty source model",
			rules:   []map[string]any{{"source_provider": "devin", "source_model": "  ", "target_provider": "moonshot", "target_model": "moonshot/kimi-k3"}},
			message: "模型价格映射规则 #1 的 source_model 不能为空",
		},
		{
			name:    "empty target provider",
			rules:   []map[string]any{{"source_model": "swe-2", "target_provider": "", "target_model": "moonshot/kimi-k3"}},
			message: "模型价格映射规则 #1 的 target_provider 不能为空",
		},
		{
			name:    "empty target model",
			rules:   []map[string]any{{"source_model": "swe-2", "target_provider": "moonshot", "target_model": " "}},
			message: "模型价格映射规则 #1 的 target_model 不能为空",
		},
		{
			name:    "two wildcards in source model",
			rules:   []map[string]any{{"source_model": "swe-*-*", "target_provider": "moonshot", "target_model": "moonshot/kimi-k3"}},
			message: "模型价格映射规则 #1 的 source_model 最多只能包含一个 *",
		},
		{
			name:    "two wildcards in target model",
			rules:   []map[string]any{{"source_model": "swe-*", "target_provider": "moonshot", "target_model": "kimi-*-*"}},
			message: "模型价格映射规则 #1 的 target_model 最多只能包含一个 *",
		},
		{
			name:    "wildcard in target without one in source",
			rules:   []map[string]any{{"source_model": "swe-2", "target_provider": "moonshot", "target_model": "kimi-*"}},
			message: "模型价格映射规则 #1 的 target_model 含有 *，但 source_model 没有 *",
		},
		{
			name:    "wildcard in a provider field",
			rules:   []map[string]any{{"source_provider": "dev*", "source_model": "swe-2", "target_provider": "moonshot", "target_model": "moonshot/kimi-k3"}},
			message: "模型价格映射规则 #1 的 source_provider 不能包含 *",
		},
		{
			name: "duplicate source pair",
			rules: []map[string]any{
				{"source_provider": "devin", "source_model": "swe-2", "target_provider": "moonshot", "target_model": "moonshot/kimi-k3"},
				{"source_provider": "DEVIN", "source_model": " swe-2 ", "target_provider": "cognition", "target_model": "swe-2"},
			},
			message: "模型价格映射规则 #2 与前面的规则重复（source_provider + source_model 相同）",
		},
		{
			name:    "field too long",
			rules:   []map[string]any{{"source_model": string(longField), "target_provider": "moonshot", "target_model": "moonshot/kimi-k3"}},
			message: "模型价格映射规则 #1 的 source_model 超出最大长度 180",
		},
		{
			name:    "too many rules",
			rules:   tooMany,
			message: "模型价格映射规则最多 50 条",
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			body := putSettingsExpectStatus(t, handler, cookies, map[string]any{"model_price_mapping_rules": testCase.rules}, http.StatusUnprocessableEntity)
			var failure struct {
				Detail struct {
					Code    string `json:"code"`
					Message string `json:"message"`
				} `json:"detail"`
			}
			if err := json.Unmarshal([]byte(body), &failure); err != nil {
				t.Fatalf("decode error body %q: %v", body, err)
			}
			if failure.Detail.Code != "validation_error" || failure.Detail.Message != testCase.message {
				t.Fatalf("error = %s/%q, want validation_error/%q", failure.Detail.Code, failure.Detail.Message, testCase.message)
			}
		})
	}

	// None of the rejections may have touched the stored rules.
	stored := storedMappingRules(t, handler, cookies)
	if len(stored) != 1 || stored[0].SourceModel != "swe-2" || stored[0].TargetModel != "moonshot/kimi-k3" {
		t.Fatalf("rejected payloads changed the stored rules: %#v", stored)
	}
}

package app

import (
	"reflect"
	"testing"
)

func priceTable(entries ...[3]any) map[[2]string]ModelPrice {
	table := map[[2]string]ModelPrice{}
	for _, entry := range entries {
		provider, model, input := entry[0].(string), entry[1].(string), entry[2].(float64)
		table[priceKey(provider, model)] = ModelPrice{Provider: provider, Model: model, InputUSDPerMillion: input}
	}
	return table
}

func matchInput(t *testing.T, prices map[[2]string]ModelPrice, provider, model string) (float64, bool) {
	t.Helper()
	price := findMatchingPrice(prices, &provider, &model)
	if price == nil {
		return 0, false
	}
	return price.InputUSDPerMillion, true
}

func TestFindMatchingPriceAssociatesReverseProxiedVariantWithLiteLLMEntry(t *testing.T) {
	// The reported field case: antigravity + tiered model vs LiteLLM's gemini/<model> key.
	prices := priceTable([3]any{"gemini", "gemini/gemini-3.8-flash", 0.3})
	for _, model := range []string{"gemini-3.8-flash-high", "gemini-3.8-flash-low", "gemini-3.8-flash", "Gemini-3.8-Flash-HIGH "} {
		if got, ok := matchInput(t, prices, "antigravity", model); !ok || got != 0.3 {
			t.Fatalf("antigravity/%q → %v,%v; want 0.3", model, got, ok)
		}
	}
	// Other gemini reverse-proxy providers associate the same way.
	for _, provider := range []string{"gemini-cli", "aistudio", "gemini"} {
		if got, ok := matchInput(t, prices, provider, "gemini-3.8-flash-high"); !ok || got != 0.3 {
			t.Fatalf("%s → %v,%v; want 0.3", provider, got, ok)
		}
	}
}

func TestFindMatchingPriceExactAlwaysWinsOverAssociation(t *testing.T) {
	prices := priceTable(
		[3]any{"gemini", "gemini/gemini-3.8-flash", 0.3},
		[3]any{"gemini", "gemini-3.8-flash-high", 0.9},      // exact tiered model on the alias provider
		[3]any{"antigravity", "gemini-3.8-flash-high", 1.5}, // exact provider + model (manual override)
	)
	if got, _ := matchInput(t, prices, "antigravity", "gemini-3.8-flash-high"); got != 1.5 {
		t.Fatalf("exact provider+model must win, got %v", got)
	}
	delete(prices, priceKey("antigravity", "gemini-3.8-flash-high"))
	if got, _ := matchInput(t, prices, "antigravity", "gemini-3.8-flash-high"); got != 0.9 {
		t.Fatalf("exact model on the associated provider must beat the stripped base model, got %v", got)
	}
}

func TestFindMatchingPriceAntigravityResolvesVendorByModelFamily(t *testing.T) {
	prices := priceTable(
		[3]any{"anthropic", "claude-sonnet-4-5", 3.0},
		[3]any{"openai", "gpt-5.2", 1.25},
		[3]any{"gemini", "gemini/gemini-3.8-flash", 0.3},
	)
	if got, ok := matchInput(t, prices, "antigravity", "claude-sonnet-4-5-thinking"); !ok || got != 3.0 {
		t.Fatalf("claude via antigravity → %v,%v", got, ok)
	}
	if got, ok := matchInput(t, prices, "antigravity", "gpt-5.2-high"); !ok || got != 1.25 {
		t.Fatalf("gpt via antigravity → %v,%v", got, ok)
	}
	// A gemini model must never borrow another vendor's price, and unknown families stay unpriced.
	if _, ok := matchInput(t, priceTable([3]any{"openai", "gemini-3.8-flash", 9.0}), "antigravity", "gemini-3.8-flash-high"); ok {
		t.Fatal("gemini model must not match an openai price")
	}
	if _, ok := matchInput(t, prices, "antigravity", "mystery-model-high"); ok {
		t.Fatal("unknown model family must stay unpriced")
	}
}

func TestFindMatchingPriceKeepsExistingBehaviour(t *testing.T) {
	prices := priceTable([3]any{"openai", "gpt-5.2", 1.25}, [3]any{"anthropic", "claude-sonnet-4-5", 3.0})
	if got, ok := matchInput(t, prices, "codex", "gpt-5.2"); !ok || got != 1.25 {
		t.Fatalf("codex→openai alias regressed: %v,%v", got, ok)
	}
	if got, ok := matchInput(t, prices, "claude", "claude-sonnet-4-5"); !ok || got != 3.0 {
		t.Fatalf("claude→anthropic alias regressed: %v,%v", got, ok)
	}
	// No cross-provider leakage for providers without an association.
	if _, ok := matchInput(t, prices, "kimi", "gpt-5.2"); ok {
		t.Fatal("unrelated provider must not match")
	}
	// A suffix is only stripped when something remains, and non-variant names are untouched.
	if _, ok := matchInput(t, priceTable([3]any{"openai", "", 1.0}), "openai", "-high"); ok {
		t.Fatal("bare suffix must not match")
	}
	if nilPrice := findMatchingPrice(prices, nil, nil); nilPrice != nil {
		t.Fatal("nil inputs must not match")
	}
}

func TestPriceModelCandidatesOrder(t *testing.T) {
	got := priceModelCandidates("claude-opus-4-5-thinking-high")
	want := []string{"claude-opus-4-5-thinking-high", "claude-opus-4-5-thinking", "claude-opus-4-5"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("candidates = %v, want %v", got, want)
	}
	if got := priceModelCandidates("gpt-5.2"); !reflect.DeepEqual(got, []string{"gpt-5.2"}) {
		t.Fatalf("plain model candidates = %v", got)
	}
}

func TestFindMatchingPriceDevinSwe2AssociatesWithKimiK3(t *testing.T) {
	prices := priceTable(
		[3]any{"moonshot", "moonshot/kimi-k3", 3.0},
		[3]any{"cognition", "cognition/swe-1.7", 0.5},
	)
	// devin/swe-2 with variants must all resolve to moonshot/kimi-k3
	for _, model := range []string{"devin/swe-2", "devin/swe-2-high", "devin/swe-2-medium", "devin/swe-2-low", "devin/swe-2-thinking", "swe-2", "swe-2-high"} {
		if got, ok := matchInput(t, prices, "devin", model); !ok || got != 3.0 {
			t.Fatalf("devin/%q → %v,%v; want 3.0", model, got, ok)
		}
	}
}

func TestFindMatchingPriceDevinOtherSweAssociatesWithCognition(t *testing.T) {
	prices := priceTable(
		[3]any{"cognition", "cognition/swe-1.6", 0.5},
		[3]any{"cognition", "cognition/swe-1.7", 0.6},
		[3]any{"cognition", "cognition/swe-1.7-lightning", 2.5},
		[3]any{"moonshot", "moonshot/kimi-k3", 3.0},
	)
	if got, ok := matchInput(t, prices, "devin", "devin/swe-1.6"); !ok || got != 0.5 {
		t.Fatalf("devin/swe-1.6 → %v,%v; want 0.5", got, ok)
	}
	if got, ok := matchInput(t, prices, "devin", "devin/swe-1.7-high"); !ok || got != 0.6 {
		t.Fatalf("devin/swe-1.7-high → %v,%v; want 0.6", got, ok)
	}
	if got, ok := matchInput(t, prices, "devin", "swe-1.7"); !ok || got != 0.6 {
		t.Fatalf("swe-1.7 → %v,%v; want 0.6", got, ok)
	}
	if got, ok := matchInput(t, prices, "devin", "devin/swe-1.7-lightning"); !ok || got != 2.5 {
		t.Fatalf("devin/swe-1.7-lightning → %v,%v; want 2.5", got, ok)
	}
	// swe-20 must not be confused with swe-2; should seek cognition/swe-20 (which is absent, so unpriced)
	if _, ok := matchInput(t, prices, "devin", "devin/swe-20"); ok {
		t.Fatal("swe-20 must not map to kimi-k3")
	}
	// Non-swe model under devin must not leak to moonshot or cognition
	if _, ok := matchInput(t, prices, "devin", "devin/custom-model"); ok {
		t.Fatal("unknown model family under devin must stay unpriced")
	}
}

func TestFindMatchingPriceKimiAssociatesWithMoonshot(t *testing.T) {
	prices := priceTable([3]any{"moonshot", "moonshot/kimi-k3", 3.0})
	if got, ok := matchInput(t, prices, "kimi", "kimi-k3"); !ok || got != 3.0 {
		t.Fatalf("kimi/kimi-k3 → %v,%v; want 3.0", got, ok)
	}
}

func TestPriceModelCandidatesDevinOrder(t *testing.T) {
	got := priceModelCandidates("devin/swe-2-high")
	want := []string{"devin/swe-2-high", "devin/swe-2", "swe-2-high", "swe-2", "kimi-k3"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("candidates = %v, want %v", got, want)
	}
	gotOther := priceModelCandidates("devin/swe-1.7-high")
	wantOther := []string{"devin/swe-1.7-high", "devin/swe-1.7", "swe-1.7-high", "swe-1.7"}
	if !reflect.DeepEqual(gotOther, wantOther) {
		t.Fatalf("candidates = %v, want %v", gotOther, wantOther)
	}
}

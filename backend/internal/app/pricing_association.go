package app

import "strings"

// Price association: reverse-proxied models are reported under the proxy's own provider name and
// often with a reasoning-tier suffix (provider "antigravity", model "gemini-3.8-flash-high",
// provider "devin", model "devin/swe-2"), while the price list — typically synced from LiteLLM —
// knows the canonical vendor entry (provider "gemini", model "gemini/gemini-3.8-flash",
// provider "moonshot", model "moonshot/kimi-k3", provider "cognition", model "cognition/swe-1.7").
// These helpers produce the ordered lookup candidates that associate the former with the latter.
// Order is most-specific first, and the caller tries every provider candidate for one model
// candidate before relaxing the model name.

// priceModelVariantSuffixes are request-variant suffixes that select a reasoning tier / mode but
// do not change the per-token price, so a variant may fall back to its base model's price.
var priceModelVariantSuffixes = []string{"-thinking", "-minimal", "-low", "-medium", "-high", "-xhigh"}

// isDevinSwe2 checks if model is swe-2 (optionally prefixed with devin/ or carrying a reasoning variant).
func isDevinSwe2(model string) bool {
	m := strings.TrimPrefix(model, "devin/")
	return m == "swe-2" || strings.HasPrefix(m, "swe-2-")
}

// priceModelCandidates returns the model itself followed by progressively stripped base names
// ("x-thinking-high" → "x-thinking" → "x"), prefix-stripped variants ("devin/swe-1.7" → "swe-1.7"),
// and canonical cross-model mappings ("devin/swe-2" → "kimi-k3").
// Input must already be lower-cased and trimmed.
func priceModelCandidates(model string) []string {
	candidates := []string{model}
	add := func(value string) {
		for _, existing := range candidates {
			if existing == value {
				return
			}
		}
		candidates = append(candidates, value)
	}

	current := model
	for i := 0; i < 3; i++ {
		stripped := current
		for _, suffix := range priceModelVariantSuffixes {
			if strings.HasSuffix(current, suffix) && len(current) > len(suffix) {
				stripped = strings.TrimSuffix(current, suffix)
				break
			}
		}
		if stripped == current {
			break
		}
		add(stripped)
		current = stripped
	}

	// Devin models: strip "devin/" prefix (e.g. devin/swe-1.7 → swe-1.7).
	if strings.HasPrefix(model, "devin/") {
		for _, c := range candidates {
			if strings.HasPrefix(c, "devin/") {
				trimmed := strings.TrimPrefix(c, "devin/")
				if trimmed != "" {
					add(trimmed)
				}
			}
		}
	}

	// devin/swe-2 maps to kimi-k3 (vendor moonshot).
	for _, c := range candidates {
		if isDevinSwe2(c) {
			add("kimi-k3")
			break
		}
	}

	return candidates
}

// priceProviderCandidates returns the provider itself followed by the canonical vendors whose
// price entries it may use. Multi-vendor reverse proxies (antigravity, devin) are resolved by the model
// family. Input must already be lower-cased and trimmed.
func priceProviderCandidates(provider, model string) []string {
	candidates := []string{provider}
	add := func(values ...string) {
		for _, value := range values {
			duplicate := false
			for _, existing := range candidates {
				if existing == value {
					duplicate = true
					break
				}
			}
			if !duplicate {
				candidates = append(candidates, value)
			}
		}
	}
	switch provider {
	case "codex":
		add("openai")
	case "claude":
		add("anthropic")
	case "gemini-cli", "aistudio":
		add("gemini")
	case "kimi":
		add("moonshot")
	case "devin":
		switch {
		case model == "kimi-k3" || isDevinSwe2(model):
			add("moonshot")
		case strings.HasPrefix(model, "swe-") || strings.HasPrefix(model, "devin/swe-") || strings.HasPrefix(model, "cognition/"):
			add("cognition")
		}
	case "cognition":
		if model == "kimi-k3" || isDevinSwe2(model) {
			add("moonshot")
		}
	case "antigravity":
		switch {
		case strings.HasPrefix(model, "gemini"):
			add("gemini")
		case strings.HasPrefix(model, "claude"):
			add("anthropic")
		case strings.HasPrefix(model, "gpt"), strings.HasPrefix(model, "o1"), strings.HasPrefix(model, "o3"), strings.HasPrefix(model, "o4"):
			add("openai")
		case model == "kimi-k3" || isDevinSwe2(model):
			add("moonshot")
		case strings.HasPrefix(model, "devin/swe-") || strings.HasPrefix(model, "swe-"):
			add("cognition")
		}
	}
	return candidates
}

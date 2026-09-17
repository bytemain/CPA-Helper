package app

import "strings"

// Price association: reverse-proxied models are reported under the proxy's own provider name and
// often with a reasoning-tier suffix (provider "antigravity", model "gemini-3.8-flash-high"),
// while the price list — typically synced from LiteLLM — knows the canonical vendor entry
// (provider "gemini", model "gemini/gemini-3.8-flash"). These helpers produce the ordered lookup
// candidates that associate the former with the latter. Order is most-specific first, and the
// caller tries every provider candidate for one model candidate before relaxing the model name.

// priceModelVariantSuffixes are request-variant suffixes that select a reasoning tier / mode but
// do not change the per-token price, so a variant may fall back to its base model's price.
var priceModelVariantSuffixes = []string{"-thinking", "-minimal", "-low", "-medium", "-high", "-xhigh"}

// priceModelCandidates returns the model itself followed by progressively stripped base names
// ("x-thinking-high" → "x-thinking" → "x"). Input must already be lower-cased and trimmed.
func priceModelCandidates(model string) []string {
	candidates := []string{model}
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
		candidates = append(candidates, stripped)
		current = stripped
	}
	return candidates
}

// priceProviderCandidates returns the provider itself followed by the canonical vendors whose
// price entries it may use. Multi-vendor reverse proxies (antigravity) are resolved by the model
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
	case "antigravity":
		switch {
		case strings.HasPrefix(model, "gemini"):
			add("gemini")
		case strings.HasPrefix(model, "claude"):
			add("anthropic")
		case strings.HasPrefix(model, "gpt"), strings.HasPrefix(model, "o1"), strings.HasPrefix(model, "o3"), strings.HasPrefix(model, "o4"):
			add("openai")
		}
	}
	return candidates
}

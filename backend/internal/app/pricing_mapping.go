package app

import (
	"fmt"
	"strings"
)

// Model price mapping: a reverse proxy can report a model name the price dictionary does not know
// (provider `devin`, model `swe-2`) while an equivalent model IS known (provider `moonshot`, model
// `moonshot/kimi-k3`). Instead of hardcoding such pairs in Go, an operator declares ordered rules
// here; the rules are stored as JSON in app_settings and evaluated by priceBook.find.
//
// The engine is deliberately total and boring — it is on the money path:
//   - An exact (provider, model) price always wins and is returned BEFORE any rule is considered,
//     so a manually created price row can always override a rule.
//   - At most ONE mapping hop is applied; the mapped result is never fed back into the engine, so
//     rules can never chain or loop (a rule A→B plus a rule B→C prices an A record by B, never C).
//   - If the mapped target has no price, the record stays UNPRICED. A rule never turns an unpriced
//     record into a silent $0 charge.

const (
	// maxModelPriceMappingRules bounds the rule list so the settings row stays small and the
	// per-lookup scan stays trivial.
	maxModelPriceMappingRules = 50
	// maxModelPriceMappingFieldLength bounds a single field; provider/model names are far shorter.
	maxModelPriceMappingFieldLength = 180
	// modelPriceMappingWildcard matches a non-empty substring. A field may contain at most one.
	modelPriceMappingWildcard = "*"
)

// ModelPriceMappingRule maps a reported (provider, model) onto the (provider, model) whose price
// should be used. SourceProvider is optional: empty matches any provider. SourceModel is either an
// exact name or a pattern containing exactly one `*` matching a non-empty substring; when
// TargetModel contains a `*` it is substituted with the text captured by the source `*`.
type ModelPriceMappingRule struct {
	SourceProvider string `json:"source_provider"`
	SourceModel    string `json:"source_model"`
	TargetProvider string `json:"target_provider"`
	TargetModel    string `json:"target_model"`
}

// normalizeModelPriceMappingRule trims every field and lower-cases them the way priceKey does, so
// rule matching and the price lookup agree on casing.
func normalizeModelPriceMappingRule(rule ModelPriceMappingRule) ModelPriceMappingRule {
	normalize := func(value string) string {
		return strings.ToLower(strings.TrimSpace(value))
	}
	return ModelPriceMappingRule{
		SourceProvider: normalize(rule.SourceProvider),
		SourceModel:    normalize(rule.SourceModel),
		TargetProvider: normalize(rule.TargetProvider),
		TargetModel:    normalize(rule.TargetModel),
	}
}

// validateModelPriceMappingRules normalizes user-supplied rules and rejects malformed ones. Every
// message names the 1-based rule index and the offending field. The returned slice is never nil,
// so an empty list round-trips as `[]` rather than `null`.
func validateModelPriceMappingRules(input []ModelPriceMappingRule) ([]ModelPriceMappingRule, error) {
	if len(input) > maxModelPriceMappingRules {
		return nil, validationError(fmt.Sprintf("模型价格映射规则最多 %d 条", maxModelPriceMappingRules))
	}
	rules := make([]ModelPriceMappingRule, 0, len(input))
	seen := make(map[[2]string]bool, len(input))
	for i, raw := range input {
		position := i + 1
		rule := normalizeModelPriceMappingRule(raw)
		for _, field := range []struct {
			name  string
			value string
		}{
			{"source_provider", rule.SourceProvider},
			{"source_model", rule.SourceModel},
			{"target_provider", rule.TargetProvider},
			{"target_model", rule.TargetModel},
		} {
			if len([]rune(field.value)) > maxModelPriceMappingFieldLength {
				return nil, validationError(fmt.Sprintf("模型价格映射规则 #%d 的 %s 超出最大长度 %d", position, field.name, maxModelPriceMappingFieldLength))
			}
		}
		for _, field := range []struct {
			name  string
			value string
		}{
			{"source_model", rule.SourceModel},
			{"target_provider", rule.TargetProvider},
			{"target_model", rule.TargetModel},
		} {
			if field.value == "" {
				return nil, validationError(fmt.Sprintf("模型价格映射规则 #%d 的 %s 不能为空", position, field.name))
			}
		}
		sourceWildcards := strings.Count(rule.SourceModel, modelPriceMappingWildcard)
		targetWildcards := strings.Count(rule.TargetModel, modelPriceMappingWildcard)
		if sourceWildcards > 1 {
			return nil, validationError(fmt.Sprintf("模型价格映射规则 #%d 的 source_model 最多只能包含一个 *", position))
		}
		if targetWildcards > 1 {
			return nil, validationError(fmt.Sprintf("模型价格映射规则 #%d 的 target_model 最多只能包含一个 *", position))
		}
		// `*` is only meaningful in the model fields; a provider is matched (and written) literally.
		for _, field := range []struct {
			name  string
			value string
		}{
			{"source_provider", rule.SourceProvider},
			{"target_provider", rule.TargetProvider},
		} {
			if strings.Contains(field.value, modelPriceMappingWildcard) {
				return nil, validationError(fmt.Sprintf("模型价格映射规则 #%d 的 %s 不能包含 *", position, field.name))
			}
		}
		if targetWildcards == 1 && sourceWildcards == 0 {
			return nil, validationError(fmt.Sprintf("模型价格映射规则 #%d 的 target_model 含有 *，但 source_model 没有 *", position))
		}
		key := [2]string{rule.SourceProvider, rule.SourceModel}
		if seen[key] {
			return nil, validationError(fmt.Sprintf("模型价格映射规则 #%d 与前面的规则重复（source_provider + source_model 相同）", position))
		}
		seen[key] = true
		rules = append(rules, rule)
	}
	return rules, nil
}

// sanitizeModelPriceMappingRules normalizes already-stored rules and drops any that violate the
// invariants validateModelPriceMappingRules enforces. Stored rules were validated on save, so this
// only matters for a hand-edited database — a malformed row is ignored instead of mispricing.
func sanitizeModelPriceMappingRules(input []ModelPriceMappingRule) []ModelPriceMappingRule {
	rules := make([]ModelPriceMappingRule, 0, len(input))
	seen := make(map[[2]string]bool, len(input))
	for _, raw := range input {
		if len(rules) >= maxModelPriceMappingRules {
			break
		}
		rule := normalizeModelPriceMappingRule(raw)
		if _, err := validateModelPriceMappingRules([]ModelPriceMappingRule{rule}); err != nil {
			continue
		}
		key := [2]string{rule.SourceProvider, rule.SourceModel}
		if seen[key] {
			continue
		}
		seen[key] = true
		rules = append(rules, rule)
	}
	return rules
}

// modelPriceMappingMatch is one rule that matched, with the data needed to rank it.
type modelPriceMappingMatch struct {
	rule ModelPriceMappingRule
	// capture is the text matched by the source `*` (empty when the rule has no wildcard).
	capture string
	// exactModel is true when the rule's source_model contains no wildcard.
	exactModel bool
	// literalLength is the number of non-`*` characters in source_model.
	literalLength int
	// providerQualified is true when the rule pins a source_provider.
	providerQualified bool
	// index is the rule's position in the configured list.
	index int
}

// beats reports whether the receiver outranks other. Precedence, most significant first:
//  1. an exact (wildcard-free) source_model beats a wildcard rule;
//  2. among wildcard rules the longest literal (non-`*`) text wins;
//  3. a rule with a source_provider beats an otherwise equal rule without one;
//  4. remaining ties break by the configured list order (first wins).
func (m modelPriceMappingMatch) beats(other modelPriceMappingMatch) bool {
	if m.exactModel != other.exactModel {
		return m.exactModel
	}
	if m.literalLength != other.literalLength {
		return m.literalLength > other.literalLength
	}
	if m.providerQualified != other.providerQualified {
		return m.providerQualified
	}
	return m.index < other.index
}

// matchModelPriceMappingRule picks the winning rule for an already normalized (lower-cased,
// trimmed) provider/model pair and returns the mapped target. ok is false when no rule matches.
func matchModelPriceMappingRule(rules []ModelPriceMappingRule, provider, model string) (string, string, bool) {
	var best modelPriceMappingMatch
	found := false
	for index, rule := range rules {
		if rule.SourceProvider != "" && rule.SourceProvider != provider {
			continue
		}
		capture, matched := matchModelPricePattern(rule.SourceModel, model)
		if !matched {
			continue
		}
		candidate := modelPriceMappingMatch{
			rule:              rule,
			capture:           capture,
			exactModel:        !strings.Contains(rule.SourceModel, modelPriceMappingWildcard),
			literalLength:     len(strings.ReplaceAll(rule.SourceModel, modelPriceMappingWildcard, "")),
			providerQualified: rule.SourceProvider != "",
			index:             index,
		}
		if !found || candidate.beats(best) {
			best = candidate
			found = true
		}
	}
	if !found {
		return "", "", false
	}
	targetModel := strings.Replace(best.rule.TargetModel, modelPriceMappingWildcard, best.capture, 1)
	return best.rule.TargetProvider, targetModel, true
}

// matchModelPricePattern matches a model against a source pattern. Without a `*` the match is an
// exact comparison. With a `*` the captured substring must be non-empty, so `swe-*` matches
// `swe-2` but never the bare `swe-`.
func matchModelPricePattern(pattern, model string) (string, bool) {
	star := strings.Index(pattern, modelPriceMappingWildcard)
	if star < 0 {
		return "", pattern == model
	}
	prefix, suffix := pattern[:star], pattern[star+1:]
	if len(model) < len(prefix)+len(suffix)+1 {
		return "", false
	}
	if !strings.HasPrefix(model, prefix) || !strings.HasSuffix(model, suffix) {
		return "", false
	}
	return model[len(prefix) : len(model)-len(suffix)], true
}

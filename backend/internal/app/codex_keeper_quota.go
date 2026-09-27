package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"math"
	"net/http"
	"sort"
	"strings"
	"time"
)

// Generic (non-Codex, non-Antigravity) quota providers. Like Antigravity these accounts
// take a separate inspection path: quota is fetched through the same per-auth
// /v0/management/api-call egress (CPA injects the account's $TOKEN$ and routes via the
// credential's proxy_url), projected into the shared []keeperAntigravityGroup shape, and
// stored in the antigravity_quota column — which is now a generic per-provider quota
// snapshot, not Antigravity-specific despite the legacy name.
const (
	keeperProviderKimi  = "kimi"
	keeperProviderXAI   = "xai"
	keeperProviderDevin = "devin"
)

// Upstream request shapes (mirroring the probes CPA's management UI runs for these
// providers). The api-call wrapper carries auth_index; $TOKEN$ is substituted by CPA.
const (
	kimiQuotaURL  = "https://api.kimi.com/coding/v1/usages"
	xaiQuotaURL   = "https://cli-chat-proxy.grok.com/v1/billing?format=credits"
	devinQuotaURL = "https://server.codeium.com/exa.seat_management_pb.SeatManagementService/GetUserStatus"
)

var xaiQuotaHeader = map[string]string{
	"Authorization":         "Bearer $TOKEN$",
	"x-xai-token-auth":      "xai-grok-cli",
	"x-grok-client-version": "0.2.91",
	"accept":                "*/*",
	"user-agent":            "grok-pager/0.2.91 grok-shell/0.2.91 (macos; aarch64)",
}

// devinQuotaBody is the Connect-JSON GetUserStatus request the Devin (chisel) client sends.
// The credential's API key is carried INSIDE the body (metadata.apiKey), so $TOKEN$ appears
// in `data`, not in a header.
const devinQuotaBody = `{"metadata":{"ideName":"chisel","ideVersion":"3000.10.21","apiKey":"$TOKEN$","locale":"en","os":"darwin","extensionVersion":"3000.10.21","clientName":"chisel"}}`

// keeperIsQuotaProvider reports whether an auth-file `type` takes the generic quota path.
func keeperIsQuotaProvider(authType string) bool {
	switch authType {
	case keeperProviderKimi, keeperProviderXAI, keeperProviderDevin:
		return true
	default:
		return false
	}
}

// quotaIdentity is the reconciled routing identity for a generic quota provider: an explicit
// auth_index plus (optionally) the account email.
type quotaIdentity struct {
	authIndex string
	email     string
}

// keeperReconcileQuotaIdentity applies the same list-vs-detail rigor as the Antigravity
// reconciliation, minus the project requirement: the list type must equal the provider, the
// detail (when it states them) must agree on type/name/provider, and the auth_index must be
// explicit on each source, reconcile, and be non-empty. The email is optional (we do not know
// whether these providers' auth files carry one): present-but-invalid or conflicting aliases
// still fail closed, but a missing email yields an empty slot rather than an error — the
// digest binds what is proven.
func keeperReconcileQuotaIdentity(authInfo, detail map[string]any, provider, name string) (quotaIdentity, bool) {
	listType, lterr := keeperExplicitStringField(authInfo, "type")
	if lterr != nil || listType != provider {
		return quotaIdentity{}, false
	}
	detailType, dterr := keeperExplicitStringField(detail, "type")
	if dterr != nil || (detailType != "" && detailType != provider) {
		return quotaIdentity{}, false
	}
	listProvider, lpverr := keeperExplicitStringField(authInfo, "provider")
	if lpverr != nil || (listProvider != "" && listProvider != provider) {
		return quotaIdentity{}, false
	}
	detailProvider, dpverr := keeperExplicitStringField(detail, "provider")
	if dpverr != nil || (detailProvider != "" && detailProvider != provider) {
		return quotaIdentity{}, false
	}
	detailName, dnerr := keeperExplicitStringField(detail, "name")
	if dnerr != nil || (detailName != "" && detailName != name) {
		return quotaIdentity{}, false
	}
	listIdx, lierr := keeperExplicitAuthIndex(authInfo)
	detailIdx, dierr := keeperExplicitAuthIndex(detail)
	if lierr != nil || dierr != nil {
		return quotaIdentity{}, false
	}
	idx, ierr := keeperReconcileIdentityField(listIdx, detailIdx)
	if ierr != nil || strings.TrimSpace(idx) == "" {
		return quotaIdentity{}, false
	}
	listEmail, leerr := keeperExplicitAntigravityEmail(authInfo)
	detailEmail, deerr := keeperExplicitAntigravityEmail(detail)
	if leerr != nil || deerr != nil {
		return quotaIdentity{}, false
	}
	email, eerr := keeperReconcileIdentityField(listEmail, detailEmail)
	if eerr != nil {
		return quotaIdentity{}, false
	}
	return quotaIdentity{authIndex: idx, email: email}, true
}

// keeperQuotaIdentityDigest returns a stable, versioned one-way digest of a quota-provider
// account identity. Unlike the Antigravity digest (provider+project+email) these providers
// have no second stable identifier, so the digest is bound to provider + auth_index +
// normalized email — a weaker binding (CPA's auth_index is a provider+path hash, so a
// filename/account swap under the same file name is only detected via the email slot, which
// may be empty). Still one-way: no raw identity is persisted.
func keeperQuotaIdentityDigest(provider, authIndex, email string) string {
	normEmail := strings.ToLower(strings.TrimSpace(email))
	sum := sha256.Sum256([]byte(provider + "\x00" + authIndex + "\x00" + normEmail))
	return "v1:" + hex.EncodeToString(sum[:])
}

// processKeeperQuotaAuth inspects one generic-quota-provider account (kimi/xai/devin). It
// mirrors processKeeperAntigravityAuth: read the download detail, reconcile identity, record
// identity/health fields, and (unless disabled) fetch the quota snapshot. A failed fetch
// preserves the previous snapshot (AntigravityQuota nil → COALESCE) and records a
// network_error rather than wiping the account.
func (a *App) processKeeperQuotaAuth(ctx context.Context, cfg AppConfig, authInfo map[string]any, provider string, logFn func(string), manualRefresh bool) keeperAccountResult {
	now := time.Now().In(appTimeLocation)
	name := keeperString(authInfo["name"])
	if name == "" {
		name = "unknown"
	}
	result := keeperAccountResult{Name: name, Result: "skipped", CheckedAt: now, Provider: &provider}
	persist := func(r keeperAccountResult) keeperAccountResult {
		if err := a.upsertKeeperState(ctx, r); err != nil {
			logFn(r.Name + "：状态写回失败（state_write_error）")
			log.Printf("codex keeper %s state write-back failed for %s: %v", provider, r.Name, err)
			r.StateWriteFailed = true
		}
		return r
	}
	detail, err := a.getKeeperRemoteAuthFile(ctx, cfg, name)
	if err != nil || detail == nil {
		message := "读取 auth file 详情失败"
		if err != nil {
			message += "：" + err.Error()
		}
		result.Result = "network_error"
		result.LastError = &message
		result.LatestAction = &message
		result = persist(result)
		logFn(name + ": " + message)
		return result
	}
	// Bind identity FIRST (same rigor as the Antigravity path): the download must be for THIS
	// account name, still be this provider, and carry an explicit reconciled auth_index. Any
	// mismatch mixes accounts, so fail closed as identity_error, preserve the prior snapshot,
	// and make NO quota call.
	identity, ok := keeperReconcileQuotaIdentity(authInfo, detail, provider, name)
	if !ok {
		message := "账号身份冲突：" + provider + " 列表与详情的 name/type/provider/auth_index/email 不一致，已保留原快照"
		result.Result = "identity_error"
		result.LastError = &message
		result.LatestAction = &message
		if err := a.markKeeperIdentityError(ctx, name, &message, result.CheckedAt); err != nil {
			logFn(name + "：状态写回失败（state_write_error）")
			log.Printf("codex keeper %s identity-error write-back failed for %s: %v", provider, name, err)
			result.StateWriteFailed = true
		}
		logFn(name + "：" + message)
		return result
	}
	merged := mergeKeeperObjects(authInfo, detail)
	result.Email = keeperStringPtr(merged["email"], merged["account_email"], merged["user_email"])
	idx := identity.authIndex
	result.AuthIndex = &idx
	// Bind a one-way DIGEST of the resolved identity regardless of the fetch outcome, so a
	// later inspection can detect an identity swap and clear the stale quota even when the
	// fresh quota fetch fails.
	digest := keeperQuotaIdentityDigest(provider, identity.authIndex, identity.email)
	result.AntigravityIdentityDigest = &digest
	result.Priority = keeperIntPtr(merged["priority"])
	disabled := keeperBool(merged["disabled"])
	result.Disabled = &disabled
	if disabled && !manualRefresh {
		result.Result = "disabled"
		result = persist(result)
		return result
	}
	if groups, ok := a.fetchProviderQuota(ctx, cfg, provider, identity.authIndex); ok {
		if encoded, err := json.Marshal(groups); err == nil {
			payload := string(encoded)
			result.AntigravityQuota = &payload
		}
		result.Result = "healthy"
		logFn(fmt.Sprintf("%s：%s 配额刷新成功（%d 组）", name, provider, len(groups)))
	} else {
		message := provider + " 配额读取失败"
		result.Result = "network_error"
		result.LastError = &message
		result.LatestAction = &message
		logFn(name + "：" + message)
	}
	result = persist(result)
	return result
}

// fetchProviderQuota dispatches the provider-specific quota fetch. Every fetcher follows the
// fetchAntigravityQuota contract: transport error, non-2xx outer/inner status, or an
// unparseable body returns ok=false so the caller preserves the prior snapshot.
func (a *App) fetchProviderQuota(ctx context.Context, cfg AppConfig, provider, authIndex string) ([]keeperAntigravityGroup, bool) {
	switch provider {
	case keeperProviderKimi:
		return a.fetchKimiQuota(ctx, cfg, authIndex)
	case keeperProviderXAI:
		return a.fetchXAIQuota(ctx, cfg, authIndex)
	case keeperProviderDevin:
		return a.fetchDevinQuota(ctx, cfg, authIndex)
	default:
		return nil, false
	}
}

// keeperQuotaAPICall issues one api-call and returns the parsed inner body. ok=false on any
// transport error, non-2xx outer status, non-2xx inner status (strict), or non-JSON body.
func (a *App) keeperQuotaAPICall(ctx context.Context, cfg AppConfig, authIndex, method, url string, header map[string]string, data string) (map[string]any, bool) {
	body := map[string]any{
		"auth_index": authIndex,
		"method":     method,
		"url":        url,
		"header":     header,
		"data":       data,
	}
	response, payload, err := a.keeperRequest(ctx, cfg, http.MethodPost, "/v0/management/api-call", nil, body, time.Duration(cfg.CodexKeeper.UsageTimeoutSeconds)*time.Second)
	if err != nil {
		return nil, false
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, false
	}
	var raw map[string]any
	if err := json.Unmarshal(payload, &raw); err != nil {
		return nil, false
	}
	if !keeperInnerStatusOK(raw) {
		return nil, false
	}
	return keeperBodyJSON(raw["body"]), true
}

func (a *App) fetchKimiQuota(ctx context.Context, cfg AppConfig, authIndex string) ([]keeperAntigravityGroup, bool) {
	body, ok := a.keeperQuotaAPICall(ctx, cfg, authIndex, "GET", kimiQuotaURL, map[string]string{
		"Authorization": "Bearer $TOKEN$",
	}, "")
	if !ok {
		return nil, false
	}
	return parseKimiQuotaGroups(body)
}

func (a *App) fetchXAIQuota(ctx context.Context, cfg AppConfig, authIndex string) ([]keeperAntigravityGroup, bool) {
	body, ok := a.keeperQuotaAPICall(ctx, cfg, authIndex, "GET", xaiQuotaURL, xaiQuotaHeader, "")
	if !ok {
		return nil, false
	}
	return parseXAIQuotaGroups(body)
}

func (a *App) fetchDevinQuota(ctx context.Context, cfg AppConfig, authIndex string) ([]keeperAntigravityGroup, bool) {
	body, ok := a.keeperQuotaAPICall(ctx, cfg, authIndex, "POST", devinQuotaURL, map[string]string{
		"Content-Type":             "application/json",
		"Connect-Protocol-Version": "1",
	}, devinQuotaBody)
	if !ok {
		return nil, false
	}
	return parseDevinQuotaGroups(body)
}

// --- shared parser helpers -------------------------------------------------

// keeperFloatValue reads a JSON number (float64, or a numeric string) as finite float64.
// Present-but-non-numeric or non-finite values fail (ok=false) so garbage never renders as a
// real amount.
func keeperFloatValue(value any) (float64, bool) {
	switch v := value.(type) {
	case float64:
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return 0, false
		}
		return v, true
	case string:
		var f float64
		if strings.TrimSpace(v) == "" {
			return 0, false
		}
		if _, err := fmt.Sscanf(strings.TrimSpace(v), "%g", &f); err != nil || math.IsNaN(f) || math.IsInf(f, 0) {
			return 0, false
		}
		return f, true
	default:
		return 0, false
	}
}

// keeperClamp01 clamps a computed fraction into [0,1].
func keeperClamp01(f float64) float64 {
	if f < 0 {
		return 0
	}
	if f > 1 {
		return 1
	}
	return f
}

// quotaFlexibleResetAt parses a reset timestamp that may be a unix number (seconds or
// milliseconds), an RFC3339 string, or absent. ok=true means the field was absent/null or
// parsed; ok=false means present but unparseable (drop just that bucket's reset).
func quotaFlexibleResetAt(values ...any) (*time.Time, bool) {
	for _, v := range values {
		if v == nil {
			continue
		}
		if s, ok := v.(string); ok {
			s = strings.TrimSpace(s)
			if s == "" {
				continue
			}
			if parsed, err := time.Parse(time.RFC3339Nano, s); err == nil {
				parsed = parsed.In(appTimeLocation)
				return &parsed, true
			}
			if ts, ok := keeperFloatValue(s); ok {
				seconds := int64(ts)
				if seconds > 10_000_000_000 {
					seconds /= 1000
				}
				if seconds <= 0 {
					continue
				}
				parsed := time.Unix(seconds, 0).In(appTimeLocation)
				return &parsed, true
			}
			return nil, false
		}
		if ts, ok := keeperFloatValue(v); ok {
			seconds := int64(ts)
			if seconds > 10_000_000_000 {
				seconds /= 1000
			}
			if seconds <= 0 {
				continue
			}
			parsed := time.Unix(seconds, 0).In(appTimeLocation)
			return &parsed, true
		}
		return nil, false
	}
	return nil, true
}

// quotaWindowLabel normalizes a window key/name into a runway-recognized label.
func quotaWindowLabel(raw string) string {
	text := strings.ToLower(strings.TrimSpace(raw))
	switch {
	case text == "":
		return ""
	case strings.Contains(text, "month"):
		return "monthly"
	case strings.Contains(text, "week") || strings.Contains(text, "7d"):
		return "weekly"
	case strings.Contains(text, "day") || strings.Contains(text, "24h") || strings.Contains(text, "1d"):
		return "daily"
	case strings.Contains(text, "5h"):
		return "5h"
	case strings.Contains(text, "hour") || strings.Contains(text, "h"):
		return text
	default:
		return text
	}
}

// --- parsers ---------------------------------------------------------------

// parseKimiQuotaGroups projects the Kimi coding usages body into one "Kimi" group. The
// response shape is not pinned: `usages` may be an array (possibly under `data`) whose
// entries map limit_* / remaining / used fields to rolling windows, or a map keyed by
// limit_* names whose entries carry used_ratio / reset_time. ok=false when no usable
// bucket is found.
func parseKimiQuotaGroups(body map[string]any) ([]keeperAntigravityGroup, bool) {
	if body == nil {
		return nil, false
	}
	usagesNode, present := body["usages"]
	if !present {
		if data, _ := body["data"].(map[string]any); data != nil {
			usagesNode = data["usages"]
		}
	}
	if usagesNode == nil {
		return nil, false
	}
	buckets := []keeperAntigravityBucket{}
	seen := map[string]bool{}
	// Map form: {"limit_5h": {"used_ratio": 0.02, "reset_time": "..."}, ...}. Keys are
	// sorted for deterministic output; entries sharing a window label keep the first.
	if usageMap, ok := usagesNode.(map[string]any); ok {
		names := make([]string, 0, len(usageMap))
		for name := range usageMap {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			entry, ok := usageMap[name].(map[string]any)
			if !ok || !strings.HasPrefix(name, "limit_") {
				continue
			}
			suffix := strings.TrimPrefix(name, "limit_")
			label := quotaWindowLabel(suffix)
			if label == "" || seen[label] {
				continue
			}
			fraction, has := kimiRatioFraction(entry)
			if !has {
				fraction, has = kimiEntryFraction(entry, suffix, 0)
			}
			if !has {
				continue
			}
			var resetAt *time.Time
			if ts, ok := quotaFlexibleResetAt(entry["reset_time"], entry["resetTime"], entry["reset_at"], entry["resetAt"]); ok {
				resetAt = ts
			}
			seen[label] = true
			buckets = append(buckets, keeperAntigravityBucket{
				BucketID:          label,
				DisplayName:       label,
				Window:            label,
				RemainingFraction: fraction,
				ResetAt:           resetAt,
			})
		}
		if len(buckets) > 0 {
			return []keeperAntigravityGroup{{DisplayName: "Kimi", Buckets: buckets}}, true
		}
	}
	rawUsages, ok := usagesNode.([]any)
	if !ok {
		return nil, false
	}
	seen = map[string]bool{}
	for _, u := range rawUsages {
		entry, ok := u.(map[string]any)
		if !ok {
			continue
		}
		resetAt, ok := quotaFlexibleResetAt(entry["reset_at"], entry["resetAt"], entry["reset_time"], entry["resetTime"])
		if !ok {
			resetAt = nil
		}
		for key, value := range entry {
			if !strings.HasPrefix(key, "limit_") {
				continue
			}
			limit, ok := keeperFloatValue(value)
			if !ok || limit <= 0 {
				continue
			}
			suffix := strings.TrimPrefix(key, "limit_")
			label := quotaWindowLabel(suffix)
			if label == "" || seen[key] {
				continue
			}
			fraction, has := kimiEntryFraction(entry, suffix, limit)
			if !has {
				continue
			}
			seen[key] = true
			buckets = append(buckets, keeperAntigravityBucket{
				BucketID:          key,
				DisplayName:       label,
				Window:            label,
				RemainingFraction: fraction,
				ResetAt:           resetAt,
			})
		}
		// Entries may instead carry an explicit window name + used/limit pair.
		if label := quotaWindowLabel(keeperStringFirst(entry["window"], entry["name"], entry["period"], entry["type"])); label != "" && !seen[label] {
			if fraction, has := kimiEntryFraction(entry, "", 0); has {
				seen[label] = true
				buckets = append(buckets, keeperAntigravityBucket{
					BucketID:          label,
					DisplayName:       label,
					Window:            label,
					RemainingFraction: fraction,
					ResetAt:           resetAt,
				})
			}
		}
	}
	if len(buckets) == 0 {
		return nil, false
	}
	return []keeperAntigravityGroup{{DisplayName: "Kimi", Buckets: buckets}}, true
}

// kimiRatioFraction resolves a remaining fraction from ratio-style fields: used_ratio /
// usedRatio mean "fraction used" (remaining = 1 - ratio); remaining_ratio / remainingRatio
// are already the remaining fraction. Ratios > 1 are treated as percents.
func kimiRatioFraction(entry map[string]any) (float64, bool) {
	for _, k := range []string{"remaining_ratio", "remainingRatio"} {
		if raw, present := entry[k]; present && raw != nil {
			if v, ok := keeperFloatValue(raw); ok {
				if v > 1 {
					v = v / 100
				}
				return keeperClamp01(v), true
			}
		}
	}
	for _, k := range []string{"used_ratio", "usedRatio"} {
		if raw, present := entry[k]; present && raw != nil {
			if v, ok := keeperFloatValue(raw); ok {
				if v > 1 {
					v = v / 100
				}
				return keeperClamp01(1 - v), true
			}
		}
	}
	return 0, false
}

// kimiEntryFraction resolves a remaining fraction for one usage entry. It prefers an explicit
// remaining/limit pair for the same suffix, then generic remaining keys, then 1-used/limit.
func kimiEntryFraction(entry map[string]any, suffix string, limit float64) (float64, bool) {
	if limit <= 0 {
		if v, ok := keeperFloatValue(entry["limit"]); ok {
			limit = v
		}
		if limit <= 0 {
			return 0, false
		}
	}
	remainingKeys := []string{"remaining", "remaining_quota", "remainingQuota", "left", "balance"}
	if suffix != "" {
		remainingKeys = append([]string{"remaining_" + suffix, "remain_" + suffix}, remainingKeys...)
	}
	for _, k := range remainingKeys {
		if raw, present := entry[k]; present && raw != nil {
			if rem, ok := keeperFloatValue(raw); ok {
				return keeperClamp01(rem / limit), true
			}
		}
	}
	usedKeys := []string{"used", "usage", "consumed"}
	if suffix != "" {
		usedKeys = append([]string{"used_" + suffix}, usedKeys...)
	}
	for _, k := range usedKeys {
		if raw, present := entry[k]; present && raw != nil {
			if used, ok := keeperFloatValue(raw); ok {
				return keeperClamp01(1 - used/limit), true
			}
		}
	}
	return 0, false
}

// parseXAIQuotaGroups projects the grok billing body ({"config":{"creditUsagePercent":…},
// "currentPeriod":{"type":…,"end":…}}) into one "xAI" group with a single weekly bucket.
// creditUsagePercent is the USED percent, so remaining = 1 - used/100. The window is taken
// from currentPeriod.type (USAGE_PERIOD_TYPE_WEEKLY/MONTHLY), defaulting to weekly.
// ok=false when the used percent cannot be resolved.
func parseXAIQuotaGroups(body map[string]any) ([]keeperAntigravityGroup, bool) {
	if body == nil {
		return nil, false
	}
	cfgMap, _ := body["config"].(map[string]any)
	if cfgMap == nil {
		cfgMap = body
	}
	var usedPct float64
	var found bool
	for _, k := range []string{"creditUsagePercent", "credit_usage_percent", "usagePercent", "usage_percent", "usedPercent", "used_percent"} {
		if raw, present := cfgMap[k]; present && raw != nil {
			if v, ok := keeperFloatValue(raw); ok {
				usedPct = v
				found = true
			}
			break
		}
	}
	if !found {
		return nil, false
	}
	var resetAt *time.Time
	window := "weekly"
	period, _ := body["currentPeriod"].(map[string]any)
	if period == nil {
		// Live responses nest the period inside "config".
		period, _ = cfgMap["currentPeriod"].(map[string]any)
	}
	if period != nil {
		if ts, ok := quotaFlexibleResetAt(period["end"], period["end_at"], period["endAt"], period["reset_at"], period["resetAt"]); ok {
			resetAt = ts
		}
		if t, _ := period["type"].(string); t != "" {
			switch {
			case strings.Contains(t, "MONTH"):
				window = "monthly"
			case strings.Contains(t, "DAY"), strings.Contains(t, "DAILY"):
				window = "daily"
			case strings.Contains(t, "HOUR"):
				window = "5h"
			default:
				window = "weekly"
			}
		}
	}
	if resetAt == nil {
		if ts, ok := quotaFlexibleResetAt(body["reset_at"], body["resetAt"], body["period_end"], body["periodEnd"]); ok {
			resetAt = ts
		}
	}
	return []keeperAntigravityGroup{{
		DisplayName: "xAI",
		Buckets: []keeperAntigravityBucket{{
			BucketID:          window,
			DisplayName:       window,
			Window:            window,
			RemainingFraction: keeperClamp01(1 - usedPct/100),
			ResetAt:           resetAt,
		}},
	}}, true
}

// parseDevinQuotaGroups projects the Codeium SeatManagementService/GetUserStatus body into one
// "Devin" group with daily/weekly buckets. The planStatus node may sit under userStatus /
// data / result wrappers; whichever carries the percent fields wins. The upstream response is
// protobuf-serialized JSON, so a zero-valued percent is OMITTED — an absent percent key counts
// as 0 remaining, and both buckets are always emitted once a usable planStatus is found. A
// planStatus with NO recognizable percent or reset keys at all is unusable → ok=false.
func parseDevinQuotaGroups(body map[string]any) ([]keeperAntigravityGroup, bool) {
	if body == nil {
		return nil, false
	}
	plan := devinPlanStatus(body)
	if plan == nil {
		return nil, false
	}
	buckets := []keeperAntigravityBucket{}
	anyField := false
	for _, spec := range []struct {
		bucket string
		pcts   []string
		resets []string
	}{
		{"daily", []string{"dailyQuotaRemainingPercent", "daily_quota_remaining_percent", "dailyRemainingPercent"}, []string{"dailyQuotaResetAtUnix", "dailyResetAtUnix", "daily_reset_at_unix", "dailyResetAt", "daily_reset_at"}},
		{"weekly", []string{"weeklyQuotaRemainingPercent", "weekly_quota_remaining_percent", "weeklyRemainingPercent"}, []string{"weeklyQuotaResetAtUnix", "weeklyResetAtUnix", "weekly_reset_at_unix", "weeklyResetAt", "weekly_reset_at"}},
	} {
		pct, present := devinPercent(plan, spec.pcts)
		if present {
			anyField = true
		}
		var resetAt *time.Time
		for _, rk := range spec.resets {
			if _, present := plan[rk]; present {
				anyField = true
			}
			if ts, ok := quotaFlexibleResetAt(plan[rk]); ok && ts != nil {
				resetAt = ts
				break
			}
		}
		buckets = append(buckets, keeperAntigravityBucket{
			BucketID:          spec.bucket,
			DisplayName:       spec.bucket,
			Window:            spec.bucket,
			RemainingFraction: keeperClamp01(pct / 100),
			ResetAt:           resetAt,
		})
	}
	if !anyField {
		return nil, false
	}
	return []keeperAntigravityGroup{{DisplayName: "Devin", Buckets: buckets}}, true
}

// devinPlanStatus unwraps the response to the node that looks like a planStatus map.
func devinPlanStatus(body map[string]any) map[string]any {
	candidates := []map[string]any{}
	for _, key := range []string{"userStatus", "user_status", "data", "result", "response"} {
		if m, _ := body[key].(map[string]any); m != nil {
			candidates = append(candidates, m)
		}
	}
	candidates = append(candidates, body)
	for _, c := range candidates {
		if m, _ := c["planStatus"].(map[string]any); m != nil {
			return m
		}
		if m, _ := c["plan_status"].(map[string]any); m != nil {
			return m
		}
	}
	// The planStatus itself may be the body (or an intermediate wrapper).
	for _, c := range candidates {
		for _, k := range []string{"dailyQuotaRemainingPercent", "weeklyQuotaRemainingPercent", "daily_quota_remaining_percent", "weekly_quota_remaining_percent"} {
			if _, present := c[k]; present {
				return c
			}
		}
	}
	return nil
}

// devinPercent reads the first present percent key. A present-but-non-numeric value counts as
// unusable for that key only; an absent key chain reports not-present.
func devinPercent(plan map[string]any, keys []string) (float64, bool) {
	for _, k := range keys {
		if raw, present := plan[k]; present && raw != nil {
			if v, ok := keeperFloatValue(raw); ok {
				return v, true
			}
			return 0, false
		}
	}
	return 0, false
}

// parseCodexQuotaGroups projects a Codex /usage response (rate_limit.primary_window /
// secondary_window) into the shared group shape so codex quota also persists in the generic
// snapshot column. used_percent is a "consumed" fraction (percent), so remaining = 1 - p/100.
// ok=false when neither window yields a usable percent.
func parseCodexQuotaGroups(payload map[string]any) ([]keeperAntigravityGroup, bool) {
	rateLimit, _ := payload["rate_limit"].(map[string]any)
	if rateLimit == nil {
		return nil, false
	}
	now := time.Now().In(appTimeLocation)
	bucket := func(id, label string, window map[string]any) (keeperAntigravityBucket, bool) {
		if window == nil {
			return keeperAntigravityBucket{}, false
		}
		used, ok := keeperFloatValue(window["used_percent"])
		if !ok {
			if v, ok2 := keeperFloatValue(window["usedPercent"]); ok2 {
				used, ok = v, true
			}
		}
		if !ok {
			return keeperAntigravityBucket{}, false
		}
		b := keeperAntigravityBucket{
			BucketID:          id,
			DisplayName:       label,
			RemainingFraction: keeperClamp01(1 - used/100),
			ResetAt:           quotaResetAt(window, now),
		}
		if seconds := quotaWindowSeconds(window); seconds != nil {
			b.Window = (time.Duration(*seconds) * time.Second).String()
		}
		return b, true
	}
	var buckets []keeperAntigravityBucket
	primary, _ := rateLimit["primary_window"].(map[string]any)
	secondary, _ := rateLimit["secondary_window"].(map[string]any)
	if b, ok := bucket("primary", "Primary window", primary); ok {
		buckets = append(buckets, b)
	}
	if b, ok := bucket("secondary", "Secondary window", secondary); ok {
		buckets = append(buckets, b)
	}
	if len(buckets) == 0 {
		return nil, false
	}
	return []keeperAntigravityGroup{{DisplayName: "Codex", Buckets: buckets}}, true
}

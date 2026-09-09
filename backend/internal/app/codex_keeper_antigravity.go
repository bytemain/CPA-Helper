package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"math"
	"net/http"
	"strings"
	"time"
)

// Provider identifiers for a keeper account. The keeper started as a Codex-only feature; it now
// dispatches per provider so non-Codex accounts (Antigravity) are inspected with their own quota
// source instead of being silently skipped.
const (
	keeperProviderCodex       = "codex"
	keeperProviderAntigravity = "antigravity"
)

// Antigravity (Google Cloud Code) quota summary is fetched through the SAME per-auth
// /v0/management/api-call egress the Codex checks use — CPA injects the account's $TOKEN$ and
// auto-routes via the credential's proxy_url (WARP), so no explicit proxy is needed. The
// endpoint moves between daily/sandbox/stable hosts, so the candidates are tried in order and
// the first successful, parseable response wins. UA mirrors the Antigravity CLI.
const antigravityQuotaUserAgent = "antigravity/cli/1.0.13 (aidev_client; os_type=darwin; arch=arm64)"

var antigravityQuotaURLs = []string{
	"https://daily-cloudcode-pa.googleapis.com/v1internal:retrieveUserQuotaSummary",
	"https://daily-cloudcode-pa.sandbox.googleapis.com/v1internal:retrieveUserQuotaSummary",
	"https://cloudcode-pa.googleapis.com/v1internal:retrieveUserQuotaSummary",
}

// keeperAntigravityBucket is one quota window inside a group (e.g. a "weekly" or "5h" window):
// the fraction of the limit still remaining (0..1) and when it fully refreshes.
type keeperAntigravityBucket struct {
	BucketID          string     `json:"bucket_id"`
	DisplayName       string     `json:"display_name"`
	Window            string     `json:"window"`
	RemainingFraction float64    `json:"remaining_fraction"`
	ResetAt           *time.Time `json:"reset_at"`
	Description       string     `json:"description,omitempty"`
}

// keeperAntigravityGroup is a set of models that share quota windows (e.g. "Gemini Models",
// "Claude and GPT models"), each carrying its own buckets.
type keeperAntigravityGroup struct {
	DisplayName string                    `json:"display_name"`
	Description string                    `json:"description,omitempty"`
	Buckets     []keeperAntigravityBucket `json:"buckets"`
}

// keeperIsInspectableProvider reports whether an auth-file `type` is one the keeper inspects.
// Codex has always been inspected; Antigravity is now included so its accounts appear and get
// their quota refreshed. Other providers are still skipped.
func keeperIsInspectableProvider(authType string) bool {
	switch authType {
	case keeperProviderCodex, keeperProviderAntigravity:
		return true
	default:
		return false
	}
}

// keeperProviderOrCodex normalizes a stored provider value to a non-nil pointer, defaulting a
// NULL/empty value to "codex" (rows written before multi-provider support).
func keeperProviderOrCodex(value *string) *string {
	if value == nil || strings.TrimSpace(*value) == "" {
		codex := keeperProviderCodex
		return &codex
	}
	normalized := strings.ToLower(strings.TrimSpace(*value))
	return &normalized
}

// parseStoredAntigravityQuota decodes the JSON antigravity_quota blob back into groups; a NULL,
// empty, or malformed blob yields nil (no quota) rather than an error.
func parseStoredAntigravityQuota(value sql.NullString) []keeperAntigravityGroup {
	if !value.Valid || strings.TrimSpace(value.String) == "" {
		return nil
	}
	var groups []keeperAntigravityGroup
	if err := json.Unmarshal([]byte(value.String), &groups); err != nil {
		return nil
	}
	return groups
}

// keeperStringFirst returns the first non-empty string among the candidates (used to accept
// both snake_case and camelCase aliases from the remote JSON).
func keeperStringFirst(values ...any) string {
	for _, v := range values {
		if s := keeperString(v); s != "" {
			return s
		}
	}
	return ""
}

// keeperParseFraction returns the first candidate that is a finite JSON number in [0,1]. A
// present-but-out-of-range/non-finite value fails (false) so a garbage fraction never renders as
// a real remaining amount.
func keeperParseFraction(values ...any) (float64, bool) {
	for _, v := range values {
		if v == nil {
			continue
		}
		f, ok := v.(float64)
		if !ok {
			return 0, false
		}
		if math.IsNaN(f) || math.IsInf(f, 0) || f < 0 || f > 1 {
			return 0, false
		}
		return f, true
	}
	return 0, false
}

// keeperAntigravityProjectID resolves the Google project id the quota call needs, from the auth
// file: the top-level field (where CPA's real antigravity-*.json carries it), then the
// metadata/attributes maps, then the installed/web OAuth blocks. Empty means unresolved.
func keeperAntigravityProjectID(detail map[string]any) string {
	if v := keeperStringFirst(detail["project_id"], detail["projectId"]); v != "" {
		return v
	}
	if m, ok := detail["metadata"].(map[string]any); ok {
		if v := keeperStringFirst(m["project_id"], m["projectId"]); v != "" {
			return v
		}
	}
	if m, ok := detail["attributes"].(map[string]any); ok {
		if v := keeperStringFirst(m["project_id"], m["projectId"], m["gemini_virtual_project"]); v != "" {
			return v
		}
	}
	if m, ok := detail["installed"].(map[string]any); ok {
		if v := keeperStringFirst(m["project_id"], m["projectId"]); v != "" {
			return v
		}
	}
	if m, ok := detail["web"].(map[string]any); ok {
		if v := keeperStringFirst(m["project_id"], m["projectId"]); v != "" {
			return v
		}
	}
	return ""
}

// parseAntigravityQuotaGroups projects a retrieveUserQuotaSummary body into groups → buckets.
// A group is kept only when it has at least one valid bucket (a valid remaining fraction; a
// present-but-unparseable reset time drops just that bucket). ok=false means the body had no
// usable groups, so the caller preserves the previous snapshot rather than trusting an empty/
// malformed response.
func parseAntigravityQuotaGroups(body map[string]any) ([]keeperAntigravityGroup, bool) {
	if body == nil {
		return nil, false
	}
	rawGroups, ok := body["groups"].([]any)
	if !ok {
		return nil, false
	}
	groups := make([]keeperAntigravityGroup, 0, len(rawGroups))
	for _, g := range rawGroups {
		gm, ok := g.(map[string]any)
		if !ok {
			continue
		}
		rawBuckets, ok := gm["buckets"].([]any)
		if !ok {
			continue
		}
		buckets := make([]keeperAntigravityBucket, 0, len(rawBuckets))
		for _, b := range rawBuckets {
			bm, ok := b.(map[string]any)
			if !ok {
				continue
			}
			fraction, ok := keeperParseFraction(bm["remainingFraction"], bm["remaining_fraction"])
			if !ok {
				continue
			}
			var resetRaw any = bm["resetTime"]
			if resetRaw == nil {
				resetRaw = bm["reset_time"]
			}
			resetAt, ok := keeperParseOptionalTime(resetRaw)
			if !ok {
				continue
			}
			buckets = append(buckets, keeperAntigravityBucket{
				BucketID:          keeperStringFirst(bm["bucketId"], bm["bucket_id"]),
				DisplayName:       keeperStringFirst(bm["displayName"], bm["display_name"]),
				Window:            keeperString(bm["window"]),
				RemainingFraction: fraction,
				ResetAt:           resetAt,
				Description:       keeperString(bm["description"]),
			})
		}
		if len(buckets) == 0 {
			continue
		}
		groups = append(groups, keeperAntigravityGroup{
			DisplayName: keeperStringFirst(gm["displayName"], gm["display_name"]),
			Description: keeperString(gm["description"]),
			Buckets:     buckets,
		})
	}
	if len(groups) == 0 {
		return nil, false
	}
	return groups, true
}

// processKeeperAntigravityAuth inspects one Antigravity account: it reads the download detail,
// records the identity/health fields, and (unless disabled) fetches the quota summary. It is a
// SEPARATE path from the Codex inspection — no ChatGPT usage/reset-credit/subscription/priority
// logic applies. A failed quota fetch preserves the previous snapshot (AntigravityQuota nil →
// COALESCE) and records a network error rather than wiping the account.
func (a *App) processKeeperAntigravityAuth(ctx context.Context, cfg AppConfig, authInfo map[string]any, logFn func(string), manualRefresh bool) keeperAccountResult {
	now := time.Now().In(appTimeLocation)
	name := keeperString(authInfo["name"])
	if name == "" {
		name = "unknown"
	}
	provider := keeperProviderAntigravity
	result := keeperAccountResult{Name: name, Result: "skipped", CheckedAt: now, Provider: &provider}
	persist := func(r keeperAccountResult) keeperAccountResult {
		if err := a.upsertKeeperState(ctx, r); err != nil {
			logFn(r.Name + "：状态写回失败（state_write_error）")
			log.Printf("codex keeper antigravity state write-back failed for %s: %v", r.Name, err)
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
	merged := mergeKeeperObjects(authInfo, detail)
	result.Email = keeperStringPtr(merged["email"], merged["account_email"], merged["user_email"])
	result.AuthIndex = keeperRemoteAuthIndex(merged)
	result.Priority = keeperIntPtr(merged["priority"])
	disabled := keeperBool(merged["disabled"])
	result.Disabled = &disabled
	if disabled && !manualRefresh {
		result.Result = "disabled"
		result = persist(result)
		return result
	}
	if groups, ok := a.fetchAntigravityQuota(ctx, cfg, merged); ok {
		if encoded, err := json.Marshal(groups); err == nil {
			payload := string(encoded)
			result.AntigravityQuota = &payload
		}
		result.Result = "healthy"
		logFn(fmt.Sprintf("%s：Antigravity 配额刷新成功（%d 组）", name, len(groups)))
	} else {
		message := "Antigravity 配额读取失败"
		result.Result = "network_error"
		result.LastError = &message
		result.LatestAction = &message
		logFn(name + "：" + message)
	}
	result = persist(result)
	return result
}

// fetchAntigravityQuota pulls the account's Antigravity quota summary through the per-auth
// api-call egress (same $TOKEN$/proxy plumbing as the Codex checks). It requires a resolvable
// project id; a transport error, non-2xx outer/inner status, or unparseable body on every
// candidate host leaves both return values empty (ok=false) so the caller preserves the prior
// snapshot instead of wiping it.
func (a *App) fetchAntigravityQuota(ctx context.Context, cfg AppConfig, detail map[string]any) ([]keeperAntigravityGroup, bool) {
	projectID := keeperAntigravityProjectID(detail)
	if projectID == "" {
		return nil, false
	}
	authIndex := keeperAuthIndex(detail)
	dataBytes, err := json.Marshal(map[string]string{"project": projectID})
	if err != nil {
		return nil, false
	}
	header := map[string]string{
		"Authorization": "Bearer $TOKEN$",
		"Content-Type":  "application/json",
		"User-Agent":    antigravityQuotaUserAgent,
	}
	for _, quotaURL := range antigravityQuotaURLs {
		body := map[string]any{
			"auth_index": authIndex,
			"method":     "POST",
			"url":        quotaURL,
			"header":     header,
			"data":       string(dataBytes),
		}
		response, payload, err := a.keeperRequest(ctx, cfg, http.MethodPost, "/v0/management/api-call", nil, body, time.Duration(cfg.CodexKeeper.UsageTimeoutSeconds)*time.Second)
		if err != nil {
			continue
		}
		if response.StatusCode < 200 || response.StatusCode >= 300 {
			continue
		}
		var raw map[string]any
		if err := json.Unmarshal(payload, &raw); err != nil {
			continue
		}
		if !keeperInnerStatusOK(raw) {
			continue
		}
		if groups, ok := parseAntigravityQuotaGroups(keeperBodyJSON(raw["body"])); ok {
			return groups, true
		}
	}
	return nil, false
}

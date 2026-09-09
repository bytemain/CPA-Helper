package app

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
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

// keeperAntigravityBucketResponse / keeperAntigravityGroupResponse are the API projection of the
// stored quota, with reset times formatted the same way as every other keeper API timestamp.
type keeperAntigravityBucketResponse struct {
	BucketID          string  `json:"bucket_id"`
	DisplayName       string  `json:"display_name"`
	Window            string  `json:"window"`
	RemainingFraction float64 `json:"remaining_fraction"`
	ResetAt           *string `json:"reset_at"`
	Description       string  `json:"description,omitempty"`
}

type keeperAntigravityGroupResponse struct {
	DisplayName string                            `json:"display_name"`
	Description string                            `json:"description,omitempty"`
	Buckets     []keeperAntigravityBucketResponse `json:"buckets"`
}

// keeperAntigravityQuotaResponses projects stored quota groups for the /accounts API response.
func keeperAntigravityQuotaResponses(groups []keeperAntigravityGroup) []keeperAntigravityGroupResponse {
	if len(groups) == 0 {
		return nil
	}
	out := make([]keeperAntigravityGroupResponse, 0, len(groups))
	for _, g := range groups {
		buckets := make([]keeperAntigravityBucketResponse, 0, len(g.Buckets))
		for _, b := range g.Buckets {
			buckets = append(buckets, keeperAntigravityBucketResponse{
				BucketID:          b.BucketID,
				DisplayName:       b.DisplayName,
				Window:            b.Window,
				RemainingFraction: b.RemainingFraction,
				ResetAt:           apiDateTimePtr(b.ResetAt),
				Description:       b.Description,
			})
		}
		out = append(out, keeperAntigravityGroupResponse{DisplayName: g.DisplayName, Description: g.Description, Buckets: buckets})
	}
	return out
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

// antigravityIdentity is the reconciled routing identity for an Antigravity inspection.
type antigravityIdentity struct {
	authIndex string
	projectID string
	email     string
}

// keeperReconcileAntigravityIdentity validates that the list entry and download detail describe
// the SAME Antigravity account before any quota call: the list type is antigravity, the detail
// (when it states them) agrees on type and name, the auth_index is explicit on each source (never
// the auth NAME), reconciles across them, and is non-empty, and a project_id is resolvable from
// the detail. ok=false on any conflict/missing field so the caller fails closed.
func keeperReconcileAntigravityIdentity(authInfo, detail map[string]any, name string) (antigravityIdentity, bool) {
	// Every identity field distinguishes absent/null from present-but-wrong-type: a present field
	// that is not the expected type is illegal (a malformed/deceptive detail) and fails closed,
	// rather than being treated as "missing" and silently backfilled from the list.
	listType, lterr := keeperExplicitStringField(authInfo, "type")
	if lterr != nil || listType != keeperProviderAntigravity {
		return antigravityIdentity{}, false
	}
	detailType, dterr := keeperExplicitStringField(detail, "type")
	if dterr != nil {
		return antigravityIdentity{}, false
	}
	if detailType != "" && detailType != keeperProviderAntigravity {
		return antigravityIdentity{}, false
	}
	detailName, dnerr := keeperExplicitStringField(detail, "name")
	if dnerr != nil {
		return antigravityIdentity{}, false
	}
	if detailName != "" && detailName != name {
		return antigravityIdentity{}, false
	}
	listIdx, lierr := keeperExplicitAuthIndex(authInfo)
	detailIdx, dierr := keeperExplicitAuthIndex(detail)
	if lierr != nil || dierr != nil {
		return antigravityIdentity{}, false
	}
	idx, ierr := keeperReconcileIdentityField(listIdx, detailIdx)
	if ierr != nil || strings.TrimSpace(idx) == "" {
		return antigravityIdentity{}, false
	}
	// The CPA list entry also exposes project_id; when BOTH sources carry one they must agree
	// (a same-name file swap / memory-vs-disk drift would otherwise let the detail's project run a
	// quota call for a different account). A non-empty project must be resolvable.
	listProject, lperr := keeperExplicitAntigravityProjectID(authInfo)
	detailProject, dperr := keeperExplicitAntigravityProjectID(detail)
	if lperr != nil || dperr != nil {
		return antigravityIdentity{}, false
	}
	project, perr := keeperReconcileIdentityField(listProject, detailProject)
	if perr != nil || strings.TrimSpace(project) == "" {
		return antigravityIdentity{}, false
	}
	// A Google project can be shared across accounts, so the project alone does not identify the
	// account. Reconcile the account email across list+detail (present must be a string; both
	// present must agree) and fold it into the stored identity digest, so a credential swap to a
	// different email under the same project is detected as an identity change.
	listEmail, leerr := keeperExplicitStringField(authInfo, "email")
	detailEmail, deerr := keeperExplicitStringField(detail, "email")
	if leerr != nil || deerr != nil {
		return antigravityIdentity{}, false
	}
	email, eerr := keeperReconcileIdentityField(listEmail, detailEmail)
	if eerr != nil {
		return antigravityIdentity{}, false
	}
	return antigravityIdentity{authIndex: idx, projectID: project, email: email}, true
}

// keeperProjectDigest returns a stable, versioned one-way digest of a project id. Only this digest
// is persisted (never the raw project id), so the DB can detect a project swap without storing or
// exposing the project identifier. The "v1:" prefix allows changing the scheme later.
// keeperAntigravityIdentityDigest returns a stable, versioned one-way digest of the Antigravity
// account identity (provider + project id + normalized email). Only this digest is persisted —
// never the raw project id or email — so the DB can detect an identity swap (project OR email
// changed) and clear the stale quota without storing or exposing the identifiers. A change to any
// confirmed component yields a different digest.
func keeperAntigravityIdentityDigest(projectID, email string) string {
	normEmail := strings.ToLower(strings.TrimSpace(email))
	sum := sha256.Sum256([]byte(keeperProviderAntigravity + "\x00" + projectID + "\x00" + normEmail))
	return "v1:" + hex.EncodeToString(sum[:])
}

// keeperExplicitAntigravityProjectID resolves the project id with present-invalid detection: the
// top-level project_id / projectId must be a string when present (a present-but-wrong-type value
// fails closed). When the top level is absent it falls back to the lenient nested resolution
// (metadata / attributes / installed / web).
func keeperExplicitAntigravityProjectID(o map[string]any) (string, error) {
	pid, err := keeperExplicitStringField(o, "project_id")
	if err != nil {
		return "", err
	}
	if pid != "" {
		return pid, nil
	}
	pidCamel, err := keeperExplicitStringField(o, "projectId")
	if err != nil {
		return "", err
	}
	if pidCamel != "" {
		return pidCamel, nil
	}
	return keeperAntigravityProjectID(o), nil
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
	// Bind identity FIRST (mirror the Codex rigor): the download must be for THIS account name,
	// still be an Antigravity account, and carry an explicit auth_index consistent with the list
	// entry (no filename fallback). Any mismatch mixes accounts, so fail closed as identity_error,
	// preserve the prior snapshot, and make NO quota call.
	identity, ok := keeperReconcileAntigravityIdentity(authInfo, detail, name)
	if !ok {
		message := "账号身份冲突：Antigravity 列表与详情的 name/type/auth_index/project_id 不一致，已保留原快照"
		result.Result = "identity_error"
		result.LastError = &message
		result.LatestAction = &message
		if err := a.markKeeperIdentityError(ctx, name, &message, result.CheckedAt); err != nil {
			logFn(name + "：状态写回失败（state_write_error）")
			log.Printf("codex keeper antigravity identity-error write-back failed for %s: %v", name, err)
			result.StateWriteFailed = true
		}
		logFn(name + "：" + message)
		return result
	}
	merged := mergeKeeperObjects(authInfo, detail)
	result.Email = keeperStringPtr(merged["email"], merged["account_email"], merged["user_email"])
	idx := identity.authIndex
	result.AuthIndex = &idx
	// Bind a one-way DIGEST of the resolved account identity (provider + project + email; never the
	// raw values) regardless of the fetch outcome, so a later inspection can detect an identity swap
	// (project OR email changed) and clear the stale quota even when the fresh quota fetch fails —
	// without persisting the project or email.
	digest := keeperAntigravityIdentityDigest(identity.projectID, identity.email)
	result.AntigravityIdentityDigest = &digest
	result.Priority = keeperIntPtr(merged["priority"])
	disabled := keeperBool(merged["disabled"])
	result.Disabled = &disabled
	if disabled && !manualRefresh {
		result.Result = "disabled"
		result = persist(result)
		return result
	}
	if groups, ok := a.fetchAntigravityQuota(ctx, cfg, identity.authIndex, identity.projectID); ok {
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
func (a *App) fetchAntigravityQuota(ctx context.Context, cfg AppConfig, authIndex, projectID string) ([]keeperAntigravityGroup, bool) {
	if strings.TrimSpace(authIndex) == "" || strings.TrimSpace(projectID) == "" {
		return nil, false
	}
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

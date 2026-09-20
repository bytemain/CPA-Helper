package accountrunway

import (
	"database/sql"
	"encoding/json"
	"math"
	"regexp"
	"sort"
	"strings"
	"time"

	backendApp "cpa-helper/backend/internal/app"
)

// emailPattern mirrors app.usageEmailPattern: production writes source_account
// as the lowercased email extracted from the usage `source` field.
var emailPattern = regexp.MustCompile(`(?i)[a-z0-9._%+\-]+@[a-z0-9.\-]+\.[a-z]{2,}`)

const (
	// Default window fallbacks when a codex account row carries no window
	// seconds: the 5h primary bucket and the weekly secondary bucket.
	codexPrimaryWindowFallbackSeconds   = int64(18000)
	codexSecondaryWindowFallbackSeconds = int64(604800)
	secondsPerWeek                      = float64(604800)

	StatusHealthy  = "HEALTHY"
	StatusCritical = "CRITICAL"
	StatusUnknown  = "UNKNOWN"
)

var statusRank = map[string]int{StatusCritical: 0, StatusUnknown: 1, StatusHealthy: 2}

type antigravityGroup struct {
	DisplayName string              `json:"display_name"`
	Description string              `json:"description,omitempty"`
	Buckets     []antigravityBucket `json:"buckets"`
}

type antigravityBucket struct {
	BucketID          string     `json:"bucket_id"`
	DisplayName       string     `json:"display_name"`
	Window            string     `json:"window"`
	RemainingFraction float64    `json:"remaining_fraction"`
	ResetAt           *time.Time `json:"reset_at"`
	Description       string     `json:"description,omitempty"`
}

// parseAntigravityQuota decodes the stored antigravity_quota JSON blob. NULL,
// empty, or malformed yields nil (no quota snapshot).
func parseAntigravityQuota(value sql.NullString) []antigravityGroup {
	if !value.Valid || strings.TrimSpace(value.String) == "" {
		return nil
	}
	var groups []antigravityGroup
	if err := json.Unmarshal([]byte(value.String), &groups); err != nil {
		return nil
	}
	return groups
}

// BucketReport is the computed runway state of one quota window of one account.
type BucketReport struct {
	Name              string   `json:"name"`
	RemainingFraction float64  `json:"remaining_fraction"`
	ResetAtText       string   `json:"reset_at,omitempty"`
	WindowSeconds     int64    `json:"window_seconds,omitempty"`
	Cap               *float64 `json:"cap_tokens,omitempty"`
	RemainingTokens   *float64 `json:"remaining_tokens,omitempty"`
	RunwayHours       *float64 `json:"runway_hours,omitempty"`
	HoursUntilReset   *float64 `json:"hours_until_reset,omitempty"`
	Coverage          *float64 `json:"coverage,omitempty"`
	Status            string   `json:"status"`
	Notes             []string `json:"notes,omitempty"`

	resetAt *time.Time
}

// AccountReport is one keeper account's runway summary.
type AccountReport struct {
	Name        string         `json:"name"`
	Email       string         `json:"email"`
	Provider    string         `json:"provider"`
	Disabled    bool           `json:"disabled"`
	BurnTokens  int64          `json:"burn_tokens"`
	BurnPerHour float64        `json:"burn_per_hour"`
	Status      string         `json:"status"`
	Buckets     []BucketReport `json:"buckets"`
}

// Report is the full account-runway result.
type Report struct {
	GeneratedAt        string          `json:"generated_at"`
	SinceDays          int             `json:"since_days"`
	Provider           string          `json:"provider"`
	Accounts           []AccountReport `json:"accounts"`
	UnattributedRows   int             `json:"unattributed_rows"`
	UnattributedTokens int64           `json:"unattributed_tokens"`
	Pool               PoolReport      `json:"pool"`
}

type PoolReport struct {
	EnabledAccounts      int      `json:"enabled_accounts"`
	DisabledAccounts     int      `json:"disabled_accounts"`
	CriticalAccounts     []string `json:"critical_accounts"`
	GapTokens            float64  `json:"gap_tokens"`
	AvgWeeklyQuotaTokens *float64 `json:"avg_weekly_quota_tokens,omitempty"`
	AdditionalAccounts   int      `json:"additional_accounts"`
	Recommendation       string   `json:"recommendation"`
}

// Compute builds the runway report from loaded rows.
func Compute(accounts []Account, rows []UsageRow, providerFilter string, sinceDays int, now time.Time) Report {
	burn := attributeBurn(accounts, rows)
	report := Report{
		GeneratedAt: backendApp.UsageDBTime(now),
		SinceDays:   sinceDays,
		Provider:    providerFilter,
	}
	windowHours := float64(sinceDays) * 24

	for _, account := range accounts {
		if providerFilter != "all" && account.Provider != providerFilter {
			continue
		}
		ar := AccountReport{
			Name:        account.Name,
			Email:       account.Email,
			Provider:    account.Provider,
			Disabled:    account.Disabled,
			BurnTokens:  burn.tokens[account.Name],
			BurnPerHour: float64(burn.tokens[account.Name]) / windowHours,
		}
		ar.Buckets = bucketsForAccount(account, ar.BurnTokens, float64(sinceDays)*3600, now)
		ar.Status = worstStatus(ar.Buckets)
		report.Accounts = append(report.Accounts, ar)
	}
	sortAccounts(report.Accounts)
	report.UnattributedRows = burn.rows
	report.UnattributedTokens = burn.unattributedTokens
	report.Pool = poolRecommendation(report.Accounts)
	return report
}

// poolRecommendation computes the quota gap (enabled accounts that run dry
// before their next reset) and how many extra accounts -- sized at the average
// enabled account's weekly-equivalent cap -- would cover it.
func poolRecommendation(accounts []AccountReport) PoolReport {
	pool := PoolReport{}
	var gap, weeklyCapSum float64
	var weeklyCapCount int
	for _, ar := range accounts {
		if ar.Disabled {
			pool.DisabledAccounts++
			continue
		}
		pool.EnabledAccounts++
		if ar.Status == StatusCritical {
			pool.CriticalAccounts = append(pool.CriticalAccounts, ar.Name)
		}
		var bestWeekly float64
		for _, b := range ar.Buckets {
			if b.Cap != nil && b.WindowSeconds > 0 {
				if weekly := *b.Cap * (secondsPerWeek / float64(b.WindowSeconds)); weekly > bestWeekly {
					bestWeekly = weekly
				}
			}
			if b.Status != StatusCritical || b.HoursUntilReset == nil || b.RemainingTokens == nil {
				continue
			}
			need := ar.BurnPerHour * *b.HoursUntilReset
			if need > *b.RemainingTokens {
				gap += need - *b.RemainingTokens
			}
		}
		if bestWeekly > 0 {
			weeklyCapSum += bestWeekly
			weeklyCapCount++
		}
	}
	pool.GapTokens = gap
	if weeklyCapCount > 0 {
		avg := weeklyCapSum / float64(weeklyCapCount)
		pool.AvgWeeklyQuotaTokens = &avg
	}
	switch {
	case gap <= 0:
		pool.Recommendation = "pool covers all enabled accounts until next reset"
	case pool.AvgWeeklyQuotaTokens == nil || *pool.AvgWeeklyQuotaTokens <= 0:
		pool.Recommendation = "pool runs dry before next reset; additional accounts needed but average weekly quota is unknown"
	default:
		pool.AdditionalAccounts = int(math.Ceil(gap / *pool.AvgWeeklyQuotaTokens))
		pool.Recommendation = "pool runs dry before next reset; add accounts to cover the quota gap"
	}
	return pool
}

type burnAttribution struct {
	tokens             map[string]int64
	rows               int
	unattributedTokens int64
}

// attributeBurn attributes usage rows to accounts the way production does
// (keeperAccountNameForUsageRecord): source_account (already the extracted
// email) wins; otherwise the email is extracted from `source`, else auth_index.
// A row whose key matches no account, or matches several, is unattributed.
func attributeBurn(accounts []Account, rows []UsageRow) burnAttribution {
	type aliasSet struct {
		byKey     map[string]string
		ambiguous map[string]bool
		add       func(key, name string)
	}
	newSet := func() *aliasSet {
		set := &aliasSet{byKey: map[string]string{}, ambiguous: map[string]bool{}}
		set.add = func(key, name string) {
			key = strings.ToLower(strings.TrimSpace(key))
			if key == "" {
				return
			}
			if existing, ok := set.byKey[key]; ok {
				if existing != name {
					set.ambiguous[key] = true
				}
				return
			}
			set.byKey[key] = name
		}
		return set
	}
	emails, indexes := newSet(), newSet()
	for _, account := range accounts {
		emails.add(account.Email, account.Name)
		if match := emailPattern.FindString(account.Name); match != "" {
			emails.add(match, account.Name)
		}
		indexes.add(account.Name, account.Name)
		indexes.add(account.AuthIndex, account.Name)
	}

	result := burnAttribution{tokens: map[string]int64{}}
	unattributed := func(row UsageRow) {
		result.rows++
		result.unattributedTokens += row.TotalTokens
	}
	for _, row := range rows {
		email := strings.ToLower(strings.TrimSpace(row.SourceAccount))
		if email == "" {
			email = strings.ToLower(emailPattern.FindString(row.Source))
		}
		var name string
		switch {
		case email != "":
			if emails.ambiguous[email] {
				unattributed(row)
				continue
			}
			n, ok := emails.byKey[email]
			if !ok {
				unattributed(row)
				continue
			}
			name = n
		case strings.TrimSpace(row.AuthIndex) != "":
			index := strings.ToLower(strings.TrimSpace(row.AuthIndex))
			if indexes.ambiguous[index] {
				unattributed(row)
				continue
			}
			n, ok := indexes.byKey[index]
			if !ok {
				unattributed(row)
				continue
			}
			name = n
		default:
			unattributed(row)
			continue
		}
		result.tokens[name] += row.TotalTokens
	}
	return result
}

func bucketsForAccount(account Account, burnTokens int64, burnWindowSeconds float64, now time.Time) []BucketReport {
	if account.Provider == "antigravity" {
		return antigravityBuckets(account, burnTokens, burnWindowSeconds, now)
	}
	return codexBuckets(account, burnTokens, burnWindowSeconds, now)
}

func codexBuckets(account Account, burnTokens int64, burnWindowSeconds float64, now time.Time) []BucketReport {
	out := []BucketReport{}
	if account.PrimaryUsedPercent != nil {
		window := codexPrimaryWindowFallbackSeconds
		if account.PrimaryWindowSeconds != nil && *account.PrimaryWindowSeconds > 0 {
			window = *account.PrimaryWindowSeconds
		}
		out = append(out, finishBucket(BucketReport{
			Name:              "primary",
			RemainingFraction: fractionFromPercent(*account.PrimaryUsedPercent),
			WindowSeconds:     window,
			resetAt:           account.PrimaryResetAt,
		}, burnTokens, burnWindowSeconds, now))
	}
	if account.SecondaryUsedPercent != nil {
		window := codexSecondaryWindowFallbackSeconds
		if account.SecondaryWindowSeconds != nil && *account.SecondaryWindowSeconds > 0 {
			window = *account.SecondaryWindowSeconds
		}
		out = append(out, finishBucket(BucketReport{
			Name:              "secondary",
			RemainingFraction: fractionFromPercent(*account.SecondaryUsedPercent),
			WindowSeconds:     window,
			resetAt:           account.SecondaryResetAt,
		}, burnTokens, burnWindowSeconds, now))
	}
	if len(out) == 0 {
		out = append(out, BucketReport{Name: "quota", Status: StatusUnknown, Notes: []string{"no quota snapshot recorded"}})
	}
	return out
}

func fractionFromPercent(used int64) float64 {
	fraction := 1 - float64(used)/100
	return math.Max(0, math.Min(1, fraction))
}

func antigravityBuckets(account Account, burnTokens int64, burnWindowSeconds float64, now time.Time) []BucketReport {
	out := []BucketReport{}
	for _, group := range account.AntigravityGroups {
		for _, bucket := range group.Buckets {
			name := bucket.DisplayName
			if name == "" {
				name = bucket.BucketID
			}
			if name == "" {
				name = "bucket"
			}
			if group.DisplayName != "" {
				name = group.DisplayName + "/" + name
			}
			report := BucketReport{
				Name:              name,
				RemainingFraction: bucket.RemainingFraction,
				resetAt:           bucket.ResetAt,
			}
			window, ok := parseAntigravityWindow(bucket.Window)
			if !ok {
				report.Status = StatusUnknown
				report.Notes = append(report.Notes, "window_unknown")
				if bucket.ResetAt != nil {
					report.ResetAtText = backendApp.UsageDBTime(*bucket.ResetAt)
				}
				out = append(out, report)
				continue
			}
			report.WindowSeconds = window
			out = append(out, finishBucket(report, burnTokens, burnWindowSeconds, now))
		}
	}
	if len(out) == 0 {
		out = append(out, BucketReport{Name: "quota", Status: StatusUnknown, Notes: []string{"no quota snapshot recorded"}})
	}
	return out
}

// parseAntigravityWindow maps the bucket's `window` label to seconds. Known
// labels: named windows ("weekly", "monthly", "daily", "5h") and Go durations.
// Anything else is window_unknown -- excluded from runway math, still shown.
func parseAntigravityWindow(window string) (int64, bool) {
	text := strings.ToLower(strings.TrimSpace(window))
	if text == "" {
		return 0, false
	}
	switch {
	case strings.Contains(text, "month"):
		return 2592000, true
	case strings.Contains(text, "week"):
		return 604800, true
	case strings.Contains(text, "day"):
		return 86400, true
	}
	if d, err := time.ParseDuration(text); err == nil && d > 0 {
		return int64(d.Seconds()), true
	}
	return 0, false
}

// finishBucket fills in derived fields: reset text, hours until reset, cap
// estimate, runway hours, coverage, status.
//
// Cap estimation (方案 B): no absolute quota is stored, so the cap is inferred
// as consumed_in_window / (1 - remaining_fraction). The window's consumption is
// approximated from the observed burn: elapsed_in_window / burn_window of the
// measured tokens. Consumption of zero (or fraction == 1) makes the cap
// indeterminate -- marked cap_unknown rather than guessed.
func finishBucket(b BucketReport, burnTokens int64, burnWindowSeconds float64, now time.Time) BucketReport {
	var elapsed float64
	if b.resetAt != nil {
		b.ResetAtText = backendApp.UsageDBTime(*b.resetAt)
		hours := b.resetAt.Sub(now).Hours()
		b.HoursUntilReset = &hours
		elapsed = float64(b.WindowSeconds) - b.resetAt.Sub(now).Seconds()
		if elapsed < 0 {
			// Reset lies further out than the window length: snapshot is
			// inconsistent, treat the whole window as consumed-at-risk.
			elapsed = float64(b.WindowSeconds)
		}
		if elapsed > float64(b.WindowSeconds) {
			elapsed = float64(b.WindowSeconds)
		}
	} else {
		b.Notes = append(b.Notes, "reset_unknown")
	}

	var consumed float64
	if elapsed > 0 {
		if elapsed <= burnWindowSeconds {
			consumed = float64(burnTokens)
		} else {
			consumed = float64(burnTokens) * elapsed / burnWindowSeconds
			// The cap estimate leans on scaling a short observation up to the
			// elapsed window; flag it so readers weigh confidence accordingly.
			b.Notes = append(b.Notes, "short_observation")
		}
	}
	if consumed > 0 && b.RemainingFraction < 1 {
		cap := consumed / (1 - b.RemainingFraction)
		b.Cap = &cap
		remaining := cap * b.RemainingFraction
		b.RemainingTokens = &remaining
		burnPerHour := float64(burnTokens) / (burnWindowSeconds / 3600)
		runway := math.Inf(1)
		if burnPerHour > 0 {
			runway = remaining / burnPerHour
		}
		b.RunwayHours = &runway
		if b.HoursUntilReset != nil && *b.HoursUntilReset > 0 {
			coverage := runway / *b.HoursUntilReset
			b.Coverage = &coverage
			if coverage >= 1 {
				b.Status = StatusHealthy
			} else {
				b.Status = StatusCritical
			}
		} else {
			// Reset already due/passed: the window refreshes imminently, so the
			// bucket is not what limits the account.
			b.Status = StatusHealthy
		}
	} else {
		b.Notes = append(b.Notes, "cap_unknown")
	}
	if b.Status == "" {
		b.Status = StatusUnknown
	}
	return b
}

func worstStatus(buckets []BucketReport) string {
	worst := StatusHealthy
	for _, b := range buckets {
		if statusRank[b.Status] < statusRank[worst] {
			worst = b.Status
		}
	}
	return worst
}

// sortAccounts orders reports by status severity then name for stable output.
func sortAccounts(accounts []AccountReport) {
	sort.SliceStable(accounts, func(i, j int) bool {
		ri, rj := statusRank[accounts[i].Status], statusRank[accounts[j].Status]
		if ri != rj {
			return ri < rj
		}
		return accounts[i].Name < accounts[j].Name
	})
}

package accountrunway

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	backendApp "cpa-helper/backend/internal/app"

	_ "modernc.org/sqlite"
)

// OpenReadOnly opens the CPA-Helper database for reporting only.
//
// Two independent guards, because this points at production data: the DSN asks
// SQLite for a read-only connection, and `query_only` is then asserted on the
// connection that was actually handed back. The second is not redundant -- a
// future change to the DSN (adding a parameter, switching helper) can quietly
// drop `mode=ro`, and without the assertion the first write would succeed
// instead of failing.
func OpenReadOnly(ctx context.Context, path string) (*sql.DB, error) {
	db, err := sql.Open("sqlite", fmt.Sprintf("file:%s?mode=ro&_pragma=query_only(1)&_pragma=busy_timeout(5000)", path))
	if err != nil {
		return nil, err
	}
	// One connection: a pool would need the pragma re-asserted per connection,
	// and this tool has no concurrency to gain from more.
	db.SetMaxOpenConns(1)
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("open %s read-only: %w", path, err)
	}
	var queryOnly int
	if err := db.QueryRowContext(ctx, "PRAGMA query_only").Scan(&queryOnly); err != nil {
		db.Close()
		return nil, fmt.Errorf("read query_only pragma: %w", err)
	}
	if queryOnly != 1 {
		db.Close()
		return nil, fmt.Errorf("refusing to continue: connection to %s is not query_only", path)
	}
	return db, nil
}

// Account is one keeper account row (codex_keeper_auth_states) plus the
// decoded antigravity quota snapshot when the account is an antigravity one.
type Account struct {
	Name                   string
	Email                  string
	AuthIndex              string
	Disabled               bool
	Provider               string // "codex" (default) or "antigravity"
	PrimaryUsedPercent     *int64
	SecondaryUsedPercent   *int64
	PrimaryResetAt         *time.Time
	SecondaryResetAt       *time.Time
	PrimaryWindowSeconds   *int64
	SecondaryWindowSeconds *int64
	AntigravityGroups      []antigravityGroup
}

// UsageRow is a usage_records row inside the burn-rate window, carrying only
// what attribution and aggregation need.
type UsageRow struct {
	SourceAccount string
	Source        string
	AuthIndex     string
	TotalTokens   int64
	Failed        bool
}

// LoadAccounts reads every keeper account. Provider NULL/empty normalises to
// "codex" -- rows predating multi-provider support are codex.
func LoadAccounts(ctx context.Context, db *sql.DB) ([]Account, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT auth_name, auth_index, email, disabled, provider,
		       primary_used_percent, secondary_used_percent,
		       primary_reset_at, secondary_reset_at,
		       primary_window_seconds, secondary_window_seconds,
		       antigravity_quota
		FROM codex_keeper_auth_states
		ORDER BY auth_name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	accounts := []Account{}
	for rows.Next() {
		var (
			account                                Account
			authName, authIndex, email             sql.NullString
			disabled                               bool
			provider, primaryReset, secondaryReset sql.NullString
			primaryUsed, secondaryUsed             sql.NullInt64
			primaryWindow, secondaryWindow         sql.NullInt64
			antigravityQuota                       sql.NullString
		)
		if err := rows.Scan(&authName, &authIndex, &email, &disabled, &provider,
			&primaryUsed, &secondaryUsed, &primaryReset, &secondaryReset,
			&primaryWindow, &secondaryWindow, &antigravityQuota); err != nil {
			return nil, err
		}
		account.Name = nullableString(authName)
		account.Email = nullableString(email)
		account.AuthIndex = nullableString(authIndex)
		account.Disabled = disabled
		account.Provider = "codex"
		if p := nullableString(provider); p != "" {
			account.Provider = p
		}
		account.PrimaryUsedPercent = nullableInt(primaryUsed)
		account.SecondaryUsedPercent = nullableInt(secondaryUsed)
		account.PrimaryWindowSeconds = nullableInt(primaryWindow)
		account.SecondaryWindowSeconds = nullableInt(secondaryWindow)
		if t, ok := backendApp.UsageParseDBTime(primaryReset.String); primaryReset.Valid && ok {
			account.PrimaryResetAt = &t
		}
		if t, ok := backendApp.UsageParseDBTime(secondaryReset.String); secondaryReset.Valid && ok {
			account.SecondaryResetAt = &t
		}
		account.AntigravityGroups = parseAntigravityQuota(antigravityQuota)
		accounts = append(accounts, account)
	}
	return accounts, rows.Err()
}

// LoadUsageRows reads usage rows at or after `since`. The bound is the UsageDBTime()
// byte shape production writes to the TEXT `timestamp` column -- a time.Time
// bound would be serialised by the driver as "2006-01-02 15:04:05 +0000 UTC"
// (space separator), and ' ' < 'T' makes the lexicographic >= let older
// same-date rows through. Binding the same layout production writes is the
// only way the comparison means what it says.
func LoadUsageRows(ctx context.Context, db *sql.DB, since time.Time) ([]UsageRow, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT source_account, source, auth_index, total_tokens, failed
		FROM usage_records
		WHERE timestamp >= ?
		ORDER BY id`, backendApp.UsageDBTime(since))
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	result := []UsageRow{}
	for rows.Next() {
		var (
			row                              UsageRow
			sourceAccount, source, authIndex sql.NullString
		)
		if err := rows.Scan(&sourceAccount, &source, &authIndex, &row.TotalTokens, &row.Failed); err != nil {
			return nil, err
		}
		row.SourceAccount = nullableString(sourceAccount)
		row.Source = nullableString(source)
		row.AuthIndex = nullableString(authIndex)
		result = append(result, row)
	}
	return result, rows.Err()
}

func nullableString(v sql.NullString) string {
	if !v.Valid {
		return ""
	}
	return v.String
}

func nullableInt(v sql.NullInt64) *int64 {
	if !v.Valid {
		return nil
	}
	value := v.Int64
	return &value
}

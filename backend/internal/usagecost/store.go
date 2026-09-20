package usagecost

import (
	"context"
	"database/sql"
	"fmt"
	"time"

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
	db, err := sql.Open("sqlite", fmt.Sprintf("file:%s?mode=ro&_pragma=query_only(1)", path))
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

// LoadPrices reads the price table into the map shape the cost derivation
// expects. Keys are lowercased and trimmed by SQLite so the lookup matches
// CPA-Helper's, which does the same normalisation in Go.
func LoadPrices(ctx context.Context, db *sql.DB) (map[PriceKey]ModelPrice, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT lower(trim(provider)), lower(trim(model)),
		       input_usd_per_million, output_usd_per_million,
		       cache_read_usd_per_million, cache_creation_usd_per_million,
		       request_usd
		FROM model_prices`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	prices := map[PriceKey]ModelPrice{}
	for rows.Next() {
		var (
			provider, model string
			price           ModelPrice
			requestUSD      sql.NullFloat64
		)
		if err := rows.Scan(&provider, &model,
			&price.InputUSDPerMillion, &price.OutputUSDPerMillion,
			&price.CacheReadUSDPerMillion, &price.CacheCreationUSDPerMillion,
			&requestUSD); err != nil {
			return nil, err
		}
		if requestUSD.Valid {
			value := requestUSD.Float64
			price.RequestUSD = &value
		}
		prices[PriceKey{provider, model}] = price
	}
	return prices, rows.Err()
}

// LoadRecords reads every usage row at or after `since`.
func LoadRecords(ctx context.Context, db *sql.DB, since time.Time) ([]Record, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT provider, model, endpoint, source_account, failed,
		       input_tokens, output_tokens, cached_tokens,
		       cache_read_tokens, cache_creation_tokens, reasoning_tokens, total_tokens
		FROM usage_records
		WHERE timestamp >= ?
		ORDER BY id`, since.UTC())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	records := []Record{}
	for rows.Next() {
		var (
			record                                   Record
			provider, model, endpoint, sourceAccount sql.NullString
			failed                                   bool
		)
		if err := rows.Scan(&provider, &model, &endpoint, &sourceAccount, &failed,
			&record.InputTokens, &record.OutputTokens, &record.CachedTokens,
			&record.CacheReadTokens, &record.CacheCreationTokens,
			&record.ReasoningTokens, &record.TotalTokens); err != nil {
			return nil, err
		}
		record.Provider = nullableString(provider)
		record.Model = nullableString(model)
		record.Endpoint = nullableString(endpoint)
		record.SourceAccount = nullableString(sourceAccount)
		record.Failed = failed
		records = append(records, record)
	}
	return records, rows.Err()
}

func nullableString(value sql.NullString) *string {
	if !value.Valid {
		return nil
	}
	text := value.String
	return &text
}

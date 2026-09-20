package app

import "time"

// UsageRecordCost exports recordCost so the `cpa-helper usage-cost` subcommand
// (internal/usagecost) can price records with the SAME derivation production
// uses -- the report must reproduce production's number, and a copied
// implementation would drift silently. The seam is narrow on purpose: only
// the record->cost question is exported, not the matching internals.
func UsageRecordCost(record UsageRecord, prices map[[2]string]ModelPrice) (usd float64, unpriced bool) {
	return recordCost(record, prices)
}

// UsageDBTime formats a timestamp the way production writes it to TEXT columns
// (dbTime(): Asia/Shanghai offset, 'T' separator). Read-side comparisons against
// those columns must bind this exact byte shape -- binding a time.Time lets the
// driver serialise it differently (space separator, UTC) and a lexicographic
// comparison then stops being a time comparison.
func UsageDBTime(t time.Time) string {
	return dbTime(t)
}

// UsageDBPath resolves the database path the same way the service does
// (CPA_HELPER_DATA_DIR, else <repo>/data) so `usage-cost` defaults to the
// database the service actually writes to.
func UsageDBPath() (string, error) {
	paths, err := resolveRuntimePaths()
	if err != nil {
		return "", err
	}
	return paths.DBPath, nil
}

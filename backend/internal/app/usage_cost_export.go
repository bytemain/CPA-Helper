package app

// UsageRecordCost exports recordCost so the `cpa-helper usage-cost` subcommand
// (internal/usagecost) can price records with the SAME derivation production
// uses -- the report must reproduce production's number, and a copied
// implementation would drift silently. The seam is narrow on purpose: only
// the record->cost question is exported, not the matching internals.
func UsageRecordCost(record UsageRecord, prices map[[2]string]ModelPrice) (usd float64, unpriced bool) {
	return recordCost(record, prices)
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

package usagecost

import (
	backendApp "cpa-helper/backend/internal/app"
)

// WiredCostFunc returns CPA-Helper's own cost derivation adapted to the
// report's narrow Record shape. Reusing app.UsageRecordCost (which wraps the
// unexported recordCost) is the whole point of living in this module: the
// report must produce the SAME number production recorded, and the only
// implementation that cannot drift from recordCost is recordCost itself.
func WiredCostFunc() CostFunc { return usageRecordCost }

func usageRecordCost(record Record, prices map[PriceKey]ModelPrice) (float64, bool) {
	return backendApp.UsageRecordCost(toUsageRecord(record), prices)
}

func toUsageRecord(record Record) backendApp.UsageRecord {
	return backendApp.UsageRecord{
		Provider:            record.Provider,
		Model:               record.Model,
		Endpoint:            record.Endpoint,
		SourceAccount:       record.SourceAccount,
		Failed:              record.Failed,
		InputTokens:         record.InputTokens,
		OutputTokens:        record.OutputTokens,
		CachedTokens:        record.CachedTokens,
		CacheReadTokens:     record.CacheReadTokens,
		CacheCreationTokens: record.CacheCreationTokens,
		ReasoningTokens:     record.ReasoningTokens,
		TotalTokens:         record.TotalTokens,
	}
}

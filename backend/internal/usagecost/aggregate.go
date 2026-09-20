package usagecost

import "sort"

// Group is one row of the report.
type Group struct {
	Key string `json:"key"`
	// Requests counts every record in the group, Failed the subset that failed.
	// Both are reported because a group's cost is only interpretable next to how
	// many calls produced it.
	Requests int `json:"requests"`
	Failed   int `json:"failed"`
	// TotalTokens is summed from the records, not recomputed from the parts.
	TotalTokens int64 `json:"total_tokens"`
	// CostUSD covers only the PRICED records in this group.
	CostUSD float64 `json:"cost_usd"`
	// UnpricedRequests counts records that consumed something billable but
	// matched no price. They contribute 0 to CostUSD, so without this column a
	// group with no prices configured is indistinguishable from a free one.
	UnpricedRequests int `json:"unpriced_requests"`
}

// Report is the whole answer, including what was asked for.
type Report struct {
	GroupBy   string  `json:"group_by"`
	SinceDays int     `json:"since_days"`
	Since     string  `json:"since"`
	Groups    []Group `json:"groups"`
	// TotalCostUSD and TotalUnpriced are summed over groups so a reader never
	// has to add the column up by hand and get a different answer.
	TotalCostUSD  float64 `json:"total_cost_usd"`
	TotalUnpriced int     `json:"total_unpriced_requests"`
}

// unattributed labels records whose grouping dimension is NULL or blank.
// They are kept in the report rather than dropped: silently discarding them
// would make the report's total disagree with the database's, and nobody would
// see why.
const unattributed = "(unattributed)"

// Aggregate groups records and costs them. It returns ErrNoCostFunc when no
// pricing implementation is available, rather than a report full of zeros.
func Aggregate(records []Record, prices map[PriceKey]ModelPrice, by GroupBy, cost CostFunc) ([]Group, error) {
	if cost == nil {
		return nil, ErrNoCostFunc
	}
	byKey := map[string]*Group{}
	for _, record := range records {
		key := groupKey(record, by)
		group, ok := byKey[key]
		if !ok {
			group = &Group{Key: key}
			byKey[key] = group
		}
		group.Requests++
		if record.Failed {
			group.Failed++
		}
		group.TotalTokens += int64(record.TotalTokens)
		usd, unpriced := cost(record, prices)
		if unpriced {
			group.UnpricedRequests++
			continue
		}
		group.CostUSD += usd
	}
	groups := make([]Group, 0, len(byKey))
	for _, group := range byKey {
		groups = append(groups, *group)
	}
	// Most expensive first; ties broken by key so the output is stable and two
	// runs over the same data can be diffed.
	sort.Slice(groups, func(i, j int) bool {
		if groups[i].CostUSD != groups[j].CostUSD {
			return groups[i].CostUSD > groups[j].CostUSD
		}
		return groups[i].Key < groups[j].Key
	})
	return groups, nil
}

func groupKey(record Record, by GroupBy) string {
	var value *string
	switch by {
	case GroupByModel:
		value = record.Model
	case GroupByProvider:
		value = record.Provider
	case GroupByEndpoint:
		value = record.Endpoint
	case GroupBySourceAccount:
		value = record.SourceAccount
	}
	if value == nil || *value == "" {
		return unattributed
	}
	return *value
}

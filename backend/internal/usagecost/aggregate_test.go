package usagecost

import (
	"errors"
	"testing"
)

func strptr(s string) *string { return &s }

// fixedCost is a test-only pricing stub. It exists so the aggregation layer can
// be tested WITHOUT deciding where the real derivation comes from -- and it is
// deliberately trivial (1 USD per priced request) so any arithmetic asserted
// below is the aggregator's, not the stub's.
func fixedCost(record Record, prices map[PriceKey]ModelPrice) (float64, bool) {
	if record.Model == nil {
		return 0, record.TotalTokens > 0
	}
	if _, ok := prices[PriceKey{"p", *record.Model}]; !ok {
		return 0, record.TotalTokens > 0
	}
	return 1, false
}

var testPrices = map[PriceKey]ModelPrice{{"p", "priced"}: {}}

func TestAggregateWithoutACostFuncRefusesInsteadOfReportingZero(t *testing.T) {
	// The whole point of the seam: "I cannot price this" must never leave the
	// package looking like "this cost nothing".
	_, err := Aggregate([]Record{{Model: strptr("priced"), TotalTokens: 10}}, testPrices, GroupByModel, nil)
	if !errors.Is(err, ErrNoCostFunc) {
		t.Fatalf("err = %v, want ErrNoCostFunc", err)
	}
}

func TestAggregateKeepsUnpricedRequestsOutOfCostButVisible(t *testing.T) {
	records := []Record{
		{Model: strptr("priced"), TotalTokens: 10},
		{Model: strptr("priced"), TotalTokens: 5, Failed: true},
		{Model: strptr("nameless"), TotalTokens: 7},
	}
	groups, err := Aggregate(records, testPrices, GroupByModel, fixedCost)
	if err != nil {
		t.Fatal(err)
	}
	if len(groups) != 2 {
		t.Fatalf("groups = %+v, want 2", groups)
	}
	priced, unpricedGroup := groups[0], groups[1]
	if priced.Key != "priced" || priced.Requests != 2 || priced.Failed != 1 ||
		priced.TotalTokens != 15 || priced.CostUSD != 2 || priced.UnpricedRequests != 0 {
		t.Fatalf("priced = %+v", priced)
	}
	// The unpriced group must NOT be dropped and must NOT be costed: a group
	// that matched no price looks exactly like a free one unless it is counted.
	if unpricedGroup.Key != "nameless" || unpricedGroup.Requests != 1 ||
		unpricedGroup.CostUSD != 0 || unpricedGroup.UnpricedRequests != 1 {
		t.Fatalf("unpriced = %+v", unpricedGroup)
	}
}

func TestAggregateKeepsRowsWhoseDimensionIsMissing(t *testing.T) {
	// Dropping these would make the report's request count disagree with the
	// database's, with nothing to point at.
	blank := ""
	records := []Record{
		{Model: strptr("priced"), Endpoint: nil, TotalTokens: 1},
		{Model: strptr("priced"), Endpoint: &blank, TotalTokens: 1},
		{Model: strptr("priced"), Endpoint: strptr("/v1/chat"), TotalTokens: 1},
	}
	groups, err := Aggregate(records, testPrices, GroupByEndpoint, fixedCost)
	if err != nil {
		t.Fatal(err)
	}
	total := 0
	seen := map[string]int{}
	for _, group := range groups {
		total += group.Requests
		seen[group.Key] = group.Requests
	}
	if total != len(records) {
		t.Fatalf("requests = %d, want %d (groups %+v)", total, len(records), groups)
	}
	// NULL and "" are the same state for reporting -- both mean "we do not know
	// which endpoint" -- so they must land in ONE bucket, not two look-alikes.
	if seen[unattributed] != 2 || seen["/v1/chat"] != 1 {
		t.Fatalf("buckets = %+v", seen)
	}
}

func TestAggregateOrdersByCostThenKeySoRunsAreDiffable(t *testing.T) {
	prices := map[PriceKey]ModelPrice{{"p", "a"}: {}, {"p", "b"}: {}, {"p", "c"}: {}}
	records := []Record{
		{Model: strptr("b"), TotalTokens: 1}, {Model: strptr("b"), TotalTokens: 1},
		{Model: strptr("c"), TotalTokens: 1},
		{Model: strptr("a"), TotalTokens: 1},
	}
	groups, err := Aggregate(records, prices, GroupByModel, fixedCost)
	if err != nil {
		t.Fatal(err)
	}
	got := []string{groups[0].Key, groups[1].Key, groups[2].Key}
	// b costs 2; a and c both cost 1 and must then sort by key, not by map order.
	if got[0] != "b" || got[1] != "a" || got[2] != "c" {
		t.Fatalf("order = %v, want [b a c]", got)
	}
}

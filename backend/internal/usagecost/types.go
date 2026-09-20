package usagecost

import (
	"errors"

	backendApp "cpa-helper/backend/internal/app"
)

// ModelPrice is CPA-Helper's own price row type. The report does not get a
// second definition: two shapes for the same priced columns is how a report
// and the production billing branch drift apart.
type ModelPrice = backendApp.ModelPrice

// PriceKey is (provider, model), both lowercased and trimmed -- the same shape
// CPA-Helper keys its price map by. It is an alias, not a new type: the price
// map must be passable to app.UsageRecordCost without a rebuild, or the wiring
// could silently hand it a different map than the one that was loaded.
type PriceKey = [2]string

// Record is one usage row, narrowed to the fields the report reads. It stays
// narrow on purpose: the report's own contract (grouping dimensions plus the
// token counts cost depends on) is visible at a glance instead of being
// implied by a 30-field struct. The JSON tags are load-bearing: the golden
// vectors are stored snake_case, and untagged fields would silently decode to
// zero while still "passing" a compile.
type Record struct {
	Provider *string `json:"provider"`
	Model    *string `json:"model"`
	Endpoint *string `json:"endpoint"`
	// SourceAccount is the upstream account the request was served by. It is
	// not part of cost, only of attribution.
	SourceAccount       *string `json:"source_account"`
	Failed              bool    `json:"failed"`
	InputTokens         int     `json:"input_tokens"`
	OutputTokens        int     `json:"output_tokens"`
	CachedTokens        int     `json:"cached_tokens"`
	CacheReadTokens     int     `json:"cache_read_tokens"`
	CacheCreationTokens int     `json:"cache_creation_tokens"`
	ReasoningTokens     int     `json:"reasoning_tokens"`
	TotalTokens         int     `json:"total_tokens"`
}

// CostFunc computes one record's estimated cost. `unpriced` reports that the
// record consumed something billable but no price matched -- that is NOT the
// same as a cost of zero, and the report keeps the two apart so a missing price
// can never be read as free usage.
type CostFunc func(Record, map[PriceKey]ModelPrice) (usd float64, unpriced bool)

// ErrNoCostFunc is returned instead of a number when no pricing implementation
// is available. "I cannot price this" must never leave the package looking
// like "this cost nothing".
var ErrNoCostFunc = errors.New("no pricing implementation is wired in")

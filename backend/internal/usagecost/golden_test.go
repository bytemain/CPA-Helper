package usagecost

import (
	"encoding/json"
	"math"
	"os"
	"testing"
)

// The golden vectors are the numbers CPA-Helper's own recordCost produced.
// Whatever implementation this repo wires into costFunc must reproduce them
// exactly -- that equivalence is the entire reason this tool is allowed to
// report a cost at all.
type goldenFile struct {
	GeneratedFrom struct {
		Repo   string `json:"repo"`
		Commit string `json:"commit"`
	} `json:"generated_from"`
	Cases []goldenCase `json:"cases"`
}

type goldenCase struct {
	Name   string `json:"name"`
	Why    string `json:"why"`
	Prices []struct {
		Provider                   string   `json:"provider"`
		Model                      string   `json:"model"`
		InputUSDPerMillion         float64  `json:"input_usd_per_million"`
		OutputUSDPerMillion        float64  `json:"output_usd_per_million"`
		CacheReadUSDPerMillion     float64  `json:"cache_read_usd_per_million"`
		CacheCreationUSDPerMillion float64  `json:"cache_creation_usd_per_million"`
		RequestUSD                 *float64 `json:"request_usd"`
	} `json:"prices"`
	Record       Record  `json:"record"`
	WantUSD      float64 `json:"want_usd"`
	WantUnpriced bool    `json:"want_unpriced"`
}

func loadGolden(t *testing.T) goldenFile {
	t.Helper()
	raw, err := os.ReadFile("testdata/cost_golden.json")
	if err != nil {
		t.Fatal(err)
	}
	var file goldenFile
	if err := json.Unmarshal(raw, &file); err != nil {
		t.Fatal(err)
	}
	if len(file.Cases) == 0 || file.GeneratedFrom.Commit == "" {
		// A vector file that lost its provenance cannot be trusted as evidence:
		// it would still pass, and nobody could tell which behaviour it pinned.
		t.Fatalf("golden file must carry cases and the commit they came from: %+v", file.GeneratedFrom)
	}
	return file
}

// TestGoldenVectorsCoverTheBranchesThatMatter guards the corpus itself. A
// shrinking vector file is the quiet way this tooth stops biting: the
// equivalence test below keeps passing while covering less and less.
func TestGoldenVectorsCoverTheBranchesThatMatter(t *testing.T) {
	file := loadGolden(t)
	required := []string{
		"token/non-claude/cached-bounded",
		"token/non-claude/cached-exceeds-input",
		"token/claude/cache-creation",
		"token/no-price/tokens-used",
		"token/no-price/no-tokens",
		"token/nil-provider-and-model",
		"alias/antigravity-to-gemini-family",
		"alias/litellm-slash-key",
		"request/image-success",
		"request/image-failed",
		"request/image-no-request-price",
		"token/rounding-to-8dp",
	}
	present := map[string]bool{}
	for _, c := range file.Cases {
		present[c.Name] = true
	}
	for _, name := range required {
		if !present[name] {
			t.Fatalf("golden vectors no longer cover %q", name)
		}
	}
	// The two "looks like zero" cases must disagree on Unpriced, or the corpus
	// cannot detect an implementation that collapses them.
	var noPriceUsed, noPriceIdle *goldenCase
	for i := range file.Cases {
		switch file.Cases[i].Name {
		case "token/no-price/tokens-used":
			noPriceUsed = &file.Cases[i]
		case "token/no-price/no-tokens":
			noPriceIdle = &file.Cases[i]
		}
	}
	if noPriceUsed.WantUSD != 0 || noPriceIdle.WantUSD != 0 {
		t.Fatal("both no-price cases must cost 0 -- that is what makes them look alike")
	}
	if !noPriceUsed.WantUnpriced || noPriceIdle.WantUnpriced {
		t.Fatalf("the no-price cases must differ on Unpriced (%v vs %v), else 'no price configured' "+
			"and 'nothing was used' are indistinguishable",
			noPriceUsed.WantUnpriced, noPriceIdle.WantUnpriced)
	}
}

// TestWiredCostFuncMatchesCPAHelper is the drift detector. It is skipped while
// no implementation is wired in -- and the skip is loud, because a silently
// skipped equivalence test is exactly how two implementations drift apart
// without anybody noticing.
func TestWiredCostFuncMatchesCPAHelper(t *testing.T) {
	file := loadGolden(t)
	cost := WiredCostFunc()
	if cost == nil {
		t.Skipf("no pricing implementation wired in yet; %d golden vectors from %s@%s are waiting",
			len(file.Cases), file.GeneratedFrom.Repo, file.GeneratedFrom.Commit[:12])
	}
	for _, c := range file.Cases {
		prices := map[PriceKey]ModelPrice{}
		for _, p := range c.Prices {
			prices[PriceKey{p.Provider, p.Model}] = ModelPrice{
				InputUSDPerMillion:         p.InputUSDPerMillion,
				OutputUSDPerMillion:        p.OutputUSDPerMillion,
				CacheReadUSDPerMillion:     p.CacheReadUSDPerMillion,
				CacheCreationUSDPerMillion: p.CacheCreationUSDPerMillion,
				RequestUSD:                 p.RequestUSD,
			}
		}
		usd, unpriced := cost(c.Record, prices)
		// Exact, not approximate: CPA-Helper rounds to 8 decimal places, so the
		// two implementations either agree to the cent-of-a-cent or they do not.
		if math.Abs(usd-c.WantUSD) > 1e-12 || unpriced != c.WantUnpriced {
			t.Fatalf("%s (%s): got (%.10f, %v), want (%.10f, %v)",
				c.Name, c.Why, usd, unpriced, c.WantUSD, c.WantUnpriced)
		}
	}
}

package app

import (
	"encoding/json"
	"math"
	"os"
	"testing"
	"time"
)

// cpaQuotaFixture mirrors one entry of testdata/cpa_quota_fixtures.json: a real
// quota API response captured from production (sanitised), keyed by the auth
// file it came from.
type cpaQuotaFixture struct {
	Type     string         `json:"type"`
	File     string         `json:"file"`
	Disabled bool           `json:"disabled"`
	Status   int            `json:"status"`
	Body     map[string]any `json:"body"`
}

func loadCPAQuotaFixtures(t *testing.T) []cpaQuotaFixture {
	t.Helper()
	raw, err := os.ReadFile("testdata/cpa_quota_fixtures.json")
	if err != nil {
		t.Fatalf("read fixtures: %v", err)
	}
	var fixtures []cpaQuotaFixture
	if err := json.Unmarshal(raw, &fixtures); err != nil {
		t.Fatalf("unmarshal fixtures: %v", err)
	}
	if len(fixtures) == 0 {
		t.Fatal("fixture file is empty")
	}
	return fixtures
}

func wantBucket(t *testing.T, g keeperAntigravityGroup, window string) keeperAntigravityBucket {
	t.Helper()
	for _, b := range g.Buckets {
		if b.Window == window {
			return b
		}
	}
	t.Fatalf("group %+v has no %q bucket", g, window)
	return keeperAntigravityBucket{}
}

func almostEqual(a, b float64) bool { return math.Abs(a-b) < 1e-4 }

func parseRFC3339Fixture(t *testing.T, s string) time.Time {
	t.Helper()
	ts, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatalf("bad expected time %q: %v", s, err)
	}
	return ts
}

// TestQuotaFixtures drives every production-captured response through its
// parser and asserts the bucket layout, remaining fraction, and reset time.
func TestQuotaFixtures(t *testing.T) {
	for _, fx := range loadCPAQuotaFixtures(t) {
		t.Run(fx.Type+"/"+fx.File, func(t *testing.T) {
			if fx.Status != 200 {
				t.Fatalf("fixture has status %d, expected 200 capture", fx.Status)
			}
			switch fx.Type {
			case "codex":
				groups, ok := parseCodexQuotaGroups(fx.Body)
				if !ok || len(groups) != 1 {
					t.Fatalf("parse = (%v, %+v)", ok, groups)
				}
				// Every captured codex account is untouched: full weekly window.
				b := groups[0].Buckets[0]
				if !almostEqual(b.RemainingFraction, 1.0) {
					t.Fatalf("codex remaining = %v, want 1.0", b.RemainingFraction)
				}
				if b.Window != "168h0m0s" {
					t.Fatalf("codex window = %q, want 168h0m0s", b.Window)
				}
			case "devin":
				assertDevinFixture(t, fx)
			case "kimi":
				assertKimiFixture(t, fx)
			case "xai":
				assertXAIFixture(t, fx)
			default:
				t.Fatalf("unknown fixture type %q", fx.Type)
			}
		})
	}
}

func assertDevinFixture(t *testing.T, fx cpaQuotaFixture) {
	groups, ok := parseDevinQuotaGroups(fx.Body)
	if !ok || len(groups) != 1 {
		t.Fatalf("parse = (%v, %+v)", ok, groups)
	}
	daily := wantBucket(t, groups[0], "daily")
	weekly := wantBucket(t, groups[0], "weekly")
	var wantDaily, wantWeekly float64
	var wantWeeklyReset string
	switch fx.File {
	case "devin-user-8250b3544ccc9cd2-max.json":
		wantDaily, wantWeekly, wantWeeklyReset = 1.0, 0.67, "2026-10-04T16:00:00+08:00"
		if daily.ResetAt == nil || !daily.ResetAt.Equal(parseRFC3339Fixture(t, "2026-09-28T16:00:00+08:00")) {
			t.Fatalf("daily reset = %v", daily.ResetAt)
		}
	case "devin-user-8250b3544ccc9cd2.json":
		wantDaily, wantWeekly, wantWeeklyReset = 0.0, 0.49, "2026-10-04T16:00:00+08:00"
	case "devin-user-fa03e24b343aafa1.json":
		wantDaily, wantWeekly, wantWeeklyReset = 0.0, 0.5, "2026-10-04T16:00:00+08:00"
	default:
		t.Fatalf("no expectations for devin file %q", fx.File)
	}
	if !almostEqual(daily.RemainingFraction, wantDaily) {
		t.Fatalf("daily remaining = %v, want %v", daily.RemainingFraction, wantDaily)
	}
	if !almostEqual(weekly.RemainingFraction, wantWeekly) {
		t.Fatalf("weekly remaining = %v, want %v", weekly.RemainingFraction, wantWeekly)
	}
	if weekly.ResetAt == nil || !weekly.ResetAt.Equal(parseRFC3339Fixture(t, wantWeeklyReset)) {
		t.Fatalf("weekly reset = %v, want %v", weekly.ResetAt, wantWeeklyReset)
	}
}

func assertKimiFixture(t *testing.T, fx cpaQuotaFixture) {
	groups, ok := parseKimiQuotaGroups(fx.Body)
	if !ok || len(groups) != 1 {
		t.Fatalf("parse = (%v, %+v)", ok, groups)
	}
	fiveH := wantBucket(t, groups[0], "5h")
	var wantFiveH float64
	var wantFiveHReset, secondWindow, secondReset string
	var wantSecond float64
	switch fx.File {
	case "kimi-1784293697497.json":
		wantFiveH, wantFiveHReset = 0.974725, "2026-09-28T04:07:34+08:00"
		secondWindow, wantSecond, secondReset = "weekly", 0.830428, "2026-10-02T21:07:34+08:00"
	case "kimi-1789629164905.json":
		wantFiveH, wantFiveHReset = 0.873279, "2026-09-28T01:08:47+08:00"
		secondWindow, wantSecond, secondReset = "monthly", 0.7636, "2026-10-18T08:00:00+08:00"
	case "kimi-1789640825364.json":
		wantFiveH, wantFiveHReset = 0.984975, "2026-09-28T04:24:26+08:00"
		secondWindow, wantSecond, secondReset = "monthly", 0.6908, "2026-10-18T08:00:00+08:00"
	default:
		t.Fatalf("no expectations for kimi file %q", fx.File)
	}
	if !almostEqual(fiveH.RemainingFraction, wantFiveH) {
		t.Fatalf("5h remaining = %v, want %v", fiveH.RemainingFraction, wantFiveH)
	}
	if fiveH.ResetAt == nil || !fiveH.ResetAt.Equal(parseRFC3339Fixture(t, wantFiveHReset)) {
		t.Fatalf("5h reset = %v, want %v", fiveH.ResetAt, wantFiveHReset)
	}
	second := wantBucket(t, groups[0], secondWindow)
	if !almostEqual(second.RemainingFraction, wantSecond) {
		t.Fatalf("%s remaining = %v, want %v", secondWindow, second.RemainingFraction, wantSecond)
	}
	if second.ResetAt == nil || !second.ResetAt.Equal(parseRFC3339Fixture(t, secondReset)) {
		t.Fatalf("%s reset = %v, want %v", secondWindow, second.ResetAt, secondReset)
	}
}

func assertXAIFixture(t *testing.T, fx cpaQuotaFixture) {
	groups, ok := parseXAIQuotaGroups(fx.Body)
	if !ok || len(groups) != 1 {
		t.Fatalf("parse = (%v, %+v)", ok, groups)
	}
	b := wantBucket(t, groups[0], "weekly")
	if !almostEqual(b.RemainingFraction, 0.97) {
		t.Fatalf("weekly remaining = %v, want 0.97", b.RemainingFraction)
	}
	if b.ResetAt == nil || math.Abs(b.ResetAt.Sub(parseRFC3339Fixture(t, "2026-10-03T03:33:06+08:00")).Seconds()) > 1 {
		t.Fatalf("weekly reset = %v", b.ResetAt)
	}
}

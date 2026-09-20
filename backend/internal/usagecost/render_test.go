package usagecost

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func testOptions() Options {
	return Options{
		GroupBy:   GroupByModel,
		SinceDays: 7,
		Since:     time.Date(2026, 9, 13, 4, 0, 0, 0, time.UTC),
	}
}

func TestBuildReportTotalsMatchTheRows(t *testing.T) {
	groups := []Group{
		{Key: "a", CostUSD: 1.25, UnpricedRequests: 2},
		{Key: "b", CostUSD: 0.75, UnpricedRequests: 1},
	}
	report := BuildReport(testOptions(), groups)
	if report.TotalCostUSD != 2 || report.TotalUnpriced != 3 {
		t.Fatalf("totals = %v / %v", report.TotalCostUSD, report.TotalUnpriced)
	}
	if report.GroupBy != "model" || report.SinceDays != 7 ||
		report.Since != "2026-09-13T04:00:00Z" {
		t.Fatalf("header = %+v", report)
	}
}

func TestWriteJSONEmitsAnEmptyArrayNotNull(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteJSON(&buf, BuildReport(testOptions(), nil)); err != nil {
		t.Fatal(err)
	}
	// Asserted on the serialized text: a nil slice also has length zero, but it
	// marshals to null, and a consumer that ranges or calls Array.isArray on the
	// decoded value then has to special-case an empty report.
	if !strings.Contains(buf.String(), `"groups": []`) {
		t.Fatalf("groups must serialize as []: %s", buf.String())
	}
	var round Report
	if err := json.Unmarshal(buf.Bytes(), &round); err != nil {
		t.Fatal(err)
	}
}

func TestWriteTextSaysWhenTheWindowIsEmpty(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteText(&buf, BuildReport(testOptions(), nil)); err != nil {
		t.Fatal(err)
	}
	// An empty table and a table that failed to load look identical once the
	// header scrolls away, so the empty case says so in words.
	if !strings.Contains(buf.String(), "no usage records") {
		t.Fatalf("empty report must say so: %q", buf.String())
	}
}

func TestWriteTextPutsTheUnpricedWarningNextToTheTotal(t *testing.T) {
	var buf bytes.Buffer
	report := BuildReport(testOptions(), []Group{
		{Key: "gemini", Requests: 3, TotalTokens: 30, CostUSD: 1.5},
		{Key: "mystery", Requests: 2, TotalTokens: 20, UnpricedRequests: 2},
	})
	if err := WriteText(&buf, report); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if !strings.Contains(out, "total 1.500000 USD") {
		t.Fatalf("missing total: %q", out)
	}
	// The qualifier must travel WITH the total. A total that silently excludes
	// unpriced usage reads as the whole bill, and the column alone is easy to
	// skip past.
	warningAt := strings.Index(out, "matched no price")
	totalAt := strings.Index(out, "total 1.500000 USD")
	if warningAt < 0 || warningAt < totalAt {
		t.Fatalf("warning must follow the total and mention the count: %q", out)
	}
	if !strings.Contains(out, "2 request(s)") {
		t.Fatalf("warning must carry the count: %q", out)
	}

	// The opposite direction: a fully priced report must NOT carry the warning,
	// or it degrades into noise everyone learns to ignore.
	buf.Reset()
	if err := WriteText(&buf, BuildReport(testOptions(), []Group{{Key: "gemini", CostUSD: 1}})); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(buf.String(), "matched no price") {
		t.Fatalf("no warning expected: %q", buf.String())
	}
}

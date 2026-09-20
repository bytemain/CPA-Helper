package usagecost

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"
)

// BuildReport assembles the answer, including the totals, so every reader gets
// the same total instead of adding the column up themselves.
func BuildReport(opts Options, groups []Group) Report {
	report := Report{
		GroupBy:   string(opts.GroupBy),
		SinceDays: opts.SinceDays,
		Since:     opts.Since.UTC().Format(time.RFC3339),
		Groups:    groups,
	}
	for _, group := range groups {
		report.TotalCostUSD += group.CostUSD
		report.TotalUnpriced += group.UnpricedRequests
	}
	return report
}

// WriteJSON emits the machine-readable form. Groups is never nil so the field
// marshals as [] rather than null: a consumer testing `Array.isArray` (or Go
// ranging over a decoded nil) must not have to special-case an empty report.
func WriteJSON(w io.Writer, report Report) error {
	if report.Groups == nil {
		report.Groups = []Group{}
	}
	encoder := json.NewEncoder(w)
	encoder.SetIndent("", "  ")
	return encoder.Encode(report)
}

// WriteText emits the human form.
func WriteText(w io.Writer, report Report) error {
	header := fmt.Sprintf("usage cost by %s, since %s (%d day(s))",
		report.GroupBy, report.Since, report.SinceDays)
	if _, err := fmt.Fprintln(w, header); err != nil {
		return err
	}
	if len(report.Groups) == 0 {
		// Said out loud, because an empty table and a table that failed to load
		// look identical once the header scrolls away.
		_, err := fmt.Fprintln(w, "no usage records in this window")
		return err
	}
	rows := [][]string{{"KEY", "REQUESTS", "FAILED", "TOKENS", "COST_USD", "UNPRICED"}}
	for _, group := range report.Groups {
		rows = append(rows, []string{
			group.Key,
			fmt.Sprintf("%d", group.Requests),
			fmt.Sprintf("%d", group.Failed),
			fmt.Sprintf("%d", group.TotalTokens),
			fmt.Sprintf("%.6f", group.CostUSD),
			fmt.Sprintf("%d", group.UnpricedRequests),
		})
	}
	widths := make([]int, len(rows[0]))
	for _, row := range rows {
		for i, cell := range row {
			if len(cell) > widths[i] {
				widths[i] = len(cell)
			}
		}
	}
	for _, row := range rows {
		parts := make([]string, len(row))
		for i, cell := range row {
			parts[i] = fmt.Sprintf("%-*s", widths[i], cell)
		}
		if _, err := fmt.Fprintln(w, strings.TrimRight(strings.Join(parts, "  "), " ")); err != nil {
			return err
		}
	}
	if _, err := fmt.Fprintf(w, "\ntotal %.6f USD\n", report.TotalCostUSD); err != nil {
		return err
	}
	if report.TotalUnpriced > 0 {
		// The qualifier travels in the same sentence as the total, not in a
		// column the reader may have skipped: a total that silently excludes
		// unpriced usage reads as the whole bill.
		_, err := fmt.Fprintf(w,
			"warning: %d request(s) matched no price and are NOT in that total\n", report.TotalUnpriced)
		return err
	}
	return nil
}

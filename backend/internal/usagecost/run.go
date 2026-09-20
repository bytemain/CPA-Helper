package usagecost

import (
	"context"
	"fmt"
	"io"
	"time"
)

// Run executes `cpa-helper usage-cost <args>`: load prices and records, cost
// every record with the production derivation, and write the report to `out`.
func Run(ctx context.Context, args []string, out io.Writer) error {
	opts, err := ParseArgs(args, time.Now().UTC())
	if err != nil {
		return err
	}
	db, err := OpenReadOnly(ctx, opts.DBPath)
	if err != nil {
		return err
	}
	defer db.Close()

	prices, err := LoadPrices(ctx, db)
	if err != nil {
		return fmt.Errorf("load prices: %w", err)
	}
	records, err := LoadRecords(ctx, db, opts.Since)
	if err != nil {
		return fmt.Errorf("load usage records: %w", err)
	}
	groups, err := Aggregate(records, prices, opts.GroupBy, WiredCostFunc())
	if err != nil {
		return err
	}
	report := BuildReport(opts, groups)
	if opts.JSON {
		return WriteJSON(out, report)
	}
	return WriteText(out, report)
}

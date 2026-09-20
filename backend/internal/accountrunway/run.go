package accountrunway

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"math"
	"strconv"
	"strings"
	"time"

	backendApp "cpa-helper/backend/internal/app"
)

// Options carries parsed account-runway flags.
type Options struct {
	DBPath   string
	Since    int // days
	Provider string
	JSON     bool
}

// ParseArgs parses the subcommand flags. `now` is injectable for tests.
func ParseArgs(args []string, now time.Time) (Options, error) {
	_ = now
	opts := Options{Since: 1, Provider: "all"}
	fs := flag.NewFlagSet("account-runway", flag.ContinueOnError)
	fs.StringVar(&opts.DBPath, "db", opts.DBPath, "SQLite database path")
	fs.IntVar(&opts.Since, "since", opts.Since, "burn-rate window in days")
	fs.StringVar(&opts.Provider, "provider", opts.Provider, "antigravity|codex|all")
	fs.BoolVar(&opts.JSON, "json", false, "emit JSON")
	if err := fs.Parse(args); err != nil {
		return Options{}, err
	}
	if opts.Since <= 0 {
		return Options{}, fmt.Errorf("--since must be a positive number of days, got %d", opts.Since)
	}
	switch opts.Provider {
	case "antigravity", "codex", "all":
	default:
		return Options{}, fmt.Errorf("--provider must be antigravity, codex or all, got %q", opts.Provider)
	}
	if opts.DBPath == "" {
		// Same resolution the service uses (CPA_HELPER_DATA_DIR, else
		// <repo>/data): omitting --db must report on the database the service
		// writes to, not fail for want of a path the user would only guess at.
		p, err := backendApp.UsageDBPath()
		if err != nil {
			return Options{}, fmt.Errorf("--db not given and the default could not be resolved: %w", err)
		}
		opts.DBPath = p
	}
	return opts, nil
}

// Run executes `cpa-helper account-runway <args>`.
func Run(ctx context.Context, args []string, out io.Writer) error {
	return RunAt(ctx, args, out, time.Now().UTC())
}

// RunAt is Run with an injectable clock for tests.
func RunAt(ctx context.Context, args []string, out io.Writer, now time.Time) error {
	opts, err := ParseArgs(args, now)
	if err != nil {
		return err
	}
	db, err := OpenReadOnly(ctx, opts.DBPath)
	if err != nil {
		return err
	}
	defer db.Close()

	accounts, err := LoadAccounts(ctx, db)
	if err != nil {
		return fmt.Errorf("load keeper accounts: %w", err)
	}
	rows, err := LoadUsageRows(ctx, db, now.Add(-time.Duration(opts.Since)*24*time.Hour))
	if err != nil {
		return fmt.Errorf("load usage rows: %w", err)
	}
	report := Compute(accounts, rows, opts.Provider, opts.Since, now)
	if opts.JSON {
		return WriteJSON(out, report)
	}
	return WriteText(out, report)
}

// WriteJSON emits the stable machine-readable report.
func WriteJSON(out io.Writer, report Report) error {
	encoder := json.NewEncoder(out)
	encoder.SetIndent("", "  ")
	return encoder.Encode(report)
}

// WriteText renders the report as a terminal table.
func WriteText(out io.Writer, report Report) error {
	w := newTableWriter(out)
	w.row("ACCOUNT", "PROVIDER", "BUCKET", "REMAIN%", "RESET", "BURN/H", "RUNWAY_H", "COVERAGE", "STATUS")
	for _, account := range report.Accounts {
		name := account.Name
		if account.Disabled {
			name += " (DISABLED)"
		}
		for _, bucket := range account.Buckets {
			w.row(
				name,
				account.Provider,
				bucket.Name,
				formatPercent(bucket.RemainingFraction),
				formatReset(bucket.ResetAtText),
				formatFloat(account.BurnPerHour, 0),
				formatFloatPtr(bucket.RunwayHours, 1),
				formatFloatPtr(bucket.Coverage, 2),
				bucketStatusText(bucket),
			)
		}
	}
	w.flush()
	if report.UnattributedRows > 0 {
		fmt.Fprintf(out, "\nunattributed usage: %d rows / %d tokens (no unique account match)\n",
			report.UnattributedRows, report.UnattributedTokens)
	}
	fmt.Fprintf(out, "\npool: %d enabled / %d disabled accounts; recommendation: %s",
		report.Pool.EnabledAccounts, report.Pool.DisabledAccounts, report.Pool.Recommendation)
	if report.Pool.AdditionalAccounts > 0 {
		fmt.Fprintf(out, " (add ~%d account(s), gap %.0f tokens)", report.Pool.AdditionalAccounts, report.Pool.GapTokens)
	}
	fmt.Fprintln(out)
	return nil
}

func formatPercent(fraction float64) string {
	return strconv.FormatFloat(fraction*100, 'f', 0, 64) + "%"
}

func formatReset(text string) string {
	if text == "" {
		return "-"
	}
	return text
}

func formatFloat(value float64, precision int) string {
	return strconv.FormatFloat(value, 'f', precision, 64)
}

func formatFloatPtr(value *float64, precision int) string {
	if value == nil {
		return "-"
	}
	if math.IsInf(*value, 1) {
		return "inf"
	}
	return strconv.FormatFloat(*value, 'f', precision, 64)
}

func bucketStatusText(bucket BucketReport) string {
	if len(bucket.Notes) == 0 {
		return bucket.Status
	}
	return bucket.Status + " (" + strings.Join(bucket.Notes, ",") + ")"
}

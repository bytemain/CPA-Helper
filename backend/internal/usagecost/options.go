package usagecost

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	backendApp "cpa-helper/backend/internal/app"
)

// GroupBy is the closed set of dimensions this tool aggregates over. It is
// closed on purpose: an unrecognised value is rejected at parse time rather
// than silently producing a report grouped by something else.
type GroupBy string

const (
	GroupByModel    GroupBy = "model"
	GroupByProvider GroupBy = "provider"
	GroupByEndpoint GroupBy = "endpoint"
	// GroupBySourceAccount answers "which upstream account is carrying the
	// traffic", which none of the other three can: a single model or provider
	// is served by several underlying accounts.
	GroupBySourceAccount GroupBy = "source-account"
)

var groupByValues = []GroupBy{GroupByModel, GroupByProvider, GroupByEndpoint, GroupBySourceAccount}

// Options is the fully validated command line. Nothing downstream re-checks
// these, so ParseArgs must leave no invalid state behind.
type Options struct {
	DBPath  string
	GroupBy GroupBy
	// Since is the start of the reporting window, already resolved against the
	// caller's clock. Storing the instant rather than the day count means the
	// window cannot shift underneath a long-running report.
	Since time.Time
	// SinceDays is kept for the report header so the output can say what was
	// asked for, not only what it resolved to.
	SinceDays int
	JSON      bool
}

// ParseArgs validates the command line. `now` is injected so tests do not
// depend on the wall clock.
func ParseArgs(args []string, now time.Time) (Options, error) {
	opts := Options{}
	var (
		groupByRaw string
		sinceRaw   string
	)
	for i := 0; i < len(args); i++ {
		arg := args[i]
		name, inlineValue, hasInline := strings.Cut(arg, "=")
		value := func() (string, error) {
			if hasInline {
				if inlineValue == "" {
					return "", fmt.Errorf("%s needs a value", name)
				}
				return inlineValue, nil
			}
			if i+1 >= len(args) {
				return "", fmt.Errorf("%s needs a value", name)
			}
			i++
			return args[i], nil
		}
		var err error
		switch name {
		case "--db":
			opts.DBPath, err = value()
		case "--group-by":
			groupByRaw, err = value()
		case "--since":
			sinceRaw, err = value()
		case "--json":
			if hasInline {
				err = fmt.Errorf("--json takes no value")
			}
			opts.JSON = true
		default:
			err = fmt.Errorf("unknown flag %q", name)
		}
		if err != nil {
			return Options{}, err
		}
	}

	if opts.DBPath == "" {
		// Same resolution the service uses (CPA_HELPER_DATA_DIR, else
		// <repo>/data): omitting --db must report on the database the service
		// writes to, not fail for want of a path the user would only guess at.
		defaultPath, err := backendApp.UsageDBPath()
		if err != nil {
			return Options{}, fmt.Errorf("--db not given and the default could not be resolved: %w", err)
		}
		opts.DBPath = defaultPath
	}
	if groupByRaw == "" {
		return Options{}, fmt.Errorf("--group-by is required (one of %s)", joinGroupBy())
	}
	matched := false
	for _, candidate := range groupByValues {
		if groupByRaw == string(candidate) {
			opts.GroupBy, matched = candidate, true
			break
		}
	}
	if !matched {
		return Options{}, fmt.Errorf("--group-by %q is not one of %s", groupByRaw, joinGroupBy())
	}
	if sinceRaw == "" {
		return Options{}, fmt.Errorf("--since is required (a whole number of days)")
	}
	// Deliberately strict: "7d", "7.0" and " 7" are all rejected rather than
	// guessed at, because a misread window silently changes every number in the
	// report and nothing downstream can detect it.
	days, err := strconv.Atoi(sinceRaw)
	if err != nil || days <= 0 {
		return Options{}, fmt.Errorf("--since %q must be a positive whole number of days", sinceRaw)
	}
	opts.SinceDays = days
	opts.Since = now.Add(-time.Duration(days) * 24 * time.Hour)
	return opts, nil
}

func joinGroupBy() string {
	parts := make([]string, 0, len(groupByValues))
	for _, value := range groupByValues {
		parts = append(parts, string(value))
	}
	return strings.Join(parts, "|")
}

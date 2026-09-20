package usagecost

import (
	"testing"
	"time"
)

var testNow = time.Date(2026, 9, 20, 4, 0, 0, 0, time.UTC)

func TestParseArgsAcceptsSeparatedAndInlineValues(t *testing.T) {
	// Both spellings must land on the same Options. The separated form consumes
	// the NEXT argv entry, so this also pins that the parser's cursor really
	// advances past a consumed value -- if it did not, "model" would be read a
	// second time as a flag and the parse would fail.
	for _, args := range [][]string{
		{"--db", "/tmp/x.sqlite3", "--group-by", "model", "--since", "7"},
		{"--db=/tmp/x.sqlite3", "--group-by=model", "--since=7"},
	} {
		opts, err := ParseArgs(args, testNow)
		if err != nil {
			t.Fatalf("ParseArgs(%v) = %v", args, err)
		}
		if opts.DBPath != "/tmp/x.sqlite3" || opts.GroupBy != GroupByModel || opts.SinceDays != 7 {
			t.Fatalf("ParseArgs(%v) = %+v", args, opts)
		}
		// Assert the same arithmetic the parser does (a rolling 7x24h window),
		// not a calendar-day equivalent that only coincides in UTC.
		if want := testNow.Add(-7 * 24 * time.Hour); !opts.Since.Equal(want) {
			t.Fatalf("Since = %s, want %s", opts.Since, want)
		}
		if opts.JSON {
			t.Fatal("JSON must default to false")
		}
	}
}

func TestParseArgsAcceptsEveryDimensionInTheClosedSet(t *testing.T) {
	// Every advertised dimension must actually parse. Adding a constant without
	// adding it to groupByValues would leave a flag the help text offers and the
	// parser rejects.
	for _, want := range []GroupBy{GroupByModel, GroupByProvider, GroupByEndpoint, GroupBySourceAccount} {
		opts, err := ParseArgs([]string{"--db", "x", "--group-by", string(want), "--since", "1"}, testNow)
		if err != nil {
			t.Fatalf("--group-by %s: %v", want, err)
		}
		if opts.GroupBy != want {
			t.Fatalf("GroupBy = %s, want %s", opts.GroupBy, want)
		}
	}
}

func TestParseArgsJSONIsAFlagNotAValue(t *testing.T) {
	opts, err := ParseArgs([]string{"--db", "x", "--group-by", "provider", "--since", "1", "--json"}, testNow)
	if err != nil {
		t.Fatal(err)
	}
	if !opts.JSON || opts.GroupBy != GroupByProvider {
		t.Fatalf("got %+v", opts)
	}
	// --json must not swallow a following argument, or "--json --since 1" would
	// silently lose the window.
	if _, err := ParseArgs([]string{"--db", "x", "--group-by", "endpoint", "--json", "--since", "3"}, testNow); err != nil {
		t.Fatalf("--json before another flag: %v", err)
	}
}

func TestParseArgsDefaultsDBToTheServicePath(t *testing.T) {
	// Omitting --db must land on the same database the service writes to
	// (CPA_HELPER_DATA_DIR / <repo>/data), not fail for want of a path.
	t.Setenv("CPA_HELPER_DATA_DIR", "/tmp/feiniu-usagecost-data")
	opts, err := ParseArgs([]string{"--group-by", "model", "--since", "7"}, testNow)
	if err != nil {
		t.Fatal(err)
	}
	want := "/tmp/feiniu-usagecost-data/db/cpa_helper.sqlite3"
	if opts.DBPath != want {
		t.Fatalf("DBPath = %q, want the service default %q", opts.DBPath, want)
	}
}

func TestParseArgsRejectsEveryInvalidShape(t *testing.T) {
	// Each of these must FAIL. A report that silently groups by the wrong
	// dimension or covers the wrong window looks exactly like a correct one.
	cases := map[string][]string{
		"missing group-by":    {"--db", "x", "--since", "7"},
		"missing since":       {"--db", "x", "--group-by", "model"},
		"unknown group-by":    {"--db", "x", "--group-by", "user", "--since", "7"},
		"group-by underscore": {"--db", "x", "--group-by", "source_account", "--since", "7"},
		"since zero":          {"--db", "x", "--group-by", "model", "--since", "0"},
		"since negative":      {"--db", "x", "--group-by", "model", "--since", "-3"},
		"since with suffix":   {"--db", "x", "--group-by", "model", "--since", "7d"},
		"since fractional":    {"--db", "x", "--group-by", "model", "--since", "7.0"},
		"unknown flag":        {"--db", "x", "--group-by", "model", "--since", "7", "--limit", "5"},
		"dangling value":      {"--db", "x", "--group-by", "model", "--since"},
		"empty inline value":  {"--db=", "--group-by", "model", "--since", "7"},
		"json with value":     {"--db", "x", "--group-by", "model", "--since", "7", "--json=true"},
	}
	for name, args := range cases {
		if _, err := ParseArgs(args, testNow); err == nil {
			t.Fatalf("%s: ParseArgs(%v) must fail", name, args)
		}
	}
}

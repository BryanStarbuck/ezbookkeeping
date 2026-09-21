package commands

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/BryanStarbuck/ezbookkeeping/cli/internal/app"
)

func asTestEnv(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("EZBK_CREDENTIALS_FILE", dir+"/creds.json")
	t.Setenv("EZBK_STATE_DIR", dir+"/state")
	t.Setenv("EZBK_STATEMENTS_DIR", "")
	t.Setenv("EZBK_API_KEY", "")
	t.Setenv("EZBK_API_KEY_FILE", "")
}

func asMustLoc(t *testing.T, name string) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation(name)

	if err != nil {
		t.Skipf("no tzdata for %s: %v", name, err)
	}

	return loc
}

func TestAsSpanRelativeWordsInTwoTimezones(t *testing.T) {
	// 2026-03-01 03:30 UTC is still 28 February in Los Angeles and already 1 March in Tokyo
	instant := time.Date(2026, 3, 1, 3, 30, 0, 0, time.UTC)

	cases := []struct {
		zone, word, start, end string
	}{
		{"America/Los_Angeles", "today", "2026-02-28", "2026-02-28"},
		{"Asia/Tokyo", "today", "2026-03-01", "2026-03-01"},
		{"America/Los_Angeles", "yesterday", "2026-02-27", "2026-02-27"},
		{"Asia/Tokyo", "yesterday", "2026-02-28", "2026-02-28"},
		{"America/Los_Angeles", "this-month", "2026-02-01", "2026-02-28"},
		{"Asia/Tokyo", "this-month", "2026-03-01", "2026-03-31"},
		{"America/Los_Angeles", "last-month", "2026-01-01", "2026-01-31"},
		{"Asia/Tokyo", "last_month", "2026-02-01", "2026-02-28"},
		{"America/Los_Angeles", "this-year", "2026-01-01", "2026-12-31"},
		{"Asia/Tokyo", "LAST-YEAR", "2025-01-01", "2025-12-31"},
	}

	for _, tc := range cases {
		now := instant.In(asMustLoc(t, tc.zone))
		s, e, rel, err := asSpan(tc.word, now)

		if err != nil {
			t.Fatalf("%s %s: %v", tc.zone, tc.word, err)
		}

		if !rel {
			t.Errorf("%s: should be relative", tc.word)
		}

		if got := s.Format("2006-01-02"); got != tc.start {
			t.Errorf("%s %s start = %s, want %s", tc.zone, tc.word, got, tc.start)
		}

		if got := e.Format("2006-01-02"); got != tc.end {
			t.Errorf("%s %s end = %s, want %s", tc.zone, tc.word, got, tc.end)
		}
	}
}

func TestAsSpanAbsoluteForms(t *testing.T) {
	now := time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC)

	for _, tc := range []struct{ in, start, end string }{
		{"2025-06-15", "2025-06-15", "2025-06-15"},
		{"2024-02", "2024-02-01", "2024-02-29"},
		{"2025", "2025-01-01", "2025-12-31"},
	} {
		s, e, rel, err := asSpan(tc.in, now)

		if err != nil || rel {
			t.Fatalf("%s: err=%v rel=%v", tc.in, err, rel)
		}

		if s.Format("2006-01-02") != tc.start || e.Format("2006-01-02") != tc.end {
			t.Errorf("%s → %s..%s, want %s..%s", tc.in, s.Format("2006-01-02"), e.Format("2006-01-02"), tc.start, tc.end)
		}
	}

	for _, bad := range []string{"2025-13-01", "2025-02-30", "next-week", "06/30/2025", ""} {
		if _, _, _, err := asSpan(bad, now); err == nil {
			t.Errorf("%q should be refused", bad)
		}
	}
}

func TestAsResolveDayEndsOfSpans(t *testing.T) {
	now := time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC)

	d, resolved, err := asResolveDay("2025-06", true, now)

	if err != nil || d != "2025-06-30" || !resolved {
		t.Fatalf("end of 2025-06 = %s %v %v", d, resolved, err)
	}

	d, resolved, err = asResolveDay("2025-06", false, now)

	if err != nil || d != "2025-06-01" || !resolved {
		t.Fatalf("start of 2025-06 = %s %v %v", d, resolved, err)
	}

	d, resolved, err = asResolveDay("2025-06-10", true, now)

	if err != nil || d != "2025-06-10" || resolved {
		t.Fatalf("a plain date is not a resolution: %s %v %v", d, resolved, err)
	}
}

func asTestCtx(verbName string, values map[string][]string, bools map[string]bool) (*app.Ctx, *bytes.Buffer, *bytes.Buffer) {
	var out, errOut bytes.Buffer
	v, _ := app.Lookup(strings.Fields(verbName))

	return app.NewTestCtx(v, nil, values, bools, &out, &errOut), &out, &errOut
}

func TestAsResolveRange(t *testing.T) {
	asTestEnv(t)
	now := time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC)

	c, _, _ := asTestCtx("analytics spending", map[string][]string{"period": {"last-month"}}, nil)
	r, err := asResolveRange(c, now, true)

	if err != nil || r.Start != "2026-08-01" || r.End != "2026-08-31" || len(r.Notes) != 1 {
		t.Fatalf("--period last-month: %+v %v", r, err)
	}

	c, _, _ = asTestCtx("analytics spending", map[string][]string{"period": {"2025"}, "start": {"2025-01-01"}}, nil)

	if _, err := asResolveRange(c, now, true); err == nil {
		t.Fatal("--period with --start must be refused")
	}

	c, _, _ = asTestCtx("analytics spending", map[string][]string{"start": {"this-year"}, "end": {"today"}}, nil)
	r, err = asResolveRange(c, now, true)

	if err != nil || r.Start != "2026-01-01" || r.End != "2026-09-21" || len(r.Notes) != 2 {
		t.Fatalf("this-year..today: %+v %v", r, err)
	}

	c, _, _ = asTestCtx("analytics spending", map[string][]string{"start": {"2026-01-01"}}, nil)

	if _, err := asResolveRange(c, now, true); err == nil || !strings.Contains(err.Error(), "--end") {
		t.Fatalf("a missing --end must be named: %v", err)
	}

	c, _, _ = asTestCtx("analytics spending", map[string][]string{"start": {"2026-02-01"}, "end": {"2026-01-01"}}, nil)

	if _, err := asResolveRange(c, now, true); err == nil {
		t.Fatal("an inverted range must be refused")
	}

	c, _, _ = asTestCtx("analytics recurring", nil, nil)
	r, err = asResolveRange(c, now, false)

	if err != nil || r.Start != "" || r.End != "" {
		t.Fatalf("an optional empty range: %+v %v", r, err)
	}
}

func asDecode(t *testing.T, s string) any {
	t.Helper()
	dec := json.NewDecoder(strings.NewReader(s))
	dec.UseNumber()
	var v any

	if err := dec.Decode(&v); err != nil {
		t.Fatal(err)
	}

	return v
}

func TestAsFlattenSeriesCSVContract(t *testing.T) {
	data := asDecode(t, `{
	  "series": [
	    {"key": "c1", "label": "Groceries", "currency": "USD",
	     "points": [{"period": "2026-01", "amount": 12345}, {"period": "2026-02", "amount": -5}, {"period": "2026-03"}]},
	    {"key": "c2", "label": "Rent", "currency": "EUR", "points": [{"period": "2026-01", "amount": 100000}]}
	  ],
	  "provenance": {"series": [{"label": "must not be flattened", "points": [{"period": "x", "amount": 1}]}]}
	}`)

	rows, ok := asFlattenSeries(data)

	if !ok || len(rows) != 4 {
		t.Fatalf("rows = %+v ok=%v", rows, ok)
	}

	recs := asSeriesRecords(rows)
	want := [][]string{
		{"Groceries", "2026-01", "USD", "12345", "123.45"},
		{"Groceries", "2026-02", "USD", "-5", "-0.05"},
		{"Groceries", "2026-03", "USD", "", ""},
		{"Rent", "2026-01", "EUR", "100000", "1000.00"},
	}

	for i := range want {
		if strings.Join(recs[i], ",") != strings.Join(want[i], ",") {
			t.Errorf("record %d = %v, want %v", i, recs[i], want[i])
		}
	}

	if strings.Join(asSeriesCSVHeader, ",") != "series,period,currency,amount_hundredths,amount" {
		t.Fatal("the charting CSV header is a contract")
	}
}

func TestAsFlattenSeriesNestedPerCurrencyAndMultiField(t *testing.T) {
	data := asDecode(t, `{
	  "currencies": [
	    {"currency": "JPY", "series": [{"label": "net", "points": [{"period": "2026", "income": 500, "expense": 200}]}]}
	  ]
	}`)

	rows, ok := asFlattenSeries(data)

	if !ok || len(rows) != 2 {
		t.Fatalf("rows = %+v", rows)
	}

	if rows[0].Series != "net.income" || rows[0].Currency != "JPY" || rows[1].Series != "net.expense" {
		t.Fatalf("rows = %+v", rows)
	}

	if _, ok := asFlattenSeries(asDecode(t, `{"payees": []}`)); ok {
		t.Fatal("no series means ok=false")
	}
}

func TestAsProvenanceLines(t *testing.T) {
	p := asDecode(t, `{
	  "range": {"start": "2026-01-01", "end": "2026-03-31", "timezone": "America/Los_Angeles"},
	  "accountIds": ["1", "2"],
	  "includeTransfers": false,
	  "convertTo": "USD",
	  "rates": [{"from": "EUR", "to": "USD", "rate": "1.0842", "source": "provider", "updateTime": "2026-09-20T00:00:00Z"}],
	  "rateBasis": "latest",
	  "unconverted": [{"currency": "XAU"}],
	  "extra": 7
	}`).(map[string]any)

	text := strings.Join(asProvenanceLines(p, map[string]any{"partial": true}, false), "\n")

	for _, want := range []string{"2026-01-01 .. 2026-03-31", "America/Los_Angeles", "2 included", "transfers", "excluded", "EUR→USD 1.0842", "provider", "latest", "UNCONVERTED", "XAU", "extra", "PARTIAL"} {
		if !strings.Contains(text, want) {
			t.Errorf("provenance lacks %q:\n%s", want, text)
		}
	}

	if asProvenanceLines(nil, nil, false) != nil {
		t.Fatal("no provenance prints nothing")
	}
}

func TestAsNameResolution(t *testing.T) {
	data := asDecode(t, `{"accounts": [
	  {"id": "3401855937219633152", "name": "Northbank Checking", "subAccounts": [{"id": "3401855937219633153", "name": "Travel"}]},
	  {"id": "3401855937219633154", "name": "northbank checking"},
	  {"id": "3401855937219633155", "name": "Card"},
	  {"id": "3401855937219633156", "name": "card"}
	]}`)

	all := asCollectNamed(data)

	if len(all) != 5 {
		t.Fatalf("collected %+v", all)
	}

	if all[1].Path != "Northbank Checking" {
		t.Errorf("a sub-account carries its parent: %+v", all[1])
	}

	if m := asMatchName(all, "Northbank Checking"); len(m) != 1 || m[0].Id != "3401855937219633152" {
		t.Errorf("exact match wins: %+v", m)
	}

	if m := asMatchName(all, "CARD"); len(m) != 2 {
		t.Errorf("two case-insensitive matches are ambiguous: %+v", m)
	}

	if m := asMatchName(all, "travel"); len(m) != 1 {
		t.Errorf("case-insensitive single match: %+v", m)
	}

	if !asIsId("3401855937219633152") || asIsId("12a") || asIsId("0") || asIsId("") {
		t.Error("asIsId")
	}
}

func TestAsValidateCurrencyAndInterval(t *testing.T) {
	if v, err := asValidateCurrency("usd"); err != nil || v != "USD" {
		t.Fatal(v, err)
	}

	for _, bad := range []string{"US", "USDX", "U$D"} {
		if _, err := asValidateCurrency(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}

	if _, err := asValidateInterval("fortnight", "day", "month"); err == nil {
		t.Error("unknown interval accepted")
	}

	if v, err := asValidateInterval("Month", "day", "month"); err != nil || v != "month" {
		t.Error(v, err)
	}
}

// every flag cli.mdx §6 shows on an analytics verb is declared on it (§7.3, AC 16)
func TestAsAnalyticsFlagsDeclared(t *testing.T) {
	want := map[string][]string{
		"analytics summary":           {"period"},
		"analytics spending":          {"start", "end", "interval", "group-by", "top", "convert-to"},
		"analytics income":            {"start", "end", "interval", "group-by", "top", "convert-to"},
		"analytics income-vs-expense": {"start", "end", "interval", "convert-to"},
		"analytics cash-flow":         {"start", "end", "account"},
		"analytics net-worth":         {"start", "end", "interval", "convert-to"},
		"analytics trend":             {"category", "start", "end"},
		"analytics tags":              {"tag", "start", "end", "convert-to"},
		"analytics payees":            {"start", "end", "top"},
		"analytics recurring":         {},
		"analytics anomalies":         {},
		"analytics runway":            {"basis"},
		"analytics import-fallout":    {"run"},
	}

	for name, flags := range want {
		v, n := app.Lookup(strings.Fields(name))

		if v == nil || n != len(strings.Fields(name)) {
			t.Errorf("verb %q is not registered", name)
			continue
		}

		declared := map[string]bool{}

		for _, f := range v.Flags {
			declared[f.Name] = true
		}

		for _, f := range flags {
			if !declared[f] {
				t.Errorf("%s: --%s is not declared", name, f)
			}
		}
	}
}

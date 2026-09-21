package commands

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/BryanStarbuck/ezbookkeeping/cli/internal/app"
	"github.com/BryanStarbuck/ezbookkeeping/cli/internal/exitcode"
)

// asTree builds a synthetic statements tree (invented names only) and returns its real path
func asTree(t *testing.T) string {
	t.Helper()
	root := t.TempDir()

	for _, d := range []string{
		"household/Northbank/Checking_x4021/2025/09",
		"household/Northbank/Card_x7734/2025/09",
		"acme_llc/Meridian/Checking_x1111/2026/01",
		".ezbk-staging",
	} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o700); err != nil {
			t.Fatal(err)
		}
	}

	if err := os.WriteFile(filepath.Join(root, "household/Northbank/Checking_x4021/2025/09/20250930-statement-4021.ofx"), []byte("<OFX/>"), 0o600); err != nil {
		t.Fatal(err)
	}

	real, err := filepath.EvalSymlinks(root)

	if err != nil {
		t.Fatal(err)
	}

	return real
}

func asExitCode(err error) int {
	var ee *app.ExitError

	if errors.As(err, &ee) {
		return ee.Code
	}

	return -1
}

func TestAsResolveRootRefusesWithoutRootAndNamesTheSetting(t *testing.T) {
	asTestEnv(t)

	_, err := asResolveRoot("", "")

	if err == nil {
		t.Fatal("no root must be refused")
	}

	var ee *app.ExitError

	if !errors.As(err, &ee) || ee.Code != exitcode.Usage {
		t.Fatalf("want exit 2, got %v", err)
	}

	for _, want := range []string{"--path", "EZBK_STATEMENTS_DIR", "ezbookkeeping.statements.root"} {
		if !strings.Contains(ee.Hint, want) {
			t.Errorf("the refusal must name %s: %q", want, ee.Hint)
		}
	}
}

func TestAsResolveRootOrder(t *testing.T) {
	asTestEnv(t)
	tree := asTree(t)
	other := asTree(t)

	// credentials file
	credPath := os.Getenv("EZBK_CREDENTIALS_FILE")

	if err := os.WriteFile(credPath, []byte(`{"ezbookkeeping":{"statements":{"root":"`+other+`"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}

	r, err := asResolveRoot("", "")

	if err != nil || r.Root != other || !strings.Contains(r.Source, "statements.root") {
		t.Fatalf("credentials root: %+v %v", r, err)
	}

	// the environment beats the credentials file
	t.Setenv("EZBK_STATEMENTS_DIR", tree)
	r, err = asResolveRoot("", "")

	if err != nil || r.Root != tree || r.Source != "EZBK_STATEMENTS_DIR" {
		t.Fatalf("env root: %+v %v", r, err)
	}

	// --path inside the configured root wins, and a PATH scopes inside it
	sub := filepath.Join(tree, "household")
	r, err = asResolveRoot(sub, "")

	if err != nil || r.Root != sub || r.Source != "--path" {
		t.Fatalf("--path root: %+v %v", r, err)
	}

	r, err = asResolveRoot("", filepath.Join(tree, "household/Northbank"))

	if err != nil || r.Root != tree || r.Target != filepath.Join(tree, "household/Northbank") {
		t.Fatalf("PATH scope: %+v %v", r, err)
	}

	entity, bank, account, year := r.asScope()

	if entity != "household" || bank != "Northbank" || account != "" || year != "" {
		t.Fatalf("scope = %s/%s/%s/%s", entity, bank, account, year)
	}
}

func TestAsResolveRootContainment(t *testing.T) {
	asTestEnv(t)
	tree := asTree(t)
	outside := t.TempDir()
	t.Setenv("EZBK_STATEMENTS_DIR", tree)

	// --path outside the configured root
	if _, err := asResolveRoot(outside, ""); asExitCode(err) != exitcode.Usage || !strings.Contains(err.Error(), "outside") {
		t.Fatalf("--path outside the root: %v", err)
	}

	// a relative escape
	wd, _ := os.Getwd()
	defer os.Chdir(wd)
	_ = os.Chdir(filepath.Join(tree, "household"))

	if _, err := asResolveRoot("", "../../"); asExitCode(err) != exitcode.Usage {
		t.Fatalf("../../ escape: %v", err)
	}

	// a symlink inside the tree that points out of it is followed, then refused
	link := filepath.Join(tree, "household", "escape")

	if err := os.Symlink(outside, link); err != nil {
		t.Skip("symlinks unavailable:", err)
	}

	if _, err := asResolveRoot("", link); asExitCode(err) != exitcode.Usage {
		t.Fatalf("symlink escape: %v", err)
	}

	// the staging directory is never a target
	if _, err := asResolveRoot("", filepath.Join(tree, ".ezbk-staging")); asExitCode(err) != exitcode.Usage {
		t.Fatalf("staging as target: %v", err)
	}

	// a missing PATH is a fact (exit 3)
	if _, err := asResolveRoot("", filepath.Join(tree, "nope")); asExitCode(err) != exitcode.NotFound {
		t.Fatalf("missing PATH: %v", err)
	}
}

func TestAsResolveRootPositionalIsRootWhenNothingConfigured(t *testing.T) {
	asTestEnv(t)
	tree := asTree(t)

	r, err := asResolveRoot("", tree)

	if err != nil || r.Root != tree || r.Target != tree || r.Source != "PATH" {
		t.Fatalf("%+v %v", r, err)
	}
}

func TestAsInside(t *testing.T) {
	for _, tc := range []struct {
		root, target string
		want         bool
	}{
		{"/a/b", "/a/b", true},
		{"/a/b", "/a/b/c", true},
		{"/a/b", "/a/bc", false},
		{"/a/b", "/a", false},
		{"/a/b", "/a/b/../c", false},
		{"/a/b", "/a/b/..c", true},
	} {
		if got := asInside(tc.root, filepath.Clean(tc.target)); got != tc.want {
			t.Errorf("asInside(%s, %s) = %v", tc.root, tc.target, got)
		}
	}
}

func TestAsParsePairs(t *testing.T) {
	m, err := asParsePairs("column-map", []string{"date=Posted Date", "amount=Amount=USD", "date=Posted Date"})

	if err != nil || m["date"] != "Posted Date" || m["amount"] != "Amount=USD" {
		t.Fatalf("%v %v", m, err)
	}

	for _, bad := range [][]string{{"=x"}, {"x="}, {"novalue"}, {"a=1", "a=2"}} {
		if _, err := asParsePairs("column-map", bad); err == nil {
			t.Errorf("%v accepted", bad)
		}
	}
}

func TestAsParseMatches(t *testing.T) {
	m, err := asParseMatches([]string{"r1=3401855937219633152", "r2=3401855937219633153,r1=3401855937219633152"})

	if err != nil || len(m) != 2 || m[0]["transaction_id"] != "3401855937219633152" || m[1]["import_id"] != "r2" {
		t.Fatalf("%v %v", m, err)
	}

	// an import id may itself contain '=': the last '=' splits
	if m, err := asParseMatches([]string{"acme/x/1111:ab==3401855937219633152"}); err != nil || m[0]["import_id"] != "acme/x/1111:ab=" {
		t.Fatalf("%v %v", m, err)
	}

	for _, bad := range []string{"r1", "=5", "r1=", "r1=3.4e18", "r1=-5"} {
		if _, err := asParseMatches([]string{bad}); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}

	if _, err := asParseMatches([]string{"r1=1", "r1=2"}); err == nil {
		t.Error("a row linked to two transactions must be refused")
	}
}

func TestAsQIFOrder(t *testing.T) {
	for in, want := range map[string]string{"mdy": "mdy", "QIF_DMY": "dmy", "": ""} {
		if got, err := asQIFOrder(in); err != nil || got != want {
			t.Errorf("%q → %q %v", in, got, err)
		}
	}

	if _, err := asQIFOrder("myd"); err == nil {
		t.Error("myd accepted")
	}
}

func TestAsTypeFromFlagsNeverGuessesQIF(t *testing.T) {
	asTestEnv(t)

	c, _, _ := asTestCtx("statements import-file", nil, nil)

	if _, err := asTypeFromFlags(c, "/x/a.qif"); err == nil {
		t.Fatal("a .qif without a date order must be refused")
	}

	c, _, _ = asTestCtx("statements import-file", map[string][]string{"qif-date-order": {"mdy"}}, nil)

	if got, err := asTypeFromFlags(c, "/x/a.QIF"); err != nil || got != "qif_mdy" {
		t.Fatalf("%q %v", got, err)
	}

	c, _, _ = asTestCtx("statements import-file", map[string][]string{"type": {"qif"}}, nil)

	if _, err := asTypeFromFlags(c, "/x/a.txt"); err == nil {
		t.Fatal("--type qif without an order must be refused")
	}

	c, _, _ = asTestCtx("statements import-file", map[string][]string{"type": {"OFX"}}, nil)

	if got, err := asTypeFromFlags(c, "/x/a.txt"); err != nil || got != "ofx" {
		t.Fatalf("%q %v", got, err)
	}

	c, _, _ = asTestCtx("statements import-file", nil, nil)

	if got, err := asTypeFromFlags(c, "/x/a.ofx"); err != nil || got != "" {
		t.Fatalf("the extension is the server's to read: %q %v", got, err)
	}
}

func TestAsApplyGate(t *testing.T) {
	c, _, _ := asTestCtx("statements apply", nil, map[string]bool{"write": true})

	if asExitCode(asApplyGate(c)) != exitcode.Usage {
		t.Fatal("--write without --yes must be refused")
	}

	c, _, _ = asTestCtx("statements apply", nil, map[string]bool{"reimport-deleted": true})

	if asExitCode(asApplyGate(c)) != exitcode.Usage {
		t.Fatal("--reimport-deleted without --yes must be refused, even as a dry run")
	}

	c, _, _ = asTestCtx("statements apply", nil, map[string]bool{"write": true, "yes": true, "reimport-deleted": true})

	if err := asApplyGate(c); err != nil {
		t.Fatal(err)
	}

	c, _, _ = asTestCtx("statements apply", nil, nil)

	if err := asApplyGate(c); err != nil {
		t.Fatal("a plain dry run needs nothing:", err)
	}
}

func TestAsDescribeIsOrdered(t *testing.T) {
	got := asDescribe(map[string]int{"skip": 3, "create": 1204, "zeta": 1, "link": 2})

	if got != "1,204 create, 2 link, 3 skip, 1 zeta" {
		t.Fatal(got)
	}

	if asDescribe(nil) != "no changes" {
		t.Fatal("empty")
	}
}

func TestAsGroupThousands(t *testing.T) {
	for n, want := range map[int64]string{0: "0", 999: "999", 1000: "1,000", 12007: "12,007", -1234567: "-1,234,567"} {
		if got := asGroupThousands(n); got != want {
			t.Errorf("%d → %s", n, got)
		}
	}
}

// every flag cli.mdx §6 shows on a statements verb is declared on it (§7.3, AC 16)
func TestAsStatementsFlagsDeclared(t *testing.T) {
	want := map[string][]string{
		"statements manifest":    {"path"},
		"statements scan":        {"path", "entity", "bank", "account", "year"},
		"statements missing":     {"path"},
		"statements extract":     {"path", "force"},
		"statements dupes":       {"path", "prefer"},
		"statements accounts":    {"path"},
		"statements plan":        {"path", "fallback-expense", "fallback-income", "qif-date-order"},
		"statements apply":       {"path", "accept-transfer", "accept-match", "reimport-deleted", "fallback-expense", "fallback-income"},
		"statements import-file": {"account", "type", "column-map"},
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

	for _, name := range []string{"statements extract", "statements apply"} {
		if v, _ := app.Lookup(strings.Fields(name)); v == nil || !v.LongRunning {
			t.Errorf("%s must be LongRunning (no client-side timeout, cli.mdx §7.4)", name)
		}
	}
}

func TestAsMissingFromCoverageAndScan(t *testing.T) {
	cov := asDecode(t, `{"accounts": [
	  {"account_key": "household/Northbank/4021", "entity": "household", "institution": "Northbank", "account": "Checking_x4021", "missing": ["2025-02", "2025-03"]},
	  {"account_key": "acme_llc/Meridian/1111", "entity": "acme_llc", "institution": "Meridian", "account": "Checking_x1111", "missing": []}
	]}`).(map[string]any)

	got := asMissingFrom(cov)

	if len(got) != 2 || got[0].line() != "household/Northbank/Checking_x4021 2025-02" || got[1].Month != "2025-03" {
		t.Fatalf("%+v", got)
	}

	scan := asDecode(t, `{"scan": {"accounts": [
	  {"entity": "household", "institution": "Northbank", "account": "Card_x7734", "missing_months": ["2026-01"]}
	]}}`).(map[string]any)

	got = asMissingFrom(scan)

	if len(got) != 1 || got[0].line() != "household/Northbank/Card_x7734 2026-01" {
		t.Fatalf("%+v", got)
	}
}

func TestAsZeroRowStatements(t *testing.T) {
	data := asDecode(t, `{
	  "zero_row_statements": [{"account_key": "household/Northbank/4021", "statement": "household/Northbank/Checking_x4021/2025/09/20250930-statement-4021.pdf", "status": "zero_rows", "reason": "no line parsed"}],
	  "no_usable_extraction": [{"statement": "household/Northbank/Checking_x4021/2025/10/scan.pdf", "status": "no_usable_extraction"}],
	  "ok_every_statement_yielded_rows": false
	}`).(map[string]any)

	got := asZeroRowStatements(data)

	if len(got) != 2 || !strings.Contains(got[0], "20250930-statement-4021.pdf") || !strings.Contains(got[0], "no line parsed") || !strings.Contains(got[1], "scan.pdf") {
		t.Fatalf("%v", got)
	}

	if asZeroRowStatements(map[string]any{}) != nil {
		t.Fatal("nothing named when nothing is zero")
	}
}

// a synthetic plan in the server's shape (routes_ingest.go / ingest_plan.go)
const asTestPlan = `{"plan": {
  "kind": "statements", "mode": "prepared",
  "accounts": [{
    "account_key": "household/Northbank/4021", "account_name": "Household · Northbank Checking ••4021", "currency": "USD", "converter": "ofx",
    "statements": {"primary": 21, "superseded": 3, "conflict": 0},
    "rows": {"parsed": 368, "after_dedupe": 368},
    "already_present": 241, "already_present_deleted": 2, "reimport_deleted": 0, "possible_matches": 1, "transfer_candidates": 4, "accepted_transfers": 0,
    "new": 120, "to_fallback": 9, "unmapped": 0, "blocked": false, "first": "2024-10-01", "last": "2026-06-30", "warnings": []
  }, {
    "account_key": "household/Northbank/7734", "currency": "USD", "statements": {"primary": 2}, "rows": {"parsed": 10, "after_dedupe": 10},
    "new": 0, "blocked": true, "blocked_rows": 10, "blocked_reasons": ["no account is mapped"]
  }],
  "totals": {"accounts": 2, "new": 120, "changes": 125, "creates": 120, "links": 5},
  "transfer_candidates": [{"id": "tc_1", "date": "2025-01-05", "amount": 50000, "currency": "USD",
    "from": {"account_key": "household/Northbank/4021"}, "to": {"account_key": "household/Northbank/7734"}, "accepted": false}],
  "already_present_deleted": [
    {"import_id": "i1", "account_key": "household/Northbank/4021", "transaction_id": "3401855937219633152", "date": "2025-03-01", "amount": -500, "currency": "USD", "description": "COFFEE", "will_reimport": true},
    {"import_id": "i2", "account_key": "household/Northbank/4021", "transaction_id": "3401855937219633153", "date": "2025-03-02", "amount": -700, "currency": "USD", "description": "TEA", "will_reimport": false}
  ]
}, "confirm_token": "cf_x"}`

func TestAsPlanText(t *testing.T) {
	data := asDecode(t, asTestPlan).(map[string]any)
	text := asPlanText(asPlanOf(data))

	for _, want := range []string{
		`household/Northbank/4021  ->  "Household · Northbank Checking ••4021" (USD)`,
		"21 primary, 3 superseded",
		"368 parsed (converter: ofx)  ->  368 after de-dupe",
		"241 already imported   2 already imported, deleted since (kept deleted)",
		"1 possible match with a hand-entered transaction (excluded)",
		"4 transfer candidates with household/Northbank/7734 (not accepted)",
		"120 NEW   (9 to the fallback category)",
		"2024-10-01 .. 2026-06-30",
		"(not mapped to an account",
		"BLOCKED",
		"no account is mapped",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("plan text lacks %q:\n%s", want, text)
		}
	}
}

func TestAsDeletedRowsNamesOnlyWhatReimports(t *testing.T) {
	all, re := asDeletedRows(asPlanOf(asDecode(t, asTestPlan).(map[string]any)))

	if len(all) != 2 || len(re) != 1 || !strings.Contains(re[0], "3401855937219633152") || !strings.Contains(re[0], "COFFEE") {
		t.Fatalf("all=%v re=%v", all, re)
	}
}

func TestAsReadApplyOutcome(t *testing.T) {
	data := asDecode(t, `{"dry_run": false, "journal_id": 12, "result": {"run_id": "run_20260921T000000Z_ab", "created": 120, "linked": 1, "all_accounts_ok": false,
	  "accounts": [{"account_key": "a/b/1", "created": 120, "status": "ok"}, {"account_key": "a/b/2", "created": 0, "status": "blocked"}, {"account_key": "a/b/3", "status": "failed"}]}}`).(map[string]any)

	out := asReadApplyOutcome(data)

	if out.RunId != "run_20260921T000000Z_ab" || out.Created != 120 || out.Linked != 1 || !out.HasAllOk || out.AllOk {
		t.Fatalf("%+v", out)
	}

	if len(out.Blocked) != 1 || out.Blocked[0] != "a/b/2" || len(out.Failed) != 1 || out.Failed[0] != "a/b/3" {
		t.Fatalf("%+v", out)
	}
}

func TestAsTotalsDiff(t *testing.T) {
	was := asDecode(t, `{"new": 120, "links": 5}`).(map[string]any)
	now := asDecode(t, `{"new": 121, "links": 5}`).(map[string]any)

	if got := asTotalsDiff(was, now); got != "new 120 → 121" {
		t.Fatal(got)
	}

	if got := asTotalsDiff(was, was); !strings.Contains(got, "same counts") {
		t.Fatal(got)
	}
}

func TestAsColumnMap(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "map.json")

	if err := os.WriteFile(path, []byte(`{"time_format": "YYYY-MM-DD", "column_mapping": {"1": 0}}`), 0o600); err != nil {
		t.Fatal(err)
	}

	m, err := asColumnMap([]string{"@" + path, "has_header_line=false", `transaction_type_mapping={"Debit": 3}`, "time_format=MM/DD/YYYY"})

	if err != nil {
		t.Fatal(err)
	}

	if m["time_format"] != "MM/DD/YYYY" || m["has_header_line"] != false || m["column_mapping"] == nil || m["transaction_type_mapping"] == nil {
		t.Fatalf("%v", m)
	}

	if m, err := asColumnMap(nil); m != nil || err != nil {
		t.Fatal("no flags, no column_map")
	}

	for _, bad := range [][]string{{"nonsense"}, {"colour=red"}, {"has_header_line=maybe"}, {"column_mapping=[1"}, {"{not json"}} {
		if _, err := asColumnMap(bad); err == nil {
			t.Errorf("%v accepted", bad)
		}
	}
}

func TestAsAccountsNotesReadsLiabilitiesAndCurrencies(t *testing.T) {
	data := asDecode(t, `{"plan": [
	  {"account_key": "household/Northbank/7734", "action": "create", "proposed": {"name": "Household · Northbank Card ••7734", "category": "credit_card", "currency": "USD", "side": "liability"}},
	  {"account_key": "household/Meridian/9999", "action": "create", "proposed": {"name": "Household · Meridian Savings ••9999", "category": "savings", "currency": "EUR", "currency_defaulted": true, "side": "asset"}},
	  {"account_key": "acme_llc/Northbank/4021", "action": "ambiguous", "reason": "two accounts end in 4021", "candidates": [{"id": "3401855937219633152", "name": "Checking A"}]}
	], "warnings": ["2 rows share a last4"]}`).(map[string]any)

	notes, amb := asAccountsNotes(data)
	text := strings.Join(notes, "\n")

	if amb != 1 {
		t.Fatal("one ambiguous row")
	}

	for _, want := range []string{"LIABILITY household/Northbank/7734", "CURRENCY household/Meridian/9999 defaulted to EUR", "AMBIGUOUS acme_llc/Northbank/4021", "--link acme_llc/Northbank/4021=<id>", "1 in EUR", "1 in USD", "2 rows share a last4"} {
		if !strings.Contains(text, want) {
			t.Errorf("notes lack %q:\n%s", want, text)
		}
	}
}

func TestAsAccountOverrides(t *testing.T) {
	c, _, _ := asTestCtx("statements accounts", map[string][]string{
		"name": {"a/b/1=Joint Checking"}, "link": {"a/b/2=3401855937219633152"}, "skip": {"a/b/3"},
	}, nil)

	ov, err := asAccountOverrides(c)

	if err != nil || ov["a/b/1"]["name"] != "Joint Checking" || ov["a/b/2"]["action"] != "link" || ov["a/b/2"]["account_id"] != "3401855937219633152" || ov["a/b/3"]["action"] != "skip" {
		t.Fatalf("%v %v", ov, err)
	}

	c, _, _ = asTestCtx("statements accounts", map[string][]string{"skip": {"a/b/3"}, "create": {"a/b/3"}}, nil)

	if _, err := asAccountOverrides(c); err == nil {
		t.Fatal("two decisions for one key must be refused")
	}

	c, _, _ = asTestCtx("statements accounts", map[string][]string{"link": {"a/b/2=Checking"}}, nil)

	if _, err := asAccountOverrides(c); err == nil {
		t.Fatal("--link takes an id")
	}
}

func TestAsScanRows(t *testing.T) {
	scan := asDecode(t, `{"accounts": [{"account_key": "household/Northbank/4021", "entity": "household", "institution": "Northbank", "account": "Checking_x4021",
	  "statements": 12, "years": [{"year": "2025", "statements": 11}, {"year": "2026", "statements": 1}],
	  "missing_months": ["2025-02"], "duplicate_scans": [{"month": "2025-03"}], "unusable": ["x.pdf", "y.pdf"]}]}`).(map[string]any)

	rows := asScanRows(scan)

	if len(rows) != 1 {
		t.Fatal(rows)
	}

	r := rows[0].(map[string]any)

	if r["years"] != "2025(11) 2026(1)" || r["missing"] != 1 || r["missing_months"] != "2025-02" || r["duplicate_scans"] != 1 || r["unusable"] != 2 {
		t.Fatalf("%v", r)
	}
}

func TestAsCategoryTypeOf(t *testing.T) {
	for in, want := range map[string][2]string{
		"expense:Groceries": {"expense", "Groceries"},
		"Income:Salary":     {"income", "Salary"},
		"Fees: bank":        {"", "Fees: bank"},
		"Groceries":         {"", "Groceries"},
	} {
		typ, name := asCategoryTypeOf(in)

		if typ != want[0] || name != want[1] {
			t.Errorf("%q → %q %q", in, typ, name)
		}
	}
}

func TestAsScopeFiltersFromPath(t *testing.T) {
	asTestEnv(t)
	tree := asTree(t)
	t.Setenv("EZBK_STATEMENTS_DIR", tree)

	r, err := asResolveRoot("", filepath.Join(tree, "household/Northbank/Checking_x4021/2025"))

	if err != nil {
		t.Fatal(err)
	}

	c, _, _ := asTestCtx("statements scan", map[string][]string{"bank": {"Meridian"}}, nil)
	f := asScopeFilters(c, r)

	if f["entity"] != "household" || f["institution"] != "Meridian" || f["account"] != "Checking_x4021" || f["year"] != "2025" {
		t.Fatalf("the flags win over the PATH: %v", f)
	}
}

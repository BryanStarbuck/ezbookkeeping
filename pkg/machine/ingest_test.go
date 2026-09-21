package machine

import (
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/mayswind/ezbookkeeping/pkg/converters"
	"github.com/mayswind/ezbookkeeping/pkg/converters/converter"
	"github.com/mayswind/ezbookkeeping/pkg/core"
	"github.com/mayswind/ezbookkeeping/pkg/errs"
	"github.com/mayswind/ezbookkeeping/pkg/models"
)

// All fixtures under testdata/ingest are synthetic: invented banks, entities and amounts.

func ingTestEnv(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("EZBK_CREDENTIALS_FILE", filepath.Join(dir, "creds.json"))
	t.Setenv("EZBK_STATE_DIR", filepath.Join(dir, "state"))
	t.Setenv(ingEnvStatementDir, "")
}

// ingCopyTree copies a fixture tree into a temp dir (tests never write into testdata)
func ingCopyTree(t *testing.T, src string) string {
	t.Helper()
	dst := filepath.Join(t.TempDir(), "statements")

	err := filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		rel, _ := filepath.Rel(src, p)
		target := filepath.Join(dst, rel)

		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}

		data, err := os.ReadFile(p)

		if err != nil {
			return err
		}

		return os.WriteFile(target, data, 0o644)
	})

	if err != nil {
		t.Fatalf("copy fixtures: %v", err)
	}

	return dst
}

func ingTestCtx() *Ctx {
	return &Ctx{Loc: time.UTC, User: &models.User{Uid: 7, Username: "operator", DefaultCurrency: "USD"}, Uid: 7}
}

// ingTestParse runs the SAME upstream converter the parse handler runs, without a database
func ingTestParse(t *testing.T, fileType string, data []byte) []*models.ImportTransactionResponse {
	t.Helper()
	imp, err := converters.GetTransactionDataImporter(fileType)

	if err != nil {
		t.Fatalf("no importer for %s: %v", fileType, err)
	}

	user := &models.User{Uid: 7, DefaultCurrency: "USD"}
	txns, _, _, _, _, _, err := imp.ParseImportedData(core.NewNullContext(), user, data, time.UTC, converter.TransactionDataImporterOptions{}, nil, nil, nil, nil, nil)

	if err != nil {
		t.Fatalf("parse %s: %v", fileType, err)
	}

	return txns.ToImportTransactionResponseList()
}

func ingFixture(t *testing.T, rel string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "ingest", rel))

	if err != nil {
		t.Fatalf("fixture %s: %v", rel, err)
	}

	return data
}

// ---------------------------------------------------------------------------------------------

func TestIngDecimalHundredths(t *testing.T) {
	cases := map[string]int64{"0": 0, "1": 100, "-5.00": -500, "+12.5": 1250, "2500.00": 250000, "0.07": 7, "-0.1": -10, "999999999999.99": 99999999999999, "3.10": 310}

	for in, want := range cases {
		got, ok := ingDecimalHundredths(in)

		if !ok || got != want {
			t.Errorf("ingDecimalHundredths(%q) = %d, %v; want %d", in, got, ok, want)
		}
	}

	for _, bad := range []string{"", "1.234", "abc", "1,00", "--1", "1e5"} {
		if _, ok := ingDecimalHundredths(bad); ok {
			t.Errorf("ingDecimalHundredths(%q) accepted", bad)
		}
	}
}

func TestIngNormDesc(t *testing.T) {
	if got := ingNormDesc("  CORNER-Coffee   #12, Inc. "); got != "corner coffee 12 inc" {
		t.Fatalf("got %q", got)
	}

	if ingNormDesc("Payroll Example Corp, Inc") != ingNormDesc("Payroll Example Corp  Inc") {
		t.Fatal("punctuation and spacing must not change identity")
	}
}

func ingTRow(key, date string, amount int64, desc, file string, idx int) *ingRow {
	return &ingRow{AccountKey: key, Date: date, Month: date[:7], Amount: amount, Currency: "USD", Description: desc, NormDesc: ingNormDesc(desc), SourceFile: file, SourceIndex: idx}
}

func TestIngTwoCoffeesBothImport(t *testing.T) {
	a := ingTRow("household/Northbank/4021", "2025-09-03", -500, "Corner Coffee", "s.csv", 1)
	b := ingTRow("household/Northbank/4021", "2025-09-03", -500, "Corner Coffee", "s.csv", 2)
	ingAssignIds([]*ingRow{a, b})

	if a.ImportId == b.ImportId {
		t.Fatal("two genuine same-day coffees must get distinct ids")
	}

	if a.Ordinal+b.Ordinal != 1 {
		t.Fatalf("ordinals %d, %d", a.Ordinal, b.Ordinal)
	}

	// deterministic: the same rows in another order get the same ids
	c := ingTRow("household/Northbank/4021", "2025-09-03", -500, "Corner Coffee", "s.csv", 2)
	d := ingTRow("household/Northbank/4021", "2025-09-03", -500, "Corner Coffee", "s.csv", 1)
	ingAssignIds([]*ingRow{c, d})
	ids := []string{a.ImportId, b.ImportId}
	ids2 := []string{c.ImportId, d.ImportId}
	sort.Strings(ids)
	sort.Strings(ids2)

	if ids[0] != ids2[0] || ids[1] != ids2[1] {
		t.Fatal("ids must not depend on input order")
	}

	if len(a.ImportId) != 32 || a.IdSource != "minted" {
		t.Fatalf("minted id %q (%s)", a.ImportId, a.IdSource)
	}
}

func TestIngCycleDatedCardFeedsOneMonth(t *testing.T) {
	// two statements (Sep 15 – Oct 14 and Oct 15 – Nov 14) both carry a genuine $9.99 on the
	// same October day; they are two real transactions and both must import
	s1 := &ingStatement{File: "card/2025-10-14.ofx", FileType: "ofx", Sha: "a", Rows: []*ingRow{ingTRow("k/b/7734", "2025-10-14", -999, "Stream Service", "card/2025-10-14.ofx", 1)}, First: "2025-09-15", Last: "2025-10-14", PeriodSource: "document"}
	s2 := &ingStatement{File: "card/2025-11-14.ofx", FileType: "ofx", Sha: "b", Rows: []*ingRow{ingTRow("k/b/7734", "2025-10-15", -999, "Stream Service", "card/2025-11-14.ofx", 1)}, First: "2025-10-15", Last: "2025-11-14", PeriodSource: "document"}
	res := ingLayerOne("k/b/7734", []*ingStatement{s1, s2})

	if len(res.Rows) != 2 || len(res.Conflicts) != 0 {
		t.Fatalf("rows %d conflicts %d", len(res.Rows), len(res.Conflicts))
	}

	// same day, same amount, from two statements feeding one calendar month
	s3 := &ingStatement{File: "a.ofx", FileType: "ofx", Sha: "c", Rows: []*ingRow{ingTRow("k/b/7734", "2025-10-14", -999, "Stream Service", "a.ofx", 1)}, First: "2025-09-15", Last: "2025-10-14", PeriodSource: "document"}
	s4 := &ingStatement{File: "b.ofx", FileType: "ofx", Sha: "d", Rows: []*ingRow{ingTRow("k/b/7734", "2025-10-14", -999, "Stream Service", "b.ofx", 1)}, First: "2025-10-15", Last: "2025-11-14", PeriodSource: "document"}
	res = ingLayerOne("k/b/7734", []*ingStatement{s3, s4})
	ingAssignIds(res.Rows)

	if len(res.Rows) != 2 || res.Rows[0].ImportId == res.Rows[1].ImportId {
		t.Fatal("the merged account-month ordinal must keep both rows")
	}
}

func TestIngLayerOneVerdicts(t *testing.T) {
	key := "household/Northbank/4021"
	sep := func(file, sha string, rows ...*ingRow) *ingStatement {
		return &ingStatement{File: file, FileType: "ofx", Sha: sha, Rows: rows}
	}

	full := sep("full.ofx", "x1", ingTRow(key, "2025-09-01", -100, "A", "full.ofx", 1), ingTRow(key, "2025-09-10", -200, "B", "full.ofx", 2), ingTRow(key, "2025-09-20", 300, "C", "full.ofx", 3))
	same := sep("same.ofx", "x1", ingTRow(key, "2025-09-01", -100, "A", "same.ofx", 1), ingTRow(key, "2025-09-10", -200, "B", "same.ofx", 2), ingTRow(key, "2025-09-20", 300, "C", "same.ofx", 3))
	partial := sep("partial.ofx", "x2", ingTRow(key, "2025-09-01", -100, "A", "partial.ofx", 1), ingTRow(key, "2025-09-10", -200, "B", "partial.ofx", 2))

	res := ingLayerOne(key, []*ingStatement{partial, same, full})
	verdicts := map[string]*ingDupeVerdict{}

	for _, v := range res.Verdicts {
		verdicts[v.File] = v
	}

	if verdicts["same.ofx"].Verdict != "duplicate_identical" {
		t.Fatalf("identical bytes: %+v", verdicts["same.ofx"])
	}

	if verdicts["partial.ofx"].Verdict != "superseded" || verdicts["partial.ofx"].Rule != "more_rows" || verdicts["partial.ofx"].Winner != "full.ofx" {
		t.Fatalf("subset: %+v", verdicts["partial.ofx"])
	}

	if len(res.Rows) != 3 {
		t.Fatalf("rows %d", len(res.Rows))
	}

	// disagreement → conflict, the month blocked, both files named
	other := sep("other.ofx", "x3", ingTRow(key, "2025-09-01", -100, "A", "other.ofx", 1), ingTRow(key, "2025-09-12", -999, "Z", "other.ofx", 2), ingTRow(key, "2025-09-20", 300, "C", "other.ofx", 3))
	res = ingLayerOne(key, []*ingStatement{full, other})

	if len(res.Conflicts) != 1 || !res.BlockedMonths["2025-09"] || len(res.Rows) != 0 {
		t.Fatalf("conflict: %+v rows=%d", res.Conflicts, len(res.Rows))
	}

	if files := res.Conflicts[0].Files; len(files) != 2 {
		t.Fatalf("both files must be named: %v", files)
	}

	// prefer resolves it
	other.Preferred = true
	res = ingLayerOne(key, []*ingStatement{full, other})

	if len(res.Conflicts) != 0 || len(res.Rows) != 3 {
		t.Fatalf("prefer: conflicts=%d rows=%d", len(res.Conflicts), len(res.Rows))
	}

	for _, v := range res.Verdicts {
		if v.File == "full.ofx" && (v.Verdict != "superseded" || v.Rule != "preferred") {
			t.Fatalf("prefer verdict %+v", v)
		}
	}

	// an empty statement is reported, never silently dropped
	res = ingLayerOne(key, []*ingStatement{sep("empty.ofx", "x9")})

	if len(res.Verdicts) != 1 || res.Verdicts[0].Verdict != "empty" {
		t.Fatalf("empty: %+v", res.Verdicts)
	}
}

func TestIngLayerTwoSameBankId(t *testing.T) {
	a := ingTRow("k/b/1", "2025-09-01", -100, "A", "a.ofx", 1)
	b := ingTRow("k/b/1", "2025-09-01", -100, "A", "b.ofx", 1)
	a.BankId, a.BankIdKind = "F1", "FITID"
	b.BankId, b.BankIdKind = "F1", "FITID"
	ingAssignIds([]*ingRow{a, b})
	rows, collapsed := ingLayerTwo([]*ingRow{b, a})

	if len(rows) != 1 || len(collapsed) != 1 || collapsed[0].Rule != "same_bank_id" || collapsed[0].KeptSource != "a.ofx" {
		t.Fatalf("rows=%d collapsed=%+v", len(rows), collapsed)
	}
}

func TestIngOfxFitidUsedVerbatim(t *testing.T) {
	data := ingFixture(t, "prepared/bank/household/Northbank/Checking_x4021/Checking_x4021_ALL.ofx")
	items := ingTestParse(t, "ofx", data)
	rows, skipped := ingNormalize(items, ingNormalizeInput{AccountKey: "household/Northbank/4021", SourceFile: "all.ofx", SourceKind: "ofx", FileType: "ofx", ExpectedCurrency: "USD"})

	if len(skipped) != 0 || len(rows) != 6 {
		t.Fatalf("rows=%d skipped=%+v", len(rows), skipped)
	}

	refs := ingBankRefs("ofx", data)

	if len(refs) != 6 {
		t.Fatalf("refs %d", len(refs))
	}

	if !ingAlignBankIds(rows, refs) {
		t.Fatal("FITIDs must pair one-to-one with the converter's rows")
	}

	ingAssignIds(rows)
	seen := map[string]bool{}

	for _, r := range rows {
		if r.IdSource != "bank" || !strings.HasPrefix(r.ImportId, "household/Northbank/4021|FITID:NB2025") {
			t.Fatalf("row %s %d has id %q", r.Date, r.Amount, r.ImportId)
		}

		if seen[r.ImportId] {
			t.Fatal("duplicate id")
		}

		seen[r.ImportId] = true

		if r.Currency != "USD" || r.CurrencySource != "file" {
			t.Fatalf("currency %s/%s", r.Currency, r.CurrencySource)
		}
	}

	// the payroll credit is money in, the card payment money out
	var payroll, payment *ingRow

	for _, r := range rows {
		switch r.BankId {
		case "NB202509050001":
			payroll = r
		case "NB202509200001":
			payment = r
		}
	}

	if payroll == nil || payroll.Amount != 250000 || payment == nil || payment.Amount != -30000 {
		t.Fatalf("signs: payroll=%+v payment=%+v", payroll, payment)
	}

	// a duplicated FITID disqualifies the file (an id that repeats is not an id)
	refs[1].Id = refs[0].Id

	for _, r := range rows {
		r.BankId = ""
	}

	if ingAlignBankIds(rows, refs) {
		t.Fatal("duplicate FITIDs must not be used")
	}
}

func TestIngMonthlyAndCombinedCollapseToOneSet(t *testing.T) {
	key := "household/Northbank/4021"
	var stmts []*ingStatement

	for _, rel := range []string{
		"prepared/bank/household/Northbank/Checking_x4021/Checking_x4021_ALL.ofx",
		"prepared/bank/household/Northbank/Checking_x4021/2025/09/20250930-statement-4021.ofx",
		"prepared/bank/household/Northbank/Checking_x4021/2025/10/20251031-statement-4021.ofx",
	} {
		data := ingFixture(t, rel)
		rows, _ := ingNormalize(ingTestParse(t, "ofx", data), ingNormalizeInput{AccountKey: key, SourceFile: rel, SourceKind: "ofx", FileType: "ofx", ExpectedCurrency: "USD"})

		if !ingAlignBankIds(rows, ingBankRefs("ofx", data)) {
			t.Fatalf("%s: FITIDs did not align", rel)
		}

		stmts = append(stmts, &ingStatement{File: rel, FileType: "ofx", Sha: ingSha256(data), Rows: rows})
	}

	res := ingLayerOne(key, stmts)
	ingAssignIds(res.Rows)
	rows, _ := ingLayerTwo(res.Rows)

	if len(rows) != 6 || len(res.Conflicts) != 0 {
		t.Fatalf("rows=%d conflicts=%+v verdicts=%+v", len(rows), res.Conflicts, res.Verdicts)
	}

	for _, v := range res.Verdicts {
		if strings.HasSuffix(v.File, "_ALL.ofx") && v.Verdict != "primary" {
			t.Fatalf("the combined file should win: %+v", v)
		}

		if !strings.HasSuffix(v.File, "_ALL.ofx") && v.Verdict != "superseded" {
			t.Fatalf("monthly files are covered by the combined file: %+v", v)
		}
	}
}

func TestIngQifNeedsDateOrder(t *testing.T) {
	root := filepath.Join("testdata", "ingest", "prepared", "bank", "household", "Northbank", "Card_x7734", "2025", "10", "20251014-statement-7734.qif")

	if _, ft, needs := ingClassifyImportable(root, ""); ft != "" || needs != "qif_date_order" {
		t.Fatalf("QIF without an order: ft=%q needs=%q", ft, needs)
	}

	if _, ft, needs := ingClassifyImportable(root, "mdy"); ft != "qif_mdy" || needs != "" {
		t.Fatalf("QIF with an order: ft=%q needs=%q", ft, needs)
	}

	rows, _ := ingNormalize(ingTestParse(t, "qif_mdy", ingFixture(t, "prepared/bank/household/Northbank/Card_x7734/2025/10/20251014-statement-7734.qif")), ingNormalizeInput{AccountKey: "household/Northbank/7734", SourceFile: "c.qif", SourceKind: "qif_mdy", FileType: "qif_mdy", ExpectedCurrency: "USD"})

	if len(rows) != 2 {
		t.Fatalf("rows %d", len(rows))
	}

	for _, r := range rows {
		if r.CurrencySource != "account" || r.Currency != "USD" {
			t.Fatalf("QIF carries no currency; got %s from %s", r.Currency, r.CurrencySource)
		}
	}
}

func TestIngClassify(t *testing.T) {
	dir := t.TempDir()
	write := func(name, content string) string {
		p := filepath.Join(dir, name)
		_ = os.WriteFile(p, []byte(content), 0o600)
		return p
	}

	cases := []struct{ path, ft, needs string }{
		{write("a.ofx", "<OFX>"), "ofx", ""},
		{write("a.QFX", "<OFX>"), "qfx", ""},
		{write("c.xml", `<Document xmlns="urn:iso:std:iso:20022:tech:xsd:camt.053.001.02">`), "camt053", ""},
		{write("d.xml", `<note/>`), "", ""},
		{write("e.csv", "Time,Timezone,Type,Category\n"), "ezbookkeeping_csv", ""},
		{write("f.csv", "Date,Payee,Amount\n"), "", "column_map"},
		{write("g.sta", ":20:X\n:61:"), "mt940", ""},
	}

	for _, c := range cases {
		if _, ft, needs := ingClassifyImportable(c.path, ""); ft != c.ft || needs != c.needs {
			t.Errorf("%s: ft=%q needs=%q; want %q %q", filepath.Base(c.path), ft, needs, c.ft, c.needs)
		}
	}
}

func TestIngManifest(t *testing.T) {
	ingTestEnv(t)
	root, err := ingResolveRoot(ingCopyTree(t, filepath.Join("testdata", "ingest", "prepared")))

	if err != nil {
		t.Fatal(err)
	}

	real, err := ingFindManifest(root, "")

	if err != nil || real == "" {
		t.Fatalf("manifest not found: %v", err)
	}

	m, err := ingReadManifest(root, real, "USD")

	if err != nil {
		t.Fatal(err)
	}

	if m.Path != "import/accounts.csv" || len(m.Rows) != 3 || len(m.MissingColumns) != 0 {
		t.Fatalf("manifest %+v", m)
	}

	var savings *ingManifestRow

	for _, r := range m.Rows {
		if r.Last4 == "5520" {
			savings = r
		}
	}

	if savings == nil || !savings.CurrencyDefaulted || savings.Currency != "USD" || savings.AccountKey() != "acme_llc/Meridian/5520" {
		t.Fatalf("defaulted currency must be said: %+v", savings)
	}

	// a manifest missing columns reports them
	bad := filepath.Join(root.Real, "bad.csv")
	_ = os.WriteFile(bad, []byte("entity,label\nhousehold,X\n"), 0o600)
	mb, err := ingReadManifest(root, bad, "USD")

	if err != nil {
		t.Fatal(err)
	}

	if strings.Join(mb.MissingColumns, ",") != "institution,last4,kind,path" {
		t.Fatalf("missing %v", mb.MissingColumns)
	}

	// the shelf chooses OFX and sees the combined file
	shelf, err := ingScanAccountShelf(root, m.Rows[0], "")

	if err != nil || shelf.FileType != "ofx" || shelf.Combined != "Checking_x4021_ALL.ofx" || shelf.Monthly != 2 {
		t.Fatalf("shelf %+v %v", shelf, err)
	}
}

func TestIngRootContainmentAndStaging(t *testing.T) {
	ingTestEnv(t)
	tree := ingCopyTree(t, filepath.Join("testdata", "ingest", "prepared"))
	root, err := ingResolveRoot(tree)

	if err != nil {
		t.Fatal(err)
	}

	if _, err := root.Resolve("../outside"); err == nil {
		t.Fatal("a path above the root must be refused")
	}

	outside := t.TempDir()
	_ = os.Symlink(outside, filepath.Join(tree, "escape"))

	if _, err := root.Resolve("escape/file.ofx"); err == nil {
		t.Fatal("a symlink out of the root must be refused")
	}

	if _, err := ingResolveRoot("relative/path"); err == nil {
		t.Fatal("a relative root must be refused")
	}

	// the LOCKED conflict rule: never stage into the manifest's own import/ directory
	if _, err := ingResolveStaging(root, "import", "import/accounts.csv"); err == nil || toFail(err).Code != CodeConflict {
		t.Fatalf("staging into import/ must conflict: %v", err)
	}

	st, err := ingResolveStaging(root, "", "import/accounts.csv")

	if err != nil || st.Rel != ".ezbk-staging" {
		t.Fatalf("default staging %v %v", st, err)
	}

	if err := st.Ensure(); err != nil {
		t.Fatal(err)
	}

	if gi, _ := os.ReadFile(filepath.Join(st.Dir, ".gitignore")); string(gi) != "*\n" {
		t.Fatalf(".gitignore = %q", gi)
	}

	// a directory holding foreign files is refused
	foreign := filepath.Join(tree, "mine")
	_ = os.MkdirAll(foreign, 0o755)
	_ = os.WriteFile(filepath.Join(foreign, "notes.txt"), []byte("operator's"), 0o600)
	st2, err := ingResolveStaging(root, "mine", "")

	if err != nil {
		t.Fatal(err)
	}

	if err := st2.Ensure(); err == nil || toFail(err).Code != CodeConflict {
		t.Fatalf("foreign files must conflict: %v", err)
	}

	if _, err := st.Path("../import/accounts.csv"); err == nil {
		t.Fatal("a staging path must not escape staging")
	}
}

func TestIngScanPrepared(t *testing.T) {
	ingTestEnv(t)
	root, _ := ingResolveRoot(ingCopyTree(t, filepath.Join("testdata", "ingest", "prepared")))
	real, _ := ingFindManifest(root, "")
	m, _ := ingReadManifest(root, real, "USD")
	res := ingScan(root, m, "", map[string]string{}, "")

	if res.Layout != "manifest" || res.Mode != "prepared" || len(res.Accounts) != 3 {
		t.Fatalf("scan %+v", res)
	}

	var checking *ingScanAccount

	for _, a := range res.Accounts {
		if a.AccountKey == "household/Northbank/4021" {
			checking = a
		}
	}

	if checking == nil || checking.First != "2025-09" || checking.Last != "2025-10" || len(checking.MissingMonths) != 0 {
		t.Fatalf("checking %+v", checking)
	}

	if len(checking.Undated) != 1 || !strings.Contains(checking.Undated[0], "_ALL") {
		t.Fatalf("the combined file has no date in its name: %v", checking.Undated)
	}

	filtered := ingScan(root, m, "", map[string]string{"entity": "acme_llc"}, "")

	if len(filtered.Accounts) != 1 {
		t.Fatalf("entity filter: %d", len(filtered.Accounts))
	}
}

func TestIngScanLayoutAndLast4Disagreement(t *testing.T) {
	ingTestEnv(t)
	tree := t.TempDir()
	dir := filepath.Join(tree, "household", "Northbank", "Checking_x4021", "2025")
	_ = os.MkdirAll(filepath.Join(dir, "07"), 0o755)
	_ = os.MkdirAll(filepath.Join(dir, "09"), 0o755)
	_ = os.WriteFile(filepath.Join(dir, "07", "20250731-statement-4021.pdf"), []byte("%PDF synthetic a"), 0o600)
	_ = os.WriteFile(filepath.Join(dir, "09", "20250930-statement-9999.pdf"), []byte("%PDF synthetic b"), 0o600)
	root, _ := ingResolveRoot(tree)
	res := ingScan(root, nil, "", map[string]string{}, "")

	if len(res.Accounts) != 1 || res.Accounts[0].AccountKey != "household/Northbank/4021" {
		t.Fatalf("accounts %+v", res.Accounts)
	}

	a := res.Accounts[0]

	if strings.Join(a.MissingMonths, ",") != "2025-08" {
		t.Fatalf("missing %v", a.MissingMonths)
	}

	if len(a.Warnings) != 1 || !strings.Contains(a.Warnings[0], "9999") {
		t.Fatalf("a last4 disagreement must be reported, never guessed: %v", a.Warnings)
	}

	if len(a.Unusable) != 2 {
		t.Fatalf("PDFs without sidecars are unusable: %v", a.Unusable)
	}
}

func TestIngSidecarDelimitedAndLines(t *testing.T) {
	claude := string(ingFixture(t, "raw/household/Northbank/Checking_x4021/2025/09/20250930-statement-4021_claude.txt"))
	r := ingParseSidecar(claude, "2025-09-30", "2025", "mdy")

	if r.Shape != "delimited" || len(r.Rows) != 4 || r.PeriodFirst != "2025-09-01" || r.PeriodLast != "2025-09-30" {
		t.Fatalf("claude %+v", r)
	}

	if r.Rows[2].Description != "Payroll Example Corp, Inc" || r.Rows[2].Amount != 250000 {
		t.Fatalf("row %+v", r.Rows[2])
	}

	ocr := string(ingFixture(t, "raw/household/Northbank/Checking_x4021/2025/10/20251031-statement-4021_ocr.txt"))
	r = ingParseSidecar(ocr, "2025-10-31", "2025", "mdy")

	if r.Shape != "lines" || len(r.Rows) != 3 {
		t.Fatalf("ocr rows %+v issues %+v", r.Rows, r.Issues)
	}

	want := map[string]int64{"Refund Grocer Mart": 750, "Grocer Mart": -4217, "Utility Example": -12000}

	for _, row := range r.Rows {
		if want[row.Description] != row.Amount {
			t.Fatalf("%q = %d", row.Description, row.Amount)
		}
	}

	if len(r.Issues) != 1 || r.Issues[0].Reason != "no_amount" || r.Issues[0].Line == 0 {
		t.Fatalf("issues %+v", r.Issues)
	}
}

func TestIngSidecarNeverGuesses(t *testing.T) {
	// no section and no sign: the direction is unknown, so the line is reported, not imported
	r := ingParseSidecar("10/02 Grocer Mart 42.17\n", "2025-10-31", "2025", "mdy")

	if len(r.Rows) != 0 || len(r.Issues) != 1 || r.Issues[0].Reason != "sign_unknown" {
		t.Fatalf("%+v", r)
	}

	// explicit signs: CR, DR, parentheses, trailing minus
	r = ingParseSidecar("10/02 Refund 5.00 CR\n10/03 Fee 2.50 DR\n10/04 Check (10.00)\n10/05 Withdrawal 1.25-\n", "2025-10-31", "", "mdy")

	if len(r.Rows) != 4 || r.Rows[0].Amount != 500 || r.Rows[1].Amount != -250 || r.Rows[2].Amount != -1000 || r.Rows[3].Amount != -125 {
		t.Fatalf("%+v", r.Rows)
	}

	// two amounts without a balance column header are ambiguous
	r = ingParseSidecar("Withdrawals\n10/02 Grocer 42.17 1,000.00\n", "2025-10-31", "", "mdy")

	if len(r.Rows) != 0 || r.Issues[0].Reason != "ambiguous_amount_columns" {
		t.Fatalf("%+v", r)
	}

	// December rows on a January statement belong to the previous year
	r = ingParseSidecar("Purchases\n12/30 Hardware Example 20.00\n01/02 Hardware Example 3.00\n", "2026-01-15", "", "mdy")

	if len(r.Rows) != 2 || r.Rows[0].Date != "2025-12-30" || r.Rows[1].Date != "2026-01-02" {
		t.Fatalf("%+v", r.Rows)
	}

	// no period, no closing date, no year directory: the year is unknown
	r = ingParseSidecar("Purchases\n12/30 Hardware Example 20.00\n", "", "", "mdy")

	if len(r.Rows) != 0 || r.Issues[0].Reason != "year_unknown" {
		t.Fatalf("%+v", r)
	}

	// day-first statements read with date_order=dmy
	r = ingParseSidecar("date,description,amount\n31/10/2025,Cafe Example,-3.50\n", "", "", "dmy")

	if len(r.Rows) != 1 || r.Rows[0].Date != "2025-10-31" {
		t.Fatalf("%+v %+v", r.Rows, r.Issues)
	}

	// the same date read month-first is unreadable, never swapped
	r = ingParseSidecar("date,description,amount\n31/10/2025,Cafe Example,-3.50\n", "", "", "mdy")

	if len(r.Rows) != 0 {
		t.Fatalf("a day-first date must not be read month-first: %+v", r.Rows)
	}
}

func TestIngExtractRaw(t *testing.T) {
	ingTestEnv(t)
	tree := ingCopyTree(t, filepath.Join("testdata", "ingest", "raw"))
	before := ingListFiles(t, tree)
	mc := ingTestCtx()
	ctx, err := ingOpenContext(mc, tree, "", "", "raw", false)

	if err != nil {
		t.Fatal(err)
	}

	res, err := ingExtract(mc, ctx, nil, map[string]bool{}, "mdy", false, 100, nil)

	if err != nil {
		t.Fatal(err)
	}

	if res.Totals["rows"] != 7 || len(res.NoUsable) != 1 || len(res.ZeroRow) != 0 || res.UnreadableTotal < 1 {
		t.Fatalf("extract %+v", res)
	}

	var rescan *ingDupeVerdict

	for _, v := range res.Statements {
		if strings.Contains(v.File, "rescan") {
			rescan = v
		}
	}

	if rescan == nil || rescan.Verdict != "superseded" || rescan.Rule != "more_rows" {
		t.Fatalf("the partial rescan must be superseded: %+v", rescan)
	}

	// nothing outside staging changed
	after := ingListFiles(t, tree)
	var outside []string

	for _, f := range after {
		if !strings.HasPrefix(f, ".ezbk-staging/") {
			outside = append(outside, f)
		}
	}

	if strings.Join(outside, "\n") != strings.Join(before, "\n") {
		t.Fatalf("extract wrote outside staging:\n%v\nvs\n%v", outside, before)
	}

	staged := filepath.Join(tree, ".ezbk-staging", "household", "Northbank", "Checking_x4021", "2025-09.csv")
	data, err := os.ReadFile(staged)

	if err != nil {
		t.Fatal(err)
	}

	// the staged month parses with upstream's own ezbookkeeping_csv converter, and the
	// provenance pairs back the original description and source lines
	items := ingTestParse(t, "ezbookkeeping_csv", data)
	rows, _ := ingNormalize(items, ingNormalizeInput{AccountKey: "household/Northbank/4021", SourceFile: "staged", SourceKind: "ezbookkeeping_csv", FileType: "ezbookkeeping_csv", ExpectedCurrency: "USD"})
	prov, _ := os.ReadFile(strings.TrimSuffix(staged, ".csv") + ".prov.csv")

	if len(rows) != 4 || !ingAttachProvenance(rows, prov) {
		t.Fatalf("rows=%d", len(rows))
	}

	var payroll *ingRow

	for _, r := range rows {
		if r.Amount == 250000 {
			payroll = r
		}

		if !strings.HasSuffix(r.SourceFile, "_claude.txt") || r.SourceLine == 0 {
			t.Fatalf("provenance %+v", r)
		}
	}

	if payroll == nil || payroll.Description != "Payroll Example Corp, Inc" {
		t.Fatalf("the original description must survive staging: %+v", payroll)
	}

	// a second run with nothing changed rewrites nothing but its own run files
	res2, err := ingExtract(mc, ctx, nil, map[string]bool{}, "mdy", false, 100, nil)

	if err != nil {
		t.Fatal(err)
	}

	for _, w := range res2.Written {
		if !strings.HasSuffix(w, "_accounts.json") && !strings.HasSuffix(w, "_manifest.csv") {
			t.Fatalf("unchanged extraction rewrote %s", w)
		}
	}

	// the statement ceiling reports the real limit
	if _, err := ingExtract(mc, ctx, nil, map[string]bool{}, "mdy", false, 1, nil); err == nil || toFail(err).Code != CodeConflict {
		t.Fatalf("ceiling: %v", err)
	}
}

func ingListFiles(t *testing.T, root string) []string {
	var out []string

	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			rel, _ := filepath.Rel(root, p)
			out = append(out, filepath.ToSlash(rel))
		}

		return nil
	})

	sort.Strings(out)

	return out
}

func TestIngNormalizeAttribution(t *testing.T) {
	items := []*models.ImportTransactionResponse{
		{Type: models.TRANSACTION_TYPE_EXPENSE, Time: 1757000000, OriginalSourceAccountName: "4021", OriginalSourceAccountCurrency: "USD", SourceAmount: 500, Comment: "Coffee"},
		{Type: models.TRANSACTION_TYPE_INCOME, Time: 1757000100, OriginalSourceAccountName: "4021", OriginalSourceAccountCurrency: "USD", SourceAmount: 1000, Comment: "Refund"},
		{Type: models.TRANSACTION_TYPE_TRANSFER, Time: 1757000200, OriginalSourceAccountName: "", OriginalDestinationAccountName: "4021", OriginalDestinationAccountCurrency: "USD", SourceAmount: 3000, DestinationAmount: 3000, Comment: "Payment in"},
		{Type: models.TRANSACTION_TYPE_TRANSFER, Time: 1757000300, OriginalSourceAccountName: "4021", OriginalSourceAccountCurrency: "USD", OriginalDestinationAccountName: "", SourceAmount: 700, Comment: "Transfer out"},
		{Type: models.TRANSACTION_TYPE_MODIFY_BALANCE, Time: 1757000400, OriginalSourceAccountName: "4021", SourceAmount: 100},
		{Type: models.TRANSACTION_TYPE_EXPENSE, Time: 1757000500, OriginalSourceAccountName: "9999", OriginalSourceAccountCurrency: "USD", SourceAmount: 100, Comment: "Other"},
	}

	rows, skipped := ingNormalize(items, ingNormalizeInput{AccountKey: "k", SourceFile: "f", FileType: "ofx", ExpectedCurrency: "USD"})
	amounts := []int64{}

	for _, r := range rows {
		amounts = append(amounts, r.Amount)
	}

	if len(rows) != 4 || amounts[0] != -500 || amounts[1] != 1000 || amounts[2] != 3000 || amounts[3] != -700 {
		t.Fatalf("amounts %v", amounts)
	}

	if len(skipped) != 2 || !strings.HasPrefix(skipped[0].Reason, "balance_modification") || !strings.HasPrefix(skipped[1].Reason, "other_account") {
		t.Fatalf("skipped %+v", skipped)
	}
}

func TestIngRenderName(t *testing.T) {
	row := &ingManifestRow{Entity: "household", Institution: "Northbank", Label: "Checking_x4021", Last4: "4021", Kind: "checking"}
	name, _, trunc := ingRenderName("", row)

	if name != "Household · Northbank Checking ••4021" || trunc {
		t.Fatalf("name %q", name)
	}

	row2 := &ingManifestRow{Entity: "household", Institution: "Meridian", Label: "Retirement", Kind: "brokerage"}

	if name, _, _ := ingRenderName("", row2); name != "Household · Meridian Retirement" {
		t.Fatalf("no last4: %q", name)
	}

	if ingHumanize("acme_llc") != "Acme LLC" {
		t.Fatalf("humanize %q", ingHumanize("acme_llc"))
	}

	long := &ingManifestRow{Entity: "household", Institution: strings.Repeat("Very Long Regional Cooperative Savings Institution ", 2), Last4: "4021", Kind: "savings"}
	name, full, trunc := ingRenderName("", long)

	if !trunc || utf8.RuneCountInString(name) > 64 || !strings.HasSuffix(name, "••4021") || full == name {
		t.Fatalf("truncated %q (%d)", name, utf8.RuneCountInString(name))
	}

	// stable: a re-run renders the same name
	if again, _, _ := ingRenderName("", long); again != name {
		t.Fatal("names must be stable")
	}
}

func TestIngParseAcceptMatches(t *testing.T) {
	raw := []json.RawMessage{json.RawMessage(`{"import_id":"abc","transaction_id":"3401855937219633152"}`), json.RawMessage(`"def=3401855937219633153"`)}
	m, err := ingParseAcceptMatches(raw)

	if err != nil || m["abc"] != 3401855937219633152 || m["def"] != 3401855937219633153 {
		t.Fatalf("%v %v", m, err)
	}

	if _, err := ingParseAcceptMatches([]json.RawMessage{json.RawMessage(`"no-equals"`)}); err == nil {
		t.Fatal("malformed entry accepted")
	}
}

func TestIngCamtRefs(t *testing.T) {
	doc := `<Document xmlns="urn:iso:std:iso:20022:tech:xsd:camt.053.001.02"><BkToCstmrStmt><Stmt>
<Ntry><NtryRef>E1</NtryRef><Amt Ccy="EUR">12.50</Amt><CdtDbtInd>DBIT</CdtDbtInd><BookgDt><Dt>2025-09-02</Dt></BookgDt><AcctSvcrRef>SVC-1</AcctSvcrRef></Ntry>
<Ntry><NtryRef>E2</NtryRef><Amt Ccy="EUR">100.00</Amt><CdtDbtInd>CRDT</CdtDbtInd><BookgDt><Dt>2025-09-03</Dt></BookgDt></Ntry>
</Stmt></BkToCstmrStmt></Document>`
	refs := ingBankRefs("camt053", []byte(doc))

	if len(refs) != 2 || refs[0].Id != "SVC-1" || refs[0].Kind != "AcctSvcrRef" || refs[0].Amount != -1250 || refs[1].Id != "E2" || refs[1].Kind != "NtryRef" || refs[1].Amount != 10000 {
		t.Fatalf("%+v", refs)
	}
}

func TestIngPlanRangeAndClip(t *testing.T) {
	mc := ingTestCtx()
	r, err := ingPlanRange(mc, "2025-09-01", "2025-09-30")

	if err != nil || r.Start != "2025-09-01" || r.End != "2025-09-30" {
		t.Fatalf("%+v %v", r, err)
	}

	if _, err := ingPlanRange(mc, "2025-10-01", "2025-09-01"); err == nil {
		t.Fatal("reversed range accepted")
	}

	if r, _ := ingPlanRange(mc, "", ""); r != nil {
		t.Fatal("no range means everything")
	}

	c, cut := ingClipComment(strings.Repeat("é", 300))

	if !cut || utf8.RuneCountInString(c) != 255 {
		t.Fatal("comments are clipped to 255 characters, by rune")
	}
}

func TestIngRoutesRegistered(t *testing.T) {
	want := map[string]bool{
		"GET /ingest/roots": true, "GET /ingest/converters": true, "GET /ingest/manifest": true, "POST /ingest/scan": true,
		"GET /ingest/coverage": true, "GET /ingest/map": true, "PUT /ingest/map": true, "POST /ingest/map/infer": true,
		"POST /ingest/accounts/plan": true, "POST /ingest/accounts/apply": true, "POST /ingest/extract": true,
		"GET /ingest/dupes": true, "GET /ingest/rows": true, "POST /ingest/plan": true, "POST /ingest/apply": true,
		"POST /ingest/file/plan": true, "POST /ingest/file/apply": true, "GET /ingest/runs": true, "GET /ingest/runs/:id": true,
	}

	for _, r := range ingRoutes() {
		key := r.Method + " " + r.Path

		if !want[key] {
			t.Errorf("unexpected route %s", key)
		}

		delete(want, key)

		if r.Tier == TierWrite && (!r.DryRunnable || r.Feature == nil) {
			t.Errorf("%s: write routes are dry-runnable and gated on [data] enable_import", key)
		}

		if r.Status != "" && r.Status != StatusLive {
			t.Errorf("%s is not live", key)
		}
	}

	for k := range want {
		t.Errorf("missing route %s", k)
	}

	for _, kind := range []string{ingInverseImport, ingInverseAccount, ingInverseMap} {
		if _, ok := LookupInverse(kind); !ok {
			t.Errorf("no inverse executor for %s", kind)
		}
	}
}

func TestIngMapFileRoundTrip(t *testing.T) {
	ingTestEnv(t)
	root, _ := ingResolveRoot(ingCopyTree(t, filepath.Join("testdata", "ingest", "prepared")))
	st, _ := ingResolveStaging(root, "", "import/accounts.csv")
	m := ingNewMap()
	m.Accounts["household/Northbank/4021"] = &ingMapEntry{AccountId: "3401855937219633152", Name: "Household · Northbank Checking ••4021", Currency: "USD"}

	raw, err := ingSaveMap(st, m, "operator")

	if err != nil {
		t.Fatal(err)
	}

	back, raw2, err := ingLoadMap(st)

	if err != nil || string(raw) != string(raw2) || back.Accounts["household/Northbank/4021"].AccountId != "3401855937219633152" {
		t.Fatalf("round trip %v", err)
	}

	if _, err := os.Stat(filepath.Join(root.Real, "import", ingMapFile)); err == nil {
		t.Fatal("the map must never be written into the archive")
	}
}

func TestIngRunIdAndReports(t *testing.T) {
	ingTestEnv(t)
	id := ingNewRunId()

	if !ingRunIdPattern(id) || ingRunIdPattern("../etc") || ingRunIdPattern("run_../x") {
		t.Fatalf("run id pattern %q", id)
	}

	ingSaveRunReport(7, &ingRunReport{RunId: id, Kind: "file", Outcome: "ok", Created: 3, Totals: map[string]int{}})
	runs, total, err := ingListRuns(7, 10)

	if err != nil || total != 1 || runs[0]["run_id"] != id || runs[0]["created"] != 3 {
		t.Fatalf("runs %v %d %v", runs, total, err)
	}

	if _, err := ingLoadRunReport(8, id); err == nil {
		t.Fatal("another user's run must not be visible")
	}

	if info, err := os.Stat(filepath.Join(os.Getenv("EZBK_STATE_DIR"), "ingest-runs", "7", id+".json")); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("report file %v", err)
	}
}

// pm/import_formats.mdx §11.2: a manifest written for this app names each account's one file,
// carries its opening balance, and may use another pipeline's kinds
func TestIngManifestFileOpeningAndKindAliases(t *testing.T) {
	ingTestEnv(t)
	root, err := ingResolveRoot(ingCopyTree(t, filepath.Join("testdata", "ingest", "prepared")))

	if err != nil {
		t.Fatal(err)
	}

	// two accounts sharing ONE directory, each naming its own file (synthetic data)
	dir := filepath.Join(root.Real, "import", "personal", "Northbank")
	_ = os.MkdirAll(dir, 0o755)
	ofx := func(acct, amt string) []byte {
		return []byte("OFXHEADER:100\nDATA:OFXSGML\nVERSION:102\nSECURITY:NONE\nENCODING:UTF-8\nCHARSET:NONE\nCOMPRESSION:NONE\nOLDFILEUID:NONE\nNEWFILEUID:NONE\n\n<OFX>\n<BANKMSGSRSV1><STMTTRNRS><TRNUID>1\n<STMTRS><CURDEF>USD\n<BANKACCTFROM><BANKID>000000000<ACCTID>" + acct + "<ACCTTYPE>CHECKING</BANKACCTFROM>\n<BANKTRANLIST><DTSTART>20260101<DTEND>20260131\n<STMTTRN><TRNTYPE>DEBIT<DTPOSTED>20260105120000<TRNAMT>" + amt + "<FITID>" + acct + "-1<NAME>Corner Cafe<MEMO>CORNER CAFE &amp; BAKERY</STMTTRN>\n</BANKTRANLIST></STMTRS></STMTTRNRS></BANKMSGSRSV1>\n</OFX>\n")
	}
	_ = os.WriteFile(filepath.Join(dir, "Checking_x4021_ALL_ezbookkeeping.ofx"), ofx("4021", "-4.50"), 0o644)
	_ = os.WriteFile(filepath.Join(dir, "Retirement_x7710_ALL_ezbookkeeping.ofx"), ofx("7710", "-1.00"), 0o644)
	manifest := "entity,institution,label,last4,kind,currency,name,path,file,opening_balance,opening_date\n" +
		"household,Northbank,Checking_x4021,4021,checking,USD,Northbank Checking ••4021,import/personal/Northbank,import/personal/Northbank/Checking_x4021_ALL_ezbookkeeping.ofx,1234.56,2026-01-01\n" +
		"household,Northbank,Retirement_x7710,7710,retirement,USD,Northbank IRA ••7710,import/personal/Northbank,import/personal/Northbank/Retirement_x7710_ALL_ezbookkeeping.ofx,-10.5,2026-01-01\n" +
		"household,Northbank,Mortgage_x5190,5190,mortgage,USD,,import/personal/Northbank,import/personal/Northbank/missing.ofx,12.345,2026-01-01\n"
	_ = os.WriteFile(filepath.Join(root.Real, "import", "personal", "manifest_ezbookkeeping.csv"), []byte(manifest), 0o644)

	// this app's manifest is found before another app's import/accounts.csv
	real, err := ingFindManifest(root, "")

	if err != nil || root.Rel(real) != "import/personal/manifest_ezbookkeeping.csv" {
		t.Fatalf("candidate order: %q %v", real, err)
	}

	m, err := ingReadManifest(root, real, "USD")

	if err != nil || len(m.Rows) != 3 || len(m.UnknownColumns) != 0 {
		t.Fatalf("manifest %+v %v", m, err)
	}

	chk, ira, mort := m.Rows[0], m.Rows[1], m.Rows[2]

	if chk.File == "" || chk.OpeningBalance == nil || *chk.OpeningBalance != 123456 || chk.OpeningDate != "2026-01-01" {
		t.Fatalf("checking row %+v", chk)
	}

	if ira.Kind != "brokerage" || ira.OpeningBalance == nil || *ira.OpeningBalance != -1050 || !strings.Contains(strings.Join(ira.Warnings, ";"), `"retirement" read as brokerage`) {
		t.Fatalf("kind alias %+v", ira)
	}

	if mort.Kind != "loan" || mort.OpeningBalance != nil || !strings.Contains(strings.Join(mort.Warnings, ";"), "not a plain decimal") {
		t.Fatalf("a non-hundredths opening is refused, never rounded: %+v", mort)
	}

	// the shelf is exactly the named file, never its neighbour in the shared directory
	for i, want := range []string{"Checking_x4021_ALL_ezbookkeeping.ofx", "Retirement_x7710_ALL_ezbookkeeping.ofx"} {
		shelf, err := ingScanAccountShelf(root, m.Rows[i], "")

		if err != nil || !shelf.Exists || len(shelf.Chosen) != 1 || filepath.Base(shelf.Chosen[0].Rel) != want || shelf.FileType != "ofx" || shelf.Combined != want {
			t.Fatalf("shelf %d %+v %v", i, shelf, err)
		}
	}

	if shelf, err := ingScanAccountShelf(root, mort, ""); err != nil || shelf.Exists || len(shelf.Chosen) != 0 {
		t.Fatalf("a missing file is not an empty shelf that exists: %+v %v", shelf, err)
	}
}

// the opening balance sorts strictly before noon-UTC statement rows and stays on its own date
func TestIngOpeningTime(t *testing.T) {
	day, _ := time.Parse("2006-01-02", "2026-01-01")
	got := ingOpeningTime(day)
	noon := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC).Unix()

	if got != noon-1 {
		t.Fatalf("opening time %d, want %d", got, noon-1)
	}

	for _, zone := range []int{-11, -8, 0, 11} {
		if d := time.Unix(got, 0).In(time.FixedZone("z", zone*3600)).Format("2006-01-02"); d != "2026-01-01" {
			t.Fatalf("UTC%+d shows the opening on %s", zone, d)
		}
	}
}

// a statement with no activity is an empty statement, not an unreadable file
func TestIngEmptyOfxIsNotAConflict(t *testing.T) {
	data := []byte("OFXHEADER:100\nDATA:OFXSGML\nVERSION:102\nSECURITY:NONE\nENCODING:UTF-8\nCHARSET:NONE\nCOMPRESSION:NONE\nOLDFILEUID:NONE\nNEWFILEUID:NONE\n\n<OFX>\n<BANKMSGSRSV1><STMTTRNRS><TRNUID>1\n<STMTRS><CURDEF>USD\n<BANKACCTFROM><BANKID>000000000<ACCTID>7710<ACCTTYPE>CHECKING</BANKACCTFROM>\n<BANKTRANLIST><DTSTART>20260101<DTEND>20260131\n</BANKTRANLIST></STMTRS></STMTTRNRS></BANKMSGSRSV1>\n</OFX>\n")
	imp, _ := converters.GetTransactionDataImporter("ofx")
	_, _, _, _, _, _, err := imp.ParseImportedData(core.NewNullContext(), &models.User{Uid: 7, DefaultCurrency: "USD"}, data, time.UTC, converter.TransactionDataImporterOptions{}, nil, nil, nil, nil, nil)

	if err == nil || !ingIsEmptyFile(err) {
		t.Fatalf("upstream's verdict on an empty statement: %v", err)
	}

	if ingIsEmptyFile(errs.ErrInvalidOFXFile) {
		t.Fatal("an invalid file must stay a conflict")
	}
}

package machine

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mayswind/ezbookkeeping/pkg/core"
	"github.com/mayswind/ezbookkeeping/pkg/models"
	"github.com/mayswind/ezbookkeeping/pkg/services"
)

// Synthetic books only (apis.mdx §20): Northbank, Meridian, ••4021 — nothing from real statements.

// catzImportBooks is xferOpenBooks plus the import fallback ("Imported > Uncategorized" for both
// types), a statements root with a manifest, and helpers to make imported rows
type catzImportBooks struct {
	*xferBooks
	root                            string
	fallbackExpense, fallbackIncome int64
}

func catzOpenImportBooks(t *testing.T) *catzImportBooks {
	b := &catzImportBooks{xferBooks: xferOpenBooks(t), root: t.TempDir()}
	c := core.NewNullContext()

	category := func(name string, ctype models.TransactionCategoryType, parent int64) int64 {
		cat := &models.TransactionCategory{Uid: 42, Name: name, Type: ctype, ParentCategoryId: parent, Icon: 1, Color: "000000"}

		if err := services.TransactionCategories.CreateCategory(c, cat); err != nil {
			t.Fatalf("create category %s: %v", name, err)
		}

		return cat.CategoryId
	}

	b.fallbackExpense = category("Uncategorized", models.CATEGORY_TYPE_EXPENSE, category("Imported", models.CATEGORY_TYPE_EXPENSE, 0))
	b.fallbackIncome = category("Uncategorized", models.CATEGORY_TYPE_INCOME, category("Imported", models.CATEGORY_TYPE_INCOME, 0))

	dir := filepath.Join(b.root, "import", "personal", "Northbank")

	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}

	manifest := "entity,institution,label,last4,kind,currency,name,path,file\n" +
		"personal,Northbank,Checking_x4021,4021,checking,USD,Northbank Checking ••4021,import/personal/Northbank,import/personal/Northbank/Checking_x4021_ALL_ezbookkeeping.ofx\n"

	if err := os.WriteFile(filepath.Join(b.root, "import", "personal", "manifest_ezbookkeeping.csv"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}

	return b
}

// imported makes a row as the statement import would — in category cat — and records its FITID
func (b *catzImportBooks) imported(t models.TransactionType, cat, amount int64, date, comment, fitid string) int64 {
	b.t.Helper()
	loc, _ := time.LoadLocation("America/Los_Angeles")
	d, _ := time.ParseInLocation("2006-01-02", date, loc)
	at := time.Date(d.Year(), d.Month(), d.Day(), 12, 0, 0, 0, loc)
	s := txnState{Type: int(t), CategoryId: idString(cat), Time: at.Unix(), UtcOffset: UTCOffsetMinutes(at, loc), SourceAccountId: idString(b.checking), SourceAmount: amount, Comment: comment, TagIds: []string{}}
	id, err := xferCreate(b.ctx(RouteDef{Method: "POST", Path: "/transactions", Tier: TierWrite}, ""), s)

	if err != nil {
		b.t.Fatalf("create row: %v", err)
	}

	if fitid != "" {
		b.record(ingBankImportId("personal/Northbank/4021", "FITID", fitid), id)
	}

	return id
}

// companion writes the account's companion TSV: ezBookkeeping's header plus FITID
func (b *catzImportBooks) companion(rows ...[4]string) {
	b.t.Helper()
	var sb strings.Builder
	sb.WriteString("Time\tTimezone\tType\tCategory\tSub Category\tAccount\tAccount Currency\tAmount\tAccount2\tAccount2 Currency\tAccount2 Amount\tGeographic Location\tTags\tDescription\tFITID\n")
	sb.WriteString("2026-02-28 11:59:59\t+00:00\tBalance Modification\t\t\tNorthbank Checking ••4021\tUSD\t100.00\t\t\t\t\tstatement-import\tOpening balance (per statement)\t\n")

	for _, r := range rows { // {type, group, sub, fitid}
		sb.WriteString("2026-03-02 12:00:00\t+00:00\t" + r[0] + "\t" + r[1] + "\t" + r[2] + "\tNorthbank Checking ••4021\tUSD\t1.00\t\t\t\t\tstatement-import\tSYNTHETIC ROW\t" + r[3] + "\n")
	}

	p := filepath.Join(b.root, "import", "personal", "Northbank", "Checking_x4021_ALL_ezbookkeeping.tsv")

	if err := os.WriteFile(p, []byte(sb.String()), 0o644); err != nil {
		b.t.Fatal(err)
	}
}

// categorize runs POST /ingest/categorize once with body (dry run unless body says otherwise)
func (b *catzImportBooks) categorize(body map[string]any) (*WriteResult, map[string]any, error) {
	b.t.Helper()
	body["root"] = b.root
	body["manifest_path"] = "import/personal/manifest_ezbookkeeping.csv"
	raw, _ := json.Marshal(body)
	res, err := ingHandleCategorize(b.ctx(ingCatzRoutes()[0], string(raw)))

	if err != nil {
		return nil, nil, err
	}

	wr := res.(*WriteResult)
	preview := map[string]any{}

	if wr.Preview != nil {
		p, _ := Integerize(wr.Preview)
		preview = p.(map[string]any)
	}

	return wr, preview, nil
}

func TestIngCatzEndToEnd(t *testing.T) {
	b := catzOpenImportBooks(t)
	unknownGroup := b.imported(models.TRANSACTION_TYPE_EXPENSE, b.fallbackExpense, 900, "2026-03-02", "MERIDIAN CAVIAR", "F-UNKNOWN")
	grocery := b.imported(models.TRANSACTION_TYPE_EXPENSE, b.fallbackExpense, 1250, "2026-03-02", "MERIDIAN MARKET", "F-GROCERY")
	pay := b.imported(models.TRANSACTION_TYPE_INCOME, b.fallbackIncome, 500000, "2026-03-03", "NORTHBANK PAYROLL", "F-PAY")
	blank := b.imported(models.TRANSACTION_TYPE_EXPENSE, b.fallbackExpense, 300, "2026-03-04", "NORTHBANK FEE", "F-BLANK")
	handSet := b.imported(models.TRANSACTION_TYPE_EXPENSE, b.groceries, 700, "2026-03-05", "MERIDIAN MARKET", "F-HAND")
	flipped := b.imported(models.TRANSACTION_TYPE_EXPENSE, b.fallbackExpense, 400, "2026-03-06", "MERIDIAN REFUND", "F-FLIP")

	b.companion(
		[4]string{"Expense", "Food", "Groceries", "F-GROCERY"},
		[4]string{"Income", "Earnings", "Salary", "F-PAY"},
		[4]string{"Expense", "", "", "F-BLANK"},
		[4]string{"Expense", "Food", "Groceries", "F-NEVER-IMPORTED"},
		[4]string{"Expense", "Earnings", "Salary", "F-HAND"},
		[4]string{"Expense", "Food", "Caviar", "F-UNKNOWN"},
		[4]string{"Income", "Earnings", "Salary", "F-FLIP"},
		[4]string{"Expense", "Food", "Groceries", ""},
	)

	before := b.balances()

	// the dry run: two rows change; every other row is skipped for its own reason
	dry, preview, err := b.categorize(map[string]any{})

	if err != nil {
		t.Fatal(err)
	}

	if !dry.DryRun || dry.ConfirmToken == "" {
		t.Fatalf("the default is a dry run with a token: %+v", dry)
	}

	want := map[string]int{"update": 2, "skip_balance_modification": 1, "skip_no_category": 1, "skip_not_imported": 1, "skip_already_categorized": 1, "skip_unknown_category": 1, "skip_type_differs": 1, "skip_no_fitid": 1}

	for k, v := range want {
		if dry.Changes[k] != v {
			t.Fatalf("changes[%s] = %d, want %d (all: %v)", k, dry.Changes[k], v, dry.Changes)
		}
	}

	unknown, _ := preview["unknownCategories"].([]any)

	if len(unknown) != 1 || unknown[0].(map[string]any)["path"] != "Expense > Food > Caviar" {
		t.Fatalf("unknownCategories: %v", preview["unknownCategories"])
	}

	if len(dry.Warnings) == 0 {
		t.Fatal("unknown paths and type differences are read aloud as warnings")
	}

	// the apply: the same plan, recomputed; only categories move
	dry2, _, err := b.categorize(map[string]any{"dry_run": false, "confirm_token": dry.ConfirmToken})

	if err != nil {
		t.Fatal(err)
	}

	if dry2.DryRun {
		t.Fatalf("apply came back as a dry run: %+v", dry2)
	}

	if got := b.state(grocery).CategoryId; got != idString(b.groceries) {
		t.Fatalf("grocery row: category %s, want %d", got, b.groceries)
	}

	if got := b.state(pay).CategoryId; got != idString(b.salary) {
		t.Fatalf("pay row: category %s, want %d", got, b.salary)
	}

	for name, id := range map[string]int64{"blank": blank, "unknown": unknownGroup, "flipped": flipped} {
		if got := b.state(id).CategoryId; got != idString(b.fallbackExpense) {
			t.Fatalf("%s row left its fallback: %s", name, got)
		}
	}

	if got := b.state(handSet).CategoryId; got != idString(b.groceries) {
		t.Fatalf("a hand-set category was overwritten: %s", got)
	}

	xferSameBalances(t, before, b.balances(), "after categorising")

	// a second run changes nothing: the two rows left the fallback
	again, _, err := b.categorize(map[string]any{})

	if err != nil {
		t.Fatal(err)
	}

	if again.Changes["update"] != 0 || again.Changes["skip_already_categorized"] != 3 {
		t.Fatalf("second run: %v", again.Changes)
	}

	// overwrite lifts the fallback-only rule — but a category of the other type still refuses
	over, _, err := b.categorize(map[string]any{"overwrite": true})

	if err != nil {
		t.Fatal(err)
	}

	if over.Changes["update"] != 0 || over.Changes["skip_already_categorized"] != 0 {
		t.Fatalf("overwrite: %v", over.Changes)
	}

	// undo puts the two rows back in the fallback
	b.undo()

	if got := b.state(grocery).CategoryId; got != idString(b.fallbackExpense) {
		t.Fatalf("undo: grocery row is in %s", got)
	}

	xferSameBalances(t, before, b.balances(), "after undo")
}

func TestIngCatzLimitAndFileArgument(t *testing.T) {
	b := catzOpenImportBooks(t)
	b.imported(models.TRANSACTION_TYPE_EXPENSE, b.fallbackExpense, 100, "2026-03-02", "MERIDIAN MARKET", "F-1")
	b.imported(models.TRANSACTION_TYPE_EXPENSE, b.fallbackExpense, 200, "2026-03-03", "MERIDIAN MARKET", "F-2")
	b.companion([4]string{"Expense", "Food", "Groceries", "F-1"}, [4]string{"Expense", "Food", "Groceries", "F-2"})

	dry, preview, err := b.categorize(map[string]any{"limit": 1})

	if err != nil {
		t.Fatal(err)
	}

	if dry.Changes["update"] != 1 || dry.Changes["skip_over_limit"] != 1 || fmt.Sprint(preview["remaining"]) != "1" {
		t.Fatalf("limit 1: %v remaining %v", dry.Changes, preview["remaining"])
	}

	// file names one account's category file; with two accounts selected it is refused
	if _, _, err := b.categorize(map[string]any{"file": "import/personal/Northbank/missing.tsv"}); txnFailCode(t, err) != CodeNotFound {
		t.Fatalf("a missing file is not_found, got %v", err)
	}

	if _, _, err := b.categorize(map[string]any{"file": "../outside.tsv"}); err == nil {
		t.Fatal("a file outside the statements root is refused")
	}
}

package machine

import (
	"strings"
	"testing"

	"github.com/mayswind/ezbookkeeping/pkg/models"
)

// Synthetic books only: no real names, no real amounts.

func catzTestLookup() *txnLookup {
	lk := txnTestLookup()
	extra := []*models.TransactionCategory{
		{CategoryId: 50, Name: "Imported", Type: models.CATEGORY_TYPE_EXPENSE},
		{CategoryId: 51, Name: "Uncategorized", ParentCategoryId: 50, Type: models.CATEGORY_TYPE_EXPENSE},
		{CategoryId: 60, Name: "Imported", Type: models.CATEGORY_TYPE_INCOME},
		{CategoryId: 61, Name: "Uncategorized", ParentCategoryId: 60, Type: models.CATEGORY_TYPE_INCOME},
	}

	return txnNewLookup(lk.accounts, append(lk.categories, extra...), lk.tags)
}

func TestCatzParsePath(t *testing.T) {
	cases := []struct {
		in   string
		want catzPath
	}{
		{"Coffee", catzPath{Sub: "Coffee"}},
		{"Food & Drink > Coffee", catzPath{Group: "Food & Drink", Sub: "Coffee"}},
		{" expense >Food & Drink> Coffee ", catzPath{Type: models.CATEGORY_TYPE_EXPENSE, Group: "Food & Drink", Sub: "Coffee"}},
		{"Transfer > Moves > Internal", catzPath{Type: models.CATEGORY_TYPE_TRANSFER, Group: "Moves", Sub: "Internal"}},
	}

	for _, c := range cases {
		got, err := catzParsePath(c.in)

		if err != nil || got != c.want {
			t.Fatalf("%q: got %+v %v, want %+v", c.in, got, err, c.want)
		}
	}

	for _, bad := range []string{"", "A > > B", "Food > Drink > Coffee", "A > B > C > D"} {
		if _, err := catzParsePath(bad); txnFailCode(t, err) != CodeInvalidInput {
			t.Fatalf("%q should be invalid_input, got %v", bad, err)
		}
	}

	if s := (catzPath{Type: models.CATEGORY_TYPE_INCOME, Group: "Earnings", Sub: "Salary"}).String(); s != "Income > Earnings > Salary" {
		t.Fatalf("String: %q", s)
	}
}

func TestCatzResolveCategoryForPrefersRowType(t *testing.T) {
	lk := catzTestLookup()

	// "Imported > Uncategorized" exists for both types: the row's own type decides
	c, err := lk.resolveCategoryFor("", "Imported > Uncategorized", models.TRANSACTION_TYPE_EXPENSE)

	if err != nil || c.CategoryId != 51 {
		t.Fatalf("expense row: %v %v", c, err)
	}

	c, err = lk.resolveCategoryFor("", "Imported > Uncategorized", models.TRANSACTION_TYPE_INCOME)

	if err != nil || c.CategoryId != 61 {
		t.Fatalf("income row: %v %v", c, err)
	}

	// a full path names the type exactly, even against the row's type
	c, err = lk.resolveCategoryFor("", "Income > Earnings > Salary", models.TRANSACTION_TYPE_EXPENSE)

	if err != nil || c.CategoryId != 31 {
		t.Fatalf("full path of another type: %v %v", c, err)
	}

	// a name only found under another type still resolves (the caller then needs allow_type_change)
	c, err = lk.resolveCategoryFor("", "Salary", models.TRANSACTION_TYPE_EXPENSE)

	if err != nil || c.CategoryId != 31 {
		t.Fatalf("cross-type name: %v %v", c, err)
	}

	// a group is refused, a missing name is not_found, an ambiguous one lists full paths
	if _, err := lk.resolveCategoryFor("", "Expense > Food", models.TRANSACTION_TYPE_EXPENSE); err == nil {
		t.Fatalf("two segments starting with a type are a group > sub path, and there is no such sub")
	}

	if _, err := lk.resolveCategoryFor("10", "", models.TRANSACTION_TYPE_EXPENSE); txnFailCode(t, err) != CodeInvalidInput {
		t.Fatalf("a group should be refused: %v", err)
	}

	if _, err := lk.resolveCategoryFor("", "Nowhere", models.TRANSACTION_TYPE_EXPENSE); txnFailCode(t, err) != CodeNotFound {
		t.Fatalf("missing should be not_found: %v", err)
	}

	_, err = lk.resolveCategoryFor("", "Other", models.TRANSACTION_TYPE_EXPENSE)

	if txnFailCode(t, err) != CodeInvalidInput {
		t.Fatalf("two expense 'Other' should be ambiguous: %v", err)
	}

	cands := err.(*Fail).Details.(map[string]any)["candidates"].([]map[string]string)

	if cands[0]["name"] != "Expense > Food > Other" && cands[0]["name"] != "Expense > Shopping > Other" {
		t.Fatalf("candidates should carry full paths: %v", cands)
	}
}

func TestCatzTargetStateTransitions(t *testing.T) {
	lk := catzTestLookup()
	expense := txnState{Id: "900", Type: int(models.TRANSACTION_TYPE_EXPENSE), CategoryId: "51", SourceAccountId: "101", SourceAmount: 1250, TagIds: []string{"501"}}
	income := txnState{Id: "901", Type: int(models.TRANSACTION_TYPE_INCOME), CategoryId: "61", SourceAccountId: "101", SourceAmount: 9900}
	transfer := txnState{Id: "902", Type: int(models.TRANSACTION_TYPE_TRANSFER), CategoryId: "41", SourceAccountId: "101", SourceAmount: 500, DestinationAccountId: "102", DestinationAmount: 500}

	cat := func(id int64) *models.TransactionCategory { return lk.catMap[id] }

	// same type: only the category moves
	s, err := catzTargetState(lk, expense, catzTarget{Category: cat(11)}, false)

	if err != nil || s.CategoryId != "11" || s.Type != expense.Type || s.SourceAmount != 1250 || len(s.TagIds) != 1 {
		t.Fatalf("same type: %+v %v", s, err)
	}

	// another type without permission is refused
	if _, err := catzTargetState(lk, expense, catzTarget{Category: cat(31)}, false); txnFailCode(t, err) != CodeInvalidInput {
		t.Fatalf("type change without allow should be refused: %v", err)
	}

	// expense → income flips the type, keeps account and amount
	s, err = catzTargetState(lk, expense, catzTarget{Category: cat(31)}, true)

	if err != nil || s.Type != int(models.TRANSACTION_TYPE_INCOME) || s.SourceAccountId != "101" || s.SourceAmount != 1250 {
		t.Fatalf("expense→income: %+v %v", s, err)
	}

	// expense → transfer needs the counter account; the row's account is the source
	if _, err := catzTargetState(lk, expense, catzTarget{Category: cat(41)}, true); txnFailCode(t, err) != CodeInvalidInput {
		t.Fatalf("transfer without counter should be refused: %v", err)
	}

	s, err = catzTargetState(lk, expense, catzTarget{Category: cat(41), Counter: lk.accountMap[102]}, true)

	if err != nil || s.Type != int(models.TRANSACTION_TYPE_TRANSFER) || s.SourceAccountId != "101" || s.DestinationAccountId != "102" || s.DestinationAmount != 1250 {
		t.Fatalf("expense→transfer: %+v %v", s, err)
	}

	// income → transfer: money came in, so the counter account is the source
	s, err = catzTargetState(lk, income, catzTarget{Category: cat(41), Counter: lk.accountMap[102]}, true)

	if err != nil || s.SourceAccountId != "102" || s.DestinationAccountId != "101" || s.SourceAmount != 9900 || s.DestinationAmount != 9900 {
		t.Fatalf("income→transfer: %+v %v", s, err)
	}

	// a counter account in another currency needs counter_amount
	if _, err := catzTargetState(lk, expense, catzTarget{Category: cat(41), Counter: lk.accountMap[103]}, true); txnFailCode(t, err) != CodeInvalidInput {
		t.Fatalf("cross-currency without counter_amount should be refused: %v", err)
	}

	yen := int64(180000)
	s, err = catzTargetState(lk, expense, catzTarget{Category: cat(41), Counter: lk.accountMap[103], CounterAmount: &yen}, true)

	if err != nil || s.DestinationAmount != yen {
		t.Fatalf("cross-currency with counter_amount: %+v %v", s, err)
	}

	// the counter account cannot be the row's own
	if _, err := catzTargetState(lk, expense, catzTarget{Category: cat(41), Counter: lk.accountMap[101]}, true); txnFailCode(t, err) != CodeInvalidInput {
		t.Fatalf("same account as counter should be refused: %v", err)
	}

	// transfer → expense keeps the source side; → income keeps the destination side
	s, err = catzTargetState(lk, transfer, catzTarget{Category: cat(11)}, true)

	if err != nil || s.Type != int(models.TRANSACTION_TYPE_EXPENSE) || s.SourceAccountId != "101" || s.DestinationAccountId != "" || s.DestinationAmount != 0 {
		t.Fatalf("transfer→expense: %+v %v", s, err)
	}

	s, err = catzTargetState(lk, transfer, catzTarget{Category: cat(31)}, true)

	if err != nil || s.Type != int(models.TRANSACTION_TYPE_INCOME) || s.SourceAccountId != "102" || s.SourceAmount != 500 || s.DestinationAccountId != "" {
		t.Fatalf("transfer→income: %+v %v", s, err)
	}

	// a counter account where none is needed is refused rather than ignored
	if _, err := catzTargetState(lk, expense, catzTarget{Category: cat(11), Counter: lk.accountMap[102]}, true); txnFailCode(t, err) != CodeInvalidInput {
		t.Fatalf("stray counter should be refused: %v", err)
	}

	// a balance modification takes no category
	bm := txnState{Id: "903", Type: int(models.TRANSACTION_TYPE_MODIFY_BALANCE), SourceAccountId: "101"}

	if _, err := catzTargetState(lk, bm, catzTarget{Category: cat(11)}, true); txnFailCode(t, err) != CodeInvalidInput {
		t.Fatalf("balance modification should be refused: %v", err)
	}
}

func TestCatzGroupQueue(t *testing.T) {
	rows := []catzQueueRow{
		{Id: "1", Date: "2025-01-03", Type: models.TRANSACTION_TYPE_EXPENSE, Amount: -450, Currency: "USD", Account: "Alpha Checking", Comment: "POS DEBIT NORTHBANK CAFE #0042"},
		{Id: "2", Date: "2025-01-01", Type: models.TRANSACTION_TYPE_EXPENSE, Amount: -500, Currency: "USD", Account: "Beta Card", Comment: "NORTHBANK CAFE 0099"},
		{Id: "3", Date: "2025-02-01", Type: models.TRANSACTION_TYPE_INCOME, Amount: 100, Currency: "USD", Account: "Alpha Checking", Comment: "Northbank Cafe refund"},
		{Id: "4", Date: "2025-01-05", Type: models.TRANSACTION_TYPE_EXPENSE, Amount: -9900, Currency: "USD", Account: "Alpha Checking", Comment: "MERIDIAN UTILITIES"},
		{Id: "5", Date: "2025-01-06", Type: models.TRANSACTION_TYPE_EXPENSE, Amount: -100, Currency: "USD", Account: "Alpha Checking", Comment: ""},
	}

	groups := catzGroupQueue(rows, 1)

	if len(groups) != 4 {
		t.Fatalf("want 4 groups, got %d", len(groups))
	}

	g := groups[0]

	if g.Payee != "NORTHBANK CAFE" || g.Count != 2 || g.FirstDate != "2025-01-01" || g.LastDate != "2025-01-03" {
		t.Fatalf("largest group first, keyed by normalised payee: %+v", g)
	}

	if len(g.Ids) != 1 || !g.IdsTruncated || len(g.AccountNames) != 2 || g.Totals[0]["amount"].(int64) != -950 {
		t.Fatalf("ids capped, accounts listed, totals signed per currency: %+v", g)
	}

	var sawEmpty bool

	for _, g := range groups {
		sawEmpty = sawEmpty || g.Payee == "(no description)"
	}

	if !sawEmpty {
		t.Fatalf("a row with no comment is its own labelled group")
	}
}

func TestCatzHistorySuggest(t *testing.T) {
	lk := catzTestLookup()
	h := newCatzHistory()
	h.add("NORTHBANK CAFE 0042", "11")
	h.add("NORTHBANK CAFE #7", "11")
	h.add("NORTHBANK CAFE", "21")
	h.add("MERIDIAN POWER CO", "12")
	h.add("SALARY ACME LLC", "31")

	s := h.suggest(lk, "NORTHBANK CAFE", nil)

	if s == nil || s.CategoryId != "11" || s.Votes != 2 || s.Of != 3 || s.Basis != "same_payee" || len(s.Alternatives) != 1 {
		t.Fatalf("majority wins with its evidence: %+v", s)
	}

	s = h.suggest(lk, "MERIDIAN POWER BILLPAY", nil)

	if s == nil || s.CategoryId != "12" || s.Basis != "payee_prefix" {
		t.Fatalf("the first two words are the looser match: %+v", s)
	}

	// only categories the group's rows could take without a type change are suggested
	if s := h.suggest(lk, "SALARY ACME LLC", map[models.TransactionCategoryType]bool{models.CATEGORY_TYPE_EXPENSE: true}); s != nil {
		t.Fatalf("an income category is not suggested for expense rows: %+v", s)
	}

	if s := h.suggest(lk, "UNSEEN PAYEE", nil); s != nil {
		t.Fatalf("no history, no suggestion: %+v", s)
	}
}

func TestCatzPlanEnsure(t *testing.T) {
	lk := catzTestLookup()

	items, parsed, err := catzPlanEnsure(lk.categories, []string{
		"Expense > Food > Groceries",
		"expense > food > groceries",
		"Expense > Food > Coffee",
		"Expense > Travel > Lodging",
		"Expense > Travel > Flights",
		"Income > Food > Groceries",
	})

	if err != nil || len(items) != 5 || len(parsed) != 5 {
		t.Fatalf("duplicates collapse: %v %v", items, err)
	}

	want := []string{"exists", "create_sub", "create_group_and_sub", "create_group_and_sub", "create_group_and_sub"}

	for i, w := range want {
		if items[i].Action != w {
			t.Fatalf("item %d %q: action %q, want %q", i, items[i].Path, items[i].Action, w)
		}
	}

	if items[0].CategoryId != "11" || items[1].GroupId != "10" {
		t.Fatalf("existing ids are reported: %+v", items[:2])
	}

	for _, bad := range []string{"Food > Groceries", "Groceries"} {
		if _, _, err := catzPlanEnsure(lk.categories, []string{bad}); txnFailCode(t, err) != CodeInvalidInput {
			t.Fatalf("%q lacks type and group: %v", bad, err)
		}
	}
}

func TestCatzRoutesRegistered(t *testing.T) {
	want := map[string]bool{"GET /transactions/uncategorized": false, "POST /transactions/categorize": false, "POST /categories/ensure": false, "GET /categories/tree": false, "POST /ingest/categorize": false}

	for _, r := range Routes() {
		k := r.Method + " " + r.Path

		if _, ok := want[k]; ok {
			want[k] = true
		}
	}

	for k, seen := range want {
		if !seen {
			t.Fatalf("route %s is not registered", k)
		}
	}
}

func TestCatzBuildTreeAndYAML(t *testing.T) {
	all := []*models.TransactionCategory{
		{CategoryId: 10, Name: "Food & Drink", Type: models.CATEGORY_TYPE_EXPENSE, DisplayOrder: 1},
		{CategoryId: 11, Name: "Groceries", ParentCategoryId: 10, Type: models.CATEGORY_TYPE_EXPENSE, DisplayOrder: 2},
		{CategoryId: 12, Name: "Coffee: Beans", ParentCategoryId: 10, Type: models.CATEGORY_TYPE_EXPENSE, DisplayOrder: 1},
		{CategoryId: 13, Name: "Old", ParentCategoryId: 10, Type: models.CATEGORY_TYPE_EXPENSE, DisplayOrder: 3, Hidden: true},
		{CategoryId: 20, Name: "Earnings", Type: models.CATEGORY_TYPE_INCOME, DisplayOrder: 1},
		{CategoryId: 21, Name: "Salary", ParentCategoryId: 20, Type: models.CATEGORY_TYPE_INCOME, DisplayOrder: 1},
	}

	groups, counts := catzBuildTree(all, false, 0)

	if counts.Groups != 2 || counts.Subcategories != 3 {
		t.Fatalf("counts: %+v", counts)
	}

	// income before expense (the type order), subs in display order, hidden left out
	if groups[0].Name != "Earnings" || groups[0].Type != "income" || groups[1].Subcategories[0].Name != "Coffee: Beans" || groups[1].Subcategories[1].Name != "Groceries" {
		t.Fatalf("order: %+v", groups)
	}

	if _, c := catzBuildTree(all, true, 0); c.Subcategories != 4 {
		t.Fatalf("include_hidden: %+v", c)
	}

	if g, c := catzBuildTree(all, false, models.CATEGORY_TYPE_EXPENSE); c.Groups != 1 || g[0].Name != "Food & Drink" {
		t.Fatalf("type filter: %+v", g)
	}

	doc, err := catzTreeYAML(groups, counts, "2026-01-02T03:04:05Z")

	if err != nil {
		t.Fatal(err)
	}

	for _, want := range []string{"app: ezbookkeeping\n", "generated_at: \"2026-01-02T03:04:05Z\"\n", "counts:\n  groups: 2\n  subcategories: 3\n", "groups:\n  - name: Earnings\n    type: income\n    id: \"20\"\n", "name: 'Coffee: Beans'", "subcategories:\n      - name: Salary\n"} {
		if !strings.Contains(doc, want) {
			t.Fatalf("yaml lacks %q:\n%s", want, doc)
		}
	}

	if !strings.HasPrefix(doc, "app:") {
		t.Fatalf("app is the first key:\n%s", doc)
	}
}

func TestIngCatzParseFile(t *testing.T) {
	tsv := "\xef\xbb\xbfTime\tTimezone\tType\tCategory\tSub Category\tAccount\tAmount\tDescription\tFITID\n" +
		"2026-01-01 11:59:59\t+00:00\tBalance Modification\t\t\tNorthbank Checking ••4021\t100.00\tOpening balance\t\n" +
		"2026-01-02 12:00:00\t+00:00\tExpense\tFood & Drink\tGroceries\tNorthbank Checking ••4021\t12.50\tMERIDIAN MARKET, INC\t4021-20260102-1250-0\n" +
		"2026-01-03 12:00:00\t+00:00\tIncome\t\t\tNorthbank Checking ••4021\t5.00\tREFUND\t4021-20260103-500-0\n" +
		"\n"

	rows, err := ingCatzParseFile([]byte(tsv), "Checking_x4021_ALL_ezbookkeeping.tsv")

	if err != nil {
		t.Fatal(err)
	}

	if len(rows) != 3 {
		t.Fatalf("rows: %d", len(rows))
	}

	if r := rows[1]; r.Line != 3 || r.Type != models.CATEGORY_TYPE_EXPENSE || r.Group != "Food & Drink" || r.Sub != "Groceries" || r.BankId != "4021-20260102-1250-0" || r.Desc != "MERIDIAN MARKET, INC" {
		t.Fatalf("expense row: %+v", r)
	}

	if rows[0].Type != 0 || rows[2].Sub != "" || rows[2].Type != models.CATEGORY_TYPE_INCOME {
		t.Fatalf("other rows: %+v %+v", rows[0], rows[2])
	}

	// the column order does not matter; a missing FITID column is refused, naming it
	if _, err := ingCatzParseFile([]byte("Type\tCategory\tSub Category\n"), "x.tsv"); txnFailCode(t, err) != CodeInvalidInput {
		t.Fatalf("missing FITID should be invalid_input, got %v", err)
	}

	// a CSV honours quoting, so a comma inside a description does not shift the columns
	csvRows, err := ingCatzParseFile([]byte("FITID,Type,Category,Sub Category,Description\nA1,Expense,Food & Drink,Groceries,\"MERIDIAN, INC\"\n"), "x.csv")

	if err != nil || len(csvRows) != 1 || csvRows[0].Sub != "Groceries" || csvRows[0].Desc != "MERIDIAN, INC" {
		t.Fatalf("csv: %+v %v", csvRows, err)
	}

	if got := ingCatzCompanion("import/personal/Northbank/Checking_x4021_ALL_ezbookkeeping.ofx"); got != "import/personal/Northbank/Checking_x4021_ALL_ezbookkeeping.tsv" {
		t.Fatalf("companion: %s", got)
	}
}

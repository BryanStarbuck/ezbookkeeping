package machine

import (
	"encoding/csv"
	"encoding/json"
	"strings"
	"testing"

	"github.com/mayswind/ezbookkeeping/pkg/core"
	"github.com/mayswind/ezbookkeeping/pkg/models"
	"github.com/mayswind/ezbookkeeping/pkg/services"
)

// export_rows_test.go — the id-carrying export (apis.mdx §10.3.1a). Synthetic data only.

func TestExportRowsFormatHundredths(t *testing.T) {
	cases := map[int64]string{0: "0.00", 5: "0.05", -5: "-0.05", 1234: "12.34", -123450: "-1234.50", 100: "1.00"}

	for in, want := range cases {
		if got := formatHundredths(in); got != want {
			t.Errorf("formatHundredths(%d) = %q, want %q", in, got, want)
		}
	}
}

func TestExportRowsRenderCarriesIdAndPath(t *testing.T) {
	lk := txnNewLookup(
		[]*models.Account{{AccountId: 11, Name: "Test Checking", Currency: "USD"}, {AccountId: 12, Name: "Test Savings", Currency: "USD"}},
		[]*models.TransactionCategory{
			{CategoryId: 1, Name: "Imported", Type: models.CATEGORY_TYPE_EXPENSE, ParentCategoryId: models.LevelOneTransactionCategoryParentId},
			{CategoryId: 2, Name: "Uncategorized", Type: models.CATEGORY_TYPE_EXPENSE, ParentCategoryId: 1},
			{CategoryId: 3, Name: "General Transfer", Type: models.CATEGORY_TYPE_TRANSFER, ParentCategoryId: models.LevelOneTransactionCategoryParentId},
			{CategoryId: 4, Name: "Bank Transfer", Type: models.CATEGORY_TYPE_TRANSFER, ParentCategoryId: 3},
		},
		nil,
	)
	rows := []map[string]any{
		{"id": "900001", "date": "2020-01-02", "type": int64(models.TRANSACTION_TYPE_EXPENSE), "categoryId": "2", "sourceAccountId": "11", "sourceAmount": int64(1250), "comment": "EXAMPLE STORE, CITY ST"},
		{"id": "900002", "date": "2020-01-03", "type": int64(models.TRANSACTION_TYPE_TRANSFER), "categoryId": "4", "sourceAccountId": "11", "destinationAccountId": "12", "sourceAmount": int64(5000), "destinationAmount": int64(5000), "comment": "move"},
	}

	data, err := exportRowsRender(rows, lk, ',')

	if err != nil {
		t.Fatal(err)
	}

	recs, err := csv.NewReader(strings.NewReader(string(data))).ReadAll()

	if err != nil {
		t.Fatal(err)
	}

	if len(recs) != 3 || recs[0][0] != "ID" {
		t.Fatalf("want header + 2 rows with ID first, got %v", recs)
	}

	if got := strings.Join(recs[1][:10], "|"); got != "900001|2020-01-02|expense|Expense > Imported > Uncategorized|Imported|Uncategorized|Test Checking|USD|-12.50|-1250" {
		t.Errorf("expense row = %s", got)
	}

	if recs[1][13] != "EXAMPLE STORE, CITY ST" {
		t.Errorf("description with a comma must survive quoting, got %q", recs[1][13])
	}

	if recs[2][10] != "Test Savings" || recs[2][11] != "50.00" || recs[2][3] != "Transfer > General Transfer > Bank Transfer" {
		t.Errorf("transfer row = %v", recs[2])
	}
}

// TestCatzTransferIntoHiddenCounterAndUndo: the round trip's write — an expense becoming a transfer
// into a HIDDEN counterpart account (upstream refuses to modify a transaction in a hidden account),
// and its undo — succeeds, and the account is hidden again afterwards (apis.mdx §10.3.1a).
func TestCatzTransferIntoHiddenCounterAndUndo(t *testing.T) {
	b := xferOpenBooks(t)
	b.addTransferCategory()

	row := b.row(models.TRANSACTION_TYPE_EXPENSE, b.checking, 3000, "2026-03-03", "Withdrawal Transfer To ******4321 Allowance")
	start := b.balances()

	var route RouteDef

	for _, r := range catzRoutes() {
		if r.Path == "/transactions/categorize" {
			route = r
		}
	}

	body := map[string]any{
		"assignments":       []map[string]any{{"ids": []string{idString(row)}, "category": "Transfer > Moves > Internal", "counter_account_name": "Market Value"}},
		"allow_type_change": true,
	}
	raw, _ := json.Marshal(body)
	res, err := catzHandleCategorize(b.ctx(route, string(raw)))

	if err != nil {
		t.Fatalf("preview: %v", err)
	}

	body["dry_run"] = false
	body["confirm_token"] = res.(*WriteResult).ConfirmToken
	raw, _ = json.Marshal(body)

	if _, err := catzHandleCategorize(b.ctx(route, string(raw))); err != nil {
		t.Fatalf("apply into a hidden counter account: %v", err)
	}

	s := b.state(row)

	if s == nil || s.Type != int(models.TRANSACTION_TYPE_TRANSFER) || s.DestinationAccountId != idString(b.market) {
		t.Fatalf("the row must be a transfer into the hidden account: %+v", s)
	}

	accts, _ := services.Accounts.GetAccountsByAccountIds(core.NewNullContext(), 42, []int64{b.market})

	if !accts[b.market].Hidden {
		t.Fatal("the counter account must be hidden again after the write")
	}

	if after := b.balances(); after[b.checking] != start[b.checking] {
		t.Fatalf("the checking balance must not move: %d → %d", start[b.checking], after[b.checking])
	}

	b.undo()

	if s := b.state(row); s == nil || s.Type != int(models.TRANSACTION_TYPE_EXPENSE) {
		t.Fatalf("undo restores the expense: %+v", s)
	}

	accts, _ = services.Accounts.GetAccountsByAccountIds(core.NewNullContext(), 42, []int64{b.market})

	if !accts[b.market].Hidden {
		t.Fatal("the counter account stays hidden after the undo")
	}
}

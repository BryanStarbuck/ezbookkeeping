package commands

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/BryanStarbuck/ezbookkeeping/cli/internal/app"
)

func orNum(s string) json.Number { return json.Number(s) }

func TestOrSpanAndRangeInTwoTimezones(t *testing.T) {
	la, _ := time.LoadLocation("America/Los_Angeles")
	tokyo, _ := time.LoadLocation("Asia/Tokyo")
	instant := time.Date(2026, 9, 1, 3, 0, 0, 0, time.UTC) // 2026-08-31 in LA, 2026-09-01 in Tokyo

	r, err := orResolveRange("this-month", "", instant.In(la))

	if err != nil || r.Start != "2026-08-01" || r.End != "2026-08-31" {
		t.Fatalf("LA this-month: %+v %v", r, err)
	}

	r, err = orResolveRange("this-month", "", instant.In(tokyo))

	if err != nil || r.Start != "2026-09-01" || r.End != "2026-09-30" {
		t.Fatalf("Tokyo this-month: %+v %v", r, err)
	}

	r, _ = orResolveRange("yesterday", "today", instant.In(la))

	if r.Start != "2026-08-30" || r.End != "2026-08-31" || len(r.Notes) != 2 {
		t.Fatalf("yesterday..today: %+v", r)
	}

	r, _ = orResolveRange("last-year", "", instant.In(tokyo))

	if r.Start != "2025-01-01" || r.End != "2025-12-31" {
		t.Fatalf("last-year: %+v", r)
	}

	r, _ = orResolveRange("2026-02", "2026-03", instant)

	if r.Start != "2026-02-01" || r.End != "2026-03-31" {
		t.Fatalf("months: %+v", r)
	}

	r, _ = orResolveRange("2026-01-15", "", instant)

	if r.Start != "2026-01-15" || r.End != "" || len(r.Notes) != 0 {
		t.Fatalf("a plain day is not relative and sets no end: %+v", r)
	}

	if _, err := orResolveRange("2026-09-10", "2026-09-01", instant); err == nil {
		t.Fatal("end before start must be refused")
	}

	if _, err := orResolveRange("2026-02-30", "", instant); err == nil {
		t.Fatal("an impossible date must be refused")
	}

	if _, err := orResolveRange("next-week", "", instant); err == nil {
		t.Fatal("an unknown word must be refused")
	}

	if _, _, _, _, err := orSpan("LAST_MONTH", instant); err != nil {
		t.Fatalf("underscores and case are accepted: %v", err)
	}
}

func TestOrResolveAsOfCapsAtToday(t *testing.T) {
	now := time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC)

	if d, note, _ := orResolveAsOf("this-month", now); d != "2026-09-21" || note == "" {
		t.Fatalf("this-month as-of must be today: %q", d)
	}

	if d, _, _ := orResolveAsOf("last-month", now); d != "2026-08-31" {
		t.Fatalf("last-month as-of: %q", d)
	}

	if d, note, _ := orResolveAsOf("2026-06-30", now); d != "2026-06-30" || note != "" {
		t.Fatalf("plain day: %q %q", d, note)
	}
}

func TestOrMatchName(t *testing.T) {
	cands := []orNamed{{ID: "1", Name: "Checking"}, {ID: "2", Name: "checking"}, {ID: "3", Name: "Card"}, {ID: "4", Name: "Savings"}, {ID: "5", Name: "SAVINGS"}}

	if m, _ := orMatchName("Checking", cands); m == nil || m.ID != "1" {
		t.Fatal("exact match first")
	}

	if m, _ := orMatchName("card", cands); m == nil || m.ID != "3" {
		t.Fatal("case-insensitive when no exact match")
	}

	if m, amb := orMatchName("savings", cands); m != nil || len(amb) != 2 {
		t.Fatalf("two case-insensitive matches are ambiguous: %v %v", m, amb)
	}

	if m, amb := orMatchName("Brokerage", cands); m != nil || len(amb) != 0 {
		t.Fatal("unknown")
	}
}

func TestOrFlattenAccounts(t *testing.T) {
	list := []map[string]any{
		{"id": "10", "name": "Card", "category": orNum("3"), "type": orNum("1"), "currency": "USD", "balance": orNum("-30000"), "isLiability": true},
		{"id": "20", "name": "Brokerage", "category": orNum("7"), "type": orNum("2"), "currency": "---", "balance": orNum("0"), "subAccounts": []any{
			map[string]any{"id": "21", "name": "Cash", "category": orNum("7"), "type": orNum("1"), "currency": "USD", "balance": orNum("100")},
			map[string]any{"id": "22", "name": "Euro", "category": orNum("7"), "type": orNum("1"), "currency": "EUR", "balance": orNum("200")},
		}},
	}

	rows := orFlattenAccounts(list)

	if len(rows) != 4 {
		t.Fatalf("parent + 2 subs + card = 4 rows, got %d", len(rows))
	}

	if rows[0]["side"] != "liability" || rows[0]["categoryName"] != "credit_card" || rows[0]["balance"] != orNum("-30000") {
		t.Fatalf("a liability keeps its sign: %v", rows[0])
	}

	if rows[1]["balance"] != nil || rows[1]["kind"] != "parent" {
		t.Fatalf("a parent account shows no balance of its own: %v", rows[1])
	}

	if rows[2]["depth"] != 1 || rows[2]["parentName"] != "Brokerage" || rows[3]["currency"] != "EUR" {
		t.Fatalf("sub-accounts indented under their parent: %v %v", rows[2], rows[3])
	}
}

func TestOrFlattenCategoriesNestedAndFlat(t *testing.T) {
	nested := []map[string]any{{"id": "1", "name": "Food", "type": orNum("2"), "parentId": "0", "subCategories": []any{
		map[string]any{"id": "2", "name": "Groceries", "type": orNum("2"), "parentId": "1"},
	}}}

	rows := orFlattenCategories(nested)

	if len(rows) != 2 || rows[1]["depth"] != 1 || rows[1]["parentName"] != "Food" || rows[0]["typeName"] != "expense" {
		t.Fatalf("nested: %v", rows)
	}

	flat := []map[string]any{
		{"id": "2", "name": "Groceries", "type": orNum("2"), "parentId": "1"},
		{"id": "1", "name": "Food", "type": orNum("2"), "parentId": "0"},
		{"id": "9", "name": "Salary", "type": orNum("1"), "parentId": "0"},
	}

	rows = orFlattenCategories(flat)

	if len(rows) != 3 || rows[0]["name"] != "Food" || rows[1]["name"] != "Groceries" || rows[2]["name"] != "Salary" {
		t.Fatalf("flat input grouped under its primary: %v", rows)
	}
}

func TestOrTxnDisplayTransferIsOneRow(t *testing.T) {
	transfer := map[string]any{
		"id": "3401855937219633152", "type": orNum("4"), "time": orNum("1758240000"), "utcOffset": orNum("-420"),
		"sourceAccount": map[string]any{"name": "Checking", "currency": "USD"}, "sourceAmount": orNum("50000"),
		"destinationAccount": map[string]any{"name": "Card", "currency": "USD"}, "destinationAmount": orNum("50000"),
		"category": map[string]any{"name": "Card payment"}, "comment": "payment",
	}

	row := orTxnDisplay(transfer, time.UTC)

	if row["id"] != "3401855937219633152" {
		t.Fatalf("ids stay strings, untouched: %v", row["id"])
	}

	if row["account"] != "Checking → Card" {
		t.Fatalf("a transfer names both sides: %v", row["account"])
	}

	if row["amountText"] != "-$500.00 USD → +$500.00 USD" {
		t.Fatalf("a transfer shows both sides in their currencies: %q", row["amountText"])
	}

	if row["date"] != "2025-09-18" {
		t.Fatalf("the date is the transaction's own offset: %v", row["date"])
	}

	expense := map[string]any{"id": "7", "typeName": "expense", "type": orNum("3"), "date": "2026-09-01", "sourceAmount": orNum("1250"), "sourceCurrency": "EUR", "categoryName": "Food > Groceries"}
	row = orTxnDisplay(expense, time.UTC)

	if row["amountText"] != "-€12.50 EUR" || row["category"] != "Food > Groceries" || row["signedAmount"] != int64(-1250) {
		t.Fatalf("expense: %v", row)
	}

	hidden := map[string]any{"id": "8", "type": orNum("2"), "sourceAmount": orNum("100"), "hideAmount": true}

	if row = orTxnDisplay(hidden, time.UTC); row["amountText"] != "***" {
		t.Fatalf("a hidden amount stays hidden: %v", row["amountText"])
	}

	cross := map[string]any{"id": "9", "type": orNum("4"), "sourceAmount": orNum("10000"), "sourceCurrency": "USD", "destinationAmount": orNum("1500000"), "destinationCurrency": "JPY"}

	if row = orTxnDisplay(cross, time.UTC); row["amountText"] != "-$100.00 USD → +¥15,000.00 JPY" {
		t.Fatalf("cross-currency transfer: %q", row["amountText"])
	}
}

func TestOrCurrencySubtotalsShapes(t *testing.T) {
	arr := map[string]any{"subtotals": []any{map[string]any{"currency": "USD", "amount": orNum("100")}, map[string]any{"currency": "EUR", "balance": orNum("-5")}}}

	if s := orCurrencySubtotals(arr); len(s) != 2 || s[1]["amount"] != orNum("-5") {
		t.Fatalf("array shape: %v", s)
	}

	m := map[string]any{"totalsByCurrency": map[string]any{"USD": orNum("100"), "EUR": map[string]any{"amount": orNum("7")}}}

	if s := orCurrencySubtotals(m); len(s) != 2 || s[0]["currency"] != "EUR" {
		t.Fatalf("map shape, sorted: %v", s)
	}

	if s := orCurrencySubtotals(map[string]any{"accounts": []any{}}); s != nil {
		t.Fatal("no server subtotals means none — the CLI never sums")
	}
}

func TestOrSplitIDsNames(t *testing.T) {
	q := url.Values{}
	orSplitIDsNames(q, "account_ids", "account_names", []string{"123", "Northbank Checking", "456"})

	if q.Get("account_ids") != "123,456" || q.Get("account_names") != "Northbank Checking" {
		t.Fatalf("%v", q)
	}
}

func TestOrFindRows(t *testing.T) {
	data := map[string]any{"filter": map[string]any{}, "transactions": []any{map[string]any{"id": "1"}}}

	if rows := orFindRows(data, "transactions"); len(rows) != 1 {
		t.Fatal("named key")
	}

	if rows := orFindRows(map[string]any{"tagGroups": []any{map[string]any{"id": "1"}}}, "groups"); len(rows) != 1 {
		t.Fatal("the only array of objects is found when the key differs")
	}
}

// TestOrVerbFlagsAreDeclared walks every verb this family registers: the parser accepts each declared
// flag, and help lists it (cli.mdx §7.3, AC 16)
func TestOrVerbFlagsAreDeclared(t *testing.T) {
	mine := []string{"status", "doctor", "up", "stop", "logs", "key init", "key show", "key rotate", "whoami",
		"accounts list", "accounts show", "accounts balance", "accounts reconciliation",
		"transactions list", "transactions show", "transactions count", "transactions export",
		"categories list", "tags list", "tag-groups list", "templates list", "schedules upcoming",
		"rates list", "rates convert", "insights list", "data stats", "data export", "raw",
		"transactions delete", "accounts delete", "categories delete", "tags delete", "tag-groups delete",
		"templates delete", "insights delete", "admin clear-data", "admin sessions list", "admin sessions revoke", "admin pictures prune"}

	for _, name := range mine {
		v, n := app.Lookup(strings.Fields(name))

		if v == nil || n != len(strings.Fields(name)) {
			t.Errorf("verb %q is not registered", name)
			continue
		}

		var help strings.Builder
		app.PrintVerbHelp(&help, v)

		for _, f := range v.Flags {
			if !strings.Contains(help.String(), "--"+f.Name) {
				t.Errorf("%s: --%s missing from help", name, f.Name)
			}

			argv := append(strings.Fields(name), make([]string, v.MinArgs)...)

			for i := range v.MinArgs {
				argv[len(strings.Fields(name))+i] = "1"
			}

			argv = append(argv, "--"+f.Name)

			if f.Value != "" {
				argv = append(argv, "x")
			}

			if _, err := app.Parse(argv); err != nil {
				t.Errorf("%s: --%s does not parse: %v", name, f.Name, err)
			}
		}
	}
}

func TestOrDecimalAmountRefused(t *testing.T) {
	orTestEnv(t)

	_, errOut, code := orRun(t, "transactions", "list", "--min", "12.50", "--api", "http://127.0.0.1:1")

	if code != 2 || !strings.Contains(errOut, "1250") {
		t.Fatalf("--min 12.50 must be exit 2 suggesting 1250: %d %q", code, errOut)
	}

	_, errOut, code = orRun(t, "rates", "convert", "500.00", "EUR", "USD")

	if code != 2 || !strings.Contains(errOut, "50000") {
		t.Fatalf("rates convert 500.00: %d %q", code, errOut)
	}
}

func TestOrAccountsListTableAndJSON(t *testing.T) {
	orTestEnv(t)
	t.Setenv("EZBK_API_KEY", orTestKey)

	var gotQuery url.Values
	srv := orTestPlane(t, map[string]func(http.ResponseWriter, *http.Request){
		"GET /accounts": func(w http.ResponseWriter, r *http.Request) {
			gotQuery = r.URL.Query()
			orWriteJSON(w, 200, map[string]any{"ok": true, "meta": map[string]any{"truncated": false}, "data": map[string]any{
				"accounts": []any{
					map[string]any{"id": "10", "name": "Northbank Checking", "category": 2, "type": 1, "currency": "USD", "balance": 421108, "isAsset": true, "hidden": false},
					map[string]any{"id": "11", "name": "Meridian Card", "category": 3, "type": 1, "currency": "USD", "balance": -30000, "isLiability": true, "hidden": false},
				},
				"subtotals": []any{map[string]any{"currency": "USD", "amount": 391108}},
			}})
		},
	})

	out, errOut, code := orRun(t, "accounts", "list", "--api", srv.URL, "--format", "table", "--include-hidden", "--currency", "usd")

	if code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}

	if gotQuery.Get("include_hidden") != "true" || gotQuery.Get("currency") != "USD" {
		t.Fatalf("query: %v", gotQuery)
	}

	for _, want := range []string{"Northbank Checking", "$4,211.08", "-$300.00", "liability", "SUBTOTAL (server)", "$3,911.08"} {
		if !strings.Contains(out, want) {
			t.Errorf("table lacks %q:\n%s", want, out)
		}
	}

	out, _, code = orRun(t, "accounts", "list", "--api", srv.URL)

	var env map[string]any

	if code != 0 || json.Unmarshal([]byte(out), &env) != nil || env["ok"] != true {
		t.Fatalf("json default must be the envelope, parseable: %q", out)
	}

	out, _, _ = orRun(t, "accounts", "list", "--api", srv.URL, "--format", "csv")

	if !strings.HasPrefix(out, "id,name,parent,category,side,balance_hundredths,balance,balance_currency,hidden\n") || !strings.Contains(out, "421108,4211.08,USD") {
		t.Fatalf("csv carries hundredths, decimal and currency:\n%s", out)
	}
}

func TestOrTransactionsListSendsNamesAndResolvedDates(t *testing.T) {
	orTestEnv(t)
	t.Setenv("EZBK_API_KEY", orTestKey)

	var gotQuery url.Values
	srv := orTestPlane(t, map[string]func(http.ResponseWriter, *http.Request){
		"GET /transactions": func(w http.ResponseWriter, r *http.Request) {
			gotQuery = r.URL.Query()
			orWriteJSON(w, 200, map[string]any{"ok": true, "data": map[string]any{"transactions": []any{}, "nextCursor": "1758240000123"}})
		},
	})

	_, errOut, code := orRun(t, "transactions", "list", "--api", srv.URL, "--account", "Northbank Checking", "--account", "42",
		"--start", "2026-08", "--tag", "trip", "--min", "5000", "--currency", "eur", "--type", "expense", "--limit", "10")

	if code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}

	want := map[string]string{"account_names": "Northbank Checking", "account_ids": "42", "start": "2026-08-01", "end": "2026-08-31",
		"tag_names": "trip", "min_amount": "5000", "currency": "EUR", "type": "expense", "limit": "10"}

	for k, v := range want {
		if gotQuery.Get(k) != v {
			t.Errorf("%s = %q, want %q (all: %v)", k, gotQuery.Get(k), v, gotQuery)
		}
	}

	if !strings.Contains(errOut, "--cursor 1758240000123") || !strings.Contains(errOut, "2026-08-01") {
		t.Fatalf("nextCursor and the resolved dates go to stderr: %q", errOut)
	}
}

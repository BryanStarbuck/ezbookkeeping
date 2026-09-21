package machine

import (
	"math/big"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/mayswind/ezbookkeeping/pkg/models"
)

// analytics_test.go — pure-logic tests of the analytics plane on synthetic books. Every entity
// here is invented; nothing touches the home directory or a database.

// ---------------------------------------------------------------------------------------------
// synthetic books (an prefix: several agents write tests in this package)

const (
	anTCheck   int64 = 101 // USD checking
	anTSave    int64 = 102 // USD savings
	anTCard    int64 = 103 // USD credit card
	anTEuro    int64 = 104 // EUR checking
	anTHidden  int64 = 105 // USD cash, hidden
	anTParent  int64 = 110 // multi-currency parent
	anTSubUSD  int64 = 111
	anTSubJPY  int64 = 112
	anTFood    int64 = 201 // expense primary
	anTGrocery int64 = 202 // expense secondary of food
	anTDining  int64 = 203 // expense secondary of food
	anTHome    int64 = 210 // expense primary
	anTRepair  int64 = 211 // expense secondary of home
	anTOrphan  int64 = 299 // secondary whose parent does not exist
	anTWork    int64 = 301 // income primary
	anTSalary  int64 = 302 // income secondary
	anTBonus   int64 = 303 // income secondary
	anTXfer    int64 = 401 // transfer primary
	anTXferSub int64 = 402 // transfer secondary
)

func anTestBooks() (*anAccounts, *anCategories) {
	acc := &anAccounts{byId: map[int64]*anAccount{}}
	addAcc := func(a *anAccount) {
		a.IsAsset, a.IsLiability = a.Category.IsAsset(), a.Category.IsLiability()

		if a.Type == 0 {
			a.Type = models.ACCOUNT_TYPE_SINGLE_ACCOUNT
		}

		acc.byId[a.Id] = a
		acc.order = append(acc.order, a.Id)

		if a.ParentId > 0 {
			p := acc.byId[a.ParentId]
			p.Children = append(p.Children, a.Id)
			a.Hidden = a.OwnHidden || p.Hidden
		} else {
			a.Hidden = a.OwnHidden
		}
	}

	addAcc(&anAccount{Id: anTCheck, Name: "Northbank Checking", Category: models.ACCOUNT_CATEGORY_CHECKING_ACCOUNT, Currency: "USD"})
	addAcc(&anAccount{Id: anTSave, Name: "Northbank Savings", Category: models.ACCOUNT_CATEGORY_SAVINGS_ACCOUNT, Currency: "USD"})
	addAcc(&anAccount{Id: anTCard, Name: "Meridian Card", Category: models.ACCOUNT_CATEGORY_CREDIT_CARD, Currency: "USD"})
	addAcc(&anAccount{Id: anTEuro, Name: "Euro Checking", Category: models.ACCOUNT_CATEGORY_CHECKING_ACCOUNT, Currency: "EUR"})
	addAcc(&anAccount{Id: anTHidden, Name: "Old Cash", Category: models.ACCOUNT_CATEGORY_CASH, Currency: "USD", OwnHidden: true})
	addAcc(&anAccount{Id: anTParent, Name: "Travel Wallet", Category: models.ACCOUNT_CATEGORY_CASH, Currency: "---", Type: models.ACCOUNT_TYPE_MULTI_SUB_ACCOUNTS})
	addAcc(&anAccount{Id: anTSubUSD, Name: "Wallet USD", ParentId: anTParent, Category: models.ACCOUNT_CATEGORY_CASH, Currency: "USD"})
	addAcc(&anAccount{Id: anTSubJPY, Name: "Wallet JPY", ParentId: anTParent, Category: models.ACCOUNT_CATEGORY_CASH, Currency: "JPY"})

	cats := &anCategories{byId: map[int64]*anCategory{}}
	addCat := func(c *anCategory) {
		cats.byId[c.Id] = c
		cats.order = append(cats.order, c.Id)

		if p := cats.byId[c.ParentId]; p != nil {
			p.Children = append(p.Children, c.Id)
		}
	}

	addCat(&anCategory{Id: anTFood, Name: "Food", Type: models.CATEGORY_TYPE_EXPENSE})
	addCat(&anCategory{Id: anTGrocery, Name: "Groceries", ParentId: anTFood, Type: models.CATEGORY_TYPE_EXPENSE})
	addCat(&anCategory{Id: anTDining, Name: "Dining", ParentId: anTFood, Type: models.CATEGORY_TYPE_EXPENSE})
	addCat(&anCategory{Id: anTHome, Name: "Home", Type: models.CATEGORY_TYPE_EXPENSE})
	addCat(&anCategory{Id: anTRepair, Name: "Home Repair", ParentId: anTHome, Type: models.CATEGORY_TYPE_EXPENSE})
	addCat(&anCategory{Id: anTOrphan, Name: "Orphan", ParentId: 9999, Type: models.CATEGORY_TYPE_EXPENSE})
	addCat(&anCategory{Id: anTWork, Name: "Work", Type: models.CATEGORY_TYPE_INCOME})
	addCat(&anCategory{Id: anTSalary, Name: "Salary", ParentId: anTWork, Type: models.CATEGORY_TYPE_INCOME})
	addCat(&anCategory{Id: anTBonus, Name: "Bonus", ParentId: anTWork, Type: models.CATEGORY_TYPE_INCOME})
	addCat(&anCategory{Id: anTXfer, Name: "Transfers", Type: models.CATEGORY_TYPE_TRANSFER})
	addCat(&anCategory{Id: anTXferSub, Name: "Between accounts", ParentId: anTXfer, Type: models.CATEGORY_TYPE_TRANSFER})

	return acc, cats
}

func anTestSelection(t *testing.T, args *anArgs) *anSelection {
	t.Helper()
	acc, cats := anTestBooks()

	if args == nil {
		args = &anArgs{}
	}

	sel, err := anBuildSelection(acc, cats, args, false)

	if err != nil {
		t.Fatalf("selection: %v", err)
	}

	return sel
}

// anTestFacts is one month of synthetic statistics rows (category × account totals, magnitudes)
func anTestFacts(bucket string) []anFact {
	f := func(cat, acc, rel int64, relType models.TransactionRelatedAccountType, amount int64) anFact {
		return anFact{Bucket: bucket, anStatItem: anStatItem{CategoryId: cat, AccountId: acc, RelatedAccountId: rel, RelatedAccountType: relType, Amount: amount}}
	}

	return []anFact{
		f(anTGrocery, anTCheck, 0, 0, 12050),
		f(anTGrocery, anTCard, 0, 0, 4999),
		f(anTDining, anTCard, 0, 0, 3100),
		f(anTRepair, anTCheck, 0, 0, 25000),
		f(anTRepair, anTHidden, 0, 0, 700),
		f(anTOrphan, anTCheck, 0, 0, 999),     // parent missing: skipped, like statistics.ts
		f(anTGrocery, 777, 0, 0, 111),         // unknown account: skipped
		f(anTGrocery, anTEuro, 0, 0, 5000),    // EUR
		f(anTDining, anTSubJPY, 0, 0, 350000), // JPY (fixed two-decimal scale: ¥3,500.00)
		f(anTSalary, anTCheck, 0, 0, 500000),
		f(anTBonus, anTSave, 0, 0, 20000),
		f(anTSalary, anTEuro, 0, 0, 100000),
		// a transfer checking -> savings, both rows as upstream returns them
		f(anTXferSub, anTCheck, anTSave, models.TRANSACTION_RELATED_ACCOUNT_TYPE_TRANSFER_TO, 30000),
		f(anTXferSub, anTSave, anTCheck, models.TRANSACTION_RELATED_ACCOUNT_TYPE_TRANSFER_FROM, 30000),
	}
}

// ---------------------------------------------------------------------------------------------
// the parity test: our grouping against a literal port of statistics.ts getCategoryTotalAmountItems

// anRefChart names the statistics.ts chart data types the port supports
type anRefChart int

const (
	anRefExpenseByPrimary anRefChart = iota
	anRefExpenseBySecondary
	anRefExpenseByAccount
	anRefIncomeByPrimary
	anRefIncomeBySecondary
	anRefIncomeByAccount
)

// anReferenceGrouping ports src/stores/statistics.ts assembleAccountAndCategoryInfo +
// getCategoryTotalAmountItems for a single-currency book (amountInDefaultCurrency == amount).
// filterAccountIds / filterCategoryIds are statistics.ts's EXCLUSION maps.
func anReferenceGrouping(items []anStatItem, acc *anAccounts, cats *anCategories, chart anRefChart, filterAccountIds, filterCategoryIds map[int64]bool) map[int64]int64 {
	out := map[int64]int64{}

	for _, item := range items {
		account := acc.byId[item.AccountId]
		var primaryAccount *anAccount

		if account != nil && account.ParentId != 0 {
			primaryAccount = acc.byId[account.ParentId]
		} else {
			primaryAccount = account
		}

		category := cats.byId[item.CategoryId]
		var primaryCategory *anCategory

		if category != nil && category.ParentId != 0 {
			primaryCategory = cats.byId[category.ParentId]
		} else {
			primaryCategory = category
		}

		if primaryAccount == nil || account == nil || primaryCategory == nil || category == nil {
			continue
		}

		switch chart {
		case anRefExpenseByPrimary, anRefExpenseBySecondary, anRefExpenseByAccount:
			if category.Type != models.CATEGORY_TYPE_EXPENSE {
				continue
			}
		default:
			if category.Type != models.CATEGORY_TYPE_INCOME {
				continue
			}
		}

		if filterAccountIds[account.Id] || filterCategoryIds[category.Id] {
			continue
		}

		switch chart {
		case anRefExpenseByAccount, anRefIncomeByAccount:
			out[account.Id] += item.Amount
		case anRefExpenseByPrimary, anRefIncomeByPrimary:
			out[primaryCategory.Id] += item.Amount
		default:
			out[category.Id] += item.Amount
		}
	}

	return out
}

func TestAnalyticsGroupingParityWithStatisticsTs(t *testing.T) {
	acc, cats := anTestBooks()

	// a single-currency book: statistics.ts converts everything into the default currency, so a
	// literal comparison needs one currency
	var items []anStatItem

	for _, f := range anTestFacts("2026-03") {
		if a := acc.byId[f.AccountId]; a == nil || a.Currency == "USD" {
			items = append(items, f.anStatItem)
		}
	}

	facts := make([]anFact, len(items))

	for i, it := range items {
		facts[i] = anFact{Bucket: "2026-03", anStatItem: it}
	}

	cases := []struct {
		name      string
		chart     anRefChart
		groupBy   string
		kind      anKind
		exclAccts map[int64]bool
		exclCats  map[int64]bool
	}{
		{"expense by primary", anRefExpenseByPrimary, anGroupPrimary, anKindExpense, nil, nil},
		{"expense by secondary", anRefExpenseBySecondary, anGroupSecondary, anKindExpense, nil, nil},
		{"expense by account", anRefExpenseByAccount, anGroupAccount, anKindExpense, nil, nil},
		{"income by primary", anRefIncomeByPrimary, anGroupPrimary, anKindIncome, nil, nil},
		{"income by secondary", anRefIncomeBySecondary, anGroupSecondary, anKindIncome, nil, nil},
		{"income by account", anRefIncomeByAccount, anGroupAccount, anKindIncome, nil, nil},
		{"expense by primary, card excluded", anRefExpenseByPrimary, anGroupPrimary, anKindExpense, map[int64]bool{anTCard: true}, nil},
		{"expense by secondary, dining excluded", anRefExpenseBySecondary, anGroupSecondary, anKindExpense, nil, map[int64]bool{anTDining: true}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// statistics.ts never hides hidden accounts from this computation; include them to compare
			sel, err := anBuildSelection(acc, cats, &anArgs{IncludeHiddenAccounts: true}, false)

			if err != nil {
				t.Fatal(err)
			}

			for id := range tc.exclAccts {
				delete(sel.AccountSet, id)
			}

			if tc.exclCats != nil {
				sel.ExcludeCategories = tc.exclCats
			}

			want := anReferenceGrouping(items, acc, cats, tc.chart, tc.exclAccts, tc.exclCats)
			series, _, err := anGroupFacts(facts, sel, anGroupOpts{GroupBy: tc.groupBy, Kinds: map[anKind]bool{tc.kind: true}})

			if err != nil {
				t.Fatal(err)
			}

			got := map[int64]int64{}

			for _, s := range series {
				if s.Currency != "USD" {
					t.Fatalf("unexpected currency %s", s.Currency)
				}

				got[anParseIdOrZero(s.Key)] = anAbs(s.Total)

				if tc.kind == anKindExpense && s.Total > 0 {
					t.Fatalf("expense series %s is positive: %d", s.Key, s.Total)
				}
			}

			if len(got) != len(want) {
				t.Fatalf("groups: got %v want %v", got, want)
			}

			for k, v := range want {
				if got[k] != v {
					t.Fatalf("group %d: got %d want %d (all got %v want %v)", k, got[k], v, got, want)
				}
			}
		})
	}
}

func TestAnalyticsGroupingHandComputed(t *testing.T) {
	sel := anTestSelection(t, nil) // hidden excluded by default
	series, st, err := anGroupFacts(anTestFacts("2026-03"), sel, anGroupOpts{GroupBy: anGroupPrimary, Kinds: map[anKind]bool{anKindExpense: true}})

	if err != nil {
		t.Fatal(err)
	}

	byKey := map[string]int64{}

	for _, s := range series {
		byKey[s.Key+"/"+s.Currency] = s.Total
	}

	// Food USD = groceries 120.50 + 49.99 + dining 31.00; Home USD = 250.00 (hidden 7.00 excluded)
	want := map[string]int64{
		"201/USD": -(12050 + 4999 + 3100),
		"210/USD": -25000,
		"201/EUR": -5000,
		"201/JPY": -350000,
	}

	for k, v := range want {
		if byKey[k] != v {
			t.Fatalf("%s: got %d want %d (all %v)", k, byKey[k], v, byKey)
		}
	}

	if len(byKey) != len(want) {
		t.Fatalf("unexpected groups %v", byKey)
	}

	if st.RowsUnknownEntities != 2 {
		t.Fatalf("unknown-entity rows: got %d want 2 (orphan category, unknown account)", st.RowsUnknownEntities)
	}

	if st.RowsOutsideFilters != 1 {
		t.Fatalf("rows outside filters: got %d want 1 (hidden account)", st.RowsOutsideFilters)
	}
}

func TestAnalyticsTransfersExcludedUnlessAsked(t *testing.T) {
	sel := anTestSelection(t, nil)
	facts := anTestFacts("r")

	series, _, err := anGroupFacts(facts, sel, anGroupOpts{GroupBy: anGroupKind, Kinds: map[anKind]bool{anKindExpense: true, anKindIncome: true}})

	if err != nil {
		t.Fatal(err)
	}

	for _, s := range series {
		if s.Kind == string(anKindTransferIn) || s.Kind == string(anKindTransferOut) {
			t.Fatalf("transfer series present without include_transfers: %+v", s)
		}
	}

	series, _, err = anGroupFacts(facts, sel, anGroupOpts{GroupBy: anGroupKind, Kinds: map[anKind]bool{anKindTransferIn: true, anKindTransferOut: true}})

	if err != nil {
		t.Fatal(err)
	}

	got := map[string]int64{}

	for _, s := range series {
		got[s.Key] = s.Total
	}

	if got["transfer_out"] != -30000 || got["transfer_in"] != 30000 {
		t.Fatalf("transfers: got %v", got)
	}
}

// ---------------------------------------------------------------------------------------------
// currencies (R11)

func anTestConverter(t *testing.T, target string) *anConverter {
	t.Helper()
	resp := &models.LatestExchangeRateResponse{
		DataSource:   "euro_central_bank",
		UpdateTime:   1789000000,
		BaseCurrency: "EUR",
		ExchangeRates: models.LatestExchangeRateSlice{
			{Currency: "USD", Rate: "1.25"},
			{Currency: "GBP", Rate: "0.8"},
		},
	}

	cv, err := anNewConverter(target, resp, nil)

	if err != nil {
		t.Fatal(err)
	}

	return cv
}

func TestAnalyticsPerCurrencyWithoutConversion(t *testing.T) {
	sel := anTestSelection(t, nil)
	series, _, err := anGroupFacts(anTestFacts("r"), sel, anGroupOpts{GroupBy: anGroupTotal, Kinds: map[anKind]bool{anKindExpense: true}})

	if err != nil {
		t.Fatal(err)
	}

	totals, err := anNativeTotals(series)

	if err != nil {
		t.Fatal(err)
	}

	if len(totals) != 3 || totals["USD"] != -(12050+4999+3100+25000) || totals["EUR"] != -5000 || totals["JPY"] != -350000 {
		t.Fatalf("per-currency totals wrong: %v", totals)
	}
}

func TestAnalyticsConvertOnceAndNameRates(t *testing.T) {
	cv := anTestConverter(t, "USD")

	// two EUR rows summed first (0.01 + 0.01 = 0.02 EUR), converted once: 0.02 × 1.25 = 0.025 → 0.03
	s := newAnSeries("x", "X", "EUR")
	_ = s.add("2026-01", 1)
	_ = s.add("2026-01", 1)
	u := newAnSeries("x", "X", "USD")
	_ = u.add("2026-01", 100)
	j := newAnSeries("x", "X", "JPY") // no JPY rate
	_ = j.add("2026-01", 500000)

	conv, unconverted, err := anConvertSeries([]*anSeries{s, u, j}, cv)

	if err != nil {
		t.Fatal(err)
	}

	if len(conv) != 1 || conv[0].Currency != "USD" || conv[0].points["2026-01"] != 103 || conv[0].Total != 103 {
		t.Fatalf("converted: %+v", conv[0])
	}

	if len(conv[0].ByCurrency) != 2 {
		t.Fatalf("byCurrency should echo the source totals: %+v", conv[0].ByCurrency)
	}

	if len(unconverted) != 1 || unconverted[0].Currency != "JPY" || unconverted[0].Total != 500000 {
		t.Fatalf("unconverted: %+v", unconverted)
	}

	if !cv.Partial() {
		t.Fatal("a currency without a rate must make the result partial")
	}

	p := cv.Provenance()

	if p["rateBasis"] != "latest" || p["note"] == "" {
		t.Fatalf("provenance must state the latest-rate basis: %v", p)
	}

	rates := p["rates"].([]anRateInfo)

	if len(rates) != 2 {
		t.Fatalf("rates used: %+v", rates)
	}

	for _, r := range rates {
		if r.Source != "provider" || r.Provider != "euro_central_bank" || r.UpdateTime == "" || r.BaseCurrency != "EUR" {
			t.Fatalf("rate not fully named: %+v", r)
		}
	}

	if missing := p["missingCurrencies"].([]string); len(missing) != 1 || missing[0] != "JPY" {
		t.Fatalf("missing currencies: %v", missing)
	}
}

func TestAnalyticsConvertTotals(t *testing.T) {
	cv := anTestConverter(t, "GBP")
	sum, sources, unconverted, err := anConvertTotals(map[string]int64{"EUR": 10000, "USD": -12500, "JPY": 7}, cv)

	if err != nil {
		t.Fatal(err)
	}

	// EUR 100.00 → GBP 80.00; USD −125.00 → EUR −100.00 → GBP −80.00
	if sum != 0 || len(sources) != 2 || len(unconverted) != 1 || unconverted[0].Currency != "JPY" {
		t.Fatalf("sum %d sources %v unconverted %v", sum, sources, unconverted)
	}
}

func TestAnalyticsUserCustomRatesAreMarkedCustom(t *testing.T) {
	resp := &models.LatestExchangeRateResponse{
		DataSource:    anUserCustomDataSource,
		UpdateTime:    1789000000,
		BaseCurrency:  "USD",
		ExchangeRates: models.LatestExchangeRateSlice{{Currency: "USD", Rate: "1"}, {Currency: "EUR", Rate: "0.9"}},
	}

	cv, _ := anNewConverter("USD", resp, map[string]int64{"EUR": 1788000000})

	if _, ok := cv.Convert("EUR", 900); !ok {
		t.Fatal("EUR should convert")
	}

	for _, r := range cv.Provenance()["rates"].([]anRateInfo) {
		if r.Source != "custom" {
			t.Fatalf("user_custom rates must be marked custom: %+v", r)
		}

		if r.Currency == "EUR" && r.UpdateUnixTime != 1788000000 {
			t.Fatalf("custom rate time must be the custom row's: %+v", r)
		}
	}
}

// ---------------------------------------------------------------------------------------------
// ranking, top-N and other (one place)

func TestAnalyticsRankTopNAndOther(t *testing.T) {
	mk := func(key, cur string, total int64) *anSeries {
		s := newAnSeries(key, strings.ToUpper(key), cur)
		_ = s.add("p", total)
		return s
	}

	in := []*anSeries{
		mk("a", "USD", -100), mk("b", "USD", -500), mk("c", "USD", -300), mk("d", "USD", -50),
		mk("e", "EUR", -10), mk("f", "EUR", -20),
	}

	out, err := anRankTopN(in, 2)

	if err != nil {
		t.Fatal(err)
	}

	var keys []string

	for _, s := range out {
		keys = append(keys, s.Currency+":"+s.Key)
	}

	want := "EUR:f,EUR:e,USD:b,USD:c,USD:other"

	if strings.Join(keys, ",") != want {
		t.Fatalf("order: got %s want %s", strings.Join(keys, ","), want)
	}

	other := out[len(out)-1]

	if other.Total != -150 || other.points["p"] != -150 || strings.Join(other.Members, ",") != "a,d" {
		t.Fatalf("other bucket: %+v", other)
	}

	// currencies are never ranked against each other, and nothing crosses into another currency
	for _, s := range out {
		if s.Key == anOtherKey && s.Currency != "USD" {
			t.Fatal("EUR had only two groups; no other bucket expected")
		}
	}
}

func TestAnalyticsAbsentPeriodsAreOmitted(t *testing.T) {
	s := newAnSeries("k", "K", "USD")
	_ = s.add("2026-01", -100)
	_ = s.add("2026-03", -300)
	s.finalize([]string{"2026-01", "2026-02", "2026-03"})

	if len(s.Points) != 2 || s.Points[0].Period != "2026-01" || s.Points[1].Period != "2026-03" {
		t.Fatalf("absent period must be omitted, not zero: %+v", s.Points)
	}
}

func TestAnalyticsIncludeZeroIsACountedZero(t *testing.T) {
	sel := anTestSelection(t, nil)
	series, _, _ := anGroupFacts(anTestFacts("r"), sel, anGroupOpts{GroupBy: anGroupSecondary, Kinds: map[anKind]bool{anKindIncome: true}})
	out := anIncludeZeroCategories(series, sel, anGroupSecondary, map[models.TransactionCategoryType]bool{models.CATEGORY_TYPE_INCOME: true}, "USD")

	for _, s := range out {
		if s.Count != nil && *s.Count == 0 && s.Total != 0 {
			t.Fatalf("counted zero with an amount: %+v", s)
		}
	}

	// every income secondary exists in USD now (salary, bonus present; nothing absent) and EUR
	// gains a counted-zero bonus
	found := false

	for _, s := range out {
		if s.Key == idString(anTBonus) && s.Currency == "EUR" {
			found = s.Count != nil && *s.Count == 0 && s.Total == 0
		}
	}

	if !found {
		t.Fatal("bonus should be listed in EUR with amount 0 and count 0")
	}
}

// ---------------------------------------------------------------------------------------------
// selection

func TestAnalyticsSelection(t *testing.T) {
	sel := anTestSelection(t, nil)

	if sel.AccountIncluded(anTHidden) {
		t.Fatal("hidden accounts are excluded by default")
	}

	if len(sel.HiddenExcluded) != 1 || sel.HiddenExcluded[0] != anTHidden {
		t.Fatalf("hidden exclusion not reported: %v", sel.HiddenExcluded)
	}

	if sel.AccountIncluded(anTParent) {
		t.Fatal("a parent account holds no transactions and is never a leaf")
	}

	// naming a parent means its sub-accounts
	sel = anTestSelection(t, &anArgs{AccountIds: []int64{anTParent}})

	if !sel.AccountIncluded(anTSubUSD) || !sel.AccountIncluded(anTSubJPY) || sel.AccountIncluded(anTCheck) {
		t.Fatalf("parent expansion wrong: %v", sel.AccountSet)
	}

	// naming a primary category means its secondaries; exclusion wins
	sel = anTestSelection(t, &anArgs{CategoryIds: []int64{anTFood}, ExcludeCategoryIds: []int64{anTDining}})

	if !sel.CategoryIncluded(anTGrocery) || sel.CategoryIncluded(anTDining) || sel.CategoryIncluded(anTRepair) {
		t.Fatal("category expansion / exclusion wrong")
	}

	excluded := sel.ExcludedCategoryIds()
	sort.Slice(excluded, func(i, j int) bool { return excluded[i] < excluded[j] })

	for _, id := range excluded {
		if id == anTGrocery || id == anTFood {
			t.Fatalf("included category %d listed as excluded", id)
		}
	}

	acc, cats := anTestBooks()

	if _, err := anBuildSelection(acc, cats, &anArgs{AccountIds: []int64{424242}}, false); err == nil || toFail(err).Code != CodeNotFound {
		t.Fatalf("unknown account must be not_found: %v", err)
	}

	if _, err := anBuildSelection(acc, cats, &anArgs{CategoryIds: []int64{424242}}, false); err == nil || toFail(err).Code != CodeNotFound {
		t.Fatalf("unknown category must be not_found: %v", err)
	}
}

// ---------------------------------------------------------------------------------------------
// time: buckets, periods, month segments

func TestAnalyticsBucketer(t *testing.T) {
	loc, _ := time.LoadLocation("America/Los_Angeles")
	rng, err := ParseDateRange("2026-01-15", "2026-07-10", "", "", loc)

	if err != nil {
		t.Fatal(err)
	}

	cases := map[string][]string{
		anIntervalMonth:   {"2026-01", "2026-02", "2026-03", "2026-04", "2026-05", "2026-06", "2026-07"},
		anIntervalQuarter: {"2026-Q1", "2026-Q2", "2026-Q3"},
		anIntervalYear:    {"2026"},
		anIntervalNone:    {"2026-01-15..2026-07-10"},
	}

	for interval, want := range cases {
		b := newAnBucketer(interval, rng, loc, time.Monday)
		got := anPeriodOrder(b.Periods())

		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Fatalf("%s: got %v want %v", interval, got, want)
		}
	}

	b := newAnBucketer(anIntervalMonth, rng, loc, time.Monday)
	ps := b.Periods()

	if ps[0].Start != "2026-01-15" || ps[0].End != "2026-01-31" || ps[len(ps)-1].End != "2026-07-10" {
		t.Fatalf("periods must be clipped to the range: %+v", ps)
	}

	// weeks start on the user's first day of week
	w := newAnBucketer(anIntervalWeek, rng, loc, time.Monday)

	if k := w.Key(time.Date(2026, 1, 18, 0, 0, 0, 0, loc)); k != "2026-01-12" { // Sunday → Monday 12th
		t.Fatalf("monday week key: %s", k)
	}

	w = newAnBucketer(anIntervalWeek, rng, loc, time.Sunday)

	if k := w.Key(time.Date(2026, 1, 18, 0, 0, 0, 0, loc)); k != "2026-01-18" {
		t.Fatalf("sunday week key: %s", k)
	}
}

func TestAnalyticsMonthSegments(t *testing.T) {
	loc := time.UTC
	segs := anMonthSegments(time.Date(2026, 1, 15, 0, 0, 0, 0, loc), time.Date(2026, 4, 30, 0, 0, 0, 0, loc), loc)

	if len(segs) != 4 || segs[0].Full || !segs[1].Full || !segs[2].Full || !segs[3].Full {
		t.Fatalf("segments: %+v", segs)
	}

	segs = anMonthSegments(time.Date(2026, 2, 3, 0, 0, 0, 0, loc), time.Date(2026, 2, 20, 0, 0, 0, 0, loc), loc)

	if len(segs) != 1 || segs[0].Full || segs[0].Start.Day() != 3 || segs[0].End.Day() != 20 {
		t.Fatalf("one partial month: %+v", segs)
	}
}

func TestAnalyticsNamedPeriods(t *testing.T) {
	loc := time.UTC
	today := time.Date(2026, 9, 21, 0, 0, 0, 0, loc) // a Monday

	check := func(name, start, end string) {
		s, e, ok := anResolveNamedPeriod(name, today, time.Sunday, nil)

		if !ok || s != start || e != end {
			t.Fatalf("%s: got %s..%s want %s..%s", name, s, e, start, end)
		}
	}

	check("today", "2026-09-21", "2026-09-21")
	check("this_week", "2026-09-20", "2026-09-26")
	check("last_week", "2026-09-13", "2026-09-19")
	check("this_month", "2026-09-01", "2026-09-30")
	check("last_month", "2026-08-01", "2026-08-31")
	check("this_year", "2026-01-01", "2026-12-31")
	check("last_year", "2025-01-01", "2025-12-31")

	if _, _, ok := anResolveNamedPeriod("custom", today, time.Sunday, nil); ok {
		t.Fatal("custom needs a range")
	}

	if _, _, ok := anResolveNamedPeriod("next_decade", today, time.Sunday, nil); ok {
		t.Fatal("unknown names are refused")
	}
}

func TestAnalyticsTimezoneDecidesTheMonth(t *testing.T) {
	// 23:30 on 31 January in Los Angeles is 07:30 on 1 February in London
	la, _ := time.LoadLocation("America/Los_Angeles")
	london, _ := time.LoadLocation("Europe/London")
	instant := time.Date(2026, 1, 31, 23, 30, 0, 0, la)

	rngLA, _ := ParseDateRange("2026-01-01", "2026-02-28", "", "", la)
	rngLon, _ := ParseDateRange("2026-01-01", "2026-02-28", "", "", london)

	if k := newAnBucketer(anIntervalMonth, rngLA, la, time.Sunday).Key(instant.In(la)); k != "2026-01" {
		t.Fatalf("LA month: %s", k)
	}

	if k := newAnBucketer(anIntervalMonth, rngLon, london, time.Sunday).Key(instant.In(london)); k != "2026-02" {
		t.Fatalf("London month: %s", k)
	}
}

// ---------------------------------------------------------------------------------------------
// balances

func TestAnalyticsBalanceTimeline(t *testing.T) {
	tl := &anBalanceTimeline{byAccount: map[int64][]anDayBalance{
		1: {{YMD: 20260101, Opening: 1000, Closing: 1000}, {YMD: 20260110, Opening: 1000, Closing: 700}, {YMD: 20260120, Opening: 700, Closing: 900}},
		2: {{YMD: 20260115, Opening: 0, Closing: 5000}},
	}}

	cases := []struct {
		acc      int64
		ymd      int32
		closing  int64
		openingA int64
	}{
		{1, 20251231, 1000, 1000},
		{1, 20260105, 1000, 1000},
		{1, 20260110, 700, 1000},
		{1, 20260115, 700, 700},
		{1, 20260131, 900, 900},
		{2, 20260101, 0, 0},
		{2, 20260115, 5000, 0},
		{2, 20260116, 5000, 5000},
		{3, 20260116, 0, 0},
	}

	for _, c := range cases {
		if got := tl.ClosingAt(c.acc, c.ymd); got != c.closing {
			t.Fatalf("closing %d@%d: got %d want %d", c.acc, c.ymd, got, c.closing)
		}

		if got := tl.OpeningAt(c.acc, c.ymd); got != c.openingA {
			t.Fatalf("opening %d@%d: got %d want %d", c.acc, c.ymd, got, c.openingA)
		}
	}
}

func TestAnalyticsCashFlowReconciles(t *testing.T) {
	loc := time.UTC
	sel := anTestSelection(t, &anArgs{AccountIds: []int64{anTCheck, anTSave}})
	rng, _ := ParseDateRange("2026-03-01", "2026-03-31", "", "", loc)
	b := newAnBucketer(anIntervalMonth, rng, loc, time.Sunday)
	at := func(d int) int64 { return time.Date(2026, 3, d, 12, 0, 0, 0, loc).Unix() }
	dest := int64(30000)

	rows := []*models.TransactionInfoResponse{
		{Id: 1, Type: models.TRANSACTION_TYPE_INCOME, Time: at(1), SourceAccountId: anTCheck, SourceAmount: 500000, CategoryId: anTSalary},
		{Id: 2, Type: models.TRANSACTION_TYPE_EXPENSE, Time: at(2), SourceAccountId: anTCheck, SourceAmount: 12050, CategoryId: anTGrocery},
		{Id: 3, Type: models.TRANSACTION_TYPE_TRANSFER, Time: at(3), SourceAccountId: anTCheck, DestinationAccountId: anTSave, SourceAmount: 30000, DestinationAmount: &dest, CategoryId: anTXferSub},
		{Id: 4, Type: models.TRANSACTION_TYPE_TRANSFER, Time: at(4), SourceAccountId: anTCheck, DestinationAccountId: anTCard, SourceAmount: 20000, DestinationAmount: &dest, CategoryId: anTXferSub},
		{Id: 5, Type: models.TRANSACTION_TYPE_EXPENSE, Time: at(5), SourceAccountId: anTCard, SourceAmount: 999, CategoryId: anTDining},
		{Id: 6, Type: models.TRANSACTION_TYPE_MODIFY_BALANCE, Time: at(6), SourceAccountId: anTSave, SourceAmount: 100},
	}

	flows, internal, err := anCashFlows(rows, sel, b, loc, false)

	if err != nil {
		t.Fatal(err)
	}

	if internal != 1 {
		t.Fatalf("checking→savings is internal: %d", internal)
	}

	f := flows["2026-03"]["USD"]

	if f.Income != 500000 || f.Expense != -12050 || f.TransfersOut != -20000 || f.TransfersIn != 0 {
		t.Fatalf("flows: %+v", f)
	}

	// opening 1000.00 + 50.00; closing moves by income − expense − card payment, plus a 1.00 adjustment
	tl := &anBalanceTimeline{byAccount: map[int64][]anDayBalance{
		anTCheck: {{YMD: 20260301, Opening: 100000, Closing: 600000}, {YMD: 20260304, Opening: 557950, Closing: 537950}},
		anTSave:  {{YMD: 20260301, Opening: 5000, Closing: 5000}, {YMD: 20260306, Opening: 35000, Closing: 35100}},
	}}
	series, err := anCashFlowSeries(flows, tl, sel, b.Periods(), loc)

	if err != nil {
		t.Fatal(err)
	}

	got := map[string]int64{}

	for _, s := range series {
		got[s.Key] = s.points["2026-03"]
	}

	opening, closing := int64(105000), int64(537950+35100)

	if got["opening"] != opening || got["closing"] != closing {
		t.Fatalf("balances: %v", got)
	}

	if got["opening"]+got["in"]+got["out"]+got["adjustments"] != got["closing"] {
		t.Fatalf("opening + in + out + adjustments must equal closing: %v", got)
	}
}

func TestAnalyticsNetWorthSeries(t *testing.T) {
	loc := time.UTC
	sel := anTestSelection(t, &anArgs{AccountIds: []int64{anTCheck, anTCard, anTEuro}})
	rng, _ := ParseDateRange("2026-01-01", "2026-02-28", "", "", loc)
	periods := newAnBucketer(anIntervalMonth, rng, loc, time.Sunday).Periods()
	tl := &anBalanceTimeline{byAccount: map[int64][]anDayBalance{
		anTCheck: {{YMD: 20260105, Opening: 100000, Closing: 90000}, {YMD: 20260210, Opening: 90000, Closing: 120000}},
		anTCard:  {{YMD: 20260115, Opening: -5000, Closing: -25000}},
		anTEuro:  {{YMD: 20260101, Opening: 7000, Closing: 7000}},
	}}

	aggs, accounts, err := anNetWorthSeries(tl, sel, periods, loc)

	if err != nil {
		t.Fatal(err)
	}

	got := map[string]int64{}

	for _, s := range aggs {
		got[s.Key+"/"+s.Currency+"/jan"] = s.points["2026-01"]
		got[s.Key+"/"+s.Currency+"/feb"] = s.points["2026-02"]
	}

	want := map[string]int64{
		"assets/USD/jan": 90000, "liabilities/USD/jan": -25000, "net/USD/jan": 65000,
		"assets/USD/feb": 120000, "liabilities/USD/feb": -25000, "net/USD/feb": 95000,
		"assets/EUR/jan": 7000, "net/EUR/jan": 7000,
	}

	for k, v := range want {
		if got[k] != v {
			t.Fatalf("%s: got %d want %d (%v)", k, got[k], v, got)
		}
	}

	if len(accounts) != 3 {
		t.Fatalf("per-account series: %d", len(accounts))
	}
}

// ---------------------------------------------------------------------------------------------
// detectors

func TestAnalyticsNormalizePayee(t *testing.T) {
	cases := map[string]string{
		"STARBUCKS STORE #12345 SEATTLE WA": "STARBUCKS STORE SEATTLE WA",
		"Starbucks Store 54321 Seattle, WA": "STARBUCKS STORE SEATTLE WA",
		"POS DEBIT ACME HARDWARE 0042":      "ACME HARDWARE",
		"amazon.com":                        "AMAZON",
		"   ":                               "",
		"Rent — September 2026":             "RENT SEPTEMBER",
	}

	for in, want := range cases {
		if got := anNormalizePayee(in); got != want {
			t.Fatalf("%q: got %q want %q", in, got, want)
		}
	}
}

func TestAnalyticsPayeeGroupingKeepsVariants(t *testing.T) {
	txns := []anTxn{
		{Id: "1", Date: "2026-01-02", Amount: -500, Currency: "USD", Comment: "STARBUCKS #1", AccountId: 1},
		{Id: "2", Date: "2026-01-09", Amount: -650, Currency: "USD", Comment: "Starbucks 22", AccountId: 2},
		{Id: "3", Date: "2026-01-10", Amount: -700, Currency: "EUR", Comment: "Starbucks", AccountId: 3},
	}

	groups, err := anGroupPayees(txns)

	if err != nil {
		t.Fatal(err)
	}

	if len(groups) != 2 {
		t.Fatalf("one group per currency expected: %d", len(groups))
	}

	g := groups[0]

	if g.key != "STARBUCKS" || g.total != -1150 || g.count != 2 || len(anVariantsOf(g)) != 2 || g.first != "2026-01-02" || g.last != "2026-01-09" {
		t.Fatalf("group: %+v", g)
	}
}

func TestAnalyticsRecurringDetection(t *testing.T) {
	loc := time.UTC
	d := func(y int, m time.Month, day int) time.Time { return time.Date(y, m, day, 0, 0, 0, 0, loc) }

	monthly := []time.Time{d(2026, 1, 3), d(2026, 2, 3), d(2026, 3, 4), d(2026, 4, 2), d(2026, 5, 3)}
	fit := anFitCadence(monthly, 3)

	if !fit.Consistent || fit.Cadence.Name != "monthly" || fit.Matches != 4 {
		t.Fatalf("monthly: %+v", fit)
	}

	random := []time.Time{d(2026, 1, 3), d(2026, 1, 9), d(2026, 3, 20), d(2026, 3, 22), d(2026, 7, 1)}

	if fit := anFitCadence(random, 3); fit.Consistent {
		t.Fatalf("random dates must not be recurring: %+v", fit)
	}

	// two charges on one day fold into one occurrence
	weekly := []time.Time{d(2026, 1, 5), d(2026, 1, 5), d(2026, 1, 12), d(2026, 1, 19), d(2026, 1, 26)}

	if fit := anFitCadence(weekly, 0); !fit.Consistent || fit.Cadence.Name != "weekly" {
		t.Fatalf("weekly: %+v", fit)
	}

	if next := anNextOccurrence(anCadences[2], d(2026, 1, 15)); !next.Equal(d(2026, 2, 15)) {
		t.Fatalf("monthly next occurrence: %s", next.Format("2006-01-02"))
	}
}

func TestAnalyticsRecurringGrouping(t *testing.T) {
	txns := []anTxn{
		{Id: "1", Kind: anKindExpense, Currency: "USD", Comment: "NETFLIX.COM 866", Amount: -1599, CategoryId: 1},
		{Id: "2", Kind: anKindExpense, Currency: "USD", Comment: "Netflix.com", Amount: -1599, CategoryId: 1},
		{Id: "3", Kind: anKindExpense, Currency: "USD", Comment: "", Amount: -200000, CategoryId: 2},
		{Id: "4", Kind: anKindExpense, Currency: "USD", Comment: "", Amount: -200000, CategoryId: 2},
		{Id: "5", Kind: anKindExpense, Currency: "USD", Comment: "", Amount: -1234, CategoryId: 2},
	}

	groups := anGroupRecurring(txns)

	if len(groups) != 3 {
		t.Fatalf("groups: %d", len(groups))
	}

	if len(groups[0].Occurrences) != 2 || len(groups[1].Occurrences) != 2 {
		t.Fatalf("netflix and rent should each have two occurrences: %+v", groups)
	}
}

func TestAnalyticsAnomalyTest(t *testing.T) {
	z, _ := new(big.Rat).SetString("2")
	history := []int64{-10000, -11000, -9000, -10000, -10500, -9500}

	calm := anAnomalyTest(history, -10200, z, 0)

	if calm.Flagged || calm.Mean != -10000 {
		t.Fatalf("calm month flagged: %+v", calm)
	}

	spike := anAnomalyTest(history, -30000, z, 0)

	if !spike.Flagged || spike.Deviation != -20000 || !strings.HasPrefix(spike.ZScore, "-") {
		t.Fatalf("spike not flagged: %+v", spike)
	}

	// population stddev of the history is 6.45… → 645 hundredths (floor)
	if spike.StdDev != 645 {
		t.Fatalf("stddev: %d", spike.StdDev)
	}

	if spike.ZScore != "-30.98" {
		t.Fatalf("zScore: %s", spike.ZScore)
	}

	// min_amount suppresses small deviations
	if small := anAnomalyTest(history, -12000, z, 5000); small.Flagged {
		t.Fatalf("below min_amount must not flag: %+v", small)
	}

	flat := anAnomalyTest([]int64{-5000, -5000, -5000}, -7000, z, 0)

	if !flat.Flagged || !flat.NoVar || flat.ZScore != "" {
		t.Fatalf("no-variance history: %+v", flat)
	}
}

func TestAnalyticsRunway(t *testing.T) {
	r := anComputeRunway(1200000, 600000, 6, 181) // 12,000.00 liquid; 6,000.00 net outflow over 6 months

	if r.Status != "burning" || r.Months != "12.0" || *r.WholeMonths != 12 || *r.AverageNetBurn != 100000 {
		t.Fatalf("runway: %+v", r)
	}

	r = anComputeRunway(100000, 70000, 3, 92) // 1000 / (700/3) = 4.2857…

	if r.Months != "4.2" || *r.WholeMonths != 4 || *r.Days != 131 {
		t.Fatalf("runway truncation: %+v days %d", r, *r.Days)
	}

	if r = anComputeRunway(100000, -5000, 3, 92); r.Status != "not_burning" || r.WholeMonths != nil {
		t.Fatalf("not burning: %+v", r)
	}

	if r = anComputeRunway(0, 5000, 3, 92); r.Status != "no_liquid_assets" {
		t.Fatalf("no liquid assets: %+v", r)
	}
}

func TestAnalyticsMedianAndRatString(t *testing.T) {
	if m := anMedian([]int64{-1599, -1599, -1799}); m != -1599 {
		t.Fatalf("odd median %d", m)
	}

	if m := anMedian([]int64{1, 2}); m != 2 { // 1.5 → 2, half away from zero
		t.Fatalf("even median %d", m)
	}

	if m := anMedian([]int64{-1, -2}); m != -2 {
		t.Fatalf("negative even median %d", m)
	}

	cases := map[string]*big.Rat{
		"0.00":  big.NewRat(0, 1),
		"2.99":  big.NewRat(2999, 1000),
		"-0.50": big.NewRat(-1, 2),
		"12.34": big.NewRat(1234, 100),
	}

	for want, r := range cases {
		if got := anRatString(r, 2); got != want {
			t.Fatalf("anRatString(%s): got %s want %s", r.String(), got, want)
		}
	}
}

// ---------------------------------------------------------------------------------------------
// arguments and routes

func anTestCtx(t *testing.T, rawQuery string) *Ctx {
	t.Helper()
	gin.SetMode(gin.TestMode)
	gc, _ := gin.CreateTestContext(httptest.NewRecorder())
	gc.Request = httptest.NewRequest("GET", "/machine/v1/analytics/x?"+rawQuery, nil)

	return &Ctx{Gin: gc, Loc: time.UTC}
}

func TestAnalyticsRejectsUnknownArguments(t *testing.T) {
	t.Setenv("EZBK_CREDENTIALS_FILE", filepath.Join(t.TempDir(), "creds.json"))
	t.Setenv("EZBK_STATE_DIR", t.TempDir())

	mc := anTestCtx(t, "start=2026-01-01&start_date=2026-01-01&interval=month")
	err := anCheckQuery(mc, "interval")

	if err == nil || toFail(err).Code != CodeInvalidInput || !strings.Contains(toFail(err).Message, "start_date") {
		t.Fatalf("a typo'd argument must be refused: %v", err)
	}

	mc = anTestCtx(t, "start=2026-01-01&account_ids[]=1&interval=month")

	if err := anCheckQuery(mc, "interval"); err != nil {
		t.Fatalf("known arguments refused: %v", err)
	}
}

func TestAnalyticsParseArgs(t *testing.T) {
	mc := anTestCtx(t, "start=2026-01-01&end=2026-03-31&account_ids=11,12&account_ids=13&convert_to=usd&include_transfers=true")
	a, err := anParseArgs(mc, "", "")

	if err != nil {
		t.Fatal(err)
	}

	if len(a.AccountIds) != 3 || a.ConvertTo != "USD" || !a.IncludeTransfers || a.Range.Start != "2026-01-01" {
		t.Fatalf("args: %+v", a)
	}

	for _, bad := range []string{"convert_to=XXX", "tag_filter=9:1", "account_ids=abc", "start=2026-13-01&end=2026-12-01", "start=2026-02-01&end=2026-01-01", "include_hidden_accounts=maybe"} {
		mc := anTestCtx(t, bad)

		if _, err := anParseArgs(mc, "2026-01-01", "2026-01-31"); err == nil || toFail(err).Code != CodeInvalidInput {
			t.Fatalf("%s must be invalid_input: %v", bad, err)
		}
	}
}

func TestAnalyticsRoutesRegistered(t *testing.T) {
	want := []string{
		"/analytics/period-summary", "/analytics/spending-by-category", "/analytics/income-by-category",
		"/analytics/income-vs-expense", "/analytics/cash-flow", "/analytics/net-worth",
		"/analytics/category-trend", "/analytics/tag-breakdown", "/analytics/payee-leaderboard",
		"/analytics/recurring", "/analytics/anomalies", "/analytics/runway", "/analytics/import-fallout",
		"/accounts/:id/balance", "/accounts/:id/balance-history",
	}

	have := map[string]RouteDef{}

	for _, r := range analyticsRoutes() {
		have[r.Method+" "+r.Path] = r
	}

	for _, p := range want {
		r, ok := have["GET "+p]

		if !ok {
			t.Fatalf("route %s not registered", p)
		}

		if r.Tier != TierRead || !r.Composed || r.Handler == nil || r.Status == StatusPlanned {
			t.Fatalf("route %s must be a live, composed, read-tier route: %+v", p, r)
		}
	}

	if len(have) != len(want) {
		t.Fatalf("unexpected extra routes: %d", len(have))
	}
}

// the canary of apis.mdx §17.1: no float in the analytics plane
func TestAnalyticsNoFloats(t *testing.T) {
	files, _ := filepath.Glob("analytics_*.go")
	files = append(files, "routes_analytics.go")

	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}

		data, err := os.ReadFile(f)

		if err != nil {
			t.Fatal(err)
		}

		for _, banned := range []string{"float64", "float32", ".Hours()", ".Minutes()", ".Seconds()"} {
			if strings.Contains(string(data), banned) {
				t.Fatalf("%s uses %s", f, banned)
			}
		}
	}
}

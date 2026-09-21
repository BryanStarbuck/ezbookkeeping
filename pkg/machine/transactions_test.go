package machine

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/mayswind/ezbookkeeping/pkg/models"
	"github.com/mayswind/ezbookkeeping/pkg/settings"
)

// Synthetic books only: no real names, no real amounts.

func txnTestLookup() *txnLookup {
	accounts := []*models.Account{
		{AccountId: 101, Name: "Alpha Checking", Type: models.ACCOUNT_TYPE_SINGLE_ACCOUNT, Currency: "USD", Category: models.ACCOUNT_CATEGORY_CHECKING_ACCOUNT},
		{AccountId: 102, Name: "Beta Savings", Type: models.ACCOUNT_TYPE_SINGLE_ACCOUNT, Currency: "USD", Category: models.ACCOUNT_CATEGORY_SAVINGS_ACCOUNT},
		{AccountId: 103, Name: "Gamma Yen", Type: models.ACCOUNT_TYPE_SINGLE_ACCOUNT, Currency: "JPY", Category: models.ACCOUNT_CATEGORY_CASH},
		{AccountId: 200, Name: "Delta Group", Type: models.ACCOUNT_TYPE_MULTI_SUB_ACCOUNTS, Currency: "---", Category: models.ACCOUNT_CATEGORY_CHECKING_ACCOUNT},
		{AccountId: 201, Name: "Sub One", ParentAccountId: 200, Type: models.ACCOUNT_TYPE_SINGLE_ACCOUNT, Currency: "EUR", Category: models.ACCOUNT_CATEGORY_CHECKING_ACCOUNT},
		{AccountId: 202, Name: "sub one", ParentAccountId: 200, Type: models.ACCOUNT_TYPE_SINGLE_ACCOUNT, Currency: "EUR", Category: models.ACCOUNT_CATEGORY_CHECKING_ACCOUNT},
		{AccountId: 300, Name: "Hidden Box", Type: models.ACCOUNT_TYPE_SINGLE_ACCOUNT, Currency: "USD", Hidden: true},
	}
	categories := []*models.TransactionCategory{
		{CategoryId: 10, Name: "Food", Type: models.CATEGORY_TYPE_EXPENSE},
		{CategoryId: 11, Name: "Groceries", ParentCategoryId: 10, Type: models.CATEGORY_TYPE_EXPENSE},
		{CategoryId: 12, Name: "Other", ParentCategoryId: 10, Type: models.CATEGORY_TYPE_EXPENSE},
		{CategoryId: 20, Name: "Shopping", Type: models.CATEGORY_TYPE_EXPENSE},
		{CategoryId: 21, Name: "Other", ParentCategoryId: 20, Type: models.CATEGORY_TYPE_EXPENSE},
		{CategoryId: 30, Name: "Earnings", Type: models.CATEGORY_TYPE_INCOME},
		{CategoryId: 31, Name: "Salary", ParentCategoryId: 30, Type: models.CATEGORY_TYPE_INCOME},
		{CategoryId: 32, Name: "Other", ParentCategoryId: 30, Type: models.CATEGORY_TYPE_INCOME},
		{CategoryId: 40, Name: "Moves", Type: models.CATEGORY_TYPE_TRANSFER},
		{CategoryId: 41, Name: "Internal", ParentCategoryId: 40, Type: models.CATEGORY_TYPE_TRANSFER},
	}
	tags := []*models.TransactionTag{
		{TagId: 501, Name: "trip"},
		{TagId: 502, Name: "work"},
		{TagId: 503, Name: "Work"},
	}

	return txnNewLookup(accounts, categories, tags)
}

func txnTestCtx() *Ctx {
	return &Ctx{Loc: time.UTC, Config: &settings.Config{}}
}

func txnFailCode(t *testing.T, err error) string {
	t.Helper()

	if err == nil {
		return ""
	}

	f, ok := err.(*Fail)

	if !ok {
		t.Fatalf("expected *Fail, got %T %v", err, err)
	}

	if f.Hint == "" {
		t.Fatalf("every error names a fix, but %q has no hint", f.Message)
	}

	return f.Code
}

func TestTxnParseTypeAndName(t *testing.T) {
	for name, want := range txnTypeByName {
		got, err := txnParseType(strings.ToUpper(name))

		if err != nil || got != want {
			t.Fatalf("txnParseType(%q) = %v, %v", name, got, err)
		}

		if txnTypeName(int64(want)) != name {
			t.Fatalf("txnTypeName(%d) = %q, want %q", want, txnTypeName(int64(want)), name)
		}
	}

	if got, err := txnParseType(""); err != nil || got != 0 {
		t.Fatalf("empty type should mean any, got %v %v", got, err)
	}

	if _, err := txnParseType("refund"); txnFailCode(t, err) != CodeInvalidInput {
		t.Fatalf("unknown type should be invalid_input")
	}
}

func TestTxnResolveName(t *testing.T) {
	lk := txnTestLookup()

	id, err := txnResolveName("account", "account_name", "Alpha Checking", lk.accountCandidates(true), "hint")

	if err != nil || id != 101 {
		t.Fatalf("exact match: %d %v", id, err)
	}

	id, err = txnResolveName("account", "account_name", "alpha checking", lk.accountCandidates(true), "hint")

	if err != nil || id != 101 {
		t.Fatalf("case-insensitive match: %d %v", id, err)
	}

	// exact wins over case-insensitive: "Sub One" is exact for 201 even though "sub one" (202) folds equal
	id, err = txnResolveName("account", "account_name", "Sub One", lk.accountCandidates(true), "hint")

	if err != nil || id != 201 {
		t.Fatalf("exact-first: %d %v", id, err)
	}

	// "SUB ONE" matches both case-insensitively → ambiguous with candidates
	_, err = txnResolveName("account", "account_name", "SUB ONE", lk.accountCandidates(true), "hint")

	if txnFailCode(t, err) != CodeInvalidInput {
		t.Fatalf("ambiguous name should be invalid_input, got %v", err)
	}

	if d, ok := err.(*Fail).Details.(map[string]any); !ok || len(d["candidates"].([]map[string]string)) != 2 {
		t.Fatalf("ambiguity must carry the candidates: %+v", err.(*Fail).Details)
	}

	// the parent path disambiguates
	id, err = txnResolveName("account", "account_name", "Delta Group > sub one", lk.accountCandidates(true), "hint")

	if err != nil || id != 202 {
		t.Fatalf("parent path: %d %v", id, err)
	}

	if _, err = txnResolveName("account", "account_name", "Nope", lk.accountCandidates(true), "hint"); txnFailCode(t, err) != CodeNotFound {
		t.Fatalf("unknown name should be not_found")
	}

	if _, err = txnResolveName("account", "account_name", "  ", lk.accountCandidates(true), "hint"); txnFailCode(t, err) != CodeInvalidInput {
		t.Fatalf("empty name should be invalid_input")
	}
}

func TestTxnResolveCategoryByTypeAndPath(t *testing.T) {
	lk := txnTestLookup()

	// "Other" exists as an expense sub-category twice and once as income
	if _, err := lk.resolveCategory("", "Other", models.TRANSACTION_TYPE_EXPENSE); txnFailCode(t, err) != CodeInvalidInput {
		t.Fatalf("two expense 'Other' categories should be ambiguous")
	}

	c, err := lk.resolveCategory("", "Other", models.TRANSACTION_TYPE_INCOME)

	if err != nil || c.CategoryId != 32 {
		t.Fatalf("income 'Other' is unique: %v %v", c, err)
	}

	c, err = lk.resolveCategory("", "Shopping > Other", models.TRANSACTION_TYPE_EXPENSE)

	if err != nil || c.CategoryId != 21 {
		t.Fatalf("path resolves: %v %v", c, err)
	}

	// a primary category is refused
	if _, err := lk.resolveCategory("10", "", models.TRANSACTION_TYPE_EXPENSE); txnFailCode(t, err) != CodeInvalidInput {
		t.Fatalf("primary category should be refused")
	}

	// a category of the wrong type is refused, naming its type
	_, err = lk.resolveCategory("31", "", models.TRANSACTION_TYPE_EXPENSE)

	if txnFailCode(t, err) != CodeInvalidInput || !strings.Contains(err.(*Fail).Message, "income") {
		t.Fatalf("type mismatch should name the category's type: %v", err)
	}

	if _, err := lk.resolveCategory("", "", models.TRANSACTION_TYPE_EXPENSE); txnFailCode(t, err) != CodeInvalidInput {
		t.Fatalf("missing category should be invalid_input")
	}
}

func TestTxnResolveAccountRefusesParent(t *testing.T) {
	lk := txnTestLookup()

	_, err := lk.resolveAccount("account_id", "200", "account_name", "", true)

	if txnFailCode(t, err) != CodeInvalidInput {
		t.Fatalf("a parent account must be refused for a write")
	}

	subs := err.(*Fail).Details.(map[string]any)["subAccounts"].([]map[string]string)

	if len(subs) != 2 {
		t.Fatalf("the refusal names the sub-accounts: %v", subs)
	}

	if a, err := lk.resolveAccount("account_id", "200", "account_name", "", false); err != nil || a.AccountId != 200 {
		t.Fatalf("a parent is fine in a filter: %v %v", a, err)
	}

	if _, err := lk.resolveAccount("account_id", "999", "account_name", "", true); txnFailCode(t, err) != CodeNotFound {
		t.Fatalf("unknown id should be not_found")
	}

	if _, err := lk.resolveAccount("account_id", "", "account_name", "", true); txnFailCode(t, err) != CodeInvalidInput {
		t.Fatalf("missing account should be invalid_input")
	}
}

func TestTxnResolveTags(t *testing.T) {
	lk := txnTestLookup()

	ids, err := lk.resolveTags([]string{"501"}, []string{"trip", "work"})

	if err != nil || strings.Join(ids, ",") != "501,502" {
		t.Fatalf("tags dedupe in order: %v %v", ids, err)
	}

	if _, err := lk.resolveTags(nil, []string{"WORK"}); txnFailCode(t, err) != CodeInvalidInput {
		t.Fatalf("WORK folds to two tags and must be ambiguous")
	}

	if _, err := lk.resolveTags([]string{"777"}, nil); txnFailCode(t, err) != CodeNotFound {
		t.Fatalf("unknown tag id should be not_found")
	}

	if ids, err := lk.resolveTags(nil, nil); err != nil || ids == nil || len(ids) != 0 {
		t.Fatalf("no tags is an empty, non-nil list")
	}
}

func TestTxnAmountFilter(t *testing.T) {
	p := func(v int64) *int64 { return &v }

	cases := []struct {
		min, max *int64
		want     string
		bad      bool
	}{
		{nil, nil, "", false},
		{p(50000), nil, "gt:49999", false},
		{nil, p(1250), "lt:1251", false},
		{p(100), p(200), "bt:100:200", false},
		{p(0), p(0), "bt:0:0", false},
		{p(300), p(200), "", true},
	}

	for _, c := range cases {
		got, err := txnAmountFilter(c.min, c.max)

		if c.bad {
			if txnFailCode(t, err) != CodeInvalidInput {
				t.Fatalf("max < min should be invalid")
			}

			continue
		}

		if err != nil || got != c.want {
			t.Fatalf("txnAmountFilter = %q %v, want %q", got, err, c.want)
		}
	}
}

func TestTxnBuildTagFilter(t *testing.T) {
	cases := []struct {
		any      []string
		raw      string
		untagged bool
		want     string
		bad      bool
	}{
		{nil, "", false, "", false},
		{nil, "", true, "none", false},
		{[]string{"1", "2"}, "", false, "0:1,2", false},
		{[]string{"1"}, "2:3", false, "0:1;2:3", false},
		{nil, "1:4,5", false, "1:4,5", false},
		{nil, "none", false, "none", false},
		{[]string{"1"}, "", true, "", true},
		{nil, "garbage", false, "", true},
		{nil, "9:1", false, "", true},
	}

	for _, c := range cases {
		got, err := txnBuildTagFilter(c.any, c.raw, c.untagged)

		if c.bad {
			if txnFailCode(t, err) != CodeInvalidInput {
				t.Fatalf("%+v should be invalid", c)
			}

			continue
		}

		if err != nil || got != c.want {
			t.Fatalf("txnBuildTagFilter(%+v) = %q %v, want %q", c, got, err, c.want)
		}
	}
}

func TestTxnResolveFilterCurrencyRule(t *testing.T) {
	lk := txnTestLookup()
	mc := txnTestCtx()

	// USD, JPY and EUR accounts exist: an amount filter with no currency is refused (R11)
	_, err := txnResolveFilter(mc, lk, &txnFilterArgs{MinAmount: "50000"})

	if txnFailCode(t, err) != CodeInvalidInput {
		t.Fatalf("min_amount across currencies must be refused, got %v", err)
	}

	// narrowed to one currency it is fine, and the accounts are that currency's
	rf, err := txnResolveFilter(mc, lk, &txnFilterArgs{MinAmount: "50000", Currency: "usd"})

	if err != nil {
		t.Fatalf("currency set: %v", err)
	}

	if rf.AmountFilter != "gt:49999" || len(rf.AccountIds) != 3 {
		t.Fatalf("resolved %q accounts %v", rf.AmountFilter, rf.AccountIds)
	}

	// accounts that share a currency need no currency
	if _, err := txnResolveFilter(mc, lk, &txnFilterArgs{MaxAmount: "100", AccountIds: []string{"101", "102"}}); err != nil {
		t.Fatalf("same-currency accounts: %v", err)
	}

	// a currency with no account selects nothing, rather than everything
	rf, err = txnResolveFilter(mc, lk, &txnFilterArgs{Currency: "GBP"})

	if err != nil || !rf.Empty {
		t.Fatalf("GBP should be an empty selection: %+v %v", rf, err)
	}

	// a parent account expands to its sub-accounts
	rf, err = txnResolveFilter(mc, lk, &txnFilterArgs{AccountNames: []string{"Delta Group"}})

	if err != nil || len(rf.AccountIds) != 2 {
		t.Fatalf("parent expands: %+v %v", rf, err)
	}

	// decimals are refused, with the integer the caller meant in the hint
	if _, err := txnResolveFilter(mc, lk, &txnFilterArgs{MinAmount: "12.50", Currency: "USD"}); txnFailCode(t, err) != CodeInvalidInput {
		t.Fatalf("decimal amount should be refused")
	}
}

func TestTxnResolveFilterDatesAndQuery(t *testing.T) {
	lk := txnTestLookup()
	mc := txnTestCtx()

	rf, err := txnResolveFilter(mc, lk, &txnFilterArgs{Start: "2026-01-01", End: "2026-01-31", Type: "expense", CategoryNames: []string{"Food"}, TagNames: []string{"trip"}, Keyword: "cafe"})

	if err != nil {
		t.Fatalf("resolve: %v", err)
	}

	q := rf.upstreamListQuery()

	if q.Get("type") != "3" || q.Get("category_ids") != "10" || q.Get("tag_filter") != "0:501" || q.Get("keyword") != "cafe" || q.Get("match_mode") != "1" {
		t.Fatalf("query: %v", q)
	}

	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).Unix()
	end := time.Date(2026, 1, 31, 23, 59, 59, 0, time.UTC).Unix()

	if q.Get("min_time") != txnItoa64(start*1000) || q.Get("max_time") != txnItoa64(end*1000+999) {
		t.Fatalf("time bounds: %v", q)
	}

	if _, err := txnResolveFilter(mc, lk, &txnFilterArgs{Start: "2026-02-01", End: "2026-01-01"}); txnFailCode(t, err) != CodeInvalidInput {
		t.Fatalf("end before start should be invalid")
	}

	if _, err := txnResolveFilter(mc, lk, &txnFilterArgs{Start: "last-month"}); txnFailCode(t, err) != CodeInvalidInput {
		t.Fatalf("relative dates never reach the wire")
	}

	// pictures are refused when upstream has them switched off
	if _, err := txnResolveFilter(mc, lk, &txnFilterArgs{WithPictures: true}); txnFailCode(t, err) != CodeForbidden {
		t.Fatalf("with_pictures with pictures off should be forbidden")
	}

	if !(&txnFilterArgs{}).isEmpty() || (&txnFilterArgs{Untagged: true}).isEmpty() {
		t.Fatalf("isEmpty")
	}
}

func txnItoa64(v int64) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func TestTxnTagSetOps(t *testing.T) {
	if got := txnTagsAdd([]string{"1", "2"}, []string{"2", "3", "3"}); strings.Join(got, ",") != "1,2,3" {
		t.Fatalf("add: %v", got)
	}

	if got := txnTagsRemove([]string{"1", "2", "3"}, []string{"2", "9"}); strings.Join(got, ",") != "1,3" {
		t.Fatalf("remove: %v", got)
	}

	if got := txnTagsRemove(nil, []string{"1"}); got == nil || len(got) != 0 {
		t.Fatalf("remove from nothing is an empty list")
	}
}

func txnSampleState() txnState {
	return txnState{
		Id: "9007199254740993", Type: int(models.TRANSACTION_TYPE_EXPENSE), CategoryId: "11", Time: 1767268800, UtcOffset: 0,
		SourceAccountId: "101", SourceAmount: 1250, Comment: "synthetic", TagIds: []string{"502", "501"}, PictureIds: []string{},
	}
}

func TestTxnStatesEqualIgnoresOrder(t *testing.T) {
	a := txnSampleState()
	b := txnSampleState()
	b.TagIds = []string{"501", "502"}

	if !txnStatesEqual(a, b) {
		t.Fatalf("tag order must not matter")
	}

	b.SourceAmount = 1251

	if txnStatesEqual(a, b) {
		t.Fatalf("an amount change must be seen")
	}

	// normalizing never mutates the original
	if a.TagIds[0] != "502" {
		t.Fatalf("normalized mutated its receiver")
	}

	// sorting is numeric, not lexicographic, for ids of different lengths
	if got := txnSortedIds([]string{"100", "99", "1000"}); strings.Join(got, ",") != "99,100,1000" {
		t.Fatalf("sorted ids: %v", got)
	}
}

func TestTxnFieldDiff(t *testing.T) {
	lk := txnTestLookup()
	a := txnSampleState()
	b := a
	b.CategoryId = "12"
	b.TagIds = []string{"501"}
	b.Comment = "changed"

	diff := txnFieldDiff(lk, a, b)
	fields := []string{}

	for _, d := range diff {
		fields = append(fields, d["field"].(string))
	}

	if strings.Join(fields, ",") != "categoryId,comment,tagIds" {
		t.Fatalf("diff fields: %v", fields)
	}

	if diff[0]["toName"] != "Food > Other" {
		t.Fatalf("category diff carries names: %v", diff[0])
	}

	if len(txnFieldDiff(lk, a, a)) != 0 {
		t.Fatalf("no diff for equal states")
	}
}

func TestTxnCreateBodyShape(t *testing.T) {
	s := txnSampleState()
	s.GeoLatitude, s.GeoLongitude = "47.6", "-122.3"
	body := txnModifyBody(s)
	data, err := json.Marshal(body)

	if err != nil {
		t.Fatal(err)
	}

	// decode into upstream's own request type: ids are strings (json:",string"), the id survives
	// above 2^53, and geo decimals pass through verbatim
	var req models.TransactionModifyRequest

	if err := json.Unmarshal(data, &req); err != nil {
		t.Fatalf("upstream cannot read our body: %v\n%s", err, data)
	}

	if req.Id != 9007199254740993 || req.CategoryId != 11 || req.SourceAccountId != 101 || req.DestinationAccountId != 0 || req.SourceAmount != 1250 {
		t.Fatalf("round trip: %+v", req)
	}

	if req.GeoLocation == nil || !strings.Contains(string(data), `"latitude":47.6`) {
		t.Fatalf("geo: %s", data)
	}

	if req.TagIds == nil || req.PictureIds == nil {
		t.Fatalf("tag and picture lists must be sent (a missing list would clear them)")
	}
}

func TestTxnResolveWhen(t *testing.T) {
	loc, err := time.LoadLocation("America/Los_Angeles")

	if err != nil {
		t.Skip("zoneinfo not available")
	}

	sec, off, err := txnResolveWhen("2026-07-04", "", loc)

	if err != nil {
		t.Fatal(err)
	}

	if got := time.Unix(sec, 0).In(loc); got.Hour() != 12 || got.Day() != 4 || off != -420 {
		t.Fatalf("date lands at local noon with the DST offset: %v %d", got, off)
	}

	sec, off, err = txnResolveWhen("", "2026-01-15T08:30:00Z", loc)

	if err != nil || sec != time.Date(2026, 1, 15, 8, 30, 0, 0, time.UTC).Unix() || off != -480 {
		t.Fatalf("RFC 3339: %d %d %v", sec, off, err)
	}

	if _, _, err := txnResolveWhen("", "", loc); txnFailCode(t, err) != CodeInvalidInput {
		t.Fatalf("missing date should be invalid")
	}

	if _, _, err := txnResolveWhen("", "yesterday", loc); txnFailCode(t, err) != CodeInvalidInput {
		t.Fatalf("relative time should be invalid")
	}
}

func TestTxnResolveCreateRow(t *testing.T) {
	lk := txnTestLookup()

	ok, err := txnResolveCreateRow(lk, time.UTC, 0, txnCreateRow{Type: "expense", Date: "2026-03-01", AccountName: "Alpha Checking", Amount: "1250", CategoryName: "Groceries", TagNames: []string{"trip"}})

	if err != nil {
		t.Fatalf("expense: %v", err)
	}

	if ok.State.SourceAccountId != "101" || ok.State.CategoryId != "11" || ok.State.SourceAmount != 1250 || ok.Preview["sourceCurrency"] != "USD" {
		t.Fatalf("resolved: %+v %+v", ok.State, ok.Preview)
	}

	// a same-currency transfer defaults destination_amount to amount
	tr, err := txnResolveCreateRow(lk, time.UTC, 1, txnCreateRow{Type: "transfer", Date: "2026-03-01", AccountId: "101", DestinationAccountId: "102", Amount: "500", CategoryName: "Internal"})

	if err != nil || tr.State.DestinationAmount != 500 || tr.State.DestinationAccountId != "102" {
		t.Fatalf("transfer: %+v %v", tr, err)
	}

	bad := []struct {
		name string
		row  txnCreateRow
		code string
	}{
		{"no type", txnCreateRow{Date: "2026-03-01", AccountId: "101", Amount: "1", CategoryId: "11"}, CodeInvalidInput},
		{"balance modification", txnCreateRow{Type: "balance_modification", Date: "2026-03-01", AccountId: "101", Amount: "1"}, CodeInvalidInput},
		{"decimal amount", txnCreateRow{Type: "expense", Date: "2026-03-01", AccountId: "101", Amount: "12.50", CategoryId: "11"}, CodeInvalidInput},
		{"negative amount", txnCreateRow{Type: "expense", Date: "2026-03-01", AccountId: "101", Amount: "-5", CategoryId: "11"}, CodeInvalidInput},
		{"parent account", txnCreateRow{Type: "expense", Date: "2026-03-01", AccountId: "200", Amount: "5", CategoryId: "11"}, CodeInvalidInput},
		{"hidden account", txnCreateRow{Type: "expense", Date: "2026-03-01", AccountId: "300", Amount: "5", CategoryId: "11"}, CodeInvalidInput},
		{"income category on expense", txnCreateRow{Type: "expense", Date: "2026-03-01", AccountId: "101", Amount: "5", CategoryId: "31"}, CodeInvalidInput},
		{"transfer to itself", txnCreateRow{Type: "transfer", Date: "2026-03-01", AccountId: "101", DestinationAccountId: "101", Amount: "5", CategoryId: "41"}, CodeInvalidInput},
		{"same-currency amounts differ", txnCreateRow{Type: "transfer", Date: "2026-03-01", AccountId: "101", DestinationAccountId: "102", Amount: "5", DestinationAmount: "6", CategoryId: "41"}, CodeInvalidInput},
		{"cross-currency without destination_amount", txnCreateRow{Type: "transfer", Date: "2026-03-01", AccountId: "101", DestinationAccountId: "103", Amount: "5", CategoryId: "41"}, CodeInvalidInput},
		{"destination on an expense", txnCreateRow{Type: "expense", Date: "2026-03-01", AccountId: "101", DestinationAccountId: "102", Amount: "5", CategoryId: "11"}, CodeInvalidInput},
		{"bad geo", txnCreateRow{Type: "expense", Date: "2026-03-01", AccountId: "101", Amount: "5", CategoryId: "11", Geo: &txnGeoArg{Latitude: "91", Longitude: "0"}}, CodeInvalidInput},
		{"unknown account", txnCreateRow{Type: "expense", Date: "2026-03-01", AccountId: "999", Amount: "5", CategoryId: "11"}, CodeNotFound},
	}

	for _, c := range bad {
		_, err := txnResolveCreateRow(lk, time.UTC, 3, c.row)

		if got := txnFailCode(t, err); got != c.code {
			t.Fatalf("%s: code %q, want %q (%v)", c.name, got, c.code, err)
		}

		if !strings.HasPrefix(err.(*Fail).Message, "transactions[3]: ") {
			t.Fatalf("%s: the error names the row: %q", c.name, err.(*Fail).Message)
		}
	}

	// a cross-currency transfer with both amounts is fine and keeps both
	x, err := txnResolveCreateRow(lk, time.UTC, 0, txnCreateRow{Type: "transfer", Date: "2026-03-01", AccountId: "101", DestinationAccountId: "103", Amount: "1000", DestinationAmount: "150000", CategoryId: "41"})

	if err != nil || x.State.DestinationAmount != 150000 || x.Preview["destinationCurrency"] != "JPY" {
		t.Fatalf("cross-currency: %+v %v", x, err)
	}
}

func TestTxnApplyPatch(t *testing.T) {
	lk := txnTestLookup()
	cur := txnSampleState()
	str := func(s string) *string { return &s }
	num := func(s string) *json.Number { n := json.Number(s); return &n }

	next, err := txnApplyPatch(lk, time.UTC, cur, txnPatchArgs{Amount: num("999"), CategoryName: str("Food > Other")})

	if err != nil || next.SourceAmount != 999 || next.CategoryId != "12" {
		t.Fatalf("amount+category: %+v %v", next, err)
	}

	// a date change keeps the time of day
	next, err = txnApplyPatch(lk, time.UTC, cur, txnPatchArgs{Date: str("2026-02-10")})

	if err != nil || time.Unix(next.Time, 0).UTC().Format("2006-01-02 15:04") != "2026-02-10 12:00" {
		t.Fatalf("date: %v %v", time.Unix(next.Time, 0).UTC(), err)
	}

	// changing type to income needs an income category
	if _, err := txnApplyPatch(lk, time.UTC, cur, txnPatchArgs{Type: str("income")}); txnFailCode(t, err) != CodeInvalidInput {
		t.Fatalf("type change without a fitting category should be refused")
	}

	next, err = txnApplyPatch(lk, time.UTC, cur, txnPatchArgs{Type: str("income"), CategoryId: str("31")})

	if err != nil || next.Type != int(models.TRANSACTION_TYPE_INCOME) {
		t.Fatalf("type change: %+v %v", next, err)
	}

	// to a transfer: destination required, same currency copies the amount
	if _, err := txnApplyPatch(lk, time.UTC, cur, txnPatchArgs{Type: str("transfer"), CategoryId: str("41")}); txnFailCode(t, err) != CodeInvalidInput {
		t.Fatalf("transfer without destination should be refused")
	}

	next, err = txnApplyPatch(lk, time.UTC, cur, txnPatchArgs{Type: str("transfer"), CategoryId: str("41"), DestinationAccountId: str("102")})

	if err != nil || next.DestinationAmount != cur.SourceAmount {
		t.Fatalf("to transfer: %+v %v", next, err)
	}

	// never to or from balance modification
	if _, err := txnApplyPatch(lk, time.UTC, cur, txnPatchArgs{Type: str("balance_modification")}); txnFailCode(t, err) != CodeInvalidInput {
		t.Fatalf("to balance_modification should be refused")
	}

	// moving to another currency needs the amount in that currency
	if _, err := txnApplyPatch(lk, time.UTC, cur, txnPatchArgs{AccountId: str("103")}); txnFailCode(t, err) != CodeInvalidInput {
		t.Fatalf("cross-currency move without amount should be refused")
	}

	// tags replace; geo clears
	withGeo := cur
	withGeo.GeoLatitude, withGeo.GeoLongitude = "1", "2"
	tags := []string{"trip"}
	next, err = txnApplyPatch(lk, time.UTC, withGeo, txnPatchArgs{TagNames: &tags, ClearGeo: true})

	if err != nil || strings.Join(next.TagIds, ",") != "501" || next.GeoLatitude != "" {
		t.Fatalf("tags/geo: %+v %v", next, err)
	}

	// an empty patch changes nothing
	next, err = txnApplyPatch(lk, time.UTC, cur, txnPatchArgs{})

	if err != nil || !txnStatesEqual(cur, next) {
		t.Fatalf("empty patch: %+v %v", next, err)
	}
}

func TestTxnSessionIdAndDecorate(t *testing.T) {
	if txnSessionId("", 0, 1) != "" || txnSessionId("k", 0, 1) != "k" || txnSessionId("k", 2, 3) != "k#2" {
		t.Fatalf("session ids")
	}

	lk := txnTestLookup()
	row := map[string]any{"time": json.Number("1767268800"), "type": json.Number("4"), "sourceAccountId": "101", "destinationAccountId": "103"}
	txnDecorate(row, lk, time.UTC)

	if row["date"] != "2026-01-01" || row["typeName"] != "transfer" || row["sourceCurrency"] != "USD" || row["destinationCurrency"] != "JPY" {
		t.Fatalf("decorate: %v", row)
	}
}

func TestTxnDistinctCurrencies(t *testing.T) {
	lk := txnTestLookup()

	if got := strings.Join(txnDistinctCurrencies(lk, nil), ","); got != "EUR,JPY,USD" {
		t.Fatalf("all: %s", got)
	}

	if got := strings.Join(txnDistinctCurrencies(lk, []int64{101, 102, 200}), ","); got != "USD" {
		t.Fatalf("subset ignores parents: %s", got)
	}
}

func TestTxnReconcileHelpers(t *testing.T) {
	if ty, mag := txnAdjustmentType(1500); ty != models.TRANSACTION_TYPE_INCOME || mag != 1500 {
		t.Fatalf("positive difference is income")
	}

	if ty, mag := txnAdjustmentType(-99); ty != models.TRANSACTION_TYPE_EXPENSE || mag != 99 {
		t.Fatalf("negative difference is expense of the magnitude")
	}

	asOf := time.Date(2026, 8, 31, 0, 0, 0, 0, time.UTC)
	later := time.Date(2026, 9, 21, 10, 0, 0, 0, time.UTC)

	if got := txnReconcileAt(asOf, later, time.UTC); got != time.Date(2026, 8, 31, 23, 59, 59, 0, time.UTC).Unix() {
		t.Fatalf("past as_of is the end of that day: %d", got)
	}

	sameDay := time.Date(2026, 8, 31, 9, 0, 0, 0, time.UTC)

	if got := txnReconcileAt(asOf, sameDay, time.UTC); got != sameDay.Unix() {
		t.Fatalf("today is now, never the future: %d", got)
	}

	a, b := int64(5), int64(5)

	if !txnInt64PtrEqual(nil, nil) || !txnInt64PtrEqual(&a, &b) || txnInt64PtrEqual(&a, nil) {
		t.Fatalf("ptr equality")
	}
}

func TestTxnRoutesMountTogether(t *testing.T) {
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	group := engine.Group(BasePath)
	noop := func(c *gin.Context) {}

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("routes conflict in gin's tree: %v", r)
		}
	}()

	// the accounts family's routes share the /accounts/:id prefix with reconciliation
	group.Handle("GET", "/accounts", noop)
	group.Handle("GET", "/accounts/:id", noop)
	group.Handle("PATCH", "/accounts/:id", noop)
	group.Handle("DELETE", "/accounts/:id/sub-accounts/:sub_id", noop)

	seen := map[string]bool{}

	for _, r := range append(txnRoutes(), txnReconcileRoutes()...) {
		key := r.Method + " " + r.Path

		if seen[key] {
			t.Fatalf("duplicate route %s", key)
		}

		seen[key] = true

		if r.Handler == nil || r.Summary == "" {
			t.Fatalf("%s needs a handler and a summary", key)
		}

		if r.Method == "DELETE" && r.Tier != TierAdmin {
			t.Fatalf("%s: every delete is admin-tier", key)
		}

		if (r.Method == "POST" || r.Method == "PATCH") && r.Tier == TierWrite && !r.DryRunnable {
			t.Fatalf("%s: write routes must be dry-runnable", key)
		}

		group.Handle(r.Method, r.Path, noop)
	}

	for _, kind := range []string{txnOpRestore, txnOpDelete, txnOpRecreate, txnOpReconciledTime} {
		if _, ok := LookupInverse(kind); !ok {
			t.Fatalf("no executor registered for %s", kind)
		}
	}
}

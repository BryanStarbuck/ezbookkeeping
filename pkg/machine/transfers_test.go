package machine

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin/binding"
	"github.com/go-playground/validator/v10"

	"github.com/mayswind/ezbookkeeping/pkg/core"
	"github.com/mayswind/ezbookkeeping/pkg/datastore"
	"github.com/mayswind/ezbookkeeping/pkg/models"
	"github.com/mayswind/ezbookkeeping/pkg/services"
	"github.com/mayswind/ezbookkeeping/pkg/settings"
	"github.com/mayswind/ezbookkeeping/pkg/uuid"
	"github.com/mayswind/ezbookkeeping/pkg/validators"
)

// Synthetic books only (apis.mdx §20): invented institutions (Northbank, Meridian), invented last
// fours, invented amounts. Nothing here comes from anyone's real statements.

// ─── pure helpers ────────────────────────────────────────────────────────────────────────────

func TestXferLastFour(t *testing.T) {
	cases := map[string]string{
		"Northbank Checking ••4021": "4021",
		"Meridian Card x7788":       "7788",
		"Card ...1234":              "1234",
		"Card *5678":                "5678",
		"Savings 99994021":          "4021",
		"Brokerage (0042)":          "0042",
		"Wallet":                    "",
		"Card 123":                  "",
		"4021 Checking":             "",
	}

	for name, want := range cases {
		if got := xferLastFour(name); got != want {
			t.Errorf("xferLastFour(%q) = %q, want %q", name, got, want)
		}
	}
}

func TestXferCommentDigitsAndVocabulary(t *testing.T) {
	for _, c := range []string{"PAYMENT TO CARD x4021", "ONLINE XFER ...4021 REF 9", "*4021", "AUTOPAY ••4021", "#4021"} {
		if !xferCommentHasDigits(c, "4021") {
			t.Errorf("%q should carry 4021", c)
		}
	}

	for _, c := range []string{"REF 140215", "40210", "no digits", "14021"} {
		if xferCommentHasDigits(c, "4021") {
			t.Errorf("%q must not match 4021 inside a longer number", c)
		}
	}

	if xferCommentHasDigits("anything 4021", "") {
		t.Fatal("no last four never matches")
	}

	cases := map[string]string{
		"Meridian autopay":            "AUTOPAY",
		"AUTO  PAY Northbank":         "AUTO PAY",
		"payment - thank you":         "PAYMENT",
		"Thank You":                   "THANK YOU",
		"online pmt 22":               "ONLINE PMT",
		"deposit transfer from sav":   "DEPOSIT TRANSFER",
		"Transfer to chk":             "TRANSFER",
		"CRD PMT":                     "CRD",
		"Northbank ePay":              "EPAY",
		"GROCERY MART":                "",
		"TRANSFERWISE is one word":    "",
		"repayment of a friend's loan": "",
	}

	for c, want := range cases {
		if got := xferVocabularyTerm(c); got != want {
			t.Errorf("xferVocabularyTerm(%q) = %q, want %q", c, got, want)
		}
	}
}

func xferTestLookup() *txnLookup {
	accounts := []*models.Account{
		{AccountId: 11, Name: "Northbank Checking ••4021", Type: models.ACCOUNT_TYPE_SINGLE_ACCOUNT, Currency: "USD"},
		{AccountId: 12, Name: "Meridian Card ••7788", Type: models.ACCOUNT_TYPE_SINGLE_ACCOUNT, Currency: "USD"},
		{AccountId: 13, Name: "Northbank Savings ••5150", Type: models.ACCOUNT_TYPE_SINGLE_ACCOUNT, Currency: "USD"},
		{AccountId: 14, Name: "Euro Wallet", Type: models.ACCOUNT_TYPE_SINGLE_ACCOUNT, Currency: "EUR"},
		{AccountId: 15, Name: "Market Value", Type: models.ACCOUNT_TYPE_SINGLE_ACCOUNT, Currency: "USD", Hidden: true},
	}
	categories := []*models.TransactionCategory{
		{CategoryId: 10, Name: "Food", Type: models.CATEGORY_TYPE_EXPENSE},
		{CategoryId: 11, Name: "Groceries", ParentCategoryId: 10, Type: models.CATEGORY_TYPE_EXPENSE},
		{CategoryId: 30, Name: "Earnings", Type: models.CATEGORY_TYPE_INCOME},
		{CategoryId: 31, Name: "Salary", ParentCategoryId: 30, Type: models.CATEGORY_TYPE_INCOME},
		{CategoryId: 40, Name: "Moves", Type: models.CATEGORY_TYPE_TRANSFER, DisplayOrder: 2},
		{CategoryId: 41, Name: "Internal", ParentCategoryId: 40, Type: models.CATEGORY_TYPE_TRANSFER, DisplayOrder: 1},
		{CategoryId: 50, Name: "Banking", Type: models.CATEGORY_TYPE_TRANSFER, DisplayOrder: 1},
		{CategoryId: 51, Name: "Card Payments", ParentCategoryId: 50, Type: models.CATEGORY_TYPE_TRANSFER, DisplayOrder: 3},
		{CategoryId: 52, Name: "Hidden One", ParentCategoryId: 50, Type: models.CATEGORY_TYPE_TRANSFER, DisplayOrder: 0, Hidden: true},
	}

	return txnNewLookup(accounts, categories, nil)
}

func xferR(id int64, t models.TransactionType, acct int64, amount int64, date, comment string) *xferRow {
	return &xferRow{Id: id, Type: t, AccountId: acct, Amount: amount, Currency: map[int64]string{14: "EUR"}[acct] + map[bool]string{true: "USD"}[acct != 14], Date: date, Comment: comment}
}

func TestXferFindCandidates(t *testing.T) {
	lk := xferTestLookup()
	E, I := models.TRANSACTION_TYPE_EXPENSE, models.TRANSACTION_TYPE_INCOME

	expenses := []*xferRow{
		xferR(101, E, 11, 50000, "2026-03-01", "MERIDIAN CARD x7788"),   // pays the card: unambiguous with 201
		xferR(102, E, 11, 20000, "2026-03-05", "TO SAV"),                // two savings deposits of 200.00 → ambiguous
		xferR(103, E, 11, 20000, "2026-03-06", "TO SAV"),                //
		xferR(104, E, 11, 7000, "2026-03-10", "GROCERY MART"),           // no hint
		xferR(105, E, 11, 9900, "2026-03-01", "CARD PAYMENT"),           // counterpart 9 days later: outside the window
		xferR(106, E, 14, 50000, "2026-03-01", "EUR TRANSFER"),          // other currency
		xferR(107, E, 12, 3000, "2026-03-20", "REFUND TO CHECKING 4021"), // hinted by last four in the expense comment
	}
	incomes := []*xferRow{
		xferR(201, I, 12, 50000, "2026-03-03", "PAYMENT THANK YOU"),
		xferR(202, I, 13, 20000, "2026-03-05", "FROM CHK"),
		xferR(203, I, 13, 20000, "2026-03-06", "FROM CHK"),
		xferR(204, I, 13, 7000, "2026-03-11", "INTEREST"),
		xferR(205, I, 12, 9900, "2026-03-10", "PAYMENT"),
		xferR(206, I, 11, 3000, "2026-03-20", "DEPOSIT"),
		xferR(207, I, 11, 50000, "2026-03-01", "same account as 101 never pairs"),
	}

	res := xferFindCandidates(lk, expenses, incomes, 4, true, nil)

	if len(res.Pairs) != 2 {
		t.Fatalf("pairs = %d, want 2 (101↔201, 107↔206): %+v", len(res.Pairs), res.Pairs)
	}

	p := res.Pairs[0]

	if p.Expense.Id != 101 || p.Income.Id != 201 || p.DayGap != 2 {
		t.Fatalf("first pair = %d↔%d gap %d", p.Expense.Id, p.Income.Id, p.DayGap)
	}

	kinds := map[string]bool{}

	for _, h := range p.Hints {
		kinds[h.Kind+"/"+h.Comment] = true
	}

	if !kinds["last_four/expense"] || !kinds["vocabulary/income"] {
		t.Fatalf("hints of 101↔201 = %+v", p.Hints)
	}

	if q := res.Pairs[1]; q.Expense.Id != 107 || q.Income.Id != 206 || q.Hints[0].Digits != "4021" {
		t.Fatalf("second pair = %+v", q)
	}

	if len(res.Ambiguous) != 1 || len(res.Ambiguous[0]) != 4 {
		t.Fatalf("the two 200.00 deposits form one ambiguous group of 4 candidates: %+v", res.Ambiguous)
	}

	// deterministic
	again := xferFindCandidates(lk, expenses, incomes, 4, true, nil)

	if again.Pairs[0].Income.Id != 201 || again.Ambiguous[0][0].Expense.Id != res.Ambiguous[0][0].Expense.Id {
		t.Fatal("candidate order must be deterministic")
	}

	// hint off: the grocery row now pairs with the interest row (1 day apart)
	loose := xferFindCandidates(lk, expenses, incomes, 4, false, nil)
	found := false

	for _, p := range loose.Pairs {
		if p.Expense.Id == 104 && p.Income.Id == 204 && len(p.Hints) == 0 {
			found = true
		}
	}

	if !found {
		t.Fatal("require_hint false must admit an unhinted candidate")
	}

	// a wider window reaches the 9-day-later card payment
	wide := xferFindCandidates(lk, expenses, incomes, 10, true, nil)
	found = false

	for _, p := range wide.Pairs {
		if p.Expense.Id == 105 && p.Income.Id == 205 && p.DayGap == 9 {
			found = true
		}
	}

	if !found {
		t.Fatal("window_days 10 must find the 9-day pair")
	}

	// range: a pair whose both sides are outside the range is dropped
	ranged := xferFindCandidates(lk, expenses, incomes, 4, true, func(d string) bool { return d >= "2026-03-15" })

	if len(ranged.Pairs) != 1 || ranged.Pairs[0].Expense.Id != 107 || len(ranged.Ambiguous) != 0 {
		t.Fatalf("ranged = %+v", ranged.Pairs)
	}
}

func TestXferDefaultTransferCategory(t *testing.T) {
	lk := xferTestLookup()

	// Banking (display order 1) comes before Moves (2); its hidden child is skipped
	if c := xferDefaultTransferCategory(lk); c == nil || c.CategoryId != 51 {
		t.Fatalf("default = %+v, want 51", c)
	}

	if c, err := xferResolveTransferCategory(lk, "41", ""); err != nil || c.CategoryId != 41 {
		t.Fatalf("explicit id: %v %v", c, err)
	}

	if _, err := xferResolveTransferCategory(lk, "11", ""); txnFailCode(t, err) != CodeInvalidInput {
		t.Fatal("an expense category is not a transfer category")
	}

	none := txnNewLookup(lk.accounts, []*models.TransactionCategory{{CategoryId: 10, Name: "Food", Type: models.CATEGORY_TYPE_EXPENSE}}, nil)

	if _, err := xferResolveTransferCategory(none, "", ""); txnFailCode(t, err) != CodeNotFound || !strings.Contains(err.(*Fail).Hint, "transfer category") {
		t.Fatalf("no transfer category must fail naming the fix: %v", err)
	}
}

func xferState(id string, t models.TransactionType, acct string, amount int64, comment string, tags ...string) txnState {
	return txnState{Id: id, Type: int(t), CategoryId: "11", Time: 1772400000, UtcOffset: -480, SourceAccountId: acct, SourceAmount: amount, Comment: comment, TagIds: tags, PictureIds: []string{}}
}

func TestXferBuildPairAndSingle(t *testing.T) {
	lk := xferTestLookup()
	E, I := models.TRANSACTION_TYPE_EXPENSE, models.TRANSACTION_TYPE_INCOME

	exp := xferState("1", E, "11", 50000, "MERIDIAN CARD x7788", "501")
	inc := xferState("2", I, "12", 50000, "PAYMENT THANK YOU", "502", "501")
	inc.Time += 2 * 86400

	// the order of the two ids does not matter: the expense is the source
	tr, originals, _, err := xferBuildPair(lk, inc, exp, "41", "both")

	if err != nil {
		t.Fatal(err)
	}

	if tr.SourceAccountId != "11" || tr.DestinationAccountId != "12" || tr.SourceAmount != 50000 || tr.DestinationAmount != 50000 || tr.Time != exp.Time || tr.UtcOffset != exp.UtcOffset {
		t.Fatalf("transfer = %+v", tr)
	}

	if tr.Comment != "MERIDIAN CARD x7788 ⇄ PAYMENT THANK YOU" || len(tr.TagIds) != 2 || originals[0].Id != "1" {
		t.Fatalf("comment/tags/originals = %q %v %v", tr.Comment, tr.TagIds, originals[0].Id)
	}

	if rows, ok := xferBalanceRows(lk, originals, tr, ""); !ok || len(rows) != 2 {
		t.Fatalf("a pair conversion leaves every balance unchanged: %v", rows)
	}

	if tr, _, _, _ := xferBuildPair(lk, exp, inc, "41", "out"); tr.Comment != "MERIDIAN CARD x7788" {
		t.Fatalf("comment_mode out = %q", tr.Comment)
	}

	// validation failures
	bad := []struct {
		name string
		a, b txnState
	}{
		{"same account", exp, xferState("3", I, "11", 50000, "x")},
		{"both income", inc, xferState("3", I, "13", 50000, "x")},
		{"both expense", exp, xferState("3", E, "13", 50000, "x")},
		{"amount mismatch", exp, xferState("3", I, "12", 49999, "x")},
		{"a transfer", exp, xferState("3", models.TRANSACTION_TYPE_TRANSFER, "12", 50000, "x")},
		{"a balance modification", exp, xferState("3", models.TRANSACTION_TYPE_MODIFY_BALANCE, "12", 50000, "x")},
	}

	for _, c := range bad {
		if _, _, _, err := xferBuildPair(lk, c.a, c.b, "41", "both"); txnFailCode(t, err) != CodeInvalidInput {
			t.Errorf("%s must be refused, got %v", c.name, err)
		}
	}

	// cross-currency pair: each side keeps its own amount
	eur := xferState("4", I, "14", 46000, "EUR IN")

	if tr, _, _, err := xferBuildPair(lk, exp, eur, "41", "both"); err != nil || tr.SourceAmount != 50000 || tr.DestinationAmount != 46000 {
		t.Fatalf("cross-currency pair: %+v %v", tr, err)
	}

	// single, both directions
	counter := lk.accountMap[15]
	out, err := xferBuildSingle(lk, exp, counter, "41")

	if err != nil || out.SourceAccountId != "11" || out.DestinationAccountId != "15" || out.Comment != exp.Comment || out.Time != exp.Time {
		t.Fatalf("expense single: %+v %v", out, err)
	}

	in, err := xferBuildSingle(lk, inc, counter, "41")

	if err != nil || in.SourceAccountId != "15" || in.DestinationAccountId != "12" || in.SourceAmount != 50000 || in.DestinationAmount != 50000 {
		t.Fatalf("income single: %+v %v", in, err)
	}

	for _, c := range []struct {
		name    string
		s       txnState
		counter *models.Account
	}{
		{"same account", exp, lk.accountMap[11]},
		{"other currency", exp, lk.accountMap[14]},
		{"a transfer", xferState("5", models.TRANSACTION_TYPE_TRANSFER, "11", 1, "x"), counter},
	} {
		if _, err := xferBuildSingle(lk, c.s, c.counter, "41"); txnFailCode(t, err) != CodeInvalidInput {
			t.Errorf("single %s must be refused, got %v", c.name, err)
		}
	}

	if got, cut := xferTruncateRunes(strings.Repeat("é", 300), 255); !cut || len([]rune(got)) != 255 {
		t.Fatalf("truncate = %d runes", len([]rune(got)))
	}
}

// ─── database-backed: the routes end to end through upstream's own handlers ─────────────────

// xferBooks is a temp SQLite with the full upstream schema, a bound user (uid 42, "operator"),
// synthetic accounts and categories. It restores the previous stores afterwards.
type xferBooks struct {
	t        *testing.T
	checking int64 // Northbank Checking ••4021
	card     int64 // Meridian Card ••7788
	savings  int64 // Northbank Savings ••5150
	brokers  int64 // Meridian Brokerage
	market   int64 // Market Value (hidden)
	groceries,
	salary,
	transfer int64
	// importIds maps a statement account key to the import id the plan engine gave its row
	importIds map[string]string
}

// xferValidators registers upstream's custom binding validators, as cmd/webserver.go does at boot
var xferValidators sync.Once

func xferOpenBooks(t *testing.T) *xferBooks {
	t.Helper()
	xferValidators.Do(func() {
		if v, ok := binding.Validator.Engine().(*validator.Validate); ok {
			for name, fn := range map[string]validator.Func{
				"notBlank": validators.NotBlank, "validUsername": validators.ValidUsername, "validEmail": validators.ValidEmail,
				"validNickname": validators.ValidNickname, "validCurrency": validators.ValidCurrency, "validHexRGBColor": validators.ValidHexRGBColor,
				"validAmountFilter": validators.ValidAmountFilter, "validTransactionAmount": validators.ValidTransactionAmount,
				"validTagFilter": validators.ValidTagFilter, "validFiscalYearStart": validators.ValidateFiscalYearStart,
			} {
				if err := v.RegisterValidation(name, fn); err != nil {
					t.Fatal(err)
				}
			}
		}
	})
	dir := jrIsolate(t)
	prevUser, prevToken, prevData := datastore.Container.UserStore, datastore.Container.TokenStore, datastore.Container.UserDataStore
	prevConfig := settings.Container.GetCurrentConfig()

	cfg := &settings.Config{
		DatabaseConfig: &settings.DatabaseConfig{
			DatabaseType: settings.Sqlite3DbType, DatabasePath: filepath.Join(dir, "xfer.db"),
			MaxIdleConnection: 1, MaxOpenConnection: 1, ConnectionMaxLifeTime: 60,
		},
		UuidGeneratorType: settings.InternalUuidGeneratorType,
	}

	if err := datastore.InitializeDataStore(cfg); err != nil {
		t.Skipf("sqlite unavailable: %v", err)
	}

	t.Cleanup(func() {
		datastore.Container.UserStore, datastore.Container.TokenStore, datastore.Container.UserDataStore = prevUser, prevToken, prevData
		settings.SetCurrentConfig(prevConfig)
	})

	settings.SetCurrentConfig(cfg)

	if err := uuid.InitializeUuidGenerator(cfg); err != nil {
		t.Fatal(err)
	}

	if err := datastore.Container.UserStore.SyncStructs(new(models.User)); err != nil {
		t.Fatal(err)
	}

	for _, bean := range []any{new(models.Account), new(models.Transaction), new(models.TransactionCategory), new(models.TransactionTagGroup), new(models.TransactionTag), new(models.TransactionTagIndex), new(models.TransactionPictureInfo)} {
		if err := datastore.Container.UserDataStore.SyncStructs(bean); err != nil {
			t.Fatal(err)
		}
	}

	if err := datastore.Container.UserDataStore.SyncStructs(machineTables...); err != nil {
		t.Fatal(err)
	}

	now := time.Now().Unix()
	user := &models.User{Uid: 42, Username: "operator", Email: "operator@example.invalid", Nickname: "operator", Password: "x", Salt: "x", DefaultCurrency: "USD", TransactionEditScope: models.TRANSACTION_EDIT_SCOPE_ALL, EmailVerified: true, CreatedUnixTime: now, UpdatedUnixTime: now}

	if _, err := datastore.Container.UserStore.Choose(42).NewSession(core.NewNullContext()).Insert(user); err != nil {
		t.Fatal(err)
	}

	b := &xferBooks{t: t, importIds: map[string]string{}}
	c := core.NewNullContext()

	account := func(name, currency string, hidden bool) int64 {
		a := &models.Account{Uid: 42, Name: name, Type: models.ACCOUNT_TYPE_SINGLE_ACCOUNT, Category: models.ACCOUNT_CATEGORY_CHECKING_ACCOUNT, Currency: currency, Icon: 1, Color: "000000"}

		if err := services.Accounts.CreateAccounts(c, a, 0, nil, nil, time.UTC); err != nil {
			t.Fatalf("create account %s: %v", name, err)
		}

		if hidden {
			if err := services.Accounts.HideAccount(c, 42, []int64{a.AccountId}, true); err != nil {
				t.Fatal(err)
			}
		}

		return a.AccountId
	}

	b.checking = account("Northbank Checking ••4021", "USD", false)
	b.card = account("Meridian Card ••7788", "USD", false)
	b.savings = account("Northbank Savings ••5150", "USD", false)
	b.brokers = account("Meridian Brokerage", "USD", false)
	b.market = account("Market Value", "USD", true)

	category := func(name string, ctype models.TransactionCategoryType, parent int64) int64 {
		cat := &models.TransactionCategory{Uid: 42, Name: name, Type: ctype, ParentCategoryId: parent, Icon: 1, Color: "000000"}

		if err := services.TransactionCategories.CreateCategory(c, cat); err != nil {
			t.Fatalf("create category %s: %v", name, err)
		}

		return cat.CategoryId
	}

	b.groceries = category("Groceries", models.CATEGORY_TYPE_EXPENSE, category("Food", models.CATEGORY_TYPE_EXPENSE, 0))
	b.salary = category("Salary", models.CATEGORY_TYPE_INCOME, category("Earnings", models.CATEGORY_TYPE_INCOME, 0))

	return b
}

func (b *xferBooks) addTransferCategory() {
	c := core.NewNullContext()
	parent := &models.TransactionCategory{Uid: 42, Name: "Moves", Type: models.CATEGORY_TYPE_TRANSFER, Icon: 1, Color: "000000"}

	if err := services.TransactionCategories.CreateCategory(c, parent); err != nil {
		b.t.Fatal(err)
	}

	child := &models.TransactionCategory{Uid: 42, Name: "Internal", Type: models.CATEGORY_TYPE_TRANSFER, ParentCategoryId: parent.CategoryId, Icon: 1, Color: "000000"}

	if err := services.TransactionCategories.CreateCategory(c, child); err != nil {
		b.t.Fatal(err)
	}

	b.transfer = child.CategoryId
}

func (b *xferBooks) ctx(route RouteDef, body string) *Ctx {
	var data []byte

	if body != "" {
		data = []byte(body)
	}

	return jrTestCtx(b.t, &route, route.Method, BasePath+route.Path, data, nil)
}

// row creates an income or expense row at noon (Los Angeles) of date, through upstream
func (b *xferBooks) row(t models.TransactionType, acct, amount int64, date, comment string) int64 {
	b.t.Helper()
	loc, _ := time.LoadLocation("America/Los_Angeles")
	d, _ := time.ParseInLocation("2006-01-02", date, loc)
	at := time.Date(d.Year(), d.Month(), d.Day(), 12, 0, 0, 0, loc)
	cat := b.groceries

	if t == models.TRANSACTION_TYPE_INCOME {
		cat = b.salary
	}

	s := txnState{Type: int(t), CategoryId: idString(cat), Time: at.Unix(), UtcOffset: UTCOffsetMinutes(at, loc), SourceAccountId: idString(acct), SourceAmount: amount, Comment: comment, TagIds: []string{}}

	id, err := xferCreate(b.ctx(RouteDef{Method: "POST", Path: "/transactions", Tier: TierWrite}, ""), s)

	if err != nil {
		b.t.Fatalf("create row: %v", err)
	}

	return id
}

func (b *xferBooks) record(importId string, txnId int64) {
	rec := &MachineImportRecord{Uid: 42, ImportId: importId, TransactionId: txnId, AccountKey: "synthetic", RunId: "run_test", CreatedUnixTime: time.Now().Unix()}

	if _, err := datastore.Container.UserDataStore.Choose(42).NewSession(core.NewNullContext()).Insert(rec); err != nil {
		b.t.Fatal(err)
	}
}

func (b *xferBooks) recordTarget(importId string) int64 {
	recs, err := ingLoadRecords(b.ctx(RouteDef{Method: "GET", Path: "/x"}, ""), []string{importId})

	if err != nil || recs[importId] == nil {
		b.t.Fatalf("record %s: %v", importId, err)
	}

	return recs[importId].TransactionId
}

// alreadyPresent is the plan's own already_present test (ingest_plan.go): the record's
// transaction exists and is not deleted
func (b *xferBooks) alreadyPresent(importId string) bool {
	id := b.recordTarget(importId)
	st, err := ingLoadTxnStates(b.ctx(RouteDef{Method: "GET", Path: "/x"}, ""), []int64{id})

	if err != nil {
		b.t.Fatal(err)
	}

	return st[id] != nil && st[id].Exists && !st[id].Deleted
}

// replan runs the statement plan engine over one synthetic statement row per account and returns
// each row's status and the plan totals — the same evaluation POST /ingest/plan makes
func (b *xferBooks) replan(rows map[int64]*ingRow) (map[string]string, map[string]int) {
	b.t.Helper()
	var inputs []*ingAccountInput

	for acct, r := range rows {
		cp := *r
		inputs = append(inputs, &ingAccountInput{AccountKey: cp.AccountKey, Currency: "USD", AccountId: acct, Statements: []*ingStatement{{File: cp.SourceFile, FileType: "csv", Rows: []*ingRow{&cp}, First: cp.Date, Last: cp.Date}}})
	}

	res, err := ingEvaluate(b.ctx(RouteDef{Method: "POST", Path: "/ingest/plan"}, ""), inputs, ingNewMap(), ingEngineOpts{Lenient: true})

	if err != nil {
		b.t.Fatalf("plan: %v", err)
	}

	status := map[string]string{}

	for _, r := range res.allRows {
		status[r.ImportId] = r.Status
		b.importIds[r.AccountKey] = r.ImportId
	}

	return status, res.Totals
}

func (b *xferBooks) balances() map[int64]int64 {
	accounts, err := services.Accounts.GetAllAccountsByUid(core.NewNullContext(), 42)

	if err != nil {
		b.t.Fatal(err)
	}

	out := map[int64]int64{}

	for _, a := range accounts {
		out[a.AccountId] = a.Balance
	}

	return out
}

func (b *xferBooks) state(id int64) *txnState {
	st, _, _, err := txnReadStates(b.ctx(RouteDef{Method: "GET", Path: "/x"}, ""), []int64{id})

	if err != nil {
		b.t.Fatal(err)
	}

	return st[id]
}

func xferRoute(path string) RouteDef {
	for _, r := range xferRoutes() {
		if r.Path == path {
			return r
		}
	}

	panic(path)
}

// convert runs the dry run then the apply; it returns the apply's result and the dry run's preview
func (b *xferBooks) convert(body map[string]any) (map[string]any, map[string]any, error) {
	b.t.Helper()
	route := xferRoute("/transactions/convert-to-transfer")
	raw, _ := json.Marshal(body)
	res, err := xferHandleConvert(b.ctx(route, string(raw)))

	if err != nil {
		return nil, nil, err
	}

	dry := res.(*WriteResult)

	if !dry.DryRun || dry.ConfirmToken == "" {
		b.t.Fatalf("the first call must be a dry run with a token: %+v", dry)
	}

	preview, _ := Integerize(dry.Preview)
	body["dry_run"] = false
	body["confirm_token"] = dry.ConfirmToken
	raw, _ = json.Marshal(body)
	res, err = xferHandleConvert(b.ctx(route, string(raw)))

	if err != nil {
		return nil, preview.(map[string]any), err
	}

	out, _ := Integerize(res.(*WriteResult).Result)

	return out.(map[string]any), preview.(map[string]any), nil
}

func (b *xferBooks) undo() {
	b.t.Helper()

	if _, err := jrCallJournalRoute(b.t, jrHandleUndo, "POST", "/undo", "{}", 42); err != nil {
		b.t.Fatalf("undo: %v", err)
	}
}

func xferSameBalances(t *testing.T, before, after map[int64]int64, when string) {
	t.Helper()

	for id, v := range before {
		if after[id] != v {
			t.Fatalf("%s: account %d balance %d → %d", when, id, v, after[id])
		}
	}
}

func TestXferConvertPairEndToEnd(t *testing.T) {
	b := xferOpenBooks(t)

	// the two statement lines, as the plan engine sees them; its first plan names their import ids
	stmt := map[int64]*ingRow{
		b.checking: {AccountKey: "northbank_checking_4021", Date: "2026-03-02", Month: "2026-03", Amount: -50000, Currency: "USD", Description: "MERIDIAN CARD AUTOPAY x7788", NormDesc: ingNormDesc("MERIDIAN CARD AUTOPAY x7788"), SourceFile: "checking_2026-03.csv"},
		b.card:     {AccountKey: "meridian_card_7788", Date: "2026-03-04", Month: "2026-03", Amount: 50000, Currency: "USD", Description: "PAYMENT THANK YOU", NormDesc: ingNormDesc("PAYMENT THANK YOU"), SourceFile: "card_2026-03.csv"},
	}
	status, totals := b.replan(stmt)

	if totals["new"] != 2 || len(status) != 2 {
		t.Fatalf("first plan: %v %v", status, totals)
	}

	impOut, impIn := b.importIds["northbank_checking_4021"], b.importIds["meridian_card_7788"]

	exp := b.row(models.TRANSACTION_TYPE_EXPENSE, b.checking, 50000, "2026-03-02", "MERIDIAN CARD AUTOPAY x7788")
	inc := b.row(models.TRANSACTION_TYPE_INCOME, b.card, 50000, "2026-03-04", "PAYMENT THANK YOU")
	b.record(impOut, exp)
	b.record(impIn, inc)
	start := b.balances()

	if status, totals := b.replan(stmt); totals["already_present"] != 2 || totals["new"] != 0 {
		t.Fatalf("after the import: %v %v", status, totals)
	}

	// no transfer category yet: a clear, actionable failure
	_, _, err := b.convert(map[string]any{"items": []map[string]any{{"id": idString(exp), "counter_id": idString(inc)}}})

	if txnFailCode(t, err) != CodeNotFound || !strings.Contains(err.(*Fail).Hint, "transfer category") {
		t.Fatalf("missing transfer category: %v", err)
	}

	b.addTransferCategory()

	// the candidate finder names exactly this pair
	cr, err := xferHandleCandidates(b.ctx(xferRoute("/transactions/transfer-candidates"), `{"start":"2026-03-01","end":"2026-03-31"}`))

	if err != nil {
		t.Fatal(err)
	}

	cands, _ := Integerize(cr)
	pairs := cands.(map[string]any)["pairs"].([]any)

	if len(pairs) != 1 {
		t.Fatalf("candidates = %v", cands)
	}

	item := pairs[0].(map[string]any)["item"].(map[string]any)

	if item["id"] != idString(exp) || item["counter_id"] != idString(inc) {
		t.Fatalf("candidate item = %v", item)
	}

	// convert, feeding the candidate's item straight in
	res, preview, err := b.convert(map[string]any{"items": []any{item}})

	if err != nil {
		t.Fatal(err)
	}

	pi := preview["items"].([]any)[0].(map[string]any)

	if fmt.Sprint(pi["importRecords"]) != "2" || fmt.Sprint(pi["dayGap"]) != "2" || pi["kind"] != "pair" {
		t.Fatalf("preview item = %v", pi)
	}

	for _, row := range pi["balanceEffect"].([]any) {
		if fmt.Sprint(row.(map[string]any)["netChange"]) != "0" {
			t.Fatalf("balance effect must be zero: %v", row)
		}
	}

	tid := xferAtoi(res["ids"].([]any)[0].(string))
	tr := b.state(tid)

	if tr == nil || tr.Type != int(models.TRANSACTION_TYPE_TRANSFER) || tr.SourceAccountId != idString(b.checking) || tr.DestinationAccountId != idString(b.card) || tr.SourceAmount != 50000 || tr.DestinationAmount != 50000 {
		t.Fatalf("transfer = %+v", tr)
	}

	if tr.Comment != "MERIDIAN CARD AUTOPAY x7788 ⇄ PAYMENT THANK YOU" || tr.CategoryId != idString(b.transfer) {
		t.Fatalf("transfer comment/category = %q %s", tr.Comment, tr.CategoryId)
	}

	if b.state(exp) != nil || b.state(inc) != nil {
		t.Fatal("the originals must be deleted")
	}

	xferSameBalances(t, start, b.balances(), "after the conversion")

	if b.recordTarget(impOut) != tid || b.recordTarget(impIn) != tid || !b.alreadyPresent(impOut) || !b.alreadyPresent(impIn) {
		t.Fatal("both import records must point at the transfer, so a re-plan reports them already_present")
	}

	if status, totals := b.replan(stmt); totals["already_present"] != 2 || totals["new"] != 0 || totals["already_present_deleted"] != 0 {
		t.Fatalf("a re-plan after the conversion must stay at new 0: %v %v", status, totals)
	}

	// converting the same rows again: they are gone
	if _, _, err := b.convert(map[string]any{"items": []any{item}}); txnFailCode(t, err) != CodeNotFound {
		t.Fatalf("a second conversion of deleted rows must be not_found: %v", err)
	}

	// the transfer itself cannot be converted
	if _, _, err := b.convert(map[string]any{"items": []map[string]any{{"id": idString(tid), "counter_account_id": idString(b.savings)}}}); txnFailCode(t, err) != CodeInvalidInput {
		t.Fatalf("a transfer is refused: %v", err)
	}

	// undo: the originals come back (new ids), their records follow, the transfer goes
	b.undo()

	if b.state(tid) != nil {
		t.Fatal("undo must delete the transfer")
	}

	newOut, newIn := b.recordTarget(impOut), b.recordTarget(impIn)

	if newOut == tid || newIn == tid || newOut == newIn || !b.alreadyPresent(impOut) || !b.alreadyPresent(impIn) {
		t.Fatalf("undo must re-point the records at the re-created rows: %d %d", newOut, newIn)
	}

	if status, totals := b.replan(stmt); totals["already_present"] != 2 || totals["new"] != 0 {
		t.Fatalf("a re-plan after the undo must stay at new 0: %v %v", status, totals)
	}

	so, si := b.state(newOut), b.state(newIn)

	if so.Type != int(models.TRANSACTION_TYPE_EXPENSE) || so.SourceAccountId != idString(b.checking) || so.Comment != "MERIDIAN CARD AUTOPAY x7788" || si.Type != int(models.TRANSACTION_TYPE_INCOME) || si.SourceAccountId != idString(b.card) || si.SourceAmount != 50000 {
		t.Fatalf("restored rows = %+v %+v", so, si)
	}

	xferSameBalances(t, start, b.balances(), "after the undo")
}

func TestXferConvertSingleBothDirectionsWithHiddenCounter(t *testing.T) {
	b := xferOpenBooks(t)
	b.addTransferCategory()

	gain := b.row(models.TRANSACTION_TYPE_INCOME, b.brokers, 123400, "2026-04-30", "MARKET VALUE CHANGE")
	loss := b.row(models.TRANSACTION_TYPE_EXPENSE, b.brokers, 45600, "2026-05-31", "MARKET VALUE CHANGE")
	b.record("imp_gain", gain)
	start := b.balances()

	res, preview, err := b.convert(map[string]any{
		"items":                  []map[string]any{{"id": idString(gain), "counter_account_name": "Market Value"}, {"id": idString(loss), "counter_account_id": idString(b.market)}},
		"transfer_category_name": "Internal",
	})

	if err != nil {
		t.Fatalf("%v %+v", err, err.(*Fail).Details)
	}

	if hidden := preview["hiddenAccounts"].([]any); len(hidden) != 1 {
		t.Fatalf("the preview names the hidden counter account: %v", preview["hiddenAccounts"])
	}

	ids := res["ids"].([]any)
	in, out := b.state(xferAtoi(ids[0].(string))), b.state(xferAtoi(ids[1].(string)))

	// income into the brokerage: the counter account is the source
	if in.SourceAccountId != idString(b.market) || in.DestinationAccountId != idString(b.brokers) || in.SourceAmount != 123400 || in.Comment != "MARKET VALUE CHANGE" {
		t.Fatalf("income single = %+v", in)
	}

	// expense out of the brokerage: the counter account is the destination
	if out.SourceAccountId != idString(b.brokers) || out.DestinationAccountId != idString(b.market) || out.SourceAmount != 45600 {
		t.Fatalf("expense single = %+v", out)
	}

	after := b.balances()

	if after[b.brokers] != start[b.brokers] {
		t.Fatalf("the brokerage balance must not move: %d → %d", start[b.brokers], after[b.brokers])
	}

	if after[b.market] != -123400+45600 {
		t.Fatalf("the counter account carries the moves: %d", after[b.market])
	}

	accts, _ := services.Accounts.GetAccountsByAccountIds(core.NewNullContext(), 42, []int64{b.market})

	if !accts[b.market].Hidden {
		t.Fatal("the counter account must be hidden again after the conversion")
	}

	if b.recordTarget("imp_gain") != xferAtoi(ids[0].(string)) {
		t.Fatal("the gain's import record must follow it")
	}

	b.undo()

	if b.state(xferAtoi(ids[0].(string))) != nil || b.state(xferAtoi(ids[1].(string))) != nil {
		t.Fatal("undo removes both transfers")
	}

	xferSameBalances(t, start, b.balances(), "after the undo")

	accts, _ = services.Accounts.GetAccountsByAccountIds(core.NewNullContext(), 42, []int64{b.market})

	if !accts[b.market].Hidden {
		t.Fatal("the counter account stays hidden after the undo")
	}

	if !b.alreadyPresent("imp_gain") {
		t.Fatal("the gain's record points at its re-created row")
	}
}

func TestXferConvertValidation(t *testing.T) {
	b := xferOpenBooks(t)
	b.addTransferCategory()
	E, I := models.TRANSACTION_TYPE_EXPENSE, models.TRANSACTION_TYPE_INCOME

	e1 := b.row(E, b.checking, 10000, "2026-06-01", "TO SAV")
	i1 := b.row(I, b.savings, 10000, "2026-06-01", "FROM CHK")
	i2 := b.row(I, b.checking, 10000, "2026-06-01", "DEPOSIT")
	i3 := b.row(I, b.savings, 9999, "2026-06-01", "FROM CHK")
	e2 := b.row(E, b.card, 10000, "2026-06-01", "x")

	cases := []struct {
		name  string
		items []map[string]any
		code  string
	}{
		{"same account", []map[string]any{{"id": idString(e1), "counter_id": idString(i2)}}, CodeInvalidInput},
		{"both income", []map[string]any{{"id": idString(i1), "counter_id": idString(i2)}}, CodeInvalidInput},
		{"both expense", []map[string]any{{"id": idString(e1), "counter_id": idString(e2)}}, CodeInvalidInput},
		{"amount mismatch", []map[string]any{{"id": idString(e1), "counter_id": idString(i3)}}, CodeInvalidInput},
		{"duplicate ids", []map[string]any{{"id": idString(e1), "counter_id": idString(i1)}, {"id": idString(e1), "counter_account_id": idString(b.market)}}, CodeInvalidInput},
		{"an id as its own counter", []map[string]any{{"id": idString(e1), "counter_id": idString(e1)}}, CodeInvalidInput},
		{"missing row", []map[string]any{{"id": "999999999999999", "counter_id": idString(i1)}}, CodeNotFound},
		{"counter_id and counter_account", []map[string]any{{"id": idString(e1), "counter_id": idString(i1), "counter_account_id": idString(b.market)}}, CodeInvalidInput},
		{"neither", []map[string]any{{"id": idString(e1)}}, CodeInvalidInput},
		{"single same account", []map[string]any{{"id": idString(e1), "counter_account_id": idString(b.checking)}}, CodeInvalidInput},
	}

	for _, c := range cases {
		_, _, err := b.convert(map[string]any{"items": c.items})

		if got := txnFailCode(t, err); got != c.code {
			t.Errorf("%s: code %q (%v), want %q", c.name, got, err, c.code)
		}
	}

	if _, _, err := b.convert(map[string]any{"items": []map[string]any{{"id": idString(e1), "counter_id": idString(i1)}}, "comment_mode": "neither"}); txnFailCode(t, err) != CodeInvalidInput {
		t.Fatal("an unknown comment_mode is refused")
	}

	// problems are reported together, each with its index
	_, _, err := b.convert(map[string]any{"items": []map[string]any{{"id": idString(e1), "counter_id": idString(i2)}, {"id": idString(e2), "counter_id": idString(i3)}}})

	if f, ok := err.(*Fail); !ok || len(f.Details.(map[string]any)["problems"].([]xferProblem)) != 2 {
		t.Fatalf("both problems must be listed: %v", err)
	}

	// nothing was touched by any refusal
	for _, id := range []int64{e1, i1, i2, i3, e2} {
		if b.state(id) == nil {
			t.Fatalf("a refused conversion deleted %d", id)
		}
	}

	// the max_changes ceiling counts the originals
	route := xferRoute("/transactions/convert-to-transfer")
	raw, _ := json.Marshal(map[string]any{"items": []map[string]any{{"id": idString(e1), "counter_id": idString(i1)}}, "max_changes": 1})

	if _, err := xferHandleConvert(b.ctx(route, string(raw))); txnFailCode(t, err) != CodeConflict {
		t.Fatalf("a pair is two changes, over a ceiling of 1: %v", err)
	}
}

func TestXferRoutesRegistered(t *testing.T) {
	want := map[string]Tier{"POST /transactions/transfer-candidates": TierRead, "POST /transactions/convert-to-transfer": TierWrite}
	got := map[string]Tier{}

	for _, r := range Routes() {
		if _, ok := want[r.Method+" "+r.Path]; ok {
			got[r.Method+" "+r.Path] = r.Tier
		}
	}

	for k, tier := range want {
		if got[k] != tier {
			t.Fatalf("%s tier %v, want %v (registered: %v)", k, got[k], tier, got)
		}
	}

	if _, ok := LookupInverse(xferOpUnconvert); !ok {
		t.Fatal("txn.unconvert must be registered for undo")
	}
}

// The MCP reaches exactly one admin route — DELETE /transactions/bulk — and only with the admin
// tier on; every other admin route still refuses it (apis.mdx §8.1).
func TestGatesMcpReachesOnlyTheBulkDelete(t *testing.T) {
	jrIsolate(t)
	var reached []string
	h := func(name string) HandlerFunc {
		return func(mc *Ctx) (any, error) {
			reached = append(reached, name)
			return map[string]any{"ok": name}, nil
		}
	}
	routes := []RouteDef{
		{Method: "DELETE", Path: "/transactions/bulk", Tier: TierAdmin, NoUser: true, Handler: h("bulk")},
		{Method: "DELETE", Path: "/transactions/:id", Tier: TierAdmin, NoUser: true, Handler: h("one")},
	}
	mcp := map[string]string{HeaderClient: "ezbookkeeping-mcp/0.1.0"}

	jrArm(t, true, false)
	r := jrGateEngine(t, routes...)

	if w := jrDo(r, jrReq{method: "DELETE", path: BasePath + "/transactions/bulk", headers: mcp}); jrErrCode(t, w) != CodeForbidden {
		t.Errorf("the bulk delete with the admin tier off: %s", w.Body)
	}

	jrArm(t, true, true)
	r = jrGateEngine(t, routes...)

	if w := jrDo(r, jrReq{method: "DELETE", path: BasePath + "/transactions/bulk", headers: mcp}); w.Code != 200 {
		t.Errorf("the bulk delete from the MCP with admin on: %d %s", w.Code, w.Body)
	}

	if w := jrDo(r, jrReq{method: "DELETE", path: BasePath + "/transactions/123", headers: mcp}); jrErrCode(t, w) != CodeForbidden {
		t.Errorf("any other admin route from the MCP: %s", w.Body)
	}

	if strings.Join(reached, ",") != "bulk" {
		t.Errorf("handlers reached = %v", reached)
	}
}

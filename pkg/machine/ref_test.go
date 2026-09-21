package machine

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/mayswind/ezbookkeeping/pkg/core"
	"github.com/mayswind/ezbookkeeping/pkg/models"
)

// Tests for the reference families (routes_accounts.go, routes_reference.go, routes_currency.go,
// routes_user.go). Everything here is pure logic over synthetic rows — no database, no home
// directory (the env seams point at t.TempDir() regardless).

func refTestEnv(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("EZBK_CREDENTIALS_FILE", dir+"/ezbookkeeping.json")
	t.Setenv("EZBK_STATE_DIR", dir)
}

func refFail(t *testing.T, err error) *Fail {
	t.Helper()

	if err == nil {
		t.Fatalf("expected an error, got nil")
	}

	f, ok := err.(*Fail)

	if !ok {
		t.Fatalf("expected *Fail, got %T %v", err, err)
	}

	if f.Hint == "" {
		t.Fatalf("every error names a fix (R6); %q has no hint", f.Message)
	}

	return f
}

// --- name resolution

type refTestItem struct {
	id   int64
	name string
}

func refTestResolve(items []refTestItem, ref string) (refTestItem, error) {
	return refResolveOne("thing", ref, items, func(i refTestItem) int64 { return i.id }, func(i refTestItem) string { return i.name }, nil, "list them")
}

func TestRefMatchByNameExactBeatsCaseInsensitive(t *testing.T) {
	items := []refTestItem{{1, "Groceries"}, {2, "groceries"}, {3, "GROCERIES"}}

	got := refMatchByName(items, "groceries", func(i refTestItem) string { return i.name })

	if len(got) != 1 || got[0].id != 2 {
		t.Fatalf("exact match must win alone, got %+v", got)
	}

	got = refMatchByName(items, "GrOcErIeS", func(i refTestItem) string { return i.name })

	if len(got) != 3 {
		t.Fatalf("with no exact match every case-insensitive match is returned, got %+v", got)
	}
}

func TestRefResolveOne(t *testing.T) {
	items := []refTestItem{{3401855937219633152, "Northbank Checking"}, {3401855937219633153, "Savings"}, {3401855937219633154, "savings"}, {42, "2024"}}

	if it, err := refTestResolve(items, "3401855937219633152"); err != nil || it.name != "Northbank Checking" {
		t.Fatalf("id above 2^53 must resolve exactly: %+v %v", it, err)
	}

	if it, err := refTestResolve(items, "northbank checking"); err != nil || it.id != 3401855937219633152 {
		t.Fatalf("case-insensitive name: %+v %v", it, err)
	}

	if it, err := refTestResolve(items, "Savings"); err != nil || it.id != 3401855937219633153 {
		t.Fatalf("exact name must beat its case variant: %+v %v", it, err)
	}

	f := refFail(t, func() error { _, err := refTestResolve(items, "SAVINGS"); return err }())

	if f.Code != CodeInvalidInput {
		t.Fatalf("ambiguity is invalid_input, got %s", f.Code)
	}

	details, _ := f.Details.(map[string]any)

	if c, ok := details["candidates"].([]refCandidate); !ok || len(c) != 2 {
		t.Fatalf("ambiguity carries the candidates, got %+v", f.Details)
	}

	if f := refFail(t, func() error { _, err := refTestResolve(items, "99999999999"); return err }()); f.Code != CodeNotFound {
		t.Fatalf("an unknown long id is not_found, got %s", f.Code)
	}

	if it, err := refTestResolve(items, "2024"); err != nil || it.id != 42 {
		t.Fatalf("a short all-digit name is a name: %+v %v", it, err)
	}

	if it, err := refTestResolve(items, "name:2024"); err != nil || it.id != 42 {
		t.Fatalf("name: prefix forces a name: %+v %v", it, err)
	}

	if f := refFail(t, func() error { _, err := refTestResolve(items, "id:2024"); return err }()); f.Code != CodeNotFound {
		t.Fatalf("id: prefix forces an id, got %s", f.Code)
	}

	if f := refFail(t, func() error { _, err := refTestResolve(items, "nope"); return err }()); f.Code != CodeNotFound {
		t.Fatalf("unknown name is not_found, got %s", f.Code)
	}
}

func TestRefRefArg(t *testing.T) {
	if r, err := refRefArg("account", "123456789", ""); err != nil || r != "id:123456789" {
		t.Fatalf("id arg: %q %v", r, err)
	}

	if r, err := refRefArg("account", "", "Checking"); err != nil || r != "name:Checking" {
		t.Fatalf("name arg: %q %v", r, err)
	}

	refFail(t, func() error { _, err := refRefArg("account", "1", "x"); return err }())
	refFail(t, func() error { _, err := refRefArg("account", "abc", ""); return err }())

	if r, err := refRefArg("account", "", ""); err != nil || r != "" {
		t.Fatalf("neither: %q %v", r, err)
	}
}

// --- display-order moves

func TestRefComputeMove(t *testing.T) {
	sibs := []refSibling{{Id: 10, Name: "a", Order: 1}, {Id: 20, Name: "b", Order: 2}, {Id: 30, Name: "c", Order: 3}, {Id: 40, Name: "d", Order: 4}}

	changes, err := refComputeMove(sibs, 40, 0)

	if err != nil {
		t.Fatal(err)
	}

	want := map[string][2]int32{"40": {4, 1}, "10": {1, 2}, "20": {2, 3}, "30": {3, 4}}

	if len(changes) != len(want) {
		t.Fatalf("moving the last to the front renumbers all four, got %+v", changes)
	}

	for _, c := range changes {
		if w := want[c.Id]; w[0] != c.From || w[1] != c.To {
			t.Fatalf("%s: got %d→%d want %d→%d", c.Id, c.From, c.To, w[0], w[1])
		}
	}

	if changes, _ := refComputeMove(sibs, 20, 1); len(changes) != 0 {
		t.Fatalf("a move to the current place changes nothing, got %+v", changes)
	}

	refFail(t, func() error { _, err := refComputeMove(sibs, 20, 4); return err }())
	refFail(t, func() error { _, err := refComputeMove(sibs, 99, 0); return err }())

	// duplicate orders (legacy data) are ordered by id and renumbered densely
	dup := []refSibling{{Id: 2, Order: 5}, {Id: 1, Order: 5}, {Id: 3, Order: 9}}
	changes, err = refComputeMove(dup, 3, 1)

	if err != nil {
		t.Fatal(err)
	}

	got := map[string]int32{}

	for _, c := range changes {
		got[c.Id] = c.To
	}

	if got["1"] != 1 || got["3"] != 2 || got["2"] != 3 {
		t.Fatalf("dense renumbering, got %+v", changes)
	}
}

// --- frequencies and the cron's calendar

func TestRefParseFrequency(t *testing.T) {
	cases := []struct {
		ft   models.TransactionScheduleFrequencyType
		raw  string
		want string
		warn bool
	}{
		{models.TRANSACTION_SCHEDULE_FREQUENCY_TYPE_DAILY, ``, "0", false},
		{models.TRANSACTION_SCHEDULE_FREQUENCY_TYPE_DAILY, `"0"`, "0", false},
		{models.TRANSACTION_SCHEDULE_FREQUENCY_TYPE_WEEKLY, `"fri,mon,1"`, "1,5", false},
		{models.TRANSACTION_SCHEDULE_FREQUENCY_TYPE_WEEKLY, `[0, "sat"]`, "0,6", false},
		{models.TRANSACTION_SCHEDULE_FREQUENCY_TYPE_MONTHLY, `"15,1"`, "1,15", false},
		{models.TRANSACTION_SCHEDULE_FREQUENCY_TYPE_MONTHLY, `[-1, 31]`, "-1,31", true},
		{models.TRANSACTION_SCHEDULE_FREQUENCY_TYPE_YEARLY, `"12-25,01-01"`, "101,1225", false},
		{models.TRANSACTION_SCHEDULE_FREQUENCY_TYPE_YEARLY, `[1225]`, "1225", false},
		{models.TRANSACTION_SCHEDULE_FREQUENCY_TYPE_YEARLY, `"02-29"`, "229", true},
		{models.TRANSACTION_SCHEDULE_FREQUENCY_TYPE_EVERY_N_DAYS, `14`, "14", false},
		{models.TRANSACTION_SCHEDULE_FREQUENCY_TYPE_DISABLED, ``, "", false},
	}

	for _, c := range cases {
		got, warnings, err := refParseFrequency(c.ft, json.RawMessage(c.raw))

		if err != nil {
			t.Fatalf("%s %s: %v", refFreqNames[c.ft], c.raw, err)
		}

		if got != c.want {
			t.Fatalf("%s %s: got %q want %q", refFreqNames[c.ft], c.raw, got, c.want)
		}

		if (len(warnings) > 0) != c.warn {
			t.Fatalf("%s %s: warnings %v", refFreqNames[c.ft], c.raw, warnings)
		}
	}

	bad := []struct {
		ft  models.TransactionScheduleFrequencyType
		raw string
	}{
		{models.TRANSACTION_SCHEDULE_FREQUENCY_TYPE_WEEKLY, ``},
		{models.TRANSACTION_SCHEDULE_FREQUENCY_TYPE_WEEKLY, `"7"`},
		{models.TRANSACTION_SCHEDULE_FREQUENCY_TYPE_MONTHLY, `0`},
		{models.TRANSACTION_SCHEDULE_FREQUENCY_TYPE_MONTHLY, `32`},
		{models.TRANSACTION_SCHEDULE_FREQUENCY_TYPE_YEARLY, `"02-30"`},
		{models.TRANSACTION_SCHEDULE_FREQUENCY_TYPE_YEARLY, `"13-01"`},
		{models.TRANSACTION_SCHEDULE_FREQUENCY_TYPE_EVERY_N_DAYS, `"3,4"`},
		{models.TRANSACTION_SCHEDULE_FREQUENCY_TYPE_EVERY_N_DAYS, `0`},
		{models.TRANSACTION_SCHEDULE_FREQUENCY_TYPE_DAILY, `"3"`},
		{models.TRANSACTION_SCHEDULE_FREQUENCY_TYPE_DISABLED, `"1"`},
	}

	for _, b := range bad {
		refFail(t, func() error { _, _, err := refParseFrequency(b.ft, json.RawMessage(b.raw)); return err }())
	}
}

// refTestSchedule builds a scheduled template the way upstream's createNewTemplateModel does
func refTestSchedule(ft models.TransactionScheduleFrequencyType, value string, utcOffset int16, start, end string) *models.TransactionTemplate {
	tz := time.FixedZone("t", int(utcOffset)*60)
	midnight := time.Date(2020, 1, 1, 0, 0, 0, 0, tz).In(time.UTC)
	t := &models.TransactionTemplate{
		TemplateId: 7, TemplateType: models.TRANSACTION_TEMPLATE_TYPE_SCHEDULE, Type: models.TRANSACTION_TYPE_EXPENSE,
		ScheduledFrequencyType: ft, ScheduledFrequency: value, ScheduledTimezoneUtcOffset: utcOffset,
		ScheduledAt: int16(midnight.Hour()*60 + midnight.Minute()),
	}

	if start != "" {
		s, _ := time.ParseInLocation("2006-01-02", start, tz)
		u := s.Unix()
		t.ScheduledStartTime = &u
	}

	if end != "" {
		e, _ := time.ParseInLocation("2006-01-02", end, tz)
		u := e.AddDate(0, 0, 1).Unix() - 1
		t.ScheduledEndTime = &u
	}

	return t
}

func refTestDates(t *models.TransactionTemplate, from, to string) []string {
	tz := time.FixedZone("t", int(t.ScheduledTimezoneUtcOffset)*60)
	f, _ := time.ParseInLocation("2006-01-02", from, tz)
	e, _ := time.ParseInLocation("2006-01-02", to, tz)
	var out []string

	for _, u := range refScheduleOccurrences(t, f.Unix(), e.AddDate(0, 0, 1).Unix()-1, 0) {
		out = append(out, time.Unix(u, 0).In(tz).Format("2006-01-02"))
	}

	return out
}

func TestRefScheduleMonthly31stSkipsShortMonths(t *testing.T) {
	s := refTestSchedule(models.TRANSACTION_SCHEDULE_FREQUENCY_TYPE_MONTHLY, "31", -420, "", "")
	got := strings.Join(refTestDates(s, "2026-01-01", "2026-06-30"), " ")

	if got != "2026-01-31 2026-03-31 2026-05-31" {
		t.Fatalf("day 31 fires only in 31-day months (the cron skips the rest), got %s", got)
	}
}

func TestRefScheduleMonthlyLastDay(t *testing.T) {
	s := refTestSchedule(models.TRANSACTION_SCHEDULE_FREQUENCY_TYPE_MONTHLY, "-1", 60, "", "")
	got := strings.Join(refTestDates(s, "2028-01-01", "2028-04-30"), " ")

	if got != "2028-01-31 2028-02-29 2028-03-31 2028-04-30" {
		t.Fatalf("-1 is the last day of every month (leap February too), got %s", got)
	}
}

func TestRefScheduleWeeklyInTemplateZone(t *testing.T) {
	// +08:00: local midnight is 16:00 UTC the day before — the weekday is the template zone's
	s := refTestSchedule(models.TRANSACTION_SCHEDULE_FREQUENCY_TYPE_WEEKLY, "1,5", 480, "", "")
	got := strings.Join(refTestDates(s, "2026-09-21", "2026-10-04"), " ")

	if got != "2026-09-21 2026-09-25 2026-09-28 2026-10-02" {
		t.Fatalf("Mondays and Fridays in the template's zone, got %s", got)
	}
}

func TestRefScheduleYearlyEveryNDaysAndBounds(t *testing.T) {
	y := refTestSchedule(models.TRANSACTION_SCHEDULE_FREQUENCY_TYPE_YEARLY, "101,1225", 0, "", "")

	if got := strings.Join(refTestDates(y, "2026-01-01", "2027-12-31"), " "); got != "2026-01-01 2026-12-25 2027-01-01 2027-12-25" {
		t.Fatalf("yearly MMDD, got %s", got)
	}

	n := refTestSchedule(models.TRANSACTION_SCHEDULE_FREQUENCY_TYPE_EVERY_N_DAYS, "10", -300, "2026-09-01", "2026-10-15")

	if got := strings.Join(refTestDates(n, "2026-08-01", "2026-12-31"), " "); got != "2026-09-01 2026-09-11 2026-09-21 2026-10-01 2026-10-11" {
		t.Fatalf("every 10 days from start, bounded by end, got %s", got)
	}

	d := refTestSchedule(models.TRANSACTION_SCHEDULE_FREQUENCY_TYPE_DAILY, "0", 0, "2026-09-30", "")

	if got := strings.Join(refTestDates(d, "2026-09-28", "2026-10-02"), " "); got != "2026-09-30 2026-10-01 2026-10-02" {
		t.Fatalf("daily from start, got %s", got)
	}

	p := refTestSchedule(models.TRANSACTION_SCHEDULE_FREQUENCY_TYPE_DISABLED, "", 0, "", "")

	if got := refTestDates(p, "2026-01-01", "2026-12-31"); len(got) != 0 {
		t.Fatalf("a paused schedule fires nothing, got %v", got)
	}

	if got := refScheduleOccurrences(d, time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC).Unix(), time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC).Unix(), 3); len(got) != 3 {
		t.Fatalf("max caps the occurrences, got %d", len(got))
	}
}

func TestRefTemplateSpecRoundTrip(t *testing.T) {
	s := refTestSchedule(models.TRANSACTION_SCHEDULE_FREQUENCY_TYPE_MONTHLY, "1,15", -420, "2026-01-01", "2026-12-31")
	s.Name, s.CategoryId, s.AccountId, s.Amount, s.TagIds = "Rent", 11, 22, 150000, "5,6"
	spec := refTemplateSpecOf(s)

	if spec.Start != "2026-01-01" || spec.End != "2026-12-31" || spec.UtcOffset != -420 || spec.Frequency != "monthly" || spec.FrequencyValue != "1,15" {
		t.Fatalf("schedule fields must read back in the template's own zone, got %+v", spec)
	}

	req, err := refTemplateCreateRequest(&spec, "k1")

	if err != nil {
		t.Fatal(err)
	}

	if req.TemplateType != models.TRANSACTION_TEMPLATE_TYPE_SCHEDULE || *req.ScheduledStartDate != "2026-01-01" || *req.ScheduledTimezoneUtcOffset != -420 || req.SourceAmount != 150000 || strings.Join(req.TagIds, ",") != "5,6" || req.ClientSessionId != "k1" {
		t.Fatalf("create request, got %+v", req)
	}
}

// --- template planning against synthetic books

func refTestLookups() *refLookups {
	accounts := []*models.Account{
		{AccountId: 1001, Name: "Checking", Category: models.ACCOUNT_CATEGORY_CHECKING_ACCOUNT, Type: models.ACCOUNT_TYPE_SINGLE_ACCOUNT, Currency: "USD"},
		{AccountId: 1002, Name: "Euro Savings", Category: models.ACCOUNT_CATEGORY_SAVINGS_ACCOUNT, Type: models.ACCOUNT_TYPE_SINGLE_ACCOUNT, Currency: "EUR"},
		{AccountId: 1003, Name: "Wallet", Category: models.ACCOUNT_CATEGORY_CASH, Type: models.ACCOUNT_TYPE_MULTI_SUB_ACCOUNTS, Currency: core.AccountCurrencyNotSetValue},
		{AccountId: 1004, Name: "Wallet USD", ParentAccountId: 1003, Category: models.ACCOUNT_CATEGORY_CASH, Type: models.ACCOUNT_TYPE_SINGLE_ACCOUNT, Currency: "USD"},
		{AccountId: 1005, Name: "Old Card", Category: models.ACCOUNT_CATEGORY_CREDIT_CARD, Type: models.ACCOUNT_TYPE_SINGLE_ACCOUNT, Currency: "USD", Hidden: true},
		{AccountId: 1006, Name: "Brokerage Cash", Category: models.ACCOUNT_CATEGORY_INVESTMENT, Type: models.ACCOUNT_TYPE_SINGLE_ACCOUNT, Currency: "USD"},
	}
	cats := []*models.TransactionCategory{
		{CategoryId: 2001, Name: "Housing", Type: models.CATEGORY_TYPE_EXPENSE},
		{CategoryId: 2002, Name: "Rent", Type: models.CATEGORY_TYPE_EXPENSE, ParentCategoryId: 2001},
		{CategoryId: 2003, Name: "Pay", Type: models.CATEGORY_TYPE_INCOME},
		{CategoryId: 2004, Name: "Salary", Type: models.CATEGORY_TYPE_INCOME, ParentCategoryId: 2003},
		{CategoryId: 2005, Name: "Moves", Type: models.CATEGORY_TYPE_TRANSFER},
		{CategoryId: 2006, Name: "Internal", Type: models.CATEGORY_TYPE_TRANSFER, ParentCategoryId: 2005},
	}
	tags := []*models.TransactionTag{{TagId: 3001, Name: "home"}, {TagId: 3002, Name: "old", Hidden: true}}
	lk := &refLookups{Accounts: accounts, AccountBy: map[int64]*models.Account{}, Categories: cats, CategoryBy: map[int64]*models.TransactionCategory{}, Tags: tags, TagBy: map[int64]*models.TransactionTag{}}

	for _, a := range accounts {
		lk.AccountBy[a.AccountId] = a
	}

	for _, c := range cats {
		lk.CategoryBy[c.CategoryId] = c
	}

	for _, t := range tags {
		lk.TagBy[t.TagId] = t
	}

	return lk
}

func refTestBody(t *testing.T, s string) *refTemplateBody {
	t.Helper()
	var b refTemplateBody

	if err := json.Unmarshal([]byte(s), &b); err != nil {
		t.Fatal(err)
	}

	return &b
}

func TestRefTemplatePlanning(t *testing.T) {
	lk := refTestLookups()
	loc := time.FixedZone("x", -7*3600)
	now := time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC)
	plan := func(body string) (*refTemplateSpec, []string, error) {
		spec := &refTemplateSpec{TagIds: []string{}, DestinationAccountId: "0"}
		w, err := refApplyTemplateBody(spec, refTestBody(t, body), lk, loc, now, true)
		return spec, w, err
	}

	spec, _, err := plan(`{"name":"Rent","type":"expense","account_name":"checking","category_name":"Rent","amount":150000,"tag_names":["home"],"frequency":"monthly","frequency_value":"1","start":"2026-10-01"}`)

	if err != nil {
		t.Fatal(err)
	}

	if spec.Kind != "scheduled" || spec.AccountId != "1001" || spec.CategoryId != "2002" || spec.UtcOffset != -420 || spec.FrequencyValue != "1" || strings.Join(spec.TagIds, ",") != "3001" {
		t.Fatalf("a frequency implies kind scheduled, names resolve, the zone is the caller's: %+v", spec)
	}

	// same-currency transfer: destination amount defaults to amount
	spec, _, err = plan(`{"name":"Sweep","type":"transfer","account_id":"1001","destination_account_name":"Brokerage Cash","category_name":"Internal","amount":5000}`)

	if err != nil || spec.DestinationAmount != 5000 || spec.Kind != "normal" {
		t.Fatalf("same-currency transfer: %+v %v", spec, err)
	}

	bad := map[string]string{
		"primary category":        `{"name":"x","type":"expense","account_id":"1001","category_name":"Housing","amount":1}`,
		"wrong category type":     `{"name":"x","type":"expense","account_id":"1001","category_name":"Salary","amount":1}`,
		"parent account":          `{"name":"x","type":"expense","account_name":"Wallet","category_name":"Rent","amount":1}`,
		"hidden account":          `{"name":"x","type":"expense","account_name":"Old Card","category_name":"Rent","amount":1}`,
		"cross-currency transfer": `{"name":"x","type":"transfer","account_id":"1001","destination_account_name":"Euro Savings","category_name":"Internal","amount":100}`,
		"hidden tag":              `{"name":"x","type":"expense","account_id":"1001","category_name":"Rent","amount":1,"tag_names":["old"]}`,
		"time of day":             `{"name":"x","type":"expense","account_id":"1001","category_name":"Rent","amount":1,"frequency":"daily","time":"09:00"}`,
		"n days without start":    `{"name":"x","type":"expense","account_id":"1001","category_name":"Rent","amount":1,"frequency":"every_n_days","frequency_value":3}`,
		"negative amount":         `{"name":"x","type":"expense","account_id":"1001","category_name":"Rent","amount":-1}`,
		"fractional amount":       `{"name":"x","type":"expense","account_id":"1001","category_name":"Rent","amount":12.5}`,
		"balance modification":    `{"name":"x","type":"balance_modification","account_id":"1001","category_name":"Rent","amount":1}`,
		"end before start":        `{"name":"x","type":"expense","account_id":"1001","category_name":"Rent","amount":1,"frequency":"daily","start":"2026-10-02","end":"2026-10-01"}`,
		"schedule fields, normal": `{"kind":"normal","name":"x","type":"expense","account_id":"1001","category_name":"Rent","amount":1,"start":"2026-10-01"}`,
		"destination on expense":  `{"name":"x","type":"expense","account_id":"1001","destination_account_id":"1002","category_name":"Rent","amount":1}`,
	}

	for name, body := range bad {
		if _, _, err := plan(body); err == nil {
			t.Fatalf("%s: expected a refusal", name)
		} else {
			refFail(t, err)
		}
	}

	// cross-currency with an explicit destination amount is fine
	if _, _, err := plan(`{"name":"FX","type":"transfer","account_id":"1001","destination_account_name":"Euro Savings","category_name":"Internal","amount":10000,"destination_amount":9200}`); err != nil {
		t.Fatalf("cross-currency with destination_amount: %v", err)
	}

	// patch: absent fields stay, kind cannot change
	before := &refTemplateSpec{Id: "9", Kind: "normal", Name: "Rent", Type: "expense", CategoryId: "2002", AccountId: "1001", DestinationAccountId: "0", Amount: 100, TagIds: []string{}}
	after := *before

	if _, err := refApplyTemplateBody(&after, refTestBody(t, `{"amount":250}`), lk, loc, now, false); err != nil {
		t.Fatal(err)
	}

	if diff := refTemplateDiff(before, &after, lk); len(diff) != 1 || diff[0].Field != "amount" {
		t.Fatalf("one field changed, got %+v", diff)
	}

	after = *before

	if _, err := refApplyTemplateBody(&after, refTestBody(t, `{"kind":"scheduled"}`), lk, loc, now, false); err == nil {
		t.Fatalf("kind cannot change on PATCH")
	}
}

// --- accounts

func TestRefPlanAccountCreate(t *testing.T) {
	loc := time.FixedZone("x", -7*3600)
	now := time.Date(2026, 9, 21, 18, 0, 0, 0, time.UTC)
	existing := []*models.Account{{AccountId: 1, Name: "Checking"}}
	spec := func(s string) *refAccountSpec {
		var v refAccountSpec

		if err := json.Unmarshal([]byte(s), &v); err != nil {
			t.Fatal(err)
		}

		return &v
	}

	p, w, err := refPlanAccountCreate(spec(`{"name":"Visa","category":"credit-card","initial_balance":25000}`), existing, "USD", loc, now)

	if err != nil {
		t.Fatal(err)
	}

	if p.Request.Currency != "USD" || p.Request.Type != models.ACCOUNT_TYPE_SINGLE_ACCOUNT || p.Request.Balance != "25000" || p.Preview["side"] != "liability" {
		t.Fatalf("defaults: %+v %+v", p.Request, p.Preview)
	}

	joined := strings.Join(w, " | ")

	if !strings.Contains(joined, "defaulted to USD") || !strings.Contains(joined, "POSITIVE") || !strings.Contains(joined, "dated today") {
		t.Fatalf("defaulted currency, the liability sign and the defaulted date are all said out loud: %s", joined)
	}

	wantTime, _ := time.ParseInLocation("2006-01-02", "2026-09-21", loc)

	if p.Request.BalanceTime != wantTime.Unix() || p.Preview["initialBalanceDate"] != "2026-09-21" {
		t.Fatalf("the opening balance is dated today in the caller's zone: %d %v", p.Request.BalanceTime, p.Preview["initialBalanceDate"])
	}

	p, w, err = refPlanAccountCreate(spec(`{"name":"Wallet","category":"cash","currency":"eur","sub_accounts":[{"name":"Coins","initial_balance":350,"initial_balance_date":"2026-01-01"},{"name":"Dollars","currency":"USD"}]}`), existing, "USD", loc, now)

	if err != nil {
		t.Fatal(err)
	}

	if p.Request.Type != models.ACCOUNT_TYPE_MULTI_SUB_ACCOUNTS || p.Request.Currency != core.AccountCurrencyNotSetValue || len(p.Request.SubAccounts) != 2 {
		t.Fatalf("a parent has no currency of its own: %+v", p.Request)
	}

	if p.Request.SubAccounts[0].Currency != "EUR" || p.Request.SubAccounts[0].Balance != "350" || p.Request.SubAccounts[1].Currency != "USD" || p.Request.SubAccounts[1].Balance != "" {
		t.Fatalf("sub-accounts take the top-level currency as their default: %+v %+v", p.Request.SubAccounts[0], p.Request.SubAccounts[1])
	}

	if p.Request.SubAccounts[0].Category != models.ACCOUNT_CATEGORY_CASH || p.Request.SubAccounts[0].Type != models.ACCOUNT_TYPE_SINGLE_ACCOUNT {
		t.Fatalf("sub-accounts share the parent's category and are single accounts")
	}

	_ = w

	bad := []string{
		`{"name":"x"}`,
		`{"name":"x","category":"piggybank"}`,
		`{"name":"","category":"cash"}`,
		`{"name":"x","category":"cash","currency":"XXQ"}`,
		`{"name":"x","category":"cash","credit_card_statement_date":5}`,
		`{"name":"x","category":"cash","credit_card_limit":100000}`,
		`{"name":"x","category":"credit_card","credit_card_statement_date":29}`,
		`{"name":"x","category":"cash","initial_balance":12.34}`,
		`{"name":"x","category":"cash","initial_balance_date":"2026-01-01"}`,
		`{"name":"x","category":"cash","initial_balance":100,"sub_accounts":[{"name":"a"}]}`,
		`{"name":"x","category":"cash","color":"red"}`,
		`{"name":"x","category":"cash","hidden":true}`,
	}

	for _, b := range bad {
		if _, _, err := refPlanAccountCreate(spec(b), existing, "USD", loc, now); err == nil {
			t.Fatalf("%s: expected a refusal", b)
		} else {
			refFail(t, err)
		}
	}

	_, w, _ = refPlanAccountCreate(spec(`{"name":"checking","category":"checking","currency":"USD"}`), existing, "USD", loc, now)

	if !strings.Contains(strings.Join(w, " "), "already exists") {
		t.Fatalf("a duplicate name is warned about: %v", w)
	}
}

func TestRefAccountModifyRequestKeepsTheWholeFamily(t *testing.T) {
	reconciled := int64(1700000000)
	stmt := 12
	limit := int64(500000)
	all := []*models.Account{
		{AccountId: 1, Name: "Card", Category: models.ACCOUNT_CATEGORY_CREDIT_CARD, Type: models.ACCOUNT_TYPE_MULTI_SUB_ACCOUNTS, Currency: "USD", Icon: 3, Color: "112233", Hidden: true, Extend: &models.AccountExtend{CreditCardStatementDate: &stmt, CreditCardLimit: &limit}},
		{AccountId: 2, Name: "Card A", ParentAccountId: 1, Category: models.ACCOUNT_CATEGORY_CREDIT_CARD, Type: models.ACCOUNT_TYPE_SINGLE_ACCOUNT, Currency: "USD", Icon: 3, Color: "112233", DisplayOrder: 2, Extend: &models.AccountExtend{LastReconciledTime: &reconciled}},
		{AccountId: 3, Name: "Card B", ParentAccountId: 1, Category: models.ACCOUNT_CATEGORY_CREDIT_CARD, Type: models.ACCOUNT_TYPE_SINGLE_ACCOUNT, Currency: "EUR", Icon: 4, Color: "445566", DisplayOrder: 1, Hidden: true},
		{AccountId: 9, Name: "Other", Category: models.ACCOUNT_CATEGORY_CASH, Type: models.ACCOUNT_TYPE_SINGLE_ACCOUNT, Currency: "USD"},
	}

	// renaming one sub-account must carry the other sub-account (upstream deletes omitted ones)
	f := refAccountFieldsOf(all[1])
	f.Name = "Card A (primary)"
	req, err := refAccountModifyRequest(all, all[1], f)

	if err != nil {
		t.Fatal(err)
	}

	if req.Id != 1 || len(req.SubAccounts) != 2 {
		t.Fatalf("the request targets the parent and carries every sub-account: %+v", req)
	}

	if req.SubAccounts[0].Id != 3 || req.SubAccounts[1].Id != 2 {
		t.Fatalf("sub-accounts in display order, got %d, %d", req.SubAccounts[0].Id, req.SubAccounts[1].Id)
	}

	if req.SubAccounts[1].Name != "Card A (primary)" || req.SubAccounts[0].Name != "Card B" || !req.SubAccounts[0].Hidden {
		t.Fatalf("only the target changes; hidden flags survive: %+v %+v", req.SubAccounts[0], req.SubAccounts[1])
	}

	if req.SubAccounts[1].LastReconciledTime == nil || *req.SubAccounts[1].LastReconciledTime != reconciled {
		t.Fatalf("the last reconciled time survives")
	}

	if !req.Hidden || req.CreditCardStatementDate != 12 || req.CreditCardLimit != "500000" || req.Name != "Card" || req.Currency != nil || req.Balance != nil {
		t.Fatalf("the parent's own fields survive and nothing is sent that upstream refuses: %+v", req)
	}

	// a single account: no sub-accounts at all
	g := refAccountFieldsOf(all[3])
	g.Comment = "petty cash"
	req, err = refAccountModifyRequest(all, all[3], g)

	if err != nil || req.Id != 9 || len(req.SubAccounts) != 0 || req.Comment != "petty cash" || req.CreditCardStatementDate != 0 {
		t.Fatalf("single account: %+v %v", req, err)
	}
}

func TestRefAccountCategory(t *testing.T) {
	for in, want := range map[string]string{"checking": "checking", "Credit Card": "credit_card", "credit-card": "credit_card", "cd": "certificate_of_deposit", "8": "savings", "savings_account": "savings"} {
		c, err := refAccountCategory(in)

		if err != nil || refAccountCategoryNames[c] != want {
			t.Fatalf("%q: got %v %v", in, refAccountCategoryNames[c], err)
		}
	}

	if refAccountSide(models.ACCOUNT_CATEGORY_DEBT) != "liability" || refAccountSide(models.ACCOUNT_CATEGORY_INVESTMENT) != "asset" {
		t.Fatalf("sides follow upstream's maps")
	}
}

func TestRefAccountTreeAndFilters(t *testing.T) {
	all := []*models.Account{
		{AccountId: 1, Name: "Wallet", Category: models.ACCOUNT_CATEGORY_CASH, Type: models.ACCOUNT_TYPE_MULTI_SUB_ACCOUNTS, Currency: core.AccountCurrencyNotSetValue, DisplayOrder: 1},
		{AccountId: 2, Name: "USD", ParentAccountId: 1, Category: models.ACCOUNT_CATEGORY_CASH, Type: models.ACCOUNT_TYPE_SINGLE_ACCOUNT, Currency: "USD", Balance: 1234},
		{AccountId: 3, Name: "EUR", ParentAccountId: 1, Category: models.ACCOUNT_CATEGORY_CASH, Type: models.ACCOUNT_TYPE_SINGLE_ACCOUNT, Currency: "EUR", Balance: -99, Hidden: true},
		{AccountId: 4, Name: "Loan", Category: models.ACCOUNT_CATEGORY_DEBT, Type: models.ACCOUNT_TYPE_SINGLE_ACCOUNT, Currency: "USD", Balance: -500000},
	}

	tree := refAccountTree(all, false, time.UTC)

	if len(tree) != 2 || tree[0].Balance != nil || len(tree[0].SubAccounts) != 1 || *tree[0].SubAccounts[0].Balance != 1234 {
		t.Fatalf("a parent's balance is null, hidden sub-accounts are dropped: %+v", tree)
	}

	if tree[1].Side != "liability" || *tree[1].Balance != -500000 {
		t.Fatalf("a liability keeps upstream's negative balance: %+v", tree[1])
	}

	tree = refAccountTree(all, true, time.UTC)

	if !refAccountMatches(tree[0], "", "EUR") || refAccountMatches(tree[1], "", "EUR") || !refAccountMatches(tree[1], "debt", "") {
		t.Fatalf("currency matches through sub-accounts; category matches exactly")
	}
}

// --- tags, settings, profile, rates

func TestRefTagBatchNames(t *testing.T) {
	raw := []json.RawMessage{json.RawMessage(`"trip-2026"`), json.RawMessage(`{"name":"reimbursable"}`), json.RawMessage(`" trip-2026 "`)}
	names, err := refTagBatchNames(raw)

	if err != nil || strings.Join(names, ",") != "trip-2026,reimbursable" {
		t.Fatalf("strings and {name} objects, de-duplicated in order: %v %v", names, err)
	}

	refFail(t, func() error { _, err := refTagBatchNames(nil); return err }())
	refFail(t, func() error {
		_, err := refTagBatchNames([]json.RawMessage{json.RawMessage(`{"nom":"x"}`)})
		return err
	}())
	refFail(t, func() error { _, err := refTagBatchNames([]json.RawMessage{json.RawMessage(`""`)}); return err }())
}

func TestRefSettingValue(t *testing.T) {
	ok := map[string][2]string{
		"showAccountBalance":              {`true`, "true"},
		"itemsCountInTransactionListPage": {`50`, "50"},
		"chartColors":                     {`"a,b"`, "a,b"},
		"totalAmountExcludeAccountIds":    {`{"123":true}`, `{"123":true}`},
	}

	for key, c := range ok {
		got, err := refSettingValue(key, json.RawMessage(c[0]))

		if err != nil || got != c[1] {
			t.Fatalf("%s: got %q %v", key, got, err)
		}
	}

	refFail(t, func() error { _, err := refSettingValue("noSuchKey", json.RawMessage(`1`)); return err }())
	refFail(t, func() error { _, err := refSettingValue("showAccountBalance", json.RawMessage(`"yes"`)); return err }())
	refFail(t, func() error {
		_, err := refSettingValue("itemsCountInTransactionListPage", json.RawMessage(`"many"`))
		return err
	}())
}

func TestRefProfileParsing(t *testing.T) {
	if d, err := refFirstDayOfWeek("Monday"); err != nil || d != 1 {
		t.Fatalf("weekday name: %v %v", d, err)
	}

	if d, err := refFirstDayOfWeek("0"); err != nil || d != 0 {
		t.Fatalf("weekday number: %v %v", d, err)
	}

	refFail(t, func() error { _, err := refFirstDayOfWeek("7"); return err }())

	if f, err := refFiscalYearStart("04-06"); err != nil || f.String() != "04-06" || uint16(f) != 0x0406 {
		t.Fatalf("fiscal year start: %v %v", f, err)
	}

	refFail(t, func() error { _, err := refFiscalYearStart("02-30"); return err }())
	refFail(t, func() error { _, err := refFiscalYearStart("April 6"); return err }())

	req, err := refProfileRequest(refProfileFields{Nickname: "a", DefaultCurrency: "USD", FirstDayOfWeek: 0, FiscalYearStart: "01-01"}, refProfileFields{Nickname: "a", DefaultCurrency: "EUR", FirstDayOfWeek: 1, FiscalYearStart: "01-01"})

	if err != nil || req.Nickname != "" || req.DefaultCurrency != "EUR" || req.FirstDayOfWeek == nil || *req.FirstDayOfWeek != 1 || req.FiscalYearStart != nil || req.Password != "" || req.Email != "" {
		t.Fatalf("only changed fields are sent, never credentials: %+v %v", req, err)
	}
}

func TestRefRatesAndConversionConvention(t *testing.T) {
	// the frontend converts amount × toRate ÷ fromRate (src/lib/numeral.ts getExchangedAmountByRate)
	usd, _ := ParseRate("1.1") // units of USD per 1 EUR (ECB base)
	eur, _ := ParseRate("1")

	if got := ConvertHundredths(10000, usd, eur); got != 9091 {
		t.Fatalf("100.00 USD at 1.1 USD/EUR is 90.91 EUR, got %d", got)
	}

	if got := ConvertHundredths(10000, eur, usd); got != 11000 {
		t.Fatalf("100.00 EUR is 110.00 USD, got %d", got)
	}

	if got := ConvertHundredths(-5, usd, eur); got != -5 {
		t.Fatalf("rounding is half away from zero on negatives too: -0.05/1.1=-0.045 → -0.05, got %d", got)
	}

	if s := refCustomRelative(92000000, models.UserCustomExchangeRateFactorInDatabase); s != "0.92" {
		t.Fatalf("stored custom rates are relative to the default row: %s", s)
	}

	if s := refCustomRelative(150000000, 0); s != "1.5" {
		t.Fatalf("a missing default row means the database factor: %s", s)
	}

	if m := refRateMeaning("USD", "EUR", "0.8"); m != "1 USD = 0.8 EUR (so 1 EUR = 1.25 USD)" {
		t.Fatalf("meaning: %s", m)
	}
}

func TestRefCheckMatches(t *testing.T) {
	a := refTagFields{Id: "1", Name: "x", GroupId: "0"}

	if !refCheckMatches(a, refMustJSON(a)) || refCheckMatches(a, refMustJSON(refTagFields{Id: "1", Name: "y", GroupId: "0"})) || !refCheckMatches(a, nil) {
		t.Fatalf("check compares canonical JSON")
	}
}

// --- the route table

func TestRefRoutesMountAndFollowTheWriteProtocol(t *testing.T) {
	refTestEnv(t)
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	group := engine.Group(BasePath)
	seen := map[string]bool{}

	var all []RouteDef
	all = append(all, refAccountRoutes()...)
	all = append(all, refReferenceRoutes()...)
	all = append(all, refCurrencyRoutes()...)
	all = append(all, refUserRoutes()...)

	for _, r := range all {
		key := r.Method + " " + r.Path

		if seen[key] {
			t.Fatalf("route %s declared twice", key)
		}

		seen[key] = true

		if r.Handler == nil || r.Summary == "" {
			t.Fatalf("route %s needs a handler and a summary", key)
		}

		switch r.Method {
		case http.MethodDelete:
			if r.Tier == TierRead {
				t.Fatalf("%s: a delete is never read tier", key)
			}

			if r.Tier == TierWrite && !strings.HasPrefix(r.Path, "/exchange-rates/custom/") {
				t.Fatalf("%s: every delete except a custom rate is admin tier", key)
			}
		case http.MethodPatch, http.MethodPut:
			if r.Tier != TierWrite || !r.DryRunnable {
				t.Fatalf("%s: writes are write tier and dry-runnable", key)
			}
		case http.MethodPost:
			if r.Tier != TierRead && !r.DryRunnable {
				t.Fatalf("%s: a write route must be dry-runnable", key)
			}
		}

		func() {
			defer func() {
				if rec := recover(); rec != nil {
					t.Fatalf("mounting %s panicked: %v", key, rec)
				}
			}()

			group.Handle(r.Method, r.Path, func(c *gin.Context) {})
		}()
	}

	for _, must := range []string{
		"GET /accounts", "GET /accounts/:id", "GET /accounts/:id/properties", "POST /accounts", "POST /accounts/batch", "PATCH /accounts/:id",
		"POST /accounts/:id/hide", "POST /accounts/:id/move", "DELETE /accounts/:id", "DELETE /accounts/:id/sub-accounts/:sub_id",
		"GET /categories", "POST /categories/batch", "DELETE /categories/:id", "GET /tags", "POST /tags/batch", "GET /tag-groups", "DELETE /tag-groups/:id",
		"GET /templates", "PATCH /templates/:id", "GET /schedules/upcoming", "GET /exchange-rates", "POST /exchange-rates/convert",
		"PUT /exchange-rates/custom/:currency", "DELETE /exchange-rates/custom/:currency", "GET /insights", "DELETE /insights/:id",
		"GET /user/profile", "PATCH /user/profile", "GET /user/settings", "PATCH /user/settings", "GET /data/statistics", "GET /data/export",
	} {
		if !seen[must] {
			t.Fatalf("route %s is missing", must)
		}
	}
}

func TestRefInverseExecutorsRegistered(t *testing.T) {
	var kinds []string

	for _, k := range []string{"account", "category", "tag", "tag_group", "template", "insight"} {
		kinds = append(kinds, "ref.delete_"+k, "ref.undelete_"+k, "ref.set_"+k+"_orders")

		if k != "tag_group" {
			kinds = append(kinds, "ref.hide_"+k)
		}
	}

	kinds = append(kinds, "ref.restore_account_fields", "ref.restore_category_fields", "ref.restore_tag_fields", "ref.restore_tag_group_fields",
		"ref.restore_template", "ref.restore_insight", "ref.set_custom_rate", "ref.delete_custom_rate", "ref.restore_profile", "ref.restore_settings")

	for _, k := range kinds {
		if _, ok := LookupInverse(k); !ok {
			t.Fatalf("inverse executor %s is not registered", k)
		}
	}
}

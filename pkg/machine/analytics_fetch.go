package machine

import (
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/mayswind/ezbookkeeping/pkg/api"
	"github.com/mayswind/ezbookkeeping/pkg/models"
)

// analytics_fetch.go — every upstream read the analytics plane makes, each through the upstream
// handler as the bound user (apis.mdx §2.3), plus the account/category books the grouping needs.

// anAccountCategoryNames are the machine plane's names for upstream's account categories (§10.2)
var anAccountCategoryNames = map[models.AccountCategory]string{
	models.ACCOUNT_CATEGORY_CASH:                   "cash",
	models.ACCOUNT_CATEGORY_CHECKING_ACCOUNT:       "checking",
	models.ACCOUNT_CATEGORY_CREDIT_CARD:            "credit_card",
	models.ACCOUNT_CATEGORY_VIRTUAL:                "virtual",
	models.ACCOUNT_CATEGORY_DEBT:                   "debt",
	models.ACCOUNT_CATEGORY_RECEIVABLES:            "receivables",
	models.ACCOUNT_CATEGORY_INVESTMENT:             "investment",
	models.ACCOUNT_CATEGORY_SAVINGS_ACCOUNT:        "savings",
	models.ACCOUNT_CATEGORY_CERTIFICATE_OF_DEPOSIT: "certificate_of_deposit",
}

// anAccountCategoryByName is the reverse of anAccountCategoryNames
func anAccountCategoryByName(name string) (models.AccountCategory, bool) {
	name = strings.ToLower(strings.TrimSpace(name))

	for k, v := range anAccountCategoryNames {
		if v == name {
			return k, true
		}
	}

	return 0, false
}

// anAccount is one account of the bound user, flattened (sub-accounts are accounts with ParentId)
type anAccount struct {
	Id           int64
	Name         string
	ParentId     int64
	Category     models.AccountCategory
	Type         models.AccountType
	Currency     string
	Balance      int64
	OwnHidden    bool
	Hidden       bool // hidden itself or through its parent
	IsAsset      bool
	IsLiability  bool
	DisplayOrder int32
	Children     []int64
}

// IsLeaf is true for an account that can hold transactions (a single account or a sub-account)
func (a *anAccount) IsLeaf() bool {
	return a.Type != models.ACCOUNT_TYPE_MULTI_SUB_ACCOUNTS
}

// anAccounts is the bound user's account book
type anAccounts struct {
	byId  map[int64]*anAccount
	order []int64 // display order, parents before their children
}

// Get returns an account or nil
func (b *anAccounts) Get(id int64) *anAccount {
	if b == nil {
		return nil
	}

	return b.byId[id]
}

// Leaves are every account that can hold transactions, in display order
func (b *anAccounts) Leaves() []*anAccount {
	var out []*anAccount

	for _, id := range b.order {
		if a := b.byId[id]; a != nil && a.IsLeaf() {
			out = append(out, a)
		}
	}

	return out
}

// anLoadAccounts reads every account through AccountListHandler
func anLoadAccounts(mc *Ctx) (*anAccounts, error) {
	var list models.AccountInfoResponseSlice

	if err := mc.CallUpstreamInto(api.Accounts.AccountListHandler, "GET", url.Values{"visible_only": {"false"}}, nil, &list); err != nil {
		return nil, err
	}

	book := &anAccounts{byId: map[int64]*anAccount{}}

	var add func(r *models.AccountInfoResponse, parent *anAccount) error

	add = func(r *models.AccountInfoResponse, parent *anAccount) error {
		if r == nil {
			return nil
		}

		balance := int64(0)

		if r.Balance != "" {
			b, err := ParseAmountString(r.Balance)

			if err != nil {
				return err
			}

			balance = b
		}

		a := &anAccount{
			Id:           r.Id,
			Name:         r.Name,
			ParentId:     r.ParentId,
			Category:     r.Category,
			Type:         r.Type,
			Currency:     r.Currency,
			Balance:      balance,
			OwnHidden:    r.Hidden,
			Hidden:       r.Hidden || (parent != nil && parent.Hidden),
			IsAsset:      r.Category.IsAsset(),
			IsLiability:  r.Category.IsLiability(),
			DisplayOrder: r.DisplayOrder,
		}

		book.byId[a.Id] = a
		book.order = append(book.order, a.Id)

		if parent != nil {
			parent.Children = append(parent.Children, a.Id)
		}

		for _, sub := range r.SubAccounts {
			if err := add(sub, a); err != nil {
				return err
			}
		}

		return nil
	}

	for _, r := range list {
		if err := add(r, nil); err != nil {
			return nil, err
		}
	}

	return book, nil
}

// anCategory is one transaction category, flattened
type anCategory struct {
	Id           int64
	Name         string
	ParentId     int64
	Type         models.TransactionCategoryType
	Hidden       bool
	DisplayOrder int32
	Children     []int64
}

// anCategories is the bound user's category book
type anCategories struct {
	byId  map[int64]*anCategory
	order []int64
}

// Get returns a category or nil
func (b *anCategories) Get(id int64) *anCategory {
	if b == nil {
		return nil
	}

	return b.byId[id]
}

// Primary is the category a secondary category rolls up into (a primary is its own primary), the
// same rule as statistics.ts assembleAccountAndCategoryInfo
func (b *anCategories) Primary(c *anCategory) *anCategory {
	if c == nil {
		return nil
	}

	if c.ParentId > 0 {
		if p := b.byId[c.ParentId]; p != nil {
			return p
		}

		// statistics.ts: a secondary category whose parent is missing has no primary and is skipped
		return nil
	}

	return c
}

// anLoadCategories reads every category through CategoryListHandler
func anLoadCategories(mc *Ctx) (*anCategories, error) {
	var byType map[string]models.TransactionCategoryInfoResponseSlice

	if err := mc.CallUpstreamInto(api.TransactionCategories.CategoryListHandler, "GET", url.Values{}, nil, &byType); err != nil {
		return nil, err
	}

	book := &anCategories{byId: map[int64]*anCategory{}}
	types := make([]string, 0, len(byType))

	for t := range byType {
		types = append(types, t)
	}

	sort.Strings(types)

	for _, t := range types {
		for _, r := range byType[t] {
			if r == nil {
				continue
			}

			p := &anCategory{Id: r.Id, Name: r.Name, ParentId: r.ParentId, Type: r.Type, Hidden: r.Hidden, DisplayOrder: r.DisplayOrder}
			book.byId[p.Id] = p
			book.order = append(book.order, p.Id)

			for _, s := range r.SubCategories {
				if s == nil {
					continue
				}

				c := &anCategory{Id: s.Id, Name: s.Name, ParentId: s.ParentId, Type: s.Type, Hidden: s.Hidden, DisplayOrder: s.DisplayOrder}
				book.byId[c.Id] = c
				book.order = append(book.order, c.Id)
				p.Children = append(p.Children, c.Id)
			}
		}
	}

	return book, nil
}

// anSelection is the resolved set of accounts and categories one call covers
type anSelection struct {
	Accounts   *anAccounts
	Categories *anCategories

	// AccountSet holds the included leaf accounts
	AccountSet map[int64]bool
	// ExplicitAccounts is true when the caller named accounts
	ExplicitAccounts bool
	// HiddenExcluded counts the leaf accounts left out because they are hidden
	HiddenExcluded []int64

	// IncludeCategories, when non-nil, holds the only categories counted (secondaries and primaries
	// named directly, expanded to their children)
	IncludeCategories map[int64]bool
	// ExcludeCategories holds the categories never counted
	ExcludeCategories map[int64]bool
}

// anResolveSelection resolves account_ids / category_ids / exclude_category_ids against the books
func anResolveSelection(mc *Ctx, args *anArgs, includeHiddenDefault bool) (*anSelection, error) {
	accounts, err := anLoadAccounts(mc)

	if err != nil {
		return nil, err
	}

	categories, err := anLoadCategories(mc)

	if err != nil {
		return nil, err
	}

	return anBuildSelection(accounts, categories, args, includeHiddenDefault)
}

// anBuildSelection is the pure half of anResolveSelection (tested directly)
func anBuildSelection(accounts *anAccounts, categories *anCategories, args *anArgs, includeHiddenDefault bool) (*anSelection, error) {
	sel := &anSelection{Accounts: accounts, Categories: categories, AccountSet: map[int64]bool{}}
	includeHidden := args.IncludeHiddenAccounts || includeHiddenDefault

	if len(args.AccountIds) > 0 {
		sel.ExplicitAccounts = true

		for _, id := range args.AccountIds {
			a := accounts.Get(id)

			if a == nil {
				return nil, NotFound("GET /machine/v1/accounts lists the account ids", "no account with id %s", idString(id)).WithDetails(map[string]any{"id": idString(id)})
			}

			if a.IsLeaf() {
				sel.AccountSet[a.Id] = true
				continue
			}

			// a parent account holds no balance of its own: it means its sub-accounts
			for _, cid := range a.Children {
				// naming the parent names its sub-accounts; a sub-account hidden on its own stays
				// out unless hidden accounts were asked for
				if c := accounts.Get(cid); c != nil && (includeHidden || !c.OwnHidden) {
					sel.AccountSet[cid] = true
				}
			}
		}
	} else {
		for _, a := range accounts.Leaves() {
			if a.Hidden && !includeHidden {
				sel.HiddenExcluded = append(sel.HiddenExcluded, a.Id)
				continue
			}

			sel.AccountSet[a.Id] = true
		}
	}

	expand := func(ids []int64, name string) (map[int64]bool, error) {
		if len(ids) == 0 {
			return nil, nil
		}

		set := map[int64]bool{}

		for _, id := range ids {
			c := categories.Get(id)

			if c == nil {
				return nil, NotFound("GET /machine/v1/categories lists the category ids", "%s: no category with id %s", name, idString(id)).WithDetails(map[string]any{"id": idString(id)})
			}

			set[c.Id] = true

			for _, cid := range c.Children {
				set[cid] = true
			}
		}

		return set, nil
	}

	inc, err := expand(args.CategoryIds, "category_ids")

	if err != nil {
		return nil, err
	}

	exc, err := expand(args.ExcludeCategoryIds, "exclude_category_ids")

	if err != nil {
		return nil, err
	}

	sel.IncludeCategories = inc
	sel.ExcludeCategories = exc

	return sel, nil
}

// AccountIncluded reports whether a leaf account is in the selection
func (s *anSelection) AccountIncluded(id int64) bool {
	return s.AccountSet[id]
}

// CategoryIncluded reports whether a category passes the category filters
func (s *anSelection) CategoryIncluded(id int64) bool {
	if s.ExcludeCategories != nil && s.ExcludeCategories[id] {
		return false
	}

	if s.IncludeCategories != nil && !s.IncludeCategories[id] {
		return false
	}

	return true
}

// AccountIdStrings lists the included accounts for provenance, in display order
func (s *anSelection) AccountIdStrings() []string {
	out := []string{}

	for _, id := range s.Accounts.order {
		if s.AccountSet[id] {
			out = append(out, idString(id))
		}
	}

	return out
}

// ExcludedLeafIds are the leaf accounts NOT in the selection (for upstream's exclude_account_ids)
func (s *anSelection) ExcludedLeafIds() []int64 {
	var out []int64

	for _, a := range s.Accounts.Leaves() {
		if !s.AccountSet[a.Id] {
			out = append(out, a.Id)
		}
	}

	return out
}

// ExcludedCategoryIds are the categories upstream must leave out (for exclude_category_ids)
func (s *anSelection) ExcludedCategoryIds() []int64 {
	var out []int64

	for _, id := range s.Categories.order {
		if !s.CategoryIncluded(id) {
			out = append(out, id)
		}
	}

	return out
}

// anJoinIds renders ids as upstream's comma-separated list
func anJoinIds(ids []int64) string {
	parts := make([]string, len(ids))

	for i, id := range ids {
		parts[i] = strconv.FormatInt(id, 10)
	}

	return strings.Join(parts, ",")
}

// anIdStrings renders ids for the wire
func anIdStrings(ids []int64) []string {
	out := make([]string, len(ids))

	for i, id := range ids {
		out[i] = idString(id)
	}

	return out
}

// anStatItem is one category × account total from upstream's statistics endpoints
type anStatItem struct {
	CategoryId         int64
	AccountId          int64
	RelatedAccountId   int64
	RelatedAccountType models.TransactionRelatedAccountType
	Amount             int64
}

func anStatItemsOf(items []*models.TransactionStatisticResponseItem) ([]anStatItem, error) {
	out := make([]anStatItem, 0, len(items))

	for _, it := range items {
		if it == nil {
			continue
		}

		amount, err := ParseAmountString(it.TotalAmount)

		if err != nil {
			return nil, err
		}

		out = append(out, anStatItem{
			CategoryId:         it.CategoryId,
			AccountId:          it.AccountId,
			RelatedAccountId:   it.RelatedAccountId,
			RelatedAccountType: it.RelatedAccountType,
			Amount:             amount,
		})
	}

	return out, nil
}

func anTimeQuery(tagFilter string, useTransactionTimezone bool) url.Values {
	q := url.Values{}

	if tagFilter != "" {
		q.Set("tag_filter", tagFilter)
	}

	if useTransactionTimezone {
		q.Set("use_transaction_timezone", "true")
	}

	return q
}

// anFetchStatistics calls transactions/statistics.json for one unix range
func anFetchStatistics(mc *Ctx, startUnix, endUnix int64, tagFilter string, useTransactionTimezone bool) ([]anStatItem, error) {
	q := anTimeQuery(tagFilter, useTransactionTimezone)
	q.Set("start_time", strconv.FormatInt(startUnix, 10))
	q.Set("end_time", strconv.FormatInt(endUnix, 10))

	var resp models.TransactionStatisticResponse

	if err := mc.CallUpstreamInto(api.Transactions.TransactionStatisticsHandler, "GET", q, nil, &resp); err != nil {
		return nil, err
	}

	return anStatItemsOf(resp.Items)
}

// anMonthStats is one month of statistics/trends.json
type anMonthStats struct {
	Year  int32
	Month int32
	Items []anStatItem
}

// anFetchTrends calls transactions/statistics/trends.json for a month range (inclusive)
func anFetchTrends(mc *Ctx, startMonth, endMonth time.Time, tagFilter string, useTransactionTimezone bool) ([]anMonthStats, error) {
	q := anTimeQuery(tagFilter, useTransactionTimezone)
	q.Set("start_year_month", startMonth.Format("2006-01"))
	q.Set("end_year_month", endMonth.Format("2006-01"))

	var resp []*models.TransactionStatisticTrendsResponseItem

	if err := mc.CallUpstreamInto(api.Transactions.TransactionStatisticsTrendsHandler, "GET", q, nil, &resp); err != nil {
		return nil, err
	}

	out := make([]anMonthStats, 0, len(resp))

	for _, m := range resp {
		if m == nil {
			continue
		}

		items, err := anStatItemsOf(m.Items)

		if err != nil {
			return nil, err
		}

		out = append(out, anMonthStats{Year: m.Year, Month: m.Month, Items: items})
	}

	return out, nil
}

// anFact is one statistics total assigned to a period bucket
type anFact struct {
	Bucket string
	anStatItem
}

// anFetchCategoryFacts reads category × account totals for a range, bucketed by b. The whole range
// (interval none) is one statistics.json call; month/quarter/year intervals read the full calendar
// months through statistics/trends.json and the partial first/last months through statistics.json,
// so a range that starts on the 15th counts from the 15th, not from the 1st.
func anFetchCategoryFacts(mc *Ctx, rng *DateRange, b *anBucketer, tagFilter string, useTransactionTimezone bool) ([]anFact, []string, error) {
	var facts []anFact
	sources := map[string]bool{}

	if b.Interval == anIntervalNone {
		items, err := anFetchStatistics(mc, rng.StartUnix, rng.EndUnix, tagFilter, useTransactionTimezone)

		if err != nil {
			return nil, nil, err
		}

		sources["GET /api/v1/transactions/statistics.json"] = true

		for _, it := range items {
			facts = append(facts, anFact{Bucket: b.rangeKey(), anStatItem: it})
		}

		return facts, anSortedKeys(sources), nil
	}

	if b.Interval != anIntervalMonth && b.Interval != anIntervalQuarter && b.Interval != anIntervalYear {
		return nil, nil, Invalid("category statistics come in months: pass interval none, month, quarter or year", "interval %s is not available for category statistics", b.Interval)
	}

	segments := anMonthSegments(b.RangeStart, b.RangeEnd, b.Loc)
	var fullFirst, fullLast time.Time
	haveFull := false

	for _, seg := range segments {
		if seg.Full {
			if !haveFull {
				fullFirst = seg.Month
				haveFull = true
			}

			fullLast = seg.Month
			continue
		}

		startUnix, _ := anDayUnixRange(seg.Start)
		_, endUnix := anDayUnixRange(seg.End)
		items, err := anFetchStatistics(mc, startUnix, endUnix, tagFilter, useTransactionTimezone)

		if err != nil {
			return nil, nil, err
		}

		sources["GET /api/v1/transactions/statistics.json"] = true
		key := b.Key(seg.Month)

		for _, it := range items {
			facts = append(facts, anFact{Bucket: key, anStatItem: it})
		}
	}

	if haveFull {
		months, err := anFetchTrends(mc, fullFirst, fullLast, tagFilter, useTransactionTimezone)

		if err != nil {
			return nil, nil, err
		}

		sources["GET /api/v1/transactions/statistics/trends.json"] = true

		for _, m := range months {
			monthStart := time.Date(int(m.Year), time.Month(m.Month), 1, 0, 0, 0, 0, b.Loc)

			if monthStart.Before(fullFirst) || monthStart.After(fullLast) {
				continue
			}

			key := b.Key(monthStart)

			for _, it := range m.Items {
				facts = append(facts, anFact{Bucket: key, anStatItem: it})
			}
		}
	}

	return facts, anSortedKeys(sources), nil
}

// anCurrencyIncomeExpense is income and expense of one currency, from amounts(.json|/daily.json)
type anCurrencyIncomeExpense struct {
	Currency string
	Income   int64
	Expense  int64
}

func anParseAmountInfos(infos []*models.TransactionAmountsResponseItemAmountInfo) ([]anCurrencyIncomeExpense, error) {
	out := make([]anCurrencyIncomeExpense, 0, len(infos))

	for _, info := range infos {
		if info == nil {
			continue
		}

		income, err := ParseAmountString(info.IncomeAmount)

		if err != nil {
			return nil, err
		}

		expense, err := ParseAmountString(info.ExpenseAmount)

		if err != nil {
			return nil, err
		}

		out = append(out, anCurrencyIncomeExpense{Currency: info.Currency, Income: income, Expense: expense})
	}

	return out, nil
}

func anExcludeQuery(sel *anSelection, useTransactionTimezone bool) url.Values {
	q := url.Values{}

	if ids := sel.ExcludedLeafIds(); len(ids) > 0 {
		q.Set("exclude_account_ids", anJoinIds(ids))
	}

	if ids := sel.ExcludedCategoryIds(); len(ids) > 0 {
		q.Set("exclude_category_ids", anJoinIds(ids))
	}

	if useTransactionTimezone {
		q.Set("use_transaction_timezone", "true")
	}

	return q
}

// anNamedRange is one named period for transactions/amounts.json
type anNamedRange struct {
	Name      string
	StartUnix int64
	EndUnix   int64
}

// anFetchAmounts calls transactions/amounts.json (up to 20 named ranges per call; this walks them in
// chunks of 20)
func anFetchAmounts(mc *Ctx, sel *anSelection, ranges []anNamedRange, useTransactionTimezone bool) (map[string][]anCurrencyIncomeExpense, error) {
	out := map[string][]anCurrencyIncomeExpense{}

	for i := 0; i < len(ranges); i += 20 {
		j := i + 20

		if j > len(ranges) {
			j = len(ranges)
		}

		parts := make([]string, 0, j-i)
		names := map[string]string{}

		for k, r := range ranges[i:j] {
			wire := "p" + strconv.Itoa(i+k)
			names[wire] = r.Name
			parts = append(parts, wire+"_"+strconv.FormatInt(r.StartUnix, 10)+"_"+strconv.FormatInt(r.EndUnix, 10))
		}

		q := anExcludeQuery(sel, useTransactionTimezone)
		q.Set("query", strings.Join(parts, "|"))

		var resp map[string]*models.TransactionAmountsResponseItem

		if err := mc.CallUpstreamInto(api.Transactions.TransactionAmountsHandler, "GET", q, nil, &resp); err != nil {
			return nil, err
		}

		for wire, item := range resp {
			if item == nil {
				continue
			}

			parsed, err := anParseAmountInfos(item.Amounts)

			if err != nil {
				return nil, err
			}

			out[names[wire]] = parsed
		}
	}

	return out, nil
}

// anDailyAmounts is one day of transactions/amounts/daily.json
type anDailyAmounts struct {
	Date    time.Time
	Amounts []anCurrencyIncomeExpense
}

// anFetchDaily calls transactions/amounts/daily.json for a unix range
func anFetchDaily(mc *Ctx, sel *anSelection, startUnix, endUnix int64, useTransactionTimezone bool) ([]anDailyAmounts, error) {
	q := anExcludeQuery(sel, useTransactionTimezone)
	q.Set("start_time", strconv.FormatInt(startUnix, 10))
	q.Set("end_time", strconv.FormatInt(endUnix, 10))

	var resp []*models.TransactionDailyAmountsResponseItem

	if err := mc.CallUpstreamInto(api.Transactions.TransactionDailyAmountsHandler, "GET", q, nil, &resp); err != nil {
		return nil, err
	}

	out := make([]anDailyAmounts, 0, len(resp))

	for _, d := range resp {
		if d == nil {
			continue
		}

		day, err := time.ParseInLocation("2006-01-02", d.Date, mc.Loc)

		if err != nil {
			return nil, NewFail(CodeUpstreamError, "read log/ezbookkeeping.log for the server-side detail", "upstream returned an unreadable date %q", d.Date)
		}

		amounts, err := anParseAmountInfos(d.Amounts)

		if err != nil {
			return nil, err
		}

		out = append(out, anDailyAmounts{Date: day, Amounts: amounts})
	}

	return out, nil
}

// anDayBalance is one account's opening and closing balance on one day (asset_trends.json)
type anDayBalance struct {
	YMD     int32
	Opening int64
	Closing int64
}

// anBalanceTimeline holds each account's days with activity, in date order
type anBalanceTimeline struct {
	byAccount map[int64][]anDayBalance
}

// anFetchAssetTrends calls transactions/statistics/asset_trends.json for a day range
func anFetchAssetTrends(mc *Ctx, startDay, endDay time.Time) (*anBalanceTimeline, error) {
	startUnix, _ := anDayUnixRange(startDay)
	_, endUnix := anDayUnixRange(endDay)

	q := url.Values{}
	q.Set("start_time", strconv.FormatInt(startUnix, 10))
	q.Set("end_time", strconv.FormatInt(endUnix, 10))

	var resp []*models.TransactionStatisticAssetTrendsResponseItem

	if err := mc.CallUpstreamInto(api.Transactions.TransactionStatisticsAssetTrendsHandler, "GET", q, nil, &resp); err != nil {
		return nil, err
	}

	tl := &anBalanceTimeline{byAccount: map[int64][]anDayBalance{}}

	for _, day := range resp {
		if day == nil {
			continue
		}

		ymd := day.Year*10000 + day.Month*100 + day.Day

		for _, it := range day.Items {
			if it == nil {
				continue
			}

			opening, err := ParseAmountString(it.AccountOpeningBalance)

			if err != nil {
				return nil, err
			}

			closing, err := ParseAmountString(it.AccountClosingBalance)

			if err != nil {
				return nil, err
			}

			tl.byAccount[it.AccountId] = append(tl.byAccount[it.AccountId], anDayBalance{YMD: ymd, Opening: opening, Closing: closing})
		}
	}

	for id := range tl.byAccount {
		days := tl.byAccount[id]
		sort.Slice(days, func(i, j int) bool { return days[i].YMD < days[j].YMD })
	}

	return tl, nil
}

// ClosingAt is an account's balance at the end of day ymd. Upstream reports a day only when the
// account moved (plus, on the first day of the range, the carried balance of accounts that did not),
// so the balance on a quiet day is the closing balance of the last active day before it; before the
// first reported day it is that day's opening balance; with no reported day at all it is zero (the
// account had no balance in or before the range).
func (tl *anBalanceTimeline) ClosingAt(accountId int64, ymd int32) int64 {
	days := tl.byAccount[accountId]

	if len(days) == 0 {
		return 0
	}

	i := sort.Search(len(days), func(i int) bool { return days[i].YMD > ymd })

	if i == 0 {
		return days[0].Opening
	}

	return days[i-1].Closing
}

// OpeningAt is an account's balance at the start of day ymd
func (tl *anBalanceTimeline) OpeningAt(accountId int64, ymd int32) int64 {
	days := tl.byAccount[accountId]

	if len(days) == 0 {
		return 0
	}

	i := sort.Search(len(days), func(i int) bool { return days[i].YMD >= ymd })

	if i < len(days) && days[i].YMD == ymd {
		return days[i].Opening
	}

	if i == 0 {
		return days[0].Opening
	}

	return days[i-1].Closing
}

// Rows is how many account-days upstream reported
func (tl *anBalanceTimeline) Rows() int {
	n := 0

	for _, d := range tl.byAccount {
		n += len(d)
	}

	return n
}

// anListFilter narrows transactions/list/all.json
type anListFilter struct {
	StartUnix   int64
	EndUnix     int64
	Type        models.TransactionType // 0 = every type
	AccountIds  []int64
	CategoryIds []int64
	TagFilter   string
}

// anFetchAllTransactions calls transactions/list/all.json, which walks every page upstream-side
func anFetchAllTransactions(mc *Ctx, f anListFilter) ([]*models.TransactionInfoResponse, error) {
	q := url.Values{}
	q.Set("trim_account", "true")
	q.Set("trim_category", "true")
	q.Set("trim_tag", "true")

	if f.StartUnix > 0 {
		q.Set("start_time", strconv.FormatInt(f.StartUnix, 10))
	}

	if f.EndUnix > 0 {
		q.Set("end_time", strconv.FormatInt(f.EndUnix, 10))
	}

	if f.Type > 0 {
		q.Set("type", strconv.Itoa(int(f.Type)))
	}

	if len(f.AccountIds) > 0 {
		q.Set("account_ids", anJoinIds(f.AccountIds))
	}

	if len(f.CategoryIds) > 0 {
		q.Set("category_ids", anJoinIds(f.CategoryIds))
	}

	if f.TagFilter != "" {
		q.Set("tag_filter", f.TagFilter)
	}

	var resp []*models.TransactionInfoResponse

	if err := mc.CallUpstreamInto(api.Transactions.TransactionListAllHandler, "GET", q, nil, &resp); err != nil {
		return nil, err
	}

	out := resp[:0]

	for _, t := range resp {
		if t != nil {
			out = append(out, t)
		}
	}

	return out, nil
}

// anFetchScheduledTemplates reads the scheduled-transaction templates, or nil when the feature is off
func anFetchScheduledTemplates(mc *Ctx) ([]*models.TransactionTemplateInfoResponse, bool, error) {
	if !mc.Config.EnableScheduledTransaction {
		return nil, false, nil
	}

	var resp []*models.TransactionTemplateInfoResponse
	q := url.Values{"templateType": {strconv.Itoa(int(models.TRANSACTION_TEMPLATE_TYPE_SCHEDULE))}}

	if err := mc.CallUpstreamInto(api.TransactionTemplates.TemplateListHandler, "GET", q, nil, &resp); err != nil {
		return nil, false, err
	}

	return resp, true, nil
}

func anSortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))

	for k := range m {
		out = append(out, k)
	}

	sort.Strings(out)

	return out
}

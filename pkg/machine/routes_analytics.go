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

// routes_analytics.go — the analytics plane (apis.mdx §12, §13): every /analytics/* route plus
// GET /accounts/:id/balance and GET /accounts/:id/balance-history. Read tier, composed (R1's named
// exception), chart-ready: series[] of {key, label, currency, points[{period, amount}]} plus
// provenance. Amounts are signed integer hundredths (negative is money out, §17.2), per currency
// unless convert_to, where every rate is named (§13.2).

func init() {
	registerRoutes(analyticsRoutes)
}

var anUntrusted = []string{"label", "name", "comment", "variants", "text"}

func analyticsRoutes() []RouteDef {
	a := func(path, summary string, h HandlerFunc, features ...string) RouteDef {
		return RouteDef{
			Method:    "GET",
			Path:      path,
			Tier:      TierRead,
			Summary:   summary,
			Handler:   h,
			Composed:  true,
			Untrusted: anUntrusted,
			Features:  append([]string{"analytics", "currency.convert"}, features...),
		}
	}

	return []RouteDef{
		a("/analytics/period-summary", "Income and expense per named period (today, this_week, this_month, this_year, last_month, custom …), per currency — the Overview page.", anHandlePeriodSummary),
		a("/analytics/spending-by-category", "Expense by primary or secondary category (or account), per interval, with top_n and an 'other' bucket — the pie / bar chart.", anHandleSpendingByCategory),
		a("/analytics/income-by-category", "Income by primary or secondary category (or account), per interval, with top_n and an 'other' bucket.", anHandleIncomeByCategory),
		a("/analytics/income-vs-expense", "Income, expense and net per interval (day, week, month, quarter, year).", anHandleIncomeVsExpense),
		a("/analytics/cash-flow", "Opening balance, money in, money out and closing balance per interval, for a set of accounts.", anHandleCashFlow),
		a("/analytics/net-worth", "Assets, liabilities and net worth per interval, and each account's closing balance.", anHandleNetWorth),
		a("/analytics/category-trend", "One category (with its sub-categories) over time, with mean and median.", anHandleCategoryTrend),
		a("/analytics/tag-breakdown", "Spending and income per tag — what the trip cost.", anHandleTagBreakdown),
		a("/analytics/payee-leaderboard", "Who got the money, grouped by normalised transaction comment (ezBookkeeping has no payee entity), with the raw variants merged.", anHandlePayeeLeaderboard),
		a("/analytics/recurring", "Detected repeating charges (and income) not yet scheduled, their cadence, evidence and annualised cost.", anHandleRecurring),
		a("/analytics/anomalies", "Categories unusually far from their own trailing monthly norm, with the evidence.", anHandleAnomalies),
		a("/analytics/runway", "Liquid assets divided by the trailing average monthly net outflow, in months.", anHandleRunway),
		a("/analytics/import-fallout", "Rows an import placed in its fallback category, by account and month, so they can be re-categorised.", anHandleImportFallout, "ingest.fallout"),
		{
			Method: "GET", Path: "/accounts/:id/balance", Tier: TierRead, Composed: true, Untrusted: anUntrusted,
			Summary: "One account's balance as of a date (default today), composed from the daily asset trend.",
			Handler: anHandleAccountBalance,
		},
		{
			Method: "GET", Path: "/accounts/:id/balance-history", Tier: TierRead, Composed: true, Untrusted: anUntrusted,
			Summary: "One account's closing balance over time (day, week or month).",
			Handler: anHandleBalanceHistory, Features: []string{"analytics"},
		},
	}
}

// anCall is the state of one analytics call: arguments, selection, converter, provenance
type anCall struct {
	mc      *Ctx
	args    *anArgs
	sel     *anSelection
	cv      *anConverter
	sources map[string]bool
	rows    int
	filters map[string]any
	notes   []string
	group   *anGroupStats
}

// anBegin validates the query, parses the shared arguments, resolves the selection and, when asked
// for, loads the converter
func anBegin(mc *Ctx, extras []string, defStart, defEnd string, includeHiddenDefault bool) (*anCall, error) {
	if err := anCheckQuery(mc, extras...); err != nil {
		return nil, err
	}

	args, err := anParseArgs(mc, defStart, defEnd)

	if err != nil {
		return nil, err
	}

	sel, err := anResolveSelection(mc, args, includeHiddenDefault)

	if err != nil {
		return nil, err
	}

	c := &anCall{mc: mc, args: args, sel: sel, sources: map[string]bool{
		"GET /api/v1/accounts/list.json":               true,
		"GET /api/v1/transaction/categories/list.json": true,
	}, filters: map[string]any{}}

	if args.ConvertTo != "" {
		cv, err := anLoadConverter(mc, args.ConvertTo)

		if err != nil {
			return nil, err
		}

		c.cv = cv
		c.source("GET /api/v1/exchange_rates/latest.json")
	}

	if includeHiddenDefault {
		c.args.IncludeHiddenAccounts = true
	}

	return c, nil
}

func (c *anCall) source(s ...string) {
	for _, x := range s {
		c.sources[x] = true
	}
}

func (c *anCall) note(s string) {
	c.notes = append(c.notes, s)
}

// rejectTransfers refuses include_transfers on routes where transfers are not income or expense
func (c *anCall) rejectTransfers(route string) error {
	if c.args.IncludeTransfers {
		return Invalid("transfers are neither income nor expense; use GET /machine/v1/analytics/cash-flow to see money moving between accounts", "%s does not take include_transfers", route)
	}

	return nil
}

// rejectCategoryFilters refuses category and tag filters on account-balance routes, where they
// cannot hold: a balance is every transaction of the account
func (c *anCall) rejectCategoryFilters(route string) error {
	if len(c.args.CategoryIds) > 0 || len(c.args.ExcludeCategoryIds) > 0 || c.args.TagFilter != "" {
		return Invalid("balances include every transaction of an account; use GET /machine/v1/analytics/income-vs-expense or spending-by-category to filter by category or tag", "%s does not take category_ids, exclude_category_ids or tag_filter", route)
	}

	return nil
}

// provenance is the block every analytics response carries (§12.3)
func (c *anCall) provenance() map[string]any {
	filters := map[string]any{
		"categoryIds":            anIdStrings(c.args.CategoryIds),
		"excludeCategoryIds":     anIdStrings(c.args.ExcludeCategoryIds),
		"tagFilter":              c.args.TagFilter,
		"includeHiddenAccounts":  c.args.IncludeHiddenAccounts,
		"includeTransfers":       c.args.IncludeTransfers,
		"useTransactionTimezone": c.args.UseTransactionTimezone,
		"balanceModifications":   "excluded",
	}

	for k, v := range c.filters {
		filters[k] = v
	}

	p := map[string]any{
		"range":                    c.args.Range,
		"timezone":                 c.mc.Loc.String(),
		"accountIds":               c.sel.AccountIdStrings(),
		"accountsExplicit":         c.sel.ExplicitAccounts,
		"hiddenAccountsExcluded":   anIdStrings(c.sel.HiddenExcluded),
		"filters":                  filters,
		"rowCount":                 c.rows,
		"upstream":                 anSortedKeys(c.sources),
		"rates":                    []anRateInfo{},
		"signConvention":           "signed: money in is positive, money out is negative",
		"amountUnit":               "integer hundredths of the currency named beside each figure",
		"absentPeriodsAreOmitted":  true,
		"convertedFiguresRounding": nil,
	}

	if c.group != nil {
		p["grouping"] = c.group
	}

	if c.cv != nil {
		cp := c.cv.Provenance()

		for k, v := range cp {
			p[k] = v
		}

		p["convertedFiguresRounding"] = "each currency's sum is converted once and rounded half away from zero; the sum of converted points can differ from a converted total by rounding"

		if c.cv.Partial() {
			c.mc.SetMeta("partial", true)
		}
	}

	if len(c.notes) > 0 {
		p["notes"] = c.notes
	}

	return p
}

// finishSeries converts (when asked), ranks with top_n and renders points in period order
func (c *anCall) finishSeries(series []*anSeries, order []string, topN int) ([]*anSeries, []*anSeries, error) {
	var unconverted []*anSeries

	if c.cv != nil {
		conv, un, err := anConvertSeries(series, c.cv)

		if err != nil {
			return nil, nil, err
		}

		series, unconverted = conv, un
	}

	ranked, err := anRankTopN(series, topN)

	if err != nil {
		return nil, nil, err
	}

	rankedUn, err := anRankTopN(unconverted, topN)

	if err != nil {
		return nil, nil, err
	}

	return anFinalizeAll(ranked, order), anFinalizeAll(rankedUn, order), nil
}

// finishOrdered converts (when asked) and renders points, keeping the given series order (for
// series whose order means something: income, expense, net; opening … closing)
func (c *anCall) finishOrdered(series []*anSeries, order []string) ([]*anSeries, []*anSeries, error) {
	var unconverted []*anSeries

	if c.cv != nil {
		conv, un, err := anConvertSeries(series, c.cv)

		if err != nil {
			return nil, nil, err
		}

		series, unconverted = conv, un
	}

	anSortSeriesStable(series)
	anSortSeriesStable(unconverted)

	return anFinalizeAll(series, order), anFinalizeAll(unconverted, order), nil
}

// anSortSeriesStable orders series by currency, keeping their relative order within a currency
func anSortSeriesStable(series []*anSeries) {
	sort.SliceStable(series, func(i, j int) bool { return series[i].Currency < series[j].Currency })
}

// totals renders the totals block: per currency, or converted once per currency and added
func (c *anCall) totals(native []*anSeries) (map[string]any, error) {
	t, err := anNativeTotals(native)

	if err != nil {
		return nil, err
	}

	if c.cv == nil {
		return map[string]any{"totals": anTotalsList(t)}, nil
	}

	sum, sources, unconverted, err := anConvertTotals(t, c.cv)

	if err != nil {
		return nil, err
	}

	out := map[string]any{
		"totals":           []anCurrencyAmount{{Currency: c.cv.Target, Amount: sum}},
		"totalsByCurrency": sources,
	}

	if sources == nil {
		out["totals"] = []anCurrencyAmount{}
		out["totalsByCurrency"] = []anCurrencyAmount{}
	}

	if unconverted == nil {
		unconverted = []anCurrencyAmount{}
	}

	out["unconvertedTotals"] = unconverted

	return out, nil
}

// anCloneSeriesList deep-copies series (so a native copy survives conversion for totals)
func anCloneSeriesList(series []*anSeries) []*anSeries {
	out := make([]*anSeries, len(series))

	for i, s := range series {
		cp := *s
		cp.points = make(map[string]int64, len(s.points))

		for k, v := range s.points {
			cp.points[k] = v
		}

		cp.ByCurrency = append([]anCurrencyAmount(nil), s.ByCurrency...)
		cp.Members = append([]string(nil), s.Members...)

		if len(cp.Members) == 0 {
			cp.Members = nil
		}

		if len(cp.ByCurrency) == 0 {
			cp.ByCurrency = nil
		}

		out[i] = &cp
	}

	return out
}

// ---------------------------------------------------------------------------------------------
// /analytics/spending-by-category and /analytics/income-by-category

func anHandleSpendingByCategory(mc *Ctx) (any, error) {
	return anCategoryBreakdown(mc, anKindExpense)
}

func anHandleIncomeByCategory(mc *Ctx) (any, error) {
	return anCategoryBreakdown(mc, anKindIncome)
}

func anCategoryBreakdown(mc *Ctx, direction anKind) (any, error) {
	ds, de := anDefaultYearToDate(mc.Loc)
	c, err := anBegin(mc, []string{"interval", "group_by", "top_n", "include_zero"}, ds, de, false)

	if err != nil {
		return nil, err
	}

	interval, err := anParseEnum(mc, "interval", anIntervalNone, anIntervalNone, anIntervalMonth, anIntervalQuarter, anIntervalYear)

	if err != nil {
		return nil, err
	}

	groupBy, err := anParseEnum(mc, "group_by", anGroupPrimary, anGroupPrimary, anGroupSecondary, anGroupAccount)

	if err != nil {
		return nil, err
	}

	topN, err := anParseIntArg(mc, "top_n", 0, 0, 1000)

	if err != nil {
		return nil, err
	}

	includeZero, err := mc.QueryBool("include_zero", false)

	if err != nil {
		return nil, err
	}

	kinds := map[anKind]bool{direction: true}
	catTypes := map[models.TransactionCategoryType]bool{}

	if direction == anKindExpense {
		catTypes[models.CATEGORY_TYPE_EXPENSE] = true

		if c.args.IncludeTransfers {
			kinds[anKindTransferOut] = true
			catTypes[models.CATEGORY_TYPE_TRANSFER] = true
		}
	} else {
		catTypes[models.CATEGORY_TYPE_INCOME] = true

		if c.args.IncludeTransfers {
			kinds[anKindTransferIn] = true
			catTypes[models.CATEGORY_TYPE_TRANSFER] = true
		}
	}

	b := newAnBucketer(interval, c.args.Range, mc.Loc, anWeekday(mc))
	facts, sources, err := anFetchCategoryFacts(mc, c.args.Range, b, c.args.TagFilter, c.args.UseTransactionTimezone)

	if err != nil {
		return nil, err
	}

	c.source(sources...)
	c.rows = len(facts)

	series, st, err := anGroupFacts(facts, c.sel, anGroupOpts{GroupBy: groupBy, Kinds: kinds})

	if err != nil {
		return nil, err
	}

	c.group = &st

	if includeZero && groupBy != anGroupAccount {
		fallback := ""

		if mc.User != nil {
			fallback = mc.User.DefaultCurrency
		}

		series = anIncludeZeroCategories(series, c.sel, groupBy, catTypes, fallback)
	}

	native := anCloneSeriesList(series)
	periods := b.Periods()
	out, unconverted, err := c.finishSeries(series, anPeriodOrder(periods), int(topN))

	if err != nil {
		return nil, err
	}

	c.filters["groupBy"] = groupBy
	c.filters["interval"] = interval
	c.filters["topN"] = topN
	c.filters["includeZero"] = includeZero

	if c.args.IncludeTransfers {
		c.note("transfer categories are included: money moved to (spending) or from (income) another account counts, including transfers between two accounts of this selection")
	}

	data := map[string]any{
		"direction":   string(direction),
		"groupBy":     groupBy,
		"interval":    interval,
		"topN":        topN,
		"periods":     periods,
		"series":      out,
		"unconverted": unconverted,
	}

	tot, err := c.totals(native)

	if err != nil {
		return nil, err
	}

	for k, v := range tot {
		data[k] = v
	}

	data["provenance"] = c.provenance()

	return data, nil
}

// ---------------------------------------------------------------------------------------------
// /analytics/period-summary

// anNamedPeriods are the period names the Overview page shows (plus a few neighbours)
var anNamedPeriods = []string{"today", "yesterday", "this_week", "last_week", "this_month", "last_month", "this_year", "last_year", "custom"}

// anResolveNamedPeriod turns a period name into a date range in loc. "custom" is start..end.
func anResolveNamedPeriod(name string, today time.Time, fdow time.Weekday, custom *DateRange) (string, string, bool) {
	loc := today.Location()
	weekStart := today.AddDate(0, 0, -((int(today.Weekday()) - int(fdow) + 7) % 7))
	monthStart := time.Date(today.Year(), today.Month(), 1, 0, 0, 0, 0, loc)
	yearStart := time.Date(today.Year(), 1, 1, 0, 0, 0, 0, loc)

	switch name {
	case "today":
		return anDateString(today), anDateString(today), true
	case "yesterday":
		y := today.AddDate(0, 0, -1)
		return anDateString(y), anDateString(y), true
	case "this_week":
		return anDateString(weekStart), anDateString(weekStart.AddDate(0, 0, 6)), true
	case "last_week":
		return anDateString(weekStart.AddDate(0, 0, -7)), anDateString(weekStart.AddDate(0, 0, -1)), true
	case "this_month":
		return anDateString(monthStart), anDateString(monthStart.AddDate(0, 1, -1)), true
	case "last_month":
		return anDateString(monthStart.AddDate(0, -1, 0)), anDateString(monthStart.AddDate(0, 0, -1)), true
	case "this_year":
		return anDateString(yearStart), anDateString(yearStart.AddDate(1, 0, -1)), true
	case "last_year":
		return anDateString(yearStart.AddDate(-1, 0, 0)), anDateString(yearStart.AddDate(0, 0, -1)), true
	case "custom":
		if custom == nil {
			return "", "", false
		}

		return custom.Start, custom.End, true
	}

	return "", "", false
}

func anHandlePeriodSummary(mc *Ctx) (any, error) {
	ds, de := anDefaultYearToDate(mc.Loc)
	c, err := anBegin(mc, []string{"periods"}, ds, de, false)

	if err != nil {
		return nil, err
	}

	if err := c.rejectTransfers("period-summary"); err != nil {
		return nil, err
	}

	names := mc.QueryList("periods")

	if len(names) == 0 {
		names = []string{"today", "this_week", "this_month", "this_year"}
	}

	today := anToday(mc.Loc)
	fdow := anWeekday(mc)
	var periods []anPeriod
	var ranges []anNamedRange
	seen := map[string]bool{}

	for _, raw := range names {
		name := strings.ToLower(raw)

		if seen[name] {
			continue
		}

		seen[name] = true
		start, end, ok := anResolveNamedPeriod(name, today, fdow, c.args.Range)

		if !ok {
			return nil, Invalid("pass periods as any of: "+strings.Join(anNamedPeriods, ", ")+" (custom uses start and end)", "unknown period %q", raw)
		}

		rng, err := ParseDateRange(start, end, "", "", mc.Loc)

		if err != nil {
			return nil, err
		}

		periods = append(periods, anPeriod{Period: name, Start: rng.Start, End: rng.End})
		ranges = append(ranges, anNamedRange{Name: name, StartUnix: rng.StartUnix, EndUnix: rng.EndUnix})
	}

	if len(periods) > 40 {
		return nil, Invalid("ask for 40 periods or fewer", "%d periods requested", len(periods))
	}

	set := newAnSeriesSet()
	income := func(cur string) *anSeries {
		return set.get("income", cur, func(s *anSeries) { s.Label, s.Kind = "Income", "income" })
	}
	expense := func(cur string) *anSeries {
		return set.get("expense", cur, func(s *anSeries) { s.Label, s.Kind = "Expense", "expense" })
	}

	if c.args.TagFilter == "" {
		// transactions/amounts.json: every period in one call, filters as upstream exclude lists
		amounts, err := anFetchAmounts(mc, c.sel, ranges, c.args.UseTransactionTimezone)

		if err != nil {
			return nil, err
		}

		c.source("GET /api/v1/transactions/amounts.json")

		for _, p := range periods {
			for _, a := range amounts[p.Period] {
				c.rows++

				if a.Income != 0 {
					if err := income(a.Currency).add(p.Period, a.Income); err != nil {
						return nil, err
					}
				}

				if a.Expense != 0 {
					if err := expense(a.Currency).add(p.Period, -a.Expense); err != nil {
						return nil, err
					}
				}
			}
		}
	} else {
		// a tag filter needs the statistics endpoint, one call per period
		for i, p := range periods {
			items, err := anFetchStatistics(mc, ranges[i].StartUnix, ranges[i].EndUnix, c.args.TagFilter, c.args.UseTransactionTimezone)

			if err != nil {
				return nil, err
			}

			facts := make([]anFact, 0, len(items))

			for _, it := range items {
				facts = append(facts, anFact{Bucket: p.Period, anStatItem: it})
			}

			c.rows += len(facts)
			grouped, _, err := anGroupFacts(facts, c.sel, anGroupOpts{GroupBy: anGroupKind, Kinds: map[anKind]bool{anKindIncome: true, anKindExpense: true}})

			if err != nil {
				return nil, err
			}

			for _, g := range grouped {
				target := income(g.Currency)

				if g.Key == string(anKindExpense) {
					target = expense(g.Currency)
				}

				for per, v := range g.points {
					if err := target.add(per, v); err != nil {
						return nil, err
					}
				}
			}
		}

		c.source("GET /api/v1/transactions/statistics.json")
	}

	series, err := anWithNet(set.list())

	if err != nil {
		return nil, err
	}

	out, unconverted, err := c.finishOrdered(series, anPeriodOrder(periods))

	if err != nil {
		return nil, err
	}

	c.filters["periods"] = anPeriodOrder(periods)
	c.note("periods overlap (this_month is inside this_year), so they are never added to each other")

	return map[string]any{
		"periods":     periods,
		"series":      out,
		"unconverted": unconverted,
		"provenance":  c.provenance(),
	}, nil
}

// anWithNet appends a "net" series per currency: income plus (negative) expense, per period
func anWithNet(series []*anSeries) ([]*anSeries, error) {
	set := newAnSeriesSet()
	var ordered []*anSeries

	for _, s := range series {
		if s.Key != "income" && s.Key != "expense" {
			ordered = append(ordered, s)
			continue
		}

		ordered = append(ordered, s)
		net := set.get("net", s.Currency, func(n *anSeries) { n.Label, n.Kind = "Net", "net" })

		for p, v := range s.points {
			if err := net.add(p, v); err != nil {
				return nil, err
			}
		}
	}

	// order: income, expense, net within each currency
	rank := map[string]int{"income": 0, "expense": 1, "net": 2}
	all := append(ordered, set.list()...)

	sort.SliceStable(all, func(i, j int) bool {
		if all[i].Currency != all[j].Currency {
			return all[i].Currency < all[j].Currency
		}

		return rank[all[i].Key] < rank[all[j].Key]
	})

	return all, nil
}

// ---------------------------------------------------------------------------------------------
// /analytics/income-vs-expense

func anHandleIncomeVsExpense(mc *Ctx) (any, error) {
	ds, de := anDefaultTrailingMonths(mc.Loc, 12)
	c, err := anBegin(mc, []string{"interval"}, ds, de, false)

	if err != nil {
		return nil, err
	}

	if err := c.rejectTransfers("income-vs-expense"); err != nil {
		return nil, err
	}

	interval, err := anParseEnum(mc, "interval", anIntervalMonth, anIntervalNone, anIntervalDay, anIntervalWeek, anIntervalMonth, anIntervalQuarter, anIntervalYear)

	if err != nil {
		return nil, err
	}

	b := newAnBucketer(interval, c.args.Range, mc.Loc, anWeekday(mc))
	set := newAnSeriesSet()
	income := func(cur string) *anSeries {
		return set.get("income", cur, func(s *anSeries) { s.Label, s.Kind = "Income", "income" })
	}
	expense := func(cur string) *anSeries {
		return set.get("expense", cur, func(s *anSeries) { s.Label, s.Kind = "Expense", "expense" })
	}

	if c.args.TagFilter == "" {
		days, err := anFetchDaily(mc, c.sel, c.args.Range.StartUnix, c.args.Range.EndUnix, c.args.UseTransactionTimezone)

		if err != nil {
			return nil, err
		}

		c.source("GET /api/v1/transactions/amounts/daily.json")

		for _, d := range days {
			key := b.Key(d.Date)

			for _, a := range d.Amounts {
				c.rows++

				if a.Income != 0 {
					if err := income(a.Currency).add(key, a.Income); err != nil {
						return nil, err
					}
				}

				if a.Expense != 0 {
					if err := expense(a.Currency).add(key, -a.Expense); err != nil {
						return nil, err
					}
				}
			}
		}
	} else {
		if interval == anIntervalDay || interval == anIntervalWeek {
			return nil, Invalid("with tag_filter pass interval none, month, quarter or year (upstream's tag-aware statistics come in months)", "interval %s cannot be combined with tag_filter", interval)
		}

		facts, sources, err := anFetchCategoryFacts(mc, c.args.Range, b, c.args.TagFilter, c.args.UseTransactionTimezone)

		if err != nil {
			return nil, err
		}

		c.source(sources...)
		c.rows = len(facts)
		grouped, st, err := anGroupFacts(facts, c.sel, anGroupOpts{GroupBy: anGroupKind, Kinds: map[anKind]bool{anKindIncome: true, anKindExpense: true}})

		if err != nil {
			return nil, err
		}

		c.group = &st

		for _, g := range grouped {
			target := income(g.Currency)

			if g.Key == string(anKindExpense) {
				target = expense(g.Currency)
			}

			for per, v := range g.points {
				if err := target.add(per, v); err != nil {
					return nil, err
				}
			}
		}
	}

	series, err := anWithNet(set.list())

	if err != nil {
		return nil, err
	}

	native := anCloneSeriesList(series)
	periods := b.Periods()
	out, unconverted, err := c.finishOrdered(series, anPeriodOrder(periods))

	if err != nil {
		return nil, err
	}

	c.filters["interval"] = interval

	data := map[string]any{
		"interval":    interval,
		"periods":     periods,
		"series":      out,
		"unconverted": unconverted,
	}

	// totals per series key (income, expense, net), per currency or converted once each
	summary := map[string]any{}

	for _, key := range []string{"income", "expense", "net"} {
		var only []*anSeries

		for _, s := range native {
			if s.Key == key {
				only = append(only, s)
			}
		}

		tot, err := c.totals(only)

		if err != nil {
			return nil, err
		}

		summary[key] = tot
	}

	data["summary"] = summary
	data["provenance"] = c.provenance()

	return data, nil
}

// ---------------------------------------------------------------------------------------------
// /analytics/category-trend

func anHandleCategoryTrend(mc *Ctx) (any, error) {
	ds, de := anDefaultTrailingMonths(mc.Loc, 12)
	c, err := anBegin(mc, []string{"category_id", "interval"}, ds, de, false)

	if err != nil {
		return nil, err
	}

	if len(c.args.CategoryIds) > 0 {
		return nil, Invalid("pass the one category as category_id (its sub-categories are included)", "category-trend takes category_id, not category_ids")
	}

	catId, err := ResolveId("category_id", mc.Query("category_id"))

	if err != nil {
		return nil, err
	}

	cat := c.sel.Categories.Get(catId)

	if cat == nil {
		return nil, NotFound("GET /machine/v1/categories lists the category ids", "no category with id %s", idString(catId)).WithDetails(map[string]any{"id": idString(catId)})
	}

	interval, err := anParseEnum(mc, "interval", anIntervalMonth, anIntervalMonth, anIntervalQuarter, anIntervalYear)

	if err != nil {
		return nil, err
	}

	// narrow the selection to this category and its children (exclusions still apply)
	c.sel.IncludeCategories = map[int64]bool{cat.Id: true}

	for _, id := range cat.Children {
		c.sel.IncludeCategories[id] = true
	}

	b := newAnBucketer(interval, c.args.Range, mc.Loc, anWeekday(mc))
	facts, sources, err := anFetchCategoryFacts(mc, c.args.Range, b, c.args.TagFilter, c.args.UseTransactionTimezone)

	if err != nil {
		return nil, err
	}

	c.source(sources...)
	c.rows = len(facts)
	series, st, err := anGroupFacts(facts, c.sel, anGroupOpts{GroupBy: anGroupTotal})

	if err != nil {
		return nil, err
	}

	c.group = &st

	for _, s := range series {
		s.Key, s.Label, s.Kind, s.Id = idString(cat.Id), cat.Name, "category", idString(cat.Id)

		if cat.ParentId > 0 {
			s.ParentId = idString(cat.ParentId)
		}
	}

	periods := b.Periods()
	order := anPeriodOrder(periods)
	out, unconverted, err := c.finishOrdered(series, order)

	if err != nil {
		return nil, err
	}

	stats := func(list []*anSeries) []map[string]any {
		res := []map[string]any{}

		for _, s := range list {
			res = append(res, anTrendStats(s, order))
		}

		return res
	}

	c.filters["categoryId"] = idString(cat.Id)
	c.filters["interval"] = interval
	c.note("mean and median count every period of the range, a period with no transactions in the category as a counted zero; meanOfActivePeriods counts only periods with transactions; with no transactions at all both are null")

	return map[string]any{
		"category": map[string]any{
			"id": idString(cat.Id), "name": cat.Name, "parentId": idString(cat.ParentId), "type": int(cat.Type),
			"subCategoryIds": anIdStrings(cat.Children),
		},
		"interval":         interval,
		"periods":          periods,
		"series":           out,
		"unconverted":      unconverted,
		"stats":            stats(out),
		"unconvertedStats": stats(unconverted),
		"provenance":       c.provenance(),
	}, nil
}

// anTrendStats is a series' mean and median over the periods of the range
func anTrendStats(s *anSeries, order []string) map[string]any {
	values := make([]int64, 0, len(order))
	var active []int64
	var sum, activeSum int64

	for _, p := range order {
		v, ok := s.points[p]

		if ok {
			active = append(active, v)
			activeSum += v
		}

		values = append(values, v)
		sum += v
	}

	out := map[string]any{
		"key":                 s.Key,
		"currency":            s.Currency,
		"periods":             len(order),
		"periodsWithData":     len(active),
		"total":               s.Total,
		"mean":                nil,
		"median":              nil,
		"meanOfActivePeriods": nil,
		"min":                 nil,
		"max":                 nil,
	}

	if len(active) == 0 {
		return out
	}

	out["mean"] = anMeanRounded(sum, int64(len(values)))
	out["median"] = anMedian(values)
	out["meanOfActivePeriods"] = anMeanRounded(activeSum, int64(len(active)))

	lo, hi := values[0], values[0]

	for _, v := range values {
		if v < lo {
			lo = v
		}

		if v > hi {
			hi = v
		}
	}

	out["min"], out["max"] = lo, hi

	return out
}

// ---------------------------------------------------------------------------------------------
// /analytics/tag-breakdown

func anHandleTagBreakdown(mc *Ctx) (any, error) {
	ds, de := anDefaultYearToDate(mc.Loc)
	c, err := anBegin(mc, []string{"tag_ids", "group_by"}, ds, de, false)

	if err != nil {
		return nil, err
	}

	tagIds, err := anParseIdList(mc, "tag_ids")

	if err != nil {
		return nil, err
	}

	if len(tagIds) == 0 {
		return nil, Invalid("pass tag_ids (GET /machine/v1/tags lists them)", "tag_ids is required")
	}

	if len(tagIds) > 50 {
		return nil, Invalid("ask for 50 tags or fewer per call", "%d tags requested", len(tagIds))
	}

	groupBy, err := anParseEnum(mc, "group_by", anGroupKind, anGroupKind, anGroupPrimary, anGroupSecondary, anGroupAccount)

	if err != nil {
		return nil, err
	}

	var tags []*models.TransactionTagInfoResponse

	if err := mc.CallUpstreamInto(api.TransactionTags.TagListHandler, "GET", url.Values{}, nil, &tags); err != nil {
		return nil, err
	}

	c.source("GET /api/v1/transaction/tags/list.json")
	tagById := map[int64]*models.TransactionTagInfoResponse{}

	for _, t := range tags {
		if t != nil {
			tagById[t.Id] = t
		}
	}

	kinds := map[anKind]bool{anKindIncome: true, anKindExpense: true}

	if c.args.IncludeTransfers {
		kinds[anKindTransferIn], kinds[anKindTransferOut] = true, true
	}

	rangeKey := newAnBucketer(anIntervalNone, c.args.Range, mc.Loc, anWeekday(mc)).rangeKey()
	var all []*anSeries
	var tagInfo []map[string]any
	stTotal := anGroupStats{}

	for _, id := range tagIds {
		t := tagById[id]

		if t == nil {
			return nil, NotFound("GET /machine/v1/tags lists the tag ids", "no tag with id %s", idString(id)).WithDetails(map[string]any{"id": idString(id)})
		}

		filter := "0:" + strconv.FormatInt(id, 10)

		if c.args.TagFilter != "" && c.args.TagFilter != models.TransactionNoTagFilterValue {
			filter = c.args.TagFilter + ";" + filter
		}

		items, err := anFetchStatistics(mc, c.args.Range.StartUnix, c.args.Range.EndUnix, filter, c.args.UseTransactionTimezone)

		if err != nil {
			return nil, err
		}

		facts := make([]anFact, 0, len(items))

		for _, it := range items {
			facts = append(facts, anFact{Bucket: rangeKey, anStatItem: it})
		}

		c.rows += len(facts)
		grouped, st, err := anGroupFacts(facts, c.sel, anGroupOpts{GroupBy: groupBy, Kinds: kinds})

		if err != nil {
			return nil, err
		}

		stTotal.RowsUsed += st.RowsUsed
		stTotal.RowsOutsideFilters += st.RowsOutsideFilters
		stTotal.RowsUnknownEntities += st.RowsUnknownEntities

		for _, g := range grouped {
			sub := g.Key
			g.Key = "tag:" + idString(id) + "|" + sub
			g.Label = t.Name + " — " + g.Label
			g.ParentId = idString(id)
			all = append(all, g)
		}

		tagInfo = append(tagInfo, map[string]any{"id": idString(id), "name": t.Name, "hidden": t.Hidden})
	}

	c.source("GET /api/v1/transactions/statistics.json")
	c.group = &stTotal
	native := anCloneSeriesList(all)
	out, unconverted, err := c.finishOrdered(all, []string{rangeKey})

	if err != nil {
		return nil, err
	}

	// per-tag totals (per currency, or converted once per currency)
	perTag := []map[string]any{}

	for _, ti := range tagInfo {
		var only []*anSeries

		for _, s := range native {
			if s.ParentId == ti["id"] {
				only = append(only, s)
			}
		}

		byKind := map[string]any{}

		for _, k := range []anKind{anKindIncome, anKindExpense, anKindTransferIn, anKindTransferOut} {
			var kindOnly []*anSeries

			for _, s := range only {
				if anSeriesKind(s, groupBy) == k {
					kindOnly = append(kindOnly, s)
				}
			}

			if len(kindOnly) == 0 {
				continue
			}

			tot, err := c.totals(kindOnly)

			if err != nil {
				return nil, err
			}

			byKind[string(k)] = tot
		}

		net, err := c.totals(only)

		if err != nil {
			return nil, err
		}

		entry := map[string]any{"tag": ti, "net": net}

		if groupBy == anGroupKind {
			entry["byKind"] = byKind
		}

		perTag = append(perTag, entry)
	}

	c.filters["tagIds"] = anIdStrings(tagIds)
	c.filters["groupBy"] = groupBy
	c.note("a transaction carrying several of the requested tags counts under each of them, so tag figures are not additive across tags")

	return map[string]any{
		"groupBy":     groupBy,
		"periods":     []anPeriod{{Period: rangeKey, Start: c.args.Range.Start, End: c.args.Range.End}},
		"tags":        perTag,
		"series":      out,
		"unconverted": unconverted,
		"provenance":  c.provenance(),
	}, nil
}

// anSeriesKind recovers the kind of a series grouped by kind (other groupings mix kinds)
func anSeriesKind(s *anSeries, groupBy string) anKind {
	if groupBy != anGroupKind {
		return ""
	}

	return anKind(s.Kind)
}

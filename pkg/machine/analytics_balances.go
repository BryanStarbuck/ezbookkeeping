package machine

import (
	"sort"
	"strings"
	"time"

	"github.com/mayswind/ezbookkeeping/pkg/models"
)

// analytics_balances.go — the balance-based analytics: cash-flow, net-worth, runway, and one
// account's balance / balance history. Balances come from upstream's daily asset trend
// (statistics/asset_trends.json), which is the app's own running balance per account per day
// (R7: the app decides, we report). Balances are signed as the app stores them: a liability that is
// owed is negative.

const anAssetTrendsSource = "GET /api/v1/transactions/statistics/asset_trends.json"
const anListAllSource = "GET /api/v1/transactions/list/all.json"

// anBalanceIntervals are the intervals a balance series can be cut into
var anBalanceIntervals = []string{anIntervalDay, anIntervalWeek, anIntervalMonth, anIntervalQuarter, anIntervalYear}

// anPeriodEndYMD / anPeriodStartYMD read a clipped period's bounds as numeric dates
func anPeriodEndYMD(p anPeriod, loc *time.Location) int32 {
	return anYMD(anDayStart(p.End, loc))
}

func anPeriodStartYMD(p anPeriod, loc *time.Location) int32 {
	return anYMD(anDayStart(p.Start, loc))
}

// ---------------------------------------------------------------------------------------------
// /analytics/cash-flow

func anHandleCashFlow(mc *Ctx) (any, error) {
	ds, de := anDefaultTrailingMonths(mc.Loc, 12)
	c, err := anBegin(mc, []string{"interval"}, ds, de, false)

	if err != nil {
		return nil, err
	}

	if err := c.rejectCategoryFilters("cash-flow"); err != nil {
		return nil, err
	}

	if c.args.UseTransactionTimezone {
		return nil, Invalid("cash flow follows the call's timezone (X-Timezone-Name), like the balances it reconciles to; omit use_transaction_timezone", "cash-flow does not take use_transaction_timezone")
	}

	interval, err := anParseEnum(mc, "interval", anIntervalMonth, append([]string{anIntervalNone}, anBalanceIntervals...)...)

	if err != nil {
		return nil, err
	}

	b := newAnBucketer(interval, c.args.Range, mc.Loc, anWeekday(mc))
	periods := b.Periods()

	tl, err := anFetchAssetTrends(mc, b.RangeStart, b.RangeEnd)

	if err != nil {
		return nil, err
	}

	rows, err := anFetchAllTransactions(mc, anListFilter{StartUnix: c.args.Range.StartUnix, EndUnix: c.args.Range.EndUnix})

	if err != nil {
		return nil, err
	}

	c.source(anAssetTrendsSource, anListAllSource)
	c.rows = tl.Rows() + len(rows)

	flows, internal, err := anCashFlows(rows, c.sel, b, mc.Loc, c.args.IncludeTransfers)

	if err != nil {
		return nil, err
	}

	series, err := anCashFlowSeries(flows, tl, c.sel, periods, mc.Loc)

	if err != nil {
		return nil, err
	}

	out, unconverted, err := c.finishOrdered(series, anPeriodOrder(periods))

	if err != nil {
		return nil, err
	}

	c.filters["interval"] = interval

	if c.args.IncludeTransfers {
		c.note("transfers between two accounts of the selection in the same currency are counted on both sides (out of one, into the other)")
	} else {
		c.note("transfers between two accounts of the selection in the same currency are left out; transfers that cross the selection or change currency are counted")
	}

	c.note("adjustments = closing − opening − in − out: balance modifications and anything else that moved a balance without being income, expense or a transfer")

	return map[string]any{
		"interval":                  interval,
		"periods":                   periods,
		"series":                    out,
		"unconverted":               unconverted,
		"internalTransfersExcluded": internal,
		"provenance":                c.provenance(),
	}, nil
}

// anFlow is money in and out of the selection in one period and currency
type anFlow struct {
	Income       int64
	Expense      int64 // negative
	TransfersIn  int64
	TransfersOut int64 // negative
}

// anCashFlows buckets transactions into per-period, per-currency flows for the selection.
// Returns the flows and how many internal transfers were left out.
func anCashFlows(rows []*models.TransactionInfoResponse, sel *anSelection, b *anBucketer, loc *time.Location, includeInternal bool) (map[string]map[string]*anFlow, int, error) {
	flows := map[string]map[string]*anFlow{}
	internal := 0

	get := func(period, currency string) *anFlow {
		m, ok := flows[period]

		if !ok {
			m = map[string]*anFlow{}
			flows[period] = m
		}

		f, ok := m[currency]

		if !ok {
			f = &anFlow{}
			m[currency] = f
		}

		return f
	}

	add := func(dst *int64, v int64) error {
		s, err := CheckedAdd(*dst, v)

		if err != nil {
			return err
		}

		*dst = s

		return nil
	}

	for _, t := range rows {
		period := b.Key(time.Unix(t.Time, 0).In(loc))
		src := sel.Accounts.Get(t.SourceAccountId)

		switch t.Type {
		case models.TRANSACTION_TYPE_INCOME:
			if src != nil && sel.AccountIncluded(src.Id) {
				if err := add(&get(period, src.Currency).Income, t.SourceAmount); err != nil {
					return nil, 0, err
				}
			}
		case models.TRANSACTION_TYPE_EXPENSE:
			if src != nil && sel.AccountIncluded(src.Id) {
				if err := add(&get(period, src.Currency).Expense, -t.SourceAmount); err != nil {
					return nil, 0, err
				}
			}
		case models.TRANSACTION_TYPE_TRANSFER:
			dst := sel.Accounts.Get(t.DestinationAccountId)
			srcIn := src != nil && sel.AccountIncluded(src.Id)
			dstIn := dst != nil && sel.AccountIncluded(dst.Id)

			if srcIn && dstIn && src.Currency == dst.Currency && !includeInternal {
				internal++
				continue
			}

			if srcIn {
				if err := add(&get(period, src.Currency).TransfersOut, -t.SourceAmount); err != nil {
					return nil, 0, err
				}
			}

			if dstIn {
				amount := t.SourceAmount

				if t.DestinationAmount != nil {
					amount = *t.DestinationAmount
				}

				if err := add(&get(period, dst.Currency).TransfersIn, amount); err != nil {
					return nil, 0, err
				}
			}
		}
	}

	return flows, internal, nil
}

// anCashFlowSeries renders opening, in, out, closing and their breakdown per currency. Balances
// are present in every period (a balance is always known); flows only where there were any.
func anCashFlowSeries(flows map[string]map[string]*anFlow, tl *anBalanceTimeline, sel *anSelection, periods []anPeriod, loc *time.Location) ([]*anSeries, error) {
	currencies := map[string]bool{}

	for _, a := range sel.Accounts.Leaves() {
		if sel.AccountIncluded(a.Id) {
			currencies[a.Currency] = true
		}
	}

	for _, m := range flows {
		for cur := range m {
			currencies[cur] = true
		}
	}

	keys := []struct{ key, label string }{
		{"opening", "Opening balance"}, {"in", "Money in"}, {"out", "Money out"}, {"closing", "Closing balance"},
		{"income", "Income"}, {"expense", "Expense"}, {"transfers_in", "Transfers in"}, {"transfers_out", "Transfers out"},
		{"adjustments", "Adjustments"},
	}

	var out []*anSeries

	for _, cur := range anSortedKeys(currencies) {
		bySeries := map[string]*anSeries{}

		for _, k := range keys {
			s := newAnSeries(k.key, k.label, cur)
			s.Kind = k.key
			bySeries[k.key] = s
			out = append(out, s)
		}

		for _, p := range periods {
			var opening, closing int64
			var err error

			for _, a := range sel.Accounts.Leaves() {
				if !sel.AccountIncluded(a.Id) || a.Currency != cur {
					continue
				}

				if opening, err = CheckedAdd(opening, tl.OpeningAt(a.Id, anPeriodStartYMD(p, loc))); err != nil {
					return nil, err
				}

				if closing, err = CheckedAdd(closing, tl.ClosingAt(a.Id, anPeriodEndYMD(p, loc))); err != nil {
					return nil, err
				}
			}

			f := flows[p.Period][cur]

			if f == nil {
				f = &anFlow{}
			}

			in, err := CheckedAdd(f.Income, f.TransfersIn)

			if err != nil {
				return nil, err
			}

			outFlow, err := CheckedAdd(f.Expense, f.TransfersOut)

			if err != nil {
				return nil, err
			}

			adjust := closing - opening - in - outFlow

			if err := bySeries["opening"].add(p.Period, opening); err != nil {
				return nil, err
			}

			if err := bySeries["closing"].add(p.Period, closing); err != nil {
				return nil, err
			}

			if flows[p.Period][cur] != nil {
				for key, v := range map[string]int64{"in": in, "out": outFlow, "income": f.Income, "expense": f.Expense, "transfers_in": f.TransfersIn, "transfers_out": f.TransfersOut} {
					if v != 0 {
						if err := bySeries[key].add(p.Period, v); err != nil {
							return nil, err
						}
					}
				}
			}

			if adjust != 0 {
				if err := bySeries["adjustments"].add(p.Period, adjust); err != nil {
					return nil, err
				}
			}
		}

		// opening/closing are balances: their "total" is meaningless as a sum, so it is the
		// range's first opening and last closing
		if len(periods) > 0 {
			bySeries["opening"].Total = bySeries["opening"].points[periods[0].Period]
			bySeries["closing"].Total = bySeries["closing"].points[periods[len(periods)-1].Period]
		}
	}

	return out, nil
}

// ---------------------------------------------------------------------------------------------
// /analytics/net-worth

func anHandleNetWorth(mc *Ctx) (any, error) {
	ds, de := anDefaultTrailingMonths(mc.Loc, 12)
	c, err := anBegin(mc, []string{"interval"}, ds, de, false)

	if err != nil {
		return nil, err
	}

	if err := c.rejectCategoryFilters("net-worth"); err != nil {
		return nil, err
	}

	interval, err := anParseEnum(mc, "interval", anIntervalMonth, append([]string{anIntervalNone}, anBalanceIntervals...)...)

	if err != nil {
		return nil, err
	}

	b := newAnBucketer(interval, c.args.Range, mc.Loc, anWeekday(mc))
	periods := b.Periods()
	tl, err := anFetchAssetTrends(mc, b.RangeStart, b.RangeEnd)

	if err != nil {
		return nil, err
	}

	c.source(anAssetTrendsSource)
	c.rows = tl.Rows()

	aggregates, accounts, err := anNetWorthSeries(tl, c.sel, periods, mc.Loc)

	if err != nil {
		return nil, err
	}

	out, unconverted, err := c.finishOrdered(aggregates, anPeriodOrder(periods))

	if err != nil {
		return nil, err
	}

	accounts = anFinalizeAll(accounts, anPeriodOrder(periods))
	c.filters["interval"] = interval
	c.note("liabilities are signed as the app stores them (money owed is negative), so net = assets + liabilities; per-account series stay in each account's own currency")
	c.note("a balance is a point in time: each series' total is its closing balance at the end of the range, not a sum of points")

	return map[string]any{
		"interval":    interval,
		"periods":     periods,
		"series":      out,
		"unconverted": unconverted,
		"accounts":    accounts,
		"provenance":  c.provenance(),
	}, nil
}

// anNetWorthSeries renders assets, liabilities and net per currency, and one series per account,
// all as closing balances at each period end
func anNetWorthSeries(tl *anBalanceTimeline, sel *anSelection, periods []anPeriod, loc *time.Location) ([]*anSeries, []*anSeries, error) {
	set := newAnSeriesSet()
	var accounts []*anSeries
	last := ""

	if len(periods) > 0 {
		last = periods[len(periods)-1].Period
	}

	for _, a := range sel.Accounts.Leaves() {
		if !sel.AccountIncluded(a.Id) {
			continue
		}

		acc := newAnSeries(idString(a.Id), a.Name, a.Currency)
		acc.Kind, acc.Id, acc.Hidden = "account", idString(a.Id), a.Hidden

		if a.ParentId > 0 {
			acc.ParentId = idString(a.ParentId)
		}

		group := "assets"

		if a.IsLiability {
			group = "liabilities"
		}

		agg := set.get(group, a.Currency, func(s *anSeries) {
			s.Kind = group
			s.Label = map[string]string{"assets": "Assets", "liabilities": "Liabilities"}[group]
		})
		net := set.get("net", a.Currency, func(s *anSeries) { s.Label, s.Kind = "Net worth", "net" })

		for _, p := range periods {
			v := tl.ClosingAt(a.Id, anPeriodEndYMD(p, loc))
			acc.points[p.Period] = v

			if err := agg.add(p.Period, v); err != nil {
				return nil, nil, err
			}

			if err := net.add(p.Period, v); err != nil {
				return nil, nil, err
			}
		}

		acc.Total = acc.points[last]
		accounts = append(accounts, acc)
	}

	aggs := set.list()

	for _, s := range aggs {
		s.Total = s.points[last]
	}

	rank := map[string]int{"assets": 0, "liabilities": 1, "net": 2}

	sort.SliceStable(aggs, func(i, j int) bool {
		if aggs[i].Currency != aggs[j].Currency {
			return aggs[i].Currency < aggs[j].Currency
		}

		return rank[aggs[i].Key] < rank[aggs[j].Key]
	})

	if accounts == nil {
		accounts = []*anSeries{}
	}

	return aggs, accounts, nil
}

// ---------------------------------------------------------------------------------------------
// /analytics/runway

// anDefaultLiquid are the account categories counted as liquid unless the caller says otherwise
var anDefaultLiquid = []string{"cash", "checking", "savings"}

func anHandleRunway(mc *Ctx) (any, error) {
	today := anDateString(anToday(mc.Loc))
	defStart := today

	if e := mc.Query("end"); e != "" {
		defStart = e // the range is only its end here: as_of (or end) is the day balances are read
	}

	c, err := anBegin(mc, []string{"basis", "liquid_categories", "as_of"}, defStart, today, false)

	if err != nil {
		return nil, err
	}

	if err := c.rejectTransfers("runway"); err != nil {
		return nil, err
	}

	if mc.Query("start") != "" {
		return nil, Invalid("runway looks back from as_of (or end) by `basis` whole months; pass as_of, not start", "runway does not take start")
	}

	basis, err := anParseIntArg(mc, "basis", 6, 1, 24)

	if err != nil {
		return nil, err
	}

	if basis != 3 && basis != 6 && basis != 12 {
		c.note("basis is usually 3, 6 or 12 months; " + mc.Query("basis") + " was used as asked")
	}

	asOf := c.args.Range.End

	if v := mc.Query("as_of"); v != "" {
		t, err := ParseDate("as_of", v, mc.Loc)

		if err != nil {
			return nil, err
		}

		asOf = anDateString(t)
	}

	liquidNames := mc.QueryList("liquid_categories")

	if len(liquidNames) == 0 {
		liquidNames = anDefaultLiquid
	}

	liquid := map[models.AccountCategory]bool{}

	for _, n := range liquidNames {
		cat, ok := anAccountCategoryByName(n)

		if !ok {
			names := make([]string, 0, len(anAccountCategoryNames))

			for _, v := range anAccountCategoryNames {
				names = append(names, v)
			}

			sort.Strings(names)

			return nil, Invalid("pass liquid_categories from: "+strings.Join(names, ", "), "unknown account category %q", n)
		}

		if !cat.IsAsset() {
			return nil, Invalid("liquid assets are asset accounts; drop "+n, "%s is a liability category", n)
		}

		liquid[cat] = true
	}

	asOfDay := anDayStart(asOf, mc.Loc)
	basisEnd := time.Date(asOfDay.Year(), asOfDay.Month(), 1, 0, 0, 0, 0, mc.Loc).AddDate(0, 0, -1)
	basisStart := time.Date(basisEnd.Year(), basisEnd.Month(), 1, 0, 0, 0, 0, mc.Loc).AddDate(0, -int(basis-1), 0)
	basisDays := int64(anDayDiff(basisStart, basisEnd) + 1)

	// liquid balances as of the day
	tl, err := anFetchAssetTrends(mc, asOfDay, asOfDay)

	if err != nil {
		return nil, err
	}

	c.source(anAssetTrendsSource)
	ymd := anYMD(asOfDay)
	liquidByCur := map[string]int64{}
	var liquidAccounts []map[string]any

	for _, a := range c.sel.Accounts.Leaves() {
		if !c.sel.AccountIncluded(a.Id) || !liquid[a.Category] {
			continue
		}

		v := tl.ClosingAt(a.Id, ymd)
		s, err := CheckedAdd(liquidByCur[a.Currency], v)

		if err != nil {
			return nil, err
		}

		liquidByCur[a.Currency] = s
		liquidAccounts = append(liquidAccounts, map[string]any{
			"accountId": idString(a.Id), "name": a.Name, "category": anAccountCategoryNames[a.Category], "currency": a.Currency, "balance": v,
		})
	}

	// trailing income and expense over the basis months (whole months before as_of's month)
	_, be := anDayUnixRange(basisEnd)
	days, err := anFetchDaily(mc, c.sel, basisStart.Unix(), be, c.args.UseTransactionTimezone)

	if err != nil {
		return nil, err
	}

	c.source("GET /api/v1/transactions/amounts/daily.json")
	incomeByCur := map[string]int64{}
	expenseByCur := map[string]int64{}

	for _, d := range days {
		for _, a := range d.Amounts {
			c.rows++

			if incomeByCur[a.Currency], err = CheckedAdd(incomeByCur[a.Currency], a.Income); err != nil {
				return nil, err
			}

			if expenseByCur[a.Currency], err = CheckedAdd(expenseByCur[a.Currency], a.Expense); err != nil {
				return nil, err
			}
		}
	}

	currencies := map[string]bool{}

	for cur := range liquidByCur {
		currencies[cur] = true
	}

	for cur := range incomeByCur {
		currencies[cur] = true
	}

	for cur := range expenseByCur {
		currencies[cur] = true
	}

	byCurrency := []map[string]any{}

	for _, cur := range anSortedKeys(currencies) {
		net := expenseByCur[cur] - incomeByCur[cur]
		r := anComputeRunway(liquidByCur[cur], net, basis, basisDays)
		byCurrency = append(byCurrency, map[string]any{
			"currency":        cur,
			"liquidAssets":    liquidByCur[cur],
			"trailingIncome":  incomeByCur[cur],
			"trailingExpense": -expenseByCur[cur],
			"runway":          r,
		})
	}

	data := map[string]any{
		"asOf":             asOf,
		"basisMonths":      basis,
		"basis":            map[string]any{"start": anDateString(basisStart), "end": anDateString(basisEnd), "days": basisDays},
		"liquidCategories": liquidNames,
		"liquidAccounts":   liquidAccounts,
		"byCurrency":       byCurrency,
	}

	if data["liquidAccounts"] == nil {
		data["liquidAccounts"] = []map[string]any{}
	}

	if c.cv != nil {
		liq, liqSrc, liqUn, err := anConvertTotals(liquidByCur, c.cv)

		if err != nil {
			return nil, err
		}

		inc, incSrc, incUn, err := anConvertTotals(incomeByCur, c.cv)

		if err != nil {
			return nil, err
		}

		exp, expSrc, expUn, err := anConvertTotals(expenseByCur, c.cv)

		if err != nil {
			return nil, err
		}

		data["converted"] = map[string]any{
			"currency":                c.cv.Target,
			"liquidAssets":            liq,
			"trailingIncome":          inc,
			"trailingExpense":         -exp,
			"runway":                  anComputeRunway(liq, exp-inc, basis, basisDays),
			"sources":                 map[string]any{"liquidAssets": liqSrc, "trailingIncome": incSrc, "trailingExpense": anNegateAll(expSrc)},
			"unconvertedLiquidAssets": liqUn,
			"unconvertedIncome":       incUn,
			"unconvertedExpense":      anNegateAll(expUn),
		}
	}

	c.filters["basis"] = basis
	c.filters["asOf"] = asOf
	c.filters["liquidCategories"] = liquidNames
	c.note("runway = liquid assets ÷ average monthly (expense − income) over the basis months; transfers and balance modifications are not spending; months are truncated, never rounded up")
	data["provenance"] = c.provenance()

	return data, nil
}

// anNegateAll flips the sign of per-currency figures (expense is reported negative)
func anNegateAll(list []anCurrencyAmount) []anCurrencyAmount {
	out := make([]anCurrencyAmount, len(list))

	for i, v := range list {
		out[i] = anCurrencyAmount{Currency: v.Currency, Amount: -v.Amount}
	}

	return out
}

// ---------------------------------------------------------------------------------------------
// /accounts/:id/balance and /accounts/:id/balance-history

// anResolveAccount reads the account book and the :id path parameter
func anResolveAccount(mc *Ctx) (*anAccounts, *anAccount, error) {
	id, err := ResolveId("id", mc.Param("id"))

	if err != nil {
		return nil, nil, err
	}

	book, err := anLoadAccounts(mc)

	if err != nil {
		return nil, nil, err
	}

	a := book.Get(id)

	if a == nil {
		return nil, nil, NotFound("GET /machine/v1/accounts lists the account ids", "no account with id %s", idString(id)).WithDetails(map[string]any{"id": idString(id)})
	}

	return book, a, nil
}

// anLeavesOf is the account itself (a leaf) or its sub-accounts (a parent)
func anLeavesOf(book *anAccounts, a *anAccount) []*anAccount {
	if a.IsLeaf() {
		return []*anAccount{a}
	}

	var out []*anAccount

	for _, id := range a.Children {
		if c := book.Get(id); c != nil {
			out = append(out, c)
		}
	}

	return out
}

func anAccountView(a *anAccount) map[string]any {
	v := map[string]any{
		"accountId":   idString(a.Id),
		"name":        a.Name,
		"currency":    a.Currency,
		"category":    anAccountCategoryNames[a.Category],
		"isAsset":     a.IsAsset,
		"isLiability": a.IsLiability,
		"hidden":      a.Hidden,
	}

	if a.ParentId > 0 {
		v["parentId"] = idString(a.ParentId)
	}

	return v
}

func anHandleAccountBalance(mc *Ctx) (any, error) {
	if err := anCheckQueryOnly(mc, "as_of"); err != nil {
		return nil, err
	}

	book, a, err := anResolveAccount(mc)

	if err != nil {
		return nil, err
	}

	day := anToday(mc.Loc)

	if v := mc.Query("as_of"); v != "" {
		if day, err = ParseDate("as_of", v, mc.Loc); err != nil {
			return nil, err
		}
	}

	tl, err := anFetchAssetTrends(mc, day, day)

	if err != nil {
		return nil, err
	}

	ymd := anYMD(day)
	leaves := anLeavesOf(book, a)
	totals := map[string]int64{}
	var subs []map[string]any

	for _, l := range leaves {
		v := tl.ClosingAt(l.Id, ymd)
		s, err := CheckedAdd(totals[l.Currency], v)

		if err != nil {
			return nil, err
		}

		totals[l.Currency] = s
		view := anAccountView(l)
		view["balance"] = v
		view["currentBalance"] = l.Balance
		subs = append(subs, view)
	}

	out := anAccountView(a)
	out["asOf"] = anDateString(day)
	out["timezone"] = mc.Loc.String()

	if a.IsLeaf() {
		out["balance"] = totals[a.Currency]
		out["currentBalance"] = a.Balance
	} else {
		out["currency"] = nil
		out["subAccounts"] = subs
		out["totals"] = anTotalsList(totals)
		out["note"] = "a parent account holds no balance of its own; its sub-accounts' balances are listed and totalled per currency"

		if subs == nil {
			out["subAccounts"] = []map[string]any{}
		}
	}

	out["provenance"] = map[string]any{
		"upstream":       []string{"GET /api/v1/accounts/list.json", anAssetTrendsSource},
		"rowCount":       tl.Rows(),
		"basis":          "closing balance at the end of as_of in the call's timezone, from the app's daily running balance",
		"currentBalance": "the app's balance now (includes transactions dated after as_of)",
	}

	return out, nil
}

// anCheckQueryOnly rejects any query argument outside the given list (routes outside /analytics)
func anCheckQueryOnly(mc *Ctx, allowed ...string) error {
	ok := map[string]bool{}

	for _, a := range allowed {
		ok[a] = true
	}

	var unknown []string

	for k := range mc.Gin.Request.URL.Query() {
		if !ok[strings.TrimSuffix(k, "[]")] {
			unknown = append(unknown, k)
		}
	}

	if len(unknown) == 0 {
		return nil
	}

	sort.Strings(unknown)

	return Invalid("this route accepts: "+strings.Join(allowed, ", "), "unknown argument %s", strings.Join(unknown, ", ")).
		WithDetails(map[string]any{"unknown": unknown, "accepted": allowed})
}

func anHandleBalanceHistory(mc *Ctx) (any, error) {
	if err := anCheckQueryOnly(mc, "start", "end", "interval"); err != nil {
		return nil, err
	}

	book, a, err := anResolveAccount(mc)

	if err != nil {
		return nil, err
	}

	ds, de := anDefaultTrailingMonths(mc.Loc, 12)
	rng, err := ParseDateRange(mc.Query("start"), mc.Query("end"), ds, de, mc.Loc)

	if err != nil {
		return nil, err
	}

	interval, err := anParseEnum(mc, "interval", anIntervalMonth, anIntervalDay, anIntervalWeek, anIntervalMonth)

	if err != nil {
		return nil, err
	}

	b := newAnBucketer(interval, rng, mc.Loc, anWeekday(mc))
	periods := b.Periods()

	if interval == anIntervalDay && len(periods) > 5000 {
		return nil, Invalid("narrow the range, or pass interval=week or interval=month", "%d daily points requested; the cap is 5000", len(periods))
	}

	tl, err := anFetchAssetTrends(mc, b.RangeStart, b.RangeEnd)

	if err != nil {
		return nil, err
	}

	leaves := anLeavesOf(book, a)
	order := anPeriodOrder(periods)
	last := ""

	if len(order) > 0 {
		last = order[len(order)-1]
	}

	totals := newAnSeriesSet()
	var series []*anSeries
	var openings []map[string]any

	for _, l := range leaves {
		s := newAnSeries(idString(l.Id), l.Name, l.Currency)
		s.Kind, s.Id, s.Hidden = "account", idString(l.Id), l.Hidden

		if l.ParentId > 0 {
			s.ParentId = idString(l.ParentId)
		}

		tot := totals.get("total", l.Currency, func(t *anSeries) { t.Label, t.Kind = "Total", "total" })

		for _, p := range periods {
			v := tl.ClosingAt(l.Id, anPeriodEndYMD(p, mc.Loc))
			s.points[p.Period] = v

			if err := tot.add(p.Period, v); err != nil {
				return nil, err
			}
		}

		s.Total = s.points[last]
		series = append(series, s)
		openings = append(openings, map[string]any{
			"accountId": idString(l.Id), "currency": l.Currency, "amount": tl.OpeningAt(l.Id, anYMD(b.RangeStart)),
		})
	}

	out := anFinalizeAll(series, order)

	if !a.IsLeaf() {
		tots := totals.list()

		for _, t := range tots {
			t.Total = t.points[last]
		}

		anSortSeriesStable(tots)
		out = append(out, anFinalizeAll(tots, order)...)
	}

	if openings == nil {
		openings = []map[string]any{}
	}

	return map[string]any{
		"account":  anAccountView(a),
		"interval": interval,
		"periods":  periods,
		"series":   out,
		"opening":  openings,
		"provenance": map[string]any{
			"range":    rng,
			"timezone": mc.Loc.String(),
			"upstream": []string{"GET /api/v1/accounts/list.json", anAssetTrendsSource},
			"rowCount": tl.Rows(),
			"basis":    "each point is the closing balance at the end of its period (clipped to the range); a series' total is its closing balance at the end of the range",
		},
	}, nil
}

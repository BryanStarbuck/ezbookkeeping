package machine

import (
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/mayswind/ezbookkeeping/pkg/datastore"
	"github.com/mayswind/ezbookkeeping/pkg/errfile"
	"github.com/mayswind/ezbookkeeping/pkg/log"
	"github.com/mayswind/ezbookkeeping/pkg/models"
)

// analytics_detectors.go — the transaction-level analytics: payee leaderboard, recurring charges,
// anomalies and import fallout. Detectors are not oracles (apis.mdx §12.3): each returns the
// evidence behind its verdict — the occurrences, the gaps, the trailing mean, the deviation.

// ---------------------------------------------------------------------------------------------
// shared: transaction rows of the selection, signed and dated

// anTxn is one transaction as the detectors see it
type anTxn struct {
	Id         string
	Day        time.Time
	Date       string
	Kind       anKind
	Amount     int64 // signed (§17.2)
	Currency   string
	AccountId  int64
	CategoryId int64
	Comment    string
}

// anSelectTxns keeps the rows of the selection in the requested directions. Balance modifications
// are never kept. A transfer is kept on the side(s) inside the selection when transfers are asked
// for; an internal transfer (both sides inside) appears once out and once in.
func anSelectTxns(rows []*models.TransactionInfoResponse, sel *anSelection, loc *time.Location, useTxTz bool, kinds map[anKind]bool) []anTxn {
	var out []anTxn

	for _, t := range rows {
		zone := loc

		if useTxTz {
			zone = time.FixedZone("transaction", int(t.UtcOffset)*60)
		}

		local := time.Unix(t.Time, 0).In(zone)
		day := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, loc)
		base := anTxn{Id: idString(t.Id), Day: day, Date: anDateString(day), CategoryId: t.CategoryId, Comment: t.Comment}
		src := sel.Accounts.Get(t.SourceAccountId)

		switch t.Type {
		case models.TRANSACTION_TYPE_INCOME, models.TRANSACTION_TYPE_EXPENSE:
			kind := anKindIncome

			if t.Type == models.TRANSACTION_TYPE_EXPENSE {
				kind = anKindExpense
			}

			if !kinds[kind] || src == nil || !sel.AccountIncluded(src.Id) || !sel.CategoryIncluded(t.CategoryId) {
				continue
			}

			x := base
			x.Kind, x.Amount, x.Currency, x.AccountId = kind, anSigned(kind, t.SourceAmount), src.Currency, src.Id
			out = append(out, x)
		case models.TRANSACTION_TYPE_TRANSFER:
			if !sel.CategoryIncluded(t.CategoryId) {
				continue
			}

			if kinds[anKindTransferOut] && src != nil && sel.AccountIncluded(src.Id) {
				x := base
				x.Kind, x.Amount, x.Currency, x.AccountId = anKindTransferOut, -t.SourceAmount, src.Currency, src.Id
				out = append(out, x)
			}

			dst := sel.Accounts.Get(t.DestinationAccountId)

			if kinds[anKindTransferIn] && dst != nil && sel.AccountIncluded(dst.Id) {
				amount := t.SourceAmount

				if t.DestinationAmount != nil {
					amount = *t.DestinationAmount
				}

				x := base
				x.Kind, x.Amount, x.Currency, x.AccountId = anKindTransferIn, amount, dst.Currency, dst.Id
				out = append(out, x)
			}
		}
	}

	return out
}

// anDirectionKinds maps direction (out|in) and include_transfers to the kinds kept
func anDirectionKinds(direction string, includeTransfers bool) map[anKind]bool {
	if direction == "in" {
		return map[anKind]bool{anKindIncome: true, anKindTransferIn: includeTransfers}
	}

	return map[anKind]bool{anKindExpense: true, anKindTransferOut: includeTransfers}
}

// anListTypeFor asks upstream only for the transaction type needed, when one suffices
func anListTypeFor(kinds map[anKind]bool) models.TransactionType {
	if kinds[anKindTransferIn] || kinds[anKindTransferOut] {
		return 0
	}

	if kinds[anKindIncome] && !kinds[anKindExpense] {
		return models.TRANSACTION_TYPE_INCOME
	}

	if kinds[anKindExpense] && !kinds[anKindIncome] {
		return models.TRANSACTION_TYPE_EXPENSE
	}

	return 0
}

// anFetchSelectedTxns reads list/all.json for the call's range and filters, and selects
func anFetchSelectedTxns(c *anCall, kinds map[anKind]bool) ([]anTxn, error) {
	f := anListFilter{StartUnix: c.args.Range.StartUnix, EndUnix: c.args.Range.EndUnix, Type: anListTypeFor(kinds), TagFilter: c.args.TagFilter}

	if c.sel.ExplicitAccounts {
		for id := range c.sel.AccountSet {
			f.AccountIds = append(f.AccountIds, id)
		}

		sort.Slice(f.AccountIds, func(i, j int) bool { return f.AccountIds[i] < f.AccountIds[j] })
	}

	rows, err := anFetchAllTransactions(c.mc, f)

	if err != nil {
		return nil, err
	}

	c.source(anListAllSource)
	c.rows += len(rows)

	return anSelectTxns(rows, c.sel, c.mc.Loc, c.args.UseTransactionTimezone, kinds), nil
}

// ---------------------------------------------------------------------------------------------
// /analytics/payee-leaderboard

// anVariant is one raw comment text a leaderboard key merged, with how often it appeared
type anVariant struct {
	Text  string `json:"text"`
	Count int64  `json:"count"`
}

// anPayeeGroup accumulates one normalised comment in one currency
type anPayeeGroup struct {
	key      string
	currency string
	total    int64
	count    int64
	variants map[string]int64
	first    string
	last     string
	accounts map[int64]bool
}

// anGroupPayees groups selected transactions by normalised comment and currency
func anGroupPayees(txns []anTxn) ([]*anPayeeGroup, error) {
	byKey := map[string]*anPayeeGroup{}
	var order []string

	for _, t := range txns {
		key := anNormalizePayee(t.Comment)
		k := key + "\x00" + t.Currency
		g, ok := byKey[k]

		if !ok {
			g = &anPayeeGroup{key: key, currency: t.Currency, variants: map[string]int64{}, accounts: map[int64]bool{}}
			byKey[k] = g
			order = append(order, k)
		}

		total, err := CheckedAdd(g.total, t.Amount)

		if err != nil {
			return nil, err
		}

		g.total = total
		g.count++
		g.variants[t.Comment]++
		g.accounts[t.AccountId] = true

		if g.first == "" || t.Date < g.first {
			g.first = t.Date
		}

		if t.Date > g.last {
			g.last = t.Date
		}
	}

	out := make([]*anPayeeGroup, 0, len(order))

	for _, k := range order {
		out = append(out, byKey[k])
	}

	return out, nil
}

// anVariantsOf lists a group's raw variants, most frequent first
func anVariantsOf(g *anPayeeGroup) []anVariant {
	out := make([]anVariant, 0, len(g.variants))

	for text, n := range g.variants {
		out = append(out, anVariant{Text: text, Count: n})
	}

	sort.Slice(out, func(i, j int) bool {
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}

		return out[i].Text < out[j].Text
	})

	return out
}

func anHandlePayeeLeaderboard(mc *Ctx) (any, error) {
	ds, de := anDefaultYearToDate(mc.Loc)
	c, err := anBegin(mc, []string{"top_n", "direction", "field"}, ds, de, false)

	if err != nil {
		return nil, err
	}

	topN, err := anParseIntArg(mc, "top_n", 20, 0, 1000)

	if err != nil {
		return nil, err
	}

	direction, err := anParseEnum(mc, "direction", "out", "out", "in")

	if err != nil {
		return nil, err
	}

	if _, err := anParseEnum(mc, "field", "comment", "comment"); err != nil {
		return nil, err
	}

	txns, err := anFetchSelectedTxns(c, anDirectionKinds(direction, c.args.IncludeTransfers))

	if err != nil {
		return nil, err
	}

	groups, err := anGroupPayees(txns)

	if err != nil {
		return nil, err
	}

	rangeKey := newAnBucketer(anIntervalNone, c.args.Range, mc.Loc, anWeekday(mc)).rangeKey()
	var series []*anSeries
	evidence := map[string]*anPayeeGroup{}

	for _, g := range groups {
		label := g.key

		if vs := anVariantsOf(g); len(vs) > 0 && vs[0].Text != "" {
			label = vs[0].Text
		}

		if g.key == "" {
			label = "(no comment)"
		}

		s := newAnSeries(g.key, label, g.currency)
		s.Kind = "payee"
		n := g.count
		s.Count = &n

		if err := s.add(rangeKey, g.total); err != nil {
			return nil, err
		}

		series = append(series, s)
		evidence[g.key+"\x00"+g.currency] = g
	}

	native := anCloneSeriesList(series)
	out, unconverted, err := c.finishSeries(series, []string{rangeKey}, int(topN))

	if err != nil {
		return nil, err
	}

	// the evidence per row: raw variants merged, first/last dates, accounts
	rows := func(list []*anSeries) []map[string]any {
		res := []map[string]any{}

		for _, s := range list {
			row := map[string]any{"key": s.Key, "label": s.Label, "currency": s.Currency, "total": s.Total, "count": s.Count}

			if s.Key == anOtherKey {
				row["members"] = s.Members
				res = append(res, row)
				continue
			}

			var variants []anVariant
			first, last := "", ""
			accounts := map[int64]bool{}

			for k, g := range evidence {
				if g.key != s.Key {
					continue
				}

				if c.cv == nil && k != s.Key+"\x00"+s.Currency {
					continue
				}

				variants = append(variants, anVariantsOf(g)...)

				if first == "" || g.first < first {
					first = g.first
				}

				if g.last > last {
					last = g.last
				}

				for a := range g.accounts {
					accounts[a] = true
				}
			}

			ids := make([]int64, 0, len(accounts))

			for a := range accounts {
				ids = append(ids, a)
			}

			sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
			row["variants"] = anMergeVariants(variants)
			row["firstDate"], row["lastDate"] = first, last
			row["accountIds"] = anIdStrings(ids)
			res = append(res, row)
		}

		return res
	}

	c.filters["direction"] = direction
	c.filters["field"] = "comment"
	c.filters["topN"] = topN

	data := map[string]any{
		"groupedBy":   "comment",
		"direction":   direction,
		"periods":     []anPeriod{{Period: rangeKey, Start: c.args.Range.Start, End: c.args.Range.End}},
		"series":      out,
		"unconverted": unconverted,
		"payees":      rows(out),
		"unconvertedPayees": func() []map[string]any {
			return rows(unconverted)
		}(),
	}

	tot, err := c.totals(native)

	if err != nil {
		return nil, err
	}

	for k, v := range tot {
		data[k] = v
	}

	c.note("ezBookkeeping has no payee entity: rows are grouped by the transaction comment, normalised (upper-case, punctuation and reference numbers dropped); the raw variants each row merged are listed so the grouping is visible evidence")
	data["provenance"] = c.provenance()

	return data, nil
}

// anMergeVariants folds identical variant texts (from several currencies) and orders them
func anMergeVariants(vs []anVariant) []anVariant {
	m := map[string]int64{}

	for _, v := range vs {
		m[v.Text] += v.Count
	}

	g := &anPayeeGroup{variants: m}

	return anVariantsOf(g)
}

// ---------------------------------------------------------------------------------------------
// /analytics/recurring

// anRecurringGroup is one candidate series of repeating transactions
type anRecurringGroup struct {
	Key         string
	Kind        anKind
	Currency    string
	Occurrences []anOccurrence
	Accounts    map[int64]bool
	Categories  map[int64]bool
	Comment     string
}

// anRecurringKey groups transactions that plausibly repeat: the normalised comment within one
// direction and currency; rows with no comment fall back to category and exact amount
func anRecurringKey(t anTxn) string {
	if norm := anNormalizePayee(t.Comment); norm != "" {
		return string(t.Kind) + "|" + t.Currency + "|c:" + norm
	}

	return string(t.Kind) + "|" + t.Currency + "|cat:" + strconv.FormatInt(t.CategoryId, 10) + "|amt:" + strconv.FormatInt(t.Amount, 10)
}

// anGroupRecurring collects candidate groups in first-seen order, occurrences sorted by date
func anGroupRecurring(txns []anTxn) []*anRecurringGroup {
	byKey := map[string]*anRecurringGroup{}
	var order []string

	for _, t := range txns {
		k := anRecurringKey(t)
		g, ok := byKey[k]

		if !ok {
			g = &anRecurringGroup{Key: k, Kind: t.Kind, Currency: t.Currency, Accounts: map[int64]bool{}, Categories: map[int64]bool{}, Comment: t.Comment}
			byKey[k] = g
			order = append(order, k)
		}

		g.Occurrences = append(g.Occurrences, anOccurrence{Id: t.Id, Date: t.Date, Amount: t.Amount, day: t.Day})
		g.Accounts[t.AccountId] = true
		g.Categories[t.CategoryId] = true
	}

	out := make([]*anRecurringGroup, 0, len(order))

	for _, k := range order {
		g := byKey[k]
		sort.SliceStable(g.Occurrences, func(i, j int) bool {
			if g.Occurrences[i].Date != g.Occurrences[j].Date {
				return g.Occurrences[i].Date < g.Occurrences[j].Date
			}

			return g.Occurrences[i].Id < g.Occurrences[j].Id
		})
		out = append(out, g)
	}

	return out
}

// anScheduledMatch finds a scheduled template that already covers a group: same normalised
// comment and direction, or the same account, category and amount
func anScheduledMatch(g *anRecurringGroup, templates []*models.TransactionTemplateInfoResponse, sel *anSelection) *models.TransactionTemplateInfoResponse {
	norm := anNormalizePayee(g.Comment)

	for _, t := range templates {
		if t == nil || t.TransactionInfoResponse == nil {
			continue
		}

		var kind anKind

		switch t.Type {
		case models.TRANSACTION_TYPE_INCOME:
			kind = anKindIncome
		case models.TRANSACTION_TYPE_EXPENSE:
			kind = anKindExpense
		case models.TRANSACTION_TYPE_TRANSFER:
			kind = anKindTransferOut

			if g.Kind == anKindTransferIn {
				kind = anKindTransferIn
			}
		default:
			continue
		}

		if kind != g.Kind {
			continue
		}

		if norm != "" && anNormalizePayee(t.Comment) == norm {
			return t
		}

		if norm == "" && g.Categories[t.CategoryId] && g.Accounts[t.SourceAccountId] && len(g.Occurrences) > 0 && anAbs(g.Occurrences[len(g.Occurrences)-1].Amount) == t.SourceAmount {
			return t
		}
	}

	return nil
}

func anHandleRecurring(mc *Ctx) (any, error) {
	ds, de := anDefaultTrailingMonths(mc.Loc, 13)
	c, err := anBegin(mc, []string{"min_occurrences", "tolerance_days", "direction", "include_scheduled", "include_inactive"}, ds, de, false)

	if err != nil {
		return nil, err
	}

	minOcc, err := anParseIntArg(mc, "min_occurrences", 3, 2, 1000)

	if err != nil {
		return nil, err
	}

	tolerance, err := anParseIntArg(mc, "tolerance_days", 3, 0, 30)

	if err != nil {
		return nil, err
	}

	direction, err := anParseEnum(mc, "direction", "out", "out", "in", "both")

	if err != nil {
		return nil, err
	}

	includeScheduled, err := mc.QueryBool("include_scheduled", false)

	if err != nil {
		return nil, err
	}

	includeInactive, err := mc.QueryBool("include_inactive", false)

	if err != nil {
		return nil, err
	}

	kinds := anDirectionKinds(direction, c.args.IncludeTransfers)

	if direction == "both" {
		kinds = map[anKind]bool{anKindIncome: true, anKindExpense: true, anKindTransferIn: c.args.IncludeTransfers, anKindTransferOut: c.args.IncludeTransfers}
	}

	txns, err := anFetchSelectedTxns(c, kinds)

	if err != nil {
		return nil, err
	}

	templates, scheduledOn, err := anFetchScheduledTemplates(mc)

	if err != nil {
		errfile.Caught("fetching the scheduled transaction templates", err)
		c.note("scheduled templates could not be read, so already-scheduled charges are not marked")
		templates, scheduledOn = nil, false
	} else if scheduledOn {
		c.source("GET /api/v1/transaction/templates/list.json")
	}

	if !scheduledOn {
		c.note("scheduled transactions are switched off in conf/ezbookkeeping.ini, so no detection is marked as already scheduled")
	}

	rangeEnd := anDayStart(c.args.Range.End, mc.Loc)
	var found []map[string]any
	var already []map[string]any
	annualByCur := map[string]int64{}
	examined := 0

	for _, g := range anGroupRecurring(txns) {
		if int64(len(g.Occurrences)) < minOcc {
			continue
		}

		examined++
		days := make([]time.Time, len(g.Occurrences))
		amounts := make([]int64, len(g.Occurrences))

		for i, o := range g.Occurrences {
			days[i], amounts[i] = o.day, o.Amount
		}

		fit := anFitCadence(days, int(tolerance))

		if !fit.Consistent || int64(fit.Matches+1) < minOcc {
			continue
		}

		last := g.Occurrences[len(g.Occurrences)-1]
		next := anNextOccurrence(fit.Cadence, last.day)
		active := !next.AddDate(0, 0, int(tolerance)).Before(rangeEnd)

		if !active && !includeInactive {
			continue
		}

		typical := anMedian(amounts)
		lo, hi := amounts[0], amounts[0]

		for _, v := range amounts {
			if v < lo {
				lo = v
			}

			if v > hi {
				hi = v
			}
		}

		annual := typical * fit.Cadence.PerYear
		accountIds := make([]int64, 0, len(g.Accounts))

		for a := range g.Accounts {
			accountIds = append(accountIds, a)
		}

		sort.Slice(accountIds, func(i, j int) bool { return accountIds[i] < accountIds[j] })
		categoryIds := make([]int64, 0, len(g.Categories))

		for cid := range g.Categories {
			categoryIds = append(categoryIds, cid)
		}

		sort.Slice(categoryIds, func(i, j int) bool { return categoryIds[i] < categoryIds[j] })

		label := g.Comment

		if label == "" {
			label = "(no comment)"

			if len(categoryIds) == 1 {
				if cat := c.sel.Categories.Get(categoryIds[0]); cat != nil {
					label = cat.Name
				}
			}
		}

		entry := map[string]any{
			"key":            g.Key,
			"label":          label,
			"kind":           string(g.Kind),
			"currency":       g.Currency,
			"cadence":        fit.Cadence.Name,
			"occurrences":    len(g.Occurrences),
			"gapsMatching":   fit.Matches,
			"gaps":           fit.Gaps,
			"gapDays":        fit.GapDays,
			"typicalAmount":  typical,
			"minAmount":      lo,
			"maxAmount":      hi,
			"amountVaries":   lo != hi,
			"annualizedCost": annual,
			"firstDate":      g.Occurrences[0].Date,
			"lastDate":       last.Date,
			"nextExpected":   anDateString(next),
			"active":         active,
			"accountIds":     anIdStrings(accountIds),
			"categoryIds":    anIdStrings(categoryIds),
			"evidence":       anLastOccurrences(g.Occurrences, 60),
		}

		if tpl := anScheduledMatch(g, templates, c.sel); tpl != nil {
			entry["scheduled"] = map[string]any{"templateId": idString(tpl.Id), "name": tpl.Name}
			already = append(already, entry)

			if !includeScheduled {
				continue
			}
		}

		found = append(found, entry)

		if active {
			v, err := CheckedAdd(annualByCur[g.Currency], annual)

			if err != nil {
				return nil, err
			}

			annualByCur[g.Currency] = v
		}
	}

	sort.SliceStable(found, func(i, j int) bool {
		ai, aj := anAbs(found[i]["annualizedCost"].(int64)), anAbs(found[j]["annualizedCost"].(int64))

		if found[i]["currency"] != found[j]["currency"] {
			return found[i]["currency"].(string) < found[j]["currency"].(string)
		}

		if ai != aj {
			return ai > aj
		}

		return found[i]["key"].(string) < found[j]["key"].(string)
	})

	if found == nil {
		found = []map[string]any{}
	}

	alreadyView := []map[string]any{}

	for _, a := range already {
		alreadyView = append(alreadyView, map[string]any{"key": a["key"], "label": a["label"], "currency": a["currency"], "cadence": a["cadence"], "scheduled": a["scheduled"]})
	}

	data := map[string]any{
		"recurring":        found,
		"alreadyScheduled": alreadyView,
		"groupsExamined":   examined,
		"annualizedTotals": anTotalsList(annualByCur),
	}

	if c.cv != nil {
		sum, src, un, err := anConvertTotals(annualByCur, c.cv)

		if err != nil {
			return nil, err
		}

		data["annualizedConverted"] = map[string]any{"currency": c.cv.Target, "amount": sum, "sources": src, "unconverted": un}
	}

	c.filters["minOccurrences"] = minOcc
	c.filters["toleranceDays"] = tolerance
	c.filters["direction"] = direction
	c.filters["includeScheduled"] = includeScheduled
	c.filters["includeInactive"] = includeInactive
	c.note("a group is recurring when at least three quarters of the gaps between its occurrences fit one cadence (weekly, biweekly, monthly, quarterly, semiannual, yearly) within tolerance_days; typicalAmount is the median; annualizedCost = typicalAmount × occurrences per year; annualizedTotals count active detections only")
	data["provenance"] = c.provenance()

	return data, nil
}

// anLastOccurrences keeps the most recent n occurrences (the evidence), oldest first
func anLastOccurrences(occ []anOccurrence, n int) []anOccurrence {
	if len(occ) <= n {
		return occ
	}

	return occ[len(occ)-n:]
}

// ---------------------------------------------------------------------------------------------
// /analytics/anomalies

func anHandleAnomalies(mc *Ctx) (any, error) {
	today := anToday(mc.Loc)
	ds := anDateString(time.Date(today.Year(), today.Month(), 1, 0, 0, 0, 0, mc.Loc))
	c, err := anBegin(mc, []string{"z", "min_amount", "lookback_months", "min_history", "group_by"}, ds, anDateString(today), false)

	if err != nil {
		return nil, err
	}

	z, zText, err := anParseRatArg(mc, "z", "2.0")

	if err != nil {
		return nil, err
	}

	minAmount, err := anAmountQuery(mc, "min_amount", 0)

	if err != nil {
		return nil, err
	}

	if minAmount < 0 {
		return nil, Invalid("min_amount is a magnitude in hundredths, e.g. min_amount=5000", "min_amount must not be negative")
	}

	lookback, err := anParseIntArg(mc, "lookback_months", 12, 2, 120)

	if err != nil {
		return nil, err
	}

	minHistory, err := anParseIntArg(mc, "min_history", 3, 2, 120)

	if err != nil {
		return nil, err
	}

	groupBy, err := anParseEnum(mc, "group_by", anGroupSecondary, anGroupPrimary, anGroupSecondary)

	if err != nil {
		return nil, err
	}

	start := anDayStart(c.args.Range.Start, mc.Loc)
	end := anDayStart(c.args.Range.End, mc.Loc)
	firstEval := time.Date(start.Year(), start.Month(), 1, 0, 0, 0, 0, mc.Loc)
	windowStart := firstEval.AddDate(0, -int(lookback), 0)

	if months := (end.Year()-firstEval.Year())*12 + int(end.Month()) - int(firstEval.Month()) + 1; months > 120 {
		return nil, Invalid("evaluate 120 months or fewer per call", "%d months requested", months)
	}

	windowRange, err := ParseDateRange(anDateString(windowStart), c.args.Range.End, "", "", mc.Loc)

	if err != nil {
		return nil, err
	}

	b := newAnBucketer(anIntervalMonth, windowRange, mc.Loc, anWeekday(mc))
	facts, sources, err := anFetchCategoryFacts(mc, windowRange, b, c.args.TagFilter, c.args.UseTransactionTimezone)

	if err != nil {
		return nil, err
	}

	c.source(sources...)
	c.rows = len(facts)
	kinds := map[anKind]bool{anKindIncome: true, anKindExpense: true}
	series, st, err := anGroupFacts(facts, c.sel, anGroupOpts{GroupBy: groupBy, Kinds: kinds})

	if err != nil {
		return nil, err
	}

	c.group = &st

	if c.cv != nil {
		conv, un, err := anConvertSeries(series, c.cv)

		if err != nil {
			return nil, err
		}

		series = conv

		if len(un) > 0 {
			c.note("categories in currencies without a rate were evaluated in their own currency")
			series = append(series, un...)
		}
	}

	allPeriods := b.Periods()
	order := anPeriodOrder(allPeriods)

	// the first month with any data, per currency: months before the books began are not zeros
	firstData := map[string]int{}

	for _, s := range series {
		for i, p := range order {
			if _, ok := s.points[p]; ok {
				if f, seen := firstData[s.Currency]; !seen || i < f {
					firstData[s.Currency] = i
				}

				break
			}
		}
	}

	evalStartIdx := int(lookback)
	var anomalies []map[string]any
	evaluated := 0

	for _, s := range series {
		for i := evalStartIdx; i < len(order); i++ {
			histFrom := i - int(lookback)

			if f, ok := firstData[s.Currency]; ok && f > histFrom {
				histFrom = f
			}

			if i-histFrom < int(minHistory) {
				continue
			}

			history := make([]int64, 0, i-histFrom)
			historyView := make([]anPoint, 0, i-histFrom)

			for j := histFrom; j < i; j++ {
				v := s.points[order[j]]
				history = append(history, v)
				historyView = append(historyView, anPoint{Period: order[j], Amount: v})
			}

			value, present := s.points[order[i]]
			evaluated++
			res := anAnomalyTest(history, value, z, minAmount)

			if !res.Flagged {
				continue
			}

			dir := "above"

			if res.Deviation < 0 {
				dir = "below"
			}

			var zScore any

			if res.ZScore != "" {
				zScore = res.ZScore
			}

			anomalies = append(anomalies, map[string]any{
				"key":           s.Key,
				"label":         s.Label,
				"categoryId":    s.Id,
				"parentId":      s.ParentId,
				"currency":      s.Currency,
				"period":        order[i],
				"periodStart":   allPeriods[i].Start,
				"periodEnd":     allPeriods[i].End,
				"amount":        value,
				"hadActivity":   present,
				"mean":          res.Mean,
				"stddev":        res.StdDev,
				"deviation":     res.Deviation,
				"zScore":        zScore,
				"noVariance":    res.NoVar,
				"direction":     dir,
				"historyMonths": len(history),
				"history":       historyView,
				"partialPeriod": !anIsFullMonth(allPeriods[i], mc.Loc),
			})
		}
	}

	sort.SliceStable(anomalies, func(i, j int) bool {
		di, dj := anAbs(anomalies[i]["deviation"].(int64)), anAbs(anomalies[j]["deviation"].(int64))

		if anomalies[i]["period"] != anomalies[j]["period"] {
			return anomalies[i]["period"].(string) > anomalies[j]["period"].(string)
		}

		if di != dj {
			return di > dj
		}

		return anomalies[i]["key"].(string) < anomalies[j]["key"].(string)
	})

	if anomalies == nil {
		anomalies = []map[string]any{}
	}

	c.filters["z"] = zText
	c.filters["minAmount"] = minAmount
	c.filters["lookbackMonths"] = lookback
	c.filters["minHistory"] = minHistory
	c.filters["groupBy"] = groupBy
	c.filters["historyWindow"] = map[string]string{"start": windowRange.Start, "end": windowRange.End}
	c.note("each month of the range is compared with the same category's previous lookback_months months (from the first month the books have data); a month with no transactions in the category counts as zero; stddev is the population standard deviation; zScore is (amount − mean) ÷ stddev to two decimals; the current month may be partial")

	return map[string]any{
		"anomalies":       anomalies,
		"evaluated":       evaluated,
		"evaluatedMonths": order[evalStartIdx:],
		"provenance":      c.provenance(),
	}, nil
}

// ---------------------------------------------------------------------------------------------
// /analytics/import-fallout

// AnalyticsRunFallbackLookup, when set by the ingest family, returns the fallback category ids an
// ingest run used (apis.mdx §14.7 fallback_category_ids), so import-fallout can find a run's
// fallback rows without the caller naming them. found=false means the run is unknown to it.
var AnalyticsRunFallbackLookup func(mc *Ctx, runId string) (categoryIds []int64, found bool, err error)

func anHandleImportFallout(mc *Ctx) (any, error) {
	if err := anCheckQuery(mc, "run_id", "fallback_category_ids", "limit"); err != nil {
		return nil, err
	}

	// the range is optional here: absent means every imported row, whenever it is dated
	var args *anArgs
	var err error

	if mc.Query("start") == "" && mc.Query("end") == "" {
		args, err = anParseArgsNoRange(mc)
	} else {
		today := anToday(mc.Loc)
		args, err = anParseArgs(mc, anDateString(today.AddDate(-49, 0, 0)), anDateString(today))
	}

	if err != nil {
		return nil, err
	}

	sel, err := anResolveSelection(mc, args, true)

	if err != nil {
		return nil, err
	}

	c := &anCall{mc: mc, args: args, sel: sel, sources: map[string]bool{
		"GET /api/v1/accounts/list.json":               true,
		"GET /api/v1/transaction/categories/list.json": true,
	}, filters: map[string]any{}}
	c.args.IncludeHiddenAccounts = true

	if args.ConvertTo != "" {
		if c.cv, err = anLoadConverter(mc, args.ConvertTo); err != nil {
			return nil, err
		}

		c.source("GET /api/v1/exchange_rates/latest.json")
	}

	limit, err := anParseIntArg(mc, "limit", DefaultLimit, 0, 1<<31)

	if err != nil {
		return nil, err
	}

	if limit > MaxLimit {
		limit = MaxLimit
	}

	runId := mc.Query("run_id")
	fallback, err := anParseIdList(mc, "fallback_category_ids")

	if err != nil {
		return nil, err
	}

	fallbackSource := "argument"

	if len(fallback) == 0 && runId != "" && AnalyticsRunFallbackLookup != nil {
		ids, found, err := AnalyticsRunFallbackLookup(mc, runId)

		if err != nil {
			return nil, err
		}

		if found {
			fallback, fallbackSource = ids, "run"
		}
	}

	if len(fallback) == 0 {
		return nil, Invalid("pass fallback_category_ids (the categories the import used as its fallback, e.g. the ids given to /ingest/plan as fallback_category_ids)", "no fallback categories are known for this call")
	}

	for _, id := range fallback {
		if sel.Categories.Get(id) == nil {
			return nil, NotFound("GET /machine/v1/categories lists the category ids", "fallback category %s does not exist", idString(id)).WithDetails(map[string]any{"id": idString(id)})
		}
	}

	if datastore.Container == nil || datastore.Container.UserDataStore == nil {
		return nil, NewFail(CodeNotReady, "wait for the server to finish starting", "the database is not open")
	}

	sess := datastore.Container.UserDataStore.Choose(mc.Uid).NewSession(mc.Web)
	exists, err := sess.IsTableExist(new(MachineImportRecord))

	if err != nil || !exists {
		if err != nil {
			errfile.Caught("checking whether the import record table exists", err)
		}

		return nil, NewFail(CodeNotReady, "no machine-plane import has run yet: import statements first (POST /machine/v1/ingest/plan, then /ingest/apply)", "the import record table does not exist yet")
	}

	var recs []*MachineImportRecord
	q := datastore.Container.UserDataStore.Choose(mc.Uid).NewSession(mc.Web).Where("uid=?", mc.Uid)

	if runId != "" {
		q = q.And("run_id=?", runId)
	}

	if err := q.Find(&recs); err != nil {
		log.Errorf(mc.Web, "[machine.anHandleImportFallout] cannot read import records: %s", err.Error())
		return nil, NewFail(CodeUpstreamError, "read ~/T/ezbookkeeping/error.err for the server-side detail", "cannot read the import records")
	}

	if runId != "" && len(recs) == 0 {
		return nil, NotFound("GET /machine/v1/ingest/runs lists the runs", "no imported rows recorded for run %q", runId)
	}

	c.source("machine_import_record")
	imported := make(map[int64]*MachineImportRecord, len(recs))
	// a transfer imported from two statements has one record per leg under the same transaction
	// id; the record does not carry the account, so both statement keys are reported
	importedKeys := make(map[int64][]string, len(recs))

	for _, r := range recs {
		imported[r.TransactionId] = r

		dup := false

		for _, k := range importedKeys[r.TransactionId] {
			dup = dup || k == r.AccountKey
		}

		if !dup {
			importedKeys[r.TransactionId] = append(importedKeys[r.TransactionId], r.AccountKey)
		}
	}

	// every transaction currently in a fallback category, intersected with the imported rows
	f := anListFilter{CategoryIds: fallback, TagFilter: args.TagFilter}

	if args.Range != nil {
		f.StartUnix, f.EndUnix = args.Range.StartUnix, args.Range.EndUnix
	}

	rows, err := anFetchAllTransactions(mc, f)

	if err != nil {
		return nil, err
	}

	c.source(anListAllSource)
	c.rows = len(rows)
	fallbackSet := map[int64]bool{}

	for _, id := range fallback {
		fallbackSet[id] = true
	}

	kinds := map[anKind]bool{anKindIncome: true, anKindExpense: true, anKindTransferIn: true, anKindTransferOut: true}
	txns := anSelectTxns(rows, sel, mc.Loc, args.UseTransactionTimezone, kinds)

	b := &anBucketer{Interval: anIntervalMonth, Loc: mc.Loc}
	set := newAnSeriesSet()
	var detail []map[string]any
	var count int64

	for _, t := range txns {
		rec := imported[anParseIdOrZero(t.Id)]

		if rec == nil || !fallbackSet[t.CategoryId] {
			continue
		}

		count++
		acc := sel.Accounts.Get(t.AccountId)
		s := set.get(idString(t.AccountId), t.Currency, func(s *anSeries) {
			s.Kind, s.Id, s.Count = "account", idString(t.AccountId), anZeroCount()

			if acc != nil {
				s.Label = acc.Name
			}
		})

		if err := s.add(b.Key(t.Day), t.Amount); err != nil {
			return nil, err
		}

		*s.Count++

		if int64(len(detail)) < limit {
			detail = append(detail, map[string]any{
				"transactionId": t.Id,
				"date":          t.Date,
				"amount":        t.Amount,
				"currency":      t.Currency,
				"accountId":     idString(t.AccountId),
				"categoryId":    idString(t.CategoryId),
				"comment":       t.Comment,
				"runId":         rec.RunId,
				"sourceFile":    rec.SourceFile,
				"accountKey":    strings.Join(importedKeys[rec.TransactionId], " / "),
			})
		}
	}

	if int64(len(detail)) < count {
		mc.Truncated(int(limit))
	}

	if detail == nil {
		detail = []map[string]any{}
	}

	series := set.list()
	var months []string
	monthSet := map[string]bool{}

	for _, s := range series {
		for p := range s.points {
			if !monthSet[p] {
				monthSet[p] = true
				months = append(months, p)
			}
		}
	}

	sort.Strings(months)
	native := anCloneSeriesList(series)
	out, unconverted, err := c.finishOrdered(series, months)

	if err != nil {
		return nil, err
	}

	c.filters["runId"] = runId
	c.filters["fallbackCategoryIds"] = anIdStrings(fallback)
	c.filters["fallbackCategoriesFrom"] = fallbackSource
	c.filters["limit"] = limit
	c.note("rows are listed while they are still in a fallback category: re-categorising a row removes it from this report")

	data := map[string]any{
		"runId":        runId,
		"months":       months,
		"series":       out,
		"unconverted":  unconverted,
		"rows":         detail,
		"count":        count,
		"importedRows": len(recs),
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

// anIsFullMonth reports whether a clipped month period covers its whole calendar month
func anIsFullMonth(p anPeriod, loc *time.Location) bool {
	start := anDayStart(p.Start, loc)
	first := time.Date(start.Year(), start.Month(), 1, 0, 0, 0, 0, loc)

	return start.Equal(first) && p.End == anDateString(first.AddDate(0, 1, -1))
}

// anParseArgsNoRange parses the shared arguments without a date range
func anParseArgsNoRange(mc *Ctx) (*anArgs, error) {
	args, err := anParseArgs(mc, "1970-01-02", "1970-01-02")

	if err != nil {
		return nil, err
	}

	args.Range = nil

	return args, nil
}

func anParseIdOrZero(s string) int64 {
	n, err := strconv.ParseInt(s, 10, 64)

	if err != nil {
		errfile.Expected("parsing an id argument", err)
		return 0
	}

	return n
}

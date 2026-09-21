package machine

import (
	"sort"
	"strconv"

	"github.com/mayswind/ezbookkeeping/pkg/models"
)

// analytics_group.go — the ONE implementation of grouping (apis.mdx §12.3): classification of an
// upstream statistics row into income / expense / transfer, primary-category roll-up, per-currency
// series, conversion of each currency's total once, ranking, top-N and the "other" bucket. Every
// analytics route that groups goes through here; nothing else sums a breakdown.

// anKind is the direction of one statistics row
type anKind string

const (
	anKindIncome      anKind = "income"
	anKindExpense     anKind = "expense"
	anKindTransferIn  anKind = "transfer_in"
	anKindTransferOut anKind = "transfer_out"
)

// Group-by values
const (
	anGroupPrimary   = "primary"
	anGroupSecondary = "secondary"
	anGroupAccount   = "account"
	anGroupKind      = "kind"
	anGroupTotal     = "total"
)

// anOtherKey is the key of the bucket that holds everything below top_n
const anOtherKey = "other"

// anClassify decides what one statistics row is, the way statistics.ts does: the category's type
// says income or expense; a transfer-category row is money out of its account when upstream marks
// the related account "transfer to", and money in when "transfer from". Balance modifications
// never reach here: upstream's statistics endpoints do not return them.
func anClassify(cat *anCategory, relatedType models.TransactionRelatedAccountType) (anKind, bool) {
	if cat == nil {
		return "", false
	}

	switch cat.Type {
	case models.CATEGORY_TYPE_INCOME:
		return anKindIncome, true
	case models.CATEGORY_TYPE_EXPENSE:
		return anKindExpense, true
	case models.CATEGORY_TYPE_TRANSFER:
		switch relatedType {
		case models.TRANSACTION_RELATED_ACCOUNT_TYPE_TRANSFER_TO:
			return anKindTransferOut, true
		case models.TRANSACTION_RELATED_ACCOUNT_TYPE_TRANSFER_FROM:
			return anKindTransferIn, true
		}
	}

	return "", false
}

// anSigned applies the analytics sign convention (§17.2): money out is negative
func anSigned(kind anKind, amount int64) int64 {
	if kind == anKindExpense || kind == anKindTransferOut {
		return -amount
	}

	return amount
}

// anKindLabel is the human label of a kind series
func anKindLabel(kind anKind) string {
	switch kind {
	case anKindIncome:
		return "Income"
	case anKindExpense:
		return "Expense"
	case anKindTransferIn:
		return "Transfers in"
	case anKindTransferOut:
		return "Transfers out"
	}

	return string(kind)
}

// anPoint is one period of a series
type anPoint struct {
	Period string `json:"period"`
	Amount int64  `json:"amount"`
}

// anCurrencyAmount is one currency's figure
type anCurrencyAmount struct {
	Currency string `json:"currency"`
	Amount   int64  `json:"amount"`
}

// anSeries is one chart-ready series (§12.4): {key, label, currency, points[{period, amount}]}
type anSeries struct {
	Key        string             `json:"key"`
	Label      string             `json:"label"`
	Currency   string             `json:"currency"`
	Kind       string             `json:"kind,omitempty"`
	Id         string             `json:"id,omitempty"`
	ParentId   string             `json:"parentId,omitempty"`
	Hidden     bool               `json:"hidden,omitempty"`
	Total      int64              `json:"total"`
	Count      *int64             `json:"count,omitempty"`
	Points     []anPoint          `json:"points"`
	Members    []string           `json:"members,omitempty"`
	ByCurrency []anCurrencyAmount `json:"byCurrency,omitempty"`

	points map[string]int64
	rank   int
}

func newAnSeries(key, label, currency string) *anSeries {
	return &anSeries{Key: key, Label: label, Currency: currency, points: map[string]int64{}}
}

// add accumulates an amount into one period, refusing to leave the safe range
func (s *anSeries) add(period string, amount int64) error {
	if s.points == nil {
		s.points = map[string]int64{}
	}

	v, err := CheckedAdd(s.points[period], amount)

	if err != nil {
		return err
	}

	s.points[period] = v

	t, err := CheckedAdd(s.Total, amount)

	if err != nil {
		return err
	}

	s.Total = t

	return nil
}

// finalize renders the points in period order. Periods with no data are ABSENT, not zero (R3).
func (s *anSeries) finalize(order []string) {
	s.Points = make([]anPoint, 0, len(s.points))
	seen := map[string]bool{}

	for _, p := range order {
		if v, ok := s.points[p]; ok {
			s.Points = append(s.Points, anPoint{Period: p, Amount: v})
			seen[p] = true
		}
	}

	var rest []string

	for p := range s.points {
		if !seen[p] {
			rest = append(rest, p)
		}
	}

	sort.Strings(rest)

	for _, p := range rest {
		s.Points = append(s.Points, anPoint{Period: p, Amount: s.points[p]})
	}
}

// anSeriesSet accumulates series keyed by (key, currency), in first-seen order
type anSeriesSet struct {
	byKey map[string]*anSeries
	order []string
}

func newAnSeriesSet() *anSeriesSet {
	return &anSeriesSet{byKey: map[string]*anSeries{}}
}

// get returns the series for (key, currency), creating it with init when absent
func (ss *anSeriesSet) get(key, currency string, init func(s *anSeries)) *anSeries {
	k := key + "\x00" + currency

	if s, ok := ss.byKey[k]; ok {
		return s
	}

	s := newAnSeries(key, "", currency)

	if init != nil {
		init(s)
	}

	ss.byKey[k] = s
	ss.order = append(ss.order, k)

	return s
}

// list returns every series in first-seen order
func (ss *anSeriesSet) list() []*anSeries {
	out := make([]*anSeries, 0, len(ss.order))

	for _, k := range ss.order {
		out = append(out, ss.byKey[k])
	}

	return out
}

// anGroupOpts says how anGroupFacts groups
type anGroupOpts struct {
	GroupBy string
	Kinds   map[anKind]bool
}

// anGroupStats reports what grouping used and skipped, for provenance
type anGroupStats struct {
	RowsUsed            int `json:"rowsUsed"`
	RowsOutsideFilters  int `json:"rowsOutsideFilters"`
	RowsUnknownEntities int `json:"rowsUnknownEntities"`
}

// anGroupFacts groups statistics rows into per-currency series. Filters (accounts, categories,
// kinds) are applied here — once — the way statistics.ts getCategoryTotalAmountItems applies them:
// a row whose account, primary account, category or primary category is unknown is skipped.
func anGroupFacts(facts []anFact, sel *anSelection, opts anGroupOpts) ([]*anSeries, anGroupStats, error) {
	set := newAnSeriesSet()
	var st anGroupStats

	for _, f := range facts {
		account := sel.Accounts.Get(f.AccountId)

		if account == nil || (account.ParentId > 0 && sel.Accounts.Get(account.ParentId) == nil) {
			st.RowsUnknownEntities++
			continue
		}

		cat := sel.Categories.Get(f.CategoryId)
		primary := sel.Categories.Primary(cat)

		if cat == nil || primary == nil {
			st.RowsUnknownEntities++
			continue
		}

		kind, ok := anClassify(cat, f.RelatedAccountType)

		if !ok || (opts.Kinds != nil && !opts.Kinds[kind]) {
			continue
		}

		if !sel.AccountIncluded(account.Id) || !sel.CategoryIncluded(cat.Id) {
			st.RowsOutsideFilters++
			continue
		}

		amount := anSigned(kind, f.Amount)
		var s *anSeries

		switch opts.GroupBy {
		case anGroupPrimary:
			s = set.get(idString(primary.Id), account.Currency, func(s *anSeries) {
				s.Label, s.Kind, s.Id, s.Hidden, s.rank = primary.Name, "category", idString(primary.Id), primary.Hidden, int(primary.DisplayOrder)
			})
		case anGroupSecondary:
			s = set.get(idString(cat.Id), account.Currency, func(s *anSeries) {
				s.Label, s.Kind, s.Id, s.Hidden = cat.Name, "category", idString(cat.Id), cat.Hidden || primary.Hidden

				if primary.Id != cat.Id {
					s.ParentId = idString(primary.Id)
				}
			})
		case anGroupAccount:
			s = set.get(idString(account.Id), account.Currency, func(s *anSeries) {
				s.Label, s.Kind, s.Id, s.Hidden = account.Name, "account", idString(account.Id), account.Hidden

				if account.ParentId > 0 {
					s.ParentId = idString(account.ParentId)
				}
			})
		case anGroupKind:
			s = set.get(string(kind), account.Currency, func(s *anSeries) {
				s.Label, s.Kind = anKindLabel(kind), string(kind)
			})
		default:
			s = set.get(anGroupTotal, account.Currency, func(s *anSeries) {
				s.Label, s.Kind = "Total", anGroupTotal
			})
		}

		if err := s.add(f.Bucket, amount); err != nil {
			return nil, st, err
		}

		st.RowsUsed++
	}

	return set.list(), st, nil
}

// anNativeTotals sums series totals per currency (never across currencies)
func anNativeTotals(series []*anSeries) (map[string]int64, error) {
	out := map[string]int64{}

	for _, s := range series {
		v, err := CheckedAdd(out[s.Currency], s.Total)

		if err != nil {
			return nil, err
		}

		out[s.Currency] = v
	}

	return out, nil
}

// anTotalsList renders per-currency totals in currency order
func anTotalsList(totals map[string]int64) []anCurrencyAmount {
	out := make([]anCurrencyAmount, 0, len(totals))

	for c, v := range totals {
		out = append(out, anCurrencyAmount{Currency: c, Amount: v})
	}

	sort.Slice(out, func(i, j int) bool { return out[i].Currency < out[j].Currency })

	return out
}

// anConvertTotals converts per-currency totals once each and adds them; currencies without a rate
// come back unconverted
func anConvertTotals(totals map[string]int64, cv *anConverter) (int64, []anCurrencyAmount, []anCurrencyAmount, error) {
	var sum int64
	var sources, unconverted []anCurrencyAmount

	for _, t := range anTotalsList(totals) {
		v, ok := cv.Convert(t.Currency, t.Amount)

		if !ok {
			unconverted = append(unconverted, t)
			continue
		}

		sources = append(sources, t)

		var err error

		if sum, err = CheckedAdd(sum, v); err != nil {
			return 0, nil, nil, err
		}
	}

	return sum, sources, unconverted, nil
}

// anConvertSeries merges the per-currency series of each key into one series in the target
// currency: each currency's per-period sum and total are converted once and then added (§13.2).
// Series in a currency with no rate come back in unconverted, in their own currency.
func anConvertSeries(series []*anSeries, cv *anConverter) ([]*anSeries, []*anSeries, error) {
	merged := newAnSeriesSet()
	var unconverted []*anSeries

	for _, s := range series {
		if !cv.CanConvert(s.Currency) {
			cv.Convert(s.Currency, 0) // records the missing currency
			unconverted = append(unconverted, s)
			continue
		}

		m := merged.get(s.Key, cv.Target, func(m *anSeries) {
			m.Label, m.Kind, m.Id, m.ParentId, m.Hidden, m.rank, m.Members = s.Label, s.Kind, s.Id, s.ParentId, s.Hidden, s.rank, s.Members
		})

		for period, v := range s.points {
			conv, _ := cv.Convert(s.Currency, v)

			if _, ok := m.points[period]; !ok {
				m.points[period] = 0
			}

			sum, err := CheckedAdd(m.points[period], conv)

			if err != nil {
				return nil, nil, err
			}

			m.points[period] = sum
		}

		conv, _ := cv.Convert(s.Currency, s.Total)
		total, err := CheckedAdd(m.Total, conv)

		if err != nil {
			return nil, nil, err
		}

		m.Total = total
		m.ByCurrency = append(m.ByCurrency, anCurrencyAmount{Currency: s.Currency, Amount: s.Total})

		if s.Count != nil {
			n := *s.Count

			if m.Count != nil {
				n += *m.Count
			}

			m.Count = &n
		}
	}

	out := merged.list()

	for _, m := range out {
		sort.Slice(m.ByCurrency, func(i, j int) bool { return m.ByCurrency[i].Currency < m.ByCurrency[j].Currency })

		// a single-currency series already in the target needs no byCurrency echo
		if len(m.ByCurrency) == 1 && m.ByCurrency[0].Currency == cv.Target {
			m.ByCurrency = nil
		}
	}

	return out, unconverted, nil
}

func anAbs(v int64) int64 {
	if v < 0 {
		return -v
	}

	return v
}

// anRankTopN orders series within each currency by magnitude (largest first, ties by key) and,
// when topN > 0, folds everything past the first topN of a currency into one "other" series. This
// is the only place top-N and "other" are computed. Currencies are never ranked against each other.
func anRankTopN(series []*anSeries, topN int) ([]*anSeries, error) {
	byCurrency := map[string][]*anSeries{}
	var currencies []string

	for _, s := range series {
		if _, ok := byCurrency[s.Currency]; !ok {
			currencies = append(currencies, s.Currency)
		}

		byCurrency[s.Currency] = append(byCurrency[s.Currency], s)
	}

	sort.Strings(currencies)
	var out []*anSeries

	for _, c := range currencies {
		list := byCurrency[c]

		sort.SliceStable(list, func(i, j int) bool {
			ai, aj := anAbs(list[i].Total), anAbs(list[j].Total)

			if ai != aj {
				return ai > aj
			}

			return list[i].Key < list[j].Key
		})

		if topN <= 0 || len(list) <= topN {
			out = append(out, list...)
			continue
		}

		out = append(out, list[:topN]...)
		other := newAnSeries(anOtherKey, "Other", c)
		other.Kind = anOtherKey
		byCur := map[string]int64{}
		var count int64
		haveCount := false

		for _, s := range list[topN:] {
			other.Members = append(other.Members, s.Key)

			for p, v := range s.points {
				sum, err := CheckedAdd(other.points[p], v)

				if err != nil {
					return nil, err
				}

				other.points[p] = sum
			}

			t, err := CheckedAdd(other.Total, s.Total)

			if err != nil {
				return nil, err
			}

			other.Total = t

			for _, bc := range s.ByCurrency {
				v, err := CheckedAdd(byCur[bc.Currency], bc.Amount)

				if err != nil {
					return nil, err
				}

				byCur[bc.Currency] = v
			}

			if s.Count != nil {
				count += *s.Count
				haveCount = true
			}
		}

		if len(byCur) > 0 {
			other.ByCurrency = anTotalsList(byCur)
		}

		if haveCount {
			other.Count = &count
		}

		out = append(out, other)
	}

	return out, nil
}

// anFinalizeAll renders every series' points in period order
func anFinalizeAll(series []*anSeries, order []string) []*anSeries {
	if series == nil {
		return []*anSeries{}
	}

	for _, s := range series {
		s.finalize(order)
	}

	return series
}

// anPeriodOrder is the keys of periods, in order
func anPeriodOrder(periods []anPeriod) []string {
	out := make([]string, len(periods))

	for i, p := range periods {
		out[i] = p.Period
	}

	return out
}

// anZeroCount is a counted zero (§17.3): the entity is listed with amount 0 AND count 0
func anZeroCount() *int64 {
	var z int64
	return &z
}

// anIncludeZeroCategories adds a counted-zero series for every category of the requested kinds
// (and group level) that passed the filters but had no rows, in each currency present (or the
// fallback currency when nothing is present)
func anIncludeZeroCategories(series []*anSeries, sel *anSelection, groupBy string, types map[models.TransactionCategoryType]bool, fallbackCurrency string) []*anSeries {
	present := map[string]bool{}
	currencies := map[string]bool{}

	for _, s := range series {
		present[s.Key+"\x00"+s.Currency] = true
		currencies[s.Currency] = true
	}

	if len(currencies) == 0 && fallbackCurrency != "" {
		currencies[fallbackCurrency] = true
	}

	curList := anSortedKeys(currencies)

	for _, id := range sel.Categories.order {
		c := sel.Categories.Get(id)

		if c == nil || !types[c.Type] || !sel.CategoryIncluded(c.Id) {
			continue
		}

		isPrimary := c.ParentId == 0

		if groupBy == anGroupPrimary && !isPrimary {
			continue
		}

		if groupBy == anGroupSecondary && isPrimary && len(c.Children) > 0 {
			continue
		}

		for _, cur := range curList {
			if present[idString(c.Id)+"\x00"+cur] {
				continue
			}

			s := newAnSeries(idString(c.Id), c.Name, cur)
			s.Kind, s.Id, s.Hidden, s.Count = "category", idString(c.Id), c.Hidden, anZeroCount()

			if !isPrimary {
				s.ParentId = strconv.FormatInt(c.ParentId, 10)
			}

			series = append(series, s)
		}
	}

	return series
}

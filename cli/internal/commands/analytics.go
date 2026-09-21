package commands

// analytics.go — `ezbk analytics …` (cli.mdx §9.3, apis.mdx §12). Every total comes from the server:
// these verbs parse flags, resolve relative dates LOCALLY (the wire never carries one), make one call,
// print the answer's provenance on stderr and render. They never sum, convert or round money.
//
// --format csv emits one row per (series, period) with the columns
// series,period,currency,amount_hundredths,amount — shaped to pipe straight into a plotting tool.

import (
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/BryanStarbuck/ezbookkeeping/cli/internal/app"
	"github.com/BryanStarbuck/ezbookkeeping/cli/internal/client"
	"github.com/BryanStarbuck/ezbookkeeping/cli/internal/render"
)

const asGroup = "ANALYTICS"

// asSeriesCSVHeader is the charting contract (cli.mdx §9.3)
var asSeriesCSVHeader = []string{"series", "period", "currency", "amount_hundredths", "amount"}

// ---------------------------------------------------------------------------------------------
// Relative dates — resolved locally, in the resolved timezone, before the call (cli.mdx §7.5)
// ---------------------------------------------------------------------------------------------

var (
	asDayRe   = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)
	asMonthRe = regexp.MustCompile(`^\d{4}-\d{2}$`)
	asYearRe  = regexp.MustCompile(`^\d{4}$`)
)

// asRelativeWords are the relative spellings the CLI accepts (hyphen or underscore)
var asRelativeWords = []string{"today", "yesterday", "this-month", "last-month", "this-year", "last-year"}

func asNormWord(v string) string {
	return strings.ReplaceAll(strings.ToLower(strings.TrimSpace(v)), "_", "-")
}

// asSpan expands a date expression into the inclusive day range it names. It accepts YYYY-MM-DD,
// YYYY-MM, YYYY and the relative words. relative reports whether the expression depended on "now".
func asSpan(v string, now time.Time) (start, end time.Time, relative bool, err error) {
	loc := now.Location()
	w := asNormWord(v)
	day := func(t time.Time) time.Time { return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, loc) }
	today := day(now)

	switch w {
	case "today":
		return today, today, true, nil
	case "yesterday":
		y := today.AddDate(0, 0, -1)
		return y, y, true, nil
	case "this-month":
		s := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, loc)
		return s, s.AddDate(0, 1, -1), true, nil
	case "last-month":
		s := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, loc).AddDate(0, -1, 0)
		return s, s.AddDate(0, 1, -1), true, nil
	case "this-year":
		s := time.Date(now.Year(), 1, 1, 0, 0, 0, 0, loc)
		return s, s.AddDate(1, 0, -1), true, nil
	case "last-year":
		s := time.Date(now.Year()-1, 1, 1, 0, 0, 0, 0, loc)
		return s, s.AddDate(1, 0, -1), true, nil
	}

	raw := strings.TrimSpace(v)

	switch {
	case asDayRe.MatchString(raw):
		t, perr := time.ParseInLocation("2006-01-02", raw, loc)

		if perr != nil {
			return time.Time{}, time.Time{}, false, fmt.Errorf("%q is not a real date", raw)
		}

		return t, t, false, nil
	case asMonthRe.MatchString(raw):
		t, perr := time.ParseInLocation("2006-01", raw, loc)

		if perr != nil {
			return time.Time{}, time.Time{}, false, fmt.Errorf("%q is not a real month", raw)
		}

		return t, t.AddDate(0, 1, -1), false, nil
	case asYearRe.MatchString(raw):
		t, perr := time.ParseInLocation("2006", raw, loc)

		if perr != nil {
			return time.Time{}, time.Time{}, false, fmt.Errorf("%q is not a year", raw)
		}

		return t, t.AddDate(1, 0, -1), false, nil
	}

	return time.Time{}, time.Time{}, false, fmt.Errorf("%q is not a date: use YYYY-MM-DD, YYYY-MM, YYYY or one of %s", v, strings.Join(asRelativeWords, ", "))
}

// asResolveDay resolves one --start / --end value to a YYYY-MM-DD. A span expression resolves to
// its first day as a start and its last day as an end.
func asResolveDay(v string, isEnd bool, now time.Time) (string, bool, error) {
	s, e, rel, err := asSpan(v, now)

	if err != nil {
		return "", false, err
	}

	if isEnd {
		return e.Format("2006-01-02"), rel || !asDayRe.MatchString(strings.TrimSpace(v)), nil
	}

	return s.Format("2006-01-02"), rel || !asDayRe.MatchString(strings.TrimSpace(v)), nil
}

// asDateRange is a resolved range and a note of what was resolved, for stderr
type asDateRange struct {
	Start, End string
	Notes      []string
}

// asResolveRange reads --period, --start and --end. --period (a span expression) sets both ends and
// cannot be combined with them. required refuses an empty range with a usage error.
func asResolveRange(c *app.Ctx, now time.Time, required bool) (*asDateRange, error) {
	return asResolveRangeWith(c, now, required, true)
}

// asResolveRangeWith is asResolveRange; periodIsSpan false ignores --period (on `analytics summary`
// it names the server's periods instead of a span)
func asResolveRangeWith(c *app.Ctx, now time.Time, required, periodIsSpan bool) (*asDateRange, error) {
	r := &asDateRange{}
	period := ""

	if periodIsSpan {
		period = c.String("period")
	}

	if period != "" {
		if c.Has("start") || c.Has("end") {
			return nil, app.Usage("--period cannot be combined with --start/--end", "use either --period last-month, or --start D --end D")
		}

		s, e, _, err := asSpan(period, now)

		if err != nil {
			return nil, app.Usage("--period: "+err.Error(), "ezbk help "+c.Verb.Name)
		}

		r.Start, r.End = s.Format("2006-01-02"), e.Format("2006-01-02")
		r.Notes = append(r.Notes, fmt.Sprintf("--period %s → %s .. %s", period, r.Start, r.End))

		return r, nil
	}

	for _, side := range []struct {
		flag  string
		isEnd bool
		dst   *string
	}{{"start", false, &r.Start}, {"end", true, &r.End}} {
		v := c.String(side.flag)

		if v == "" {
			continue
		}

		d, resolved, err := asResolveDay(v, side.isEnd, now)

		if err != nil {
			return nil, app.Usage("--"+side.flag+": "+err.Error(), "ezbk help "+c.Verb.Name)
		}

		*side.dst = d

		if resolved {
			r.Notes = append(r.Notes, fmt.Sprintf("--%s %s → %s", side.flag, v, d))
		}
	}

	if required && (r.Start == "" || r.End == "") {
		missing := "--start and --end"

		if r.Start != "" {
			missing = "--end"
		} else if r.End != "" {
			missing = "--start"
		}

		return nil, app.Usage("`ezbk "+c.Verb.Name+"` needs "+missing, "pass --start D --end D (YYYY-MM-DD, or this-month, last-year, 2025, 2025-06), or --period last-month")
	}

	if r.Start != "" && r.End != "" && r.End < r.Start {
		return nil, app.Usage(fmt.Sprintf("--end %s is before --start %s", r.End, r.Start), "swap them")
	}

	return r, nil
}

// asNow is the clock in the resolved timezone (a variable so tests can pin it)
var asNow = func(loc *time.Location) time.Time { return time.Now().In(loc) }

// ---------------------------------------------------------------------------------------------
// Flags shared by the analytics verbs
// ---------------------------------------------------------------------------------------------

var (
	asFlagStart  = app.Flag{Name: "start", Value: "YYYY-MM-DD", Help: "first day, inclusive (also YYYY-MM, YYYY, today, yesterday, this-month, last-month, this-year, last-year)"}
	asFlagEnd    = app.Flag{Name: "end", Value: "YYYY-MM-DD", Help: "last day, inclusive (same spellings as --start; a month or year means its last day)"}
	asFlagPeriod = app.Flag{Name: "period", Value: "SPAN", Help: "shorthand for --start/--end: this-month, last-month, this-year, last-year, 2025, 2025-06"}

	asFlagAccount   = app.Flag{Name: "account", Value: "id|name", Repeat: true, Help: "only these accounts (resolved server-side; repeatable)"}
	asFlagCategory  = app.Flag{Name: "category", Value: "id|name", Repeat: true, Help: "only these categories (repeatable)"}
	asFlagExclude   = app.Flag{Name: "exclude-category", Value: "id|name", Repeat: true, Help: "leave these categories out (repeatable)"}
	asFlagTagFilter = app.Flag{Name: "tag-filter", Value: "EXPR", Help: "upstream's tag_filter expression, passed through verbatim"}
	asFlagHidden    = app.Flag{Name: "include-hidden", Help: "include hidden accounts (excluded by default; the response says which)"}
	asFlagTransfers = app.Flag{Name: "include-transfers", Help: "count transfers (excluded by default — a transfer is not spending)"}
	asFlagConvert   = app.Flag{Name: "convert-to", Value: "CUR", Help: "convert every figure with the app's own rates (named on stderr); per currency without it"}
	asFlagTxnTZ     = app.Flag{Name: "use-transaction-timezone", Help: "bucket by each transaction's own timezone instead of --tz"}
	asFlagInterval  = app.Flag{Name: "interval", Value: "none|day|week|month|quarter|year", Help: "the period each point covers"}
	asFlagTop       = app.Flag{Name: "top", Value: "N", Help: "keep the N largest; the rest go to the server's \"other\" bucket"}
)

// asRangeFlags are the flags every date-ranged analytics verb takes
func asRangeFlags(extra ...app.Flag) []app.Flag {
	flags := []app.Flag{asFlagStart, asFlagEnd, asFlagPeriod, asFlagAccount, asFlagCategory, asFlagExclude, asFlagTagFilter, asFlagHidden, asFlagTransfers, asFlagConvert, asFlagTxnTZ}

	return append(flags, extra...)
}

// asValidateCurrency refuses a malformed --convert-to before the round trip (the server re-checks)
func asValidateCurrency(v string) (string, error) {
	cur := strings.ToUpper(strings.TrimSpace(v))

	if cur == "" {
		return "", nil
	}

	if len(cur) != 3 || strings.Trim(cur, "ABCDEFGHIJKLMNOPQRSTUVWXYZ") != "" {
		return "", app.Usage(fmt.Sprintf("--convert-to %q is not an ISO 4217 code", v), "e.g. --convert-to USD")
	}

	return cur, nil
}

// asValidateInterval checks --interval against the values a route accepts
func asValidateInterval(v string, allowed ...string) (string, error) {
	v = strings.ToLower(strings.TrimSpace(v))

	if v == "" {
		return "", nil
	}

	for _, a := range allowed {
		if v == a {
			return v, nil
		}
	}

	return "", app.Usage(fmt.Sprintf("--interval %q is not one of %s", v, strings.Join(allowed, ", ")), "")
}

// asPositiveInt reads an optional positive integer flag
func asPositiveInt(c *app.Ctx, name string) (string, error) {
	if !c.Has(name) {
		return "", nil
	}

	n, err := c.Int(name, 0)

	if err != nil {
		return "", err
	}

	if n <= 0 {
		return "", app.Usage("--"+name+" must be a positive whole number", "")
	}

	return strconv.Itoa(n), nil
}

// asBuildQuery turns the shared flags into the route's snake_case query (apis.mdx §12.2)
func asBuildQuery(c *app.Ctx, r *asDateRange) (url.Values, error) {
	q := url.Values{}

	if r != nil {
		if r.Start != "" {
			q.Set("start", r.Start)
		}

		if r.End != "" {
			q.Set("end", r.End)
		}
	}

	for _, l := range []struct{ flag, arg, kind, list string }{
		{"account", "account_ids", "account", "/accounts"},
		{"category", "category_ids", "category", "/categories"},
		{"exclude-category", "exclude_category_ids", "category", "/categories"},
	} {
		ids, err := asResolveIds(c, l.kind, l.flag, l.list, c.Strings(l.flag))

		if err != nil {
			return nil, err
		}

		app.AddList(q, l.arg, ids)
	}

	if v := c.String("tag-filter"); v != "" {
		q.Set("tag_filter", v)
	}

	if c.Bool("include-hidden") {
		q.Set("include_hidden_accounts", "true")
	}

	if c.Bool("include-transfers") {
		q.Set("include_transfers", "true")
	}

	if c.Bool("use-transaction-timezone") {
		q.Set("use_transaction_timezone", "true")
	}

	cur, err := asValidateCurrency(c.String("convert-to"))

	if err != nil {
		return nil, err
	}

	if cur != "" {
		q.Set("convert_to", cur)
	}

	return q, nil
}

// ---------------------------------------------------------------------------------------------
// Series flattening — reshaping only; no arithmetic on any amount
// ---------------------------------------------------------------------------------------------

// asSeriesRow is one (series, period) point
type asSeriesRow struct {
	Series   string
	Period   string
	Currency string
	Amount   any // the server's value verbatim (json.Number); nil when absent (absent is not zero)
}

// asPointMoneyKeys are the amount fields a point may carry when it has no single `amount`
var asPointMoneyKeys = []string{"income", "expense", "net", "in", "out", "inflow", "outflow", "opening", "closing", "assets", "liabilities", "netWorth", "balance", "total", "mean", "median", "converted"}

func asStr(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case json.Number:
		return t.String()
	}

	return fmt.Sprint(v)
}

func asFirstStr(m map[string]any, keys ...string) string {
	for _, k := range keys {
		if s := asStr(m[k]); s != "" {
			return s
		}
	}

	return ""
}

// asFlattenSeries finds every `series` array in data — at the top or nested under a per-currency
// group — and returns one row per point. Currency is inherited from the nearest enclosing object
// that names one. ok is false when data holds no series at all.
func asFlattenSeries(data any) ([]asSeriesRow, bool) {
	var rows []asSeriesRow
	found := false

	var walk func(v any, currency, prefix string)

	walk = func(v any, currency, prefix string) {
		switch t := v.(type) {
		case map[string]any:
			if cur := asFirstStr(t, "currency", "convertedCurrency"); cur != "" {
				currency = cur
			}

			var seriesLists [][]any

			for _, key := range []string{"series", "unconverted"} {
				if s, ok := t[key].([]any); ok && (key == "series" || asLooksLikeSeries(s)) {
					found = true
					seriesLists = append(seriesLists, s)
				}
			}

			for _, s := range seriesLists {
				for _, item := range s {
					sm, ok := item.(map[string]any)

					if !ok {
						continue
					}

					label := asFirstStr(sm, "label", "name", "key", "id")

					if prefix != "" && label != "" {
						label = prefix + "/" + label
					} else if label == "" {
						label = prefix
					}

					cur := currency

					if c := asFirstStr(sm, "currency"); c != "" {
						cur = c
					}

					points, _ := sm["points"].([]any)

					if points == nil {
						points, _ = sm["values"].([]any)
					}

					for _, p := range points {
						pm, ok := p.(map[string]any)

						if !ok {
							continue
						}

						pcur := cur

						if c := asFirstStr(pm, "currency"); c != "" {
							pcur = c
						}

						period := asFirstStr(pm, "period", "date", "month", "key", "label")

						if _, has := pm["amount"]; has {
							rows = append(rows, asSeriesRow{Series: label, Period: period, Currency: pcur, Amount: pm["amount"]})
							continue
						}

						emitted := false

						for _, k := range asPointMoneyKeys {
							if val, has := pm[k]; has {
								rows = append(rows, asSeriesRow{Series: label + "." + k, Period: period, Currency: pcur, Amount: val})
								emitted = true
							}
						}

						if !emitted {
							// absent is not zero: the period is listed with an empty amount
							rows = append(rows, asSeriesRow{Series: label, Period: period, Currency: pcur})
						}
					}
				}
			}

			keys := make([]string, 0, len(t))

			for k := range t {
				if k != "series" && k != "unconverted" && k != "provenance" {
					keys = append(keys, k)
				}
			}

			sort.Strings(keys)

			for _, k := range keys {
				switch t[k].(type) {
				case map[string]any, []any:
					walk(t[k], currency, prefix)
				}
			}
		case []any:
			for _, item := range t {
				walk(item, currency, prefix)
			}
		}
	}

	walk(data, "", "")

	return rows, found
}

// asLooksLikeSeries reports whether a list holds series objects (each with points)
func asLooksLikeSeries(l []any) bool {
	for _, item := range l {
		if m, ok := item.(map[string]any); ok {
			if _, has := m["points"]; has {
				return true
			}
		}
	}

	return false
}

// asSeriesRecords renders flattened rows as the charting CSV contract
func asSeriesRecords(rows []asSeriesRow) [][]string {
	out := make([][]string, 0, len(rows))

	for _, r := range rows {
		n, ok := render.ToInt64(r.Amount)

		if !ok || r.Amount == nil {
			out = append(out, []string{r.Series, r.Period, r.Currency, "", ""})
			continue
		}

		out = append(out, []string{r.Series, r.Period, r.Currency, strconv.FormatInt(n, 10), render.Hundredths(n, false)})
	}

	return out
}

// asSeriesMaps turns rows into generic maps for the table renderer
func asSeriesMaps(rows []asSeriesRow) []any {
	out := make([]any, 0, len(rows))

	for _, r := range rows {
		out = append(out, map[string]any{"series": r.Series, "period": r.Period, "currency": r.Currency, "amount": r.Amount})
	}

	return out
}

var asSeriesColumns = []render.Column{
	{Header: "Series", Key: "series"},
	{Header: "Period", Key: "period"},
	{Header: "Amount", Key: "amount", Kind: render.KMoney, CurrencyKey: "currency"},
	{Header: "Currency", Key: "currency"},
}

// ---------------------------------------------------------------------------------------------
// Provenance — the answer's scope, on stderr (cli.mdx §9.3, apis.mdx §12.3)
// ---------------------------------------------------------------------------------------------

func asProvenance(data map[string]any) map[string]any {
	if p, ok := data["provenance"].(map[string]any); ok {
		return p
	}

	return nil
}

// asProvenanceLines renders a provenance object as stderr lines
func asProvenanceLines(p map[string]any, meta map[string]any, verbose bool) []string {
	if p == nil && meta == nil {
		return nil
	}

	var lines []string
	add := func(k, v string) {
		if v != "" {
			lines = append(lines, fmt.Sprintf("  %-12s %s", k, v))
		}
	}

	used := map[string]bool{}
	take := func(keys ...string) string {
		for _, k := range keys {
			if v, ok := p[k]; ok && v != nil {
				used[k] = true

				if s := asStr(v); s != "" {
					return s
				}
			}
		}

		return ""
	}

	if p != nil {
		rng := ""

		if r, ok := p["range"].(map[string]any); ok {
			used["range"] = true
			rng = asFirstStr(r, "start") + " .. " + asFirstStr(r, "end")

			if tz := asFirstStr(r, "timezone"); tz != "" {
				rng += "  (" + tz + ")"
			}
		} else if s, e := take("start"), take("end"); s != "" || e != "" {
			rng = s + " .. " + e
		}

		tz := take("timezone")

		if tz != "" && !strings.Contains(rng, tz) {
			rng += "  (" + tz + ")"
		}

		if strings.TrimSpace(strings.Trim(strings.TrimSpace(rng), ".()")) != "" && !strings.HasPrefix(strings.TrimSpace(rng), "(") {
			add("range", strings.TrimSpace(rng))
		}

		for _, k := range []string{"accountIds", "accounts", "accountsIncluded"} {
			if v, ok := p[k]; ok {
				used[k] = true

				switch t := v.(type) {
				case []any:
					add("accounts", fmt.Sprintf("%d included", len(t)))
				default:
					add("accounts", asStr(t))
				}
			}
		}

		if v, ok := p["includeTransfers"].(bool); ok {
			used["includeTransfers"] = true
			add("transfers", map[bool]string{true: "included", false: "excluded"}[v])
		}

		if v, ok := p["includeHiddenAccounts"].(bool); ok {
			used["includeHiddenAccounts"] = true
			add("hidden", map[bool]string{true: "hidden accounts included", false: "hidden accounts excluded"}[v])
		}

		if s := take("convertTo", "convert_to"); s != "" {
			add("converted", "to "+s)
		}

		if rates, ok := p["rates"].([]any); ok {
			used["rates"] = true

			for _, r := range rates {
				if rm, ok := r.(map[string]any); ok {
					desc := asFirstStr(rm, "from", "currency")

					if to := asFirstStr(rm, "to"); to != "" {
						desc += "→" + to
					}

					if rate := asFirstStr(rm, "rate"); rate != "" {
						desc += " " + rate
					}

					if base := asFirstStr(rm, "baseCurrency", "base"); base != "" {
						desc += " per " + base
					}

					src := strings.TrimSpace(asFirstStr(rm, "source") + " " + asFirstStr(rm, "provider"))

					if u := asFirstStr(rm, "updateTime", "updated", "updatedAt"); u != "" {
						src = strings.TrimSpace(src + ", " + u)
					}

					if src != "" {
						desc += "  (" + src + ")"
					}

					add("rate", desc)
				}
			}
		}

		add("rate basis", take("rateBasis"))
		add("note", take("note"))

		if un, ok := p["unconverted"].([]any); ok && len(un) > 0 {
			used["unconverted"] = true
			var curs []string

			for _, u := range un {
				if um, ok := u.(map[string]any); ok {
					curs = append(curs, asFirstStr(um, "currency"))
				} else {
					curs = append(curs, asStr(u))
				}
			}

			add("UNCONVERTED", strings.Join(curs, ", ")+" — no rate; returned per currency, not in the converted total")
		}

		if rc := take("rowCount", "rows", "transactionCount"); rc != "" {
			add("rows", rc)
		}

		if v, ok := p["hiddenAccountsExcluded"].([]any); ok {
			used["hiddenAccountsExcluded"] = true

			if len(v) > 0 {
				add("hidden", fmt.Sprintf("%d hidden accounts excluded (--include-hidden to count them)", len(v)))
			}
		}

		if f, ok := p["filters"].(map[string]any); ok {
			used["filters"] = true

			if s := asCompactFilters(f, verbose); s != "" {
				add("filters", s)
			}
		}

		if notes, ok := p["notes"].([]any); ok {
			used["notes"] = true

			for _, n := range notes {
				add("note", asStr(n))
			}
		}

		if !verbose {
			for _, k := range asProvenanceQuiet {
				used[k] = true
			}
		}

		keys := make([]string, 0, len(p))

		for k := range p {
			if !used[k] {
				keys = append(keys, k)
			}
		}

		sort.Strings(keys)

		for _, k := range keys {
			switch t := p[k].(type) {
			case []any:
				if len(t) == 0 && !verbose {
					continue
				}

				data, _ := json.Marshal(t)
				add(k, string(data))
			case map[string]any:
				if len(t) == 0 && !verbose {
					continue
				}

				data, _ := json.Marshal(t)
				add(k, string(data))
			default:
				add(k, asStr(t))
			}
		}
	}

	if meta != nil {
		if v, ok := meta["partial"].(bool); ok && v {
			add("PARTIAL", "some figures could not be converted (see unconverted in the answer)")
		}
	}

	if len(lines) == 0 {
		return nil
	}

	return append([]string{"provenance:"}, lines...)
}

// asProvenanceQuiet are provenance fields that restate the contract rather than this answer's
// scope; they print only under --verbose (they are always in --format json)
var asProvenanceQuiet = []string{"upstream", "amountUnit", "absentPeriodsAreOmitted", "signConvention", "convertedFiguresRounding", "accountsExplicit", "grouping"}

// asCompactFilters renders the filters that narrowed this answer (defaults omitted unless verbose)
func asCompactFilters(f map[string]any, verbose bool) string {
	keys := make([]string, 0, len(f))

	for k := range f {
		keys = append(keys, k)
	}

	sort.Strings(keys)

	var parts []string

	for _, k := range keys {
		v := f[k]

		if !verbose {
			switch t := v.(type) {
			case nil:
				continue
			case bool:
				if !t {
					continue
				}
			case string:
				if t == "" {
					continue
				}
			case []any:
				if len(t) == 0 {
					continue
				}
			case json.Number:
				if t.String() == "0" {
					continue
				}
			}
		}

		switch t := v.(type) {
		case []any, map[string]any:
			data, _ := json.Marshal(t)
			parts = append(parts, k+"="+string(data))
		default:
			parts = append(parts, k+"="+asStr(t))
		}
	}

	return strings.Join(parts, " ")
}

func asPrintProvenance(c *app.Ctx, env *client.Envelope) {
	if c.Quiet() || env == nil {
		return
	}

	data := env.DataMap()

	for _, l := range asProvenanceLines(asProvenance(data), env.Meta, c.Verbose()) {
		fmt.Fprintln(c.Err, l)
	}
}

// ---------------------------------------------------------------------------------------------
// The one emitter every analytics verb goes through
// ---------------------------------------------------------------------------------------------

// asEmit prints provenance on stderr, then the answer: JSON as received; CSV as the charting
// series when the answer has series, otherwise the verb's own view; table likewise.
func asEmit(c *app.Ctx, env *client.Envelope, view render.View) error {
	asPrintProvenance(c, env)

	if c.Format == render.FormatJSON {
		return c.Emit(env, render.View{})
	}

	var data any

	if err := env.Decode(&data); err != nil {
		return err
	}

	rows, hasSeries := asFlattenSeries(data)

	if hasSeries && (c.Format == render.FormatCSV || len(view.Columns) == 0) {
		if len(rows) == 0 {
			c.Info("no data in that range")
		}

		if c.Format == render.FormatCSV {
			return c.EmitCSV(asSeriesCSVHeader, asSeriesRecords(rows))
		}

		return c.Emit(asSyntheticEnvelope(env, map[string]any{"rows": asSeriesMaps(rows)}), render.View{Rows: "rows", Columns: asSeriesColumns})
	}

	return c.Emit(env, view)
}

// asSyntheticEnvelope wraps reshaped data (for the table renderer) keeping the original meta
func asSyntheticEnvelope(orig *client.Envelope, data any) *client.Envelope {
	raw, _ := json.Marshal(data)
	env := &client.Envelope{OK: true, Data: raw, Meta: map[string]any{}}

	if orig != nil {
		env.Meta = orig.Meta
	}

	full, _ := json.Marshal(map[string]any{"ok": true, "data": json.RawMessage(raw), "meta": env.Meta})
	env.Raw = full

	return env
}

// asRangeNotes prints which dates relative expressions resolved to
func asRangeNotes(c *app.Ctx, r *asDateRange) {
	if r == nil || len(r.Notes) == 0 {
		return
	}

	c.Info("dates (%s): %s", c.Location().String(), strings.Join(r.Notes, "; "))
}

// asSpec describes one one-route analytics verb
type asSpec struct {
	path          string
	rangeRequired bool
	noRange       bool
	intervals     []string
	extra         func(c *app.Ctx, q url.Values) error
	view          render.View
	pathArg       func(c *app.Ctx) (string, error)
	// after prints the answer's headline figures on stderr (the server's totals, verbatim)
	after func(c *app.Ctx, data map[string]any)
}

// asRun is the body shared by every one-route analytics verb
func asRun(s asSpec) func(c *app.Ctx) error {
	return func(c *app.Ctx) error {
		var r *asDateRange

		if !s.noRange {
			var err error

			r, err = asResolveRange(c, asNow(c.Location()), s.rangeRequired)

			if err != nil {
				return err
			}
		}

		q, err := asBuildQuery(c, r)

		if err != nil {
			return err
		}

		if len(s.intervals) > 0 {
			iv, err := asValidateInterval(c.String("interval"), s.intervals...)

			if err != nil {
				return err
			}

			if iv != "" {
				q.Set("interval", iv)
			}
		}

		if s.extra != nil {
			if err := s.extra(c, q); err != nil {
				return err
			}
		}

		path := s.path

		if s.pathArg != nil {
			if path, err = s.pathArg(c); err != nil {
				return err
			}
		}

		asRangeNotes(c, r)

		env, err := c.Call("GET", path, q, nil)

		if err != nil {
			return err
		}

		if s.after != nil && !c.Quiet() {
			s.after(c, env.DataMap())
		}

		return asEmit(c, env, s.view)
	}
}

// ---------------------------------------------------------------------------------------------
// Headline figures on stderr — the server's own totals, rendered, never recomputed
// ---------------------------------------------------------------------------------------------

// asMoneyList renders a [{currency, amount}] list as "$1.00 · €2.00"
func asMoneyList(v any) string {
	var parts []string

	for _, item := range asList(v) {
		m := asMap(item)

		if m == nil {
			continue
		}

		n, ok := render.ToInt64(m["amount"])

		if !ok {
			continue
		}

		parts = append(parts, render.Money(n, asFirstStr(m, "currency"))+" "+asFirstStr(m, "currency"))
	}

	return strings.Join(parts, " · ")
}

// asTotalsLines describes a totals block ({totals, totalsByCurrency, unconvertedTotals})
func asTotalsLines(label string, block map[string]any) []string {
	if block == nil {
		return nil
	}

	var lines []string

	if s := asMoneyList(block["totals"]); s != "" {
		lines = append(lines, fmt.Sprintf("%-10s %s", label, s))
	}

	if s := asMoneyList(block["totalsByCurrency"]); s != "" {
		lines = append(lines, fmt.Sprintf("%-10s   converted from %s", "", s))
	}

	if s := asMoneyList(block["unconvertedTotals"]); s != "" {
		lines = append(lines, fmt.Sprintf("%-10s   NOT converted (no rate): %s", "", s))
	}

	return lines
}

func asPrintLines(c *app.Ctx, lines []string) {
	for _, l := range lines {
		fmt.Fprintln(c.Err, "  "+l)
	}
}

// asAfterTotals prints data.totals (spending, income, payees, import-fallout)
func asAfterTotals(c *app.Ctx, data map[string]any) {
	asPrintLines(c, asTotalsLines("total", data))
}

// asAfterSummary prints income-vs-expense's per-key totals
func asAfterSummary(c *app.Ctx, data map[string]any) {
	s := asMap(data["summary"])

	for _, k := range []string{"income", "expense", "net"} {
		asPrintLines(c, asTotalsLines(k, asMap(s[k])))
	}
}

// asAfterTrend prints category-trend's mean and median per currency
func asAfterTrend(c *app.Ctx, data map[string]any) {
	if cat := asMap(data["category"]); cat != nil {
		c.Info("category %s (%s), sub-categories included", asFirstStr(cat, "name"), asFirstStr(cat, "id"))
	}

	for _, key := range []string{"stats", "unconvertedStats"} {
		for _, st := range asList(data[key]) {
			m := asMap(st)
			cur := asFirstStr(m, "currency")
			show := func(k string) string {
				if n, ok := render.ToInt64(m[k]); ok {
					return render.Money(n, cur)
				}

				return "—"
			}

			asPrintLines(c, []string{fmt.Sprintf("%s  total %s  mean %s  median %s  (over %s periods, %s with data)",
				cur, show("total"), show("mean"), show("median"), asStr(m["periods"]), asStr(m["periodsWithData"]))})
		}
	}
}

// asAfterRecurring prints the annualised totals
func asAfterRecurring(c *app.Ctx, data map[string]any) {
	if s := asMoneyList(data["annualizedTotals"]); s != "" {
		asPrintLines(c, []string{"annualised " + s})
	}

	if conv := asMap(data["annualizedConverted"]); conv != nil {
		if n, ok := render.ToInt64(conv["amount"]); ok {
			asPrintLines(c, []string{"annualised, converted " + render.Money(n, asFirstStr(conv, "currency")) + " " + asFirstStr(conv, "currency")})
		}
	}

	if n := len(asList(data["alreadyScheduled"])); n > 0 {
		c.Info("%d repeating charges are already scheduled (not listed; see alreadyScheduled in --format json)", n)
	}
}

// asAfterFallout prints the fallout's counts and totals
func asAfterFallout(c *app.Ctx, data map[string]any) {
	c.Info("%s rows still in a fallback category (of %s rows imported%s)", asStr(data["count"]), asStr(data["importedRows"]),
		map[bool]string{true: " by run " + asStr(data["runId"]), false: ""}[asStr(data["runId"]) != ""])
	asAfterTotals(c, data)
	c.Info("re-categorise with: ezbk transactions set-category <id>… --category <name>")
}

// ---------------------------------------------------------------------------------------------
// The verbs
// ---------------------------------------------------------------------------------------------

// asPeriodNames are the server's named periods for /analytics/period-summary
var asPeriodNames = map[string]string{
	"today": "today", "yesterday": "yesterday", "this-week": "this_week", "last-week": "last_week",
	"this-month": "this_month", "last-month": "last_month", "this-year": "this_year", "last-year": "last_year",
}

func asSummaryExtra(c *app.Ctx, q url.Values) error {
	var periods []string

	for _, p := range c.Strings("period") {
		name, ok := asPeriodNames[asNormWord(p)]

		if !ok {
			return app.Usage(fmt.Sprintf("--period %q is not a named period", p), "use today, yesterday, this-week, last-week, this-month, last-month, this-year, last-year (repeatable), or --start/--end for a custom range")
		}

		periods = append(periods, name)
	}

	if q.Get("start") != "" || q.Get("end") != "" {
		periods = append(periods, "custom")
	}

	app.AddList(q, "periods", periods)

	return nil
}

// asRunSummary runs `analytics summary`: --period names the server's periods (repeatable) and
// --start/--end, resolved locally, add a custom one
func asRunSummary(c *app.Ctx) error {
	var r *asDateRange

	if c.String("start") != "" || c.String("end") != "" {
		var err error

		if r, err = asResolveRangeWith(c, asNow(c.Location()), true, false); err != nil {
			return err
		}
	}

	q, err := asBuildQuery(c, r)

	if err != nil {
		return err
	}

	if err := asSummaryExtra(c, q); err != nil {
		return err
	}

	asRangeNotes(c, r)

	env, err := c.Call("GET", "/analytics/period-summary", q, nil)

	if err != nil {
		return err
	}

	if !c.Quiet() {
		for _, p := range asList(env.DataMap()["periods"]) {
			pm := asMap(p)
			c.Info("  %-11s %s .. %s", asFirstStr(pm, "period"), asFirstStr(pm, "start"), asFirstStr(pm, "end"))
		}
	}

	return asEmit(c, env, render.View{})
}

func asTopExtra(c *app.Ctx, q url.Values) error {
	top, err := asPositiveInt(c, "top")

	if err != nil {
		return err
	}

	if top != "" {
		q.Set("top_n", top)
	}

	return nil
}

// asEnumFlag copies an enumerated flag into the query after checking it locally
func asEnumFlag(c *app.Ctx, q url.Values, flag, arg string, allowed ...string) error {
	v := strings.ToLower(strings.TrimSpace(c.String(flag)))

	if v == "" {
		return nil
	}

	for _, a := range allowed {
		if v == a {
			q.Set(arg, v)
			return nil
		}
	}

	return app.Usage(fmt.Sprintf("--%s %q is not one of %s", flag, v, strings.Join(allowed, ", ")), "")
}

// asIntFlag copies a positive integer flag into the query
func asIntFlag(c *app.Ctx, q url.Values, flag, arg string) error {
	v, err := asPositiveInt(c, flag)

	if err != nil {
		return err
	}

	if v != "" {
		q.Set(arg, v)
	}

	return nil
}

func asCategoryExtra(c *app.Ctx, q url.Values) error {
	if err := asTopExtra(c, q); err != nil {
		return err
	}

	if err := asEnumFlag(c, q, "group-by", "group_by", "primary", "secondary", "account"); err != nil {
		return err
	}

	if c.Bool("include-zero") {
		q.Set("include_zero", "true")
	}

	return nil
}

var asCategoryView = render.View{Rows: "series", Empty: "nothing in that range", Columns: []render.Column{
	{Header: "Category", Key: "label"},
	{Header: "Currency", Key: "currency"},
	{Header: "Total", Key: "total", Kind: render.KMoney, CurrencyKey: "currency"},
	{Header: "Count", Key: "count", Kind: render.KInt},
}}

// asFlagsWithout drops flags a route refuses (so the parser refuses them first, with a better message)
func asFlagsWithout(flags []app.Flag, drop ...string) []app.Flag {
	skip := map[string]bool{}

	for _, d := range drop {
		skip[d] = true
	}

	out := make([]app.Flag, 0, len(flags))

	for _, f := range flags {
		if !skip[f.Name] {
			out = append(out, f)
		}
	}

	return out
}

// asAccountPathArg resolves the positional account (id or name) into /accounts/:id/<suffix>
func asAccountPathArg(suffix string) func(c *app.Ctx) (string, error) {
	return func(c *app.Ctx) (string, error) {
		ids, err := asResolveIds(c, "account", "account", "/accounts", []string{c.Args[0]})

		if err != nil {
			return "", err
		}

		return "/accounts/" + url.PathEscape(ids[0]) + suffix, nil
	}
}

var asGroupByFlag = app.Flag{Name: "group-by", Value: "primary|secondary|account", Help: "roll secondary categories up to their primary (default), keep them, or group by account"}

func init() {
	balanceFlags := asFlagsWithout(asRangeFlags(asFlagInterval), "category", "exclude-category", "tag-filter")

	app.Register(
		app.Verb{
			Name: "analytics summary", Group: asGroup, MaxArgs: 0,
			Summary: "income, expense and net per named period, per currency — the Overview page",
			Flags: []app.Flag{
				{Name: "period", Value: "NAME", Repeat: true, Help: "today, yesterday, this-week, last-week, this-month, last-month, this-year, last-year (repeatable; default today, this-week, this-month, this-year)"},
				{Name: "start", Value: "YYYY-MM-DD", Help: "a custom period's first day (with --end; relative spellings accepted)"},
				{Name: "end", Value: "YYYY-MM-DD", Help: "a custom period's last day"},
				asFlagAccount, asFlagCategory, asFlagExclude, asFlagTagFilter, asFlagHidden, asFlagConvert, asFlagTxnTZ,
			},
			Run: asRunSummary,
		},
		app.Verb{
			Name: "analytics spending", Group: asGroup,
			Summary: "spending by category (the pie / bar chart), per currency unless --convert-to",
			Flags: asRangeFlags(asFlagInterval, asFlagTop, asGroupByFlag,
				app.Flag{Name: "include-zero", Help: "list categories with no spending too"}),
			Run: asRun(asSpec{path: "/analytics/spending-by-category", rangeRequired: true, intervals: []string{"none", "month", "quarter", "year"}, extra: asCategoryExtra, view: asCategoryView, after: asAfterTotals}),
		},
		app.Verb{
			Name: "analytics income", Group: asGroup,
			Summary: "income by category — the income side of the same chart",
			Flags: asRangeFlags(asFlagInterval, asFlagTop, asGroupByFlag,
				app.Flag{Name: "include-zero", Help: "list categories with no income too"}),
			Run: asRun(asSpec{path: "/analytics/income-by-category", rangeRequired: true, intervals: []string{"none", "month", "quarter", "year"}, extra: asCategoryExtra, view: asCategoryView, after: asAfterTotals}),
		},
		app.Verb{
			Name: "analytics income-vs-expense", Group: asGroup,
			Summary: "income, expense and net per interval",
			Flags:   asFlagsWithout(asRangeFlags(asFlagInterval), "include-transfers"),
			Run:     asRun(asSpec{path: "/analytics/income-vs-expense", rangeRequired: true, intervals: []string{"none", "day", "week", "month", "quarter", "year"}, after: asAfterSummary}),
		},
		app.Verb{
			Name: "analytics cash-flow", Group: asGroup,
			Summary: "opening balance, money in, money out and closing balance per interval",
			Flags:   asFlagsWithout(balanceFlags, "use-transaction-timezone"),
			Run:     asRun(asSpec{path: "/analytics/cash-flow", rangeRequired: true, intervals: []string{"none", "day", "week", "month", "quarter", "year"}}),
		},
		app.Verb{
			Name: "analytics net-worth", Group: asGroup,
			Summary: "assets, liabilities and net worth per interval (and each account's closing balance in --format json)",
			Flags:   balanceFlags,
			Run:     asRun(asSpec{path: "/analytics/net-worth", rangeRequired: true, intervals: []string{"none", "day", "week", "month", "quarter", "year"}}),
		},
		app.Verb{
			Name: "analytics trend", Group: asGroup,
			Summary: "one category (with its sub-categories) over time, with its mean and median",
			Flags:   asRangeFlags(app.Flag{Name: "interval", Value: "month|quarter|year", Help: "one point per month (default), quarter or year"}),
			Run: asRun(asSpec{path: "/analytics/category-trend", rangeRequired: true, intervals: []string{"month", "quarter", "year"}, after: asAfterTrend, extra: func(c *app.Ctx, q url.Values) error {
				if len(c.Strings("category")) != 1 {
					return app.Usage("`ezbk analytics trend` needs exactly one --category", "ezbk categories list")
				}

				// asBuildQuery has already resolved the name to an id
				id := q.Get("category_ids")
				q.Del("category_ids")
				q.Set("category_id", id)

				return nil
			}}),
		},
		app.Verb{
			Name: "analytics tags", Group: asGroup,
			Summary: "spending and income per tag — what the trip cost",
			Flags: asRangeFlags(
				app.Flag{Name: "tag", Value: "id|name", Repeat: true, Help: "the tags to break down (repeatable; required; up to 50)"},
				app.Flag{Name: "group-by", Value: "kind|primary|secondary|account", Help: "split each tag by kind (default), category or account"}),
			Run: asRun(asSpec{path: "/analytics/tag-breakdown", rangeRequired: true, after: asAfterTags, extra: func(c *app.Ctx, q url.Values) error {
				tags := c.Strings("tag")

				if len(tags) == 0 {
					return app.Usage("`ezbk analytics tags` needs at least one --tag", "ezbk tags list")
				}

				ids, err := asResolveIds(c, "tag", "tag", "/tags", tags)

				if err != nil {
					return err
				}

				app.AddList(q, "tag_ids", ids)

				return asEnumFlag(c, q, "group-by", "group_by", "kind", "primary", "secondary", "account")
			}}),
		},
		app.Verb{
			Name: "analytics payees", Group: asGroup,
			Summary: "who got the money — grouped by normalised transaction comment (ezBookkeeping has no payee entity)",
			Flags: asRangeFlags(asFlagTop,
				app.Flag{Name: "direction", Value: "out|in", Help: "money out (default) or money in"}),
			Run: asRun(asSpec{path: "/analytics/payee-leaderboard", rangeRequired: true, after: asAfterTotals, extra: func(c *app.Ctx, q url.Values) error {
				if err := asTopExtra(c, q); err != nil {
					return err
				}

				return asEnumFlag(c, q, "direction", "direction", "out", "in")
			}, view: render.View{Rows: "payees", Empty: "no transactions in that range", Columns: []render.Column{
				{Header: "Payee (comment)", Key: "label"},
				{Header: "Currency", Key: "currency"},
				{Header: "Total", Key: "total", Kind: render.KMoney, CurrencyKey: "currency"},
				{Header: "Count", Key: "count", Kind: render.KInt},
				{Header: "First", Key: "firstDate"},
				{Header: "Last", Key: "lastDate"},
			}}}),
		},
		app.Verb{
			Name: "analytics recurring", Group: asGroup,
			Summary: "repeating charges not yet scheduled: cadence, evidence and annualised cost",
			Flags: asRangeFlags(
				app.Flag{Name: "min-occurrences", Value: "N", Help: "how many repeats make a pattern (default 3)"},
				app.Flag{Name: "tolerance-days", Value: "N", Help: "how far a repeat may drift (default 3)"},
				app.Flag{Name: "direction", Value: "out|in|both", Help: "charges (default), income, or both"},
				app.Flag{Name: "include-scheduled", Help: "also list patterns a scheduled template already covers"},
				app.Flag{Name: "include-inactive", Help: "also list patterns that have stopped"}),
			Run: asRun(asSpec{path: "/analytics/recurring", after: asAfterRecurring, extra: func(c *app.Ctx, q url.Values) error {
				if err := asIntFlag(c, q, "min-occurrences", "min_occurrences"); err != nil {
					return err
				}

				if c.Has("tolerance-days") {
					n, err := c.Int("tolerance-days", 0)

					if err != nil {
						return err
					}

					if n < 0 {
						return app.Usage("--tolerance-days cannot be negative", "")
					}

					q.Set("tolerance_days", strconv.Itoa(n))
				}

				if c.Bool("include-scheduled") {
					q.Set("include_scheduled", "true")
				}

				if c.Bool("include-inactive") {
					q.Set("include_inactive", "true")
				}

				return asEnumFlag(c, q, "direction", "direction", "out", "in", "both")
			}, view: render.View{Rows: "recurring", Empty: "no repeating transactions found", Columns: []render.Column{
				{Header: "Comment", Key: "label"},
				{Header: "Kind", Key: "kind"},
				{Header: "Every", Key: "cadence"},
				{Header: "Seen", Key: "occurrences", Kind: render.KInt},
				{Header: "Typical", Key: "typicalAmount", Kind: render.KMoney, CurrencyKey: "currency"},
				{Header: "Annualised", Key: "annualizedCost", Kind: render.KMoney, CurrencyKey: "currency"},
				{Header: "Currency", Key: "currency"},
				{Header: "Last", Key: "lastDate"},
				{Header: "Next", Key: "nextExpected"},
				{Header: "Active", Key: "active", Kind: render.KBool},
			}}}),
		},
		app.Verb{
			Name: "analytics anomalies", Group: asGroup,
			Summary: "categories unusually far from their own trailing monthly norm, with the evidence",
			Flags: asRangeFlags(
				app.Flag{Name: "z", Value: "N", Help: "how many standard deviations counts as unusual (default 2.0)"},
				app.Flag{Name: "min-amount", Value: "N", Help: "ignore deviations smaller than this many hundredths"},
				app.Flag{Name: "lookback-months", Value: "N", Help: "months of history each norm uses (default 12)"},
				app.Flag{Name: "min-history", Value: "N", Help: "months of history a category needs before it is judged (default 3)"},
				app.Flag{Name: "group-by", Value: "primary|secondary", Help: "judge secondary categories (default) or their primaries"}),
			Run: asRun(asSpec{path: "/analytics/anomalies", extra: func(c *app.Ctx, q url.Values) error {
				if z := c.String("z"); z != "" {
					if f, err := strconv.ParseFloat(z, 64); err != nil || f <= 0 || f > 100 {
						return app.Usage("--z must be a positive number up to 100, e.g. 2.5", "")
					}

					q.Set("z", z)
				}

				if c.Has("min-amount") {
					n, _, err := c.Amount("min-amount")

					if err != nil {
						return err
					}

					q.Set("min_amount", strconv.FormatInt(n, 10))
				}

				if err := asIntFlag(c, q, "lookback-months", "lookback_months"); err != nil {
					return err
				}

				if err := asIntFlag(c, q, "min-history", "min_history"); err != nil {
					return err
				}

				return asEnumFlag(c, q, "group-by", "group_by", "primary", "secondary")
			}, view: render.View{Rows: "anomalies", Empty: "nothing unusual", Columns: []render.Column{
				{Header: "Category", Key: "label"},
				{Header: "Period", Key: "period"},
				{Header: "Amount", Key: "amount", Kind: render.KMoney, CurrencyKey: "currency"},
				{Header: "Mean", Key: "mean", Kind: render.KMoney, CurrencyKey: "currency"},
				{Header: "z", Key: "zScore"},
				{Header: "Direction", Key: "direction"},
				{Header: "Currency", Key: "currency"},
			}}}),
		},
		app.Verb{
			Name: "analytics runway", Group: asGroup, MaxArgs: 0,
			Summary: "liquid assets ÷ trailing average monthly net outflow, in months",
			Flags: []app.Flag{
				{Name: "basis", Value: "N", Help: "months of history the average uses (default 6; usually 3, 6 or 12)"},
				{Name: "as-of", Value: "YYYY-MM-DD", Help: "measure as of this day (default today; relative spellings accepted)"},
				{Name: "liquid-category", Value: "CATEGORY", Repeat: true, Help: "account categories counted as liquid (default cash, checking, savings; repeatable)"},
				asFlagAccount, asFlagHidden, asFlagConvert,
			},
			Run: asRun(asSpec{path: "/analytics/runway", noRange: true, extra: func(c *app.Ctx, q url.Values) error {
				if c.Has("basis") {
					n, err := c.Int("basis", 0)

					if err != nil {
						return err
					}

					if n < 1 || n > 24 {
						return app.Usage("--basis must be between 1 and 24 months", "3, 6 and 12 are the usual choices")
					}

					q.Set("basis", strconv.Itoa(n))
				}

				if v := c.String("as-of"); v != "" {
					d, resolved, err := asResolveDay(v, true, asNow(c.Location()))

					if err != nil {
						return app.Usage("--as-of: "+err.Error(), "")
					}

					if resolved {
						c.Info("dates (%s): --as-of %s → %s", c.Location().String(), v, d)
					}

					q.Set("as_of", d)
				}

				app.AddList(q, "liquid_categories", c.Strings("liquid-category"))

				return nil
			}, view: render.View{Rows: "byCurrency", Empty: "no liquid accounts", Columns: []render.Column{
				{Header: "Currency", Key: "currency"},
				{Header: "Liquid", Key: "liquidAssets", Kind: render.KMoney, CurrencyKey: "currency"},
				{Header: "Income (basis)", Key: "trailingIncome", Kind: render.KMoney, CurrencyKey: "currency"},
				{Header: "Expense (basis)", Key: "trailingExpense", Kind: render.KMoney, CurrencyKey: "currency"},
				{Header: "Avg monthly outflow", Key: "runway.averageMonthlyNetOutflow", Kind: render.KMoney, CurrencyKey: "currency"},
				{Header: "Months", Key: "runway.months"},
				{Header: "Status", Key: "runway.status"},
			}}, after: func(c *app.Ctx, data map[string]any) {
				if b := asMap(data["basis"]); b != nil {
					c.Info("as of %s, basis %s months (%s .. %s)", asStr(data["asOf"]), asStr(data["basisMonths"]), asFirstStr(b, "start"), asFirstStr(b, "end"))
				}
			}}),
		},
		app.Verb{
			Name: "analytics import-fallout", Group: asGroup, MaxArgs: 0,
			Summary: "rows an import placed in its fallback category, by account and month",
			Flags: []app.Flag{
				{Name: "run", Value: "RUN_ID", Help: "one ingest run (its fallback categories are remembered)"},
				{Name: "fallback-category", Value: "CATEGORY", Repeat: true, Help: "the fallback categories to report (id or name; needed without --run)"},
				{Name: "limit", Value: "N", Help: "how many rows to list (the counts are always complete)"},
				asFlagStart, asFlagEnd, asFlagPeriod, asFlagAccount, asFlagConvert,
			},
			Run: asRun(asSpec{path: "/analytics/import-fallout", after: asAfterFallout, extra: func(c *app.Ctx, q url.Values) error {
				if r := c.String("run"); r != "" {
					q.Set("run_id", r)
				}

				fb := c.Strings("fallback-category")

				if len(fb) == 0 && c.String("run") == "" {
					return app.Usage("`ezbk analytics import-fallout` needs --run or --fallback-category", "the run id is printed by `ezbk statements apply`")
				}

				ids, err := asResolveIds(c, "category", "fallback-category", "/categories", fb)

				if err != nil {
					return err
				}

				app.AddList(q, "fallback_category_ids", ids)

				return asIntFlag(c, q, "limit", "limit")
			}, view: render.View{Rows: "rows", Empty: "nothing left in a fallback category", Columns: []render.Column{
				{Header: "Date", Key: "date"},
				{Header: "Account key", Key: "accountKey"},
				{Header: "Comment", Key: "comment"},
				{Header: "Amount", Key: "amount", Kind: render.KMoney, CurrencyKey: "currency"},
				{Header: "Currency", Key: "currency"},
				{Header: "Transaction", Key: "transactionId"},
				{Header: "Run", Key: "runId"},
			}}}),
		},
		app.Verb{
			Name: "analytics balance-history", Group: asGroup, Args: "<account id|name>", MinArgs: 1, MaxArgs: 1,
			Summary: "one account's closing balance over time",
			Flags: []app.Flag{asFlagStart, asFlagEnd, asFlagPeriod,
				{Name: "interval", Value: "day|week|month", Help: "one point per day, week or month (default month)"}},
			Run: asRun(asSpec{path: "", rangeRequired: false, intervals: []string{"day", "week", "month"}, pathArg: asAccountPathArg("/balance-history"), after: func(c *app.Ctx, data map[string]any) {
				if a := asMap(data["account"]); a != nil {
					c.Info("account %s (%s, %s)", asFirstStr(a, "name"), asFirstStr(a, "accountId", "id"), asFirstStr(a, "currency"))
				}
			}}),
		},
	)
}

// asAfterTags prints each tag's net per currency
func asAfterTags(c *app.Ctx, data map[string]any) {
	for _, t := range asList(data["tags"]) {
		tm := asMap(t)
		tag := asMap(tm["tag"])
		asPrintLines(c, asTotalsLines(asFirstStr(tag, "name"), asMap(tm["net"])))
	}

	c.Info("a transaction with several of these tags counts under each: tag figures are not additive")
}

// ---------------------------------------------------------------------------------------------
// Names where ids are accepted (cli.mdx §7.5). The analytics routes take ids only; a name typed
// on the command line is looked up in the plane's own list route — exact match first, then
// case-insensitive — and an ambiguous name is exit 2 listing the candidates with their ids.
// ---------------------------------------------------------------------------------------------

// asNamed is one id/name pair found in a list answer
type asNamed struct {
	Id   string
	Name string
	Path string // the parent's name, for a sub-account or secondary category
}

// asIsId reports whether v is an ezBookkeeping id (a positive decimal)
func asIsId(v string) bool {
	if v == "" || len(v) > 19 {
		return false
	}

	for _, r := range v {
		if r < '0' || r > '9' {
			return false
		}
	}

	return strings.TrimLeft(v, "0") != ""
}

// asCollectNamed walks a list answer and returns every object carrying a string id and a name
func asCollectNamed(data any) []asNamed {
	var out []asNamed

	var walk func(v any, parent string)

	walk = func(v any, parent string) {
		switch t := v.(type) {
		case map[string]any:
			id, name := asStr(t["id"]), asStr(t["name"])
			next := parent

			if id != "" && name != "" && asIsId(id) {
				out = append(out, asNamed{Id: id, Name: name, Path: parent})
				next = name
			}

			keys := make([]string, 0, len(t))

			for k := range t {
				keys = append(keys, k)
			}

			sort.Strings(keys)

			for _, k := range keys {
				switch t[k].(type) {
				case map[string]any, []any:
					walk(t[k], next)
				}
			}
		case []any:
			for _, item := range t {
				walk(item, parent)
			}
		}
	}

	walk(data, "")

	return out
}

// asMatchName finds the candidates a typed name means: exact matches, else case-insensitive ones
func asMatchName(all []asNamed, v string) []asNamed {
	var exact, fold []asNamed
	seen := map[string]bool{}

	for _, n := range all {
		if seen[n.Id] {
			continue
		}

		switch {
		case n.Name == v:
			exact = append(exact, n)
			seen[n.Id] = true
		case strings.EqualFold(n.Name, v):
			fold = append(fold, n)
		}
	}

	if len(exact) > 0 {
		return exact
	}

	var out []asNamed

	for _, n := range fold {
		if !seen[n.Id] {
			seen[n.Id] = true
			out = append(out, n)
		}
	}

	return out
}

// asResolveIds maps each value to an id; listPath is the plane's list route for that kind
func asResolveIds(c *app.Ctx, kind, flag, listPath string, values []string) ([]string, error) {
	return asResolveIdsQ(c, kind, flag, listPath, url.Values{"include_hidden": {"true"}}, values)
}

// asResolveIdsQ is asResolveIds with the list route's query (e.g. a category type)
func asResolveIdsQ(c *app.Ctx, kind, flag, listPath string, listQuery url.Values, values []string) ([]string, error) {
	var out []string
	var all []asNamed
	loaded := false

	for _, v := range values {
		if asIsId(v) {
			out = append(out, v)
			continue
		}

		if !loaded {
			env, err := c.Call("GET", listPath, listQuery, nil)

			if err != nil {
				return nil, err
			}

			var data any

			if err := env.Decode(&data); err != nil {
				return nil, err
			}

			all = asCollectNamed(data)
			loaded = true
		}

		matches := asMatchName(all, v)

		switch len(matches) {
		case 0:
			words := strings.Fields(kind)
			list := words[len(words)-1]

			return nil, &app.ExitError{Code: 3, Msg: fmt.Sprintf("no %s named %q (--%s)", kind, v, flag), Hint: fmt.Sprintf("ezbk %s list shows the names and ids; an exact name or the id works", map[string]string{"account": "accounts", "category": "categories", "tag": "tags"}[list])}
		case 1:
			out = append(out, matches[0].Id)

			if c.Verbose() {
				c.Info("--%s %q → %s", flag, v, matches[0].Id)
			}
		default:
			var cands []string

			for _, m := range matches {
				label := m.Name

				if m.Path != "" {
					label = m.Path + " / " + m.Name
				}

				cands = append(cands, fmt.Sprintf("%s (%s)", m.Id, label))
			}

			return nil, app.Usage(fmt.Sprintf("--%s %q is ambiguous: %d %ss have that name", flag, v, len(matches), kind), "pass the id instead: "+strings.Join(cands, ", "))
		}
	}

	return out, nil
}

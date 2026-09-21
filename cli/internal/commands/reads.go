package commands

// reads.go — READING THE BOOKS (cli.mdx §6, §9): accounts list|show|balance|reconciliation,
// transactions list|show|count|export, categories list, tags list, tag-groups list, templates list,
// schedules upcoming, rates list|convert, insights list, data stats|export.
//
// Thin by construction: parse, call one route, render. The CLI never re-filters, re-sums, converts
// or rounds; a total it prints is a total the server returned. The only work done here is
// presentation — flattening a nested list for a table, naming an enum, putting a transfer's two
// sides on one row, and turning hundredths into a decimal string through render.Money.
//
// Names are accepted where ids are (`--account "Northbank Checking"`): a value that is not a
// decimal id is looked up in the matching list route (exact match first, then case-insensitive);
// an ambiguous name is exit 2 listing the candidates with their ids, an unknown one exit 3.

import (
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/BryanStarbuck/ezbookkeeping/cli/internal/app"
	"github.com/BryanStarbuck/ezbookkeeping/cli/internal/client"
	"github.com/BryanStarbuck/ezbookkeeping/cli/internal/errfile"
	"github.com/BryanStarbuck/ezbookkeeping/cli/internal/exitcode"
	"github.com/BryanStarbuck/ezbookkeeping/cli/internal/render"
)

const orGroupRead = "READING THE BOOKS"

// The filter flags `transactions list|count|export` share — the same language the bulk write verbs
// take, so "show me these" and "change these" cannot select different sets (apis.mdx §10.3)
var orTxnFilterFlags = []app.Flag{
	{Name: "account", Value: "id|name", Repeat: true, Help: "only these accounts (repeatable)"},
	{Name: "start", Value: "YYYY-MM-DD", Help: "first day, inclusive (also YYYY-MM, today, yesterday, this-month, last-month, this-year, last-year)"},
	{Name: "end", Value: "YYYY-MM-DD", Help: "last day, inclusive (same spellings; a month means its last day)"},
	{Name: "type", Value: "income|expense|transfer|balance_modification", Help: "only this transaction type"},
	{Name: "category", Value: "id|name", Repeat: true, Help: "only these secondary categories (repeatable)"},
	{Name: "tag", Value: "id|name", Repeat: true, Help: "only transactions carrying any of these tags (repeatable)"},
	{Name: "tag-filter", Value: "EXPR", Help: "upstream's tag_filter expression verbatim, e.g. 1:<id>,<id> (has all)"},
	{Name: "untagged", Help: "only transactions with no tag"},
	{Name: "keyword", Value: "TEXT", Help: "comment contains TEXT"},
	{Name: "min", Value: "hundredths", Help: "smallest amount, integer hundredths (50000 = 500.00); needs --currency across mixed currencies"},
	{Name: "max", Value: "hundredths", Help: "largest amount, integer hundredths"},
	{Name: "currency", Value: "CUR", Help: "only transactions in this currency"},
	{Name: "with-pictures", Help: "only transactions that have pictures"},
}

func init() {
	app.Register(
		// accounts
		app.Verb{
			Name: "accounts list", Group: orGroupRead, MaxArgs: 0,
			Summary: "id, name, category, currency, balance, hidden — sub-accounts indented under their parent",
			Flags: []app.Flag{
				{Name: "include-hidden", Help: "include hidden accounts"},
				{Name: "currency", Value: "CUR", Help: "only accounts in this currency"},
				{Name: "category", Value: "cash|checking|savings|credit_card|virtual|debt|receivables|investment|certificate_of_deposit", Help: "only this account category"},
			},
			Run: orRunAccountsList,
		},
		app.Verb{
			Name: "accounts show", Group: orGroupRead, Args: "<id|name>", MinArgs: 1, MaxArgs: 1,
			Summary: "one account, with its sub-accounts",
			Run:     orRunAccountsShow,
		},
		app.Verb{
			Name: "accounts balance", Group: orGroupRead, Args: "<id|name>", MinArgs: 1, MaxArgs: 1,
			Summary: "one integer and its currency — a historical balance is asked of the server, never summed here",
			Flags: []app.Flag{
				{Name: "as-of", Value: "YYYY-MM-DD", Help: "the balance at the end of this day (default today; also yesterday, last-month, …)"},
			},
			Run: orRunAccountsBalance,
		},
		app.Verb{
			Name: "accounts reconciliation", Group: orGroupRead, Args: "<id|name>", MinArgs: 1, MaxArgs: 1,
			Summary: "the app's reconciliation statement: opening, inflows, outflows, closing, per-row running balance",
			Flags: []app.Flag{
				{Name: "start", Value: "YYYY-MM-DD", Help: "first day, inclusive (required; relative words accepted)"},
				{Name: "end", Value: "YYYY-MM-DD", Help: "last day, inclusive (required unless --start names a month or year)"},
			},
			Run: orRunAccountsReconciliation,
		},

		// transactions
		app.Verb{
			Name: "transactions list", Group: orGroupRead, MaxArgs: 0,
			Summary: "rows paged by the server's cursor (--limit up to 5000; nextCursor on stderr); a transfer is ONE row with both sides",
			Flags: append(append([]app.Flag{}, orTxnFilterFlags...),
				app.Flag{Name: "limit", Value: "N", Help: "rows to return (default 200, capped at 5000 by the server)"},
				app.Flag{Name: "cursor", Value: "X", Help: "continue from a previous page's nextCursor"},
				app.Flag{Name: "order", Value: "time|-time", Help: "ordering (default newest first)"},
			),
			Run: orRunTransactionsList,
		},
		app.Verb{
			Name: "transactions show", Group: orGroupRead, Args: "<id>", MinArgs: 1, MaxArgs: 1,
			Summary: "one transaction, with its tags and the transfer's other side",
			Run:     orRunTransactionsShow,
		},
		app.Verb{
			Name: "transactions count", Group: orGroupRead, MaxArgs: 0,
			Summary: "one integer — the server's count over the same filters (a `list | wc -l` counts one page)",
			Flags:   append([]app.Flag{}, orTxnFilterFlags...),
			Run:     orRunTransactionsCount,
		},
		app.Verb{
			Name: "transactions export", Group: orGroupRead, MaxArgs: 0,
			Summary: "upstream's ezBookkeeping-format CSV (or --tsv) of the filtered transactions, streamed to stdout",
			Flags: append(append([]app.Flag{}, orTxnFilterFlags...),
				app.Flag{Name: "tsv", Help: "tab-separated instead of CSV"},
			),
			Run: orRunTransactionsExport,
		},

		// reference lists
		app.Verb{
			Name: "categories list", Group: orGroupRead, MaxArgs: 0,
			Summary: "two levels: primary categories with their secondaries indented",
			Flags: []app.Flag{
				{Name: "type", Value: "income|expense|transfer", Help: "only this category type"},
				{Name: "include-hidden", Help: "include hidden categories"},
				{Name: "parent", Value: "id|name", Help: "only the secondaries under this primary"},
			},
			Run: orRunCategoriesList,
		},
		app.Verb{
			Name: "categories tree", Group: orGroupRead, MaxArgs: 0,
			Summary: "the whole tree as one YAML document: every group with its sub-categories, by major category",
			Flags: []app.Flag{
				{Name: "type", Value: "income|expense|transfer", Help: "only groups of this major category"},
				{Name: "include-hidden", Help: "include hidden groups and sub-categories"},
			},
			Run: orRunCategoriesTree,
		},
		app.Verb{
			Name: "tags list", Group: orGroupRead, MaxArgs: 0,
			Summary: "tags, with their group",
			Flags: []app.Flag{
				{Name: "include-hidden", Help: "include hidden tags"},
				{Name: "group", Value: "id|name", Help: "only tags in this tag group"},
			},
			Run: orRunTagsList,
		},
		app.Verb{
			Name: "tag-groups list", Group: orGroupRead, MaxArgs: 0,
			Summary: "tag groups",
			Run:     orRunTagGroupsList,
		},
		app.Verb{
			Name: "templates list", Group: orGroupRead, MaxArgs: 0,
			Summary: "transaction templates and scheduled transactions (--scheduled / --normal for one kind)",
			Flags: []app.Flag{
				{Name: "scheduled", Help: "only scheduled transactions"},
				{Name: "normal", Help: "only normal (non-scheduled) templates"},
				{Name: "include-hidden", Help: "include hidden (for a schedule: paused) templates"},
			},
			Run: orRunTemplatesList,
		},
		app.Verb{
			Name: "schedules upcoming", Group: orGroupRead, MaxArgs: 0,
			Summary: "what the scheduled transactions will create in the next N days (nothing is fired)",
			Flags: []app.Flag{
				{Name: "days", Value: "N", Help: "look this many days ahead (default 30)"},
				{Name: "from", Value: "YYYY-MM-DD", Help: "start the window on this day instead of today (relative words accepted)"},
				{Name: "template", Value: "id|name", Repeat: true, Help: "only these scheduled templates (repeatable)"},
				{Name: "limit", Value: "N", Help: "at most N occurrences (default 200, cap 5000)"},
			},
			Run: orRunSchedulesUpcoming,
		},

		// currency
		app.Verb{
			Name: "rates list", Group: orGroupRead, MaxArgs: 0,
			Summary: "exchange rates, each with its source (provider or custom) and update time",
			Flags: []app.Flag{
				{Name: "currency", Value: "CUR", Repeat: true, Help: "only these currencies (repeatable)"},
			},
			Run: orRunRatesList,
		},
		app.Verb{
			Name: "rates convert", Group: orGroupRead, Args: "<hundredths> <FROM> <TO>", MinArgs: 3, MaxArgs: 3,
			Summary: "the server's conversion (50000 = 500.00); the rate and its date on stderr — the CLI never multiplies",
			Run:     orRunRatesConvert,
		},

		// insights & data
		app.Verb{
			Name: "insights list", Group: orGroupRead, MaxArgs: 0,
			Summary: "saved Insights Explorer definitions (definitions, not numbers — the numbers are `ezbk analytics`)",
			Flags: []app.Flag{
				{Name: "include-hidden", Help: "include hidden explorers"},
			},
			Run: orRunInsightsList,
		},
		app.Verb{
			Name: "data stats", Group: orGroupRead, MaxArgs: 0,
			Summary: "counts of accounts, transactions, categories, tags, templates, pictures",
			Run:     orRunDataStats,
		},
		app.Verb{
			Name: "data export", Group: orGroupRead, MaxArgs: 0,
			Summary: "every transaction in upstream's ezBookkeeping CSV (or --tsv), streamed to stdout",
			Flags: []app.Flag{
				{Name: "tsv", Help: "tab-separated instead of CSV"},
				{Name: "start", Value: "YYYY-MM-DD", Help: "only transactions on or after this day (relative words accepted)"},
				{Name: "end", Value: "YYYY-MM-DD", Help: "only transactions on or before this day"},
			},
			Run: orRunDataExport,
		},
	)
}

// ---------------------------------------------------------------------------------------------
// Shared helpers
// ---------------------------------------------------------------------------------------------

var orDigitsRe = regexp.MustCompile(`^[0-9]+$`)

// orIsID reports whether v is an ezBookkeeping id (a decimal string; never parsed into a number)
func orIsID(v string) bool {
	return orDigitsRe.MatchString(strings.TrimSpace(v))
}

// orData decodes an envelope's data as an object (numbers kept exact)
func orData(env *client.Envelope) map[string]any {
	if env == nil {
		return nil
	}

	return env.DataMap()
}

// orFindRows returns the row array under the first of keys present in data; failing that, the only
// array-of-objects value in data. `data` is never a bare array on this plane (apis.mdx §7.1a).
func orFindRows(data map[string]any, keys ...string) []map[string]any {
	if data == nil {
		return nil
	}

	for _, k := range keys {
		if arr, ok := data[k].([]any); ok {
			return orMaps(arr)
		}
	}

	var found []any
	n := 0

	for _, v := range data {
		if arr, ok := v.([]any); ok {
			if len(arr) == 0 {
				continue
			}

			if _, isObj := arr[0].(map[string]any); isObj {
				found = arr
				n++
			}
		}
	}

	if n == 1 {
		return orMaps(found)
	}

	return nil
}

func orMaps(arr []any) []map[string]any {
	out := make([]map[string]any, 0, len(arr))

	for _, r := range arr {
		if m, ok := r.(map[string]any); ok {
			out = append(out, m)
		}
	}

	return out
}

// orStr returns the first non-empty string at any of the dotted paths
func orStr(row map[string]any, paths ...string) string {
	for _, p := range paths {
		switch v := render.Get(row, p).(type) {
		case nil:
			continue
		case string:
			if v != "" {
				return v
			}
		case json.Number:
			return v.String()
		case bool:
			return strconv.FormatBool(v)
		default:
			s := fmt.Sprint(v)

			if s != "" {
				return s
			}
		}
	}

	return ""
}

// orVal returns the first non-nil value at any of the dotted paths
func orVal(row map[string]any, paths ...string) any {
	for _, p := range paths {
		if v := render.Get(row, p); v != nil {
			return v
		}
	}

	return nil
}

// orEmitRows prints a list answer: the envelope as-is for json, the prepared rows for table/csv.
// It notes an empty result and a truncation on stderr.
func orEmitRows(c *app.Ctx, env *client.Envelope, rows []map[string]any, cols []render.Column, empty string) error {
	if len(rows) == 0 && empty != "" {
		c.Info("%s", empty)
	}

	orNoteTruncation(c, env)

	switch c.Format {
	case render.FormatJSON:
		return c.Emit(env, render.View{})
	case render.FormatCSV:
		return render.CSV(c.Out, rows, cols)
	}

	return render.Table(c.Out, rows, cols)
}

func orNoteTruncation(c *app.Ctx, env *client.Envelope) {
	if env == nil || env.Meta == nil {
		return
	}

	if t, ok := env.Meta["truncated"].(bool); ok && t {
		c.Info("note: truncated at %v rows — more exist; narrow the filter or raise --limit", env.Meta["limitApplied"])
	}
}

// orEmitObject prints a single-object answer: json envelope, or key/value pairs (money keys
// rendered with their currency)
func orEmitObject(c *app.Ctx, env *client.Envelope, flat map[string]any, moneyKeys map[string]string) error {
	switch c.Format {
	case render.FormatJSON:
		return c.Emit(env, render.View{})
	case render.FormatCSV:
		rows := make([]map[string]any, 0, len(flat))

		for _, kv := range orSortedPairs(flat) {
			rows = append(rows, map[string]any{"key": kv[0], "value": kv[1]})
		}

		return render.CSV(c.Out, rows, []render.Column{{Header: "key", Key: "key"}, {Header: "value", Key: "value"}})
	}

	return render.KeyValues(c.Out, flat, moneyKeys)
}

// ---------------------------------------------------------------------------------------------
// Relative dates — resolved locally in the resolved timezone before the call; the wire never
// carries one (cli.mdx §7.5)
// ---------------------------------------------------------------------------------------------

var (
	orDayRe   = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)
	orMonthRe = regexp.MustCompile(`^\d{4}-\d{2}$`)
	orYearRe  = regexp.MustCompile(`^\d{4}$`)
)

// orSpan expands a date expression into the inclusive day range it names. relative reports whether
// it depended on "now"; ranged whether it names more than one day.
func orSpan(v string, now time.Time) (start, end time.Time, relative, ranged bool, err error) {
	loc := now.Location()
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, loc)
	firstOfMonth := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, loc)
	firstOfYear := time.Date(now.Year(), 1, 1, 0, 0, 0, 0, loc)

	switch strings.ReplaceAll(strings.ToLower(strings.TrimSpace(v)), "_", "-") {
	case "today":
		return today, today, true, false, nil
	case "yesterday":
		y := today.AddDate(0, 0, -1)
		return y, y, true, false, nil
	case "this-month":
		return firstOfMonth, firstOfMonth.AddDate(0, 1, -1), true, true, nil
	case "last-month":
		s := firstOfMonth.AddDate(0, -1, 0)
		return s, s.AddDate(0, 1, -1), true, true, nil
	case "this-year":
		return firstOfYear, firstOfYear.AddDate(1, 0, -1), true, true, nil
	case "last-year":
		s := firstOfYear.AddDate(-1, 0, 0)
		return s, s.AddDate(1, 0, -1), true, true, nil
	}

	raw := strings.TrimSpace(v)

	switch {
	case orDayRe.MatchString(raw):
		t, perr := time.ParseInLocation("2006-01-02", raw, loc)

		if perr != nil {
			errfile.Expected("parsing a typed day", perr)
			return time.Time{}, time.Time{}, false, false, fmt.Errorf("%q is not a real date", raw)
		}

		return t, t, false, false, nil
	case orMonthRe.MatchString(raw):
		t, perr := time.ParseInLocation("2006-01", raw, loc)

		if perr != nil {
			errfile.Expected("parsing a typed month", perr)
			return time.Time{}, time.Time{}, false, false, fmt.Errorf("%q is not a real month", raw)
		}

		return t, t.AddDate(0, 1, -1), false, true, nil
	case orYearRe.MatchString(raw):
		t, perr := time.ParseInLocation("2006", raw, loc)

		if perr != nil {
			errfile.Expected("parsing a typed year", perr)
			return time.Time{}, time.Time{}, false, false, fmt.Errorf("%q is not a year", raw)
		}

		return t, t.AddDate(1, 0, -1), false, true, nil
	}

	return time.Time{}, time.Time{}, false, false, fmt.Errorf("%q is not a date: use YYYY-MM-DD, YYYY-MM, YYYY, today, yesterday, this-month, last-month, this-year or last-year", v)
}

// orNow is the clock in the resolved timezone (a variable so tests can pin it)
var orNow = func(loc *time.Location) time.Time { return time.Now().In(loc) }

// orDateRange is a resolved --start/--end and the notes of what was resolved (for stderr)
type orDateRange struct {
	Start, End string
	Notes      []string
}

// orResolveRange resolves --start / --end. A span given as --start with no --end (this-month,
// 2025-06, last-year) covers the whole span; a span given as --end means its last day.
func orResolveRange(startV, endV string, now time.Time) (*orDateRange, error) {
	r := &orDateRange{}

	if startV != "" {
		s, e, rel, ranged, err := orSpan(startV, now)

		if err != nil {
			return nil, fmt.Errorf("--start: %w", err)
		}

		r.Start = s.Format("2006-01-02")

		if rel || ranged {
			r.Notes = append(r.Notes, fmt.Sprintf("--start %s → %s", startV, r.Start))
		}

		if endV == "" && ranged {
			r.End = e.Format("2006-01-02")
			r.Notes = append(r.Notes, fmt.Sprintf("--end (from --start %s) → %s", startV, r.End))
		}
	}

	if endV != "" {
		_, e, rel, ranged, err := orSpan(endV, now)

		if err != nil {
			return nil, fmt.Errorf("--end: %w", err)
		}

		r.End = e.Format("2006-01-02")

		if rel || ranged {
			r.Notes = append(r.Notes, fmt.Sprintf("--end %s → %s", endV, r.End))
		}
	}

	if r.Start != "" && r.End != "" && r.End < r.Start {
		return nil, fmt.Errorf("--end %s is before --start %s", r.End, r.Start)
	}

	return r, nil
}

// orResolveAsOf resolves --as-of to one day: a span means its last day, capped at today (a balance
// "as of this month" is the balance now, not at the end of a month that has not happened)
func orResolveAsOf(v string, now time.Time) (string, string, error) {
	if v == "" {
		return "", "", nil
	}

	_, e, rel, ranged, err := orSpan(v, now)

	if err != nil {
		return "", "", fmt.Errorf("--as-of: %w", err)
	}

	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())

	if (rel || ranged) && e.After(today) {
		e = today
	}

	d := e.Format("2006-01-02")
	note := ""

	if rel || ranged {
		note = fmt.Sprintf("--as-of %s → %s", v, d)
	}

	return d, note, nil
}

func orRangeFromFlags(c *app.Ctx) (*orDateRange, error) {
	r, err := orResolveRange(c.String("start"), c.String("end"), orNow(c.Location()))

	if err != nil {
		return nil, app.Usage(err.Error(), "ezbk help "+c.Verb.Name)
	}

	for _, n := range r.Notes {
		c.Info("dates: %s (%s)", n, c.Location())
	}

	return r, nil
}

// ---------------------------------------------------------------------------------------------
// Name → id resolution (ids are the contract; names are a convenience)
// ---------------------------------------------------------------------------------------------

// orNamed is one candidate for name resolution
type orNamed struct {
	ID   string
	Name string
	Note string
}

// orMatchName picks the one candidate named v: exact match first, then case-insensitive. It
// returns the match, or the candidates when the name is ambiguous, or neither when it is unknown.
func orMatchName(v string, cands []orNamed) (*orNamed, []orNamed) {
	var exact, fold []orNamed

	for _, c := range cands {
		if c.Name == v {
			exact = append(exact, c)
		} else if strings.EqualFold(strings.TrimSpace(c.Name), strings.TrimSpace(v)) {
			fold = append(fold, c)
		}
	}

	if len(exact) == 1 {
		return &exact[0], nil
	}

	if len(exact) > 1 {
		return nil, exact
	}

	if len(fold) == 1 {
		return &fold[0], nil
	}

	return nil, fold
}

// orCandCache holds each kind's candidates for the life of this one invocation
var orCandCache = map[string][]orNamed{}

// orKinds describes how each kind of entity is listed
var orKinds = map[string]struct {
	path    string
	rows    []string
	listCmd string
}{
	"account":  {"/accounts", []string{"accounts"}, "ezbk accounts list --include-hidden"},
	"category": {"/categories", []string{"categories"}, "ezbk categories list --include-hidden"},
	"tag":      {"/tags", []string{"tags"}, "ezbk tags list --include-hidden"},
}

func orCandidates(c *app.Ctx, kind string) ([]orNamed, error) {
	if cands, ok := orCandCache[kind]; ok {
		return cands, nil
	}

	k := orKinds[kind]
	env, err := c.Call("GET", k.path, url.Values{"include_hidden": {"true"}}, nil)

	if err != nil {
		return nil, err
	}

	rows := orFindRows(orData(env), k.rows...)
	var cands []orNamed

	switch kind {
	case "account":
		for _, r := range orFlattenAccounts(rows) {
			note := orStr(r, "currency")

			if p := orStr(r, "parentName"); p != "" {
				note = "sub-account of " + p + ", " + note
			}

			cands = append(cands, orNamed{ID: orStr(r, "id"), Name: orStr(r, "name"), Note: note})
		}
	case "category":
		for _, r := range orFlattenCategories(rows) {
			note := orCategoryTypeName(r["type"])

			if p := orStr(r, "parentName"); p != "" {
				note += ", under " + p
			} else {
				note += ", primary"
			}

			cands = append(cands, orNamed{ID: orStr(r, "id"), Name: orStr(r, "name"), Note: note})
		}
	default:
		for _, r := range rows {
			cands = append(cands, orNamed{ID: orStr(r, "id"), Name: orStr(r, "name")})
		}
	}

	orCandCache[kind] = cands

	return cands, nil
}

// orResolveID turns an id-or-name into an id. Ids pass through untouched and uncalled.
func orResolveID(c *app.Ctx, kind, v string) (string, error) {
	v = strings.TrimSpace(v)

	if v == "" {
		return "", app.Usage("an empty "+kind+" was given", "ezbk help "+c.Verb.Name)
	}

	if orIsID(v) {
		return v, nil
	}

	cands, err := orCandidates(c, kind)

	if err != nil {
		return "", err
	}

	match, ambiguous := orMatchName(v, cands)

	if match != nil {
		if match.Name != v {
			c.Info("%s %q → %q (id %s)", kind, v, match.Name, match.ID)
		}

		return match.ID, nil
	}

	if len(ambiguous) > 0 {
		lines := make([]string, 0, len(ambiguous))

		for _, a := range ambiguous {
			l := fmt.Sprintf("%s  %s", a.ID, a.Name)

			if a.Note != "" {
				l += "  (" + a.Note + ")"
			}

			lines = append(lines, l)
		}

		return "", app.Usage(fmt.Sprintf("the %s name %q is ambiguous — %d candidates", kind, v, len(ambiguous)),
			"pass the id instead:\n    "+strings.Join(lines, "\n    "))
	}

	return "", app.Fail(exitcode.NotFound, fmt.Sprintf("no %s named %q", kind, v), orKinds[kind].listCmd)
}

func orResolveIDs(c *app.Ctx, kind string, vs []string) ([]string, error) {
	out := make([]string, 0, len(vs))

	for _, v := range vs {
		id, err := orResolveID(c, kind, v)

		if err != nil {
			return nil, err
		}

		out = append(out, id)
	}

	return out, nil
}

// ---------------------------------------------------------------------------------------------
// Enum names (upstream's integers → the words the operator types)
// ---------------------------------------------------------------------------------------------

var orAccountCategoryNames = map[int64]string{
	1: "cash", 2: "checking", 3: "credit_card", 4: "virtual", 5: "debt",
	6: "receivables", 7: "investment", 8: "savings", 9: "certificate_of_deposit",
}

var orLiabilityCategories = map[string]bool{"credit_card": true, "debt": true}

func orAccountCategoryName(v any) string {
	if n, ok := render.ToInt64(v); ok {
		if s, ok := orAccountCategoryNames[n]; ok {
			return s
		}

		return strconv.FormatInt(n, 10)
	}

	return orAnyString(v)
}

var orTxnTypeNames = map[int64]string{1: "balance_modification", 2: "income", 3: "expense", 4: "transfer"}

func orTxnTypeName(v any) string {
	if n, ok := render.ToInt64(v); ok {
		if s, ok := orTxnTypeNames[n]; ok {
			return s
		}

		return strconv.FormatInt(n, 10)
	}

	return strings.ToLower(orAnyString(v))
}

var orCategoryTypeNames = map[int64]string{1: "income", 2: "expense", 3: "transfer"}

func orCategoryTypeName(v any) string {
	if n, ok := render.ToInt64(v); ok {
		if s, ok := orCategoryTypeNames[n]; ok {
			return s
		}

		return strconv.FormatInt(n, 10)
	}

	return strings.ToLower(orAnyString(v))
}

var orFrequencyNames = map[int64]string{0: "disabled", 1: "weekly", 2: "monthly", 3: "daily", 4: "yearly", 5: "every_n_days"}

func orFrequencyName(v any) string {
	if v == nil {
		return ""
	}

	if n, ok := render.ToInt64(v); ok {
		if s, ok := orFrequencyNames[n]; ok {
			return s
		}
	}

	return strings.ToLower(orAnyString(v))
}

var orTokenTypeNames = map[int64]string{1: "session", 2: "2fa_pending", 3: "email_verify", 4: "password_reset", 5: "mcp", 6: "oauth2_verify", 7: "oauth2_callback", 8: "api"}

func orTokenTypeName(v any) string {
	if n, ok := render.ToInt64(v); ok {
		if s, ok := orTokenTypeNames[n]; ok {
			return s
		}
	}

	return orAnyString(v)
}

// orUnixDisplay renders a unix timestamp (seconds, or milliseconds when it is that large) in loc
func orUnixDisplay(v any, loc *time.Location, layout string) string {
	n, ok := render.ToInt64(v)

	if !ok || n <= 0 {
		return orAnyString(v)
	}

	if n > 100_000_000_000 {
		n /= 1000
	}

	if loc == nil {
		loc = time.UTC
	}

	return time.Unix(n, 0).In(loc).Format(layout)
}

// ---------------------------------------------------------------------------------------------
// accounts
// ---------------------------------------------------------------------------------------------

// orFlattenAccounts turns the nested account list into display rows: each parent at depth 0 and
// its sub-accounts at depth 1 right under it. Nothing is summed; a parent (type 2, currency "---")
// holds no balance of its own and shows none.
func orFlattenAccounts(list []map[string]any) []map[string]any {
	out := make([]map[string]any, 0, len(list))

	for _, a := range list {
		row := orAccountRow(a, 0, "")
		out = append(out, row)

		if subs, ok := a["subAccounts"].([]any); ok {
			for _, s := range orMaps(subs) {
				out = append(out, orAccountRow(s, 1, orStr(a, "name")))
			}
		}
	}

	return out
}

func orAccountRow(a map[string]any, depth int, parent string) map[string]any {
	row := map[string]any{}

	for k, v := range a {
		if k != "subAccounts" {
			row[k] = v
		}
	}

	row["depth"] = depth
	row["categoryName"] = orAccountCategoryName(a["category"])

	if parent != "" {
		row["parentName"] = parent
	}

	side := orStr(a, "side")

	switch {
	case side != "":
	case a["isLiability"] == true || orLiabilityCategories[row["categoryName"].(string)]:
		side = "liability"
	case a["isAsset"] == true:
		side = "asset"
	}

	row["side"] = side

	isParent := orStr(a, "type") == "parent"

	if n, ok := render.ToInt64(a["type"]); ok && n == 2 {
		isParent = true
	}

	if subs, ok := a["subAccounts"].([]any); ok && len(subs) > 0 {
		isParent = true
	}

	if isParent || orStr(a, "currency") == "---" {
		row["balance"] = nil
		row["currency"] = ""
		row["kind"] = "parent"
	}

	return row
}

var orAccountColumns = []render.Column{
	{Header: "NAME", Key: "name", Indent: "depth"},
	{Header: "ID", Key: "id"},
	{Header: "CATEGORY", Key: "categoryName"},
	{Header: "SIDE", Key: "side"},
	{Header: "CURRENCY", Key: "currency"},
	{Header: "BALANCE", Key: "balance", Kind: render.KMoney, CurrencyKey: "currency"},
	{Header: "HIDDEN", Key: "hidden", Kind: render.KBool},
}

var orAccountCSVColumns = []render.Column{
	{Header: "id", Key: "id"},
	{Header: "name", Key: "name"},
	{Header: "parent", Key: "parentName"},
	{Header: "category", Key: "categoryName"},
	{Header: "side", Key: "side"},
	{Header: "balance", Key: "balance", Kind: render.KMoney, CurrencyKey: "currency"},
	{Header: "hidden", Key: "hidden", Kind: render.KBool},
}

// orCurrencySubtotals reads per-currency subtotals the SERVER returned with the list, in whichever
// of the shapes it uses; the CLI never adds balances (cli.mdx §9.2)
func orCurrencySubtotals(data map[string]any) []map[string]any {
	for _, k := range []string{"subtotals", "totalsByCurrency", "currencyTotals", "totals", "balancesByCurrency"} {
		switch v := data[k].(type) {
		case []any:
			var out []map[string]any

			for _, r := range orMaps(v) {
				cur := orStr(r, "currency")
				amt := orVal(r, "amount", "balance", "total", "totalBalance")

				if cur != "" && amt != nil {
					out = append(out, map[string]any{"currency": cur, "amount": amt})
				}
			}

			if len(out) > 0 {
				return out
			}
		case map[string]any:
			keys := make([]string, 0, len(v))

			for cur := range v {
				keys = append(keys, cur)
			}

			sort.Strings(keys)

			var out []map[string]any

			for _, cur := range keys {
				amt := v[cur]

				if m, ok := amt.(map[string]any); ok {
					amt = orVal(m, "amount", "balance", "total")
				}

				if _, ok := render.ToInt64(amt); ok {
					out = append(out, map[string]any{"currency": cur, "amount": amt})
				}
			}

			if len(out) > 0 {
				return out
			}
		}
	}

	return nil
}

func orRunAccountsList(c *app.Ctx) error {
	q := url.Values{}

	if c.Bool("include-hidden") {
		q.Set("include_hidden", "true")
	}

	if v := c.String("currency"); v != "" {
		q.Set("currency", strings.ToUpper(v))
	}

	if v := c.String("category"); v != "" {
		q.Set("category", strings.ToLower(v))
	}

	env, err := c.Call("GET", "/accounts", q, nil)

	if err != nil {
		return err
	}

	data := orData(env)
	rows := orFlattenAccounts(orFindRows(data, "accounts"))

	if c.Format == render.FormatCSV {
		return orEmitRows(c, env, rows, orAccountCSVColumns, "no accounts")
	}

	if err := orEmitRows(c, env, rows, orAccountColumns, "no accounts (add one in the browser or with ezbk accounts add)"); err != nil {
		return err
	}

	if c.Format != render.FormatTable {
		return nil
	}

	if subs := orCurrencySubtotals(data); len(subs) > 0 {
		if _, err := c.Out.Write([]byte("\n")); err != nil {
			return err
		}

		return render.Table(c.Out, subs, []render.Column{
			{Header: "CURRENCY", Key: "currency"},
			{Header: "SUBTOTAL (server)", Key: "amount", Kind: render.KMoney, CurrencyKey: "currency"},
		})
	}

	return nil
}

func orRunAccountsShow(c *app.Ctx) error {
	id, err := orResolveID(c, "account", c.Args[0])

	if err != nil {
		return err
	}

	env, err := c.Call("GET", "/accounts/"+url.PathEscape(id), nil, nil)

	if err != nil {
		return err
	}

	if c.Format == render.FormatJSON {
		return c.Emit(env, render.View{})
	}

	data := orData(env)
	acct := data

	if inner, ok := data["account"].(map[string]any); ok {
		acct = inner
	}

	rows := orFlattenAccounts([]map[string]any{acct})

	if c.Format == render.FormatCSV {
		return render.CSV(c.Out, rows, orAccountCSVColumns)
	}

	flat := map[string]any{}

	for k, v := range rows[0] {
		switch k {
		case "depth", "subAccounts", "icon", "iconType", "color", "displayOrder":
			continue
		}

		flat[k] = v
	}

	if v, ok := flat["lastReconciledTime"]; ok {
		if d, ok := flat["lastReconciledDate"]; ok && d != nil {
			delete(flat, "lastReconciledTime")
		} else {
			flat["lastReconciledTime"] = orUnixDisplay(v, c.Location(), "2006-01-02 15:04 MST")
		}
	}

	delete(flat, "categoryCode")
	delete(flat, "category")

	moneyKeys := map[string]string{"balance": "currency"}

	if _, ok := flat["creditCardLimit"]; ok {
		moneyKeys["creditCardLimit"] = "currency"
	}

	if err := render.KeyValues(c.Out, flat, moneyKeys); err != nil {
		return err
	}

	if len(rows) > 1 {
		if _, err := c.Out.Write([]byte("\nsub-accounts\n")); err != nil {
			return err
		}

		return render.Table(c.Out, rows[1:], orAccountColumns)
	}

	return nil
}

func orRunAccountsBalance(c *app.Ctx) error {
	id, err := orResolveID(c, "account", c.Args[0])

	if err != nil {
		return err
	}

	asOf, note, err := orResolveAsOf(c.String("as-of"), orNow(c.Location()))

	if err != nil {
		return app.Usage(err.Error(), "ezbk help accounts balance")
	}

	if note != "" {
		c.Info("dates: %s (%s)", note, c.Location())
	}

	q := url.Values{}

	if asOf != "" {
		q.Set("as_of", asOf)
	}

	env, err := c.Call("GET", "/accounts/"+url.PathEscape(id)+"/balance", q, nil)

	if err != nil {
		return err
	}

	if c.Format == render.FormatJSON {
		return c.Emit(env, render.View{})
	}

	data := orData(env)
	cur := orStr(data, "currency", "account.currency")
	day := orStr(data, "asOf", "as_of", "date")

	if day == "" {
		day = asOf
	}

	name := orStr(data, "name", "accountName", "account.name")

	if name != "" {
		c.Info("%s (id %s), as of %s%s", name, id, orNonEmpty(day, "today"), map[bool]string{true: "", false: " (" + orStr(data, "timezone") + ")"}[orStr(data, "timezone") == ""])
	}

	// a parent account holds no balance of its own: the server lists its sub-accounts and totals
	// them per currency; the CLI prints both and adds nothing
	if subs, ok := data["subAccounts"].([]any); ok && data["balance"] == nil {
		rows := orMaps(subs)

		for _, r := range rows {
			r["id"] = orStr(r, "accountId", "id")
		}

		cols := []render.Column{
			{Header: "SUB-ACCOUNT", Key: "name"}, {Header: "ID", Key: "id"}, {Header: "CURRENCY", Key: "currency"},
			{Header: "BALANCE", Key: "balance", Kind: render.KMoney, CurrencyKey: "currency"},
		}

		if c.Format == render.FormatCSV {
			return render.CSV(c.Out, rows, []render.Column{
				{Header: "account_id", Key: "id"}, {Header: "name", Key: "name"}, {Header: "as_of", Key: "asOf"},
				{Header: "balance", Key: "balance", Kind: render.KMoney, CurrencyKey: "currency"},
			})
		}

		if err := render.Table(c.Out, rows, cols); err != nil {
			return err
		}

		if totals := orCurrencySubtotals(data); len(totals) > 0 {
			if _, err := c.Out.Write([]byte("\n")); err != nil {
				return err
			}

			return render.Table(c.Out, totals, []render.Column{
				{Header: "CURRENCY", Key: "currency"},
				{Header: "TOTAL (server)", Key: "amount", Kind: render.KMoney, CurrencyKey: "currency"},
			})
		}

		return nil
	}

	bal := orVal(data, "balance", "closingBalance", "amount")

	if now, ok := render.ToInt64(data["currentBalance"]); ok {
		if b, ok2 := render.ToInt64(bal); ok2 && b != now {
			c.Info("the balance now, including transactions dated after %s: %s", day, render.Money(now, cur))
		}
	}

	row := map[string]any{"id": id, "asOf": day, "balance": bal, "currency": cur}

	if c.Format == render.FormatCSV {
		return render.CSV(c.Out, []map[string]any{row}, []render.Column{
			{Header: "account_id", Key: "id"}, {Header: "as_of", Key: "asOf"},
			{Header: "balance", Key: "balance", Kind: render.KMoney, CurrencyKey: "currency"},
		})
	}

	n, ok := render.ToInt64(bal)

	if !ok {
		return app.Fail(exitcode.Failed, "the server's balance answer carried no integer balance", "ezbk accounts balance "+id+" --format json")
	}

	return c.EmitText(render.Hundredths(n, true) + " " + cur)
}

func orRunAccountsReconciliation(c *app.Ctx) error {
	id, err := orResolveID(c, "account", c.Args[0])

	if err != nil {
		return err
	}

	r, err := orRangeFromFlags(c)

	if err != nil {
		return err
	}

	if r.Start == "" || r.End == "" {
		return app.Usage("`ezbk accounts reconciliation` needs --start and --end", "e.g. --start 2026-08-01 --end 2026-08-31, or --start last-month")
	}

	env, err := c.Call("GET", "/accounts/"+url.PathEscape(id)+"/reconciliation", app.Query("start", r.Start, "end", r.End), nil)

	if err != nil {
		return err
	}

	if c.Format == render.FormatJSON {
		return c.Emit(env, render.View{})
	}

	data := orData(env)
	stmt := data

	if inner, ok := data["statement"].(map[string]any); ok {
		stmt = inner
	}

	txns := orFindRows(stmt, "transactions", "items", "rows")
	cur := orStr(stmt, "currency", "account.currency")

	if cur == "" {
		cur = orStr(data, "currency", "account.currency")
	}

	if cur == "" && len(txns) > 0 {
		cur = orStr(txns[0], "sourceAccount.currency", "currency")
	}

	rows := make([]map[string]any, 0, len(txns))

	for _, t := range txns {
		row := orTxnDisplay(t, c.Location())
		row["closing"] = orVal(t, "accountClosingBalance", "closingBalance", "runningBalance")
		row["opening"] = orVal(t, "accountOpeningBalance", "openingBalance")

		if row["currency"] == "" {
			row["currency"] = cur
		}

		row["statementCurrency"] = cur
		rows = append(rows, row)
	}

	if c.Format == render.FormatCSV {
		return render.CSV(c.Out, rows, []render.Column{
			{Header: "id", Key: "id"}, {Header: "date", Key: "date"}, {Header: "type", Key: "type"},
			{Header: "category", Key: "category"}, {Header: "comment", Key: "comment"},
			{Header: "amount", Key: "signedAmount", Kind: render.KMoney, CurrencyKey: "currency"},
			{Header: "running_balance", Key: "closing", Kind: render.KMoney, CurrencyKey: "statementCurrency"},
		})
	}

	summary := []map[string]any{}

	for _, kv := range [][2]string{{"opening balance", "openingBalance"}, {"inflows", "totalInflows"}, {"outflows", "totalOutflows"}, {"closing balance", "closingBalance"}} {
		if v := orVal(stmt, kv[1]); v != nil {
			summary = append(summary, map[string]any{"what": kv[0], "amount": v, "currency": cur})
		}
	}

	c.Info("reconciliation statement %s .. %s (%s)", r.Start, r.End, c.Location())

	if len(summary) > 0 {
		if err := render.Table(c.Out, summary, []render.Column{{Header: "STATEMENT", Key: "what"}, {Header: "AMOUNT", Key: "amount", Kind: render.KMoney, CurrencyKey: "currency"}}); err != nil {
			return err
		}

		if _, err := c.Out.Write([]byte("\n")); err != nil {
			return err
		}
	}

	if len(rows) == 0 {
		c.Info("no transactions in this range")
	}

	return render.Table(c.Out, rows, []render.Column{
		{Header: "DATE", Key: "date"}, {Header: "TYPE", Key: "type"}, {Header: "CATEGORY", Key: "category"},
		{Header: "AMOUNT", Key: "amountText", Kind: render.KInt},
		{Header: "BALANCE", Key: "closing", Kind: render.KMoney, CurrencyKey: "statementCurrency"},
		{Header: "COMMENT", Key: "comment"}, {Header: "ID", Key: "id"},
	})
}

// ---------------------------------------------------------------------------------------------
// transactions
// ---------------------------------------------------------------------------------------------

var orTxnTypes = map[string]bool{"income": true, "expense": true, "transfer": true, "balance_modification": true}

// orTxnQuery builds the shared filter query. Names are resolved to ids first; relative dates are
// resolved locally and reported on stderr.
func orTxnQuery(c *app.Ctx) (url.Values, error) {
	q := url.Values{}

	orSplitIDsNames(q, "account_ids", "account_names", c.Strings("account"))

	r, err := orRangeFromFlags(c)

	if err != nil {
		return nil, err
	}

	if r.Start != "" {
		q.Set("start", r.Start)
	}

	if r.End != "" {
		q.Set("end", r.End)
	}

	if t := strings.ToLower(strings.ReplaceAll(c.String("type"), "-", "_")); t != "" {
		if !orTxnTypes[t] {
			return nil, app.Usage(fmt.Sprintf("--type %q is not income, expense, transfer or balance_modification", c.String("type")), "ezbk help "+c.Verb.Name)
		}

		q.Set("type", t)
	}

	orSplitIDsNames(q, "category_ids", "category_names", c.Strings("category"))

	tags := c.Strings("tag")
	expr := c.String("tag-filter")

	if len(tags) > 0 && expr != "" {
		return nil, app.Usage("--tag and --tag-filter cannot be combined", "use --tag for \"has any of these\", or write the whole expression in --tag-filter")
	}

	orSplitIDsNames(q, "tag_ids", "tag_names", tags)

	if c.Bool("untagged") {
		if expr != "" || len(tags) > 0 {
			return nil, app.Usage("--untagged cannot be combined with --tag / --tag-filter", "pick one")
		}

		q.Set("untagged", "true")
	}

	if expr != "" {
		q.Set("tag_filter", expr)
	}

	if v := c.String("keyword"); v != "" {
		q.Set("keyword", v)
	}

	for _, pair := range [][2]string{{"min", "min_amount"}, {"max", "max_amount"}} {
		n, given, err := c.Amount(pair[0])

		if err != nil {
			return nil, err
		}

		if given {
			q.Set(pair[1], strconv.FormatInt(n, 10))
		}
	}

	if cur := c.String("currency"); cur != "" {
		q.Set("currency", strings.ToUpper(cur))
	}

	if c.Bool("with-pictures") {
		q.Set("with_pictures", "true")
	}

	return q, nil
}

// orSplitIDsNames puts id values in idKey and names in nameKey: the server resolves names
// (exact first, then case-insensitive; ambiguity is an error carrying the candidates)
func orSplitIDsNames(q url.Values, idKey, nameKey string, values []string) {
	var ids, names []string

	for _, v := range values {
		if orIsID(v) {
			ids = append(ids, v)
		} else {
			names = append(names, v)
		}
	}

	app.AddList(q, idKey, ids)

	for _, n := range names {
		q.Add(nameKey, n)
	}
}

// orTxnDate is a transaction's calendar date: the server's `date` when it sends one, otherwise the
// day of `time` at the transaction's own UTC offset (each transaction keeps its own, §17.4)
func orTxnDate(t map[string]any, loc *time.Location) string {
	if d := orStr(t, "date"); d != "" {
		return d
	}

	sec, ok := render.ToInt64(orVal(t, "time", "transactionTime"))

	if !ok || sec <= 0 {
		return ""
	}

	if sec > 100_000_000_000 {
		sec /= 1000
	}

	if off, ok := render.ToInt64(t["utcOffset"]); ok {
		return time.Unix(sec, 0).In(time.FixedZone("", int(off)*60)).Format("2006-01-02")
	}

	if loc == nil {
		loc = time.UTC
	}

	return time.Unix(sec, 0).In(loc).Format("2006-01-02")
}

// orTxnDisplay flattens one transaction for table/CSV. A transfer stays ONE row carrying both sides
// in their own currencies; nothing is added, and the sign is display only (the JSON keeps
// upstream's {type, amount} shape verbatim).
func orTxnDisplay(t map[string]any, loc *time.Location) map[string]any {
	typ := orStr(t, "typeName")

	if typ == "" {
		typ = orTxnTypeName(t["type"])
	}
	srcName := orStr(t, "sourceAccount.name", "sourceAccountName", "account.name", "accountName", "transfer.source.accountName")
	dstName := orStr(t, "destinationAccount.name", "destinationAccountName", "transfer.destination.accountName")
	srcCur := orStr(t, "sourceAccount.currency", "sourceCurrency", "currency")
	dstCur := orStr(t, "destinationAccount.currency", "destinationCurrency")

	if srcName == "" {
		srcName = orStr(t, "sourceAccountId", "accountId")
	}

	if dstName == "" {
		dstName = orStr(t, "destinationAccountId")
	}

	var tagNames []string

	if tags, ok := t["tags"].([]any); ok {
		for _, tg := range orMaps(tags) {
			tagNames = append(tagNames, orStr(tg, "name"))
		}
	} else if names, ok := t["tagNames"].([]any); ok {
		for _, n := range names {
			tagNames = append(tagNames, orAnyString(n))
		}
	}

	srcAmt := orVal(t, "sourceAmount", "amount")
	dstAmt := orVal(t, "destinationAmount")
	hidden := t["hideAmount"] == true

	row := map[string]any{
		"id":                  orStr(t, "id"),
		"date":                orTxnDate(t, loc),
		"type":                typ,
		"account":             srcName,
		"sourceAccountId":     orStr(t, "sourceAccountId"),
		"sourceAccount":       srcName,
		"destinationAccount":  "",
		"category":            orStr(t, "category.name", "categoryName"),
		"categoryId":          orStr(t, "categoryId"),
		"comment":             orStr(t, "comment"),
		"tags":                strings.Join(tagNames, ","),
		"currency":            srcCur,
		"sourceAmount":        srcAmt,
		"destinationAmount":   nil,
		"destinationCurrency": "",
		"signedAmount":        orSigned(srcAmt, typ),
	}

	srcN, srcOK := render.ToInt64(srcAmt)
	amountText := ""

	switch {
	case hidden:
		amountText = "***"
	case typ == "transfer":
		row["account"] = srcName + " → " + dstName
		row["destinationAccount"] = dstName
		row["destinationAccountId"] = orStr(t, "destinationAccountId")
		row["destinationAmount"] = dstAmt
		row["destinationCurrency"] = dstCur

		dstN, dstOK := render.ToInt64(dstAmt)

		if !dstOK {
			dstN, dstOK = srcN, srcOK
		}

		if dstCur == "" {
			dstCur = srcCur
		}

		if srcOK && dstOK {
			amountText = render.Money(-srcN, srcCur) + orCurSuffix(srcCur) + " → +" + render.Money(dstN, dstCur) + orCurSuffix(dstCur)
		}
	case srcOK && typ == "expense":
		amountText = render.Money(-srcN, srcCur) + orCurSuffix(srcCur)
	case srcOK && typ == "income":
		amountText = "+" + render.Money(srcN, srcCur) + orCurSuffix(srcCur)
	case srcOK:
		amountText = render.Money(srcN, srcCur) + orCurSuffix(srcCur)
	}

	row["amountText"] = amountText

	return row
}

// orCurSuffix appends the code when render.Money used a symbol (so "$" and "CA$" never get mixed up
// in a column that spans currencies)
func orCurSuffix(cur string) string {
	if cur == "" {
		return ""
	}

	if strings.HasSuffix(render.Money(0, cur), " "+cur) {
		return ""
	}

	return " " + cur
}

// orSigned is the display sign of a transaction amount: expense and transfer-out negative
func orSigned(v any, typ string) any {
	n, ok := render.ToInt64(v)

	if !ok {
		return v
	}

	if typ == "expense" || typ == "transfer" {
		return -n
	}

	return n
}

var orTxnTableColumns = []render.Column{
	{Header: "DATE", Key: "date"},
	{Header: "TYPE", Key: "type"},
	{Header: "ACCOUNT", Key: "account"},
	{Header: "CATEGORY", Key: "category"},
	{Header: "AMOUNT", Key: "amountText", Kind: render.KInt},
	{Header: "COMMENT", Key: "comment"},
	{Header: "TAGS", Key: "tags"},
	{Header: "ID", Key: "id"},
}

var orTxnCSVColumns = []render.Column{
	{Header: "id", Key: "id"},
	{Header: "date", Key: "date"},
	{Header: "type", Key: "type"},
	{Header: "source_account_id", Key: "sourceAccountId"},
	{Header: "source_account", Key: "sourceAccount"},
	{Header: "source_amount", Key: "sourceAmount", Kind: render.KMoney, CurrencyKey: "currency"},
	{Header: "destination_account", Key: "destinationAccount"},
	{Header: "destination_amount", Key: "destinationAmount", Kind: render.KMoney, CurrencyKey: "destinationCurrency"},
	{Header: "category_id", Key: "categoryId"},
	{Header: "category", Key: "category"},
	{Header: "comment", Key: "comment"},
	{Header: "tags", Key: "tags"},
}

func orRunTransactionsList(c *app.Ctx) error {
	q, err := orTxnQuery(c)

	if err != nil {
		return err
	}

	limit, err := c.Int("limit", 0)

	if err != nil {
		return err
	}

	if limit < 0 {
		return app.Usage("--limit must be positive", "ezbk transactions list --limit 500")
	}

	if limit > 5000 {
		c.Info("note: --limit %d is above the server's cap; it will return at most 5000 rows", limit)
	}

	if limit > 0 {
		q.Set("limit", strconv.Itoa(limit))
	}

	if v := c.String("cursor"); v != "" {
		q.Set("cursor", v)
	}

	if v := c.String("order"); v != "" {
		q.Set("order", v)
	}

	env, err := c.Call("GET", "/transactions", q, nil)

	if err != nil {
		return err
	}

	data := orData(env)
	txns := orFindRows(data, "transactions", "items")
	rows := make([]map[string]any, 0, len(txns))

	for _, t := range txns {
		rows = append(rows, orTxnDisplay(t, c.Location()))
	}

	cols := orTxnTableColumns

	if c.Format == render.FormatCSV {
		cols = orTxnCSVColumns
	}

	if err := orEmitRows(c, env, rows, cols, "no transactions match"); err != nil {
		return err
	}

	if next := orStr(data, "nextCursor", "next_cursor"); next != "" {
		c.Info("more: --cursor %s", next)
	}

	return nil
}

func orRunTransactionsShow(c *app.Ctx) error {
	id := strings.TrimSpace(c.Args[0])

	if !orIsID(id) {
		return app.Usage(fmt.Sprintf("%q is not a transaction id (a decimal string)", id), "ezbk transactions list")
	}

	env, err := c.Call("GET", "/transactions/"+url.PathEscape(id), nil, nil)

	if err != nil {
		return err
	}

	if c.Format == render.FormatJSON {
		return c.Emit(env, render.View{})
	}

	data := orData(env)
	t := data

	if inner, ok := data["transaction"].(map[string]any); ok {
		t = inner
	}

	row := orTxnDisplay(t, c.Location())

	if c.Format == render.FormatCSV {
		return render.CSV(c.Out, []map[string]any{row}, orTxnCSVColumns)
	}

	flat := map[string]any{
		"id": row["id"], "date": row["date"], "type": row["type"], "category": row["category"],
		"comment": row["comment"], "tags": row["tags"], "amount": row["amountText"],
		"sourceAccount": row["sourceAccount"],
	}

	if row["type"] == "transfer" {
		flat["destinationAccount"] = row["destinationAccount"]
	}

	if v := orVal(t, "time", "transactionTime"); v != nil {
		flat["time"] = orUnixDisplay(v, c.Location(), "2006-01-02 15:04:05 MST")
	}

	if pics, ok := t["pictures"].([]any); ok {
		flat["pictures"] = len(pics)
	}

	if g, ok := t["geoLocation"].(map[string]any); ok {
		flat["geoLocation"] = fmt.Sprintf("%v,%v", g["latitude"], g["longitude"])
	}

	if e, ok := t["editable"].(bool); ok {
		flat["editable"] = e
	}

	return render.KeyValues(c.Out, flat, nil)
}

func orRunTransactionsCount(c *app.Ctx) error {
	q, err := orTxnQuery(c)

	if err != nil {
		return err
	}

	env, err := c.Call("GET", "/transactions/count", q, nil)

	if err != nil {
		return err
	}

	if c.Format == render.FormatJSON {
		return c.Emit(env, render.View{})
	}

	n := orVal(orData(env), "count", "totalCount")

	if c.Format == render.FormatCSV {
		return render.CSV(c.Out, []map[string]any{{"count": n}}, []render.Column{{Header: "count", Key: "count"}})
	}

	return c.EmitText(orAnyString(n))
}

// orExportFormat picks csv or tsv. The universal --format only knows json|table|csv (the spine
// validates it before any verb runs), so TSV is asked for with --tsv.
func orExportFormat(c *app.Ctx) string {
	if c.Bool("tsv") {
		return "tsv"
	}

	if c.Has("format") && c.Format != render.FormatCSV {
		c.Info("note: an export is always ezBookkeeping CSV (or --tsv); --format %s is ignored", c.Format)
	}

	return "csv"
}

func orRunTransactionsExport(c *app.Ctx) error {
	q, err := orTxnQuery(c)

	if err != nil {
		return err
	}

	q.Set("format", orExportFormat(c))

	env, err := c.Call("GET", "/transactions/export", q, nil)

	if err != nil {
		return err
	}

	return orEmitExport(c, env)
}

// orEmitExport writes an export body to stdout byte for byte
func orEmitExport(c *app.Ctx, env *client.Envelope) error {
	if env == nil {
		return nil
	}

	if len(env.Data) == 0 && env.Meta == nil && len(env.Raw) > 0 {
		_, err := c.Out.Write(env.Raw)
		return err
	}

	// a server that wraps the export in the envelope puts the text in data.content / data.csv
	data := orData(env)

	if s := orStr(data, "content", "csv", "tsv", "data"); s != "" {
		return c.EmitText(s)
	}

	return c.Emit(env, render.View{})
}

// ---------------------------------------------------------------------------------------------
// categories, tags, tag groups
// ---------------------------------------------------------------------------------------------

// orFlattenCategories returns primaries at depth 0 with their secondaries at depth 1, whether the
// server nests them (subCategories) or lists them flat with parentId
func orFlattenCategories(list []map[string]any) []map[string]any {
	out := make([]map[string]any, 0, len(list))
	nested := false

	for _, c := range list {
		if _, ok := c["subCategories"]; ok {
			nested = true
			break
		}
	}

	row := func(c map[string]any, depth int, parent string) map[string]any {
		r := map[string]any{}

		for k, v := range c {
			if k != "subCategories" {
				r[k] = v
			}
		}

		r["depth"] = depth
		r["typeName"] = orCategoryTypeName(c["type"])
		r["level"] = map[int]string{0: "primary", 1: "secondary"}[depth]

		if parent != "" {
			r["parentName"] = parent
		}

		return r
	}

	if nested {
		for _, c := range list {
			out = append(out, row(c, 0, ""))

			if subs, ok := c["subCategories"].([]any); ok {
				for _, s := range orMaps(subs) {
					out = append(out, row(s, 1, orStr(c, "name")))
				}
			}
		}

		return out
	}

	isPrimary := func(c map[string]any) bool {
		p := orStr(c, "parentId")
		return p == "" || p == "0"
	}

	children := map[string][]map[string]any{}
	known := map[string]bool{}

	for _, c := range list {
		if isPrimary(c) {
			known[orStr(c, "id")] = true
		}
	}

	var orphans []map[string]any

	for _, c := range list {
		if isPrimary(c) {
			continue
		}

		p := orStr(c, "parentId")

		if known[p] {
			children[p] = append(children[p], c)
		} else {
			orphans = append(orphans, c)
		}
	}

	for _, c := range list {
		if !isPrimary(c) {
			continue
		}

		out = append(out, row(c, 0, ""))

		for _, s := range children[orStr(c, "id")] {
			out = append(out, row(s, 1, orStr(c, "name")))
		}
	}

	for _, s := range orphans {
		out = append(out, row(s, 1, ""))
	}

	return out
}

var orCategoryColumns = []render.Column{
	{Header: "NAME", Key: "name", Indent: "depth"},
	{Header: "ID", Key: "id"},
	{Header: "TYPE", Key: "typeName"},
	{Header: "LEVEL", Key: "level"},
	{Header: "HIDDEN", Key: "hidden", Kind: render.KBool},
}

var orCategoryCSVColumns = []render.Column{
	{Header: "id", Key: "id"},
	{Header: "name", Key: "name"},
	{Header: "type", Key: "typeName"},
	{Header: "level", Key: "level"},
	{Header: "parent_id", Key: "parentId"},
	{Header: "parent", Key: "parentName"},
	{Header: "hidden", Key: "hidden", Kind: render.KBool},
}

func orRunCategoriesList(c *app.Ctx) error {
	q := url.Values{}

	if t := strings.ToLower(c.String("type")); t != "" {
		if t != "income" && t != "expense" && t != "transfer" {
			return app.Usage(fmt.Sprintf("--type %q is not income, expense or transfer", t), "ezbk help categories list")
		}

		q.Set("type", t)
	}

	if c.Bool("include-hidden") {
		q.Set("include_hidden", "true")
	}

	if v := c.String("parent"); v != "" {
		q.Set("parent_id", v) // an id or a name; the server resolves it
	}

	env, err := c.Call("GET", "/categories", q, nil)

	if err != nil {
		return err
	}

	rows := orFlattenCategories(orFindRows(orData(env), "categories"))
	cols := orCategoryColumns

	if c.Format == render.FormatCSV {
		cols = orCategoryCSVColumns
	}

	return orEmitRows(c, env, rows, cols, "no categories (the browser offers a default set; or ezbk categories add)")
}

// orRunCategoriesTree prints the server's YAML document (data.yaml) even when piped; an explicit
// --format json prints the whole envelope, the structured groups included. The CLI never assembles the tree itself.
func orRunCategoriesTree(c *app.Ctx) error {
	q := url.Values{}

	if t := strings.ToLower(c.String("type")); t != "" {
		if t != "income" && t != "expense" && t != "transfer" {
			return app.Usage(fmt.Sprintf("--type %q is not income, expense or transfer", t), "ezbk help categories tree")
		}

		q.Set("type", t)
	}

	if c.Bool("include-hidden") {
		q.Set("include_hidden", "true")
	}

	env, err := c.Call("GET", "/categories/tree", q, nil)

	if err != nil {
		return err
	}

	// YAML is the point of this verb, piped or not; only an explicit --format json asks for the envelope
	if strings.EqualFold(strings.TrimSpace(c.String("format")), "json") {
		return render.PrettyJSON(c.Out, env.Raw)
	}

	doc, _ := orData(env)["yaml"].(string)

	if doc == "" {
		c.Info("%s", "no categories (the browser offers a default set; or ezbk categories add)")
		return nil
	}

	_, err = io.WriteString(c.Out, doc)

	return err
}

func orRunTagsList(c *app.Ctx) error {
	q := url.Values{}

	if c.Bool("include-hidden") {
		q.Set("include_hidden", "true")
	}

	if v := c.String("group"); v != "" {
		if orIsID(v) {
			q.Set("group_id", v)
		} else {
			q.Set("group_name", v)
		}
	}

	env, err := c.Call("GET", "/tags", q, nil)

	if err != nil {
		return err
	}

	rows := orFindRows(orData(env), "tags")

	for _, r := range rows {
		if g := orStr(r, "groupId", "tagGroupId"); g == "0" {
			r["groupId"] = ""
		}
	}

	return orEmitRows(c, env, rows, []render.Column{
		{Header: "NAME", Key: "name"},
		{Header: "ID", Key: "id"},
		{Header: "GROUP", Key: "groupName"},
		{Header: "GROUP ID", Key: "groupId"},
		{Header: "HIDDEN", Key: "hidden", Kind: render.KBool},
	}, "no tags")
}

func orRunTagGroupsList(c *app.Ctx) error {
	env, err := c.Call("GET", "/tag-groups", nil, nil)

	if err != nil {
		return err
	}

	rows := orFindRows(orData(env), "tagGroups", "tag_groups", "groups")

	return orEmitRows(c, env, rows, []render.Column{
		{Header: "NAME", Key: "name"},
		{Header: "ID", Key: "id"},
		{Header: "TAGS", Key: "tagCount", Kind: render.KInt},
		{Header: "ORDER", Key: "displayOrder", Kind: render.KInt},
	}, "no tag groups")
}

// ---------------------------------------------------------------------------------------------
// templates and schedules
// ---------------------------------------------------------------------------------------------

func orRunTemplatesList(c *app.Ctx) error {
	q := url.Values{}

	switch {
	case c.Bool("scheduled") && c.Bool("normal"):
		return app.Usage("--scheduled and --normal together is every template; drop both", "ezbk templates list")
	case c.Bool("scheduled"):
		q.Set("kind", "scheduled")
	case c.Bool("normal"):
		q.Set("kind", "normal")
	}

	if c.Bool("include-hidden") {
		q.Set("include_hidden", "true")
	}

	env, err := c.Call("GET", "/templates", q, nil)

	if err != nil {
		return err
	}

	tpls := orFindRows(orData(env), "templates", "schedules")
	rows := make([]map[string]any, 0, len(tpls))

	for _, t := range tpls {
		row := orTxnDisplay(t, c.Location())
		row["name"] = orStr(t, "name")
		row["hidden"] = t["hidden"]
		row["kind"] = orStr(t, "kind")

		if row["kind"] == "" {
			row["kind"] = map[string]string{"1": "normal", "2": "scheduled"}[orStr(t, "templateType")]
		}

		row["frequency"] = orStr(t, "schedule.frequency")

		if row["frequency"] == "" {
			row["frequency"] = orFrequencyName(orVal(t, "scheduledFrequencyType", "frequency"))
		}

		row["every"] = orStr(t, "schedule.describe")

		if row["every"] == "" {
			row["every"] = row["frequency"]
		}

		row["frequencyValue"] = orStr(t, "schedule.frequencyValue", "scheduledFrequency", "frequencyValue")
		row["start"] = orStr(t, "schedule.start", "scheduledStartDate", "start")
		row["end"] = orStr(t, "schedule.end", "scheduledEndDate", "end")
		row["next"] = orStr(t, "schedule.nextOccurrence")

		if t["schedule"] != nil {
			switch {
			case render.Get(t, "schedule.paused") == true:
				row["state"] = "paused"
			case render.Get(t, "schedule.ended") == true:
				row["state"] = "ended"
			default:
				row["state"] = "active"
			}
		}
		rows = append(rows, row)
	}

	cols := []render.Column{
		{Header: "NAME", Key: "name"},
		{Header: "ID", Key: "id"},
		{Header: "KIND", Key: "kind"},
		{Header: "TYPE", Key: "type"},
		{Header: "ACCOUNT", Key: "account"},
		{Header: "CATEGORY", Key: "category"},
		{Header: "AMOUNT", Key: "amountText", Kind: render.KInt},
	}

	if !c.Bool("normal") {
		cols = append(cols,
			render.Column{Header: "SCHEDULE", Key: "every"},
			render.Column{Header: "STATE", Key: "state"},
			render.Column{Header: "NEXT", Key: "next"},
			render.Column{Header: "START", Key: "start"},
			render.Column{Header: "END", Key: "end"},
		)
	}

	cols = append(cols, render.Column{Header: "HIDDEN", Key: "hidden", Kind: render.KBool})

	if c.Format == render.FormatCSV {
		cols = []render.Column{
			{Header: "id", Key: "id"}, {Header: "name", Key: "name"}, {Header: "kind", Key: "kind"}, {Header: "type", Key: "type"},
			{Header: "source_account", Key: "sourceAccount"}, {Header: "destination_account", Key: "destinationAccount"},
			{Header: "category", Key: "category"},
			{Header: "source_amount", Key: "sourceAmount", Kind: render.KMoney, CurrencyKey: "currency"},
			{Header: "destination_amount", Key: "destinationAmount", Kind: render.KMoney, CurrencyKey: "destinationCurrency"},
			{Header: "frequency", Key: "frequency"}, {Header: "frequency_value", Key: "frequencyValue"},
			{Header: "state", Key: "state"}, {Header: "next", Key: "next"},
			{Header: "start", Key: "start"}, {Header: "end", Key: "end"}, {Header: "comment", Key: "comment"},
			{Header: "hidden", Key: "hidden", Kind: render.KBool},
		}
	}

	empty := "no templates"

	if c.Bool("scheduled") {
		empty = "no scheduled transactions (ezbk schedules add)"
	}

	return orEmitRows(c, env, rows, cols, empty)
}

func orRunSchedulesUpcoming(c *app.Ctx) error {
	days, err := c.Int("days", 0)

	if err != nil {
		return err
	}

	if days < 0 || days > 3660 {
		return app.Usage("--days must be between 1 and 3660", "ezbk schedules upcoming --days 30")
	}

	q := url.Values{}

	if days > 0 {
		q.Set("days", strconv.Itoa(days))
	}

	for _, t := range c.Strings("template") {
		q.Add("template_ids", t) // ids or names; the server resolves them
	}

	if v := c.String("from"); v != "" {
		r, err := orResolveRange(v, "", orNow(c.Location()))

		if err != nil {
			return app.Usage(strings.Replace(err.Error(), "--start", "--from", 1), "ezbk help schedules upcoming")
		}

		if r.Start != v {
			c.Info("dates: --from %s → %s (%s)", v, r.Start, c.Location())
		}

		q.Set("from", r.Start)
	}

	if limit, err := c.Int("limit", 0); err != nil {
		return err
	} else if limit > 0 {
		q.Set("limit", strconv.Itoa(limit))
	}

	env, err := c.Call("GET", "/schedules/upcoming", q, nil)

	if err != nil {
		return err
	}

	occ := orFindRows(orData(env), "occurrences", "upcoming", "items")
	rows := make([]map[string]any, 0, len(occ))

	for _, o := range occ {
		src := o

		if tx, ok := o["transaction"].(map[string]any); ok {
			src = tx
		} else if tp, ok := o["template"].(map[string]any); ok {
			src = tp
		}

		row := orTxnDisplay(src, c.Location())
		row["date"] = orStr(o, "date", "dueDate", "on")

		if row["date"] == "" {
			row["date"] = orTxnDate(o, c.Location())
		}

		row["template"] = orStr(o, "templateName", "name", "template.name")
		row["templateId"] = orStr(o, "templateId", "template.id", "id")
		rows = append(rows, row)
	}

	cols := []render.Column{
		{Header: "DATE", Key: "date"}, {Header: "SCHEDULE", Key: "template"}, {Header: "TYPE", Key: "type"},
		{Header: "ACCOUNT", Key: "account"}, {Header: "CATEGORY", Key: "category"},
		{Header: "AMOUNT", Key: "amountText", Kind: render.KInt}, {Header: "TEMPLATE ID", Key: "templateId"},
	}

	if c.Format == render.FormatCSV {
		cols = []render.Column{
			{Header: "date", Key: "date"}, {Header: "template_id", Key: "templateId"}, {Header: "template", Key: "template"},
			{Header: "type", Key: "type"}, {Header: "source_account", Key: "sourceAccount"}, {Header: "destination_account", Key: "destinationAccount"},
			{Header: "category", Key: "category"},
			{Header: "source_amount", Key: "sourceAmount", Kind: render.KMoney, CurrencyKey: "currency"},
			{Header: "destination_amount", Key: "destinationAmount", Kind: render.KMoney, CurrencyKey: "destinationCurrency"},
		}
	}

	data := orData(env)

	if w, ok := data["window"].(map[string]any); ok {
		c.Info("window %s .. %s (%s)", orStr(w, "start"), orStr(w, "end"), orStr(w, "timezone"))
	}

	if notes, ok := data["notes"].([]any); ok {
		for _, n := range notes {
			c.Info("note: %s", orAnyString(n))
		}
	} else if note := orStr(data, "note", "cronNote"); note != "" {
		c.Info("note: %s", note)
	}

	return orEmitRows(c, env, rows, cols, "nothing scheduled in this window")
}

// ---------------------------------------------------------------------------------------------
// exchange rates
// ---------------------------------------------------------------------------------------------

var orCurrencyRe = regexp.MustCompile(`^[A-Za-z]{3}$`)

func orCurrencyArg(name, v string) (string, error) {
	v = strings.TrimSpace(v)

	if !orCurrencyRe.MatchString(v) {
		return "", app.Usage(fmt.Sprintf("%s %q is not a three-letter currency code", name, v), "e.g. USD, EUR, JPY")
	}

	return strings.ToUpper(v), nil
}

func orRunRatesList(c *app.Ctx) error {
	q := url.Values{}
	var curs []string

	for _, v := range c.Strings("currency") {
		cur, err := orCurrencyArg("--currency", v)

		if err != nil {
			return err
		}

		curs = append(curs, cur)
	}

	app.AddList(q, "currencies", curs)

	env, err := c.Call("GET", "/exchange-rates", q, nil)

	if err != nil {
		return err
	}

	data := orData(env)

	if base := orStr(data, "baseCurrency", "base"); base != "" {
		src := orStr(data, "dataSource", "provider")
		upd := orUnixDisplay(orVal(data, "updateTime"), c.Location(), "2006-01-02 15:04 MST")
		c.Info("rates are per 1 %s%s%s", base, map[bool]string{true: "", false: " from " + src}[src == ""], map[bool]string{true: "", false: ", updated " + upd}[upd == ""])
	}

	rates := orFindRows(data, "rates", "exchangeRates")
	rows := make([]map[string]any, 0, len(rates))

	for _, r := range rates {
		row := map[string]any{
			"currency": orStr(r, "currency"),
			"rate":     orStr(r, "rate"),
			"source":   orStr(r, "source"),
			"updated":  orUnixDisplay(orVal(r, "updateTime", "updatedAt"), c.Location(), "2006-01-02 15:04"),
		}

		if row["source"] == "" {
			row["source"] = "provider"
		}

		rows = append(rows, row)
	}

	if missing, ok := data["missing"].([]any); ok && len(missing) > 0 {
		c.Warnf("the app has no rate for %v", missing)
	}

	if err := orEmitRows(c, env, rows, []render.Column{
		{Header: "CURRENCY", Key: "currency"},
		{Header: "RATE", Key: "rate", Kind: render.KInt},
		{Header: "SOURCE", Key: "source"},
		{Header: "UPDATED", Key: "updated"},
	}, "no exchange rates (is [exchange_rates] data_source configured?)"); err != nil {
		return err
	}

	custom := orFindRows(map[string]any{"customRates": data["customRates"]}, "customRates")

	if c.Format != render.FormatTable || len(custom) == 0 {
		return nil
	}

	if data["customRatesActive"] != true {
		c.Info("custom rates are stored but not in use (the rate source is %s)", orNonEmpty(orStr(data, "provider", "dataSource"), "a provider"))
	}

	if _, err := c.Out.Write([]byte("\ncustom rates\n")); err != nil {
		return err
	}

	return render.Table(c.Out, custom, []render.Column{
		{Header: "CURRENCY", Key: "currency"},
		{Header: "RATE", Key: "rate", Kind: render.KInt},
		{Header: "MEANING", Key: "meaning"},
		{Header: "ACTIVE", Key: "active", Kind: render.KBool},
		{Header: "UPDATED", Key: "updatedAt"},
	})
}

func orRunRatesConvert(c *app.Ctx) error {
	amount, msg := render.ParseHundredthsArg("amount", c.Args[0])

	if msg != "" {
		return app.Usage(strings.Replace(msg, "--amount", "", 1), "ezbk rates convert 50000 EUR USD   (50000 = 500.00)")
	}

	from, err := orCurrencyArg("FROM", c.Args[1])

	if err != nil {
		return err
	}

	to, err := orCurrencyArg("TO", c.Args[2])

	if err != nil {
		return err
	}

	env, err := c.Call("POST", "/exchange-rates/convert", nil, map[string]any{"amount": amount, "from": from, "to": to})

	if err != nil {
		return err
	}

	data := orData(env)
	result := orVal(data, "converted", "convertedAmount", "result", "toAmount", "to_amount")

	if result == nil {
		if r, ok := data["result"].(map[string]any); ok {
			result = orVal(r, "amount")
		}
	}

	rate := orStr(data, "rate", "rateUsed")
	src := orStr(data, "provider", "source", "rateSource")
	when := orStr(data, "rateDate", "updatedAt")

	if rs := orFindRows(data, "rates"); when == "" && len(rs) > 0 {
		when = orStr(rs[len(rs)-1], "updatedAt")
	}

	if when == "" {
		when = orUnixDisplay(orVal(data, "updateTime", "rateUpdateTime"), c.Location(), "2006-01-02 15:04 MST")
	}

	meaning := orStr(data, "rateMeaning")

	if meaning == "" && rate != "" {
		meaning = "1 " + from + " = " + rate + " " + to
	}

	if meaning != "" || src != "" || when != "" {
		c.Info("rate: %s%s%s (the server's conversion; the CLI never multiplies)", orNonEmpty(meaning, "?"),
			map[bool]string{true: "", false: ", " + src}[src == ""], map[bool]string{true: "", false: ", updated " + when}[when == ""])
	}

	if note := orStr(data, "note"); note != "" {
		c.Info("note: %s", note)
	}

	if c.Format == render.FormatJSON {
		return c.Emit(env, render.View{})
	}

	row := map[string]any{"from": from, "to": to, "amount": amount, "converted": result, "rate": rate, "source": src}

	if c.Format == render.FormatCSV {
		return render.CSV(c.Out, []map[string]any{row}, []render.Column{
			{Header: "amount", Key: "amount", Kind: render.KMoney, Currency: from},
			{Header: "converted", Key: "converted", Kind: render.KMoney, Currency: to},
			{Header: "rate", Key: "rate"}, {Header: "source", Key: "source"},
		})
	}

	n, ok := render.ToInt64(result)

	if !ok {
		return app.Fail(exitcode.Failed, "the server's conversion answer carried no integer amount", "ezbk rates convert "+strings.Join(c.Args, " ")+" --format json")
	}

	return c.EmitText(render.Hundredths(n, true) + " " + to)
}

// ---------------------------------------------------------------------------------------------
// insights and data
// ---------------------------------------------------------------------------------------------

func orRunInsightsList(c *app.Ctx) error {
	q := url.Values{}

	if c.Bool("include-hidden") {
		q.Set("include_hidden", "true")
	}

	env, err := c.Call("GET", "/insights", q, nil)

	if err != nil {
		return err
	}

	rows := orFindRows(orData(env), "insights", "explorers")

	return orEmitRows(c, env, rows, []render.Column{
		{Header: "NAME", Key: "name"},
		{Header: "ID", Key: "id"},
		{Header: "ORDER", Key: "displayOrder", Kind: render.KInt},
		{Header: "HIDDEN", Key: "hidden", Kind: render.KBool},
	}, "no saved insights")
}

func orRunDataStats(c *app.Ctx) error {
	env, err := c.Call("GET", "/data/statistics", nil, nil)

	if err != nil {
		return err
	}

	data := orData(env)

	if inner, ok := data["statistics"].(map[string]any); ok {
		data = inner
	}

	flat := map[string]any{}

	for k, v := range data {
		key := strings.TrimSuffix(strings.TrimPrefix(k, "total"), "Count")

		if key == "" {
			key = k
		}

		flat[orSnake(key)] = v
	}

	return orEmitObject(c, env, flat, nil)
}

// orSnake turns camelCase into snake_case for display keys
func orSnake(s string) string {
	var b strings.Builder

	for i, r := range s {
		if r >= 'A' && r <= 'Z' {
			if i > 0 {
				b.WriteByte('_')
			}

			b.WriteRune(r + ('a' - 'A'))

			continue
		}

		b.WriteRune(r)
	}

	return b.String()
}

func orRunDataExport(c *app.Ctx) error {
	r, err := orRangeFromFlags(c)

	if err != nil {
		return err
	}

	q := url.Values{"format": {orExportFormat(c)}}

	if r.Start != "" {
		q.Set("start", r.Start)
	}

	if r.End != "" {
		q.Set("end", r.End)
	}

	env, err := c.Call("GET", "/data/export", q, nil)

	if err != nil {
		return err
	}

	return orEmitExport(c, env)
}

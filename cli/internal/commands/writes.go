package commands

// writes.go is the WRITING family (cli.mdx §6 WRITING, §11): every verb that changes the books.
//
// Every verb here goes through c.WriteFlow, so without --write it is a dry run that prints the
// server's own preview, and with --write it performs the two-step confirm-token protocol
// (apis.mdx §9). The CLI owns argument parsing only: names are sent as *_name for the server to
// resolve, amounts are integer hundredths parsed by c.Amount, relative dates are resolved locally
// in c.Location() and echoed on stderr, and nothing here adds, converts or compares money.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
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

const wrGroup = "WRITING"

// wrNow is the clock relative dates are resolved against (tests replace it)
var wrNow = time.Now

// ---------------------------------------------------------------------------------------------
// Registration
// ---------------------------------------------------------------------------------------------

func init() {
	app.Register(
		// transactions
		app.Verb{
			Name:    "transactions add",
			Summary: "add one transaction (or many with --from-file); a transfer takes --to-account",
			Group:   wrGroup,
			MaxArgs: 0,
			Flags:   wrTxnAddFlags(),
			Run:     wrRunTxnAdd,
		},
		app.Verb{
			Name:    "transactions edit",
			Summary: "change fields of one transaction",
			Args:    "<id>",
			MinArgs: 1,
			MaxArgs: 1,
			Group:   wrGroup,
			Flags:   wrTxnEditFlags(),
			Run:     wrRunTxnEdit,
		},
		app.Verb{
			Name:    "transactions set-category",
			Summary: "re-categorise transactions by id or by the transactions-list filters",
			Args:    "[<id>…]",
			MaxArgs: -1,
			Group:   wrGroup,
			Flags:   wrBulkFlags(wrTargetCategory),
			Run:     wrRunSetCategory,
		},
		app.Verb{
			Name:    "transactions set-account",
			Summary: "move transactions to another account by id or by the transactions-list filters",
			Args:    "[<id>…]",
			MaxArgs: -1,
			Group:   wrGroup,
			Flags:   wrBulkFlags(wrTargetAccount),
			Run:     wrRunSetAccount,
		},
		app.Verb{
			Name:    "transactions tag add",
			Summary: "add tags to transactions by id or by the transactions-list filters",
			Args:    "[<id>…]",
			MaxArgs: -1,
			Group:   wrGroup,
			Flags:   wrBulkFlags(wrTargetTags),
			Run:     func(c *app.Ctx) error { return wrRunTagBulk(c, "add") },
		},
		app.Verb{
			Name:    "transactions tag remove",
			Summary: "remove tags from transactions by id or by the transactions-list filters",
			Args:    "[<id>…]",
			MaxArgs: -1,
			Group:   wrGroup,
			Flags:   wrBulkFlags(wrTargetTags),
			Run:     func(c *app.Ctx) error { return wrRunTagBulk(c, "remove") },
		},
		app.Verb{
			Name:    "transactions tag clear",
			Summary: "remove every tag from transactions by id or by the transactions-list filters",
			Args:    "[<id>…]",
			MaxArgs: -1,
			Group:   wrGroup,
			Flags:   wrBulkFlags(wrTargetNone),
			Run:     func(c *app.Ctx) error { return wrRunTagBulk(c, "clear") },
		},

		// accounts
		app.Verb{
			Name:    "accounts add",
			Summary: "create an account (the preview shows its side and currency — neither is cheap to fix)",
			Group:   wrGroup,
			Flags:   wrAccountAddFlags(),
			Run:     wrRunAccountAdd,
		},
		app.Verb{
			Name:    "accounts edit",
			Summary: "rename or re-describe an account (currency is immutable)",
			Args:    "<id|name>",
			MinArgs: 1,
			MaxArgs: 1,
			Group:   wrGroup,
			Flags:   wrAccountEditFlags(),
			Run:     wrRunAccountEdit,
		},
		app.Verb{
			Name:    "accounts hide",
			Summary: "hide an account",
			Args:    "<id|name>",
			MinArgs: 1,
			MaxArgs: 1,
			Group:   wrGroup,
			Run:     func(c *app.Ctx) error { return wrRunHide(c, wrKindAccount, true) },
		},
		app.Verb{
			Name:    "accounts unhide",
			Summary: "show a hidden account again",
			Args:    "<id|name>",
			MinArgs: 1,
			MaxArgs: 1,
			Group:   wrGroup,
			Run:     func(c *app.Ctx) error { return wrRunHide(c, wrKindAccount, false) },
		},
		app.Verb{
			Name:    "accounts reconcile",
			Summary: "compare the app's balance to a statement's and mark the account reconciled",
			Args:    "<id|name>",
			MinArgs: 1,
			MaxArgs: 1,
			Group:   wrGroup,
			Flags: []app.Flag{
				{Name: "balance", Value: "hundredths", Help: "the statement's closing balance, integer hundredths (421108 = 4,211.08); liabilities negative"},
				{Name: "as-of", Value: "YYYY-MM-DD", Help: "the statement date (default today; also today, yesterday, last-month, …)"},
				{Name: "adjust-into", Value: "category", Help: "create ONE visible adjustment transaction for the difference in this category (id or name)"},
				{Name: "adjust-comment", Value: "text", Help: "comment on the adjustment transaction"},
				{Name: "no-mark", Help: "do not set the account's last-reconciled time"},
			},
			Run: wrRunReconcile,
		},

		// categories
		app.Verb{
			Name:    "categories add",
			Summary: "create a category (omit --parent for a primary category)",
			Group:   wrGroup,
			Flags: []app.Flag{
				{Name: "name", Value: "text", Help: "the category name (required)"},
				{Name: "type", Value: "income|expense|transfer", Help: "which side it categorises (required; a secondary inherits its parent's)"},
				{Name: "parent", Value: "id|name", Help: "the primary category this one sits under"},
				{Name: "color", Value: "hex", Help: "display colour, e.g. 1a8f3c"},
				{Name: "icon", Value: "id", Help: "upstream icon id"},
				{Name: "comment", Value: "text", Help: "free-text comment"},
			},
			Run: wrRunCategoryAdd,
		},
		app.Verb{
			Name:    "categories edit",
			Summary: "rename, re-parent or re-describe a category",
			Args:    "<id|name>",
			MinArgs: 1,
			MaxArgs: 1,
			Group:   wrGroup,
			Flags: []app.Flag{
				{Name: "name", Value: "text", Help: "new name"},
				{Name: "parent", Value: "id|name", Help: "move under this primary category"},
				{Name: "color", Value: "hex", Help: "display colour"},
				{Name: "icon", Value: "id", Help: "upstream icon id"},
				{Name: "comment", Value: "text", Help: "free-text comment (pass an empty string to clear)"},
			},
			Run: wrRunCategoryEdit,
		},
		app.Verb{
			Name:    "categories hide",
			Summary: "hide a category",
			Args:    "<id|name>",
			MinArgs: 1,
			MaxArgs: 1,
			Group:   wrGroup,
			Run:     func(c *app.Ctx) error { return wrRunHide(c, wrKindCategory, true) },
		},
		app.Verb{
			Name:    "categories unhide",
			Summary: "show a hidden category again",
			Args:    "<id|name>",
			MinArgs: 1,
			MaxArgs: 1,
			Group:   wrGroup,
			Run:     func(c *app.Ctx) error { return wrRunHide(c, wrKindCategory, false) },
		},

		// tags
		app.Verb{
			Name:    "tags add",
			Summary: "create a tag",
			Group:   wrGroup,
			Flags: []app.Flag{
				{Name: "name", Value: "text", Help: "the tag name (required)"},
				{Name: "group", Value: "id|name", Help: "the tag group it belongs to"},
			},
			Run: wrRunTagAdd,
		},
		app.Verb{
			Name:    "tags edit",
			Summary: "rename a tag or move it to another group",
			Args:    "<id|name>",
			MinArgs: 1,
			MaxArgs: 1,
			Group:   wrGroup,
			Flags: []app.Flag{
				{Name: "name", Value: "text", Help: "new name"},
				{Name: "group", Value: "id|name", Help: "move to this tag group"},
				{Name: "no-group", Help: "take it out of its tag group"},
			},
			Run: wrRunTagEdit,
		},
		app.Verb{
			Name:    "tags hide",
			Summary: "hide a tag",
			Args:    "<id|name>",
			MinArgs: 1,
			MaxArgs: 1,
			Group:   wrGroup,
			Run:     func(c *app.Ctx) error { return wrRunHide(c, wrKindTag, true) },
		},
		app.Verb{
			Name:    "tags unhide",
			Summary: "show a hidden tag again",
			Args:    "<id|name>",
			MinArgs: 1,
			MaxArgs: 1,
			Group:   wrGroup,
			Run:     func(c *app.Ctx) error { return wrRunHide(c, wrKindTag, false) },
		},

		// schedules
		app.Verb{
			Name:    "schedules add",
			Summary: "create a scheduled transaction (see what it will create with `ezbk schedules upcoming`)",
			Group:   wrGroup,
			Flags:   wrScheduleFlags(true),
			Run:     wrRunScheduleAdd,
		},
		app.Verb{
			Name:    "schedules edit",
			Summary: "change a scheduled transaction",
			Args:    "<id|name>",
			MinArgs: 1,
			MaxArgs: 1,
			Group:   wrGroup,
			Flags:   wrScheduleFlags(false),
			Run:     wrRunScheduleEdit,
		},
		app.Verb{
			Name:    "schedules pause",
			Summary: "pause a scheduled transaction (its frequency becomes disabled; it creates nothing)",
			Args:    "<id|name>",
			MinArgs: 1,
			MaxArgs: 1,
			Group:   wrGroup,
			Run:     wrRunSchedulePause,
		},
		app.Verb{
			Name:    "schedules resume",
			Summary: "resume a paused scheduled transaction (a paused schedule has no frequency: give it one)",
			Args:    "<id|name>",
			MinArgs: 1,
			MaxArgs: 1,
			Group:   wrGroup,
			Flags: []app.Flag{
				{Name: "every", Value: "daily|weekly|monthly|yearly|every-n-days", Help: "the frequency to resume with (required)"},
				{Name: "day", Value: "D", Help: "weekly: sun..sat; monthly: 1-31 or -1; yearly: MM-DD; every-n-days: N (repeatable)", Repeat: true},
				{Name: "start", Value: "YYYY-MM-DD", Help: "restart the count from this date (every-n-days)"},
			},
			Run: wrRunScheduleResume,
		},

		// rates
		app.Verb{
			Name:    "rates set",
			Summary: "set a custom exchange rate (a ratio, so the one place a decimal is typed on purpose)",
			Args:    "<CUR> <rate>",
			MinArgs: 2,
			MaxArgs: 2,
			Group:   wrGroup,
			Run:     wrRunRateSet,
		},
		app.Verb{
			Name:    "rates clear",
			Summary: "remove a custom exchange rate and go back to the provider's",
			Args:    "<CUR>",
			MinArgs: 1,
			MaxArgs: 1,
			Group:   wrGroup,
			Run:     wrRunRateClear,
		},

		// undo / redo / journal
		app.Verb{
			Name:    "undo",
			Summary: "reverse the last machine-plane write (never a browser edit); dry run without --write",
			Group:   wrGroup,
			Flags: []app.Flag{
				{Name: "journal-id", Value: "id", Help: "act only if this journal entry is next in line (default: the one the preview names)"},
			},
			Run: func(c *app.Ctx) error { return wrRunUndoRedo(c, "undo") },
		},
		app.Verb{
			Name:    "redo",
			Summary: "re-apply the last undone machine-plane write; dry run without --write",
			Group:   wrGroup,
			Flags: []app.Flag{
				{Name: "journal-id", Value: "id", Help: "act only if this journal entry is next in line (default: the one the preview names)"},
			},
			Run: func(c *app.Ctx) error { return wrRunUndoRedo(c, "redo") },
		},
		app.Verb{
			Name:    "journal",
			Summary: "recent machine-plane writes: route, when, counts, client (never amounts)",
			Group:   wrGroup,
			Flags: []app.Flag{
				{Name: "limit", Value: "N", Help: "how many entries, newest first (default 20)"},
				{Name: "offset", Value: "N", Help: "skip this many (page with the offset printed on stderr)"},
				{Name: "active-only", Help: "leave out entries that were undone"},
			},
			Run: wrRunJournal,
		},
	)
}

// ---------------------------------------------------------------------------------------------
// Wire field names — the server's request contract (apis.mdx §10), kept in one place
// ---------------------------------------------------------------------------------------------

const (
	wrPathTransactions = "/transactions"
	wrPathAccounts     = "/accounts"
	wrPathCategories   = "/categories"
	wrPathTags         = "/tags"
	wrPathTemplates    = "/templates"
	wrPathCustomRate   = "/exchange-rates/custom/"
	wrPathJournal      = "/journal"
)

// ---------------------------------------------------------------------------------------------
// Dates — relative words are resolved HERE, in the resolved timezone, and never sent (§7.5)
// ---------------------------------------------------------------------------------------------

// wrDateMode says which end of a period a relative word or a month resolves to
type wrDateMode int

const (
	wrDatePoint wrDateMode = iota // a single day: months and period words take their first day
	wrDateStart                   // the start of a range: first day
	wrDateEnd                     // the end of a range, or an "as of": last day
)

var (
	wrDayRe   = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)
	wrMonthRe = regexp.MustCompile(`^\d{4}-\d{2}$`)
	wrYearRe  = regexp.MustCompile(`^\d{4}$`)
)

// wrResolveDate turns a date argument into YYYY-MM-DD. relative reports whether the value was a
// word or a month the operator should see resolved.
func wrResolveDate(v string, mode wrDateMode, loc *time.Location, now time.Time) (string, bool, error) {
	v = strings.ToLower(strings.TrimSpace(v))

	if v == "" {
		return "", false, nil
	}

	now = now.In(loc)
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, loc)
	period := func(first, last time.Time) (string, bool, error) {
		if mode == wrDateEnd {
			return last.Format("2006-01-02"), true, nil
		}

		return first.Format("2006-01-02"), true, nil
	}
	monthFirst := time.Date(today.Year(), today.Month(), 1, 0, 0, 0, 0, loc)

	switch v {
	case "today":
		return today.Format("2006-01-02"), true, nil
	case "yesterday":
		return today.AddDate(0, 0, -1).Format("2006-01-02"), true, nil
	case "this-month", "this_month":
		return period(monthFirst, monthFirst.AddDate(0, 1, -1))
	case "last-month", "last_month":
		first := monthFirst.AddDate(0, -1, 0)
		return period(first, monthFirst.AddDate(0, 0, -1))
	case "this-year", "this_year":
		first := time.Date(today.Year(), 1, 1, 0, 0, 0, 0, loc)
		return period(first, time.Date(today.Year(), 12, 31, 0, 0, 0, 0, loc))
	case "last-year", "last_year":
		first := time.Date(today.Year()-1, 1, 1, 0, 0, 0, 0, loc)
		return period(first, time.Date(today.Year()-1, 12, 31, 0, 0, 0, 0, loc))
	}

	if wrDayRe.MatchString(v) {
		t, err := time.ParseInLocation("2006-01-02", v, loc)

		if err != nil {
			errfile.Expected("parsing a typed date", err)
			return "", false, fmt.Errorf("%q is not a real date", v)
		}

		return t.Format("2006-01-02"), false, nil
	}

	if wrYearRe.MatchString(v) {
		first, err := time.ParseInLocation("2006", v, loc)

		if err != nil {
			errfile.Expected("parsing a typed year", err)
			return "", false, fmt.Errorf("%q is not a real year", v)
		}

		return period(first, time.Date(first.Year(), 12, 31, 0, 0, 0, 0, loc))
	}

	if wrMonthRe.MatchString(v) {
		first, err := time.ParseInLocation("2006-01", v, loc)

		if err != nil {
			errfile.Expected("parsing a typed month", err)
			return "", false, fmt.Errorf("%q is not a real month", v)
		}

		return period(first, first.AddDate(0, 1, -1))
	}

	return "", false, fmt.Errorf("%q is not a date: use YYYY-MM-DD, YYYY-MM, YYYY, today, yesterday, this-month, last-month, this-year or last-year", v)
}

// wrDate reads a date flag, resolves it, and echoes a resolved relative value on stderr
func wrDate(c *app.Ctx, flag string, mode wrDateMode) (string, error) {
	raw := c.String(flag)

	if raw == "" {
		return "", nil
	}

	loc := c.Location()
	out, relative, err := wrResolveDate(raw, mode, loc, wrNow())

	if err != nil {
		return "", app.Usage("--"+flag+": "+err.Error(), "ezbk help "+c.Verb.Name)
	}

	if relative {
		c.Info("--%s %s → %s (%s)", flag, raw, out, loc.String())
	}

	return out, nil
}

var wrClockRe = regexp.MustCompile(`^([01]?\d|2[0-3]):([0-5]\d)(?::([0-5]\d))?$`)

// wrTimeOfDay validates HH:MM[:SS]
func wrTimeOfDay(v string) (string, error) {
	v = strings.TrimSpace(v)

	if v == "" {
		return "", nil
	}

	m := wrClockRe.FindStringSubmatch(v)

	if m == nil {
		return "", fmt.Errorf("%q is not a time of day (HH:MM or HH:MM:SS, 24-hour)", v)
	}

	h, _ := strconv.Atoi(m[1])
	sec := m[3]

	if sec == "" {
		sec = "00"
	}

	return fmt.Sprintf("%02d:%s:%s", h, m[2], sec), nil
}

// ---------------------------------------------------------------------------------------------
// Ids and names — ids are opaque strings; a name is sent as *_name for the server to resolve
// ---------------------------------------------------------------------------------------------

var wrDigitsRe = regexp.MustCompile(`^[0-9]+$`)

// wrRefKind classifies an id-or-name argument. ezBookkeeping ids are long decimal strings (they
// exceed 2^53), so an all-digit value of eight or more digits is an id; anything else is a name.
// "id:…" and "name:…" force the reading (a tag literally named "2024" is `name:2024`).
func wrRefKind(v string) (value string, isId bool) {
	v = strings.TrimSpace(v)

	switch {
	case strings.HasPrefix(v, "id:"):
		return strings.TrimSpace(v[3:]), true
	case strings.HasPrefix(v, "name:"):
		return strings.TrimSpace(v[5:]), false
	}

	return v, len(v) >= 8 && wrDigitsRe.MatchString(v)
}

// wrSetRef puts an id-or-name into body as <base>_id or <base>_name
func wrSetRef(body map[string]any, base, v string) {
	value, isId := wrRefKind(v)

	if isId {
		body[base+"_id"] = value
	} else {
		body[base+"_name"] = value
	}
}

// wrSetRefs puts a list of ids-or-names into body as <base>_ids and/or <base>_names
func wrSetRefs(body map[string]any, base string, values []string) {
	var ids, names []string

	for _, v := range values {
		value, isId := wrRefKind(v)

		if value == "" {
			continue
		}

		if isId {
			ids = append(ids, value)
		} else {
			names = append(names, value)
		}
	}

	if len(ids) > 0 {
		body[base+"_ids"] = ids
	}

	if len(names) > 0 {
		body[base+"_names"] = names
	}
}

// wrIds validates positional transaction ids (ids only — a bulk selection by name is a filter)
func wrIds(c *app.Ctx) ([]string, error) {
	var out []string
	seen := map[string]bool{}

	for _, a := range c.Args {
		for _, p := range strings.Split(a, ",") {
			p = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(p), "id:"))

			if p == "" {
				continue
			}

			if !wrDigitsRe.MatchString(p) {
				return nil, app.Usage(fmt.Sprintf("%q is not a transaction id (ids are the decimal strings `ezbk transactions list` prints)", p), "select by filter instead, e.g. --account A --start D --end D")
			}

			if !seen[p] {
				seen[p] = true
				out = append(out, p)
			}
		}
	}

	return out, nil
}

// wrKind is an entity family a verb addresses by <id|name>
type wrKind struct {
	Path   string // collection path, e.g. /accounts
	Rows   string // the list route's data key, e.g. accounts
	Noun   string // for messages
	ListIt string // the command that lists them
	// Query narrows the list route used to resolve a name
	Query map[string]string
}

var (
	wrKindAccount  = wrKind{Path: wrPathAccounts, Rows: "accounts", Noun: "account", ListIt: "ezbk accounts list --include-hidden"}
	wrKindCategory = wrKind{Path: wrPathCategories, Rows: "categories", Noun: "category", ListIt: "ezbk categories list --include-hidden"}
	wrKindTag      = wrKind{Path: wrPathTags, Rows: "tags", Noun: "tag", ListIt: "ezbk tags list"}
	wrKindSchedule = wrKind{Path: wrPathTemplates, Rows: "templates", Noun: "scheduled transaction", ListIt: "ezbk templates list --scheduled", Query: map[string]string{"kind": "scheduled"}}
)

// wrPathId turns an <id|name> positional into the /:id path segment. The server resolves a name
// in the path itself (exact first, then case-insensitive; ambiguity is invalid_input carrying the
// candidates, exit 2), so an id or a plain name is sent as typed. A name containing "/" cannot
// travel in a path segment; for that one case the CLI asks the collection's list route for rows
// of that name (its `name` argument) and uses the single id it returns — never a guess: zero rows
// is exit 3, several is exit 2 listing the candidates.
func wrPathId(c *app.Ctx, kind wrKind, ref string) (string, error) {
	value, isId := wrRefKind(ref)

	if value == "" {
		return "", app.Usage("an empty "+kind.Noun+" id or name", kind.ListIt)
	}

	if isId {
		return value, nil
	}

	if !strings.Contains(value, "/") {
		if wrDigitsRe.MatchString(value) {
			// a short all-digit NAME must reach the server as a name, not be read as an id
			return "name:" + value, nil
		}

		return value, nil
	}

	q := url.Values{}
	q.Set("name", value)
	q.Set("include_hidden", "true")

	for k, v := range kind.Query {
		q.Set(k, v)
	}

	env, err := c.Call("GET", kind.Path, q, nil)

	if err != nil {
		return "", err
	}

	var data map[string]any

	if err := env.Decode(&data); err != nil {
		return "", err
	}

	rows, _ := data[kind.Rows].([]any)
	matches := wrNameMatches(rows, value)

	switch len(matches) {
	case 0:
		return "", &app.ExitError{Code: exitcode.NotFound, Msg: fmt.Sprintf("no %s named %q", kind.Noun, value), Hint: kind.ListIt}
	case 1:
		id := fmt.Sprint(matches[0]["id"])
		c.Info("%s %q is id %s", kind.Noun, value, id)

		return id, nil
	}

	var cands []string

	for _, m := range matches {
		cands = append(cands, fmt.Sprintf("%s (%v)", m["id"], m["name"]))
	}

	return "", app.Usage(fmt.Sprintf("%d %s rows are named %q: %s", len(matches), kind.Noun, value, strings.Join(cands, ", ")), "pass the id instead")
}

// wrNameMatches keeps the rows (walking subAccounts / subCategories children too) whose name
// matches exactly, or — only when nothing matches exactly — case-insensitively. The server
// already filtered by name; this only guards against a server that ignored the filter.
func wrNameMatches(rows []any, name string) []map[string]any {
	var flat []map[string]any
	var walk func([]any)

	walk = func(list []any) {
		for _, r := range list {
			m, ok := r.(map[string]any)

			if !ok {
				continue
			}

			flat = append(flat, m)

			for _, k := range []string{"subAccounts", "subCategories", "children"} {
				if kids, ok := m[k].([]any); ok {
					walk(kids)
				}
			}
		}
	}

	walk(rows)

	var exact, folded []map[string]any
	seen := map[string]bool{}

	for _, m := range flat {
		n, _ := m["name"].(string)
		id := fmt.Sprint(m["id"])

		if seen[id] {
			continue
		}

		if n == name {
			exact = append(exact, m)
			seen[id] = true
		} else if strings.EqualFold(n, name) {
			folded = append(folded, m)
		}
	}

	if len(exact) > 0 {
		return exact
	}

	return folded
}

// ---------------------------------------------------------------------------------------------
// Filters — the SAME flags `transactions list` takes, sent as filter{} (apis.mdx §10.3)
// ---------------------------------------------------------------------------------------------

// wrTarget names the flag a bulk verb uses for its target, so a filter flag of the same name is
// renamed --where-<name> on that verb (set-category --category is the NEW category;
// --where-category selects by the old one)
type wrTarget string

const (
	wrTargetNone     wrTarget = ""
	wrTargetCategory wrTarget = "category"
	wrTargetAccount  wrTarget = "account"
	wrTargetTags     wrTarget = "tag"
)

// wrFilterSpec is one list filter flag
type wrFilterSpec struct {
	Flag   string
	Value  string
	Help   string
	Repeat bool
}

var wrFilterSpecs = []wrFilterSpec{
	{Flag: "account", Value: "id|name", Help: "only this account", Repeat: true},
	{Flag: "start", Value: "YYYY-MM-DD", Help: "on or after this date (also YYYY-MM, today, this-month, last-year, …)"},
	{Flag: "end", Value: "YYYY-MM-DD", Help: "on or before this date (inclusive)"},
	{Flag: "type", Value: "income|expense|transfer|balance_modification", Help: "only this transaction type"},
	{Flag: "category", Value: "id|name", Help: "only this category", Repeat: true},
	{Flag: "tag", Value: "id|name", Help: "only transactions carrying this tag", Repeat: true},
	{Flag: "tag-filter", Value: "EXPR", Help: "upstream's tag_filter expression verbatim, e.g. 1:<id>,<id> (has all)"},
	{Flag: "untagged", Help: "only transactions with no tags"},
	{Flag: "keyword", Value: "text", Help: "comment contains this text"},
	{Flag: "min", Value: "hundredths", Help: "amount at least this, integer hundredths (50000 = 500.00); needs --currency across mixed currencies"},
	{Flag: "max", Value: "hundredths", Help: "amount at most this, integer hundredths"},
	{Flag: "currency", Value: "CUR", Help: "only transactions in this currency"},
	{Flag: "with-pictures", Help: "only transactions that have pictures"},
}

// wrFilterFlagName is the flag a filter spec is declared as on a verb with this target
func wrFilterFlagName(spec wrFilterSpec, target wrTarget) string {
	if target != wrTargetNone && spec.Flag == string(target) {
		return "where-" + spec.Flag
	}

	return spec.Flag
}

// wrFilterFlags declares the filter flags for a bulk verb
func wrFilterFlags(target wrTarget) []app.Flag {
	out := make([]app.Flag, 0, len(wrFilterSpecs))

	for _, s := range wrFilterSpecs {
		name := wrFilterFlagName(s, target)
		help := "filter: " + s.Help

		if name != s.Flag {
			help = "filter: " + s.Help + " (--" + s.Flag + " is this verb's target)"
		}

		out = append(out, app.Flag{Name: name, Value: s.Value, Help: help, Repeat: s.Repeat})
	}

	return out
}

// wrTxnTypes are the transaction types a filter or a row may name, mapped to the wire value
var wrTxnTypes = map[string]string{
	"income": "income", "expense": "expense", "transfer": "transfer",
	"balance_modification": "balance_modification", "balance-modification": "balance_modification",
}

// wrBuildFilter reads the filter flags into the filter{} object, returning how many were given
func wrBuildFilter(c *app.Ctx, target wrTarget) (map[string]any, int, error) {
	f := map[string]any{}
	n := 0
	name := func(flag string) string {
		for _, s := range wrFilterSpecs {
			if s.Flag == flag {
				return wrFilterFlagName(s, target)
			}
		}

		return flag
	}

	if v := c.Strings(name("account")); len(v) > 0 {
		wrSetRefs(f, "account", v)
		n++
	}

	start, err := wrDate(c, name("start"), wrDateStart)

	if err != nil {
		return nil, 0, err
	}

	end, err := wrDate(c, name("end"), wrDateEnd)

	if err != nil {
		return nil, 0, err
	}

	if start != "" {
		f["start"] = start
		n++
	}

	if end != "" {
		f["end"] = end
		n++
	}

	if start != "" && end != "" && end < start {
		return nil, 0, app.Usage(fmt.Sprintf("--%s %s is before --%s %s", name("end"), end, name("start"), start), "swap them")
	}

	if v := c.String(name("type")); v != "" {
		t, ok := wrTxnTypes[strings.ToLower(v)]

		if !ok {
			return nil, 0, app.Usage("--"+name("type")+" must be income, expense, transfer or balance_modification", "ezbk help "+c.Verb.Name)
		}

		f["type"] = t
		n++
	}

	if v := c.Strings(name("category")); len(v) > 0 {
		wrSetRefs(f, "category", v)
		n++
	}

	tags := c.Strings(name("tag"))

	if len(tags) > 0 {
		wrSetRefs(f, "tag", tags)
		n++
	}

	expr := strings.TrimSpace(c.String(name("tag-filter")))

	if expr != "" {
		if len(tags) > 0 {
			return nil, 0, app.Usage("--"+name("tag")+" and --"+name("tag-filter")+" cannot be combined", "use --"+name("tag")+" for \"has any of these\", or write the whole expression in --"+name("tag-filter"))
		}

		f["tag_filter"] = expr
		n++
	}

	if c.Bool(name("untagged")) {
		if len(tags) > 0 || expr != "" {
			return nil, 0, app.Usage("--"+name("untagged")+" cannot be combined with --"+name("tag")+" / --"+name("tag-filter"), "pick one")
		}

		f["untagged"] = true
		n++
	}

	if v := c.String(name("keyword")); v != "" {
		f["keyword"] = v
		n++
	}

	minV, hasMin, err := c.Amount(name("min"))

	if err != nil {
		return nil, 0, err
	}

	maxV, hasMax, err := c.Amount(name("max"))

	if err != nil {
		return nil, 0, err
	}

	if hasMin {
		f["min_amount"] = minV
		n++
	}

	if hasMax {
		f["max_amount"] = maxV
		n++
	}

	if v := c.String(name("currency")); v != "" {
		cur, err := wrCurrency(v)

		if err != nil {
			return nil, 0, app.Usage("--"+name("currency")+": "+err.Error(), "")
		}

		f["currency"] = cur
		n++
	}

	if c.Bool(name("with-pictures")) {
		f["with_pictures"] = true
		n++
	}

	return f, n, nil
}

// wrSelection builds the ids[] | filter{} half of a bulk write body. Exactly one of the two:
// positional ids, or at least one filter flag — never "every transaction" by omission.
func wrSelection(c *app.Ctx, target wrTarget, body map[string]any) error {
	ids, err := wrIds(c)

	if err != nil {
		return err
	}

	filter, n, err := wrBuildFilter(c, target)

	if err != nil {
		return err
	}

	switch {
	case len(ids) > 0 && n > 0:
		return app.Usage("select by ids OR by filter flags, not both", "drop the ids, or drop the filter flags")
	case len(ids) > 0:
		body["ids"] = ids
	case n > 0:
		body["filter"] = filter
		c.Info("selecting by filter — the same filter `ezbk transactions list` takes; run it with these flags to see the rows")
	default:
		return app.Usage("no transactions selected: pass ids, or filter flags (--account, --start, --end, --category, --tag, --keyword, …)", "ezbk help "+c.Verb.Name)
	}

	return nil
}

// wrCurrency validates a three-letter currency code
func wrCurrency(v string) (string, error) {
	v = strings.ToUpper(strings.TrimSpace(v))

	if len(v) != 3 {
		return "", fmt.Errorf("%q is not a three-letter currency code (USD, EUR, JPY, …)", v)
	}

	for _, r := range v {
		if r < 'A' || r > 'Z' {
			return "", fmt.Errorf("%q is not a three-letter currency code (USD, EUR, JPY, …)", v)
		}
	}

	return v, nil
}

// ---------------------------------------------------------------------------------------------
// The write flow, and how a preview lays out for --format table|csv
// ---------------------------------------------------------------------------------------------

// wrFlow runs the write protocol through c.WriteFlow — dry run and preview without --write; with
// it, the confirm-token round trip — for every verb in this family.
//
// --format json (the default) prints the server's envelope verbatim. For table and CSV the
// previews of this family are nested (a bulk preview row carries a list of field changes), so the
// envelope WriteFlow prints is captured and laid out flat: one line per changed field, with only
// the columns some row actually uses. Nothing is computed; every value is the server's.
func wrFlow(c *app.Ctx, method, path string, body map[string]any) error {
	if key := c.String("idempotency-key"); key != "" {
		if len(key) > 128 {
			return app.Usage("--idempotency-key is at most 128 characters", "")
		}

		body["idempotency_key"] = key
	}

	if c.Format == render.FormatJSON {
		return c.WriteFlow(method, path, body, render.View{})
	}

	realOut, realFormat := c.Out, c.Format
	var buf bytes.Buffer

	c.Out, c.Format = &buf, render.FormatJSON
	err := c.WriteFlow(method, path, body, render.View{})
	c.Out, c.Format = realOut, realFormat

	if buf.Len() == 0 {
		return err
	}

	if emitErr := wrEmitFlat(c, buf.Bytes()); emitErr != nil && err == nil {
		return emitErr
	}

	return err
}

// wrEmitFlat renders a captured write envelope as a flat table/CSV
func wrEmitFlat(c *app.Ctx, raw []byte) error {
	var envelope struct {
		Data map[string]any `json:"data"`
		Meta map[string]any `json:"meta"`
	}

	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()

	if err := dec.Decode(&envelope); err != nil {
		errfile.Caught("decoding the captured write envelope", err)
		_, werr := c.Out.Write(raw)
		return werr
	}

	rows := wrFlatRows(envelope.Data)
	cols := wrFlatColumns(rows)
	env, err := wrEnvelope(map[string]any{"rows": rows}, envelope.Meta)

	if err != nil {
		return err
	}

	return c.Emit(env, render.View{Rows: "rows", Columns: cols, Empty: "nothing to show (--format json has the whole answer)"})
}

// wrFlatColumnSet is every column a flat write row may carry, in display order
var wrFlatColumnSet = []render.Column{
	{Header: "Op", Key: "op"},
	{Header: "Status", Key: "status"},
	{Header: "ID", Key: "id"},
	{Header: "Name", Key: "name"},
	{Header: "Date", Key: "date"},
	{Header: "Type", Key: "type"},
	{Header: "Account", Key: "account"},
	{Header: "Amount", Key: "amount", Kind: render.KMoney, CurrencyKey: "currency"},
	{Header: "To Account", Key: "toAccount"},
	{Header: "To Amount", Key: "toAmount", Kind: render.KMoney, CurrencyKey: "toCurrency"},
	{Header: "Category", Key: "category"},
	{Header: "Tags", Key: "tags"},
	{Header: "Comment", Key: "comment"},
	{Header: "Field", Key: "field"},
	{Header: "From", Key: "from"},
	{Header: "To", Key: "to"},
	{Header: "Meaning", Key: "meaning"},
	{Header: "Detail", Key: "detail"},
}

// wrFlatColumns keeps the columns at least one row uses (a hide preview needs five, a bulk
// re-categorisation twelve)
func wrFlatColumns(rows []map[string]any) []render.Column {
	var cols []render.Column

	for _, col := range wrFlatColumnSet {
		for _, r := range rows {
			if v, ok := r[col.Key]; ok && v != nil && v != "" {
				cols = append(cols, col)
				break
			}
		}
	}

	if len(cols) == 0 {
		cols = []render.Column{{Header: "Status", Key: "status"}}
	}

	return cols
}

// wrFlatRows turns a write answer — a dry run's preview or an apply's result — into flat rows
func wrFlatRows(data map[string]any) []map[string]any {
	if data == nil {
		return nil
	}

	applied := data["dry_run"] == false

	if !applied {
		status := "preview"

		var out []map[string]any

		if pm, ok := data["preview"].(map[string]any); ok {
			if ops, ok := pm["operations"].([]any); ok {
				// a batch: each operation's own preview, labelled with its op
				for _, o := range ops {
					op, _ := o.(map[string]any)

					for _, item := range wrPreviewItems(op["preview"]) {
						for _, row := range wrFlattenItem(item, status) {
							row["op"] = fmt.Sprintf("%v %v", op["index"], op["op"])
							out = append(out, row)
						}
					}
				}

				return out
			}
		}

		for _, item := range wrPreviewItems(data["preview"]) {
			out = append(out, wrFlattenItem(item, status)...)
		}

		if len(out) == 0 {
			out = append(out, map[string]any{"status": "no changes: " + wrDescribeCounts(data["changes"])})
		}

		return out
	}

	result, _ := data["result"].(map[string]any)
	status := "applied: " + wrDescribeCounts(data["changes"])

	if jid, ok := data["journal_id"]; ok && jid != nil {
		status += fmt.Sprintf(" (journal %v — `ezbk undo` reverses it)", jid)
	}

	if result == nil {
		return []map[string]any{{"status": status}}
	}

	ids, _ := result["ids"].([]any)

	if len(ids) == 0 {
		row := map[string]any{"status": status}

		if d, ok := result["reconciledDate"]; ok && d != nil && d != "" {
			result["reconciledAt"] = d
		}

		for _, k := range []string{"id", "name", "adjustmentId", "reconciledAt"} {
			if v, ok := result[k]; ok && v != nil {
				if k == "adjustmentId" {
					row["field"], row["to"] = "adjustment", v
				} else if k == "reconciledAt" {
					row["field"], row["to"] = "reconciled", v
				} else {
					row[k] = v
				}
			}
		}

		return []map[string]any{row}
	}

	verb := "changed"

	for _, k := range []string{"created", "updated", "deleted"} {
		if _, ok := result[k]; ok {
			verb = k
		}
	}

	out := make([]map[string]any, 0, len(ids))

	for i, id := range ids {
		row := map[string]any{"status": verb, "id": id}

		if i == 0 {
			row["status"] = verb + " — " + status
		}

		out = append(out, row)
	}

	return out
}

// wrPreviewItems finds the list of preview items, whatever shape the route used
func wrPreviewItems(p any) []map[string]any {
	var list []any

	switch t := p.(type) {
	case []any:
		list = t
	case map[string]any:
		if rows, ok := t["rows"].([]any); ok {
			list = rows
		} else {
			list = []any{t}
		}
	}

	out := make([]map[string]any, 0, len(list))

	for _, e := range list {
		if m, ok := e.(map[string]any); ok {
			out = append(out, m)
		}
	}

	return out
}

// wrFlattenItem turns one preview item into one row per field change
func wrFlattenItem(item map[string]any, status string) []map[string]any {
	base := map[string]any{"status": status}
	pick := func(dst string, keys ...string) {
		for _, k := range keys {
			v := render.Get(item, k)

			if v != nil && v != "" {
				base[dst] = v
				return
			}
		}
	}

	pick("id", "id")
	pick("name", "name")
	pick("date", "date", "asOf")
	pick("type", "typeName", "type")
	pick("account", "sourceAccountName", "account.name", "accountName")
	pick("amount", "sourceAmount", "amount")
	pick("currency", "sourceCurrency", "currency", "account.currency")
	pick("toAccount", "destinationAccountName", "destinationAccount.name")
	pick("toAmount", "destinationAmount")
	pick("toCurrency", "destinationCurrency", "destinationAccount.currency")
	pick("category", "categoryName", "category.name")
	pick("tags", "tagNames")
	pick("comment", "comment")
	pick("meaning", "meaning")

	_, hasField := item["field"]
	_, hasTo := item["to"]

	if !hasField && hasTo {
		// a single-value change (e.g. a custom rate): {from, to} with no field name
		item = map[string]any{"field": wrFirst(item["currency"], item["name"], "value"), "from": item["from"], "to": item["to"]}
		hasField = true
	}

	if hasField {
		base["field"] = item["field"]
		base["from"] = wrShown(item["from"])
		base["to"] = wrShown(item["to"])

		return []map[string]any{base}
	}

	changes, ok := item["changes"].([]any)

	if !ok {
		if item["index"] != nil || item["sourceAccountId"] != nil {
			base["status"] = "create"
		}

		if len(base) == 1 && len(item) > 0 {
			// nothing recognisable was picked (e.g. a clear-data preview {scope, counts}): show the
			// server's preview itself rather than a bare status
			base["detail"] = wrShown(item)
		}

		return []map[string]any{base}
	}

	if len(changes) == 0 {
		base["status"] = "unchanged"
		return []map[string]any{base}
	}

	out := make([]map[string]any, 0, len(changes))

	for _, ch := range changes {
		m, ok := ch.(map[string]any)

		if !ok {
			continue
		}

		row := map[string]any{}

		for k, v := range base {
			row[k] = v
		}

		row["status"] = "update"
		row["field"] = m["field"]
		row["from"] = wrShown(wrFirst(m["fromName"], m["from"]))
		row["to"] = wrShown(wrFirst(m["toName"], m["to"]))
		out = append(out, row)
	}

	return out
}

func wrFirst(vals ...any) any {
	for _, v := range vals {
		if v != nil && v != "" {
			return v
		}
	}

	return nil
}

// wrShown makes a preview value printable in one cell (lists joined, objects as compact JSON)
func wrShown(v any) any {
	switch t := v.(type) {
	case nil:
		return "—"
	case []any:
		parts := make([]string, 0, len(t))

		for _, e := range t {
			parts = append(parts, fmt.Sprint(wrShown(e)))
		}

		return "[" + strings.Join(parts, ", ") + "]"
	case map[string]any:
		if n, ok := t["name"]; ok {
			return n
		}

		data, _ := json.Marshal(t)

		return string(data)
	}

	return v
}

// wrDescribeCounts renders {"create": 1, "update": 0} as "1 create"
func wrDescribeCounts(v any) string {
	m, _ := v.(map[string]any)

	if len(m) == 0 {
		return "no changes"
	}

	keys := make([]string, 0, len(m))

	for k := range m {
		keys = append(keys, k)
	}

	sort.Strings(keys)
	var parts []string

	for _, k := range keys {
		if n, ok := render.ToInt64(m[k]); ok && n > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", n, k))
		}
	}

	if len(parts) == 0 {
		return "no changes"
	}

	return strings.Join(parts, ", ")
}

var wrViewJournal = render.View{
	Rows: "entries",
	Columns: []render.Column{
		{Header: "Journal ID", Key: "journalId"},
		{Header: "When", Key: "createdAt"},
		{Header: "Route", Key: "route"},
		{Header: "Summary", Key: "summary"},
		{Header: "Changes", Key: "changes", Kind: render.KInt},
		{Header: "Client", Key: "client"},
		{Header: "Undone", Key: "undone", Kind: render.KBool},
	},
	Empty: "the journal is empty — no machine-plane writes yet",
}

// ---------------------------------------------------------------------------------------------
// transactions add
// ---------------------------------------------------------------------------------------------

func wrTxnAddFlags() []app.Flag {
	return []app.Flag{
		{Name: "account", Value: "id|name", Help: "the account (for a transfer, the source)"},
		{Name: "type", Value: "expense|income|transfer", Help: "the transaction type (transfer is implied by --to-account)"},
		{Name: "amount", Value: "hundredths", Help: "integer hundredths in the account's currency (1250 = 12.50), always positive; --type carries the direction"},
		{Name: "category", Value: "id|name", Help: "a SECONDARY category matching the type"},
		{Name: "date", Value: "YYYY-MM-DD", Help: "the transaction date (default today; also yesterday)"},
		{Name: "time", Value: "HH:MM", Help: "time of day in the resolved timezone (default: the date at 12:00)"},
		{Name: "comment", Value: "text", Help: "free-text comment"},
		{Name: "tag", Value: "id|name", Help: "tag it (repeatable; at most 10)", Repeat: true},
		{Name: "to-account", Value: "id|name", Help: "transfer: the destination account"},
		{Name: "to-amount", Value: "hundredths", Help: "transfer: the amount that ARRIVES, in the destination's currency (required across currencies)"},
		{Name: "hide-amount", Help: "hide the amount in the app's UI"},
		{Name: "from-file", Value: "rows.json", Help: "add many: a JSON array of rows (keys as below, or the wire names); `-` reads stdin"},
		{Name: "idempotency-key", Value: "key", Help: "a retry with the same key returns the original result instead of adding twice"},
	}
}

// wrTxnRowKeys maps every key a --from-file row may use to its wire name. Friendly keys mirror
// the flags; wire keys pass through. Anything else is refused, as the server would.
var wrTxnRowKeys = map[string]string{
	"type": "type", "date": "date", "time": "time", "comment": "comment", "hide_amount": "hide_amount",
	"amount":  "amount",
	"account": "account", "account_id": "account_id", "account_name": "account_name",
	"to_account": "destination_account", "destination_account": "destination_account",
	"destination_account_id": "destination_account_id", "destination_account_name": "destination_account_name",
	"to_amount": "destination_amount", "destination_amount": "destination_amount",
	"category": "category", "category_id": "category_id", "category_name": "category_name",
	"tags": "tags", "tag_ids": "tag_ids", "tag_names": "tag_names",
}

// wrNormalizeTxnRow validates one transaction row and returns it in wire form. Amounts must be
// integer hundredths (a decimal is refused with the integer probably meant); dates may be
// relative and are resolved here; ids stay strings.
func wrNormalizeTxnRow(row map[string]any, loc *time.Location, now time.Time) (map[string]any, []string, error) {
	out := map[string]any{}
	var notes []string
	clock := ""

	keys := make([]string, 0, len(row))

	for k := range row {
		keys = append(keys, k)
	}

	sort.Strings(keys)

	for _, k := range keys {
		v := row[k]
		wire, ok := wrTxnRowKeys[strings.ToLower(strings.ReplaceAll(k, "-", "_"))]

		if !ok {
			return nil, nil, fmt.Errorf("unknown key %q (use type, date, time, account, amount, to_account, to_amount, category, tags, comment, hide_amount)", k)
		}

		switch wire {
		case "type":
			s, _ := v.(string)
			t, ok := wrTxnTypes[strings.ToLower(strings.TrimSpace(s))]

			if !ok || t == "balance_modification" {
				return nil, nil, fmt.Errorf("type must be expense, income or transfer, got %v", v)
			}

			out["type"] = t
		case "date":
			s, ok := v.(string)

			if !ok {
				return nil, nil, fmt.Errorf("date must be a YYYY-MM-DD string")
			}

			d, relative, err := wrResolveDate(s, wrDatePoint, loc, now)

			if err != nil {
				return nil, nil, fmt.Errorf("date: %v", err)
			}

			if relative {
				notes = append(notes, fmt.Sprintf("date %s → %s", s, d))
			}

			out["date"] = d
		case "time":
			s, ok := v.(string)

			if !ok {
				return nil, nil, fmt.Errorf("time must be a string: HH:MM, or an RFC 3339 instant")
			}

			if s = strings.TrimSpace(s); s == "" {
				continue
			}

			if _, err := time.Parse(time.RFC3339, s); err == nil {
				out["time"] = s
				continue
			}

			t, err := wrTimeOfDay(s)

			if err != nil {
				return nil, nil, fmt.Errorf("time: %v, or an RFC 3339 instant such as 2026-09-21T14:30:00-07:00", err)
			}

			clock = t
		case "amount", "destination_amount":
			n, err := wrRowAmount(wire, v)

			if err != nil {
				return nil, nil, err
			}

			out[wire] = n
		case "account", "destination_account", "category":
			s, err := wrRowString(k, v)

			if err != nil {
				return nil, nil, err
			}

			wrSetRef(out, wire, s)
		case "account_id", "destination_account_id", "category_id":
			s, err := wrRowString(k, v)

			if err != nil {
				return nil, nil, err
			}

			if !wrDigitsRe.MatchString(s) {
				return nil, nil, fmt.Errorf("%s must be an id string, got %q (use %s for a name)", k, s, strings.TrimSuffix(wire, "_id")+"_name")
			}

			out[wire] = s
		case "account_name", "destination_account_name", "category_name", "comment":
			s, err := wrRowString(k, v)

			if err != nil {
				return nil, nil, err
			}

			out[wire] = s
		case "tags", "tag_ids", "tag_names":
			list, err := wrRowStrings(k, v)

			if err != nil {
				return nil, nil, err
			}

			switch wire {
			case "tags":
				wrSetRefs(out, "tag", list)
			default:
				out[wire] = list
			}
		case "hide_amount":
			b, ok := v.(bool)

			if !ok {
				return nil, nil, fmt.Errorf("hide_amount must be true or false")
			}

			out["hide_amount"] = b
		}
	}

	if out["time"] != nil && out["date"] != nil {
		return nil, nil, fmt.Errorf("give date (with an optional HH:MM time) or an RFC 3339 time, not both")
	}

	if clock != "" {
		date, _ := out["date"].(string)

		if date == "" {
			date = now.In(loc).Format("2006-01-02")
		}

		instant, err := wrInstant(date, clock, loc)

		if err != nil {
			return nil, nil, err
		}

		out["time"] = instant
		delete(out, "date")
		notes = append(notes, fmt.Sprintf("%s %s in %s → %s", date, clock[:5], loc.String(), instant))
	}

	return wrCheckTxnRow(out, now.In(loc).Format("2006-01-02"), notes)
}

// wrInstant combines a YYYY-MM-DD and an HH:MM:SS into an RFC 3339 instant in loc — the form the
// plane takes for a time of day (a bare date means 12:00 there)
func wrInstant(date, clock string, loc *time.Location) (string, error) {
	t, err := time.ParseInLocation("2006-01-02 15:04:05", date+" "+clock, loc)

	if err != nil {
		errfile.Expected("parsing a typed date and time", err)
		return "", fmt.Errorf("%s %s is not a valid date and time", date, clock)
	}

	return t.Format(time.RFC3339), nil
}

// wrCheckTxnRow applies the row-shape rules shared by flags and files: required fields, the
// transfer implication, and the default date
func wrCheckTxnRow(out map[string]any, today string, notes []string) (map[string]any, []string, error) {
	hasDest := out["destination_account_id"] != nil || out["destination_account_name"] != nil

	if _, ok := out["type"]; !ok {
		if !hasDest {
			return nil, nil, fmt.Errorf("type is required (expense, income or transfer)")
		}

		out["type"] = "transfer"
	}

	typ := out["type"].(string)

	if out["account_id"] == nil && out["account_name"] == nil {
		return nil, nil, fmt.Errorf("account is required")
	}

	if _, ok := out["amount"]; !ok {
		return nil, nil, fmt.Errorf("amount is required (integer hundredths)")
	}

	if n, _ := out["amount"].(int64); n < 0 {
		return nil, nil, fmt.Errorf("amount must not be negative: the type carries the direction (an expense of 1250 is money out)")
	}

	if typ == "transfer" {
		if !hasDest {
			return nil, nil, fmt.Errorf("a transfer needs to_account (--to-account)")
		}

		if out["category_id"] == nil && out["category_name"] == nil {
			// upstream requires a transfer category too; the server names the fix if it has no default
			notes = append(notes, "no category given for the transfer; the server will use or require a transfer category")
		}
	} else {
		if hasDest || out["destination_amount"] != nil {
			return nil, nil, fmt.Errorf("to_account/to_amount only apply to a transfer (type %s)", typ)
		}

		if out["category_id"] == nil && out["category_name"] == nil {
			return nil, nil, fmt.Errorf("category is required for %s (a secondary %s category)", typ, typ)
		}
	}

	if tags, ok := out["tag_ids"].([]string); ok && len(tags)+wrLen(out["tag_names"]) > 10 {
		return nil, nil, fmt.Errorf("at most 10 tags per transaction")
	} else if tags, ok := out["tag_names"].([]string); ok && len(tags) > 10 {
		return nil, nil, fmt.Errorf("at most 10 tags per transaction")
	}

	if out["date"] == nil && out["time"] == nil {
		out["date"] = today
		notes = append(notes, "no date given → today "+today)
	}

	return out, notes, nil
}

func wrLen(v any) int {
	if s, ok := v.([]string); ok {
		return len(s)
	}

	return 0
}

// wrRowAmount accepts an integer number or an integer string of hundredths
func wrRowAmount(key string, v any) (int64, error) {
	var s string

	switch t := v.(type) {
	case json.Number:
		s = t.String()
	case string:
		s = t
	case float64:
		s = strconv.FormatFloat(t, 'f', -1, 64)
	default:
		return 0, fmt.Errorf("%s must be an integer of hundredths", key)
	}

	if strings.ContainsAny(s, "eE") {
		return 0, fmt.Errorf("%s %s: write amounts as plain integers of hundredths", key, s)
	}

	n, msg := render.ParseHundredthsArg(key, s)

	if msg != "" {
		return 0, fmt.Errorf("%s", strings.Replace(msg, "did you mean "+key+" ", "did you mean \""+key+"\": ", 1))
	}

	return n, nil
}

func wrRowString(key string, v any) (string, error) {
	switch t := v.(type) {
	case string:
		return strings.TrimSpace(t), nil
	case json.Number:
		// an id written as a JSON number has been through a float somewhere; refuse it rather
		// than trust digits that may already be rounded (apis.mdx §17.5)
		return "", fmt.Errorf("%s must be a string (ids are strings: \"%s\")", key, t.String())
	}

	return "", fmt.Errorf("%s must be a string", key)
}

func wrRowStrings(key string, v any) ([]string, error) {
	switch t := v.(type) {
	case string:
		var out []string

		for _, p := range strings.Split(t, ",") {
			if p = strings.TrimSpace(p); p != "" {
				out = append(out, p)
			}
		}

		return out, nil
	case []any:
		out := make([]string, 0, len(t))

		for _, e := range t {
			s, err := wrRowString(key, e)

			if err != nil {
				return nil, err
			}

			if s != "" {
				out = append(out, s)
			}
		}

		return out, nil
	}

	return nil, fmt.Errorf("%s must be a list of strings", key)
}

// wrReadRowsFile reads --from-file: a JSON array of rows, or {"transactions": [...]}
func wrReadRowsFile(path string) ([]map[string]any, error) {
	var data []byte
	var err error

	if path == "-" {
		data, err = wrReadAllStdin()
	} else {
		data, err = os.ReadFile(path)
	}

	if err != nil {
		return nil, err
	}

	return wrParseRows(data)
}

var wrReadAllStdin = func() ([]byte, error) {
	var buf bytes.Buffer
	_, err := buf.ReadFrom(os.Stdin)

	return buf.Bytes(), err
}

func wrParseRows(data []byte) ([]map[string]any, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()

	var top any

	if err := dec.Decode(&top); err != nil {
		return nil, fmt.Errorf("not valid JSON: %v", err)
	}

	var list []any

	switch t := top.(type) {
	case []any:
		list = t
	case map[string]any:
		inner, ok := t["transactions"].([]any)

		if !ok {
			return nil, fmt.Errorf(`expected a JSON array of rows, or {"transactions": [ … ]}`)
		}

		list = inner
	default:
		return nil, fmt.Errorf(`expected a JSON array of rows, or {"transactions": [ … ]}`)
	}

	rows := make([]map[string]any, 0, len(list))

	for i, e := range list {
		m, ok := e.(map[string]any)

		if !ok {
			return nil, fmt.Errorf("row %d is not a JSON object", i+1)
		}

		rows = append(rows, m)
	}

	if len(rows) == 0 {
		return nil, fmt.Errorf("the file holds no rows")
	}

	return rows, nil
}

// wrTxnRowFromFlags builds the friendly row a single `transactions add` describes
func wrTxnRowFromFlags(c *app.Ctx) (map[string]any, error) {
	row := map[string]any{}

	if v := c.String("type"); v != "" {
		row["type"] = v
	}

	if v := c.String("account"); v != "" {
		row["account"] = v
	}

	if v := c.String("to-account"); v != "" {
		row["to_account"] = v
	}

	if v := c.String("category"); v != "" {
		row["category"] = v
	}

	if v := c.String("comment"); v != "" {
		row["comment"] = v
	}

	if v := c.String("time"); v != "" {
		row["time"] = v
	}

	if tags := c.Strings("tag"); len(tags) > 0 {
		list := make([]any, len(tags))

		for i, t := range tags {
			list[i] = t
		}

		row["tags"] = list
	}

	if c.Bool("hide-amount") {
		row["hide_amount"] = true
	}

	amount, has, err := c.Amount("amount")

	if err != nil {
		return nil, err
	}

	if has {
		row["amount"] = json.Number(strconv.FormatInt(amount, 10))
	}

	toAmount, has, err := c.Amount("to-amount")

	if err != nil {
		return nil, err
	}

	if has {
		row["to_amount"] = json.Number(strconv.FormatInt(toAmount, 10))
	}

	date, err := wrDate(c, "date", wrDatePoint)

	if err != nil {
		return nil, err
	}

	if date != "" {
		row["date"] = date
	}

	return row, nil
}

func wrRunTxnAdd(c *app.Ctx) error {
	loc := c.Location()
	now := wrNow()
	var rows []map[string]any

	if path := c.String("from-file"); path != "" {
		for _, f := range []string{"account", "type", "amount", "category", "date", "time", "comment", "tag", "to-account", "to-amount", "hide-amount"} {
			if c.Has(f) || c.Bool(f) {
				return app.Usage("--from-file carries every field; drop --"+f, "put the field in each row of the file")
			}
		}

		raw, err := wrReadRowsFile(path)

		if err != nil {
			return app.Usage("--from-file "+path+": "+err.Error(), "the file is a JSON array of {type, date, account, amount, category, …} rows")
		}

		for i, r := range raw {
			norm, notes, err := wrNormalizeTxnRow(r, loc, now)

			if err != nil {
				return app.Usage(fmt.Sprintf("--from-file %s row %d: %v", path, i+1, err), "fix that row; nothing was sent")
			}

			for _, n := range notes {
				c.Info("row %d: %s", i+1, n)
			}

			rows = append(rows, norm)
		}

		c.Info("%d rows read from %s", len(rows), path)
	} else {
		row, err := wrTxnRowFromFlags(c)

		if err != nil {
			return err
		}

		norm, notes, err := wrNormalizeTxnRow(row, loc, now)

		if err != nil {
			return app.Usage(wrFlagWording(err.Error()), "ezbk help transactions add")
		}

		for _, n := range notes {
			c.Info("%s", n)
		}

		rows = append(rows, norm)
	}

	list := make([]any, len(rows))

	for i, r := range rows {
		list[i] = r
	}

	return wrFlow(c, "POST", wrPathTransactions, map[string]any{"transactions": list})
}

// wrFlagWording rewrites a row-key message into flag wording for the single-row form
func wrFlagWording(s string) string {
	r := strings.NewReplacer("to_account", "--to-account", "to_amount", "--to-amount", "type is", "--type is", "account is", "--account is", "amount is", "--amount is", "category is", "--category is", "amount must", "--amount must")

	return r.Replace(s)
}

// ---------------------------------------------------------------------------------------------
// transactions edit
// ---------------------------------------------------------------------------------------------

func wrTxnEditFlags() []app.Flag {
	return []app.Flag{
		{Name: "amount", Value: "hundredths", Help: "new amount, integer hundredths (1250 = 12.50)"},
		{Name: "to-amount", Value: "hundredths", Help: "transfer: new destination amount"},
		{Name: "account", Value: "id|name", Help: "move to this account"},
		{Name: "to-account", Value: "id|name", Help: "transfer: new destination account"},
		{Name: "category", Value: "id|name", Help: "new secondary category (must match the type)"},
		{Name: "date", Value: "YYYY-MM-DD", Help: "new date (also today, yesterday)"},
		{Name: "time", Value: "HH:MM", Help: "new time of day (on --date, else on the transaction's own date)"},
		{Name: "comment", Value: "text", Help: "new comment (--comment \"\" clears it)"},
		{Name: "tag", Value: "id|name", Help: "REPLACE the tags with these (repeatable); see `transactions tag add` to add", Repeat: true},
		{Name: "clear-tags", Help: "remove every tag"},
		{Name: "hide-amount", Help: "hide the amount in the app's UI"},
		{Name: "show-amount", Help: "stop hiding the amount"},
	}
}

func wrRunTxnEdit(c *app.Ctx) error {
	ids, err := wrIds(c)

	if err != nil {
		return err
	}

	if len(ids) != 1 {
		return app.Usage("`ezbk transactions edit` takes exactly one id", "for many rows use set-category, set-account or tag")
	}

	body := map[string]any{}

	if amount, has, err := c.Amount("amount"); err != nil {
		return err
	} else if has {
		if amount < 0 {
			return app.Usage("--amount must not be negative: the type carries the direction", "")
		}

		body["amount"] = amount
	}

	if amount, has, err := c.Amount("to-amount"); err != nil {
		return err
	} else if has {
		body["destination_amount"] = amount
	}

	if v := c.String("account"); v != "" {
		wrSetRef(body, "account", v)
	}

	if v := c.String("to-account"); v != "" {
		wrSetRef(body, "destination_account", v)
	}

	if v := c.String("category"); v != "" {
		wrSetRef(body, "category", v)
	}

	date, err := wrDate(c, "date", wrDatePoint)

	if err != nil {
		return err
	}

	if v := c.String("time"); v != "" {
		clock, err := wrTimeOfDay(v)

		if err != nil {
			return app.Usage("--time: "+err.Error(), "")
		}

		if date == "" {
			// the time of day moves within the transaction's own date, which the server reports
			env, err := c.Call("GET", wrPathTransactions+"/"+url.PathEscape(ids[0]), nil, nil)

			if err != nil {
				return err
			}

			date, _ = render.Get(env.DataMap(), "transaction.date").(string)

			if date == "" {
				return app.Usage("--time needs --date: the server did not report this transaction's date", "pass --date YYYY-MM-DD too")
			}
		}

		instant, err := wrInstant(date, clock, c.Location())

		if err != nil {
			return app.Usage("--time: "+err.Error(), "")
		}

		body["time"] = instant
		c.Info("%s %s in %s → %s", date, clock[:5], c.Location().String(), instant)
	} else if date != "" {
		body["date"] = date
	}

	if c.Has("comment") {
		body["comment"] = c.String("comment")
	}

	tags := c.Strings("tag")

	switch {
	case len(tags) > 0 && c.Bool("clear-tags"):
		return app.Usage("--tag and --clear-tags contradict each other", "pick one")
	case len(tags) > 10:
		return app.Usage("at most 10 tags per transaction", "")
	case len(tags) > 0:
		wrSetRefs(body, "tag", tags)
	case c.Bool("clear-tags"):
		body["tag_ids"] = []string{}
	}

	switch {
	case c.Bool("hide-amount") && c.Bool("show-amount"):
		return app.Usage("--hide-amount and --show-amount contradict each other", "pick one")
	case c.Bool("hide-amount"):
		body["hide_amount"] = true
	case c.Bool("show-amount"):
		body["hide_amount"] = false
	}

	if len(body) == 0 {
		return app.Usage("nothing to change: pass at least one of --amount, --category, --date, --comment, --tag, …", "ezbk help transactions edit")
	}

	return wrFlow(c, "PATCH", wrPathTransactions+"/"+url.PathEscape(ids[0]), body)
}

// ---------------------------------------------------------------------------------------------
// Bulk: set-category, set-account, tag add|remove|clear
// ---------------------------------------------------------------------------------------------

func wrBulkFlags(target wrTarget) []app.Flag {
	var flags []app.Flag

	switch target {
	case wrTargetCategory:
		flags = append(flags, app.Flag{Name: "category", Value: "id|name", Help: "the NEW secondary category (required)"})
	case wrTargetAccount:
		flags = append(flags,
			app.Flag{Name: "account", Value: "id|name", Help: "the account to MOVE them to (required; same currency)"},
			app.Flag{Name: "side", Value: "source|destination", Help: "which side of a transfer moves (default source)"},
		)
	case wrTargetTags:
		flags = append(flags, app.Flag{Name: "tag", Value: "id|name", Help: "the tag(s) to add or remove (required, repeatable)", Repeat: true})
	}

	return append(flags, wrFilterFlags(target)...)
}

func wrRunSetCategory(c *app.Ctx) error {
	target := c.String("category")

	if target == "" {
		return app.Usage("--category is required: the category to set", "select the rows with ids or --where-category / --account / --start …")
	}

	body := map[string]any{}

	if err := wrSelection(c, wrTargetCategory, body); err != nil {
		return err
	}

	wrSetRef(body, "category", target)

	return wrFlow(c, "POST", wrPathTransactions+"/set-category", body)
}

func wrRunSetAccount(c *app.Ctx) error {
	target := c.String("account")

	if target == "" {
		return app.Usage("--account is required: the account to move them to", "select the rows with ids or --where-account / --start …")
	}

	body := map[string]any{}

	if err := wrSelection(c, wrTargetAccount, body); err != nil {
		return err
	}

	wrSetRef(body, "account", target)

	if side := strings.ToLower(strings.TrimSpace(c.String("side"))); side != "" {
		if side != "source" && side != "destination" {
			return app.Usage("--side is source or destination", "")
		}

		body["side"] = side
	}

	return wrFlow(c, "POST", wrPathTransactions+"/set-account", body)
}

func wrRunTagBulk(c *app.Ctx, op string) error {
	body := map[string]any{}
	target := wrTargetTags

	if op == "clear" {
		target = wrTargetNone
	} else {
		tags := c.Strings("tag")

		if len(tags) == 0 {
			return app.Usage("--tag is required: the tag(s) to "+op, "select the rows with ids or --where-tag / --account / --start …")
		}

		wrSetRefs(body, "tag", tags)
	}

	if err := wrSelection(c, target, body); err != nil {
		return err
	}

	return wrFlow(c, "POST", wrPathTransactions+"/tags/"+op, body)
}

// ---------------------------------------------------------------------------------------------
// accounts add / edit / hide / reconcile
// ---------------------------------------------------------------------------------------------

// wrAccountCategoryNames is the --category vocabulary, for help and hints. The server validates
// it and decides the side of the books each category sits on; the CLI only prints its verdict.
const wrAccountCategoryNames = "cash|checking|savings|credit_card|virtual|debt|receivables|investment|certificate_of_deposit"

func wrAccountAddFlags() []app.Flag {
	return []app.Flag{
		{Name: "name", Value: "text", Help: "the account name (required)"},
		{Name: "category", Value: wrAccountCategoryNames, Help: "the account category (required); credit_card and debt are liabilities"},
		{Name: "currency", Value: "CUR", Help: "the account's currency (required; immutable once created)"},
		{Name: "initial-balance", Value: "hundredths", Help: "opening balance, integer hundredths; negative for money owed on a liability"},
		{Name: "initial-date", Value: "YYYY-MM-DD", Help: "date of the opening balance (default today)"},
		{Name: "color", Value: "hex", Help: "display colour, 6 hex digits"},
		{Name: "icon", Value: "id", Help: "upstream icon id"},
		{Name: "comment", Value: "text", Help: "free-text comment"},
		{Name: "statement-date", Value: "1-28", Help: "credit card: the statement day of the month"},
		{Name: "credit-limit", Value: "hundredths", Help: "credit card: the limit, integer hundredths"},
		{Name: "sub-account", Value: "name[:CUR[:hundredths]]", Help: "create as a parent with this sub-account (repeatable); a parent holds no balance itself", Repeat: true},
		{Name: "idempotency-key", Value: "key", Help: "a retry with the same key returns the original result"},
	}
}

// wrParseSubAccount parses name[:CUR[:hundredths]]
func wrParseSubAccount(v string) (map[string]any, error) {
	parts := strings.Split(v, ":")

	if len(parts) > 3 || strings.TrimSpace(parts[0]) == "" {
		return nil, fmt.Errorf("--sub-account %q: use name[:CUR[:hundredths]]", v)
	}

	sub := map[string]any{"name": strings.TrimSpace(parts[0])}

	if len(parts) > 1 && strings.TrimSpace(parts[1]) != "" {
		cur, err := wrCurrency(parts[1])

		if err != nil {
			return nil, fmt.Errorf("--sub-account %q: %v", v, err)
		}

		sub["currency"] = cur
	}

	if len(parts) > 2 && strings.TrimSpace(parts[2]) != "" {
		n, msg := render.ParseHundredthsArg("--sub-account balance", parts[2])

		if msg != "" {
			return nil, fmt.Errorf("--sub-account %q: %s", v, msg)
		}

		sub["initial_balance"] = n
	}

	return sub, nil
}

func wrRunAccountAdd(c *app.Ctx) error {
	name := strings.TrimSpace(c.String("name"))
	category := strings.ToLower(strings.TrimSpace(strings.NewReplacer("-", "_", " ", "_").Replace(c.String("category"))))

	if name == "" {
		return app.Usage("--name is required", "ezbk help accounts add")
	}

	if category == "" {
		return app.Usage("--category is required: one of "+strings.ReplaceAll(wrAccountCategoryNames, "|", ", "), "ezbk help accounts add")
	}

	body := map[string]any{"name": name, "category": category}
	subs := c.Strings("sub-account")

	if c.String("currency") == "" && len(subs) == 0 {
		return app.Usage("--currency is required (it cannot be changed after creation)", "ezbk help accounts add")
	}

	if v := c.String("currency"); v != "" {
		cur, err := wrCurrency(v)

		if err != nil {
			return app.Usage("--currency: "+err.Error(), "")
		}

		body["currency"] = cur
	}

	balance, hasBalance, err := c.Amount("initial-balance")

	if err != nil {
		return err
	}

	if hasBalance {
		if len(subs) > 0 {
			return app.Usage("a parent account holds no balance; give each --sub-account its own", "--sub-account \"Name:USD:125000\"")
		}

		body["initial_balance"] = balance
	}

	date, err := wrDate(c, "initial-date", wrDatePoint)

	if err != nil {
		return err
	}

	if date != "" {
		body["initial_balance_date"] = date
	}

	for _, k := range []struct{ flag, key string }{{"color", "color"}, {"icon", "icon"}, {"comment", "comment"}} {
		if c.Has(k.flag) {
			body[k.key] = c.String(k.flag)
		}
	}

	if c.Has("statement-date") {
		n, err := c.Int("statement-date", 0)

		if err != nil {
			return err
		}

		if n < 1 || n > 28 {
			return app.Usage("--statement-date is a day of the month, 1 to 28", "")
		}

		body["credit_card_statement_date"] = n
	}

	if limit, has, err := c.Amount("credit-limit"); err != nil {
		return err
	} else if has {
		if limit < 0 {
			return app.Usage("--credit-limit must not be negative", "")
		}

		body["credit_card_limit"] = limit
	}

	if len(subs) > 0 {
		list := make([]any, 0, len(subs))

		for _, s := range subs {
			sub, err := wrParseSubAccount(s)

			if err != nil {
				return app.Usage(err.Error(), "")
			}

			if sub["initial_balance"] != nil && date != "" {
				sub["initial_balance_date"] = date
			}

			list = append(list, sub)
		}

		body["sub_accounts"] = list
	}

	// the side of the books and the currency are neither cheap to fix: print the SERVER's
	// verdict on both before anything is written (cli.mdx §11)
	peek := map[string]any{"dry_run": true}

	for k, v := range body {
		peek[k] = v
	}

	env, err := c.Call("POST", wrPathAccounts, nil, peek)

	if err != nil {
		return err
	}

	if preview, ok := env.DataMap()["preview"].(map[string]any); ok {
		c.Info("%s", wrAccountVerdict(preview, balance, hasBalance))
	}

	return wrFlow(c, "POST", wrPathAccounts, body)
}

// wrAccountVerdict is the stderr line naming what accounts add will create
func wrAccountVerdict(preview map[string]any, balance int64, hasBalance bool) string {
	side, _ := preview["side"].(string)
	cur, _ := preview["currency"].(string)

	if cur == "" {
		cur = "per sub-account"
	}

	line := fmt.Sprintf("account %q: category %v — the %s side of the books; currency %s (immutable once created)", preview["name"], preview["category"], strings.ToUpper(wrFirst(side, "?").(string)), cur)

	if subs, ok := preview["subAccounts"].([]any); ok && len(subs) > 0 {
		line += fmt.Sprintf("; a parent with %d sub-accounts", len(subs))
	}

	if hasBalance {
		line += "; opening balance " + render.Money(balance, cur)

		if strings.EqualFold(side, "liability") && balance > 0 {
			line += fmt.Sprintf(" — POSITIVE on a liability means the account is in credit; money OWED is negative (--initial-balance -%d)", balance)
		}
	}

	return line
}

func wrAccountEditFlags() []app.Flag {
	return []app.Flag{
		{Name: "name", Value: "text", Help: "new name"},
		{Name: "comment", Value: "text", Help: "new comment (--comment \"\" clears it)"},
		{Name: "color", Value: "hex", Help: "display colour"},
		{Name: "icon", Value: "id", Help: "upstream icon id"},
		{Name: "statement-date", Value: "1-28", Help: "credit card: the statement day of the month"},
		{Name: "credit-limit", Value: "hundredths", Help: "credit card: the limit, integer hundredths"},
		{Name: "currency", Value: "CUR", Help: "refused: currency is immutable (a new account plus a transfer is the honest move)"},
	}
}

func wrRunAccountEdit(c *app.Ctx) error {
	if c.Has("currency") {
		return app.Usage("an account's currency cannot be changed after creation", "create a new account in the new currency and transfer the balance: ezbk accounts add … && ezbk transactions add --type transfer …")
	}

	body := map[string]any{}

	for _, k := range []struct{ flag, key string }{{"name", "name"}, {"comment", "comment"}, {"color", "color"}, {"icon", "icon"}} {
		if c.Has(k.flag) {
			body[k.key] = c.String(k.flag)
		}
	}

	if v, ok := body["name"].(string); ok && strings.TrimSpace(v) == "" {
		return app.Usage("--name cannot be empty", "")
	}

	if c.Has("statement-date") {
		n, err := c.Int("statement-date", 0)

		if err != nil {
			return err
		}

		if n < 1 || n > 28 {
			return app.Usage("--statement-date is a day of the month, 1 to 28", "")
		}

		body["credit_card_statement_date"] = n
	}

	if limit, has, err := c.Amount("credit-limit"); err != nil {
		return err
	} else if has {
		if limit < 0 {
			return app.Usage("--credit-limit must not be negative", "")
		}

		body["credit_card_limit"] = limit
	}

	if len(body) == 0 {
		return app.Usage("nothing to change: pass --name, --comment, --color, --icon, --statement-date or --credit-limit", "ezbk help accounts edit")
	}

	id, err := wrPathId(c, wrKindAccount, c.Args[0])

	if err != nil {
		return err
	}

	return wrFlow(c, "PATCH", wrPathAccounts+"/"+url.PathEscape(id), body)
}

// wrRunHide hides/unhides an account, category or tag, or pauses/resumes a schedule
func wrRunHide(c *app.Ctx, kind wrKind, hidden bool) error {
	id, err := wrPathId(c, kind, c.Args[0])

	if err != nil {
		return err
	}

	return wrFlow(c, "POST", kind.Path+"/"+url.PathEscape(id)+"/hide", map[string]any{"hidden": hidden})
}

// wrReconcileView lays out the reconcile plan: the rows since the last reconciliation, which is
// where a missing transaction is looked for
var wrReconcileView = render.View{
	Rows: "transactionsSinceLastReconciled",
	Columns: []render.Column{
		{Header: "ID", Key: "id"},
		{Header: "Date", Key: "date"},
		{Header: "Type", Key: "typeName"},
		{Header: "Amount", Key: "sourceAmount", Kind: render.KMoney, CurrencyKey: "sourceCurrency"},
		{Header: "To Amount", Key: "destinationAmount", Kind: render.KMoney, CurrencyKey: "destinationCurrency"},
		{Header: "Category", Key: "categoryName"},
		{Header: "Comment", Key: "comment"},
	},
	Empty: "no transactions since the last reconciliation",
}

// wrReconcileArgs builds the arguments plan and apply share (the server's token covers exactly
// the change set these describe)
func wrReconcileArgs(c *app.Ctx) (map[string]any, error) {
	balance, has, err := c.Amount("balance")

	if err != nil {
		return nil, err
	}

	if !has {
		return nil, app.Usage("--balance is required: the statement's closing balance in integer hundredths (a liability owed is negative)", "ezbk accounts reconcile <acct> --balance 421108 --as-of 2026-08-31")
	}

	asOf, err := wrDate(c, "as-of", wrDateEnd)

	if err != nil {
		return nil, err
	}

	if asOf == "" {
		asOf = wrNow().In(c.Location()).Format("2006-01-02")
		c.Info("--as-of not given → today %s (%s)", asOf, c.Location().String())
	}

	args := map[string]any{"target_balance": balance, "as_of": asOf}

	if cat := c.String("adjust-into"); cat != "" {
		args["create_adjustment"] = true
		wrSetRef(args, "adjustment_category", cat)

		if c.Has("adjust-comment") {
			args["adjustment_comment"] = c.String("adjust-comment")
		}
	} else if c.Has("adjust-comment") {
		return nil, app.Usage("--adjust-comment needs --adjust-into", "")
	}

	if c.Bool("no-mark") {
		// without --write a --no-mark plan is a pure comparison; with --write it must change something
		if args["create_adjustment"] == nil && c.Bool("write") {
			return nil, app.Usage("--no-mark with nothing to adjust leaves nothing to write", "drop --no-mark, or pass --adjust-into")
		}

		args["mark_reconciled"] = false
	}

	return args, nil
}

func wrRunReconcile(c *app.Ctx) error {
	args, err := wrReconcileArgs(c)

	if err != nil {
		return err
	}

	id, err := wrPathId(c, wrKindAccount, c.Args[0])

	if err != nil {
		return err
	}

	base := wrPathAccounts + "/" + url.PathEscape(id)

	// the plan is a read: the app's balance as of the date, the operator's figure, the signed
	// difference and the rows since the last reconciliation — all computed by the server
	planBody := map[string]any{}

	for k, v := range args {
		planBody[k] = v
	}

	env, err := c.Call("POST", base+"/reconcile/plan", nil, planBody)

	if err != nil {
		return err
	}

	plan := env.DataMap()
	diff, diffKnown := wrReconcileDifference(plan)
	c.Info("%s", wrReconcileSummary(plan))

	if since, ok := render.ToInt64(plan["sinceCount"]); ok {
		c.Info("%d transactions since the last reconciliation (%v)", since, wrFirst(plan["lastReconciledDate"], "never reconciled"))
	}

	if w, ok := plan["adjustmentSkipped"].(string); ok {
		c.Info("%s", w)
	}

	if w, ok := plan["markSkipped"].(string); ok {
		c.Info("%s", w)
	}

	adjusting := args["create_adjustment"] == true

	if !c.Bool("write") {
		if diffKnown && diff != 0 && !adjusting {
			c.Info("the difference is not zero: find the missing transaction first (the rows since the last reconciliation are listed), or pass --adjust-into <category>")
		}

		c.Info("DRY RUN — nothing was changed. Re-run with --write to apply.")

		return c.Emit(env, wrReconcileView)
	}

	if !diffKnown {
		return app.Fail(exitcode.Failed, "the server's reconcile plan did not report a difference", "ezbk accounts reconcile … --format json shows the plan; the server may be older than this CLI")
	}

	if diff != 0 && !adjusting {
		return &app.ExitError{
			Code: exitcode.Conflict,
			Msg:  "refused: the app's balance and the statement's differ, so the account was NOT marked reconciled",
			Hint: "find the missing transaction first, or pass --adjust-into <category>",
		}
	}

	if blocked, ok := plan["blockedReason"].(string); ok && blocked != "" {
		return &app.ExitError{Code: exitcode.Conflict, Msg: "refused: " + blocked, Hint: "find the missing transaction first, or pass --adjust-into <category>"}
	}

	return wrFlow(c, "POST", base+"/reconcile/apply", args)
}

// wrReconcileDifference reads the signed difference (statement − app) the server computed
func wrReconcileDifference(plan map[string]any) (int64, bool) {
	for _, k := range []string{"difference", "diff"} {
		if n, ok := render.ToInt64(plan[k]); ok {
			return n, true
		}
	}

	return 0, false
}

// wrReconcileSummary is the one stderr line: the app's balance, the operator's, the difference
func wrReconcileSummary(plan map[string]any) string {
	cur, _ := plan["currency"].(string)
	money := func(keys ...string) string {
		for _, k := range keys {
			if n, ok := render.ToInt64(plan[k]); ok {
				return render.Money(n, cur)
			}
		}

		return "—"
	}

	asOf, _ := plan["asOf"].(string)

	return fmt.Sprintf("as of %s: app balance %s   statement %s   difference %s", wrFirst(asOf, "?"), money("appBalance", "app_balance"), money("targetBalance", "target_balance"), money("difference", "diff"))
}

// ---------------------------------------------------------------------------------------------
// categories / tags
// ---------------------------------------------------------------------------------------------

func wrRunCategoryAdd(c *app.Ctx) error {
	name := strings.TrimSpace(c.String("name"))

	if name == "" {
		return app.Usage("--name is required", "ezbk help categories add")
	}

	body := map[string]any{"name": name}

	if v := c.String("type"); v != "" {
		t := strings.ToLower(v)

		if t != "income" && t != "expense" && t != "transfer" {
			return app.Usage("--type must be income, expense or transfer", "")
		}

		body["type"] = t
	} else if !c.Has("parent") {
		return app.Usage("--type is required for a primary category (income, expense or transfer)", "a secondary category (--parent) takes its parent's type")
	}

	if v := c.String("parent"); v != "" {
		wrSetRef(body, "parent", v)
	}

	for _, k := range []string{"color", "icon", "comment"} {
		if c.Has(k) {
			body[k] = c.String(k)
		}
	}

	if !c.Has("parent") {
		c.Info("%q will be a PRIMARY category: transactions use secondary categories, so add one under it with --parent", name)
	}

	return wrFlow(c, "POST", wrPathCategories, body)
}

func wrRunCategoryEdit(c *app.Ctx) error {
	body := map[string]any{}

	if c.Has("name") {
		if strings.TrimSpace(c.String("name")) == "" {
			return app.Usage("--name cannot be empty", "")
		}

		body["name"] = c.String("name")
	}

	if v := c.String("parent"); v != "" {
		wrSetRef(body, "parent", v)
	}

	for _, k := range []string{"color", "icon", "comment"} {
		if c.Has(k) {
			body[k] = c.String(k)
		}
	}

	if len(body) == 0 {
		return app.Usage("nothing to change: pass --name, --parent, --color, --icon or --comment", "ezbk help categories edit")
	}

	id, err := wrPathId(c, wrKindCategory, c.Args[0])

	if err != nil {
		return err
	}

	return wrFlow(c, "PATCH", wrPathCategories+"/"+url.PathEscape(id), body)
}

func wrRunTagAdd(c *app.Ctx) error {
	name := strings.TrimSpace(c.String("name"))

	if name == "" {
		return app.Usage("--name is required", "ezbk help tags add")
	}

	body := map[string]any{"name": name}

	if v := c.String("group"); v != "" {
		wrSetRef(body, "group", v)
	}

	return wrFlow(c, "POST", wrPathTags, body)
}

func wrRunTagEdit(c *app.Ctx) error {
	body := map[string]any{}

	if c.Has("name") {
		if strings.TrimSpace(c.String("name")) == "" {
			return app.Usage("--name cannot be empty", "")
		}

		body["name"] = c.String("name")
	}

	if v := c.String("group"); v != "" {
		if c.Bool("no-group") {
			return app.Usage("--group and --no-group contradict each other", "pick one")
		}

		wrSetRef(body, "group", v)
	} else if c.Bool("no-group") {
		body["group_id"] = "0"
	}

	if len(body) == 0 {
		return app.Usage("nothing to change: pass --name, --group or --no-group", "ezbk help tags edit")
	}

	id, err := wrPathId(c, wrKindTag, c.Args[0])

	if err != nil {
		return err
	}

	return wrFlow(c, "PATCH", wrPathTags+"/"+url.PathEscape(id), body)
}

// ---------------------------------------------------------------------------------------------
// schedules
// ---------------------------------------------------------------------------------------------

func wrScheduleFlags(add bool) []app.Flag {
	req := ""

	if add {
		req = " (required)"
	}

	flags := []app.Flag{
		{Name: "name", Value: "text", Help: "the schedule's name" + req},
		{Name: "every", Value: "daily|weekly|monthly|yearly|every-n-days", Help: "how often" + req},
		{Name: "day", Value: "D", Help: "weekly: sun..sat or 0-6; monthly: 1-31, or -1 for the last day; yearly: MM-DD; every-n-days: the interval N (repeatable)", Repeat: true},
		{Name: "start", Value: "YYYY-MM-DD", Help: "first date it may fire (required for every-n-days, which counts from it)"},
		{Name: "end", Value: "YYYY-MM-DD", Help: "last date it may fire"},
		{Name: "type", Value: "expense|income|transfer", Help: "the transaction type" + req + " (transfer is implied by --to-account)"},
		{Name: "account", Value: "id|name", Help: "the account" + req},
		{Name: "amount", Value: "hundredths", Help: "integer hundredths (1250 = 12.50)" + req},
		{Name: "category", Value: "id|name", Help: "a secondary category matching the type" + req},
		{Name: "to-account", Value: "id|name", Help: "transfer: the destination account"},
		{Name: "to-amount", Value: "hundredths", Help: "transfer: the amount that arrives, in the destination's currency (required across currencies)"},
		{Name: "tag", Value: "id|name", Help: "tag the created transactions (repeatable, at most 10)", Repeat: true},
		{Name: "comment", Value: "text", Help: "comment on the created transactions"},
		{Name: "hide-amount", Help: "hide the amount of the created transactions in the app's UI"},
	}

	if add {
		return append(flags, app.Flag{Name: "idempotency-key", Value: "key", Help: "a retry with the same key returns the original result"})
	}

	return append(flags,
		app.Flag{Name: "no-end", Help: "remove the end date"},
		app.Flag{Name: "clear-tags", Help: "remove every tag"},
		app.Flag{Name: "show-amount", Help: "stop hiding the amount"},
	)
}

// wrFrequencies are the --every values; the day values themselves are parsed by the server, with
// the cron's own calendar rules (apis.mdx §10.6), never by a second implementation here
var wrFrequencies = map[string]string{
	"daily": "daily", "weekly": "weekly", "monthly": "monthly", "yearly": "yearly",
	"every-n-days": "every_n_days", "every_n_days": "every_n_days",
}

// wrFrequency checks --every and passes --day through as frequency_value
func wrFrequency(every string, days []string) (string, []string, error) {
	freq, ok := wrFrequencies[strings.ToLower(strings.TrimSpace(every))]

	if !ok {
		return "", nil, fmt.Errorf("--every must be daily, weekly, monthly, yearly or every-n-days")
	}

	if freq == "daily" && len(days) > 0 {
		return "", nil, fmt.Errorf("--day does not apply to --every daily")
	}

	if freq != "daily" && len(days) == 0 {
		return "", nil, fmt.Errorf("--every %s needs --day (%s)", every, map[string]string{
			"weekly": "sun..sat, or 0-6 with Sunday 0", "monthly": "1-31, or -1 for the last day",
			"yearly": "MM-DD", "every_n_days": "the interval in days",
		}[freq])
	}

	if freq == "every_n_days" && len(days) != 1 {
		return "", nil, fmt.Errorf("--every every-n-days takes exactly one --day N (the interval in days)")
	}

	if days == nil {
		days = []string{}
	}

	return freq, days, nil
}

// wrScheduleBody builds the template body from the schedule flags
func wrScheduleBody(c *app.Ctx, add bool) (map[string]any, error) {
	body := map[string]any{}

	if add {
		body["kind"] = "scheduled"
	}

	if c.Has("name") {
		if strings.TrimSpace(c.String("name")) == "" {
			return nil, app.Usage("--name cannot be empty", "")
		}

		body["name"] = strings.TrimSpace(c.String("name"))
	}

	days := c.Strings("day")

	if c.Has("every") {
		freq, value, err := wrFrequency(c.String("every"), days)

		if err != nil {
			return nil, app.Usage(err.Error(), "ezbk help "+c.Verb.Name)
		}

		body["frequency"] = freq
		body["frequency_value"] = value
	} else if len(days) > 0 {
		return nil, app.Usage("--day needs --every (the frequency it belongs to)", "")
	}

	start, err := wrDate(c, "start", wrDatePoint)

	if err != nil {
		return nil, err
	}

	end, err := wrDate(c, "end", wrDateEnd)

	if err != nil {
		return nil, err
	}

	if start != "" {
		body["start"] = start
	}

	if end != "" {
		if c.Bool("no-end") {
			return nil, app.Usage("--end and --no-end contradict each other", "pick one")
		}

		body["end"] = end
	} else if c.Bool("no-end") {
		body["end"] = ""
	}

	if start != "" && end != "" && end < start {
		return nil, app.Usage(fmt.Sprintf("--end %s is before --start %s", end, start), "")
	}

	if add && body["frequency"] == "every_n_days" && start == "" {
		return nil, app.Usage("--every every-n-days needs --start (the day the count starts from)", "")
	}

	if v := c.String("type"); v != "" {
		t, ok := wrTxnTypes[strings.ToLower(v)]

		if !ok || t == "balance_modification" {
			return nil, app.Usage("--type must be expense, income or transfer", "")
		}

		body["type"] = t
	} else if add && c.Has("to-account") {
		body["type"] = "transfer"
	}

	for _, r := range []struct{ flag, base string }{{"account", "account"}, {"to-account", "destination_account"}, {"category", "category"}} {
		if v := c.String(r.flag); v != "" {
			wrSetRef(body, r.base, v)
		}
	}

	if amount, has, err := c.Amount("amount"); err != nil {
		return nil, err
	} else if has {
		if amount < 0 {
			return nil, app.Usage("--amount must not be negative: the type carries the direction", "")
		}

		body["amount"] = amount
	}

	if amount, has, err := c.Amount("to-amount"); err != nil {
		return nil, err
	} else if has {
		if amount < 0 {
			return nil, app.Usage("--to-amount must not be negative", "")
		}

		body["destination_amount"] = amount
	}

	tags := c.Strings("tag")

	switch {
	case len(tags) > 0 && c.Bool("clear-tags"):
		return nil, app.Usage("--tag and --clear-tags contradict each other", "pick one")
	case len(tags) > 10:
		return nil, app.Usage("at most 10 tags", "")
	case len(tags) > 0:
		wrSetRefs(body, "tag", tags)
	case c.Bool("clear-tags"):
		body["tag_ids"] = []string{}
	}

	if c.Has("comment") {
		body["comment"] = c.String("comment")
	}

	switch {
	case c.Bool("hide-amount") && c.Bool("show-amount"):
		return nil, app.Usage("--hide-amount and --show-amount contradict each other", "pick one")
	case c.Bool("hide-amount"):
		body["hide_amount"] = true
	case c.Bool("show-amount"):
		body["hide_amount"] = false
	}

	if add {
		var missing []string

		for _, r := range []struct {
			flag string
			ok   bool
		}{
			{"--name", body["name"] != nil},
			{"--every", body["frequency"] != nil},
			{"--type", body["type"] != nil},
			{"--account", body["account_id"] != nil || body["account_name"] != nil},
			{"--amount", body["amount"] != nil},
			{"--category", body["category_id"] != nil || body["category_name"] != nil},
		} {
			if !r.ok {
				missing = append(missing, r.flag)
			}
		}

		if body["type"] == "transfer" && body["destination_account_id"] == nil && body["destination_account_name"] == nil {
			missing = append(missing, "--to-account")
		}

		if len(missing) > 0 {
			return nil, app.Usage("`ezbk schedules add` needs "+strings.Join(missing, ", "), "ezbk schedules add --name Rent --every monthly --day 1 --type expense --account Checking --amount 150000 --category Rent --start 2026-10-01")
		}
	}

	return body, nil
}

func wrRunScheduleAdd(c *app.Ctx) error {
	body, err := wrScheduleBody(c, true)

	if err != nil {
		return err
	}

	c.Info("scheduled transactions are created at 00:00 in the schedule's timezone (%s); after writing, `ezbk schedules upcoming` shows what it will create", c.Location().String())

	return wrFlow(c, "POST", wrPathTemplates, body)
}

func wrRunScheduleEdit(c *app.Ctx) error {
	body, err := wrScheduleBody(c, false)

	if err != nil {
		return err
	}

	if len(body) == 0 {
		return app.Usage("nothing to change: pass --every/--day, --start, --end, --amount, --account, --category, …", "ezbk help schedules edit")
	}

	id, err := wrPathId(c, wrKindSchedule, c.Args[0])

	if err != nil {
		return err
	}

	return wrFlow(c, "PATCH", wrPathTemplates+"/"+url.PathEscape(id), body)
}

// wrScheduleOf reads one schedule's current frequency from the server
func wrScheduleOf(c *app.Ctx, id string) (map[string]any, error) {
	env, err := c.Call("GET", wrPathTemplates+"/"+url.PathEscape(id), nil, nil)

	if err != nil {
		return nil, err
	}

	tpl, _ := env.DataMap()["template"].(map[string]any)

	if tpl == nil {
		return nil, app.Fail(exitcode.Failed, "the server did not return the template", "ezbk templates list --scheduled")
	}

	if kind, _ := tpl["kind"].(string); kind != "" && kind != "scheduled" {
		return nil, app.Usage(fmt.Sprintf("%q is a normal template, not a scheduled transaction", c.Args[0]), "ezbk templates list --scheduled")
	}

	return tpl, nil
}

// wrResumeCommand is the exact command that restores a schedule's current frequency
func wrResumeCommand(ref string, sched map[string]any) string {
	freq, _ := sched["frequency"].(string)
	value, _ := sched["frequencyValue"].(string)

	if freq == "" || freq == "disabled" {
		return ""
	}

	cmd := "ezbk schedules resume " + strconv.Quote(ref) + " --every " + strings.ReplaceAll(freq, "_", "-")

	if freq != "daily" {
		for _, v := range strings.Split(value, ",") {
			if v = strings.TrimSpace(v); v != "" {
				cmd += " --day " + v
			}
		}
	}

	return cmd
}

// wrRunSchedulePause pauses a schedule by setting its frequency to disabled — hiding a template
// does NOT stop upstream's cron. The frequency it had is printed first, because a paused schedule
// no longer stores it.
func wrRunSchedulePause(c *app.Ctx) error {
	id, err := wrPathId(c, wrKindSchedule, c.Args[0])

	if err != nil {
		return err
	}

	tpl, err := wrScheduleOf(c, id)

	if err != nil {
		return err
	}

	sched, _ := tpl["schedule"].(map[string]any)

	if paused, _ := sched["paused"].(bool); paused {
		c.Info("%q is already paused", c.Args[0])
	} else if cmd := wrResumeCommand(c.Args[0], sched); cmd != "" {
		c.Info("currently %v — to resume later exactly as it is now:\n  %s", wrFirst(sched["describe"], sched["frequency"]), cmd)
	}

	return wrFlow(c, "PATCH", wrPathTemplates+"/"+url.PathEscape(id), map[string]any{"frequency": "disabled"})
}

// wrRunScheduleResume sets a paused schedule's frequency again
func wrRunScheduleResume(c *app.Ctx) error {
	id, err := wrPathId(c, wrKindSchedule, c.Args[0])

	if err != nil {
		return err
	}

	if !c.Has("every") {
		tpl, err := wrScheduleOf(c, id)

		if err != nil {
			return err
		}

		sched, _ := tpl["schedule"].(map[string]any)

		if paused, _ := sched["paused"].(bool); !paused {
			return app.Usage(fmt.Sprintf("%q is not paused (%v)", c.Args[0], wrFirst(sched["describe"], sched["frequency"])), "ezbk schedules edit to change its frequency")
		}

		return app.Usage("a paused schedule no longer stores its frequency: pass --every (and --day)", "if the pause was the last machine-plane write, `ezbk undo --write` restores it exactly; otherwise e.g. --every monthly --day 1")
	}

	freq, value, err := wrFrequency(c.String("every"), c.Strings("day"))

	if err != nil {
		return app.Usage(err.Error(), "ezbk help schedules resume")
	}

	body := map[string]any{"frequency": freq, "frequency_value": value}

	if start, err := wrDate(c, "start", wrDatePoint); err != nil {
		return err
	} else if start != "" {
		body["start"] = start
	}

	c.Info("after writing, `ezbk schedules upcoming` shows what it will create")

	return wrFlow(c, "PATCH", wrPathTemplates+"/"+url.PathEscape(id), body)
}

// ---------------------------------------------------------------------------------------------
// rates
// ---------------------------------------------------------------------------------------------

var wrRateRe = regexp.MustCompile(`^[0-9]+(\.[0-9]+)?$`)

// wrRate validates a rate: a positive decimal ratio, kept as the string typed (never a float here)
func wrRate(v string) (string, error) {
	v = strings.TrimSpace(v)

	if !wrRateRe.MatchString(v) {
		return "", fmt.Errorf("%q is not a rate: a positive decimal like 1.0842", v)
	}

	if strings.Trim(strings.ReplaceAll(v, ".", ""), "0") == "" {
		return "", fmt.Errorf("a rate of zero converts everything to nothing")
	}

	if len(v) > 32 {
		return "", fmt.Errorf("%q has more digits than any exchange rate needs", v)
	}

	return v, nil
}

func wrRunRateSet(c *app.Ctx) error {
	cur, err := wrCurrency(c.Args[0])

	if err != nil {
		return app.Usage(err.Error(), "ezbk rates set EUR 1.0842")
	}

	rate, err := wrRate(c.Args[1])

	if err != nil {
		return app.Usage(err.Error(), "ezbk rates set EUR 1.0842")
	}

	c.Info("a custom rate is a preference: `ezbk rates clear %s` restores the provider's", cur)

	return wrFlow(c, "PUT", wrPathCustomRate+url.PathEscape(cur), map[string]any{"rate": rate})
}

func wrRunRateClear(c *app.Ctx) error {
	cur, err := wrCurrency(c.Args[0])

	if err != nil {
		return app.Usage(err.Error(), "ezbk rates clear EUR")
	}

	return wrFlow(c, "DELETE", wrPathCustomRate+url.PathEscape(cur), map[string]any{})
}

// ---------------------------------------------------------------------------------------------
// undo / redo / journal
// ---------------------------------------------------------------------------------------------

// wrJournalPeek reads the journal and returns the entry undo (or redo) would act on next — the
// server names it as nextUndo / nextRedo — together with the journal envelope
func wrJournalPeek(c *app.Ctx, op string) (map[string]any, *client.Envelope, error) {
	q := url.Values{}
	q.Set("limit", "100")

	env, err := c.Call("GET", wrPathJournal, q, nil)

	if err != nil {
		return nil, nil, err
	}

	return wrNextJournalEntry(env.DataMap(), op), env, nil
}

// wrNextJournalEntry finds the entry the server named as next for op in a /journal answer. When
// the named entry is older than the page, a stub carrying only its id is returned.
func wrNextJournalEntry(data map[string]any, op string) map[string]any {
	key := "nextUndo"

	if op == "redo" {
		key = "nextRedo"
	}

	next := data[key]

	if next == nil {
		return nil
	}

	id := fmt.Sprint(next)

	if id == "" {
		return nil
	}

	entries, _ := data["entries"].([]any)

	for _, e := range entries {
		if m, ok := e.(map[string]any); ok && fmt.Sprint(m["journalId"]) == id {
			return m
		}
	}

	return map[string]any{"journalId": id}
}

// wrEnvelope wraps a CLI-composed answer in the envelope shape so every --format renders it
func wrEnvelope(data any, meta map[string]any) (*client.Envelope, error) {
	d, err := json.Marshal(data)

	if err != nil {
		return nil, err
	}

	raw, err := json.Marshal(map[string]any{"ok": true, "data": json.RawMessage(d), "meta": meta})

	if err != nil {
		return nil, err
	}

	return &client.Envelope{OK: true, Data: d, Meta: meta, Raw: raw}, nil
}

// wrRunUndoRedo is the write protocol for undo and redo. The routes have no dry_run (apis.mdx
// §9.2), so the preview is the journal entry the SERVER names as next; with --write the CLI sends
// that entry's id as the journal_id guard, so the write can only reverse the entry the operator
// was shown — if another write landed in between, the server refuses with conflict (exit 4).
func wrRunUndoRedo(c *app.Ctx, op string) error {
	var entry map[string]any
	var journalEnv *client.Envelope

	guard := strings.TrimSpace(c.String("journal-id"))

	if guard != "" && !wrDigitsRe.MatchString(guard) {
		return app.Usage("--journal-id is a journal entry id (a decimal string from `ezbk journal`)", "ezbk journal")
	}

	if guard == "" || !c.Bool("write") {
		e, env, err := wrJournalPeek(c, op)

		if err != nil {
			return err
		}

		entry, journalEnv = e, env
	}

	if entry == nil && guard == "" {
		verb := "undo"

		if op == "redo" {
			verb = "redo (redo is available only until the next write)"
		}

		out, err := wrEnvelope(map[string]any{"dry_run": !c.Bool("write"), "op": op, "next": nil, "note": wrJournalNote(journalEnv)}, journalEnv.Meta)

		if err != nil {
			return err
		}

		_ = c.Emit(out, wrViewUndo)

		return &app.ExitError{Code: exitcode.NotFound, Msg: "nothing to " + verb + ": no machine-plane write is waiting for it", Hint: "ezbk journal — undo reaches only writes made through this plane, never browser edits"}
	}

	if guard != "" && entry != nil && fmt.Sprint(entry["journalId"]) != guard {
		c.Warnf("journal entry %s is not next in line (%v is); the server will refuse unless it is", guard, entry["journalId"])
	}

	if guard == "" {
		guard = fmt.Sprint(entry["journalId"])
	}

	if !c.Bool("write") {
		c.Info("DRY RUN — nothing was changed. `ezbk %s --write` would %s journal entry %s: %s. Browser edits are never undone.", op, op, guard, wrDescribeEntry(entry))

		out, err := wrEnvelope(map[string]any{"dry_run": true, "op": op, "next": entry, "note": wrJournalNote(journalEnv)}, journalEnv.Meta)

		if err != nil {
			return err
		}

		return c.Emit(out, wrViewUndo)
	}

	if entry != nil {
		c.Info("target %s — %s journal entry %s: %s", c.BaseURL(), op, guard, wrDescribeEntry(entry))
	} else {
		c.Info("target %s — %s journal entry %s", c.BaseURL(), op, guard)
	}

	env, err := c.Call("POST", "/"+op, nil, map[string]any{"journal_id": guard})

	if err != nil {
		if ee, ok := err.(*app.ExitError); ok && ee.APICode == "conflict" {
			if op == "undo" && strings.Contains(ee.Msg, "not the most recent") {
				ee.Hint = "another write landed since the preview; run `ezbk undo` again to see what is next"
			} else if ee.Hint == "" {
				ee.Hint = "a row was edited after the write (probably in the browser); undo will not overwrite a human's later edit"
			}
		}

		return err
	}

	data := env.DataMap()

	if done, ok := data["undone"].(bool); ok && !done {
		c.Info("journal entry %s was already undone; nothing changed", guard)
	}

	if done, ok := data["redone"].(bool); ok && !done {
		c.Info("journal entry %s was already applied; nothing changed", guard)
	}

	if op == "undo" {
		if avail, _ := data["redoAvailable"].(bool); avail {
			c.Info("`ezbk redo --write` re-applies it (until the next write)")
		}
	}

	return c.Emit(env, wrViewUndoResult)
}

func wrJournalNote(env *client.Envelope) any {
	if env == nil {
		return nil
	}

	return env.DataMap()["note"]
}

// wrDescribeEntry is the one-line description of a journal entry (never amounts: the journal has none)
func wrDescribeEntry(e map[string]any) string {
	if e == nil {
		return "(unknown entry)"
	}

	parts := []string{}

	if s, ok := e["summary"].(string); ok && s != "" {
		parts = append(parts, strconv.Quote(s))
	}

	if r, ok := e["route"].(string); ok && r != "" {
		parts = append(parts, r)
	}

	if n, ok := render.ToInt64(e["changes"]); ok {
		parts = append(parts, fmt.Sprintf("%d changes", n))
	}

	if t, ok := e["createdAt"].(string); ok && t != "" {
		parts = append(parts, "at "+t)
	}

	if cl, ok := e["client"].(string); ok && cl != "" {
		parts = append(parts, "by "+cl)
	}

	if len(parts) == 0 {
		return "(details beyond the journal page; `ezbk journal --limit 500`)"
	}

	return strings.Join(parts, ", ")
}

var (
	wrViewUndo = render.View{
		Rows: "next",
		Columns: []render.Column{
			{Header: "Journal ID", Key: "journalId"},
			{Header: "When", Key: "createdAt"},
			{Header: "Route", Key: "route"},
			{Header: "Summary", Key: "summary"},
			{Header: "Changes", Key: "changes", Kind: render.KInt},
			{Header: "Client", Key: "client"},
		},
		Empty: "nothing waiting",
	}

	wrViewUndoResult = render.View{
		Rows: "entry",
		Columns: []render.Column{
			{Header: "Journal ID", Key: "journalId"},
			{Header: "Route", Key: "route"},
			{Header: "Summary", Key: "summary"},
			{Header: "Changes", Key: "changes", Kind: render.KInt},
			{Header: "Undone", Key: "undone", Kind: render.KBool},
			{Header: "Undone At", Key: "undoneAt"},
		},
	}
)

func wrRunJournal(c *app.Ctx) error {
	limit, err := c.Int("limit", 20)

	if err != nil {
		return err
	}

	if limit < 1 {
		return app.Usage("--limit must be at least 1", "")
	}

	offset, err := c.Int("offset", 0)

	if err != nil {
		return err
	}

	if offset < 0 {
		return app.Usage("--offset must be 0 or more", "")
	}

	q := url.Values{}
	q.Set("limit", strconv.Itoa(limit))

	if offset > 0 {
		q.Set("offset", strconv.Itoa(offset))
	}

	if c.Bool("active-only") {
		q.Set("include_undone", "false")
	}

	env, err := c.Call("GET", wrPathJournal, q, nil)

	if err != nil {
		return err
	}

	data := env.DataMap()

	if next := data["nextUndo"]; next != nil {
		c.Info("next undo: journal entry %v   (ezbk undo)", next)
	}

	if next := data["nextRedo"]; next != nil {
		c.Info("next redo: journal entry %v   (ezbk redo)", next)
	}

	if n, ok := render.ToInt64(data["nextOffset"]); ok {
		c.Info("more entries: --offset %d", n)
	}

	return c.Emit(env, wrViewJournal)
}

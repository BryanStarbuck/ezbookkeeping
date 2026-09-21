package commands

// extras.go covers the plane routes the first CLI pass left without a verb: the bound user's
// profile and settings, tag groups, reordering, normal templates, saved insights, account
// properties, the earliest/latest transaction, move-all, batch, capabilities, and the ingest
// plane's map / rows / runs / roots / converters.
//
// The same rules as every other family hold: writes go through wrFlow (dry run unless --write),
// names are sent as *_name for the server to resolve, amounts are integer hundredths via c.Amount,
// and nothing here adds, converts or compares money.

import (
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"strconv"
	"strings"

	"github.com/BryanStarbuck/ezbookkeeping/cli/internal/app"
	"github.com/BryanStarbuck/ezbookkeeping/cli/internal/client"
	"github.com/BryanStarbuck/ezbookkeeping/cli/internal/render"
)

var (
	exKindTagGroup = wrKind{Path: "/tag-groups", Rows: "tagGroups", Noun: "tag group", ListIt: "ezbk tag-groups list"}
	exKindTemplate = wrKind{Path: wrPathTemplates, Rows: "templates", Noun: "template", ListIt: "ezbk templates list --include-hidden", Query: map[string]string{"kind": "all"}}
	exKindInsight  = wrKind{Path: "/insights", Rows: "insights", Noun: "insight", ListIt: "ezbk insights list --include-hidden"}
)

// exMovable are the families whose display order `<family> move` changes
var exMovable = []struct {
	family string
	kind   wrKind
}{
	{"accounts", wrKindAccount},
	{"categories", wrKindCategory},
	{"tags", wrKindTag},
	{"tag-groups", exKindTagGroup},
	{"templates", exKindTemplate},
	{"insights", exKindInsight},
}

var exPathFlag = app.Flag{Name: "path", Value: "DIR", Help: "the statements root (else EZBK_STATEMENTS_DIR, else statements.root in the credentials file)"}
var exManifestFlag = app.Flag{Name: "manifest-path", Value: "FILE", Help: "the manifest, relative to the root (default: the root's manifest)"}

func init() {
	verbs := []app.Verb{
		// ---- the bound user
		{
			Name: "user show", Summary: "the bound user's profile: nickname, default currency, first day of week, fiscal year start, reconciled-time setting",
			Group: orGroupRead, MaxArgs: 0, Run: exRunUserShow,
		},
		{
			Name: "user edit", Summary: "change the bound user's profile (email, password and avatar stay in the web UI)",
			Group: wrGroup, MaxArgs: 0, Run: exRunUserEdit,
			Flags: []app.Flag{
				{Name: "nickname", Value: "text", Help: "display name"},
				{Name: "default-currency", Value: "CUR", Help: "the currency totals default to"},
				{Name: "first-day-of-week", Value: "sun..sat|0-6", Help: "the day weeks start on"},
				{Name: "fiscal-year-start", Value: "MM-DD", Help: "the first day of the fiscal year"},
				{Name: "reconciled-time", Value: "on|off", Help: "use the last-reconciled time (needed by `ezbk accounts reconcile` to mark an account)"},
			},
		},
		{
			Name: "user settings", Summary: "the bound user's application settings (key, value, type)",
			Group: orGroupRead, MaxArgs: 0, Run: exRunUserSettings,
		},
		{
			Name: "user settings set", Summary: "change application settings: KEY VALUE pairs (the server type-checks each value)",
			Args: "<key> <value> [<key> <value>…]", MinArgs: 2, MaxArgs: -1, Group: wrGroup, Run: exRunUserSettingsSet,
		},

		// ---- tag groups
		{
			Name: "tag-groups show", Summary: "one tag group with its tags", Args: "<id|name>", MinArgs: 1, MaxArgs: 1,
			Group: orGroupRead, Run: exRunShow(exKindTagGroup, "tagGroup"),
		},
		{
			Name: "tag-groups add", Summary: "create a tag group", Group: wrGroup, MaxArgs: 0, Run: exRunTagGroupAdd,
			Flags: []app.Flag{{Name: "name", Value: "text", Help: "the group name (required)"}},
		},
		{
			Name: "tag-groups edit", Summary: "rename a tag group", Args: "<id|name>", MinArgs: 1, MaxArgs: 1, Group: wrGroup, Run: exRunTagGroupEdit,
			Flags: []app.Flag{{Name: "name", Value: "text", Help: "the new name (required)"}},
		},

		// ---- templates (normal) and insights
		{
			Name: "templates show", Summary: "one template or scheduled transaction", Args: "<id|name>", MinArgs: 1, MaxArgs: 1,
			Group: orGroupRead, Run: exRunShow(exKindTemplate, "template"),
		},
		{
			Name: "templates add", Summary: "create a transaction template (a scheduled one is `ezbk schedules add`)",
			Group: wrGroup, MaxArgs: 0, Run: exRunTemplateAdd, Flags: exTemplateFlags(true),
		},
		{
			Name: "templates edit", Summary: "change a template's name, amounts, accounts, category, tags or comment",
			Args: "<id|name>", MinArgs: 1, MaxArgs: 1, Group: wrGroup, Run: exRunTemplateEdit, Flags: exTemplateFlags(false),
		},
		{
			Name: "templates hide", Summary: "hide a template (a hidden schedule still fires: pause it with `ezbk schedules pause`)",
			Args: "<id|name>", MinArgs: 1, MaxArgs: 1, Group: wrGroup, Run: func(c *app.Ctx) error { return wrRunHide(c, exKindTemplate, true) },
		},
		{
			Name: "templates unhide", Summary: "show a hidden template again",
			Args: "<id|name>", MinArgs: 1, MaxArgs: 1, Group: wrGroup, Run: func(c *app.Ctx) error { return wrRunHide(c, exKindTemplate, false) },
		},
		{
			Name: "insights show", Summary: "one saved Insights Explorer definition, with its data", Args: "<id|name>", MinArgs: 1, MaxArgs: 1,
			Group: orGroupRead, Run: exRunShow(exKindInsight, "insight"),
		},
		{
			Name: "insights add", Summary: "save an Insights Explorer definition", Group: wrGroup, MaxArgs: 0, Run: exRunInsightAdd,
			Flags: []app.Flag{
				{Name: "name", Value: "text", Help: "the insight's name (required)"},
				{Name: "definition", Value: "JSON|@file|-", Help: "the definition object (required): inline JSON, @file, or - for stdin"},
			},
		},
		{
			Name: "insights edit", Summary: "rename an insight or replace its definition", Args: "<id|name>", MinArgs: 1, MaxArgs: 1, Group: wrGroup, Run: exRunInsightEdit,
			Flags: []app.Flag{
				{Name: "name", Value: "text", Help: "the new name"},
				{Name: "definition", Value: "JSON|@file|-", Help: "the new definition object"},
			},
		},
		{
			Name: "insights hide", Summary: "hide an insight", Args: "<id|name>", MinArgs: 1, MaxArgs: 1, Group: wrGroup,
			Run: func(c *app.Ctx) error { return wrRunHide(c, exKindInsight, true) },
		},
		{
			Name: "insights unhide", Summary: "show a hidden insight again", Args: "<id|name>", MinArgs: 1, MaxArgs: 1, Group: wrGroup,
			Run: func(c *app.Ctx) error { return wrRunHide(c, exKindInsight, false) },
		},

		// ---- accounts / transactions extras
		{
			Name: "accounts properties", Summary: "an account's facts: transaction count, first and last transaction, reconciled time, balance modifications",
			Args: "<id|name>", MinArgs: 1, MaxArgs: 1, Group: orGroupRead, Run: exRunAccountProperties,
		},
		{
			Name: "transactions earliest", Summary: "the oldest transaction (optionally of one account)", Group: orGroupRead, MaxArgs: 0,
			Flags: []app.Flag{{Name: "account", Value: "id|name", Help: "only this account (a parent includes its sub-accounts)"}},
			Run:   func(c *app.Ctx) error { return exRunEdge(c, "earliest") },
		},
		{
			Name: "transactions latest", Summary: "the newest transaction (optionally of one account)", Group: orGroupRead, MaxArgs: 0,
			Flags: []app.Flag{{Name: "account", Value: "id|name", Help: "only this account (a parent includes its sub-accounts)"}},
			Run:   func(c *app.Ctx) error { return exRunEdge(c, "latest") },
		},
		{
			Name: "transactions move-all", Summary: "move every transaction of one account to another of the same currency",
			Group: wrGroup, MaxArgs: 0, Run: exRunMoveAll,
			Flags: []app.Flag{
				{Name: "from", Value: "id|name", Help: "the account emptied (required)"},
				{Name: "to", Value: "id|name", Help: "the account that receives them (required; same currency)"},
			},
		},

		// ---- batch and capabilities
		{
			Name: "batch", Summary: "run several write operations as ONE reviewed, journaled change (one `ezbk undo` reverses it)",
			Args: "<ops.json|->", MinArgs: 1, MaxArgs: 1, Group: wrGroup, Run: exRunBatch,
			Flags: []app.Flag{{Name: "summary", Value: "text", Help: "a one-line description for the journal"}},
		},
		{
			Name: "batch ops", Summary: "the operations a batch may contain (method, path, summary)", Group: orGroupRead, MaxArgs: 0, Run: exRunBatchOps,
		},
		{
			Name: "capabilities", Summary: "every plane route with its tier and status, the limits and the features this server has on",
			Group: orGroupOrientation, MaxArgs: 0, Run: exRunCapabilities,
		},

		// ---- statements: map, rows, runs, roots, converters
		{
			Name: "statements map", Summary: "the statement-account → ezBookkeeping-account map, with each entry's status",
			Args: "[PATH]", MaxArgs: 1, Group: asStmtGroup, Run: exRunMapShow, Flags: []app.Flag{exPathFlag, exManifestFlag},
		},
		{
			Name: "statements map set", Summary: "map statement accounts to ezBookkeeping accounts: KEY=ACCOUNT_ID pairs (dry run unless --write)",
			Args: "<KEY=ID>…", MinArgs: 1, MaxArgs: -1, Group: asStmtGroup, Run: exRunMapSet,
			Flags: []app.Flag{exPathFlag, exManifestFlag, {Name: "replace", Help: "replace the whole map instead of merging into it"}},
		},
		{
			Name: "statements map infer", Summary: "propose a map from the manifest and the existing accounts (nothing is saved)",
			Args: "[PATH]", MaxArgs: 1, Group: asStmtGroup, Run: exRunMapInfer, Flags: []app.Flag{exPathFlag, exManifestFlag},
		},
		{
			Name: "statements rows", Summary: "the parsed statement rows with their import status (new, already present, blocked …)",
			Args: "[PATH]", MaxArgs: 1, Group: asStmtGroup, Run: exRunRows,
			Flags: []app.Flag{
				exPathFlag, exManifestFlag,
				{Name: "mode", Value: "prepared|raw", Help: "which archive to read (default: what the root holds)"},
				{Name: "account", Value: "KEY", Repeat: true, Help: "only these statement accounts (repeatable)"},
				{Name: "start", Value: "YYYY-MM-DD", Help: "first day"},
				{Name: "end", Value: "YYYY-MM-DD", Help: "last day"},
				{Name: "status", Value: "STATUS", Help: "only rows with this status"},
				{Name: "limit", Value: "N", Help: "rows per page (default 200)"},
				{Name: "offset", Value: "N", Help: "rows to skip"},
			},
		},
		{
			Name: "statements runs", Summary: "past imports: run id, when, outcome, created / linked / reimported counts",
			Group: asStmtGroup, MaxArgs: 0, Run: exRunRuns, Flags: []app.Flag{{Name: "limit", Value: "N", Help: "how many (default 50)"}},
		},
		{
			Name: "statements run", Summary: "one import run's report, including the rows it put in a fallback category",
			Args: "<run-id>", MinArgs: 1, MaxArgs: 1, Group: asStmtGroup, Run: exRunRunShow,
		},
		{
			Name: "statements roots", Summary: "the statements roots the server accepts, and what each holds",
			Group: asStmtGroup, MaxArgs: 0, Run: exRunRoots,
		},
		{
			Name: "statements converters", Summary: "the file formats the server can import and what each needs",
			Group: asStmtGroup, MaxArgs: 0, Run: exRunConverters,
		},
	}

	for _, m := range exMovable {
		kind := m.kind
		verbs = append(verbs, app.Verb{
			Name:    m.family + " move",
			Summary: "change a " + kind.Noun + "'s display order (0 is first)",
			Args:    "<id|name>", MinArgs: 1, MaxArgs: 1, Group: wrGroup,
			Flags: []app.Flag{{Name: "to-index", Value: "N", Help: "the new 0-based position among its siblings (required)"}},
			Run:   func(c *app.Ctx) error { return exRunMove(c, kind) },
		})
	}

	app.Register(verbs...)
}

// ---------------------------------------------------------------------------------------------
// shared helpers

// exReadJSONArg reads a JSON value given inline, as @file, or as - (stdin)
func exReadJSONArg(v string, stdin io.Reader) (json.RawMessage, error) {
	var data []byte
	var err error

	switch {
	case v == "-":
		data, err = io.ReadAll(stdin)
	case strings.HasPrefix(v, "@"):
		data, err = os.ReadFile(strings.TrimPrefix(v, "@"))
	default:
		data = []byte(v)
	}

	if err != nil {
		return nil, err
	}

	data = []byte(strings.TrimSpace(string(data)))

	if !json.Valid(data) {
		return nil, fmt.Errorf("not valid JSON")
	}

	return json.RawMessage(data), nil
}

// exSettingValue turns a typed VALUE into JSON: true/false, integers and JSON literals keep their
// type, anything else is a string (the server type-checks it against the setting)
func exSettingValue(v string) json.RawMessage {
	t := strings.TrimSpace(v)

	switch {
	case t == "true" || t == "false" || t == "null":
		return json.RawMessage(t)
	case strings.HasPrefix(t, "{") || strings.HasPrefix(t, "[") || strings.HasPrefix(t, "\""):
		if json.Valid([]byte(t)) {
			return json.RawMessage(t)
		}
	}

	if _, err := strconv.ParseInt(t, 10, 64); err == nil {
		return json.RawMessage(t)
	}

	b, _ := json.Marshal(v)

	return json.RawMessage(b)
}

// exWeekday accepts sun..sat (any case, 3+ letters) or 0-6
func exWeekday(v string) (int, error) {
	t := strings.ToLower(strings.TrimSpace(v))

	if n, err := strconv.Atoi(t); err == nil {
		if n < 0 || n > 6 {
			return 0, fmt.Errorf("a weekday number is 0 (Sunday) to 6 (Saturday)")
		}

		return n, nil
	}

	for i, d := range []string{"sunday", "monday", "tuesday", "wednesday", "thursday", "friday", "saturday"} {
		if len(t) >= 3 && strings.HasPrefix(d, t) {
			return i, nil
		}
	}

	return 0, fmt.Errorf("%q is not a weekday (sun..sat or 0-6)", v)
}

// exOnOff parses on|off (and the usual boolean spellings)
func exOnOff(v string) (bool, error) {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "on", "true", "yes", "1":
		return true, nil
	case "off", "false", "no", "0":
		return false, nil
	}

	return false, fmt.Errorf("%q is not on or off", v)
}

// exParseKeyIds parses KEY=ID pairs for the ingest map
func exParseKeyIds(args []string) (map[string]string, error) {
	out := map[string]string{}

	for _, a := range args {
		eq := strings.LastIndex(a, "=")

		if eq <= 0 || eq == len(a)-1 {
			return nil, fmt.Errorf("%q is not KEY=ACCOUNT_ID", a)
		}

		key, id := strings.TrimSpace(a[:eq]), strings.TrimSpace(a[eq+1:])

		if !orIsID(id) {
			return nil, fmt.Errorf("%q: the account must be an id (ezbk accounts list shows them)", a)
		}

		if _, dup := out[key]; dup {
			return nil, fmt.Errorf("%s is mapped twice", key)
		}

		out[key] = id
	}

	return out, nil
}

// exRunShow is `<family> show <id|name>`: GET /<family>/:id, printed as key/value pairs
func exRunShow(kind wrKind, key string) func(c *app.Ctx) error {
	return func(c *app.Ctx) error {
		id, err := wrPathId(c, kind, c.Args[0])

		if err != nil {
			return err
		}

		env, err := c.Call("GET", kind.Path+"/"+url.PathEscape(id), nil, nil)

		if err != nil {
			return err
		}

		return exEmitObject(c, env, key)
	}
}

// exFlatten turns nested values into dotted key → text pairs (for CSV)
func exFlatten(prefix string, v any) map[string]string {
	out := map[string]string{}

	switch t := v.(type) {
	case map[string]any:
		for k, x := range t {
			key := k

			if prefix != "" {
				key = prefix + "." + k
			}

			for kk, vv := range exFlatten(key, x) {
				out[kk] = vv
			}
		}
	case []any:
		b, _ := json.Marshal(t)
		out[prefix] = string(b)
	case nil:
		out[prefix] = ""
	default:
		out[prefix] = fmt.Sprint(t)
	}

	return out
}

func exSortPairs(rows [][]string) {
	for i := 1; i < len(rows); i++ {
		for j := i; j > 0 && rows[j][0] < rows[j-1][0]; j-- {
			rows[j], rows[j-1] = rows[j-1], rows[j]
		}
	}
}

// ---------------------------------------------------------------------------------------------
// user

func exRunUserShow(c *app.Ctx) error {
	env, err := c.Call("GET", "/user/profile", nil, nil)

	if err != nil {
		return err
	}

	return exEmitObject(c, env, "profile")
}

// exEmitObject prints one object of the answer: JSON as-is, a table as key/value pairs, CSV as
// key,value rows
func exEmitObject(c *app.Ctx, env *client.Envelope, key string) error {
	if c.Format == render.FormatJSON {
		return c.Emit(env, render.View{})
	}

	data := orData(env)
	row, _ := data[key].(map[string]any)

	if row == nil {
		row = data
	}

	if c.Format == render.FormatCSV {
		rows := [][]string{}

		for k, v := range exFlatten("", row) {
			rows = append(rows, []string{k, v})
		}

		exSortPairs(rows)

		return c.EmitCSV([]string{"key", "value"}, rows)
	}

	return render.KeyValues(c.Out, row, nil)
}

func exRunUserEdit(c *app.Ctx) error {
	body := map[string]any{}

	if c.Has("nickname") {
		if strings.TrimSpace(c.String("nickname")) == "" {
			return app.Usage("--nickname cannot be empty", "")
		}

		body["nickname"] = c.String("nickname")
	}

	if c.Has("default-currency") {
		cur, err := wrCurrency(c.String("default-currency"))

		if err != nil {
			return err
		}

		body["default_currency"] = cur
	}

	if c.Has("first-day-of-week") {
		d, err := exWeekday(c.String("first-day-of-week"))

		if err != nil {
			return app.Usage("--first-day-of-week: "+err.Error(), "")
		}

		body["first_day_of_week"] = d
	}

	if c.Has("fiscal-year-start") {
		body["fiscal_year_start"] = strings.TrimSpace(c.String("fiscal-year-start"))
	}

	if c.Has("reconciled-time") {
		on, err := exOnOff(c.String("reconciled-time"))

		if err != nil {
			return app.Usage("--reconciled-time: "+err.Error(), "")
		}

		body["use_last_reconciled_time"] = on
	}

	if len(body) == 0 {
		return app.Usage("nothing to change", "ezbk help user edit")
	}

	return wrFlow(c, "PATCH", "/user/profile", body)
}

func exRunUserSettings(c *app.Ctx) error {
	env, err := c.Call("GET", "/user/settings", nil, nil)

	if err != nil {
		return err
	}

	return c.Emit(env, render.View{Rows: "settings", Empty: "no settings", Columns: []render.Column{
		{Header: "Key", Key: "key"},
		{Header: "Value", Key: "value"},
		{Header: "Type", Key: "type"},
	}})
}

func exRunUserSettingsSet(c *app.Ctx) error {
	if len(c.Args)%2 != 0 {
		return app.Usage("settings are KEY VALUE pairs; one value is missing", "e.g. ezbk user settings set timezoneUsedForStatisticsInHomePage 1")
	}

	settings := map[string]json.RawMessage{}

	for i := 0; i < len(c.Args); i += 2 {
		k := strings.TrimSpace(c.Args[i])

		if k == "" {
			return app.Usage("an empty setting key", "ezbk user settings lists the keys")
		}

		if _, dup := settings[k]; dup {
			return app.Usage(k+" is given twice", "")
		}

		settings[k] = exSettingValue(c.Args[i+1])
	}

	return wrFlow(c, "PATCH", "/user/settings", map[string]any{"settings": settings})
}

// ---------------------------------------------------------------------------------------------
// tag groups

func exRunTagGroupAdd(c *app.Ctx) error {
	name := strings.TrimSpace(c.String("name"))

	if name == "" {
		return app.Usage("--name is required", "ezbk help tag-groups add")
	}

	return wrFlow(c, "POST", exKindTagGroup.Path, map[string]any{"name": name})
}

func exRunTagGroupEdit(c *app.Ctx) error {
	name := strings.TrimSpace(c.String("name"))

	if name == "" {
		return app.Usage("--name is required", "ezbk help tag-groups edit")
	}

	id, err := wrPathId(c, exKindTagGroup, c.Args[0])

	if err != nil {
		return err
	}

	return wrFlow(c, "PATCH", exKindTagGroup.Path+"/"+url.PathEscape(id), map[string]any{"name": name})
}

// ---------------------------------------------------------------------------------------------
// move

func exRunMove(c *app.Ctx, kind wrKind) error {
	if !c.Has("to-index") {
		return app.Usage("--to-index is required", "0 moves it first among its siblings")
	}

	n, err := c.Int("to-index", -1)

	if err != nil || n < 0 {
		return app.Usage("--to-index is a whole number, 0 or more", "")
	}

	id, err := wrPathId(c, kind, c.Args[0])

	if err != nil {
		return err
	}

	return wrFlow(c, "POST", kind.Path+"/"+url.PathEscape(id)+"/move", map[string]any{"to_index": n})
}

// ---------------------------------------------------------------------------------------------
// templates

func exTemplateFlags(add bool) []app.Flag {
	req := func(s string) string {
		if add {
			return s + " (required)"
		}

		return s
	}

	flags := []app.Flag{
		{Name: "name", Value: "text", Help: req("the template's name")},
		{Name: "type", Value: "expense|income|transfer", Help: req("the transaction type") + " (transfer is implied by --to-account)"},
		{Name: "account", Value: "id|name", Help: req("the account")},
		{Name: "amount", Value: "hundredths", Help: "integer hundredths (1250 = 12.50)"},
		{Name: "category", Value: "id|name", Help: req("a secondary category matching the type")},
		{Name: "to-account", Value: "id|name", Help: "transfer: the destination account"},
		{Name: "to-amount", Value: "hundredths", Help: "transfer: the amount that arrives, in the destination's currency"},
		{Name: "tag", Value: "id|name", Repeat: true, Help: "tag it (repeatable)"},
		{Name: "comment", Value: "text", Help: "comment on the transactions made from it"},
		{Name: "hide-amount", Help: "hide the amount in the app's UI"},
	}

	if !add {
		flags = append(flags, app.Flag{Name: "clear-tags", Help: "remove every tag"})
	} else {
		flags = append(flags, app.Flag{Name: "idempotency-key", Value: "key", Help: "a retry with the same key returns the original result"})
	}

	return flags
}

func exTemplateBody(c *app.Ctx, add bool) (map[string]any, error) {
	body := map[string]any{}

	if add {
		body["kind"] = "normal"

		for _, f := range []string{"name", "account", "category"} {
			if strings.TrimSpace(c.String(f)) == "" {
				return nil, app.Usage("--"+f+" is required", "ezbk help templates add")
			}
		}
	}

	if c.Has("name") {
		body["name"] = c.String("name")
	}

	typ := strings.ToLower(strings.TrimSpace(c.String("type")))

	if c.Has("to-account") {
		if typ != "" && typ != "transfer" {
			return nil, app.Usage("--to-account makes a transfer; drop --type "+typ, "")
		}

		typ = "transfer"
	}

	switch typ {
	case "":
		if add {
			return nil, app.Usage("--type is required (expense, income or transfer)", "ezbk help templates add")
		}
	case "expense", "income", "transfer":
		body["type"] = typ
	default:
		return nil, app.Usage("--type is expense, income or transfer", "")
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

	for flag, key := range map[string]string{"amount": "amount", "to-amount": "destination_amount"} {
		n, ok, err := c.Amount(flag)

		if err != nil {
			return nil, err
		}

		if ok {
			if n < 0 {
				return nil, app.Usage("--"+flag+" is positive; --type carries the direction", "")
			}

			body[key] = n
		}
	}

	if tags := c.Strings("tag"); len(tags) > 0 {
		if c.Bool("clear-tags") {
			return nil, app.Usage("--tag and --clear-tags contradict each other", "")
		}

		wrSetRefs(body, "tag", tags)
	} else if c.Bool("clear-tags") {
		body["tag_ids"] = []string{}
	}

	if c.Has("comment") {
		body["comment"] = c.String("comment")
	}

	if c.Bool("hide-amount") {
		body["hide_amount"] = true
	}

	return body, nil
}

func exRunTemplateAdd(c *app.Ctx) error {
	body, err := exTemplateBody(c, true)

	if err != nil {
		return err
	}

	return wrFlow(c, "POST", wrPathTemplates, body)
}

func exRunTemplateEdit(c *app.Ctx) error {
	body, err := exTemplateBody(c, false)

	if err != nil {
		return err
	}

	if len(body) == 0 {
		return app.Usage("nothing to change", "ezbk help templates edit")
	}

	id, err := wrPathId(c, exKindTemplate, c.Args[0])

	if err != nil {
		return err
	}

	return wrFlow(c, "PATCH", wrPathTemplates+"/"+url.PathEscape(id), body)
}

// ---------------------------------------------------------------------------------------------
// insights

func exInsightBody(c *app.Ctx, add bool) (map[string]any, error) {
	body := map[string]any{}

	if c.Has("name") {
		if strings.TrimSpace(c.String("name")) == "" {
			return nil, app.Usage("--name cannot be empty", "")
		}

		body["name"] = c.String("name")
	} else if add {
		return nil, app.Usage("--name is required", "ezbk help insights add")
	}

	if c.Has("definition") {
		raw, err := exReadJSONArg(c.String("definition"), os.Stdin)

		if err != nil {
			return nil, app.Usage("--definition: "+err.Error(), "pass a JSON object, @file.json, or - for stdin")
		}

		if !strings.HasPrefix(string(raw), "{") {
			return nil, app.Usage("--definition must be a JSON object", "")
		}

		body["definition"] = raw
	} else if add {
		return nil, app.Usage("--definition is required", "ezbk insights show <id> --format json prints an existing definition to start from")
	}

	return body, nil
}

func exRunInsightAdd(c *app.Ctx) error {
	body, err := exInsightBody(c, true)

	if err != nil {
		return err
	}

	return wrFlow(c, "POST", exKindInsight.Path, body)
}

func exRunInsightEdit(c *app.Ctx) error {
	body, err := exInsightBody(c, false)

	if err != nil {
		return err
	}

	if len(body) == 0 {
		return app.Usage("nothing to change: pass --name or --definition", "")
	}

	id, err := wrPathId(c, exKindInsight, c.Args[0])

	if err != nil {
		return err
	}

	return wrFlow(c, "PATCH", exKindInsight.Path+"/"+url.PathEscape(id), body)
}

// ---------------------------------------------------------------------------------------------
// accounts / transactions extras

func exRunAccountProperties(c *app.Ctx) error {
	id, err := wrPathId(c, wrKindAccount, c.Args[0])

	if err != nil {
		return err
	}

	env, err := c.Call("GET", wrPathAccounts+"/"+url.PathEscape(id)+"/properties", nil, nil)

	if err != nil {
		return err
	}

	if c.Format == render.FormatJSON {
		return c.Emit(env, render.View{})
	}

	data := orData(env)
	cur := orStr(data, "currency")
	flat := map[string]any{}

	for k, v := range data {
		switch k {
		case "balanceModifications", "accountIds":
			continue
		}

		flat[k] = v
	}

	if mods, ok := data["balanceModifications"].([]any); ok {
		parts := []string{}

		for _, m := range orMaps(mods) {
			amount := "?"

			if n, ok := render.ToInt64(m["amount"]); ok {
				amount = render.Money(n, orStr(m, "currency", cur))
			}

			parts = append(parts, orStr(m, "date")+" "+amount)
		}

		flat["balanceModifications"] = strings.Join(parts, "; ")
	}

	if c.Format == render.FormatCSV {
		rows := [][]string{}

		for k, v := range exFlatten("", flat) {
			rows = append(rows, []string{k, v})
		}

		exSortPairs(rows)

		return c.EmitCSV([]string{"key", "value"}, rows)
	}

	return render.KeyValues(c.Out, flat, nil)
}

func exRunEdge(c *app.Ctx, which string) error {
	q := url.Values{}

	if v := c.String("account"); v != "" {
		value, isId := wrRefKind(v)

		if isId {
			q.Set("account_id", value)
		} else {
			q.Set("account_name", value)
		}
	}

	env, err := c.Call("GET", "/transactions/"+which, q, nil)

	if err != nil {
		return err
	}

	if c.Format == render.FormatJSON {
		return c.Emit(env, render.View{})
	}

	t, _ := orData(env)["transaction"].(map[string]any)

	if t == nil {
		c.Info("no transactions")
		return nil
	}

	row := orTxnDisplay(t, c.Location())

	if c.Format == render.FormatCSV {
		return render.CSV(c.Out, []map[string]any{row}, orTxnCSVColumns)
	}

	return render.Table(c.Out, []map[string]any{row}, orTxnTableColumns)
}

func exRunMoveAll(c *app.Ctx) error {
	from, to := strings.TrimSpace(c.String("from")), strings.TrimSpace(c.String("to"))

	if from == "" || to == "" {
		return app.Usage("--from and --to are both required", "ezbk help transactions move-all")
	}

	body := map[string]any{}
	wrSetRef(body, "from_account", from)
	wrSetRef(body, "to_account", to)

	return wrFlow(c, "POST", "/transactions/move-all", body)
}

// ---------------------------------------------------------------------------------------------
// batch / capabilities

// exBatchFile is what `ezbk batch` reads: {"operations":[…], "summary":"…"} or a bare array
func exBatchFile(raw json.RawMessage) (map[string]any, error) {
	var ops []json.RawMessage

	if err := json.Unmarshal(raw, &ops); err == nil {
		return map[string]any{"operations": ops}, nil
	}

	var obj struct {
		Operations []json.RawMessage `json:"operations"`
		Summary    string            `json:"summary"`
	}

	if err := json.Unmarshal(raw, &obj); err != nil || obj.Operations == nil {
		return nil, fmt.Errorf(`expected [{"op":"POST /transactions","args":{…}}, …] or {"operations":[…]}`)
	}

	body := map[string]any{"operations": obj.Operations}

	if obj.Summary != "" {
		body["summary"] = obj.Summary
	}

	return body, nil
}

func exRunBatch(c *app.Ctx) error {
	src := c.Args[0]

	if src != "-" && !strings.HasPrefix(src, "@") {
		src = "@" + src
	}

	raw, err := exReadJSONArg(src, os.Stdin)

	if err != nil {
		return app.Usage("the batch file: "+err.Error(), "ezbk batch ops lists what a batch may contain")
	}

	body, err := exBatchFile(raw)

	if err != nil {
		return app.Usage("the batch file: "+err.Error(), "ezbk batch ops lists what a batch may contain")
	}

	if ops, _ := body["operations"].([]json.RawMessage); len(ops) == 0 {
		return app.Usage("the batch has no operations", "")
	}

	if s := c.String("summary"); s != "" {
		body["summary"] = s
	}

	return wrFlow(c, "POST", "/batch", body)
}

func exRunBatchOps(c *app.Ctx) error {
	env, err := c.Call("GET", "/batch/ops", nil, nil)

	if err != nil {
		return err
	}

	return c.Emit(env, render.View{Rows: "ops", Columns: []render.Column{
		{Header: "Op", Key: "op"},
		{Header: "Summary", Key: "summary"},
	}})
}

func exRunCapabilities(c *app.Ctx) error {
	env, err := c.Call("GET", "/capabilities", nil, nil)

	if err != nil {
		return err
	}

	if c.Format == render.FormatJSON {
		return c.Emit(env, render.View{})
	}

	var rows []map[string]any

	for _, r := range orMaps(asList(orData(env)["routes"])) {
		methods := asStrings(r["methods"])
		tiers, _ := r["tier"].(map[string]any)
		status, _ := r["status"].(map[string]any)

		for _, m := range methods {
			rows = append(rows, map[string]any{
				"method": m, "path": r["path"], "tier": tiers[m], "status": status[m],
			})
		}
	}

	return orEmitRows(c, env, rows, []render.Column{
		{Header: "METHOD", Key: "method"},
		{Header: "PATH", Key: "path"},
		{Header: "TIER", Key: "tier"},
		{Header: "STATUS", Key: "status"},
	}, "no routes")
}

// ---------------------------------------------------------------------------------------------
// statements extras

func exRootQuery(c *app.Ctx) (*asRoot, url.Values, error) {
	r, err := asRootFor(c)

	if err != nil {
		return nil, nil, err
	}

	q := url.Values{"root": {r.Root}}

	if v := c.String("manifest-path"); v != "" {
		q.Set("manifest_path", v)
	}

	return r, q, nil
}

func exRunMapShow(c *app.Ctx) error {
	_, q, err := exRootQuery(c)

	if err != nil {
		return err
	}

	env, err := c.Call("GET", "/ingest/map", q, nil)

	if err != nil {
		return err
	}

	if !c.Quiet() {
		data := orData(env)

		if unmapped := asStrings(data["unmapped_manifest_accounts"]); len(unmapped) > 0 {
			c.Info("unmapped manifest accounts: %s", strings.Join(unmapped, ", "))
		}
	}

	return c.Emit(env, render.View{Rows: "map", Empty: "the map is empty: ezbk statements accounts --write --yes, or ezbk statements map set KEY=ID", Columns: []render.Column{
		{Header: "Account key", Key: "account_key"},
		{Header: "Account id", Key: "account_id"},
		{Header: "Name", Key: "current_name"},
		{Header: "Currency", Key: "current_currency"},
		{Header: "Side", Key: "side"},
		{Header: "Status", Key: "status"},
		{Header: "Source", Key: "source"},
	}})
}

func exRunMapSet(c *app.Ctx) error {
	pairs, err := exParseKeyIds(c.Args)

	if err != nil {
		return app.Usage(err.Error(), "ezbk statements map set CHASE/CHK/1234=3843979048535457792 …")
	}

	r, err := asResolveRoot(c.String("path"), "")

	if err != nil {
		return err
	}

	body := map[string]any{"root": r.Root, "map": pairs}

	if v := c.String("manifest-path"); v != "" {
		body["manifest_path"] = v
	}

	if c.Bool("replace") {
		body["replace"] = true
	}

	return wrFlow(c, "PUT", "/ingest/map", body)
}

func exRunMapInfer(c *app.Ctx) error {
	r, err := asRootFor(c)

	if err != nil {
		return err
	}

	body := map[string]any{"root": r.Root}

	if v := c.String("manifest-path"); v != "" {
		body["manifest_path"] = v
	}

	env, err := c.Call("POST", "/ingest/map/infer", nil, body)

	if err != nil {
		return err
	}

	return c.Emit(env, render.View{Rows: "proposals", Empty: "nothing to propose", Columns: []render.Column{
		{Header: "Account key", Key: "account_key"},
		{Header: "Proposal", Key: "proposal"},
		{Header: "Confidence", Key: "confidence"},
		{Header: "Would create", Key: "would_create", Kind: render.KBool},
		{Header: "Reason", Key: "reason"},
	}})
}

func exRunRows(c *app.Ctx) error {
	_, q, err := exRootQuery(c)

	if err != nil {
		return err
	}

	for _, f := range []string{"mode", "status"} {
		if v := c.String(f); v != "" {
			q.Set(f, v)
		}
	}

	for f, mode := range map[string]wrDateMode{"start": wrDateStart, "end": wrDateEnd} {
		if v := c.String(f); v != "" {
			d, resolved, err := wrResolveDate(v, mode, c.Location(), wrNow())

			if err != nil {
				return app.Usage("--"+f+": "+err.Error(), "")
			}

			if resolved {
				c.Info("dates: --%s %s → %s (%s)", f, v, d, c.Location())
			}

			q.Set(f, d)
		}
	}

	for _, f := range []string{"limit", "offset"} {
		if c.Has(f) {
			n, err := c.Int(f, 0)

			if err != nil || n < 0 {
				return app.Usage("--"+f+" is a whole number", "")
			}

			q.Set(f, strconv.Itoa(n))
		}
	}

	app.AddList(q, "account", c.Strings("account"))

	env, err := c.Call("GET", "/ingest/rows", q, nil)

	if err != nil {
		return err
	}

	if !c.Quiet() {
		data := orData(env)

		if counts, ok := data["status_counts"].(map[string]any); ok && len(counts) > 0 {
			parts := []string{}

			for k, v := range counts {
				parts = append(parts, fmt.Sprintf("%s %v", k, v))
			}

			exSortStrings(parts)
			c.Info("%v rows: %s", data["total"], strings.Join(parts, ", "))
		}
	}

	return c.Emit(env, render.View{Rows: "rows", Empty: "no rows", Columns: []render.Column{
		{Header: "Account key", Key: "account_key"},
		{Header: "Date", Key: "date"},
		{Header: "Amount", Key: "amount", Kind: render.KMoney, CurrencyKey: "currency"},
		{Header: "Description", Key: "description"},
		{Header: "Status", Key: "status"},
		{Header: "Transaction", Key: "transaction_id"},
		{Header: "Import id", Key: "import_id"},
		{Header: "Source", Key: "source_file"},
	}})
}

func exSortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

func exRunRuns(c *app.Ctx) error {
	q := url.Values{}

	if c.Has("limit") {
		n, err := c.Int("limit", 50)

		if err != nil || n <= 0 {
			return app.Usage("--limit is a positive whole number", "")
		}

		q.Set("limit", strconv.Itoa(n))
	}

	env, err := c.Call("GET", "/ingest/runs", q, nil)

	if err != nil {
		return err
	}

	return c.Emit(env, render.View{Rows: "runs", Empty: "no import runs yet", Columns: []render.Column{
		{Header: "Run", Key: "run_id"},
		{Header: "Kind", Key: "kind"},
		{Header: "Finished", Key: "finished_at"},
		{Header: "Outcome", Key: "outcome"},
		{Header: "Created", Key: "created", Kind: render.KInt},
		{Header: "Linked", Key: "linked", Kind: render.KInt},
		{Header: "Reimported", Key: "reimported", Kind: render.KInt},
		{Header: "Journal", Key: "journal_id"},
	}})
}

func exRunRunShow(c *app.Ctx) error {
	id := strings.TrimSpace(c.Args[0])

	if id == "" || strings.ContainsAny(id, "/\\") {
		return app.Usage(fmt.Sprintf("%q is not a run id", id), "ezbk statements runs lists them")
	}

	env, err := c.Call("GET", "/ingest/runs/"+url.PathEscape(id), nil, nil)

	if err != nil {
		return err
	}

	if c.Format == render.FormatJSON {
		return c.Emit(env, render.View{})
	}

	data := orData(env)
	run, _ := data["run"].(map[string]any)

	if run == nil {
		run = map[string]any{}
	}

	if !c.Quiet() {
		c.Info("run %s: %v — created %v, linked %v, reimported %v (journal %v)", orStr(run, "run_id"), run["outcome"], run["created"], run["linked"], run["reimported"], run["journal_id"])
	}

	fallback := orMaps(asList(run["fallback_transactions"]))
	rows := make([]map[string]any, 0, len(fallback))

	for _, f := range fallback {
		rows = append(rows, f)
	}

	if len(rows) == 0 {
		c.Info("no rows went to a fallback category")
	}

	return orEmitRows(c, env, rows, []render.Column{
		{Header: "Transaction", Key: "transaction_id"},
		{Header: "Account key", Key: "account_key"},
		{Header: "Date", Key: "date"},
		{Header: "Kind", Key: "kind"},
		{Header: "Fallback category", Key: "category_id"},
	}, "")
}

func exRunRoots(c *app.Ctx) error {
	env, err := c.Call("GET", "/ingest/roots", nil, nil)

	if err != nil {
		return err
	}

	return c.Emit(env, render.View{Rows: "roots", Empty: "no statements root is configured on the server", Columns: []render.Column{
		{Header: "Root", Key: "root"},
		{Header: "Source", Key: "source"},
		{Header: "Exists", Key: "exists", Kind: render.KBool},
		{Header: "Mode", Key: "mode"},
		{Header: "Mapped", Key: "mapped_accounts", Kind: render.KInt},
	}})
}

func exRunConverters(c *app.Ctx) error {
	env, err := c.Call("GET", "/ingest/converters", nil, nil)

	if err != nil {
		return err
	}

	return c.Emit(env, render.View{Rows: "converters", Columns: []render.Column{
		{Header: "File type", Key: "file_type"},
		{Header: "Format", Key: "format"},
		{Header: "Extensions", Key: "extensions"},
		{Header: "Enabled", Key: "enabled", Kind: render.KBool},
		{Header: "Currency from", Key: "currency_from"},
		{Header: "Bank ids", Key: "bank_ids"},
		{Header: "Needs", Key: "needs"},
	}})
}

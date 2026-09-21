package commands

// admin.go — ADMIN (cli.mdx §6, apis.mdx §10.10 and every family's DELETE): the verbs that can lose
// data. Each needs three things: --write, --yes, and a server started with the admin tier
// (`ezbk up --allow-admin`). Without --write each one is a dry run that prints exactly what would
// be deleted — through the same two-step protocol as every write (app.WriteFlow): preview, confirm
// token, re-resolve at apply time.
//
// Deletes take ids only, never names: a delete is the one place a case-insensitive name match must
// not be allowed to pick the row.

import (
	"fmt"
	"net/url"
	"strings"

	"github.com/BryanStarbuck/ezbookkeeping/cli/internal/app"
	"github.com/BryanStarbuck/ezbookkeeping/cli/internal/exitcode"
	"github.com/BryanStarbuck/ezbookkeeping/cli/internal/render"
)

const orGroupAdmin = "ADMIN"

// orDeleteSpec is one entity family's delete verb
type orDeleteSpec struct {
	verb    string
	noun    string
	path    string // the collection path; the id is appended
	listCmd string
	summary string
}

var orDeleteSpecs = []orDeleteSpec{
	{"categories delete", "category", "/categories", "ezbk categories list --include-hidden", "delete a category (refused while transactions use it, naming the count)"},
	{"tags delete", "tag", "/tags", "ezbk tags list --include-hidden", "delete a tag (it is removed from every transaction carrying it)"},
	{"tag-groups delete", "tag group", "/tag-groups", "ezbk tag-groups list", "delete a tag group"},
	{"templates delete", "template", "/templates", "ezbk templates list --include-hidden (add --scheduled for schedules)", "delete a template or a scheduled transaction"},
	{"insights delete", "saved insight", "/insights", "ezbk insights list --include-hidden", "delete a saved Insights Explorer definition"},
}

func init() {
	verbs := []app.Verb{
		{
			Name: "transactions delete", Group: orGroupAdmin, Args: "<id>...", MinArgs: 1, MaxArgs: -1,
			Summary: "delete transactions by id (a transfer is one id); needs --write --yes and the admin tier",
			Run:     orRunTransactionsDelete,
		},
		{
			Name: "accounts delete", Group: orGroupAdmin, Args: "<id>", MinArgs: 1, MaxArgs: 1,
			Summary: "delete an account and its transactions, or one sub-account with --sub-account; needs --write --yes",
			Flags: []app.Flag{
				{Name: "sub-account", Value: "id", Help: "delete only this sub-account of the account"},
			},
			Run: orRunAccountsDelete,
		},
		{
			Name: "admin clear-data", Group: orGroupAdmin, MaxArgs: 0,
			Summary: "clear the books: --scope transactions|all, or transactions_of_account --account ID; needs --write --yes",
			Flags: []app.Flag{
				{Name: "scope", Value: "transactions|all|transactions_of_account", Help: "what to clear (required)"},
				{Name: "account", Value: "id", Help: "the account, for --scope transactions_of_account"},
			},
			Run: orRunClearData,
		},
		{
			Name: "admin sessions list", Group: orGroupAdmin, MaxArgs: 0,
			Summary: "the bound user's upstream sessions and API/MCP tokens (never the token values); admin tier",
			Run:     orRunSessionsList,
		},
		{
			Name: "admin sessions revoke", Group: orGroupAdmin, Args: "<token-id>", MinArgs: 1, MaxArgs: 1,
			Summary: "revoke one upstream session or token; needs --write --yes",
			Run:     orRunSessionsRevoke,
		},
		{
			Name: "admin pictures prune", Group: orGroupAdmin, MaxArgs: 0,
			Summary: "delete uploaded transaction pictures no transaction references; needs --write --yes",
			Flags: []app.Flag{
				{Name: "older-than-days", Value: "N", Help: "only uploads older than N days (server default 1 — a browser may still be attaching a fresh one)"},
				{Name: "id", Value: "picture-id", Repeat: true, Help: "only these unused pictures (repeatable)"},
			},
			Run: orRunPicturesPrune,
		},
	}

	for _, spec := range orDeleteSpecs {
		spec := spec
		verbs = append(verbs, app.Verb{
			Name: spec.verb, Group: orGroupAdmin, Args: "<id>", MinArgs: 1, MaxArgs: 1,
			Summary: spec.summary + "; needs --write --yes",
			Run:     func(c *app.Ctx) error { return orRunSimpleDelete(c, spec) },
		})
	}

	app.Register(verbs...)
}

// orTxnDeleteView lays out the transaction delete preview (preview.rows: one summary per API
// transaction — a transfer is one row, shown by its source side)
var orTxnDeleteView = render.View{
	Rows: "preview.rows",
	Columns: []render.Column{
		{Header: "DATE", Key: "date"},
		{Header: "TYPE", Key: "typeName"},
		{Header: "ACCOUNT", Key: "sourceAccountName"},
		{Header: "CATEGORY", Key: "categoryName"},
		{Header: "AMOUNT", Key: "sourceAmount", Kind: render.KMoney, CurrencyKey: "sourceCurrency"},
		{Header: "COMMENT", Key: "comment"},
		{Header: "ID", Key: "id"},
	},
	Empty: "nothing to delete (the ids are already gone)",
}

// orSessionView lays out a session revoke preview
var orSessionView = render.View{
	Rows: "preview.session",
	Columns: []render.Column{
		{Header: "TOKEN ID", Key: "tokenId"},
		{Header: "KIND", Key: "kind"},
		{Header: "LAST SEEN", Key: "lastSeenAt"},
		{Header: "USER AGENT", Key: "userAgent"},
	},
	Empty: "no such session (already revoked?)",
}

// orPicturesView lays out the unused-pictures preview
var orPicturesView = render.View{
	Rows: "preview.pictures",
	Columns: []render.Column{
		{Header: "PICTURE ID", Key: "pictureId"},
		{Header: "EXT", Key: "extension"},
		{Header: "UPLOADED", Key: "createdAt"},
	},
	Empty: "no unused pictures",
}

// orAdminGuard enforces the CLI's half of the admin contract before any call: --write needs --yes
func orAdminGuard(c *app.Ctx) error {
	if c.Bool("write") && !c.Bool("yes") {
		return app.Usage(fmt.Sprintf("`ezbk %s` can lose data and needs --write --yes", c.Verb.Name),
			"run it without --write first to see exactly what it would delete, then add --write --yes")
	}

	return nil
}

// orAdminFlow runs an admin write through the two-step protocol. Without --write it is the dry run.
// A zero view prints the whole answer as key/value pairs (table) — the preview's shape differs per
// family and every field of it is worth reading before a delete.
func orAdminFlow(c *app.Ctx, method, path string, body map[string]any, view render.View) error {
	if err := orAdminGuard(c); err != nil {
		return err
	}

	if c.Bool("write") {
		c.Info("target %s — %s %s (admin tier)", c.BaseURL(), method, path)
	}

	var err error

	if len(view.Columns) == 0 || c.Bool("write") {
		// the dedicated views lay out a PREVIEW; an applied answer (or a verb with no dedicated
		// view) prints the same flat preview/result rows every write verb prints
		err = wrFlow(c, method, path, body)
	} else {
		err = c.WriteFlow(method, path, body, view)
	}

	return orExplainAdminError(err)
}

// orExplainAdminError turns the plane's tier refusal into the fix, and keeps exit 7 for it
func orExplainAdminError(err error) error {
	ee, ok := err.(*app.ExitError)

	if !ok {
		return err
	}

	if ee.APICode == "forbidden" && strings.Contains(strings.ToLower(ee.Msg), "admin tier") {
		ee.Code = exitcode.TierRefused

		if ee.Hint == "" {
			ee.Hint = "restart with the admin tier: ezbk stop && ezbk up --allow-write --allow-admin"
		}
	}

	return ee
}

// orRequireID refuses anything but a decimal id
func orRequireID(noun, v, listCmd string) (string, error) {
	v = strings.TrimSpace(v)

	if !orIsID(v) {
		return "", app.Usage(fmt.Sprintf("%q is not a %s id — deletes take ids, never names", v, noun), listCmd)
	}

	return v, nil
}

func orRunTransactionsDelete(c *app.Ctx) error {
	seen := map[string]bool{}
	var ids []string

	for _, a := range c.Args {
		for _, part := range strings.Split(a, ",") {
			part = strings.TrimSpace(part)

			if part == "" {
				continue
			}

			id, err := orRequireID("transaction", part, "ezbk transactions list")

			if err != nil {
				return err
			}

			if !seen[id] {
				seen[id] = true
				ids = append(ids, id)
			}
		}
	}

	if len(ids) == 0 {
		return app.Usage("no transaction ids were given", "ezbk transactions delete <id>...")
	}

	if len(ids) == 1 {
		return orAdminFlow(c, "DELETE", "/transactions/"+url.PathEscape(ids[0]), map[string]any{}, orTxnDeleteView)
	}

	return orAdminFlow(c, "DELETE", "/transactions/bulk", map[string]any{"ids": ids}, orTxnDeleteView)
}

func orRunAccountsDelete(c *app.Ctx) error {
	id, err := orRequireID("account", c.Args[0], "ezbk accounts list --include-hidden")

	if err != nil {
		return err
	}

	path := "/accounts/" + url.PathEscape(id)

	if sub := c.String("sub-account"); sub != "" {
		subID, err := orRequireID("sub-account", sub, "ezbk accounts show "+id)

		if err != nil {
			return err
		}

		path += "/sub-accounts/" + url.PathEscape(subID)
	} else if !c.Bool("write") {
		c.Info("deleting an account deletes every transaction in it; transfers to other accounts lose their other side")
	}

	return orAdminFlow(c, "DELETE", path, map[string]any{}, render.View{})
}

func orRunSimpleDelete(c *app.Ctx, spec orDeleteSpec) error {
	id, err := orRequireID(spec.noun, c.Args[0], spec.listCmd)

	if err != nil {
		return err
	}

	return orAdminFlow(c, "DELETE", spec.path+"/"+url.PathEscape(id), map[string]any{}, render.View{})
}

var orClearScopes = map[string]bool{"transactions": true, "all": true, "transactions_of_account": true}

func orRunClearData(c *app.Ctx) error {
	scope := strings.ToLower(strings.ReplaceAll(strings.TrimSpace(c.String("scope")), "-", "_"))

	if scope == "" {
		return app.Usage("`ezbk admin clear-data` needs --scope transactions|all|transactions_of_account", "ezbk help admin clear-data")
	}

	if !orClearScopes[scope] {
		return app.Usage(fmt.Sprintf("--scope %q is not transactions, all or transactions_of_account", c.String("scope")), "ezbk help admin clear-data")
	}

	body := map[string]any{"scope": scope}
	acct := c.String("account")

	switch {
	case scope == "transactions_of_account" && acct == "":
		return app.Usage("--scope transactions_of_account needs --account <id>", "ezbk accounts list")
	case scope != "transactions_of_account" && acct != "":
		return app.Usage("--account only goes with --scope transactions_of_account", "ezbk help admin clear-data")
	case acct != "":
		id, err := orRequireID("account", acct, "ezbk accounts list --include-hidden")

		if err != nil {
			return err
		}

		body["account_id"] = id
	}

	if scope == "all" && !c.Bool("write") {
		c.Info("--scope all clears every account, transaction, category, tag, template and picture of the bound user")
	}

	return orAdminFlow(c, "POST", "/admin/data/clear", body, render.View{})
}

func orRunSessionsList(c *app.Ctx) error {
	env, err := c.Call("GET", "/admin/sessions", nil, nil)

	if err != nil {
		return orExplainAdminError(err)
	}

	rows := orFindRows(orData(env), "sessions", "tokens")
	out := make([]map[string]any, 0, len(rows))

	for _, r := range rows {
		kind := orStr(r, "kind")

		if kind == "" {
			kind = orTokenTypeName(orVal(r, "tokenType", "type"))
		}

		out = append(out, map[string]any{
			"tokenId":   orStr(r, "tokenId", "id"),
			"type":      kind,
			"userAgent": orStr(r, "userAgent"),
			"lastSeen":  orUnixDisplay(orVal(r, "lastSeen", "lastSeenTime"), c.Location(), "2006-01-02 15:04 MST"),
			"current":   r["isCurrent"],
		})
	}

	return orEmitRows(c, env, out, []render.Column{
		{Header: "TOKEN ID", Key: "tokenId"},
		{Header: "TYPE", Key: "type"},
		{Header: "LAST SEEN", Key: "lastSeen"},
		{Header: "CURRENT", Key: "current", Kind: render.KBool},
		{Header: "USER AGENT", Key: "userAgent"},
	}, "no upstream sessions")
}

func orRunSessionsRevoke(c *app.Ctx) error {
	tok := strings.TrimSpace(c.Args[0])

	if tok == "" || strings.ContainsAny(tok, "/?# ") {
		return app.Usage(fmt.Sprintf("%q is not a token id", c.Args[0]), "ezbk admin sessions list")
	}

	return orAdminFlow(c, "DELETE", "/admin/sessions/"+url.PathEscape(tok), map[string]any{}, orSessionView)
}

func orRunPicturesPrune(c *app.Ctx) error {
	body := map[string]any{}

	if c.Has("older-than-days") {
		n, err := c.Int("older-than-days", 1)

		if err != nil {
			return err
		}

		if n < 0 {
			return app.Usage("--older-than-days must be 0 or more", "ezbk admin pictures prune --older-than-days 7")
		}

		body["older_than_days"] = n
	}

	var ids []string

	for _, v := range c.Strings("id") {
		id, err := orRequireID("picture", v, "ezbk admin pictures prune   (the dry run lists them)")

		if err != nil {
			return err
		}

		ids = append(ids, id)
	}

	if len(ids) > 0 {
		body["ids"] = ids
	}

	return orAdminFlow(c, "DELETE", "/admin/pictures/unused", body, orPicturesView)
}

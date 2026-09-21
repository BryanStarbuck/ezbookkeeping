package commands

// transfers.go — own-account moves as transfers (cli.mdx §6, §10.9a; apis.mdx §10.3.2).
//
//   ezbk transactions transfer-candidates   finds expense/income pairs that are one move (reads)
//   ezbk transactions convert-to-transfer   turns pairs, or single rows against a counter account,
//                                           into transfers (dry run unless --write)
//
// Both are thin: the pairing, the checks and the per-account balance assertion are the server's.

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/BryanStarbuck/ezbookkeeping/cli/internal/app"
	"github.com/BryanStarbuck/ezbookkeeping/cli/internal/errfile"
	"github.com/BryanStarbuck/ezbookkeeping/cli/internal/render"
)

func init() {
	app.Register(
		app.Verb{
			Name: "transactions transfer-candidates", Group: orGroupRead, MaxArgs: 0, Run: xfRunCandidates,
			Summary: "find own-account moves recorded as an expense in one account and an income in another (nothing is changed)",
			Flags: []app.Flag{
				{Name: "account", Value: "id|name", Repeat: true, Help: "only these accounts (repeatable; default every visible account)"},
				{Name: "start", Value: "YYYY-MM-DD", Help: "first day, inclusive"},
				{Name: "end", Value: "YYYY-MM-DD", Help: "last day, inclusive"},
				{Name: "window-days", Value: "N", Help: "how many days apart the two sides may post (default 4, at most 31)"},
				{Name: "no-hint", Help: "also list same-amount candidates without a last-four or transfer-word hint"},
				{Name: "limit", Value: "N", Help: "the most pairs (and ambiguous groups) returned (default 1000)"},
			},
		},
		app.Verb{
			Name: "transactions convert-to-transfer", Group: wrGroup, MaxArgs: 0, Run: xfRunConvert,
			Summary: "turn expense/income pairs (or one row against a counter account) into transfers; balances unchanged, undoable",
			Flags: []app.Flag{
				{Name: "pair", Value: "ID:COUNTER_ID", Repeat: true, Help: "an expense row and its income row (either order) that are one move (repeatable)"},
				{Name: "counter", Value: "ID=ACCOUNT", Repeat: true, Help: "one row and the account (id or name) on the other side of it (repeatable)"},
				{Name: "items", Value: "JSON|@file|-", Help: "the items array as JSON — e.g. the `item`s of transfer-candidates' pairs"},
				{Name: "comment-mode", Value: "both|out", Help: "a pair's comment: both (default: expense ⇄ income) or out (the expense's)"},
				{Name: "transfer-category", Value: "id|name", Help: "the transfer sub-category (default: the first visible one)"},
			},
		},
	)
}

func xfRunCandidates(c *app.Ctx) error {
	body := map[string]any{}

	if accts := c.Strings("account"); len(accts) > 0 {
		wrSetRefs(body, "account", accts)
	}

	for _, k := range []string{"start", "end"} {
		if v := strings.TrimSpace(c.String(k)); v != "" {
			body[k] = v
		}
	}

	for flag, key := range map[string]string{"window-days": "window_days", "limit": "limit"} {
		if c.Has(flag) {
			n, err := c.Int(flag, 0)

			if err != nil {
				return err
			}

			body[key] = n
		}
	}

	if c.Bool("no-hint") {
		body["require_hint"] = false
	}

	env, err := c.Call("POST", "/transactions/transfer-candidates", nil, body)

	if err != nil {
		return err
	}

	if c.Format == render.FormatJSON {
		return c.Emit(env, render.View{})
	}

	data := orData(env)

	if counts, ok := data["counts"].(map[string]any); ok {
		c.Info("%v unambiguous pairs, %v ambiguous groups (%v candidates) — scanned %v expenses and %v incomes", counts["pairs"], counts["ambiguousGroups"], counts["ambiguousCandidates"], counts["scannedExpenses"], counts["scannedIncomes"])
	}

	if data["scanTruncated"] == true {
		c.Warnf("more rows exist than one scan reads; narrow with --start/--end or --account")
	}

	return c.Emit(env, render.View{Rows: "pairs", Columns: []render.Column{
		{Header: "Expense", Key: "expense.id"},
		{Header: "Date", Key: "expense.date"},
		{Header: "From", Key: "expense.accountName"},
		{Header: "Income", Key: "income.id"},
		{Header: "Date", Key: "income.date"},
		{Header: "To", Key: "income.accountName"},
		{Header: "Amount", Key: "expense.amount", Kind: render.KMoney, CurrencyKey: "expense.currency"},
		{Header: "Gap", Key: "dayGap", Kind: render.KInt},
		{Header: "Expense comment", Key: "expense.comment"},
	}})
}

// xfItems builds the items array from --items, --pair and --counter
func xfItems(c *app.Ctx) ([]any, error) {
	var items []any

	if src := strings.TrimSpace(c.String("items")); src != "" {
		raw, err := exReadJSONArg(src, os.Stdin)

		if err != nil {
			return nil, app.Usage("--items: "+err.Error(), "pass a JSON array of {id, counter_id} / {id, counter_account_id|counter_account_name}, @file.json, or - for stdin")
		}

		if err := json.Unmarshal(raw, &items); err != nil {
			errfile.Expected("decoding the --items array", err)
			return nil, app.Usage("--items must be a JSON array", "ezbk help transactions convert-to-transfer")
		}
	}

	for _, p := range c.Strings("pair") {
		a, b, ok := strings.Cut(p, ":")

		if !ok || strings.TrimSpace(a) == "" || strings.TrimSpace(b) == "" {
			return nil, app.Usage(fmt.Sprintf("--pair %q is not ID:COUNTER_ID", p), "ezbk help transactions convert-to-transfer")
		}

		items = append(items, map[string]any{"id": strings.TrimSpace(a), "counter_id": strings.TrimSpace(b)})
	}

	for _, p := range c.Strings("counter") {
		id, acct, ok := strings.Cut(p, "=")

		if !ok || strings.TrimSpace(id) == "" || strings.TrimSpace(acct) == "" {
			return nil, app.Usage(fmt.Sprintf("--counter %q is not ID=ACCOUNT", p), "ezbk help transactions convert-to-transfer")
		}

		item := map[string]any{"id": strings.TrimSpace(id)}
		wrSetRef(item, "counter_account", acct)
		items = append(items, item)
	}

	if len(items) == 0 {
		return nil, app.Usage("nothing to convert: pass --pair ID:COUNTER_ID, --counter ID=ACCOUNT or --items", "ezbk transactions transfer-candidates finds pairs")
	}

	return items, nil
}

func xfRunConvert(c *app.Ctx) error {
	items, err := xfItems(c)

	if err != nil {
		return err
	}

	body := map[string]any{"items": items}

	if m := strings.TrimSpace(c.String("comment-mode")); m != "" {
		body["comment_mode"] = m
	}

	if v := strings.TrimSpace(c.String("transfer-category")); v != "" {
		wrSetRef(body, "transfer_category", v)
	}

	return wrFlow(c, "POST", "/transactions/convert-to-transfer", body)
}

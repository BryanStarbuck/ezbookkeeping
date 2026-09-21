package commands

// statements.go — `ezbk statements …`, the statements pipeline (cli.mdx §10, apis.mdx §14–15).
//
// The CLI's part is small on purpose: resolve the statements root (--path → EZBK_STATEMENTS_DIR →
// ezbookkeeping.statements.root in the credentials file → refuse naming the setting), check every
// path is inside it (the server re-checks), call the ingest route, and print the answer. Parsing,
// both de-duplication layers, the account and category maps, the transfer and match detection and
// the import itself all happen on the server. The CLI never reads a statement's contents (except
// to upload one named file to /ingest/file/plan when it lies outside the root), never writes under
// the root, and never decides what is a duplicate.

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"mime/multipart"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/BryanStarbuck/ezbookkeeping/cli/internal/app"
	"github.com/BryanStarbuck/ezbookkeeping/cli/internal/client"
	"github.com/BryanStarbuck/ezbookkeeping/cli/internal/credentials"
	"github.com/BryanStarbuck/ezbookkeeping/cli/internal/exitcode"
	"github.com/BryanStarbuck/ezbookkeeping/cli/internal/render"
)

const asStmtGroup = "THE STATEMENTS PIPELINE"

// asStagingDir is the only directory under the root anything is ever written to (by the server)
const asStagingDir = ".ezbk-staging"

// ---------------------------------------------------------------------------------------------
// The statements root and path containment (cli.mdx §10.1, §17)
// ---------------------------------------------------------------------------------------------

// asRoot is a resolved statements root and the directory a verb was pointed at inside it
type asRoot struct {
	// Root is the absolute real path of the statements root
	Root string
	// Target is the absolute real path the verb operates on (Root, or a directory inside it)
	Target string
	// Source names where Root came from, for messages
	Source string
}

// asConfiguredRoot returns the root from the environment or the credentials file ("" when neither)
func asConfiguredRoot() (string, string) {
	if v := strings.TrimSpace(os.Getenv("EZBK_STATEMENTS_DIR")); v != "" {
		return v, "EZBK_STATEMENTS_DIR"
	}

	if creds, err := credentials.Read(); err == nil && creds != nil && strings.TrimSpace(creds.StatementsRoot) != "" {
		path, _ := credentials.Path()

		return strings.TrimSpace(creds.StatementsRoot), path + " (ezbookkeeping.statements.root)"
	}

	return "", ""
}

// asExpandHome turns a leading ~ into the home directory
func asExpandHome(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, strings.TrimPrefix(p, "~"))
		}
	}

	return p
}

// asRealPath resolves a path to an absolute path with every symlink followed
func asRealPath(p string) (string, error) {
	abs, err := filepath.Abs(asExpandHome(p))

	if err != nil {
		return "", err
	}

	real, err := filepath.EvalSymlinks(abs)

	if err != nil {
		return "", err
	}

	return filepath.Clean(real), nil
}

// asInside reports whether target is root or lies beneath it (both already real paths)
func asInside(root, target string) bool {
	rel, err := filepath.Rel(root, target)

	if err != nil {
		return false
	}

	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel))
}

// asRefuseNoRoot is the refusal when no root is configured anywhere — it names the setting
func asRefuseNoRoot() error {
	path, _ := credentials.Path()

	return app.Usage("no statements root is configured",
		"pass --path /path/to/statements, or set EZBK_STATEMENTS_DIR, or set ezbookkeeping.statements.root in "+path)
}

// asResolveRoot resolves the root and the verb's target. flagPath is --path, arg the optional
// positional PATH. A configured root (env or credentials) is the boundary: --path and PATH must lie
// inside it. With nothing configured, --path (or PATH) is the root, and the server re-checks it
// against its own configured roots.
func asResolveRoot(flagPath, arg string) (*asRoot, error) {
	configured, source := asConfiguredRoot()
	root := &asRoot{}

	var boundary string

	if configured != "" {
		real, err := asRealPath(configured)

		if err != nil {
			return nil, app.Usage(fmt.Sprintf("the statements root %s (from %s) cannot be opened: %v", configured, source, err), "fix the setting, or pass --path")
		}

		boundary = real
	}

	pick := func(label, p string) (string, error) {
		real, err := asRealPath(p)

		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return "", &app.ExitError{Code: exitcode.NotFound, Msg: fmt.Sprintf("%s %s does not exist", label, p), Hint: "check the path; statements roots are directories"}
			}

			return "", app.Usage(fmt.Sprintf("%s %s cannot be resolved: %v", label, p, err), "")
		}

		if boundary != "" && !asInside(boundary, real) {
			return "", app.Usage(fmt.Sprintf("refused: %s %s is outside the statements root %s (from %s)", label, real, boundary, source),
				"every statements path must lie inside the configured root; change EZBK_STATEMENTS_DIR / ezbookkeeping.statements.root to move the root")
		}

		return real, nil
	}

	switch {
	case flagPath != "":
		real, err := pick("--path", flagPath)

		if err != nil {
			return nil, err
		}

		root.Root, root.Source = real, "--path"
	case boundary != "":
		root.Root, root.Source = boundary, source
	case arg != "":
		// nothing configured: the positional PATH is the root (the server still re-checks it)
		real, err := asRealPath(arg)

		if err != nil {
			return nil, &app.ExitError{Code: exitcode.NotFound, Msg: fmt.Sprintf("PATH %s cannot be opened: %v", arg, err), Hint: "check the path"}
		}

		root.Root, root.Source = real, "PATH"
	default:
		return nil, asRefuseNoRoot()
	}

	if fi, err := os.Stat(root.Root); err != nil || !fi.IsDir() {
		return nil, app.Usage(fmt.Sprintf("the statements root %s is not a readable directory", root.Root), "point --path / EZBK_STATEMENTS_DIR at the directory that holds {ENTITY}/{BANK}/{ACCOUNT}/{YYYY}/{MM}/")
	}

	root.Target = root.Root

	if arg != "" && root.Source != "PATH" {
		real, err := asRealPath(arg)

		if err != nil {
			return nil, &app.ExitError{Code: exitcode.NotFound, Msg: fmt.Sprintf("PATH %s cannot be opened: %v", arg, err), Hint: "check the path"}
		}

		if !asInside(root.Root, real) {
			return nil, app.Usage(fmt.Sprintf("refused: PATH %s is outside the statements root %s (from %s)", real, root.Root, root.Source), "pass a directory inside the root, or change the root with --path")
		}

		root.Target = real
	}

	if rel, _ := filepath.Rel(root.Root, root.Target); rel == asStagingDir || strings.HasPrefix(rel, asStagingDir+string(filepath.Separator)) {
		return nil, app.Usage("refused: "+root.Target+" is the staging directory, not a statements tree", "point at the root or a directory of statements")
	}

	return root, nil
}

// asRel returns p relative to the root (for server arguments that are root-relative)
func (r *asRoot) asRel(p string) string {
	rel, err := filepath.Rel(r.Root, p)

	if err != nil {
		return p
	}

	return filepath.ToSlash(rel)
}

// asScope turns a target directory inside the root into the {ENTITY}/{BANK}/{ACCOUNT}/{YYYY} filter
// its first path components name (cli.mdx §10.1). Deeper components (the month) are ignored.
func (r *asRoot) asScope() (entity, bank, account, year string) {
	rel := r.asRel(r.Target)

	if rel == "." || rel == "" {
		return
	}

	parts := strings.Split(rel, "/")
	dst := []*string{&entity, &bank, &account, &year}

	for i := 0; i < len(parts) && i < len(dst); i++ {
		*dst[i] = parts[i]
	}

	return
}

// asRootFor resolves the root for a verb from its --path flag and optional positional PATH
func asRootFor(c *app.Ctx) (*asRoot, error) {
	arg := ""

	if len(c.Args) > 0 {
		arg = c.Args[0]
	}

	r, err := asResolveRoot(c.String("path"), arg)

	if err != nil {
		return nil, err
	}

	if c.Verbose() {
		c.Info("statements root %s (from %s)", r.Root, r.Source)

		if r.Target != r.Root {
			c.Info("scope %s", r.Target)
		}
	}

	return r, nil
}

// asScopeBody adds the scope filters a target directory implies, plus the explicit flags (which
// win), to a body. Keys follow apis.mdx §14.4: entity, institution, account, year.
func asScopeBody(c *app.Ctx, r *asRoot, body map[string]any) {
	entity, bank, account, year := r.asScope()

	for key, v := range map[string]string{"entity": entity, "institution": bank, "account": account, "year": year} {
		if v != "" {
			body[key] = v
		}
	}

	if v := c.String("entity"); v != "" {
		body["entity"] = v
	}

	if v := c.String("bank"); v != "" {
		body["institution"] = v
	}

	if v := c.String("account"); v != "" {
		body["account"] = v
	}

	if v := c.String("year"); v != "" {
		body["year"] = v
	}
}

// asScopeFilters returns the scope as the scan route's filters (entity, institution, account,
// year) — from the positional PATH and the explicit flags, the flags winning
func asScopeFilters(c *app.Ctx, r *asRoot) map[string]string {
	body := map[string]any{}
	asScopeBody(c, r, body)
	out := map[string]string{}

	for k, v := range body {
		out[k] = fmt.Sprint(v)
	}

	return out
}

// asScopeAccountKeys turns a scope into the account keys the plan, apply, extract and dupes routes
// take as accounts[]. With no scope it returns nil (every account). --only keys pass through as
// given. Otherwise the server's own scan says which accounts the scope holds; an empty answer is
// exit 3.
func asScopeAccountKeys(c *app.Ctx, r *asRoot) ([]string, error) {
	if only := c.Strings("only"); len(only) > 0 {
		return only, nil
	}

	filters := asScopeFilters(c, r)
	delete(filters, "year")

	if len(filters) == 0 {
		return nil, nil
	}

	body := map[string]any{"root": r.Root}

	for k, v := range filters {
		body[k] = v
	}

	if v := c.String("manifest-path"); v != "" {
		body["manifest_path"] = v
	}

	env, err := c.Call("POST", "/ingest/scan", nil, body)

	if err != nil {
		return nil, err
	}

	scan := asMap(env.DataMap()["scan"])
	var keys []string

	for _, a := range asList(scan["accounts"]) {
		if k := asFirstStr(asMap(a), "account_key"); k != "" {
			keys = append(keys, k)
		}
	}

	if len(keys) == 0 {
		var parts []string

		for _, k := range []string{"entity", "institution", "account"} {
			if v := filters[k]; v != "" {
				parts = append(parts, k+"="+v)
			}
		}

		return nil, &app.ExitError{Code: exitcode.NotFound, Msg: "no statement accounts match " + strings.Join(parts, " "), Hint: "ezbk statements scan lists the entities, banks and accounts"}
	}

	if c.Verbose() {
		c.Info("scope: %d account(s): %s", len(keys), strings.Join(keys, ", "))
	}

	return keys, nil
}

// ---------------------------------------------------------------------------------------------
// Flags
// ---------------------------------------------------------------------------------------------

var (
	asFlagPath     = app.Flag{Name: "path", Value: "path", Help: "the statements root (overrides EZBK_STATEMENTS_DIR and the credentials file)"}
	asFlagEntity   = app.Flag{Name: "entity", Value: "E", Help: "only this entity (the first directory level)"}
	asFlagBank     = app.Flag{Name: "bank", Value: "B", Help: "only this bank (the second directory level)"}
	asFlagStmtAcct = app.Flag{Name: "account", Value: "A", Help: "only this account: its directory (Checking_x4021), account key, path or last4"}
	asFlagYear     = app.Flag{Name: "year", Value: "YYYY", Help: "only this year"}
	asFlagManifest = app.Flag{Name: "manifest-path", Value: "REL", Help: "the manifest, relative to the root (default: import/accounts.csv and the other usual names)"}
	asFlagQIF      = app.Flag{Name: "qif-date-order", Value: "ymd|mdy|dmy", Help: "the date order of QIF files (required for QIF; never guessed)"}
	asFlagFbExp    = app.Flag{Name: "fallback-expense", Value: "CATEGORY", Help: "expense category (name or id) for rows whose category matches none of yours"}
	asFlagFbInc    = app.Flag{Name: "fallback-income", Value: "CATEGORY", Help: "income category (name or id) for rows whose category matches none of yours"}
	asFlagFbTrf    = app.Flag{Name: "fallback-transfer", Value: "CATEGORY", Help: "transfer category (name or id) for accepted transfer candidates"}
	asFlagCatMap   = app.Flag{Name: "category-map", Value: "FROM=TO", Repeat: true, Help: "map a statement category name (optionally expense:Name / income:Name) to one of yours, by name or id (repeatable)"}
	asFlagOnly     = app.Flag{Name: "only", Value: "ACCOUNT_KEY", Repeat: true, Help: "limit to these accounts: account key ({entity}/{bank}/{last4}), label, path or last4 (repeatable)"}
	asFlagPrefer   = app.Flag{Name: "prefer", Value: "FILE", Repeat: true, Help: "resolve a statement conflict in favour of this file (repeatable)"}
	asFlagMode     = app.Flag{Name: "mode", Value: "prepared|raw", Help: "override the detected archive mode"}
)

// asScopeFlags are the scope flags the tree-walking verbs share
var asScopeFlags = []app.Flag{asFlagPath, asFlagEntity, asFlagBank, asFlagStmtAcct, asFlagYear, asFlagManifest}

// asQIFOrder validates --qif-date-order and returns the server's qif_* value
func asQIFOrder(v string) (string, error) {
	v = strings.ToLower(strings.TrimSpace(v))
	v = strings.TrimPrefix(v, "qif_")

	switch v {
	case "":
		return "", nil
	case "ymd", "mdy", "dmy":
		return v, nil
	}

	return "", app.Usage(fmt.Sprintf("--qif-date-order %q is not ymd, mdy or dmy", v), "look at a QIF date in the file: 2025-06-30 is ymd, 06/30/2025 is mdy, 30/06/2025 is dmy")
}

// asParsePairs parses repeatable KEY=VALUE flags. Values may contain '='; keys may not be empty.
func asParsePairs(flag string, values []string) (map[string]string, error) {
	out := map[string]string{}

	for _, v := range values {
		i := strings.Index(v, "=")

		if i <= 0 || i == len(v)-1 {
			return nil, app.Usage(fmt.Sprintf("--%s %q is not KEY=VALUE", flag, v), "")
		}

		k, val := strings.TrimSpace(v[:i]), strings.TrimSpace(v[i+1:])

		if prev, dup := out[k]; dup && prev != val {
			return nil, app.Usage(fmt.Sprintf("--%s gives %q twice with different values (%q, %q)", flag, k, prev, val), "")
		}

		out[k] = val
	}

	return out, nil
}

// asPreferPaths resolves --prefer files to root-relative paths, each checked inside the root
func asPreferPaths(c *app.Ctx, r *asRoot) ([]string, error) {
	var out []string

	for _, p := range c.Flags("prefer") {
		cand := p

		if !filepath.IsAbs(asExpandHome(p)) {
			// a relative --prefer is tried against the root first (the way dupes prints it)
			if _, err := os.Stat(filepath.Join(r.Root, p)); err == nil {
				cand = filepath.Join(r.Root, p)
			}
		}

		real, err := asRealPath(cand)

		if err != nil {
			return nil, &app.ExitError{Code: exitcode.NotFound, Msg: "--prefer " + p + " cannot be opened", Hint: "name one of the conflicting statement files `ezbk statements dupes` listed"}
		}

		if !asInside(r.Root, real) {
			return nil, app.Usage("refused: --prefer "+real+" is outside the statements root "+r.Root, "")
		}

		if fi, err := os.Stat(real); err != nil || fi.IsDir() {
			return nil, app.Usage("--prefer names a statement FILE, and "+real+" is a directory", "")
		}

		out = append(out, r.asRel(real))
	}

	return out, nil
}

// asCategoryTypeOf splits a category-map key "expense:Name" into its type and name
func asCategoryTypeOf(key string) (string, string) {
	if i := strings.Index(key, ":"); i > 0 {
		switch t := strings.ToLower(key[:i]); t {
		case "expense", "income", "transfer":
			return t, key[i+1:]
		}
	}

	return "", key
}

// asCategoryArgs resolves --category-map and --fallback-expense/-income/-transfer to the ids the
// ingest routes take (category_map, fallback_category_ids). Names are looked up among the
// categories of the right type; an ambiguous or missing name is refused before the call.
func asCategoryArgs(c *app.Ctx, body map[string]any) error {
	cm, err := asParsePairs("category-map", c.Flags("category-map"))

	if err != nil {
		return err
	}

	if len(cm) > 0 {
		resolved := map[string]string{}
		keys := make([]string, 0, len(cm))

		for k := range cm {
			keys = append(keys, k)
		}

		sort.Strings(keys)

		for _, k := range keys {
			typ, _ := asCategoryTypeOf(k)
			q := url.Values{}

			if typ != "" {
				q.Set("type", typ)
			}

			ids, err := asResolveIdsQ(c, "category", "category-map", "/categories", q, []string{cm[k]})

			if err != nil {
				return err
			}

			resolved[k] = ids[0]
		}

		body["category_map"] = resolved
	}

	fb := map[string]string{}

	for _, side := range []struct{ flag, typ string }{{"fallback-expense", "expense"}, {"fallback-income", "income"}, {"fallback-transfer", "transfer"}} {
		v := c.String(side.flag)

		if v == "" {
			continue
		}

		ids, err := asResolveIdsQ(c, side.typ+" category", side.flag, "/categories", url.Values{"type": {side.typ}}, []string{v})

		if err != nil {
			return err
		}

		fb[side.typ] = ids[0]

		if !asIsId(v) {
			c.Info("--%s %q → category %s", side.flag, v, ids[0])
		}
	}

	if len(fb) > 0 {
		body["fallback_category_ids"] = fb
	}

	return nil
}

// asRangeArgs adds --start/--end (relative spellings resolved locally) to a body
func asRangeArgs(c *app.Ctx, body map[string]any) error {
	var notes []string

	for _, side := range []struct {
		flag  string
		isEnd bool
	}{{"start", false}, {"end", true}} {
		v := c.String(side.flag)

		if v == "" {
			continue
		}

		d, resolved, err := asResolveDay(v, side.isEnd, asNow(c.Location()))

		if err != nil {
			return app.Usage("--"+side.flag+": "+err.Error(), "")
		}

		if resolved {
			notes = append(notes, fmt.Sprintf("--%s %s → %s", side.flag, v, d))
		}

		body[side.flag] = d
	}

	if s, e := asStr(body["start"]), asStr(body["end"]); s != "" && e != "" && e < s {
		return app.Usage(fmt.Sprintf("--end %s is before --start %s", e, s), "swap them")
	}

	if len(notes) > 0 {
		c.Info("dates (%s): %s", c.Location().String(), strings.Join(notes, "; "))
	}

	return nil
}

// asPlanBody builds the /ingest/plan arguments (apis.mdx §14.4) shared by plan and apply, so
// "show me" and "do it" can never select different rows
func asPlanBody(c *app.Ctx, r *asRoot) (map[string]any, error) {
	body := map[string]any{"root": r.Root}

	if v := c.String("manifest-path"); v != "" {
		body["manifest_path"] = v
	}

	if m := strings.ToLower(c.String("mode")); m != "" {
		if m != "prepared" && m != "raw" {
			return nil, app.Usage("--mode must be prepared or raw", "ezbk statements manifest tells you which the tree is")
		}

		body["mode"] = m
	}

	keys, err := asScopeAccountKeys(c, r)

	if err != nil {
		return nil, err
	}

	if len(keys) > 0 {
		body["accounts"] = keys
	}

	if err := asRangeArgs(c, body); err != nil {
		return nil, err
	}

	if err := asCategoryArgs(c, body); err != nil {
		return nil, err
	}

	qif, err := asQIFOrder(c.String("qif-date-order"))

	if err != nil {
		return nil, err
	}

	if qif != "" {
		body["qif_date_order"] = qif
	}

	prefer, err := asPreferPaths(c, r)

	if err != nil {
		return nil, err
	}

	if len(prefer) > 0 {
		body["prefer"] = prefer
	}

	if ids := c.Strings("accept-transfer"); len(ids) > 0 {
		body["accept_transfers"] = ids
	}

	matches, err := asParseMatches(c.Flags("accept-match"))

	if err != nil {
		return nil, err
	}

	if len(matches) > 0 {
		body["accept_matches"] = matches
	}

	if c.Bool("reimport-deleted") {
		body["reimport_deleted"] = true
	}

	return body, nil
}

// asParseMatches parses --accept-match IMPORT_ID=TRANSACTION_ID (ids stay strings — they exceed
// 2^53). The import id is the plan's; it may itself contain '=' so the LAST '=' splits.
func asParseMatches(values []string) ([]map[string]string, error) {
	var out []map[string]string
	seen := map[string]string{}

	for _, v := range values {
		for _, part := range strings.Split(v, ",") {
			part = strings.TrimSpace(part)

			if part == "" {
				continue
			}

			i := strings.LastIndex(part, "=")

			if i <= 0 || i == len(part)-1 {
				return nil, app.Usage(fmt.Sprintf("--accept-match %q is not IMPORT_ID=TRANSACTION_ID", part), "ezbk statements plan lists each possible match as IMPORT_ID=TRANSACTION_ID")
			}

			row, txn := strings.TrimSpace(part[:i]), strings.TrimSpace(part[i+1:])

			if !asIsId(txn) {
				return nil, app.Usage(fmt.Sprintf("--accept-match %q: %q is not a transaction id (a whole-number string)", part, txn), "")
			}

			if prev, dup := seen[row]; dup {
				if prev != txn {
					return nil, app.Usage(fmt.Sprintf("--accept-match links row %s to two transactions (%s, %s)", row, prev, txn), "")
				}

				continue
			}

			seen[row] = txn
			out = append(out, map[string]string{"import_id": row, "transaction_id": txn})
		}
	}

	return out, nil
}

// ---------------------------------------------------------------------------------------------
// Generic helpers over the server's answers (snake_case, apis.mdx §14)
// ---------------------------------------------------------------------------------------------

func asMap(v any) map[string]any {
	m, _ := v.(map[string]any)
	return m
}

func asList(v any) []any {
	l, _ := v.([]any)
	return l
}

// asInt reads the first integer found under any of keys
func asInt(m map[string]any, keys ...string) (int64, bool) {
	for _, k := range keys {
		if n, ok := render.ToInt64(m[k]); ok {
			return n, true
		}
	}

	return 0, false
}

func asIntOr0(m map[string]any, keys ...string) int64 {
	n, _ := asInt(m, keys...)
	return n
}

// asGroupThousands formats a count as 12,007 (a count, not money — no currency)
func asGroupThousands(n int64) string {
	s := strconv.FormatInt(n, 10)
	neg := strings.HasPrefix(s, "-")
	s = strings.TrimPrefix(s, "-")

	var b strings.Builder

	for i, ch := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte(',')
		}

		b.WriteRune(ch)
	}

	if neg {
		return "-" + b.String()
	}

	return b.String()
}

func asN(m map[string]any, keys ...string) string {
	return asGroupThousands(asIntOr0(m, keys...))
}

func orDash(s string) string {
	if s == "" {
		return "—"
	}

	return s
}

// asStrings reads a list of strings (or objects with a message) from a JSON value
func asStrings(v any) []string {
	var out []string

	for _, item := range asList(v) {
		switch t := item.(type) {
		case string:
			out = append(out, t)
		case map[string]any:
			if s := asFirstStr(t, "message", "warning", "text", "name", "account_key"); s != "" {
				out = append(out, s)
			}
		}
	}

	return out
}

// asMoneyOf renders an amount field beside its currency field
func asMoneyOf(m map[string]any, amountKey, currencyKey string) string {
	n, ok := render.ToInt64(m[amountKey])

	if !ok {
		return "—"
	}

	return render.Money(n, asFirstStr(m, currencyKey))
}

// asWithRows wraps synthetic rows (reshaped for a table) in an envelope that keeps the meta
func asWithRows(env *client.Envelope, rows []any) *client.Envelope {
	if rows == nil {
		rows = []any{}
	}

	return asSyntheticEnvelope(env, map[string]any{"rows": rows})
}

// asEmitRows prints the envelope for JSON, and reshaped rows for table/CSV
func asEmitRows(c *app.Ctx, env *client.Envelope, rows []any, cols []render.Column, empty string) error {
	if c.Format == render.FormatJSON {
		return c.Emit(env, render.View{})
	}

	return c.Emit(asWithRows(env, rows), render.View{Rows: "rows", Columns: cols, Empty: empty})
}

// ---------------------------------------------------------------------------------------------
// manifest (cli.mdx §10.2)
// ---------------------------------------------------------------------------------------------

func asRunManifest(c *app.Ctx) error {
	r, err := asRootFor(c)

	if err != nil {
		return err
	}

	q := url.Values{"root": {r.Root}}

	if v := c.String("manifest-path"); v != "" {
		q.Set("manifest_path", v)
	}

	qif, err := asQIFOrder(c.String("qif-date-order"))

	if err != nil {
		return err
	}

	if qif != "" {
		q.Set("qif_date_order", qif)
	}

	env, err := c.Call("GET", "/ingest/manifest", q, nil)

	if err != nil {
		return err
	}

	data := env.DataMap()

	if !c.Quiet() {
		asPrintManifestSummary(c, data)
	}

	return c.Emit(env, render.View{Rows: "accounts", Empty: "the manifest lists no accounts", Columns: []render.Column{
		{Header: "Account key", Key: "account_key"},
		{Header: "Kind", Key: "kind"},
		{Header: "Currency", Key: "currency"},
		{Header: "Defaulted", Key: "currency_defaulted", Kind: render.KBool},
		{Header: "Converter", Key: "files.converter"},
		{Header: "Needs", Key: "files.needs"},
		{Header: "Combined", Key: "files.combined"},
		{Header: "Monthly", Key: "files.monthly", Kind: render.KInt},
		{Header: "Statements", Key: "statements", Kind: render.KInt},
		{Header: "Transactions", Key: "transactions", Kind: render.KInt},
		{Header: "First", Key: "first"},
		{Header: "Last", Key: "last"},
		{Header: "Mapped to", Key: "mapped_account_id"},
	}})
}

func asPrintManifestSummary(c *app.Ctx, data map[string]any) {
	c.Info("mode %s   manifest %s (%s)   root %s   staging %s", orDash(asFirstStr(data, "mode")), orDash(asFirstStr(data, "manifest_path")),
		orDash(asFirstStr(data, "format")), orDash(asFirstStr(data, "root")), orDash(asFirstStr(data, "staging")))

	if t := asMap(data["totals"]); t != nil {
		c.Info("%s accounts (%s mapped), %s statements, %s transactions, %s unreconciled statements, %s with a defaulted currency, %s with no importable file",
			asN(t, "accounts"), asN(t, "mapped"), asN(t, "statements"), asN(t, "transactions"), asN(t, "unreconciled"), asN(t, "currency_defaulted"), asN(t, "without_importable_files"))
	}

	for _, col := range asStrings(data["missing_columns"]) {
		c.Warnf("the manifest has no %q column (it is required; nothing is guessed)", col)
	}

	if cols := asStrings(data["unknown_columns"]); len(cols) > 0 {
		c.Info("ignored manifest columns: %s", strings.Join(cols, ", "))
	}

	if al := asMap(data["aliases_used"]); len(al) > 0 {
		keys := make([]string, 0, len(al))

		for k := range al {
			keys = append(keys, k+"→"+asStr(al[k]))
		}

		sort.Strings(keys)
		c.Info("column aliases used: %s", strings.Join(keys, ", "))
	}

	for _, w := range asStrings(data["warnings"]) {
		c.Warnf("%s", w)
	}

	for _, a := range asList(data["accounts"]) {
		am := asMap(a)

		for _, w := range asStrings(am["warnings"]) {
			c.Warnf("%s: %s", asFirstStr(am, "account_key"), w)
		}
	}
}

// ---------------------------------------------------------------------------------------------
// scan (cli.mdx §10.3)
// ---------------------------------------------------------------------------------------------

func asRunScan(c *app.Ctx) error {
	r, err := asRootFor(c)

	if err != nil {
		return err
	}

	body := map[string]any{"root": r.Root}

	for k, v := range asScopeFilters(c, r) {
		body[k] = v
	}

	if v := c.String("manifest-path"); v != "" {
		body["manifest_path"] = v
	}

	if m := strings.ToLower(c.String("mode")); m != "" {
		if m != "prepared" && m != "raw" {
			return app.Usage("--mode must be prepared or raw", "ezbk statements manifest tells you which the tree is")
		}

		body["mode"] = m
	}

	if qif, err := asQIFOrder(c.String("qif-date-order")); err != nil {
		return err
	} else if qif != "" {
		body["qif_date_order"] = qif
	}

	env, err := c.CallStreamStages("POST", "/ingest/scan", nil, body, "Scanning statements")

	if err != nil {
		return err
	}

	scan := asMap(env.DataMap()["scan"])

	if !c.Quiet() {
		asPrintScanSummary(c, scan)
	}

	return asEmitRows(c, env, asScanRows(scan), []render.Column{
		{Header: "Entity", Key: "entity"},
		{Header: "Bank", Key: "institution"},
		{Header: "Account", Key: "account"},
		{Header: "Years", Key: "years"},
		{Header: "Statements", Key: "statements", Kind: render.KInt},
		{Header: "First", Key: "first"},
		{Header: "Last", Key: "last"},
		{Header: "Missing", Key: "missing", Kind: render.KInt},
		{Header: "Missing months", Key: "missing_months"},
		{Header: "Dup scans", Key: "duplicate_scans", Kind: render.KInt},
		{Header: "Unusable", Key: "unusable", Kind: render.KInt},
	}, "no statements found under that path")
}

// asScanRows lays each scanned account out as one row: entity → bank → account → years, with the
// counts of what the server flagged (lists are counted for the table; --format json has them all)
func asScanRows(scan map[string]any) []any {
	var rows []any

	for _, a := range asList(scan["accounts"]) {
		am := asMap(a)
		var years []string

		for _, y := range asList(am["years"]) {
			ym := asMap(y)
			years = append(years, fmt.Sprintf("%s(%s)", asFirstStr(ym, "year"), asStr(ym["statements"])))
		}

		missing := asStrings(am["missing_months"])
		rows = append(rows, map[string]any{
			"account_key": asFirstStr(am, "account_key"), "entity": asFirstStr(am, "entity"), "institution": asFirstStr(am, "institution"),
			"account": asFirstStr(am, "account"), "years": strings.Join(years, " "), "statements": am["statements"],
			"first": am["first"], "last": am["last"], "missing": len(missing), "missing_months": strings.Join(missing, " "),
			"duplicate_scans": len(asList(am["duplicate_scans"])), "unusable": len(asList(am["unusable"])),
		})
	}

	return rows
}

// asPrintScanSummary prints the scan's headline on stderr: missing months, duplicate scans and
// files with no usable extraction — each one a thing the operator acts on
func asPrintScanSummary(c *app.Ctx, scan map[string]any) {
	if scan == nil {
		return
	}

	t := asMap(scan["totals"])
	c.Info("scan (%s, %s layout): %s files, %s statements in %s accounts — %s account-months missing, %s duplicate scans, %s with no usable extraction",
		orDash(asFirstStr(scan, "mode")), orDash(asFirstStr(scan, "layout")), asN(t, "files"), asN(t, "statements"), asN(t, "accounts"),
		asN(t, "missing_months"), asN(t, "duplicate_scans"), asN(t, "unusable"))

	if n := asIntOr0(t, "unclassified"); n > 0 {
		c.Info("%s files were not recognised as statements, sidecars or importable files (listed in --format json)", asGroupThousands(n))
	}

	if n := asIntOr0(t, "unattributed"); n > 0 {
		c.Warnf("%s statement files could not be attributed to an account (listed in --format json as unattributed)", asGroupThousands(n))
	}

	if tr, _ := scan["truncated"].(bool); tr {
		c.Warnf("the scan stopped at the server's file cap; narrow it with --entity / --bank / --account")
	}

	for _, a := range asList(scan["accounts"]) {
		am := asMap(a)

		for _, w := range asStrings(am["warnings"]) {
			c.Warnf("%s: %s", asFirstStr(am, "account_key"), w)
		}

		for _, d := range asList(am["duplicate_scans"]) {
			dm := asMap(d)
			data, _ := json.Marshal(dm)

			if dm == nil {
				data, _ = json.Marshal(d)
			}

			c.Info("duplicate scan in %s: %s", asFirstStr(am, "account_key"), string(data))
		}
	}
}

// ---------------------------------------------------------------------------------------------
// missing (cli.mdx §10.3) — one ENTITY/BANK/ACCOUNT YYYY-MM per line on stdout
// ---------------------------------------------------------------------------------------------

// asMissingEntry is one missing account-month
type asMissingEntry struct {
	Entity, Bank, Account, Month string
}

func (e asMissingEntry) line() string {
	return e.Entity + "/" + e.Bank + "/" + e.Account + " " + e.Month
}

// asMissingFrom reads the gaps from a coverage answer ({accounts[{entity, institution, account,
// missing[]}]}) or a scan answer ({scan{accounts[{…, missing_months[]}]}}), in the server's order
func asMissingFrom(data map[string]any) []asMissingEntry {
	accounts := asList(data["accounts"])

	if scan := asMap(data["scan"]); scan != nil {
		accounts = asList(scan["accounts"])
	}

	var out []asMissingEntry

	for _, a := range accounts {
		am := asMap(a)
		months := asStrings(am["missing"])

		if _, has := am["missing"]; !has {
			months = asStrings(am["missing_months"])
		}

		acct := asFirstStr(am, "account", "label", "last4")

		for _, m := range months {
			out = append(out, asMissingEntry{Entity: asFirstStr(am, "entity"), Bank: asFirstStr(am, "institution"), Account: acct, Month: m})
		}
	}

	return out
}

func asRunMissing(c *app.Ctx) error {
	r, err := asRootFor(c)

	if err != nil {
		return err
	}

	filters := asScopeFilters(c, r)
	var env *client.Envelope

	// the coverage route knows combined-file accounts (it reads their rows); it filters by account
	// only, so a wider scope (entity, bank, year) asks the scan route, which filters by all of them
	if filters["entity"] == "" && filters["institution"] == "" && filters["year"] == "" {
		q := url.Values{"root": {r.Root}}

		if v := filters["account"]; v != "" {
			q.Set("account", v)
		}

		if v := c.String("manifest-path"); v != "" {
			q.Set("manifest_path", v)
		}

		env, err = c.Call("GET", "/ingest/coverage", q, nil)
	} else {
		body := map[string]any{"root": r.Root}

		for k, v := range filters {
			body[k] = v
		}

		if v := c.String("manifest-path"); v != "" {
			body["manifest_path"] = v
		}

		env, err = c.Call("POST", "/ingest/scan", nil, body)
	}

	if err != nil {
		return err
	}

	entries := asMissingFrom(env.DataMap())

	if len(entries) == 0 {
		c.Info("no missing account-months")
	} else {
		c.Info("%d account-months missing", len(entries))
	}

	// the default (and --format table) is one gap per line, so it pipes; --format json is the envelope
	switch {
	case c.Has("format") && c.Format == render.FormatJSON:
		return c.Emit(env, render.View{})
	case c.Format == render.FormatCSV:
		recs := make([][]string, 0, len(entries))

		for _, e := range entries {
			recs = append(recs, []string{e.Entity, e.Bank, e.Account, e.Month})
		}

		return c.EmitCSV([]string{"entity", "bank", "account", "month"}, recs)
	}

	lines := make([]string, 0, len(entries))

	for _, e := range entries {
		lines = append(lines, e.line())
	}

	return c.EmitLines(lines)
}

// ---------------------------------------------------------------------------------------------
// extract (cli.mdx §10.4) — raw mode only, long-running, exit 1 if any statement yielded zero rows
// ---------------------------------------------------------------------------------------------

// asZeroRowStatements names every statement the extraction produced nothing from
func asZeroRowStatements(data map[string]any) []string {
	var out []string
	seen := map[string]bool{}

	for _, key := range []string{"zero_row_statements", "no_usable_extraction"} {
		for _, z := range asList(data[key]) {
			zm := asMap(z)
			name := asFirstStr(zm, "statement", "source", "path")

			if zm == nil {
				name = asStr(z)
			}

			if name == "" || seen[name] {
				continue
			}

			seen[name] = true
			reason := asFirstStr(zm, "reason", "status")

			if reason != "" {
				name += "  (" + reason + ")"
			}

			out = append(out, name)
		}
	}

	return out
}

// asDateOrder validates --date-order for raw extraction
func asDateOrder(v string) (string, error) {
	v = strings.ToLower(strings.TrimSpace(v))

	switch v {
	case "", "mdy", "dmy", "ymd":
		return v, nil
	}

	return "", app.Usage(fmt.Sprintf("--date-order %q is not mdy, dmy or ymd", v), "the order of day and month in the statements' dates")
}

func asRunExtract(c *app.Ctx) error {
	r, err := asRootFor(c)

	if err != nil {
		return err
	}

	body := map[string]any{"root": r.Root}

	if v := c.String("manifest-path"); v != "" {
		body["manifest_path"] = v
	}

	keys, err := asScopeAccountKeys(c, r)

	if err != nil {
		return err
	}

	if len(keys) > 0 {
		body["accounts"] = keys
	}

	if c.Bool("force") {
		body["force"] = true
	}

	order, err := asDateOrder(c.String("date-order"))

	if err != nil {
		return err
	}

	if order == "" {
		c.Info("dates in the statements are read as month/day/year (the server's default); pass --date-order dmy for day/month/year")
	} else {
		body["date_order"] = order
	}

	prefer, err := asPreferPaths(c, r)

	if err != nil {
		return err
	}

	if len(prefer) > 0 {
		body["prefer"] = prefer
	}

	if c.Has("max-statements") {
		n, err := asPositiveInt(c, "max-statements")

		if err != nil {
			return err
		}

		m, _ := strconv.Atoi(n)
		body["max_statements"] = m
	}

	env, err := c.CallStreamStages("POST", "/ingest/extract", nil, body, "Extracting statements")

	if err != nil {
		return err
	}

	data := env.DataMap()

	if !c.Quiet() {
		t := asMap(data["totals"])
		keysT := make([]string, 0, len(t))

		for k := range t {
			keysT = append(keysT, k+" "+asStr(t[k]))
		}

		sort.Strings(keysT)
		c.Info("extract into %s/%s: %s", strings.TrimRight(orDash(asFirstStr(data, "root")), "/"), orDash(asFirstStr(data, "staging")), strings.Join(keysT, ", "))
		c.Info("%d files written, %s unchanged; dates read as %s", len(asList(data["written"])), asStr(data["unchanged"]), orDash(asFirstStr(data, "date_order")))

		for _, cf := range asList(data["conflicts"]) {
			cm := asMap(cf)
			c.Warnf("CONFLICT %s: %s %s", asFirstStr(cm, "account_key"), asFirstStr(cm, "message"), asFirstStr(cm, "hint"))
		}

		if n := asIntOr0(data, "unreadable_total"); n > 0 {
			c.Warnf("%s statement lines could not be read as transactions (named in --format json as unreadable_lines, and in the staging _conflicts.csv)", asGroupThousands(n))
		}

		for _, w := range asStrings(data["warnings"]) {
			c.Warnf("%s", w)
		}
	}

	zero := asZeroRowStatements(data)

	if err := c.Emit(env, render.View{Rows: "accounts", Empty: "nothing needed extracting (use --force to re-extract)", Columns: []render.Column{
		{Header: "Account key", Key: "account_key"},
		{Header: "Staged in", Key: "dir"},
		{Header: "Currency", Key: "currency"},
		{Header: "Statements", Key: "statements", Kind: render.KInt},
		{Header: "Rows", Key: "rows", Kind: render.KInt},
		{Header: "Months", Key: "months", Kind: render.KInt},
		{Header: "Collapsed", Key: "collapsed", Kind: render.KInt},
		{Header: "Zero-row", Key: "zero_row_statements"},
		{Header: "Blocked months", Key: "blocked_months"},
	}}); err != nil {
		return err
	}

	if ok, has := data["ok_every_statement_yielded_rows"].(bool); len(zero) > 0 || (has && !ok) {
		for _, z := range zero {
			fmt.Fprintf(c.Err, "  zero rows: %s\n", z)
		}

		return app.Fail(exitcode.Failed, fmt.Sprintf("%d statement(s) yielded zero rows — a silent zero is how a year goes missing", len(zero)),
			"check each file above (a scanned PDF needs a _claude.txt / _brew.txt / _ocr.txt sidecar), then re-run with --force")
	}

	return nil
}

// ---------------------------------------------------------------------------------------------
// dupes (cli.mdx §10.5)
// ---------------------------------------------------------------------------------------------

func asRunDupes(c *app.Ctx) error {
	r, err := asRootFor(c)

	if err != nil {
		return err
	}

	q := url.Values{"root": {r.Root}}

	if v := c.String("manifest-path"); v != "" {
		q.Set("manifest_path", v)
	}

	if m := strings.ToLower(c.String("mode")); m != "" {
		if m != "prepared" && m != "raw" {
			return app.Usage("--mode must be prepared or raw", "")
		}

		q.Set("mode", m)
	}

	keys, err := asScopeAccountKeys(c, r)

	if err != nil {
		return err
	}

	for _, k := range keys {
		q.Add("account", k)
	}

	prefer, err := asPreferPaths(c, r)

	if err != nil {
		return err
	}

	for _, p := range prefer {
		q.Add("prefer", p)
	}

	if qif, err := asQIFOrder(c.String("qif-date-order")); err != nil {
		return err
	} else if qif != "" {
		q.Set("qif_date_order", qif)
	}

	if order, err := asDateOrder(c.String("date-order")); err != nil {
		return err
	} else if order != "" {
		q.Set("date_order", order)
	}

	env, err := c.CallStreamStages("GET", "/ingest/dupes", q, nil, "Comparing statements")

	if err != nil {
		return err
	}

	data := env.DataMap()

	if !c.Quiet() {
		t := asMap(data["totals"])
		c.Info("dupes (%s): %s primary, %s identical copies, %s superseded, %s conflicts, %s empty; %s rows collapsed at the transaction level",
			orDash(asFirstStr(data, "mode")), asN(t, "primary"), asN(t, "duplicate_identical"), asN(t, "superseded"), asN(t, "conflicts"), asN(t, "empty"), asN(t, "collapsed_rows"))

		for _, cf := range asList(data["conflicts"]) {
			cm := asMap(cf)
			files := asStrings(cm["files"])
			fmt.Fprintf(c.Err, "CONFLICT %s %s — %s\n", asFirstStr(cm, "account_key"), strings.Join(asStrings(cm["months"]), " "), asFirstStr(cm, "message"))

			for _, f := range files {
				fmt.Fprintf(c.Err, "    %s\n", f)
			}

			hint := asFirstStr(cm, "hint")

			if hint == "" && len(files) > 0 {
				hint = "pick one: --prefer " + files[0]
			}

			if hint != "" {
				fmt.Fprintf(c.Err, "  %s\n", hint)
			}
		}

		if len(prefer) > 0 {
			c.Info("--prefer is per call: pass the same --prefer to `ezbk statements plan` and `apply`")
		}
	}

	// the table lists the verdicts that did something (primary statements are counted above)
	var rows []any

	for _, v := range asList(data["statement_level"]) {
		if vm := asMap(v); vm != nil && asFirstStr(vm, "verdict") != "primary" {
			rows = append(rows, vm)
		}
	}

	return asEmitRows(c, env, rows, []render.Column{
		{Header: "Account key", Key: "account_key"},
		{Header: "Verdict", Key: "verdict"},
		{Header: "File", Key: "file"},
		{Header: "Kept", Key: "winner"},
		{Header: "Rule", Key: "rule"},
		{Header: "Rows", Key: "rows", Kind: render.KInt},
		{Header: "First", Key: "first"},
		{Header: "Last", Key: "last"},
	}, "no duplicate statements")
}

// ---------------------------------------------------------------------------------------------
// accounts (cli.mdx §10.8, apis.mdx §15)
// ---------------------------------------------------------------------------------------------

// asAccountOverrides builds the per-row overrides from --name KEY=NAME, --link KEY=ID, --skip KEY
// and --create KEY
func asAccountOverrides(c *app.Ctx) (map[string]map[string]string, error) {
	out := map[string]map[string]string{}
	get := func(key string) map[string]string {
		if out[key] == nil {
			out[key] = map[string]string{}
		}

		return out[key]
	}

	names, err := asParsePairs("name", c.Flags("name"))

	if err != nil {
		return nil, err
	}

	for k, v := range names {
		get(k)["name"] = v
	}

	links, err := asParsePairs("link", c.Flags("link"))

	if err != nil {
		return nil, err
	}

	for k, v := range links {
		if !asIsId(v) {
			return nil, app.Usage(fmt.Sprintf("--link %s=%s: link to an account id (ezbk accounts list)", k, v), "")
		}

		get(k)["action"] = "link"
		get(k)["account_id"] = v
	}

	for _, flag := range []string{"skip", "create"} {
		for _, k := range c.Strings(flag) {
			if prev := get(k)["action"]; prev != "" && prev != flag {
				return nil, app.Usage(fmt.Sprintf("%s is given two decisions (%s and %s)", k, prev, flag), "")
			}

			get(k)["action"] = flag
		}
	}

	return out, nil
}

// asAccountsBody builds the /ingest/accounts/plan arguments
func asAccountsBody(c *app.Ctx, r *asRoot) (map[string]any, error) {
	body := map[string]any{"root": r.Root}

	if v := c.String("naming"); v != "" {
		body["naming"] = v
	}

	km, err := asParsePairs("kind-map", c.Flags("kind-map"))

	if err != nil {
		return nil, err
	}

	if len(km) > 0 {
		body["kind_to_category"] = km
	}

	if v := c.String("manifest-path"); v != "" {
		body["manifest_path"] = v
	}

	ov, err := asAccountOverrides(c)

	if err != nil {
		return nil, err
	}

	if len(ov) > 0 {
		body["overrides"] = ov
	}

	keys, err := asScopeAccountKeys(c, r)

	if err != nil {
		return nil, err
	}

	if len(keys) > 0 {
		body["accounts"] = keys
	}

	return body, nil
}

var asAccountsView = render.View{Rows: "plan", Empty: "the manifest lists no accounts", Columns: []render.Column{
	{Header: "Action", Key: "action"},
	{Header: "Account key", Key: "account_key"},
	{Header: "Name", Key: "proposed.name"},
	{Header: "Category", Key: "proposed.category"},
	{Header: "Side", Key: "proposed.side"},
	{Header: "Currency", Key: "proposed.currency"},
	{Header: "Defaulted", Key: "proposed.currency_defaulted", Kind: render.KBool},
	{Header: "Existing", Key: "existing.name"},
	{Header: "Existing id", Key: "existing.id"},
	{Header: "Reason", Key: "reason"},
}}

// asAccountsNotes lists what the operator must read before --write: every liability, every
// defaulted currency, every ambiguous row with its candidates, every truncated name (cli.mdx §10.8)
func asAccountsNotes(data map[string]any) (notes []string, ambiguous int) {
	src := data

	if prev := asMap(data["preview"]); prev != nil && data["plan"] == nil {
		src = prev
	}

	currencies := map[string]int{}

	for _, row := range asList(src["plan"]) {
		m := asMap(row)
		p := asMap(m["proposed"])
		key := asFirstStr(m, "account_key")

		switch asFirstStr(m, "action") {
		case "ambiguous":
			ambiguous++
			var cands []string

			for _, cand := range asList(m["candidates"]) {
				cm := asMap(cand)
				cands = append(cands, fmt.Sprintf("%s %q", asFirstStr(cm, "id"), asFirstStr(cm, "name")))
			}

			note := fmt.Sprintf("AMBIGUOUS %s — %s (never created)", key, asFirstStr(m, "reason"))

			if len(cands) > 0 {
				note += "; candidates: " + strings.Join(cands, ", ") + "; choose with --link " + key + "=<id> or --create " + key
			}

			notes = append(notes, note)
		case "create":
			if strings.EqualFold(asFirstStr(p, "side"), "liability") {
				notes = append(notes, fmt.Sprintf("LIABILITY %s → %q as %s (its balance is what is owed; payments are transfers in)", key, asFirstStr(p, "name"), asFirstStr(p, "category")))
			}

			cur := asFirstStr(p, "currency")
			currencies[cur]++

			if b, _ := p["currency_defaulted"].(bool); b {
				notes = append(notes, fmt.Sprintf("CURRENCY %s defaulted to %s — not stated in the manifest, and it cannot be changed after the account is created", key, cur))
			}

			if b, _ := p["truncated"].(bool); b {
				notes = append(notes, fmt.Sprintf("NAME %s truncated to %q (from %q)", key, asFirstStr(p, "name"), asFirstStr(p, "full_name")))
			}
		}

		for _, w := range asStrings(m["warnings"]) {
			notes = append(notes, fmt.Sprintf("%s: %s", key, w))
		}
	}

	if len(currencies) > 0 {
		var parts []string

		for cur, n := range currencies {
			parts = append(parts, fmt.Sprintf("%d in %s", n, orDash(cur)))
		}

		sort.Strings(parts)
		notes = append(notes, "CURRENCIES of the accounts to create: "+strings.Join(parts, ", "))
	}

	for _, w := range asStrings(src["warnings"]) {
		notes = append(notes, w)
	}

	return notes, ambiguous
}

func asRunAccounts(c *app.Ctx) error {
	r, err := asRootFor(c)

	if err != nil {
		return err
	}

	body, err := asAccountsBody(c, r)

	if err != nil {
		return err
	}

	if !c.Bool("write") {
		env, err := c.Call("POST", "/ingest/accounts/plan", nil, body)

		if err != nil {
			return err
		}

		if !c.Quiet() {
			asPrintAccountsSummary(c, env.DataMap())
			c.Info("PLAN ONLY — nothing was created. Read every LIABILITY and CURRENCY line above, then: ezbk statements accounts --write --yes")
		}

		return c.Emit(env, asAccountsView)
	}

	if !c.Bool("yes") {
		return app.Usage("`ezbk statements accounts --write` creates accounts and needs --yes as well", "run it without --write first and read the plan, then add --write --yes")
	}

	return c.WriteFlow("POST", "/ingest/accounts/apply", body, render.View{Rows: "result.created", Empty: "no accounts were created (links and the map may still have been written)", Columns: []render.Column{
		{Header: "Account key", Key: "account_key"},
		{Header: "Id", Key: "account_id"},
		{Header: "Name", Key: "name"},
		{Header: "Category", Key: "category"},
		{Header: "Side", Key: "side"},
		{Header: "Currency", Key: "currency"},
	}})
}

func asPrintAccountsSummary(c *app.Ctx, data map[string]any) {
	if s := asMap(data["summary"]); s != nil {
		c.Info("accounts: %s create, %s link, %s skip, %s ambiguous (naming %q)", asN(s, "create"), asN(s, "link"), asN(s, "skip"), asN(s, "ambiguous"), asFirstStr(data, "naming"))
	}

	notes, _ := asAccountsNotes(data)

	for _, n := range notes {
		fmt.Fprintln(c.Err, "  "+n)
	}
}

// ---------------------------------------------------------------------------------------------
// plan (cli.mdx §10.9) — the per-account summary
// ---------------------------------------------------------------------------------------------

// asPlanOf returns the plan object of an answer: /ingest/plan and /ingest/file/plan put it under
// data.plan, a dry run of /ingest/apply under data.preview
func asPlanOf(data map[string]any) map[string]any {
	if p := asMap(data["plan"]); p != nil {
		return p
	}

	if p := asMap(data["preview"]); p != nil {
		return p
	}

	return data
}

// asPlanText lays a plan out the way cli.mdx §10.9 shows it — one block per account
func asPlanText(plan map[string]any) string {
	var b strings.Builder

	for i, a := range asList(plan["accounts"]) {
		am := asMap(a)

		if am == nil {
			continue
		}

		if i > 0 {
			b.WriteString("\n")
		}

		key := asFirstStr(am, "account_key")
		name := asFirstStr(am, "account_name")
		head := key + "  ->  "

		if name != "" {
			head += strconv.Quote(name)
		} else {
			head += "(not mapped to an account — ezbk statements accounts)"
		}

		if cur := asFirstStr(am, "currency"); cur != "" {
			head += " (" + cur + ")"
		}

		b.WriteString(head + "\n")

		st := asMap(am["statements"])
		fmt.Fprintf(&b, "  statements   %5s primary, %s superseded, %s identical copies, %s conflicts\n",
			asN(st, "primary"), asN(st, "superseded"), asN(st, "duplicate_identical"), asN(st, "conflict"))

		rows := asMap(am["rows"])
		conv := ""

		if cv := asFirstStr(am, "converter"); cv != "" {
			conv = " (converter: " + cv + ")"
		}

		fmt.Fprintf(&b, "  rows         %5s parsed%s  ->  %s after de-dupe\n", asN(rows, "parsed"), conv, asN(rows, "after_dedupe"))

		if n := asIntOr0(rows, "out_of_range"); n > 0 {
			fmt.Fprintf(&b, "               %5s outside --start/--end\n", asGroupThousands(n))
		}

		line := fmt.Sprintf("  vs. books    %5s already imported", asN(am, "already_present"))

		if n := asIntOr0(am, "already_present_deleted"); n > 0 {
			if re := asIntOr0(am, "reimport_deleted"); re > 0 {
				line += fmt.Sprintf("   %s already imported, deleted since (%s to RE-IMPORT)", asGroupThousands(n), asGroupThousands(re))
			} else {
				line += fmt.Sprintf("   %s already imported, deleted since (kept deleted)", asGroupThousands(n))
			}
		}

		b.WriteString(line + "\n")

		if n := asIntOr0(am, "possible_matches"); n > 0 {
			acc := ""

			if ac := asIntOr0(am, "accepted_matches"); ac > 0 {
				acc = fmt.Sprintf(", %d accepted", ac)
			}

			fmt.Fprintf(&b, "               %5s possible match%s with a hand-entered transaction (excluded%s)\n", asGroupThousands(n), map[bool]string{true: "", false: "es"}[n == 1], acc)
		}

		if n := asIntOr0(am, "transfer_candidates"); n > 0 {
			with := asTransferPartners(plan, key)
			state := "not accepted"

			if ac := asIntOr0(am, "accepted_transfers"); ac > 0 {
				state = fmt.Sprintf("%d accepted", ac)
			}

			if with != "" {
				fmt.Fprintf(&b, "               %5s transfer candidates with %s (%s)\n", asGroupThousands(n), with, state)
			} else {
				fmt.Fprintf(&b, "               %5s transfer candidates (%s)\n", asGroupThousands(n), state)
			}
		}

		line = fmt.Sprintf("               %5s NEW", asN(am, "new"))

		if n := asIntOr0(am, "to_fallback"); n > 0 {
			line += fmt.Sprintf("   (%s to the fallback category)", asGroupThousands(n))
		}

		b.WriteString(line + "\n")

		if n := asIntOr0(am, "unmapped"); n > 0 {
			fmt.Fprintf(&b, "               %5s with a category that maps to none of yours\n", asGroupThousands(n))
		}

		if first, last := asFirstStr(am, "first"), asFirstStr(am, "last"); first != "" || last != "" {
			fmt.Fprintf(&b, "  date range   %s .. %s\n", first, last)
		}

		if blocked, _ := am["blocked"].(bool); blocked {
			fmt.Fprintf(&b, "  BLOCKED      %s rows: %s\n", asN(am, "blocked_rows"), strings.Join(asStrings(am["blocked_reasons"]), "; "))
		}

		for _, w := range asStrings(am["warnings"]) {
			fmt.Fprintf(&b, "  warning      %s\n", w)
		}
	}

	return b.String()
}

// asTransferPartners names the other accounts an account's transfer candidates pair it with
func asTransferPartners(plan map[string]any, key string) string {
	seen := map[string]bool{}
	var out []string

	for _, t := range asList(plan["transfer_candidates"]) {
		tm := asMap(t)
		from, to := asFirstStr(asMap(tm["from"]), "account_key"), asFirstStr(asMap(tm["to"]), "account_key")
		other := ""

		switch key {
		case from:
			other = to
		case to:
			other = from
		}

		if other != "" && !seen[other] {
			seen[other] = true
			out = append(out, other)
		}
	}

	return strings.Join(out, ", ")
}

// asPlanFooter describes the plan's cross-account lists on stderr: unmapped categories, transfer
// candidates (with their ids, for --accept-transfer), possible matches (for --accept-match),
// conflicts, and the totals
func asPlanFooter(c *app.Ctx, plan map[string]any) {
	if c.Quiet() || plan == nil {
		return
	}

	if t := asMap(plan["totals"]); t != nil {
		c.Info("plan (%s): %s accounts; %s rows parsed, %s after de-dupe; %s already imported (%s deleted since); %s NEW, %s to a fallback; %s possible matches; %s transfer candidates (%s accepted); %s blocked rows in %s accounts",
			orDash(asFirstStr(plan, "mode")), asN(t, "accounts"), asN(t, "parsed"), asN(t, "after_dedupe"), asN(t, "already_present"), asN(t, "already_present_deleted"),
			asN(t, "new"), asN(t, "to_fallback"), asN(t, "possible_matches"), asN(t, "transfer_candidates"), asN(t, "accepted_transfers"), asN(t, "blocked_rows"), asN(t, "blocked_accounts"))
	}

	if un := asList(plan["unmapped"]); len(un) > 0 {
		fmt.Fprintln(c.Err, "Unmapped categories (map with --category-map FROM=TO, or name a --fallback-expense / --fallback-income):")

		for _, u := range un {
			um := asMap(u)
			dest := "BLOCKS its accounts"

			if fb := asFirstStr(um, "fallback_category_id"); fb != "" {
				dest = "→ fallback " + fb
			}

			name := strconv.Quote(asFirstStr(um, "name"))

			if asFirstStr(um, "name") == "" {
				name = "(no category)"
			}

			fmt.Fprintf(c.Err, "  %-8s %-36s %6s rows  %s  %s\n", asFirstStr(um, "type"), name, asN(um, "rows"), dest, asFirstStr(um, "reason"))
		}
	}

	if tc := asList(plan["transfer_candidates"]); len(tc) > 0 {
		fmt.Fprintf(c.Err, "Transfer candidates (%d) — accept one with --accept-transfer <id>; unaccepted, both sides import and spending may be double-counted:\n", len(tc))

		for _, t := range tc {
			tm := asMap(t)
			mark := " "

			if a, _ := tm["accepted"].(bool); a {
				mark = "✓"
			}

			fmt.Fprintf(c.Err, "  %s %-20s %s  %12s  %s → %s\n", mark, asFirstStr(tm, "id"), asFirstStr(tm, "date"), asMoneyOf(tm, "amount", "currency"),
				asFirstStr(asMap(tm["from"]), "account_key"), asFirstStr(asMap(tm["to"]), "account_key"))
		}
	}

	if pm := asList(plan["possible_matches"]); len(pm) > 0 {
		fmt.Fprintf(c.Err, "Possible matches with hand-entered transactions (%d, excluded) — link one with --accept-match <import-id>=<transaction-id>:\n", len(pm))

		for _, p := range pm {
			m := asMap(p)

			for _, cand := range asList(m["candidates"]) {
				cm := asMap(cand)
				fmt.Fprintf(c.Err, "  %s=%s  %s  %s  (%s days apart)\n", asFirstStr(m, "import_id"), asFirstStr(cm, "transaction_id"),
					asFirstStr(m, "date"), asMoneyOf(m, "amount", "currency"), asStr(cm["days_apart"]))
			}

			if acc := asFirstStr(m, "accepted_transaction_id"); acc != "" {
				fmt.Fprintf(c.Err, "  ✓ %s linked to %s\n", asFirstStr(m, "import_id"), acc)
			}
		}
	}

	for _, cf := range asList(plan["conflicts"]) {
		cm := asMap(cf)
		c.Warnf("CONFLICT %s %s: %s %s", asFirstStr(cm, "account_key"), strings.Join(asStrings(cm["months"]), " "), asFirstStr(cm, "message"), asFirstStr(cm, "hint"))
	}

	for _, w := range asStrings(plan["warnings"]) {
		c.Warnf("%s", w)
	}
}

// asDeletedRows returns the deleted-since-import rows, marking those a --reimport-deleted would
// bring back
func asDeletedRows(plan map[string]any) (all []string, reimport []string) {
	for _, d := range asList(plan["already_present_deleted"]) {
		m := asMap(d)

		if m == nil {
			continue
		}

		line := fmt.Sprintf("%s  %s  %s  %s  %q  (was transaction %s)", asFirstStr(m, "import_id"), asFirstStr(m, "account_key"),
			asFirstStr(m, "date"), asMoneyOf(m, "amount", "currency"), asFirstStr(m, "description"), asFirstStr(m, "transaction_id"))
		all = append(all, line)

		if w, _ := m["will_reimport"].(bool); w {
			reimport = append(reimport, line)
		}
	}

	return all, reimport
}

// asPlanView is the CSV / table layout of a plan (one row per account)
var asPlanView = render.View{Rows: "plan.accounts", Empty: "the plan covers no accounts", Columns: []render.Column{
	{Header: "Account key", Key: "account_key"},
	{Header: "Account", Key: "account_name"},
	{Header: "Account id", Key: "account_id"},
	{Header: "Currency", Key: "currency"},
	{Header: "Converter", Key: "converter"},
	{Header: "Parsed", Key: "rows.parsed", Kind: render.KInt},
	{Header: "After dedupe", Key: "rows.after_dedupe", Kind: render.KInt},
	{Header: "Already", Key: "already_present", Kind: render.KInt},
	{Header: "Deleted since", Key: "already_present_deleted", Kind: render.KInt},
	{Header: "Matches", Key: "possible_matches", Kind: render.KInt},
	{Header: "Transfers", Key: "transfer_candidates", Kind: render.KInt},
	{Header: "New", Key: "new", Kind: render.KInt},
	{Header: "Fallback", Key: "to_fallback", Kind: render.KInt},
	{Header: "Unmapped", Key: "unmapped", Kind: render.KInt},
	{Header: "Blocked", Key: "blocked", Kind: render.KBool},
	{Header: "First", Key: "first"},
	{Header: "Last", Key: "last"},
}}

// asEmitPlan prints a plan: the §10.9 text for --format table, the envelope for json, one row per
// account for csv. The footer (cross-account lists) goes to stderr.
func asEmitPlan(c *app.Ctx, env *client.Envelope) error {
	data := env.DataMap()
	plan := asPlanOf(data)
	asPlanFooter(c, plan)

	if c.Format == render.FormatTable {
		text := asPlanText(plan)

		if text == "" {
			c.Info("the plan covers no accounts")
			return nil
		}

		return c.EmitText(text)
	}

	view := asPlanView

	if data["plan"] == nil {
		view.Rows = "preview.accounts"
	}

	return c.Emit(env, view)
}

func asRunPlan(c *app.Ctx) error {
	r, err := asRootFor(c)

	if err != nil {
		return err
	}

	body, err := asPlanBody(c, r)

	if err != nil {
		return err
	}

	env, err := c.CallStreamStages("POST", "/ingest/plan", nil, body, "Planning the import")

	if err != nil {
		return err
	}

	asNoteReimport(c, asPlanOf(env.DataMap()))
	c.Info("PLAN ONLY — nothing was imported. Apply with the same flags: ezbk statements apply --write --yes …")

	return asEmitPlan(c, env)
}

// asNoteReimport names every deleted row a --reimport-deleted run would bring back (cli.mdx §10.6)
func asNoteReimport(c *app.Ctx, plan map[string]any) {
	if !c.Bool("reimport-deleted") {
		return
	}

	_, re := asDeletedRows(plan)

	if len(re) == 0 {
		c.Info("--reimport-deleted: no deleted rows would be brought back")
		return
	}

	fmt.Fprintf(c.Err, "--reimport-deleted will RESURRECT %d row(s) that were deleted after import:\n", len(re))

	for _, d := range re {
		fmt.Fprintln(c.Err, "  "+d)
	}
}

// ---------------------------------------------------------------------------------------------
// apply (cli.mdx §10.9) — plan, then apply that plan's token; the server recomputes
// ---------------------------------------------------------------------------------------------

// asApplyGate refuses an apply that lacks --yes, and a --reimport-deleted without --yes
func asApplyGate(c *app.Ctx) error {
	if c.Bool("reimport-deleted") && !c.Bool("yes") {
		return app.Usage("--reimport-deleted resurrects rows that were deleted after import and needs --yes", "add --yes; without --write it only names the rows it would bring back")
	}

	if c.Bool("write") && !c.Bool("yes") {
		return app.Usage("`ezbk "+c.Verb.Name+" --write` imports into the books and needs --yes as well", "read the plan (run without --write), then add --write --yes")
	}

	return nil
}

// asApplyOutcome summarises an apply result (WriteResult.result = the ingest apply result)
type asApplyOutcome struct {
	RunId    string
	Created  int64
	Linked   int64
	AllOk    bool
	HasAllOk bool
	Failed   []string
	Blocked  []string
	Accounts []map[string]any
	Journal  string
}

func asReadApplyOutcome(data map[string]any) asApplyOutcome {
	out := asApplyOutcome{}
	res := asMap(data["result"])

	if res == nil {
		res = data
	}

	out.RunId = asFirstStr(res, "run_id")
	out.Created = asIntOr0(res, "created")
	out.Linked = asIntOr0(res, "linked")
	out.AllOk, out.HasAllOk = res["all_accounts_ok"].(bool)
	out.Journal = asStr(data["journal_id"])

	for _, a := range asList(res["accounts"]) {
		m := asMap(a)

		if m == nil {
			continue
		}

		out.Accounts = append(out.Accounts, m)
		key := asFirstStr(m, "account_key")

		switch asFirstStr(m, "status") {
		case "failed":
			out.Failed = append(out.Failed, key)
		case "blocked":
			out.Blocked = append(out.Blocked, key)
		}
	}

	return out
}

// asTotalsDiff describes how two plans' totals differ (for the "plan moved" message)
func asTotalsDiff(was, now map[string]any) string {
	var parts []string

	for _, k := range []string{"new", "creates", "links", "already_present", "already_present_deleted", "possible_matches", "transfer_candidates", "to_fallback", "blocked_rows"} {
		a, b := asStr(was[k]), asStr(now[k])

		if a != b {
			parts = append(parts, fmt.Sprintf("%s %s → %s", k, orDash(a), orDash(b)))
		}
	}

	if len(parts) == 0 {
		return "the same counts, but different rows"
	}

	return strings.Join(parts, "; ")
}

func asRunApply(c *app.Ctx) error {
	if err := asApplyGate(c); err != nil {
		return err
	}

	r, err := asRootFor(c)

	if err != nil {
		return err
	}

	body, err := asPlanBody(c, r)

	if err != nil {
		return err
	}

	return asIngestFlow(c, asIngestRoutes{plan: "/ingest/plan", apply: "/ingest/apply"}, body, nil)
}

// asIngestRoutes names the plan and apply routes of one ingest flow
type asIngestRoutes struct {
	plan, apply string
}

// asIngestFlow is the ingest write protocol from the terminal:
//
//  1. plan (read tier) — the preview and its confirm_token;
//  2. without --write, stop: the plan IS the answer (exit 0);
//  3. with --write --yes, apply that token (the server recomputes the plan from the arguments it
//     remembered and refuses on a moved fingerprint), streaming progress;
//  4. a moved plan is shown with its difference and applies nothing (exit 4).
//
// planBody is the JSON body of the plan call, or nil when upload builds a multipart body instead.
func asIngestFlow(c *app.Ctx, routes asIngestRoutes, planBody map[string]any, upload func() (*client.MultipartBody, error)) error {
	maxChanges, err := c.Int("max-changes", 0)

	if err != nil {
		return err
	}

	planCall := func() (*client.Envelope, error) {
		if upload != nil {
			mb, err := upload()

			if err != nil {
				return nil, err
			}

			return c.CallStreamStages("POST", routes.plan, nil, mb, "Planning the import")
		}

		return c.CallStreamStages("POST", routes.plan, nil, planBody, "Planning the import")
	}

	env, err := planCall()

	if err != nil {
		return err
	}

	data := env.DataMap()
	plan := asPlanOf(data)
	totals := asMap(plan["totals"])
	asNoteReimport(c, plan)

	ceiling := int64(maxChanges)

	if ceiling <= 0 {
		ceiling = 200
	}

	changes := asIntOr0(totals, "changes")

	if !c.Bool("write") {
		c.Info("DRY RUN — nothing was imported. Re-run with --write --yes to apply.")

		if changes > ceiling {
			c.Warnf("this plan makes %s changes and the ceiling is %s: the apply will be refused unless you pass --max-changes %d (deliberately)", asGroupThousands(changes), asGroupThousands(ceiling), changes)
		}

		return asEmitPlan(c, env)
	}

	if _, has := totals["changes"]; has && changes == 0 {
		c.Info("nothing to import: every row is already in the books (or excluded) — no run was started")

		return asEmitPlan(c, env)
	}

	token := asFirstStr(data, "confirm_token")

	if token == "" {
		return app.Fail(exitcode.Failed, "the server returned no confirm_token for the plan", "ezbk status (is the server older than this CLI?)")
	}

	if !c.Quiet() {
		asPlanFooter(c, plan)

		for _, a := range asList(plan["accounts"]) {
			am := asMap(a)
			c.Info("  %-44s %6s new%s", asFirstStr(am, "account_key"), asN(am, "new"), map[bool]string{true: "   BLOCKED", false: ""}[am["blocked"] == true])
		}
	}

	if changes > ceiling {
		return &app.ExitError{Code: exitcode.Conflict, Msg: fmt.Sprintf("nothing was imported: the plan makes %s changes and the ceiling is %s", asGroupThousands(changes), asGroupThousands(ceiling)),
			Hint: fmt.Sprintf("a large first import is supposed to be deliberate: re-run with --max-changes %d --write --yes", changes)}
	}

	c.Info("target %s — applying %s creates and %s links", c.BaseURL(), asN(totals, "creates"), asN(totals, "links"))

	applyBody := map[string]any{"confirm_token": token, "dry_run": false}

	if maxChanges > 0 {
		applyBody["max_changes"] = maxChanges
	}

	env2, err := c.CallStreamStages("POST", routes.apply, nil, applyBody, "Importing")

	if err != nil {
		var ee *app.ExitError

		if errors.As(err, &ee) && ee.APICode == "conflict" && !asIsCeiling(ee) {
			// the plan moved between the preview and the apply: show the difference, apply nothing
			if env3, err3 := planCall(); err3 == nil {
				fresh := asPlanOf(env3.DataMap())
				fmt.Fprintf(c.Err, "the plan changed since it was printed: %s\n", asTotalsDiff(totals, asMap(fresh["totals"])))
				_ = asEmitPlan(c, env3)
			}

			return &app.ExitError{Code: exitcode.Conflict, Msg: "nothing was imported: " + ee.Msg, Hint: "read the new plan above, then re-run the same command to apply it", APICode: ee.APICode}
		}

		return asExplainIngestErr(err)
	}

	out := asReadApplyOutcome(env2.DataMap())

	if !c.Quiet() {
		for _, a := range out.Accounts {
			c.Info("  %-44s %6s created  %s linked  %s", asFirstStr(a, "account_key"), asN(a, "created"), asN(a, "linked"), asFirstStr(a, "status"))
		}

		if out.RunId != "" {
			c.Info("run %s — %s transactions created, %s linked (undo: ezbk undo; fallback rows: ezbk analytics import-fallout --run %s)", out.RunId, asGroupThousands(out.Created), asGroupThousands(out.Linked), out.RunId)
		}

		for _, w := range asStrings(asMap(env2.DataMap()["result"])["warnings"]) {
			c.Warnf("%s", w)
		}
	}

	if err := c.Emit(env2, render.View{Rows: "result.accounts", Columns: []render.Column{
		{Header: "Account key", Key: "account_key"},
		{Header: "Account id", Key: "account_id"},
		{Header: "Created", Key: "created", Kind: render.KInt},
		{Header: "Linked", Key: "linked", Kind: render.KInt},
		{Header: "Re-imported", Key: "reimported", Kind: render.KInt},
		{Header: "Status", Key: "status"},
	}}); err != nil {
		return err
	}

	if len(out.Failed) > 0 || (out.HasAllOk && !out.AllOk && len(out.Blocked) == 0) {
		return app.Fail(exitcode.Failed, fmt.Sprintf("%d account(s) failed to import: %s", len(out.Failed), strings.Join(out.Failed, ", ")), "ezbk logs; what succeeded is recorded and will not re-import")
	}

	if len(out.Blocked) > 0 {
		return app.Fail(exitcode.Conflict, fmt.Sprintf("%d account(s) were blocked and not imported: %s", len(out.Blocked), strings.Join(out.Blocked, ", ")),
			"resolve with ezbk statements dupes --prefer <file>, --category-map, or --fallback-expense/--fallback-income, then re-run")
	}

	return nil
}

// asIsCeiling reports whether a conflict is the max_changes ceiling rather than a moved plan
func asIsCeiling(ee *app.ExitError) bool {
	if d, ok := ee.Details.(map[string]any); ok {
		if _, has := d["would_change"]; has {
			return true
		}
	}

	return false
}

// asExplainIngestErr turns the ceiling conflict into its remedy
func asExplainIngestErr(err error) error {
	var ee *app.ExitError

	if !errors.As(err, &ee) || ee.APICode != "conflict" || !asIsCeiling(ee) {
		return err
	}

	d, _ := ee.Details.(map[string]any)
	n, _ := render.ToInt64(d["would_change"])
	ceiling, _ := render.ToInt64(d["max_changes"])
	ee.Hint = fmt.Sprintf("%s changes would be made and the ceiling is %s; a first import of a large archive is supposed to be deliberate: re-run with --max-changes %d --write --yes",
		asGroupThousands(n), asGroupThousands(ceiling), n)

	return ee
}

// asDescribe renders a changes map in a fixed order
func asDescribe(changes map[string]int) string {
	if len(changes) == 0 {
		return "no changes"
	}

	keys := make([]string, 0, len(changes))

	for k := range changes {
		keys = append(keys, k)
	}

	order := map[string]int{"create": 0, "link": 1, "transfer": 2, "update": 3, "skip": 4, "unchanged": 5}

	sort.Slice(keys, func(i, j int) bool {
		oi, iok := order[keys[i]]
		oj, jok := order[keys[j]]

		switch {
		case iok && jok:
			return oi < oj
		case iok:
			return true
		case jok:
			return false
		}

		return keys[i] < keys[j]
	})

	parts := make([]string, 0, len(keys))

	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s %s", asGroupThousands(int64(changes[k])), k))
	}

	return strings.Join(parts, ", ")
}

// ---------------------------------------------------------------------------------------------
// import-file (cli.mdx §10.9) — the browser's Import dialog for one file
// ---------------------------------------------------------------------------------------------

// asFileTypes are the converter names this CLI knows; the server's list (GET /ingest/converters) is
// the authority and re-checks
var asFileTypes = []string{"ofx", "qfx", "camt053", "camt052", "mt940", "qif_ymd", "qif_mdy", "qif_dmy", "iif",
	"ezbookkeeping_csv", "ezbookkeeping_tsv", "ezbookkeeping_json", "gnucash", "firefly_iii_csv", "beancount",
	"custom_csv", "custom_tsv", "custom_ssv", "custom_xlsx", "custom_xls",
	"feidee_mymoney_csv", "feidee_mymoney_xls", "feidee_mymoney_elecloud_xlsx", "alipay_app_csv", "alipay_web_csv",
	"wechat_pay_app_xlsx", "wechat_pay_app_csv", "jdcom_finance_app_csv"}

// asTypeFromFlags resolves --type (and --qif-date-order); a QIF never goes without a date order
func asTypeFromFlags(c *app.Ctx, file string) (string, error) {
	t := strings.ToLower(strings.TrimSpace(c.String("type")))
	qif, err := asQIFOrder(c.String("qif-date-order"))

	if err != nil {
		return "", err
	}

	if t == "qif" {
		if qif == "" {
			return "", app.Usage("--type qif needs a date order", "use --type qif_ymd|qif_mdy|qif_dmy, or add --qif-date-order")
		}

		t = "qif_" + qif
	}

	if t == "" && strings.EqualFold(filepath.Ext(file), ".qif") {
		if qif == "" {
			return "", app.Usage("a QIF file needs its date order: the server will not guess it, and neither will the CLI", "add --qif-date-order ymd|mdy|dmy (or --type qif_mdy)")
		}

		t = "qif_" + qif
	}

	if t == "" {
		return "", nil
	}

	for _, known := range asFileTypes {
		if t == known {
			return t, nil
		}
	}

	// unknown to this CLI's list — the server's may be newer, so pass it through with a note
	c.Info("note: --type %s is not in this CLI's list; the server decides (GET /machine/v1/ingest/converters)", t)

	return t, nil
}

// asColumnMapStringKeys are the column_map options that take a plain string
var asColumnMapStringKeys = map[string]bool{"file_type": true, "file_encoding": true, "time_format": true, "timezone_format": true,
	"amount_decimal_separator": true, "amount_digit_grouping_symbol": true, "geo_separator": true, "geo_order": true, "tag_separator": true}

// asColumnMap builds the column_map object of a custom CSV/TSV/Excel import (apis.mdx §14.4). Each
// --column-map is one of:
//
//	a JSON object                   {"column_mapping": {...}, "time_format": "YYYY-MM-DD", ...}
//	@FILE                           the same JSON object, read from a file
//	KEY=VALUE                       time_format=YYYY-MM-DD, has_header_line=false, file_encoding=utf-8, …
//	column_mapping=JSON / transaction_type_mapping=JSON   the two maps upstream's Import dialog builds
//
// Later values override earlier ones.
func asColumnMap(values []string) (map[string]any, error) {
	out := map[string]any{}

	merge := func(raw []byte, from string) error {
		var m map[string]any
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.UseNumber()

		if err := dec.Decode(&m); err != nil {
			return app.Usage(fmt.Sprintf("--column-map %s is not a JSON object: %v", from, err), `e.g. --column-map '{"column_mapping":{"1":0,"3":2},"time_format":"YYYY-MM-DD"}'`)
		}

		for k, v := range m {
			out[k] = v
		}

		return nil
	}

	for _, v := range values {
		v = strings.TrimSpace(v)

		switch {
		case v == "":
			continue
		case strings.HasPrefix(v, "{"):
			if err := merge([]byte(v), "value"); err != nil {
				return nil, err
			}
		case strings.HasPrefix(v, "@"):
			data, err := os.ReadFile(asExpandHome(v[1:]))

			if err != nil {
				return nil, app.Usage("--column-map "+v+": "+err.Error(), "")
			}

			if err := merge(data, v); err != nil {
				return nil, err
			}
		default:
			i := strings.Index(v, "=")

			if i <= 0 {
				return nil, app.Usage(fmt.Sprintf("--column-map %q is not KEY=VALUE, a JSON object or @FILE", v), "")
			}

			k, val := strings.TrimSpace(v[:i]), strings.TrimSpace(v[i+1:])

			switch {
			case k == "column_mapping" || k == "transaction_type_mapping":
				var m any
				dec := json.NewDecoder(strings.NewReader(val))
				dec.UseNumber()

				if err := dec.Decode(&m); err != nil {
					return nil, app.Usage(fmt.Sprintf("--column-map %s= must be a JSON object: %v", k, err), "")
				}

				out[k] = m
			case k == "has_header_line":
				b, err := strconv.ParseBool(val)

				if err != nil {
					return nil, app.Usage("--column-map has_header_line= must be true or false", "")
				}

				out[k] = b
			case asColumnMapStringKeys[k]:
				out[k] = val
			default:
				return nil, app.Usage(fmt.Sprintf("--column-map: unknown option %q", k), "options: column_mapping, transaction_type_mapping, has_header_line, file_type, file_encoding, time_format, timezone_format, amount_decimal_separator, amount_digit_grouping_symbol, geo_separator, geo_order, tag_separator")
			}
		}
	}

	if len(out) == 0 {
		return nil, nil
	}

	return out, nil
}

func asRunImportFile(c *app.Ctx) error {
	if err := asApplyGate(c); err != nil {
		return err
	}

	file := c.Args[0]
	real, err := asRealPath(file)

	if err != nil {
		return &app.ExitError{Code: exitcode.NotFound, Msg: "FILE " + file + " cannot be opened", Hint: "check the path"}
	}

	fi, err := os.Stat(real)

	if err != nil || fi.IsDir() {
		return app.Usage(real+" is not a file", "name one statement file, e.g. 20250930-statement-4021.ofx")
	}

	acct := strings.TrimSpace(c.String("account"))

	if acct == "" {
		return app.Usage("`ezbk statements import-file` needs --account", "ezbk accounts list (an id or an exact name)")
	}

	body := map[string]any{}

	if asIsId(acct) {
		body["account_id"] = acct
	} else {
		body["account_name"] = acct
	}

	fileType, err := asTypeFromFlags(c, real)

	if err != nil {
		return err
	}

	if fileType != "" {
		body["file_type"] = fileType
	}

	cm, err := asColumnMap(c.Flags("column-map"))

	if err != nil {
		return err
	}

	if cm != nil {
		body["column_map"] = cm
	}

	if strings.HasPrefix(fileType, "custom_") && cm == nil {
		return app.Usage("--type "+fileType+" needs --column-map", "the same fields the browser's Import dialog asks for: column_mapping, transaction_type_mapping, time_format")
	}

	if err := asRangeArgs(c, body); err != nil {
		return err
	}

	if err := asCategoryArgs(c, body); err != nil {
		return err
	}

	matches, err := asParseMatches(c.Flags("accept-match"))

	if err != nil {
		return err
	}

	if len(matches) > 0 {
		body["accept_matches"] = matches
	}

	if c.Bool("reimport-deleted") {
		body["reimport_deleted"] = true
	}

	routes := asIngestRoutes{plan: "/ingest/file/plan", apply: "/ingest/file/apply"}

	// a file inside the statements root is named by path — the server reads it and records its
	// root-relative source file, and the root's map names the account; anything else is uploaded
	if v := strings.TrimSpace(c.String("account-key")); v != "" {
		body["account_key"] = v
	}

	root := asImportRoot(c)

	if root != "" && asInside(root, real) {
		body["root"] = root
		rel, _ := filepath.Rel(root, real)
		body["path"] = filepath.ToSlash(rel)

		return asIngestFlow(c, routes, body, nil)
	}

	if root != "" {
		// an upload still names the root, so the root's statements map can name the account
		body["root"] = root
	}

	if fi.Size() > 16<<20 {
		return app.Usage(fmt.Sprintf("%s is %s bytes; an upload is capped at 16 MiB", real, asGroupThousands(fi.Size())), "put it under the statements root and import it by path")
	}

	return asIngestFlow(c, routes, nil, func() (*client.MultipartBody, error) {
		return asMultipart(real, body)
	})
}

// asImportRoot is the statements root for import-file: --path, else the configured root ("" when
// neither is set or it cannot be resolved — the file is then uploaded)
func asImportRoot(c *app.Ctx) string {
	p := c.String("path")

	if p == "" {
		p, _ = asConfiguredRoot()
	}

	if p == "" {
		return ""
	}

	real, err := asRealPath(p)

	if err != nil {
		return ""
	}

	return real
}

// asMultipart builds the /ingest/file/plan upload: the file in field "file", every argument as a
// form field (maps and lists as JSON strings, the way the route reads them)
func asMultipart(real string, fields map[string]any) (*client.MultipartBody, error) {
	content, err := os.ReadFile(real)

	if err != nil {
		return nil, app.Fail(exitcode.Failed, "cannot read "+real+": "+err.Error(), "")
	}

	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	keys := make([]string, 0, len(fields))

	for k := range fields {
		keys = append(keys, k)
	}

	sort.Strings(keys)

	for _, k := range keys {
		var s string

		switch t := fields[k].(type) {
		case string:
			s = t
		case bool:
			s = strconv.FormatBool(t)
		default:
			data, err := json.Marshal(t)

			if err != nil {
				return nil, err
			}

			s = string(data)
		}

		if err := w.WriteField(k, s); err != nil {
			return nil, err
		}
	}

	part, err := w.CreateFormFile("file", filepath.Base(real))

	if err != nil {
		return nil, err
	}

	if _, err := part.Write(content); err != nil {
		return nil, err
	}

	if err := w.Close(); err != nil {
		return nil, err
	}

	return &client.MultipartBody{Body: &buf, ContentType: w.FormDataContentType()}, nil
}

// ---------------------------------------------------------------------------------------------
// Registration
// ---------------------------------------------------------------------------------------------

func init() {
	planFlags := []app.Flag{
		asFlagPath, asFlagEntity, asFlagBank, asFlagStmtAcct, asFlagOnly, asFlagManifest, asFlagMode,
		{Name: "start", Value: "YYYY-MM-DD", Help: "only rows on or after this date (also YYYY-MM, YYYY, this-year, …)"},
		{Name: "end", Value: "YYYY-MM-DD", Help: "only rows on or before this date"},
		asFlagFbExp, asFlagFbInc, asFlagFbTrf, asFlagCatMap, asFlagQIF, asFlagPrefer,
		{Name: "accept-transfer", Value: "CANDIDATE_ID", Repeat: true, Help: "import this transfer candidate as ONE transfer instead of an expense plus an income (repeatable)"},
		{Name: "accept-match", Value: "IMPORT_ID=TXN_ID", Repeat: true, Help: "link a statement row to an existing hand-entered transaction (repeatable)"},
		{Name: "reimport-deleted", Help: "bring back rows deleted since they were imported (names every one; needs --yes)"},
	}

	app.Register(
		app.Verb{
			Name: "statements manifest", Group: asStmtGroup, Args: "[PATH]", MaxArgs: 1,
			Summary: "stage 0 — prepared or raw? the manifest per account, and the converter the server will use",
			Flags:   []app.Flag{asFlagPath, asFlagManifest, asFlagQIF},
			Run:     asRunManifest,
		},
		app.Verb{
			Name: "statements scan", Group: asStmtGroup, Args: "[PATH]", MaxArgs: 1,
			Summary: "stage 1 — walk the tree: entity → bank → account → year, missing months, duplicate scans; changes nothing",
			Flags:   append(append([]app.Flag{}, asScopeFlags...), asFlagMode, asFlagQIF),
			Run:     asRunScan,
		},
		app.Verb{
			Name: "statements missing", Group: asStmtGroup, Args: "[PATH]", MaxArgs: 1,
			Summary: "only the gaps: one ENTITY/BANK/ACCOUNT YYYY-MM per line on stdout",
			Flags:   asScopeFlags,
			Run:     asRunMissing,
		},
		app.Verb{
			Name: "statements extract", Group: asStmtGroup, Args: "[PATH]", MaxArgs: 1, LongRunning: true,
			Summary: "stage 2 (raw mode) — statements to ezBookkeeping CSV in .ezbk-staging/; exit 1 if any statement yields zero rows",
			Flags: append(append([]app.Flag{}, asScopeFlags...), asFlagOnly, asFlagPrefer,
				app.Flag{Name: "force", Help: "re-extract statements that already have rows"},
				app.Flag{Name: "date-order", Value: "mdy|dmy|ymd", Help: "how the statements write dates (the server's default is mdy)"},
				app.Flag{Name: "max-statements", Value: "N", Help: "stop after this many statements (the server's ceiling for one run)"}),
			Run: asRunExtract,
		},
		app.Verb{
			Name: "statements dupes", Group: asStmtGroup, Args: "[PATH]", MaxArgs: 1,
			Summary: "stage 3 — the same month twice: identical, superseded, or a conflict to resolve with --prefer",
			Flags: append(append([]app.Flag{}, asScopeFlags...), asFlagOnly, asFlagPrefer, asFlagMode, asFlagQIF,
				app.Flag{Name: "date-order", Value: "mdy|dmy|ymd", Help: "raw mode: how the statements write dates"}),
			Run: asRunDupes,
		},
		app.Verb{
			Name: "statements accounts", Group: asStmtGroup, Args: "[PATH]", MaxArgs: 1,
			Summary: "plan the accounts (create / link / skip / ambiguous); --write --yes creates them and writes the map",
			Flags: []app.Flag{asFlagPath, asFlagEntity, asFlagBank, asFlagStmtAcct, asFlagOnly, asFlagManifest,
				{Name: "naming", Value: "TEMPLATE", Help: "account name template (default \"{Entity} · {Institution} {Kind} ••{last4}\")"},
				{Name: "kind-map", Value: "KIND=CATEGORY", Repeat: true, Help: "override a kind's ezBookkeeping category, e.g. brokerage=investment (repeatable)"},
				{Name: "name", Value: "KEY=NAME", Repeat: true, Help: "name one account yourself (account key = name; repeatable)"},
				{Name: "link", Value: "KEY=ACCOUNT_ID", Repeat: true, Help: "link an account key to an existing account — resolves an ambiguous row (repeatable)"},
				{Name: "skip", Value: "KEY", Repeat: true, Help: "leave this account key out (repeatable)"},
				{Name: "create", Value: "KEY", Repeat: true, Help: "create this account key even though a similar account exists (repeatable)"}},
			Run: asRunAccounts,
		},
		app.Verb{
			Name: "statements plan", Group: asStmtGroup, Args: "[PATH]", MaxArgs: 1,
			Summary: "stage 6 — what an apply would do, per account; changes nothing",
			Flags:   planFlags,
			Run:     asRunPlan,
		},
		app.Verb{
			Name: "statements apply", Group: asStmtGroup, Args: "[PATH]", MaxArgs: 1, LongRunning: true,
			Summary: "import the recomputed plan — needs --write, --yes and a server started with --allow-write",
			Flags:   planFlags,
			Run:     asRunApply,
		},
		app.Verb{
			Name: "statements import-file", Group: asStmtGroup, Args: "<FILE>", MinArgs: 1, MaxArgs: 1, LongRunning: true,
			Summary: "the Import dialog for one file: plan (and with --write --yes, apply) into one account",
			Flags: []app.Flag{
				{Name: "account", Value: "id|name", Help: "the ezBookkeeping account to import into (required)"},
				{Name: "account-key", Value: "KEY", Help: "the statements account key ({entity}/{bank}/{last4}) the file belongs to, so its rows are recognised by a later statements apply"},
				{Name: "type", Value: "CONVERTER", Help: "the converter when the extension is not enough: ofx, qfx, qif_mdy, camt053, mt940, custom_csv, … (GET /ingest/converters)"},
				{Name: "column-map", Value: "KEY=VALUE|JSON|@FILE", Repeat: true, Help: "a custom CSV/Excel's column map: column_mapping=JSON, transaction_type_mapping=JSON, time_format=…, has_header_line=… (repeatable)"},
				asFlagPath,
				{Name: "start", Value: "YYYY-MM-DD", Help: "only rows on or after this date"},
				{Name: "end", Value: "YYYY-MM-DD", Help: "only rows on or before this date"},
				asFlagQIF, asFlagFbExp, asFlagFbInc, asFlagCatMap,
				{Name: "accept-match", Value: "IMPORT_ID=TXN_ID", Repeat: true, Help: "link a row to an existing hand-entered transaction (repeatable)"},
				{Name: "reimport-deleted", Help: "bring back rows deleted since they were imported (needs --yes)"},
			},
			Run: asRunImportFile,
		},
	)
}

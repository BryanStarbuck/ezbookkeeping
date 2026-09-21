package machine

import (
	"encoding/json"
	"os"
	"sort"
	"strconv"
	"strings"

	"github.com/mayswind/ezbookkeeping/pkg/converters"
	"github.com/mayswind/ezbookkeeping/pkg/errfile"
)

// routes_ingest.go — the ingest plane (apis.mdx §14) and account provisioning (§15).
//
// The machine plane is the DUPLICATE AUTHORITY for imports on this fork: ezBookkeeping's own
// importer writes whatever rows it is handed, so the plane decides "already imported" exactly
// once, deterministically, with a durable record (machine_import_record), and never by a model.
// Every statement file is parsed by upstream's own converters; the statements archive is strictly
// read-only; the only directory written under the root is {ROOT}/.ezbk-staging/.

func init() {
	registerRoutes(ingRoutes)
}

var ingUntrusted = []string{"description", "comment", "name", "account_name", "text", "current_name"}

func ingRoutes() []RouteDef {
	return []RouteDef{
		{Method: "GET", Path: "/ingest/roots", Tier: TierRead, Handler: ingHandleRoots, Features: []string{"ingest.prepared", "ingest.raw"},
			Summary: "Configured statement roots, whether each is readable, its manifest and staging directory."},
		{Method: "GET", Path: "/ingest/converters", Tier: TierRead, Handler: ingHandleConverters,
			Summary: "The fileTypes this build's pkg/converters supports, which are enabled, which carry bank ids, and what each needs (qif date order, column map)."},
		{Method: "GET", Path: "/ingest/manifest", Tier: TierRead, Handler: ingHandleManifest, Untrusted: []string{"label"},
			Summary: "Read the archive's manifest (root, manifest_path): per account the converter the plane will use, combined/monthly files, formats, warnings; missing columns reported, never guessed."},
		{Method: "POST", Path: "/ingest/scan", Tier: TierRead, Handler: ingHandleScan,
			Summary: "Walk the statements root and classify every file (root, mode, entity, institution, account, year, manifest_path): statements per account-year-month, missing months, duplicate scans, unusable files. Changes nothing."},
		{Method: "GET", Path: "/ingest/coverage", Tier: TierRead, Handler: ingHandleCoverage,
			Summary: "Which account-months are present and which are missing (root, account, manifest_path, from_rows)."},
		{Method: "GET", Path: "/ingest/map", Tier: TierRead, Handler: ingHandleMapGet, Untrusted: []string{"name", "current_name"},
			Summary: "The statements→books map (root): each account key's ezBookkeeping account and whether it is still usable; manifest accounts not yet mapped."},
		{Method: "PUT", Path: "/ingest/map", Tier: TierWrite, DryRunnable: true, Feature: featureImport, Handler: ingHandleMapPut, Untrusted: []string{"name"},
			Summary: "Write the statements→books map beside the statements ({ROOT}/.ezbk-staging/_map.json, never the repo). Args: root, map {account_key: account_id}, replace, dry_run, confirm_token. Undoable."},
		{Method: "POST", Path: "/ingest/map/infer", Tier: TierRead, Handler: ingHandleMapInfer, Untrusted: []string{"name"},
			Summary: "Propose account mappings for the manifest (root, manifest_path): map, exact name, last4; never saves."},
		{Method: "POST", Path: "/ingest/accounts/plan", Tier: TierRead, Handler: ingHandleAccountsPlan, Untrusted: []string{"name"},
			Summary: "Plan the accounts from the manifest (root, naming, kind_to_category, overrides, accounts[]): per row create/link/skip/ambiguous with the category, its asset/liability side and the currency read aloud. Creates nothing; returns the confirm_token for /ingest/accounts/apply."},
		{Method: "POST", Path: "/ingest/accounts/apply", Tier: TierWrite, DryRunnable: true, Composed: true, Feature: featureImport, Handler: ingHandleAccountsApply, Untrusted: []string{"name"},
			Summary: "Create the plan's create rows, link the link rows and write the map (confirm_token from /ingest/accounts/plan, dry_run). Ambiguous rows are never created. Undoable."},
		{Method: "POST", Path: "/ingest/extract", Tier: TierRead, Handler: ingHandleExtract, Composed: true, Untrusted: []string{"text", "description"}, Features: []string{"ingest.raw"},
			Summary: "RAW MODE ONLY. Read-tier but WRITES to the staging directory only: produce ezBookkeeping CSV per account-month from _claude/_brew/_ocr sidecars with a conservative line parser (root, force, date_order, accounts[], prefer[], max_statements). Zero-row statements are named. NDJSON progress with Accept: application/x-ndjson."},
		{Method: "GET", Path: "/ingest/dupes", Tier: TierRead, Handler: ingHandleDupes,
			Summary: "What both de-duplication layers collapsed, and why (root, mode, account, prefer, date_order): duplicate_identical, superseded (with the deciding rule), conflicts, same-bank-id collapses."},
		{Method: "GET", Path: "/ingest/rows", Tier: TierRead, Handler: ingHandleRows, Untrusted: ingUntrusted,
			Summary: "The de-duplicated statement rows with their import_id and their verdict against the books (root, account, start, end, status, limit, offset)."},
		{Method: "POST", Path: "/ingest/plan", Tier: TierRead, Handler: ingHandlePlan, Composed: true, Untrusted: ingUntrusted, Features: []string{"ingest.prepared"},
			Summary: "Plan the import of a statements root (root, accounts[], start, end, category_map, fallback_category_ids, accept_matches[], accept_transfers[], reimport_deleted, prefer[], qif_date_order): per account parsed, collapsed, already present (and deleted since), possible matches, transfer candidates, unmapped, NEW; plus the confirm_token for /ingest/apply."},
		{Method: "POST", Path: "/ingest/apply", Tier: TierWrite, DryRunnable: true, Composed: true, Feature: featureImport, Handler: ingHandleApply, Untrusted: ingUntrusted,
			Summary: "Import the plan (confirm_token, max_changes, dry_run; or the plan's arguments again). Recomputes the plan and refuses on a moved fingerprint; creates through upstream's import service path; records machine_import_record; undoable. NDJSON progress with Accept: application/x-ndjson."},
		{Method: "POST", Path: "/ingest/file/plan", Tier: TierRead, Handler: ingHandleFilePlan, Composed: true, Untrusted: ingUntrusted,
			Summary: "The Import dialog for one file: account_id (or account_name), path under the root OR multipart field \"file\", file_type, column_map for custom files; category_map, fallback_category_ids, accept_matches[]. Returns the plan and the confirm_token for /ingest/file/apply."},
		{Method: "POST", Path: "/ingest/file/apply", Tier: TierWrite, DryRunnable: true, Composed: true, Feature: featureImport, Handler: ingHandleFileApply, Untrusted: ingUntrusted,
			Summary: "Import one planned file (confirm_token, dry_run, max_changes). Undoable."},
		{Method: "GET", Path: "/ingest/runs", Tier: TierRead, Handler: ingHandleRuns,
			Summary: "The ingest run log of the bound user, newest first (limit)."},
		{Method: "GET", Path: "/ingest/runs/:id", Tier: TierRead, Handler: ingHandleRun, Untrusted: []string{"name"},
			Summary: "One run's full report: per-account outcome, unmapped categories, the rows that went to a fallback category, the journal entry that undoes it."},
	}
}

// ---------------------------------------------------------------------------------------------
// Converters
// ---------------------------------------------------------------------------------------------

type ingConverterInfo struct {
	FileType   string   `json:"file_type"`
	Format     string   `json:"format"`
	Extensions []string `json:"extensions"`
	BankIds    string   `json:"bank_ids,omitempty"`
	Needs      string   `json:"needs,omitempty"`
	Currency   string   `json:"currency"`
	Plane      bool     `json:"used_by_plane"`
	Note       string   `json:"note,omitempty"`
}

var ingConverterCatalog = []ingConverterInfo{
	{FileType: "ofx", Format: "ofx", Extensions: []string{".ofx"}, BankIds: "FITID", Currency: "file", Plane: true},
	{FileType: "qfx", Format: "qfx", Extensions: []string{".qfx"}, BankIds: "FITID", Currency: "file", Plane: true},
	{FileType: "camt053", Format: "camt053", Extensions: []string{".xml"}, BankIds: "AcctSvcrRef / NtryRef", Currency: "file", Plane: true},
	{FileType: "camt052", Format: "camt052", Extensions: []string{".xml"}, BankIds: "AcctSvcrRef / NtryRef", Currency: "file", Plane: true},
	{FileType: "mt940", Format: "mt940", Extensions: []string{".sta", ".mt940", ".940", ".txt"}, Currency: "file", Plane: true, Note: "the converter does not surface the statement reference; ids are minted"},
	{FileType: "qif_ymd", Format: "qif", Extensions: []string{".qif"}, Needs: "qif_date_order", Currency: "account", Plane: true},
	{FileType: "qif_mdy", Format: "qif", Extensions: []string{".qif"}, Needs: "qif_date_order", Currency: "account", Plane: true},
	{FileType: "qif_dmy", Format: "qif", Extensions: []string{".qif"}, Needs: "qif_date_order", Currency: "account", Plane: true},
	{FileType: "iif", Format: "iif", Extensions: []string{".iif"}, Currency: "account", Plane: true},
	{FileType: "ezbookkeeping_csv", Format: "ezbookkeeping_csv", Extensions: []string{".csv"}, Currency: "file", Plane: true, Note: "raw mode stages this format"},
	{FileType: "ezbookkeeping_tsv", Format: "ezbookkeeping_tsv", Extensions: []string{".tsv"}, Currency: "file", Plane: true},
	{FileType: "ezbookkeeping_json", Format: "ezbookkeeping_json", Extensions: []string{".json"}, Currency: "file", Plane: true},
	{FileType: "gnucash", Format: "gnucash", Extensions: []string{".gnucash"}, Currency: "file", Plane: true},
	{FileType: "firefly_iii_csv", Format: "firefly_iii_csv", Extensions: []string{".csv"}, Currency: "file", Plane: true},
	{FileType: "beancount", Format: "beancount", Extensions: []string{".beancount", ".bean"}, Currency: "file", Plane: true},
	{FileType: "custom_csv", Format: "csv", Extensions: []string{".csv"}, Needs: "column_map", Currency: "file", Plane: true},
	{FileType: "custom_tsv", Format: "tsv", Extensions: []string{".tsv"}, Needs: "column_map", Currency: "file", Plane: true},
	{FileType: "custom_ssv", Format: "csv", Extensions: []string{".csv"}, Needs: "column_map", Currency: "file", Plane: true},
	{FileType: "custom_xlsx", Format: "xlsx", Extensions: []string{".xlsx"}, Needs: "column_map", Currency: "file", Plane: true},
	{FileType: "custom_xls", Format: "xls", Extensions: []string{".xls"}, Needs: "column_map", Currency: "file", Plane: true},
	{FileType: "feidee_mymoney_csv", Format: "feidee_mymoney_csv", Extensions: []string{".csv"}, Currency: "file", Plane: true},
	{FileType: "feidee_mymoney_xls", Format: "feidee_mymoney_xls", Extensions: []string{".xls"}, Currency: "file", Plane: true},
	{FileType: "feidee_mymoney_elecloud_xlsx", Format: "feidee_mymoney_elecloud_xlsx", Extensions: []string{".xlsx"}, Currency: "file", Plane: true},
	{FileType: "alipay_app_csv", Format: "alipay_app_csv", Extensions: []string{".csv"}, Currency: "file", Plane: true},
	{FileType: "alipay_web_csv", Format: "alipay_web_csv", Extensions: []string{".csv"}, Currency: "file", Plane: true},
	{FileType: "wechat_pay_app_xlsx", Format: "wechat_pay_app_xlsx", Extensions: []string{".xlsx"}, Currency: "file", Plane: true},
	{FileType: "wechat_pay_app_csv", Format: "wechat_pay_app_csv", Extensions: []string{".csv"}, Currency: "file", Plane: true},
	{FileType: "jdcom_finance_app_csv", Format: "jdcom_finance_app_csv", Extensions: []string{".csv"}, Currency: "file", Plane: true},
	{FileType: "ai_txt", Format: "ai_txt", Plane: false, Currency: "-", Note: "excluded: the machine plane never sends a statement to an LLM (apis.mdx §14.9)"},
	{FileType: "ai_image", Format: "ai_image", Plane: false, Currency: "-", Note: "excluded: the machine plane never sends a statement to an LLM (apis.mdx §14.9)"},
}

func ingHandleConverters(mc *Ctx) (any, error) {
	enabled := mc.Config.EnableDataImport
	out := make([]map[string]any, 0, len(ingConverterCatalog))

	for _, c := range ingConverterCatalog {
		supported := converters.IsCustomFileFormatFileType(c.FileType)

		if !supported {
			if imp, err := converters.GetTransactionDataImporter(c.FileType); err == nil && imp != nil {
				supported = true
			}
		}

		item := map[string]any{
			"file_type": c.FileType, "format": c.Format, "extensions": ingNonNil(c.Extensions), "supported": supported,
			"enabled": enabled && supported && c.Plane, "used_by_plane": c.Plane, "currency_from": c.Currency,
		}

		if c.BankIds != "" {
			item["bank_ids"] = c.BankIds
		}

		if c.Needs != "" {
			item["needs"] = c.Needs
		}

		if c.Note != "" {
			item["note"] = c.Note
		}

		out = append(out, item)
	}

	res := map[string]any{"converters": out, "import_enabled": enabled, "max_import_file_size": mc.Config.MaxImportFileSize}

	if !enabled {
		res["hint"] = "import is switched off: set [data] enable_import = true in conf/ezbookkeeping.ini and restart"
	}

	return res, nil
}

func ingNonNil(s []string) []string {
	if s == nil {
		return []string{}
	}

	return s
}

// ---------------------------------------------------------------------------------------------
// Roots, manifest, scan, coverage
// ---------------------------------------------------------------------------------------------

func ingHandleRoots(mc *Ctx) (any, error) {
	var roots []map[string]any
	seen := map[string]bool{}

	add := func(display, source string) {
		if display == "" || seen[display] {
			return
		}

		seen[display] = true
		item := map[string]any{"root": display, "source": source, "readable": false, "exists": false}
		root, err := ingResolveRoot(display)

		if err != nil {
			item["problem"] = toFail(err).Message
			item["hint"] = toFail(err).Hint
			roots = append(roots, item)
			return
		}

		item["exists"] = true

		if _, err := os.ReadDir(root.Real); err == nil {
			item["readable"] = true
		}

		if mreal, err := ingFindManifest(root, ""); err == nil && mreal != "" {
			item["manifest_path"] = root.Rel(mreal)
			item["mode"] = "prepared"
		} else {
			errfile.Expected("looking for a manifest in the statements root", err)
			item["manifest_path"] = nil
			item["mode"] = "raw"
		}

		if st, err := ingResolveStaging(root, "", ""); err == nil {
			item["staging"] = map[string]any{"path": st.Rel, "exists": st.Exists()}

			if st.Exists() {
				if data, _ := st.ReadFile(ingAccountsFile); data != nil {
					item["extracted"] = true
				}

				if m, _, err := ingLoadMap(st); err == nil {
					item["mapped_accounts"] = len(m.Accounts)
				}
			}
		}

		roots = append(roots, item)
	}

	if creds, err := ReadCredentials(); err == nil && strings.TrimSpace(creds.StatementsRoot) != "" {
		add(strings.TrimSpace(creds.StatementsRoot), "ezbookkeeping.statements.root")
	}

	add(strings.TrimSpace(os.Getenv(ingEnvStatementDir)), ingEnvStatementDir+" (server environment)")

	out := map[string]any{"roots": roots, "configured": len(roots) > 0}

	if roots == nil {
		out["roots"] = []any{}
		out["hint"] = "set ezbookkeeping.statements.root in ~/.credentials/ezbookkeeping.json, or pass root on each ingest call"
	}

	return out, nil
}

func ingHandleManifest(mc *Ctx) (any, error) {
	ctx, err := ingOpenContext(mc, mc.Query("root"), mc.Query("manifest_path"), mc.Query("staging"), "", true)

	if err != nil {
		return nil, err
	}

	qif := strings.ToLower(mc.Query("qif_date_order"))
	accounts := make([]map[string]any, 0, len(ctx.Manifest.Rows))
	totals := map[string]int64{"accounts": 0, "transactions": 0, "statements": 0, "unreconciled": 0, "currency_defaulted": 0, "without_importable_files": 0, "mapped": 0}

	for _, row := range ctx.Manifest.Rows {
		shelf, serr := ingScanAccountShelf(ctx.Root, row, qif)
		warnings := append([]string{}, row.Warnings...)
		files := map[string]any{"formats": []string{}, "monthly": 0}

		if serr != nil {
			errfile.Expected("resolving the manifest row's account path", serr)
			warnings = append(warnings, "the account path is outside the statements root")
		} else {
			if !shelf.Exists {
				warnings = append(warnings, "the account directory does not exist")
			}

			files = map[string]any{"combined": nil, "monthly": shelf.Monthly, "formats": shelf.Formats, "converter": nil, "pdfs": shelf.PDFs, "sidecars": shelf.Sidecars}

			if shelf.Combined != "" {
				files["combined"] = shelf.Combined
			}

			if shelf.FileType != "" {
				files["converter"] = shelf.FileType
			}

			if shelf.Needs != "" {
				files["needs"] = shelf.Needs

				if shelf.Needs == "qif_date_order" {
					warnings = append(warnings, "QIF needs a date order (qif_ymd, qif_mdy or qif_dmy) in the manifest's qif_date_order column or the call; the plane never guesses it")
				} else {
					warnings = append(warnings, "custom CSV/Excel files need a column map; import them one at a time with POST /ingest/file/plan")
				}
			}

			if len(shelf.Chosen) == 0 {
				totals["without_importable_files"]++
			}
		}

		item := map[string]any{
			"account_key": row.AccountKey(), "entity": row.Entity, "institution": row.Institution, "label": row.Label, "last4": row.Last4,
			"kind": row.Kind, "currency": row.Currency, "currency_defaulted": row.CurrencyDefaulted, "path": row.Path, "line": row.Line,
			"file": ingNilIfEmpty(row.File), "name": ingNilIfEmpty(row.Name), "opening_balance": row.OpeningBalance, "opening_date": ingNilIfEmpty(row.OpeningDate),
			"statements": row.Statements, "transactions": row.Transactions, "first": ingNilIfEmpty(row.First), "last": ingNilIfEmpty(row.Last),
			"reconciled": row.Reconciled, "recon_na": row.ReconNA, "skip": row.Skip, "files": files, "warnings": warnings,
		}

		if e := ctx.Map.Accounts[row.AccountKey()]; e != nil {
			item["mapped_account_id"] = e.AccountId
			totals["mapped"]++
		} else {
			item["mapped_account_id"] = nil
		}

		accounts = append(accounts, item)
		totals["accounts"]++

		if row.Transactions != nil {
			totals["transactions"] += *row.Transactions
		}

		if row.Statements != nil {
			totals["statements"] += *row.Statements

			if row.Reconciled != nil {
				na := int64(0)

				if row.ReconNA != nil {
					na = *row.ReconNA
				}

				if d := *row.Statements - *row.Reconciled - na; d > 0 {
					totals["unreconciled"] += d
				}
			}
		}

		if row.CurrencyDefaulted {
			totals["currency_defaulted"]++
		}
	}

	return map[string]any{
		"root": ctx.Root.Display, "manifest_path": ctx.Manifest.Path, "format": ctx.Manifest.Format, "mode": ctx.Mode, "staging": ctx.Staging.Rel,
		"columns": ctx.Manifest.Columns, "missing_columns": ctx.Manifest.MissingColumns, "unknown_columns": ctx.Manifest.UnknownColumns,
		"aliases_used": ctx.Manifest.AliasesUsed, "accounts": accounts, "totals": totals, "warnings": ctx.Manifest.Warnings,
	}, nil
}

func ingNilIfEmpty(s string) any {
	if s == "" {
		return nil
	}

	return s
}

type ingScanArgs struct {
	Root         string `json:"root,omitempty"`
	ManifestPath string `json:"manifest_path,omitempty"`
	Mode         string `json:"mode,omitempty"`
	Entity       string `json:"entity,omitempty"`
	Institution  string `json:"institution,omitempty"`
	Account      string `json:"account,omitempty"`
	Year         string `json:"year,omitempty"`
	QifDateOrder string `json:"qif_date_order,omitempty"`
	Statements   bool   `json:"statements,omitempty"`
}

func ingHandleScan(mc *Ctx) (any, error) {
	var args ingScanArgs

	if err := mc.BindBody(&args); err != nil {
		return nil, err
	}

	if args.Year != "" && !ingYearDir.MatchString(args.Year) {
		return nil, Invalid("year is four digits, e.g. 2025", "year %q is not a year", args.Year)
	}

	mode := strings.ToLower(args.Mode)

	if mode != "" && mode != "prepared" && mode != "raw" && mode != "auto" {
		return nil, Invalid("mode is prepared or raw", "unknown mode %q", args.Mode)
	}

	if mode == "auto" {
		mode = ""
	}

	ctx, err := ingOpenContext(mc, args.Root, args.ManifestPath, "", "", false)

	if err != nil {
		return nil, err
	}

	filters := map[string]string{}

	for k, v := range map[string]string{"entity": args.Entity, "institution": args.Institution, "account": args.Account, "year": args.Year} {
		if v != "" {
			filters[k] = v
		}
	}

	res := ingScan(ctx.Root, ctx.Manifest, mode, filters, strings.ToLower(args.QifDateOrder))
	ingYearFilter(res, args.Year)

	if res.Truncated {
		mc.Truncated(ingScanMaxFiles)
	}

	out := map[string]any{"scan": res}

	if args.Statements {
		var stmts []*ingScanStatement

		for _, sa := range res.Accounts {
			for _, st := range sa.stmts {
				if args.Year == "" || strings.HasPrefix(st.Period, args.Year) {
					stmts = append(stmts, st)
				}
			}
		}

		limit := MaxLimit

		if len(stmts) > limit {
			stmts = stmts[:limit]
			mc.Truncated(limit)
		}

		if stmts == nil {
			stmts = []*ingScanStatement{}
		}

		out["statements"] = stmts
	}

	return out, nil
}

func ingHandleCoverage(mc *Ctx) (any, error) {
	ctx, err := ingOpenContext(mc, mc.Query("root"), mc.Query("manifest_path"), mc.Query("staging"), "", false)

	if err != nil {
		return nil, err
	}

	fromRows, err := mc.QueryBool("from_rows", false)

	if err != nil {
		return nil, err
	}

	account := mc.Query("account")
	filters := map[string]string{}

	if account != "" {
		filters["account"] = account
	}

	scan := ingScan(ctx.Root, ctx.Manifest, "", filters, "")
	var out []map[string]any
	totalMissing := 0

	for _, sa := range scan.Accounts {
		item := map[string]any{"account_key": sa.AccountKey, "entity": sa.Entity, "institution": sa.Institution, "account": sa.Account, "path": sa.Path, "source": "statements"}
		var present []string

		for _, y := range sa.Years {
			for _, m := range y.Months {
				present = append(present, m.Month)
			}
		}

		missing := sa.MissingMonths

		// a combined-file account has no dated statements: read its months from its rows
		if (len(present) == 0 || fromRows) && sa.manifest != nil && ctx.Mode == "prepared" {
			inputs, ierr := ingPreparedInputs(mc, ctx, []string{sa.AccountKey}, nil, "", nil)

			if ierr == nil && len(inputs) == 1 {
				months := map[string]bool{}

				for _, st := range inputs[0].Statements {
					for _, r := range st.Rows {
						months[r.Month] = true
					}
				}

				if len(months) > 0 {
					present = present[:0]

					for m := range months {
						present = append(present, m)
					}

					sort.Strings(present)
					missing = []string{}
					have := map[string]bool{}

					for _, m := range present {
						have[m] = true
					}

					for _, m := range ingMonthsBetween(present[0]+"-01", present[len(present)-1]+"-01") {
						if !have[m] {
							missing = append(missing, m)
						}
					}

					item["source"] = "rows"
				}
			}
		}

		if present == nil {
			present = []string{}
		}

		if missing == nil {
			missing = []string{}
		}

		item["present"] = present
		item["missing"] = missing

		if len(present) > 0 {
			item["first"], item["last"] = present[0], present[len(present)-1]
		}

		if sa.manifest != nil {
			item["manifest_first"], item["manifest_last"] = ingNilIfEmpty(sa.manifest.First), ingNilIfEmpty(sa.manifest.Last)
		}

		totalMissing += len(missing)
		out = append(out, item)
	}

	if out == nil {
		out = []map[string]any{}
	}

	return map[string]any{"root": ctx.Root.Display, "accounts": out, "totals": map[string]int{"accounts": len(out), "missing_months": totalMissing}}, nil
}

// ---------------------------------------------------------------------------------------------
// The map
// ---------------------------------------------------------------------------------------------

func ingHandleMapGet(mc *Ctx) (any, error) {
	ctx, err := ingOpenContext(mc, mc.Query("root"), mc.Query("manifest_path"), mc.Query("staging"), "", false)

	if err != nil {
		return nil, err
	}

	accounts, err := ingLoadAccounts(mc)

	if err != nil {
		return nil, err
	}

	unmapped := []map[string]any{}

	if ctx.Manifest != nil {
		for _, row := range ctx.Manifest.Rows {
			if ctx.Map.Accounts[row.AccountKey()] == nil && !row.Skip {
				unmapped = append(unmapped, map[string]any{"account_key": row.AccountKey(), "label": row.Label, "kind": row.Kind, "currency": row.Currency, "path": row.Path})
			}
		}
	}

	out := map[string]any{"root": ctx.Root.Display, "path": ctx.Staging.Rel + "/" + ingMapFile, "exists": ctx.MapRaw != nil, "map": ingMapView(ctx.Map, accounts), "unmapped_manifest_accounts": unmapped}

	if ctx.Map.UpdatedAt != "" {
		out["updated_at"] = ctx.Map.UpdatedAt
	}

	return out, nil
}

type ingMapPutBody struct {
	ingMapPutArgs
	WriteOpts
}

func ingHandleMapPut(mc *Ctx) (any, error) {
	var body ingMapPutBody

	if err := mc.BindBody(&body); err != nil {
		return nil, err
	}

	var ctx *ingContext
	var next *ingMap

	resolve := func() (*Plan, error) {
		c, n, diff, err := ingPlanMapPut(mc, &body.ingMapPutArgs)

		if err != nil {
			return nil, err
		}

		ctx, next = c, n
		counts := map[string]int{}
		var changing []*ingMapDiff

		for _, d := range diff {
			counts[d.Change]++

			if d.Change != "unchanged" {
				changing = append(changing, d)
			}
		}

		if changing == nil {
			changing = []*ingMapDiff{}
		}

		return &Plan{Changes: counts, Count: len(changing), Preview: map[string]any{"path": c.Staging.Rel + "/" + ingMapFile, "diff": diff}, Fingerprinted: changing}, nil
	}

	apply := func(p *Plan) (any, error) {
		prevRaw := ctx.MapRaw
		newRaw, err := ingSaveMap(ctx.Staging, next, mc.User.Username)

		if err != nil {
			return nil, err
		}

		if _, err := RecordJournal(mc, "ingest map: "+strings.TrimSpace(ingCountsLine(p.Changes)), p.Count, []InverseOp{ingMapRestoreOp(ctx, prevRaw, newRaw)}); err != nil {
			errfile.Caught("recording the undo journal entry for the ingest map", err)
			return map[string]any{"path": ctx.Staging.Rel + "/" + ingMapFile, "entries": len(next.Accounts), "warning": "the map was written but the undo journal entry was not"}, nil
		}

		return map[string]any{"path": ctx.Staging.Rel + "/" + ingMapFile, "entries": len(next.Accounts)}, nil
	}

	return RunWrite(mc, body.WriteOpts, resolve, apply)
}

func ingCountsLine(m map[string]int) string {
	keys := make([]string, 0, len(m))

	for k := range m {
		keys = append(keys, k)
	}

	sort.Strings(keys)
	var parts []string

	for _, k := range keys {
		parts = append(parts, k+"="+strconv.Itoa(m[k]))
	}

	return strings.Join(parts, " ")
}

func ingHandleMapInfer(mc *Ctx) (any, error) {
	var args ingAccountsArgs

	if err := mc.BindBody(&args); err != nil {
		return nil, err
	}

	return ingInferMap(mc, &args)
}

// ---------------------------------------------------------------------------------------------
// Accounts
// ---------------------------------------------------------------------------------------------

func ingHandleAccountsPlan(mc *Ctx) (any, error) {
	var args ingAccountsArgs

	if err := mc.BindBody(&args); err != nil {
		return nil, err
	}

	plan, err := ingPlanAccounts(mc, &args)

	if err != nil {
		return nil, err
	}

	changes := ingAccountsChanges(plan)
	fp := ChangeFingerprint(changes)
	token, expires := ingIssueToken(mc, "POST", "/ingest/accounts/apply", fp)
	ingRememberArgs(token, "POST /ingest/accounts/apply", mc.Uid, args, nil, "")

	return map[string]any{
		"root": plan.Root, "manifest_path": plan.ManifestPath, "staging": plan.Staging, "naming": plan.Naming, "kind_to_category": plan.KindToCategory,
		"plan": plan.Plan, "summary": plan.Summary, "liabilities": plan.Liabilities, "currency_defaulted": plan.CurrencyDefaulted, "warnings": plan.Warnings,
		"changes": map[string]int{"create": plan.Summary["create"], "link": len(changes) - plan.Summary["create"]}, "change_set": changes,
		"confirm_token": token, "expires_at": expires.UTC().Format("2006-01-02T15:04:05Z"), "fingerprint": fp, "apply": "POST /machine/v1/ingest/accounts/apply",
	}, nil
}

type ingAccountsApplyBody struct {
	ingAccountsArgs
	WriteOpts
}

func ingHandleAccountsApply(mc *Ctx) (any, error) {
	var body ingAccountsApplyBody

	if err := mc.BindBody(&body); err != nil {
		return nil, err
	}

	args := body.ingAccountsArgs

	if body.ConfirmToken != "" && ingAccountsArgsEmpty(args) {
		if s := ingRecallArgs(body.ConfirmToken, "POST /ingest/accounts/apply", mc.Uid); s != nil {
			_ = json.Unmarshal(s.args, &args)
		}
	}

	var plan *ingAccountsPlan
	var changes []*ingAccountChange

	resolve := func() (*Plan, error) {
		p, err := ingPlanAccounts(mc, &args)

		if err != nil {
			return nil, err
		}

		plan, changes = p, ingAccountsChanges(p)
		counts := map[string]int{"create": 0, "link": 0, "skip": p.Summary["skip"], "ambiguous": p.Summary["ambiguous"]}

		for _, c := range changes {
			counts[c.Action]++
		}

		return &Plan{Changes: counts, Count: len(changes), Preview: map[string]any{"plan": p.Plan, "summary": p.Summary, "change_set": changes, "liabilities": p.Liabilities, "currency_defaulted": p.CurrencyDefaulted}, Fingerprinted: changes, Warnings: p.Warnings}, nil
	}

	apply := func(p *Plan) (any, error) {
		return ingApplyAccounts(mc, plan, changes)
	}

	out, err := RunWrite(mc, body.WriteOpts, resolve, apply)

	if err == nil {
		if wr, ok := out.(*WriteResult); ok && wr.DryRun && wr.ConfirmToken != "" {
			ingRememberArgs(wr.ConfirmToken, "POST /ingest/accounts/apply", mc.Uid, args, nil, "")
		}
	}

	return out, err
}

func ingAccountsArgsEmpty(a ingAccountsArgs) bool {
	return a.Root == "" && a.ManifestPath == "" && a.Staging == "" && a.Naming == "" && len(a.KindToCategory) == 0 && len(a.Overrides) == 0 && len(a.Accounts) == 0
}

// ---------------------------------------------------------------------------------------------
// Extract, dupes, rows
// ---------------------------------------------------------------------------------------------

type ingExtractArgs struct {
	Root          string   `json:"root,omitempty"`
	ManifestPath  string   `json:"manifest_path,omitempty"`
	Staging       string   `json:"staging,omitempty"`
	Force         bool     `json:"force,omitempty"`
	DateOrder     string   `json:"date_order,omitempty"`
	Accounts      []string `json:"accounts,omitempty"`
	Prefer        []string `json:"prefer,omitempty"`
	MaxStatements int      `json:"max_statements,omitempty"`
}

const ingDefaultMaxStatements = 5000

func ingHandleExtract(mc *Ctx) (any, error) {
	var args ingExtractArgs

	if err := mc.BindBody(&args); err != nil {
		return nil, err
	}

	order := strings.ToLower(strings.TrimSpace(args.DateOrder))

	switch order {
	case "":
		order = "mdy"
	case "mdy", "dmy", "ymd":
	default:
		return nil, Invalid("date_order is mdy, dmy or ymd", "unknown date_order %q", args.DateOrder)
	}

	if args.MaxStatements <= 0 {
		args.MaxStatements = ingDefaultMaxStatements
	}

	ctx, err := ingOpenContext(mc, args.Root, args.ManifestPath, args.Staging, "raw", false)

	if err != nil {
		return nil, err
	}

	prog := ingNewProgress(mc)
	prog.Emit("scan", 0, 1, nil)

	res, err := ingExtract(mc, ctx, args.Accounts, ingPreferSet(args.Prefer), order, args.Force, args.MaxStatements, prog.Func())

	if err != nil {
		return nil, err
	}

	prog.Emit("done", 1, 1, nil)

	if len(res.ZeroRow) > 0 {
		mc.SetMeta("partial", true)
	}

	return res, nil
}

func ingHandleDupes(mc *Ctx) (any, error) {
	ctx, err := ingOpenContext(mc, mc.Query("root"), mc.Query("manifest_path"), mc.Query("staging"), mc.Query("mode"), false)

	if err != nil {
		return nil, err
	}

	prefer := ingPreferSet(mc.QueryList("prefer"))
	accounts := mc.QueryList("account")
	var verdicts []*ingDupeVerdict
	var collapsed []*ingCollapse
	var conflicts []*ingConflict

	if ctx.Mode == "prepared" {
		inputs, err := ingPreparedInputs(mc, ctx, accounts, prefer, strings.ToLower(mc.Query("qif_date_order")), nil)

		if err != nil {
			return nil, err
		}

		for _, in := range inputs {
			conflicts = append(conflicts, in.Conflicts...)
			l1 := ingLayerOne(in.AccountKey, in.Statements)
			ingAssignIds(l1.Rows)
			_, c := ingLayerTwo(l1.Rows)
			verdicts = append(verdicts, l1.Verdicts...)
			collapsed = append(collapsed, c...)
			conflicts = append(conflicts, l1.Conflicts...)
		}
	} else {
		order := strings.ToLower(mc.Query("date_order"))

		if order == "" {
			order = "mdy"
		}

		verdicts, collapsed, conflicts, err = ingRawDupes(mc, ctx, accounts, prefer, order)

		if err != nil {
			return nil, err
		}
	}

	counts := map[string]int{}

	for _, v := range verdicts {
		counts[v.Verdict]++
	}

	counts["collapsed_rows"] = len(collapsed)
	counts["conflicts"] = len(conflicts)

	if verdicts == nil {
		verdicts = []*ingDupeVerdict{}
	}

	if collapsed == nil {
		collapsed = []*ingCollapse{}
	}

	if conflicts == nil {
		conflicts = []*ingConflict{}
	}

	return map[string]any{
		"root": ctx.Root.Display, "mode": ctx.Mode,
		"statement_level":   verdicts,
		"transaction_level": collapsed,
		"conflicts":         conflicts,
		"totals":            counts,
		"rules":             []string{"identical_bytes", "more_rows", "wider_coverage", "better_source", "newer_mtime", "first_path", "preferred", "same_bank_id"},
		"note":              "the machine_import_record comparison (already imported) is in POST /ingest/plan; this route shows only what the two layers collapsed",
	}, nil
}

func ingHandleRows(mc *Ctx) (any, error) {
	ctx, err := ingOpenContext(mc, mc.Query("root"), mc.Query("manifest_path"), mc.Query("staging"), mc.Query("mode"), false)

	if err != nil {
		return nil, err
	}

	limit, err := mc.QueryInt("limit", DefaultLimit)

	if err != nil {
		return nil, err
	}

	offset, err := mc.QueryInt("offset", 0)

	if err != nil {
		return nil, err
	}

	if limit <= 0 {
		limit = DefaultLimit
	}

	if limit > MaxLimit {
		limit = MaxLimit
		mc.Truncated(MaxLimit)
	}

	rng, err := ingPlanRange(mc, mc.Query("start"), mc.Query("end"))

	if err != nil {
		return nil, err
	}

	accounts := mc.QueryList("account")
	var inputs []*ingAccountInput

	if ctx.Mode == "prepared" {
		inputs, err = ingPreparedInputs(mc, ctx, accounts, ingPreferSet(mc.QueryList("prefer")), strings.ToLower(mc.Query("qif_date_order")), nil)
	} else {
		inputs, err = ingRawInputs(mc, ctx, accounts, nil)
	}

	if err != nil {
		return nil, err
	}

	plan, err := ingEvaluate(mc, inputs, ctx.Map, ingEngineOpts{Range: rng, Lenient: true})

	if err != nil {
		return nil, err
	}

	statuses := map[string]bool{}

	for _, s := range mc.QueryList("status") {
		statuses[s] = true
	}

	var rows []*ingPlanRow

	for _, r := range plan.allRows {
		if r.Status == "out_of_range" && rng != nil {
			continue
		}

		if len(statuses) > 0 && !statuses[r.Status] {
			continue
		}

		rows = append(rows, r)
	}

	total := len(rows)

	if int(offset) < len(rows) {
		rows = rows[offset:]
	} else {
		rows = nil
	}

	if int64(len(rows)) > limit {
		rows = rows[:limit]
		mc.Truncated(int(limit))
	}

	if rows == nil {
		rows = []*ingPlanRow{}
	}

	counts := map[string]int{}

	for _, r := range plan.allRows {
		counts[r.Status]++
	}

	return map[string]any{"root": ctx.Root.Display, "mode": ctx.Mode, "range": plan.Range, "rows": rows, "total": total, "offset": offset, "status_counts": counts,
		"note": "status is computed without requiring the map or categories; POST /ingest/plan is the authority on what an apply would do"}, nil
}

// ---------------------------------------------------------------------------------------------
// Plan and apply (statements)
// ---------------------------------------------------------------------------------------------

func ingHandlePlan(mc *Ctx) (any, error) {
	var args ingPlanArgs

	if err := mc.BindBody(&args); err != nil {
		return nil, err
	}

	plan, err := ingPlanStatements(mc, &args, nil)

	if err != nil {
		return nil, err
	}

	fp := ChangeFingerprint(ingPlanFingerprintSource(plan))
	token, expires := ingIssueToken(mc, "POST", "/ingest/apply", fp)
	ingRememberArgs(token, "POST /ingest/apply", mc.Uid, args, nil, "")

	return map[string]any{
		"plan":          plan,
		"changes":       map[string]int{"create": len(plan.changes.Creates), "link": len(plan.changes.Links)},
		"confirm_token": token,
		"expires_at":    expires.UTC().Format("2006-01-02T15:04:05Z"),
		"fingerprint":   fp,
		"apply":         "POST /machine/v1/ingest/apply with {\"confirm_token\": …, \"dry_run\": false, \"max_changes\": N}",
	}, nil
}

type ingApplyBody struct {
	ingPlanArgs
	WriteOpts
}

func ingPlanArgsEmpty(a ingPlanArgs) bool {
	b, _ := json.Marshal(a)
	return string(b) == "{}"
}

func ingHandleApply(mc *Ctx) (any, error) {
	var body ingApplyBody

	if err := mc.BindBody(&body); err != nil {
		return nil, err
	}

	args := body.ingPlanArgs

	if body.ConfirmToken != "" && ingPlanArgsEmpty(args) {
		if s := ingRecallArgs(body.ConfirmToken, "POST /ingest/apply", mc.Uid); s != nil {
			_ = json.Unmarshal(s.args, &args)
		} else {
			return nil, Conflict("re-run POST /ingest/plan and use its new confirm_token (tokens last 10 minutes and do not survive a restart)", "the confirm_token is unknown or expired")
		}
	}

	prog := ingNewProgress(mc)
	var plan *ingPlanResult
	var fingerprint string

	resolve := func() (*Plan, error) {
		p, err := ingPlanStatements(mc, &args, prog.Func())

		if err != nil {
			return nil, err
		}

		plan = p
		fingerprint = ChangeFingerprint(ingPlanFingerprintSource(p))

		return ingPlanToWritePlan(p), nil
	}

	apply := func(p *Plan) (any, error) {
		return ingApplyChanges(mc, plan, fingerprint, prog)
	}

	out, err := RunWrite(mc, body.WriteOpts, resolve, apply)

	if err == nil {
		if wr, ok := out.(*WriteResult); ok && wr.DryRun && wr.ConfirmToken != "" {
			ingRememberArgs(wr.ConfirmToken, "POST /ingest/apply", mc.Uid, args, nil, "")
		}
	}

	return out, err
}

// ---------------------------------------------------------------------------------------------
// One file
// ---------------------------------------------------------------------------------------------

func ingHandleFilePlan(mc *Ctx) (any, error) {
	args, _, data, name, err := ingFileRequest(mc, false)

	if err != nil {
		return nil, err
	}

	plan, fileData, fileName, err := ingPlanFile(mc, args, data, name)

	if err != nil {
		return nil, err
	}

	fp := ChangeFingerprint(ingPlanFingerprintSource(plan))
	token, expires := ingIssueToken(mc, "POST", "/ingest/file/apply", fp)

	var keep []byte

	if data != nil {
		keep = fileData
	}

	ingRememberArgs(token, "POST /ingest/file/apply", mc.Uid, args, keep, fileName)

	return map[string]any{
		"plan":          plan,
		"changes":       map[string]int{"create": len(plan.changes.Creates), "link": len(plan.changes.Links)},
		"confirm_token": token,
		"expires_at":    expires.UTC().Format("2006-01-02T15:04:05Z"),
		"fingerprint":   fp,
		"apply":         "POST /machine/v1/ingest/file/apply with {\"confirm_token\": …, \"dry_run\": false}",
	}, nil
}

func ingHandleFileApply(mc *Ctx) (any, error) {
	args, opts, data, name, err := ingFileRequest(mc, true)

	if err != nil {
		return nil, err
	}

	if opts.ConfirmToken != "" && data == nil && args.Path == "" {
		s := ingRecallArgs(opts.ConfirmToken, "POST /ingest/file/apply", mc.Uid)

		if s == nil {
			return nil, Conflict("re-run POST /ingest/file/plan and use its new confirm_token (tokens last 10 minutes and do not survive a restart)", "the confirm_token is unknown or expired")
		}

		stored := &ingFileArgs{}
		_ = json.Unmarshal(s.args, stored)
		ingMergeFileArgs(stored, args)
		args = stored
		data, name = s.fileData, s.fileName
	}

	prog := ingNewProgress(mc)
	var plan *ingPlanResult
	var fingerprint string
	var keptData []byte
	var keptName string

	resolve := func() (*Plan, error) {
		p, fd, fn, err := ingPlanFile(mc, args, data, name)

		if err != nil {
			return nil, err
		}

		plan, keptData, keptName = p, fd, fn
		fingerprint = ChangeFingerprint(ingPlanFingerprintSource(p))

		return ingPlanToWritePlan(p), nil
	}

	apply := func(p *Plan) (any, error) {
		return ingApplyChanges(mc, plan, fingerprint, prog)
	}

	out, err := RunWrite(mc, *opts, resolve, apply)

	if err == nil {
		if wr, ok := out.(*WriteResult); ok && wr.DryRun && wr.ConfirmToken != "" {
			var keep []byte

			if data != nil {
				keep = keptData
			}

			ingRememberArgs(wr.ConfirmToken, "POST /ingest/file/apply", mc.Uid, args, keep, keptName)
		}
	}

	return out, err
}

// ingMergeFileArgs lets an apply's explicit arguments (max_changes aside) override stored ones
func ingMergeFileArgs(dst, src *ingFileArgs) {
	if src.Start != "" {
		dst.Start = src.Start
	}

	if src.End != "" {
		dst.End = src.End
	}

	if src.CategoryMap != nil {
		dst.CategoryMap = src.CategoryMap
	}

	if src.FallbackCategoryIds != nil {
		dst.FallbackCategoryIds = src.FallbackCategoryIds
	}

	if src.AcceptMatches != nil {
		dst.AcceptMatches = src.AcceptMatches
	}

	if src.ReimportDeleted {
		dst.ReimportDeleted = true
	}
}

// ---------------------------------------------------------------------------------------------
// Runs
// ---------------------------------------------------------------------------------------------

func ingHandleRuns(mc *Ctx) (any, error) {
	limit, err := mc.QueryInt("limit", 50)

	if err != nil {
		return nil, err
	}

	if limit <= 0 {
		limit = 50
	}

	if limit > MaxLimit {
		limit = MaxLimit
	}

	runs, total, err := ingListRuns(mc.Uid, int(limit))

	if err != nil {
		return nil, err
	}

	if total > len(runs) {
		mc.Truncated(int(limit))
	}

	return map[string]any{"runs": runs, "total": total}, nil
}

func ingHandleRun(mc *Ctx) (any, error) {
	rep, err := ingLoadRunReport(mc.Uid, mc.Param("id"))

	if err != nil {
		return nil, err
	}

	recs, err := ingRecordsOfRun(mc, rep.RunId)

	if err != nil {
		return nil, err
	}

	byAccount := map[string]int{}

	for _, r := range recs {
		byAccount[r.AccountKey]++
	}

	return map[string]any{"run": rep, "records": map[string]any{"total": len(recs), "by_account": byAccount}}, nil
}

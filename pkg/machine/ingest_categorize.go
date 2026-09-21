package machine

import (
	"bytes"
	"encoding/csv"
	"fmt"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/mayswind/ezbookkeeping/pkg/errfile"
	"github.com/mayswind/ezbookkeeping/pkg/models"
)

// ingest_categorize.go — POST /ingest/categorize (apis.mdx §14.10): give rows that are ALREADY in
// the books the categories a statement file now carries, without importing anything again.
//
// The import (OFX) carries no category, so every imported row lands in a fallback category. The
// categoriser then writes the categories into the account's companion file — the ezBookkeeping TSV
// next to the import, with its Type / Category / Sub Category columns filled and a FITID column
// that repeats the OFX row's bank id. This route reads that file, turns each FITID into the durable
// import id ({account_key}|FITID:{fitid}), asks machine_import_record which transaction the row
// became, and hands the lot to the same plan POST /transactions/categorize builds: one previewed,
// confirmed, undoable write.
//
// The plane still never picks a category: every category comes from the file. It only decides
// which rows may take one:
//   - by default only rows still in a fallback category change (every sub-category named
//     "Uncategorized", or fallback_category_ids), so a category the operator set by hand is never
//     overwritten; overwrite: true lifts that;
//   - a row whose major category differs from the file's Type (an import row the reclassify pass
//     turned into a transfer, say) is skipped, never converted — the type change belongs to
//     /transactions/categorize with allow_type_change or /transactions/convert-to-transfer;
//   - a category path that does not exist is reported, never created: POST /categories/ensure first.

func init() {
	registerRoutes(ingCatzRoutes)
}

func ingCatzRoutes() []RouteDef {
	return []RouteDef{
		{Method: "POST", Path: "/ingest/categorize", Tier: TierWrite, DryRunnable: true, Composed: true, Summary: "Categorise already-imported rows from a statement file: each manifest account's companion TSV (Type, Category, Sub Category, FITID) → import record → transaction; only rows still in a fallback category unless overwrite; unknown paths reported, never created; dry run by default, undoable.", Handler: ingHandleCategorize, Untrusted: []string{"comment", "description", "path"}},
	}
}

type ingCatzBody struct {
	WriteOpts
	Root                string   `json:"root,omitempty"`
	ManifestPath        string   `json:"manifest_path,omitempty"`
	Accounts            []string `json:"accounts,omitempty"`
	File                string   `json:"file,omitempty"`
	FallbackCategoryIds []string `json:"fallback_category_ids,omitempty"`
	Overwrite           bool     `json:"overwrite,omitempty"`
	Limit               int      `json:"limit,omitempty"`
	SummaryOnly         bool     `json:"summary_only,omitempty"`
}

// ingCatzRow is one data row of a category file
type ingCatzRow struct {
	Line     int
	Type     models.TransactionCategoryType
	TypeName string
	Group    string
	Sub      string
	BankId   string
	Desc     string
}

// ingCatzSkip is one file row the route will not categorise, with the reason
type ingCatzSkip struct {
	Account string `json:"account"`
	Line    int    `json:"line"`
	Reason  string `json:"reason"`
	Detail  string `json:"detail,omitempty"`
}

// ingCatzReasons are the skip reasons, in the order the preview lists their counts
var ingCatzReasons = []string{"no_category", "no_fitid", "not_imported", "transaction_gone", "balance_modification", "type_differs", "already_categorized", "unknown_category", "duplicate", "over_limit"}

// ingCatzParseFile reads a category file: ezBookkeeping's own TSV (or a quoted CSV) with a header
// naming Type, Category, Sub Category and FITID; the other columns are ignored. Pure.
func ingCatzParseFile(data []byte, name string) ([]*ingCatzRow, error) {
	r := csv.NewReader(bytes.NewReader(bytes.TrimPrefix(data, []byte("\xef\xbb\xbf"))))
	r.FieldsPerRecord = -1
	r.LazyQuotes = true

	if strings.EqualFold(filepath.Ext(name), ".tsv") {
		r.Comma = '\t'
		r.LazyQuotes = false
		r.ReuseRecord = false
	}

	records, err := r.ReadAll()

	if err != nil {
		errfile.Expected("parsing a category file", err)
		return nil, Invalid("the file is ezBookkeeping's TSV (or a CSV) with a header row", "%s cannot be parsed: %v", name, err)
	}

	if len(records) == 0 {
		return nil, Invalid("the first row is the header: Type, Category, Sub Category, FITID", "%s is empty", name)
	}

	col := map[string]int{}

	for i, h := range records[0] {
		col[strings.ToLower(strings.TrimSpace(h))] = i
	}

	var missing []string

	for _, h := range []string{"type", "category", "sub category", "fitid"} {
		if _, ok := col[h]; !ok {
			missing = append(missing, h)
		}
	}

	if len(missing) > 0 {
		return nil, Invalid("the header needs Type, Category, Sub Category and FITID (the OFX row's bank id); regenerate the companion file", "%s has no column(s) %s", name, strings.Join(missing, ", ")).WithDetails(map[string]any{"missing_columns": missing})
	}

	get := func(rec []string, h string) string {
		i, ok := col[h]

		if !ok || i >= len(rec) {
			return ""
		}

		return strings.TrimSpace(rec[i])
	}

	rows := make([]*ingCatzRow, 0, len(records)-1)

	for n, rec := range records[1:] {
		if len(rec) == 1 && strings.TrimSpace(rec[0]) == "" {
			continue
		}

		row := &ingCatzRow{Line: n + 2, TypeName: get(rec, "type"), Group: get(rec, "category"), Sub: get(rec, "sub category"), BankId: get(rec, "fitid"), Desc: get(rec, "description")}

		switch strings.ToLower(row.TypeName) {
		case "income":
			row.Type = models.CATEGORY_TYPE_INCOME
		case "expense":
			row.Type = models.CATEGORY_TYPE_EXPENSE
		case "transfer":
			row.Type = models.CATEGORY_TYPE_TRANSFER
		}

		rows = append(rows, row)
	}

	return rows, nil
}

// ingCatzCompanion is the companion file of an account's import: same stem, .tsv
func ingCatzCompanion(file string) string {
	return strings.TrimSuffix(file, filepath.Ext(file)) + ".tsv"
}

// ingCatzAccountFile is one account's category file, resolved
type ingCatzAccountFile struct {
	Key  string
	Rel  string
	Real string
}

func ingCatzFiles(mc *Ctx, body *ingCatzBody) ([]ingCatzAccountFile, *ingContext, error) {
	ctx, err := ingOpenContext(mc, body.Root, body.ManifestPath, "", "", true)

	if err != nil {
		return nil, nil, err
	}

	var out []ingCatzAccountFile

	for _, row := range ctx.Manifest.Rows {
		if row.Skip || !ingAccountSelected(body.Accounts, row.AccountKey(), row.Label, row.Path, row.Last4) {
			continue
		}

		rel := strings.TrimSpace(body.File)

		if rel == "" {
			if strings.TrimSpace(row.File) == "" {
				return nil, nil, Invalid("give the manifest row a file column (its one importable file) or pass file", "account %s has no file in the manifest, so its companion file is unknown", row.AccountKey())
			}

			rel = ingCatzCompanion(row.File)
		}

		real, err := ctx.Root.Resolve(rel)

		if err != nil {
			return nil, nil, err
		}

		if !ingFileExists(real) {
			return nil, nil, NotFound("write the companion file (the ezBookkeeping TSV next to the import) or pass file", "account %s: %s does not exist", row.AccountKey(), rel)
		}

		out = append(out, ingCatzAccountFile{Key: row.AccountKey(), Rel: ctx.Root.Rel(real), Real: real})
	}

	if len(out) == 0 {
		return nil, nil, NotFound("GET /machine/v1/ingest/manifest lists the account keys", "no manifest account matches accounts %v", body.Accounts)
	}

	if strings.TrimSpace(body.File) != "" && len(out) > 1 {
		return nil, nil, Invalid("pass exactly one account with file", "file names one account's rows but %d accounts are selected", len(out))
	}

	return out, ctx, nil
}

func ingHandleCategorize(mc *Ctx) (any, error) {
	var body ingCatzBody

	if err := mc.BindBody(&body); err != nil {
		return nil, err
	}

	limit := body.Limit

	if limit <= 0 || limit > txnSelectCap {
		limit = txnSelectCap
	}

	resolve := func() (*Plan, error) {
		files, _, err := ingCatzFiles(mc, &body)

		if err != nil {
			return nil, err
		}

		lk, err := txnLoadLookup(mc)

		if err != nil {
			return nil, err
		}

		fallback := map[int64]bool{}
		fallbackIds := catzDefaultFallback(lk)

		if len(body.FallbackCategoryIds) > 0 {
			fallbackIds = nil

			for i, s := range body.FallbackCategoryIds {
				id, err := ResolveId(fmt.Sprintf("fallback_category_ids[%d]", i), s)

				if err != nil {
					return nil, err
				}

				fallbackIds = append(fallbackIds, id)
			}
		}

		for _, id := range fallbackIds {
			fallback[id] = true

			for _, c := range lk.categories {
				if c.ParentCategoryId == id {
					fallback[c.CategoryId] = true
				}
			}
		}

		if len(fallback) == 0 && !body.Overwrite {
			return nil, Invalid("pass fallback_category_ids (the import's fallback categories) or overwrite: true", "no sub-category is named Uncategorized, so there is no fallback to categorise out of")
		}

		type pending struct {
			account string
			row     *ingCatzRow
			path    string
			txnId   int64
		}

		var skips []ingCatzSkip
		counts := map[string]int{}
		skip := func(account string, line int, reason, detail string) {
			counts[reason]++
			skips = append(skips, ingCatzSkip{Account: account, Line: line, Reason: reason, Detail: detail})
		}

		var queue []pending
		fileRows := 0
		perAccount := make([]map[string]any, 0, len(files))

		for _, f := range files {
			data, err := ingReadLimited(f.Real, 0)

			if err != nil {
				errfile.Expected("reading a category file", err)
				return nil, NotFound("check the file is readable and under 64 MiB", "cannot read %s", f.Rel)
			}

			rows, err := ingCatzParseFile(data, f.Real)

			if err != nil {
				return nil, err
			}

			ids := make([]string, 0, len(rows))

			for _, r := range rows {
				if r.BankId != "" {
					ids = append(ids, ingBankImportId(f.Key, "FITID", r.BankId))
				}
			}

			recs, err := ingLoadRecords(mc, ids)

			if err != nil {
				return nil, err
			}

			withCategory := 0

			for _, r := range rows {
				fileRows++

				switch {
				case strings.EqualFold(r.TypeName, "Balance Modification"):
					skip(f.Key, r.Line, "balance_modification", "an opening balance takes no category")
					continue
				case r.Sub == "":
					skip(f.Key, r.Line, "no_category", "")
					continue
				case r.Type == 0:
					skip(f.Key, r.Line, "no_category", fmt.Sprintf("Type %q is not Income, Expense or Transfer", r.TypeName))
					continue
				case r.BankId == "":
					skip(f.Key, r.Line, "no_fitid", "the row carries no FITID, so it cannot be traced to its transaction")
					continue
				}

				withCategory++
				rec := recs[ingBankImportId(f.Key, "FITID", r.BankId)]

				if rec == nil {
					skip(f.Key, r.Line, "not_imported", "no import record for FITID "+r.BankId)
					continue
				}

				path := catzPath{Type: r.Type, Group: r.Group, Sub: r.Sub}.String()
				queue = append(queue, pending{account: f.Key, row: r, path: path, txnId: rec.TransactionId})
			}

			perAccount = append(perAccount, map[string]any{"account": f.Key, "file": f.Rel, "rows": len(rows), "withCategory": withCategory})
		}

		ids := make([]int64, 0, len(queue))

		for _, q := range queue {
			ids = append(ids, q.txnId)
		}

		states, canon, _, err := txnReadStates(mc, ids)

		if err != nil {
			return nil, err
		}

		// one assignment per category path, rows in file order
		byPath := map[string]*catzAssignment{}
		var order []string
		unknown := map[string]int{}
		resolved := map[string]*models.TransactionCategory{}
		seen := map[int64]bool{}
		assigned, overLimit := 0, 0

		for _, q := range queue {
			apiId, ok := canon[q.txnId]

			if !ok {
				skip(q.account, q.row.Line, "transaction_gone", "transaction "+idString(q.txnId)+" no longer exists")
				continue
			}

			s := states[apiId]
			have := txnCategoryTypeFor(models.TransactionType(s.Type))

			if models.TransactionType(s.Type) == models.TRANSACTION_TYPE_MODIFY_BALANCE {
				skip(q.account, q.row.Line, "balance_modification", "")
				continue
			}

			if have != q.row.Type {
				skip(q.account, q.row.Line, "type_differs", fmt.Sprintf("the file says %s, the transaction is now %s", txnCategoryTypeName(q.row.Type), txnTypeName(int64(s.Type))))
				continue
			}

			current, _ := strconv.ParseInt(s.CategoryId, 10, 64)

			if !body.Overwrite && !fallback[current] {
				counts["already_categorized"]++
				continue
			}

			if seen[apiId] {
				skip(q.account, q.row.Line, "duplicate", "another row of the file names transaction "+s.Id)
				continue
			}

			key := strings.ToLower(q.path)
			cat, known := resolved[key]

			if !known {
				cat, err = lk.resolveCategoryFor("", q.path, models.TransactionType(s.Type))

				if err != nil {
					if f, ok := err.(*Fail); ok && (f.Code == CodeNotFound || f.Code == CodeInvalidInput) {
						cat = nil
					} else {
						return nil, err
					}
				}

				resolved[key] = cat
			}

			if cat == nil {
				unknown[q.path]++
				counts["unknown_category"]++
				continue
			}

			if cat.CategoryId == current {
				counts["unchanged"]++
				continue
			}

			if assigned >= limit {
				overLimit++
				continue
			}

			seen[apiId] = true
			assigned++
			a := byPath[key]

			if a == nil {
				a = &catzAssignment{CategoryId: idString(cat.CategoryId)}
				byPath[key] = a
				order = append(order, key)
			}

			a.Ids = append(a.Ids, s.Id)
		}

		if overLimit > 0 {
			counts["over_limit"] = overLimit
		}

		unknownPaths := make([]map[string]any, 0, len(unknown))

		for p, n := range unknown {
			unknownPaths = append(unknownPaths, map[string]any{"path": p, "rows": n})
		}

		sort.Slice(unknownPaths, func(i, j int) bool {
			return unknownPaths[i]["rows"].(int) > unknownPaths[j]["rows"].(int) || (unknownPaths[i]["rows"].(int) == unknownPaths[j]["rows"].(int) && unknownPaths[i]["path"].(string) < unknownPaths[j]["path"].(string))
		})

		var plan *Plan

		if len(order) == 0 {
			plan = &Plan{Changes: map[string]int{"update": 0}, Preview: map[string]any{"rows": []any{}, "byCategory": []any{}}}
		} else {
			cb := catzCategorizeBody{WriteOpts: body.WriteOpts, SummaryOnly: body.SummaryOnly, SkipInvalid: true}

			for _, k := range order {
				cb.Assignments = append(cb.Assignments, *byPath[k])
			}

			plan, err = catzPlanAssignments(mc, lk, &cb)

			if err != nil {
				return nil, err
			}
		}

		reasons := map[string]int{}

		for _, r := range ingCatzReasons {
			if counts[r] > 0 {
				reasons[r] = counts[r]
			}
		}

		sort.SliceStable(skips, func(i, j int) bool { return skips[i].Account < skips[j].Account })
		samples := skips

		if len(samples) > 50 {
			samples = samples[:50]
		}

		if samples == nil {
			samples = []ingCatzSkip{}
		}

		preview := plan.Preview.(map[string]any)
		preview["files"] = perAccount
		preview["fileRows"] = fileRows
		preview["skippedByReason"] = reasons
		preview["skippedSamples"] = samples
		preview["unknownCategories"] = unknownPaths
		preview["alreadyCategorized"] = counts["already_categorized"]
		preview["remaining"] = overLimit

		for _, r := range ingCatzReasons {
			if counts[r] > 0 {
				plan.Changes["skip_"+r] = counts[r]
			}
		}

		if counts["unchanged"] > 0 {
			plan.Changes["unchanged"] += counts["unchanged"]
		}

		if len(unknownPaths) > 0 {
			plan.Warnings = append(plan.Warnings, fmt.Sprintf("%d rows name %d category paths that do not exist (preview.unknownCategories); create them with POST /machine/v1/categories/ensure, then plan again", counts["unknown_category"], len(unknownPaths)))
		}

		if overLimit > 0 {
			plan.Warnings = append(plan.Warnings, fmt.Sprintf("%d more rows would change beyond limit %d; apply this plan, then plan again — changed rows leave the fallback, so the next call picks up the rest", overLimit, limit))
		}

		if counts["type_differs"] > 0 {
			plan.Warnings = append(plan.Warnings, fmt.Sprintf("%d rows are skipped because their transaction's major category is no longer the file's Type (usually an import row reclassified into a transfer); nothing converts them here", counts["type_differs"]))
		}

		return plan, nil
	}

	return RunWrite(mc, body.WriteOpts, resolve, catzApply(mc))
}

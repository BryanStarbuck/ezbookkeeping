package machine

import (
	"bytes"
	"encoding/csv"
	"encoding/json"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/mayswind/ezbookkeeping/pkg/utils"
)

// ingest_extract.go — raw mode (apis.mdx §14.1, §14.3; cli.mdx §10.4).
//
// For every statement, rows are produced from its text sidecars, preferring _claude.txt >
// _brew.txt > _ocr.txt (a PDF with no sidecar has no usable extraction and is reported). Statement-
// level de-duplication runs on what the sidecars contain, and the result is written to the staging
// directory as ezBookkeeping CSV — one file per account-month — so even the raw path ends in the
// app's own ezbookkeeping_csv converter. Every row records source_file, source_kind and
// source_line. A statement yielding zero rows is named, never skipped silently.
//
// POST /ingest/extract is the one read-tier route that writes to disk, only under staging.

const (
	ingAccountsFile  = "_accounts.json"
	ingExtractorVer  = "1"
	ingEzbCSVHeader  = "Time,Timezone,Type,Category,Sub Category,Account,Account Currency,Amount,Account2,Account2 Currency,Account2 Amount,Geographic Location,Tags,Description"
	ingProvHeader    = "date,amount,currency,description,source_file,source_kind,source_line,import_id"
	ingSidecarMaxLen = 32 << 20
)

// ingRawAccount is one account of the staged raw archive (written to _accounts.json)
type ingRawAccount struct {
	AccountKey     string   `json:"account_key"`
	Entity         string   `json:"entity"`
	Institution    string   `json:"institution"`
	Account        string   `json:"account"`
	Last4          string   `json:"last4,omitempty"`
	Kind           string   `json:"kind,omitempty"`
	Currency       string   `json:"currency"`
	CurrencySource string   `json:"currency_source"`
	Dir            string   `json:"dir"`
	Path           string   `json:"path,omitempty"`
	Months         []string `json:"months"`
	BlockedMonths  []string `json:"blocked_months,omitempty"`
}

type ingRawAccountsFile struct {
	Version     int              `json:"version"`
	Extractor   string           `json:"extractor"`
	ExtractedAt string           `json:"extracted_at"`
	DateOrder   string           `json:"date_order"`
	Timezone    string           `json:"timezone"`
	Accounts    []*ingRawAccount `json:"accounts"`
}

// ingRawIssueRow is an unreadable sidecar line, named with its file
type ingRawIssueRow struct {
	AccountKey string `json:"account_key"`
	File       string `json:"file"`
	Line       int    `json:"line"`
	Reason     string `json:"reason"`
	Text       string `json:"text,omitempty"`
}

// ingRawStatementReport is extract's verdict on one statement
type ingRawStatementReport struct {
	AccountKey  string `json:"account_key"`
	Statement   string `json:"statement"`
	Source      string `json:"source,omitempty"`
	SourceKind  string `json:"source_kind,omitempty"`
	Shape       string `json:"shape,omitempty"`
	Rows        int    `json:"rows"`
	Unreadable  int    `json:"unreadable_lines"`
	PeriodFirst string `json:"period_first,omitempty"`
	PeriodLast  string `json:"period_last,omitempty"`
	Status      string `json:"status"` // extracted | zero_rows | no_usable_extraction
	Reason      string `json:"reason,omitempty"`
}

// ingRawBuild is the in-memory result of reading every sidecar
type ingRawBuild struct {
	Accounts   map[string]*ingRawAccount
	Statements map[string][]*ingStatement
	Reports    []*ingRawStatementReport
	Issues     []*ingRawIssueRow
	Order      []string
}

// ingBuildRaw reads every statement's best sidecar. It writes nothing.
func ingBuildRaw(mc *Ctx, ctx *ingContext, filter []string, prefer map[string]bool, dateOrder string, maxStatements int) (*ingRawBuild, *ingScanResult, error) {
	scan := ingScan(ctx.Root, ctx.Manifest, "raw", map[string]string{}, "")
	b := &ingRawBuild{Accounts: map[string]*ingRawAccount{}, Statements: map[string][]*ingStatement{}}
	count := 0

	for _, sa := range scan.Accounts {
		if !ingAccountSelected(filter, sa.AccountKey, sa.Account, sa.Path, sa.Last4) {
			continue
		}

		ra := &ingRawAccount{AccountKey: sa.AccountKey, Entity: sa.Entity, Institution: sa.Institution, Account: sa.Account, Last4: sa.Last4, Path: sa.Path, Months: []string{}}
		ra.Currency, ra.CurrencySource = ingRawCurrency(mc, ctx, sa)

		if sa.manifest != nil {
			ra.Kind = sa.manifest.Kind
		}

		ra.Dir = ingSafeComponent(sa.Entity) + "/" + ingSafeComponent(sa.Institution) + "/" + ingSafeComponent(sa.Account)
		b.Accounts[sa.AccountKey] = ra
		b.Order = append(b.Order, sa.AccountKey)

		for _, st := range sa.stmts {
			count++

			if maxStatements > 0 && count > maxStatements {
				return nil, nil, Conflict("narrow the run with accounts[], or raise max_statements deliberately", "the archive holds more than %d statements", maxStatements).WithDetails(map[string]any{"max_statements": maxStatements})
			}

			stemRel := ingJoinRel(st.Dir, st.Stem)
			rep := &ingRawStatementReport{AccountKey: sa.AccountKey, Statement: stemRel}
			b.Reports = append(b.Reports, rep)

			var chosen *ingSidecarResult
			var chosenKind, chosenRel string
			var chosenData []byte

			for _, kind := range []string{"claude", "brew", "ocr"} {
				p, ok := st.sidecarPaths[kind]

				if !ok {
					continue
				}

				data, err := ingReadLimited(p, ingSidecarMaxLen)

				if err != nil {
					b.Issues = append(b.Issues, &ingRawIssueRow{AccountKey: sa.AccountKey, File: ctx.Root.Rel(p), Reason: "unreadable_file"})
					continue
				}

				r := ingParseSidecar(string(bytes.TrimPrefix(data, []byte("\xef\xbb\xbf"))), st.PeriodDate, st.year, dateOrder)

				if chosen == nil || (len(chosen.Rows) == 0 && len(r.Rows) > 0) {
					chosen, chosenKind, chosenRel, chosenData = r, kind, ctx.Root.Rel(p), data
				}

				if len(r.Rows) > 0 {
					break
				}
			}

			if chosen == nil {
				rep.Status = "no_usable_extraction"

				if st.HasPDF {
					rep.Reason = "a PDF with no _claude.txt, _brew.txt or _ocr.txt sidecar; run the extraction pipeline first"
				} else {
					rep.Reason = "no text sidecar"
				}

				continue
			}

			rep.Source, rep.SourceKind, rep.Shape = chosenRel, "raw_"+chosenKind, chosen.Shape
			rep.Rows, rep.Unreadable = len(chosen.Rows), len(chosen.Issues)
			rep.PeriodFirst, rep.PeriodLast = chosen.PeriodFirst, chosen.PeriodLast

			for _, is := range chosen.Issues {
				b.Issues = append(b.Issues, &ingRawIssueRow{AccountKey: sa.AccountKey, File: chosenRel, Line: is.Line, Reason: is.Reason, Text: is.Text})
			}

			rowCurrency, rowCurrencySource := ra.Currency, ra.CurrencySource

			switch {
			case chosen.CurrencySeen == "MIXED":
				// one statement in two currencies is not something to split by guesswork
				b.Issues = append(b.Issues, &ingRawIssueRow{AccountKey: sa.AccountKey, File: chosenRel, Reason: "mixed_currencies: the sidecar's currency column holds more than one currency"})
				chosen.Rows = nil
			case chosen.CurrencySeen != "":
				// the statement says its currency; a mismatch with the account blocks it at plan time
				rowCurrency, rowCurrencySource = chosen.CurrencySeen, "file"

				if chosen.CurrencySeen != ra.Currency {
					b.Issues = append(b.Issues, &ingRawIssueRow{AccountKey: sa.AccountKey, File: chosenRel, Reason: "the statement is in " + chosen.CurrencySeen + " but the account's currency is " + ra.Currency})
				}
			}

			if len(chosen.Rows) == 0 {
				rep.Status = "zero_rows"
				rep.Reason = "the sidecar yielded no readable transaction rows"
				continue
			}

			rep.Status = "extracted"

			// the statement's identity for identical-bytes detection is its primary document
			sha := ingSha256(chosenData)

			if st.Primary != "" && strings.HasSuffix(strings.ToLower(st.Primary), ".pdf") {
				if real, err := ctx.Root.Resolve(st.Primary); err == nil {
					if data, err := ingReadLimited(real, 256<<20); err == nil {
						sha = ingSha256(data)
					}
				}
			}

			var mod time.Time

			if real, err := ctx.Root.Resolve(chosenRel); err == nil {
				if info, err := os.Stat(real); err == nil {
					mod = info.ModTime()
				}
			}

			stmt := &ingStatement{File: chosenRel, FileType: "raw_" + chosenKind, Sha: sha, ModTime: mod, Preferred: prefer[chosenRel] || prefer[st.Primary] || prefer[stemRel]}

			if chosen.PeriodFirst != "" {
				stmt.First, stmt.Last, stmt.PeriodSource = chosen.PeriodFirst, chosen.PeriodLast, "document"
			}

			for i, rr := range chosen.Rows {
				row := ingRawToRow(mc, sa.AccountKey, rowCurrency, rowCurrencySource, rr, chosenRel, "raw_"+chosenKind, i+1)
				stmt.Rows = append(stmt.Rows, row)
			}

			b.Statements[sa.AccountKey] = append(b.Statements[sa.AccountKey], stmt)
		}
	}

	sort.Strings(b.Order)

	return b, scan, nil
}

// ingRawToRow places a sidecar row at local midnight of its date in the call's timezone
func ingRawToRow(mc *Ctx, accountKey, currency, currencySource string, rr ingRawRow, file, kind string, index int) *ingRow {
	t, _ := time.ParseInLocation("2006-01-02", rr.Date, mc.Loc)

	return &ingRow{
		AccountKey: accountKey, Date: rr.Date, Month: rr.Date[:7], Time: t.Unix(), UtcOffset: UTCOffsetMinutes(t, mc.Loc),
		Amount: rr.Amount, Currency: currency, CurrencySource: currencySource, Description: rr.Description, NormDesc: ingNormDesc(rr.Description),
		SourceFile: file, SourceKind: kind, SourceLine: rr.Line, SourceIndex: index,
	}
}

// ingRawCurrency picks the account's currency: the manifest, then the map, then the bound user's
// default (said so)
func ingRawCurrency(mc *Ctx, ctx *ingContext, sa *ingScanAccount) (string, string) {
	if sa.manifest != nil && !sa.manifest.CurrencyDefaulted && sa.manifest.Currency != "" {
		return sa.manifest.Currency, "manifest"
	}

	if e := ctx.Map.Accounts[sa.AccountKey]; e != nil && e.Currency != "" {
		return e.Currency, "map"
	}

	return ingDefaultCurrency(mc), "default"
}

// ingSafeComponent makes a directory component safe inside staging
func ingSafeComponent(s string) string {
	s = strings.TrimSpace(s)
	s = strings.NewReplacer("/", "_", "\\", "_", "\x00", "_").Replace(s)

	if s == "" || s == "." || s == ".." {
		return "_"
	}

	if strings.HasPrefix(s, ".") {
		s = "_" + s[1:]
	}

	return s
}

// ingExtractResult is what POST /ingest/extract returns
type ingExtractResult struct {
	Root            string                   `json:"root"`
	Staging         string                   `json:"staging"`
	Mode            string                   `json:"mode"`
	DateOrder       string                   `json:"date_order"`
	Timezone        string                   `json:"timezone"`
	Force           bool                     `json:"force"`
	Totals          map[string]int           `json:"totals"`
	Accounts        []map[string]any         `json:"accounts"`
	ZeroRow         []*ingRawStatementReport `json:"zero_row_statements"`
	NoUsable        []*ingRawStatementReport `json:"no_usable_extraction"`
	Statements      []*ingDupeVerdict        `json:"statements"`
	Conflicts       []*ingConflict           `json:"conflicts"`
	Unreadable      []*ingRawIssueRow        `json:"unreadable_lines"`
	UnreadableTotal int                      `json:"unreadable_total"`
	Written         []string                 `json:"written"`
	Removed         []string                 `json:"removed"`
	Unchanged       int                      `json:"unchanged"`
	Warnings        []string                 `json:"warnings"`
	Ok              bool                     `json:"ok_every_statement_yielded_rows"`
}

// ingExtract runs extraction and writes the staging directory
func ingExtract(mc *Ctx, ctx *ingContext, filter []string, prefer map[string]bool, dateOrder string, force bool, maxStatements int, progress func(string, int, int)) (*ingExtractResult, error) {
	build, _, err := ingBuildRaw(mc, ctx, filter, prefer, dateOrder, maxStatements)

	if err != nil {
		return nil, err
	}

	if err := ctx.Staging.Ensure(); err != nil {
		return nil, err
	}

	out := &ingExtractResult{Root: ctx.Root.Display, Staging: ctx.Staging.Rel, Mode: "raw", DateOrder: dateOrder, Timezone: mc.Loc.String(), Force: force, Totals: map[string]int{},
		Accounts: []map[string]any{}, ZeroRow: []*ingRawStatementReport{}, NoUsable: []*ingRawStatementReport{}, Statements: []*ingDupeVerdict{}, Conflicts: []*ingConflict{},
		Unreadable: []*ingRawIssueRow{}, Written: []string{}, Removed: []string{}, Warnings: []string{}}

	if dateOrder == "mdy" {
		out.Warnings = append(out.Warnings, "numeric dates were read month-first (date_order=mdy); pass date_order=dmy for statements that write day-first")
	}

	// previous extraction, to remove account-months that no longer exist
	prev := &ingRawAccountsFile{}

	if data, _ := ctx.Staging.ReadFile(ingAccountsFile); data != nil {
		_ = json.Unmarshal(data, prev)
	}

	file := &ingRawAccountsFile{Version: 1, Extractor: ingExtractorVer, ExtractedAt: time.Now().UTC().Format(time.RFC3339), DateOrder: dateOrder, Timezone: mc.Loc.String()}
	var manifestRows, allRows, dupeRows, conflictRows [][]string
	keep := map[string]bool{}

	for i, key := range build.Order {
		if progress != nil {
			progress("extract", i, len(build.Order))
		}

		ra := build.Accounts[key]
		l1 := ingLayerOne(key, build.Statements[key])
		ingAssignIds(l1.Rows)
		rows, collapsed := ingLayerTwo(l1.Rows)
		out.Statements = append(out.Statements, l1.Verdicts...)
		out.Conflicts = append(out.Conflicts, l1.Conflicts...)

		for m := range l1.BlockedMonths {
			ra.BlockedMonths = append(ra.BlockedMonths, m)
		}

		sort.Strings(ra.BlockedMonths)

		for _, v := range l1.Verdicts {
			manifestRows = append(manifestRows, []string{key, v.File, v.Verdict, v.Rule, v.Winner, v.First, v.Last, strconv.Itoa(v.Rows), strconv.Itoa(v.RowsUsed)})

			if v.Verdict != "primary" {
				dupeRows = append(dupeRows, []string{"statement", key, v.File, v.Verdict, v.Rule, v.Winner, ""})
			}
		}

		for _, c := range collapsed {
			dupeRows = append(dupeRows, []string{"transaction", key, c.CollapsedSource, "collapsed", c.Rule, c.KeptSource, c.ImportId})
		}

		for _, c := range l1.Conflicts {
			conflictRows = append(conflictRows, []string{key, c.Kind, strings.Join(c.Files, " | "), strings.Join(c.Months, " "), "", c.Message})
		}

		byMonth := map[string][]*ingRow{}

		for _, r := range rows {
			byMonth[r.Month] = append(byMonth[r.Month], r)
		}

		months := make([]string, 0, len(byMonth))

		for m := range byMonth {
			months = append(months, m)
		}

		sort.Strings(months)
		ra.Months = months

		for _, m := range months {
			mrows := byMonth[m]
			ingSortRows(mrows)
			csvRel := ra.Dir + "/" + m + ".csv"
			provRel := ra.Dir + "/" + m + ".prov.csv"
			keep[csvRel], keep[provRel] = true, true

			csvData := ingRenderEzbCSV(mrows, ra.Account)
			provData := ingRenderProvenance(mrows)

			for _, pair := range [][2]any{{csvRel, csvData}, {provRel, provData}} {
				rel := pair[0].(string)
				data := pair[1].([]byte)
				existing, _ := ctx.Staging.ReadFile(rel)

				if !force && existing != nil && bytes.Equal(existing, data) {
					out.Unchanged++
					continue
				}

				if err := ctx.Staging.WriteFile(rel, data); err != nil {
					return nil, err
				}

				out.Written = append(out.Written, ctx.Staging.Rel+"/"+rel)
			}

			for _, r := range mrows {
				allRows = append(allRows, []string{key, r.ImportId, r.Date, strconv.FormatInt(r.Amount, 10), r.Currency, r.Description, r.SourceFile, r.SourceKind, strconv.Itoa(r.SourceLine)})
			}
		}

		acctRows := len(rows)
		zero := 0

		for _, rep := range build.Reports {
			if rep.AccountKey == key && rep.Status == "zero_rows" {
				zero++
			}
		}

		out.Accounts = append(out.Accounts, map[string]any{"account_key": key, "dir": ctx.Staging.Rel + "/" + ra.Dir, "currency": ra.Currency, "currency_source": ra.CurrencySource, "statements": len(build.Statements[key]), "rows": acctRows, "months": len(months), "blocked_months": ra.BlockedMonths, "zero_row_statements": zero, "collapsed": len(collapsed)})
		out.Totals["rows"] += acctRows
		out.Totals["collapsed"] += len(collapsed)
		out.Totals["account_months"] += len(months)
		out.Totals["blocked_months"] += len(ra.BlockedMonths)
		file.Accounts = append(file.Accounts, ra)
	}

	for _, rep := range build.Reports {
		out.Totals["statements"]++

		switch rep.Status {
		case "zero_rows":
			out.ZeroRow = append(out.ZeroRow, rep)
			conflictRows = append(conflictRows, []string{rep.AccountKey, "zero_rows", rep.Source, "", "", rep.Reason})
		case "no_usable_extraction":
			out.NoUsable = append(out.NoUsable, rep)
			conflictRows = append(conflictRows, []string{rep.AccountKey, "no_usable_extraction", rep.Statement, "", "", rep.Reason})
		default:
			out.Totals["extracted"]++
		}
	}

	for _, is := range build.Issues {
		conflictRows = append(conflictRows, []string{is.AccountKey, "unreadable_line", is.File, "", strconv.Itoa(is.Line), is.Reason + ": " + is.Text})
	}

	out.UnreadableTotal = len(build.Issues)

	if len(build.Issues) > 500 {
		out.Unreadable = build.Issues[:500]
		out.Warnings = append(out.Warnings, "unreadable_lines is capped at 500; the full list is in "+ctx.Staging.Rel+"/_conflicts.csv")
	} else if build.Issues != nil {
		out.Unreadable = build.Issues
	}

	out.Totals["zero_row_statements"] = len(out.ZeroRow)
	out.Totals["no_usable_extraction"] = len(out.NoUsable)
	out.Totals["unreadable_lines"] = len(build.Issues)
	out.Totals["conflicts"] = len(out.Conflicts)

	// remove account-months the previous extraction staged and this one did not
	if !(len(filter) > 0) {
		for _, pa := range prev.Accounts {
			for _, m := range pa.Months {
				for _, rel := range []string{pa.Dir + "/" + m + ".csv", pa.Dir + "/" + m + ".prov.csv"} {
					if !keep[rel] {
						if p, err := ctx.Staging.Path(rel); err == nil && ingFileExists(p) {
							if err := ctx.Staging.Remove(rel); err == nil {
								out.Removed = append(out.Removed, ctx.Staging.Rel+"/"+rel)
							}
						}
					}
				}
			}
		}
	} else {
		// a filtered run keeps the other accounts' entries
		have := map[string]bool{}

		for _, a := range file.Accounts {
			have[a.AccountKey] = true
		}

		for _, pa := range prev.Accounts {
			if !have[pa.AccountKey] {
				file.Accounts = append(file.Accounts, pa)
			}
		}

		sort.Slice(file.Accounts, func(i, j int) bool { return file.Accounts[i].AccountKey < file.Accounts[j].AccountKey })
	}

	accountsJSON, _ := json.MarshalIndent(file, "", "  ")

	writes := map[string][]byte{
		ingAccountsFile:  append(accountsJSON, '\n'),
		"_manifest.csv":  ingRenderCSV([]string{"account_key", "file", "verdict", "rule", "winner", "first", "last", "rows", "rows_used"}, manifestRows),
		"_rows.csv":      ingRenderCSV([]string{"account_key", "import_id", "date", "amount", "currency", "description", "source_file", "source_kind", "source_line"}, allRows),
		"_dupes.csv":     ingRenderCSV([]string{"level", "account_key", "file", "verdict", "rule", "kept", "import_id"}, dupeRows),
		"_conflicts.csv": ingRenderCSV([]string{"account_key", "kind", "files", "months", "line", "message"}, conflictRows),
	}

	names := make([]string, 0, len(writes))

	for n := range writes {
		names = append(names, n)
	}

	sort.Strings(names)

	for _, n := range names {
		if n == ingAccountsFile || n == "_manifest.csv" {
			// always rewritten (they carry the extraction time and verdicts)
		} else if existing, _ := ctx.Staging.ReadFile(n); !force && existing != nil && bytes.Equal(existing, writes[n]) {
			out.Unchanged++
			continue
		}

		if err := ctx.Staging.WriteFile(n, writes[n]); err != nil {
			return nil, err
		}

		out.Written = append(out.Written, ctx.Staging.Rel+"/"+n)
	}

	out.Ok = len(out.ZeroRow) == 0

	return out, nil
}

// ingRenderEzbCSV renders rows as ezBookkeeping CSV (the format upstream's exporter writes: the
// column separator inside a value becomes a space, as upstream's ReplaceDelimiters does)
func ingRenderEzbCSV(rows []*ingRow, accountName string) []byte {
	var b strings.Builder
	b.WriteString(ingEzbCSVHeader)
	b.WriteString("\n")
	clean := func(s string) string {
		return strings.NewReplacer("\r\n", " ", "\r", " ", "\n", " ", ",", " ").Replace(s)
	}

	for _, r := range rows {
		typ := "Expense"
		amt := -r.Amount

		if r.Amount > 0 {
			typ, amt = "Income", r.Amount
		}

		loc := time.FixedZone("row", int(r.UtcOffset)*60)
		t := time.Unix(r.Time, 0).In(loc)
		fields := []string{
			t.Format("2006-01-02 15:04:05"), utils.FormatTimezoneOffset(r.Time, loc), typ, "", "", clean(accountName), r.Currency,
			utils.FormatAmount(amt), "", "", "", "", "", clean(r.Description),
		}
		b.WriteString(strings.Join(fields, ","))
		b.WriteString("\n")
	}

	return []byte(b.String())
}

// ingRenderProvenance renders the provenance beside a staged month (the original description,
// untouched, and where each row came from)
func ingRenderProvenance(rows []*ingRow) []byte {
	var recs [][]string

	for _, r := range rows {
		recs = append(recs, []string{r.Date, strconv.FormatInt(r.Amount, 10), r.Currency, r.Description, r.SourceFile, r.SourceKind, strconv.Itoa(r.SourceLine), r.ImportId})
	}

	return ingRenderCSV(strings.Split(ingProvHeader, ","), recs)
}

func ingRenderCSV(header []string, rows [][]string) []byte {
	var buf bytes.Buffer
	w := csv.NewWriter(&buf)
	_ = w.Write(header)

	for _, r := range rows {
		_ = w.Write(r)
	}

	w.Flush()

	return buf.Bytes()
}

// ingRawInputs builds engine inputs from the staged raw archive (after POST /ingest/extract). Each
// staged account-month is parsed by upstream's ezbookkeeping_csv converter and its rows paired
// back to their provenance.
func ingRawInputs(mc *Ctx, ctx *ingContext, filter []string, progress func(string, int, int)) ([]*ingAccountInput, error) {
	data, err := ctx.Staging.ReadFile(ingAccountsFile)

	if err != nil {
		return nil, err
	}

	if data == nil {
		return nil, NotFound("run POST /ingest/extract first (ezbk statements extract); raw mode imports what extraction staged", "nothing has been extracted into %s yet", ctx.Staging.Rel)
	}

	file := &ingRawAccountsFile{}

	if err := json.Unmarshal(data, file); err != nil {
		return nil, Conflict("re-run POST /ingest/extract with force: true", "the staged %s is damaged", ingAccountsFile)
	}

	var inputs []*ingAccountInput
	var selected []*ingRawAccount

	for _, ra := range file.Accounts {
		if ingAccountSelected(filter, ra.AccountKey, ra.Account, ra.Path, ra.Last4) {
			selected = append(selected, ra)
		}
	}

	if len(filter) > 0 && len(selected) == 0 {
		return nil, NotFound("GET /ingest/scan lists the account keys", "accounts[] matched no extracted account")
	}

	for i, ra := range selected {
		if progress != nil {
			progress("parse", i, len(selected))
		}

		in := &ingAccountInput{AccountKey: ra.AccountKey, Entity: ra.Entity, Institution: ra.Institution, Label: ra.Account, Last4: ra.Last4, Kind: ra.Kind, Path: ra.Path, Currency: ra.Currency, CurrencySource: ra.CurrencySource, Converter: "ezbookkeeping_csv"}

		if ra.CurrencySource == "default" {
			in.Warnings = append(in.Warnings, "currency not stated in a manifest or the map; defaulted to "+ra.Currency)
		}

		if len(ra.BlockedMonths) > 0 {
			in.Warnings = append(in.Warnings, "account-months blocked at extraction by disagreeing statements: "+strings.Join(ra.BlockedMonths, ", "))
		}

		for _, m := range ra.Months {
			csvRel := ra.Dir + "/" + m + ".csv"
			provRel := ra.Dir + "/" + m + ".prov.csv"
			csvData, err := ctx.Staging.ReadFile(csvRel)

			if err != nil || csvData == nil {
				in.Conflicts = append(in.Conflicts, &ingConflict{AccountKey: ra.AccountKey, Kind: "missing_staged_file", Files: []string{ctx.Staging.Rel + "/" + csvRel}, Message: "a staged account-month is missing", Hint: "re-run POST /ingest/extract"})
				continue
			}

			provData, _ := ctx.Staging.ReadFile(provRel)
			in.Files++
			items, perr := ingParseUpstream(mc, csvData, m+".csv", "ezbookkeeping_csv", nil)

			if perr != nil {
				in.Conflicts = append(in.Conflicts, &ingConflict{AccountKey: ra.AccountKey, Kind: "unreadable_file", Files: []string{ctx.Staging.Rel + "/" + csvRel}, Message: "the ezbookkeeping_csv converter could not read the staged file: " + toFail(perr).Message, Hint: "re-run POST /ingest/extract with force: true"})
				continue
			}

			rows, skipped := ingNormalize(items, ingNormalizeInput{AccountKey: ra.AccountKey, SourceFile: ctx.Staging.Rel + "/" + csvRel, SourceKind: "ezbookkeeping_csv", FileType: "ezbookkeeping_csv", ExpectedCurrency: ra.Currency})
			in.Skipped = append(in.Skipped, skipped...)

			if !ingAttachProvenance(rows, provData) {
				in.Warnings = append(in.Warnings, csvRel+": provenance could not be paired with the staged rows; source lines are not reported for this month")
			}

			in.Statements = append(in.Statements, &ingStatement{File: ctx.Staging.Rel + "/" + csvRel, FileType: "ezbookkeeping_csv", Sha: ingSha256(csvData), Rows: rows})
		}

		inputs = append(inputs, in)
	}

	return inputs, nil
}

// ingAttachProvenance pairs converter rows with the provenance file (original description,
// source file and line)
func ingAttachProvenance(rows []*ingRow, prov []byte) bool {
	if len(prov) == 0 {
		return false
	}

	r := csv.NewReader(bytes.NewReader(prov))
	r.FieldsPerRecord = -1
	recs, err := r.ReadAll()

	if err != nil || len(recs) < 1 {
		return false
	}

	var refs []ingRef

	for _, rec := range recs[1:] {
		if len(rec) < 8 {
			return false
		}

		amt, err := strconv.ParseInt(rec[1], 10, 64)

		if err != nil {
			return false
		}

		line, _ := strconv.Atoi(rec[6])
		refs = append(refs, ingRef{Date: rec[0], Amount: amt, NormDesc: ingNormDesc(rec[3]), Id: rec[3], Source: rec[4], SKind: rec[5], Line: line})
	}

	match, ok := ingAlign(rows, refs, true)

	if !ok {
		return false
	}

	for i, row := range rows {
		ref := refs[match[i]]
		row.Description = ref.Id
		row.NormDesc = ingNormDesc(ref.Id)
		row.SourceFile = ref.Source
		row.SourceKind = ref.SKind
		row.SourceLine = ref.Line
	}

	return true
}

// ingRawDupes runs layer one over the raw sidecars without writing (for GET /ingest/dupes)
func ingRawDupes(mc *Ctx, ctx *ingContext, filter []string, prefer map[string]bool, dateOrder string) ([]*ingDupeVerdict, []*ingCollapse, []*ingConflict, error) {
	build, _, err := ingBuildRaw(mc, ctx, filter, prefer, dateOrder, 0)

	if err != nil {
		return nil, nil, nil, err
	}

	var verdicts []*ingDupeVerdict
	var collapsed []*ingCollapse
	var conflicts []*ingConflict

	for _, key := range build.Order {
		l1 := ingLayerOne(key, build.Statements[key])
		ingAssignIds(l1.Rows)
		_, c := ingLayerTwo(l1.Rows)
		verdicts = append(verdicts, l1.Verdicts...)
		collapsed = append(collapsed, c...)
		conflicts = append(conflicts, l1.Conflicts...)
	}

	return verdicts, collapsed, conflicts, nil
}

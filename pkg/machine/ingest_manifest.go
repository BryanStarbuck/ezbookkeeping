package machine

import (
	"bytes"
	"encoding/csv"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/mayswind/ezbookkeeping/pkg/errfile"
	"github.com/mayswind/ezbookkeeping/pkg/validators"
)

// ingest_manifest.go — the manifest (apis.mdx §14.2) and the per-account importable-file shelf.
//
// The manifest is the operator's: read, never rewritten. Columns the route needs and cannot find
// are reported, never guessed.

// ingManifestCandidates are tried, in order, when no manifest_path is given
//
// A manifest written for THIS app (…_ezbookkeeping.csv, pm/import_formats.mdx §11.2) is looked for
// first, so a statements root that also holds another app's manifest — which may list accounts
// this app must never import — never has the foreign one picked by default.
var ingManifestCandidates = []string{
	"import/personal/manifest_ezbookkeeping.csv", "import/manifest_ezbookkeeping.csv", "manifest_ezbookkeeping.csv",
	"import/accounts.csv", "import/accounts.json",
	"accounts.csv", "accounts.json",
	"manifest.csv", "manifest.json",
	"import/manifest.csv", "import/manifest.json",
}

var ingManifestRequired = []string{"entity", "institution", "label", "last4", "kind", "path"}

var ingManifestKnown = map[string]bool{
	"entity": true, "institution": true, "label": true, "last4": true, "kind": true, "currency": true, "path": true,
	"transactions": true, "statements": true, "first": true, "last": true, "reconciled": true, "recon_na": true,
	"qif_date_order": true, "file_type": true, "converter": true, "name": true, "skip": true, "notes": true, "note": true,
	"file": true, "opening_balance": true, "opening_date": true,
	// informational columns a statement pipeline writes (pm/import_formats.mdx §11.2); read by the
	// operator and the import script, never by the plane
	"type": true, "source_kind": true, "mode": true, "import_group": true, "last_printed_balance": true, "last_printed_date": true,
}

// ingManifestAliases are exact alternative spellings accepted for a column (echoed when used)
var ingManifestAliases = map[string]string{
	"bank": "institution", "account": "label", "account_label": "label", "last_4": "last4", "last_four": "last4",
	"type": "kind", "account_kind": "kind", "dir": "path", "directory": "path", "ccy": "currency",
	"import_file": "file", "combined_file": "file",
}

// ingKindAliases are account kinds other statement pipelines write, read as one of ingKinds and
// echoed as a row warning so the reading is visible (pm/import_formats.mdx §9)
var ingKindAliases = map[string]string{
	"retirement": "brokerage", "education": "brokerage", "investment": "brokerage", "cash_management": "brokerage",
	"mortgage": "loan", "debt": "loan", "credit_card": "card", "creditcard": "card",
}

// ingKinds are the manifest kinds (§14.2) plus cd (§15.3)
var ingKinds = map[string]bool{"checking": true, "savings": true, "card": true, "brokerage": true, "loan": true, "cash": true, "cd": true}

// ingManifestRow is one account of the manifest
type ingManifestRow struct {
	Line              int    `json:"line"`
	Entity            string `json:"entity"`
	Institution       string `json:"institution"`
	Label             string `json:"label"`
	Last4             string `json:"last4,omitempty"`
	Kind              string `json:"kind"`
	Currency          string `json:"currency"`
	CurrencyDefaulted bool   `json:"currency_defaulted,omitempty"`
	Path              string `json:"path"`
	Transactions      *int64 `json:"transactions,omitempty"`
	Statements        *int64 `json:"statements,omitempty"`
	First             string `json:"first,omitempty"`
	Last              string `json:"last,omitempty"`
	Reconciled        *int64 `json:"reconciled,omitempty"`
	ReconNA           *int64 `json:"recon_na,omitempty"`
	QifDateOrder      string `json:"qif_date_order,omitempty"`
	FileType          string `json:"file_type,omitempty"`
	Name              string `json:"name,omitempty"`
	Skip              bool   `json:"skip,omitempty"`
	// File names the account's one importable file, relative to the root. When set, the account's
	// shelf is exactly that file and path is not walked (several accounts may share one directory).
	File string `json:"file,omitempty"`
	// OpeningBalance is the balance before the first imported row, in signed hundredths (a liability
	// is negative), set on the account when /ingest/accounts/apply creates it; OpeningDate is the
	// first day the imported rows cover (YYYY-MM-DD). Both or neither.
	OpeningBalance *int64 `json:"opening_balance,omitempty"`
	OpeningDate    string `json:"opening_date,omitempty"`

	Warnings []string `json:"warnings"`
}

// AccountKey is {entity}/{institution}/{last4}; an account with no last4 keys on its label
func (r *ingManifestRow) AccountKey() string {
	return ingAccountKey(r.Entity, r.Institution, r.Last4, r.Label)
}

func ingAccountKey(entity, institution, last4, label string) string {
	tail := strings.TrimSpace(last4)

	if tail == "" {
		tail = strings.TrimSpace(label)
	}

	return strings.TrimSpace(entity) + "/" + strings.TrimSpace(institution) + "/" + tail
}

// ingManifest is a parsed manifest
type ingManifest struct {
	Path           string            `json:"manifest_path"`
	Format         string            `json:"format"`
	Columns        []string          `json:"columns"`
	MissingColumns []string          `json:"missing_columns"`
	UnknownColumns []string          `json:"unknown_columns"`
	AliasesUsed    map[string]string `json:"aliases_used,omitempty"`
	Rows           []*ingManifestRow `json:"-"`
	Warnings       []string          `json:"warnings"`
}

// ingFindManifest locates the manifest; ("", nil) when there is none and none was asked for
func ingFindManifest(root *ingRoot, manifestPath string) (string, error) {
	if strings.TrimSpace(manifestPath) != "" {
		real, err := root.Resolve(manifestPath)

		if err != nil {
			return "", err
		}

		if info, serr := os.Stat(real); serr != nil || info.IsDir() {
			errfile.Expected("checking the manifest path under the statements root", serr)
			return "", NotFound("pass manifest_path relative to the statements root, e.g. import/accounts.csv", "no manifest at %q", manifestPath)
		}

		return real, nil
	}

	for _, cand := range ingManifestCandidates {
		real, err := root.Resolve(cand)

		if err != nil {
			errfile.Expected("resolving a manifest candidate path", err)
			continue
		}

		if info, serr := os.Stat(real); serr == nil && !info.IsDir() {
			return real, nil
		}
	}

	return "", nil
}

// ingReadManifest parses the manifest at real (CSV or JSON)
func ingReadManifest(root *ingRoot, real, defaultCurrency string) (*ingManifest, error) {
	data, err := os.ReadFile(real)

	if err != nil {
		errfile.Caught("reading the manifest file", err)
		return nil, NotFound("check the server's user can read the manifest", "the manifest %q cannot be read", root.Rel(real))
	}

	m := &ingManifest{Path: root.Rel(real), AliasesUsed: map[string]string{}, MissingColumns: []string{}, UnknownColumns: []string{}, Warnings: []string{}}
	var records []map[string]string
	var lines []int

	if strings.EqualFold(filepath.Ext(real), ".json") {
		m.Format = "json"
		records, lines, err = ingParseManifestJSON(data)
	} else {
		m.Format = "csv"
		records, lines, err = ingParseManifestCSV(data)
	}

	if err != nil {
		return nil, Invalid("fix the manifest file; it must be a CSV with a header row or a JSON array of objects", "the manifest %q cannot be parsed: %s", m.Path, err.Error())
	}

	colSet := map[string]bool{}

	for _, rec := range records {
		for k := range rec {
			colSet[k] = true
		}
	}

	// normalise aliases
	for alias, canonical := range ingManifestAliases {
		if colSet[alias] && !colSet[canonical] {
			m.AliasesUsed[alias] = canonical

			for _, rec := range records {
				if v, ok := rec[alias]; ok {
					rec[canonical] = v
					delete(rec, alias)
				}
			}

			delete(colSet, alias)
			colSet[canonical] = true
		}
	}

	for k := range colSet {
		m.Columns = append(m.Columns, k)

		if !ingManifestKnown[k] {
			m.UnknownColumns = append(m.UnknownColumns, k)
		}
	}

	sort.Strings(m.Columns)
	sort.Strings(m.UnknownColumns)

	for _, req := range ingManifestRequired {
		if !colSet[req] {
			m.MissingColumns = append(m.MissingColumns, req)
		}
	}

	for i, rec := range records {
		row := &ingManifestRow{Line: lines[i], Warnings: []string{}}
		row.Entity = strings.TrimSpace(rec["entity"])
		row.Institution = strings.TrimSpace(rec["institution"])
		row.Label = strings.TrimSpace(rec["label"])
		row.Last4 = strings.TrimSpace(rec["last4"])
		row.Kind = strings.ToLower(strings.TrimSpace(rec["kind"]))
		row.Currency = strings.ToUpper(strings.TrimSpace(rec["currency"]))
		row.Path = strings.Trim(strings.TrimSpace(filepath.ToSlash(rec["path"])), "/")
		row.First = strings.TrimSpace(rec["first"])
		row.Last = strings.TrimSpace(rec["last"])
		row.QifDateOrder = strings.ToLower(strings.TrimSpace(rec["qif_date_order"]))
		row.FileType = strings.ToLower(strings.TrimSpace(rec["file_type"]))

		if row.FileType == "" {
			row.FileType = strings.ToLower(strings.TrimSpace(rec["converter"]))
		}

		row.Name = strings.TrimSpace(rec["name"])
		row.File = strings.Trim(strings.TrimSpace(filepath.ToSlash(rec["file"])), "/")

		if canon, ok := ingKindAliases[row.Kind]; ok {
			row.Warnings = append(row.Warnings, "kind "+strconv.Quote(row.Kind)+" read as "+canon)
			row.Kind = canon
		}

		if ob, od := strings.TrimSpace(rec["opening_balance"]), strings.TrimSpace(rec["opening_date"]); ob != "" || od != "" {
			amt, ok := ingDecimalHundredths(strings.ReplaceAll(ob, ",", ""))
			_, derr := time.Parse("2006-01-02", od)

			switch {
			case ob == "" || od == "":
				row.Warnings = append(row.Warnings, "opening_balance and opening_date go together; both ignored")
			case !ok:
				row.Warnings = append(row.Warnings, "opening_balance "+strconv.Quote(ob)+" is not a plain decimal; ignored")
			case derr != nil:
				errfile.Expected("parsing the manifest opening_date", derr)
				row.Warnings = append(row.Warnings, "opening_date "+strconv.Quote(od)+" is not YYYY-MM-DD; ignored")
			default:
				row.OpeningBalance, row.OpeningDate = &amt, od
			}
		}

		switch strings.ToLower(strings.TrimSpace(rec["skip"])) {
		case "1", "true", "yes", "y", "x":
			row.Skip = true
		}

		row.Transactions = ingOptInt(rec["transactions"])
		row.Statements = ingOptInt(rec["statements"])
		row.Reconciled = ingOptInt(rec["reconciled"])
		row.ReconNA = ingOptInt(rec["recon_na"])

		for _, req := range []string{"entity", "institution", "label", "kind", "path"} {
			if strings.TrimSpace(rec[req]) == "" {
				row.Warnings = append(row.Warnings, req+" is empty")
			}
		}

		if row.Last4 != "" && !ingDigits4.MatchString(row.Last4) {
			row.Warnings = append(row.Warnings, "last4 "+strconv.Quote(row.Last4)+" is not four digits")
		}

		if row.Kind != "" && !ingKinds[row.Kind] {
			row.Warnings = append(row.Warnings, "kind "+strconv.Quote(row.Kind)+" is not one of checking, savings, card, brokerage, loan, cash, cd")
		}

		if row.Currency == "" {
			row.Currency = defaultCurrency
			row.CurrencyDefaulted = true
			row.Warnings = append(row.Warnings, "currency not stated; defaulted to the bound user's default currency "+defaultCurrency)
		} else if !validators.AllCurrencyNames[row.Currency] {
			row.Warnings = append(row.Warnings, "currency "+strconv.Quote(row.Currency)+" is not an ISO 4217 code ezBookkeeping knows")
		}

		if row.QifDateOrder != "" && !ingQifOrders[row.QifDateOrder] {
			row.Warnings = append(row.Warnings, "qif_date_order "+strconv.Quote(row.QifDateOrder)+" is not ymd, mdy or dmy")
			row.QifDateOrder = ""
		}

		if row.Reconciled != nil && row.Statements != nil && *row.Reconciled < *row.Statements {
			na := int64(0)

			if row.ReconNA != nil {
				na = *row.ReconNA
			}

			if *row.Reconciled+na < *row.Statements {
				row.Warnings = append(row.Warnings, strconv.FormatInt(*row.Statements-*row.Reconciled-na, 10)+" statement(s) did not reconcile in the archive")
			}
		}

		m.Rows = append(m.Rows, row)
	}

	// two manifest rows with one account key cannot both be imported
	seen := map[string]*ingManifestRow{}

	for _, row := range m.Rows {
		if prev, ok := seen[row.AccountKey()]; ok {
			row.Warnings = append(row.Warnings, "same account key as manifest line "+strconv.Itoa(prev.Line)+"; add last4 or a distinct label")
		} else {
			seen[row.AccountKey()] = row
		}
	}

	if len(m.MissingColumns) > 0 {
		m.Warnings = append(m.Warnings, "missing required column(s): "+strings.Join(m.MissingColumns, ", "))
	}

	return m, nil
}

var ingDigits4 = regexp.MustCompile(`^\d{4}$`)

var ingQifOrders = map[string]bool{"ymd": true, "mdy": true, "dmy": true}

func ingOptInt(s string) *int64 {
	s = strings.TrimSpace(strings.ReplaceAll(s, ",", ""))

	if s == "" {
		return nil
	}

	n, err := strconv.ParseInt(s, 10, 64)

	if err != nil {
		errfile.Expected("parsing an integer cell of the manifest", err)
		return nil
	}

	return &n
}

func ingNormHeader(h string) string {
	h = strings.TrimSpace(strings.TrimPrefix(h, "\ufeff"))
	h = strings.ToLower(h)
	h = strings.NewReplacer(" ", "_", "-", "_").Replace(h)

	return h
}

func ingParseManifestCSV(data []byte) ([]map[string]string, []int, error) {
	r := csv.NewReader(bytes.NewReader(data))
	r.FieldsPerRecord = -1
	r.TrimLeadingSpace = true

	header, err := r.Read()

	if err != nil {
		return nil, nil, err
	}

	for i := range header {
		header[i] = ingNormHeader(header[i])
	}

	var out []map[string]string
	var lines []int

	for {
		rec, err := r.Read()

		if err == io.EOF {
			break
		}

		if err != nil {
			return nil, nil, err
		}

		line, _ := r.FieldPos(0)
		empty := true

		for _, v := range rec {
			if strings.TrimSpace(v) != "" {
				empty = false
				break
			}
		}

		if empty || strings.HasPrefix(strings.TrimSpace(rec[0]), "#") {
			continue
		}

		m := map[string]string{}

		for i, h := range header {
			if h == "" {
				continue
			}

			if i < len(rec) {
				m[h] = rec[i]
			} else {
				m[h] = ""
			}
		}

		out = append(out, m)
		lines = append(lines, line)
	}

	return out, lines, nil
}

func ingParseManifestJSON(data []byte) ([]map[string]string, []int, error) {
	var raw any
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()

	if err := dec.Decode(&raw); err != nil {
		return nil, nil, err
	}

	var items []any

	switch t := raw.(type) {
	case []any:
		items = t
	case map[string]any:
		if a, ok := t["accounts"].([]any); ok {
			items = a
		} else {
			return nil, nil, errNotArray
		}
	default:
		return nil, nil, errNotArray
	}

	var out []map[string]string
	var lines []int

	for i, it := range items {
		obj, ok := it.(map[string]any)

		if !ok {
			continue
		}

		m := map[string]string{}

		for k, v := range obj {
			key := ingNormHeader(k)

			switch vv := v.(type) {
			case string:
				m[key] = vv
			case json.Number:
				m[key] = vv.String()
			case bool:
				m[key] = strconv.FormatBool(vv)
			case nil:
				m[key] = ""
			default:
				b, _ := json.Marshal(vv)
				m[key] = string(b)
			}
		}

		out = append(out, m)
		lines = append(lines, i+1)
	}

	return out, lines, nil
}

type ingErr string

func (e ingErr) Error() string { return string(e) }

const errNotArray = ingErr("expected a JSON array of account objects (or {\"accounts\": [...]})")

// ---------------------------------------------------------------------------------------------
// The importable-file shelf of one account
// ---------------------------------------------------------------------------------------------

// ingSourceFile is one file under an account directory
type ingSourceFile struct {
	Rel      string    `json:"path"`
	Real     string    `json:"-"`
	Ext      string    `json:"ext"`
	Size     int64     `json:"size"`
	ModTime  time.Time `json:"-"`
	FileType string    `json:"converter,omitempty"`
	// Needs names what is missing before this file can be parsed (qif_date_order, column_map)
	Needs    string `json:"needs,omitempty"`
	Combined bool   `json:"combined,omitempty"`
	Format   string `json:"format"`
}

// ingFormatRank orders formats by how much a bank's own file is worth: a bank download with its own
// transaction ids first
var ingFormatRank = map[string]int{
	"ofx": 0, "qfx": 1, "camt053": 2, "camt052": 3, "mt940": 4,
	"ezbookkeeping_csv": 5, "ezbookkeeping_tsv": 6, "qif": 7, "iif": 8, "gnucash": 9, "beancount": 10,
	"firefly_iii_csv": 11, "csv": 20, "tsv": 21, "xlsx": 22, "xls": 23,
}

// ingSourceRank is the "better source" rule of the layer-one chain (lower is better)
func ingSourceRank(fileType string) int {
	switch {
	case fileType == "ofx" || fileType == "qfx" || strings.HasPrefix(fileType, "camt") || fileType == "mt940":
		return 0
	case fileType == "ezbookkeeping_csv" || fileType == "ezbookkeeping_tsv" || strings.HasPrefix(fileType, "qif") || fileType == "iif" || fileType == "gnucash" || fileType == "beancount" || fileType == "firefly_iii_csv":
		return 1
	case strings.HasPrefix(fileType, "custom_"):
		return 2
	case fileType == "raw_claude":
		return 3
	case fileType == "raw_brew":
		return 4
	case fileType == "raw_ocr":
		return 5
	}

	return 9
}

var ingCombinedName = regexp.MustCompile(`(?:^|[_\-. ])(?:ALL|All|all|combined|Combined|COMBINED)(?:[_\-. ]|$)`)

// ingClassifyImportable decides the converter for a file by extension and sniffed content. It
// returns format "" for files that are not importable.
func ingClassifyImportable(real string, qifOrder string) (format, fileType, needs string) {
	return ingClassifyName(filepath.Base(real), func(n int) string { return ingSniff(real, n) }, qifOrder)
}

// ingClassifyName classifies by name, sniffing content through head when the extension is not
// enough (an uploaded file has no path)
func ingClassifyName(name string, head func(n int) string, qifOrder string) (format, fileType, needs string) {
	ext := strings.ToLower(filepath.Ext(name))

	switch ext {
	case ".ofx":
		return "ofx", "ofx", ""
	case ".qfx":
		return "qfx", "qfx", ""
	case ".qif":
		if qifOrder == "" {
			return "qif", "", "qif_date_order"
		}

		return "qif", "qif_" + qifOrder, ""
	case ".iif":
		return "iif", "iif", ""
	case ".gnucash":
		return "gnucash", "gnucash", ""
	case ".beancount", ".bean":
		return "beancount", "beancount", ""
	case ".mt940", ".sta", ".940":
		return "mt940", "mt940", ""
	case ".xml", ".camt":
		head := head(4096)

		switch {
		case strings.Contains(head, "camt.053"):
			return "camt053", "camt053", ""
		case strings.Contains(head, "camt.052"):
			return "camt052", "camt052", ""
		}

		return "", "", ""
	case ".txt":
		head := head(2048)

		if strings.Contains(head, ":20:") && (strings.Contains(head, ":60F:") || strings.Contains(head, ":61:")) {
			return "mt940", "mt940", ""
		}

		return "", "", ""
	case ".csv", ".tsv":
		head := head(1024)
		first := head

		if i := strings.IndexAny(head, "\r\n"); i >= 0 {
			first = head[:i]
		}

		first = strings.TrimPrefix(first, "\ufeff")

		if ext == ".csv" && strings.HasPrefix(first, "Time,Timezone,Type,") {
			return "ezbookkeeping_csv", "ezbookkeeping_csv", ""
		}

		if ext == ".tsv" && strings.HasPrefix(first, "Time\tTimezone\tType\t") {
			return "ezbookkeeping_tsv", "ezbookkeeping_tsv", ""
		}

		if ext == ".csv" && strings.Contains(first, "source_name") && strings.Contains(first, "destination_name") && strings.Contains(first, "amount") {
			return "firefly_iii_csv", "firefly_iii_csv", ""
		}

		return strings.TrimPrefix(ext, "."), "", "column_map"
	case ".xlsx", ".xls":
		return strings.TrimPrefix(ext, "."), "", "column_map"
	}

	return "", "", ""
}

func ingSniff(real string, n int) string {
	f, err := os.Open(real)

	if err != nil {
		errfile.Expected("opening a statement file to sniff its format", err)
		return ""
	}

	defer f.Close()

	buf := make([]byte, n)
	k, _ := io.ReadFull(f, buf)
	s := string(buf[:k])

	if !utf8.ValidString(s) {
		s = strings.ToValidUTF8(s, "")
	}

	return s
}

// ingAccountShelf is what an account directory holds, and which files the plane will read
type ingAccountShelf struct {
	Dir      string           `json:"-"`
	DirRel   string           `json:"path"`
	Exists   bool             `json:"exists"`
	All      []*ingSourceFile `json:"-"`
	Chosen   []*ingSourceFile `json:"-"`
	Format   string           `json:"format"`
	FileType string           `json:"converter"`
	Combined string           `json:"combined,omitempty"`
	Monthly  int              `json:"monthly"`
	Formats  []string         `json:"formats"`
	Needs    string           `json:"needs,omitempty"`
	Sidecars int              `json:"sidecars"`
	PDFs     int              `json:"pdfs"`
}

// ingScanAccountShelf walks one account directory and chooses the format the plane will import
func ingScanAccountShelf(root *ingRoot, row *ingManifestRow, qifOrder string) (*ingAccountShelf, error) {
	shelf := &ingAccountShelf{DirRel: row.Path, Formats: []string{}}

	if row.File != "" {
		return ingScanFileShelf(root, row, qifOrder, shelf)
	}

	if row.Path == "" {
		return shelf, nil
	}

	dir, err := root.Resolve(row.Path)

	if err != nil {
		return nil, err
	}

	shelf.Dir = dir

	if info, err := os.Stat(dir); err != nil || !info.IsDir() {
		errfile.Expected("checking whether the statement directory of the account exists", err)
		return shelf, nil
	}

	shelf.Exists = true

	if row.QifDateOrder != "" {
		qifOrder = row.QifDateOrder
	}

	formats := map[string]bool{}

	_ = filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			errfile.Warn("walking the statement directory of the account", err)
			return nil
		}

		if d.IsDir() {
			if p != dir && ingSkipDir(d.Name()) {
				return filepath.SkipDir
			}

			return nil
		}

		if strings.HasPrefix(d.Name(), ".") {
			return nil
		}

		real, rerr := filepath.EvalSymlinks(p)

		if rerr != nil || !ingWithin(root.Real, real) {
			errfile.Expected("resolving a statement file path", rerr)
			return nil
		}

		lower := strings.ToLower(d.Name())

		switch {
		case strings.HasSuffix(lower, "_claude.txt") || strings.HasSuffix(lower, "_brew.txt") || strings.HasSuffix(lower, "_ocr.txt"):
			shelf.Sidecars++
			return nil
		case strings.HasSuffix(lower, ".pdf"):
			shelf.PDFs++
			return nil
		}

		format, fileType, needs := ingClassifyImportable(real, qifOrder)

		if format == "" {
			return nil
		}

		info, ierr := os.Stat(real)

		if ierr != nil {
			errfile.Warn("reading the size of a statement file", ierr)
			return nil
		}

		if row.FileType != "" && needs == "" && ingFormatOfFileType(row.FileType) == format {
			fileType = row.FileType
		} else if row.FileType != "" && strings.HasPrefix(row.FileType, "qif_") && format == "qif" {
			fileType, needs = row.FileType, ""
		}

		sf := &ingSourceFile{Rel: root.Rel(real), Real: real, Ext: strings.ToLower(filepath.Ext(real)), Size: info.Size(), ModTime: info.ModTime(), FileType: fileType, Needs: needs, Format: format, Combined: ingCombinedName.MatchString(strings.TrimSuffix(d.Name(), filepath.Ext(d.Name())))}
		shelf.All = append(shelf.All, sf)
		formats[format] = true

		return nil
	})

	for f := range formats {
		shelf.Formats = append(shelf.Formats, f)
	}

	sort.Slice(shelf.Formats, func(i, j int) bool { return ingFormatLess(shelf.Formats[i], shelf.Formats[j]) })
	sort.Slice(shelf.All, func(i, j int) bool { return shelf.All[i].Rel < shelf.All[j].Rel })

	// the manifest may name the converter; otherwise the best-ranked format present wins
	want := ""

	if row.FileType != "" {
		want = ingFormatOfFileType(row.FileType)
	} else if len(shelf.Formats) > 0 {
		want = shelf.Formats[0]
	}

	shelf.Format = want

	for _, f := range shelf.All {
		if f.Format != want {
			continue
		}

		shelf.Chosen = append(shelf.Chosen, f)

		if f.Needs != "" {
			shelf.Needs = f.Needs
		}

		if shelf.FileType == "" {
			shelf.FileType = f.FileType
		}

		if f.Combined && shelf.Combined == "" {
			shelf.Combined = filepath.Base(f.Rel)
		} else if !f.Combined {
			shelf.Monthly++
		}
	}

	return shelf, nil
}

func ingFormatLess(a, b string) bool {
	ra, oka := ingFormatRank[a]
	rb, okb := ingFormatRank[b]

	if !oka {
		ra = 99
	}

	if !okb {
		rb = 99
	}

	if ra != rb {
		return ra < rb
	}

	return a < b
}

// ingFormatOfFileType maps an ezBookkeeping fileType to our format family
func ingFormatOfFileType(ft string) string {
	switch {
	case strings.HasPrefix(ft, "qif_"):
		return "qif"
	case ft == "custom_csv":
		return "csv"
	case ft == "custom_tsv":
		return "tsv"
	case ft == "custom_ssv":
		return "csv"
	case ft == "custom_xlsx":
		return "xlsx"
	case ft == "custom_xls":
		return "xls"
	}

	return ft
}

// ingScanFileShelf is the shelf of a manifest row that names its one file: exactly that file,
// classified like any other; path is not walked
func ingScanFileShelf(root *ingRoot, row *ingManifestRow, qifOrder string, shelf *ingAccountShelf) (*ingAccountShelf, error) {
	real, err := root.Resolve(row.File)

	if err != nil {
		return nil, err
	}

	shelf.DirRel = row.File
	info, serr := os.Stat(real)

	if serr != nil || info.IsDir() {
		errfile.Expected("checking the manifest row's file", serr)
		return shelf, nil
	}

	shelf.Exists = true

	if row.QifDateOrder != "" {
		qifOrder = row.QifDateOrder
	}

	format, fileType, needs := ingClassifyImportable(real, qifOrder)

	if format == "" {
		return shelf, nil
	}

	if row.FileType != "" && needs == "" && ingFormatOfFileType(row.FileType) == format {
		fileType = row.FileType
	} else if row.FileType != "" && strings.HasPrefix(row.FileType, "qif_") && format == "qif" {
		fileType, needs = row.FileType, ""
	}

	sf := &ingSourceFile{Rel: root.Rel(real), Real: real, Ext: strings.ToLower(filepath.Ext(real)), Size: info.Size(), ModTime: info.ModTime(), FileType: fileType, Needs: needs, Format: format,
		Combined: ingCombinedName.MatchString(strings.TrimSuffix(filepath.Base(real), filepath.Ext(real)))}
	shelf.All = []*ingSourceFile{sf}
	shelf.Chosen = []*ingSourceFile{sf}
	shelf.Formats = []string{format}
	shelf.Format, shelf.FileType, shelf.Needs = format, fileType, needs

	if sf.Combined {
		shelf.Combined = filepath.Base(sf.Rel)
	} else {
		shelf.Monthly = 1
	}

	return shelf, nil
}

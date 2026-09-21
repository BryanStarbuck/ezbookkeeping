package machine

import (
	"bytes"
	"encoding/json"
	"html"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/mayswind/ezbookkeeping/pkg/api"
	"github.com/mayswind/ezbookkeeping/pkg/models"
)

// ingest_parse.go — every statement file is parsed by ezBookkeeping's OWN converter, through the
// same handler the browser's Import dialog calls (TransactionParseImportFileHandler). The plane
// never parses OFX/QFX/CAMT/MT940/QIF/CSV itself (apis.mdx §14.1, R7): it only normalises the
// converter's verdict into signed rows for the one account a file belongs to.
//
// The one thing the converter does not surface is the bank's own transaction id (OFX/QFX FITID,
// CAMT AcctSvcrRef/NtryRef). ingBankRefs reads ONLY those ids (with the posting date and amount
// that key them) and ingAlignBankIds attaches them to the converter's rows when the pairing is
// one-to-one; when it is not, the file falls back to minted ids and says so. Nothing the converter
// decided — dates, amounts, descriptions — is ever taken from that read.

// ingRow is one statement row, normalised to the perspective of the account it belongs to
type ingRow struct {
	AccountKey     string `json:"account_key"`
	Date           string `json:"date"`
	Month          string `json:"-"`
	Time           int64  `json:"time"`
	UtcOffset      int16  `json:"utc_offset"`
	Amount         int64  `json:"amount"` // signed hundredths: negative is money out of the account
	Currency       string `json:"currency"`
	CurrencySource string `json:"currency_source,omitempty"`
	Description    string `json:"description"`
	NormDesc       string `json:"-"`
	CategoryName   string `json:"category_name"`
	SourceFile     string `json:"source_file"`
	SourceKind     string `json:"source_kind"`
	SourceLine     int    `json:"source_line,omitempty"`
	SourceIndex    int    `json:"-"`
	BankId         string `json:"bank_id,omitempty"`
	BankIdKind     string `json:"bank_id_kind,omitempty"`
	ImportId       string `json:"import_id"`
	IdSource       string `json:"id_source"`
	Ordinal        int    `json:"ordinal,omitempty"`
}

// Key is the content identity used by statement-level comparison (never includes provenance)
func (r *ingRow) Key() string {
	return r.Date + "\x1f" + strconv.FormatInt(r.Amount, 10) + "\x1f" + r.Currency + "\x1f" + r.NormDesc
}

// ingSkippedRow is a converter row the plane will not import, and why
type ingSkippedRow struct {
	SourceFile string `json:"source_file"`
	Index      int    `json:"index"`
	Date       string `json:"date,omitempty"`
	Reason     string `json:"reason"`
}

// ingCustomOpts is the column map a custom delimited/Excel file needs (upstream's Import dialog
// fields, in snake_case)
type ingCustomOpts struct {
	FileType                  string          `json:"file_type,omitempty"`
	FileEncoding              string          `json:"file_encoding,omitempty"`
	ColumnMapping             json.RawMessage `json:"column_mapping,omitempty"`
	TransactionTypeMapping    json.RawMessage `json:"transaction_type_mapping,omitempty"`
	HasHeaderLine             *bool           `json:"has_header_line,omitempty"`
	TimeFormat                string          `json:"time_format,omitempty"`
	TimezoneFormat            string          `json:"timezone_format,omitempty"`
	AmountDecimalSeparator    string          `json:"amount_decimal_separator,omitempty"`
	AmountDigitGroupingSymbol string          `json:"amount_digit_grouping_symbol,omitempty"`
	GeoSeparator              string          `json:"geo_separator,omitempty"`
	GeoOrder                  string          `json:"geo_order,omitempty"`
	TagSeparator              string          `json:"tag_separator,omitempty"`
}

// ingNoCurrencyTypes are converters whose files carry no currency; their rows take the account's
var ingNoCurrencyTypes = map[string]bool{"qif_ymd": true, "qif_mdy": true, "qif_dmy": true, "iif": true}

// ingParseUpstream hands one file to upstream's parse handler and returns the converter's rows
func ingParseUpstream(mc *Ctx, data []byte, fileName, fileType string, custom *ingCustomOpts) ([]*models.ImportTransactionResponse, error) {
	fields := map[string]string{"fileType": fileType}

	if strings.HasPrefix(fileType, "custom_") {
		if custom == nil || len(custom.ColumnMapping) == 0 {
			return nil, Invalid("pass column_map with column_mapping, transaction_type_mapping and time_format (the same fields the browser's Import dialog asks for)", "file type %s needs a column map", fileType)
		}

		fields["fileEncoding"] = custom.FileEncoding
		fields["columnMapping"] = string(custom.ColumnMapping)
		fields["transactionTypeMapping"] = string(custom.TransactionTypeMapping)

		if custom.HasHeaderLine == nil || *custom.HasHeaderLine {
			fields["hasHeaderLine"] = "true"
		} else {
			fields["hasHeaderLine"] = "false"
		}

		fields["timeFormat"] = custom.TimeFormat
		fields["timezoneFormat"] = custom.TimezoneFormat
		fields["amountDecimalSeparator"] = custom.AmountDecimalSeparator
		fields["amountDigitGroupingSymbol"] = custom.AmountDigitGroupingSymbol
		fields["geoSeparator"] = custom.GeoSeparator
		fields["geoOrder"] = custom.GeoOrder
		fields["tagSeparator"] = custom.TagSeparator
	}

	if fileName == "" {
		fileName = "statement"
	}

	result, err := mc.CallUpstreamMultipart(api.Transactions.TransactionParseImportFileHandler, fields, []MultipartFile{{Field: "file", FileName: filepath.Base(fileName), Data: data}})

	if err != nil {
		return nil, err
	}

	switch t := result.(type) {
	case *models.ImportTransactionResponsePageWrapper:
		return t.Items, nil
	case models.ImportTransactionResponsePageWrapper:
		return t.Items, nil
	}

	var wrapper models.ImportTransactionResponsePageWrapper
	raw, merr := json.Marshal(result)

	if merr != nil {
		return nil, NewFail(CodeInternal, "read ~/T/ezbookkeeping/error.err", "unexpected parse result")
	}

	if uerr := json.Unmarshal(raw, &wrapper); uerr != nil {
		return nil, NewFail(CodeInternal, "read ~/T/ezbookkeeping/error.err", "unexpected parse result")
	}

	return wrapper.Items, nil
}

// ingNormalizeInput is what ingNormalize needs to know about the file
type ingNormalizeInput struct {
	AccountKey       string
	SourceFile       string
	SourceKind       string
	FileType         string
	ExpectedCurrency string
}

// ingNormalize turns the converter's rows into signed rows of the one account the file belongs to.
// Rows it cannot attribute are returned as skipped with a reason, never guessed.
func ingNormalize(items []*models.ImportTransactionResponse, in ingNormalizeInput) ([]*ingRow, []*ingSkippedRow) {
	ours := ingOurAccountName(items)
	var rows []*ingRow
	var skipped []*ingSkippedRow

	for i, it := range items {
		if it == nil {
			continue
		}

		offset := time.FixedZone("row", int(it.UtcOffset)*60)
		t := time.Unix(it.Time, 0).In(offset)
		date := t.Format("2006-01-02")
		row := &ingRow{
			AccountKey:   in.AccountKey,
			Date:         date,
			Month:        date[:7],
			Time:         it.Time,
			UtcOffset:    it.UtcOffset,
			Description:  strings.TrimSpace(it.Comment),
			CategoryName: strings.TrimSpace(it.OriginalCategoryName),
			SourceFile:   in.SourceFile,
			SourceKind:   in.SourceKind,
			SourceIndex:  i + 1,
		}

		switch it.Type {
		case models.TRANSACTION_TYPE_MODIFY_BALANCE:
			skipped = append(skipped, &ingSkippedRow{SourceFile: in.SourceFile, Index: i + 1, Date: date, Reason: "balance_modification: an opening balance is set on the account, never imported as a row"})
			continue
		case models.TRANSACTION_TYPE_INCOME, models.TRANSACTION_TYPE_EXPENSE:
			if ours != "" && it.OriginalSourceAccountName != ours {
				skipped = append(skipped, &ingSkippedRow{SourceFile: in.SourceFile, Index: i + 1, Date: date, Reason: "other_account: the row belongs to another account in the same file"})
				continue
			}

			row.Currency = it.OriginalSourceAccountCurrency

			if it.Type == models.TRANSACTION_TYPE_INCOME {
				row.Amount = it.SourceAmount
			} else {
				row.Amount = -it.SourceAmount
			}
		case models.TRANSACTION_TYPE_TRANSFER:
			src, dst := it.OriginalSourceAccountName, it.OriginalDestinationAccountName

			switch {
			case src == "" || (dst == ours && src != ours):
				row.Amount = it.DestinationAmount

				if row.Amount == 0 && it.SourceAmount != 0 {
					row.Amount = it.SourceAmount
				}

				row.Currency = it.OriginalDestinationAccountCurrency

				if row.Currency == "" {
					row.Currency = it.OriginalSourceAccountCurrency
				}
			case src == ours || ours == "":
				row.Amount = -it.SourceAmount
				row.Currency = it.OriginalSourceAccountCurrency
			default:
				skipped = append(skipped, &ingSkippedRow{SourceFile: in.SourceFile, Index: i + 1, Date: date, Reason: "other_account: a transfer between two accounts that are not this one"})
				continue
			}
		default:
			skipped = append(skipped, &ingSkippedRow{SourceFile: in.SourceFile, Index: i + 1, Date: date, Reason: "unknown_type: the converter returned a transaction type the plane does not import"})
			continue
		}

		row.CurrencySource = "file"

		if ingNoCurrencyTypes[in.FileType] || row.Currency == "" {
			row.Currency = in.ExpectedCurrency
			row.CurrencySource = "account"
		}

		row.NormDesc = ingNormDesc(row.Description)
		rows = append(rows, row)
	}

	return rows, skipped
}

// ingOurAccountName is the account name the converter used for the file's own account: the most
// frequent non-empty name across the rows (ties broken by name, deterministic)
func ingOurAccountName(items []*models.ImportTransactionResponse) string {
	counts := map[string]int{}

	for _, it := range items {
		if it == nil {
			continue
		}

		switch it.Type {
		case models.TRANSACTION_TYPE_INCOME, models.TRANSACTION_TYPE_EXPENSE:
			if it.OriginalSourceAccountName != "" {
				counts[it.OriginalSourceAccountName] += 2
			}
		case models.TRANSACTION_TYPE_TRANSFER:
			if it.OriginalSourceAccountName != "" {
				counts[it.OriginalSourceAccountName]++
			}

			if it.OriginalDestinationAccountName != "" {
				counts[it.OriginalDestinationAccountName]++
			}
		}
	}

	best, bestN := "", 0

	for name, n := range counts {
		if n > bestN || (n == bestN && name < best) {
			best, bestN = name, n
		}
	}

	return best
}

// ---------------------------------------------------------------------------------------------
// Bank ids (FITID / AcctSvcrRef / NtryRef) — read, never parsed into transactions
// ---------------------------------------------------------------------------------------------

// ingRef is one reference to align with a converter row: its identity (date + signed amount) and
// what to attach when it matches
type ingRef struct {
	Date     string
	Amount   int64
	NormDesc string
	// payload
	Id     string
	Kind   string
	Source string
	Line   int
	SKind  string
}

var (
	ingOfxTxnBlock = regexp.MustCompile(`(?is)<STMTTRN>(.*?)</STMTTRN>`)
	ingOfxFitid    = regexp.MustCompile(`(?i)<FITID>\s*([^<\r\n]+)`)
	ingOfxPosted   = regexp.MustCompile(`(?i)<DTPOSTED>\s*(\d{8})`)
	ingOfxAmount   = regexp.MustCompile(`(?i)<TRNAMT>\s*([^<\r\n]+)`)
	ingOfxMemo     = regexp.MustCompile(`(?i)<MEMO>\s*([^<\r\n]+)`)
	ingOfxName     = regexp.MustCompile(`(?i)<NAME>\s*([^<\r\n]+)`)

	ingCamtEntry   = regexp.MustCompile(`(?s)<(?:\w+:)?Ntry>(.*?)</(?:\w+:)?Ntry>`)
	ingCamtAmt     = regexp.MustCompile(`<(?:\w+:)?Amt[^>]*>\s*([0-9.,\-]+)\s*</`)
	ingCamtInd     = regexp.MustCompile(`<(?:\w+:)?CdtDbtInd>\s*(CRDT|DBIT)\s*<`)
	ingCamtBookg   = regexp.MustCompile(`(?s)<(?:\w+:)?BookgDt>\s*<(?:\w+:)?Dt(?:Tm)?>\s*(\d{4}-\d{2}-\d{2})`)
	ingCamtVal     = regexp.MustCompile(`(?s)<(?:\w+:)?ValDt>\s*<(?:\w+:)?Dt(?:Tm)?>\s*(\d{4}-\d{2}-\d{2})`)
	ingCamtSvcrRef = regexp.MustCompile(`<(?:\w+:)?AcctSvcrRef>\s*([^<]+?)\s*</`)
	ingCamtNtryRef = regexp.MustCompile(`<(?:\w+:)?NtryRef>\s*([^<]+?)\s*</`)
	ingCamtTxDtls  = regexp.MustCompile(`<(?:\w+:)?TxDtls>`)
)

// ingBankRefs reads the bank's own transaction ids from a file, with the posting date and signed
// amount that key them. It returns nil when the format carries none or the file has none.
func ingBankRefs(fileType string, data []byte) []ingRef {
	switch fileType {
	case "ofx", "qfx":
		return ingOfxRefs(data)
	case "camt052", "camt053":
		return ingCamtRefs(data)
	}

	return nil
}

func ingOfxRefs(data []byte) []ingRef {
	var refs []ingRef

	for _, m := range ingOfxTxnBlock.FindAllSubmatch(data, -1) {
		block := m[1]
		fitid := ingFirst(ingOfxFitid, block)
		posted := ingFirst(ingOfxPosted, block)
		amt := ingFirst(ingOfxAmount, block)

		if fitid == "" || posted == "" || amt == "" {
			return nil
		}

		amount, ok := ingDecimalHundredths(strings.ReplaceAll(amt, ",", "."))

		if !ok {
			return nil
		}

		desc := ingFirst(ingOfxMemo, block)

		if desc == "" {
			desc = ingFirst(ingOfxName, block)
		}

		refs = append(refs, ingRef{Date: posted[:4] + "-" + posted[4:6] + "-" + posted[6:8], Amount: amount, NormDesc: ingNormDesc(html.UnescapeString(desc)), Id: html.UnescapeString(fitid), Kind: "FITID"})
	}

	return refs
}

func ingCamtRefs(data []byte) []ingRef {
	var refs []ingRef

	for _, m := range ingCamtEntry.FindAllSubmatch(data, -1) {
		block := m[1]

		if len(ingCamtTxDtls.FindAllIndex(block, 2)) > 1 {
			// a batch entry the converter may split into several rows: no one-to-one id
			return nil
		}

		id, kind := ingFirst(ingCamtSvcrRef, block), "AcctSvcrRef"

		if id == "" {
			id, kind = ingFirst(ingCamtNtryRef, block), "NtryRef"
		}

		date := ingFirst(ingCamtBookg, block)

		if date == "" {
			date = ingFirst(ingCamtVal, block)
		}

		amt := ingFirst(ingCamtAmt, block)
		ind := ingFirst(ingCamtInd, block)

		if id == "" || date == "" || amt == "" || ind == "" {
			return nil
		}

		amount, ok := ingDecimalHundredths(strings.ReplaceAll(amt, ",", "."))

		if !ok {
			return nil
		}

		if amount < 0 {
			amount = -amount
		}

		if ind == "DBIT" {
			amount = -amount
		}

		refs = append(refs, ingRef{Date: date, Amount: amount, Id: html.UnescapeString(id), Kind: kind})
	}

	return refs
}

func ingFirst(re *regexp.Regexp, b []byte) string {
	m := re.FindSubmatch(b)

	if m == nil {
		return ""
	}

	return strings.TrimSpace(string(m[1]))
}

// ingDecimalHundredths parses a plain decimal ("-12.5", "+1234.56", "12") into hundredths exactly,
// with no float. More than two non-zero decimals is refused (the value is not whole hundredths).
func ingDecimalHundredths(s string) (int64, bool) {
	s = strings.TrimSpace(s)
	neg := false

	if strings.HasPrefix(s, "-") {
		neg, s = true, s[1:]
	} else if strings.HasPrefix(s, "+") {
		s = s[1:]
	}

	if s == "" {
		return 0, false
	}

	intPart, frac := s, ""

	if i := strings.IndexByte(s, '.'); i >= 0 {
		intPart, frac = s[:i], s[i+1:]
	}

	if intPart == "" {
		intPart = "0"
	}

	for _, c := range intPart + frac {
		if c < '0' || c > '9' {
			return 0, false
		}
	}

	frac = strings.TrimRight(frac, "0")

	if len(frac) > 2 {
		return 0, false
	}

	for len(frac) < 2 {
		frac += "0"
	}

	if len(intPart) > 13 {
		return 0, false
	}

	n, err := strconv.ParseInt(intPart+frac, 10, 64)

	if err != nil {
		return 0, false
	}

	if neg {
		n = -n
	}

	return n, true
}

// ingAlign pairs rows with refs one-to-one on (date, signed amount), preferring an equal normalised
// description inside a tie group and document order otherwise. It returns, for each row index, the
// matched ref index, and false unless EVERY row got exactly one ref and every ref was used.
func ingAlign(rows []*ingRow, refs []ingRef, requireAllRefs bool) ([]int, bool) {
	if len(rows) == 0 || len(refs) == 0 {
		return nil, false
	}

	if requireAllRefs && len(rows) != len(refs) {
		return nil, false
	}

	type group struct{ idx []int }
	groups := map[string]*group{}

	for i, r := range refs {
		k := r.Date + "\x1f" + strconv.FormatInt(r.Amount, 10)
		g := groups[k]

		if g == nil {
			g = &group{}
			groups[k] = g
		}

		g.idx = append(g.idx, i)
	}

	order := make([]int, len(rows))

	for i := range order {
		order[i] = i
	}

	sort.SliceStable(order, func(a, b int) bool {
		ra, rb := rows[order[a]], rows[order[b]]

		if ra.Time != rb.Time {
			return ra.Time < rb.Time
		}

		return ra.SourceIndex < rb.SourceIndex
	})

	used := make([]bool, len(refs))
	match := make([]int, len(rows))

	for _, ri := range order {
		row := rows[ri]
		g := groups[row.Date+"\x1f"+strconv.FormatInt(row.Amount, 10)]

		if g == nil {
			return nil, false
		}

		pick := -1

		for _, ci := range g.idx {
			if !used[ci] && refs[ci].NormDesc != "" && refs[ci].NormDesc == row.NormDesc {
				pick = ci
				break
			}
		}

		if pick < 0 {
			for _, ci := range g.idx {
				if !used[ci] {
					pick = ci
					break
				}
			}
		}

		if pick < 0 {
			return nil, false
		}

		used[pick] = true
		match[ri] = pick
	}

	if requireAllRefs {
		for _, u := range used {
			if !u {
				return nil, false
			}
		}
	}

	return match, true
}

// ingAlignBankIds attaches bank ids to rows; it reports whether it did. Duplicate ids inside one
// file disqualify the file (an id that is not unique is not an id).
func ingAlignBankIds(rows []*ingRow, refs []ingRef) bool {
	if len(refs) == 0 {
		return false
	}

	seen := map[string]bool{}

	for _, r := range refs {
		if r.Id == "" || seen[r.Id] {
			return false
		}

		seen[r.Id] = true
	}

	match, ok := ingAlign(rows, refs, true)

	if !ok {
		return false
	}

	for i, row := range rows {
		ref := refs[match[i]]
		row.BankId = ref.Id
		row.BankIdKind = ref.Kind
	}

	return true
}

// ingReadAll reads a file inside the root; errors are reported per file, never fatal to a plan
func ingReadFileBytes(real string, maxBytes int64) ([]byte, error) {
	data, err := ingReadLimited(real, maxBytes)

	if err != nil {
		return nil, err
	}

	return bytes.TrimPrefix(data, []byte("\xef\xbb\xbf")), nil
}

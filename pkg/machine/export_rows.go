package machine

import (
	"bytes"
	"encoding/csv"
	"strconv"
	"strings"

	"github.com/mayswind/ezbookkeeping/pkg/models"
)

// export_rows.go — GET /transactions/export?with_ids=true (apis.mdx §10.3.1a).
//
// Upstream's CSV/TSV export has no id column, so a file pulled out of the app cannot be written back
// to the same rows. With with_ids the plane renders its own file from the same filtered walk that
// GET /transactions uses: one row per API transaction (a transfer once), the database primary key
// first, and the category as the "Type > Group > Sub" path every write tool accepts. A categoriser
// fills the empty New Category column and sends the file back through POST /transactions/categorize
// (the MCP's ezb_set_transaction_categories_csv does exactly that).

// exportRowsCap is the most rows one id-carrying export may hold; narrow with start/end past it
const exportRowsCap = 50000

// exportRowsHeader is the column contract; the first column is always ID
var exportRowsHeader = []string{"ID", "Date", "Type", "Category Path", "Category", "Sub Category", "Account", "Currency", "Amount", "Amount Hundredths", "Counter Account", "Counter Amount", "Tags", "Description", "New Category", "New Counter Account", "Rule"}

// formatHundredths renders integer hundredths as a plain decimal ("-1234" -> "-12.34")
func formatHundredths(v int64) string {
	neg := v < 0

	if neg {
		v = -v
	}

	s := strconv.FormatInt(v/100, 10) + "." + strconv.FormatInt(100+v%100, 10)[1:]

	if neg {
		return "-" + s
	}

	return s
}

// exportRowsRender renders the walked rows; money OUT of the row's account is negative
func exportRowsRender(rows []map[string]any, lk *txnLookup, sep rune) ([]byte, error) {
	var buf bytes.Buffer
	w := csv.NewWriter(&buf)
	w.Comma = sep

	if err := w.Write(exportRowsHeader); err != nil {
		return nil, err
	}

	for _, r := range rows {
		t, _ := txnAsInt64(r["type"])
		src, _ := txnAsInt64(r["sourceAccountId"])
		cat, _ := txnAsInt64(r["categoryId"])
		amt, _ := txnAsInt64(r["sourceAmount"])

		if models.TransactionType(t) == models.TRANSACTION_TYPE_EXPENSE || models.TransactionType(t) == models.TRANSACTION_TYPE_TRANSFER {
			amt = -amt
		}

		group, sub := "", ""

		if c := lk.catMap[cat]; c != nil {
			sub = c.Name

			if p := lk.catMap[c.ParentCategoryId]; p != nil {
				group = p.Name
			}
		}

		counter, counterAmt := "", ""

		if models.TransactionType(t) == models.TRANSACTION_TYPE_TRANSFER {
			dst, _ := txnAsInt64(r["destinationAccountId"])
			counter = lk.accountName(dst)

			if d, ok := txnAsInt64(r["destinationAmount"]); ok {
				counterAmt = formatHundredths(d)
			}
		}

		var tagIds []string

		if list, ok := r["tagIds"].([]any); ok {
			for _, v := range list {
				tagIds = append(tagIds, txnAsString(v))
			}
		}

		rec := []string{
			txnAsString(r["id"]),
			txnAsString(r["date"]),
			txnTypeName(t),
			lk.categoryFullPath(cat),
			group,
			sub,
			lk.accountName(src),
			lk.accountCurrency(src),
			formatHundredths(amt),
			strconv.FormatInt(amt, 10),
			counter,
			counterAmt,
			strings.Join(lk.tagNames(tagIds), ";"),
			txnAsString(r["comment"]),
			"", "", "",
		}

		if err := w.Write(rec); err != nil {
			return nil, err
		}
	}

	w.Flush()

	return buf.Bytes(), w.Error()
}

// txnExportWithIds is the with_ids branch of txnHandleExport
func txnExportWithIds(mc *Ctx, lk *txnLookup, rf *txnResolvedFilter, format string) (any, error) {
	rows, next, err := txnWalk(mc, rf, exportRowsCap, 0, false, false)

	if err != nil {
		return nil, err
	}

	if next > 0 {
		return nil, Invalid("narrow the export with start/end", "more than %d transactions match; an id-carrying export holds at most %d", exportRowsCap, exportRowsCap)
	}

	for _, r := range rows {
		txnDecorate(r, lk, mc.Loc)
	}

	sep, contentType, ext := ',', "text/csv; charset=utf-8", "csv"

	if format == "tsv" {
		sep, contentType, ext = '\t', "text/tab-separated-values; charset=utf-8", "tsv"
	}

	data, err := exportRowsRender(rows, lk, sep)

	if err != nil {
		return nil, err
	}

	return &RawResult{ContentType: contentType, FileName: "ezbookkeeping_transactions_with_ids." + ext, Data: data}, nil
}

package machine

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"
	"unicode"
)

// ingest_identity.go — the import_id (apis.mdx §14.5, layer two).
//
//   - a bank's own id (OFX/QFX FITID, CAMT AcctSvcrRef/NtryRef) is THE id, prefixed by its account
//     key — we never mint over a bank's id;
//   - otherwise sha256(account_key ‖ date ‖ amount_hundredths ‖ currency ‖ normalised_description ‖
//     ordinal)[:32], where the ordinal is the index inside the tie group of the MERGED ACCOUNT-MONTH,
//     so two genuine same-day $5.00 coffees import as two rows and a cycle-dated card feeding one
//     calendar month from two statements does not collapse two real transactions into one.
//
// Category, tags and comment are deliberately NOT in the hash: the operator edits those after
// import, and an id that changed when they did would re-import every edited row.

const ingMaxImportIdLen = 255

// ingNormDesc normalises a statement description for identity: lower case, letters and digits
// only, single spaces
func ingNormDesc(s string) string {
	var b strings.Builder
	space := false

	for _, r := range strings.ToLower(s) {
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			if space && b.Len() > 0 {
				b.WriteByte(' ')
			}

			space = false
			b.WriteRune(r)
		default:
			space = true
		}
	}

	return b.String()
}

// ingMintId is the deterministic id of a row without a bank id
func ingMintId(accountKey, date string, amount int64, currency, normDesc string, ordinal int) string {
	h := sha256.New()

	for i, part := range []string{accountKey, date, strconv.FormatInt(amount, 10), currency, normDesc, strconv.Itoa(ordinal)} {
		if i > 0 {
			_, _ = h.Write([]byte{0x1f})
		}

		_, _ = h.Write([]byte(part))
	}

	return hex.EncodeToString(h.Sum(nil))[:32]
}

// ingBankImportId is the id of a row that carries the bank's own id
func ingBankImportId(accountKey, kind, bankId string) string {
	id := accountKey + "|" + kind + ":" + bankId

	if len(id) > ingMaxImportIdLen {
		sum := sha256.Sum256([]byte(id))
		return "bk:" + hex.EncodeToString(sum[:])
	}

	return id
}

// ingAssignIds gives every row its import_id. Rows are grouped by account and calendar month (the
// merged account-month, across every primary statement), ordered deterministically, and minted
// rows get their ordinal inside their (date, amount, currency, description) tie group.
func ingAssignIds(rows []*ingRow) {
	byMonth := map[string][]*ingRow{}

	for _, r := range rows {
		if r.BankId != "" {
			r.ImportId = ingBankImportId(r.AccountKey, r.BankIdKind, r.BankId)
			r.IdSource = "bank"
			r.Ordinal = 0
			continue
		}

		k := r.AccountKey + "\x1f" + r.Month
		byMonth[k] = append(byMonth[k], r)
	}

	for _, group := range byMonth {
		sort.SliceStable(group, func(i, j int) bool {
			a, b := group[i], group[j]

			if a.Date != b.Date {
				return a.Date < b.Date
			}

			if a.Amount != b.Amount {
				return a.Amount < b.Amount
			}

			if a.Currency != b.Currency {
				return a.Currency < b.Currency
			}

			if a.NormDesc != b.NormDesc {
				return a.NormDesc < b.NormDesc
			}

			if a.SourceFile != b.SourceFile {
				return a.SourceFile < b.SourceFile
			}

			if a.SourceLine != b.SourceLine {
				return a.SourceLine < b.SourceLine
			}

			return a.SourceIndex < b.SourceIndex
		})

		ordinal := 0

		for i, r := range group {
			if i > 0 && group[i-1].Key() == r.Key() {
				ordinal++
			} else {
				ordinal = 0
			}

			r.Ordinal = ordinal
			r.ImportId = ingMintId(r.AccountKey, r.Date, r.Amount, r.Currency, r.NormDesc, ordinal)
			r.IdSource = "minted"
		}
	}
}

// ingSortRows orders rows for output: account, date, amount, import id
func ingSortRows(rows []*ingRow) {
	sort.SliceStable(rows, func(i, j int) bool {
		a, b := rows[i], rows[j]

		if a.AccountKey != b.AccountKey {
			return a.AccountKey < b.AccountKey
		}

		if a.Date != b.Date {
			return a.Date < b.Date
		}

		if a.Time != b.Time {
			return a.Time < b.Time
		}

		if a.Amount != b.Amount {
			return a.Amount < b.Amount
		}

		return a.ImportId < b.ImportId
	})
}

// ingReadLimited reads a whole file, refusing one larger than maxBytes (0 means 64 MiB)
func ingReadLimited(real string, maxBytes int64) ([]byte, error) {
	if maxBytes <= 0 {
		maxBytes = 64 << 20
	}

	f, err := os.Open(real)

	if err != nil {
		return nil, err
	}

	defer f.Close()

	data, err := io.ReadAll(io.LimitReader(f, maxBytes+1))

	if err != nil {
		return nil, err
	}

	if int64(len(data)) > maxBytes {
		return nil, ingErr("file is larger than the import limit")
	}

	return data, nil
}

// ingSha256 is the hex sha256 of bytes
func ingSha256(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

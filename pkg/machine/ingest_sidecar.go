package machine

import (
	"encoding/csv"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// ingest_sidecar.go — the raw-mode line parser for statement text sidecars (_claude.txt,
// _brew.txt, _ocr.txt). It is CONSERVATIVE by design: a line becomes a row only when its date,
// amount and direction are all stated. A line that starts with a date but cannot be read that way
// is reported (with its line number and reason), never completed by a guess — no invented date,
// no invented sign, no invented amount (apis.mdx §14.9).
//
// Two shapes are read:
//   - delimited text with a header naming a date column and an amount (or debit/credit) column,
//     which is what an LLM extraction usually emits;
//   - statement-like text lines "DATE  DESCRIPTION  AMOUNT [BALANCE]", where the direction comes
//     from an explicit sign (-, parentheses, trailing -, CR/DR) or from the section the line sits
//     in ("Deposits and additions", "Withdrawals", "Purchases", "Payments and credits", …).

// ingRawRow is one row read from a sidecar
type ingRawRow struct {
	Date        string
	Amount      int64
	Description string
	Line        int
}

// ingRawIssue is one line the parser would not turn into a row
type ingRawIssue struct {
	Line   int    `json:"line"`
	Reason string `json:"reason"`
	Text   string `json:"text,omitempty"`
}

// ingSidecarResult is what one sidecar yielded
type ingSidecarResult struct {
	Rows         []ingRawRow
	Issues       []ingRawIssue
	PeriodFirst  string
	PeriodLast   string
	Shape        string
	CurrencySeen string
}

var (
	ingMonthNames = map[string]int{
		"jan": 1, "january": 1, "feb": 2, "february": 2, "mar": 3, "march": 3, "apr": 4, "april": 4,
		"may": 5, "jun": 6, "june": 6, "jul": 7, "july": 7, "aug": 8, "august": 8, "sep": 9, "sept": 9,
		"september": 9, "oct": 10, "october": 10, "nov": 11, "november": 11, "dec": 12, "december": 12,
	}

	// a date at the very start of a line
	ingLeadDate = regexp.MustCompile(`^(\d{4}-\d{2}-\d{2}|\d{1,2}/\d{1,2}/\d{2,4}|\d{1,2}-\d{1,2}-\d{2,4}|\d{1,2}/\d{1,2}|\d{1,2}-\d{1,2}|[A-Za-z]{3,9}\.? \d{1,2}(?:,? \d{4})?|\d{1,2} [A-Za-z]{3,9}(?: \d{4})?)(?:\s+|$)`)

	// an amount token: 1,234.56  -1234.56  (1,234.56)  1,234.56-  $1,234.56  1,234.56 CR
	ingAmountToken = regexp.MustCompile(`^\(?[-+]?\$?(?:\d{1,3}(?:,\d{3})+|\d+)\.\d{2}\)?-?$`)

	ingPeriodRe = regexp.MustCompile(`(?i)(?:statement period|period|for the period|billing (?:cycle|period)|statement dates?|opening/closing date|from)\s*:?\s*(\d{1,2}/\d{1,2}/\d{2,4}|\d{4}-\d{2}-\d{2}|[A-Za-z]{3,9}\.? \d{1,2},? \d{4})\s*(?:-|–|—|to|through|thru)\s*(\d{1,2}/\d{1,2}/\d{2,4}|\d{4}-\d{2}-\d{2}|[A-Za-z]{3,9}\.? \d{1,2},? \d{4})`)
)

var ingInflowSections = []string{"deposits", "credits", "payments and credits", "payments & credits", "additions", "refunds", "incoming", "money in", "interest paid", "dividends"}
var ingOutflowSections = []string{"withdrawals", "debits", "checks paid", "checks", "purchases", "fees", "charges", "subtractions", "atm", "card transactions", "electronic withdrawals", "interest charged", "cash advances", "money out", "outgoing", "payments made"}
var ingNotRows = []string{"beginning balance", "ending balance", "balance forward", "previous balance", "new balance", "opening balance", "closing balance", "total ", "subtotal", "daily balance"}

// ingParseSidecar reads one sidecar. hintDate is the statement's closing date (YYYY-MM-DD) when
// the file name carried one; hintYear the year directory; both only help place a date written
// without a year, and a date that still cannot be placed is reported.
func ingParseSidecar(text, hintDate, hintYear, order string) *ingSidecarResult {
	res := &ingSidecarResult{}

	if order != "dmy" && order != "ymd" {
		order = "mdy"
	}
	text = strings.ReplaceAll(text, "\r\n", "\n")
	text = strings.ReplaceAll(text, "\r", "\n")
	lines := strings.Split(text, "\n")

	// the statement's own period, from inside the document
	if m := ingPeriodRe.FindStringSubmatch(text); m != nil {
		a, okA := ingParseFullDate(m[1], order)
		b, okB := ingParseFullDate(m[2], order)

		if okA && okB && a <= b {
			res.PeriodFirst, res.PeriodLast = a, b
		}
	}

	if ingLooksDelimited(lines) {
		res.Shape = "delimited"
		ingParseDelimited(lines, res, hintDate, hintYear, order)
	} else {
		res.Shape = "lines"
		ingParseLines(lines, res, hintDate, hintYear, order)
	}

	return res
}

// ingParseFullDate parses a date that carries its own year. Numeric day/month order is the
// caller's stated order (mdy by default, dmy when the statements are European), never inferred.
func ingParseFullDate(s, order string) (string, bool) {
	s = strings.TrimSpace(strings.ReplaceAll(s, ",", ""))
	numeric := []string{"1/2/2006", "01/02/2006", "1/2/06", "01/02/06", "1-2-2006", "01-02-2006"}

	if order == "dmy" {
		numeric = []string{"2/1/2006", "02/01/2006", "2/1/06", "02/01/06", "2-1-2006", "02-01-2006"}
	}

	layouts := append([]string{"2006-01-02"}, numeric...)
	layouts = append(layouts, "Jan 2 2006", "January 2 2006", "Jan. 2 2006", "2 Jan 2006", "2 January 2006")

	for _, layout := range layouts {
		if t, err := time.Parse(layout, s); err == nil {
			return t.Format("2006-01-02"), true
		}
	}

	return "", false
}

// ingPlaceDate turns a date token into YYYY-MM-DD, using the period (or hints) only to supply a
// missing year. It never picks between two plausible years.
func ingPlaceDate(tok string, periodFirst, periodLast, hintDate, hintYear, order string) (string, string) {
	tok = strings.TrimSpace(strings.TrimSuffix(tok, "."))

	if d, ok := ingParseFullDate(tok, order); ok {
		return d, ""
	}

	month, day := 0, 0
	parts := strings.FieldsFunc(tok, func(r rune) bool { return r == '/' || r == '-' || r == ' ' || r == '.' })

	if len(parts) == 2 {
		if a, err := strconv.Atoi(parts[0]); err == nil {
			if b, err := strconv.Atoi(parts[1]); err == nil {
				if order == "dmy" {
					month, day = b, a
				} else {
					month, day = a, b
				}
			} else if m, ok := ingMonthNames[strings.ToLower(parts[1])]; ok {
				month, day = m, a
			}
		} else if m, ok := ingMonthNames[strings.ToLower(parts[0])]; ok {
			if b, err := strconv.Atoi(parts[1]); err == nil {
				month, day = m, b
			}
		}
	}

	if month < 1 || month > 12 || day < 1 || day > 31 {
		return "", "unreadable_date"
	}

	var candidates []string

	try := func(year int) {
		t := time.Date(year, time.Month(month), day, 0, 0, 0, 0, time.UTC)

		if t.Month() != time.Month(month) {
			return
		}

		d := t.Format("2006-01-02")

		for _, c := range candidates {
			if c == d {
				return
			}
		}

		candidates = append(candidates, d)
	}

	switch {
	case periodFirst != "" && periodLast != "":
		y1, _ := strconv.Atoi(periodFirst[:4])
		y2, _ := strconv.Atoi(periodLast[:4])

		for y := y1; y <= y2; y++ {
			try(y)
		}

		// keep only the candidates inside the period (with a week of slack for posting lag)
		lo, _ := time.Parse("2006-01-02", periodFirst)
		hi, _ := time.Parse("2006-01-02", periodLast)
		var inside []string

		for _, c := range candidates {
			t, _ := time.Parse("2006-01-02", c)

			if !t.Before(lo.AddDate(0, 0, -7)) && !t.After(hi.AddDate(0, 0, 7)) {
				inside = append(inside, c)
			}
		}

		candidates = inside
	case hintDate != "":
		closing, _ := time.Parse("2006-01-02", hintDate)
		y := closing.Year()

		// a statement closing in January lists December's rows: the row's month after the
		// closing month belongs to the previous year
		if month > int(closing.Month()) {
			y--
		}

		try(y)
	case hintYear != "":
		y, _ := strconv.Atoi(hintYear)
		try(y)
	}

	if len(candidates) != 1 {
		return "", "year_unknown"
	}

	return candidates[0], ""
}

// ingAmount parses one amount token into signed hundredths. explicit reports whether the token
// itself carried a direction.
func ingAmount(tok string) (amount int64, explicit bool, ok bool) {
	t := strings.TrimSpace(tok)
	neg := false

	upper := strings.ToUpper(t)

	switch {
	case strings.HasSuffix(upper, " CR") || strings.HasSuffix(upper, "CR"):
		t = strings.TrimSpace(t[:len(t)-2])
		explicit = true
	case strings.HasSuffix(upper, " DR") || strings.HasSuffix(upper, "DR"):
		t = strings.TrimSpace(t[:len(t)-2])
		neg, explicit = true, true
	}

	if strings.HasPrefix(t, "(") && strings.HasSuffix(t, ")") {
		t = t[1 : len(t)-1]
		neg, explicit = true, true
	}

	if strings.HasSuffix(t, "-") {
		t = t[:len(t)-1]
		neg, explicit = true, true
	}

	if strings.HasPrefix(t, "-") {
		t = t[1:]
		neg, explicit = true, true
	} else if strings.HasPrefix(t, "+") {
		t = t[1:]
		explicit = true
	}

	t = strings.TrimPrefix(t, "$")
	t = strings.ReplaceAll(t, ",", "")

	n, good := ingDecimalHundredths(t)

	if !good || n < 0 {
		return 0, false, false
	}

	if neg {
		n = -n
	}

	return n, explicit, true
}

// ingSectionSign reads a section header line: +1 inflow, -1 outflow, 0 not a header
func ingSectionSign(line string) int {
	l := strings.ToLower(strings.TrimSpace(line))

	if len(l) == 0 || len(l) > 60 {
		return 0
	}

	for _, k := range ingOutflowSections {
		if strings.Contains(l, k) {
			return -1
		}
	}

	for _, k := range ingInflowSections {
		if strings.Contains(l, k) {
			return 1
		}
	}

	return 0
}

func ingIsNotRow(line string) bool {
	l := strings.ToLower(line)

	for _, k := range ingNotRows {
		if strings.Contains(l, k) {
			return true
		}
	}

	return false
}

func ingParseLines(lines []string, res *ingSidecarResult, hintDate, hintYear, order string) {
	section := 0
	balanceColumn := false

	for i, raw := range lines {
		lineNo := i + 1
		line := strings.Join(strings.Fields(raw), " ")

		if line == "" {
			continue
		}

		lower := strings.ToLower(line)

		m := ingLeadDateMatch(line)

		if strings.Contains(lower, "balance") && strings.Contains(lower, "amount") && m == nil {
			balanceColumn = true
		}

		if m == nil {
			if s := ingSectionSign(line); s != 0 && !ingHasAmount(line) {
				section = s
			}

			continue
		}

		if ingIsNotRow(line) {
			continue
		}

		rest := strings.TrimSpace(line[len(m[0]):])
		tokens := strings.Fields(rest)

		// peel amounts off the end (with an optional CR/DR marker)
		var amounts []string

		for len(tokens) > 0 {
			last := tokens[len(tokens)-1]
			up := strings.ToUpper(last)

			if (up == "CR" || up == "DR") && len(tokens) > 1 && ingAmountToken.MatchString(tokens[len(tokens)-2]) {
				amounts = append([]string{tokens[len(tokens)-2] + " " + up}, amounts...)
				tokens = tokens[:len(tokens)-2]
				continue
			}

			if ingAmountToken.MatchString(strings.TrimSuffix(strings.TrimSuffix(up, "CR"), "DR")) {
				amounts = append([]string{last}, amounts...)
				tokens = tokens[:len(tokens)-1]
				continue
			}

			break
		}

		desc := strings.TrimSpace(strings.Join(tokens, " "))

		if len(amounts) == 0 {
			res.Issues = append(res.Issues, ingRawIssue{Line: lineNo, Reason: "no_amount", Text: ingClip(line)})
			continue
		}

		var amtTok string

		switch {
		case len(amounts) == 1:
			amtTok = amounts[0]
		case len(amounts) == 2 && balanceColumn:
			amtTok = amounts[0]
		default:
			res.Issues = append(res.Issues, ingRawIssue{Line: lineNo, Reason: "ambiguous_amount_columns", Text: ingClip(line)})
			continue
		}

		amount, explicit, ok := ingAmount(amtTok)

		if !ok {
			res.Issues = append(res.Issues, ingRawIssue{Line: lineNo, Reason: "unreadable_amount", Text: ingClip(line)})
			continue
		}

		if !explicit {
			if section == 0 {
				res.Issues = append(res.Issues, ingRawIssue{Line: lineNo, Reason: "sign_unknown", Text: ingClip(line)})
				continue
			}

			if section < 0 {
				amount = -amount
			}
		}

		date, why := ingPlaceDate(m[1], res.PeriodFirst, res.PeriodLast, hintDate, hintYear, order)

		if why != "" {
			res.Issues = append(res.Issues, ingRawIssue{Line: lineNo, Reason: why, Text: ingClip(line)})
			continue
		}

		if desc == "" {
			res.Issues = append(res.Issues, ingRawIssue{Line: lineNo, Reason: "no_description", Text: ingClip(line)})
			continue
		}

		res.Rows = append(res.Rows, ingRawRow{Date: date, Amount: amount, Description: desc, Line: lineNo})
	}
}

// ingLeadDateMatch matches a leading date, rejecting word-number pairs that are not month names
// ("Deposits 3 items" is a section line, not "Deposits 3")
func ingLeadDateMatch(line string) []string {
	m := ingLeadDate.FindStringSubmatch(line)

	if m == nil {
		return nil
	}

	for _, w := range strings.Fields(strings.ReplaceAll(m[1], ",", " ")) {
		w = strings.TrimSuffix(w, ".")

		if w == "" || (w[0] >= '0' && w[0] <= '9') {
			continue
		}

		if _, ok := ingMonthNames[strings.ToLower(w)]; !ok {
			return nil
		}
	}

	return m
}

func ingHasAmount(line string) bool {
	for _, tok := range strings.Fields(line) {
		if ingAmountToken.MatchString(tok) {
			return true
		}
	}

	return false
}

// ---- delimited shape --------------------------------------------------------------------------

var ingDelims = []rune{',', '\t', '|', ';'}

func ingLooksDelimited(lines []string) bool {
	_, _, ok := ingFindHeader(lines)
	return ok
}

// ingFindHeader finds a header line naming a date column and an amount column
func ingFindHeader(lines []string) (int, rune, bool) {
	for i, l := range lines {
		if i > 40 {
			break
		}

		lower := strings.ToLower(l)

		if !strings.Contains(lower, "date") {
			continue
		}

		for _, d := range ingDelims {
			if !strings.ContainsRune(l, d) {
				continue
			}

			cols := ingSplitDelimited(l, d)
			hasAmount := false

			for _, c := range cols {
				c = strings.ToLower(strings.TrimSpace(c))

				if c == "amount" || c == "debit" || c == "credit" || c == "withdrawal" || c == "withdrawals" || c == "deposit" || c == "deposits" || strings.HasPrefix(c, "amount") {
					hasAmount = true
				}
			}

			if hasAmount && len(cols) >= 3 {
				return i, d, true
			}
		}
	}

	return 0, 0, false
}

func ingSplitDelimited(line string, d rune) []string {
	r := csv.NewReader(strings.NewReader(line))
	r.Comma = d
	r.LazyQuotes = true
	r.FieldsPerRecord = -1

	rec, err := r.Read()

	if err != nil {
		return strings.Split(line, string(d))
	}

	return rec
}

func ingParseDelimited(lines []string, res *ingSidecarResult, hintDate, hintYear, order string) {
	hi, d, _ := ingFindHeader(lines)
	header := ingSplitDelimited(lines[hi], d)
	col := map[string]int{}

	for i, h := range header {
		h = strings.ToLower(strings.TrimSpace(h))

		if _, ok := col[h]; !ok {
			col[h] = i
		}
	}

	find := func(names ...string) int {
		for _, n := range names {
			if i, ok := col[n]; ok {
				return i
			}
		}

		return -1
	}

	dateCol := find("date", "posting date", "posted date", "transaction date", "trans date", "post date")
	descCol := find("description", "payee", "merchant", "details", "memo", "name", "narrative")
	amtCol := find("amount", "amount (usd)", "amt")
	debitCol := find("debit", "debits", "withdrawal", "withdrawals", "money out")
	creditCol := find("credit", "credits", "deposit", "deposits", "money in")
	typeCol := find("type", "dr/cr", "debit/credit", "direction")
	ccyCol := find("currency", "ccy")

	if dateCol < 0 {
		for h, i := range col {
			if strings.Contains(h, "date") {
				dateCol = i
				break
			}
		}
	}

	for i := hi + 1; i < len(lines); i++ {
		lineNo := i + 1
		raw := strings.TrimSpace(lines[i])

		if raw == "" {
			continue
		}

		rec := ingSplitDelimited(raw, d)
		get := func(c int) string {
			if c < 0 || c >= len(rec) {
				return ""
			}

			return strings.TrimSpace(rec[c])
		}

		dateTok := get(dateCol)

		if dateTok == "" {
			continue
		}

		desc := get(descCol)

		if ingIsNotRow(desc) {
			continue
		}

		var amount int64
		var ok bool

		switch {
		case amtCol >= 0 && get(amtCol) != "":
			var explicit bool
			amount, explicit, ok = ingAmount(get(amtCol))

			if ok && !explicit {
				switch strings.ToLower(get(typeCol)) {
				case "debit", "dr", "withdrawal", "out", "expense", "charge", "purchase":
					amount = -amount
				case "credit", "cr", "deposit", "in", "income", "payment", "refund":
				}
			}
		case debitCol >= 0 && get(debitCol) != "":
			amount, _, ok = ingAmount(get(debitCol))

			if amount > 0 {
				amount = -amount
			}
		case creditCol >= 0 && get(creditCol) != "":
			amount, _, ok = ingAmount(get(creditCol))

			if amount < 0 {
				amount = -amount
			}
		}

		if !ok {
			res.Issues = append(res.Issues, ingRawIssue{Line: lineNo, Reason: "unreadable_amount", Text: ingClip(raw)})
			continue
		}

		date, why := ingPlaceDate(dateTok, res.PeriodFirst, res.PeriodLast, hintDate, hintYear, order)

		if why != "" {
			res.Issues = append(res.Issues, ingRawIssue{Line: lineNo, Reason: why, Text: ingClip(raw)})
			continue
		}

		if c := strings.ToUpper(get(ccyCol)); c != "" {
			if res.CurrencySeen == "" {
				res.CurrencySeen = c
			} else if res.CurrencySeen != c {
				res.CurrencySeen = "MIXED"
			}
		}

		res.Rows = append(res.Rows, ingRawRow{Date: date, Amount: amount, Description: desc, Line: lineNo})
	}
}

func ingClip(s string) string {
	if len(s) > 160 {
		return s[:160] + "…"
	}

	return s
}

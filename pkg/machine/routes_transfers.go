package machine

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/mayswind/ezbookkeeping/pkg/api"
	"github.com/mayswind/ezbookkeeping/pkg/errfile"
	"github.com/mayswind/ezbookkeeping/pkg/models"
)

// routes_transfers.go — own-account moves as transfers (apis.mdx §10.3.2).
//
// A statement import writes one income/expense row per statement line, per account. A move between
// two of the operator's own accounts therefore arrives twice — an EXPENSE in one account and an
// INCOME in the other — and both count in every income/expense chart. Upstream statistics already
// leave transfers out, so the fix is to turn such rows into ONE transfer:
//
//   POST /transactions/transfer-candidates  finds the pairs (read only): an expense in A and an
//                                           income in B, same currency and amount, a few days apart,
//                                           with a hint (the other account's last four digits, or a
//                                           transfer-ish word) — unambiguous pairs apart from the rest
//   POST /transactions/convert-to-transfer  converts: a pair {id, counter_id} becomes one transfer;
//                                           a single row {id, counter_account_*} becomes a transfer
//                                           against a named (typically hidden) counterpart account
//
// A conversion creates the transfer through upstream's create handler, deletes the originals
// through upstream's delete handler, and re-points the machine_import_record rows that named the
// originals at the transfer, so a later statement re-plan still reports those statement rows as
// already_present. Every account balance is unchanged; the plan asserts it per account.

func init() {
	registerRoutes(xferRoutes)
	RegisterInverse(xferOpUnconvert, xferExecUnconvert)
}

// xferOpUnconvert is the inverse op of a conversion: delete the transfer, re-create the originals
// (new ids) and re-point the import records at them (apis.mdx §9.6)
const xferOpUnconvert = "txn.unconvert"

// window_days bounds (transfer-candidates)
const (
	xferDefaultWindowDays = 4
	xferMaxWindowDays     = 31
	xferDefaultPairLimit  = 1000
)

// xferCommentJoiner joins the two comments of a pair when comment_mode is "both"
const xferCommentJoiner = " ⇄ "

// XferTransferVocabulary is the documented transfer-ish vocabulary a candidate hint accepts
// (apis.mdx §10.3.2). Matching ignores case and needs word boundaries.
var XferTransferVocabulary = []string{
	"DEPOSIT TRANSFER", "ONLINE PMT", "CREDIT CARD", "THANK YOU", "AUTO PAY", "AUTOPAY", "PAYMENT",
	"TRANSFER", "XFER", "EPAY", "CRD", "FROM CHK", "TO CHK", "FROM SAV", "TO SAV",
}

var xferVocabularyPattern = func() *regexp.Regexp {
	terms := make([]string, 0, len(XferTransferVocabulary))

	for _, t := range XferTransferVocabulary {
		terms = append(terms, strings.ReplaceAll(regexp.QuoteMeta(t), " ", `\s+`))
	}

	return regexp.MustCompile(`(?i)\b(` + strings.Join(terms, "|") + `)\b`)
}()

func xferRoutes() []RouteDef {
	untrusted := []string{"comment", "accountName", "sourceAccountName", "destinationAccountName", "categoryName"}

	return []RouteDef{
		{Method: "POST", Path: "/transactions/transfer-candidates", Tier: TierRead, Composed: true, Summary: "Find own-account moves recorded as an expense in one account and an income in another (same currency and amount, within window_days, with a last-four or transfer-word hint); unambiguous pairs apart from ambiguous groups. Writes nothing.", Handler: xferHandleCandidates, Untrusted: untrusted},
		{Method: "POST", Path: "/transactions/convert-to-transfer", Tier: TierWrite, DryRunnable: true, Composed: true, Summary: "Turn income/expense rows into transfers: a pair {id, counter_id} becomes one transfer, a single row {id, counter_account_id|counter_account_name} a transfer against that account; balances unchanged, import records re-pointed; dry run by default, undoable.", Handler: xferHandleConvert, Untrusted: untrusted, Features: []string{"undo"}},
	}
}

// ─── the last four digits and the hints ──────────────────────────────────────────────────────

var xferTrailingDigits = regexp.MustCompile(`(\d+)[\s)\]]*$`)

// xferLastFour parses an account's last four digits from its NAME: the trailing run of digits, when
// it is at least four long ("Checking ••4021" → "4021", "Card x4021", "…4021", "*4021", "Savings
// 99994021" → "4021"). A name without one returns "".
func xferLastFour(name string) string {
	m := xferTrailingDigits.FindStringSubmatch(strings.TrimSpace(name))

	if m == nil || len(m[1]) < 4 {
		return ""
	}

	return m[1][len(m[1])-4:]
}

// xferCommentHasDigits reports a comment carrying the four digits as their own number (not inside a
// longer run of digits), whatever prefix it uses: x4021, ...4021, *4021, ••4021, #4021
func xferCommentHasDigits(comment, four string) bool {
	if four == "" {
		return false
	}

	for i := 0; ; {
		j := strings.Index(comment[i:], four)

		if j < 0 {
			return false
		}

		at := i + j
		before := at == 0 || comment[at-1] < '0' || comment[at-1] > '9'
		after := at+len(four) >= len(comment) || comment[at+len(four)] < '0' || comment[at+len(four)] > '9'

		if before && after {
			return true
		}

		i = at + 1
	}
}

// xferVocabularyTerm returns the first vocabulary term a comment contains ("" when none), upper-cased
func xferVocabularyTerm(comment string) string {
	m := xferVocabularyPattern.FindStringSubmatch(comment)

	if m == nil {
		return ""
	}

	return strings.ToUpper(strings.Join(strings.Fields(m[1]), " "))
}

// xferHint is why a candidate looks like a transfer
type xferHint struct {
	Kind    string `json:"kind"`              // last_four | vocabulary
	Digits  string `json:"digits,omitempty"`  // last_four: the other account's last four
	Term    string `json:"term,omitempty"`    // vocabulary: the matched term
	Comment string `json:"comment"`           // which row's comment matched: expense | income
	Account string `json:"account,omitempty"` // last_four: the account whose digits matched
}

// ─── candidate detection (pure) ──────────────────────────────────────────────────────────────

// xferRow is one income or expense row, as candidate detection sees it
type xferRow struct {
	Id         int64
	Type       models.TransactionType
	AccountId  int64
	Amount     int64
	Currency   string
	Time       int64
	Date       string
	Comment    string
	CategoryId int64
}

func (r xferRow) summary(lk *txnLookup) map[string]any {
	return map[string]any{
		"id":           idString(r.Id),
		"date":         r.Date,
		"typeName":     txnTypeName(int64(r.Type)),
		"accountId":    idString(r.AccountId),
		"accountName":  lk.accountName(r.AccountId),
		"amount":       r.Amount,
		"currency":     r.Currency,
		"comment":      r.Comment,
		"categoryId":   idString(r.CategoryId),
		"categoryName": lk.categoryPath(r.CategoryId),
	}
}

// xferEdge is one candidate: an expense and an income that could be the two sides of one move
type xferEdge struct {
	Expense *xferRow
	Income  *xferRow
	DayGap  int
	Hints   []xferHint
}

// xferDayGap is the absolute number of calendar days between two YYYY-MM-DD dates
func xferDayGap(a, b string) int {
	ta, err1 := time.Parse("2006-01-02", a)
	tb, err2 := time.Parse("2006-01-02", b)

	if err1 != nil || err2 != nil {
		errfile.Expected("parsing candidate dates", fmt.Errorf("%v %v", err1, err2))
		return 1 << 30
	}

	d := int(ta.Sub(tb).Hours() / 24)

	if d < 0 {
		d = -d
	}

	return d
}

// xferHintsFor lists the hints of an expense/income pair
func xferHintsFor(lk *txnLookup, e, in *xferRow) []xferHint {
	var hints []xferHint

	if four := xferLastFour(lk.accountName(in.AccountId)); xferCommentHasDigits(e.Comment, four) {
		hints = append(hints, xferHint{Kind: "last_four", Digits: four, Comment: "expense", Account: lk.accountName(in.AccountId)})
	}

	if four := xferLastFour(lk.accountName(e.AccountId)); xferCommentHasDigits(in.Comment, four) {
		hints = append(hints, xferHint{Kind: "last_four", Digits: four, Comment: "income", Account: lk.accountName(e.AccountId)})
	}

	if t := xferVocabularyTerm(e.Comment); t != "" {
		hints = append(hints, xferHint{Kind: "vocabulary", Term: t, Comment: "expense"})
	}

	if t := xferVocabularyTerm(in.Comment); t != "" {
		hints = append(hints, xferHint{Kind: "vocabulary", Term: t, Comment: "income"})
	}

	return hints
}

// xferCandidateResult is what detection found
type xferCandidateResult struct {
	Pairs     []*xferEdge
	Ambiguous [][]*xferEdge
	Edges     int
}

// xferFindCandidates builds every candidate edge (an expense in A and an income in B≠A, same
// currency, same amount, at most windowDays apart, a hint when requireHint, and at least one side
// inside the range when inRange is set), then splits them: an edge whose expense and income have no
// other candidate is a PAIR; every other edge belongs to an AMBIGUOUS group (a connected component
// of the candidate graph). Output order is deterministic.
func xferFindCandidates(lk *txnLookup, expenses, incomes []*xferRow, windowDays int, requireHint bool, inRange func(date string) bool) *xferCandidateResult {
	type key struct {
		cur    string
		amount int64
	}

	byKey := map[key][]*xferRow{}

	for _, in := range incomes {
		byKey[key{in.Currency, in.Amount}] = append(byKey[key{in.Currency, in.Amount}], in)
	}

	var edges []*xferEdge

	for _, e := range expenses {
		for _, in := range byKey[key{e.Currency, e.Amount}] {
			if in.AccountId == e.AccountId || e.Amount == 0 {
				continue
			}

			if inRange != nil && !inRange(e.Date) && !inRange(in.Date) {
				continue
			}

			gap := xferDayGap(e.Date, in.Date)

			if gap > windowDays {
				continue
			}

			hints := xferHintsFor(lk, e, in)

			if requireHint && len(hints) == 0 {
				continue
			}

			if hints == nil {
				hints = []xferHint{}
			}

			edges = append(edges, &xferEdge{Expense: e, Income: in, DayGap: gap, Hints: hints})
		}
	}

	// connected components over row ids (expense and income ids never collide: they are rows)
	parent := map[int64]int64{}

	var find func(x int64) int64
	find = func(x int64) int64 {
		if p, ok := parent[x]; ok && p != x {
			r := find(p)
			parent[x] = r
			return r
		}

		parent[x] = x
		return x
	}

	for _, ed := range edges {
		a, b := find(ed.Expense.Id), find(ed.Income.Id)

		if a != b {
			parent[a] = b
		}
	}

	comps := map[int64][]*xferEdge{}

	for _, ed := range edges {
		r := find(ed.Expense.Id)
		comps[r] = append(comps[r], ed)
	}

	res := &xferCandidateResult{Edges: len(edges)}

	edgeLess := func(a, b *xferEdge) bool {
		if a.Expense.Date != b.Expense.Date {
			return a.Expense.Date < b.Expense.Date
		}

		if a.Expense.Id != b.Expense.Id {
			return a.Expense.Id < b.Expense.Id
		}

		if a.Income.Date != b.Income.Date {
			return a.Income.Date < b.Income.Date
		}

		return a.Income.Id < b.Income.Id
	}

	for _, list := range comps {
		sort.Slice(list, func(i, j int) bool { return edgeLess(list[i], list[j]) })

		if len(list) == 1 {
			res.Pairs = append(res.Pairs, list[0])
		} else {
			res.Ambiguous = append(res.Ambiguous, list)
		}
	}

	sort.Slice(res.Pairs, func(i, j int) bool { return edgeLess(res.Pairs[i], res.Pairs[j]) })
	sort.Slice(res.Ambiguous, func(i, j int) bool { return edgeLess(res.Ambiguous[i][0], res.Ambiguous[j][0]) })

	return res
}

// xferGroupView lays out one ambiguous group: its distinct expenses and incomes and every candidate
func xferGroupView(lk *txnLookup, group []*xferEdge) map[string]any {
	var expenses, incomes []map[string]any
	seenE, seenI := map[int64]bool{}, map[int64]bool{}
	cands := make([]map[string]any, 0, len(group))

	for _, ed := range group {
		if !seenE[ed.Expense.Id] {
			seenE[ed.Expense.Id] = true
			expenses = append(expenses, ed.Expense.summary(lk))
		}

		if !seenI[ed.Income.Id] {
			seenI[ed.Income.Id] = true
			incomes = append(incomes, ed.Income.summary(lk))
		}

		cands = append(cands, map[string]any{"expenseId": idString(ed.Expense.Id), "incomeId": idString(ed.Income.Id), "dayGap": ed.DayGap, "hints": ed.Hints})
	}

	return map[string]any{"expenses": expenses, "incomes": incomes, "candidates": cands}
}

// ─── POST /transactions/transfer-candidates ──────────────────────────────────────────────────

type xferCandidatesBody struct {
	AccountIds   []string    `json:"account_ids,omitempty"`
	AccountNames []string    `json:"account_names,omitempty"`
	Start        string      `json:"start,omitempty"`
	End          string      `json:"end,omitempty"`
	WindowDays   json.Number `json:"window_days,omitempty"`
	RequireHint  *bool       `json:"require_hint,omitempty"`
	Limit        json.Number `json:"limit,omitempty"`
}

// xferRowsFromWalk turns walked upstream rows into detection rows
func xferRowsFromWalk(rows []map[string]any, lk *txnLookup, loc *time.Location) []*xferRow {
	out := make([]*xferRow, 0, len(rows))

	for _, r := range rows {
		id, ok := txnAsInt64(r["id"])

		if !ok {
			continue
		}

		t, _ := txnAsInt64(r["type"])
		acct, _ := txnAsInt64(r["sourceAccountId"])
		amount, _ := txnAsInt64(r["sourceAmount"])
		sec, _ := txnAsInt64(r["time"])
		cat, _ := txnAsInt64(r["categoryId"])

		out = append(out, &xferRow{
			Id: id, Type: models.TransactionType(t), AccountId: acct, Amount: amount, Currency: lk.accountCurrency(acct),
			Time: sec, Date: DateOfUnix(sec, loc), Comment: txnAsString(r["comment"]), CategoryId: cat,
		})
	}

	return out
}

func xferParseIntArg(name string, v json.Number, def, min, max int64) (int64, error) {
	if v == "" {
		return def, nil
	}

	n, err := strconv.ParseInt(v.String(), 10, 64)

	if err != nil || n < min || n > max {
		errfile.Expected("parsing an integer argument of transfer-candidates", err)
		return 0, Invalid(fmt.Sprintf("%s is an integer from %d to %d", name, min, max), "%s %q is out of range", name, v.String())
	}

	return n, nil
}

func xferHandleCandidates(mc *Ctx) (any, error) {
	var body xferCandidatesBody

	if err := mc.BindBody(&body); err != nil {
		return nil, err
	}

	window, err := xferParseIntArg("window_days", body.WindowDays, xferDefaultWindowDays, 0, xferMaxWindowDays)

	if err != nil {
		return nil, err
	}

	limit, err := xferParseIntArg("limit", body.Limit, xferDefaultPairLimit, 1, int64(MaxLimit))

	if err != nil {
		return nil, err
	}

	requireHint := body.RequireHint == nil || *body.RequireHint

	lk, err := txnLoadLookup(mc)

	if err != nil {
		return nil, err
	}

	// the accounts: named ones (parents expand to their sub-accounts), else every visible leaf
	var accountIds []int64
	echo := map[string]any{"windowDays": window, "requireHint": requireHint}

	if len(body.AccountIds)+len(body.AccountNames) > 0 {
		rf, err := txnResolveFilter(mc, lk, &txnFilterArgs{AccountIds: body.AccountIds, AccountNames: body.AccountNames})

		if err != nil {
			return nil, err
		}

		accountIds = rf.AccountIds
	} else {
		for _, a := range lk.accounts {
			if a.Type != models.ACCOUNT_TYPE_MULTI_SUB_ACCOUNTS && !a.Hidden {
				accountIds = append(accountIds, a.AccountId)
			}
		}
	}

	echo["accountIds"] = txnInt64Strings(accountIds)

	// the range: rows inside it, and their counterparts up to window_days outside it
	var startDate, endDate string
	walk := &txnResolvedFilter{AccountIds: accountIds}

	if body.Start != "" {
		s, err := ParseDate("start", body.Start, mc.Loc)

		if err != nil {
			return nil, err
		}

		startDate = s.Format("2006-01-02")
		walk.StartUnix = s.AddDate(0, 0, -int(window)).Unix()
		echo["start"] = startDate
	}

	if body.End != "" {
		e, err := ParseDate("end", body.End, mc.Loc)

		if err != nil {
			return nil, err
		}

		endDate = e.Format("2006-01-02")
		walk.EndUnix = e.AddDate(0, 0, 1+int(window)).Add(-time.Second).Unix()
		echo["end"] = endDate
	}

	if startDate != "" && endDate != "" && endDate < startDate {
		return nil, Invalid("end must be on or after start", "end %s is before start %s", endDate, startDate)
	}

	echo["timezone"] = mc.Loc.String()

	out := map[string]any{"filter": echo, "pairs": []any{}, "ambiguous": []any{}, "scanTruncated": false}

	if len(accountIds) < 2 {
		out["counts"] = map[string]any{"pairs": 0, "ambiguousGroups": 0, "ambiguousCandidates": 0, "candidates": 0, "scannedExpenses": 0, "scannedIncomes": 0}
		return out, nil
	}

	scan := func(t models.TransactionType) ([]*xferRow, bool, error) {
		rf := *walk
		rf.Type = t
		rows, next, err := txnWalk(mc, &rf, MaxLimit, 0, false, true)

		if err != nil {
			return nil, false, err
		}

		return xferRowsFromWalk(rows, lk, mc.Loc), next > 0, nil
	}

	expenses, truncE, err := scan(models.TRANSACTION_TYPE_EXPENSE)

	if err != nil {
		return nil, err
	}

	incomes, truncI, err := scan(models.TRANSACTION_TYPE_INCOME)

	if err != nil {
		return nil, err
	}

	var inRange func(string) bool

	if startDate != "" || endDate != "" {
		inRange = func(d string) bool {
			return (startDate == "" || d >= startDate) && (endDate == "" || d <= endDate)
		}
	}

	res := xferFindCandidates(lk, expenses, incomes, int(window), requireHint, inRange)

	pairs := make([]map[string]any, 0, len(res.Pairs))

	for i, ed := range res.Pairs {
		if int64(i) >= limit {
			break
		}

		pairs = append(pairs, map[string]any{
			"expense": ed.Expense.summary(lk),
			"income":  ed.Income.summary(lk),
			"dayGap":  ed.DayGap,
			"hints":   ed.Hints,
			"item":    map[string]string{"id": idString(ed.Expense.Id), "counter_id": idString(ed.Income.Id)},
		})
	}

	groups := make([]map[string]any, 0, len(res.Ambiguous))
	ambiguousCandidates := 0

	for i, g := range res.Ambiguous {
		ambiguousCandidates += len(g)

		if int64(i) < limit {
			groups = append(groups, xferGroupView(lk, g))
		}
	}

	out["pairs"] = pairs
	out["ambiguous"] = groups
	out["counts"] = map[string]any{
		"pairs": len(res.Pairs), "ambiguousGroups": len(res.Ambiguous), "ambiguousCandidates": ambiguousCandidates,
		"candidates": res.Edges, "scannedExpenses": len(expenses), "scannedIncomes": len(incomes),
		"pairsReturned": len(pairs), "ambiguousGroupsReturned": len(groups),
	}

	if truncE || truncI {
		out["scanTruncated"] = true
		mc.Truncated(MaxLimit)
	} else if len(pairs) < len(res.Pairs) || len(groups) < len(res.Ambiguous) {
		mc.Truncated(int(limit))
	}

	return out, nil
}

// ─── POST /transactions/convert-to-transfer ──────────────────────────────────────────────────

// xferItemArg is one item of the request
type xferItemArg struct {
	Id                 string `json:"id"`
	CounterId          string `json:"counter_id,omitempty"`
	CounterAccountId   string `json:"counter_account_id,omitempty"`
	CounterAccountName string `json:"counter_account_name,omitempty"`
}

type xferConvertBody struct {
	WriteOpts
	Items                []xferItemArg `json:"items"`
	CommentMode          string        `json:"comment_mode,omitempty"`
	TransferCategoryId   string        `json:"transfer_category_id,omitempty"`
	TransferCategoryName string        `json:"transfer_category_name,omitempty"`
}

// xferPlanItem is one resolved conversion
type xferPlanItem struct {
	Index     int
	Kind      string     // pair | single
	Originals []txnState // a pair: the expense first
	Transfer  txnState   // Id empty until created
	Preview   map[string]any
}

// xferProblem is one item that cannot be converted
type xferProblem struct {
	Index   int    `json:"index"`
	Id      string `json:"id,omitempty"`
	Code    string `json:"code"`
	Problem string `json:"problem"`
}

// xferBalanceEffect is what a state does to each account's balance, in that account's own currency
func xferBalanceEffect(s txnState) map[string]int64 {
	out := map[string]int64{}

	switch models.TransactionType(s.Type) {
	case models.TRANSACTION_TYPE_EXPENSE:
		out[s.SourceAccountId] -= s.SourceAmount
	case models.TRANSACTION_TYPE_INCOME:
		out[s.SourceAccountId] += s.SourceAmount
	case models.TRANSACTION_TYPE_TRANSFER:
		out[s.SourceAccountId] -= s.SourceAmount
		out[s.DestinationAccountId] += s.DestinationAmount
	case models.TRANSACTION_TYPE_MODIFY_BALANCE:
		out[s.SourceAccountId] += s.SourceAmount
	}

	return out
}

// xferBalanceRows compares before and after per account; ok is false when any account would move.
// counterpart, when set, is the named counter account of a single conversion: taking the other side
// of the move is its whole purpose, so its row is marked and exempt from the check.
func xferBalanceRows(lk *txnLookup, before []txnState, after txnState, counterpart string) ([]map[string]any, bool) {
	b, a := map[string]int64{}, xferBalanceEffect(after)

	for _, s := range before {
		for k, v := range xferBalanceEffect(s) {
			b[k] += v
		}
	}

	keys := map[string]bool{}

	for k := range b {
		keys[k] = true
	}

	for k := range a {
		keys[k] = true
	}

	ids := make([]string, 0, len(keys))

	for k := range keys {
		ids = append(ids, k)
	}

	ids = txnSortedIds(ids)
	rows := make([]map[string]any, 0, len(ids))
	ok := true

	for _, k := range ids {
		id, _ := strconv.ParseInt(k, 10, 64)
		net := a[k] - b[k]

		row := map[string]any{"accountId": k, "accountName": lk.accountName(id), "currency": lk.accountCurrency(id), "before": b[k], "after": a[k], "netChange": net}

		if k == counterpart {
			row["counterpart"] = true
		} else if net != 0 {
			ok = false
		}

		rows = append(rows, row)
	}

	return rows, ok
}

// xferDefaultTransferCategory is the first visible transfer-type secondary category, in display order
func xferDefaultTransferCategory(lk *txnLookup) *models.TransactionCategory {
	var cands []*models.TransactionCategory

	for _, c := range lk.categories {
		if c.Type != models.CATEGORY_TYPE_TRANSFER || c.Hidden || c.ParentCategoryId == models.LevelOneTransactionCategoryParentId {
			continue
		}

		if p := lk.catMap[c.ParentCategoryId]; p == nil || p.Hidden {
			continue
		}

		cands = append(cands, c)
	}

	sort.SliceStable(cands, func(i, j int) bool {
		pi, pj := lk.catMap[cands[i].ParentCategoryId], lk.catMap[cands[j].ParentCategoryId]

		if pi.DisplayOrder != pj.DisplayOrder {
			return pi.DisplayOrder < pj.DisplayOrder
		}

		if pi.CategoryId != pj.CategoryId {
			return pi.CategoryId < pj.CategoryId
		}

		if cands[i].DisplayOrder != cands[j].DisplayOrder {
			return cands[i].DisplayOrder < cands[j].DisplayOrder
		}

		return cands[i].CategoryId < cands[j].CategoryId
	})

	if len(cands) == 0 {
		return nil
	}

	return cands[0]
}

// xferResolveTransferCategory resolves transfer_category_id / _name, or picks the default
func xferResolveTransferCategory(lk *txnLookup, id, name string) (*models.TransactionCategory, error) {
	if strings.TrimSpace(id) != "" || strings.TrimSpace(name) != "" {
		return lk.resolveCategory(id, name, models.TRANSACTION_TYPE_TRANSFER)
	}

	if c := xferDefaultTransferCategory(lk); c != nil {
		return c, nil
	}

	return nil, NotFound("create a transfer category first (POST /machine/v1/categories with type transfer, `ezbk categories add --type transfer`, or ezb_add_category), or pass transfer_category_id", "the bound user has no visible transfer-type sub-category; a transfer needs one")
}

// xferJoinComments builds the pair's comment
func xferJoinComments(expense, income, mode string) string {
	if mode == "out" || strings.TrimSpace(income) == "" || income == expense {
		if strings.TrimSpace(expense) == "" {
			return income
		}

		return expense
	}

	if strings.TrimSpace(expense) == "" {
		return income
	}

	return expense + xferCommentJoiner + income
}

// xferTruncateRunes shortens s to n runes (upstream's comment limit is 255)
func xferTruncateRunes(s string, n int) (string, bool) {
	r := []rune(s)

	if len(r) <= n {
		return s, false
	}

	return string(r[:n-1]) + "…", true
}

// xferBuildPair composes the transfer for an expense/income pair. It is pure.
func xferBuildPair(lk *txnLookup, a, b txnState, categoryId, mode string) (txnState, []txnState, []string, error) {
	ta, tb := models.TransactionType(a.Type), models.TransactionType(b.Type)
	flow := func(t models.TransactionType) bool {
		return t == models.TRANSACTION_TYPE_INCOME || t == models.TRANSACTION_TYPE_EXPENSE
	}

	if !flow(ta) || !flow(tb) {
		return txnState{}, nil, nil, Invalid("both rows of a pair are income or expense rows (a transfer or balance modification cannot be paired)", "a pair needs two income/expense rows, got %s and %s", txnTypeName(int64(ta)), txnTypeName(int64(tb)))
	}

	if ta == tb {
		return txnState{}, nil, nil, Invalid("pair one expense row with one income row", "both rows are %s", txnTypeName(int64(ta)))
	}

	exp, inc := a, b

	if ta == models.TRANSACTION_TYPE_INCOME {
		exp, inc = b, a
	}

	if exp.SourceAccountId == inc.SourceAccountId {
		return txnState{}, nil, nil, Invalid("the two rows of a pair must be in different accounts", "both rows are in account %q", lk.accountName(xferAtoi(exp.SourceAccountId)))
	}

	expCur, incCur := lk.accountCurrency(xferAtoi(exp.SourceAccountId)), lk.accountCurrency(xferAtoi(inc.SourceAccountId))

	if expCur == incCur && exp.SourceAmount != inc.SourceAmount {
		return txnState{}, nil, nil, Invalid("a same-currency move has one amount on both sides; these are not the two sides of one move", "the expense is %d and the income is %d hundredths of %s", exp.SourceAmount, inc.SourceAmount, expCur)
	}

	var warnings []string
	comment, cut := xferTruncateRunes(xferJoinComments(exp.Comment, inc.Comment, mode), 255)

	if cut {
		warnings = append(warnings, "the joined comment was cut to 255 characters")
	}

	tags := txnTagsAdd(exp.TagIds, inc.TagIds)

	if len(tags) > models.MaximumTagsCountOfTransaction {
		return txnState{}, nil, nil, Invalid(fmt.Sprintf("a transaction holds at most %d tags; remove some from one side first", models.MaximumTagsCountOfTransaction), "the two rows carry %d distinct tags together", len(tags))
	}

	t := txnState{
		Type:                 int(models.TRANSACTION_TYPE_TRANSFER),
		CategoryId:           categoryId,
		Time:                 exp.Time,
		UtcOffset:            exp.UtcOffset,
		SourceAccountId:      exp.SourceAccountId,
		DestinationAccountId: inc.SourceAccountId,
		SourceAmount:         exp.SourceAmount,
		DestinationAmount:    inc.SourceAmount,
		HideAmount:           exp.HideAmount || inc.HideAmount,
		Comment:              comment,
		TagIds:               tags,
		PictureIds:           []string{},
		GeoLatitude:          exp.GeoLatitude,
		GeoLongitude:         exp.GeoLongitude,
	}

	if t.GeoLatitude == "" && t.GeoLongitude == "" {
		t.GeoLatitude, t.GeoLongitude = inc.GeoLatitude, inc.GeoLongitude
	}

	return t, []txnState{exp, inc}, warnings, nil
}

// xferBuildSingle composes the transfer for one row against a counter account. It is pure.
func xferBuildSingle(lk *txnLookup, s txnState, counter *models.Account, categoryId string) (txnState, error) {
	ts := models.TransactionType(s.Type)

	if ts != models.TRANSACTION_TYPE_INCOME && ts != models.TRANSACTION_TYPE_EXPENSE {
		return txnState{}, Invalid("only an income or expense row becomes a transfer", "the row is a %s", txnTypeName(int64(ts)))
	}

	counterId := idString(counter.AccountId)

	if counterId == s.SourceAccountId {
		return txnState{}, Invalid("the counter account must be a different account", "the row is already in %q", counter.Name)
	}

	if cur := lk.accountCurrency(xferAtoi(s.SourceAccountId)); cur != counter.Currency {
		return txnState{}, Invalid("the counter account must share the row's currency ("+cur+"); a cross-currency single conversion would need an amount the plane never invents", "%q is %s and the row is %s", counter.Name, counter.Currency, cur)
	}

	t := s
	t.Id = ""
	t.Type = int(models.TRANSACTION_TYPE_TRANSFER)
	t.CategoryId = categoryId
	t.TagIds = append([]string{}, s.TagIds...)
	t.PictureIds = []string{}
	t.DestinationAmount = s.SourceAmount

	if ts == models.TRANSACTION_TYPE_EXPENSE {
		t.DestinationAccountId = counterId
	} else {
		// money came IN to the row's account: the counter account is the source
		t.DestinationAccountId = s.SourceAccountId
		t.SourceAccountId = counterId
	}

	return t, nil
}

func xferAtoi(s string) int64 {
	n, _ := strconv.ParseInt(s, 10, 64)
	return n
}

// xferRecordsByTxn returns import_id → transaction_id for records pointing at the given transactions
func xferRecordsByTxn(mc *Ctx, txnIds []int64) (map[string]int64, error) {
	out := map[string]int64{}

	if len(txnIds) == 0 {
		return out, nil
	}

	db, err := ingDB(mc)

	if err != nil {
		return nil, err
	}

	for start := 0; start < len(txnIds); start += ingInChunk {
		end := start + ingInChunk

		if end > len(txnIds) {
			end = len(txnIds)
		}

		var recs []*MachineImportRecord

		if err := db.NewSession(mc.Web).Cols("uid", "import_id", "transaction_id").Where("uid=?", mc.Uid).In("transaction_id", txnIds[start:end]).Find(&recs); err != nil {
			errfile.Caught("reading the import records of the rows being converted", err)
			return nil, NewFail(CodeUpstreamError, "check ~/T/ezbookkeeping/error.err; the machine_import_record table could not be read", "cannot read import records")
		}

		for _, r := range recs {
			out[r.ImportId] = r.TransactionId
		}
	}

	return out, nil
}

// xferRepoint re-points import records: import_id → {transaction it points at now, the new one}.
// A record that already moved elsewhere is left alone (ingRelinkRecords checks the current id).
func xferRepoint(mc *Ctx, moves map[string][2]int64) error {
	if err := ingRelinkRecords(mc, moves); err != nil {
		errfile.Caught("re-pointing the import records of converted rows", err)
		return NewFail(CodeUpstreamError, "check ~/T/ezbookkeeping/error.err; the machine_import_record table could not be updated", "cannot re-point import records")
	}

	return nil
}

// xferWithVisible runs fn with every hidden account among ids temporarily visible: upstream refuses
// to add a transaction to, or delete one from, a hidden account. The accounts are hidden again
// afterwards, whether fn failed or not.
func xferWithVisible(mc *Ctx, lk *txnLookup, ids []int64, fn func() error) (err error) {
	var unhidden []int64
	seen := map[int64]bool{}

	for _, id := range ids {
		a := lk.accountMap[id]

		if a == nil || !a.Hidden || seen[id] {
			continue
		}

		seen[id] = true

		if _, herr := mc.CallUpstream(api.Accounts.AccountHideHandler, "POST", nil, map[string]any{"id": idString(id), "hidden": false}); herr != nil {
			err = herr
			break
		}

		unhidden = append(unhidden, id)
	}

	defer func() {
		for _, id := range unhidden {
			if _, herr := mc.CallUpstream(api.Accounts.AccountHideHandler, "POST", nil, map[string]any{"id": idString(id), "hidden": true}); herr != nil {
				errfile.Caught("hiding an account again after a transfer conversion", herr)

				if err == nil {
					err = toFail(herr).WithDetails(map[string]any{"hint": "the conversion itself succeeded; hide account " + idString(id) + " again in the web UI"})
				}
			}
		}
	}()

	if err != nil {
		return err
	}

	return fn()
}

// xferAccountsOf lists the accounts a set of states touches
func xferAccountsOf(states ...txnState) []int64 {
	var out []int64

	for _, s := range states {
		out = append(out, xferAtoi(s.SourceAccountId))

		if s.DestinationAccountId != "" && s.DestinationAccountId != "0" {
			out = append(out, xferAtoi(s.DestinationAccountId))
		}
	}

	return out
}

// xferCreate creates one transaction from a state through upstream's create handler
func xferCreate(mc *Ctx, s txnState) (int64, error) {
	var resp map[string]any

	if err := mc.CallUpstreamInto(api.Transactions.TransactionCreateHandler, "POST", nil, txnCreateBodyFromState(s), &resp); err != nil {
		return 0, err
	}

	id, ok := txnAsInt64(resp["id"])

	if !ok || id <= 0 {
		return 0, NewFail(CodeUpstreamError, "check ~/T/ezbookkeeping/error.err", "upstream created a transaction but returned no id")
	}

	return id, nil
}

// xferDelete deletes one transaction through upstream's delete handler
func xferDelete(mc *Ctx, id int64) error {
	_, err := mc.CallUpstream(api.Transactions.TransactionDeleteHandler, "POST", nil, map[string]any{"id": idString(id)})
	return err
}

// xferJournalItem is one conversion as the journal records it
type xferJournalItem struct {
	// TransferId is the transfer the conversion created; Transfer its state right after
	TransferId string   `json:"transferId"`
	Transfer   txnState `json:"transfer"`
	// Originals are the rows the conversion deleted, as they were (old ids)
	Originals []txnState `json:"originals"`
	// Records maps each re-pointed import id to the original transaction id it named
	Records map[string]string `json:"records"`
}

// xferRestore re-creates originals (new ids), re-points the import records to them, and deletes
// the transfer (when transferId > 0). recordsNow maps import id → the transaction the record points
// at now; recordsOrigin maps import id → the original (old) id it came from. On a failure part way,
// the rows it already re-created are deleted again, so it is all or nothing as far as upstream allows.
// The caller makes hidden accounts visible first (xferWithVisible).
func xferRestore(mc *Ctx, transferId int64, originals []txnState, recordsOrigin map[string]string, recordsNow map[string]int64) (map[string]string, error) {
	newIds := map[string]string{}
	var created []int64

	run := func() error {
		for _, s := range originals {
			id, err := xferCreate(mc, s)

			if err != nil {
				for j := len(created) - 1; j >= 0; j-- {
					if derr := xferDelete(mc, created[j]); derr != nil {
						errfile.Caught("deleting a re-created original after a failed restore", derr)
					}
				}

				return err
			}

			created = append(created, id)
			newIds[s.Id] = idString(id)
		}

		moves := map[string][2]int64{}

		for imp, orig := range recordsOrigin {
			now, ok := recordsNow[imp]
			nid := newIds[orig]

			if !ok || nid == "" {
				continue
			}

			moves[imp] = [2]int64{now, xferAtoi(nid)}
		}

		if err := xferRepoint(mc, moves); err != nil {
			return err
		}

		if transferId > 0 {
			if err := xferDelete(mc, transferId); err != nil {
				// put the records back on the transfer so nothing points at a row about to vanish
				back := map[string][2]int64{}

				for imp, m := range moves {
					back[imp] = [2]int64{m[1], m[0]}
				}

				if rerr := xferRepoint(mc, back); rerr != nil {
					errfile.Caught("re-pointing import records back after a failed transfer delete", rerr)
				}

				for j := len(created) - 1; j >= 0; j-- {
					if derr := xferDelete(mc, created[j]); derr != nil {
						errfile.Caught("deleting a re-created original after a failed transfer delete", derr)
					}
				}

				return err
			}
		}

		return nil
	}

	if err := run(); err != nil {
		return nil, err
	}

	return newIds, nil
}

func xferHandleConvert(mc *Ctx) (any, error) {
	var body xferConvertBody

	if err := mc.BindBody(&body); err != nil {
		return nil, err
	}

	if len(body.Items) == 0 {
		return nil, Invalid("pass items: [{\"id\": …, \"counter_id\": …}] for a pair, or [{\"id\": …, \"counter_account_id\": …}] for one row; POST /transactions/transfer-candidates finds pairs", "no items were given")
	}

	mode := strings.ToLower(strings.TrimSpace(body.CommentMode))

	if mode == "" {
		mode = "both"
	}

	if mode != "both" && mode != "out" {
		return nil, Invalid("comment_mode is both (the default: expense comment ⇄ income comment) or out (the expense comment only)", "unknown comment_mode %q", body.CommentMode)
	}

	resolve := func() (*Plan, error) {
		lk, err := txnLoadLookup(mc)

		if err != nil {
			return nil, err
		}

		cat, err := xferResolveTransferCategory(lk, body.TransferCategoryId, body.TransferCategoryName)

		if err != nil {
			return nil, err
		}

		catId := idString(cat.CategoryId)

		// every id once across the whole request
		var problems []xferProblem
		var wanted []int64
		count := map[int64]int{}

		for i, it := range body.Items {
			if strings.TrimSpace(it.Id) == "" {
				return nil, Invalid("every item names the row to convert in id", "items[%d] has no id", i)
			}

			hasPair := strings.TrimSpace(it.CounterId) != ""
			hasAcct := strings.TrimSpace(it.CounterAccountId) != "" || strings.TrimSpace(it.CounterAccountName) != ""

			if hasPair == hasAcct {
				return nil, Invalid("give each item exactly one of counter_id (the other row of a pair) or counter_account_id / counter_account_name (the account on the other side)", "items[%d] has %s", i, map[bool]string{true: "both", false: "neither"}[hasPair])
			}

			ids := []string{it.Id}

			if hasPair {
				ids = append(ids, it.CounterId)
			}

			for _, s := range ids {
				id, err := ResolveId(fmt.Sprintf("items[%d]", i), s)

				if err != nil {
					return nil, err
				}

				if count[id] == 0 {
					wanted = append(wanted, id)
				}

				count[id]++
			}
		}

		var dups []string

		for _, id := range wanted {
			if count[id] > 1 {
				dups = append(dups, idString(id))
			}
		}

		if len(dups) > 0 {
			return nil, Invalid("name every transaction once in a request (a row can be converted only once)", "%d ids appear more than once", len(dups)).WithDetails(map[string]any{"duplicates": dups})
		}

		states, canon, missing, err := txnReadStates(mc, wanted)

		if err != nil {
			return nil, err
		}

		missingSet := map[int64]bool{}

		for _, id := range missing {
			missingSet[id] = true
		}

		// a TRANSFER_IN row id canonicalises to its out-row: it is already a transfer
		stateOf := func(id int64) (txnState, string, string) {
			if missingSet[id] {
				return txnState{}, CodeNotFound, "the transaction does not exist (deleted, or never was)"
			}

			s := *states[canon[id]]

			if models.TransactionType(s.Type) == models.TRANSACTION_TYPE_TRANSFER {
				return s, CodeInvalidInput, "the transaction is already a transfer"
			}

			if len(s.PictureIds) > 0 {
				return s, CodeInvalidInput, "the transaction has pictures attached; a conversion would not carry them — remove them first"
			}

			return s, "", ""
		}

		var items []*xferPlanItem
		var warnings []string
		hiddenSeen := map[int64]bool{}
		var hidden []map[string]any
		fp := []any{catId, mode}

		for i, it := range body.Items {
			id := xferAtoi(strings.TrimSpace(it.Id))
			a, code, msg := stateOf(id)

			if code != "" {
				problems = append(problems, xferProblem{Index: i, Id: idString(id), Code: code, Problem: msg})
				continue
			}

			item := &xferPlanItem{Index: i}

			if strings.TrimSpace(it.CounterId) != "" {
				cid := xferAtoi(strings.TrimSpace(it.CounterId))
				b, code, msg := stateOf(cid)

				if code != "" {
					problems = append(problems, xferProblem{Index: i, Id: idString(cid), Code: code, Problem: msg})
					continue
				}

				t, originals, w, err := xferBuildPair(lk, a, b, catId, mode)

				if err != nil {
					problems = append(problems, xferProblem{Index: i, Id: idString(id), Code: toFail(err).Code, Problem: toFail(err).Message})
					continue
				}

				for _, s := range w {
					warnings = append(warnings, fmt.Sprintf("items[%d]: %s", i, s))
				}

				item.Kind, item.Transfer, item.Originals = "pair", t, originals
			} else {
				counter, err := lk.resolveAccount(fmt.Sprintf("items[%d].counter_account_id", i), it.CounterAccountId, fmt.Sprintf("items[%d].counter_account_name", i), it.CounterAccountName, true)

				if err != nil {
					return nil, err
				}

				t, err := xferBuildSingle(lk, a, counter, catId)

				if err != nil {
					problems = append(problems, xferProblem{Index: i, Id: idString(id), Code: toFail(err).Code, Problem: toFail(err).Message})
					continue
				}

				item.Kind, item.Transfer, item.Originals = "single", t, []txnState{a}
			}

			counterpart := ""

			if item.Kind == "single" {
				counterpart = item.Transfer.DestinationAccountId

				if models.TransactionType(item.Originals[0].Type) == models.TRANSACTION_TYPE_INCOME {
					counterpart = item.Transfer.SourceAccountId
				}
			}

			balance, ok := xferBalanceRows(lk, item.Originals, item.Transfer, counterpart)

			if !ok {
				problems = append(problems, xferProblem{Index: i, Id: idString(id), Code: CodeInvalidInput, Problem: "the conversion would change an account balance"})
				continue
			}

			before := make([]map[string]any, 0, len(item.Originals))

			for _, s := range item.Originals {
				before = append(before, txnRowSummary(lk, s, mc.Loc))
			}

			after := txnRowSummary(lk, item.Transfer, mc.Loc)
			delete(after, "id")
			after["tagIds"] = txnNonNil(item.Transfer.TagIds)
			after["tagNames"] = lk.tagNames(item.Transfer.TagIds)

			item.Preview = map[string]any{"index": i, "kind": item.Kind, "before": before, "after": after, "balanceEffect": balance}

			if item.Kind == "pair" {
				gap := xferDayGap(DateOfUnix(item.Originals[0].Time, mc.Loc), DateOfUnix(item.Originals[1].Time, mc.Loc))
				item.Preview["dayGap"] = gap

				if gap > 0 {
					item.Preview["destinationDateShift"] = fmt.Sprintf("the %s side moves from %s to %s; its final balance is unchanged", lk.accountName(xferAtoi(item.Originals[1].SourceAccountId)), DateOfUnix(item.Originals[1].Time, mc.Loc), DateOfUnix(item.Originals[0].Time, mc.Loc))
				}
			}

			for _, acct := range xferAccountsOf(append([]txnState{item.Transfer}, item.Originals...)...) {
				if a := lk.accountMap[acct]; a != nil && a.Hidden && !hiddenSeen[acct] {
					hiddenSeen[acct] = true
					hidden = append(hidden, map[string]any{"accountId": idString(acct), "accountName": a.Name})
				}
			}

			fp = append(fp, item.Transfer.normalized())

			for _, s := range item.Originals {
				fp = append(fp, s.normalized())
			}

			items = append(items, item)
		}

		if len(problems) > 0 {
			code := CodeNotFound

			for _, p := range problems {
				if p.Code != CodeNotFound {
					code = CodeInvalidInput
				}
			}

			hint := "fix or drop the items listed in details.problems and preview again; POST /machine/v1/transactions/transfer-candidates finds valid pairs"
			msg := fmt.Sprintf("%d of %d items cannot be converted", len(problems), len(body.Items))

			return nil, NewFail(code, hint, "%s", msg).WithDetails(map[string]any{"problems": problems})
		}

		// the import records of every original, so the preview can say how many move
		var origIds []int64

		for _, it := range items {
			for _, s := range it.Originals {
				origIds = append(origIds, xferAtoi(s.Id))
			}
		}

		recs, err := xferRecordsByTxn(mc, origIds)

		if err != nil {
			return nil, err
		}

		recCount := map[int64]int{}

		for _, tid := range recs {
			recCount[tid]++
		}

		rows := make([]map[string]any, 0, len(items))
		originals := 0

		for _, it := range items {
			n := 0

			for _, s := range it.Originals {
				n += recCount[xferAtoi(s.Id)]
			}

			it.Preview["importRecords"] = n
			rows = append(rows, it.Preview)
			originals += len(it.Originals)
		}

		if len(hidden) > 0 {
			warnings = append(warnings, fmt.Sprintf("%d hidden accounts are made visible for the moment of each conversion and hidden again (upstream refuses transactions in a hidden account)", len(hidden)))
		}

		warnings = append(warnings, "the originals are deleted and the transfers are new transactions with new ids; ezb_undo / POST /undo reverses the whole conversion (the originals come back with new ids)")

		preview := map[string]any{
			"items":                rows,
			"transferCategoryId":   catId,
			"transferCategoryPath": lk.categoryFullPath(cat.CategoryId),
			"commentMode":          mode,
			"hiddenAccounts":       hidden,
		}

		if hidden == nil {
			preview["hiddenAccounts"] = []any{}
		}

		return &Plan{
			Changes:       map[string]int{"convert": len(items), "create": len(items), "delete": originals},
			Count:         originals,
			Preview:       preview,
			Fingerprinted: fp,
			Warnings:      warnings,
			State:         items,
		}, nil
	}

	apply := func(p *Plan) (any, error) {
		items := p.State.([]*xferPlanItem)

		lk, err := txnLoadLookup(mc)

		if err != nil {
			return nil, err
		}

		var journal []xferJournalItem
		results := make([]map[string]any, 0, len(items))
		var transferIds []string

		record := func() error {
			if len(journal) == 0 {
				return nil
			}

			rows := 0

			for _, j := range journal {
				rows += len(j.Originals)
			}

			summary := fmt.Sprintf("converted %d income/expense rows into %d transfers", rows, len(journal))

			_, err := RecordJournal(mc, summary, len(journal), []InverseOp{NewInverseOp(xferOpUnconvert, map[string]any{"items": journal}, nil)})

			return err
		}

		for _, it := range items {
			res, jitem, err := xferApplyItem(mc, lk, it)

			if jitem != nil {
				journal = append(journal, *jitem)
			}

			if err != nil {
				if jerr := record(); jerr != nil {
					errfile.Caught("journaling a partially applied transfer conversion", jerr)
				}

				f := toFail(err)
				f.Message = fmt.Sprintf("items[%d]: %s", it.Index, f.Message)

				return nil, f.WithDetails(map[string]any{"failedIndex": it.Index, "converted": len(journal), "transferIds": txnNonNil(transferIds), "hint": "items before failedIndex are converted and journaled (POST /undo reverses them); the failed item was rolled back"})
			}

			results = append(results, res)
			transferIds = append(transferIds, res["transferId"].(string))
		}

		if err := record(); err != nil {
			return nil, err
		}

		return map[string]any{"converted": len(results), "ids": txnNonNil(transferIds), "items": results}, nil
	}

	return RunWrite(mc, body.WriteOpts, resolve, apply)
}

// xferApplyItem converts one item: create the transfer, delete the originals, re-point the import
// records. A failure part way rolls the item back (the originals come back with new ids and their
// records follow them); the journal item is returned only when the conversion stands.
func xferApplyItem(mc *Ctx, lk *txnLookup, it *xferPlanItem) (map[string]any, *xferJournalItem, error) {
	var transferId int64
	var deleted []txnState
	var result map[string]any
	var jitem *xferJournalItem

	origIds := make([]int64, 0, len(it.Originals))

	for _, s := range it.Originals {
		origIds = append(origIds, xferAtoi(s.Id))
	}

	err := xferWithVisible(mc, lk, xferAccountsOf(append([]txnState{it.Transfer}, it.Originals...)...), func() error {
		recs, err := xferRecordsByTxn(mc, origIds)

		if err != nil {
			return err
		}

		if transferId, err = xferCreate(mc, it.Transfer); err != nil {
			return err
		}

		rollback := func(cause error) error {
			origin, now := map[string]string{}, map[string]int64{}

			for imp, tid := range recs {
				origin[imp] = idString(tid)
				now[imp] = tid
			}

			if _, rerr := xferRestore(mc, transferId, deleted, origin, now); rerr != nil {
				errfile.Caught("rolling back a failed transfer conversion", rerr)
				return toFail(cause).WithDetails(map[string]any{"rollbackFailed": toFail(rerr).Message, "transferId": idString(transferId), "deletedOriginals": len(deleted)})
			}

			return cause
		}

		for _, s := range it.Originals {
			if err := xferDelete(mc, xferAtoi(s.Id)); err != nil {
				return rollback(err)
			}

			deleted = append(deleted, s)
		}

		moves := map[string][2]int64{}
		records := map[string]string{}

		for imp, tid := range recs {
			moves[imp] = [2]int64{tid, transferId}
			records[imp] = idString(tid)
		}

		if err := xferRepoint(mc, moves); err != nil {
			return rollback(err)
		}

		// the conversion stands from here on; its journal item carries the transfer as upstream stored it
		st := it.Transfer
		st.Id = idString(transferId)

		if after, _, _, rerr := txnReadStates(mc, []int64{transferId}); rerr != nil {
			errfile.Caught("reading a transfer back after converting rows into it", rerr)
		} else if s := after[transferId]; s != nil {
			st = *s
		}

		jitem = &xferJournalItem{TransferId: idString(transferId), Transfer: st, Originals: it.Originals, Records: records}
		result = map[string]any{"index": it.Index, "kind": it.Kind, "transferId": idString(transferId), "deletedIds": txnInt64Strings(origIds), "recordsRepointed": len(moves)}

		return nil
	})

	// err with a journal item: the conversion stood and only re-hiding an account failed
	return result, jitem, err
}

// ─── the inverse executor (undo) ─────────────────────────────────────────────────────────────

// xferExecUnconvert reverses conversions: every item is checked first (the transfer unchanged since,
// the originals still gone) so a refusal changes nothing; then, newest item first, the originals
// are re-created (new ids), their import records re-pointed at them, and the transfer deleted.
func xferExecUnconvert(mc *Ctx, payload json.RawMessage, _ json.RawMessage) error {
	var p struct {
		Items []xferJournalItem `json:"items"`
	}

	if err := json.Unmarshal(payload, &p); err != nil {
		return err
	}

	transfers := make([]txnState, 0, len(p.Items))
	var origIds []int64

	for _, it := range p.Items {
		transfers = append(transfers, it.Transfer)

		for _, s := range it.Originals {
			origIds = append(origIds, xferAtoi(s.Id))
		}
	}

	if _, err := txnCheckRows(mc, transfers); err != nil {
		return err
	}

	current, _, _, err := txnReadStates(mc, origIds)

	if err != nil {
		return err
	}

	for _, id := range origIds {
		if current[id] != nil {
			return Conflict("the original row exists again; undo will not create a second copy", "transaction %s is not deleted", idString(id)).WithDetails(map[string]any{"id": idString(id)})
		}
	}

	lk, err := txnLoadLookup(mc)

	if err != nil {
		return err
	}

	for i := len(p.Items) - 1; i >= 0; i-- {
		it := p.Items[i]
		tid := xferAtoi(it.TransferId)
		now := map[string]int64{}

		for imp := range it.Records {
			now[imp] = tid
		}

		err := xferWithVisible(mc, lk, xferAccountsOf(append([]txnState{it.Transfer}, it.Originals...)...), func() error {
			_, err := xferRestore(mc, tid, it.Originals, it.Records, now)
			return err
		})

		if err != nil {
			f := toFail(err)

			return f.WithDetails(map[string]any{"failedItem": i, "itemsReversed": len(p.Items) - 1 - i, "transferId": it.TransferId})
		}
	}

	return nil
}

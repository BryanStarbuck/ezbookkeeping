package machine

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/mayswind/ezbookkeeping/pkg/api"
	"github.com/mayswind/ezbookkeeping/pkg/core"
	"github.com/mayswind/ezbookkeeping/pkg/errfile"
	"github.com/mayswind/ezbookkeeping/pkg/models"
)

// routes_categorize.go — the categorisation workflow (apis.mdx §9.8). Imported rows land in a
// fallback category ("Imported > Uncategorized"); a categoriser — usually an agent driving the MCP —
// then works through them. Three routes serve it:
//
//   GET  /transactions/uncategorized  the work queue: fallback rows grouped by normalised payee,
//                                     each group with its ids and a suggestion drawn from rows the
//                                     operator (or an earlier pass) already categorised
//   POST /transactions/categorize     per-row assignments — many categories in one previewed,
//                                     confirmed, undoable write; may change the major category
//                                     (income / expense / transfer) when allowed
//   POST /categories/ensure           make sure "Type > Group > Sub" paths exist, creating only
//                                     what is missing
//   GET  /categories/tree             the whole two-level tree — every group with its
//                                     sub-categories — as JSON and as one YAML document, so a
//                                     categoriser reads the choices before it picks one
//
// The three levels an operator thinks in map onto ezBookkeeping like this: the MAJOR category is
// the category type (income, expense, transfer — it is also the transaction's type); the GROUP is
// the primary category; the SUB-CATEGORY is the secondary category a transaction actually holds.
// A path "Expense > Food & Drink > Coffee" names all three.

func init() {
	registerRoutes(catzRoutes)
}

func catzRoutes() []RouteDef {
	return []RouteDef{
		{Method: "GET", Path: "/transactions/uncategorized", Tier: TierRead, Composed: true, Summary: "The categorisation work queue: rows in the fallback categories grouped by normalised payee, largest group first, each with its ids and a suggestion learned from already-categorised rows with the same payee.", Handler: catzHandleQueue, Untrusted: []string{"payee", "variants", "accountNames"}},
		{Method: "POST", Path: "/transactions/categorize", Tier: TierWrite, DryRunnable: true, Composed: true, Summary: "Categorise many transactions in one write: assignments [{ids, category (\"Type > Group > Sub\") | category_id}]; allow_type_change lets the major category (income/expense/transfer) change; dry run by default, undoable.", Handler: catzHandleCategorize},
		{Method: "GET", Path: "/categories/tree", Tier: TierRead, Summary: "Every category group (primary) with its sub-categories, by major category (income, expense, transfer) in display order — as JSON and as one YAML document (data.yaml; format=yaml answers the document alone). Args: type, include_hidden (false), format (json|yaml).", Handler: catzHandleTree, Untrusted: []string{"name", "yaml"}},
		{Method: "POST", Path: "/categories/ensure", Tier: TierWrite, DryRunnable: true, Composed: true, Summary: "Make sure category paths \"Type > Group > Sub\" exist, creating only the missing groups and sub-categories; existing ones are reported with their ids.", Handler: catzHandleEnsure},
	}
}

// ─── paths ─────────────────────────────────────────────────────────────────────────────────────

// catzPath is a parsed category path; Type is 0 when the path did not name one
type catzPath struct {
	Type  models.TransactionCategoryType
	Group string
	Sub   string
}

func (p catzPath) String() string {
	parts := []string{}

	if p.Type != 0 {
		parts = append(parts, txnCategoryTypeLabel(p.Type))
	}

	if p.Group != "" {
		parts = append(parts, p.Group)
	}

	return strings.Join(append(parts, p.Sub), " > ")
}

// catzParsePath parses "Sub", "Group > Sub" or "Type > Group > Sub"
func catzParsePath(s string) (catzPath, error) {
	raw := strings.Split(s, ">")
	parts := make([]string, 0, len(raw))

	for _, r := range raw {
		r = strings.TrimSpace(r)

		if r == "" {
			return catzPath{}, Invalid("write a path as \"Type > Group > Sub\", e.g. \"Expense > Food & Drink > Coffee\"", "category path %q has an empty segment", s)
		}

		parts = append(parts, r)
	}

	switch len(parts) {
	case 1:
		return catzPath{Sub: parts[0]}, nil
	case 2:
		return catzPath{Group: parts[0], Sub: parts[1]}, nil
	case 3:
		t, err := refCategoryType(parts[0])

		if err != nil || t == 0 {
			errfile.Expected("parsing the major category of a category path", err)
			return catzPath{}, Invalid("the first of three segments is the major category: Income, Expense or Transfer", "category path %q does not start with a type", s)
		}

		return catzPath{Type: t, Group: parts[1], Sub: parts[2]}, nil
	}

	return catzPath{}, Invalid("categories have two levels under a type: \"Type > Group > Sub\"", "category path %q has %d segments", s, len(parts))
}

// resolveCategoryFor resolves a category reference for a row of type ttype: a path that matches a
// sub-category of the row's own type wins; only when none does is every type considered (which is
// a change of major category). "Imported > Uncategorized" therefore means the expense one for an
// expense row and the income one for an income row.
func (lk *txnLookup) resolveCategoryFor(id, ref string, ttype models.TransactionType) (*models.TransactionCategory, error) {
	var categoryId int64
	var err error

	switch {
	case strings.TrimSpace(id) != "":
		if categoryId, err = ResolveId("category_id", id); err != nil {
			return nil, err
		}
	case strings.TrimSpace(ref) != "":
		if _, err := catzParsePath(ref); err != nil {
			return nil, err
		}

		hint := "GET /machine/v1/categories lists them; \"Type > Group > Sub\" names one exactly; POST /machine/v1/categories/ensure creates missing ones"
		own := txnCategoryTypeFor(ttype)

		if own != 0 {
			categoryId, err = txnResolveName("category", "category", ref, lk.categoryCandidates(true, own), hint)
		}

		if own == 0 || (err != nil && err.(*Fail).Code == CodeNotFound) {
			categoryId, err = txnResolveName("category", "category", ref, lk.categoryCandidates(true, 0), hint)
		}

		if err != nil {
			return nil, err
		}
	default:
		return nil, Invalid("pass category (\"Type > Group > Sub\") or category_id", "an assignment names no category")
	}

	c := lk.catMap[categoryId]

	if c == nil {
		return nil, NotFound("GET /machine/v1/categories lists them", "no category with id %s", idString(categoryId))
	}

	if c.ParentCategoryId == models.LevelOneTransactionCategoryParentId {
		return nil, Invalid("pick one of its sub-categories; transactions take a secondary category", "category %q is a group (primary category)", lk.categoryFullPath(c.CategoryId))
	}

	return c, nil
}

// ─── POST /transactions/categorize ─────────────────────────────────────────────────────────────

type catzAssignment struct {
	Id                 string      `json:"id,omitempty"`
	Ids                []string    `json:"ids,omitempty"`
	CategoryId         string      `json:"category_id,omitempty"`
	Category           string      `json:"category,omitempty"`
	CounterAccountId   string      `json:"counter_account_id,omitempty"`
	CounterAccountName string      `json:"counter_account_name,omitempty"`
	CounterAmount      json.Number `json:"counter_amount,omitempty"`
}

type catzCategorizeBody struct {
	WriteOpts
	Assignments     []catzAssignment `json:"assignments"`
	AllowTypeChange bool             `json:"allow_type_change,omitempty"`
	SkipInvalid     bool             `json:"skip_invalid,omitempty"`
	SummaryOnly     bool             `json:"summary_only,omitempty"`
}

// catzTarget is what one assignment asks of one row
type catzTarget struct {
	Index    int
	Category *models.TransactionCategory
	Counter  *models.Account
	// CounterAmount is the amount on the counter account's side; nil means "the same amount"
	CounterAmount *int64
}

// catzRejection is one row an assignment could not be applied to
type catzRejection struct {
	Index   int    `json:"index"`
	Id      string `json:"id"`
	Code    string `json:"code"`
	Message string `json:"message"`
	Hint    string `json:"hint,omitempty"`
}

// catzTargetState computes a row's after-state. It is pure: it reads only the lookup.
func catzTargetState(lk *txnLookup, s txnState, t catzTarget, allowTypeChange bool) (txnState, error) {
	have := models.TransactionType(s.Type)
	want := map[models.TransactionCategoryType]models.TransactionType{
		models.CATEGORY_TYPE_INCOME:   models.TRANSACTION_TYPE_INCOME,
		models.CATEGORY_TYPE_EXPENSE:  models.TRANSACTION_TYPE_EXPENSE,
		models.CATEGORY_TYPE_TRANSFER: models.TRANSACTION_TYPE_TRANSFER,
	}[t.Category.Type]
	path := lk.categoryFullPath(t.Category.CategoryId)

	s.TagIds = append([]string{}, s.TagIds...)
	s.CategoryId = idString(t.Category.CategoryId)

	if have == models.TRANSACTION_TYPE_MODIFY_BALANCE {
		return s, Invalid("an opening balance or balance modification takes no category; leave it out", "transaction %s is a balance modification", s.Id)
	}

	if have == want {
		if t.Counter != nil {
			return s, Invalid("drop counter_account; it is only for turning a row into a transfer or out of one", "the row is already a %s", txnTypeName(int64(have)))
		}

		return s, nil
	}

	if !allowTypeChange {
		return s, Invalid("pass allow_type_change: true to move the row to another major category, or pick a "+txnTypeName(int64(have))+" category", "%q is in the %s major category but transaction %s is of type %s", path, txnCategoryTypeName(t.Category.Type), s.Id, txnTypeName(int64(have)))
	}

	src, _ := strconv.ParseInt(s.SourceAccountId, 10, 64)

	switch {
	case want == models.TRANSACTION_TYPE_TRANSFER:
		// income or expense → transfer: the row's own account is one side, the counter account the other
		if t.Counter == nil {
			return s, Invalid("pass counter_account_id or counter_account_name: the operator's other account the money went to or came from", "turning transaction %s into a transfer needs the other account", s.Id)
		}

		if t.Counter.AccountId == src {
			return s, Invalid("the counter account must be a different account", "transaction %s is already on %q", s.Id, t.Counter.Name)
		}

		amount := s.SourceAmount
		counterAmount := amount

		if t.CounterAmount != nil {
			counterAmount = *t.CounterAmount
		} else if lk.accountCurrency(src) != t.Counter.Currency {
			return s, Invalid("pass counter_amount in integer hundredths of "+t.Counter.Currency, "%q is in %s and the row is in %s", t.Counter.Name, t.Counter.Currency, lk.accountCurrency(src))
		}

		s.Type = int(models.TRANSACTION_TYPE_TRANSFER)

		if have == models.TRANSACTION_TYPE_EXPENSE {
			s.DestinationAccountId = idString(t.Counter.AccountId)
			s.DestinationAmount = counterAmount
		} else {
			// money came IN to this account: the counter account is the source
			s.DestinationAccountId = s.SourceAccountId
			s.DestinationAmount = amount
			s.SourceAccountId = idString(t.Counter.AccountId)
			s.SourceAmount = counterAmount
		}
	case have == models.TRANSACTION_TYPE_TRANSFER:
		// transfer → expense keeps the source side; transfer → income keeps the destination side
		if t.Counter != nil {
			return s, Invalid("drop counter_account; a transfer becoming income or expense keeps one of its own sides", "counter_account given for a transfer becoming %s", txnTypeName(int64(want)))
		}

		s.Type = int(want)

		if want == models.TRANSACTION_TYPE_INCOME {
			s.SourceAccountId = s.DestinationAccountId
			s.SourceAmount = s.DestinationAmount
		}

		s.DestinationAccountId = ""
		s.DestinationAmount = 0
	default:
		// income ↔ expense: same account, same amount, the direction flips
		if t.Counter != nil {
			return s, Invalid("drop counter_account; income and expense rows have one account", "counter_account given for a %s becoming %s", txnTypeName(int64(have)), txnTypeName(int64(want)))
		}

		s.Type = int(want)
	}

	return s, nil
}

func catzHandleCategorize(mc *Ctx) (any, error) {
	var body catzCategorizeBody

	if err := mc.BindBody(&body); err != nil {
		return nil, err
	}

	if len(body.Assignments) == 0 {
		return nil, Invalid("pass assignments: [{ids: [...], category: \"Type > Group > Sub\"}]", "assignments is empty")
	}

	resolve := func() (*Plan, error) {
		lk, err := txnLoadLookup(mc)

		if err != nil {
			return nil, err
		}

		return catzPlanAssignments(mc, lk, &body)
	}

	return RunWrite(mc, body.WriteOpts, resolve, catzApply(mc))
}

// catzPlanAssignments resolves a categorise body against the lookup into the bulk plan: the half of
// POST /transactions/categorize that POST /ingest/categorize shares (it builds the assignments from
// a statement file instead of a caller's ids)
func catzPlanAssignments(mc *Ctx, lk *txnLookup, body *catzCategorizeBody) (*Plan, error) {
	// every id once, in request order, remembering which assignment named it
	var wanted []int64
	owner := map[int64]int{}

	for i, a := range body.Assignments {
		ids := append([]string{}, a.Ids...)

		if strings.TrimSpace(a.Id) != "" {
			ids = append(ids, a.Id)
		}

		if len(ids) == 0 {
			return nil, Invalid("give every assignment id or ids", "assignment %d names no transaction", i)
		}

		for _, s := range ids {
			id, err := ResolveId(fmt.Sprintf("assignments[%d].ids", i), s)

			if err != nil {
				return nil, err
			}

			if prev, dup := owner[id]; dup {
				return nil, Invalid("name each transaction in one assignment only", "transaction %s is in assignments %d and %d", s, prev, i)
			}

			owner[id] = i
			wanted = append(wanted, id)
		}
	}

	if len(wanted) > txnSelectCap {
		return nil, Conflict(fmt.Sprintf("split the work into calls of %d rows or fewer", txnSelectCap), "%d transactions were named; one call categorises at most %d", len(wanted), txnSelectCap)
	}

	states, canon, missing, err := txnReadStates(mc, wanted)

	if err != nil {
		return nil, err
	}

	var rejected []catzRejection
	reject := func(i int, id string, err error) error {
		f, ok := err.(*Fail)

		if !ok {
			return err
		}

		if !body.SkipInvalid {
			return f
		}

		rejected = append(rejected, catzRejection{Index: i, Id: id, Code: f.Code, Message: f.Message, Hint: f.Hint})
		return nil
	}

	for _, id := range missing {
		if err := reject(owner[id], idString(id), NotFound("GET /machine/v1/transactions lists them; drop the id", "transaction %s does not exist", idString(id))); err != nil {
			return nil, err
		}
	}

	// resolve each assignment's category per row type, and its counter account, once
	type catKey struct {
		index int
		ttype models.TransactionType
	}

	type catResolved struct {
		cat *models.TransactionCategory
		err error
	}

	catCache := map[catKey]catResolved{}
	counters := map[int]*models.Account{}
	counterAmounts := map[int]*int64{}

	for i, a := range body.Assignments {
		if strings.TrimSpace(a.CounterAccountId) != "" || strings.TrimSpace(a.CounterAccountName) != "" {
			acc, err := lk.resolveAccount("counter_account_id", a.CounterAccountId, "counter_account_name", a.CounterAccountName, true)

			if err != nil {
				return nil, err
			}

			counters[i] = acc
		}

		if a.CounterAmount != "" {
			v, err := AmountArg(fmt.Sprintf("assignments[%d].counter_amount", i), a.CounterAmount)

			if err != nil {
				return nil, err
			}

			counterAmounts[i] = &v
		}
	}

	sel := &txnSelection{States: map[int64]*txnState{}, By: "ids"}
	afters := map[int64]txnState{}
	assignedTo := map[int64]int{}

	for _, id := range wanted {
		apiId, ok := canon[id]

		if !ok {
			continue
		}

		if _, seen := sel.States[apiId]; seen {
			continue // both legs of one transfer were named
		}

		i := owner[id]
		s := states[apiId]
		key := catKey{i, models.TransactionType(s.Type)}
		res, ok := catCache[key]

		if !ok {
			a := body.Assignments[i]
			res.cat, res.err = lk.resolveCategoryFor(a.CategoryId, a.Category, models.TransactionType(s.Type))
			catCache[key] = res
		}

		if res.err != nil {
			if err := reject(i, s.Id, res.err); err != nil {
				return nil, err
			}

			continue
		}

		cat := res.cat

		after, err := catzTargetState(lk, *s, catzTarget{Index: i, Category: cat, Counter: counters[i], CounterAmount: counterAmounts[i]}, body.AllowTypeChange)

		if err != nil {
			if err := reject(i, s.Id, err); err != nil {
				return nil, err
			}

			continue
		}

		sel.Ids = append(sel.Ids, apiId)
		sel.States[apiId] = s
		afters[apiId] = after
		assignedTo[apiId] = i
	}

	plan, err := txnBulkPlan(mc, lk, sel, func(s txnState) (txnState, error) {
		id, _ := strconv.ParseInt(s.Id, 10, 64)
		return afters[id], nil
	}, nil)

	if err != nil {
		return nil, err
	}

	st := plan.State.(*txnBulkState)
	typeChanges := 0

	type bucket struct {
		CategoryId   string `json:"categoryId"`
		CategoryPath string `json:"categoryPath"`
		Count        int    `json:"count"`
		TypeChanges  int    `json:"typeChanges,omitempty"`
	}

	buckets := map[string]*bucket{}
	var order []string

	for _, id := range st.Changed {
		before, after := st.Before[id], st.After[id]
		b := buckets[after.CategoryId]

		if b == nil {
			cid, _ := strconv.ParseInt(after.CategoryId, 10, 64)
			b = &bucket{CategoryId: after.CategoryId, CategoryPath: lk.categoryFullPath(cid)}
			buckets[after.CategoryId] = b
			order = append(order, after.CategoryId)
		}

		b.Count++

		if before.Type != after.Type {
			b.TypeChanges++
			typeChanges++
		}
	}

	byCategory := make([]*bucket, 0, len(order))

	for _, k := range order {
		byCategory = append(byCategory, buckets[k])
	}

	sort.SliceStable(byCategory, func(i, j int) bool { return byCategory[i].Count > byCategory[j].Count })

	preview := plan.Preview.(map[string]any)
	preview["byCategory"] = byCategory
	preview["assignments"] = len(body.Assignments)

	if rejected == nil {
		rejected = []catzRejection{}
	}

	preview["skipped"] = rejected
	plan.Changes["skipped"] = len(rejected)

	if typeChanges > 0 {
		plan.Changes["type_change"] = typeChanges
		plan.Warnings = append(plan.Warnings, fmt.Sprintf("%d rows change their major category (income / expense / transfer); each one's before and after type is in its changes", typeChanges))
	}

	if body.SummaryOnly {
		preview["rowsOmitted"] = len(st.Changed)
		delete(preview, "rows")
	}

	return plan, nil
}

// catzApply is the apply half of a categorise plan: one bulk update, journaled for /undo
func catzApply(mc *Ctx) func(p *Plan) (any, error) {
	return func(p *Plan) (any, error) {
		st := p.State.(*txnBulkState)

		if len(st.Changed) == 0 {
			return map[string]any{"updated": 0, "ids": []string{}}, nil
		}

		if err := txnApplyBulkStates(mc, st); err != nil {
			return nil, err
		}

		return txnBulkFinish(mc, st, fmt.Sprintf("categorise %d transactions", len(st.Changed)))
	}
}

// ─── GET /transactions/uncategorized ───────────────────────────────────────────────────────────

// catzQueueRow is one fallback row, flattened for grouping
type catzQueueRow struct {
	Id       string
	Date     string
	Type     models.TransactionType
	Amount   int64 // signed: expense negative
	Currency string
	Account  string
	Comment  string
}

// catzVote is one category seen for a payee in the categorised history
type catzVote struct {
	CategoryId   string `json:"categoryId"`
	CategoryPath string `json:"categoryPath"`
	Votes        int    `json:"votes"`
}

// catzSuggestion is the queue's best guess for a group, with its evidence
type catzSuggestion struct {
	catzVote
	// Of is how many categorised rows share the key; Votes of Of chose this category
	Of int `json:"of"`
	// Basis is "same_payee" (the whole normalised payee matched) or "payee_prefix" (its first two words)
	Basis        string     `json:"basis"`
	Alternatives []catzVote `json:"alternatives"`
}

// catzPrefixKey is the first two words of a normalised payee, the looser match
func catzPrefixKey(key string) string {
	f := strings.Fields(key)

	if len(f) < 2 {
		return ""
	}

	return f[0] + " " + f[1]
}

// catzHistory counts the categories chosen for each payee among categorised rows
type catzHistory struct {
	exact  map[string]map[string]int
	prefix map[string]map[string]int
	rows   int
}

func newCatzHistory() *catzHistory {
	return &catzHistory{exact: map[string]map[string]int{}, prefix: map[string]map[string]int{}}
}

func (h *catzHistory) add(comment, categoryId string) {
	key := anNormalizePayee(comment)

	if key == "" {
		return
	}

	h.rows++

	if h.exact[key] == nil {
		h.exact[key] = map[string]int{}
	}

	h.exact[key][categoryId]++

	if p := catzPrefixKey(key); p != "" {
		if h.prefix[p] == nil {
			h.prefix[p] = map[string]int{}
		}

		h.prefix[p][categoryId]++
	}
}

// suggest ranks the categories seen for key; allowed filters to the category types a group's rows
// could take without a type change (nil means any)
func (h *catzHistory) suggest(lk *txnLookup, key string, allowed map[models.TransactionCategoryType]bool) *catzSuggestion {
	rank := func(votes map[string]int, basis string) *catzSuggestion {
		var list []catzVote
		of := 0

		for cid, n := range votes {
			id, _ := strconv.ParseInt(cid, 10, 64)
			c := lk.catMap[id]

			if c == nil || (allowed != nil && !allowed[c.Type]) {
				continue
			}

			of += n
			list = append(list, catzVote{CategoryId: cid, CategoryPath: lk.categoryFullPath(id), Votes: n})
		}

		if len(list) == 0 {
			return nil
		}

		sort.Slice(list, func(i, j int) bool {
			if list[i].Votes != list[j].Votes {
				return list[i].Votes > list[j].Votes
			}

			return list[i].CategoryPath < list[j].CategoryPath
		})

		alt := list[1:]

		if len(alt) > 3 {
			alt = alt[:3]
		}

		return &catzSuggestion{catzVote: list[0], Of: of, Basis: basis, Alternatives: append([]catzVote{}, alt...)}
	}

	if key == "" {
		return nil
	}

	if s := rank(h.exact[key], "same_payee"); s != nil {
		return s
	}

	if p := catzPrefixKey(key); p != "" {
		return rank(h.prefix[p], "payee_prefix")
	}

	return nil
}

// catzQueueGroup is one payee's rows in the work queue
type catzQueueGroup struct {
	Payee        string           `json:"payee"`
	Count        int              `json:"count"`
	Types        map[string]int   `json:"types"`
	Totals       []map[string]any `json:"totals"`
	FirstDate    string           `json:"firstDate"`
	LastDate     string           `json:"lastDate"`
	AccountNames []string         `json:"accountNames"`
	Variants     []string         `json:"variants"`
	Ids          []string         `json:"ids"`
	IdsTruncated bool             `json:"idsTruncated"`
	Suggestion   *catzSuggestion  `json:"suggestion"`
	Samples      []map[string]any `json:"samples"`
	typesSeen    map[models.TransactionType]bool
	totals       map[string]int64
}

// catzGroupQueue groups fallback rows by normalised payee, largest group first (ties by payee)
func catzGroupQueue(rows []catzQueueRow, idsPerGroup int) []*catzQueueGroup {
	groups := map[string]*catzQueueGroup{}
	var order []string

	for _, r := range rows {
		key := anNormalizePayee(r.Comment)
		g := groups[key]

		if g == nil {
			g = &catzQueueGroup{Payee: key, Types: map[string]int{}, typesSeen: map[models.TransactionType]bool{}, totals: map[string]int64{}, FirstDate: r.Date, LastDate: r.Date}
			groups[key] = g
			order = append(order, key)
		}

		g.Count++
		g.Types[txnTypeName(int64(r.Type))]++
		g.typesSeen[r.Type] = true
		g.totals[r.Currency] += r.Amount

		if r.Date < g.FirstDate {
			g.FirstDate = r.Date
		}

		if r.Date > g.LastDate {
			g.LastDate = r.Date
		}

		if !catzContains(g.AccountNames, r.Account) {
			g.AccountNames = append(g.AccountNames, r.Account)
		}

		if len(g.Variants) < 5 && !catzContains(g.Variants, r.Comment) {
			g.Variants = append(g.Variants, r.Comment)
		}

		if len(g.Ids) < idsPerGroup {
			g.Ids = append(g.Ids, r.Id)
		} else {
			g.IdsTruncated = true
		}

		if len(g.Samples) < 3 {
			g.Samples = append(g.Samples, map[string]any{"id": r.Id, "date": r.Date, "type": txnTypeName(int64(r.Type)), "amount": r.Amount, "currency": r.Currency, "account": r.Account, "comment": r.Comment})
		}
	}

	out := make([]*catzQueueGroup, 0, len(order))

	for _, k := range order {
		g := groups[k]

		if g.Payee == "" {
			g.Payee = "(no description)"
		}

		currencies := make([]string, 0, len(g.totals))

		for c := range g.totals {
			currencies = append(currencies, c)
		}

		sort.Strings(currencies)

		for _, c := range currencies {
			g.Totals = append(g.Totals, map[string]any{"currency": c, "amount": g.totals[c]})
		}

		sort.Strings(g.AccountNames)
		out = append(out, g)
	}

	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}

		return out[i].Payee < out[j].Payee
	})

	return out
}

func catzContains(list []string, v string) bool {
	for _, s := range list {
		if s == v {
			return true
		}
	}

	return false
}

// catzDefaultFallback is every sub-category named "Uncategorized" — the conventional fallback the
// import playbook creates under "Imported" for each type
func catzDefaultFallback(lk *txnLookup) []int64 {
	var out []int64

	for _, c := range lk.categories {
		if c.ParentCategoryId != models.LevelOneTransactionCategoryParentId && strings.EqualFold(strings.TrimSpace(c.Name), "Uncategorized") {
			out = append(out, c.CategoryId)
		}
	}

	return out
}

func catzHandleQueue(mc *Ctx) (any, error) {
	if err := anCheckQueryOnly(mc, "category_ids", "account_ids", "start", "end", "group_limit", "offset", "ids_per_group", "min_count"); err != nil {
		return nil, err
	}

	lk, err := txnLoadLookup(mc)

	if err != nil {
		return nil, err
	}

	fallback, err := anParseIdList(mc, "category_ids")

	if err != nil {
		return nil, err
	}

	source := "argument"

	if len(fallback) == 0 {
		fallback, source = catzDefaultFallback(lk), "default: every sub-category named Uncategorized"
	}

	if len(fallback) == 0 {
		return nil, Invalid("pass category_ids: the fallback categories the import used (ezb_list_categories shows them)", "no category_ids were given and no sub-category is named Uncategorized")
	}

	fallbackSet := map[int64]bool{}
	cats := make([]map[string]string, 0, len(fallback))

	for _, id := range fallback {
		c := lk.catMap[id]

		if c == nil {
			return nil, NotFound("GET /machine/v1/categories lists the category ids", "category %s does not exist", idString(id))
		}

		if c.ParentCategoryId == models.LevelOneTransactionCategoryParentId {
			for _, sub := range lk.categories {
				if sub.ParentCategoryId == id {
					fallbackSet[sub.CategoryId] = true
				}
			}
		}

		fallbackSet[id] = true
		cats = append(cats, map[string]string{"id": idString(id), "path": lk.categoryFullPath(id)})
	}

	accountFilter, err := anParseIdList(mc, "account_ids")

	if err != nil {
		return nil, err
	}

	accountSet := map[int64]bool{}

	for _, id := range accountFilter {
		accountSet[id] = true
	}

	groupLimit, err := anParseIntArg(mc, "group_limit", 50, 1, 1000)

	if err != nil {
		return nil, err
	}

	offset, err := anParseIntArg(mc, "offset", 0, 0, 1<<31)

	if err != nil {
		return nil, err
	}

	idsPerGroup, err := anParseIntArg(mc, "ids_per_group", 100, 1, int64(MaxLimit))

	if err != nil {
		return nil, err
	}

	minCount, err := anParseIntArg(mc, "min_count", 1, 1, 1<<31)

	if err != nil {
		return nil, err
	}

	f := anListFilter{}

	if v := mc.Query("start"); v != "" {
		d, err := ParseDate("start", v, mc.Loc)

		if err != nil {
			return nil, err
		}

		f.StartUnix = d.Unix()
	}

	if v := mc.Query("end"); v != "" {
		d, err := ParseDate("end", v, mc.Loc)

		if err != nil {
			return nil, err
		}

		f.EndUnix = d.AddDate(0, 0, 1).Unix() - 1
	}

	// one pass over every row: the fallback rows are the queue, the rest are the history
	rows, err := anFetchAllTransactions(mc, f)

	if err != nil {
		return nil, err
	}

	history := newCatzHistory()
	var queue []catzQueueRow
	seenTransfer := map[string]bool{}

	for _, t := range rows {
		if t.Type == models.TRANSACTION_TYPE_MODIFY_BALANCE {
			continue
		}

		if t.Type == models.TRANSACTION_TYPE_TRANSFER {
			// list/all carries a transfer once per side; count it once
			k := fmt.Sprintf("%d|%d|%d|%d|%d", t.Time, t.SourceAccountId, t.DestinationAccountId, t.SourceAmount, t.CategoryId)

			if seenTransfer[k] {
				continue
			}

			seenTransfer[k] = true
		}

		if !fallbackSet[t.CategoryId] {
			history.add(t.Comment, idString(t.CategoryId))
			continue
		}

		if len(accountSet) > 0 && !accountSet[t.SourceAccountId] && !accountSet[t.DestinationAccountId] {
			continue
		}

		amount := t.SourceAmount

		if t.Type == models.TRANSACTION_TYPE_EXPENSE {
			amount = -amount
		}

		queue = append(queue, catzQueueRow{
			Id:       idString(t.Id),
			Date:     DateOfUnix(t.Time, mc.Loc),
			Type:     t.Type,
			Amount:   amount,
			Currency: lk.accountCurrency(t.SourceAccountId),
			Account:  lk.accountName(t.SourceAccountId),
			Comment:  t.Comment,
		})
	}

	all := catzGroupQueue(queue, int(idsPerGroup))
	var kept []*catzQueueGroup

	for _, g := range all {
		if int64(g.Count) >= minCount {
			kept = append(kept, g)
		}
	}

	total := len(kept)
	start := int(offset)

	if start > total {
		start = total
	}

	end := start + int(groupLimit)

	if end > total {
		end = total
	}

	page := kept[start:end]

	for _, g := range page {
		allowed := map[models.TransactionCategoryType]bool{}

		for t := range g.typesSeen {
			if ct := txnCategoryTypeFor(t); ct != 0 {
				allowed[ct] = true
			}
		}

		g.Suggestion = history.suggest(lk, strings.TrimPrefix(g.Payee, "(no description)"), allowed)
	}

	if end < total {
		mc.Truncated(int(groupLimit))
		mc.SetMeta("nextOffset", end)
	}

	if page == nil {
		page = []*catzQueueGroup{}
	}

	return map[string]any{
		"categories":        cats,
		"categoriesSource":  source,
		"uncategorizedRows": len(queue),
		"groupCount":        total,
		"offset":            start,
		"groups":            page,
		"historyRows":       history.rows,
		"note":              "groups are keyed by the normalised payee (upper-case, punctuation, card-network words and reference numbers dropped); a suggestion is only a count of what already-categorised rows with the same payee were given — confirm it before using it. Categorised groups leave the queue, so fetch offset 0 again after each write.",
	}, nil
}

// ─── POST /categories/ensure ───────────────────────────────────────────────────────────────────

type catzEnsureBody struct {
	WriteOpts
	Paths []string `json:"paths"`
	// Color and Icon apply to new groups; a new sub-category takes its group's
	Color string  `json:"color,omitempty"`
	Icon  refFlex `json:"icon,omitempty"`
}

// catzEnsureItem is one path's decision
type catzEnsureItem struct {
	Path       string `json:"path"`
	Action     string `json:"action"` // exists | create_sub | create_group_and_sub
	GroupId    string `json:"groupId,omitempty"`
	CategoryId string `json:"categoryId,omitempty"`
}

// catzPlanEnsure decides, for each full path, what exists and what must be created. Pure.
func catzPlanEnsure(all []*models.TransactionCategory, paths []string) ([]catzEnsureItem, []catzPath, error) {
	var items []catzEnsureItem
	var parsed []catzPath
	seen := map[string]bool{}

	findGroup := func(t models.TransactionCategoryType, name string) *models.TransactionCategory {
		for _, c := range all {
			if c.Type == t && c.ParentCategoryId == models.LevelOneTransactionCategoryParentId && strings.EqualFold(c.Name, name) {
				return c
			}
		}

		return nil
	}

	findSub := func(parent int64, name string) *models.TransactionCategory {
		for _, c := range all {
			if c.ParentCategoryId == parent && parent != models.LevelOneTransactionCategoryParentId && strings.EqualFold(c.Name, name) {
				return c
			}
		}

		return nil
	}

	for _, raw := range paths {
		p, err := catzParsePath(raw)

		if err != nil {
			return nil, nil, err
		}

		if p.Type == 0 || p.Group == "" {
			return nil, nil, Invalid("give every path all three levels: \"Type > Group > Sub\"", "path %q does not name its type and group", raw)
		}

		for _, n := range []string{p.Group, p.Sub} {
			if _, err := refName("category", n); err != nil {
				return nil, nil, err
			}
		}

		key := strings.ToLower(p.String())

		if seen[key] {
			continue
		}

		seen[key] = true
		item := catzEnsureItem{Path: p.String()}

		if g := findGroup(p.Type, p.Group); g != nil {
			item.GroupId = idString(g.CategoryId)

			if s := findSub(g.CategoryId, p.Sub); s != nil {
				item.Action, item.CategoryId = "exists", idString(s.CategoryId)
			} else {
				item.Action = "create_sub"
			}
		} else {
			item.Action = "create_group_and_sub"
		}

		items = append(items, item)
		parsed = append(parsed, p)
	}

	return items, parsed, nil
}

func catzHandleEnsure(mc *Ctx) (any, error) {
	var body catzEnsureBody

	if err := mc.BindBody(&body); err != nil {
		return nil, err
	}

	if len(body.Paths) == 0 {
		return nil, Invalid("pass paths: [\"Expense > Food & Drink > Coffee\", …]", "paths is empty")
	}

	color, err := refColor(body.Color, refDefaultColor)

	if err != nil {
		return nil, err
	}

	icon, err := refIcon(body.Icon, refDefaultIcon)

	if err != nil {
		return nil, err
	}

	type ensureState struct {
		Items  []catzEnsureItem
		Parsed []catzPath
	}

	resolve := func() (*Plan, error) {
		all, err := refLoadCategories(mc)

		if err != nil {
			return nil, err
		}

		items, parsed, err := catzPlanEnsure(all, body.Paths)

		if err != nil {
			return nil, err
		}

		groups := map[string]bool{}
		changes := map[string]int{"exists": 0, "create_group": 0, "create_sub": 0}

		for i, it := range items {
			switch it.Action {
			case "exists":
				changes["exists"]++
			case "create_group_and_sub":
				gk := strings.ToLower(txnCategoryTypeLabel(parsed[i].Type) + ">" + parsed[i].Group)

				if !groups[gk] {
					groups[gk] = true
					changes["create_group"]++
				}

				changes["create_sub"]++
			default:
				changes["create_sub"]++
			}
		}

		count := changes["create_group"] + changes["create_sub"]
		var warnings []string

		if changes["create_group"] > 0 {
			warnings = append(warnings, "new groups get the default colour and icon unless color and icon are passed; rename or restyle them later with PATCH /machine/v1/categories/:id")
		}

		return &Plan{Changes: changes, Count: count, Preview: map[string]any{"paths": items}, Warnings: warnings, State: &ensureState{Items: items, Parsed: parsed}}, nil
	}

	apply := func(p *Plan) (any, error) {
		st := p.State.(*ensureState)
		groupIds := map[string]int64{}
		var newGroups, newSubsOfOld, allNew []int64

		create := func(req models.TransactionCategoryCreateRequest) (int64, error) {
			res, err := mc.CallUpstream(api.TransactionCategories.CategoryCreateHandler, "POST", nil, req)

			if err != nil {
				return 0, err
			}

			return refCreatedId(res)
		}

		journal := func() {
			deleteIds := append(append([]int64{}, newGroups...), newSubsOfOld...)

			if err := refJournalCreate(mc, "category", deleteIds, allNew, fmt.Sprintf("ensure %d category paths (%d created)", len(st.Items), len(allNew))); err != nil {
				errfile.Caught("journaling the categories /categories/ensure created", err)
				mc.SetMeta("journalError", err.Error())
			}
		}

		out := make([]catzEnsureItem, len(st.Items))
		copy(out, st.Items)

		for i, it := range st.Items {
			if it.Action == "exists" {
				continue
			}

			p := st.Parsed[i]
			gk := strings.ToLower(txnCategoryTypeLabel(p.Type) + ">" + p.Group)
			groupId, known := groupIds[gk]
			subColor, subIcon := color, icon

			switch {
			case it.GroupId != "":
				groupId, _ = strconv.ParseInt(it.GroupId, 10, 64)

				all, err := refLoadCategories(mc)

				if err != nil {
					journal()
					return nil, err
				}

				for _, c := range all {
					if c.CategoryId == groupId {
						subColor, subIcon = c.Color, c.Icon
					}
				}
			case !known:
				id, err := create(models.TransactionCategoryCreateRequest{Name: p.Group, Type: p.Type, ParentId: 0, Icon: icon, IconType: core.ICON_TYPE_SYSTEM, Color: color})

				if err != nil {
					journal()
					return nil, err
				}

				groupId = id
				groupIds[gk] = id
				newGroups = append(newGroups, id)
				allNew = append(allNew, id)
			}

			subId, err := create(models.TransactionCategoryCreateRequest{Name: p.Sub, Type: p.Type, ParentId: groupId, Icon: subIcon, IconType: core.ICON_TYPE_SYSTEM, Color: subColor})

			if err != nil {
				journal()
				return nil, err
			}

			if it.GroupId != "" {
				newSubsOfOld = append(newSubsOfOld, subId)
			}

			allNew = append(allNew, subId)
			out[i].GroupId = idString(groupId)
			out[i].CategoryId = idString(subId)
		}

		journal()

		return map[string]any{"paths": out, "created": len(allNew)}, nil
	}

	return RunWrite(mc, body.WriteOpts, resolve, apply)
}

// ─── GET /categories/tree ──────────────────────────────────────────────────────────────────────

// catzTreeSub is one sub-category of the tree
type catzTreeSub struct {
	Name   string `json:"name" yaml:"name"`
	Id     string `json:"id" yaml:"id"`
	Hidden bool   `json:"hidden" yaml:"hidden"`
}

// catzTreeGroup is one group (primary category) with its sub-categories
type catzTreeGroup struct {
	Name          string        `json:"name" yaml:"name"`
	Type          string        `json:"type" yaml:"type"`
	Id            string        `json:"id" yaml:"id"`
	Hidden        bool          `json:"hidden" yaml:"hidden"`
	Subcategories []catzTreeSub `json:"subcategories" yaml:"subcategories"`
}

type catzTreeCounts struct {
	Groups        int `json:"groups" yaml:"groups"`
	Subcategories int `json:"subcategories" yaml:"subcategories"`
}

// catzTreeDoc is the YAML document. Its shape is shared with the sister apps' planes (Actual Budget,
// Firefly III): app, generated_at, counts, groups[name, type, id, hidden, subcategories[…]].
type catzTreeDoc struct {
	App         string          `yaml:"app"`
	GeneratedAt string          `yaml:"generated_at"`
	Counts      catzTreeCounts  `yaml:"counts"`
	Groups      []catzTreeGroup `yaml:"groups"`
}

// catzBuildTree turns the category list into the tree, reusing the list route's ordering (major
// category, then display order). Pure.
func catzBuildTree(all []*models.TransactionCategory, includeHidden bool, typ models.TransactionCategoryType) ([]catzTreeGroup, catzTreeCounts) {
	views := refCategoryTree(all, includeHidden, typ)
	groups := make([]catzTreeGroup, 0, len(views))
	counts := catzTreeCounts{}

	for _, v := range views {
		g := catzTreeGroup{Name: v.Name, Type: v.Type, Id: v.Id, Hidden: v.Hidden, Subcategories: []catzTreeSub{}}

		for _, s := range v.SubCategories {
			g.Subcategories = append(g.Subcategories, catzTreeSub{Name: s.Name, Id: s.Id, Hidden: s.Hidden})
		}

		counts.Groups++
		counts.Subcategories += len(g.Subcategories)
		groups = append(groups, g)
	}

	return groups, counts
}

// catzTreeYAML renders the tree as the shared YAML document
func catzTreeYAML(groups []catzTreeGroup, counts catzTreeCounts, generatedAt string) (string, error) {
	var buf strings.Builder
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)

	if err := enc.Encode(catzTreeDoc{App: "ezbookkeeping", GeneratedAt: generatedAt, Counts: counts, Groups: groups}); err != nil {
		errfile.Caught("rendering the category tree as YAML", err)
		return "", NewFail(CodeUpstreamError, "check ~/T/ezbookkeeping/error.err", "the category tree could not be rendered as YAML")
	}

	if err := enc.Close(); err != nil {
		errfile.Caught("closing the category tree's YAML encoder", err)
		return "", NewFail(CodeUpstreamError, "check ~/T/ezbookkeeping/error.err", "the category tree could not be rendered as YAML")
	}

	return buf.String(), nil
}

func catzHandleTree(mc *Ctx) (any, error) {
	if err := anCheckQueryOnly(mc, "type", "include_hidden", "format"); err != nil {
		return nil, err
	}

	typ, err := refCategoryType(mc.Query("type"))

	if err != nil {
		return nil, err
	}

	includeHidden, err := mc.QueryBool("include_hidden", false)

	if err != nil {
		return nil, err
	}

	format := strings.ToLower(strings.TrimSpace(mc.Query("format")))

	if format != "" && format != "json" && format != "yaml" {
		return nil, Invalid("format is json (the envelope, with the YAML document in data.yaml) or yaml (the document alone)", "unknown format %q", format)
	}

	all, err := refLoadCategories(mc)

	if err != nil {
		return nil, err
	}

	groups, counts := catzBuildTree(all, includeHidden, typ)
	generatedAt := time.Now().UTC().Format(time.RFC3339)
	doc, err := catzTreeYAML(groups, counts, generatedAt)

	if err != nil {
		return nil, err
	}

	if format == "yaml" {
		return &RawResult{ContentType: "application/yaml; charset=utf-8", Data: []byte(doc)}, nil
	}

	return map[string]any{
		"app":         "ezbookkeeping",
		"generatedAt": generatedAt,
		"counts":      counts,
		"groups":      groups,
		"yaml":        doc,
		"filters":     map[string]any{"type": refCategoryTypeNames[typ], "includeHidden": includeHidden},
		"note":        "the major category is each group's type (income, expense, transfer); transactions hold a sub-category, named by the path \"Type > Group > Sub\". POST /machine/v1/categories/ensure creates missing paths.",
	}, nil
}

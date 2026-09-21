package machine

import (
	"encoding/json"
	"fmt"
	"net/url"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/mayswind/ezbookkeeping/pkg/api"
	"github.com/mayswind/ezbookkeeping/pkg/core"
	"github.com/mayswind/ezbookkeeping/pkg/datastore"
	"github.com/mayswind/ezbookkeeping/pkg/models"
	"github.com/mayswind/ezbookkeeping/pkg/services"
	"github.com/mayswind/ezbookkeeping/pkg/utils"
)

// routes_transactions.go — the transaction family (apis.mdx §10.3).
//
// Reads walk upstream's own list / count / get / export handlers as the bound user. Writes go
// through RunWrite: the resolve half selects the exact rows (by ids, or by the SAME filter language
// GET /transactions takes) and computes every before/after; the apply half calls upstream's own
// batch handlers and journals the inverse. A transfer is ONE API transaction (its TRANSFER_OUT row)
// everywhere in this file — ids are canonicalised to the out-row, so a transfer is one change.

func init() {
	registerRoutes(txnRoutes)
	RegisterInverse(txnOpRestore, txnExecRestore)
	RegisterInverse(txnOpDelete, txnExecDelete)
	RegisterInverse(txnOpRecreate, txnExecRecreate)
}

// Inverse op kinds this family emits (apis.mdx §9.6)
const (
	// txn.restore sets rows back to a recorded state through TransactionModifyHandler
	txnOpRestore = "txn.restore"
	// txn.delete soft-deletes rows this plane created (undo of a create — §9.6's one sanctioned delete)
	txnOpDelete = "txn.delete"
	// txn.recreate re-creates rows an admin delete removed (new ids; pictures are not restored)
	txnOpRecreate = "txn.recreate"
)

// upstream pages transactions/list.json 1–50 rows at a time
const txnUpstreamPage = 50

// txnSelectCap is the most rows one bulk write may select by filter
const txnSelectCap = MaxLimit

var txnFilterHint = "filters: account_ids, account_names, start, end (YYYY-MM-DD), type, category_ids, category_names, tag_ids, tag_names, tag_filter, untagged, keyword, min_amount, max_amount, currency, with_pictures"

func txnRoutes() []RouteDef {
	untrusted := []string{"comment", "name", "sourceAccount.name", "destinationAccount.name", "category.name", "tags.name"}

	return []RouteDef{
		{Method: "GET", Path: "/transactions", Tier: TierRead, Summary: "Transactions matching the filters, newest first, walked through upstream's cursor up to limit (cap 5000); nextCursor pages on.", Handler: txnHandleList, Composed: true, Untrusted: untrusted, Features: []string{"transactions"}},
		{Method: "GET", Path: "/transactions/count", Tier: TierRead, Summary: "How many transactions match the filters (a transfer counts once).", Handler: txnHandleCount},
		{Method: "GET", Path: "/transactions/earliest", Tier: TierRead, Summary: "The earliest transaction, optionally of one account.", Handler: txnHandleEarliest, Composed: true, Untrusted: untrusted},
		{Method: "GET", Path: "/transactions/latest", Tier: TierRead, Summary: "The latest transaction, optionally of one account.", Handler: txnHandleLatest, Composed: true, Untrusted: untrusted},
		{Method: "GET", Path: "/transactions/export", Tier: TierRead, Summary: "The matching transactions as upstream's ezBookkeeping CSV or TSV.", Handler: txnHandleExport, Feature: featureExport},
		{Method: "GET", Path: "/transactions/:id", Tier: TierRead, Summary: "One transaction, with its tags, pictures metadata and both sides of a transfer.", Handler: txnHandleGet, Untrusted: untrusted},
		{Method: "POST", Path: "/transactions", Tier: TierWrite, DryRunnable: true, Composed: true, Summary: "Add transactions (income, expense, transfer); names resolve to ids; dry run by default.", Handler: txnHandleCreate, Untrusted: untrusted, Features: []string{"undo"}},
		{Method: "PATCH", Path: "/transactions/:id", Tier: TierWrite, DryRunnable: true, Summary: "Edit one transaction; the preview is the exact field-by-field before/after.", Handler: txnHandlePatch, Untrusted: untrusted},
		{Method: "POST", Path: "/transactions/set-category", Tier: TierWrite, DryRunnable: true, Composed: true, Summary: "Re-categorise transactions chosen by ids[] or by filter{} (the GET /transactions filter language).", Handler: txnHandleSetCategory},
		{Method: "POST", Path: "/transactions/set-account", Tier: TierWrite, DryRunnable: true, Composed: true, Summary: "Move transactions chosen by ids[] or filter{} to another account of the same currency.", Handler: txnHandleSetAccount},
		{Method: "POST", Path: "/transactions/tags/add", Tier: TierWrite, DryRunnable: true, Composed: true, Summary: "Add tags to transactions chosen by ids[] or filter{}.", Handler: txnHandleTagsAdd},
		{Method: "POST", Path: "/transactions/tags/remove", Tier: TierWrite, DryRunnable: true, Composed: true, Summary: "Remove tags from transactions chosen by ids[] or filter{}.", Handler: txnHandleTagsRemove},
		{Method: "POST", Path: "/transactions/tags/clear", Tier: TierWrite, DryRunnable: true, Composed: true, Summary: "Remove every tag from transactions chosen by ids[] or filter{}.", Handler: txnHandleTagsClear},
		{Method: "POST", Path: "/transactions/move-all", Tier: TierWrite, DryRunnable: true, Composed: true, Summary: "Move every transaction of one account to another account of the same currency.", Handler: txnHandleMoveAll},
		{Method: "DELETE", Path: "/transactions/bulk", Tier: TierAdmin, DryRunnable: true, Composed: true, Summary: "Delete transactions by ids[] (admin). Undo re-creates them with new ids.", Handler: txnHandleDeleteBulk},
		{Method: "DELETE", Path: "/transactions/:id", Tier: TierAdmin, DryRunnable: true, Summary: "Delete one transaction (admin). Undo re-creates it with a new id.", Handler: txnHandleDeleteOne},
	}
}

// ─── type names ────────────────────────────────────────────────────────────────────────────────

var txnTypeByName = map[string]models.TransactionType{
	"balance_modification": models.TRANSACTION_TYPE_MODIFY_BALANCE,
	"income":               models.TRANSACTION_TYPE_INCOME,
	"expense":              models.TRANSACTION_TYPE_EXPENSE,
	"transfer":             models.TRANSACTION_TYPE_TRANSFER,
}

// txnTypeName renders upstream's API transaction type as the plane's name
func txnTypeName(t int64) string {
	switch models.TransactionType(t) {
	case models.TRANSACTION_TYPE_MODIFY_BALANCE:
		return "balance_modification"
	case models.TRANSACTION_TYPE_INCOME:
		return "income"
	case models.TRANSACTION_TYPE_EXPENSE:
		return "expense"
	case models.TRANSACTION_TYPE_TRANSFER:
		return "transfer"
	}

	return "unknown"
}

// txnParseType parses a type name; empty means "any"
func txnParseType(v string) (models.TransactionType, error) {
	v = strings.ToLower(strings.TrimSpace(v))

	if v == "" {
		return 0, nil
	}

	if t, ok := txnTypeByName[v]; ok {
		return t, nil
	}

	return 0, Invalid("type is one of income, expense, transfer, balance_modification", "unknown transaction type %q", v)
}

// txnCategoryTypeFor is the category type a transaction type takes (0 for balance modification)
func txnCategoryTypeFor(t models.TransactionType) models.TransactionCategoryType {
	switch t {
	case models.TRANSACTION_TYPE_INCOME:
		return models.CATEGORY_TYPE_INCOME
	case models.TRANSACTION_TYPE_EXPENSE:
		return models.CATEGORY_TYPE_EXPENSE
	case models.TRANSACTION_TYPE_TRANSFER:
		return models.CATEGORY_TYPE_TRANSFER
	}

	return 0
}

func txnCategoryTypeName(t models.TransactionCategoryType) string {
	switch t {
	case models.CATEGORY_TYPE_INCOME:
		return "income"
	case models.CATEGORY_TYPE_EXPENSE:
		return "expense"
	case models.CATEGORY_TYPE_TRANSFER:
		return "transfer"
	}

	return "unknown"
}

// ─── the lookup: accounts, categories, tags of the bound user, loaded once per request ─────────

type txnLookup struct {
	accounts   []*models.Account
	accountMap map[int64]*models.Account
	categories []*models.TransactionCategory
	catMap     map[int64]*models.TransactionCategory
	tags       []*models.TransactionTag
	tagMap     map[int64]*models.TransactionTag
}

func txnLoadLookup(mc *Ctx) (*txnLookup, error) {
	accounts, err := services.Accounts.GetAllAccountsByUid(mc.Web, mc.Uid)

	if err != nil {
		return nil, err
	}

	categories, err := services.TransactionCategories.GetAllCategoriesByUid(mc.Web, mc.Uid, 0, -1)

	if err != nil {
		return nil, err
	}

	tags, err := services.TransactionTags.GetAllTagsByUid(mc.Web, mc.Uid)

	if err != nil {
		return nil, err
	}

	return txnNewLookup(accounts, categories, tags), nil
}

func txnNewLookup(accounts []*models.Account, categories []*models.TransactionCategory, tags []*models.TransactionTag) *txnLookup {
	lk := &txnLookup{
		accounts:   accounts,
		accountMap: make(map[int64]*models.Account, len(accounts)),
		categories: categories,
		catMap:     make(map[int64]*models.TransactionCategory, len(categories)),
		tags:       tags,
		tagMap:     make(map[int64]*models.TransactionTag, len(tags)),
	}

	for _, a := range accounts {
		lk.accountMap[a.AccountId] = a
	}

	for _, c := range categories {
		lk.catMap[c.CategoryId] = c
	}

	for _, t := range tags {
		lk.tagMap[t.TagId] = t
	}

	return lk
}

func (lk *txnLookup) accountName(id int64) string {
	if a := lk.accountMap[id]; a != nil {
		return a.Name
	}

	return ""
}

func (lk *txnLookup) accountCurrency(id int64) string {
	if a := lk.accountMap[id]; a != nil {
		return a.Currency
	}

	return ""
}

// categoryPath is "Parent > Child" for a sub-category, the name for a primary
func (lk *txnLookup) categoryPath(id int64) string {
	c := lk.catMap[id]

	if c == nil {
		return ""
	}

	if p := lk.catMap[c.ParentCategoryId]; p != nil && c.ParentCategoryId != models.LevelOneTransactionCategoryParentId {
		return p.Name + " > " + c.Name
	}

	return c.Name
}

func (lk *txnLookup) tagNames(ids []string) []string {
	out := make([]string, 0, len(ids))

	for _, s := range ids {
		id, _ := strconv.ParseInt(s, 10, 64)

		if t := lk.tagMap[id]; t != nil {
			out = append(out, t.Name)
		} else {
			out = append(out, s)
		}
	}

	return out
}

// childrenOf lists the sub-accounts of a parent account
func (lk *txnLookup) childrenOf(parentId int64) []*models.Account {
	var out []*models.Account

	for _, a := range lk.accounts {
		if a.ParentAccountId == parentId && parentId != models.LevelOneAccountParentId {
			out = append(out, a)
		}
	}

	return out
}

// txnNamed is one candidate for name resolution
type txnNamed struct {
	Id   int64
	Name string
	// Alt is an alternative full name (e.g. "Parent > Child") matched like Name
	Alt string
}

// txnResolveName resolves a name exact-match-first, then case-insensitively; ambiguity is an error
// carrying the candidates (apis.mdx §17.5)
func txnResolveName(kind, argName, name string, candidates []txnNamed, listHint string) (int64, error) {
	name = strings.TrimSpace(name)

	if name == "" {
		return 0, Invalid("pass "+argName, "%s is empty", argName)
	}

	pick := func(match func(c txnNamed) bool) []txnNamed {
		var out []txnNamed
		seen := map[int64]bool{}

		for _, c := range candidates {
			if match(c) && !seen[c.Id] {
				out = append(out, c)
				seen[c.Id] = true
			}
		}

		return out
	}

	exact := pick(func(c txnNamed) bool { return c.Name == name || (c.Alt != "" && c.Alt == name) })

	if len(exact) == 0 {
		exact = pick(func(c txnNamed) bool {
			return strings.EqualFold(c.Name, name) || (c.Alt != "" && strings.EqualFold(c.Alt, name))
		})
	}

	switch len(exact) {
	case 1:
		return exact[0].Id, nil
	case 0:
		return 0, NotFound(listHint+" — names match exactly first, then ignoring case", "no %s named %q", kind, name)
	}

	cands := make([]map[string]string, 0, len(exact))

	for _, c := range exact {
		label := c.Name

		if c.Alt != "" {
			label = c.Alt
		}

		cands = append(cands, map[string]string{"id": idString(c.Id), "name": label})
	}

	return 0, Invalid("pass the id instead of the name (the candidates are in details)", "%d %ss are named %q", len(exact), kind, name).WithDetails(map[string]any{"candidates": cands})
}

// accountCandidates lists accounts for name resolution; leafOnly drops parent accounts
func (lk *txnLookup) accountCandidates(leafOnly bool) []txnNamed {
	out := make([]txnNamed, 0, len(lk.accounts))

	for _, a := range lk.accounts {
		if leafOnly && a.Type == models.ACCOUNT_TYPE_MULTI_SUB_ACCOUNTS {
			continue
		}

		n := txnNamed{Id: a.AccountId, Name: a.Name}

		if p := lk.accountMap[a.ParentAccountId]; p != nil && a.ParentAccountId != models.LevelOneAccountParentId {
			n.Alt = p.Name + " > " + a.Name
		}

		out = append(out, n)
	}

	return out
}

// categoryCandidates lists categories; subOnly drops primaries; ctype 0 means every type
func (lk *txnLookup) categoryCandidates(subOnly bool, ctype models.TransactionCategoryType) []txnNamed {
	out := make([]txnNamed, 0, len(lk.categories))

	for _, c := range lk.categories {
		if subOnly && c.ParentCategoryId == models.LevelOneTransactionCategoryParentId {
			continue
		}

		if ctype != 0 && c.Type != ctype {
			continue
		}

		out = append(out, txnNamed{Id: c.CategoryId, Name: c.Name, Alt: lk.categoryPath(c.CategoryId)})
	}

	return out
}

func (lk *txnLookup) tagCandidates() []txnNamed {
	out := make([]txnNamed, 0, len(lk.tags))

	for _, t := range lk.tags {
		out = append(out, txnNamed{Id: t.TagId, Name: t.Name})
	}

	return out
}

// resolveAccount resolves an id or a name to an account. leaf refuses a parent account, naming its
// sub-accounts (apis.mdx §9.5).
func (lk *txnLookup) resolveAccount(idArg, id, nameArg, name string, leaf bool) (*models.Account, error) {
	var accountId int64
	var err error

	switch {
	case strings.TrimSpace(id) != "":
		accountId, err = ResolveId(idArg, id)

		if err != nil {
			return nil, err
		}
	case strings.TrimSpace(name) != "":
		accountId, err = txnResolveName("account", nameArg, name, lk.accountCandidates(false), "GET /machine/v1/accounts lists the accounts")

		if err != nil {
			return nil, err
		}
	default:
		return nil, Invalid("pass "+idArg+" (or "+nameArg+")", "%s is required", idArg)
	}

	a := lk.accountMap[accountId]

	if a == nil {
		return nil, NotFound("GET /machine/v1/accounts — or pass "+nameArg+" instead of "+idArg, "no account with id %s", idString(accountId)).WithDetails(map[string]any{"id": idString(accountId)})
	}

	if leaf && a.Type == models.ACCOUNT_TYPE_MULTI_SUB_ACCOUNTS {
		subs := lk.childrenOf(a.AccountId)
		names := make([]map[string]string, 0, len(subs))

		for _, s := range subs {
			names = append(names, map[string]string{"id": idString(s.AccountId), "name": s.Name, "currency": s.Currency})
		}

		return nil, Invalid("use one of its sub-accounts (listed in details)", "account %q is a parent account and holds no transactions of its own", a.Name).WithDetails(map[string]any{"subAccounts": names})
	}

	return a, nil
}

// resolveCategory resolves an id or name to a sub-category of the type the transaction takes
func (lk *txnLookup) resolveCategory(id, name string, ttype models.TransactionType) (*models.TransactionCategory, error) {
	want := txnCategoryTypeFor(ttype)
	var categoryId int64
	var err error

	switch {
	case strings.TrimSpace(id) != "":
		categoryId, err = ResolveId("category_id", id)

		if err != nil {
			return nil, err
		}
	case strings.TrimSpace(name) != "":
		categoryId, err = txnResolveName("category", "category_name", name, lk.categoryCandidates(true, want), "GET /machine/v1/categories?type="+txnCategoryTypeName(want)+" lists them; \"Parent > Child\" disambiguates")

		if err != nil {
			return nil, err
		}
	default:
		return nil, Invalid("pass category_id or category_name (a "+txnCategoryTypeName(want)+" sub-category)", "a %s transaction needs a category", txnTypeName(int64(ttype)))
	}

	c := lk.catMap[categoryId]

	if c == nil {
		return nil, NotFound("GET /machine/v1/categories lists them", "no category with id %s", idString(categoryId))
	}

	if c.ParentCategoryId == models.LevelOneTransactionCategoryParentId {
		return nil, Invalid("pick one of its sub-categories; transactions take a secondary category", "category %q is a primary category", c.Name)
	}

	if want != 0 && c.Type != want {
		return nil, Invalid("a "+txnTypeName(int64(ttype))+" transaction takes a "+txnCategoryTypeName(want)+" category", "category %q is a %s category", lk.categoryPath(c.CategoryId), txnCategoryTypeName(c.Type))
	}

	return c, nil
}

// resolveTags resolves tag ids and names to a de-duplicated id list, in order
func (lk *txnLookup) resolveTags(ids, names []string) ([]string, error) {
	var out []string
	seen := map[int64]bool{}

	add := func(id int64) {
		if !seen[id] {
			seen[id] = true
			out = append(out, idString(id))
		}
	}

	for _, s := range ids {
		id, err := ResolveId("tag_ids", s)

		if err != nil {
			return nil, err
		}

		if lk.tagMap[id] == nil {
			return nil, NotFound("GET /machine/v1/tags lists them — or pass tag_names", "no tag with id %s", s)
		}

		add(id)
	}

	for _, n := range names {
		id, err := txnResolveName("tag", "tag_names", n, lk.tagCandidates(), "GET /machine/v1/tags lists them")

		if err != nil {
			return nil, err
		}

		add(id)
	}

	if out == nil {
		out = []string{}
	}

	return out, nil
}

// ─── the filter language (one language for GET and for filter{} on bulk writes) ────────────────

// txnFilterArgs is the transaction filter, shared by GET /transactions's query string and the
// filter{} object of every bulk write (apis.mdx §10.3: "one filter language")
type txnFilterArgs struct {
	AccountIds    []string    `json:"account_ids,omitempty"`
	AccountNames  []string    `json:"account_names,omitempty"`
	Start         string      `json:"start,omitempty"`
	End           string      `json:"end,omitempty"`
	Type          string      `json:"type,omitempty"`
	CategoryIds   []string    `json:"category_ids,omitempty"`
	CategoryNames []string    `json:"category_names,omitempty"`
	TagIds        []string    `json:"tag_ids,omitempty"`
	TagNames      []string    `json:"tag_names,omitempty"`
	TagFilter     string      `json:"tag_filter,omitempty"`
	Untagged      bool        `json:"untagged,omitempty"`
	Keyword       string      `json:"keyword,omitempty"`
	CaseSensitive bool        `json:"case_sensitive,omitempty"`
	MinAmount     json.Number `json:"min_amount,omitempty"`
	MaxAmount     json.Number `json:"max_amount,omitempty"`
	Currency      string      `json:"currency,omitempty"`
	WithPictures  bool        `json:"with_pictures,omitempty"`
}

// isEmpty reports a filter that would select every transaction
func (f *txnFilterArgs) isEmpty() bool {
	return len(f.AccountIds) == 0 && len(f.AccountNames) == 0 && f.Start == "" && f.End == "" && f.Type == "" &&
		len(f.CategoryIds) == 0 && len(f.CategoryNames) == 0 && len(f.TagIds) == 0 && len(f.TagNames) == 0 &&
		f.TagFilter == "" && !f.Untagged && f.Keyword == "" && f.MinAmount == "" && f.MaxAmount == "" &&
		f.Currency == "" && !f.WithPictures
}

// txnFilterFromQuery reads the filter from a GET query string
func txnFilterFromQuery(mc *Ctx) (*txnFilterArgs, error) {
	f := &txnFilterArgs{
		AccountIds:    mc.QueryList("account_ids"),
		AccountNames:  mc.QueryList("account_names"),
		Start:         mc.Query("start"),
		End:           mc.Query("end"),
		Type:          mc.Query("type"),
		CategoryIds:   mc.QueryList("category_ids"),
		CategoryNames: mc.QueryList("category_names"),
		TagIds:        mc.QueryList("tag_ids"),
		TagNames:      mc.QueryList("tag_names"),
		TagFilter:     mc.Query("tag_filter"),
		Keyword:       mc.Query("keyword"),
		MinAmount:     json.Number(mc.Query("min_amount")),
		MaxAmount:     json.Number(mc.Query("max_amount")),
		Currency:      mc.Query("currency"),
	}

	// singular spellings are accepted too (account_id=…&category_id=…)
	f.AccountIds = append(f.AccountIds, mc.QueryList("account_id")...)
	f.AccountNames = append(f.AccountNames, mc.QueryList("account_name")...)
	f.CategoryIds = append(f.CategoryIds, mc.QueryList("category_id")...)
	f.CategoryNames = append(f.CategoryNames, mc.QueryList("category_name")...)

	var err error

	if f.Untagged, err = mc.QueryBool("untagged", false); err != nil {
		return nil, err
	}

	if f.CaseSensitive, err = mc.QueryBool("case_sensitive", false); err != nil {
		return nil, err
	}

	if f.WithPictures, err = mc.QueryBool("with_pictures", false); err != nil {
		return nil, err
	}

	return f, nil
}

// txnResolvedFilter is a filter resolved to upstream's arguments
type txnResolvedFilter struct {
	Type             models.TransactionType
	AccountIds       []int64 // nil = every account
	CategoryIds      []int64 // nil = every category (primaries are expanded by upstream)
	TagFilter        string  // upstream syntax; "none" = untagged
	AmountFilter     string  // upstream syntax (gt/lt/bt)
	Keyword          string
	MatchMode        core.MatchMode
	MustHavePictures bool
	StartUnix        int64 // 0 = open
	EndUnix          int64 // 0 = open (inclusive end of the end day)
	// Empty is true when the filter provably selects nothing (e.g. no account in that currency)
	Empty bool
	// Echo is what the plane resolved, returned to the caller
	Echo map[string]any
}

// txnAmountFilter builds upstream's amount_filter from inclusive min/max hundredths
func txnAmountFilter(min, max *int64) (string, error) {
	switch {
	case min != nil && max != nil:
		if *max < *min {
			return "", Invalid("max_amount must be at least min_amount", "max_amount %d is below min_amount %d", *max, *min)
		}

		return fmt.Sprintf("bt:%d:%d", *min, *max), nil
	case min != nil:
		return fmt.Sprintf("gt:%d", *min-1), nil
	case max != nil:
		return fmt.Sprintf("lt:%d", *max+1), nil
	}

	return "", nil
}

// txnBuildTagFilter combines the "has any of these tags" ids with a raw upstream tag_filter
func txnBuildTagFilter(anyTagIds []string, raw string, untagged bool) (string, error) {
	raw = strings.TrimSpace(raw)

	if untagged {
		if len(anyTagIds) > 0 || (raw != "" && raw != models.TransactionNoTagFilterValue) {
			return "", Invalid("pass either untagged or a tag filter, not both", "untagged contradicts a tag filter")
		}

		return models.TransactionNoTagFilterValue, nil
	}

	if raw == models.TransactionNoTagFilterValue {
		if len(anyTagIds) > 0 {
			return "", Invalid("pass either untagged or a tag filter, not both", "tag_filter=none contradicts tag_ids")
		}

		return raw, nil
	}

	if raw != "" {
		if _, err := models.ParseTransactionTagFilter(raw); err != nil {
			return "", Invalid("tag_filter is upstream's syntax: <mode>:<id>,<id>[;…] with mode 0 has-any, 1 has-all, 2 not-any, 3 not-all — or use tag_ids / tag_names", "tag_filter %q is malformed", raw)
		}
	}

	parts := []string{}

	if len(anyTagIds) > 0 {
		parts = append(parts, "0:"+strings.Join(anyTagIds, ","))
	}

	if raw != "" {
		parts = append(parts, raw)
	}

	return strings.Join(parts, ";"), nil
}

// txnDistinctCurrencies lists the currencies of the given leaf accounts (all leaf accounts when ids is nil)
func txnDistinctCurrencies(lk *txnLookup, ids []int64) []string {
	set := map[string]bool{}

	consider := func(a *models.Account) {
		if a != nil && a.Type != models.ACCOUNT_TYPE_MULTI_SUB_ACCOUNTS && a.Currency != "" {
			set[a.Currency] = true
		}
	}

	if ids == nil {
		for _, a := range lk.accounts {
			consider(a)
		}
	} else {
		for _, id := range ids {
			consider(lk.accountMap[id])
		}
	}

	out := make([]string, 0, len(set))

	for c := range set {
		out = append(out, c)
	}

	sort.Strings(out)

	return out
}

// txnResolveFilter resolves a filter against the bound user's accounts, categories and tags
func txnResolveFilter(mc *Ctx, lk *txnLookup, f *txnFilterArgs) (*txnResolvedFilter, error) {
	rf := &txnResolvedFilter{Echo: map[string]any{}, MatchMode: core.MATCH_MODE_IGNORE_CASE}

	t, err := txnParseType(f.Type)

	if err != nil {
		return nil, err
	}

	rf.Type = t

	if t != 0 {
		rf.Echo["type"] = txnTypeName(int64(t))
	}

	// accounts: ids and names, parents expanded to their sub-accounts
	var accountIds []int64
	seen := map[int64]bool{}
	addAccount := func(a *models.Account) {
		if a.Type == models.ACCOUNT_TYPE_MULTI_SUB_ACCOUNTS {
			for _, s := range lk.childrenOf(a.AccountId) {
				if !seen[s.AccountId] {
					seen[s.AccountId] = true
					accountIds = append(accountIds, s.AccountId)
				}
			}

			return
		}

		if !seen[a.AccountId] {
			seen[a.AccountId] = true
			accountIds = append(accountIds, a.AccountId)
		}
	}

	for _, s := range f.AccountIds {
		a, err := lk.resolveAccount("account_ids", s, "account_names", "", false)

		if err != nil {
			return nil, err
		}

		addAccount(a)
	}

	for _, n := range f.AccountNames {
		a, err := lk.resolveAccount("account_ids", "", "account_names", n, false)

		if err != nil {
			return nil, err
		}

		addAccount(a)
	}

	accountsGiven := len(f.AccountIds)+len(f.AccountNames) > 0

	if accountsGiven {
		rf.AccountIds = accountIds

		if len(accountIds) == 0 {
			rf.Empty = true
		}
	}

	// currency narrows to the accounts in that currency
	if cur := strings.ToUpper(strings.TrimSpace(f.Currency)); cur != "" {
		if len(cur) != 3 {
			return nil, Invalid("currency is an ISO 4217 code such as USD", "currency %q is not a 3-letter code", f.Currency)
		}

		var narrowed []int64

		base := rf.AccountIds

		if !accountsGiven {
			for _, a := range lk.accounts {
				if a.Type != models.ACCOUNT_TYPE_MULTI_SUB_ACCOUNTS {
					base = append(base, a.AccountId)
				}
			}
		}

		for _, id := range base {
			if a := lk.accountMap[id]; a != nil && a.Currency == cur {
				narrowed = append(narrowed, id)
			}
		}

		rf.AccountIds = narrowed

		if len(narrowed) == 0 {
			rf.Empty = true
		}

		rf.Echo["currency"] = cur
	}

	if rf.AccountIds != nil {
		ids := make([]string, 0, len(rf.AccountIds))

		for _, id := range rf.AccountIds {
			ids = append(ids, idString(id))
		}

		rf.Echo["accountIds"] = ids
	}

	// categories
	catSeen := map[int64]bool{}

	for _, s := range f.CategoryIds {
		id, err := ResolveId("category_ids", s)

		if err != nil {
			return nil, err
		}

		if lk.catMap[id] == nil {
			return nil, NotFound("GET /machine/v1/categories lists them — or pass category_names", "no category with id %s", s)
		}

		if !catSeen[id] {
			catSeen[id] = true
			rf.CategoryIds = append(rf.CategoryIds, id)
		}
	}

	for _, n := range f.CategoryNames {
		id, err := txnResolveName("category", "category_names", n, lk.categoryCandidates(false, 0), "GET /machine/v1/categories lists them; \"Parent > Child\" disambiguates")

		if err != nil {
			return nil, err
		}

		if !catSeen[id] {
			catSeen[id] = true
			rf.CategoryIds = append(rf.CategoryIds, id)
		}
	}

	if len(rf.CategoryIds) > 0 {
		ids := make([]string, 0, len(rf.CategoryIds))

		for _, id := range rf.CategoryIds {
			ids = append(ids, idString(id))
		}

		rf.Echo["categoryIds"] = ids
	}

	// tags
	anyTags, err := lk.resolveTags(f.TagIds, f.TagNames)

	if err != nil {
		return nil, err
	}

	if rf.TagFilter, err = txnBuildTagFilter(anyTags, f.TagFilter, f.Untagged); err != nil {
		return nil, err
	}

	if rf.TagFilter != "" {
		rf.Echo["tagFilter"] = rf.TagFilter
	}

	// dates
	if f.Start != "" {
		s, err := ParseDate("start", f.Start, mc.Loc)

		if err != nil {
			return nil, err
		}

		rf.StartUnix = s.Unix()
		rf.Echo["start"] = s.Format("2006-01-02")
	}

	if f.End != "" {
		e, err := ParseDate("end", f.End, mc.Loc)

		if err != nil {
			return nil, err
		}

		rf.EndUnix = e.AddDate(0, 0, 1).Add(-time.Second).Unix()
		rf.Echo["end"] = e.Format("2006-01-02")
	}

	if rf.StartUnix != 0 && rf.EndUnix != 0 && rf.EndUnix < rf.StartUnix {
		return nil, Invalid("end must be on or after start", "end %s is before start %s", f.End, f.Start)
	}

	if rf.StartUnix != 0 || rf.EndUnix != 0 {
		rf.Echo["timezone"] = mc.Loc.String()
		rf.Echo["startUnix"] = rf.StartUnix
		rf.Echo["endUnix"] = rf.EndUnix
	}

	// amounts — in each transaction's own currency, so never across currencies (R11)
	var minP, maxP *int64

	if f.MinAmount != "" {
		v, err := AmountArg("min_amount", f.MinAmount)

		if err != nil {
			return nil, err
		}

		minP = &v
	}

	if f.MaxAmount != "" {
		v, err := AmountArg("max_amount", f.MaxAmount)

		if err != nil {
			return nil, err
		}

		maxP = &v
	}

	if minP != nil || maxP != nil {
		if strings.TrimSpace(f.Currency) == "" {
			if curs := txnDistinctCurrencies(lk, rf.AccountIds); len(curs) > 1 {
				return nil, Invalid("amounts apply in each transaction's own currency; pass currency (e.g. \"currency\": \""+curs[0]+"\") to narrow to one", "min_amount/max_amount across %d currencies (%s) compares unlike amounts", len(curs), strings.Join(curs, ", ")).WithDetails(map[string]any{"currencies": curs})
			}
		}

		if rf.AmountFilter, err = txnAmountFilter(minP, maxP); err != nil {
			return nil, err
		}

		if minP != nil {
			rf.Echo["minAmount"] = *minP
		}

		if maxP != nil {
			rf.Echo["maxAmount"] = *maxP
		}
	}

	if kw := strings.TrimSpace(f.Keyword); kw != "" {
		rf.Keyword = kw
		rf.Echo["keyword"] = kw

		if f.CaseSensitive {
			rf.MatchMode = core.MATCH_MODE_DEFAULT
		}
	}

	if f.WithPictures {
		if !mc.Config.EnableTransactionPictures {
			return nil, NewFail(CodeForbidden, "switch on [transaction] enable_transaction_pictures in conf/ezbookkeeping.ini and restart", "transaction pictures are switched off upstream")
		}

		rf.MustHavePictures = true
		rf.Echo["withPictures"] = true
	}

	return rf, nil
}

func txnJoinIds(ids []int64) string {
	parts := make([]string, 0, len(ids))

	for _, id := range ids {
		parts = append(parts, idString(id))
	}

	return strings.Join(parts, ",")
}

// upstreamListQuery is the query string for transactions/list.json and count.json
func (rf *txnResolvedFilter) upstreamListQuery() url.Values {
	q := url.Values{}

	if rf.Type != 0 {
		q.Set("type", strconv.Itoa(int(rf.Type)))
	}

	if len(rf.AccountIds) > 0 {
		q.Set("account_ids", txnJoinIds(rf.AccountIds))
	}

	if len(rf.CategoryIds) > 0 {
		q.Set("category_ids", txnJoinIds(rf.CategoryIds))
	}

	if rf.TagFilter != "" {
		q.Set("tag_filter", rf.TagFilter)
	}

	if rf.AmountFilter != "" {
		q.Set("amount_filter", rf.AmountFilter)
	}

	if rf.Keyword != "" {
		q.Set("keyword", rf.Keyword)
		q.Set("match_mode", strconv.Itoa(int(rf.MatchMode)))
	}

	if rf.MustHavePictures {
		q.Set("must_have_pictures", "true")
	}

	if rf.StartUnix != 0 {
		q.Set("min_time", strconv.FormatInt(utils.GetMinTransactionTimeFromUnixTime(rf.StartUnix), 10))
	}

	if rf.EndUnix != 0 {
		q.Set("max_time", strconv.FormatInt(utils.GetMaxTransactionTimeFromUnixTime(rf.EndUnix), 10))
	}

	return q
}

// ─── reading ────────────────────────────────────────────────────────────────────────────────────

// txnListPage is upstream's TransactionInfoPageWrapperResponse, read generically so every upstream
// field passes through in upstream's casing (apis.mdx §7.1a)
type txnListPage struct {
	Items              []map[string]any `json:"items"`
	NextTimeSequenceId any              `json:"nextTimeSequenceId"`
}

func txnAsInt64(v any) (int64, bool) {
	switch t := v.(type) {
	case json.Number:
		n, err := t.Int64()
		return n, err == nil
	case string:
		n, err := strconv.ParseInt(t, 10, 64)
		return n, err == nil
	case int64:
		return t, true
	case int:
		return int64(t), true
	}

	return 0, false
}

func txnAsString(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case json.Number:
		return t.String()
	case nil:
		return ""
	}

	return fmt.Sprint(v)
}

// txnDecorate adds the plane's fields beside upstream's: date (YYYY-MM-DD in the call's zone),
// typeName, and each side's currency, so no amount travels without its currency (R11)
func txnDecorate(row map[string]any, lk *txnLookup, loc *time.Location) map[string]any {
	if row == nil {
		return nil
	}

	if sec, ok := txnAsInt64(row["time"]); ok {
		row["date"] = DateOfUnix(sec, loc)
	}

	if t, ok := txnAsInt64(row["type"]); ok {
		row["typeName"] = txnTypeName(t)
	}

	currencyOf := func(objKey, idKey string) string {
		if obj, ok := row[objKey].(map[string]any); ok {
			if c := txnAsString(obj["currency"]); c != "" {
				return c
			}
		}

		if lk != nil {
			if id, ok := txnAsInt64(row[idKey]); ok {
				return lk.accountCurrency(id)
			}
		}

		return ""
	}

	row["sourceCurrency"] = currencyOf("sourceAccount", "sourceAccountId")

	if t, _ := txnAsInt64(row["type"]); models.TransactionType(t) == models.TRANSACTION_TYPE_TRANSFER {
		row["destinationCurrency"] = currencyOf("destinationAccount", "destinationAccountId")
	}

	return row
}

// txnWalk walks upstream's cursor list up to limit rows starting below cursor (0 = newest)
func txnWalk(mc *Ctx, rf *txnResolvedFilter, limit int, cursor int64, withPictures, trim bool) ([]map[string]any, int64, error) {
	var rows []map[string]any
	base := rf.upstreamListQuery()
	maxTime := int64(0)

	if v := base.Get("max_time"); v != "" {
		maxTime, _ = strconv.ParseInt(v, 10, 64)
	}

	if cursor > 0 && (maxTime == 0 || cursor < maxTime) {
		maxTime = cursor
	}

	next := int64(0)

	for len(rows) < limit {
		count := limit - len(rows)

		if count > txnUpstreamPage {
			count = txnUpstreamPage
		}

		q := url.Values{}

		for k, v := range base {
			q[k] = v
		}

		q.Set("count", strconv.Itoa(count))

		if maxTime > 0 {
			q.Set("max_time", strconv.FormatInt(maxTime, 10))
		}

		if withPictures {
			q.Set("with_pictures", "true")
		}

		if trim {
			q.Set("trim_account", "true")
			q.Set("trim_category", "true")
			q.Set("trim_tag", "true")
		}

		var page txnListPage

		if err := mc.CallUpstreamInto(api.Transactions.TransactionListHandler, "GET", q, nil, &page); err != nil {
			return nil, 0, err
		}

		rows = append(rows, page.Items...)
		n, ok := txnAsInt64(page.NextTimeSequenceId)

		if !ok || n <= 0 {
			next = 0
			break
		}

		if maxTime > 0 && n >= maxTime && len(page.Items) == 0 {
			// defensive: a cursor that does not move would loop forever
			next = 0
			break
		}

		next = n
		maxTime = n
	}

	if len(rows) > limit {
		rows = rows[:limit]
	}

	return rows, next, nil
}

// txnCount asks upstream's count handler (a transfer counts once)
func txnCount(mc *Ctx, rf *txnResolvedFilter) (int64, error) {
	if rf.Empty {
		return 0, nil
	}

	var resp struct {
		TotalCount json.Number `json:"totalCount"`
	}

	if err := mc.CallUpstreamInto(api.Transactions.TransactionCountHandler, "GET", rf.upstreamListQuery(), nil, &resp); err != nil {
		return 0, err
	}

	n, _ := resp.TotalCount.Int64()

	return n, nil
}

func txnHandleList(mc *Ctx) (any, error) {
	f, err := txnFilterFromQuery(mc)

	if err != nil {
		return nil, err
	}

	limit64, err := mc.QueryInt("limit", DefaultLimit)

	if err != nil {
		return nil, err
	}

	limit := int(limit64)
	clamped := false

	if limit <= 0 {
		limit = DefaultLimit
	}

	if limit > MaxLimit {
		limit = MaxLimit
		clamped = true
	}

	var cursor int64

	if c := mc.Query("cursor"); c != "" {
		cursor, err = strconv.ParseInt(c, 10, 64)

		if err != nil || cursor <= 0 {
			return nil, Invalid("pass back the nextCursor a previous page returned", "cursor %q is not a cursor this route issued", c)
		}
	}

	order := mc.Query("order")

	switch order {
	case "", "-time", "-date":
		order = "-time"
	case "time", "date":
		order = "time"

		if cursor > 0 {
			return nil, Invalid("cursor pages newest-first; drop cursor, or narrow with start/end", "cursor cannot be combined with order=time")
		}
	default:
		return nil, Invalid("order is -time (newest first, the default) or time (oldest first)", "unknown order %q", order)
	}

	lk, err := txnLoadLookup(mc)

	if err != nil {
		return nil, err
	}

	rf, err := txnResolveFilter(mc, lk, f)

	if err != nil {
		return nil, err
	}

	out := map[string]any{"filter": rf.Echo, "order": order, "nextCursor": nil}

	if rf.Empty {
		out["transactions"] = []any{}
		out["count"] = 0

		return out, nil
	}

	if order == "time" {
		total, err := txnCount(mc, rf)

		if err != nil {
			return nil, err
		}

		if total > int64(limit) {
			return nil, Invalid("oldest-first needs the whole selection; narrow with start/end, or raise limit (cap 5000)", "%d transactions match, more than limit %d, so the oldest cannot be returned in one page", total, limit).WithDetails(map[string]any{"matching": total, "limit": limit})
		}
	}

	rows, next, err := txnWalk(mc, rf, limit, cursor, f.WithPictures, false)

	if err != nil {
		return nil, err
	}

	for _, r := range rows {
		txnDecorate(r, lk, mc.Loc)
	}

	if order == "time" {
		for i, j := 0, len(rows)-1; i < j; i, j = i+1, j-1 {
			rows[i], rows[j] = rows[j], rows[i]
		}
	}

	if rows == nil {
		rows = []map[string]any{}
	}

	out["transactions"] = rows
	out["count"] = len(rows)

	if next > 0 {
		out["nextCursor"] = strconv.FormatInt(next, 10)
		mc.Truncated(limit)
	} else if clamped {
		mc.SetMeta("limitApplied", limit)
	}

	return out, nil
}

func txnHandleCount(mc *Ctx) (any, error) {
	f, err := txnFilterFromQuery(mc)

	if err != nil {
		return nil, err
	}

	lk, err := txnLoadLookup(mc)

	if err != nil {
		return nil, err
	}

	rf, err := txnResolveFilter(mc, lk, f)

	if err != nil {
		return nil, err
	}

	n, err := txnCount(mc, rf)

	if err != nil {
		return nil, err
	}

	return map[string]any{"count": n, "filter": rf.Echo}, nil
}

// txnGetRow reads one transaction through upstream's get handler and decorates it
func txnGetRow(mc *Ctx, lk *txnLookup, id int64) (map[string]any, error) {
	q := url.Values{"id": {idString(id)}}

	if mc.Config.EnableTransactionPictures {
		q.Set("with_pictures", "true")
	}

	var row map[string]any

	err := mc.CallUpstreamInto(api.Transactions.TransactionGetHandler, "GET", q, nil, &row)

	// upstream's get handler cannot load a balance modification (it has no category, id 0):
	// ask again without the category and name it from the lookup instead
	if err != nil && integIsCategoryIdInvalid(err) {
		q.Set("trim_category", "true")
		row = nil
		err = mc.CallUpstreamInto(api.Transactions.TransactionGetHandler, "GET", q, nil, &row)

		if err == nil {
			integNameRow(row, lk)
		}
	}

	if err != nil {
		if f := toFail(err); f.Code == CodeNotFound {
			return nil, NotFound("GET /machine/v1/transactions lists them", "no transaction with id %s", idString(id)).WithDetails(map[string]any{"id": idString(id)})
		}

		return nil, err
	}

	txnDecorate(row, lk, mc.Loc)

	if t, _ := txnAsInt64(row["type"]); models.TransactionType(t) == models.TRANSACTION_TYPE_TRANSFER {
		src, _ := txnAsInt64(row["sourceAccountId"])
		dst, _ := txnAsInt64(row["destinationAccountId"])
		row["transfer"] = map[string]any{
			"source":      map[string]any{"accountId": idString(src), "accountName": lk.accountName(src), "amount": row["sourceAmount"], "currency": lk.accountCurrency(src)},
			"destination": map[string]any{"accountId": idString(dst), "accountName": lk.accountName(dst), "amount": row["destinationAmount"], "currency": lk.accountCurrency(dst)},
		}
	}

	return row, nil
}

func txnHandleGet(mc *Ctx) (any, error) {
	id, err := ResolveId("id", mc.Param("id"))

	if err != nil {
		return nil, err
	}

	lk, err := txnLoadLookup(mc)

	if err != nil {
		return nil, err
	}

	row, err := txnGetRow(mc, lk, id)

	if err != nil {
		return nil, err
	}

	return map[string]any{"transaction": row}, nil
}

// txnEdge finds the earliest (asc) or latest transaction, of one account or of all
func txnEdge(mc *Ctx, asc bool) (any, error) {
	lk, err := txnLoadLookup(mc)

	if err != nil {
		return nil, err
	}

	var accountIds []int64
	echo := map[string]any{}

	if mc.Query("account_id") != "" || mc.Query("account_name") != "" {
		a, err := lk.resolveAccount("account_id", mc.Query("account_id"), "account_name", mc.Query("account_name"), false)

		if err != nil {
			return nil, err
		}

		if a.Type == models.ACCOUNT_TYPE_MULTI_SUB_ACCOUNTS {
			for _, s := range lk.childrenOf(a.AccountId) {
				accountIds = append(accountIds, s.AccountId)
			}
		} else {
			accountIds = []int64{a.AccountId}
		}

		echo["accountId"] = idString(a.AccountId)

		if len(accountIds) == 0 {
			return map[string]any{"transaction": nil, "filter": echo}, nil
		}
	}

	sess := datastore.Container.UserDataStore.Choose(mc.Uid).NewSession(mc.Web).Where("uid=? AND deleted=?", mc.Uid, false)

	if len(accountIds) > 0 {
		sess = sess.In("account_id", accountIds)
	} else {
		sess = sess.And("type<>?", models.TRANSACTION_DB_TYPE_TRANSFER_IN)
	}

	order := "transaction_time desc"

	if asc {
		order = "transaction_time asc"
	}

	t := &models.Transaction{}
	has, err := sess.OrderBy(order).Limit(1).Get(t)

	if err != nil {
		return nil, err
	}

	if !has {
		return map[string]any{"transaction": nil, "filter": echo}, nil
	}

	id := t.TransactionId

	if t.Type == models.TRANSACTION_DB_TYPE_TRANSFER_IN {
		id = t.RelatedId
	}

	row, err := txnGetRow(mc, lk, id)

	if err != nil {
		return nil, err
	}

	return map[string]any{"transaction": row, "filter": echo}, nil
}

func txnHandleEarliest(mc *Ctx) (any, error) { return txnEdge(mc, true) }

func txnHandleLatest(mc *Ctx) (any, error) { return txnEdge(mc, false) }

func txnHandleExport(mc *Ctx) (any, error) {
	format := strings.ToLower(mc.Query("format"))

	if format == "" {
		format = "csv"
	}

	if format != "csv" && format != "tsv" {
		return nil, Invalid("format is csv or tsv", "unknown format %q", format)
	}

	f, err := txnFilterFromQuery(mc)

	if err != nil {
		return nil, err
	}

	if f.WithPictures {
		return nil, Invalid("drop with_pictures; upstream's export cannot filter by pictures", "with_pictures is not supported by the export")
	}

	lk, err := txnLoadLookup(mc)

	if err != nil {
		return nil, err
	}

	rf, err := txnResolveFilter(mc, lk, f)

	if err != nil {
		return nil, err
	}

	if rf.Empty {
		return nil, NotFound("widen the filter; no account matches it", "the filter selects no account, so there is nothing to export")
	}

	q := rf.upstreamListQuery()
	q.Del("min_time")
	q.Del("max_time")
	q.Del("must_have_pictures")

	// the export handler takes unix SECONDS (unlike the list's time-sequence ids)
	if rf.StartUnix != 0 {
		q.Set("min_time", strconv.FormatInt(rf.StartUnix, 10))
	}

	if rf.EndUnix != 0 {
		q.Set("max_time", strconv.FormatInt(rf.EndUnix, 10))
	}

	fn := api.DataManagements.ExportDataToEzbookkeepingCSVHandler
	contentType := "text/csv; charset=utf-8"

	if format == "tsv" {
		fn = api.DataManagements.ExportDataToEzbookkeepingTSVHandler
		contentType = "text/tab-separated-values; charset=utf-8"
	}

	data, fileName, err := mc.CallUpstreamData(fn, q)

	if err != nil {
		return nil, err
	}

	return &RawResult{ContentType: contentType, FileName: fileName, Data: data}, nil
}

// ─── state snapshots: what a row looks like, for previews, checks and undo ─────────────────────

// txnState is one API transaction's editable state (the TRANSFER_OUT view of a transfer). It is
// what previews diff, what journal checks compare, and what txn.restore writes back.
type txnState struct {
	Id                   string   `json:"id"`
	Type                 int      `json:"type"`
	CategoryId           string   `json:"categoryId"`
	Time                 int64    `json:"time"`
	UtcOffset            int16    `json:"utcOffset"`
	SourceAccountId      string   `json:"sourceAccountId"`
	DestinationAccountId string   `json:"destinationAccountId,omitempty"`
	SourceAmount         int64    `json:"sourceAmount"`
	DestinationAmount    int64    `json:"destinationAmount,omitempty"`
	HideAmount           bool     `json:"hideAmount"`
	Comment              string   `json:"comment"`
	TagIds               []string `json:"tagIds"`
	PictureIds           []string `json:"pictureIds"`
	GeoLatitude          string   `json:"geoLatitude,omitempty"`
	GeoLongitude         string   `json:"geoLongitude,omitempty"`
}

func txnSortedIds(ids []string) []string {
	out := append([]string{}, ids...)
	sort.Slice(out, func(i, j int) bool {
		if len(out[i]) != len(out[j]) {
			return len(out[i]) < len(out[j])
		}

		return out[i] < out[j]
	})

	return out
}

// normalized returns a copy with id lists sorted, so equality ignores order
func (s txnState) normalized() txnState {
	s.TagIds = txnSortedIds(s.TagIds)
	s.PictureIds = txnSortedIds(s.PictureIds)

	return s
}

// txnStatesEqual compares two states, ignoring the order of tags and pictures
func txnStatesEqual(a, b txnState) bool {
	return reflect.DeepEqual(a.normalized(), b.normalized())
}

func txnInt64Strings(ids []int64) []string {
	out := make([]string, 0, len(ids))

	for _, id := range ids {
		out = append(out, idString(id))
	}

	return out
}

// txnStateFromModel builds the state of an out-row (or non-transfer row)
func txnStateFromModel(t *models.Transaction, tagIds, pictureIds []int64) txnState {
	apiType, _ := t.Type.ToTransactionType()
	s := txnState{
		Id:              idString(t.TransactionId),
		Type:            int(apiType),
		CategoryId:      idString(t.CategoryId),
		Time:            utils.GetUnixTimeFromTransactionTime(t.TransactionTime),
		UtcOffset:       t.TimezoneUtcOffset,
		SourceAccountId: idString(t.AccountId),
		SourceAmount:    t.Amount,
		HideAmount:      t.HideAmount,
		Comment:         t.Comment,
		TagIds:          txnInt64Strings(tagIds),
		PictureIds:      txnInt64Strings(pictureIds),
	}

	if t.Type == models.TRANSACTION_DB_TYPE_TRANSFER_OUT {
		s.DestinationAccountId = idString(t.RelatedAccountId)
		s.DestinationAmount = t.RelatedAccountAmount
	}

	if t.GeoLatitude != 0 || t.GeoLongitude != 0 {
		s.GeoLatitude = strconv.FormatFloat(t.GeoLatitude, 'f', -1, 64)
		s.GeoLongitude = strconv.FormatFloat(t.GeoLongitude, 'f', -1, 64)
	}

	return s
}

// txnReadStates reads the current state of the given ids. A TRANSFER_IN row id is canonicalised to
// its out-row. It returns the states keyed by API id, the canonical id of each requested id, and
// the requested ids that do not exist (or are deleted).
func txnReadStates(mc *Ctx, ids []int64) (map[int64]*txnState, map[int64]int64, []int64, error) {
	states := map[int64]*txnState{}
	canon := map[int64]int64{}
	var missing []int64

	if len(ids) == 0 {
		return states, canon, missing, nil
	}

	rows := map[int64]*models.Transaction{}

	fetch := func(want []int64) error {
		for start := 0; start < len(want); start += 500 {
			end := start + 500

			if end > len(want) {
				end = len(want)
			}

			got, err := services.Transactions.GetTransactionsByTransactionIds(mc.Web, mc.Uid, want[start:end])

			if err != nil {
				return err
			}

			for _, t := range got {
				rows[t.TransactionId] = t
			}
		}

		return nil
	}

	if err := fetch(ids); err != nil {
		return nil, nil, nil, err
	}

	var needOut []int64

	for _, id := range ids {
		t, ok := rows[id]

		if !ok {
			continue
		}

		if t.Type == models.TRANSACTION_DB_TYPE_TRANSFER_IN {
			if _, have := rows[t.RelatedId]; !have {
				needOut = append(needOut, t.RelatedId)
			}
		}
	}

	if len(needOut) > 0 {
		if err := fetch(needOut); err != nil {
			return nil, nil, nil, err
		}
	}

	var apiIds []int64
	apiSeen := map[int64]bool{}

	for _, id := range ids {
		t, ok := rows[id]

		if !ok {
			missing = append(missing, id)
			continue
		}

		apiId := id

		if t.Type == models.TRANSACTION_DB_TYPE_TRANSFER_IN {
			apiId = t.RelatedId

			if _, ok := rows[apiId]; !ok {
				missing = append(missing, id)
				continue
			}
		}

		canon[id] = apiId

		if !apiSeen[apiId] {
			apiSeen[apiId] = true
			apiIds = append(apiIds, apiId)
		}
	}

	if len(apiIds) == 0 {
		return states, canon, missing, nil
	}

	tagMap, err := services.TransactionTags.GetAllTagIdsOfTransactions(mc.Web, mc.Uid, apiIds)

	if err != nil {
		return nil, nil, nil, err
	}

	pictureMap := map[int64][]int64{}

	if mc.Config.EnableTransactionPictures {
		infos, err := services.TransactionPictures.GetPictureInfosByTransactionIds(mc.Web, mc.Uid, apiIds)

		if err != nil {
			return nil, nil, nil, err
		}

		for tid, list := range infos {
			for _, p := range list {
				pictureMap[tid] = append(pictureMap[tid], p.PictureId)
			}
		}
	}

	for _, apiId := range apiIds {
		st := txnStateFromModel(rows[apiId], tagMap[apiId], pictureMap[apiId])
		states[apiId] = &st
	}

	return states, canon, missing, nil
}

// txnModifyBody is upstream's TransactionModifyRequest for a state
func txnModifyBody(s txnState) map[string]any {
	body := txnCreateBodyFromState(s)
	body["id"] = s.Id
	body["pictureIds"] = txnNonNil(s.PictureIds)

	return body
}

func txnNonNil(v []string) []string {
	if v == nil {
		return []string{}
	}

	return v
}

// txnCreateBodyFromState is upstream's TransactionCreateRequest for a state (no pictures)
func txnCreateBodyFromState(s txnState) map[string]any {
	dest := s.DestinationAccountId

	if dest == "" {
		dest = "0"
	}

	body := map[string]any{
		"type":                 s.Type,
		"categoryId":           s.CategoryId,
		"time":                 s.Time,
		"utcOffset":            s.UtcOffset,
		"sourceAccountId":      s.SourceAccountId,
		"destinationAccountId": dest,
		"sourceAmount":         s.SourceAmount,
		"destinationAmount":    s.DestinationAmount,
		"hideAmount":           s.HideAmount,
		"tagIds":               txnNonNil(s.TagIds),
		"comment":              s.Comment,
	}

	if s.GeoLatitude != "" || s.GeoLongitude != "" {
		body["geoLocation"] = map[string]any{"latitude": json.Number(s.GeoLatitude), "longitude": json.Number(s.GeoLongitude)}
	}

	return body
}

// txnFieldDiff lists the fields that differ between two states, with names for ids
func txnFieldDiff(lk *txnLookup, from, to txnState) []map[string]any {
	var out []map[string]any

	add := func(field string, a, b any, names ...string) {
		d := map[string]any{"field": field, "from": a, "to": b}

		if len(names) == 2 {
			d["fromName"] = names[0]
			d["toName"] = names[1]
		}

		out = append(out, d)
	}

	acctName := func(s string) string {
		id, _ := strconv.ParseInt(s, 10, 64)
		return lk.accountName(id)
	}

	catName := func(s string) string {
		id, _ := strconv.ParseInt(s, 10, 64)
		return lk.categoryPath(id)
	}

	if from.Type != to.Type {
		add("type", txnTypeName(int64(from.Type)), txnTypeName(int64(to.Type)))
	}

	if from.CategoryId != to.CategoryId {
		add("categoryId", from.CategoryId, to.CategoryId, catName(from.CategoryId), catName(to.CategoryId))
	}

	if from.Time != to.Time {
		add("time", from.Time, to.Time)
	}

	if from.UtcOffset != to.UtcOffset {
		add("utcOffset", from.UtcOffset, to.UtcOffset)
	}

	if from.SourceAccountId != to.SourceAccountId {
		add("sourceAccountId", from.SourceAccountId, to.SourceAccountId, acctName(from.SourceAccountId), acctName(to.SourceAccountId))
	}

	if from.DestinationAccountId != to.DestinationAccountId {
		add("destinationAccountId", from.DestinationAccountId, to.DestinationAccountId, acctName(from.DestinationAccountId), acctName(to.DestinationAccountId))
	}

	if from.SourceAmount != to.SourceAmount {
		add("sourceAmount", from.SourceAmount, to.SourceAmount)
	}

	if from.DestinationAmount != to.DestinationAmount {
		add("destinationAmount", from.DestinationAmount, to.DestinationAmount)
	}

	if from.HideAmount != to.HideAmount {
		add("hideAmount", from.HideAmount, to.HideAmount)
	}

	if from.Comment != to.Comment {
		add("comment", from.Comment, to.Comment)
	}

	if !reflect.DeepEqual(txnSortedIds(from.TagIds), txnSortedIds(to.TagIds)) {
		d := map[string]any{"field": "tagIds", "from": txnNonNil(from.TagIds), "to": txnNonNil(to.TagIds), "fromNames": lk.tagNames(from.TagIds), "toNames": lk.tagNames(to.TagIds)}
		out = append(out, d)
	}

	if from.GeoLatitude != to.GeoLatitude || from.GeoLongitude != to.GeoLongitude {
		geo := func(s txnState) any {
			if s.GeoLatitude == "" && s.GeoLongitude == "" {
				return nil
			}

			return map[string]string{"latitude": s.GeoLatitude, "longitude": s.GeoLongitude}
		}

		add("geoLocation", geo(from), geo(to))
	}

	return out
}

// txnRowSummary is the short, currency-carrying description of a row in a bulk preview
func txnRowSummary(lk *txnLookup, s txnState, loc *time.Location) map[string]any {
	src, _ := strconv.ParseInt(s.SourceAccountId, 10, 64)
	cat, _ := strconv.ParseInt(s.CategoryId, 10, 64)
	m := map[string]any{
		"id":                s.Id,
		"date":              DateOfUnix(s.Time, loc),
		"typeName":          txnTypeName(int64(s.Type)),
		"sourceAccountId":   s.SourceAccountId,
		"sourceAccountName": lk.accountName(src),
		"sourceAmount":      s.SourceAmount,
		"sourceCurrency":    lk.accountCurrency(src),
		"categoryId":        s.CategoryId,
		"categoryName":      lk.categoryPath(cat),
		"comment":           s.Comment,
	}

	if s.Type == int(models.TRANSACTION_TYPE_TRANSFER) {
		dst, _ := strconv.ParseInt(s.DestinationAccountId, 10, 64)
		m["destinationAccountId"] = s.DestinationAccountId
		m["destinationAccountName"] = lk.accountName(dst)
		m["destinationAmount"] = s.DestinationAmount
		m["destinationCurrency"] = lk.accountCurrency(dst)
	}

	return m
}

// ─── selection: ids[] or filter{} ─────────────────────────────────────────────────────────────

// txnSelection is the exact row set a bulk write acts on, in a stable order
type txnSelection struct {
	Ids    []int64 // API ids, newest first when chosen by filter, request order when by ids
	States map[int64]*txnState
	By     string // "ids" | "filter"
	Filter map[string]any
}

// txnSelect resolves ids[] or filter{} to the exact rows. A filter is walked through the same
// upstream list GET /transactions walks, so "show me these" and "change these" cannot differ.
func txnSelect(mc *Ctx, lk *txnLookup, ids []string, filter *txnFilterArgs) (*txnSelection, error) {
	if len(ids) > 0 && filter != nil {
		return nil, Invalid("pass ids or filter, not both", "both ids and filter were given")
	}

	sel := &txnSelection{States: map[int64]*txnState{}}

	var wanted []int64

	switch {
	case len(ids) > 0:
		sel.By = "ids"
		seen := map[int64]bool{}

		for _, s := range ids {
			id, err := ResolveId("ids", s)

			if err != nil {
				return nil, err
			}

			if !seen[id] {
				seen[id] = true
				wanted = append(wanted, id)
			}
		}
	case filter != nil:
		sel.By = "filter"

		if filter.isEmpty() {
			return nil, Invalid("an empty filter would select every transaction; pass at least one of: "+txnFilterHint, "filter is empty")
		}

		rf, err := txnResolveFilter(mc, lk, filter)

		if err != nil {
			return nil, err
		}

		sel.Filter = rf.Echo

		if rf.Empty {
			return sel, nil
		}

		total, err := txnCount(mc, rf)

		if err != nil {
			return nil, err
		}

		if total > int64(txnSelectCap) {
			return nil, Conflict(fmt.Sprintf("narrow the filter (start/end, account, type) to %d rows or fewer per call", txnSelectCap), "%d transactions match the filter; one bulk write selects at most %d", total, txnSelectCap).WithDetails(map[string]any{"would_change": total, "cap": txnSelectCap})
		}

		rows, _, err := txnWalk(mc, rf, int(total), 0, false, true)

		if err != nil {
			return nil, err
		}

		seen := map[int64]bool{}

		for _, r := range rows {
			if id, ok := txnAsInt64(r["id"]); ok && !seen[id] {
				seen[id] = true
				wanted = append(wanted, id)
			}
		}
	default:
		return nil, Invalid("pass ids (transaction ids) or filter (the GET /transactions filter language)", "no transactions were chosen")
	}

	if len(wanted) == 0 {
		return sel, nil
	}

	states, canon, missing, err := txnReadStates(mc, wanted)

	if err != nil {
		return nil, err
	}

	if len(missing) > 0 && sel.By == "ids" {
		return nil, NotFound("GET /machine/v1/transactions lists them; drop the missing ids", "%d of the ids do not exist", len(missing)).WithDetails(map[string]any{"missing": txnInt64Strings(missing)})
	}

	seen := map[int64]bool{}

	for _, id := range wanted {
		apiId, ok := canon[id]

		if !ok || seen[apiId] {
			continue
		}

		seen[apiId] = true
		sel.Ids = append(sel.Ids, apiId)
		sel.States[apiId] = states[apiId]
	}

	return sel, nil
}

// ─── bulk update machinery shared by set-category, set-account, tags/* and move-all ───────────

// txnBulkState carries the resolve half's decision to the apply half
type txnBulkState struct {
	Before  map[int64]txnState
	After   map[int64]txnState
	Changed []int64
}

// txnBulkPlan builds a Plan from before/after states; rows whose after equals before are unchanged
func txnBulkPlan(mc *Ctx, lk *txnLookup, sel *txnSelection, target func(s txnState) (txnState, error), extra map[string]any) (*Plan, error) {
	st := &txnBulkState{Before: map[int64]txnState{}, After: map[int64]txnState{}}
	preview := make([]map[string]any, 0, len(sel.Ids))
	fp := make([]any, 0, len(sel.Ids))
	unchanged := 0

	for _, id := range sel.Ids {
		before := *sel.States[id]
		after, err := target(before)

		if err != nil {
			return nil, err
		}

		fp = append(fp, before.normalized(), after.normalized())

		if txnStatesEqual(before, after) {
			unchanged++
			continue
		}

		st.Before[id] = before
		st.After[id] = after
		st.Changed = append(st.Changed, id)

		row := txnRowSummary(lk, before, mc.Loc)
		row["changes"] = txnFieldDiff(lk, before, after)
		preview = append(preview, row)
	}

	p := &Plan{
		Changes:       map[string]int{"update": len(st.Changed), "unchanged": unchanged},
		Count:         len(st.Changed),
		Preview:       map[string]any{"rows": preview, "selectedBy": sel.By, "selected": len(sel.Ids)},
		Fingerprinted: fp,
		State:         st,
	}

	if sel.Filter != nil {
		p.Preview.(map[string]any)["filter"] = sel.Filter
	}

	for k, v := range extra {
		p.Preview.(map[string]any)[k] = v
	}

	return p, nil
}

// txnBulkFinish reads the rows after an upstream batch call and journals their inverse
func txnBulkFinish(mc *Ctx, st *txnBulkState, summary string) (any, error) {
	after, _, _, err := txnReadStates(mc, st.Changed)

	if err != nil {
		return nil, err
	}

	prior := make([]txnState, 0, len(st.Changed))
	now := make([]txnState, 0, len(st.Changed))

	for _, id := range st.Changed {
		prior = append(prior, st.Before[id])

		if s := after[id]; s != nil {
			now = append(now, *s)
		} else {
			now = append(now, st.After[id])
		}
	}

	op := NewInverseOp(txnOpRestore, prior, now)
	redo := NewInverseOp(txnOpRestore, now, prior)
	op.Redo = &redo

	if _, err := RecordJournal(mc, summary, len(st.Changed), []InverseOp{op}); err != nil {
		return nil, err
	}

	return map[string]any{"updated": len(st.Changed), "ids": txnInt64Strings(st.Changed)}, nil
}

// txnBulkBody is the body shape every bulk write shares
type txnBulkBody struct {
	WriteOpts
	Ids    []string       `json:"ids,omitempty"`
	Filter *txnFilterArgs `json:"filter,omitempty"`
}

func txnHandleSetCategory(mc *Ctx) (any, error) {
	var body struct {
		txnBulkBody
		CategoryId   string `json:"category_id,omitempty"`
		CategoryName string `json:"category_name,omitempty"`
	}

	if err := mc.BindBody(&body); err != nil {
		return nil, err
	}

	var lk *txnLookup

	resolve := func() (*Plan, error) {
		var err error

		if lk, err = txnLoadLookup(mc); err != nil {
			return nil, err
		}

		var cat *models.TransactionCategory

		switch {
		case strings.TrimSpace(body.CategoryId) != "":
			id, err := ResolveId("category_id", body.CategoryId)

			if err != nil {
				return nil, err
			}

			if cat = lk.catMap[id]; cat == nil {
				return nil, NotFound("GET /machine/v1/categories lists them", "no category with id %s", body.CategoryId)
			}
		case strings.TrimSpace(body.CategoryName) != "":
			id, err := txnResolveName("category", "category_name", body.CategoryName, lk.categoryCandidates(true, 0), "GET /machine/v1/categories lists them; \"Parent > Child\" disambiguates")

			if err != nil {
				return nil, err
			}

			cat = lk.catMap[id]
		default:
			return nil, Invalid("pass category_id or category_name", "category_id is required")
		}

		if cat.ParentCategoryId == models.LevelOneTransactionCategoryParentId {
			return nil, Invalid("pick one of its sub-categories; transactions take a secondary category", "category %q is a primary category", cat.Name)
		}

		sel, err := txnSelect(mc, lk, body.Ids, body.Filter)

		if err != nil {
			return nil, err
		}

		wantType := map[models.TransactionCategoryType]models.TransactionType{
			models.CATEGORY_TYPE_INCOME:   models.TRANSACTION_TYPE_INCOME,
			models.CATEGORY_TYPE_EXPENSE:  models.TRANSACTION_TYPE_EXPENSE,
			models.CATEGORY_TYPE_TRANSFER: models.TRANSACTION_TYPE_TRANSFER,
		}[cat.Type]

		var mismatched []map[string]any

		for _, id := range sel.Ids {
			if s := sel.States[id]; models.TransactionType(s.Type) != wantType {
				mismatched = append(mismatched, map[string]any{"id": s.Id, "typeName": txnTypeName(int64(s.Type))})
			}
		}

		if len(mismatched) > 0 {
			return nil, Invalid("category \""+lk.categoryPath(cat.CategoryId)+"\" is a "+txnCategoryTypeName(cat.Type)+" category; add \"type\": \""+txnTypeName(int64(wantType))+"\" to the filter, or drop the other rows", "%d selected transactions are not %s transactions", len(mismatched), txnTypeName(int64(wantType))).WithDetails(map[string]any{"mismatched": mismatched})
		}

		newCat := idString(cat.CategoryId)

		return txnBulkPlan(mc, lk, sel, func(s txnState) (txnState, error) {
			s.CategoryId = newCat
			s.TagIds = append([]string{}, s.TagIds...)
			return s, nil
		}, map[string]any{"categoryId": newCat, "categoryName": lk.categoryPath(cat.CategoryId)})
	}

	apply := func(p *Plan) (any, error) {
		st := p.State.(*txnBulkState)

		if len(st.Changed) == 0 {
			return map[string]any{"updated": 0, "ids": []string{}}, nil
		}

		catId := st.After[st.Changed[0]].CategoryId

		if _, err := mc.CallUpstream(api.Transactions.TransactionBatchUpdateCategoriesHandler, "POST", nil, map[string]any{"transactionIds": txnInt64Strings(st.Changed), "categoryId": catId}); err != nil {
			return nil, err
		}

		return txnBulkFinish(mc, st, fmt.Sprintf("set category of %d transactions", len(st.Changed)))
	}

	return RunWrite(mc, body.WriteOpts, resolve, apply)
}

func txnHandleSetAccount(mc *Ctx) (any, error) {
	var body struct {
		txnBulkBody
		AccountId   string `json:"account_id,omitempty"`
		AccountName string `json:"account_name,omitempty"`
		// Side is which side of a transfer moves: source (the default) or destination
		Side string `json:"side,omitempty"`
	}

	if err := mc.BindBody(&body); err != nil {
		return nil, err
	}

	side := strings.ToLower(strings.TrimSpace(body.Side))

	if side == "" {
		side = "source"
	}

	if side != "source" && side != "destination" {
		return nil, Invalid("side is source (the default) or destination", "unknown side %q", body.Side)
	}

	resolve := func() (*Plan, error) {
		lk, err := txnLoadLookup(mc)

		if err != nil {
			return nil, err
		}

		acct, err := lk.resolveAccount("account_id", body.AccountId, "account_name", body.AccountName, true)

		if err != nil {
			return nil, err
		}

		if acct.Hidden {
			return nil, Invalid("unhide the account first (POST /accounts/:id/hide with hidden: false)", "account %q is hidden", acct.Name)
		}

		sel, err := txnSelect(mc, lk, body.Ids, body.Filter)

		if err != nil {
			return nil, err
		}

		newId := idString(acct.AccountId)
		var problems []map[string]any

		for _, id := range sel.Ids {
			s := sel.States[id]
			isTransfer := models.TransactionType(s.Type) == models.TRANSACTION_TYPE_TRANSFER
			oldId := s.SourceAccountId

			if side == "destination" {
				if !isTransfer {
					problems = append(problems, map[string]any{"id": s.Id, "problem": "not a transfer; it has no destination account"})
					continue
				}

				oldId = s.DestinationAccountId
			}

			if oldId == newId {
				continue
			}

			if isTransfer && ((side == "source" && s.DestinationAccountId == newId) || (side == "destination" && s.SourceAccountId == newId)) {
				problems = append(problems, map[string]any{"id": s.Id, "problem": "would become a transfer from an account to itself"})
				continue
			}

			oldNum, _ := strconv.ParseInt(oldId, 10, 64)

			if cur := lk.accountCurrency(oldNum); cur != acct.Currency {
				problems = append(problems, map[string]any{"id": s.Id, "problem": "currency " + cur + " differs from the account's " + acct.Currency})
			}
		}

		if len(problems) > 0 {
			return nil, Invalid("moving a transaction keeps its amount, so only same-currency moves are allowed; drop these rows or narrow with currency", "%d selected transactions cannot move to %q", len(problems), acct.Name).WithDetails(map[string]any{"problems": problems})
		}

		return txnBulkPlan(mc, lk, sel, func(s txnState) (txnState, error) {
			if side == "destination" {
				s.DestinationAccountId = newId
			} else {
				s.SourceAccountId = newId
			}

			return s, nil
		}, map[string]any{"accountId": newId, "accountName": acct.Name, "currency": acct.Currency, "side": side})
	}

	apply := func(p *Plan) (any, error) {
		st := p.State.(*txnBulkState)

		if len(st.Changed) == 0 {
			return map[string]any{"updated": 0, "ids": []string{}}, nil
		}

		first := st.After[st.Changed[0]]
		acct := first.SourceAccountId

		if side == "destination" {
			acct = first.DestinationAccountId
		}

		if _, err := mc.CallUpstream(api.Transactions.TransactionBatchUpdateAccountsHandler, "POST", nil, map[string]any{"transactionIds": txnInt64Strings(st.Changed), "accountId": acct, "isDestinationAccount": side == "destination"}); err != nil {
			return nil, err
		}

		return txnBulkFinish(mc, st, fmt.Sprintf("set %s account of %d transactions", side, len(st.Changed)))
	}

	return RunWrite(mc, body.WriteOpts, resolve, apply)
}

// txnTagsAdd returns current ∪ add, current first, in order
func txnTagsAdd(current, add []string) []string {
	out := append([]string{}, current...)
	have := map[string]bool{}

	for _, t := range current {
		have[t] = true
	}

	for _, t := range add {
		if !have[t] {
			have[t] = true
			out = append(out, t)
		}
	}

	return out
}

// txnTagsRemove returns current minus remove, in order
func txnTagsRemove(current, remove []string) []string {
	drop := map[string]bool{}

	for _, t := range remove {
		drop[t] = true
	}

	out := []string{}

	for _, t := range current {
		if !drop[t] {
			out = append(out, t)
		}
	}

	return out
}

func txnHandleTagsAdd(mc *Ctx) (any, error)    { return txnTagsWrite(mc, "add") }
func txnHandleTagsRemove(mc *Ctx) (any, error) { return txnTagsWrite(mc, "remove") }
func txnHandleTagsClear(mc *Ctx) (any, error)  { return txnTagsWrite(mc, "clear") }

func txnTagsWrite(mc *Ctx, mode string) (any, error) {
	var body struct {
		txnBulkBody
		TagIds   []string `json:"tag_ids,omitempty"`
		TagNames []string `json:"tag_names,omitempty"`
	}

	if err := mc.BindBody(&body); err != nil {
		return nil, err
	}

	if mode == "clear" && (len(body.TagIds) > 0 || len(body.TagNames) > 0) {
		return nil, Invalid("tags/clear removes every tag; use tags/remove to remove particular ones", "tags/clear takes no tag_ids")
	}

	resolve := func() (*Plan, error) {
		lk, err := txnLoadLookup(mc)

		if err != nil {
			return nil, err
		}

		var tagIds []string

		if mode != "clear" {
			tagIds, err = lk.resolveTags(body.TagIds, body.TagNames)

			if err != nil {
				return nil, err
			}

			if len(tagIds) == 0 {
				return nil, Invalid("pass tag_ids or tag_names", "no tags were given")
			}
		}

		sel, err := txnSelect(mc, lk, body.Ids, body.Filter)

		if err != nil {
			return nil, err
		}

		if mode == "add" {
			var tooMany []map[string]any

			for _, id := range sel.Ids {
				if n := len(txnTagsAdd(sel.States[id].TagIds, tagIds)); n > models.MaximumTagsCountOfTransaction {
					tooMany = append(tooMany, map[string]any{"id": sel.States[id].Id, "tags": n})
				}
			}

			if len(tooMany) > 0 {
				return nil, Invalid(fmt.Sprintf("a transaction holds at most %d tags; remove some first, or drop these rows", models.MaximumTagsCountOfTransaction), "%d transactions would exceed the tag limit", len(tooMany)).WithDetails(map[string]any{"rows": tooMany})
			}
		}

		extra := map[string]any{"mode": mode}

		if mode != "clear" {
			extra["tagIds"] = tagIds
			extra["tagNames"] = lk.tagNames(tagIds)
		}

		return txnBulkPlan(mc, lk, sel, func(s txnState) (txnState, error) {
			switch mode {
			case "add":
				s.TagIds = txnTagsAdd(s.TagIds, tagIds)
			case "remove":
				s.TagIds = txnTagsRemove(s.TagIds, tagIds)
			default:
				s.TagIds = []string{}
			}

			return s, nil
		}, extra)
	}

	apply := func(p *Plan) (any, error) {
		st := p.State.(*txnBulkState)

		if len(st.Changed) == 0 {
			return map[string]any{"updated": 0, "ids": []string{}}, nil
		}

		ids := txnInt64Strings(st.Changed)
		var err error

		switch mode {
		case "add":
			var tagIds []string

			for _, id := range st.Changed {
				tagIds = txnTagsAdd(tagIds, txnTagsRemove(st.After[id].TagIds, st.Before[id].TagIds))
			}

			_, err = mc.CallUpstream(api.Transactions.TransactionBatchAddTagsHandler, "POST", nil, map[string]any{"transactionIds": ids, "tagIds": tagIds})
		case "remove":
			var tagIds []string

			for _, id := range st.Changed {
				tagIds = txnTagsAdd(tagIds, txnTagsRemove(st.Before[id].TagIds, st.After[id].TagIds))
			}

			_, err = mc.CallUpstream(api.Transactions.TransactionBatchRemoveTagsHandler, "POST", nil, map[string]any{"transactionIds": ids, "tagIds": tagIds})
		default:
			_, err = mc.CallUpstream(api.Transactions.TransactionBatchClearTagsHandler, "POST", nil, map[string]any{"transactionIds": ids})
		}

		if err != nil {
			return nil, err
		}

		return txnBulkFinish(mc, st, fmt.Sprintf("%s tags on %d transactions", mode, len(st.Changed)))
	}

	return RunWrite(mc, body.WriteOpts, resolve, apply)
}

func txnHandleMoveAll(mc *Ctx) (any, error) {
	var body struct {
		WriteOpts
		FromAccountId   string `json:"from_account_id,omitempty"`
		FromAccountName string `json:"from_account_name,omitempty"`
		ToAccountId     string `json:"to_account_id,omitempty"`
		ToAccountName   string `json:"to_account_name,omitempty"`
	}

	if err := mc.BindBody(&body); err != nil {
		return nil, err
	}

	var fromId, toId int64

	resolve := func() (*Plan, error) {
		lk, err := txnLoadLookup(mc)

		if err != nil {
			return nil, err
		}

		from, err := lk.resolveAccount("from_account_id", body.FromAccountId, "from_account_name", body.FromAccountName, true)

		if err != nil {
			return nil, err
		}

		to, err := lk.resolveAccount("to_account_id", body.ToAccountId, "to_account_name", body.ToAccountName, true)

		if err != nil {
			return nil, err
		}

		if from.AccountId == to.AccountId {
			return nil, Invalid("name two different accounts", "from and to are the same account")
		}

		if from.Hidden || to.Hidden {
			return nil, Invalid("unhide both accounts first (POST /accounts/:id/hide with hidden: false)", "a hidden account cannot give or take transactions")
		}

		if from.Currency != to.Currency {
			return nil, Invalid("moving keeps every amount, so both accounts must share a currency; for a different currency create a transfer instead", "%q is %s and %q is %s", from.Name, from.Currency, to.Name, to.Currency)
		}

		fromId, toId = from.AccountId, to.AccountId

		// every row the account touches, both sides of transfers, walked through upstream's list
		rf := &txnResolvedFilter{AccountIds: []int64{from.AccountId}, Echo: map[string]any{"accountIds": []string{idString(from.AccountId)}}}
		total, err := txnCount(mc, rf)

		if err != nil {
			return nil, err
		}

		sel := &txnSelection{By: "account", States: map[int64]*txnState{}, Filter: rf.Echo}

		if total > 0 {
			rows, _, err := txnWalk(mc, rf, int(total), 0, false, true)

			if err != nil {
				return nil, err
			}

			var wanted []int64

			for _, r := range rows {
				if id, ok := txnAsInt64(r["id"]); ok {
					wanted = append(wanted, id)
				}
			}

			states, canon, _, err := txnReadStates(mc, wanted)

			if err != nil {
				return nil, err
			}

			seen := map[int64]bool{}

			for _, id := range wanted {
				if apiId, ok := canon[id]; ok && !seen[apiId] {
					seen[apiId] = true
					sel.Ids = append(sel.Ids, apiId)
					sel.States[apiId] = states[apiId]
				}
			}
		}

		fromS, toS := idString(from.AccountId), idString(to.AccountId)
		var selfTransfers []string

		for _, id := range sel.Ids {
			s := sel.States[id]

			if (s.SourceAccountId == fromS && s.DestinationAccountId == toS) || (s.SourceAccountId == toS && s.DestinationAccountId == fromS) {
				selfTransfers = append(selfTransfers, s.Id)
			}
		}

		if len(selfTransfers) > 0 {
			return nil, Invalid("delete or re-point the transfers between these two accounts first (they would become transfers from an account to itself)", "%d transfers run between %q and %q", len(selfTransfers), from.Name, to.Name).WithDetails(map[string]any{"ids": selfTransfers})
		}

		return txnBulkPlan(mc, lk, sel, func(s txnState) (txnState, error) {
			if s.SourceAccountId == fromS {
				s.SourceAccountId = toS
			}

			if s.DestinationAccountId == fromS {
				s.DestinationAccountId = toS
			}

			return s, nil
		}, map[string]any{"fromAccountId": fromS, "fromAccountName": from.Name, "toAccountId": toS, "toAccountName": to.Name, "currency": from.Currency})
	}

	apply := func(p *Plan) (any, error) {
		st := p.State.(*txnBulkState)

		if len(st.Changed) == 0 {
			return map[string]any{"updated": 0, "ids": []string{}}, nil
		}

		if _, err := mc.CallUpstream(api.Transactions.TransactionMoveAllBetweenAccountsHandler, "POST", nil, map[string]any{"fromAccountId": idString(fromId), "toAccountId": idString(toId)}); err != nil {
			return nil, err
		}

		return txnBulkFinish(mc, st, fmt.Sprintf("moved %d transactions between accounts", len(st.Changed)))
	}

	return RunWrite(mc, body.WriteOpts, resolve, apply)
}

// ─── create ────────────────────────────────────────────────────────────────────────────────────

// txnGeoArg is a geo location; decimals travel as JSON numbers and are passed through verbatim
type txnGeoArg struct {
	Latitude  json.Number `json:"latitude"`
	Longitude json.Number `json:"longitude"`
}

func (g *txnGeoArg) validate() (string, string, error) {
	lat, err1 := strconv.ParseFloat(g.Latitude.String(), 64)
	lon, err2 := strconv.ParseFloat(g.Longitude.String(), 64)

	if err1 != nil || err2 != nil || lat < -90 || lat > 90 || lon < -180 || lon > 180 {
		return "", "", Invalid("geo is {\"latitude\": -90..90, \"longitude\": -180..180}", "geo %s,%s is not a location", g.Latitude, g.Longitude)
	}

	return g.Latitude.String(), g.Longitude.String(), nil
}

// txnCreateRow is one row of POST /transactions
type txnCreateRow struct {
	Type                   string      `json:"type"`
	Date                   string      `json:"date,omitempty"`
	Time                   string      `json:"time,omitempty"`
	AccountId              string      `json:"account_id,omitempty"`
	AccountName            string      `json:"account_name,omitempty"`
	Amount                 json.Number `json:"amount"`
	DestinationAccountId   string      `json:"destination_account_id,omitempty"`
	DestinationAccountName string      `json:"destination_account_name,omitempty"`
	DestinationAmount      json.Number `json:"destination_amount,omitempty"`
	CategoryId             string      `json:"category_id,omitempty"`
	CategoryName           string      `json:"category_name,omitempty"`
	TagIds                 []string    `json:"tag_ids,omitempty"`
	TagNames               []string    `json:"tag_names,omitempty"`
	Comment                string      `json:"comment,omitempty"`
	HideAmount             bool        `json:"hide_amount,omitempty"`
	Geo                    *txnGeoArg  `json:"geo,omitempty"`
}

// txnResolveWhen turns date (YYYY-MM-DD, placed at 12:00 in loc) or time (RFC 3339) into unix
// seconds and the utc offset in force in loc at that instant
func txnResolveWhen(date, instant string, loc *time.Location) (int64, int16, error) {
	var t time.Time

	switch {
	case strings.TrimSpace(instant) != "":
		parsed, err := time.Parse(time.RFC3339, strings.TrimSpace(instant))

		if err != nil {
			return 0, 0, Invalid("time is an RFC 3339 instant such as 2026-09-21T14:30:00-07:00 — or pass date as YYYY-MM-DD", "time %q is not RFC 3339", instant)
		}

		t = parsed
	case strings.TrimSpace(date) != "":
		d, err := ParseDate("date", date, loc)

		if err != nil {
			return 0, 0, err
		}

		t = time.Date(d.Year(), d.Month(), d.Day(), 12, 0, 0, 0, loc)
	default:
		return 0, 0, Invalid("pass date (YYYY-MM-DD) or time (RFC 3339)", "a transaction needs a date")
	}

	if t.Unix() <= 0 {
		return 0, 0, Invalid("pass a date after 1970-01-01", "the date is out of range")
	}

	return t.Unix(), UTCOffsetMinutes(t, loc), nil
}

// txnResolvedCreate is one row resolved to upstream's create request
type txnResolvedCreate struct {
	State   txnState
	Preview map[string]any
}

func txnResolveCreateRow(lk *txnLookup, loc *time.Location, i int, r txnCreateRow) (*txnResolvedCreate, error) {
	at := func(err error) error {
		if f, ok := err.(*Fail); ok {
			f.Message = fmt.Sprintf("transactions[%d]: %s", i, f.Message)
			return f
		}

		return err
	}

	ttype, err := txnParseType(r.Type)

	if err != nil {
		return nil, at(err)
	}

	if ttype == 0 {
		return nil, at(Invalid("type is income, expense or transfer", "type is required"))
	}

	if ttype == models.TRANSACTION_TYPE_MODIFY_BALANCE {
		return nil, at(Invalid("an opening balance is set when the account is created (POST /accounts with initial_balance); to correct a balance use POST /accounts/:id/reconcile/plan", "balance-modification transactions are not created here"))
	}

	sec, offset, err := txnResolveWhen(r.Date, r.Time, loc)

	if err != nil {
		return nil, at(err)
	}

	if r.Amount == "" {
		return nil, at(Invalid("amount is integer hundredths: 12.50 is 1250", "amount is required"))
	}

	amount, err := AmountArg("amount", r.Amount)

	if err != nil {
		return nil, at(err)
	}

	if amount < 0 {
		return nil, at(Invalid("amounts are magnitudes; the type (income/expense/transfer) carries direction", "amount %d is negative", amount))
	}

	src, err := lk.resolveAccount("account_id", r.AccountId, "account_name", r.AccountName, true)

	if err != nil {
		return nil, at(err)
	}

	if src.Hidden {
		return nil, at(Invalid("unhide the account first (POST /accounts/:id/hide with hidden: false)", "account %q is hidden", src.Name))
	}

	cat, err := lk.resolveCategory(r.CategoryId, r.CategoryName, ttype)

	if err != nil {
		return nil, at(err)
	}

	tagIds, err := lk.resolveTags(r.TagIds, r.TagNames)

	if err != nil {
		return nil, at(err)
	}

	if len(tagIds) > models.MaximumTagsCountOfTransaction {
		return nil, at(Invalid(fmt.Sprintf("a transaction holds at most %d tags", models.MaximumTagsCountOfTransaction), "%d tags given", len(tagIds)))
	}

	if len([]rune(r.Comment)) > 255 {
		return nil, at(Invalid("shorten the comment to 255 characters", "comment is %d characters", len([]rune(r.Comment))))
	}

	s := txnState{
		Type:            int(ttype),
		CategoryId:      idString(cat.CategoryId),
		Time:            sec,
		UtcOffset:       offset,
		SourceAccountId: idString(src.AccountId),
		SourceAmount:    amount,
		HideAmount:      r.HideAmount,
		Comment:         r.Comment,
		TagIds:          tagIds,
		PictureIds:      []string{},
	}

	preview := map[string]any{
		"index":             i,
		"typeName":          txnTypeName(int64(ttype)),
		"date":              DateOfUnix(sec, loc),
		"time":              sec,
		"utcOffset":         offset,
		"sourceAccountId":   s.SourceAccountId,
		"sourceAccountName": src.Name,
		"sourceAmount":      amount,
		"sourceCurrency":    src.Currency,
		"categoryId":        s.CategoryId,
		"categoryName":      lk.categoryPath(cat.CategoryId),
		"tagIds":            tagIds,
		"tagNames":          lk.tagNames(tagIds),
		"comment":           r.Comment,
		"hideAmount":        r.HideAmount,
	}

	if ttype == models.TRANSACTION_TYPE_TRANSFER {
		dst, err := lk.resolveAccount("destination_account_id", r.DestinationAccountId, "destination_account_name", r.DestinationAccountName, true)

		if err != nil {
			return nil, at(err)
		}

		if dst.AccountId == src.AccountId {
			return nil, at(Invalid("a transfer moves money between two different accounts", "source and destination are the same account"))
		}

		if dst.Hidden {
			return nil, at(Invalid("unhide the account first (POST /accounts/:id/hide with hidden: false)", "account %q is hidden", dst.Name))
		}

		destAmount := amount

		if r.DestinationAmount != "" {
			if destAmount, err = AmountArg("destination_amount", r.DestinationAmount); err != nil {
				return nil, at(err)
			}

			if destAmount < 0 {
				return nil, at(Invalid("amounts are magnitudes", "destination_amount %d is negative", destAmount))
			}
		}

		if dst.Currency == src.Currency && destAmount != amount {
			return nil, at(Invalid("both accounts are "+src.Currency+"; drop destination_amount or make it equal amount", "a same-currency transfer moves one amount, but destination_amount %d differs from amount %d", destAmount, amount))
		}

		if dst.Currency != src.Currency && r.DestinationAmount == "" {
			return nil, at(Invalid("pass destination_amount in "+dst.Currency+" hundredths (the amount that arrived); the plane never converts it for you", "a %s → %s transfer needs destination_amount", src.Currency, dst.Currency))
		}

		s.DestinationAccountId = idString(dst.AccountId)
		s.DestinationAmount = destAmount
		preview["destinationAccountId"] = s.DestinationAccountId
		preview["destinationAccountName"] = dst.Name
		preview["destinationAmount"] = destAmount
		preview["destinationCurrency"] = dst.Currency
	} else if r.DestinationAccountId != "" || r.DestinationAccountName != "" || r.DestinationAmount != "" {
		return nil, at(Invalid("drop destination_* or set type to transfer", "only a transfer has a destination"))
	}

	if r.Geo != nil {
		lat, lon, err := r.Geo.validate()

		if err != nil {
			return nil, at(err)
		}

		s.GeoLatitude, s.GeoLongitude = lat, lon
		preview["geoLocation"] = map[string]string{"latitude": lat, "longitude": lon}
	}

	return &txnResolvedCreate{State: s, Preview: preview}, nil
}

// txnReplays remembers idempotent creates within the process lifetime (apis.mdx §7.6)
var txnReplays = struct {
	sync.Mutex
	m map[string]txnReplay
}{m: map[string]txnReplay{}}

type txnReplay struct {
	fingerprint string
	result      any
	uid         int64
}

// txnSessionId derives upstream's clientSessionId for row i of a keyed request
func txnSessionId(key string, i, n int) string {
	if key == "" {
		return ""
	}

	if n == 1 {
		return key
	}

	return fmt.Sprintf("%s#%d", key, i)
}

func txnHandleCreate(mc *Ctx) (any, error) {
	var body struct {
		WriteOpts
		Transactions []txnCreateRow `json:"transactions"`
	}

	if err := mc.BindBody(&body); err != nil {
		return nil, err
	}

	if len(body.Transactions) == 0 {
		return nil, Invalid("pass transactions: [{type, date, account_id, amount, category_id, …}]", "no transactions were given")
	}

	if len(body.IdempotencyKey) > 128 {
		return nil, Invalid("use an idempotency_key of at most 128 characters", "idempotency_key is too long")
	}

	resolve := func() (*Plan, error) {
		lk, err := txnLoadLookup(mc)

		if err != nil {
			return nil, err
		}

		rows := make([]*txnResolvedCreate, 0, len(body.Transactions))
		preview := make([]map[string]any, 0, len(body.Transactions))
		var warnings []string

		for i, r := range body.Transactions {
			rc, err := txnResolveCreateRow(lk, mc.Loc, i, r)

			if err != nil {
				return nil, err
			}

			rows = append(rows, rc)
			preview = append(preview, rc.Preview)

			if rc.State.Time > time.Now().Unix()+86400 {
				warnings = append(warnings, fmt.Sprintf("transactions[%d] is dated in the future (%s)", i, rc.Preview["date"]))
			}
		}

		return &Plan{
			Changes:  map[string]int{"create": len(rows)},
			Count:    len(rows),
			Preview:  map[string]any{"rows": preview},
			Warnings: warnings,
			State:    rows,
		}, nil
	}

	// a repeat of a keyed create returns the original response (apis.mdx §7.6)
	if key := body.IdempotencyKey; key != "" && !body.IsDryRun() {
		txnReplays.Lock()
		prior, seen := txnReplays.m[key]
		txnReplays.Unlock()

		if seen && prior.uid == mc.Uid {
			p, err := resolve()

			if err != nil {
				return nil, err
			}

			if ChangeFingerprint(p.Preview) != prior.fingerprint {
				return nil, Conflict("use a new idempotency_key for a different request", "idempotency_key %q was already used for a different set of transactions", key)
			}

			mc.SetMeta("replayed", true)

			return prior.result, nil
		}
	}

	var fingerprint string

	apply := func(p *Plan) (any, error) {
		rows := p.State.([]*txnResolvedCreate)
		fingerprint = ChangeFingerprint(p.Preview)
		created := make([]map[string]any, 0, len(rows))
		var ids []int64

		rollback := func() int {
			n := 0

			for j := len(ids) - 1; j >= 0; j-- {
				if _, err := mc.CallUpstream(api.Transactions.TransactionDeleteHandler, "POST", nil, map[string]any{"id": idString(ids[j])}); err == nil {
					n++
				}
			}

			return n
		}

		for i, rc := range rows {
			req := txnCreateBodyFromState(rc.State)

			if sid := txnSessionId(body.IdempotencyKey, i, len(rows)); sid != "" {
				req["clientSessionId"] = sid
			}

			var resp map[string]any

			if err := mc.CallUpstreamInto(api.Transactions.TransactionCreateHandler, "POST", nil, req, &resp); err != nil {
				rolledBack := rollback()
				f := toFail(err)
				f.Message = fmt.Sprintf("transactions[%d]: %s", i, f.Message)

				return nil, f.WithDetails(map[string]any{"failedIndex": i, "rolledBack": rolledBack})
			}

			id, _ := txnAsInt64(resp["id"])
			ids = append(ids, id)
			created = append(created, resp)
		}

		lk, err := txnLoadLookup(mc)

		if err == nil {
			for _, r := range created {
				txnDecorate(r, lk, mc.Loc)
			}
		}

		states, _, _, err := txnReadStates(mc, ids)

		if err != nil {
			return nil, err
		}

		check := make([]txnState, 0, len(ids))

		for _, id := range ids {
			if s := states[id]; s != nil {
				check = append(check, *s)
			}
		}

		if _, err := RecordJournal(mc, fmt.Sprintf("created %d transactions", len(ids)), len(ids), []InverseOp{NewInverseOp(txnOpDelete, map[string]any{"ids": txnInt64Strings(ids)}, check)}); err != nil {
			return nil, err
		}

		return map[string]any{"created": len(ids), "ids": txnInt64Strings(ids), "transactions": created}, nil
	}

	result, err := RunWrite(mc, body.WriteOpts, resolve, apply)

	if err == nil && body.IdempotencyKey != "" && !body.IsDryRun() && fingerprint != "" {
		txnReplays.Lock()
		txnReplays.m[body.IdempotencyKey] = txnReplay{fingerprint: fingerprint, result: result, uid: mc.Uid}
		txnReplays.Unlock()
	}

	return result, err
}

// ─── patch ─────────────────────────────────────────────────────────────────────────────────────

// txnPatchArgs are the editable fields of PATCH /transactions/:id; absent means unchanged
type txnPatchArgs struct {
	Type                   *string      `json:"type,omitempty"`
	Date                   *string      `json:"date,omitempty"`
	Time                   *string      `json:"time,omitempty"`
	AccountId              *string      `json:"account_id,omitempty"`
	AccountName            *string      `json:"account_name,omitempty"`
	Amount                 *json.Number `json:"amount,omitempty"`
	DestinationAccountId   *string      `json:"destination_account_id,omitempty"`
	DestinationAccountName *string      `json:"destination_account_name,omitempty"`
	DestinationAmount      *json.Number `json:"destination_amount,omitempty"`
	CategoryId             *string      `json:"category_id,omitempty"`
	CategoryName           *string      `json:"category_name,omitempty"`
	TagIds                 *[]string    `json:"tag_ids,omitempty"`
	TagNames               *[]string    `json:"tag_names,omitempty"`
	Comment                *string      `json:"comment,omitempty"`
	HideAmount             *bool        `json:"hide_amount,omitempty"`
	Geo                    *txnGeoArg   `json:"geo,omitempty"`
	ClearGeo               bool         `json:"clear_geo,omitempty"`
}

func txnStrOr(p *string) string {
	if p == nil {
		return ""
	}

	return *p
}

// txnApplyPatch merges a patch onto the current state and validates the result
func txnApplyPatch(lk *txnLookup, loc *time.Location, cur txnState, a txnPatchArgs) (txnState, error) {
	next := cur
	next.TagIds = append([]string{}, cur.TagIds...)
	next.PictureIds = append([]string{}, cur.PictureIds...)
	curType := models.TransactionType(cur.Type)
	newType := curType

	if a.Type != nil {
		t, err := txnParseType(*a.Type)

		if err != nil {
			return next, err
		}

		if t != 0 {
			newType = t
		}
	}

	if (curType == models.TRANSACTION_TYPE_MODIFY_BALANCE) != (newType == models.TRANSACTION_TYPE_MODIFY_BALANCE) {
		return next, Invalid("an opening balance stays an opening balance; create a new income/expense instead", "a transaction cannot change to or from balance_modification")
	}

	next.Type = int(newType)

	// when
	if a.Time != nil && strings.TrimSpace(*a.Time) != "" {
		sec, off, err := txnResolveWhen("", *a.Time, loc)

		if err != nil {
			return next, err
		}

		next.Time, next.UtcOffset = sec, off
	} else if a.Date != nil && strings.TrimSpace(*a.Date) != "" {
		d, err := ParseDate("date", *a.Date, loc)

		if err != nil {
			return next, err
		}

		old := time.Unix(cur.Time, 0).In(loc)
		t := time.Date(d.Year(), d.Month(), d.Day(), old.Hour(), old.Minute(), old.Second(), 0, loc)
		next.Time, next.UtcOffset = t.Unix(), UTCOffsetMinutes(t, loc)
	}

	// source account and amount
	if txnStrOr(a.AccountId) != "" || txnStrOr(a.AccountName) != "" {
		acct, err := lk.resolveAccount("account_id", txnStrOr(a.AccountId), "account_name", txnStrOr(a.AccountName), true)

		if err != nil {
			return next, err
		}

		if acct.Hidden {
			return next, Invalid("unhide the account first", "account %q is hidden", acct.Name)
		}

		oldId, _ := strconv.ParseInt(cur.SourceAccountId, 10, 64)

		if lk.accountCurrency(oldId) != acct.Currency && a.Amount == nil {
			return next, Invalid("the new account is "+acct.Currency+"; pass amount in its hundredths too (the plane never converts)", "moving to an account in another currency needs amount")
		}

		next.SourceAccountId = idString(acct.AccountId)
	}

	amountChanged := false

	if a.Amount != nil {
		v, err := AmountArg("amount", *a.Amount)

		if err != nil {
			return next, err
		}

		if v < 0 && newType != models.TRANSACTION_TYPE_MODIFY_BALANCE {
			return next, Invalid("amounts are magnitudes; the type carries direction", "amount %d is negative", v)
		}

		next.SourceAmount = v
		amountChanged = true
	}

	// transfer destination
	if newType == models.TRANSACTION_TYPE_TRANSFER {
		if txnStrOr(a.DestinationAccountId) != "" || txnStrOr(a.DestinationAccountName) != "" {
			acct, err := lk.resolveAccount("destination_account_id", txnStrOr(a.DestinationAccountId), "destination_account_name", txnStrOr(a.DestinationAccountName), true)

			if err != nil {
				return next, err
			}

			if acct.Hidden {
				return next, Invalid("unhide the account first", "account %q is hidden", acct.Name)
			}

			next.DestinationAccountId = idString(acct.AccountId)
		}

		if next.DestinationAccountId == "" || next.DestinationAccountId == "0" {
			return next, Invalid("pass destination_account_id (or destination_account_name) for a transfer", "a transfer needs a destination account")
		}

		if next.DestinationAccountId == next.SourceAccountId {
			return next, Invalid("a transfer moves money between two different accounts", "source and destination are the same account")
		}

		srcId, _ := strconv.ParseInt(next.SourceAccountId, 10, 64)
		dstId, _ := strconv.ParseInt(next.DestinationAccountId, 10, 64)
		sameCurrency := lk.accountCurrency(srcId) == lk.accountCurrency(dstId)

		if a.DestinationAmount != nil {
			v, err := AmountArg("destination_amount", *a.DestinationAmount)

			if err != nil {
				return next, err
			}

			if v < 0 {
				return next, Invalid("amounts are magnitudes", "destination_amount %d is negative", v)
			}

			next.DestinationAmount = v
		} else if sameCurrency && (amountChanged || curType != models.TRANSACTION_TYPE_TRANSFER) {
			next.DestinationAmount = next.SourceAmount
		} else if !sameCurrency && curType != models.TRANSACTION_TYPE_TRANSFER {
			return next, Invalid("pass destination_amount in "+lk.accountCurrency(dstId)+" hundredths", "a cross-currency transfer needs destination_amount")
		}

		if sameCurrency && next.DestinationAmount != next.SourceAmount {
			return next, Invalid("both accounts share a currency, so both sides carry one amount; drop destination_amount or make it equal amount", "destination_amount %d differs from amount %d", next.DestinationAmount, next.SourceAmount)
		}
	} else {
		if a.DestinationAccountId != nil || a.DestinationAccountName != nil || a.DestinationAmount != nil {
			return next, Invalid("drop destination_* or set type to transfer", "only a transfer has a destination")
		}

		next.DestinationAccountId = ""
		next.DestinationAmount = 0
	}

	// category
	if newType == models.TRANSACTION_TYPE_MODIFY_BALANCE {
		if txnStrOr(a.CategoryId) != "" || txnStrOr(a.CategoryName) != "" {
			return next, Invalid("drop category_*; an opening balance has no category", "a balance_modification transaction takes no category")
		}
	} else if txnStrOr(a.CategoryId) != "" || txnStrOr(a.CategoryName) != "" {
		cat, err := lk.resolveCategory(txnStrOr(a.CategoryId), txnStrOr(a.CategoryName), newType)

		if err != nil {
			return next, err
		}

		next.CategoryId = idString(cat.CategoryId)
	} else if newType != curType {
		catId, _ := strconv.ParseInt(cur.CategoryId, 10, 64)

		if c := lk.catMap[catId]; c == nil || c.Type != txnCategoryTypeFor(newType) {
			return next, Invalid("changing the type needs a "+txnCategoryTypeName(txnCategoryTypeFor(newType))+" category: pass category_id or category_name", "the current category does not fit a %s transaction", txnTypeName(int64(newType)))
		}
	}

	// tags
	if a.TagIds != nil || a.TagNames != nil {
		var ids, names []string

		if a.TagIds != nil {
			ids = *a.TagIds
		}

		if a.TagNames != nil {
			names = *a.TagNames
		}

		tags, err := lk.resolveTags(ids, names)

		if err != nil {
			return next, err
		}

		if len(tags) > models.MaximumTagsCountOfTransaction {
			return next, Invalid(fmt.Sprintf("a transaction holds at most %d tags", models.MaximumTagsCountOfTransaction), "%d tags given", len(tags))
		}

		next.TagIds = tags
	}

	if a.Comment != nil {
		if len([]rune(*a.Comment)) > 255 {
			return next, Invalid("shorten the comment to 255 characters", "comment is %d characters", len([]rune(*a.Comment)))
		}

		next.Comment = *a.Comment
	}

	if a.HideAmount != nil {
		next.HideAmount = *a.HideAmount
	}

	if a.ClearGeo && a.Geo != nil {
		return next, Invalid("pass geo or clear_geo, not both", "geo and clear_geo contradict")
	}

	if a.ClearGeo {
		next.GeoLatitude, next.GeoLongitude = "", ""
	} else if a.Geo != nil {
		lat, lon, err := a.Geo.validate()

		if err != nil {
			return next, err
		}

		next.GeoLatitude, next.GeoLongitude = lat, lon
	}

	return next, nil
}

func txnHandlePatch(mc *Ctx) (any, error) {
	id, err := ResolveId("id", mc.Param("id"))

	if err != nil {
		return nil, err
	}

	var body struct {
		WriteOpts
		txnPatchArgs
	}

	if err := mc.BindBody(&body); err != nil {
		return nil, err
	}

	type patchState struct {
		before, after txnState
		apiId         int64
	}

	resolve := func() (*Plan, error) {
		lk, err := txnLoadLookup(mc)

		if err != nil {
			return nil, err
		}

		states, canon, _, err := txnReadStates(mc, []int64{id})

		if err != nil {
			return nil, err
		}

		apiId, ok := canon[id]

		if !ok {
			return nil, NotFound("GET /machine/v1/transactions lists them", "no transaction with id %s", idString(id)).WithDetails(map[string]any{"id": idString(id)})
		}

		before := *states[apiId]
		after, err := txnApplyPatch(lk, mc.Loc, before, body.txnPatchArgs)

		if err != nil {
			return nil, err
		}

		diff := txnFieldDiff(lk, before, after)
		changed := !txnStatesEqual(before, after)
		row := txnRowSummary(lk, before, mc.Loc)
		row["changes"] = diff

		if diff == nil {
			row["changes"] = []any{}
		}

		p := &Plan{
			Changes:       map[string]int{"update": 0, "unchanged": 0},
			Preview:       map[string]any{"rows": []any{row}},
			Fingerprinted: []any{before.normalized(), after.normalized()},
			State:         &patchState{before: before, after: after, apiId: apiId},
		}

		if changed {
			p.Changes["update"] = 1
			p.Count = 1
		} else {
			p.Changes["unchanged"] = 1
			p.Warnings = []string{"nothing would change"}
		}

		return p, nil
	}

	apply := func(p *Plan) (any, error) {
		st := p.State.(*patchState)

		if p.Count == 0 {
			return map[string]any{"updated": 0, "ids": []string{}}, nil
		}

		if _, err := mc.CallUpstream(api.Transactions.TransactionModifyHandler, "POST", nil, txnModifyBody(st.after)); err != nil {
			return nil, err
		}

		return txnBulkFinish(mc, &txnBulkState{Before: map[int64]txnState{st.apiId: st.before}, After: map[int64]txnState{st.apiId: st.after}, Changed: []int64{st.apiId}}, "edited 1 transaction")
	}

	return RunWrite(mc, body.WriteOpts, resolve, apply)
}

// ─── delete (admin) ────────────────────────────────────────────────────────────────────────────

func txnHandleDeleteOne(mc *Ctx) (any, error) {
	id, err := ResolveId("id", mc.Param("id"))

	if err != nil {
		return nil, err
	}

	var body struct {
		WriteOpts
	}

	if err := mc.BindBody(&body); err != nil {
		return nil, err
	}

	return txnDelete(mc, []int64{id}, body.WriteOpts)
}

func txnHandleDeleteBulk(mc *Ctx) (any, error) {
	var body struct {
		WriteOpts
		Ids []string `json:"ids"`
	}

	if err := mc.BindBody(&body); err != nil {
		return nil, err
	}

	if len(body.Ids) == 0 {
		return nil, Invalid("pass ids: [\"…\"] (bulk delete takes ids only, never a filter)", "no ids were given")
	}

	var ids []int64
	seen := map[int64]bool{}

	for _, s := range body.Ids {
		id, err := ResolveId("ids", s)

		if err != nil {
			return nil, err
		}

		if !seen[id] {
			seen[id] = true
			ids = append(ids, id)
		}
	}

	return txnDelete(mc, ids, body.WriteOpts)
}

// txnDelete is the shared delete: a delete of rows already gone is ok with deleted: 0 (§7.6)
func txnDelete(mc *Ctx, ids []int64, opts WriteOpts) (any, error) {
	type deleteState struct {
		ids    []int64
		states map[int64]*txnState
	}

	resolve := func() (*Plan, error) {
		lk, err := txnLoadLookup(mc)

		if err != nil {
			return nil, err
		}

		states, canon, missing, err := txnReadStates(mc, ids)

		if err != nil {
			return nil, err
		}

		st := &deleteState{states: states}
		seen := map[int64]bool{}
		rows := []map[string]any{}
		fp := []any{}

		for _, id := range ids {
			apiId, ok := canon[id]

			if !ok || seen[apiId] {
				continue
			}

			seen[apiId] = true
			st.ids = append(st.ids, apiId)
			rows = append(rows, txnRowSummary(lk, *states[apiId], mc.Loc))
			fp = append(fp, states[apiId].normalized())
		}

		preview := map[string]any{"rows": rows, "missing": txnInt64Strings(missing)}
		p := &Plan{
			Changes:       map[string]int{"delete": len(st.ids), "missing": len(missing)},
			Count:         len(st.ids),
			Preview:       preview,
			Fingerprinted: fp,
			State:         st,
		}

		if len(st.ids) > 0 {
			p.Warnings = append(p.Warnings, "undo re-creates deleted transactions with new ids; their pictures are not restored")
		}

		return p, nil
	}

	// nothing left to delete: answer ok with deleted: 0 without a token, so a retry loop ends
	if !opts.IsDryRun() {
		p, err := resolve()

		if err != nil {
			return nil, err
		}

		if p.Count == 0 {
			mc.SetMeta("dryRun", false)
			return &WriteResult{DryRun: false, Changes: p.Changes, Result: map[string]any{"deleted": 0, "ids": []string{}, "missing": p.Preview.(map[string]any)["missing"]}}, nil
		}
	}

	apply := func(p *Plan) (any, error) {
		st := p.State.(*deleteState)
		var done []int64
		var failure error

		for _, id := range st.ids {
			if _, err := mc.CallUpstream(api.Transactions.TransactionDeleteHandler, "POST", nil, map[string]any{"id": idString(id)}); err != nil {
				failure = err
				break
			}

			done = append(done, id)
		}

		if len(done) > 0 {
			prior := make([]txnState, 0, len(done))

			for _, id := range done {
				prior = append(prior, *st.states[id])
			}

			if _, err := RecordJournal(mc, fmt.Sprintf("deleted %d transactions", len(done)), len(done), []InverseOp{NewInverseOp(txnOpRecreate, prior, map[string]any{"ids": txnInt64Strings(done)})}); err != nil {
				return nil, err
			}
		}

		if failure != nil {
			f := toFail(failure)

			return nil, f.WithDetails(map[string]any{"deleted": txnInt64Strings(done), "failedId": idString(st.ids[len(done)]), "hint": "the rows already deleted are journaled; POST /undo re-creates them"})
		}

		return map[string]any{"deleted": len(done), "ids": txnInt64Strings(done)}, nil
	}

	return RunWrite(mc, opts, resolve, apply)
}

// ─── inverse executors (undo) ──────────────────────────────────────────────────────────────────

// txnCheckRows refuses when any row differs from the recorded state
func txnCheckRows(mc *Ctx, expected []txnState) (map[int64]*txnState, error) {
	ids := make([]int64, 0, len(expected))

	for _, s := range expected {
		id, err := strconv.ParseInt(s.Id, 10, 64)

		if err != nil {
			return nil, Conflict("the journal entry is damaged; it cannot be undone", "bad id %q in journal", s.Id)
		}

		ids = append(ids, id)
	}

	current, _, _, err := txnReadStates(mc, ids)

	if err != nil {
		return nil, err
	}

	for i, s := range expected {
		cur := current[ids[i]]

		if cur == nil {
			return nil, Conflict("the transaction was deleted since; undo cannot safely restore it", "transaction %s no longer exists", s.Id).WithDetails(map[string]any{"id": s.Id})
		}

		if !txnStatesEqual(*cur, s) {
			return nil, Conflict("the transaction was edited since the write (in the browser or by another call); undo will not overwrite a later edit", "transaction %s changed since the write", s.Id).WithDetails(map[string]any{"id": s.Id})
		}
	}

	return current, nil
}

// txnExecRestore writes rows back to recorded states after checking nobody edited them since
func txnExecRestore(mc *Ctx, payload json.RawMessage, check json.RawMessage) error {
	var target, expected []txnState

	if err := json.Unmarshal(payload, &target); err != nil {
		return err
	}

	if len(check) > 0 {
		if err := json.Unmarshal(check, &expected); err != nil {
			return err
		}

		if _, err := txnCheckRows(mc, expected); err != nil {
			return err
		}
	}

	for _, s := range target {
		id, _ := strconv.ParseInt(s.Id, 10, 64)
		current, _, _, err := txnReadStates(mc, []int64{id})

		if err != nil {
			return err
		}

		if cur := current[id]; cur != nil && txnStatesEqual(*cur, s) {
			continue
		}

		if _, err := mc.CallUpstream(api.Transactions.TransactionModifyHandler, "POST", nil, txnModifyBody(s)); err != nil {
			return err
		}
	}

	return nil
}

// txnExecDelete deletes rows this plane created; rows already gone are skipped
func txnExecDelete(mc *Ctx, payload json.RawMessage, check json.RawMessage) error {
	var p struct {
		Ids []string `json:"ids"`
	}

	if err := json.Unmarshal(payload, &p); err != nil {
		return err
	}

	var expected []txnState

	if len(check) > 0 {
		if err := json.Unmarshal(check, &expected); err != nil {
			return err
		}
	}

	byId := map[string]txnState{}

	for _, s := range expected {
		byId[s.Id] = s
	}

	ids := make([]int64, 0, len(p.Ids))

	for _, s := range p.Ids {
		id, err := strconv.ParseInt(s, 10, 64)

		if err != nil {
			return Conflict("the journal entry is damaged; it cannot be undone", "bad id %q in journal", s)
		}

		ids = append(ids, id)
	}

	current, _, _, err := txnReadStates(mc, ids)

	if err != nil {
		return err
	}

	// check every row first, so a refusal changes nothing
	for _, id := range ids {
		cur := current[id]

		if cur == nil {
			continue
		}

		if want, ok := byId[idString(id)]; ok && !txnStatesEqual(*cur, want) {
			return Conflict("the transaction was edited since it was created; undo will not delete a later edit — delete it yourself if intended", "transaction %s changed since it was created", idString(id)).WithDetails(map[string]any{"id": idString(id)})
		}
	}

	for _, id := range ids {
		if current[id] == nil {
			continue
		}

		if _, err := mc.CallUpstream(api.Transactions.TransactionDeleteHandler, "POST", nil, map[string]any{"id": idString(id)}); err != nil {
			return err
		}
	}

	return nil
}

// txnExecRecreate re-creates deleted rows (new ids) after checking they are still deleted
func txnExecRecreate(mc *Ctx, payload json.RawMessage, check json.RawMessage) error {
	var rows []txnState

	if err := json.Unmarshal(payload, &rows); err != nil {
		return err
	}

	ids := make([]int64, 0, len(rows))

	for _, s := range rows {
		id, _ := strconv.ParseInt(s.Id, 10, 64)
		ids = append(ids, id)
	}

	current, _, _, err := txnReadStates(mc, ids)

	if err != nil {
		return err
	}

	for _, id := range ids {
		if current[id] != nil {
			return Conflict("the transaction exists again; nothing to re-create", "transaction %s is not deleted", idString(id))
		}
	}

	for _, s := range rows {
		req := txnCreateBodyFromState(s)

		if _, err := mc.CallUpstream(api.Transactions.TransactionCreateHandler, "POST", nil, req); err != nil {
			return err
		}
	}

	return nil
}

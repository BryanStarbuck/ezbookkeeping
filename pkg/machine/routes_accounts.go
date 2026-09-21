package machine

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"xorm.io/xorm"

	"github.com/mayswind/ezbookkeeping/pkg/api"
	"github.com/mayswind/ezbookkeeping/pkg/core"
	"github.com/mayswind/ezbookkeeping/pkg/models"
	"github.com/mayswind/ezbookkeeping/pkg/services"
	"github.com/mayswind/ezbookkeeping/pkg/validators"
)

// routes_accounts.go — the account family (apis.mdx §10.2): list, one, properties, create (with an
// initial balance and sub-accounts), batch create (§15 builds on it), patch, hide, move, delete and
// sub-account delete. The balance, balance-history and reconciliation routes belong to other
// families and are not here.
//
// Money on these routes is upstream's: a balance is the signed integer number of hundredths in the
// account's own currency (a liability's money owed is negative, as upstream stores it). A parent
// account with sub-accounts holds no balance of its own, so its balance is null, never 0 (R3).

// ---------------------------------------------------------------------------------------------
// categories of account, and the side of the balance sheet they land on
// ---------------------------------------------------------------------------------------------

var refAccountCategoryNames = map[models.AccountCategory]string{
	models.ACCOUNT_CATEGORY_CASH:                   "cash",
	models.ACCOUNT_CATEGORY_CHECKING_ACCOUNT:       "checking",
	models.ACCOUNT_CATEGORY_CREDIT_CARD:            "credit_card",
	models.ACCOUNT_CATEGORY_VIRTUAL:                "virtual",
	models.ACCOUNT_CATEGORY_DEBT:                   "debt",
	models.ACCOUNT_CATEGORY_RECEIVABLES:            "receivables",
	models.ACCOUNT_CATEGORY_INVESTMENT:             "investment",
	models.ACCOUNT_CATEGORY_SAVINGS_ACCOUNT:        "savings",
	models.ACCOUNT_CATEGORY_CERTIFICATE_OF_DEPOSIT: "certificate_of_deposit",
}

// refAccountCategoryList is the argument vocabulary, for hints
const refAccountCategoryList = "cash, checking, savings, credit_card, virtual, debt, receivables, investment, certificate_of_deposit"

// refAccountCategory parses an account category name (or its upstream number)
func refAccountCategory(v string) (models.AccountCategory, error) {
	s := strings.ToLower(strings.TrimSpace(v))
	s = strings.NewReplacer("-", "_", " ", "_").Replace(s)

	switch s {
	case "checking_account":
		s = "checking"
	case "savings_account", "saving":
		s = "savings"
	case "creditcard", "credit":
		s = "credit_card"
	case "cd":
		s = "certificate_of_deposit"
	}

	for c, name := range refAccountCategoryNames {
		if name == s || strconv.Itoa(int(c)) == s {
			return c, nil
		}
	}

	return 0, Invalid("category is one of: "+refAccountCategoryList, "unknown account category %q", v)
}

// refAccountSide is asset or liability (pkg/models/account.go asset/liability maps)
func refAccountSide(c models.AccountCategory) string {
	if c.IsLiability() {
		return "liability"
	}

	return "asset"
}

// refCurrency validates an ISO 4217 code the app knows
func refCurrency(v string) (string, error) {
	s := strings.ToUpper(strings.TrimSpace(v))

	if !validators.AllCurrencyNames[s] {
		return "", Invalid("currencies are ISO 4217 codes the app knows, e.g. USD, EUR, JPY", "unknown currency %q", v)
	}

	return s, nil
}

// ---------------------------------------------------------------------------------------------
// loading, resolving and rendering
// ---------------------------------------------------------------------------------------------

func refLoadAccounts(mc *Ctx) ([]*models.Account, error) {
	return services.Accounts.GetAllAccountsByUid(mc.Web, mc.Uid)
}

func refAccountName(a *models.Account) string { return a.Name }
func refAccountId(a *models.Account) int64    { return a.AccountId }

// refResolveAccount resolves an <id|name> among the bound user's accounts (sub-accounts included)
func refResolveAccount(all []*models.Account, ref string) (*models.Account, error) {
	byId := map[int64]*models.Account{}

	for _, a := range all {
		byId[a.AccountId] = a
	}

	parentOf := func(a *models.Account) string {
		if p, ok := byId[a.ParentAccountId]; ok {
			return p.Name
		}

		return ""
	}

	return refResolveOne("account", ref, all, refAccountId, refAccountName, parentOf, "GET /machine/v1/accounts?include_hidden=true lists them — or pass account_name")
}

// refAccountView is one account on the wire
type refAccountView struct {
	Id                      string            `json:"id"`
	Name                    string            `json:"name"`
	ParentId                string            `json:"parentId"`
	ParentName              string            `json:"parentName,omitempty"`
	Category                string            `json:"category"`
	CategoryCode            int               `json:"categoryCode"`
	Side                    string            `json:"side"`
	IsAsset                 bool              `json:"isAsset"`
	IsLiability             bool              `json:"isLiability"`
	Type                    string            `json:"type"`
	Currency                string            `json:"currency"`
	Balance                 *int64            `json:"balance"`
	Hidden                  bool              `json:"hidden"`
	Icon                    string            `json:"icon"`
	IconType                int               `json:"iconType"`
	Color                   string            `json:"color"`
	Comment                 string            `json:"comment"`
	DisplayOrder            int32             `json:"displayOrder"`
	LastReconciledTime      *int64            `json:"lastReconciledTime,omitempty"`
	LastReconciledDate      *string           `json:"lastReconciledDate,omitempty"`
	CreditCardStatementDate *int              `json:"creditCardStatementDate,omitempty"`
	CreditCardLimit         *int64            `json:"creditCardLimit,omitempty"`
	SubAccounts             []*refAccountView `json:"subAccounts,omitempty"`
}

func refAccountViewOf(a *models.Account, parentName string, loc *time.Location) *refAccountView {
	v := &refAccountView{
		Id: idString(a.AccountId), Name: a.Name, ParentId: idString(a.ParentAccountId), ParentName: parentName,
		Category: refAccountCategoryNames[a.Category], CategoryCode: int(a.Category), Side: refAccountSide(a.Category),
		IsAsset: a.Category.IsAsset(), IsLiability: a.Category.IsLiability(), Type: "single", Currency: a.Currency,
		Hidden: a.Hidden, Icon: strconv.FormatInt(a.Icon, 10), IconType: int(a.IconType), Color: a.Color, Comment: a.Comment, DisplayOrder: a.DisplayOrder,
	}

	if a.Type == models.ACCOUNT_TYPE_MULTI_SUB_ACCOUNTS {
		v.Type = "parent"
	} else {
		b := a.Balance
		v.Balance = &b
	}

	if a.Extend != nil && a.Extend.LastReconciledTime != nil && *a.Extend.LastReconciledTime > 0 {
		t := *a.Extend.LastReconciledTime
		d := DateOfUnix(t, loc)
		v.LastReconciledTime, v.LastReconciledDate = &t, &d
	}

	if a.ParentAccountId == models.LevelOneAccountParentId && a.Category == models.ACCOUNT_CATEGORY_CREDIT_CARD {
		date, limit := 0, int64(0)

		if a.Extend != nil && a.Extend.CreditCardStatementDate != nil {
			date = *a.Extend.CreditCardStatementDate
		}

		if a.Extend != nil && a.Extend.CreditCardLimit != nil {
			limit = *a.Extend.CreditCardLimit
		}

		v.CreditCardStatementDate, v.CreditCardLimit = &date, &limit
	}

	return v
}

func refSortAccounts(list []*models.Account) {
	sort.SliceStable(list, func(i, j int) bool {
		if list[i].Category != list[j].Category {
			return list[i].Category < list[j].Category
		}

		if list[i].DisplayOrder != list[j].DisplayOrder {
			return list[i].DisplayOrder < list[j].DisplayOrder
		}

		return list[i].AccountId < list[j].AccountId
	})
}

// refAccountTree renders top-level accounts with their sub-accounts nested
func refAccountTree(all []*models.Account, includeHidden bool, loc *time.Location) []*refAccountView {
	var tops []*models.Account
	children := map[int64][]*models.Account{}

	for _, a := range all {
		if !includeHidden && a.Hidden {
			continue
		}

		if a.ParentAccountId == models.LevelOneAccountParentId {
			tops = append(tops, a)
		} else {
			children[a.ParentAccountId] = append(children[a.ParentAccountId], a)
		}
	}

	refSortAccounts(tops)
	out := make([]*refAccountView, 0, len(tops))

	for _, t := range tops {
		v := refAccountViewOf(t, "", loc)
		subs := children[t.AccountId]
		refSortAccounts(subs)

		for _, s := range subs {
			v.SubAccounts = append(v.SubAccounts, refAccountViewOf(s, t.Name, loc))
		}

		out = append(out, v)
	}

	return out
}

// refAccountMatches applies the list filters to one top-level account (and its sub-accounts): a
// parent matches a currency filter when any of its sub-accounts is in that currency
func refAccountMatches(v *refAccountView, category, currency string) bool {
	if category != "" && v.Category != category {
		return false
	}

	if currency == "" || v.Currency == currency {
		return true
	}

	for _, s := range v.SubAccounts {
		if s.Currency == currency {
			return true
		}
	}

	return false
}

// refHandleAccountList is GET /accounts
func refHandleAccountList(mc *Ctx) (any, error) {
	includeHidden, err := mc.QueryBool("include_hidden", false)

	if err != nil {
		return nil, err
	}

	withSubs, err := mc.QueryBool("with_sub_accounts", true)

	if err != nil {
		return nil, err
	}

	filters := map[string]any{"includeHidden": includeHidden, "withSubAccounts": withSubs}
	category := ""

	if v := mc.Query("category"); v != "" {
		c, err := refAccountCategory(v)

		if err != nil {
			return nil, err
		}

		category = refAccountCategoryNames[c]
		filters["category"] = category
	}

	currency := ""

	if v := mc.Query("currency"); v != "" {
		if currency, err = refCurrency(v); err != nil {
			return nil, err
		}

		filters["currency"] = currency
	}

	all, err := refLoadAccounts(mc)

	if err != nil {
		return nil, err
	}

	// a name filter answers flat rows, so a sub-account can be found by name
	if name := mc.Query("name"); name != "" {
		byId := map[int64]*models.Account{}

		for _, a := range all {
			byId[a.AccountId] = a
		}

		var pool []*models.Account

		for _, a := range all {
			if includeHidden || !a.Hidden {
				pool = append(pool, a)
			}
		}

		pool = refMatchByName(pool, name, refAccountName)
		refSortAccounts(pool)
		rows := make([]*refAccountView, 0, len(pool))

		for _, a := range pool {
			parentName := ""

			if p, ok := byId[a.ParentAccountId]; ok {
				parentName = p.Name
			}

			v := refAccountViewOf(a, parentName, mc.Loc)

			if refAccountMatches(v, category, currency) {
				rows = append(rows, v)
			}
		}

		filters["name"] = name

		return map[string]any{"accounts": rows, "count": len(rows), "filters": filters}, nil
	}

	tree := refAccountTree(all, includeHidden, mc.Loc)
	rows := make([]*refAccountView, 0, len(tree))
	count := 0

	for _, v := range tree {
		if !refAccountMatches(v, category, currency) {
			continue
		}

		if currency != "" && v.Currency != currency {
			var kept []*refAccountView

			for _, s := range v.SubAccounts {
				if s.Currency == currency {
					kept = append(kept, s)
				}
			}

			v.SubAccounts = kept
		}

		count += 1 + len(v.SubAccounts)

		if !withSubs {
			v.SubAccounts = nil
		}

		rows = append(rows, v)
	}

	currencies := map[string]bool{}

	for _, v := range rows {
		if v.Balance != nil {
			currencies[v.Currency] = true
		}

		for _, s := range v.SubAccounts {
			currencies[s.Currency] = true
		}
	}

	curList := make([]string, 0, len(currencies))

	for c := range currencies {
		curList = append(curList, c)
	}

	sort.Strings(curList)

	return map[string]any{"accounts": rows, "count": len(rows), "accountCount": count, "currencies": curList, "filters": filters,
		"note": "balances are per account in its own currency; no total is given across currencies (GET /machine/v1/analytics/net-worth converts with named rates)"}, nil
}

// refHandleAccountGet is GET /accounts/:id (an id or a name; a sub-account works too)
func refHandleAccountGet(mc *Ctx) (any, error) {
	all, err := refLoadAccounts(mc)

	if err != nil {
		return nil, err
	}

	a, err := refResolveAccount(all, mc.Param("id"))

	if err != nil {
		return nil, err
	}

	return map[string]any{"account": refAccountFull(all, a, mc.Loc)}, nil
}

// refAccountFull renders one account with its parent name or its sub-accounts
func refAccountFull(all []*models.Account, a *models.Account, loc *time.Location) *refAccountView {
	parentName := ""
	var subs []*models.Account

	for _, o := range all {
		if o.AccountId == a.ParentAccountId {
			parentName = o.Name
		}

		if o.ParentAccountId == a.AccountId && a.ParentAccountId == models.LevelOneAccountParentId {
			subs = append(subs, o)
		}
	}

	v := refAccountViewOf(a, parentName, loc)
	refSortAccounts(subs)

	for _, s := range subs {
		v.SubAccounts = append(v.SubAccounts, refAccountViewOf(s, a.Name, loc))
	}

	return v
}

// refAccountIdsOf returns an account and its sub-accounts
func refAccountIdsOf(all []*models.Account, a *models.Account) []int64 {
	ids := []int64{a.AccountId}

	if a.ParentAccountId == models.LevelOneAccountParentId {
		for _, o := range all {
			if o.ParentAccountId == a.AccountId {
				ids = append(ids, o.AccountId)
			}
		}
	}

	return ids
}

// refHandleAccountProperties is GET /accounts/:id/properties
func refHandleAccountProperties(mc *Ctx) (any, error) {
	all, err := refLoadAccounts(mc)

	if err != nil {
		return nil, err
	}

	a, err := refResolveAccount(all, mc.Param("id"))

	if err != nil {
		return nil, err
	}

	ids := refAccountIdsOf(all, a)
	count, err := services.Transactions.GetTransactionCount(mc.Web, mc.Uid, 0, 0, 0, nil, ids, nil, false, "", "", core.MATCH_MODE_DEFAULT, false)

	if err != nil {
		return nil, err
	}

	edge := func(order string) (*models.Transaction, error) {
		t := &models.Transaction{}
		has, err := refDB(mc).NewSession(mc.Web).Cols("transaction_id", "transaction_time", "type", "account_id", "amount").Where("uid=? AND deleted=?", mc.Uid, false).In("account_id", ids).OrderBy(order).Limit(1).Get(t)

		if err != nil || !has {
			return nil, err
		}

		return t, nil
	}

	first, err := edge("transaction_time asc")

	if err != nil {
		return nil, err
	}

	last, err := edge("transaction_time desc")

	if err != nil {
		return nil, err
	}

	stamp := func(t *models.Transaction) any {
		if t == nil {
			return nil
		}

		return map[string]any{"transactionTime": t.TransactionTime, "date": DateOfUnixMilli(t.TransactionTime, mc.Loc), "id": idString(t.TransactionId)}
	}

	v := refAccountViewOf(a, "", mc.Loc)
	out := map[string]any{
		"accountId": v.Id, "name": a.Name, "category": v.Category, "side": v.Side, "type": v.Type, "currency": a.Currency,
		"accountIds": refIdStrings(ids), "transactionCount": count,
		"firstTransaction": stamp(first), "lastTransaction": stamp(last),
		"lastReconciledTime": nil, "lastReconciledDate": nil,
	}

	if v.LastReconciledTime != nil {
		out["lastReconciledTime"], out["lastReconciledDate"] = *v.LastReconciledTime, *v.LastReconciledDate
	}

	if v.CreditCardStatementDate != nil {
		out["creditCardStatementDate"] = *v.CreditCardStatementDate
		out["creditCardLimit"] = *v.CreditCardLimit
	}

	if a.Type == models.ACCOUNT_TYPE_SINGLE_ACCOUNT {
		var opening []*models.Transaction

		if err := refDB(mc).NewSession(mc.Web).Cols("transaction_id", "transaction_time", "amount").Where("uid=? AND deleted=? AND account_id=? AND type=?", mc.Uid, false, a.AccountId, models.TRANSACTION_DB_TYPE_MODIFY_BALANCE).OrderBy("transaction_time asc").Find(&opening); err != nil {
			return nil, err
		}

		rows := make([]map[string]any, 0, len(opening))

		for _, t := range opening {
			rows = append(rows, map[string]any{"id": idString(t.TransactionId), "amount": t.Amount, "currency": a.Currency, "date": DateOfUnixMilli(t.TransactionTime, mc.Loc)})
		}

		out["balanceModifications"] = rows
	}

	return out, nil
}

// ---------------------------------------------------------------------------------------------
// create (single, with sub-accounts, and batch)
// ---------------------------------------------------------------------------------------------

// refAccountSpec is one account to create (POST /accounts body, and each row of /accounts/batch)
type refAccountSpec struct {
	Name                    string           `json:"name"`
	Category                string           `json:"category"`
	Currency                string           `json:"currency"`
	InitialBalance          *json.Number     `json:"initial_balance"`
	InitialBalanceDate      string           `json:"initial_balance_date"`
	Color                   string           `json:"color"`
	Icon                    refFlex          `json:"icon"`
	Comment                 string           `json:"comment"`
	CreditCardStatementDate *int             `json:"credit_card_statement_date"`
	CreditCardLimit         *json.Number     `json:"credit_card_limit"`
	SubAccounts             []refSubAcctSpec `json:"sub_accounts"`
	Hidden                  *bool            `json:"hidden"`
}

type refSubAcctSpec struct {
	Name               string       `json:"name"`
	Currency           string       `json:"currency"`
	InitialBalance     *json.Number `json:"initial_balance"`
	InitialBalanceDate string       `json:"initial_balance_date"`
	Color              string       `json:"color"`
	Icon               refFlex      `json:"icon"`
	Comment            string       `json:"comment"`
}

// refPlannedAccount is the resolved create: upstream's request plus what the preview shows
type refPlannedAccount struct {
	Request models.AccountCreateRequest
	Preview map[string]any
}

// refBalanceTime resolves an initial balance and its date. A non-zero balance with no date is
// dated today (said in a warning); the time is the start of that day in loc.
func refBalanceTime(label string, amount *json.Number, date string, loc *time.Location, now time.Time) (int64, int64, string, []string, error) {
	if amount == nil {
		if strings.TrimSpace(date) != "" {
			return 0, 0, "", nil, Invalid("initial_balance_date needs initial_balance", "%s: a date was given without an initial balance", label)
		}

		return 0, 0, "", nil, nil
	}

	n, err := AmountArg("initial_balance", *amount)

	if err != nil {
		return 0, 0, "", nil, err
	}

	if n == 0 {
		return 0, 0, "", nil, nil
	}

	var warnings []string
	d := strings.TrimSpace(date)

	if d == "" {
		d = now.In(loc).Format("2006-01-02")
		warnings = append(warnings, fmt.Sprintf("%s: initial_balance_date not given; the opening balance is dated today (%s)", label, d))
	}

	t, err := ParseDate("initial_balance_date", d, loc)

	if err != nil {
		return 0, 0, "", nil, err
	}

	return n, t.Unix(), t.Format("2006-01-02"), warnings, nil
}

// refPlanAccountCreate resolves one create spec into upstream's AccountCreateRequest — every
// validation upstream's AccountCreateHandler runs, first, with a hint that names the fix. It is
// the write handler's own first half: the preview IS the request (apis.mdx §9.2).
func refPlanAccountCreate(spec *refAccountSpec, existing []*models.Account, defaultCurrency string, loc *time.Location, now time.Time) (*refPlannedAccount, []string, error) {
	var warnings []string

	name, err := refName("account", spec.Name)

	if err != nil {
		return nil, nil, err
	}

	if strings.TrimSpace(spec.Category) == "" {
		return nil, nil, Invalid("pass category: one of "+refAccountCategoryList+" (it decides asset or liability)", "category is required")
	}

	cat, err := refAccountCategory(spec.Category)

	if err != nil {
		return nil, nil, err
	}

	if spec.Hidden != nil {
		return nil, nil, Invalid("create it, then POST /machine/v1/accounts/:id/hide", "hidden is not a create field")
	}

	color, err := refColor(spec.Color, refDefaultColor)

	if err != nil {
		return nil, nil, err
	}

	icon, err := refIcon(spec.Icon, refDefaultIcon)

	if err != nil {
		return nil, nil, err
	}

	comment, err := refComment(spec.Comment)

	if err != nil {
		return nil, nil, err
	}

	for _, a := range existing {
		if strings.EqualFold(a.Name, name) {
			warnings = append(warnings, fmt.Sprintf("an account named %q already exists (id %s); this creates another", a.Name, idString(a.AccountId)))
		}
	}

	req := models.AccountCreateRequest{Name: name, Category: cat, Icon: icon, IconType: core.ICON_TYPE_SYSTEM, Color: color, Comment: comment}
	preview := map[string]any{
		"action": "create", "name": name, "category": refAccountCategoryNames[cat], "side": refAccountSide(cat),
		"color": color, "icon": strconv.FormatInt(icon, 10), "comment": comment,
	}

	// credit-card extras
	if spec.CreditCardStatementDate != nil {
		if cat != models.ACCOUNT_CATEGORY_CREDIT_CARD {
			return nil, nil, Invalid("only a credit_card account has a statement date", "credit_card_statement_date given for a %s account", refAccountCategoryNames[cat])
		}

		if *spec.CreditCardStatementDate < 0 || *spec.CreditCardStatementDate > 28 {
			return nil, nil, Invalid("the statement date is a day of the month, 1-28 (0 = not set)", "credit_card_statement_date %d is out of range", *spec.CreditCardStatementDate)
		}

		req.CreditCardStatementDate = *spec.CreditCardStatementDate
		preview["creditCardStatementDate"] = req.CreditCardStatementDate
	}

	limit := int64(0)

	if spec.CreditCardLimit != nil {
		if cat != models.ACCOUNT_CATEGORY_CREDIT_CARD {
			return nil, nil, Invalid("only a credit_card account has a credit limit", "credit_card_limit given for a %s account", refAccountCategoryNames[cat])
		}

		limit, err = AmountArg("credit_card_limit", *spec.CreditCardLimit)

		if err != nil {
			return nil, nil, err
		}

		if limit < 0 {
			return nil, nil, Invalid("the credit limit is a positive number of hundredths", "credit_card_limit %d is negative", limit)
		}

		if limit > 0 {
			req.CreditCardLimit = strconv.FormatInt(limit, 10)
			preview["creditCardLimit"] = limit
		}
	}

	signWarning := func(label string, amount int64) {
		if cat.IsLiability() && amount > 0 {
			warnings = append(warnings, fmt.Sprintf("%s: a POSITIVE balance on a %s (liability) account means you are owed money; money you owe is NEGATIVE (upstream's sign convention)", label, refAccountCategoryNames[cat]))
		}
	}

	if len(spec.SubAccounts) == 0 {
		req.Type = models.ACCOUNT_TYPE_SINGLE_ACCOUNT
		currency := spec.Currency

		if strings.TrimSpace(currency) == "" {
			currency = defaultCurrency
			warnings = append(warnings, fmt.Sprintf("currency not given: defaulted to %s (the bound user's default); currency CANNOT be changed after creation", defaultCurrency))
		}

		if req.Currency, err = refCurrency(currency); err != nil {
			return nil, nil, err
		}

		balance, balanceTime, balanceDate, w, err := refBalanceTime(name, spec.InitialBalance, spec.InitialBalanceDate, loc, now)

		if err != nil {
			return nil, nil, err
		}

		warnings = append(warnings, w...)
		req.Balance, req.BalanceTime = strconv.FormatInt(balance, 10), balanceTime

		if balance == 0 {
			req.Balance = ""
		}

		signWarning(name, balance)
		preview["type"] = "single"
		preview["currency"] = req.Currency
		preview["initialBalance"] = balance

		if balanceDate != "" {
			preview["initialBalanceDate"] = balanceDate
		}

		return &refPlannedAccount{Request: req, Preview: preview}, warnings, nil
	}

	// a parent with sub-accounts
	req.Type = models.ACCOUNT_TYPE_MULTI_SUB_ACCOUNTS

	if spec.InitialBalance != nil {
		if n, _ := AmountArg("initial_balance", *spec.InitialBalance); n != 0 {
			return nil, nil, Invalid("a parent account holds no balance of its own: put initial_balance on each sub-account", "initial_balance given for an account with sub-accounts")
		}
	}

	subDefaultCurrency := strings.TrimSpace(spec.Currency)
	req.Currency = core.AccountCurrencyNotSetValue

	if cat == models.ACCOUNT_CATEGORY_CREDIT_CARD && limit > 0 {
		cur := subDefaultCurrency

		if cur == "" {
			cur = defaultCurrency
			warnings = append(warnings, fmt.Sprintf("a credit limit needs the parent's currency: defaulted to %s", defaultCurrency))
		}

		if req.Currency, err = refCurrency(cur); err != nil {
			return nil, nil, err
		}
	}

	subPreview := make([]map[string]any, 0, len(spec.SubAccounts))
	seen := map[string]bool{}

	for i, s := range spec.SubAccounts {
		label := fmt.Sprintf("sub_accounts[%d]", i)
		sname, err := refName("sub-account", s.Name)

		if err != nil {
			return nil, nil, Invalid(label+" needs a non-empty name of at most 64 characters", "%s", err.(*Fail).Message)
		}

		if seen[strings.ToLower(sname)] {
			warnings = append(warnings, fmt.Sprintf("two sub-accounts are named %q", sname))
		}

		seen[strings.ToLower(sname)] = true
		currency := strings.TrimSpace(s.Currency)

		if currency == "" {
			currency = subDefaultCurrency
		}

		if currency == "" {
			currency = defaultCurrency
			warnings = append(warnings, fmt.Sprintf("%s %q: currency not given, defaulted to %s; it cannot be changed after creation", label, sname, defaultCurrency))
		}

		cur, err := refCurrency(currency)

		if err != nil {
			return nil, nil, err
		}

		scolor, err := refColor(s.Color, color)

		if err != nil {
			return nil, nil, err
		}

		sicon, err := refIcon(s.Icon, icon)

		if err != nil {
			return nil, nil, err
		}

		scomment, err := refComment(s.Comment)

		if err != nil {
			return nil, nil, err
		}

		date := s.InitialBalanceDate

		if strings.TrimSpace(date) == "" {
			date = spec.InitialBalanceDate
		}

		balance, balanceTime, balanceDate, w, err := refBalanceTime(label+" "+sname, s.InitialBalance, date, loc, now)

		if err != nil {
			return nil, nil, err
		}

		warnings = append(warnings, w...)
		signWarning(sname, balance)

		sub := &models.AccountCreateRequest{Name: sname, Category: cat, Type: models.ACCOUNT_TYPE_SINGLE_ACCOUNT, Icon: sicon, IconType: core.ICON_TYPE_SYSTEM, Color: scolor, Currency: cur, Comment: scomment, BalanceTime: balanceTime}

		if balance != 0 {
			sub.Balance = strconv.FormatInt(balance, 10)
		}

		req.SubAccounts = append(req.SubAccounts, sub)
		sp := map[string]any{"name": sname, "currency": cur, "initialBalance": balance}

		if balanceDate != "" {
			sp["initialBalanceDate"] = balanceDate
		}

		subPreview = append(subPreview, sp)
	}

	preview["type"] = "parent"
	preview["currency"] = req.Currency
	preview["subAccounts"] = subPreview

	return &refPlannedAccount{Request: req, Preview: preview}, warnings, nil
}

type refAccountCreateReq struct {
	WriteOpts
	refAccountSpec
}

// refCreateOneAccount calls upstream's AccountCreateHandler and returns the created main id and
// every created id (main + sub-accounts)
func refCreateOneAccount(mc *Ctx, req models.AccountCreateRequest, clientSessionId string) (int64, []int64, error) {
	req.ClientSessionId = clientSessionId
	res, err := mc.CallUpstream(api.Accounts.AccountCreateHandler, "POST", nil, req)

	if err != nil {
		return 0, nil, err
	}

	id, err := refCreatedId(res)

	if err != nil {
		return 0, nil, err
	}

	family, err := services.Accounts.GetAccountAndSubAccountsByAccountId(mc.Web, mc.Uid, id)

	if err != nil {
		return id, []int64{id}, nil
	}

	ids := []int64{id}

	for _, a := range family {
		if a.AccountId != id {
			ids = append(ids, a.AccountId)
		}
	}

	return id, ids, nil
}

// refHandleAccountCreate is POST /accounts
func refHandleAccountCreate(mc *Ctx) (any, error) {
	var req refAccountCreateReq

	if err := mc.BindBody(&req); err != nil {
		return nil, err
	}

	if err := refCheckIdemKey(req.IdempotencyKey); err != nil {
		return nil, err
	}

	now := time.Now()

	return RunWrite(mc, req.WriteOpts, func() (*Plan, error) {
		existing, err := refLoadAccounts(mc)

		if err != nil {
			return nil, err
		}

		planned, warnings, err := refPlanAccountCreate(&req.refAccountSpec, existing, mc.User.DefaultCurrency, mc.Loc, now)

		if err != nil {
			return nil, err
		}

		return &Plan{Changes: map[string]int{"create": 1 + len(planned.Request.SubAccounts)}, Count: 1, Preview: planned.Preview, Fingerprinted: planned.Request, Warnings: warnings, State: planned}, nil
	}, func(p *Plan) (any, error) {
		if v, ok := refIdemGet(mc, req.IdempotencyKey); ok {
			return v, nil
		}

		planned := p.State.(*refPlannedAccount)
		id, ids, err := refCreateOneAccount(mc, planned.Request, req.IdempotencyKey)

		if err != nil {
			return nil, err
		}

		if err := refJournalCreate(mc, "account", []int64{id}, ids, "create account "+idString(id)); err != nil {
			return nil, err
		}

		out, err := refAccountResult(mc, id)

		if err != nil {
			return nil, err
		}

		refIdemPut(mc, req.IdempotencyKey, out)

		return out, nil
	})
}

func refAccountResult(mc *Ctx, id int64) (map[string]any, error) {
	all, err := refLoadAccounts(mc)

	if err != nil {
		return nil, err
	}

	for _, a := range all {
		if a.AccountId == id {
			return map[string]any{"account": refAccountFull(all, a, mc.Loc)}, nil
		}
	}

	return map[string]any{"account": map[string]any{"id": idString(id)}}, nil
}

// refAccountBatchReq is POST /accounts/batch — §15's provisioning builds on it
type refAccountBatchReq struct {
	WriteOpts
	Accounts []refAccountSpec `json:"accounts"`
}

// refAccountBatchMax bounds one batch; the change ceiling (max_changes) is the real limit
const refAccountBatchMax = 500

// refHandleAccountBatch is POST /accounts/batch: all or nothing, honestly bounded — accounts are
// created in order, and on the first failure the ones already created are deleted again (they hold
// only their opening balances) and the response names the failing index
func refHandleAccountBatch(mc *Ctx) (any, error) {
	var req refAccountBatchReq

	if err := mc.BindBody(&req); err != nil {
		return nil, err
	}

	if err := refCheckIdemKey(req.IdempotencyKey); err != nil {
		return nil, err
	}

	if len(req.Accounts) == 0 {
		return nil, Invalid("pass accounts: [{name, category, currency, …}]", "no accounts to create")
	}

	if len(req.Accounts) > refAccountBatchMax {
		return nil, Invalid(fmt.Sprintf("split the batch into chunks of %d", refAccountBatchMax), "%d accounts in one batch", len(req.Accounts))
	}

	now := time.Now()

	return RunWrite(mc, req.WriteOpts, func() (*Plan, error) {
		existing, err := refLoadAccounts(mc)

		if err != nil {
			return nil, err
		}

		var planned []*refPlannedAccount
		var preview []map[string]any
		var warnings []string
		names := map[string]int{}
		total := 0

		for i := range req.Accounts {
			pa, w, err := refPlanAccountCreate(&req.Accounts[i], existing, mc.User.DefaultCurrency, mc.Loc, now)

			if err != nil {
				f := toFail(err)

				return nil, (&Fail{Code: f.Code, Message: fmt.Sprintf("accounts[%d]: %s", i, f.Message), Hint: f.Hint, Details: f.Details}).WithDetails(map[string]any{"index": i, "details": f.Details})
			}

			if j, dup := names[strings.ToLower(pa.Request.Name)]; dup {
				warnings = append(warnings, fmt.Sprintf("accounts[%d] and accounts[%d] are both named %q", j, i, pa.Request.Name))
			}

			names[strings.ToLower(pa.Request.Name)] = i

			for _, s := range w {
				warnings = append(warnings, fmt.Sprintf("accounts[%d]: %s", i, s))
			}

			pa.Preview["index"] = i
			planned = append(planned, pa)
			preview = append(preview, pa.Preview)
			total += 1 + len(pa.Request.SubAccounts)
		}

		fp := make([]models.AccountCreateRequest, 0, len(planned))

		for _, pa := range planned {
			fp = append(fp, pa.Request)
		}

		return &Plan{Changes: map[string]int{"create": total}, Count: len(planned), Preview: preview, Fingerprinted: fp, Warnings: warnings, State: planned}, nil
	}, func(p *Plan) (any, error) {
		if v, ok := refIdemGet(mc, req.IdempotencyKey); ok {
			return v, nil
		}

		planned := p.State.([]*refPlannedAccount)
		var mains, all []int64

		for i, pa := range planned {
			session := ""

			if req.IdempotencyKey != "" {
				session = req.IdempotencyKey + "#" + strconv.Itoa(i)
			}

			id, ids, err := refCreateOneAccount(mc, pa.Request, session)

			if err != nil {
				rolledBack := 0

				for j := len(mains) - 1; j >= 0; j-- {
					if derr := services.Accounts.DeleteAccount(mc.Web, mc.Uid, mains[j]); derr == nil {
						rolledBack++
					}
				}

				f := toFail(err)

				return nil, (&Fail{Code: f.Code, Message: fmt.Sprintf("accounts[%d] (%q) failed: %s", i, pa.Request.Name, f.Message), Hint: f.Hint, UpstreamCode: f.UpstreamCode}).WithDetails(map[string]any{"failedIndex": i, "rolledBack": rolledBack, "created": len(mains)})
			}

			mains = append(mains, id)
			all = append(all, ids...)
		}

		if err := refJournalCreate(mc, "account", mains, all, fmt.Sprintf("create %d accounts", len(mains))); err != nil {
			return nil, err
		}

		accounts, err := refLoadAccounts(mc)

		if err != nil {
			return nil, err
		}

		byId := map[int64]*models.Account{}

		for _, a := range accounts {
			byId[a.AccountId] = a
		}

		rows := make([]*refAccountView, 0, len(mains))

		for _, id := range mains {
			if a, ok := byId[id]; ok {
				rows = append(rows, refAccountFull(accounts, a, mc.Loc))
			}
		}

		out := map[string]any{"accounts": rows, "created": len(mains), "rolledBack": 0}
		refIdemPut(mc, req.IdempotencyKey, out)

		return out, nil
	})
}

// ---------------------------------------------------------------------------------------------
// patch
// ---------------------------------------------------------------------------------------------

// refAccountFields are the editable fields of an account (journal payloads). The credit-card fields
// are set only on a top-level credit card.
type refAccountFields struct {
	Id                      string `json:"id"`
	Name                    string `json:"name"`
	Color                   string `json:"color"`
	Icon                    int64  `json:"icon"`
	Comment                 string `json:"comment"`
	CreditCardStatementDate *int   `json:"creditCardStatementDate,omitempty"`
	CreditCardLimit         *int64 `json:"creditCardLimit,omitempty"`
}

func refAccountFieldsOf(a *models.Account) refAccountFields {
	f := refAccountFields{Id: idString(a.AccountId), Name: a.Name, Color: a.Color, Icon: a.Icon, Comment: a.Comment}

	if a.ParentAccountId == models.LevelOneAccountParentId && a.Category == models.ACCOUNT_CATEGORY_CREDIT_CARD {
		date, limit := 0, int64(0)

		if a.Extend != nil && a.Extend.CreditCardStatementDate != nil {
			date = *a.Extend.CreditCardStatementDate
		}

		if a.Extend != nil && a.Extend.CreditCardLimit != nil {
			limit = *a.Extend.CreditCardLimit
		}

		f.CreditCardStatementDate, f.CreditCardLimit = &date, &limit
	}

	return f
}

func refLastReconciled(a *models.Account) *int64 {
	if a.Extend == nil || a.Extend.LastReconciledTime == nil {
		return nil
	}

	t := *a.Extend.LastReconciledTime

	return &t
}

// refAccountModifyRequest builds upstream's AccountModifyRequest for editing `target` (a top-level
// account or a sub-account) to hold f. Upstream's modify replaces the WHOLE account family: every
// sub-account the request omits is DELETED, and every field it omits is reset. So this carries
// every sub-account and every current value (hidden, last reconciled time, icon type, credit-card
// fields) and changes only what f says.
func refAccountModifyRequest(all []*models.Account, target *models.Account, f refAccountFields) (*models.AccountModifyRequest, error) {
	main := target

	if target.ParentAccountId != models.LevelOneAccountParentId {
		main = nil

		for _, a := range all {
			if a.AccountId == target.ParentAccountId {
				main = a
			}
		}

		if main == nil {
			return nil, NotFound("GET /machine/v1/accounts lists them", "the parent of sub-account %s was not found", idString(target.AccountId))
		}
	}

	pick := func(a *models.Account) refAccountFields {
		if a.AccountId == target.AccountId {
			return f
		}

		return refAccountFieldsOf(a)
	}

	mf := pick(main)
	req := &models.AccountModifyRequest{
		Id: main.AccountId, Name: mf.Name, Category: main.Category, Icon: mf.Icon, IconType: main.IconType, Color: mf.Color,
		LastReconciledTime: refLastReconciled(main), Comment: mf.Comment, Hidden: main.Hidden,
	}

	if main.Category == models.ACCOUNT_CATEGORY_CREDIT_CARD {
		if mf.CreditCardStatementDate != nil {
			req.CreditCardStatementDate = *mf.CreditCardStatementDate
		}

		if mf.CreditCardLimit != nil && *mf.CreditCardLimit > 0 {
			req.CreditCardLimit = strconv.FormatInt(*mf.CreditCardLimit, 10)
		}
	}

	var subs []*models.Account

	for _, a := range all {
		if a.ParentAccountId == main.AccountId {
			subs = append(subs, a)
		}
	}

	refSortAccounts(subs)

	for _, s := range subs {
		sf := pick(s)
		req.SubAccounts = append(req.SubAccounts, &models.AccountModifyRequest{
			Id: s.AccountId, Name: sf.Name, Category: main.Category, Icon: sf.Icon, IconType: s.IconType, Color: sf.Color,
			LastReconciledTime: refLastReconciled(s), Comment: sf.Comment, Hidden: s.Hidden,
		})
	}

	if main.Type == models.ACCOUNT_TYPE_MULTI_SUB_ACCOUNTS && len(req.SubAccounts) == 0 {
		return nil, Conflict("reload the accounts and retry", "the parent account %s has no sub-accounts left to carry", idString(main.AccountId))
	}

	return req, nil
}

func refApplyAccountFields(mc *Ctx, f refAccountFields) error {
	all, err := refLoadAccounts(mc)

	if err != nil {
		return err
	}

	id, err := ResolveId("id", f.Id)

	if err != nil {
		return err
	}

	var target *models.Account

	for _, a := range all {
		if a.AccountId == id {
			target = a
		}
	}

	if target == nil {
		return NotFound("GET /machine/v1/accounts lists them", "account %s no longer exists", f.Id)
	}

	req, err := refAccountModifyRequest(all, target, f)

	if err != nil {
		return err
	}

	_, err = mc.CallUpstream(api.Accounts.AccountModifyHandler, "POST", nil, req)

	return err
}

type refAccountPatchReq struct {
	WriteOpts
	Name                    *string      `json:"name"`
	Color                   *string      `json:"color"`
	Icon                    *refFlex     `json:"icon"`
	Comment                 *string      `json:"comment"`
	CreditCardStatementDate *int         `json:"credit_card_statement_date"`
	CreditCardLimit         *json.Number `json:"credit_card_limit"`
	// refused with a hint rather than "unknown argument"
	Currency       *string      `json:"currency"`
	Category       *string      `json:"category"`
	InitialBalance *json.Number `json:"initial_balance"`
	Balance        *json.Number `json:"balance"`
	Hidden         *bool        `json:"hidden"`
}

// refHandleAccountPatch is PATCH /accounts/:id (a sub-account works too)
func refHandleAccountPatch(mc *Ctx) (any, error) {
	var req refAccountPatchReq

	if err := mc.BindBody(&req); err != nil {
		return nil, err
	}

	switch {
	case req.Currency != nil:
		return nil, Invalid("currency is immutable after creation (upstream enforces it): create a new account in the other currency and move the money with a transfer", "currency cannot change")
	case req.Category != nil:
		return nil, Invalid("the category decides asset or liability and so the sign of every balance; change it in the web UI if you really mean to", "category is not editable here")
	case req.InitialBalance != nil || req.Balance != nil:
		return nil, Invalid("a balance is the sum of transactions: add a transaction, or reconcile (POST /machine/v1/accounts/:id/reconcile/plan)", "the balance cannot be edited")
	case req.Hidden != nil:
		return nil, Invalid("use POST /machine/v1/accounts/:id/hide {hidden}", "hidden is set through the hide route")
	}

	ref := mc.Param("id")

	type state struct {
		Before, After refAccountFields
		Request       *models.AccountModifyRequest
	}

	return RunWrite(mc, req.WriteOpts, func() (*Plan, error) {
		all, err := refLoadAccounts(mc)

		if err != nil {
			return nil, err
		}

		a, err := refResolveAccount(all, ref)

		if err != nil {
			return nil, err
		}

		before := refAccountFieldsOf(a)
		after := before
		var changes []refFieldChange
		add := func(field string, from, to any) {
			changes = append(changes, refFieldChange{Id: before.Id, Name: a.Name, Field: field, From: from, To: to})
		}

		if req.Name != nil {
			name, err := refName("account", *req.Name)

			if err != nil {
				return nil, err
			}

			if name != a.Name {
				after.Name = name
				add("name", a.Name, name)
			}
		}

		if req.Color != nil {
			color, err := refColor(*req.Color, a.Color)

			if err != nil {
				return nil, err
			}

			if color != a.Color {
				after.Color = color
				add("color", a.Color, color)
			}
		}

		if req.Icon != nil {
			icon, err := refIcon(*req.Icon, a.Icon)

			if err != nil {
				return nil, err
			}

			if icon != a.Icon {
				after.Icon = icon
				add("icon", strconv.FormatInt(a.Icon, 10), strconv.FormatInt(icon, 10))
			}
		}

		if req.Comment != nil {
			comment, err := refComment(*req.Comment)

			if err != nil {
				return nil, err
			}

			if comment != a.Comment {
				after.Comment = comment
				add("comment", a.Comment, comment)
			}
		}

		if req.CreditCardStatementDate != nil || req.CreditCardLimit != nil {
			if before.CreditCardStatementDate == nil {
				return nil, Invalid("only a top-level credit_card account has a statement date and a credit limit", "%q is not a top-level credit card", a.Name)
			}
		}

		if req.CreditCardStatementDate != nil {
			d := *req.CreditCardStatementDate

			if d < 0 || d > 28 {
				return nil, Invalid("the statement date is a day of the month, 1-28 (0 = not set)", "credit_card_statement_date %d is out of range", d)
			}

			if d != *before.CreditCardStatementDate {
				after.CreditCardStatementDate = &d
				add("creditCardStatementDate", *before.CreditCardStatementDate, d)
			}
		}

		if req.CreditCardLimit != nil {
			n, err := AmountArg("credit_card_limit", *req.CreditCardLimit)

			if err != nil {
				return nil, err
			}

			if n < 0 {
				return nil, Invalid("the credit limit is a positive number of hundredths (0 clears it)", "credit_card_limit %d is negative", n)
			}

			if n != *before.CreditCardLimit {
				if a.Type == models.ACCOUNT_TYPE_MULTI_SUB_ACCOUNTS && a.Currency == core.AccountCurrencyNotSetValue && n > 0 {
					return nil, Invalid("a credit limit on a parent card needs the parent's currency, which upstream only sets at creation; set the limit in the web UI", "the parent card %q has no currency", a.Name)
				}

				after.CreditCardLimit = &n
				add("creditCardLimit", *before.CreditCardLimit, n)
			}
		}

		if changes == nil {
			changes = []refFieldChange{}
		}

		modify, err := refAccountModifyRequest(all, a, after)

		if err != nil {
			return nil, err
		}

		plan := &Plan{Preview: changes, Fingerprinted: []any{before, after}, State: &state{Before: before, After: after, Request: modify}, Changes: map[string]int{"update": 0}}

		if len(changes) > 0 {
			plan.Changes["update"] = 1
			plan.Count = 1
		} else {
			plan.Changes["unchanged"] = 1
		}

		return plan, nil
	}, func(p *Plan) (any, error) {
		st := p.State.(*state)
		id, _ := strconv.ParseInt(st.Before.Id, 10, 64)

		if p.Count > 0 {
			if _, err := mc.CallUpstream(api.Accounts.AccountModifyHandler, "POST", nil, st.Request); err != nil {
				return nil, err
			}

			if err := refJournalRestore(mc, "ref.restore_account_fields", st.Before, st.After, "edit account "+st.Before.Id); err != nil {
				return nil, err
			}
		}

		out, err := refAccountResult(mc, id)

		if err != nil {
			return nil, err
		}

		out["updated"] = p.Count

		return out, nil
	})
}

func refInvRestoreAccount(mc *Ctx, payload, check json.RawMessage) error {
	var want refAccountFields

	if err := json.Unmarshal(payload, &want); err != nil {
		return err
	}

	id, err := ResolveId("id", want.Id)

	if err != nil {
		return err
	}

	a, err := services.Accounts.GetAccountByAccountId(mc.Web, mc.Uid, id)

	if err != nil || a == nil {
		return Conflict("the account was deleted since; nothing to restore", "account %s no longer exists", want.Id)
	}

	current := refAccountFieldsOf(a)

	if !refCheckMatches(current, check) {
		return Conflict("the account was edited again since (in the browser or by another write); nothing was changed", "account %s changed since the write", want.Id)
	}

	if refCheckMatches(current, refMustJSON(want)) {
		return nil
	}

	return refApplyAccountFields(mc, want)
}

// ---------------------------------------------------------------------------------------------
// hide, move, delete
// ---------------------------------------------------------------------------------------------

func refAccountHideTarget(mc *Ctx, ref string, hidden bool) (*refHideTarget, error) {
	all, err := refLoadAccounts(mc)

	if err != nil {
		return nil, err
	}

	a, err := refResolveAccount(all, ref)

	if err != nil {
		return nil, err
	}

	t := &refHideTarget{Id: a.AccountId, Name: a.Name, Hidden: a.Hidden}

	if hidden {
		t.Warnings = append(t.Warnings, "a hidden account still counts in balances and net worth; it is only hidden from lists and pickers, and hidden accounts cannot receive new transactions or templates")
	}

	return t, nil
}

// refAccountSiblings is the display-order group: same parent, and for top-level accounts the same
// category (the web UI orders accounts within their category)
func refAccountSiblings(mc *Ctx, ref string) (int64, []refSibling, error) {
	all, err := refLoadAccounts(mc)

	if err != nil {
		return 0, nil, err
	}

	a, err := refResolveAccount(all, ref)

	if err != nil {
		return 0, nil, err
	}

	var sibs []refSibling

	for _, o := range all {
		if o.ParentAccountId != a.ParentAccountId {
			continue
		}

		if a.ParentAccountId == models.LevelOneAccountParentId && o.Category != a.Category {
			continue
		}

		sibs = append(sibs, refSibling{Id: o.AccountId, Name: o.Name, Order: o.DisplayOrder})
	}

	return a.AccountId, sibs, nil
}

// refAccountInUse refuses a delete upstream would refuse, naming the count and the fix
func refAccountInUse(mc *Ctx, a *models.Account, ids []int64) error {
	used, err := refDB(mc).NewSession(mc.Web).Where("uid=? AND deleted=? AND type<>?", mc.Uid, false, models.TRANSACTION_DB_TYPE_MODIFY_BALANCE).In("account_id", ids).Count(&models.Transaction{})

	if err != nil {
		return err
	}

	if used > 0 {
		return Conflict("move them to another account first (POST /machine/v1/transactions/move-all), or hide the account instead", "%d transactions are on %q; an account in use cannot be deleted", used, a.Name).WithDetails(map[string]any{"transactions": used})
	}

	idSet := map[int64]bool{}

	for _, id := range ids {
		idSet[id] = true
	}

	templates, err := refTemplatesUsing(mc, func(t *models.TransactionTemplate) bool {
		return idSet[t.AccountId] || idSet[t.RelatedAccountId]
	})

	if err != nil {
		return err
	}

	if len(templates) > 0 {
		return Conflict("edit or delete those templates first (GET /machine/v1/templates)", "%d templates or active schedules use %q", len(templates), a.Name).WithDetails(map[string]any{"templates": templates})
	}

	return nil
}

func refAccountDeleteTarget(mc *Ctx, ref string) (*refDeleteTarget, error) {
	all, err := refLoadAccounts(mc)

	if err != nil {
		return nil, err
	}

	a, err := refResolveAccount(all, ref)

	if err != nil {
		return nil, err
	}

	if a.ParentAccountId != models.LevelOneAccountParentId {
		return nil, Invalid("delete a sub-account with DELETE /machine/v1/accounts/"+idString(a.ParentAccountId)+"/sub-accounts/"+idString(a.AccountId), "%q is a sub-account", a.Name)
	}

	ids := refAccountIdsOf(all, a)

	if err := refAccountInUse(mc, a, ids); err != nil {
		return nil, err
	}

	var subs []string

	for _, o := range all {
		if o.ParentAccountId == a.AccountId {
			subs = append(subs, o.Name)
		}
	}

	preview := map[string]any{"action": "delete", "id": idString(a.AccountId), "name": a.Name, "category": refAccountCategoryNames[a.Category], "currency": a.Currency, "subAccounts": subs,
		"note": "its opening-balance transactions are deleted with it; undo restores both"}

	return &refDeleteTarget{Id: a.AccountId, Ids: ids, Preview: preview}, nil
}

// refHandleSubAccountDelete is DELETE /accounts/:id/sub-accounts/:sub_id (admin)
func refHandleSubAccountDelete(mc *Ctx) (any, error) {
	parentRef := mc.Param("id")

	return refHandleDelete(mc, refAccountKind, mc.Param("sub_id"), func(mc *Ctx, ref string) (*refDeleteTarget, error) {
		all, err := refLoadAccounts(mc)

		if err != nil {
			return nil, err
		}

		parent, err := refResolveAccount(all, parentRef)

		if err != nil {
			return nil, err
		}

		var subs []*models.Account

		for _, o := range all {
			if o.ParentAccountId == parent.AccountId {
				subs = append(subs, o)
			}
		}

		sub, err := refResolveOne("sub-account", ref, subs, refAccountId, refAccountName, nil, "GET /machine/v1/accounts/"+idString(parent.AccountId)+" lists its sub-accounts")

		if err != nil {
			return nil, err
		}

		if len(subs) <= 1 {
			return nil, Invalid("a parent account keeps at least one sub-account: delete the whole account instead (DELETE /machine/v1/accounts/"+idString(parent.AccountId)+")", "%q is the only sub-account of %q", sub.Name, parent.Name)
		}

		if err := refAccountInUse(mc, sub, []int64{sub.AccountId}); err != nil {
			return nil, err
		}

		preview := map[string]any{"action": "delete", "id": idString(sub.AccountId), "name": sub.Name, "parentId": idString(parent.AccountId), "parentName": parent.Name, "currency": sub.Currency}

		return &refDeleteTarget{Id: sub.AccountId, Preview: preview}, nil
	})
}

func refAccountRows(mc *Ctx, ids []int64) (map[int64]refRow, error) {
	var rows []*models.Account

	if err := refDB(mc).NewSession(mc.Web).Where("uid=?", mc.Uid).In("account_id", ids).Find(&rows); err != nil {
		return nil, err
	}

	out := map[int64]refRow{}

	for _, r := range rows {
		out[r.AccountId] = refRow{Id: r.AccountId, Name: r.Name, Hidden: r.Hidden, Order: r.DisplayOrder, Deleted: r.Deleted}
	}

	return out, nil
}

func refDeleteAccountById(mc *Ctx, id int64) error {
	a := &models.Account{}
	has, err := refDB(mc).NewSession(mc.Web).Where("uid=? AND account_id=?", mc.Uid, id).Get(a)

	if err != nil {
		return err
	}

	if !has || a.Deleted {
		return nil
	}

	handler := api.Accounts.AccountDeleteHandler

	if a.ParentAccountId != models.LevelOneAccountParentId {
		handler = api.Accounts.SubAccountDeleteHandler
	}

	_, err = mc.CallUpstream(handler, "POST", nil, map[string]any{"id": idString(id)})

	return err
}

// refUndeleteAccounts restores soft-deleted accounts the journal names, with the opening-balance
// transactions upstream's delete soft-deleted in the same instant, and their balances (an account
// can only have been deleted while it held nothing but opening balances, so its balance is their sum)
func refUndeleteAccounts(mc *Ctx, ids []int64) (int, error) {
	if len(ids) == 0 {
		return 0, nil
	}

	restored := 0

	err := refDB(mc).DoTransaction(mc.Web, func(sess *xorm.Session) error {
		var rows []*models.Account

		if err := sess.Where("uid=? AND deleted=?", mc.Uid, true).In("account_id", ids).Find(&rows); err != nil {
			return err
		}

		for _, a := range rows {
			var txns []*models.Transaction

			if err := sess.Where("uid=? AND deleted=? AND account_id=? AND type=? AND deleted_unix_time=?", mc.Uid, true, a.AccountId, models.TRANSACTION_DB_TYPE_MODIFY_BALANCE, a.DeletedUnixTime).Find(&txns); err != nil {
				return err
			}

			balance := int64(0)
			txnIds := make([]int64, 0, len(txns))

			for _, t := range txns {
				balance += t.Amount
				txnIds = append(txnIds, t.TransactionId)
			}

			if len(txnIds) > 0 {
				if _, err := sess.Cols("deleted", "deleted_unix_time").Where("uid=? AND deleted=?", mc.Uid, true).In("transaction_id", txnIds).Update(&models.Transaction{Deleted: false, DeletedUnixTime: 0}); err != nil {
					return err
				}
			}

			n, err := sess.Cols("deleted", "deleted_unix_time", "balance").Where("uid=? AND deleted=? AND account_id=?", mc.Uid, true, a.AccountId).Update(&models.Account{Deleted: false, DeletedUnixTime: 0, Balance: balance})

			if err != nil {
				return err
			}

			restored += int(n)
		}

		return nil
	})

	return restored, err
}

var refAccountKind = &refKind{
	Name:     "account",
	List:     "GET /machine/v1/accounts",
	Rows:     refAccountRows,
	Delete:   refDeleteAccountById,
	Undelete: refUndeleteAccounts,
}

// ---------------------------------------------------------------------------------------------
// the route table
// ---------------------------------------------------------------------------------------------

func init() {
	refAccountKind.Hide = api.Accounts.AccountHideHandler
	refAccountKind.Move = api.Accounts.AccountMoveHandler
	refRegisterKind(refAccountKind)
	RegisterInverse("ref.restore_account_fields", refInvRestoreAccount)
	registerRoutes(refAccountRoutes)
}

func refAccountRoutes() []RouteDef {
	return []RouteDef{
		{Method: "GET", Path: "/accounts", Tier: TierRead, Handler: refHandleAccountList, Untrusted: []string{"name", "comment"},
			Summary: "Accounts with balances (integer hundredths in each account's own currency; a parent's balance is null), sub-accounts nested. Args: include_hidden (false), category, currency, with_sub_accounts (true), name (flat rows, exact then case-insensitive)."},
		{Method: "GET", Path: "/accounts/:id", Tier: TierRead, Handler: refHandleAccountGet, Untrusted: []string{"name", "comment"},
			Summary: "One account (id or name; sub-accounts too) with its sub-accounts or parent name."},
		{Method: "GET", Path: "/accounts/:id/properties", Tier: TierRead, Handler: refHandleAccountProperties, Composed: true,
			Summary: "Transaction count, first/last transaction, last reconciled time, credit-card statement date and limit, and the opening-balance rows."},
		{Method: "POST", Path: "/accounts", Tier: TierWrite, DryRunnable: true, Handler: refHandleAccountCreate,
			Summary: "Create an account. Body: name, category (" + refAccountCategoryList + "), currency (immutable later), initial_balance (hundredths; liabilities negative for money owed), initial_balance_date, color, icon, comment, credit_card_statement_date, credit_card_limit, sub_accounts: [{name, currency, initial_balance, initial_balance_date, color, icon, comment}], idempotency_key; dry_run default true."},
		{Method: "POST", Path: "/accounts/batch", Tier: TierWrite, DryRunnable: true, Composed: true, Handler: refHandleAccountBatch,
			Summary: "Create many accounts in order under one token; on a failure the ones already created are removed again and the failing index is named. Body: accounts: [create bodies]."},
		{Method: "PATCH", Path: "/accounts/:id", Tier: TierWrite, DryRunnable: true, Handler: refHandleAccountPatch,
			Summary: "Edit an account or sub-account. Body: name, color, icon, comment, credit_card_statement_date, credit_card_limit. Currency and category are immutable here."},
		{Method: "POST", Path: "/accounts/:id/hide", Tier: TierWrite, DryRunnable: true, Summary: "Hide or show an account. Body: hidden (default true).",
			Handler: func(mc *Ctx) (any, error) { return refHandleHide(mc, refAccountKind, refAccountHideTarget) }},
		{Method: "POST", Path: "/accounts/:id/move", Tier: TierWrite, DryRunnable: true, Summary: "Reorder an account among its siblings (same category, or same parent). Body: to_index (0-based).",
			Handler: func(mc *Ctx) (any, error) { return refHandleMove(mc, refAccountKind, refAccountSiblings) }},
		{Method: "DELETE", Path: "/accounts/:id", Tier: TierAdmin, DryRunnable: true, Summary: "Delete an account with its sub-accounts and opening balances; refused while transactions or templates use it, naming the count.",
			Handler: func(mc *Ctx) (any, error) {
				return refHandleDelete(mc, refAccountKind, mc.Param("id"), refAccountDeleteTarget)
			}},
		{Method: "DELETE", Path: "/accounts/:id/sub-accounts/:sub_id", Tier: TierAdmin, DryRunnable: true, Handler: refHandleSubAccountDelete,
			Summary: "Delete one sub-account (never the last one); refused while transactions or templates use it."},
	}
}

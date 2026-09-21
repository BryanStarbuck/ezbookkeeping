package machine

import (
	"encoding/json"
	"errors"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/mayswind/ezbookkeeping/pkg/api"
	"github.com/mayswind/ezbookkeeping/pkg/errfile"
	"github.com/mayswind/ezbookkeeping/pkg/errs"
	"github.com/mayswind/ezbookkeeping/pkg/log"
	"github.com/mayswind/ezbookkeeping/pkg/models"
	"github.com/mayswind/ezbookkeeping/pkg/services"
	"github.com/mayswind/ezbookkeeping/pkg/validators"
)

// ingest_accounts.go — account provisioning from a manifest (apis.mdx §15) and the map writes
// (PUT /ingest/map, POST /ingest/map/infer).
//
// The plan decides, per manifest row, create · link · skip · ambiguous, with its reasoning, and
// creates nothing. `ambiguous` never becomes a create: two accounts sharing a last4 across two
// entities is the normal case in a real archive. The consequential decisions are read aloud: the
// ezBookkeeping CATEGORY (it decides asset versus liability, so the sign of every balance) and the
// CURRENCY (immutable after creation), with every defaulted currency flagged.

const (
	ingDefaultNaming = "{Entity} · {Institution} {Kind} ••{last4}"
	ingAccountName   = 64
)

// ingKindCategory is the §15.3 table
var ingKindCategory = map[string]string{
	"checking":  "checking",
	"savings":   "savings",
	"cash":      "cash",
	"card":      "credit_card",
	"brokerage": "investment",
	"loan":      "debt",
	"cd":        "certificate_of_deposit",
}

var ingKindWhy = map[string]string{
	"checking":  "money you spend",
	"savings":   "an asset that holds savings",
	"cash":      "cash on hand",
	"card":      "a liability: the balance owed is negative and payments are transfers in",
	"brokerage": "market movement is not income; it stays out of income and expense by construction",
	"loan":      "a liability: the balance owed",
	"cd":        "an asset held to maturity",
}

var ingKindDisplay = map[string]string{"checking": "Checking", "savings": "Savings", "cash": "Cash", "card": "Card", "brokerage": "Brokerage", "loan": "Loan", "cd": "CD"}

// ingCategoryIcon is the browser's default icon per account category (src/core/account.ts)
var ingCategoryIcon = map[models.AccountCategory]string{
	models.ACCOUNT_CATEGORY_CASH: "1", models.ACCOUNT_CATEGORY_CHECKING_ACCOUNT: "100", models.ACCOUNT_CATEGORY_SAVINGS_ACCOUNT: "100",
	models.ACCOUNT_CATEGORY_CREDIT_CARD: "100", models.ACCOUNT_CATEGORY_VIRTUAL: "500", models.ACCOUNT_CATEGORY_DEBT: "600",
	models.ACCOUNT_CATEGORY_RECEIVABLES: "700", models.ACCOUNT_CATEGORY_CERTIFICATE_OF_DEPOSIT: "110", models.ACCOUNT_CATEGORY_INVESTMENT: "800",
}

// ingAccountsArgs are the arguments of /ingest/accounts/plan and /apply
type ingAccountsArgs struct {
	Root           string                        `json:"root,omitempty"`
	ManifestPath   string                        `json:"manifest_path,omitempty"`
	Staging        string                        `json:"staging,omitempty"`
	Naming         string                        `json:"naming,omitempty"`
	KindToCategory map[string]string             `json:"kind_to_category,omitempty"`
	Overrides      map[string]ingAccountOverride `json:"overrides,omitempty"`
	Accounts       []string                      `json:"accounts,omitempty"`
}

// ingAccountOverride is a per-row decision the caller makes
type ingAccountOverride struct {
	Name      string `json:"name,omitempty"`
	Action    string `json:"action,omitempty"` // skip | link | create
	AccountId string `json:"account_id,omitempty"`
}

type ingProposed struct {
	Name              string `json:"name"`
	FullName          string `json:"full_name,omitempty"`
	Truncated         bool   `json:"truncated,omitempty"`
	Category          string `json:"category"`
	Currency          string `json:"currency"`
	CurrencyDefaulted bool   `json:"currency_defaulted,omitempty"`
	Side              string `json:"side"`
	// OpeningBalance (signed hundredths) and OpeningTime (unix seconds) come from the manifest's
	// opening_balance / opening_date and become the account's own balance-modification row
	OpeningBalance *int64 `json:"opening_balance,omitempty"`
	OpeningDate    string `json:"opening_date,omitempty"`
	OpeningTime    int64  `json:"opening_time,omitempty"`
}

type ingExisting struct {
	Id       string `json:"id"`
	Name     string `json:"name"`
	Currency string `json:"currency"`
	Category string `json:"category"`
	Side     string `json:"side"`
}

// ingAccountDecision is one manifest row's decision
type ingAccountDecision struct {
	Manifest   map[string]any `json:"manifest"`
	AccountKey string         `json:"account_key"`
	Action     string         `json:"action"`
	Proposed   *ingProposed   `json:"proposed,omitempty"`
	Existing   *ingExisting   `json:"existing,omitempty"`
	Candidates []*ingExisting `json:"candidates,omitempty"`
	Reason     string         `json:"reason"`
	Warnings   []string       `json:"warnings"`

	row      *ingManifestRow
	category models.AccountCategory
	linkId   int64
}

// ingAccountsPlan is the whole provisioning plan
type ingAccountsPlan struct {
	Root              string                `json:"root"`
	ManifestPath      string                `json:"manifest_path"`
	Staging           string                `json:"staging"`
	Naming            string                `json:"naming"`
	KindToCategory    map[string]string     `json:"kind_to_category"`
	Plan              []*ingAccountDecision `json:"plan"`
	Summary           map[string]int        `json:"summary"`
	Liabilities       []string              `json:"liabilities"`
	CurrencyDefaulted []string              `json:"currency_defaulted"`
	Warnings          []string              `json:"warnings"`

	ctx *ingContext
}

type ingAccountChange struct {
	Action     string `json:"action"`
	AccountKey string `json:"account_key"`
	Name       string `json:"name,omitempty"`
	Category   string `json:"category,omitempty"`
	Currency   string `json:"currency,omitempty"`
	AccountId  string `json:"account_id,omitempty"`
	// Balance and BalanceTime are the opening balance a create sets (in the fingerprint, so a changed
	// manifest opening invalidates the token)
	Balance     int64 `json:"balance,omitempty"`
	BalanceTime int64 `json:"balance_time,omitempty"`
}

// ingHumanize turns "acme_llc" into "Acme LLC"
func ingHumanize(s string) string {
	words := strings.FieldsFunc(s, func(r rune) bool { return r == '_' || r == '-' || unicode.IsSpace(r) })

	for i, w := range words {
		lw := strings.ToLower(w)

		switch lw {
		case "llc", "inc", "ltd", "plc", "lp", "llp", "gmbh", "ag", "sa", "bv", "co":
			words[i] = strings.ToUpper(lw)

			if lw == "gmbh" {
				words[i] = "GmbH"
			} else if lw == "co" {
				words[i] = "Co"
			}

			continue
		}

		if w == strings.ToLower(w) {
			r, n := utf8.DecodeRuneInString(w)
			words[i] = string(unicode.ToUpper(r)) + w[n:]
		}
	}

	return strings.Join(words, " ")
}

// ingRenderName applies the naming rule (§15.2). It keeps the last4 intact and truncates the
// institution in the middle when the result exceeds ezBookkeeping's 64-character limit.
func ingRenderName(template string, row *ingManifestRow) (name, full string, truncated bool) {
	if template == "" {
		template = ingDefaultNaming
	}

	if row.Last4 == "" {
		if template == ingDefaultNaming {
			template = "{Entity} · {Institution} {Label}"
		} else {
			template = strings.ReplaceAll(template, " ••{last4}", "")
			template = strings.ReplaceAll(template, "••{last4}", "")
			template = strings.ReplaceAll(template, "{last4}", "")
		}
	}

	kind := ingKindDisplay[row.Kind]

	if kind == "" {
		kind = ingHumanize(row.Kind)
	}

	render := func(entity, institution string) string {
		r := strings.NewReplacer(
			"{Entity}", entity, "{entity}", row.Entity,
			"{Institution}", institution, "{institution}", institution,
			"{Kind}", kind, "{kind}", row.Kind,
			"{Label}", row.Label, "{label}", row.Label,
			"{last4}", row.Last4, "{Last4}", row.Last4,
			"{currency}", row.Currency,
		)

		return strings.Join(strings.Fields(r.Replace(template)), " ")
	}

	entity := ingHumanize(row.Entity)
	institution := row.Institution
	full = render(entity, institution)

	if utf8.RuneCountInString(full) <= ingAccountName {
		return full, full, false
	}

	inst := []rune(institution)

	for keep := len(inst) - 1; keep >= 1; keep-- {
		head := (keep + 1) / 2
		tail := keep - head
		short := string(inst[:head]) + "…" + string(inst[len(inst)-tail:])

		if candidate := render(entity, short); utf8.RuneCountInString(candidate) <= ingAccountName {
			return candidate, full, true
		}
	}

	ent := []rune(entity)

	for keep := len(ent) - 1; keep >= 1; keep-- {
		if candidate := render(string(ent[:keep])+"…", "…"); utf8.RuneCountInString(candidate) <= ingAccountName {
			return candidate, full, true
		}
	}

	r := []rune(full)

	return string(r[len(r)-ingAccountName:]), full, true
}

func ingLast4Pattern(last4 string) *regexp.Regexp {
	return regexp.MustCompile(`(?:^|\D)` + regexp.QuoteMeta(last4) + `(?:\D|$)`)
}

func ingExistingOf(a *models.Account) *ingExisting {
	return &ingExisting{Id: idString(a.AccountId), Name: a.Name, Currency: a.Currency, Category: ingAccountCategoryName(a.Category), Side: ingSide(a.Category)}
}

// ingPlanAccounts builds the §15.1 plan
func ingPlanAccounts(mc *Ctx, args *ingAccountsArgs) (*ingAccountsPlan, error) {
	ctx, err := ingOpenContext(mc, args.Root, args.ManifestPath, args.Staging, "prepared", true)

	if err != nil {
		return nil, err
	}

	kindTable := map[string]string{}

	for k, v := range ingKindCategory {
		kindTable[k] = v
	}

	for k, v := range args.KindToCategory {
		k = strings.ToLower(strings.TrimSpace(k))

		if _, ok := ingAccountCategoryNames[v]; !ok {
			return nil, Invalid("categories are cash, checking, savings, credit_card, virtual, debt, receivables, investment, certificate_of_deposit", "kind_to_category[%s] = %q is not an ezBookkeeping account category", k, v)
		}

		kindTable[k] = v
	}

	naming := args.Naming

	if naming == "" {
		naming = ingDefaultNaming
	}

	accounts, err := ingLoadAccounts(mc)

	if err != nil {
		return nil, err
	}

	plan := &ingAccountsPlan{Root: ctx.Root.Display, ManifestPath: ctx.Manifest.Path, Staging: ctx.Staging.Rel, Naming: naming, KindToCategory: kindTable, Plan: []*ingAccountDecision{},
		Summary: map[string]int{"create": 0, "link": 0, "skip": 0, "ambiguous": 0}, Liabilities: []string{}, CurrencyDefaulted: []string{}, Warnings: append([]string{}, ctx.Manifest.Warnings...), ctx: ctx}

	// how many manifest rows share each last4 (the normal case the plan must not paper over)
	last4Rows := map[string][]string{}

	for _, row := range ctx.Manifest.Rows {
		if row.Last4 != "" {
			last4Rows[row.Last4] = append(last4Rows[row.Last4], row.AccountKey())
		}
	}

	// accounts the map already claims
	claimed := map[int64]string{}

	for key, e := range ctx.Map.Accounts {
		if id, err := strconv.ParseInt(e.AccountId, 10, 64); err == nil {
			claimed[id] = key
		}
	}

	proposedNames := map[string]string{}

	for _, row := range ctx.Manifest.Rows {
		if !ingAccountSelected(args.Accounts, row.AccountKey(), row.Label, row.Path, row.Last4) {
			continue
		}

		key := row.AccountKey()
		d := &ingAccountDecision{AccountKey: key, Warnings: append([]string{}, row.Warnings...), row: row}
		d.Manifest = map[string]any{"entity": row.Entity, "institution": row.Institution, "label": row.Label, "last4": row.Last4, "kind": row.Kind, "currency": row.Currency, "path": row.Path, "file": row.File, "line": row.Line}
		plan.Plan = append(plan.Plan, d)
		ov := args.Overrides[key]

		// the proposal is printed for every row, so the category and currency are read aloud
		catName, known := kindTable[row.Kind]

		if known {
			d.category = ingAccountCategoryNames[catName]
			name, full, truncated := ingRenderName(naming, row)

			if row.Name != "" {
				name, full, truncated = row.Name, row.Name, false
			}

			if ov.Name != "" {
				name, full, truncated = ov.Name, ov.Name, false
			}

			d.Proposed = &ingProposed{Name: name, Category: catName, Currency: row.Currency, CurrencyDefaulted: row.CurrencyDefaulted, Side: ingSide(d.category)}

			if row.OpeningBalance != nil {
				t, _ := time.Parse("2006-01-02", row.OpeningDate)
				d.Proposed.OpeningBalance, d.Proposed.OpeningDate, d.Proposed.OpeningTime = row.OpeningBalance, row.OpeningDate, ingOpeningTime(t)

				if *row.OpeningBalance > 0 && d.category.IsLiability() {
					d.Warnings = append(d.Warnings, "a positive opening balance on a liability means the account starts in credit; check the sign (money owed is negative)")
				}
			}

			if truncated {
				d.Proposed.FullName, d.Proposed.Truncated = full, true
				d.Warnings = append(d.Warnings, "the name was shortened in the middle of the institution to fit 64 characters")
			}

			if utf8.RuneCountInString(name) > ingAccountName {
				d.Action, d.Reason = "ambiguous", "the given name is longer than 64 characters"
			}
		}

		switch {
		case d.Action != "":
		case row.Skip || strings.EqualFold(ov.Action, "skip"):
			d.Action, d.Reason = "skip", "marked skip in the manifest or the call"
		case ov.Action != "" && !strings.EqualFold(ov.Action, "link") && !strings.EqualFold(ov.Action, "create"):
			return nil, Invalid("overrides[].action is skip, link or create", "unknown override action %q for %s", ov.Action, key)
		case strings.EqualFold(ov.Action, "link"):
			id, err := ResolveId("overrides["+key+"].account_id", ov.AccountId)

			if err != nil {
				return nil, err
			}

			a := accounts.ById[id]

			if a == nil {
				return nil, NotFound("GET /machine/v1/accounts lists the account ids", "overrides[%s] names no account", key)
			}

			d.Action, d.Existing, d.linkId, d.Reason = "link", ingExistingOf(a), id, "linked by the caller"
		case !known:
			d.Action = "ambiguous"
			d.Reason = "kind " + strconv.Quote(row.Kind) + " has no account category; set kind in the manifest (checking, savings, card, brokerage, loan, cash, cd) or pass kind_to_category"
		case !validators.AllCurrencyNames[row.Currency]:
			d.Action, d.Reason = "ambiguous", "currency "+strconv.Quote(row.Currency)+" is not an ISO 4217 code ezBookkeeping knows"
		default:
			ingDecideLink(d, row, accounts, ctx.Map, claimed, last4Rows, strings.EqualFold(ov.Action, "create"))
		}

		if d.Action == "create" && d.Proposed != nil {
			if prev, ok := proposedNames[d.Proposed.Name]; ok {
				d.Action = "ambiguous"
				d.Reason = "the proposed name collides with " + prev + "; give one row a name override"
			} else {
				proposedNames[d.Proposed.Name] = key
			}
		}

		if d.Action == "create" && d.category.IsLiability() {
			plan.Liabilities = append(plan.Liabilities, key)
		}

		if d.Action == "link" && d.Existing != nil && d.Proposed != nil && d.Existing.Side != d.Proposed.Side {
			d.Warnings = append(d.Warnings, "the existing account is "+d.Existing.Side+" but kind="+row.Kind+" expects "+d.Proposed.Side+"; balances and income/expense will read inverted")
		}

		if d.Action == "link" && d.Existing != nil && !row.CurrencyDefaulted && d.Existing.Currency != row.Currency {
			d.Action = "ambiguous"
			d.Reason = "the matching account is in " + d.Existing.Currency + " but the manifest says " + row.Currency + "; currency cannot change after creation"
		}

		if row.CurrencyDefaulted && d.Action == "create" {
			plan.CurrencyDefaulted = append(plan.CurrencyDefaulted, key)
		}

		if d.Action == "create" && d.Proposed != nil {
			d.Reason = strings.TrimSpace(d.Reason + "; kind=" + row.Kind + " → " + d.Proposed.Category + " (" + d.Proposed.Side + "): " + ingKindWhy[row.Kind])
			d.Reason = strings.TrimPrefix(d.Reason, "; ")
		}

		plan.Summary[d.Action]++
	}

	if len(plan.CurrencyDefaulted) > 0 {
		plan.Warnings = append(plan.Warnings, strconv.Itoa(len(plan.CurrencyDefaulted))+" account(s) would be created with a DEFAULTED currency; currency cannot be changed after creation — add a currency column to the manifest if any is wrong")
	}

	if len(plan.Liabilities) > 0 {
		plan.Warnings = append(plan.Warnings, strconv.Itoa(len(plan.Liabilities))+" account(s) would be created as liabilities (credit card, debt); check each is really a liability before --write")
	}

	return plan, nil
}

// ingDecideLink looks for an existing account for one row: the map, then the exact proposed name,
// then the last4. Anything not exactly one candidate is ambiguous (never a create) unless there is
// no candidate at all.
func ingDecideLink(d *ingAccountDecision, row *ingManifestRow, accounts *ingAccountIndex, m *ingMap, claimed map[int64]string, last4Rows map[string][]string, forceCreate bool) {
	key := row.AccountKey()

	if e := m.Accounts[key]; e != nil && !forceCreate {
		if id, err := strconv.ParseInt(e.AccountId, 10, 64); err == nil {
			if a := accounts.ById[id]; a != nil {
				d.Action, d.Existing, d.linkId, d.Reason = "link", ingExistingOf(a), id, "already mapped in "+ingMapFile
				return
			}
		}

		d.Warnings = append(d.Warnings, "the map pointed at an account that no longer exists")
	}

	if forceCreate {
		d.Action, d.Reason = "create", "create requested by the caller"
		return
	}

	if d.Proposed != nil {
		byName := accounts.ByName[d.Proposed.Name]

		if len(byName) == 1 {
			a := byName[0]
			d.Action, d.Existing, d.linkId, d.Reason = "link", ingExistingOf(a), a.AccountId, "matched on the proposed name"

			if other, ok := claimed[a.AccountId]; ok && other != key {
				d.Action, d.Reason = "ambiguous", "the account with this name is already mapped to "+other
			}

			return
		}

		if len(byName) > 1 {
			d.Action, d.Reason = "ambiguous", "several accounts already have the proposed name"

			for _, a := range byName {
				d.Candidates = append(d.Candidates, ingExistingOf(a))
			}

			return
		}
	}

	if row.Last4 != "" {
		re := ingLast4Pattern(row.Last4)
		var cands []*models.Account

		for _, a := range accounts.All {
			if a.Type == models.ACCOUNT_TYPE_MULTI_SUB_ACCOUNTS {
				continue
			}

			if other, ok := claimed[a.AccountId]; ok && other != key {
				continue
			}

			if re.MatchString(a.Name) {
				cands = append(cands, a)
			}
		}

		switch {
		case len(cands) == 1 && len(last4Rows[row.Last4]) > 1:
			d.Action = "ambiguous"
			d.Reason = "an existing account matches last4 " + row.Last4 + " but " + strconv.Itoa(len(last4Rows[row.Last4])) + " manifest rows share it (" + strings.Join(last4Rows[row.Last4], ", ") + "); link it explicitly with overrides"
			d.Candidates = []*ingExisting{ingExistingOf(cands[0])}

			return
		case len(cands) == 1:
			d.Action, d.Existing, d.linkId, d.Reason = "link", ingExistingOf(cands[0]), cands[0].AccountId, "matched on last4 "+row.Last4

			return
		case len(cands) > 1:
			d.Action, d.Reason = "ambiguous", strconv.Itoa(len(cands))+" existing accounts match last4 "+row.Last4

			for _, a := range cands {
				d.Candidates = append(d.Candidates, ingExistingOf(a))
			}

			return
		}
	}

	d.Action = "create"

	if row.Last4 != "" {
		d.Reason = "no existing account matched on last4 or name"
	} else {
		d.Reason = "no existing account matched on name"
	}
}

// ingAccountsChanges is the fingerprinted change set of a provisioning apply
func ingAccountsChanges(plan *ingAccountsPlan) []*ingAccountChange {
	var out []*ingAccountChange

	for _, d := range plan.Plan {
		switch d.Action {
		case "create":
			ch := &ingAccountChange{Action: "create", AccountKey: d.AccountKey, Name: d.Proposed.Name, Category: d.Proposed.Category, Currency: d.Proposed.Currency}

			if d.Proposed.OpeningBalance != nil && *d.Proposed.OpeningBalance != 0 {
				ch.Balance, ch.BalanceTime = *d.Proposed.OpeningBalance, d.Proposed.OpeningTime
			}

			out = append(out, ch)
		case "link":
			if e := plan.ctx.Map.Accounts[d.AccountKey]; e == nil || e.AccountId != d.Existing.Id {
				out = append(out, &ingAccountChange{Action: "link", AccountKey: d.AccountKey, AccountId: d.Existing.Id})
			}
		}
	}

	sort.SliceStable(out, func(i, j int) bool { return out[i].AccountKey < out[j].AccountKey })

	if out == nil {
		out = []*ingAccountChange{}
	}

	return out
}

// ingApplyAccounts creates the create rows through upstream's account-create handler, links the
// link rows, and writes the map beside the statements
func ingApplyAccounts(mc *Ctx, plan *ingAccountsPlan, changes []*ingAccountChange) (any, error) {
	ingApplyMu.Lock()
	defer ingApplyMu.Unlock()

	decisions := map[string]*ingAccountDecision{}

	for _, d := range plan.Plan {
		decisions[d.AccountKey] = d
	}

	var ops []InverseOp
	var created []map[string]any
	var linked []map[string]any
	m := plan.ctx.Map
	prevRaw := plan.ctx.MapRaw
	now := time.Now().UTC().Format(time.RFC3339)
	var failure error

	for _, ch := range changes {
		d := decisions[ch.AccountKey]
		row := d.row

		switch ch.Action {
		case "create":
			body := map[string]any{
				"name": ch.Name, "category": int(d.category), "type": int(models.ACCOUNT_TYPE_SINGLE_ACCOUNT),
				"icon": ingCategoryIcon[d.category], "iconType": 0, "color": "000000", "currency": ch.Currency,
				"balance": strconv.FormatInt(ch.Balance, 10), "balanceTime": ch.BalanceTime, "comment": "", "creditCardStatementDate": 0, "subAccounts": []any{},
			}

			var resp models.AccountInfoResponse

			if err := mc.CallUpstreamInto(api.Accounts.AccountCreateHandler, "POST", nil, body, &resp); err != nil {
				failure = err
				break
			}

			acct, err := services.Accounts.GetAccountByAccountId(mc.Web, mc.Uid, resp.Id)
			updated := int64(0)

			if err == nil && acct != nil {
				updated = acct.UpdatedUnixTime
			}

			ops = append(ops, NewInverseOp(ingInverseAccount, map[string]any{"account_id": idString(resp.Id), "account_key": ch.AccountKey}, map[string]any{"updated_unix_time": updated}))
			m.Accounts[ch.AccountKey] = &ingMapEntry{AccountKey: ch.AccountKey, AccountId: idString(resp.Id), Name: ch.Name, Currency: ch.Currency, Category: ch.Category, Entity: row.Entity, Institution: row.Institution, Label: row.Label, Last4: row.Last4, Path: row.Path, Source: "accounts_apply", UpdatedAt: now}
			item := map[string]any{"account_key": ch.AccountKey, "account_id": idString(resp.Id), "name": ch.Name, "category": ch.Category, "currency": ch.Currency, "side": ingSide(d.category)}

			if ch.Balance != 0 {
				item["opening_balance"], item["opening_time"] = ch.Balance, ch.BalanceTime
			}

			created = append(created, item)
		case "link":
			e := &ingMapEntry{AccountKey: ch.AccountKey, AccountId: ch.AccountId, Entity: row.Entity, Institution: row.Institution, Label: row.Label, Last4: row.Last4, Path: row.Path, Source: "accounts_apply", UpdatedAt: now}

			if d.Existing != nil {
				e.Name, e.Currency, e.Category = d.Existing.Name, d.Existing.Currency, d.Existing.Category
			}

			m.Accounts[ch.AccountKey] = e
			linked = append(linked, map[string]any{"account_key": ch.AccountKey, "account_id": ch.AccountId, "name": e.Name})
		}

		if failure != nil {
			reportLost("applying an account change", failure)
			break
		}
	}

	// the map is written for everything that succeeded, even after a failure, so nothing created
	// is left unmapped
	var mapOp *InverseOp

	if len(created) > 0 || len(linked) > 0 {
		newRaw, err := ingSaveMap(plan.ctx.Staging, m, mc.User.Username)

		if err != nil {
			if failure == nil {
				failure = err
			}
		} else {
			op := ingMapRestoreOp(plan.ctx, prevRaw, newRaw)
			mapOp = &op
		}
	}

	if mapOp != nil {
		ops = append(ops, *mapOp)
	}

	if len(ops) > 0 {
		if _, err := RecordJournal(mc, "ingest accounts: "+strconv.Itoa(len(created))+" created, "+strconv.Itoa(len(linked))+" linked", len(created)+len(linked), ops); err != nil {
			log.Errorf(mc.Web, "[machine.ingest] journal write failed after account provisioning: %s", err.Error())
		}
	}

	if created == nil {
		created = []map[string]any{}
	}

	if linked == nil {
		linked = []map[string]any{}
	}

	if failure != nil {
		f := toFail(failure)

		return nil, (&Fail{Code: f.Code, Message: f.Message, UpstreamCode: f.UpstreamCode, Hint: "the accounts created before the failure are in the map; fix the cause and re-run POST /ingest/accounts/plan (they will show as link). " + f.Hint}).WithDetails(map[string]any{"created": created, "linked": linked})
	}

	return map[string]any{
		"created":   created,
		"linked":    linked,
		"skipped":   plan.Summary["skip"],
		"ambiguous": plan.Summary["ambiguous"],
		"map":       map[string]any{"path": plan.ctx.Staging.Rel + "/" + ingMapFile, "entries": len(m.Accounts)},
	}, nil
}

// ---- the map-restore inverse ----------------------------------------------------------------

type ingMapRestorePayload struct {
	Root     string `json:"root"`
	Staging  string `json:"staging"`
	Previous string `json:"previous"`
	Existed  bool   `json:"existed"`
}

type ingMapRestoreCheck struct {
	Sha256 string `json:"sha256"`
}

func ingMapRestoreOp(ctx *ingContext, prevRaw, newRaw []byte) InverseOp {
	undo := NewInverseOp(ingInverseMap, ingMapRestorePayload{Root: ctx.Root.Display, Staging: ctx.Staging.Rel, Previous: string(prevRaw), Existed: prevRaw != nil}, ingMapRestoreCheck{Sha256: ingSha256(newRaw)})
	redo := NewInverseOp(ingInverseMap, ingMapRestorePayload{Root: ctx.Root.Display, Staging: ctx.Staging.Rel, Previous: string(newRaw), Existed: true}, ingMapRestoreCheck{Sha256: ingShaOrEmpty(prevRaw)})
	undo.Redo = &redo

	return undo
}

func ingShaOrEmpty(b []byte) string {
	if b == nil {
		return ""
	}

	return ingSha256(b)
}

// ingUndoMapRestore puts the map back as it was, refusing when it changed since
func ingUndoMapRestore(mc *Ctx, payload json.RawMessage, check json.RawMessage) error {
	var p ingMapRestorePayload
	var c ingMapRestoreCheck

	if err := json.Unmarshal(payload, &p); err != nil {
		errfile.Caught("decoding the map inverse from the journal", err)
		return NewFail(CodeInternal, "the journal entry is damaged", "cannot read the map inverse")
	}

	_ = json.Unmarshal(check, &c)

	root, err := ingResolveRoot(p.Root)

	if err != nil {
		return err
	}

	st, err := ingResolveStaging(root, p.Staging, "")

	if err != nil {
		return err
	}

	current, err := st.ReadFile(ingMapFile)

	if err != nil {
		return err
	}

	if ingShaOrEmpty(current) != c.Sha256 {
		return Conflict("the statements map was rewritten since; re-map deliberately with PUT /ingest/map", "the map at %s/%s changed after this write", st.Rel, ingMapFile)
	}

	if !p.Existed {
		return st.Remove(ingMapFile)
	}

	if err := st.Ensure(); err != nil {
		return err
	}

	return st.WriteFile(ingMapFile, []byte(p.Previous))
}

// ingUndoAccountCreate deletes an account the provisioning created, only while it is unused and
// unchanged since
func ingUndoAccountCreate(mc *Ctx, payload json.RawMessage, check json.RawMessage) error {
	var p struct {
		AccountId  string `json:"account_id"`
		AccountKey string `json:"account_key"`
	}

	var c struct {
		UpdatedUnixTime int64 `json:"updated_unix_time"`
	}

	if err := json.Unmarshal(payload, &p); err != nil {
		errfile.Caught("decoding the account inverse from the journal", err)
		return NewFail(CodeInternal, "the journal entry is damaged", "cannot read the account inverse")
	}

	_ = json.Unmarshal(check, &c)
	id, err := ResolveId("account_id", p.AccountId)

	if err != nil {
		return err
	}

	acct, gerr := services.Accounts.GetAccountByAccountId(mc.Web, mc.Uid, id)

	if gerr != nil || acct == nil || acct.Deleted {
		if gerr != nil && !errors.Is(gerr, errs.ErrAccountNotFound) {
			errfile.Caught("loading the account to undo its creation", gerr)
		}

		return nil // already gone
	}

	if c.UpdatedUnixTime != 0 && acct.UpdatedUnixTime != c.UpdatedUnixTime {
		return Conflict("the account was edited after it was created; delete it in the browser if it should go", "account %s changed since provisioning", p.AccountId).WithDetails(map[string]any{"account_id": p.AccountId})
	}

	if err := services.Accounts.DeleteAccount(mc.Web, mc.Uid, id); err != nil {
		if errors.Is(err, errs.ErrAccountInUseCannotBeDeleted) {
			return Conflict("undo the import into this account first (POST /undo reverses the newest write first)", "account %s now holds transactions", p.AccountId).WithDetails(map[string]any{"account_id": p.AccountId})
		}

		return NewFail(CodeUpstreamError, "retry POST /undo", "could not delete account %s", p.AccountId)
	}

	return nil
}

func init() {
	RegisterInverse(ingInverseAccount, ingUndoAccountCreate)
	RegisterInverse(ingInverseMap, ingUndoMapRestore)
}

// ---- PUT /ingest/map and POST /ingest/map/infer ------------------------------------------------

// ingMapPutArgs are the arguments of PUT /ingest/map
type ingMapPutArgs struct {
	Root         string          `json:"root,omitempty"`
	Staging      string          `json:"staging,omitempty"`
	ManifestPath string          `json:"manifest_path,omitempty"`
	Map          json.RawMessage `json:"map"`
	Replace      bool            `json:"replace,omitempty"`
}

type ingMapDiff struct {
	AccountKey string `json:"account_key"`
	Change     string `json:"change"` // add | change | remove | unchanged
	From       string `json:"from,omitempty"`
	To         string `json:"to,omitempty"`
	Name       string `json:"name,omitempty"`
	Currency   string `json:"currency,omitempty"`
}

// ingParseMapArg reads {key: id} or [{account_key, account_id}]
func ingParseMapArg(raw json.RawMessage) (map[string]string, error) {
	out := map[string]string{}

	if len(raw) == 0 || string(raw) == "null" {
		return nil, Invalid("pass map as {\"entity/institution/last4\": \"account id\"} or [{\"account_key\": …, \"account_id\": …}]", "map is required")
	}

	var obj map[string]string

	if err := json.Unmarshal(raw, &obj); err == nil {
		for k, v := range obj {
			out[ingNormKey(k)] = strings.TrimSpace(v)
		}

		return out, nil
	}

	var list []struct {
		AccountKey string `json:"account_key"`
		AccountId  string `json:"account_id"`
	}

	if err := json.Unmarshal(raw, &list); err != nil {
		errfile.Expected("decoding the account map argument", err)
		return nil, Invalid("pass map as {\"entity/institution/last4\": \"account id\"}; ids are strings", "map is malformed")
	}

	for _, e := range list {
		out[ingNormKey(e.AccountKey)] = strings.TrimSpace(e.AccountId)
	}

	return out, nil
}

// ingPlanMapPut resolves the map write: validates every id and computes the diff
func ingPlanMapPut(mc *Ctx, args *ingMapPutArgs) (*ingContext, *ingMap, []*ingMapDiff, error) {
	ctx, err := ingOpenContext(mc, args.Root, args.ManifestPath, args.Staging, "", false)

	if err != nil {
		return nil, nil, nil, err
	}

	want, err := ingParseMapArg(args.Map)

	if err != nil {
		return nil, nil, nil, err
	}

	accounts, err := ingLoadAccounts(mc)

	if err != nil {
		return nil, nil, nil, err
	}

	rows := map[string]*ingManifestRow{}

	if ctx.Manifest != nil {
		for _, r := range ctx.Manifest.Rows {
			rows[r.AccountKey()] = r
		}
	}

	next := ingNewMap()

	if !args.Replace {
		for k, e := range ctx.Map.Accounts {
			cp := *e
			next.Accounts[k] = &cp
		}
	}

	now := time.Now().UTC().Format(time.RFC3339)
	var diff []*ingMapDiff

	keys := make([]string, 0, len(want))

	for k := range want {
		keys = append(keys, k)
	}

	sort.Strings(keys)

	for _, k := range keys {
		if k == "" || strings.Count(k, "/") < 2 {
			return nil, nil, nil, Invalid("account keys are entity/institution/last4 (GET /ingest/manifest prints them)", "map key %q is not an account key", k)
		}

		id, err := ResolveId("map["+k+"]", want[k])

		if err != nil {
			return nil, nil, nil, err
		}

		a := accounts.ById[id]

		if p := ingMapProblem(a, ""); p != "" {
			return nil, nil, nil, Invalid("map each key to a visible single account (GET /machine/v1/accounts)", "map[%s]: %s", k, p)
		}

		e := &ingMapEntry{AccountKey: k, AccountId: idString(id), Name: a.Name, Currency: a.Currency, Category: ingAccountCategoryName(a.Category), Source: "put", UpdatedAt: now}

		if r := rows[k]; r != nil {
			e.Entity, e.Institution, e.Label, e.Last4, e.Path = r.Entity, r.Institution, r.Label, r.Last4, r.Path

			if !r.CurrencyDefaulted && r.Currency != a.Currency {
				return nil, nil, nil, Invalid("map the key to an account in "+r.Currency+", or fix the manifest's currency", "map[%s]: the manifest says %s but the account is in %s", k, r.Currency, a.Currency)
			}
		}

		prev := ctx.Map.Accounts[k]

		switch {
		case prev == nil:
			diff = append(diff, &ingMapDiff{AccountKey: k, Change: "add", To: e.AccountId, Name: a.Name, Currency: a.Currency})
		case prev.AccountId != e.AccountId:
			diff = append(diff, &ingMapDiff{AccountKey: k, Change: "change", From: prev.AccountId, To: e.AccountId, Name: a.Name, Currency: a.Currency})
		default:
			diff = append(diff, &ingMapDiff{AccountKey: k, Change: "unchanged", From: prev.AccountId, To: e.AccountId, Name: a.Name, Currency: a.Currency})
			e = prev
		}

		next.Accounts[k] = e
	}

	if args.Replace {
		for k, e := range ctx.Map.Accounts {
			if _, ok := want[k]; !ok {
				diff = append(diff, &ingMapDiff{AccountKey: k, Change: "remove", From: e.AccountId, Name: e.Name})
			}
		}
	}

	sort.SliceStable(diff, func(i, j int) bool { return diff[i].AccountKey < diff[j].AccountKey })

	return ctx, next, diff, nil
}

// ingInferMap proposes a mapping for every manifest row without saving anything
func ingInferMap(mc *Ctx, args *ingAccountsArgs) (any, error) {
	plan, err := ingPlanAccounts(mc, args)

	if err != nil {
		return nil, err
	}

	var proposals []map[string]any
	counts := map[string]int{}

	for _, d := range plan.Plan {
		p := map[string]any{"account_key": d.AccountKey, "reason": d.Reason, "manifest": d.Manifest}

		switch d.Action {
		case "link":
			p["proposal"] = d.Existing
			p["confidence"] = "matched"

			if strings.HasPrefix(d.Reason, "already mapped") {
				p["confidence"] = "mapped"
			}
		case "ambiguous":
			p["proposal"] = nil
			p["confidence"] = "ambiguous"
			p["candidates"] = d.Candidates
		default:
			p["proposal"] = nil
			p["confidence"] = "none"

			if d.Action == "create" {
				p["would_create"] = d.Proposed
			}
		}

		counts[p["confidence"].(string)]++
		proposals = append(proposals, p)
	}

	if proposals == nil {
		proposals = []map[string]any{}
	}

	suggested := map[string]string{}

	for _, d := range plan.Plan {
		if d.Action == "link" && d.Existing != nil {
			suggested[d.AccountKey] = d.Existing.Id
		}
	}

	return map[string]any{"root": plan.Root, "manifest_path": plan.ManifestPath, "proposals": proposals, "summary": counts, "suggested_map": suggested, "saved": false,
		"next": "PUT /machine/v1/ingest/map with suggested_map (after review), or POST /ingest/accounts/apply to create the rest"}, nil
}

// ingOpeningTime is when an opening balance is dated: one second before noon UTC of the opening
// date. Imported statement rows are dated at noon UTC of their day (pm/import_formats.mdx §10), and
// upstream refuses any transaction dated before an account's balance-modification row, so the
// opening must sort strictly first while still showing on the opening date in every timezone from
// UTC-11 to UTC+11.
func ingOpeningTime(day time.Time) int64 {
	return time.Date(day.Year(), day.Month(), day.Day(), 12, 0, 0, 0, time.UTC).Unix() - 1
}

package machine

import (
	"encoding/json"
	"sort"
	"strings"
	"time"

	"github.com/mayswind/ezbookkeeping/pkg/models"
	"github.com/mayswind/ezbookkeeping/pkg/services"
)

// ingest_map.go — the statements→books map (apis.mdx §14.7): which ezBookkeeping account each
// statement account key imports into. It is written beside the statements, inside the staging
// directory ({ROOT}/.ezbk-staging/_map.json), never in the repo and never in the archive itself.
// It is written only by write-tier routes (PUT /ingest/map, POST /ingest/accounts/apply).

const ingMapFile = "_map.json"

// ingMapEntry maps one statement account to one ezBookkeeping account
type ingMapEntry struct {
	AccountKey  string `json:"account_key"`
	AccountId   string `json:"account_id"`
	Name        string `json:"name,omitempty"`
	Currency    string `json:"currency,omitempty"`
	Category    string `json:"category,omitempty"`
	Entity      string `json:"entity,omitempty"`
	Institution string `json:"institution,omitempty"`
	Label       string `json:"label,omitempty"`
	Last4       string `json:"last4,omitempty"`
	Path        string `json:"path,omitempty"`
	Source      string `json:"source,omitempty"`
	UpdatedAt   string `json:"updated_at,omitempty"`
}

// ingMap is the whole map file
type ingMap struct {
	Version   int                     `json:"version"`
	User      string                  `json:"user,omitempty"`
	UpdatedAt string                  `json:"updated_at,omitempty"`
	Accounts  map[string]*ingMapEntry `json:"accounts"`
}

func ingNewMap() *ingMap {
	return &ingMap{Version: 1, Accounts: map[string]*ingMapEntry{}}
}

// ingLoadMap reads the map (an absent map is an empty one). raw is the file's bytes, nil if absent.
func ingLoadMap(st *ingStaging) (*ingMap, []byte, error) {
	if st == nil {
		return ingNewMap(), nil, nil
	}

	raw, err := st.ReadFile(ingMapFile)

	if err != nil {
		return nil, nil, err
	}

	if raw == nil {
		return ingNewMap(), nil, nil
	}

	m := ingNewMap()

	if err := json.Unmarshal(raw, m); err != nil {
		return nil, nil, Conflict("the map file is damaged; rebuild it with POST /ingest/accounts/apply or PUT /ingest/map", "the statements map %s/%s cannot be parsed", st.Rel, ingMapFile)
	}

	if m.Accounts == nil {
		m.Accounts = map[string]*ingMapEntry{}
	}

	for k, e := range m.Accounts {
		if e == nil {
			delete(m.Accounts, k)
			continue
		}

		e.AccountKey = k
	}

	return m, raw, nil
}

// ingMarshalMap renders the map deterministically
func ingMarshalMap(m *ingMap) []byte {
	data, _ := json.MarshalIndent(m, "", "  ")
	return append(data, '\n')
}

// ingSaveMap writes the map atomically into staging (creating staging if needed)
func ingSaveMap(st *ingStaging, m *ingMap, username string) ([]byte, error) {
	if err := st.Ensure(); err != nil {
		return nil, err
	}

	m.Version = 1
	m.User = username
	m.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
	data := ingMarshalMap(m)

	if err := st.WriteFile(ingMapFile, data); err != nil {
		return nil, err
	}

	return data, nil
}

// ingAccountIndex is the bound user's accounts, indexed for resolution
type ingAccountIndex struct {
	All    []*models.Account
	ById   map[int64]*models.Account
	ByName map[string][]*models.Account
}

func ingLoadAccounts(mc *Ctx) (*ingAccountIndex, error) {
	accounts, err := services.Accounts.GetAllAccountsByUid(mc.Web, mc.Uid)

	if err != nil {
		return nil, NewFail(CodeUpstreamError, "check ~/T/ezbookkeeping/error.err", "cannot read the bound user's accounts")
	}

	idx := &ingAccountIndex{ById: map[int64]*models.Account{}, ByName: map[string][]*models.Account{}}

	for _, a := range accounts {
		if a.Deleted {
			continue
		}

		idx.All = append(idx.All, a)
		idx.ById[a.AccountId] = a
		idx.ByName[a.Name] = append(idx.ByName[a.Name], a)
	}

	sort.Slice(idx.All, func(i, j int) bool { return idx.All[i].AccountId < idx.All[j].AccountId })

	return idx, nil
}

// ingAccountCategoryNames are the §15.3 category names, as the wire spells them
var ingAccountCategoryNames = map[string]models.AccountCategory{
	"cash":                   models.ACCOUNT_CATEGORY_CASH,
	"checking":               models.ACCOUNT_CATEGORY_CHECKING_ACCOUNT,
	"credit_card":            models.ACCOUNT_CATEGORY_CREDIT_CARD,
	"virtual":                models.ACCOUNT_CATEGORY_VIRTUAL,
	"debt":                   models.ACCOUNT_CATEGORY_DEBT,
	"receivables":            models.ACCOUNT_CATEGORY_RECEIVABLES,
	"investment":             models.ACCOUNT_CATEGORY_INVESTMENT,
	"savings":                models.ACCOUNT_CATEGORY_SAVINGS_ACCOUNT,
	"certificate_of_deposit": models.ACCOUNT_CATEGORY_CERTIFICATE_OF_DEPOSIT,
}

func ingAccountCategoryName(c models.AccountCategory) string {
	for name, v := range ingAccountCategoryNames {
		if v == c {
			return name
		}
	}

	return "unknown"
}

func ingSide(c models.AccountCategory) string {
	switch {
	case c.IsLiability():
		return "liability"
	case c.IsAsset():
		return "asset"
	}

	return "unknown"
}

// ingMapProblem checks a mapped account is usable for importing rows of the given currency
func ingMapProblem(a *models.Account, currency string) string {
	switch {
	case a == nil:
		return "the mapped account no longer exists"
	case a.Type == models.ACCOUNT_TYPE_MULTI_SUB_ACCOUNTS:
		return "the mapped account is a parent account with sub-accounts; map a sub-account instead"
	case a.Hidden:
		return "the mapped account is hidden; unhide it in the browser or map another"
	case currency != "" && a.Currency != currency:
		return "the statement currency " + currency + " differs from the account's currency " + a.Currency + "; rows are never converted on import"
	}

	return ""
}

// ingMapView renders the map with each entry's live account state
func ingMapView(m *ingMap, idx *ingAccountIndex) []map[string]any {
	keys := make([]string, 0, len(m.Accounts))

	for k := range m.Accounts {
		keys = append(keys, k)
	}

	sort.Strings(keys)
	out := make([]map[string]any, 0, len(keys))

	for _, k := range keys {
		e := m.Accounts[k]
		item := map[string]any{
			"account_key": k,
			"account_id":  e.AccountId,
			"name":        e.Name,
			"currency":    e.Currency,
			"category":    e.Category,
			"source":      e.Source,
			"updated_at":  e.UpdatedAt,
		}

		status := "ok"

		if idx != nil {
			id, err := ResolveId("account_id", e.AccountId)
			var acct *models.Account

			if err == nil {
				acct = idx.ById[id]
			}

			if p := ingMapProblem(acct, ""); p != "" {
				status = p
			} else {
				item["current_name"] = acct.Name
				item["current_currency"] = acct.Currency
				item["side"] = ingSide(acct.Category)
			}
		}

		item["status"] = status
		out = append(out, item)
	}

	return out
}

// ingNormKey trims an account key argument
func ingNormKey(k string) string {
	return strings.Trim(strings.TrimSpace(k), "/")
}

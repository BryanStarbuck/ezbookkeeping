package machine

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/mayswind/ezbookkeeping/pkg/core"
	"github.com/mayswind/ezbookkeeping/pkg/models"
	"github.com/mayswind/ezbookkeeping/pkg/services"
)

// ingest_plan.go — plan and apply are ONE path (apis.mdx §14.8). ingEvaluate parses every file
// through upstream's converters, runs both de-duplication layers, consults machine_import_record,
// applies the maps and returns, per account, rows parsed, collapsed, already present (and already
// present but deleted), possible matches, transfer candidates, unmapped and NEW — plus the exact
// change set whose fingerprint the confirm token covers. The apply calls the same function again
// and refuses if the fingerprint moved.

// ingPlanArgs are the arguments of /ingest/plan and /ingest/apply
type ingPlanArgs struct {
	Root                string            `json:"root,omitempty"`
	Staging             string            `json:"staging,omitempty"`
	ManifestPath        string            `json:"manifest_path,omitempty"`
	Mode                string            `json:"mode,omitempty"`
	Accounts            []string          `json:"accounts,omitempty"`
	Start               string            `json:"start,omitempty"`
	End                 string            `json:"end,omitempty"`
	CategoryMap         map[string]string `json:"category_map,omitempty"`
	FallbackCategoryIds map[string]string `json:"fallback_category_ids,omitempty"`
	AcceptMatches       []json.RawMessage `json:"accept_matches,omitempty"`
	AcceptTransfers     []string          `json:"accept_transfers,omitempty"`
	ReimportDeleted     bool              `json:"reimport_deleted,omitempty"`
	Prefer              []string          `json:"prefer,omitempty"`
	QifDateOrder        string            `json:"qif_date_order,omitempty"`
	IncludeRows         bool              `json:"include_rows,omitempty"`
	RowsLimit           int               `json:"rows_limit,omitempty"`
}

// ingAccountInput is one account handed to the engine
type ingAccountInput struct {
	AccountKey     string
	Entity         string
	Institution    string
	Label          string
	Last4          string
	Kind           string
	Path           string
	Currency       string
	CurrencySource string
	Converter      string
	Statements     []*ingStatement
	Skipped        []*ingSkippedRow
	Conflicts      []*ingConflict
	Warnings       []string
	PreBlocked     []string
	// AccountId is set when the caller names the account directly (file import)
	AccountId int64
	BankIds   int
	Files     int
}

// ingRowCounts are the per-account row counts
type ingRowCounts struct {
	Parsed           int `json:"parsed"`
	Skipped          int `json:"skipped"`
	Superseded       int `json:"superseded"`
	BlockedConflict  int `json:"blocked_by_conflict"`
	Collapsed        int `json:"collapsed"`
	AfterDedupe      int `json:"after_dedupe"`
	OutOfRange       int `json:"out_of_range"`
	WithBankIds      int `json:"with_bank_ids"`
	WithMintedIds    int `json:"with_minted_ids"`
	CommentsTruncate int `json:"comments_truncated,omitempty"`
}

// ingPlanAccount is the plan's verdict on one account
type ingPlanAccount struct {
	AccountKey            string         `json:"account_key"`
	Entity                string         `json:"entity,omitempty"`
	Institution           string         `json:"institution,omitempty"`
	Label                 string         `json:"label,omitempty"`
	Last4                 string         `json:"last4,omitempty"`
	Kind                  string         `json:"kind,omitempty"`
	Path                  string         `json:"path,omitempty"`
	AccountId             string         `json:"account_id,omitempty"`
	AccountName           string         `json:"account_name,omitempty"`
	Currency              string         `json:"currency"`
	CurrencySource        string         `json:"currency_source,omitempty"`
	Converter             string         `json:"converter,omitempty"`
	Files                 int            `json:"files"`
	Statements            map[string]int `json:"statements"`
	Rows                  ingRowCounts   `json:"rows"`
	AlreadyPresent        int            `json:"already_present"`
	AlreadyPresentDeleted int            `json:"already_present_deleted"`
	ReimportDeleted       int            `json:"reimport_deleted"`
	PossibleMatches       int            `json:"possible_matches"`
	AcceptedMatches       int            `json:"accepted_matches"`
	TransferCandidates    int            `json:"transfer_candidates"`
	AcceptedTransfers     int            `json:"accepted_transfers"`
	Unmapped              int            `json:"unmapped"`
	ToFallback            int            `json:"to_fallback"`
	New                   int            `json:"new"`
	Blocked               bool           `json:"blocked"`
	BlockedRows           int            `json:"blocked_rows"`
	BlockedReasons        []string       `json:"blocked_reasons"`
	First                 string         `json:"first,omitempty"`
	Last                  string         `json:"last,omitempty"`
	Warnings              []string       `json:"warnings"`

	input   *ingAccountInput
	account *models.Account
}

// ingPlanRow is one row with the plan's verdict
type ingPlanRow struct {
	*ingRow
	Status             string   `json:"status"`
	TransactionId      string   `json:"transaction_id,omitempty"`
	CategoryId         string   `json:"category_id,omitempty"`
	CategoryVia        string   `json:"category_via,omitempty"`
	MatchCandidates    []string `json:"match_candidates,omitempty"`
	TransferCandidate  string   `json:"transfer_candidate,omitempty"`
	Reason             string   `json:"reason,omitempty"`
	accountId          int64
	priorTransactionId int64
}

// ingUnmapped is one category name the plan could not map
type ingUnmapped struct {
	Type       string   `json:"type"`
	Name       string   `json:"name"`
	Rows       int      `json:"rows"`
	Accounts   []string `json:"accounts"`
	Reason     string   `json:"reason"`
	FallbackId string   `json:"fallback_category_id,omitempty"`
	Candidates []string `json:"candidates,omitempty"`
}

// ingMatchCandidate is an existing, un-recorded transaction a statement row may duplicate
type ingMatchCandidate struct {
	TransactionId string `json:"transaction_id"`
	Date          string `json:"date"`
	Amount        int64  `json:"amount"`
	Comment       string `json:"comment"`
	DaysApart     int    `json:"days_apart"`
}

// ingPossibleMatch is a statement row with hand-entered twins (never decided by the plane)
type ingPossibleMatch struct {
	ImportId    string               `json:"import_id"`
	AccountKey  string               `json:"account_key"`
	Date        string               `json:"date"`
	Amount      int64                `json:"amount"`
	Currency    string               `json:"currency"`
	Description string               `json:"description"`
	Accepted    string               `json:"accepted_transaction_id,omitempty"`
	Candidates  []*ingMatchCandidate `json:"candidates"`
}

// ingTransferSide is one leg of a transfer candidate
type ingTransferSide struct {
	AccountKey  string `json:"account_key"`
	AccountId   string `json:"account_id,omitempty"`
	ImportId    string `json:"import_id"`
	Amount      int64  `json:"amount"`
	Description string `json:"description"`
	SourceFile  string `json:"source_file"`
}

// ingTransferCandidate is an equal-and-opposite pair across two mapped accounts on one date
type ingTransferCandidate struct {
	Id       string           `json:"id"`
	Date     string           `json:"date"`
	Amount   int64            `json:"amount"`
	Currency string           `json:"currency"`
	From     *ingTransferSide `json:"from"`
	To       *ingTransferSide `json:"to"`
	Accepted bool             `json:"accepted"`
}

// ingDeletedRow is a row whose imported transaction the operator deleted
type ingDeletedRow struct {
	ImportId      string `json:"import_id"`
	AccountKey    string `json:"account_key"`
	TransactionId string `json:"transaction_id"`
	Date          string `json:"date"`
	Amount        int64  `json:"amount"`
	Currency      string `json:"currency"`
	Description   string `json:"description"`
	WillReimport  bool   `json:"will_reimport"`
}

// ingCreate is one transaction the apply creates
type ingCreate struct {
	Kind           string   `json:"kind"` // expense | income | transfer
	ImportIds      []string `json:"import_ids"`
	AccountKey     string   `json:"account_key"`
	DestAccountKey string   `json:"dest_account_key,omitempty"`
	AccountId      string   `json:"account_id"`
	DestAccountId  string   `json:"dest_account_id,omitempty"`
	Amount         int64    `json:"amount"`
	DestAmount     int64    `json:"dest_amount,omitempty"`
	Currency       string   `json:"currency"`
	Date           string   `json:"date"`
	Time           int64    `json:"time"`
	UtcOffset      int16    `json:"utc_offset"`
	CategoryId     string   `json:"category_id"`
	Comment        string   `json:"comment"`
	SourceFiles    []string `json:"source_files"`
	Relink         []string `json:"relink,omitempty"`
	PriorTxnIds    []string `json:"prior_transaction_ids,omitempty"`
}

// ingLink links a statement row to an existing transaction (record only, nothing created)
type ingLink struct {
	ImportId      string `json:"import_id"`
	TransactionId string `json:"transaction_id"`
	AccountKey    string `json:"account_key"`
	SourceFile    string `json:"source_file"`
	// Relink re-points an existing record (its transaction was deleted) instead of inserting one
	Relink           bool   `json:"relink,omitempty"`
	PriorTransaction string `json:"prior_transaction_id,omitempty"`
}

// ingChangeSet is exactly what an apply writes; its fingerprint is what the token covers
type ingChangeSet struct {
	Creates []*ingCreate `json:"creates"`
	Links   []*ingLink   `json:"links"`
}

// Count is what the ceiling measures (API transactions created + records linked)
func (c *ingChangeSet) Count() int {
	return len(c.Creates) + len(c.Links)
}

// ingPlanResult is the whole plan
type ingPlanResult struct {
	Kind                string                  `json:"kind"`
	Root                string                  `json:"root,omitempty"`
	Staging             string                  `json:"staging,omitempty"`
	Mode                string                  `json:"mode"`
	ManifestPath        string                  `json:"manifest_path,omitempty"`
	Range               map[string]any          `json:"range"`
	Accounts            []*ingPlanAccount       `json:"accounts"`
	Totals              map[string]int          `json:"totals"`
	Unmapped            []*ingUnmapped          `json:"unmapped"`
	PossibleMatches     []*ingPossibleMatch     `json:"possible_matches"`
	TransferCandidates  []*ingTransferCandidate `json:"transfer_candidates"`
	DeletedRows         []*ingDeletedRow        `json:"already_present_deleted"`
	Conflicts           []*ingConflict          `json:"conflicts"`
	Collapsed           []*ingCollapse          `json:"collapsed"`
	Skipped             []*ingSkippedRow        `json:"skipped"`
	Statements          []*ingDupeVerdict       `json:"statements"`
	CategoryMap         map[string]string       `json:"category_map"`
	FallbackCategoryIds map[string]string       `json:"fallback_category_ids"`
	Warnings            []string                `json:"warnings"`
	Rows                []*ingPlanRow           `json:"rows,omitempty"`
	RowsTruncated       bool                    `json:"rows_truncated,omitempty"`

	changes *ingChangeSet
	allRows []*ingPlanRow
	staging *ingStaging
	root    *ingRoot
}

// ---------------------------------------------------------------------------------------------
// Categories
// ---------------------------------------------------------------------------------------------

type ingCategoryIndex struct {
	byId   map[int64]*models.TransactionCategory
	byName map[models.TransactionCategoryType]map[string][]*models.TransactionCategory
}

func ingLoadCategories(mc *Ctx) (*ingCategoryIndex, error) {
	cats, err := services.TransactionCategories.GetAllCategoriesByUid(mc.Web, mc.Uid, 0, -1)

	if err != nil {
		return nil, NewFail(CodeUpstreamError, "check ~/T/ezbookkeeping/error.err", "cannot read the bound user's categories")
	}

	idx := &ingCategoryIndex{byId: map[int64]*models.TransactionCategory{}, byName: map[models.TransactionCategoryType]map[string][]*models.TransactionCategory{}}

	for _, c := range cats {
		idx.byId[c.CategoryId] = c
	}

	for _, c := range cats {
		if !ingCategoryUsable(idx, c) {
			continue
		}

		if idx.byName[c.Type] == nil {
			idx.byName[c.Type] = map[string][]*models.TransactionCategory{}
		}

		idx.byName[c.Type][c.Name] = append(idx.byName[c.Type][c.Name], c)
	}

	return idx, nil
}

// ingCategoryUsable reports whether a category can carry a transaction (secondary, visible, with a
// visible parent)
func ingCategoryUsable(idx *ingCategoryIndex, c *models.TransactionCategory) bool {
	if c == nil || c.Deleted || c.Hidden || c.ParentCategoryId == 0 {
		return false
	}

	parent := idx.byId[c.ParentCategoryId]

	return parent != nil && !parent.Hidden && !parent.Deleted
}

var ingCategoryTypeNames = map[string]models.TransactionCategoryType{
	"expense":  models.CATEGORY_TYPE_EXPENSE,
	"income":   models.CATEGORY_TYPE_INCOME,
	"transfer": models.CATEGORY_TYPE_TRANSFER,
}

func ingCategoryTypeName(t models.TransactionCategoryType) string {
	for n, v := range ingCategoryTypeNames {
		if v == t {
			return n
		}
	}

	return "unknown"
}

// ingResolvedCategoryArgs are the validated category_map and fallback_category_ids
type ingResolvedCategoryArgs struct {
	mapTyped map[string]*models.TransactionCategory // "expense:Name" → category
	mapAny   map[string]*models.TransactionCategory // "Name" → category (type from the category)
	fallback map[models.TransactionCategoryType]*models.TransactionCategory
}

func ingResolveCategoryArgs(idx *ingCategoryIndex, catMap map[string]string, fallback map[string]string) (*ingResolvedCategoryArgs, error) {
	out := &ingResolvedCategoryArgs{mapTyped: map[string]*models.TransactionCategory{}, mapAny: map[string]*models.TransactionCategory{}, fallback: map[models.TransactionCategoryType]*models.TransactionCategory{}}

	get := func(arg, v string) (*models.TransactionCategory, error) {
		id, err := ResolveId(arg, v)

		if err != nil {
			return nil, err
		}

		c := idx.byId[id]

		if c == nil {
			return nil, NotFound("GET /machine/v1/categories lists the secondary categories and their ids", "%s names no category (%s)", arg, v)
		}

		if !ingCategoryUsable(idx, c) {
			return nil, Invalid("name a visible SECONDARY category (a primary category cannot carry a transaction)", "%s names a category that cannot carry a transaction (%s)", arg, v)
		}

		return c, nil
	}

	for key, v := range catMap {
		c, err := get("category_map["+key+"]", v)

		if err != nil {
			return nil, err
		}

		if i := strings.Index(key, ":"); i > 0 {
			if t, ok := ingCategoryTypeNames[strings.ToLower(key[:i])]; ok {
				if c.Type != t {
					return nil, Invalid("map an "+key[:i]+" name to an "+key[:i]+" category", "category_map[%s] names a %s category", key, ingCategoryTypeName(c.Type))
				}

				out.mapTyped[strings.ToLower(key[:i])+":"+key[i+1:]] = c
				continue
			}
		}

		out.mapAny[key] = c
	}

	for key, v := range fallback {
		t, ok := ingCategoryTypeNames[strings.ToLower(key)]

		if !ok {
			return nil, Invalid("fallback_category_ids takes the keys expense, income and transfer", "unknown fallback key %q", key)
		}

		c, err := get("fallback_category_ids."+key, v)

		if err != nil {
			return nil, err
		}

		if c.Type != t {
			return nil, Invalid("the "+key+" fallback must be an "+key+" category", "fallback_category_ids.%s names a %s category", key, ingCategoryTypeName(c.Type))
		}

		out.fallback[t] = c
	}

	return out, nil
}

// ingMapCategory maps a row's category name for a given type. via is exact | category_map |
// fallback; reason explains an unmapped row.
func ingMapCategory(idx *ingCategoryIndex, args *ingResolvedCategoryArgs, t models.TransactionCategoryType, name string) (c *models.TransactionCategory, via, reason string, candidates []string) {
	tn := ingCategoryTypeName(t)

	if m, ok := args.mapTyped[tn+":"+name]; ok {
		return m, "category_map", "", nil
	}

	if m, ok := args.mapAny[name]; ok && m.Type == t {
		return m, "category_map", "", nil
	}

	if name != "" {
		found := idx.byName[t][name]

		if len(found) == 1 {
			return found[0], "exact", "", nil
		}

		if len(found) > 1 {
			for _, f := range found {
				candidates = append(candidates, idString(f.CategoryId))
			}

			sort.Strings(candidates)
			reason = "ambiguous: several " + tn + " categories share this name"
		} else {
			reason = "no " + tn + " category has this name"
		}
	} else {
		reason = "the statement carries no category"
	}

	if fb := args.fallback[t]; fb != nil {
		return fb, "fallback", reason, candidates
	}

	return nil, "", reason, candidates
}

// ---------------------------------------------------------------------------------------------
// Existing transactions (for possible matches)
// ---------------------------------------------------------------------------------------------

type ingExistingTxn struct {
	Id      int64
	Date    string
	Amount  int64 // signed from the account's perspective
	Comment string
}

// ingLoadExisting reads non-deleted transactions of the given accounts in a date window, signed
// from each account's perspective, excluding balance modifications
func ingLoadExisting(mc *Ctx, accountIds []int64, first, last string) (map[int64][]*ingExistingTxn, error) {
	out := map[int64][]*ingExistingTxn{}

	if len(accountIds) == 0 || first == "" || last == "" {
		return out, nil
	}

	db, err := ingDB(mc)

	if err != nil {
		return nil, err
	}

	a, _ := time.Parse("2006-01-02", first)
	b, _ := time.Parse("2006-01-02", last)
	minMs := a.AddDate(0, 0, -4).Unix() * 1000
	maxMs := b.AddDate(0, 0, 5).Unix()*1000 + 999

	var txns []*models.Transaction

	if err := db.NewSession(mc.Web).Cols("transaction_id", "uid", "deleted", "type", "account_id", "amount", "transaction_time", "timezone_utc_offset", "comment").
		Where("uid=? AND deleted=? AND transaction_time>=? AND transaction_time<=?", mc.Uid, false, minMs, maxMs).
		In("account_id", accountIds).Find(&txns); err != nil {
		return nil, NewFail(CodeUpstreamError, "check ~/T/ezbookkeeping/error.err", "cannot read existing transactions")
	}

	for _, t := range txns {
		var signed int64

		switch t.Type {
		case models.TRANSACTION_DB_TYPE_INCOME, models.TRANSACTION_DB_TYPE_TRANSFER_IN:
			signed = t.Amount
		case models.TRANSACTION_DB_TYPE_EXPENSE, models.TRANSACTION_DB_TYPE_TRANSFER_OUT:
			signed = -t.Amount
		default:
			continue
		}

		sec := t.TransactionTime / 1000
		date := time.Unix(sec, 0).In(time.FixedZone("t", int(t.TimezoneUtcOffset)*60)).Format("2006-01-02")
		out[t.AccountId] = append(out[t.AccountId], &ingExistingTxn{Id: t.TransactionId, Date: date, Amount: signed, Comment: t.Comment})
	}

	for _, list := range out {
		sort.Slice(list, func(i, j int) bool {
			if list[i].Date != list[j].Date {
				return list[i].Date < list[j].Date
			}

			return list[i].Id < list[j].Id
		})
	}

	return out, nil
}

func ingDaysApart(a, b string) int {
	ta, err1 := time.Parse("2006-01-02", a)
	tb, err2 := time.Parse("2006-01-02", b)

	if err1 != nil || err2 != nil {
		return 1 << 20
	}

	d := int(ta.Sub(tb).Hours() / 24)

	if d < 0 {
		d = -d
	}

	return d
}

// ---------------------------------------------------------------------------------------------
// accept_matches
// ---------------------------------------------------------------------------------------------

// ingParseAcceptMatches reads [{"import_id":…,"transaction_id":…}] or ["import_id=transaction_id"]
func ingParseAcceptMatches(raw []json.RawMessage) (map[string]int64, error) {
	out := map[string]int64{}

	for _, r := range raw {
		var s string
		var importId, txn string

		if err := json.Unmarshal(r, &s); err == nil {
			i := strings.LastIndex(s, "=")

			if i <= 0 {
				return nil, Invalid("pass accept_matches as [{\"import_id\": \"…\", \"transaction_id\": \"…\"}] or [\"IMPORT_ID=TRANSACTION_ID\"]", "accept_matches entry %q is not IMPORT_ID=TRANSACTION_ID", s)
			}

			importId, txn = s[:i], s[i+1:]
		} else {
			var obj struct {
				ImportId      string `json:"import_id"`
				RowId         string `json:"row_id"`
				TransactionId string `json:"transaction_id"`
			}

			if err := json.Unmarshal(r, &obj); err != nil {
				return nil, Invalid("pass accept_matches as [{\"import_id\": \"…\", \"transaction_id\": \"…\"}]", "an accept_matches entry is malformed")
			}

			importId, txn = obj.ImportId, obj.TransactionId

			if importId == "" {
				importId = obj.RowId
			}
		}

		id, err := ResolveId("accept_matches transaction_id", txn)

		if err != nil {
			return nil, err
		}

		importId = strings.TrimSpace(importId)

		if importId == "" {
			return nil, Invalid("name the statement row by its import_id (from the plan's possible_matches)", "an accept_matches entry has no import_id")
		}

		out[importId] = id
	}

	return out, nil
}

// ---------------------------------------------------------------------------------------------
// The engine
// ---------------------------------------------------------------------------------------------

// ingEngineOpts are the evaluated arguments
type ingEngineOpts struct {
	Range           *DateRange
	CategoryMap     map[string]string
	Fallback        map[string]string
	AcceptMatches   []json.RawMessage
	AcceptTransfers []string
	ReimportDeleted bool
	// Lenient skips the account-map and category requirements (the /ingest/rows view)
	Lenient bool
}

const ingCommentMax = 255

// ingEvaluate turns account inputs into a plan. It reads the books; it never writes them.
func ingEvaluate(mc *Ctx, inputs []*ingAccountInput, m *ingMap, opts ingEngineOpts) (*ingPlanResult, error) {
	res := &ingPlanResult{
		Accounts: []*ingPlanAccount{}, Totals: map[string]int{}, Unmapped: []*ingUnmapped{}, PossibleMatches: []*ingPossibleMatch{},
		TransferCandidates: []*ingTransferCandidate{}, DeletedRows: []*ingDeletedRow{}, Conflicts: []*ingConflict{}, Collapsed: []*ingCollapse{},
		Skipped: []*ingSkippedRow{}, Statements: []*ingDupeVerdict{}, CategoryMap: opts.CategoryMap, FallbackCategoryIds: opts.Fallback, Warnings: []string{},
		changes: &ingChangeSet{Creates: []*ingCreate{}, Links: []*ingLink{}},
	}

	if res.CategoryMap == nil {
		res.CategoryMap = map[string]string{}
	}

	if res.FallbackCategoryIds == nil {
		res.FallbackCategoryIds = map[string]string{}
	}

	res.Range = map[string]any{}

	if opts.Range != nil {
		res.Range = map[string]any{"start": opts.Range.Start, "end": opts.Range.End, "startUnix": opts.Range.StartUnix, "endUnix": opts.Range.EndUnix, "timezone": opts.Range.Timezone}
	}

	accounts, err := ingLoadAccounts(mc)

	if err != nil {
		return nil, err
	}

	cats, err := ingLoadCategories(mc)

	if err != nil {
		return nil, err
	}

	catArgs, err := ingResolveCategoryArgs(cats, opts.CategoryMap, opts.Fallback)

	if err != nil {
		return nil, err
	}

	accepted, err := ingParseAcceptMatches(opts.AcceptMatches)

	if err != nil {
		return nil, err
	}

	// ---- per account: layer one, ids, layer two, range -------------------------------------
	var allRows []*ingPlanRow
	byImportId := map[string]*ingPlanRow{}

	for _, in := range inputs {
		pa := &ingPlanAccount{
			AccountKey: in.AccountKey, Entity: in.Entity, Institution: in.Institution, Label: in.Label, Last4: in.Last4, Kind: in.Kind, Path: in.Path,
			Currency: in.Currency, CurrencySource: in.CurrencySource, Converter: in.Converter, Files: in.Files, Statements: map[string]int{},
			BlockedReasons: []string{}, Warnings: append([]string{}, in.Warnings...), input: in,
		}

		res.Accounts = append(res.Accounts, pa)
		res.Conflicts = append(res.Conflicts, in.Conflicts...)
		res.Skipped = append(res.Skipped, in.Skipped...)
		pa.Rows.Skipped = len(in.Skipped)

		for _, s := range in.Statements {
			pa.Rows.Parsed += len(s.Rows)
		}

		l1 := ingLayerOne(in.AccountKey, in.Statements)
		res.Statements = append(res.Statements, l1.Verdicts...)
		res.Conflicts = append(res.Conflicts, l1.Conflicts...)

		for _, v := range l1.Verdicts {
			pa.Statements[v.Verdict]++

			if v.Verdict != "conflict" {
				// rows another statement already carries (a whole superseded or duplicate file,
				// or the overlapping part of a primary one)
				pa.Rows.Superseded += v.Rows - v.RowsUsed
			}
		}

		pa.Rows.BlockedConflict = l1.BlockedRows

		if len(l1.BlockedMonths) > 0 {
			months := make([]string, 0, len(l1.BlockedMonths))

			for mth := range l1.BlockedMonths {
				months = append(months, mth)
			}

			sort.Strings(months)
			pa.Warnings = append(pa.Warnings, "account-months blocked by disagreeing statements: "+strings.Join(months, ", "))
		}

		ingAssignIds(l1.Rows)
		rows, collapsed := ingLayerTwo(l1.Rows)
		res.Collapsed = append(res.Collapsed, collapsed...)
		pa.Rows.Collapsed = len(collapsed)
		pa.Rows.AfterDedupe = len(rows)

		for _, r := range rows {
			if r.IdSource == "bank" {
				pa.Rows.WithBankIds++
			} else {
				pa.Rows.WithMintedIds++
			}

			if pa.First == "" || r.Date < pa.First {
				pa.First = r.Date
			}

			if pa.Last == "" || r.Date > pa.Last {
				pa.Last = r.Date
			}

			pr := &ingPlanRow{ingRow: r, Status: "new"}

			if opts.Range != nil && (r.Date < opts.Range.Start || r.Date > opts.Range.End) {
				pr.Status = "out_of_range"
				pa.Rows.OutOfRange++
			}

			if prev, dup := byImportId[r.ImportId]; dup {
				// the same bank id under two account keys cannot happen (the key is in the id);
				// a repeat here means two inputs share an account key
				pr.Status = "collapsed"
				pr.Reason = "same import_id as a row of " + prev.SourceFile
				continue
			}

			byImportId[r.ImportId] = pr
			allRows = append(allRows, pr)
		}

		// resolve the account
		if in.AccountId > 0 {
			pa.account = accounts.ById[in.AccountId]

			if pa.account == nil {
				pa.BlockedReasons = append(pa.BlockedReasons, "account "+idString(in.AccountId)+" does not exist")
			}
		} else if e := m.Accounts[in.AccountKey]; e != nil {
			if id, err := ResolveId("account_id", e.AccountId); err == nil {
				pa.account = accounts.ById[id]
			}

			if pa.account == nil {
				pa.BlockedReasons = append(pa.BlockedReasons, "the map points at an account that no longer exists; re-run POST /ingest/accounts/plan and /apply, or PUT /ingest/map")
			}
		} else if !opts.Lenient && len(rows) == 0 {
			pa.Warnings = append(pa.Warnings, "no rows to import, and no ezBookkeeping account is mapped yet")
		} else if !opts.Lenient {
			pa.BlockedReasons = append(pa.BlockedReasons, "no ezBookkeeping account is mapped; run POST /ingest/accounts/plan then /ingest/accounts/apply (ezbk statements accounts --write), or PUT /ingest/map")
		}

		if pa.account != nil {
			pa.AccountId = idString(pa.account.AccountId)
			pa.AccountName = pa.account.Name

			if p := ingMapProblem(pa.account, ""); p != "" {
				pa.BlockedReasons = append(pa.BlockedReasons, p)
			}

			if pa.Currency == "" {
				pa.Currency = pa.account.Currency
			}

			currencies := map[string]int{}

			for _, r := range rows {
				currencies[r.Currency]++
			}

			for c, n := range currencies {
				if c != pa.account.Currency {
					pa.BlockedReasons = append(pa.BlockedReasons, strconv.Itoa(n)+" row(s) are in "+c+" but the account is in "+pa.account.Currency+"; rows are never converted on import — map a "+c+" account")
				}
			}
		}

		pa.BlockedReasons = append(pa.BlockedReasons, in.PreBlocked...)
		sort.Strings(pa.BlockedReasons)
		pa.BlockedReasons = ingUniq(pa.BlockedReasons)
	}

	// ---- vs. the books: machine_import_record ----------------------------------------------
	ids := make([]string, 0, len(allRows))

	for _, r := range allRows {
		ids = append(ids, r.ImportId)
	}

	records, err := ingLoadRecords(mc, ids)

	if err != nil {
		return nil, err
	}

	txnIds := make([]int64, 0, len(records))

	for _, rec := range records {
		txnIds = append(txnIds, rec.TransactionId)
	}

	states, err := ingLoadTxnStates(mc, txnIds)

	if err != nil {
		return nil, err
	}

	accountOf := map[string]*ingPlanAccount{}

	for _, pa := range res.Accounts {
		accountOf[pa.AccountKey] = pa
	}

	for _, r := range allRows {
		rec := records[r.ImportId]

		if rec == nil || r.Status == "out_of_range" {
			continue
		}

		st := states[rec.TransactionId]
		pa := accountOf[r.AccountKey]

		if st != nil && st.Exists && !st.Deleted {
			r.Status = "already_present"
			r.TransactionId = idString(rec.TransactionId)
			pa.AlreadyPresent++
			continue
		}

		r.TransactionId = idString(rec.TransactionId)
		r.priorTransactionId = rec.TransactionId
		res.DeletedRows = append(res.DeletedRows, &ingDeletedRow{ImportId: r.ImportId, AccountKey: r.AccountKey, TransactionId: idString(rec.TransactionId), Date: r.Date, Amount: r.Amount, Currency: r.Currency, Description: r.Description, WillReimport: opts.ReimportDeleted})
		pa.AlreadyPresentDeleted++

		if opts.ReimportDeleted {
			r.Status = "reimport"
			pa.ReimportDeleted++
		} else {
			r.Status = "already_present_deleted"
		}
	}

	// ---- possible matches against hand-entered, un-recorded transactions ------------------
	recorded, err := ingRecordedTransactionIds(mc)

	if err != nil {
		return nil, err
	}

	var acctIds []int64
	first, last := "", ""

	for _, pa := range res.Accounts {
		if pa.account != nil {
			acctIds = append(acctIds, pa.account.AccountId)
		}
	}

	for _, r := range allRows {
		if r.Status != "new" {
			continue
		}

		if first == "" || r.Date < first {
			first = r.Date
		}

		if last == "" || r.Date > last {
			last = r.Date
		}
	}

	existing, err := ingLoadExisting(mc, ingUniqInt64(acctIds), first, last)

	if err != nil {
		return nil, err
	}

	for _, r := range allRows {
		pa := accountOf[r.AccountKey]

		if pa.account == nil {
			continue
		}

		r.accountId = pa.account.AccountId

		if r.Status != "new" && r.Status != "reimport" {
			continue
		}

		var cands []*ingMatchCandidate

		for _, t := range existing[pa.account.AccountId] {
			if recorded[t.Id] || t.Amount != r.Amount {
				continue
			}

			if d := ingDaysApart(t.Date, r.Date); d <= 3 {
				cands = append(cands, &ingMatchCandidate{TransactionId: idString(t.Id), Date: t.Date, Amount: t.Amount, Comment: t.Comment, DaysApart: d})
			}
		}

		acceptedTxn, isAccepted := accepted[r.ImportId]

		if len(cands) == 0 && !isAccepted {
			continue
		}

		pm := &ingPossibleMatch{ImportId: r.ImportId, AccountKey: r.AccountKey, Date: r.Date, Amount: r.Amount, Currency: r.Currency, Description: r.Description, Candidates: cands}

		if pm.Candidates == nil {
			pm.Candidates = []*ingMatchCandidate{}
		}

		for _, c := range cands {
			r.MatchCandidates = append(r.MatchCandidates, c.TransactionId)
		}

		if isAccepted {
			pm.Accepted = idString(acceptedTxn)
			r.Status = "link"
			r.TransactionId = idString(acceptedTxn)
			pa.AcceptedMatches++
		} else {
			r.Status = "possible_match"
			pa.PossibleMatches++
		}

		res.PossibleMatches = append(res.PossibleMatches, pm)
	}

	// validate every accepted match
	if len(accepted) > 0 {
		var linkTxnIds []int64

		for importId, txn := range accepted {
			r := byImportId[importId]

			if r == nil || r.Status != "link" {
				// re-running an apply with the same flags must be harmless: a row linked (or
				// imported) by an earlier run is simply no longer linkable
				status := "not in this plan"

				if r != nil {
					status = r.Status
				}

				res.Warnings = append(res.Warnings, "accept_matches: "+importId+" ignored ("+status+")")
				delete(accepted, importId)

				continue
			}

			if recorded[txn] {
				return nil, Conflict("that transaction is already linked to another statement row", "transaction %s is already recorded as imported", idString(txn))
			}

			linkTxnIds = append(linkTxnIds, txn)
		}

		lst, err := ingLoadTxnStates(mc, linkTxnIds)

		if err != nil {
			return nil, err
		}

		for importId, txn := range accepted {
			st := lst[txn]
			r := byImportId[importId]

			if st == nil || !st.Exists || st.Deleted {
				return nil, NotFound("name an existing, non-deleted transaction id", "accept_matches: transaction %s does not exist", idString(txn))
			}

			if st.AccountId != r.accountId && !(st.Type == models.TRANSACTION_DB_TYPE_TRANSFER_OUT || st.Type == models.TRANSACTION_DB_TYPE_TRANSFER_IN) {
				return nil, Invalid("link a row only to a transaction in the same account", "accept_matches: transaction %s is not in the row's account", idString(txn))
			}
		}
	}

	// ---- transfer candidates -----------------------------------------------------------------
	acceptSet := map[string]bool{}

	for _, id := range opts.AcceptTransfers {
		if id = strings.TrimSpace(id); id != "" {
			acceptSet[id] = true
		}
	}

	var pool []*ingPlanRow

	for _, r := range allRows {
		if (r.Status == "new" || r.Status == "reimport") && r.accountId > 0 && r.Amount != 0 {
			pool = append(pool, r)
		}
	}

	sort.SliceStable(pool, func(i, j int) bool {
		a, b := pool[i], pool[j]

		if a.Date != b.Date {
			return a.Date < b.Date
		}

		if ingAbs(a.Amount) != ingAbs(b.Amount) {
			return ingAbs(a.Amount) < ingAbs(b.Amount)
		}

		if a.AccountKey != b.AccountKey {
			return a.AccountKey < b.AccountKey
		}

		return a.ImportId < b.ImportId
	})

	paired := map[*ingPlanRow]bool{}
	seenCandidate := map[string]bool{}

	for _, out := range pool {
		if out.Amount >= 0 || paired[out] {
			continue
		}

		for _, in := range pool {
			if in.Amount <= 0 || paired[in] || in.Date != out.Date || in.Amount != -out.Amount || in.Currency != out.Currency || in.accountId == out.accountId {
				continue
			}

			sum := sha256.Sum256([]byte(out.ImportId + "|" + in.ImportId))
			tc := &ingTransferCandidate{
				Id: "tc_" + hex.EncodeToString(sum[:])[:16], Date: out.Date, Amount: in.Amount, Currency: out.Currency,
				From: &ingTransferSide{AccountKey: out.AccountKey, AccountId: idString(out.accountId), ImportId: out.ImportId, Amount: out.Amount, Description: out.Description, SourceFile: out.SourceFile},
				To:   &ingTransferSide{AccountKey: in.AccountKey, AccountId: idString(in.accountId), ImportId: in.ImportId, Amount: in.Amount, Description: in.Description, SourceFile: in.SourceFile},
			}

			paired[out], paired[in] = true, true
			out.TransferCandidate, in.TransferCandidate = tc.Id, tc.Id
			seenCandidate[tc.Id] = true
			accountOf[out.AccountKey].TransferCandidates++
			accountOf[in.AccountKey].TransferCandidates++

			if acceptSet[tc.Id] {
				tc.Accepted = true
				out.Status, in.Status = "transfer", "transfer"
				accountOf[out.AccountKey].AcceptedTransfers++
				accountOf[in.AccountKey].AcceptedTransfers++
			}

			res.TransferCandidates = append(res.TransferCandidates, tc)

			break
		}
	}

	var gone []string

	for id := range acceptSet {
		if !seenCandidate[id] {
			gone = append(gone, id)
		}
	}

	if len(gone) > 0 {
		// an accepted candidate from an earlier plan that has since been imported is not an
		// error on a re-run; a mistyped id is reported the same way, and nothing is merged for it
		sort.Strings(gone)
		res.Warnings = append(res.Warnings, "accept_transfers: not a candidate in this plan (already imported, out of range, or mistyped): "+strings.Join(gone, ", "))
	}

	unaccepted := 0

	for _, tc := range res.TransferCandidates {
		if !tc.Accepted {
			unaccepted++
		}
	}

	if unaccepted > 0 {
		res.Warnings = append(res.Warnings, strconv.Itoa(unaccepted)+" transfer candidate(s) not accepted: both legs import as an expense and an income, so spending may be double-counted; accept them with accept_transfers")
	}

	// ---- categories --------------------------------------------------------------------------
	unmapped := map[string]*ingUnmapped{}

	for _, r := range allRows {
		pa := accountOf[r.AccountKey]

		switch r.Status {
		case "new", "reimport":
			t := models.CATEGORY_TYPE_EXPENSE

			if r.Amount > 0 {
				t = models.CATEGORY_TYPE_INCOME
			}

			c, via, reason, cands := ingMapCategory(cats, catArgs, t, r.CategoryName)

			if via != "exact" && via != "category_map" {
				key := ingCategoryTypeName(t) + "\x1f" + r.CategoryName
				u := unmapped[key]

				if u == nil {
					u = &ingUnmapped{Type: ingCategoryTypeName(t), Name: r.CategoryName, Reason: reason, Candidates: cands}

					if c != nil {
						u.FallbackId = idString(c.CategoryId)
					}

					unmapped[key] = u
				}

				u.Rows++
				u.Accounts = ingAddUniq(u.Accounts, r.AccountKey)
				pa.Unmapped++
			}

			if c == nil {
				r.Reason = reason

				if !opts.Lenient {
					pa.BlockedReasons = ingAddUniq(pa.BlockedReasons, "unmapped "+ingCategoryTypeName(t)+" categories and no fallback_category_ids."+ingCategoryTypeName(t))
				}

				continue
			}

			if via == "fallback" {
				pa.ToFallback++
			}

			r.CategoryId = idString(c.CategoryId)
			r.CategoryVia = via
		}
	}

	// transfers take a transfer category
	for _, tc := range res.TransferCandidates {
		if !tc.Accepted {
			continue
		}

		out, in := byImportId[tc.From.ImportId], byImportId[tc.To.ImportId]
		c, via, _, _ := ingMapCategory(cats, catArgs, models.CATEGORY_TYPE_TRANSFER, out.CategoryName)

		if c == nil && in.CategoryName != "" {
			c, via, _, _ = ingMapCategory(cats, catArgs, models.CATEGORY_TYPE_TRANSFER, in.CategoryName)
		}

		if c == nil {
			return nil, Invalid("pass fallback_category_ids.transfer (a secondary transfer category) to accept transfers", "accepted transfer %s needs a transfer category and none maps", tc.Id)
		}

		out.CategoryId, in.CategoryId = idString(c.CategoryId), idString(c.CategoryId)
		out.CategoryVia, in.CategoryVia = via, via
	}

	keys := make([]string, 0, len(unmapped))

	for k := range unmapped {
		keys = append(keys, k)
	}

	sort.Strings(keys)

	for _, k := range keys {
		res.Unmapped = append(res.Unmapped, unmapped[k])
	}

	// ---- blocks ------------------------------------------------------------------------------
	for _, pa := range res.Accounts {
		sort.Strings(pa.BlockedReasons)
		pa.Blocked = len(pa.BlockedReasons) > 0
	}

	for _, r := range allRows {
		pa := accountOf[r.AccountKey]

		switch r.Status {
		case "new", "reimport", "link", "transfer":
		default:
			continue
		}

		blocked := pa.Blocked

		if r.Status == "transfer" {
			other := byImportId[ingOtherLeg(res.TransferCandidates, r)]

			if other != nil && accountOf[other.AccountKey].Blocked {
				blocked = true
			}
		}

		if blocked {
			if r.Reason == "" {
				r.Reason = strings.Join(pa.BlockedReasons, "; ")
			}

			r.Status = "blocked"
			pa.BlockedRows++
		}
	}

	// ---- the change set ----------------------------------------------------------------------
	for _, r := range allRows {
		pa := accountOf[r.AccountKey]

		switch r.Status {
		case "new", "reimport":
			comment, cut := ingClipComment(r.Description)

			if cut {
				pa.Rows.CommentsTruncate++
			}

			cr := &ingCreate{
				Kind: "expense", ImportIds: []string{r.ImportId}, AccountKey: r.AccountKey, AccountId: idString(r.accountId),
				Amount: -r.Amount, Currency: r.Currency, Date: r.Date, Time: r.Time, UtcOffset: r.UtcOffset,
				CategoryId: r.CategoryId, Comment: comment, SourceFiles: []string{r.SourceFile},
			}

			if r.Amount > 0 {
				cr.Kind, cr.Amount = "income", r.Amount
			}

			if r.Status == "reimport" {
				cr.Relink = []string{r.ImportId}
				cr.PriorTxnIds = []string{idString(r.priorTransactionId)}
			}

			res.changes.Creates = append(res.changes.Creates, cr)
			pa.New++
		case "link":
			lk := &ingLink{ImportId: r.ImportId, TransactionId: r.TransactionId, AccountKey: r.AccountKey, SourceFile: r.SourceFile}

			if r.priorTransactionId > 0 {
				lk.Relink, lk.PriorTransaction = true, idString(r.priorTransactionId)
			}

			res.changes.Links = append(res.changes.Links, lk)
		}
	}

	for _, tc := range res.TransferCandidates {
		if !tc.Accepted {
			continue
		}

		out, in := byImportId[tc.From.ImportId], byImportId[tc.To.ImportId]

		if out.Status != "transfer" || in.Status != "transfer" {
			continue
		}

		comment, cut := ingClipComment(out.Description)

		if cut {
			accountOf[out.AccountKey].Rows.CommentsTruncate++
		}

		cr := &ingCreate{
			Kind: "transfer", ImportIds: []string{out.ImportId, in.ImportId}, AccountKey: out.AccountKey, DestAccountKey: in.AccountKey,
			AccountId: idString(out.accountId), DestAccountId: idString(in.accountId), Amount: -out.Amount, DestAmount: in.Amount,
			Currency: out.Currency, Date: out.Date, Time: out.Time, UtcOffset: out.UtcOffset, CategoryId: out.CategoryId,
			Comment: comment, SourceFiles: ingUniq([]string{out.SourceFile, in.SourceFile}),
		}

		for _, leg := range []*ingPlanRow{out, in} {
			if leg.priorTransactionId > 0 {
				cr.Relink = append(cr.Relink, leg.ImportId)
				cr.PriorTxnIds = append(cr.PriorTxnIds, idString(leg.priorTransactionId))
			}
		}

		res.changes.Creates = append(res.changes.Creates, cr)
		accountOf[out.AccountKey].New++
	}

	sort.SliceStable(res.changes.Creates, func(i, j int) bool {
		a, b := res.changes.Creates[i], res.changes.Creates[j]

		if a.AccountKey != b.AccountKey {
			return a.AccountKey < b.AccountKey
		}

		if a.Time != b.Time {
			return a.Time < b.Time
		}

		return a.ImportIds[0] < b.ImportIds[0]
	})

	sort.SliceStable(res.changes.Links, func(i, j int) bool { return res.changes.Links[i].ImportId < res.changes.Links[j].ImportId })

	// ---- totals ------------------------------------------------------------------------------
	for _, pa := range res.Accounts {
		res.Totals["accounts"]++
		res.Totals["parsed"] += pa.Rows.Parsed
		res.Totals["after_dedupe"] += pa.Rows.AfterDedupe
		res.Totals["collapsed"] += pa.Rows.Collapsed
		res.Totals["superseded_rows"] += pa.Rows.Superseded
		res.Totals["blocked_by_conflict"] += pa.Rows.BlockedConflict
		res.Totals["out_of_range"] += pa.Rows.OutOfRange
		res.Totals["already_present"] += pa.AlreadyPresent
		res.Totals["already_present_deleted"] += pa.AlreadyPresentDeleted
		res.Totals["reimport_deleted"] += pa.ReimportDeleted
		res.Totals["possible_matches"] += pa.PossibleMatches
		res.Totals["accepted_matches"] += pa.AcceptedMatches
		res.Totals["unmapped"] += pa.Unmapped
		res.Totals["to_fallback"] += pa.ToFallback
		res.Totals["new"] += pa.New
		res.Totals["blocked_rows"] += pa.BlockedRows
		res.Totals["skipped"] += pa.Rows.Skipped

		if pa.Blocked {
			res.Totals["blocked_accounts"]++
		}
	}

	res.Totals["transfer_candidates"] = len(res.TransferCandidates)
	res.Totals["accepted_transfers"] = 0

	for _, tc := range res.TransferCandidates {
		if tc.Accepted {
			res.Totals["accepted_transfers"]++
		}
	}

	res.Totals["creates"] = len(res.changes.Creates)
	res.Totals["links"] = len(res.changes.Links)
	res.Totals["changes"] = res.changes.Count()
	res.Totals["conflicts"] = len(res.Conflicts)

	ingSortPlanRows(allRows)
	res.allRows = allRows

	return res, nil
}

func ingOtherLeg(tcs []*ingTransferCandidate, r *ingPlanRow) string {
	for _, tc := range tcs {
		if tc.Id == r.TransferCandidate {
			if tc.From.ImportId == r.ImportId {
				return tc.To.ImportId
			}

			return tc.From.ImportId
		}
	}

	return ""
}

// ingClipComment keeps a description within the comment column's 255 characters
func ingClipComment(s string) (string, bool) {
	if utf8.RuneCountInString(s) <= ingCommentMax {
		return s, false
	}

	r := []rune(s)

	return string(r[:ingCommentMax]), true
}

func ingSortPlanRows(rows []*ingPlanRow) {
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

		return a.ImportId < b.ImportId
	})
}

func ingAbs(n int64) int64 {
	if n < 0 {
		return -n
	}

	return n
}

func ingUniq(in []string) []string {
	out := make([]string, 0, len(in))
	seen := map[string]bool{}

	for _, s := range in {
		if s != "" && !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}

	return out
}

func ingAddUniq(list []string, s string) []string {
	for _, x := range list {
		if x == s {
			return list
		}
	}

	return append(list, s)
}

func ingUniqInt64(in []int64) []int64 {
	seen := map[int64]bool{}
	var out []int64

	for _, v := range in {
		if !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}

	return out
}

// ---------------------------------------------------------------------------------------------
// Building inputs: prepared mode and raw mode
// ---------------------------------------------------------------------------------------------

// ingContext is the resolved root, staging, manifest and map of a call
type ingContext struct {
	Root     *ingRoot
	Staging  *ingStaging
	Manifest *ingManifest
	Map      *ingMap
	MapRaw   []byte
	Mode     string
}

// ingOpenContext resolves the root, the manifest (if any), the staging directory and the map
func ingOpenContext(mc *Ctx, rootArg, manifestArg, stagingArg, modeArg string, requireManifest bool) (*ingContext, error) {
	root, err := ingResolveRoot(rootArg)

	if err != nil {
		return nil, err
	}

	ctx := &ingContext{Root: root}
	mreal, err := ingFindManifest(root, manifestArg)

	if err != nil {
		return nil, err
	}

	if mreal != "" {
		ctx.Manifest, err = ingReadManifest(root, mreal, ingDefaultCurrency(mc))

		if err != nil {
			return nil, err
		}
	} else if requireManifest {
		return nil, NotFound("add import/accounts.csv to the archive (apis.mdx §14.2) or pass manifest_path", "no manifest found under the statements root")
	}

	manifestRel := ""

	if ctx.Manifest != nil {
		manifestRel = ctx.Manifest.Path
	}

	ctx.Staging, err = ingResolveStaging(root, stagingArg, manifestRel)

	if err != nil {
		return nil, err
	}

	if ctx.Staging.Exists() {
		ctx.Map, ctx.MapRaw, err = ingLoadMap(ctx.Staging)

		if err != nil {
			return nil, err
		}
	} else {
		ctx.Map = ingNewMap()
	}

	switch strings.ToLower(strings.TrimSpace(modeArg)) {
	case "prepared":
		ctx.Mode = "prepared"
	case "raw":
		ctx.Mode = "raw"
	case "", "auto":
		if ctx.Manifest != nil {
			ctx.Mode = "prepared"
		} else {
			ctx.Mode = "raw"
		}
	default:
		return nil, Invalid("mode is prepared or raw", "unknown mode %q", modeArg)
	}

	return ctx, nil
}

func ingDefaultCurrency(mc *Ctx) string {
	if mc.User != nil && mc.User.DefaultCurrency != "" {
		return mc.User.DefaultCurrency
	}

	return "USD"
}

// ingAccountSelected applies the accounts[] filter (key, label, path or last4)
func ingAccountSelected(filter []string, key, label, path, last4 string) bool {
	if len(filter) == 0 {
		return true
	}

	for _, f := range filter {
		f = ingNormKey(f)

		if f == "" {
			continue
		}

		if strings.EqualFold(f, key) || strings.EqualFold(f, label) || (path != "" && strings.EqualFold(f, path)) || (last4 != "" && f == last4) {
			return true
		}
	}

	return false
}

// ingPreparedInputs builds engine inputs from the manifest (prepared mode)
func ingPreparedInputs(mc *Ctx, ctx *ingContext, filter []string, prefer map[string]bool, qifOrder string, progress func(string, int, int)) ([]*ingAccountInput, error) {
	if ctx.Manifest == nil {
		return nil, NotFound("add import/accounts.csv (apis.mdx §14.2) or pass manifest_path; a raw archive uses mode=raw after POST /ingest/extract", "prepared mode needs a manifest and none was found")
	}

	if qifOrder != "" && !ingQifOrders[qifOrder] {
		return nil, Invalid("qif_date_order is ymd, mdy or dmy", "unknown qif_date_order %q", qifOrder)
	}

	var inputs []*ingAccountInput
	var rows []*ingManifestRow

	for _, row := range ctx.Manifest.Rows {
		if !row.Skip && ingAccountSelected(filter, row.AccountKey(), row.Label, row.Path, row.Last4) {
			rows = append(rows, row)
		}
	}

	if len(filter) > 0 && len(rows) == 0 {
		return nil, NotFound("GET /ingest/manifest lists the account keys", "accounts[] matched no manifest row")
	}

	for i, row := range rows {
		if progress != nil {
			progress("parse", i, len(rows))
		}

		in := &ingAccountInput{AccountKey: row.AccountKey(), Entity: row.Entity, Institution: row.Institution, Label: row.Label, Last4: row.Last4, Kind: row.Kind, Path: row.Path, Currency: row.Currency, CurrencySource: "manifest"}

		if row.CurrencyDefaulted {
			in.CurrencySource = "default"
		}

		if e := ctx.Map.Accounts[in.AccountKey]; e != nil && e.Currency != "" && row.CurrencyDefaulted {
			in.Currency, in.CurrencySource = e.Currency, "map"
		}

		in.Warnings = append(in.Warnings, row.Warnings...)
		shelf, err := ingScanAccountShelf(ctx.Root, row, qifOrder)

		if err != nil {
			in.PreBlocked = append(in.PreBlocked, "the account path is outside the statements root")
			inputs = append(inputs, in)
			continue
		}

		in.Converter = shelf.FileType

		switch {
		case !shelf.Exists:
			in.PreBlocked = append(in.PreBlocked, "the account directory "+row.Path+" does not exist under the root")
		case len(shelf.Chosen) == 0:
			in.PreBlocked = append(in.PreBlocked, "no importable file (ofx, qfx, qif, camt, mt940, ezbookkeeping csv) under "+row.Path+"; for PDFs use raw mode")
		case shelf.Needs == "qif_date_order":
			in.PreBlocked = append(in.PreBlocked, "QIF files need a date order: set qif_date_order (ymd, mdy or dmy) in the manifest or the call; the plane never guesses it")
		case shelf.Needs == "column_map":
			in.PreBlocked = append(in.PreBlocked, "custom delimited or Excel files need a column map; import them one at a time with POST /ingest/file/plan")
		}

		if len(in.PreBlocked) > 0 {
			inputs = append(inputs, in)
			continue
		}

		for _, f := range shelf.Chosen {
			in.Files++

			if limit := int64(mc.Config.MaxImportFileSize); limit > 0 && f.Size > limit {
				in.Conflicts = append(in.Conflicts, &ingConflict{AccountKey: in.AccountKey, Kind: "file_too_large", Files: []string{f.Rel}, Message: "the file is larger than the server's import limit (" + strconv.FormatInt(limit, 10) + " bytes)", Hint: "raise [data] max_import_file_size in conf/ezbookkeeping.ini and restart, or rely on the monthly files"})
				continue
			}

			data, err := ingReadFileBytes(f.Real, 0)

			if err != nil {
				in.Conflicts = append(in.Conflicts, &ingConflict{AccountKey: in.AccountKey, Kind: "unreadable_file", Files: []string{f.Rel}, Message: "the file cannot be read", Hint: "check the file's permissions and size"})
				continue
			}

			items, perr := ingParseUpstream(mc, data, filepath.Base(f.Rel), f.FileType, nil)

			if perr != nil {
				fl := toFail(perr)
				in.Conflicts = append(in.Conflicts, &ingConflict{AccountKey: in.AccountKey, Kind: "unreadable_file", Files: []string{f.Rel}, Message: "the " + f.FileType + " converter could not read the file: " + fl.Message, Hint: "open the file; if it is valid, the converter may need an upstream fix (apis.mdx §20)"})
				continue
			}

			nrows, skipped := ingNormalize(items, ingNormalizeInput{AccountKey: in.AccountKey, SourceFile: f.Rel, SourceKind: f.FileType, FileType: f.FileType, ExpectedCurrency: in.Currency})

			if refs := ingBankRefs(f.FileType, data); len(refs) > 0 {
				if ingAlignBankIds(nrows, refs) {
					in.BankIds += len(nrows)
				} else {
					in.Warnings = append(in.Warnings, f.Rel+": the bank's transaction ids could not be paired one-to-one with the converter's rows; minted ids used for this file")
				}
			}

			in.Skipped = append(in.Skipped, skipped...)
			in.Statements = append(in.Statements, &ingStatement{File: f.Rel, FileType: f.FileType, Sha: ingSha256(data), ModTime: f.ModTime, Rows: nrows, Preferred: prefer[f.Rel]})
		}

		inputs = append(inputs, in)
	}

	return inputs, nil
}

// ---------------------------------------------------------------------------------------------
// Token → arguments (so an apply can take only confirm_token, apis.mdx §14.4)
// ---------------------------------------------------------------------------------------------

type ingStoredArgs struct {
	route    string
	uid      int64
	args     json.RawMessage
	fileData []byte
	fileName string
	expires  time.Time
}

var ingTokenArgs = struct {
	sync.Mutex
	m map[string]*ingStoredArgs
}{m: map[string]*ingStoredArgs{}}

func ingRememberArgs(token, route string, uid int64, args any, fileData []byte, fileName string) {
	raw, _ := json.Marshal(args)
	now := time.Now()

	ingTokenArgs.Lock()
	defer ingTokenArgs.Unlock()

	for k, v := range ingTokenArgs.m {
		if now.After(v.expires) {
			delete(ingTokenArgs.m, k)
		}
	}

	ingTokenArgs.m[token] = &ingStoredArgs{route: route, uid: uid, args: raw, fileData: fileData, fileName: fileName, expires: now.Add(ConfirmTTL)}
}

func ingRecallArgs(token, route string, uid int64) *ingStoredArgs {
	ingTokenArgs.Lock()
	defer ingTokenArgs.Unlock()

	s := ingTokenArgs.m[token]

	if s == nil || s.route != route || s.uid != uid || time.Now().After(s.expires) {
		return nil
	}

	return s
}

// ingIssueToken issues a confirm token for another route's apply (the plan route's token is the
// apply route's). It is bound to that route, the user and the fingerprint.
func ingIssueToken(mc *Ctx, applyMethod, applyPath, fingerprint string) (string, time.Time) {
	shadow := *mc
	shadow.Route = &RouteDef{Method: applyMethod, Path: applyPath}

	return issueConfirmToken(&shadow, fingerprint)
}

// ingPlanRange resolves start/end (both optional)
func ingPlanRange(mc *Ctx, start, end string) (*DateRange, error) {
	if start == "" && end == "" {
		return nil, nil
	}

	if start == "" {
		start = "1900-01-01"
	}

	if end == "" {
		end = time.Now().In(mc.Loc).AddDate(1, 0, 0).Format("2006-01-02")
	}

	s, err := ParseDate("start", start, mc.Loc)

	if err != nil {
		return nil, err
	}

	e, err := ParseDate("end", end, mc.Loc)

	if err != nil {
		return nil, err
	}

	if e.Before(s) {
		return nil, Invalid("end must be on or after start", "end %s is before start %s", end, start)
	}

	return &DateRange{Start: s.Format("2006-01-02"), End: e.Format("2006-01-02"), StartUnix: s.Unix(), EndUnix: e.AddDate(0, 0, 1).Unix() - 1, Timezone: mc.Loc.String()}, nil
}

// ingRequireImportAllowed refuses when the bound user may not import (upstream's restriction)
func ingRequireImportAllowed(mc *Ctx) error {
	if mc.User != nil && mc.User.FeatureRestriction.Contains(core.USER_FEATURE_RESTRICTION_TYPE_IMPORT_TRANSACTION) {
		return NewFail(CodeForbidden, "an administrator must lift the bound user's import restriction", "the bound user %q may not import transactions", mc.User.Username)
	}

	return nil
}

// ingPlanStatements runs the whole prepared/raw plan for a statements root
func ingPlanStatements(mc *Ctx, args *ingPlanArgs, progress func(string, int, int)) (*ingPlanResult, error) {
	if err := ingRequireImportAllowed(mc); err != nil {
		return nil, err
	}

	ctx, err := ingOpenContext(mc, args.Root, args.ManifestPath, args.Staging, args.Mode, false)

	if err != nil {
		return nil, err
	}

	rng, err := ingPlanRange(mc, args.Start, args.End)

	if err != nil {
		return nil, err
	}

	prefer := ingPreferSet(args.Prefer)
	var inputs []*ingAccountInput

	if ctx.Mode == "prepared" {
		inputs, err = ingPreparedInputs(mc, ctx, args.Accounts, prefer, strings.ToLower(args.QifDateOrder), progress)
	} else {
		inputs, err = ingRawInputs(mc, ctx, args.Accounts, progress)
	}

	if err != nil {
		return nil, err
	}

	res, err := ingEvaluate(mc, inputs, ctx.Map, ingEngineOpts{Range: rng, CategoryMap: args.CategoryMap, Fallback: args.FallbackCategoryIds, AcceptMatches: args.AcceptMatches, AcceptTransfers: args.AcceptTransfers, ReimportDeleted: args.ReimportDeleted})

	if err != nil {
		return nil, err
	}

	res.Kind = "statements"
	res.Root = ctx.Root.Display
	res.Staging = ctx.Staging.Rel
	res.Mode = ctx.Mode
	res.staging = ctx.Staging
	res.root = ctx.Root

	if ctx.Manifest != nil {
		res.ManifestPath = ctx.Manifest.Path
	}

	ingAttachRows(res, args.IncludeRows, args.RowsLimit)

	return res, nil
}

// ingAttachRows adds the row list to the response when asked (capped, and the cap reported)
func ingAttachRows(res *ingPlanResult, include bool, limit int) {
	if !include {
		return
	}

	if limit <= 0 {
		limit = DefaultLimit
	}

	if limit > MaxLimit {
		limit = MaxLimit
	}

	if len(res.allRows) > limit {
		res.Rows = res.allRows[:limit]
		res.RowsTruncated = true
	} else {
		res.Rows = res.allRows
	}

	if res.Rows == nil {
		res.Rows = []*ingPlanRow{}
	}
}

// ingFileExists reports a regular file
func ingFileExists(p string) bool {
	info, err := os.Stat(p)
	return err == nil && !info.IsDir()
}

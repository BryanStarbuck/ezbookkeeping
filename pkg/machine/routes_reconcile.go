package machine

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/mayswind/ezbookkeeping/pkg/api"
	"github.com/mayswind/ezbookkeeping/pkg/models"
	"github.com/mayswind/ezbookkeeping/pkg/services"
)

// routes_reconcile.go — reconciliation (apis.mdx §10.2). ezBookkeeping has a reconciliation
// STATEMENT and a last-reconciled time per account, but no cleared flag per transaction and no
// "adjust" step, so plan/apply is composed here: the plan shows the app's balance, the operator's
// figure, the signed difference and the rows since the last reconciliation; apply optionally adds
// ONE visible income/expense adjustment and sets the last reconciled time.

func init() {
	registerRoutes(txnReconcileRoutes)
	RegisterInverse(txnOpReconciledTime, txnExecReconciledTime)
}

// txn.reconciled_time sets an account's last reconciled time back to a recorded value
const txnOpReconciledTime = "txn.reconciled_time"

const txnReconcileApplyRoute = "POST /accounts/:id/reconcile/apply"

func txnReconcileRoutes() []RouteDef {
	untrusted := []string{"comment", "name", "sourceAccount.name", "destinationAccount.name"}

	return []RouteDef{
		{Method: "GET", Path: "/accounts/:id/reconciliation", Tier: TierRead, Summary: "Upstream's reconciliation statement: opening, inflows, outflows, closing, per-row running balance.", Handler: txnHandleReconciliation, Untrusted: untrusted, Features: []string{"reconcile"}},
		{Method: "POST", Path: "/accounts/:id/reconcile/plan", Tier: TierRead, Composed: true, Summary: "The app's balance as of a date against the operator's figure, the difference, and the rows since the last reconciliation; returns the confirm_token for apply.", Handler: txnHandleReconcilePlan, Untrusted: untrusted},
		{Method: "POST", Path: "/accounts/:id/reconcile/apply", Tier: TierWrite, DryRunnable: true, Composed: true, Summary: "Mark the account reconciled; with create_adjustment, first add ONE visible adjustment for the difference.", Handler: txnHandleReconcileApply},
	}
}

// txnResolvePathAccount resolves /accounts/:id where :id is an id or, as a convenience, a name
func txnResolvePathAccount(mc *Ctx, lk *txnLookup) (*models.Account, error) {
	raw := strings.TrimSpace(mc.Param("id"))

	if raw == "" {
		return nil, Invalid("pass the account id in the path", "account id is required")
	}

	if _, err := strconv.ParseInt(raw, 10, 64); err == nil {
		return lk.resolveAccount("id", raw, "account_name", "", true)
	}

	name, _ := url.PathUnescape(raw)

	return lk.resolveAccount("id", "", "account name", name, true)
}

func txnAccountInfo(a *models.Account) map[string]any {
	info := map[string]any{
		"id":          idString(a.AccountId),
		"name":        a.Name,
		"currency":    a.Currency,
		"category":    int(a.Category),
		"isAsset":     a.Category.IsAsset(),
		"isLiability": a.Category.IsLiability(),
		"hidden":      a.Hidden,
	}

	return info
}

func txnLastReconciled(a *models.Account, loc *time.Location) (any, any) {
	if t := a.GetLastReconciledTime(); t > 0 {
		return t, DateOfUnix(t, loc)
	}

	return nil, nil
}

// txnStatement is upstream's TransactionReconciliationStatementResponse, read generically
type txnStatement struct {
	Transactions   []map[string]any `json:"transactions"`
	TotalInflows   string           `json:"totalInflows"`
	TotalOutflows  string           `json:"totalOutflows"`
	OpeningBalance string           `json:"openingBalance"`
	ClosingBalance string           `json:"closingBalance"`
}

func txnReadStatement(mc *Ctx, accountId int64, startUnix, endUnix int64) (*txnStatement, error) {
	q := url.Values{"account_id": {idString(accountId)}}

	if startUnix > 0 {
		q.Set("start_time", strconv.FormatInt(startUnix, 10))
	}

	if endUnix > 0 {
		q.Set("end_time", strconv.FormatInt(endUnix, 10))
	}

	var st txnStatement

	if err := mc.CallUpstreamInto(api.Transactions.TransactionReconciliationStatementHandler, "GET", q, nil, &st); err != nil {
		return nil, err
	}

	return &st, nil
}

func txnStatementAmount(field, v string) (int64, error) {
	if strings.TrimSpace(v) == "" {
		return 0, nil
	}

	n, err := ParseAmountString(v)

	if err != nil {
		if f, ok := err.(*Fail); ok {
			return 0, f
		}

		return 0, NewFail(CodeUpstreamError, "read ~/T/ezbookkeeping/error.err for the server-side detail", "upstream returned a malformed %s", field)
	}

	return n, nil
}

func txnHandleReconciliation(mc *Ctx) (any, error) {
	lk, err := txnLoadLookup(mc)

	if err != nil {
		return nil, err
	}

	acct, err := txnResolvePathAccount(mc, lk)

	if err != nil {
		return nil, err
	}

	var startUnix, endUnix int64
	rng := map[string]any{"timezone": mc.Loc.String()}

	if s := mc.Query("start"); s != "" {
		t, err := ParseDate("start", s, mc.Loc)

		if err != nil {
			return nil, err
		}

		startUnix = t.Unix()
		rng["start"] = t.Format("2006-01-02")
		rng["startUnix"] = startUnix
	}

	if e := mc.Query("end"); e != "" {
		t, err := ParseDate("end", e, mc.Loc)

		if err != nil {
			return nil, err
		}

		endUnix = t.AddDate(0, 0, 1).Add(-time.Second).Unix()
		rng["end"] = t.Format("2006-01-02")
		rng["endUnix"] = endUnix
	}

	if startUnix > 0 && endUnix > 0 && endUnix < startUnix {
		return nil, Invalid("end must be on or after start", "end is before start")
	}

	st, err := txnReadStatement(mc, acct.AccountId, startUnix, endUnix)

	if err != nil {
		return nil, err
	}

	integFillTags(mc, st.Transactions)

	for _, r := range st.Transactions {
		txnDecorate(r, lk, mc.Loc)
		integNameRow(r, lk)
	}

	if st.Transactions == nil {
		st.Transactions = []map[string]any{}
	}

	lastTime, lastDate := txnLastReconciled(acct, mc.Loc)

	return map[string]any{
		"account":               txnAccountInfo(acct),
		"currency":              acct.Currency,
		"range":                 rng,
		"openingBalance":        st.OpeningBalance,
		"closingBalance":        st.ClosingBalance,
		"totalInflows":          st.TotalInflows,
		"totalOutflows":         st.TotalOutflows,
		"transactions":          st.Transactions,
		"count":                 len(st.Transactions),
		"order":                 "-time",
		"lastReconciledTime":    lastTime,
		"lastReconciledDate":    lastDate,
		"balanceSignConvention": "the account's own balance sign: a liability owed is negative",
		"useLastReconciledTime": mc.User != nil && mc.User.UseLastReconciledTime,
	}, nil
}

// txnReconcileArgs are the plan/apply arguments; both routes take the same ones so the plan's token
// covers exactly the change set apply will make
type txnReconcileArgs struct {
	TargetBalance          json.Number `json:"target_balance"`
	AsOf                   string      `json:"as_of,omitempty"`
	CreateAdjustment       bool        `json:"create_adjustment,omitempty"`
	AdjustmentCategoryId   string      `json:"adjustment_category_id,omitempty"`
	AdjustmentCategoryName string      `json:"adjustment_category_name,omitempty"`
	AdjustmentComment      *string     `json:"adjustment_comment,omitempty"`
	MarkReconciled         *bool       `json:"mark_reconciled,omitempty"`
}

// txnReconcileState is the resolve half's decision
type txnReconcileState struct {
	Account        *models.Account
	AsOf           string
	At             int64 // unix seconds: the adjustment's time and the new last reconciled time
	AppBalance     int64
	Target         int64
	Difference     int64
	Adjustment     *txnState
	Mark           bool
	PriorReconcile *int64
	Blocked        string
	Info           map[string]any
}

// txnAdjustmentType decides the adjustment's type from the signed difference (target − app)
func txnAdjustmentType(diff int64) (models.TransactionType, int64) {
	if diff > 0 {
		return models.TRANSACTION_TYPE_INCOME, diff
	}

	return models.TRANSACTION_TYPE_EXPENSE, -diff
}

// txnReconcileAt is the instant a reconciliation "as of" a date stands at: the end of that day, or
// now when that is earlier (a reconciliation cannot be in the future)
func txnReconcileAt(asOf time.Time, now time.Time, loc *time.Location) int64 {
	end := time.Date(asOf.Year(), asOf.Month(), asOf.Day(), 23, 59, 59, 0, loc).Unix()

	if n := now.Unix(); n < end {
		return n
	}

	return end
}

func txnResolveReconcile(mc *Ctx, args txnReconcileArgs) (*txnReconcileState, error) {
	lk, err := txnLoadLookup(mc)

	if err != nil {
		return nil, err
	}

	acct, err := txnResolvePathAccount(mc, lk)

	if err != nil {
		return nil, err
	}

	if args.TargetBalance == "" {
		return nil, Invalid("pass target_balance: the statement's closing balance in integer hundredths (a liability owed is negative)", "target_balance is required")
	}

	target, err := AmountArg("target_balance", args.TargetBalance)

	if err != nil {
		return nil, err
	}

	now := time.Now().In(mc.Loc)
	asOfStr := strings.TrimSpace(args.AsOf)

	if asOfStr == "" {
		asOfStr = now.Format("2006-01-02")
	}

	asOf, err := ParseDate("as_of", asOfStr, mc.Loc)

	if err != nil {
		return nil, err
	}

	if asOf.After(now) {
		return nil, Invalid("reconcile as of today or an earlier date", "as_of %s is in the future", asOfStr)
	}

	at := txnReconcileAt(asOf, now, mc.Loc)
	prior := acct.GetLastReconciledTime()

	if prior > at {
		return nil, Invalid("reconcile as of "+DateOfUnix(prior, mc.Loc)+" or later; the reconciled time only moves forward", "the account was last reconciled on %s, after as_of %s", DateOfUnix(prior, mc.Loc), asOfStr)
	}

	start := int64(0)

	if prior > 0 {
		start = prior + 1
	}

	st, err := txnReadStatement(mc, acct.AccountId, start, at)

	if err != nil {
		return nil, err
	}

	closing, err := txnStatementAmount("closingBalance", st.ClosingBalance)

	if err != nil {
		return nil, err
	}

	// with no transactions at all upstream answers 0; a real balance still exists only when rows do
	diff := target - closing

	if diff > MaxSafeInteger || diff < -MaxSafeInteger {
		return nil, Invalid("check target_balance", "the difference is out of range")
	}

	integFillTags(mc, st.Transactions)

	for _, r := range st.Transactions {
		txnDecorate(r, lk, mc.Loc)
		integNameRow(r, lk)
	}

	if st.Transactions == nil {
		st.Transactions = []map[string]any{}
	}

	mark := args.MarkReconciled == nil || *args.MarkReconciled

	rs := &txnReconcileState{
		Account:    acct,
		AsOf:       asOf.Format("2006-01-02"),
		At:         at,
		AppBalance: closing,
		Target:     target,
		Difference: diff,
		Mark:       mark,
	}

	if prior > 0 {
		p := prior
		rs.PriorReconcile = &p
	}

	var adjustmentView any

	if args.CreateAdjustment {
		if diff == 0 {
			rs.Info = map[string]any{"adjustmentSkipped": "the balances agree; no adjustment is needed"}
		} else {
			ttype, magnitude := txnAdjustmentType(diff)
			cat, err := lk.resolveCategory(args.AdjustmentCategoryId, args.AdjustmentCategoryName, ttype)

			if err != nil {
				if f, ok := err.(*Fail); ok && f.Code == CodeInvalidInput && args.AdjustmentCategoryId == "" && args.AdjustmentCategoryName == "" {
					f.Hint = "pass adjustment_category_id (a " + txnCategoryTypeName(txnCategoryTypeFor(ttype)) + " sub-category): the difference is " + strconv.FormatInt(diff, 10) + ", so the adjustment is " + txnTypeName(int64(ttype))
				}

				return nil, err
			}

			if acct.Hidden {
				return nil, Invalid("unhide the account first", "account %q is hidden", acct.Name)
			}

			comment := "Reconciliation adjustment (as of " + rs.AsOf + ")"

			if args.AdjustmentComment != nil {
				comment = *args.AdjustmentComment
			}

			if len([]rune(comment)) > 255 {
				return nil, Invalid("shorten adjustment_comment to 255 characters", "adjustment_comment is too long")
			}

			adj := txnState{
				Type:            int(ttype),
				CategoryId:      idString(cat.CategoryId),
				Time:            at,
				UtcOffset:       UTCOffsetMinutes(time.Unix(at, 0), mc.Loc),
				SourceAccountId: idString(acct.AccountId),
				SourceAmount:    magnitude,
				Comment:         comment,
				TagIds:          []string{},
				PictureIds:      []string{},
			}
			rs.Adjustment = &adj
			adjustmentView = map[string]any{
				"typeName":     txnTypeName(int64(ttype)),
				"amount":       magnitude,
				"currency":     acct.Currency,
				"categoryId":   adj.CategoryId,
				"categoryName": lk.categoryPath(cat.CategoryId),
				"date":         DateOfUnix(at, mc.Loc),
				"time":         at,
				"comment":      comment,
			}
		}
	} else if diff != 0 {
		rs.Blocked = "the balances differ by " + strconv.FormatInt(diff, 10) + " " + acct.Currency + " hundredths; find the missing transaction first (the rows since the last reconciliation are listed), or pass create_adjustment: true with adjustment_category_id"
	}

	if mark {
		if mc.User == nil || !mc.User.UseLastReconciledTime {
			return nil, NewFail(CodeForbidden, "turn on \"use last reconciled time\" in the web UI's settings (or pass mark_reconciled: false)", "the bound user has last-reconciled-time switched off")
		}

		if prior == at {
			rs.Mark = false
			rs.Info = map[string]any{"markSkipped": "the account is already reconciled at that time"}
		}
	}

	lastTime, lastDate := txnLastReconciled(acct, mc.Loc)
	rs.Info = txnMergeInfo(rs.Info, map[string]any{
		"account":                         txnAccountInfo(acct),
		"asOf":                            rs.AsOf,
		"currency":                        acct.Currency,
		"appBalance":                      closing,
		"targetBalance":                   target,
		"difference":                      diff,
		"lastReconciledTime":              lastTime,
		"lastReconciledDate":              lastDate,
		"transactionsSinceLastReconciled": st.Transactions,
		"sinceCount":                      len(st.Transactions),
		"adjustment":                      adjustmentView,
		"markReconciled":                  rs.Mark,
		"markReconciledAt":                at,
		"markReconciledDate":              DateOfUnix(at, mc.Loc),
		"appliable":                       rs.Blocked == "",
		"balanceSignConvention":           "the account's own balance sign: a liability owed is negative",
	})

	if rs.Blocked != "" {
		rs.Info["blockedReason"] = rs.Blocked
	}

	return rs, nil
}

func txnMergeInfo(a, b map[string]any) map[string]any {
	if a == nil {
		return b
	}

	for k, v := range b {
		a[k] = v
	}

	return a
}

// txnReconcilePlan turns the resolved reconciliation into the write protocol's Plan
func txnReconcilePlan(rs *txnReconcileState) *Plan {
	count := 0
	changes := map[string]int{"create": 0, "update": 0}

	if rs.Adjustment != nil {
		count++
		changes["create"] = 1
	}

	if rs.Mark {
		count++
		changes["update"] = 1
	}

	fp := map[string]any{
		"account":    idString(rs.Account.AccountId),
		"at":         rs.At,
		"appBalance": rs.AppBalance,
		"target":     rs.Target,
		"adjustment": rs.Adjustment,
		"mark":       rs.Mark,
		"prior":      rs.PriorReconcile,
	}

	return &Plan{Changes: changes, Count: count, Preview: rs.Info, Fingerprinted: fp, State: rs}
}

// txnIssueTokenFor issues a confirm token bound to another route of this plane (the plan route
// hands out the apply route's token). It mirrors issueConfirmToken exactly.
func txnIssueTokenFor(mc *Ctx, routeKey, fingerprint string) (string, time.Time) {
	buf := make([]byte, 16)
	_, _ = io.ReadFull(rand.Reader, buf)
	token := "cf_" + hex.EncodeToString(buf)
	expires := time.Now().Add(ConfirmTTL)

	confirmStore.Lock()
	defer confirmStore.Unlock()

	now := time.Now()

	for k, v := range confirmStore.m {
		if now.After(v.expires) {
			delete(confirmStore.m, k)
		}
	}

	confirmStore.m[token] = confirmEntry{route: routeKey, uid: mc.Uid, fingerprint: fingerprint, expires: expires}

	return token, expires
}

func txnHandleReconcilePlan(mc *Ctx) (any, error) {
	var args txnReconcileArgs

	if err := mc.BindBody(&args); err != nil {
		return nil, err
	}

	rs, err := txnResolveReconcile(mc, args)

	if err != nil {
		return nil, err
	}

	p := txnReconcilePlan(rs)
	out := rs.Info
	out["dry_run"] = true
	out["changes"] = p.Changes

	if rs.Blocked == "" && p.Count > 0 {
		fingerprint := ChangeFingerprint(p.Fingerprinted)
		token, expires := txnIssueTokenFor(mc, txnReconcileApplyRoute, fingerprint)
		out["confirm_token"] = token
		out["expires_at"] = expires.UTC().Format(time.RFC3339)
		out["fingerprint"] = fingerprint
		out["applyWith"] = "POST /machine/v1/accounts/" + idString(rs.Account.AccountId) + "/reconcile/apply with the same arguments, dry_run: false and this confirm_token"
	}

	return out, nil
}

func txnHandleReconcileApply(mc *Ctx) (any, error) {
	var body struct {
		WriteOpts
		txnReconcileArgs
	}

	if err := mc.BindBody(&body); err != nil {
		return nil, err
	}

	resolve := func() (*Plan, error) {
		rs, err := txnResolveReconcile(mc, body.txnReconcileArgs)

		if err != nil {
			return nil, err
		}

		if rs.Blocked != "" {
			return nil, Invalid("find the missing transaction first (POST /accounts/:id/reconcile/plan lists the rows since the last reconciliation), or pass create_adjustment: true with adjustment_category_id", "%s", rs.Blocked).WithDetails(map[string]any{"appBalance": rs.AppBalance, "targetBalance": rs.Target, "difference": rs.Difference, "currency": rs.Account.Currency})
		}

		return txnReconcilePlan(rs), nil
	}

	apply := func(p *Plan) (any, error) {
		rs := p.State.(*txnReconcileState)
		out := map[string]any{"adjustmentId": nil, "reconciledAt": nil}
		var ops []InverseOp
		var adjustmentId int64

		if rs.Adjustment != nil {
			var resp map[string]any

			if err := mc.CallUpstreamInto(api.Transactions.TransactionCreateHandler, "POST", nil, txnCreateBodyFromState(*rs.Adjustment), &resp); err != nil {
				return nil, err
			}

			adjustmentId, _ = txnAsInt64(resp["id"])
			out["adjustmentId"] = idString(adjustmentId)

			states, _, _, err := txnReadStates(mc, []int64{adjustmentId})

			if err != nil {
				return nil, err
			}

			var check []txnState

			if s := states[adjustmentId]; s != nil {
				check = []txnState{*s}
			}

			ops = append(ops, NewInverseOp(txnOpDelete, map[string]any{"ids": []string{idString(adjustmentId)}}, check))
		}

		if rs.Mark {
			if _, err := mc.CallUpstream(api.Accounts.AccountUpdateLastReconciledTimeHandler, "POST", nil, map[string]any{"id": idString(rs.Account.AccountId), "lastReconciledTime": rs.At}); err != nil {
				// never leave a half-done reconciliation: remove the adjustment this call created
				if adjustmentId > 0 {
					_, _ = mc.CallUpstream(api.Transactions.TransactionDeleteHandler, "POST", nil, map[string]any{"id": idString(adjustmentId)})
				}

				return nil, err
			}

			at := rs.At
			op := NewInverseOp(txnOpReconciledTime, map[string]any{"accountId": idString(rs.Account.AccountId), "value": rs.PriorReconcile}, map[string]any{"value": &at})
			redo := NewInverseOp(txnOpReconciledTime, map[string]any{"accountId": idString(rs.Account.AccountId), "value": &at}, map[string]any{"value": rs.PriorReconcile})
			op.Redo = &redo
			ops = append(ops, op)
			out["reconciledAt"] = rs.At
			out["reconciledDate"] = DateOfUnix(rs.At, mc.Loc)
		}

		if _, err := RecordJournal(mc, fmt.Sprintf("reconciled an account (%d changes)", p.Count), p.Count, ops); err != nil {
			return nil, err
		}

		out["appBalance"] = rs.AppBalance
		out["targetBalance"] = rs.Target
		out["difference"] = rs.Difference
		out["currency"] = rs.Account.Currency

		return out, nil
	}

	return RunWrite(mc, body.WriteOpts, resolve, apply)
}

// txnExecReconciledTime restores an account's last reconciled time (nil clears it)
func txnExecReconciledTime(mc *Ctx, payload json.RawMessage, check json.RawMessage) error {
	var p struct {
		AccountId string `json:"accountId"`
		Value     *int64 `json:"value"`
	}

	if err := json.Unmarshal(payload, &p); err != nil {
		return err
	}

	accountId, err := strconv.ParseInt(p.AccountId, 10, 64)

	if err != nil {
		return Conflict("the journal entry is damaged; it cannot be undone", "bad account id %q in journal", p.AccountId)
	}

	account, err := services.Accounts.GetAccountByAccountId(mc.Web, mc.Uid, accountId)

	if err != nil {
		return err
	}

	var current *int64

	if account.Extend != nil {
		current = account.Extend.LastReconciledTime
	}

	if len(check) > 0 {
		var c struct {
			Value *int64 `json:"value"`
		}

		if err := json.Unmarshal(check, &c); err != nil {
			return err
		}

		if !txnInt64PtrEqual(current, c.Value) {
			return Conflict("the account was reconciled again since; undo will not overwrite a later reconciliation", "the last reconciled time of account %s changed since the write", p.AccountId)
		}
	}

	if txnInt64PtrEqual(current, p.Value) {
		return nil
	}

	if account.Extend == nil {
		account.Extend = &models.AccountExtend{}
	}

	account.Extend.LastReconciledTime = p.Value

	return services.Accounts.UpdateAccountExtend(mc.Web, mc.Uid, account)
}

func txnInt64PtrEqual(a, b *int64) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}

	return *a == *b
}

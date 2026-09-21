package machine

import (
	"net/url"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/mayswind/ezbookkeeping/pkg/api"
	"github.com/mayswind/ezbookkeeping/pkg/core"
	"github.com/mayswind/ezbookkeeping/pkg/errfile"
	"github.com/mayswind/ezbookkeeping/pkg/errs"
	"github.com/mayswind/ezbookkeeping/pkg/log"
	"github.com/mayswind/ezbookkeeping/pkg/models"
	"github.com/mayswind/ezbookkeeping/pkg/services"
)

// routes_admin.go — key rotation, clear data, upstream sessions, unused pictures (apis.mdx §10.10).
// Every route here is admin-tier: the admin switch must be on and the MCP client is refused at the
// gate. Every one that changes something speaks the write protocol (dry_run default true, confirm
// token, ceiling). None of them is journaled: a rotated key, cleared data, a revoked session and a
// removed upload have no inverse the plane can apply, and every preview says so.

func init() {
	registerRoutes(jrAdminRoutes)
}

func jrAdminRoutes() []RouteDef {
	return []RouteDef{
		{
			Method: "POST", Path: "/admin/key/rotate", Tier: TierAdmin, Handler: jrHandleKeyRotate,
			Summary: "Mint a new API secret key into the credentials file (dry_run default). Answers fingerprints only; the running server keeps the old key until it is restarted (ezbk stop && ezbk up).",
		},
		{
			Method: "POST", Path: "/admin/data/clear", Tier: TierAdmin, Handler: jrHandleDataClear,
			Summary:   "Clear data (dry_run default): scope all | transactions | transactions_of_account (with account_id). Not undoable. Body: {scope, account_id?, dry_run, confirm_token, max_changes}.",
			Untrusted: []string{"accountName"},
		},
		{
			Method: "GET", Path: "/admin/sessions", Tier: TierAdmin, Handler: jrHandleSessionsList,
			Summary: "The bound user's upstream sessions and API/MCP tokens: id, type, user agent, last seen. Never a token value.",
		},
		{
			Method: "DELETE", Path: "/admin/sessions/:token_id", Tier: TierAdmin, Handler: jrHandleSessionRevoke,
			Summary: "Revoke one upstream session or token (dry_run default). An id already gone answers ok with deleted: 0.",
		},
		{
			Method: "DELETE", Path: "/admin/pictures/unused", Tier: TierAdmin, Handler: jrHandlePicturesUnused, Feature: jrFeaturePictures,
			Summary: "Remove transaction pictures uploaded but never attached (dry_run default). Body: {ids?, older_than_days? (default 1), dry_run, confirm_token, max_changes}.",
		},
	}
}

// jrNotUndoable is the warning every admin preview carries
const jrNotUndoable = "this change is not journaled and cannot be undone with POST /undo"

// ---------------------------------------------------------------------------------------------
// POST /admin/key/rotate
// ---------------------------------------------------------------------------------------------

type jrKeyRotateRequest struct {
	WriteOpts
}

func jrHandleKeyRotate(mc *Ctx) (any, error) {
	var req jrKeyRotateRequest

	if err := mc.BindBody(&req); err != nil {
		return nil, err
	}

	st := currentState()
	credsPath, err := CredentialsPath()

	if err != nil {
		errfile.Caught("resolving the credentials file path", err)
		return nil, NewFail(CodeInternal, "set EZBK_CREDENTIALS_FILE, or make sure the server's home directory is readable", "the credentials file path cannot be resolved")
	}

	resolve := func() (*Plan, error) {
		current := "none"

		if creds, err := ReadCredentials(); err != nil {
			return nil, NewFail(CodeConflict, "fix the credentials file first (chmod 600, not a symlink, valid JSON), then rotate", "the credentials file cannot be read: %s", jrScrubPath(err.Error(), credsPath))
		} else if creds.APIKey != "" {
			current = Fingerprint(creds.APIKey)
		}

		running := ""
		source := ""

		if st != nil {
			running = st.fingerprint
			source = st.keySource
		}

		warnings := []string{
			"the running server keeps answering to the old key until it is restarted: ezbk stop && ezbk up",
			"every client reading the credentials file picks up the new key on its next call",
			jrNotUndoable,
		}

		if source == "EZBK_API_KEY" || source == "EZBK_API_KEY_FILE" {
			warnings = append(warnings, "this server's key comes from "+source+", which outranks the credentials file; unset it or the new key will not be used after the restart")
		}

		preview := map[string]any{
			"credentialsFile":        homeRelative(credsPath),
			"currentFileFingerprint": current,
			"runningFingerprint":     running,
			"runningKeySource":       jrSourceLabel(source, credsPath),
		}

		return &Plan{Changes: map[string]int{"rotate": 1}, Count: 1, Preview: preview, Warnings: warnings}, nil
	}

	apply := func(p *Plan) (any, error) {
		key, err := MintIntoCredentials("server", true)

		if err != nil {
			return nil, NewFail(CodeInternal, "fix the credentials file (chmod 600, not a symlink, valid JSON) and retry", "the new key could not be written: %s", jrScrubPath(err.Error(), credsPath))
		}

		log.Infof(mc.Web, "[machine.admin] API secret key rotated in the credentials file; new fingerprint %s; restart required", Fingerprint(key))

		return map[string]any{
			"rotated":            true,
			"newFingerprint":     Fingerprint(key),
			"runningFingerprint": p.Preview.(map[string]any)["runningFingerprint"],
			"credentialsFile":    homeRelative(credsPath),
			"restartRequired":    true,
			"hint":               "restart the server so it answers to the new key: ezbk stop && ezbk up",
		}, nil
	}

	return RunWrite(mc, req.WriteOpts, resolve, apply)
}

func jrSourceLabel(source, credsPath string) string {
	switch source {
	case "":
		return "unknown"
	case credsPath:
		return "credentials file"
	}

	return source
}

// jrScrubPath keeps error text from carrying absolute paths other than the credentials file's own
func jrScrubPath(msg, credsPath string) string {
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		msg = strings.ReplaceAll(msg, credsPath, "<credentials file>")
		msg = strings.ReplaceAll(msg, home, "~")
	}

	return msg
}

// ---------------------------------------------------------------------------------------------
// POST /admin/data/clear
// ---------------------------------------------------------------------------------------------

const (
	jrScopeAll                   = "all"
	jrScopeTransactions          = "transactions"
	jrScopeTransactionsOfAccount = "transactions_of_account"
)

type jrDataClearRequest struct {
	WriteOpts
	Scope     string `json:"scope"`
	AccountId string `json:"account_id,omitempty"`
}

// jrParseClearScope validates scope and account_id together
func jrParseClearScope(scope, accountId string) (string, int64, error) {
	scope = strings.ToLower(strings.TrimSpace(scope))

	switch scope {
	case jrScopeAll, jrScopeTransactions:
		if strings.TrimSpace(accountId) != "" {
			return "", 0, Invalid("account_id goes with scope transactions_of_account only", "account_id is not used with scope %s", scope)
		}

		return scope, 0, nil
	case jrScopeTransactionsOfAccount:
		id, err := ResolveId("account_id", accountId)

		if err != nil {
			return "", 0, err
		}

		return scope, id, nil
	case "":
		return "", 0, Invalid("pass scope: all | transactions | transactions_of_account", "scope is required")
	}

	return "", 0, Invalid("pass scope: all | transactions | transactions_of_account", "scope %q is not one of all, transactions, transactions_of_account", scope)
}

func jrHandleDataClear(mc *Ctx) (any, error) {
	var req jrDataClearRequest

	if err := mc.BindBody(&req); err != nil {
		return nil, err
	}

	scope, accountId, err := jrParseClearScope(req.Scope, req.AccountId)

	if err != nil {
		return nil, err
	}

	if mc.User != nil && mc.User.FeatureRestriction.Contains(core.USER_FEATURE_RESTRICTION_TYPE_CLEAR_ALL_DATA) {
		return nil, NewFail(CodeForbidden, "lift the bound user's clear-data restriction first (ezbookkeeping userdata …), or clear it in the web UI", "clearing data is restricted for the bound user")
	}

	resolve := func() (*Plan, error) {
		switch scope {
		case jrScopeTransactionsOfAccount:
			return jrPlanClearAccount(mc, accountId)
		default:
			return jrPlanClearUser(mc, scope)
		}
	}

	apply := func(p *Plan) (any, error) {
		var err error

		switch scope {
		case jrScopeAll:
			err = jrClearAll(mc)
		case jrScopeTransactions:
			err = services.Transactions.DeleteAllTransactions(mc.Web, mc.Uid, false)
		case jrScopeTransactionsOfAccount:
			err = services.Transactions.DeleteAllTransactionsOfAccount(mc.Web, mc.Uid, accountId, 1000)
		}

		if err != nil {
			return nil, jrServiceErr(err)
		}

		log.Infof(mc.Web, "[machine.admin] user \"uid:%d\" cleared data (scope %s) through the machine plane", mc.Uid, scope)

		return map[string]any{"cleared": true, "scope": scope, "counts": p.Preview.(map[string]any)["counts"]}, nil
	}

	return RunWrite(mc, req.WriteOpts, resolve, apply)
}

func jrServiceErr(err error) error {
	if err == nil {
		return nil
	}

	if ue, ok := err.(*errs.Error); ok {
		return Upstream(ue)
	}

	return err
}

func jrPlanClearUser(mc *Ctx, scope string) (*Plan, error) {
	var stats models.DataStatisticsResponse

	if err := mc.CallUpstreamInto(api.DataManagements.DataStatisticsHandler, "GET", nil, nil, &stats); err != nil {
		return nil, err
	}

	counts := map[string]int64{"transactions": stats.TotalTransactionCount}
	total := stats.TotalTransactionCount

	if scope == jrScopeAll {
		counts["accounts"] = stats.TotalAccountCount
		counts["categories"] = stats.TotalTransactionCategoryCount
		counts["tags"] = stats.TotalTransactionTagCount
		counts["templates"] = stats.TotalTransactionTemplateCount
		counts["scheduledTransactions"] = stats.TotalScheduledTransactionCount
		counts["pictures"] = stats.TotalTransactionPictureCount
		counts["explorations"] = stats.TotalExplorationCount
		counts["customIcons"] = stats.TotalCustomIconCount
		total = 0

		for _, v := range counts {
			total += v
		}
	}

	changes := map[string]int{}

	for k, v := range counts {
		changes["delete_"+k] = int(v)
	}

	warnings := []string{jrNotUndoable, "export first if you may want this back: GET /machine/v1/data/export?format=csv"}

	if scope == jrScopeAll {
		warnings = append(warnings, "scope all also removes accounts, categories, tags, tag groups, templates, scheduled transactions, custom icons, custom exchange rates and saved insights; the user and its settings stay")
	}

	return &Plan{
		Changes:  changes,
		Count:    int(total),
		Preview:  map[string]any{"scope": scope, "counts": counts},
		Warnings: warnings,
	}, nil
}

func jrPlanClearAccount(mc *Ctx, accountId int64) (*Plan, error) {
	account, err := services.Accounts.GetAccountByAccountId(mc.Web, mc.Uid, accountId)

	if err != nil {
		if errs.IsCustomError(err) {
			return nil, NotFound("GET /machine/v1/accounts lists the account ids", "no account %s", idString(accountId)).WithDetails(map[string]any{"id": idString(accountId)})
		}

		return nil, err
	}

	if account.Type == models.ACCOUNT_TYPE_MULTI_SUB_ACCOUNTS {
		subs, _ := services.Accounts.GetSubAccountsByAccountId(mc.Web, mc.Uid, accountId)
		ids := make([]string, 0, len(subs))

		for _, s := range subs {
			ids = append(ids, idString(s.AccountId))
		}

		return nil, Invalid("clear each sub-account instead (account_id of one of details.subAccountIds)", "account %s is a parent account; its transactions live in its sub-accounts", idString(accountId)).WithDetails(map[string]any{"subAccountIds": ids})
	}

	if account.Hidden {
		return nil, Invalid("unhide the account first (PATCH /machine/v1/accounts/:id with hidden: false)", "transactions cannot be cleared from a hidden account")
	}

	n, err := services.Transactions.GetTransactionCount(mc.Web, mc.Uid, 0, 0, 0, nil, []int64{accountId}, nil, false, "", "", core.MATCH_MODE_DEFAULT, false)

	if err != nil {
		return nil, jrServiceErr(err)
	}

	return &Plan{
		Changes: map[string]int{"delete_transactions": int(n)},
		Count:   int(n),
		Preview: map[string]any{
			"scope":       jrScopeTransactionsOfAccount,
			"accountId":   idString(accountId),
			"accountName": account.Name,
			"currency":    account.Currency,
			"counts":      map[string]int64{"transactions": n},
		},
		Warnings: []string{jrNotUndoable, "a transfer to or from another account is removed from both sides", "export first if you may want this back: GET /machine/v1/data/export?format=csv"},
	}, nil
}

// jrClearAll mirrors upstream's ClearAllDataHandler step for step, minus the password prompt: on
// this plane the admin tier plus the confirm token stand in for it
func jrClearAll(mc *Ctx) error {
	steps := []struct {
		name string
		fn   func() error
	}{
		{"templates", func() error { return services.TransactionTemplates.DeleteAllTemplates(mc.Web, mc.Uid) }},
		{"transactions", func() error { return services.Transactions.DeleteAllTransactions(mc.Web, mc.Uid, true) }},
		{"categories", func() error { return services.TransactionCategories.DeleteAllCategories(mc.Web, mc.Uid) }},
		{"tags", func() error { return services.TransactionTags.DeleteAllTags(mc.Web, mc.Uid) }},
		{"tag groups", func() error { return services.TransactionTagGroups.DeleteAllTagGroups(mc.Web, mc.Uid) }},
		{"custom icons", func() error { return services.UserCustomIcons.DeleteAllCustomIcons(mc.Web, mc.Uid) }},
		{"custom exchange rates", func() error { return services.UserCustomExchangeRates.DeleteAllCustomExchangeRates(mc.Web, mc.Uid) }},
		{"explorations", func() error { return services.InsightsExplorers.DeleteAllExplorations(mc.Web, mc.Uid) }},
	}

	for i, s := range steps {
		if err := s.fn(); err != nil {
			log.Errorf(mc.Web, "[machine.admin] clear all: deleting %s failed: %s", s.name, err.Error())
			done := make([]string, 0, i)

			for _, d := range steps[:i] {
				done = append(done, d.name)
			}

			f := toFail(jrServiceErr(err))

			return jrWithDetails(f, map[string]any{"failedStep": s.name, "alreadyCleared": done})
		}
	}

	return nil
}

// ---------------------------------------------------------------------------------------------
// GET /admin/sessions · DELETE /admin/sessions/:token_id
// ---------------------------------------------------------------------------------------------

// jrSession is the view of one upstream token record — never the token value
type jrSession struct {
	TokenId    string `json:"tokenId"`
	TokenType  int    `json:"tokenType"`
	Kind       string `json:"kind"`
	UserAgent  string `json:"userAgent"`
	LastSeen   int64  `json:"lastSeen"`
	LastSeenAt string `json:"lastSeenAt,omitempty"`
}

func jrTokenKind(t int) string {
	switch core.TokenType(t) {
	case core.USER_TOKEN_TYPE_NORMAL:
		return "session"
	case core.USER_TOKEN_TYPE_MCP:
		return "mcp_token"
	case core.USER_TOKEN_TYPE_API:
		return "api_token"
	case core.USER_TOKEN_TYPE_REQUIRE_2FA:
		return "pending_2fa"
	}

	return "other"
}

func jrListSessions(mc *Ctx) ([]jrSession, error) {
	var raw []struct {
		TokenId   string `json:"tokenId"`
		TokenType int    `json:"tokenType"`
		UserAgent string `json:"userAgent"`
		LastSeen  int64  `json:"lastSeen"`
	}

	if err := mc.CallUpstreamInto(api.Tokens.TokenListHandler, "GET", nil, nil, &raw); err != nil {
		return nil, err
	}

	out := make([]jrSession, 0, len(raw))

	for _, r := range raw {
		s := jrSession{TokenId: r.TokenId, TokenType: r.TokenType, Kind: jrTokenKind(r.TokenType), UserAgent: r.UserAgent, LastSeen: r.LastSeen}

		if r.LastSeen > 0 {
			s.LastSeenAt = time.Unix(r.LastSeen, 0).UTC().Format(time.RFC3339)
		}

		out = append(out, s)
	}

	sort.SliceStable(out, func(i, j int) bool {
		if out[i].LastSeen != out[j].LastSeen {
			return out[i].LastSeen > out[j].LastSeen
		}

		return out[i].TokenId < out[j].TokenId
	})

	return out, nil
}

func jrHandleSessionsList(mc *Ctx) (any, error) {
	sessions, err := jrListSessions(mc)

	if err != nil {
		return nil, err
	}

	return map[string]any{
		"sessions": sessions,
		"count":    len(sessions),
		"note":     "the machine plane itself holds no upstream session; token values are never shown",
	}, nil
}

type jrSessionRevokeRequest struct {
	WriteOpts
}

func jrHandleSessionRevoke(mc *Ctx) (any, error) {
	var req jrSessionRevokeRequest

	if err := mc.BindBody(&req); err != nil {
		return nil, err
	}

	tokenId, err := url.PathUnescape(strings.TrimSpace(mc.Param("token_id")))

	if err != nil || tokenId == "" {
		errfile.Expected("unescaping the token_id path parameter", err)
		return nil, Invalid("pass the tokenId GET /machine/v1/admin/sessions returns", "token_id is missing or malformed")
	}

	resolve := func() (*Plan, error) {
		sessions, err := jrListSessions(mc)

		if err != nil {
			return nil, err
		}

		for _, s := range sessions {
			if s.TokenId == tokenId {
				return &Plan{
					Changes:  map[string]int{"revoke": 1},
					Count:    1,
					Preview:  map[string]any{"session": s},
					Warnings: []string{"the client holding this session or token is signed out at its next call", jrNotUndoable},
					State:    true,
				}, nil
			}
		}

		return &Plan{Changes: map[string]int{"revoke": 0}, Count: 0, Preview: map[string]any{"session": nil, "tokenId": tokenId}, State: false}, nil
	}

	apply := func(p *Plan) (any, error) {
		if found, _ := p.State.(bool); !found {
			return map[string]any{"deleted": 0, "tokenId": tokenId}, nil
		}

		if _, err := mc.CallUpstream(api.Tokens.TokenRevokeHandler, "POST", nil, map[string]string{"tokenId": tokenId}); err != nil {
			return nil, err
		}

		return map[string]any{"deleted": 1, "tokenId": tokenId}, nil
	}

	return RunWrite(mc, req.WriteOpts, resolve, apply)
}

// ---------------------------------------------------------------------------------------------
// DELETE /admin/pictures/unused
// ---------------------------------------------------------------------------------------------

type jrPicturesUnusedRequest struct {
	WriteOpts
	Ids           []string `json:"ids,omitempty"`
	OlderThanDays *int     `json:"older_than_days,omitempty"`
}

// jrUnusedPicture is one upload never attached to a transaction
type jrUnusedPicture struct {
	PictureId string `json:"pictureId"`
	Extension string `json:"extension"`
	CreatedAt string `json:"createdAt"`
}

func jrHandlePicturesUnused(mc *Ctx) (any, error) {
	var req jrPicturesUnusedRequest

	if err := mc.BindBody(&req); err != nil {
		return nil, err
	}

	olderThanDays := 1

	if req.OlderThanDays != nil {
		if *req.OlderThanDays < 0 || *req.OlderThanDays > 36500 {
			return nil, Invalid("older_than_days is 0 or more (0 includes uploads from the last day, which a browser may still be attaching)", "older_than_days %d is out of range", *req.OlderThanDays)
		}

		olderThanDays = *req.OlderThanDays
	}

	wanted := map[int64]bool{}

	for _, s := range req.Ids {
		id, err := ResolveId("ids[]", s)

		if err != nil {
			return nil, err
		}

		wanted[id] = true
	}

	resolve := func() (*Plan, error) {
		db, err := jrDB(mc)

		if err != nil {
			return nil, err
		}

		cutoff := time.Now().Add(-time.Duration(olderThanDays) * 24 * time.Hour).Unix()
		var rows []*models.TransactionPictureInfo

		sess := db.NewSession(mc.Web).Where("uid=? AND deleted=? AND transaction_id=?", mc.Uid, false, models.TransactionPictureNewPictureTransactionId)

		if olderThanDays > 0 {
			sess = sess.And("created_unix_time<=?", cutoff)
		}

		if err := sess.OrderBy("picture_id asc").Find(&rows); err != nil {
			return nil, err
		}

		pictures := make([]jrUnusedPicture, 0, len(rows))
		found := map[int64]bool{}

		for _, r := range rows {
			if len(wanted) > 0 && !wanted[r.PictureId] {
				continue
			}

			found[r.PictureId] = true
			pictures = append(pictures, jrUnusedPicture{PictureId: idString(r.PictureId), Extension: r.PictureExtension, CreatedAt: time.Unix(r.CreatedUnixTime, 0).UTC().Format(time.RFC3339)})
		}

		warnings := []string{jrNotUndoable}
		var missing []string

		for id := range wanted {
			if !found[id] {
				missing = append(missing, idString(id))
			}
		}

		sort.Strings(missing)

		if len(missing) > 0 {
			warnings = append(warnings, "skipped ids that are not unused uploads of the bound user (attached, already removed, too recent or unknown): "+strings.Join(missing, ", "))
		}

		return &Plan{
			Changes:  map[string]int{"delete": len(pictures), "skipped": len(missing)},
			Count:    len(pictures),
			Preview:  map[string]any{"pictures": pictures, "olderThanDays": olderThanDays},
			Warnings: warnings,
			State:    pictures,
		}, nil
	}

	apply := func(p *Plan) (any, error) {
		pictures := p.State.([]jrUnusedPicture)
		deleted := 0
		var gone []string

		for _, pic := range pictures {
			_, err := mc.CallUpstream(api.TransactionPictures.TransactionPictureRemoveUnusedHandler, "POST", nil, map[string]string{"id": pic.PictureId})

			if err != nil {
				if f := toFail(err); f.Code == CodeNotFound {
					gone = append(gone, pic.PictureId)
					continue
				}

				return nil, jrWithDetails(toFail(err), map[string]any{"deletedBeforeFailure": deleted, "failedPictureId": pic.PictureId})
			}

			deleted++
		}

		out := map[string]any{"deleted": deleted}

		if len(gone) > 0 {
			out["alreadyGone"] = gone
		}

		return out, nil
	}

	return RunWrite(mc, req.WriteOpts, resolve, apply)
}

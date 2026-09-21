package machine

import (
	"bytes"
	"encoding/json"
	"strings"
	"sync"

	"github.com/mayswind/ezbookkeeping/pkg/api"
	"github.com/mayswind/ezbookkeeping/pkg/core"
	"github.com/mayswind/ezbookkeeping/pkg/settings"
)

// routes_passthrough.go — /machine/v1/api/*path forwards to the upstream /api/v1 handler of the
// same path, as the bound user, behind the same gates (apis.mdx §16).
//
// It is the escape hatch, not the write protocol: no dry run, no confirm token, no ceiling, no
// journal. `data` is upstream's `result` verbatim (big-integer strings and all — the one place R2's
// conversion does not apply) and meta.passthrough is true. The MCP never calls it.

func init() {
	registerRoutes(jrPassthroughRoutes)
}

func jrPassthroughRoutes() []RouteDef {
	untrusted := []string{"name", "comment", "description", "note", "tags"}

	return []RouteDef{
		{
			Method: "GET", Path: "/api/*path", Tier: TierRead, Handler: jrHandlePassthrough, NoIntegerize: true,
			Summary:   "Passthrough: any upstream GET /api/v1/<path> as the bound user; data is upstream's result verbatim (amounts stay upstream's strings). tokens/*, users/2fa/*, users/external_auth/*, users/avatar/*, users/verify_email/*, llm/* are refused.",
			Untrusted: untrusted,
			Features:  []string{"passthrough"},
		},
		{
			Method: "POST", Path: "/api/*path", Tier: TierWrite, Handler: jrHandlePassthrough, NoIntegerize: true,
			Summary:   "Passthrough: any upstream POST /api/v1/<path> as the bound user, with NO dry run, confirm token, ceiling or journal. */delete.json, */batch_delete.json, data/clear/*, */remove_unused.json need the admin tier; tokens/*, users/2fa/*, users/external_auth/*, users/avatar/*, users/verify_email/*, llm/* are refused.",
			Untrusted: untrusted,
			Features:  []string{"passthrough"},
		},
	}
}

// Passthrough classes (apis.mdx §16)
const (
	jrClassRead    = "read"
	jrClassWrite   = "write"
	jrClassAdmin   = "admin"
	jrClassRefused = "refused"
)

// jrRefusedPrefixes are the upstream path classes the passthrough never reaches: credentials and
// identity (tokens, 2FA, external auth, avatar) and egress (the LLM recognisers send the operator's
// data to a third party; verify_email sends mail — §19.2 no egress)
var jrRefusedPrefixes = []string{"/tokens/", "/users/2fa/", "/users/external_auth/", "/users/avatar/", "/users/verify_email/", "/llm/"}

// jrPassthroughClass classifies an upstream path relative to /api/v1 (e.g. "/accounts/list.json")
func jrPassthroughClass(method, path string) string {
	for _, p := range jrRefusedPrefixes {
		if strings.HasPrefix(path, p) {
			return jrClassRefused
		}
	}

	if method == "GET" {
		return jrClassRead
	}

	if strings.HasSuffix(path, "/delete.json") || strings.HasSuffix(path, "/batch_delete.json") ||
		strings.HasPrefix(path, "/data/clear/") || strings.HasSuffix(path, "/remove_unused.json") {
		return jrClassAdmin
	}

	return jrClassWrite
}

// jrUpstreamEntry is one upstream /api/v1 registration mirrored from cmd/webserver.go
type jrUpstreamEntry struct {
	api     core.ApiHandlerFunc
	data    core.DataHandlerFunc
	ctype   string
	feature FeatureCheck
}

var (
	jrFeaturePictures = func(c *settings.Config) (bool, string) {
		return c.EnableTransactionPictures, "[user] enable_transaction_picture"
	}
	jrFeatureCustomIcons = func(c *settings.Config) (bool, string) {
		return c.EnableUserCustomIcon, "[user] enable_custom_icon"
	}
)

// jrUpstreamTable mirrors cmd/webserver.go's apiV1Route registrations (the refused classes are
// left out on purpose — they are refused before the table is consulted). Keep it in step with
// upstream: a path missing here answers not_found, never a wrong handler.
func jrUpstreamTable() map[string]jrUpstreamEntry {
	a := func(fn core.ApiHandlerFunc) jrUpstreamEntry { return jrUpstreamEntry{api: fn} }
	af := func(fn core.ApiHandlerFunc, f FeatureCheck) jrUpstreamEntry {
		return jrUpstreamEntry{api: fn, feature: f}
	}

	return map[string]jrUpstreamEntry{
		// users
		"GET /users/profile/get.json":             a(api.Users.UserProfileHandler),
		"POST /users/profile/update.json":         a(api.Users.UserUpdateProfileHandler),
		"GET /users/settings/cloud/get.json":      a(api.UserApplicationCloudSettings.ApplicationSettingsGetHandler),
		"POST /users/settings/cloud/update.json":  a(api.UserApplicationCloudSettings.ApplicationSettingsUpdateHandler),
		"POST /users/settings/cloud/disable.json": a(api.UserApplicationCloudSettings.ApplicationSettingsDisableHandler),

		// data
		"GET /data/statistics.json":                     a(api.DataManagements.DataStatisticsHandler),
		"POST /data/clear/all.json":                     a(api.DataManagements.ClearAllDataHandler),
		"POST /data/clear/transactions.json":            a(api.DataManagements.ClearAllTransactionsHandler),
		"POST /data/clear/transactions/by_account.json": a(api.DataManagements.ClearAllTransactionsByAccountHandler),
		"GET /data/export.csv":                          {data: api.DataManagements.ExportDataToEzbookkeepingCSVHandler, ctype: "text/csv; charset=utf-8", feature: featureExport},
		"GET /data/export.tsv":                          {data: api.DataManagements.ExportDataToEzbookkeepingTSVHandler, ctype: "text/tab-separated-values; charset=utf-8", feature: featureExport},

		// accounts
		"GET /accounts/list.json":                         a(api.Accounts.AccountListHandler),
		"GET /accounts/get.json":                          a(api.Accounts.AccountGetHandler),
		"POST /accounts/add.json":                         a(api.Accounts.AccountCreateHandler),
		"POST /accounts/modify.json":                      a(api.Accounts.AccountModifyHandler),
		"POST /accounts/update/last_reconciled_time.json": a(api.Accounts.AccountUpdateLastReconciledTimeHandler),
		"POST /accounts/hide.json":                        a(api.Accounts.AccountHideHandler),
		"POST /accounts/move.json":                        a(api.Accounts.AccountMoveHandler),
		"POST /accounts/delete.json":                      a(api.Accounts.AccountDeleteHandler),
		"POST /accounts/sub_account/delete.json":          a(api.Accounts.SubAccountDeleteHandler),

		// transactions
		"GET /transactions/count.json":                     a(api.Transactions.TransactionCountHandler),
		"GET /transactions/list.json":                      a(api.Transactions.TransactionListHandler),
		"GET /transactions/list/by_month.json":             a(api.Transactions.TransactionMonthListHandler),
		"GET /transactions/list/all.json":                  a(api.Transactions.TransactionListAllHandler),
		"GET /transactions/reconciliation_statements.json": a(api.Transactions.TransactionReconciliationStatementHandler),
		"GET /transactions/statistics.json":                a(api.Transactions.TransactionStatisticsHandler),
		"GET /transactions/statistics/trends.json":         a(api.Transactions.TransactionStatisticsTrendsHandler),
		"GET /transactions/statistics/asset_trends.json":   a(api.Transactions.TransactionStatisticsAssetTrendsHandler),
		"GET /transactions/amounts.json":                   a(api.Transactions.TransactionAmountsHandler),
		"GET /transactions/amounts/daily.json":             a(api.Transactions.TransactionDailyAmountsHandler),
		"GET /transactions/get.json":                       a(api.Transactions.TransactionGetHandler),
		"POST /transactions/add.json":                      a(api.Transactions.TransactionCreateHandler),
		"POST /transactions/modify.json":                   a(api.Transactions.TransactionModifyHandler),
		"POST /transactions/batch_update/category.json":    a(api.Transactions.TransactionBatchUpdateCategoriesHandler),
		"POST /transactions/batch_update/account.json":     a(api.Transactions.TransactionBatchUpdateAccountsHandler),
		"POST /transactions/batch_update/tag/add.json":     a(api.Transactions.TransactionBatchAddTagsHandler),
		"POST /transactions/batch_update/tag/remove.json":  a(api.Transactions.TransactionBatchRemoveTagsHandler),
		"POST /transactions/batch_update/tag/clear.json":   a(api.Transactions.TransactionBatchClearTagsHandler),
		"POST /transactions/move/all.json":                 a(api.Transactions.TransactionMoveAllBetweenAccountsHandler),
		"POST /transactions/delete.json":                   a(api.Transactions.TransactionDeleteHandler),
		"POST /transactions/batch_delete.json":             a(api.Transactions.TransactionBatchDeleteHandler),
		"POST /transactions/parse_custom_file.json":        af(api.Transactions.TransactionParseImportCustomFileDataHandler, featureImport),
		"POST /transactions/parse_import.json":             af(api.Transactions.TransactionParseImportFileHandler, featureImport),
		"POST /transactions/import.json":                   af(api.Transactions.TransactionImportHandler, featureImport),
		"GET /transactions/import/process.json":            af(api.Transactions.TransactionImportProcessHandler, featureImport),

		// transaction pictures
		"POST /transaction/pictures/upload.json":        af(api.TransactionPictures.TransactionPictureUploadHandler, jrFeaturePictures),
		"POST /transaction/pictures/remove_unused.json": af(api.TransactionPictures.TransactionPictureRemoveUnusedHandler, jrFeaturePictures),

		// categories
		"GET /transaction/categories/list.json":       a(api.TransactionCategories.CategoryListHandler),
		"GET /transaction/categories/get.json":        a(api.TransactionCategories.CategoryGetHandler),
		"POST /transaction/categories/add.json":       a(api.TransactionCategories.CategoryCreateHandler),
		"POST /transaction/categories/add_batch.json": a(api.TransactionCategories.CategoryCreateBatchHandler),
		"POST /transaction/categories/modify.json":    a(api.TransactionCategories.CategoryModifyHandler),
		"POST /transaction/categories/hide.json":      a(api.TransactionCategories.CategoryHideHandler),
		"POST /transaction/categories/move.json":      a(api.TransactionCategories.CategoryMoveHandler),
		"POST /transaction/categories/delete.json":    a(api.TransactionCategories.CategoryDeleteHandler),

		// tag groups
		"GET /transaction/tags/groups/list.json":    a(api.TransactionTagGroups.TagGroupListHandler),
		"GET /transaction/tags/groups/get.json":     a(api.TransactionTagGroups.TagGroupGetHandler),
		"POST /transaction/tags/groups/add.json":    a(api.TransactionTagGroups.TagGroupCreateHandler),
		"POST /transaction/tags/groups/modify.json": a(api.TransactionTagGroups.TagGroupModifyHandler),
		"POST /transaction/tags/groups/move.json":   a(api.TransactionTagGroups.TagGroupMoveHandler),
		"POST /transaction/tags/groups/delete.json": a(api.TransactionTagGroups.TagGroupDeleteHandler),

		// tags
		"GET /transaction/tags/list.json":       a(api.TransactionTags.TagListHandler),
		"GET /transaction/tags/get.json":        a(api.TransactionTags.TagGetHandler),
		"POST /transaction/tags/add.json":       a(api.TransactionTags.TagCreateHandler),
		"POST /transaction/tags/add_batch.json": a(api.TransactionTags.TagCreateBatchHandler),
		"POST /transaction/tags/modify.json":    a(api.TransactionTags.TagModifyHandler),
		"POST /transaction/tags/hide.json":      a(api.TransactionTags.TagHideHandler),
		"POST /transaction/tags/move.json":      a(api.TransactionTags.TagMoveHandler),
		"POST /transaction/tags/delete.json":    a(api.TransactionTags.TagDeleteHandler),

		// templates (and scheduled transactions)
		"GET /transaction/templates/list.json":    a(api.TransactionTemplates.TemplateListHandler),
		"GET /transaction/templates/get.json":     a(api.TransactionTemplates.TemplateGetHandler),
		"POST /transaction/templates/add.json":    a(api.TransactionTemplates.TemplateCreateHandler),
		"POST /transaction/templates/modify.json": a(api.TransactionTemplates.TemplateModifyHandler),
		"POST /transaction/templates/hide.json":   a(api.TransactionTemplates.TemplateHideHandler),
		"POST /transaction/templates/move.json":   a(api.TransactionTemplates.TemplateMoveHandler),
		"POST /transaction/templates/delete.json": a(api.TransactionTemplates.TemplateDeleteHandler),

		// insights explorers
		"GET /insights/explorers/list.json":    a(api.InsightsExplorers.InsightsExplorerListHandler),
		"GET /insights/explorers/get.json":     a(api.InsightsExplorers.InsightsExplorerGetHandler),
		"POST /insights/explorers/add.json":    a(api.InsightsExplorers.InsightsExplorerCreateHandler),
		"POST /insights/explorers/modify.json": a(api.InsightsExplorers.InsightsExplorerModifyHandler),
		"POST /insights/explorers/hide.json":   a(api.InsightsExplorers.InsightsExplorerHideHandler),
		"POST /insights/explorers/move.json":   a(api.InsightsExplorers.InsightsExplorerMoveHandler),
		"POST /insights/explorers/delete.json": a(api.InsightsExplorers.InsightsExplorerDeleteHandler),

		// custom icons
		"GET /custom_icons/list.json":    af(api.UserCustomIcons.CustomIconListHandler, jrFeatureCustomIcons),
		"POST /custom_icons/upload.json": af(api.UserCustomIcons.CustomIconUploadHandler, jrFeatureCustomIcons),
		"POST /custom_icons/move.json":   af(api.UserCustomIcons.CustomIconMoveHandler, jrFeatureCustomIcons),
		"POST /custom_icons/delete.json": af(api.UserCustomIcons.CustomIconDeleteHandler, jrFeatureCustomIcons),

		// exchange rates
		"GET /exchange_rates/latest.json":              a(api.ExchangeRates.LatestExchangeRateHandler),
		"POST /exchange_rates/user_custom/update.json": a(api.ExchangeRates.UserCustomExchangeRateUpdateHandler),
		"POST /exchange_rates/user_custom/delete.json": a(api.ExchangeRates.UserCustomExchangeRateDeleteHandler),

		// systems
		"GET /systems/version.json": a(api.Systems.VersionHandler),
	}
}

var (
	jrUpstream     map[string]jrUpstreamEntry
	jrUpstreamOnce sync.Once
)

func jrLookupUpstream(method, path string) (jrUpstreamEntry, bool) {
	jrUpstreamOnce.Do(func() { jrUpstream = jrUpstreamTable() })

	e, ok := jrUpstream[method+" "+path]

	return e, ok
}

// jrPassthroughMethodsFor lists the methods upstream serves a path with (for the wrong-method hint)
func jrPassthroughMethodsFor(path string) []string {
	var out []string

	for _, m := range []string{"GET", "POST"} {
		if _, ok := jrLookupUpstream(m, path); ok {
			out = append(out, m)
		}
	}

	return out
}

// jrForbiddenProfileFields are identity fields the passthrough will not change: a password change
// mints a new upstream JWT into the response, and an email change sends mail (§11.1, §19.2–19.3)
var jrForbiddenProfileFields = []string{"password", "oldPassword", "email"}

func jrCleanPassthroughPath(raw string) (string, error) {
	path := "/" + strings.TrimLeft(raw, "/")

	if strings.Contains(path, "..") || strings.Contains(path, "//") || strings.ContainsAny(path, "\\?#") {
		return "", NotFound("the path is the upstream /api/v1 path, e.g. /machine/v1/api/accounts/list.json", "no passthrough path %s", path)
	}

	if strings.HasPrefix(path, "/v1/") {
		path = strings.TrimPrefix(path, "/v1")
	}

	return path, nil
}

func jrHandlePassthrough(mc *Ctx) (any, error) {
	method := mc.Gin.Request.Method
	mc.SetMeta("passthrough", true)

	if strings.HasPrefix(strings.ToLower(mc.Client), "ezbookkeeping-mcp") {
		return nil, NewFail(CodeForbidden, "use the typed routes (GET /machine/v1/capabilities); the passthrough bypasses the write protocol and is for the CLI's `ezbk raw` only", "the MCP server never calls the passthrough")
	}

	path, err := jrCleanPassthroughPath(mc.Param("path"))

	if err != nil {
		return nil, err
	}

	class := jrPassthroughClass(method, path)

	if class == jrClassRefused {
		return nil, NotFound("credentials, identity and third-party (LLM, mail) paths are not reachable through the machine plane (apis.mdx §16); use the web UI", "no passthrough path %s %s", method, path)
	}

	entry, ok := jrLookupUpstream(method, path)

	if !ok {
		if methods := jrPassthroughMethodsFor(path); len(methods) > 0 {
			return nil, Invalid("upstream serves this path with "+strings.Join(methods, " or "), "%s is not served for %s", path, method)
		}

		return nil, NotFound("passthrough paths are upstream's /api/v1 paths, e.g. GET /machine/v1/api/accounts/list.json", "upstream has no /api/v1%s", path)
	}

	if class == jrClassAdmin {
		st := currentState()

		if st == nil || !st.allowAdmin {
			return nil, NewFail(CodeForbidden, "restart the app with admin allowed: ezbk stop && ezbk up --allow-write --allow-admin (sets EZBK_MACHINE_ALLOW_ADMIN=1)", "this passthrough path deletes data and needs the admin tier, which is off on this server")
		}
	}

	if entry.feature != nil {
		if enabled, key := entry.feature(mc.Config); !enabled {
			return nil, NewFail(CodeForbidden, "switch on "+key+" in conf/ezbookkeeping.ini and restart", "this feature is switched off upstream (%s)", key)
		}
	}

	query := mc.Gin.Request.URL.Query()

	if entry.data != nil {
		data, fileName, err := mc.CallUpstreamData(entry.data, query)

		if err != nil {
			return nil, err
		}

		return &RawResult{ContentType: entry.ctype, FileName: fileName, Data: data}, nil
	}

	var body []byte
	contentType := ""

	if method == "POST" {
		body, err = mc.RawBody()

		if err != nil {
			return nil, err
		}

		contentType = mc.Gin.GetHeader("Content-Type")

		if len(bytes.TrimSpace(body)) == 0 {
			body = []byte("{}")
		}

		if contentType == "" {
			contentType = "application/json"
		}

		if path == "/users/profile/update.json" {
			if err := jrCheckProfileBody(body); err != nil {
				return nil, err
			}
		}
	}

	var reader *bytes.Reader

	if body != nil {
		reader = bytes.NewReader(body)
	}

	var web *core.WebContext

	if reader != nil {
		web, err = mc.newUpstreamContext(method, query, reader, contentType)
	} else {
		web, err = mc.newUpstreamContext(method, query, nil, "")
	}

	if err != nil {
		return nil, err
	}

	result, uerr := entry.api(web)

	if uerr != nil {
		return nil, Upstream(uerr)
	}

	if method == "POST" {
		auditWrite(mc, 1, true)
	}

	return result, nil
}

func jrCheckProfileBody(body []byte) error {
	var fields map[string]json.RawMessage

	if err := json.Unmarshal(body, &fields); err != nil {
		return Invalid("send a JSON object", "the profile update body is not a JSON object")
	}

	for _, k := range jrForbiddenProfileFields {
		if v, ok := fields[k]; ok && strings.TrimSpace(string(v)) != `""` && strings.TrimSpace(string(v)) != "null" {
			return NewFail(CodeForbidden, "change the password or email in the web UI; the machine plane never changes identity or credentials (apis.mdx §11.1)", "the passthrough refuses %s in a profile update", k)
		}
	}

	return nil
}

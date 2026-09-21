package machine

import (
	"encoding/json"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/mayswind/ezbookkeeping/pkg/api"
	"github.com/mayswind/ezbookkeeping/pkg/core"
	"github.com/mayswind/ezbookkeeping/pkg/models"
	"github.com/mayswind/ezbookkeeping/pkg/services"
)

// routes_user.go — the bound user, their settings and their data (apis.mdx §10.1): profile (read and
// the four display preferences — never password, email, avatar or 2FA, §11.1), the application
// cloud settings, data statistics and the ezBookkeeping-format data export.

var refWeekdayNames = []string{"sunday", "monday", "tuesday", "wednesday", "thursday", "friday", "saturday"}

// refFirstDayOfWeek parses 0-6 (Sunday 0) or a weekday name
func refFirstDayOfWeek(v refFlex) (core.WeekDay, error) {
	s := strings.ToLower(strings.TrimSpace(string(v)))

	if n, ok := refWeekdays[s]; ok {
		return core.WeekDay(n), nil
	}

	n, err := strconv.Atoi(s)

	if err != nil || n < 0 || n > 6 {
		return 0, Invalid("first_day_of_week is 0-6 (Sunday 0) or a weekday name", "%q is not a weekday", string(v))
	}

	return core.WeekDay(n), nil
}

// refFiscalYearStart parses MM-DD
func refFiscalYearStart(v string) (core.FiscalYearStart, error) {
	m := refMonthDayRe.FindStringSubmatch(strings.TrimSpace(v))

	if m == nil {
		return 0, Invalid("fiscal_year_start is MM-DD, e.g. 04-06", "%q is not MM-DD", v)
	}

	month, _ := strconv.Atoi(m[1])
	day, _ := strconv.Atoi(m[2])
	f, err := core.NewFiscalYearStart(uint8(month), uint8(day))

	if err != nil || month > 12 || day > 31 {
		return 0, Invalid("fiscal_year_start is a real MM-DD date", "%q is not a real month and day", v)
	}

	return f, nil
}

// refProfileFields are the profile fields this plane may change (journal payloads)
type refProfileFields struct {
	Nickname              string `json:"nickname"`
	DefaultCurrency       string `json:"defaultCurrency"`
	FirstDayOfWeek        int    `json:"firstDayOfWeek"`
	FiscalYearStart       string `json:"fiscalYearStart"`
	UseLastReconciledTime bool   `json:"useLastReconciledTime"`
}

func refProfileFieldsOf(u *models.User) refProfileFields {
	return refProfileFields{Nickname: u.Nickname, DefaultCurrency: u.DefaultCurrency, FirstDayOfWeek: int(u.FirstDayOfWeek), FiscalYearStart: u.FiscalYearStart.String(), UseLastReconciledTime: u.UseLastReconciledTime}
}

// refProfileRequest builds upstream's profile update request holding only the fields that differ
// (upstream answers "nothing will be updated" to a no-op, and treats absent as unchanged)
func refProfileRequest(before, after refProfileFields) (*models.UserProfileUpdateRequest, error) {
	req := &models.UserProfileUpdateRequest{}

	if after.Nickname != before.Nickname {
		req.Nickname = after.Nickname
	}

	if after.DefaultCurrency != before.DefaultCurrency {
		req.DefaultCurrency = after.DefaultCurrency
	}

	if after.FirstDayOfWeek != before.FirstDayOfWeek {
		d := core.WeekDay(after.FirstDayOfWeek)
		req.FirstDayOfWeek = &d
	}

	if after.FiscalYearStart != before.FiscalYearStart {
		f, err := refFiscalYearStart(after.FiscalYearStart)

		if err != nil {
			return nil, err
		}

		req.FiscalYearStart = &f
	}

	if after.UseLastReconciledTime != before.UseLastReconciledTime {
		b := after.UseLastReconciledTime
		req.UseLastReconciledTime = &b
	}

	return req, nil
}

// refHandleProfile is GET /user/profile
func refHandleProfile(mc *Ctx) (any, error) {
	var profile map[string]any

	if err := mc.CallUpstreamInto(api.Users.UserProfileHandler, "GET", nil, nil, &profile); err != nil {
		return nil, err
	}

	user, err := services.Users.GetUserById(mc.Web, mc.Uid)

	if err != nil {
		return nil, err
	}

	day := int(user.FirstDayOfWeek)

	if day >= 0 && day < len(refWeekdayNames) {
		profile["firstDayOfWeekName"] = refWeekdayNames[day]
	}

	profile["fiscalYearStartDate"] = user.FiscalYearStart.String()
	profile["twoFactorNote"] = "2FA protects browser logins; the machine plane is not a login (apis.mdx §6.3)"

	return map[string]any{"profile": profile, "editable": []string{"nickname", "default_currency", "first_day_of_week", "fiscal_year_start", "use_last_reconciled_time"}}, nil
}

type refProfilePatchReq struct {
	WriteOpts
	Nickname              *string  `json:"nickname"`
	DefaultCurrency       *string  `json:"default_currency"`
	FirstDayOfWeek        *refFlex `json:"first_day_of_week"`
	FiscalYearStart       *string  `json:"fiscal_year_start"`
	UseLastReconciledTime *bool    `json:"use_last_reconciled_time"`
	// identity and credentials are the browser's (§11.1) — refused with a hint, not "unknown argument"
	Email       *string `json:"email"`
	Password    *string `json:"password"`
	OldPassword *string `json:"old_password"`
	Avatar      *string `json:"avatar"`
}

// refHandleProfilePatch is PATCH /user/profile
func refHandleProfilePatch(mc *Ctx) (any, error) {
	var req refProfilePatchReq

	if err := mc.BindBody(&req); err != nil {
		return nil, err
	}

	if req.Email != nil || req.Password != nil || req.OldPassword != nil || req.Avatar != nil {
		return nil, NewFail(CodeForbidden, "change the email, password or avatar in the web UI (Settings → User Profile); the machine plane never touches identity or credentials", "identity fields are not editable on the machine plane")
	}

	type state struct {
		Before, After refProfileFields
	}

	return RunWrite(mc, req.WriteOpts, func() (*Plan, error) {
		user, err := services.Users.GetUserById(mc.Web, mc.Uid)

		if err != nil {
			return nil, err
		}

		if user.FeatureRestriction.Contains(core.USER_FEATURE_RESTRICTION_TYPE_UPDATE_PROFILE_BASIC_INFO) && (req.Nickname != nil) {
			return nil, NewFail(CodeForbidden, "an administrator restricted profile changes for this user (feature restriction 3)", "the bound user may not change their basic profile")
		}

		before := refProfileFieldsOf(user)
		after := before
		var changes []refFieldChange
		var warnings []string
		add := func(field string, from, to any) {
			changes = append(changes, refFieldChange{Id: user.Username, Field: field, From: from, To: to})
		}

		if req.Nickname != nil {
			n := strings.TrimSpace(*req.Nickname)

			if n == "" || len([]rune(n)) > 64 {
				return nil, Invalid("nickname is 1-64 characters", "nickname %q is not valid", *req.Nickname)
			}

			if n != before.Nickname {
				after.Nickname = n
				add("nickname", before.Nickname, n)
			}
		}

		if req.DefaultCurrency != nil {
			cur, err := refCurrency(*req.DefaultCurrency)

			if err != nil {
				return nil, err
			}

			if cur != before.DefaultCurrency {
				after.DefaultCurrency = cur
				add("defaultCurrency", before.DefaultCurrency, cur)
				warnings = append(warnings, "the default currency is the currency totals are shown in (meta.defaultCurrency); it never converts stored amounts", "custom exchange rates are stored relative to the default currency; with data_source = user_custom, review them after this change (GET /machine/v1/exchange-rates)")
			}
		}

		if req.FirstDayOfWeek != nil {
			d, err := refFirstDayOfWeek(*req.FirstDayOfWeek)

			if err != nil {
				return nil, err
			}

			if int(d) != before.FirstDayOfWeek {
				after.FirstDayOfWeek = int(d)
				add("firstDayOfWeek", refWeekdayNames[before.FirstDayOfWeek%7], refWeekdayNames[int(d)])
			}
		}

		if req.FiscalYearStart != nil {
			f, err := refFiscalYearStart(*req.FiscalYearStart)

			if err != nil {
				return nil, err
			}

			if f.String() != before.FiscalYearStart {
				after.FiscalYearStart = f.String()
				add("fiscalYearStart", before.FiscalYearStart, f.String())
			}
		}

		if req.UseLastReconciledTime != nil && *req.UseLastReconciledTime != before.UseLastReconciledTime {
			after.UseLastReconciledTime = *req.UseLastReconciledTime
			add("useLastReconciledTime", before.UseLastReconciledTime, after.UseLastReconciledTime)
		}

		if changes == nil {
			changes = []refFieldChange{}
		}

		plan := &Plan{Preview: changes, Warnings: warnings, State: &state{Before: before, After: after}, Changes: map[string]int{"update": 0}}

		if len(changes) > 0 {
			plan.Changes["update"], plan.Count = 1, 1
		} else {
			plan.Changes["unchanged"] = 1
		}

		return plan, nil
	}, func(p *Plan) (any, error) {
		st := p.State.(*state)

		if p.Count > 0 {
			if err := refApplyProfile(mc, st.Before, st.After); err != nil {
				return nil, err
			}

			if err := refJournalRestore(mc, "ref.restore_profile", st.Before, st.After, "edit profile"); err != nil {
				return nil, err
			}
		}

		return map[string]any{"updated": p.Count, "profile": st.After}, nil
	})
}

func refApplyProfile(mc *Ctx, before, after refProfileFields) error {
	req, err := refProfileRequest(before, after)

	if err != nil {
		return err
	}

	_, err = mc.CallUpstream(api.Users.UserUpdateProfileHandler, "POST", nil, req)

	return err
}

func refInvRestoreProfile(mc *Ctx, payload, check json.RawMessage) error {
	var want refProfileFields

	if err := json.Unmarshal(payload, &want); err != nil {
		return err
	}

	user, err := services.Users.GetUserById(mc.Web, mc.Uid)

	if err != nil {
		return err
	}

	current := refProfileFieldsOf(user)

	if !refCheckMatches(current, check) {
		return Conflict("the profile was changed again since (in the browser or by another write); nothing was changed", "the profile changed since the write")
	}

	if current == want {
		return nil
	}

	return refApplyProfile(mc, current, want)
}

// --- application cloud settings

// refSettingsState is the stored cloud settings: nil Settings means cloud sync is off
type refSettingsState struct {
	Enabled  bool              `json:"enabled"`
	Settings map[string]string `json:"settings"`
}

func refLoadSettings(mc *Ctx) (refSettingsState, error) {
	row, err := services.UserApplicationCloudSettings.GetUserApplicationCloudSettingsByUid(mc.Web, mc.Uid)

	if err != nil {
		return refSettingsState{}, err
	}

	st := refSettingsState{Settings: map[string]string{}}

	if row != nil && len(row.Settings) > 0 {
		st.Enabled = true

		for _, s := range row.Settings {
			st.Settings[s.SettingKey] = s.SettingValue
		}
	}

	return st, nil
}

// refSettingValue converts a JSON value to upstream's stored string form, validated against the
// key's declared type (upstream silently DROPS an ill-typed value; the plane refuses it instead)
func refSettingValue(key string, raw json.RawMessage) (string, error) {
	typ, ok := models.ALL_ALLOWED_CLOUD_SYNC_APP_SETTING_KEY_TYPES[key]

	if !ok {
		return "", Invalid("GET /machine/v1/user/settings lists the keys (allowedKeys)", "%q is not a synchronisable application setting", key)
	}

	s := strings.TrimSpace(string(raw))

	switch typ {
	case models.USER_APPLICATION_CLOUD_SETTING_TYPE_BOOLEAN:
		var b bool

		if err := json.Unmarshal(raw, &b); err != nil {
			var str string

			if json.Unmarshal(raw, &str) == nil && (str == "true" || str == "false") {
				return str, nil
			}

			return "", Invalid(key+" is a boolean", "%s must be true or false", key)
		}

		return strconv.FormatBool(b), nil
	case models.USER_APPLICATION_CLOUD_SETTING_TYPE_NUMBER:
		var f refFlex

		if err := json.Unmarshal(raw, &f); err != nil {
			return "", Invalid(key+" is a number", "%s must be a number", key)
		}

		if _, err := strconv.ParseFloat(string(f), 64); err != nil {
			return "", Invalid(key+" is a number", "%s value %q is not a number", key, string(f))
		}

		return string(f), nil
	case models.USER_APPLICATION_CLOUD_SETTING_TYPE_STRING:
		var str string

		if err := json.Unmarshal(raw, &str); err != nil {
			return "", Invalid(key+" is a string", "%s must be a string", key)
		}

		return str, nil
	case models.USER_APPLICATION_CLOUD_SETTING_TYPE_STRING_BOOLEAN_MAP:
		var m map[string]bool

		if err := json.Unmarshal(raw, &m); err != nil {
			var str string

			if json.Unmarshal(raw, &str) == nil && json.Unmarshal([]byte(str), &m) == nil {
				return str, nil
			}

			return "", Invalid(key+" is an object of booleans, e.g. {\"123\": true}", "%s must be a map of booleans", key)
		}

		data, _ := json.Marshal(m)

		return string(data), nil
	}

	return s, nil
}

// refHandleSettings is GET /user/settings
func refHandleSettings(mc *Ctx) (any, error) {
	st, err := refLoadSettings(mc)

	if err != nil {
		return nil, err
	}

	keys := make([]string, 0, len(st.Settings))

	for k := range st.Settings {
		keys = append(keys, k)
	}

	sort.Strings(keys)
	rows := make([]map[string]any, 0, len(keys))

	for _, k := range keys {
		rows = append(rows, map[string]any{"key": k, "value": st.Settings[k], "type": string(models.ALL_ALLOWED_CLOUD_SYNC_APP_SETTING_KEY_TYPES[k])})
	}

	allowed := map[string]string{}

	for k, t := range models.ALL_ALLOWED_CLOUD_SYNC_APP_SETTING_KEY_TYPES {
		allowed[k] = string(t)
	}

	return map[string]any{"cloudSyncEnabled": st.Enabled, "settings": rows, "count": len(rows), "allowedKeys": allowed,
		"note": "values are upstream's stored strings; these are the web UI's synchronised display preferences"}, nil
}

type refSettingsPatchReq struct {
	WriteOpts
	Settings map[string]json.RawMessage `json:"settings"`
}

// refHandleSettingsPatch is PATCH /user/settings {settings: {key: value}} — merged into the stored
// set and written as upstream's full update
func refHandleSettingsPatch(mc *Ctx) (any, error) {
	var req refSettingsPatchReq

	if err := mc.BindBody(&req); err != nil {
		return nil, err
	}

	if len(req.Settings) == 0 {
		return nil, Invalid("pass settings: {key: value, …} (GET /machine/v1/user/settings lists allowedKeys)", "no settings given")
	}

	type state struct {
		Before, After refSettingsState
	}

	return RunWrite(mc, req.WriteOpts, func() (*Plan, error) {
		user, err := services.Users.GetUserById(mc.Web, mc.Uid)

		if err != nil {
			return nil, err
		}

		if user.FeatureRestriction.Contains(core.USER_FEATURE_RESTRICTION_TYPE_SYNC_APPLICATION_SETTINGS) {
			return nil, NewFail(CodeForbidden, "an administrator restricted settings sync for this user (feature restriction 12)", "the bound user may not sync application settings")
		}

		before, err := refLoadSettings(mc)

		if err != nil {
			return nil, err
		}

		after := refSettingsState{Enabled: true, Settings: map[string]string{}}

		for k, v := range before.Settings {
			after.Settings[k] = v
		}

		keys := make([]string, 0, len(req.Settings))

		for k := range req.Settings {
			keys = append(keys, k)
		}

		sort.Strings(keys)
		changes := []refFieldChange{}

		for _, k := range keys {
			v, err := refSettingValue(k, req.Settings[k])

			if err != nil {
				return nil, err
			}

			old, had := before.Settings[k]

			if had && old == v {
				continue
			}

			after.Settings[k] = v
			var from any

			if had {
				from = old
			}

			changes = append(changes, refFieldChange{Id: k, Field: k, From: from, To: v})
		}

		var warnings []string

		if !before.Enabled && len(changes) > 0 {
			warnings = append(warnings, "cloud sync of application settings is currently OFF for this user; writing settings turns it ON (the web UI will start using the stored values)")
		}

		plan := &Plan{Preview: changes, Warnings: warnings, State: &state{Before: before, After: after}, Changes: map[string]int{"update": len(changes)}, Count: 0}

		if len(changes) > 0 {
			plan.Count = 1
		} else {
			plan.Changes["unchanged"] = len(keys)
		}

		return plan, nil
	}, func(p *Plan) (any, error) {
		st := p.State.(*state)

		if p.Count == 0 {
			return map[string]any{"updated": 0}, nil
		}

		if err := refWriteSettings(mc, st.After); err != nil {
			return nil, err
		}

		if err := refJournalRestore(mc, "ref.restore_settings", st.Before, st.After, "edit application settings"); err != nil {
			return nil, err
		}

		return map[string]any{"updated": len(p.Preview.([]refFieldChange)), "cloudSyncEnabled": true}, nil
	})
}

// refWriteSettings writes a full settings set, or disables cloud sync when st is disabled
func refWriteSettings(mc *Ctx, st refSettingsState) error {
	if !st.Enabled {
		_, err := mc.CallUpstream(api.UserApplicationCloudSettings.ApplicationSettingsDisableHandler, "POST", nil, map[string]any{})

		return err
	}

	keys := make([]string, 0, len(st.Settings))

	for k := range st.Settings {
		keys = append(keys, k)
	}

	sort.Strings(keys)
	list := make(models.ApplicationCloudSettingSlice, 0, len(keys))

	for _, k := range keys {
		list = append(list, models.ApplicationCloudSetting{SettingKey: k, SettingValue: st.Settings[k]})
	}

	_, err := mc.CallUpstream(api.UserApplicationCloudSettings.ApplicationSettingsUpdateHandler, "POST", nil, &models.UserApplicationCloudSettingsUpdateRequest{Settings: list, FullUpdate: true})

	return err
}

func refInvRestoreSettings(mc *Ctx, payload, check json.RawMessage) error {
	var want refSettingsState

	if err := json.Unmarshal(payload, &want); err != nil {
		return err
	}

	current, err := refLoadSettings(mc)

	if err != nil {
		return err
	}

	if !refCheckMatches(current, check) {
		return Conflict("the settings were changed again since (probably by the web UI's sync); nothing was changed", "application settings changed since the write")
	}

	if refCheckMatches(current, refMustJSON(want)) {
		return nil
	}

	return refWriteSettings(mc, want)
}

// --- data statistics and export

func refHandleDataStatistics(mc *Ctx) (any, error) {
	var raw map[string]string

	if err := mc.CallUpstreamInto(api.DataManagements.DataStatisticsHandler, "GET", nil, nil, &raw); err != nil {
		return nil, err
	}

	counts := map[string]any{}

	for k, v := range raw {
		n, err := strconv.ParseInt(v, 10, 64)

		if err != nil {
			return nil, NewFail(CodeUpstreamError, "read ~/T/ezbookkeeping/error.err", "the data statistics returned a non-integer %s", k)
		}

		counts[k] = n
	}

	return map[string]any{"statistics": counts}, nil
}

// refHandleDataExport is GET /data/export — upstream's ezBookkeeping-format CSV/TSV, returned as the
// raw file (not the JSON envelope). Optional start/end (YYYY-MM-DD, inclusive) narrow it.
func refHandleDataExport(mc *Ctx) (any, error) {
	format := strings.ToLower(mc.Query("format"))

	if format == "" {
		format = "csv"
	}

	q := url.Values{}

	if s, e := mc.Query("start"), mc.Query("end"); s != "" || e != "" {
		r, err := ParseDateRange(s, e, "1970-01-01", time.Now().In(mc.Loc).Format("2006-01-02"), mc.Loc)

		if err != nil {
			return nil, err
		}

		q.Set("min_time", strconv.FormatInt(r.StartUnix, 10))
		q.Set("max_time", strconv.FormatInt(r.EndUnix, 10))
	}

	var data []byte
	var fileName string
	var err error
	contentType := ""

	switch format {
	case "csv":
		data, fileName, err = mc.CallUpstreamData(api.DataManagements.ExportDataToEzbookkeepingCSVHandler, q)
		contentType = "text/csv; charset=utf-8"
	case "tsv":
		data, fileName, err = mc.CallUpstreamData(api.DataManagements.ExportDataToEzbookkeepingTSVHandler, q)
		contentType = "text/tab-separated-values; charset=utf-8"
	default:
		return nil, Invalid("format is csv or tsv", "unknown format %q", format)
	}

	if err != nil {
		return nil, err
	}

	if fileName == "" {
		fileName = fmt.Sprintf("ezbookkeeping_export.%s", format)
	}

	return &RawResult{ContentType: contentType, FileName: fileName, Data: data}, nil
}

func init() {
	RegisterInverse("ref.restore_profile", refInvRestoreProfile)
	RegisterInverse("ref.restore_settings", refInvRestoreSettings)
	registerRoutes(refUserRoutes)
}

func refUserRoutes() []RouteDef {
	return []RouteDef{
		{Method: "GET", Path: "/user/profile", Tier: TierRead, Handler: refHandleProfile, Untrusted: []string{"nickname"},
			Summary: "The bound user's profile: nickname, default currency, first day of week, fiscal year start, display formats."},
		{Method: "PATCH", Path: "/user/profile", Tier: TierWrite, DryRunnable: true, Handler: refHandleProfilePatch,
			Summary: "Change nickname, default_currency, first_day_of_week (0-6 or name), fiscal_year_start (MM-DD), use_last_reconciled_time. Never password, email or avatar."},
		{Method: "GET", Path: "/user/settings", Tier: TierRead, Handler: refHandleSettings,
			Summary: "The web UI's synchronised application settings, with the allowed keys and their types."},
		{Method: "PATCH", Path: "/user/settings", Tier: TierWrite, DryRunnable: true, Handler: refHandleSettingsPatch,
			Summary: "Set one or more application settings. Body: settings: {key: value}; values are type-checked against allowedKeys."},
		{Method: "GET", Path: "/data/statistics", Tier: TierRead, Handler: refHandleDataStatistics,
			Summary: "Counts of accounts, categories, tags, transactions, pictures, templates, schedules, insights and custom icons."},
		{Method: "GET", Path: "/data/export", Tier: TierRead, Handler: refHandleDataExport, Feature: featureExport,
			Summary: "Upstream's ezBookkeeping-format export as a raw file. Args: format (csv|tsv), start, end (YYYY-MM-DD)."},
	}
}

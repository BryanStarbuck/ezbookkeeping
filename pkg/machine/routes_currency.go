package machine

import (
	"encoding/json"
	"math/big"
	"sort"
	"strings"
	"time"

	"github.com/mayswind/ezbookkeeping/pkg/api"
	"github.com/mayswind/ezbookkeeping/pkg/exchangerates"
	"github.com/mayswind/ezbookkeeping/pkg/models"
	"github.com/mayswind/ezbookkeeping/pkg/services"
	"github.com/mayswind/ezbookkeeping/pkg/settings"
)

// routes_currency.go — exchange rates and conversion (apis.mdx §10.7, the rule in §13).
//
// The rates are the app's, exactly as the browser sees them: exchangerates.Container.
// GetLatestExchangeRates is what upstream's /api/v1/exchange_rates/latest.json answers and what the
// web UI's exchangeRates store converts with (src/stores/exchangeRates.ts getExchangedAmount →
// src/lib/numeral.ts getExchangedAmountByRate: amount × toRate ÷ fromRate). Every rate is "units of
// that currency per ONE unit of the base currency", so money.go's ConvertHundredths(amount, fromRate,
// toRate) = amount ÷ fromRate × toRate is the same arithmetic, rounded once, half away from zero.
//
// Custom rates are upstream's too, and upstream uses them ONLY when the server's exchange-rate data
// source is "user_custom" ([exchange_rates] data_source); with a provider configured they are
// stored but dormant. The plane reports that verbatim (R7) rather than merging dormant rows into
// conversions the browser would not make — each rate is marked source provider|custom and every
// custom row says whether it is active.

// refRateSourceCustom is upstream's data source name for user-entered rates
const refRateSourceCustom = settings.UserCustomExchangeRatesDataSource

// refLatestRates fetches the app's latest rates (the provider's, or the user's own custom table)
func refLatestRates(mc *Ctx) (*models.LatestExchangeRateResponse, error) {
	resp, err := exchangerates.Container.GetLatestExchangeRates(mc.Web, mc.Uid, mc.Config)

	if err != nil || resp == nil {
		return nil, NewFail(CodeUpstreamError, "the rate source ("+mc.Config.ExchangeRatesDataSource+") could not be read; check the server's network, or set [exchange_rates] data_source in conf/ezbookkeeping.ini", "the exchange rates are unavailable")
	}

	return resp, nil
}

// refRateView is one rate on the wire: units of Currency per ONE unit of BaseCurrency
type refRateView struct {
	Currency     string `json:"currency"`
	Rate         string `json:"rate"`
	BaseCurrency string `json:"baseCurrency"`
	Source       string `json:"source"`
	Provider     string `json:"provider"`
	UpdateTime   int64  `json:"updateTime"`
	UpdatedAt    string `json:"updatedAt"`
	IsBase       bool   `json:"isBase,omitempty"`
}

// refCustomRateView is one stored custom rate: units of Currency per ONE unit of the user's default
// currency. Active is false when the server's data source is a provider (upstream ignores them then).
type refCustomRateView struct {
	Currency     string `json:"currency"`
	Rate         string `json:"rate"`
	RelativeTo   string `json:"relativeTo"`
	UpdateTime   int64  `json:"updateTime"`
	UpdatedAt    string `json:"updatedAt"`
	Active       bool   `json:"active"`
	Meaning      string `json:"meaning"`
	IsDefaultRow bool   `json:"isDefaultRow,omitempty"`
}

// refRatString renders a positive ratio as a trimmed decimal (at most 12 places)
func refRatString(r *big.Rat) string {
	s := r.FloatString(12)

	if strings.Contains(s, ".") {
		s = strings.TrimRight(strings.TrimRight(s, "0"), ".")
	}

	return s
}

// refCustomRelative is a stored custom rate relative to the default currency's stored rate
func refCustomRelative(raw, defaultRaw int64) string {
	if defaultRaw <= 0 {
		defaultRaw = models.UserCustomExchangeRateFactorInDatabase
	}

	return refRatString(new(big.Rat).SetFrac64(raw, defaultRaw))
}

// refRateMeaning spells a rate out, both ways, so nobody sets EUR 1.08 meaning the inverse
func refRateMeaning(base, currency, rate string) string {
	r, err := ParseRate(rate)

	if err != nil {
		return ""
	}

	inv := new(big.Rat).Inv(r.r)

	return "1 " + base + " = " + rate + " " + currency + " (so 1 " + currency + " = " + refRatString(inv) + " " + base + ")"
}

func refUnixRFC3339(sec int64) string {
	if sec <= 0 {
		return ""
	}

	return time.Unix(sec, 0).UTC().Format(time.RFC3339)
}

// refCustomRows returns the user's stored custom rates keyed by currency, and the default
// currency's stored rate (the denominator of every other row)
func refCustomRows(mc *Ctx) (map[string]*models.UserCustomExchangeRate, int64, error) {
	rows, err := services.UserCustomExchangeRates.GetAllCustomExchangeRatesByUid(mc.Web, mc.Uid)

	if err != nil {
		return nil, 0, err
	}

	out := map[string]*models.UserCustomExchangeRate{}

	for _, r := range rows {
		out[r.Currency] = r
	}

	defaultRaw := int64(0)

	if d, ok := out[mc.User.DefaultCurrency]; ok {
		defaultRaw = d.Rate
	}

	return out, defaultRaw, nil
}

// refRateTable is the app's effective rates, keyed by currency, with their provenance
func refRateTable(mc *Ctx) (map[string]*refRateView, *models.LatestExchangeRateResponse, error) {
	latest, err := refLatestRates(mc)

	if err != nil {
		return nil, nil, err
	}

	source := "provider"
	var custom map[string]*models.UserCustomExchangeRate

	if latest.DataSource == refRateSourceCustom {
		source = "custom"

		if custom, _, err = refCustomRows(mc); err != nil {
			return nil, nil, err
		}
	}

	table := map[string]*refRateView{}

	for _, r := range latest.ExchangeRates {
		v := &refRateView{Currency: r.Currency, Rate: r.Rate, BaseCurrency: latest.BaseCurrency, Source: source, Provider: latest.DataSource, UpdateTime: latest.UpdateTime, IsBase: r.Currency == latest.BaseCurrency}

		if c, ok := custom[r.Currency]; ok && c.UpdatedUnixTime > 0 {
			v.UpdateTime = c.UpdatedUnixTime
		}

		v.UpdatedAt = refUnixRFC3339(v.UpdateTime)
		table[r.Currency] = v
	}

	if _, ok := table[latest.BaseCurrency]; !ok && latest.BaseCurrency != "" {
		table[latest.BaseCurrency] = &refRateView{Currency: latest.BaseCurrency, Rate: "1", BaseCurrency: latest.BaseCurrency, Source: source, Provider: latest.DataSource, UpdateTime: latest.UpdateTime, UpdatedAt: refUnixRFC3339(latest.UpdateTime), IsBase: true}
	}

	return table, latest, nil
}

// refHandleRates is GET /exchange-rates
func refHandleRates(mc *Ctx) (any, error) {
	table, latest, err := refRateTable(mc)

	if err != nil {
		return nil, err
	}

	want := map[string]bool{}
	var missing []string

	for _, c := range mc.QueryList("currencies") {
		cur, err := refCurrency(c)

		if err != nil {
			return nil, err
		}

		want[cur] = true

		if _, ok := table[cur]; !ok {
			missing = append(missing, cur)
		}
	}

	rates := make([]*refRateView, 0, len(table))

	for cur, v := range table {
		if len(want) == 0 || want[cur] {
			rates = append(rates, v)
		}
	}

	sort.Slice(rates, func(i, j int) bool { return rates[i].Currency < rates[j].Currency })

	custom, defaultRaw, err := refCustomRows(mc)

	if err != nil {
		return nil, err
	}

	active := latest.DataSource == refRateSourceCustom
	customRows := make([]*refCustomRateView, 0, len(custom))

	for cur, c := range custom {
		if len(want) > 0 && !want[cur] {
			continue
		}

		rel := refCustomRelative(c.Rate, defaultRaw)
		customRows = append(customRows, &refCustomRateView{
			Currency: cur, Rate: rel, RelativeTo: mc.User.DefaultCurrency, UpdateTime: c.UpdatedUnixTime, UpdatedAt: refUnixRFC3339(c.UpdatedUnixTime),
			Active: active, Meaning: refRateMeaning(mc.User.DefaultCurrency, cur, rel), IsDefaultRow: cur == mc.User.DefaultCurrency,
		})
	}

	sort.Slice(customRows, func(i, j int) bool { return customRows[i].Currency < customRows[j].Currency })

	if missing == nil {
		missing = []string{}
	}

	out := map[string]any{
		"rates": rates, "count": len(rates), "baseCurrency": latest.BaseCurrency, "provider": latest.DataSource, "referenceUrl": latest.ReferenceUrl,
		"updateTime": latest.UpdateTime, "updatedAt": refUnixRFC3339(latest.UpdateTime), "defaultCurrency": mc.User.DefaultCurrency,
		"customRates": customRows, "customRatesActive": active, "missing": missing, "rateBasis": "latest",
		"convention": "rate = units of that currency per ONE unit of baseCurrency; convert with amount × toRate ÷ fromRate (POST /machine/v1/exchange-rates/convert does it for you)",
		"note":       "ezBookkeeping keeps only the latest rates, not historical ones",
	}

	if !active && len(customRows) > 0 {
		out["customNote"] = "custom rates are stored but INACTIVE: the server's data source is " + latest.DataSource + "; upstream uses custom rates only with [exchange_rates] data_source = user_custom"
	}

	return out, nil
}

type refConvertReq struct {
	Amount json.Number `json:"amount"`
	From   string      `json:"from"`
	To     string      `json:"to"`
}

// refHandleConvert is POST /exchange-rates/convert (read tier): the app's own rate, named, rounded once
func refHandleConvert(mc *Ctx) (any, error) {
	var req refConvertReq

	if err := mc.BindBody(&req); err != nil {
		return nil, err
	}

	if req.Amount == "" {
		return nil, Invalid("pass amount in integer hundredths (500.00 is 50000)", "amount is required")
	}

	amount, err := AmountArg("amount", req.Amount)

	if err != nil {
		return nil, err
	}

	from, err := refCurrency(req.From)

	if err != nil {
		return nil, err
	}

	to, err := refCurrency(req.To)

	if err != nil {
		return nil, err
	}

	table, latest, err := refRateTable(mc)

	if err != nil {
		return nil, err
	}

	note := "converted at the app's latest rate; ezBookkeeping keeps no historical rates, so a past amount is converted at today's rate"
	out := map[string]any{"amount": amount, "from": from, "to": to, "currency": to, "baseCurrency": latest.BaseCurrency, "provider": latest.DataSource,
		"referenceUrl": latest.ReferenceUrl, "rateBasis": "latest", "rounding": "once, half away from zero, to hundredths", "note": note}

	if from == to {
		out["converted"] = amount
		out["rate"] = "1"
		out["rates"] = []*refRateView{}

		return out, nil
	}

	fromView, okFrom := table[from]
	toView, okTo := table[to]

	if !okFrom || !okTo {
		var missing []string

		if !okFrom {
			missing = append(missing, from)
		}

		if !okTo {
			missing = append(missing, to)
		}

		hint := "GET /machine/v1/exchange-rates lists the currencies the app has rates for"

		if latest.DataSource == refRateSourceCustom {
			hint = "set one with PUT /machine/v1/exchange-rates/custom/" + missing[0] + " {rate} — " + hint
		}

		return nil, NotFound(hint, "the app has no %s rate for %s", latest.DataSource, strings.Join(missing, " or ")).WithDetails(map[string]any{"missing": missing})
	}

	fromRate, err := ParseRate(fromView.Rate)

	if err != nil {
		return nil, NewFail(CodeUpstreamError, "the rate source returned an unusable rate; refresh the rates in the web UI", "the %s rate %q is not a positive decimal", from, fromView.Rate)
	}

	toRate, err := ParseRate(toView.Rate)

	if err != nil {
		return nil, NewFail(CodeUpstreamError, "the rate source returned an unusable rate; refresh the rates in the web UI", "the %s rate %q is not a positive decimal", to, toView.Rate)
	}

	converted := ConvertHundredths(amount, fromRate, toRate)

	if converted > MaxSafeInteger || converted < -MaxSafeInteger {
		return nil, NewFail(CodeUpstreamError, "convert a smaller amount", "the converted amount exceeds the safe integer range and was refused rather than rounded")
	}

	out["converted"] = converted
	out["rate"] = refRatString(new(big.Rat).Quo(toRate.r, fromRate.r))
	out["rateMeaning"] = "1 " + from + " = " + out["rate"].(string) + " " + to
	out["rates"] = []*refRateView{fromView, toView}

	return out, nil
}

// --- custom rates (write tier)

type refCustomRateReq struct {
	WriteOpts
	Rate refFlex `json:"rate"`
}

// refCustomRateState is a custom rate's stored value, for the journal: Raw is upstream's stored
// integer (nil = no row), Rate the same relative to the default currency
type refCustomRateState struct {
	Currency string  `json:"currency"`
	Rate     *string `json:"rate"`
	Raw      *int64  `json:"raw"`
}

func refCustomRateNow(mc *Ctx, currency string) (refCustomRateState, error) {
	rows, defaultRaw, err := refCustomRows(mc)

	if err != nil {
		return refCustomRateState{}, err
	}

	st := refCustomRateState{Currency: currency}

	if r, ok := rows[currency]; ok {
		raw := r.Rate
		rel := refCustomRelative(r.Rate, defaultRaw)
		st.Raw, st.Rate = &raw, &rel
	}

	return st, nil
}

func refCustomRateCurrency(mc *Ctx) (string, error) {
	cur, err := refCurrency(mc.Param("currency"))

	if err != nil {
		return "", err
	}

	if cur == mc.User.DefaultCurrency {
		return "", Invalid("custom rates are relative to your default currency ("+cur+"), whose rate is always 1; set the OTHER currency's rate", "%s is the default currency", cur)
	}

	return cur, nil
}

func refRateDataSourceWarning(mc *Ctx) []string {
	if mc.Config.ExchangeRatesDataSource != refRateSourceCustom {
		return []string{"this server's rate source is " + mc.Config.ExchangeRatesDataSource + ": upstream uses custom rates only with [exchange_rates] data_source = user_custom, so this row is stored but conversions keep using the provider's rate"}
	}

	return nil
}

// refHandleCustomRatePut is PUT /exchange-rates/custom/:currency {rate}
func refHandleCustomRatePut(mc *Ctx) (any, error) {
	var req refCustomRateReq

	if err := mc.BindBody(&req); err != nil {
		return nil, err
	}

	type state struct {
		Currency string
		Rate     string
		Before   refCustomRateState
	}

	return RunWrite(mc, req.WriteOpts, func() (*Plan, error) {
		cur, err := refCustomRateCurrency(mc)

		if err != nil {
			return nil, err
		}

		if strings.TrimSpace(string(req.Rate)) == "" {
			return nil, Invalid("pass rate: units of "+cur+" per ONE "+mc.User.DefaultCurrency+", as a decimal", "rate is required")
		}

		r, err := ParseRate(string(req.Rate))

		if err != nil {
			return nil, Invalid("a rate is a positive decimal: units of "+cur+" per ONE "+mc.User.DefaultCurrency, "rate %q is not a positive decimal", string(req.Rate))
		}

		rate := refRatString(r.r)
		before, err := refCustomRateNow(mc, cur)

		if err != nil {
			return nil, err
		}

		preview := map[string]any{
			"currency": cur, "from": before.Rate, "to": rate, "relativeTo": mc.User.DefaultCurrency,
			"meaning": refRateMeaning(mc.User.DefaultCurrency, cur, rate),
		}
		plan := &Plan{Preview: preview, Warnings: refRateDataSourceWarning(mc), State: &state{Currency: cur, Rate: rate, Before: before}, Changes: map[string]int{}}

		switch {
		case before.Rate == nil:
			plan.Changes["create"], plan.Count = 1, 1
		case *before.Rate == rate:
			plan.Changes["unchanged"] = 1
		default:
			plan.Changes["update"], plan.Count = 1, 1
		}

		return plan, nil
	}, func(p *Plan) (any, error) {
		st := p.State.(*state)

		if p.Count == 0 {
			return map[string]any{"currency": st.Currency, "rate": st.Rate, "changed": 0}, nil
		}

		if _, err := mc.CallUpstream(api.ExchangeRates.UserCustomExchangeRateUpdateHandler, "POST", nil, &models.UserCustomExchangeRateUpdateRequest{Currency: st.Currency, Rate: st.Rate}); err != nil {
			return nil, err
		}

		after, err := refCustomRateNow(mc, st.Currency)

		if err != nil {
			return nil, err
		}

		var op InverseOp

		if st.Before.Rate == nil {
			op = NewInverseOp("ref.delete_custom_rate", map[string]string{"currency": st.Currency}, after)
		} else {
			op = NewInverseOp("ref.set_custom_rate", st.Before, after)
		}

		redo := NewInverseOp("ref.set_custom_rate", after, nil)
		op.Redo = &redo

		if _, err := RecordJournal(mc, "set custom rate "+st.Currency, 1, []InverseOp{op}); err != nil {
			return nil, err
		}

		return map[string]any{"currency": st.Currency, "rate": after.Rate, "relativeTo": mc.User.DefaultCurrency, "changed": 1,
			"active": mc.Config.ExchangeRatesDataSource == refRateSourceCustom, "meaning": refRateMeaning(mc.User.DefaultCurrency, st.Currency, refStr(after.Rate))}, nil
	})
}

// refHandleCustomRateDelete is DELETE /exchange-rates/custom/:currency — a custom rate is a
// preference, not a record, so this is write tier; deleting a rate that is not set is ok, deleted: 0
func refHandleCustomRateDelete(mc *Ctx) (any, error) {
	var req refDeleteReq

	if err := mc.BindBody(&req); err != nil {
		return nil, err
	}

	cur, err := refCustomRateCurrency(mc)

	if err != nil {
		return nil, err
	}

	if before, err := refCustomRateNow(mc, cur); err != nil {
		return nil, err
	} else if before.Rate == nil {
		return map[string]any{"currency": cur, "deleted": 0, "note": "no custom rate was set"}, nil
	}

	return RunWrite(mc, req.WriteOpts, func() (*Plan, error) {
		before, err := refCustomRateNow(mc, cur)

		if err != nil {
			return nil, err
		}

		if before.Rate == nil {
			return nil, Conflict("it was removed meanwhile; nothing to do", "no custom %s rate is set", cur)
		}

		preview := map[string]any{"currency": cur, "from": *before.Rate, "to": nil, "relativeTo": mc.User.DefaultCurrency}
		warnings := []string{}

		if mc.Config.ExchangeRatesDataSource == refRateSourceCustom {
			warnings = append(warnings, "the server's rate source is user_custom: with this row gone, "+cur+" has NO rate and amounts in it cannot be converted until one is set again")
		}

		return &Plan{Changes: map[string]int{"delete": 1}, Count: 1, Preview: preview, Warnings: warnings, State: before}, nil
	}, func(p *Plan) (any, error) {
		before := p.State.(refCustomRateState)

		if _, err := mc.CallUpstream(api.ExchangeRates.UserCustomExchangeRateDeleteHandler, "POST", nil, &models.UserCustomExchangeRateDeleteRequest{Currency: cur}); err != nil {
			return nil, err
		}

		op := NewInverseOp("ref.set_custom_rate", before, refCustomRateState{Currency: cur})
		redo := NewInverseOp("ref.delete_custom_rate", map[string]string{"currency": cur}, nil)
		op.Redo = &redo

		if _, err := RecordJournal(mc, "delete custom rate "+cur, 1, []InverseOp{op}); err != nil {
			return nil, err
		}

		return map[string]any{"currency": cur, "deleted": 1}, nil
	})
}

// refCustomRateCheck compares the stored raw value against a journal check
func refCustomRateCheck(current refCustomRateState, check json.RawMessage) bool {
	if len(check) == 0 || string(check) == "null" {
		return true
	}

	var want refCustomRateState

	if err := json.Unmarshal(check, &want); err != nil {
		return false
	}

	if want.Raw == nil || current.Raw == nil {
		return want.Raw == nil && current.Raw == nil
	}

	return *want.Raw == *current.Raw
}

func refInvSetCustomRate(mc *Ctx, payload, check json.RawMessage) error {
	var want refCustomRateState

	if err := json.Unmarshal(payload, &want); err != nil {
		return err
	}

	current, err := refCustomRateNow(mc, want.Currency)

	if err != nil {
		return err
	}

	if !refCustomRateCheck(current, check) {
		return Conflict("the "+want.Currency+" rate was changed again since; nothing was changed", "custom %s rate changed since the write", want.Currency)
	}

	if want.Rate == nil {
		if current.Rate == nil {
			return nil
		}

		_, err := mc.CallUpstream(api.ExchangeRates.UserCustomExchangeRateDeleteHandler, "POST", nil, &models.UserCustomExchangeRateDeleteRequest{Currency: want.Currency})

		return err
	}

	_, err = mc.CallUpstream(api.ExchangeRates.UserCustomExchangeRateUpdateHandler, "POST", nil, &models.UserCustomExchangeRateUpdateRequest{Currency: want.Currency, Rate: *want.Rate})

	return err
}

func refInvDeleteCustomRate(mc *Ctx, payload, check json.RawMessage) error {
	var p struct {
		Currency string `json:"currency"`
	}

	if err := json.Unmarshal(payload, &p); err != nil {
		return err
	}

	current, err := refCustomRateNow(mc, p.Currency)

	if err != nil {
		return err
	}

	if current.Rate == nil {
		return nil
	}

	if !refCustomRateCheck(current, check) {
		return Conflict("the "+p.Currency+" rate was changed again since; nothing was changed", "custom %s rate changed since the write", p.Currency)
	}

	_, err = mc.CallUpstream(api.ExchangeRates.UserCustomExchangeRateDeleteHandler, "POST", nil, &models.UserCustomExchangeRateDeleteRequest{Currency: p.Currency})

	return err
}

func init() {
	RegisterInverse("ref.set_custom_rate", refInvSetCustomRate)
	RegisterInverse("ref.delete_custom_rate", refInvDeleteCustomRate)
	registerRoutes(refCurrencyRoutes)
}

func refCurrencyRoutes() []RouteDef {
	return []RouteDef{
		{Method: "GET", Path: "/exchange-rates", Tier: TierRead, Handler: refHandleRates, Features: []string{"currency.rates"},
			Summary: "The app's latest exchange rates (units per ONE base-currency unit), each with source (provider|custom), provider and updateTime, plus the stored custom rates and whether upstream uses them. Args: currencies (list)."},
		{Method: "POST", Path: "/exchange-rates/convert", Tier: TierRead, Composed: true, Handler: refHandleConvert, Features: []string{"currency.convert"},
			Summary: "Convert an amount with the app's own rates. Body: amount (hundredths), from, to. Returns converted (hundredths), the rate used, both source rates with their source and updateTime, rateBasis latest."},
		{Method: "PUT", Path: "/exchange-rates/custom/:currency", Tier: TierWrite, DryRunnable: true, Handler: refHandleCustomRatePut,
			Summary: "Set a custom rate: rate = units of :currency per ONE unit of the user's default currency (a decimal). Used by upstream only when [exchange_rates] data_source = user_custom; the preview says so."},
		{Method: "DELETE", Path: "/exchange-rates/custom/:currency", Tier: TierWrite, DryRunnable: true, Handler: refHandleCustomRateDelete,
			Summary: "Remove a custom rate (a preference, not a record — write tier). Not set is ok with deleted: 0."},
	}
}

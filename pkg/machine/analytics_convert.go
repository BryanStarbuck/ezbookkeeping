package machine

import (
	"sort"
	"strings"
	"time"

	"github.com/mayswind/ezbookkeeping/pkg/api"
	"github.com/mayswind/ezbookkeeping/pkg/errfile"
	"github.com/mayswind/ezbookkeeping/pkg/models"
	"github.com/mayswind/ezbookkeeping/pkg/services"
)

// analytics_convert.go — `convert_to` (apis.mdx §13.2). The app's rates only; every rate used is
// named with its source and time; the rate basis is always "latest"; each currency's total is
// converted ONCE (the multiplication itself lives in money.go); a currency with no rate is reported
// in unconverted[], never dropped.

const anRateNote = "ezBookkeeping keeps latest exchange rates only, not historical ones: every converted figure, including ones from past periods, uses the current rate"

const anUserCustomDataSource = "user_custom"

// anRateInfo is the provenance of one rate
type anRateInfo struct {
	Currency       string `json:"currency"`
	Rate           string `json:"rate"`
	BaseCurrency   string `json:"baseCurrency"`
	Source         string `json:"source"`
	Provider       string `json:"provider"`
	UpdateTime     string `json:"updateTime"`
	UpdateUnixTime int64  `json:"updateUnixTime"`
}

// anConverter converts per-currency totals into one target currency
type anConverter struct {
	Target string

	rates   map[string]Rate
	info    map[string]anRateInfo
	used    map[string]bool
	missing map[string]bool
}

// anLoadConverter reads the app's latest rates (upstream's LatestExchangeRateHandler, which is the
// user's custom rates when the data source is user_custom) and prepares a converter to target
func anLoadConverter(mc *Ctx, target string) (*anConverter, error) {
	var resp models.LatestExchangeRateResponse

	if err := mc.CallUpstreamInto(api.ExchangeRates.LatestExchangeRateHandler, "GET", nil, nil, &resp); err != nil {
		if f := toFail(err); f != nil {
			f.Hint = "convert_to needs the app's exchange rates: check [exchange_rates] data_source in conf/ezbookkeeping.ini, or omit convert_to to get per-currency figures"
			return nil, f
		}

		return nil, err
	}

	customTimes := map[string]int64{}

	if resp.DataSource == anUserCustomDataSource {
		if customs, err := services.UserCustomExchangeRates.GetAllCustomExchangeRatesByUid(mc.Web, mc.Uid); err == nil {
			for _, c := range customs {
				customTimes[c.Currency] = c.UpdatedUnixTime
			}
		}
	}

	return anNewConverter(target, &resp, customTimes)
}

// anNewConverter is the pure half of anLoadConverter
func anNewConverter(target string, resp *models.LatestExchangeRateResponse, customTimes map[string]int64) (*anConverter, error) {
	cv := &anConverter{
		Target:  target,
		rates:   map[string]Rate{},
		info:    map[string]anRateInfo{},
		used:    map[string]bool{},
		missing: map[string]bool{},
	}

	source := "provider"

	if resp.DataSource == anUserCustomDataSource {
		source = "custom"
	}

	add := func(currency, rate string) {
		r, err := ParseRate(rate)

		if err != nil {
			errfile.Warn("parsing an exchange rate from the rates response", err, errfile.F("currency", currency))
			return
		}

		updated := resp.UpdateTime

		if t, ok := customTimes[currency]; ok && t > 0 {
			updated = t
		}

		cv.rates[currency] = r
		cv.info[currency] = anRateInfo{
			Currency:       currency,
			Rate:           strings.TrimSpace(rate),
			BaseCurrency:   resp.BaseCurrency,
			Source:         source,
			Provider:       resp.DataSource,
			UpdateTime:     time.Unix(updated, 0).UTC().Format(time.RFC3339),
			UpdateUnixTime: updated,
		}
	}

	for _, r := range resp.ExchangeRates {
		if r != nil {
			add(r.Currency, r.Rate)
		}
	}

	// the base currency is implicit in most providers' lists
	if _, ok := cv.rates[resp.BaseCurrency]; !ok && resp.BaseCurrency != "" {
		add(resp.BaseCurrency, "1")
	}

	return cv, nil
}

// Convert converts one amount (already summed in its own currency) to the target. ok is false when
// either side has no rate; that currency is then recorded as missing.
func (cv *anConverter) Convert(currency string, amount int64) (int64, bool) {
	if currency == cv.Target {
		return amount, true
	}

	from, okFrom := cv.rates[currency]
	to, okTo := cv.rates[cv.Target]

	if !okFrom || !okTo {
		cv.missing[currency] = true
		return 0, false
	}

	cv.used[currency] = true
	cv.used[cv.Target] = true

	if amount == 0 {
		return 0, true
	}

	return ConvertHundredths(amount, from, to), true
}

// CanConvert reports whether a currency has a usable rate (without recording anything)
func (cv *anConverter) CanConvert(currency string) bool {
	if currency == cv.Target {
		return true
	}

	_, okFrom := cv.rates[currency]
	_, okTo := cv.rates[cv.Target]

	return okFrom && okTo
}

// Provenance is the rates block of a response: every rate used, the basis and the note
func (cv *anConverter) Provenance() map[string]any {
	used := make([]string, 0, len(cv.used))

	for c := range cv.used {
		used = append(used, c)
	}

	sort.Strings(used)
	rates := make([]anRateInfo, 0, len(used))

	for _, c := range used {
		rates = append(rates, cv.info[c])
	}

	missing := make([]string, 0, len(cv.missing))

	for c := range cv.missing {
		missing = append(missing, c)
	}

	sort.Strings(missing)

	return map[string]any{
		"convertTo":         cv.Target,
		"rates":             rates,
		"rateBasis":         "latest",
		"note":              anRateNote,
		"missingCurrencies": missing,
	}
}

// Partial reports whether some currency could not be converted
func (cv *anConverter) Partial() bool {
	return len(cv.missing) > 0
}

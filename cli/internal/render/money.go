package render

import (
	"encoding/json"
	"strconv"
	"strings"
)

// money.go is the CLI's ONE conversion from integer hundredths to a decimal string. It is
// presentation only; --format json always carries the raw integer (cli.mdx §14.3).

var currencySymbols = map[string]string{
	"USD": "$", "EUR": "€", "GBP": "£", "JPY": "¥", "CNY": "¥", "INR": "₹", "KRW": "₩",
	"CAD": "CA$", "AUD": "A$", "CHF": "CHF ", "MXN": "MX$", "BRL": "R$", "HKD": "HK$",
}

// ToInt64 extracts an integer from a JSON value (json.Number, float64 or string)
func ToInt64(v any) (int64, bool) {
	switch t := v.(type) {
	case json.Number:
		n, err := t.Int64()
		return n, err == nil
	case float64:
		return int64(t), true
	case int64:
		return t, true
	case int:
		return int64(t), true
	case string:
		n, err := strconv.ParseInt(strings.TrimSpace(t), 10, 64)
		return n, err == nil
	}

	return 0, false
}

// Hundredths renders an integer of hundredths as "1,234.56" (grouped) or "1234.56" (plain)
func Hundredths(n int64, grouped bool) string {
	neg := n < 0

	if neg {
		n = -n
	}

	whole := strconv.FormatInt(n/100, 10)
	frac := n % 100

	if grouped && len(whole) > 3 {
		var b strings.Builder
		pre := len(whole) % 3

		if pre > 0 {
			b.WriteString(whole[:pre])
		}

		for i := pre; i < len(whole); i += 3 {
			if b.Len() > 0 {
				b.WriteByte(',')
			}

			b.WriteString(whole[i : i+3])
		}

		whole = b.String()
	}

	s := whole + "." + leftPad2(frac)

	if neg {
		s = "-" + s
	}

	return s
}

func leftPad2(n int64) string {
	if n < 10 {
		return "0" + strconv.FormatInt(n, 10)
	}

	return strconv.FormatInt(n, 10)
}

// Money renders hundredths with a currency for a human: "$1,234.56", "-€12.00", "1,234.56 XYZ"
func Money(n int64, currency string) string {
	s := Hundredths(n, true)
	sym, ok := currencySymbols[strings.ToUpper(currency)]

	if !ok {
		if currency == "" {
			return s
		}

		return s + " " + currency
	}

	if strings.HasPrefix(s, "-") {
		return "-" + sym + s[1:]
	}

	return sym + s
}

// ParseHundredthsArg parses a CLI amount argument. A decimal point is refused with the integer the
// operator probably meant (cli.mdx §7.5).
func ParseHundredthsArg(flag, v string) (int64, string) {
	v = strings.TrimSpace(strings.ReplaceAll(v, "_", ""))

	if strings.Contains(v, ".") {
		suggestion := ""

		if f, err := strconv.ParseFloat(v, 64); err == nil {
			if f < 0 {
				suggestion = strconv.FormatInt(int64(f*100-0.5), 10)
			} else {
				suggestion = strconv.FormatInt(int64(f*100+0.5), 10)
			}
		}

		msg := "amounts are integer hundredths (12.50 is 1250)"

		if suggestion != "" {
			msg += "; did you mean " + flag + " " + suggestion + "?"
		}

		return 0, msg
	}

	n, err := strconv.ParseInt(v, 10, 64)

	if err != nil {
		return 0, flag + " must be an integer of hundredths, got " + strconv.Quote(v)
	}

	if n > 999999999999999 || n < -999999999999999 {
		return 0, flag + " is outside ±999,999,999,999,999"
	}

	return n, ""
}

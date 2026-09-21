package machine

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math/big"
	"regexp"
	"strconv"
	"strings"
)

// money.go is the ONE place a big-integer string becomes an int64, and the one place an exchange
// rate is multiplied (apis.mdx §17.1). Amounts are integer hundredths at the app's fixed scale.

// MaxSafeInteger is JSON's largest exactly-representable integer (2^53-1)
const MaxSafeInteger int64 = 1<<53 - 1

// amountKeys are the response fields upstream carries as amounts (some as strings)
var amountKeys = map[string]bool{
	"amount": true, "balance": true, "sourceAmount": true, "destinationAmount": true,
	"incomeAmount": true, "expenseAmount": true, "outstandingBalance": true,
	"accountOpeningBalance": true, "accountClosingBalance": true,
	"openingBalance": true, "closingBalance": true, "totalInflows": true, "totalOutflows": true,
	"creditCardLimit": true, "totalAmount": true, "totalBalance": true,
}

var integerString = regexp.MustCompile(`^-?\d+$`)

// ParseAmountString turns upstream's integer string into hundredths, refusing anything outside
// JSON's safe range rather than rounding it
func ParseAmountString(s string) (int64, error) {
	s = strings.TrimSpace(s)

	if !integerString.MatchString(s) {
		return 0, fmt.Errorf("amount %q is not an integer of hundredths", s)
	}

	n, err := strconv.ParseInt(s, 10, 64)

	if err != nil || n > MaxSafeInteger || n < -MaxSafeInteger {
		return 0, NewFail(CodeUpstreamError, "narrow the range or filter", "an amount exceeds the safe integer range and was refused rather than rounded")
	}

	return n, nil
}

// Integerize re-encodes a value as generic JSON with every known amount field turned from an
// integer string into a JSON integer. Ids and everything else are left verbatim.
func Integerize(v any) (any, error) {
	data, err := json.Marshal(v)

	if err != nil {
		return nil, err
	}

	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()

	var generic any

	if err := dec.Decode(&generic); err != nil {
		return nil, err
	}

	return walkAmounts(generic, "")
}

func walkAmounts(v any, key string) (any, error) {
	switch t := v.(type) {
	case map[string]any:
		for k, child := range t {
			converted, err := walkAmounts(child, k)

			if err != nil {
				return nil, err
			}

			t[k] = converted
		}

		return t, nil
	case []any:
		for i, child := range t {
			converted, err := walkAmounts(child, key)

			if err != nil {
				return nil, err
			}

			t[i] = converted
		}

		return t, nil
	case string:
		if amountKeys[key] && integerString.MatchString(t) {
			n, err := ParseAmountString(t)

			if err != nil {
				return nil, err
			}

			return n, nil
		}

		return t, nil
	default:
		return v, nil
	}
}

// Rate is an exchange rate as a ratio (the one non-integer on the wire, §17.1)
type Rate struct {
	r *big.Rat
}

// ParseRate parses upstream's decimal rate string
func ParseRate(s string) (Rate, error) {
	r, ok := new(big.Rat).SetString(strings.TrimSpace(s))

	if !ok || r.Sign() <= 0 {
		return Rate{}, fmt.Errorf("rate %q is not a positive decimal", s)
	}

	return Rate{r: r}, nil
}

// ConvertHundredths converts an amount between two currencies given each currency's rate against
// the same base (upstream's convention: amountInBase = amount / rateToBase). Rounds once,
// half away from zero.
func ConvertHundredths(amount int64, fromRateToBase, toRateToBase Rate) int64 {
	x := new(big.Rat).SetInt64(amount)
	x.Quo(x, fromRateToBase.r)
	x.Mul(x, toRateToBase.r)

	return roundHalfAwayFromZero(x)
}

func roundHalfAwayFromZero(x *big.Rat) int64 {
	num := new(big.Int).Set(x.Num())
	den := new(big.Int).Set(x.Denom())
	neg := num.Sign() < 0

	if neg {
		num.Neg(num)
	}

	q, rem := new(big.Int).QuoRem(num, den, new(big.Int))
	rem.Mul(rem, big.NewInt(2))

	if rem.Cmp(den) >= 0 {
		q.Add(q, big.NewInt(1))
	}

	if neg {
		q.Neg(q)
	}

	return q.Int64()
}

// RateString renders a rate for provenance
func (r Rate) String() string {
	if r.r == nil {
		return ""
	}

	return r.r.FloatString(8)
}

// CheckedAdd adds two amounts, refusing to leave the safe range
func CheckedAdd(a, b int64) (int64, error) {
	s := a + b

	if s > MaxSafeInteger || s < -MaxSafeInteger {
		return 0, NewFail(CodeUpstreamError, "narrow the range or filter", "a total exceeds the safe integer range and was refused rather than rounded")
	}

	return s, nil
}

// AmountArg validates an amount argument: an integer of hundredths
func AmountArg(name string, v json.Number) (int64, error) {
	s := v.String()

	if strings.ContainsAny(s, ".eE") {
		return 0, Invalid("amounts are integer hundredths: 12.50 is 1250", "%s %s is not an integer of hundredths", name, s)
	}

	n, err := strconv.ParseInt(s, 10, 64)

	if err != nil || n > 999999999999999 || n < -999999999999999 {
		return 0, Invalid("amounts are integer hundredths within ±999,999,999,999,999", "%s %s is out of range", name, s)
	}

	return n, nil
}

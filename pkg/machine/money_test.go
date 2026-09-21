package machine

import (
	"encoding/json"
	"strings"
	"testing"
)

// money_test.go — integer hundredths, adversarial values, one rounding (apis.mdx §17.1, §21 "Money")

func jrIntegerizeJSON(t *testing.T, in string) (string, error) {
	t.Helper()
	var v any

	if err := json.Unmarshal([]byte(in), &v); err != nil {
		t.Fatal(err)
	}

	out, err := Integerize(v)

	if err != nil {
		return "", err
	}

	data, _ := json.Marshal(out)

	return string(data), nil
}

func TestMoneyIntegerizeConvertsAmountKeysOnly(t *testing.T) {
	got, err := jrIntegerizeJSON(t, `{
		"accounts": [{"id": "9007199254740993", "balance": "123456", "name": "12345", "currency": "USD"}],
		"amount": "-50",
		"sourceAmount": "007",
		"totalAmount": "0",
		"comment": "999",
		"nested": {"deeper": [{"expenseAmount": "1", "categoryId": "3401855937219633152"}]}
	}`)

	if err != nil {
		t.Fatal(err)
	}

	for _, want := range []string{`"balance":123456`, `"amount":-50`, `"sourceAmount":7`, `"totalAmount":0`, `"expenseAmount":1`,
		`"id":"9007199254740993"`, `"name":"12345"`, `"comment":"999"`, `"categoryId":"3401855937219633152"`} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %s in %s", want, got)
		}
	}
}

func TestMoneyIntegerizeAdversarial(t *testing.T) {
	// values that are not integer strings stay as they are (never parsed as floats)
	got, err := jrIntegerizeJSON(t, `{"items":[{"amount":"12.50"},{"amount":"1e3"},{"amount":"+5"},{"amount":" 5"},{"amount":""},{"amount":"0x10"},{"amount":"١٢٣"}]}`)

	if err != nil {
		t.Fatal(err)
	}

	for _, want := range []string{`"12.50"`, `"1e3"`, `"+5"`, `" 5"`, `""`, `"0x10"`} {
		if !strings.Contains(got, want) {
			t.Errorf("%s was altered: %s", want, got)
		}
	}

	// JSON numbers already integers pass through exactly, even large ones
	got, err = jrIntegerizeJSON(t, `{"amount": 9007199254740991}`)

	if err != nil || !strings.Contains(got, "9007199254740991") {
		t.Errorf("large number altered: %s %v", got, err)
	}

	// beyond 2^53-1 is refused, never rounded
	for _, bad := range []string{`{"amount":"9007199254740992"}`, `{"balance":"-9007199254740992"}`, `{"amount":"99999999999999999999999"}`} {
		if _, err := jrIntegerizeJSON(t, bad); err == nil {
			t.Errorf("%s must be refused", bad)
		}
	}

	if got, err := jrIntegerizeJSON(t, `{"amount":"-9007199254740991"}`); err != nil || !strings.Contains(got, "-9007199254740991") {
		t.Errorf("the safe edge must pass: %s %v", got, err)
	}
}

func TestMoneyIntegerizeTypedStructs(t *testing.T) {
	type row struct {
		Id      int64 `json:"id,string"`
		Balance int64 `json:"balance,string"`
	}

	out, err := Integerize(map[string]any{"accounts": []row{{Id: 1 << 60, Balance: -250}}})

	if err != nil {
		t.Fatal(err)
	}

	data, _ := json.Marshal(out)

	if !strings.Contains(string(data), `"balance":-250`) || !strings.Contains(string(data), `"id":"1152921504606846976"`) {
		t.Fatalf("got %s", data)
	}
}

func TestMoneyParseAmountString(t *testing.T) {
	for in, want := range map[string]int64{"0": 0, "-1": -1, "1250": 1250, " 42 ": 42, "000": 0} {
		got, err := ParseAmountString(in)

		if err != nil || got != want {
			t.Errorf("ParseAmountString(%q) = %d, %v", in, got, err)
		}
	}

	for _, bad := range []string{"", "1.5", "abc", "9007199254740992", "1e2", "--1"} {
		if _, err := ParseAmountString(bad); err == nil {
			t.Errorf("ParseAmountString(%q) must fail", bad)
		}
	}
}

func jrRate(t *testing.T, s string) Rate {
	t.Helper()
	r, err := ParseRate(s)

	if err != nil {
		t.Fatal(err)
	}

	return r
}

func TestMoneyConvertHundredthsRoundsOnceHalfAwayFromZero(t *testing.T) {
	one := jrRate(t, "1")
	half := jrRate(t, "0.5")
	two := jrRate(t, "2")

	cases := []struct {
		amount   int64
		from, to Rate
		want     int64
	}{
		{1, one, half, 1},   // 0.5 → 1
		{-1, one, half, -1}, // -0.5 → -1
		{3, two, one, 2},    // 1.5 → 2
		{-3, two, one, -2},  // -1.5 → -2
		{5, two, one, 3},    // 2.5 → 3
		{1, two, one, 1},    // 0.5 → 1
		{4, two, one, 2},    // exact
		{10000, jrRate(t, "1"), jrRate(t, "0.92"), 9200},
		{12345, jrRate(t, "1.0800"), jrRate(t, "1"), 11431}, // 11430.555… → 11431
		{0, two, half, 0},
	}

	for _, c := range cases {
		if got := ConvertHundredths(c.amount, c.from, c.to); got != c.want {
			t.Errorf("ConvertHundredths(%d, %s, %s) = %d, want %d", c.amount, c.from, c.to, got, c.want)
		}
	}

	// a thousand conversions of 1 at a rate of 1/3 do not drift: each is rounded once, on its own
	third := jrRate(t, "3")

	for i := 0; i < 1000; i++ {
		if ConvertHundredths(1, third, one) != 0 {
			t.Fatal("1/3 of a hundredth must round to 0")
		}

		if ConvertHundredths(2, third, one) != 1 {
			t.Fatal("2/3 of a hundredth must round to 1")
		}
	}
}

func TestMoneyParseRate(t *testing.T) {
	for _, bad := range []string{"0", "-1", "abc", "", "1/0"} {
		if _, err := ParseRate(bad); err == nil {
			t.Errorf("ParseRate(%q) must fail", bad)
		}
	}

	if r := jrRate(t, "0.123456789"); r.String() != "0.12345679" {
		t.Errorf("rate string = %s", r.String())
	}

	if (Rate{}).String() != "" {
		t.Error("zero rate renders empty")
	}
}

func TestMoneyCheckedAdd(t *testing.T) {
	if s, err := CheckedAdd(MaxSafeInteger-1, 1); err != nil || s != MaxSafeInteger {
		t.Fatalf("edge add: %d %v", s, err)
	}

	if _, err := CheckedAdd(MaxSafeInteger, 1); err == nil {
		t.Fatal("a sum beyond 2^53-1 must be refused")
	}

	if _, err := CheckedAdd(-MaxSafeInteger, -1); err == nil {
		t.Fatal("a sum below -(2^53-1) must be refused")
	}
}

func TestMoneyAmountArg(t *testing.T) {
	good := map[string]int64{"1250": 1250, "-1": -1, "0": 0, "999999999999999": 999999999999999}

	for in, want := range good {
		got, err := AmountArg("amount", json.Number(in))

		if err != nil || got != want {
			t.Errorf("AmountArg(%s) = %d %v", in, got, err)
		}
	}

	for _, bad := range []string{"12.5", "12.50", "1e3", "1E3", "1000000000000000", "-1000000000000000", "abc", ""} {
		_, err := AmountArg("amount", json.Number(bad))

		if err == nil {
			t.Errorf("AmountArg(%q) must be refused", bad)
			continue
		}

		if f := toFail(err); f.Code != CodeInvalidInput || f.Hint == "" {
			t.Errorf("AmountArg(%q) → %+v", bad, f)
		}
	}
}

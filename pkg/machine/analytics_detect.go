package machine

import (
	"math/big"
	"sort"
	"strings"
	"time"
	"unicode"
)

// analytics_detect.go — the pure logic behind the detectors and ratios: payee normalisation,
// recurring-charge detection, the anomaly test and the runway ratio. Detectors are not oracles
// (apis.mdx §12.3): every function here returns its evidence alongside its verdict. There is no
// float anywhere — ratios are computed with math/big and rendered as decimal strings.

// anPayeeNoise are descriptor tokens that say how a card was used, not who was paid
var anPayeeNoise = map[string]bool{
	"POS": true, "DEBIT": true, "PURCHASE": true, "CHECKCARD": true, "CARD": true, "VISA": true,
	"MC": true, "SQ": true, "TST": true, "ACH": true, "WWW": true, "COM": true, "RECURRING": true,
}

// anNormalizePayee turns a transaction comment into the key the leaderboard groups by: upper-case,
// punctuation to spaces, reference numbers and card-network noise dropped, whitespace collapsed.
// Deterministic; the raw variants a key merged are always returned beside it.
func anNormalizePayee(s string) string {
	var b strings.Builder

	for _, r := range strings.ToUpper(s) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
		} else {
			b.WriteRune(' ')
		}
	}

	var kept []string

	for _, tok := range strings.Fields(b.String()) {
		if anPayeeNoise[tok] {
			continue
		}

		digits := 0

		for _, r := range tok {
			if unicode.IsDigit(r) {
				digits++
			}
		}

		// a pure number, or a token that is mostly digits (store numbers, references, dates)
		if digits == len([]rune(tok)) || (digits >= 3 && digits*2 >= len([]rune(tok))) {
			continue
		}

		kept = append(kept, tok)
	}

	return strings.Join(kept, " ")
}

// anMedian is the median of integer amounts; an even count takes the mean of the middle two,
// rounded half away from zero (a median of money is money, so it is integer hundredths)
func anMedian(values []int64) int64 {
	if len(values) == 0 {
		return 0
	}

	sorted := append([]int64(nil), values...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	n := len(sorted)

	if n%2 == 1 {
		return sorted[n/2]
	}

	return anRoundRat(new(big.Rat).SetFrac(big.NewInt(sorted[n/2-1]+sorted[n/2]), big.NewInt(2)))
}

// anMeanRounded is the mean of integer amounts, rounded half away from zero
func anMeanRounded(sum int64, n int64) int64 {
	if n == 0 {
		return 0
	}

	return anRoundRat(new(big.Rat).SetFrac(big.NewInt(sum), big.NewInt(n)))
}

// anRoundRat rounds a ratio half away from zero to an integer
func anRoundRat(x *big.Rat) int64 {
	return roundHalfAwayFromZero(x)
}

// anRatString renders a ratio with `places` decimals, truncated toward zero (never rounded up: a
// runway of 2.99 months is not 3)
func anRatString(x *big.Rat, places int) string {
	scale := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(places)), nil)
	num := new(big.Int).Mul(x.Num(), scale)
	q := new(big.Int).Quo(num, x.Denom()) // truncates toward zero
	neg := q.Sign() < 0

	if neg {
		q.Neg(q)
	}

	s := q.String()

	for len(s) <= places {
		s = "0" + s
	}

	out := s

	if places > 0 {
		out = s[:len(s)-places] + "." + s[len(s)-places:]
	}

	if neg && strings.Trim(out, "0.") != "" {
		out = "-" + out
	}

	return out
}

// Cadences the recurring detector knows, with their nominal gap and yearly frequency
type anCadence struct {
	Name    string
	MinDays int
	MaxDays int
	PerYear int64
}

var anCadences = []anCadence{
	{Name: "weekly", MinDays: 7, MaxDays: 7, PerYear: 52},
	{Name: "biweekly", MinDays: 14, MaxDays: 14, PerYear: 26},
	{Name: "monthly", MinDays: 28, MaxDays: 31, PerYear: 12},
	{Name: "quarterly", MinDays: 89, MaxDays: 92, PerYear: 4},
	{Name: "semiannual", MinDays: 181, MaxDays: 184, PerYear: 2},
	{Name: "yearly", MinDays: 365, MaxDays: 366, PerYear: 1},
}

// anNextOccurrence is the date the cadence expects after last
func anNextOccurrence(c anCadence, last time.Time) time.Time {
	switch c.Name {
	case "monthly":
		return last.AddDate(0, 1, 0)
	case "quarterly":
		return last.AddDate(0, 3, 0)
	case "semiannual":
		return last.AddDate(0, 6, 0)
	case "yearly":
		return last.AddDate(1, 0, 0)
	default:
		return last.AddDate(0, 0, c.MinDays)
	}
}

// anOccurrence is one transaction in a candidate recurring group
type anOccurrence struct {
	Id     string    `json:"id"`
	Date   string    `json:"date"`
	Amount int64     `json:"amount"`
	day    time.Time // midnight in the call's zone
}

// anCadenceFit is the verdict of the recurring detector on one group
type anCadenceFit struct {
	Cadence    anCadence
	Matches    int   // gaps that fit the cadence
	Gaps       int   // gaps examined
	GapDays    []int // every gap, in order (the evidence)
	Consistent bool
}

// anDayDiff is the whole number of calendar days from a to b
func anDayDiff(a, b time.Time) int {
	ay, am, ad := a.Date()
	by, bm, bd := b.Date()
	ua := time.Date(ay, am, ad, 0, 0, 0, 0, time.UTC)
	ub := time.Date(by, bm, bd, 0, 0, 0, 0, time.UTC)

	return int(ub.Sub(ua) / (24 * time.Hour))
}

// anFitCadence finds the cadence most of the gaps between consecutive occurrences agree on. At least
// three quarters of the gaps must fit (within tolerance days) for the group to count as recurring;
// same-day repeats are folded first (two charges on one day are one occurrence of the cadence).
func anFitCadence(days []time.Time, tolerance int) anCadenceFit {
	var uniq []time.Time

	for _, d := range days {
		if len(uniq) > 0 && anDayDiff(uniq[len(uniq)-1], d) == 0 {
			continue
		}

		uniq = append(uniq, d)
	}

	fit := anCadenceFit{}

	if len(uniq) < 2 {
		return fit
	}

	gaps := make([]int, 0, len(uniq)-1)

	for i := 1; i < len(uniq); i++ {
		gaps = append(gaps, anDayDiff(uniq[i-1], uniq[i]))
	}

	fit.GapDays = gaps
	fit.Gaps = len(gaps)

	for _, c := range anCadences {
		matches := 0

		for _, g := range gaps {
			if g >= c.MinDays-tolerance && g <= c.MaxDays+tolerance {
				matches++
			}
		}

		if matches > fit.Matches {
			fit.Matches = matches
			fit.Cadence = c
		}
	}

	fit.Consistent = fit.Matches > 0 && fit.Matches*4 >= fit.Gaps*3

	return fit
}

// anAnomalyStats is the trailing-norm evidence for one value
type anAnomalyStats struct {
	N         int64
	Sum       int64
	SumSq     *big.Int
	Mean      int64  // rounded half away from zero
	StdDev    int64  // population standard deviation, floor, integer hundredths
	Deviation int64  // value − mean (rounded mean)
	ZScore    string // two decimals, truncated; empty when the history has no variance
	Flagged   bool
	NoVar     bool
}

// anAnomalyTest decides whether value is more than z standard deviations from the mean of history,
// entirely in integer arithmetic: |n·x − S| > z·sqrt(n·SS − S²)  ⟺  (n·x − S)² > z²·(n·SS − S²).
// A history with no variance flags any different value (zScore empty, NoVar true). minAmount
// ignores deviations smaller than it (in hundredths).
func anAnomalyTest(history []int64, value int64, z *big.Rat, minAmount int64) anAnomalyStats {
	st := anAnomalyStats{N: int64(len(history)), SumSq: new(big.Int)}

	for _, v := range history {
		st.Sum += v
		sq := new(big.Int).Mul(big.NewInt(v), big.NewInt(v))
		st.SumSq.Add(st.SumSq, sq)
	}

	if st.N == 0 {
		return st
	}

	st.Mean = anMeanRounded(st.Sum, st.N)
	st.Deviation = value - st.Mean

	n := big.NewInt(st.N)
	s := big.NewInt(st.Sum)

	// variance numerator D = n·SS − S²  (variance = D / n²)
	d := new(big.Int).Mul(n, st.SumSq)
	d.Sub(d, new(big.Int).Mul(s, s))

	if d.Sign() < 0 {
		d.SetInt64(0)
	}

	// stddev = sqrt(D) / n
	st.StdDev = new(big.Int).Quo(new(big.Int).Sqrt(d), n).Int64()

	// e = n·x − S
	e := new(big.Int).Mul(n, big.NewInt(value))
	e.Sub(e, s)

	if anAbs(st.Deviation) < minAmount {
		return st
	}

	if d.Sign() == 0 {
		st.NoVar = true
		st.Flagged = e.Sign() != 0

		return st
	}

	// zScore = e / sqrt(D), to two decimals: scale sqrt by 10^4 (sqrt(D·10^8)) and e by 10^6
	root := new(big.Int).Sqrt(new(big.Int).Mul(d, big.NewInt(100000000)))

	if root.Sign() > 0 {
		zr := new(big.Rat).SetFrac(new(big.Int).Mul(e, big.NewInt(10000)), root)
		st.ZScore = anRatString(zr, 2)
	}

	// flagged when e²·den² > num²·D, with z = num/den
	lhs := new(big.Int).Mul(e, e)
	lhs.Mul(lhs, new(big.Int).Mul(z.Denom(), z.Denom()))
	rhs := new(big.Int).Mul(z.Num(), z.Num())
	rhs.Mul(rhs, d)
	st.Flagged = lhs.Cmp(rhs) > 0

	return st
}

// anRunway is the runway ratio of one currency (or the converted whole)
type anRunway struct {
	Status          string `json:"status"`
	Months          string `json:"months"`
	WholeMonths     *int64 `json:"wholeMonths"`
	Days            *int64 `json:"days"`
	AverageNetBurn  *int64 `json:"averageMonthlyNetOutflow"`
	NetOutflowTotal int64  `json:"netOutflow"`
}

// anComputeRunway divides liquid assets by the trailing average net outflow. netOutflow is expense
// minus income over the basis (positive means money is leaving); basisMonths and basisDays describe
// the window. Absent is not zero: a book that is not burning money has no runway figure (null).
func anComputeRunway(liquid, netOutflow, basisMonths, basisDays int64) anRunway {
	r := anRunway{NetOutflowTotal: netOutflow}

	if basisMonths <= 0 {
		r.Status = "no_history"
		return r
	}

	avg := anMeanRounded(netOutflow, basisMonths)
	r.AverageNetBurn = &avg

	if netOutflow <= 0 {
		r.Status = "not_burning"
		return r
	}

	if liquid <= 0 {
		zero := int64(0)
		r.Status = "no_liquid_assets"
		r.Months = "0.0"
		r.WholeMonths = &zero
		r.Days = &zero

		return r
	}

	// months = liquid / (netOutflow / basisMonths) = liquid·basisMonths / netOutflow
	months := new(big.Rat).SetFrac(new(big.Int).Mul(big.NewInt(liquid), big.NewInt(basisMonths)), big.NewInt(netOutflow))
	r.Months = anRatString(months, 1)
	whole := new(big.Int).Quo(months.Num(), months.Denom()).Int64()
	r.WholeMonths = &whole

	if basisDays > 0 {
		days := new(big.Int).Quo(new(big.Int).Mul(big.NewInt(liquid), big.NewInt(basisDays)), big.NewInt(netOutflow)).Int64()
		r.Days = &days
	}

	r.Status = "burning"

	return r
}

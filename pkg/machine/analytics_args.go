package machine

import (
	"fmt"
	"math/big"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/mayswind/ezbookkeeping/pkg/models"
	"github.com/mayswind/ezbookkeeping/pkg/validators"
)

// analytics_args.go — the arguments every /analytics/* route shares (apis.mdx §12.2), and the ONE
// bucketer that turns a date into a period key (day, week, month, quarter, year or the whole range).

// Intervals the analytics plane understands
const (
	anIntervalNone    = "none"
	anIntervalDay     = "day"
	anIntervalWeek    = "week"
	anIntervalMonth   = "month"
	anIntervalQuarter = "quarter"
	anIntervalYear    = "year"
)

// anCommonArgNames are the query arguments every analytics route accepts (§12.2)
var anCommonArgNames = []string{
	"start", "end", "account_ids", "category_ids", "exclude_category_ids", "tag_filter",
	"include_hidden_accounts", "include_transfers", "convert_to", "use_transaction_timezone",
}

// anCheckQuery rejects unknown query arguments: a typo'd `start_date` that silently means "no
// filter" is how someone misreads a decade (apis.mdx §7.7)
func anCheckQuery(mc *Ctx, extra ...string) error {
	allowed := map[string]bool{}

	for _, n := range anCommonArgNames {
		allowed[n] = true
	}

	for _, n := range extra {
		allowed[n] = true
	}

	var unknown []string

	for k := range mc.Gin.Request.URL.Query() {
		name := strings.TrimSuffix(k, "[]")

		if !allowed[name] {
			unknown = append(unknown, k)
		}
	}

	if len(unknown) == 0 {
		return nil
	}

	sort.Strings(unknown)
	names := make([]string, 0, len(allowed))

	for n := range allowed {
		names = append(names, n)
	}

	sort.Strings(names)

	return Invalid("this route accepts: "+strings.Join(names, ", "), "unknown argument %s", strings.Join(unknown, ", ")).
		WithDetails(map[string]any{"unknown": unknown, "accepted": names})
}

// anArgs are the resolved shared arguments of one analytics call
type anArgs struct {
	Range                  *DateRange
	AccountIds             []int64
	CategoryIds            []int64
	ExcludeCategoryIds     []int64
	TagFilter              string
	IncludeHiddenAccounts  bool
	IncludeTransfers       bool
	ConvertTo              string
	UseTransactionTimezone bool
}

// anParseArgs reads the shared arguments. defStart/defEnd are the route's defaults (YYYY-MM-DD);
// an empty default makes that end of the range required.
func anParseArgs(mc *Ctx, defStart, defEnd string) (*anArgs, error) {
	a := &anArgs{}
	var err error

	a.Range, err = ParseDateRange(mc.Query("start"), mc.Query("end"), defStart, defEnd, mc.Loc)

	if err != nil {
		return nil, err
	}

	if a.AccountIds, err = anParseIdList(mc, "account_ids"); err != nil {
		return nil, err
	}

	if a.CategoryIds, err = anParseIdList(mc, "category_ids"); err != nil {
		return nil, err
	}

	if a.ExcludeCategoryIds, err = anParseIdList(mc, "exclude_category_ids"); err != nil {
		return nil, err
	}

	a.TagFilter = mc.Query("tag_filter")

	if a.TagFilter != "" && a.TagFilter != models.TransactionNoTagFilterValue {
		if _, perr := models.ParseTransactionTagFilter(a.TagFilter); perr != nil {
			return nil, Invalid("tag_filter is upstream's syntax: `<mode>:<tagId>,<tagId>` joined by `;` (mode 0 has any, 1 has all, 2 not has any, 3 not has all), or `none`", "tag_filter %q is not a valid tag filter", a.TagFilter)
		}
	}

	if a.IncludeHiddenAccounts, err = mc.QueryBool("include_hidden_accounts", false); err != nil {
		return nil, err
	}

	if a.IncludeTransfers, err = mc.QueryBool("include_transfers", false); err != nil {
		return nil, err
	}

	if a.UseTransactionTimezone, err = mc.QueryBool("use_transaction_timezone", false); err != nil {
		return nil, err
	}

	if a.ConvertTo, err = anParseCurrency("convert_to", mc.Query("convert_to")); err != nil {
		return nil, err
	}

	return a, nil
}

// anParseCurrency validates an ISO 4217 code (empty is allowed and means "no conversion")
func anParseCurrency(name, v string) (string, error) {
	v = strings.ToUpper(strings.TrimSpace(v))

	if v == "" {
		return "", nil
	}

	if _, ok := validators.AllCurrencyNames[v]; !ok || len(v) != 3 {
		return "", Invalid("pass an ISO 4217 code the app knows, e.g. "+name+"=USD", "%s %q is not a currency ezBookkeeping supports", name, v)
	}

	return v, nil
}

// anParseIdList parses a list of ezBookkeeping ids (decimal strings)
func anParseIdList(mc *Ctx, name string) ([]int64, error) {
	raw := mc.QueryList(name)

	if len(raw) == 0 {
		return nil, nil
	}

	out := make([]int64, 0, len(raw))
	seen := map[int64]bool{}

	for _, v := range raw {
		id, err := ResolveId(name, v)

		if err != nil {
			return nil, err
		}

		if !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}

	return out, nil
}

// anParseEnum reads an enumerated argument
func anParseEnum(mc *Ctx, name, def string, allowed ...string) (string, error) {
	v := strings.ToLower(mc.Query(name))

	if v == "" {
		return def, nil
	}

	for _, a := range allowed {
		if v == a {
			return v, nil
		}
	}

	return "", Invalid("pass "+name+" as one of: "+strings.Join(allowed, ", "), "%s %q is not one of %s", name, v, strings.Join(allowed, "|"))
}

// anParseIntArg reads an integer argument bounded to [min, max]
func anParseIntArg(mc *Ctx, name string, def, min, max int64) (int64, error) {
	n, err := mc.QueryInt(name, def)

	if err != nil {
		return 0, err
	}

	if n < min || n > max {
		return 0, Invalid(fmt.Sprintf("pass %s between %d and %d", name, min, max), "%s %d is out of range", name, n)
	}

	return n, nil
}

// anParseRatArg reads a positive decimal argument (a ratio such as a z threshold — never money)
func anParseRatArg(mc *Ctx, name, def string) (*big.Rat, string, error) {
	v := mc.Query(name)

	if v == "" {
		v = def
	}

	r, ok := new(big.Rat).SetString(v)

	if !ok || r.Sign() <= 0 {
		return nil, "", Invalid("pass "+name+" as a positive decimal, e.g. "+name+"="+def, "%s %q is not a positive decimal", name, v)
	}

	if r.Cmp(big.NewRat(100, 1)) > 0 {
		return nil, "", Invalid("pass "+name+" of 100 or less", "%s %q is out of range", name, v)
	}

	return r, v, nil
}

// anAmountQuery reads an amount argument (integer hundredths) from the query string
func anAmountQuery(mc *Ctx, name string, def int64) (int64, error) {
	v := mc.Query(name)

	if v == "" {
		return def, nil
	}

	if strings.ContainsAny(v, ".eE") {
		return 0, Invalid("amounts are integer hundredths: 12.50 is 1250", "%s %s is not an integer of hundredths", name, v)
	}

	n, err := strconv.ParseInt(v, 10, 64)

	if err != nil || n > 999999999999999 || n < -999999999999999 {
		return 0, Invalid("amounts are integer hundredths within ±999,999,999,999,999", "%s %s is out of range", name, v)
	}

	return n, nil
}

// anToday is today's date in loc
func anToday(loc *time.Location) time.Time {
	now := time.Now().In(loc)

	return time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, loc)
}

func anDateString(t time.Time) string {
	return t.Format("2006-01-02")
}

// anDefaultYearToDate is the default range of most analytics routes: 1 January of this year to today
func anDefaultYearToDate(loc *time.Location) (string, string) {
	today := anToday(loc)

	return anDateString(time.Date(today.Year(), 1, 1, 0, 0, 0, 0, loc)), anDateString(today)
}

// anDefaultTrailingMonths is the first day of the month `months-1` months before today, to today
func anDefaultTrailingMonths(loc *time.Location, months int) (string, string) {
	today := anToday(loc)
	start := time.Date(today.Year(), today.Month(), 1, 0, 0, 0, 0, loc).AddDate(0, -(months - 1), 0)

	return anDateString(start), anDateString(today)
}

// anDayStart parses a resolved range bound back into a time at midnight in loc
func anDayStart(date string, loc *time.Location) time.Time {
	t, _ := time.ParseInLocation("2006-01-02", date, loc)

	return t
}

// anDayUnixRange is [00:00:00, 23:59:59] of one day as unix seconds
func anDayUnixRange(day time.Time) (int64, int64) {
	return day.Unix(), day.AddDate(0, 0, 1).Add(-time.Second).Unix()
}

// anYMD is a numeric year-month-day (upstream's convention, e.g. 20260921)
func anYMD(t time.Time) int32 {
	return int32(t.Year())*10000 + int32(t.Month())*100 + int32(t.Day())
}

// anYMDTime turns a numeric year-month-day back into midnight in loc
func anYMDTime(ymd int32, loc *time.Location) time.Time {
	return time.Date(int(ymd/10000), time.Month((ymd%10000)/100), int(ymd%100), 0, 0, 0, 0, loc)
}

// anBucketer maps dates to period keys for one interval
type anBucketer struct {
	Interval       string
	Loc            *time.Location
	FirstDayOfWeek time.Weekday
	RangeStart     time.Time
	RangeEnd       time.Time
}

// anPeriod is one bucket of a series, clipped to the requested range
type anPeriod struct {
	Period string `json:"period"`
	Start  string `json:"start"`
	End    string `json:"end"`
}

func newAnBucketer(interval string, rng *DateRange, loc *time.Location, firstDayOfWeek time.Weekday) *anBucketer {
	return &anBucketer{
		Interval:       interval,
		Loc:            loc,
		FirstDayOfWeek: firstDayOfWeek,
		RangeStart:     anDayStart(rng.Start, loc),
		RangeEnd:       anDayStart(rng.End, loc),
	}
}

// rangeKey is the single period key of interval none
func (b *anBucketer) rangeKey() string {
	return anDateString(b.RangeStart) + ".." + anDateString(b.RangeEnd)
}

// bucketStart is the first day of the (unclipped) bucket containing day
func (b *anBucketer) bucketStart(day time.Time) time.Time {
	day = time.Date(day.Year(), day.Month(), day.Day(), 0, 0, 0, 0, b.Loc)

	switch b.Interval {
	case anIntervalDay:
		return day
	case anIntervalWeek:
		back := (int(day.Weekday()) - int(b.FirstDayOfWeek) + 7) % 7
		return day.AddDate(0, 0, -back)
	case anIntervalMonth:
		return time.Date(day.Year(), day.Month(), 1, 0, 0, 0, 0, b.Loc)
	case anIntervalQuarter:
		q := (int(day.Month()) - 1) / 3
		return time.Date(day.Year(), time.Month(q*3+1), 1, 0, 0, 0, 0, b.Loc)
	case anIntervalYear:
		return time.Date(day.Year(), 1, 1, 0, 0, 0, 0, b.Loc)
	default:
		return b.RangeStart
	}
}

// nextBucketStart is the first day after the bucket starting at start
func (b *anBucketer) nextBucketStart(start time.Time) time.Time {
	switch b.Interval {
	case anIntervalDay:
		return start.AddDate(0, 0, 1)
	case anIntervalWeek:
		return start.AddDate(0, 0, 7)
	case anIntervalMonth:
		return start.AddDate(0, 1, 0)
	case anIntervalQuarter:
		return start.AddDate(0, 3, 0)
	case anIntervalYear:
		return start.AddDate(1, 0, 0)
	default:
		return b.RangeEnd.AddDate(0, 0, 1)
	}
}

// keyOfStart renders the key of the bucket starting at start
func (b *anBucketer) keyOfStart(start time.Time) string {
	switch b.Interval {
	case anIntervalDay, anIntervalWeek:
		return anDateString(start)
	case anIntervalMonth:
		return start.Format("2006-01")
	case anIntervalQuarter:
		return fmt.Sprintf("%d-Q%d", start.Year(), (int(start.Month())-1)/3+1)
	case anIntervalYear:
		return strconv.Itoa(start.Year())
	default:
		return b.rangeKey()
	}
}

// Key is the period key of the bucket holding day
func (b *anBucketer) Key(day time.Time) string {
	if b.Interval == anIntervalNone || b.Interval == "" {
		return b.rangeKey()
	}

	return b.keyOfStart(b.bucketStart(day))
}

// KeyOfYMD is Key for upstream's numeric year-month-day
func (b *anBucketer) KeyOfYMD(ymd int32) string {
	return b.Key(anYMDTime(ymd, b.Loc))
}

// Periods lists every bucket of the range in order, clipped to it
func (b *anBucketer) Periods() []anPeriod {
	if b.Interval == anIntervalNone || b.Interval == "" {
		return []anPeriod{{Period: b.rangeKey(), Start: anDateString(b.RangeStart), End: anDateString(b.RangeEnd)}}
	}

	var out []anPeriod

	for s := b.bucketStart(b.RangeStart); !s.After(b.RangeEnd); s = b.nextBucketStart(s) {
		start := s

		if start.Before(b.RangeStart) {
			start = b.RangeStart
		}

		end := b.nextBucketStart(s).AddDate(0, 0, -1)

		if end.After(b.RangeEnd) {
			end = b.RangeEnd
		}

		out = append(out, anPeriod{Period: b.keyOfStart(s), Start: anDateString(start), End: anDateString(end)})
	}

	return out
}

// anWeekday converts upstream's first-day-of-week setting
func anWeekday(mc *Ctx) time.Weekday {
	if mc.User == nil {
		return time.Sunday
	}

	return time.Weekday(int(mc.User.FirstDayOfWeek) % 7)
}

// anMonthSegment is one calendar month of a range, clipped to it
type anMonthSegment struct {
	Month time.Time // first day of the month
	Start time.Time // clipped start
	End   time.Time // clipped end (inclusive day)
	Full  bool      // the whole month is inside the range
}

// anMonthSegments splits a range into calendar months; only the first and last can be partial
func anMonthSegments(start, end time.Time, loc *time.Location) []anMonthSegment {
	var out []anMonthSegment

	for m := time.Date(start.Year(), start.Month(), 1, 0, 0, 0, 0, loc); !m.After(end); m = m.AddDate(0, 1, 0) {
		last := m.AddDate(0, 1, -1)
		s, e := m, last

		if s.Before(start) {
			s = start
		}

		if e.After(end) {
			e = end
		}

		out = append(out, anMonthSegment{Month: m, Start: s, End: e, Full: s.Equal(m) && e.Equal(last)})
	}

	return out
}

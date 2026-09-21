package machine

import (
	"testing"
	"time"
)

// clock_test.go — dates, inclusive ranges, and whose midnight it is (apis.mdx §17.4, §21 "Time")

func jrLoc(t *testing.T, name string) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation(name)

	if err != nil {
		t.Skipf("tzdata for %s unavailable: %v", name, err)
	}

	return loc
}

func TestClockParseDateRangeInclusive(t *testing.T) {
	la := jrLoc(t, "America/Los_Angeles")
	r, err := ParseDateRange("2026-03-01", "2026-03-31", "", "", la)

	if err != nil {
		t.Fatal(err)
	}

	start := time.Date(2026, 3, 1, 0, 0, 0, 0, la)
	end := time.Date(2026, 3, 31, 23, 59, 59, 0, la) // crosses the March DST change

	if r.StartUnix != start.Unix() || r.EndUnix != end.Unix() {
		t.Fatalf("range = %d..%d, want %d..%d", r.StartUnix, r.EndUnix, start.Unix(), end.Unix())
	}

	if r.Start != "2026-03-01" || r.End != "2026-03-31" || r.Timezone != "America/Los_Angeles" {
		t.Fatalf("echo = %+v", r)
	}

	// one day is a whole day, inclusive on both ends
	d, err := ParseDateRange("2026-09-21", "2026-09-21", "", "", la)

	if err != nil || d.EndUnix-d.StartUnix != 86399 {
		t.Fatalf("single day = %+v %v", d, err)
	}

	// a day containing the DST fall-back is 25 hours long
	fb, err := ParseDateRange("2026-11-01", "2026-11-01", "", "", la)

	if err != nil || fb.EndUnix-fb.StartUnix != 25*3600-1 {
		t.Fatalf("fall-back day = %d seconds", fb.EndUnix-fb.StartUnix+1)
	}
}

func TestClockParseDateRangeRefusals(t *testing.T) {
	utc := time.UTC

	cases := [][4]string{
		{"", "", "", ""},
		{"2026-03-31", "2026-03-01", "", ""},
		{"2026-3-1", "2026-03-31", "", ""},
		{"last-month", "2026-03-31", "", ""},
		{"2026-02-30", "2026-03-31", "", ""},
		{"1970-01-01", "2026-01-01", "", ""},
	}

	for _, c := range cases {
		if _, err := ParseDateRange(c[0], c[1], c[2], c[3], utc); err == nil {
			t.Errorf("ParseDateRange(%q, %q) must be refused", c[0], c[1])
		} else if f := toFail(err); f.Code != CodeInvalidInput || f.Hint == "" {
			t.Errorf("ParseDateRange(%q, %q) → %+v", c[0], c[1], f)
		}
	}

	// defaults fill a missing side
	r, err := ParseDateRange("", "", "2026-01-01", "2026-01-31", utc)

	if err != nil || r.Start != "2026-01-01" || r.End != "2026-01-31" {
		t.Fatalf("defaults = %+v %v", r, err)
	}
}

// §21: a transaction at 23:30 on the 31st in UTC−8 lands in the right month under both zones
func TestClockMonthBoundaryInTwoTimezones(t *testing.T) {
	la := jrLoc(t, "America/Los_Angeles")
	london := jrLoc(t, "Europe/London")

	// 2026-01-31 23:30 in Los Angeles (UTC−8) is 2026-02-01 07:30 UTC
	tx := time.Date(2026, 1, 31, 23, 30, 0, 0, la)

	in := func(start, end string, loc *time.Location) bool {
		r, err := ParseDateRange(start, end, "", "", loc)

		if err != nil {
			t.Fatal(err)
		}

		return tx.Unix() >= r.StartUnix && tx.Unix() <= r.EndUnix
	}

	if !in("2026-01-01", "2026-01-31", la) || in("2026-02-01", "2026-02-28", la) {
		t.Error("under America/Los_Angeles the transaction is January's")
	}

	if in("2026-01-01", "2026-01-31", london) || !in("2026-02-01", "2026-02-28", london) {
		t.Error("under Europe/London the transaction is February's")
	}

	if DateOfUnix(tx.Unix(), la) != "2026-01-31" || DateOfUnix(tx.Unix(), london) != "2026-02-01" {
		t.Errorf("DateOfUnix: %s / %s", DateOfUnix(tx.Unix(), la), DateOfUnix(tx.Unix(), london))
	}

	if DateOfUnixMilli(tx.UnixMilli()+123, la) != "2026-01-31" {
		t.Error("DateOfUnixMilli ignores the sequence digits")
	}
}

func TestClockParseMonth(t *testing.T) {
	la := jrLoc(t, "America/Los_Angeles")
	m, err := ParseMonth("month", "2026-02", la)

	if err != nil || !m.Equal(time.Date(2026, 2, 1, 0, 0, 0, 0, la)) {
		t.Fatalf("ParseMonth = %v %v", m, err)
	}

	for _, bad := range []string{"2026-13", "2026-2", "2026-02-01", "Feb 2026"} {
		if _, err := ParseMonth("month", bad, la); err == nil {
			t.Errorf("ParseMonth(%q) must fail", bad)
		}
	}
}

func TestClockUTCOffsetMinutes(t *testing.T) {
	la := jrLoc(t, "America/Los_Angeles")

	if got := UTCOffsetMinutes(time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC), la); got != -480 {
		t.Errorf("winter offset = %d", got)
	}

	if got := UTCOffsetMinutes(time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC), la); got != -420 {
		t.Errorf("summer offset = %d", got)
	}

	if got := UTCOffsetMinutes(time.Now(), jrLoc(t, "Asia/Kolkata")); got != 330 {
		t.Errorf("half-hour offset = %d", got)
	}
}

func TestClockResolveLocation(t *testing.T) {
	jrIsolate(t)
	t.Setenv("TZ", "Asia/Tokyo")

	if got := ResolveLocation("Europe/London").String(); got != "Europe/London" {
		t.Errorf("header zone ignored: %s", got)
	}

	// no header, no credentials file → the machine's zone, by name
	if got := ResolveLocation("").String(); got != "Asia/Tokyo" {
		t.Errorf("fallback zone = %s", got)
	}

	// an unknown header zone falls through rather than failing
	if got := ResolveLocation("Not/AZone").String(); got != "Asia/Tokyo" {
		t.Errorf("bad header zone = %s", got)
	}

	// the credentials file's timezone outranks the machine's
	jrWriteFile(t, jrCredsPath(t), []byte(`{"ezbookkeeping":{"timezone":"America/Los_Angeles"}}`), 0o600)

	if got := ResolveLocation("").String(); got != "America/Los_Angeles" {
		t.Errorf("credentials zone = %s", got)
	}

	if got := ResolveLocation("Europe/London").String(); got != "Europe/London" {
		t.Errorf("the header must outrank the credentials file: %s", got)
	}
}

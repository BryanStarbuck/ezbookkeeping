package machine

import (
	"os"
	"strings"
	"time"

	"github.com/mayswind/ezbookkeeping/pkg/errfile"
)

// clock.go is the ONE place a YYYY-MM-DD becomes unix seconds (apis.mdx §17.4)

// ResolveLocation picks the timezone for a call: the X-Timezone-Name header, then the credentials
// file's ezbookkeeping.timezone, then the server's local zone
func ResolveLocation(header string) *time.Location {
	if header = strings.TrimSpace(header); header != "" {
		if loc, err := time.LoadLocation(header); err == nil {
			return loc
		}
	}

	if creds, err := ReadCredentials(); err == nil && creds.Timezone != "" {
		if loc, err := time.LoadLocation(creds.Timezone); err == nil {
			return loc
		}
	}

	return systemLocation()
}

// systemLocation names the machine's zone (time.Local's name is just "Local", which tells a
// caller nothing): TZ, then the /etc/localtime link, then time.Local
func systemLocation() *time.Location {
	if tz := strings.TrimSpace(os.Getenv("TZ")); tz != "" {
		if loc, err := time.LoadLocation(strings.TrimPrefix(tz, ":")); err == nil {
			return loc
		}
	}

	if target, err := os.Readlink("/etc/localtime"); err == nil {
		if i := strings.Index(target, "zoneinfo/"); i >= 0 {
			if loc, err := time.LoadLocation(target[i+len("zoneinfo/"):]); err == nil {
				return loc
			}
		}
	}

	return time.Local
}

// ParseDate parses YYYY-MM-DD as midnight in loc
func ParseDate(name, v string, loc *time.Location) (time.Time, error) {
	t, err := time.ParseInLocation("2006-01-02", strings.TrimSpace(v), loc)

	if err != nil {
		errfile.Expected("parsing a YYYY-MM-DD date argument", err)
		return time.Time{}, Invalid("dates are YYYY-MM-DD; relative dates are resolved by the client", "%s %q is not a YYYY-MM-DD date", name, v)
	}

	return t, nil
}

// ParseMonth parses YYYY-MM as the first instant of that month in loc
func ParseMonth(name, v string, loc *time.Location) (time.Time, error) {
	t, err := time.ParseInLocation("2006-01", strings.TrimSpace(v), loc)

	if err != nil {
		errfile.Expected("parsing a YYYY-MM month argument", err)
		return time.Time{}, Invalid("months are YYYY-MM", "%s %q is not a YYYY-MM month", name, v)
	}

	return t, nil
}

// DateRange is an inclusive date range resolved to unix seconds
type DateRange struct {
	Start     string `json:"start"`
	End       string `json:"end"`
	StartUnix int64  `json:"startUnix"`
	EndUnix   int64  `json:"endUnix"`
	Timezone  string `json:"timezone"`
}

// ParseDateRange resolves inclusive start/end dates. A missing start or end falls back to the
// given defaults (which may be empty, meaning required).
func ParseDateRange(start, end, defStart, defEnd string, loc *time.Location) (*DateRange, error) {
	if start == "" {
		start = defStart
	}

	if end == "" {
		end = defEnd
	}

	if start == "" || end == "" {
		return nil, Invalid("pass start and end as YYYY-MM-DD", "a date range is required")
	}

	s, err := ParseDate("start", start, loc)

	if err != nil {
		return nil, err
	}

	e, err := ParseDate("end", end, loc)

	if err != nil {
		return nil, err
	}

	if e.Before(s) {
		return nil, Invalid("end must be on or after start", "end %s is before start %s", end, start)
	}

	if e.Sub(s) > 50*366*24*time.Hour {
		return nil, Invalid("narrow the range to 50 years or less", "the range %s..%s is longer than 50 years", start, end)
	}

	endOfDay := e.AddDate(0, 0, 1).Add(-time.Second)

	return &DateRange{
		Start:     s.Format("2006-01-02"),
		End:       e.Format("2006-01-02"),
		StartUnix: s.Unix(),
		EndUnix:   endOfDay.Unix(),
		Timezone:  loc.String(),
	}, nil
}

// DateOfUnix renders unix seconds as YYYY-MM-DD in loc
func DateOfUnix(sec int64, loc *time.Location) string {
	return time.Unix(sec, 0).In(loc).Format("2006-01-02")
}

// DateOfUnixMilli renders upstream's transactionTime (unix milliseconds) as YYYY-MM-DD in loc
func DateOfUnixMilli(ms int64, loc *time.Location) string {
	return time.UnixMilli(ms).In(loc).Format("2006-01-02")
}

// UTCOffsetMinutes returns the offset of loc at t, in minutes, the way upstream's API wants it
func UTCOffsetMinutes(t time.Time, loc *time.Location) int16 {
	_, off := t.In(loc).Zone()

	return int16(off / 60)
}

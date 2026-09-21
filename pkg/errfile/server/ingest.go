package server

import (
	"encoding/json"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/mayswind/ezbookkeeping/pkg/errfile"
)

// The ingest route's caps (§9.2).
const (
	// BodyCap is the largest request body read.
	BodyCap = 64 * 1024
	// EventsPerRequest is the most events kept from one request.
	EventsPerRequest = 50
	// RatePerClient is the events allowed per minute per (app, remote address).
	RatePerClient = 240
	// RateGlobal is the events allowed per minute across every client.
	RateGlobal = 1200
	rateWindow = time.Minute
)

// event is one browser record as the browser sink sends it (§9.1).
type event struct {
	TS    string          `json:"ts"`
	Level string          `json:"level"`
	App   string          `json:"app"`
	Where string          `json:"where"`
	Doing string          `json:"doing"`
	Error string          `json:"error"`
	Cause string          `json:"cause"`
	Stack json.RawMessage `json:"stack"`
	Data  map[string]any  `json:"data"`
}

type body struct {
	App    string  `json:"app"`
	Events []event `json:"events"`
}

// limiter is the per-minute rate limit with one WARN per minute for what it dropped.
type limiter struct {
	mu          sync.Mutex
	windowStart time.Time
	global      int
	perClient   map[string]int
	dropped     int
	summaryDue  bool
	now         func() time.Time
}

func newLimiter(now func() time.Time) *limiter {
	return &limiter{perClient: map[string]int{}, now: now}
}

func (l *limiter) admit(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()

	t := l.now()

	if t.Sub(l.windowStart) >= rateWindow {
		l.windowStart = t
		l.global = 0
		l.perClient = map[string]int{}
	}

	if l.perClient[key] >= RatePerClient || l.global >= RateGlobal {
		return false
	}

	l.perClient[key]++
	l.global++

	return true
}

func (l *limiter) drop(n int, via string) {
	if n <= 0 {
		return
	}

	l.mu.Lock()
	l.dropped += n
	schedule := !l.summaryDue
	l.summaryDue = true
	l.mu.Unlock()

	if schedule {
		time.AfterFunc(rateWindow, func() { l.writeSummary(via) })
	}
}

func (l *limiter) writeSummary(via string) {
	l.mu.Lock()
	n := l.dropped
	l.dropped = 0
	l.summaryDue = false
	l.mu.Unlock()

	if n == 0 {
		return
	}

	errfile.WriteRecord(&errfile.Record{
		Level: errfile.LevelWarn,
		App:   "server",
		Where: "pkg/errfile/server/ingest.go",
		Doing: "receiving browser error reports",
		Error: "logged: dropped " + strconv.Itoa(n) + " browser reports over the rate limit",
		Data:  []errfile.KV{{Key: "via", Value: via}},
	})
}

// PeerCheck reports whether the request's real socket peer is loopback.
type PeerCheck func(c *gin.Context) bool

// DefaultPeerCheck reads the kernel's view of the peer from c.Request.RemoteAddr. It never calls
// gin's ClientIP(), which honours proxy headers.
func DefaultPeerCheck(c *gin.Context) bool {
	host, _, err := net.SplitHostPort(c.Request.RemoteAddr)

	if err != nil {
		host = c.Request.RemoteAddr
	}

	ip := net.ParseIP(strings.Trim(host, "[]"))

	return ip != nil && ip.IsLoopback()
}

// Handler is the gin handler for POST /error-report with the default peer check.
func Handler() gin.HandlerFunc {
	return HandlerWithPeerCheck(DefaultPeerCheck)
}

// HandlerWithPeerCheck is Handler with the machine plane's own loopback check (which also knows
// about a private unix-socket listener). Guards run in the order of §9.2, and the route ALWAYS
// answers 204: it never echoes the body back, sets no cookie, and carries no CORS header.
func HandlerWithPeerCheck(isLoopback PeerCheck) gin.HandlerFunc {
	lim := newLimiter(time.Now)

	return func(c *gin.Context) {
		defer func() {
			if r := recover(); r != nil {
				c.Status(http.StatusNoContent)
			}
		}()

		defer c.Abort()
		c.Status(http.StatusNoContent)

		// 1. loopback only — the real socket peer
		if isLoopback == nil || !isLoopback(c) {
			return
		}

		// 2. content type
		ct := strings.ToLower(c.GetHeader("Content-Type"))

		if !strings.HasPrefix(ct, "application/json") && !strings.HasPrefix(ct, "text/plain") {
			return
		}

		// 3. size cap
		raw, err := io.ReadAll(http.MaxBytesReader(c.Writer, c.Request.Body, BodyCap))

		if err != nil {
			return
		}

		var b body

		if err := json.Unmarshal(raw, &b); err != nil {
			return
		}

		via := viaOf(c)

		// 4. event cap
		events := b.Events

		if len(events) > EventsPerRequest {
			lim.drop(len(events)-EventsPerRequest, via)
			events = events[:EventsPerRequest]
		}

		peer := peerHost(c)

		for i := range events {
			rec := sanitize(&events[i], b.App, via)

			// 5. rate limit
			if !lim.admit(rec.App + "|" + peer) {
				lim.drop(1, via)
				continue
			}

			// 7. tag and write
			errfile.WriteRecord(rec)
		}
	}
}

func peerHost(c *gin.Context) string {
	host, _, err := net.SplitHostPort(c.Request.RemoteAddr)

	if err != nil {
		return c.Request.RemoteAddr
	}

	return host
}

// viaOf is "vite" when the request came through the Vite dev proxy, else "server".
func viaOf(c *gin.Context) string {
	if strings.EqualFold(c.GetHeader("X-Ezbk-Via"), "vite") || c.GetHeader("X-Forwarded-Host") != "" || c.GetHeader("X-Forwarded-For") != "" {
		return "vite"
	}

	return "server"
}

var isoLayouts = []string{"2006-01-02T15:04:05.000Z07:00", time.RFC3339Nano, time.RFC3339}

// sanitize is §9.2 step 6: the server never trusts the browser's own sanitising or redaction.
func sanitize(e *event, bodyApp, via string) *errfile.Record {
	app := strings.ToLower(strings.TrimSpace(firstNonEmpty(e.App, bodyApp)))

	if app != "web" && app != "sw" {
		app = "web"
	}

	level := errfile.LevelError

	switch strings.ToUpper(e.Level) {
	case "WARN":
		level = errfile.LevelWarn
	case "EXPECTED":
		level = errfile.LevelExpected
	}

	ts := time.Now()

	for _, layout := range isoLayouts {
		if t, err := time.Parse(layout, e.TS); err == nil {
			ts = t
			break
		}
	}

	where := clip(e.Where, 200)

	if where == "" {
		where = "(browser)"
	}

	doing := clip(e.Doing, 200)

	if doing == "" {
		doing = "an unspecified browser operation"
	}

	data := errfile.RedactData(e.Data)
	data = append(data, errfile.KV{Key: "via", Value: via})

	return &errfile.Record{
		TS:    ts,
		Level: level,
		App:   app,
		Where: where,
		Doing: doing,
		Error: errfile.RedactURLs(clip(e.Error, errfile.MessageCap)),
		Cause: causeField(errfile.RedactURLs(clip(e.Cause, errfile.MessageCap))),
		Stack: stackLines(e.Stack),
		Data:  data,
	}
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}

	return b
}

// clip strips control characters and caps a browser string; the record formatter does it again.
func clip(s string, max int) string {
	s = strings.TrimSpace(s)
	runes := []rune(s)

	if len(runes) > max {
		s = string(runes[:max])
	}

	var b strings.Builder

	for _, r := range s {
		if r < 0x20 || r == 0x7f || r == 0x2028 || r == 0x2029 {
			b.WriteByte(' ')
		} else {
			b.WriteRune(r)
		}
	}

	return b.String()
}

// stackLines accepts a stack as a string or an array of strings and keeps at most StackFrames
// lines of at most 400 characters, each with a leading "at " removed.
func stackLines(raw json.RawMessage) []string {
	if len(raw) == 0 {
		return nil
	}

	var lines []string
	var asString string

	if err := json.Unmarshal(raw, &asString); err == nil {
		lines = strings.Split(asString, "\n")
	} else if err := json.Unmarshal(raw, &lines); err != nil {
		return nil
	}

	var out []string

	for _, line := range lines {
		line = strings.TrimSpace(line)
		line = strings.TrimPrefix(line, "at ")

		if line == "" {
			continue
		}

		out = append(out, clip(line, 400))

		if len(out) >= errfile.StackFrames {
			break
		}
	}

	return out
}

// causeField accepts the browser's ready-made " | cause: …" chain as well as a bare description
func causeField(c string) string {
	if c == "" || strings.HasPrefix(c, " | cause: ") || strings.HasPrefix(c, "| cause: ") {
		if c != "" && !strings.HasPrefix(c, " ") {
			return " " + c
		}

		return c
	}

	return " | cause: " + c
}

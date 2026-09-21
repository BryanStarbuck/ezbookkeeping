package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/mayswind/ezbookkeeping/pkg/errfile"
	"github.com/mayswind/ezbookkeeping/pkg/log"
)

type recordingSink struct {
	mu      sync.Mutex
	records []*errfile.Record
}

func (s *recordingSink) Write(r *errfile.Record) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.records = append(s.records, r)
}

func (s *recordingSink) Flush() {}

func (s *recordingSink) all() []*errfile.Record {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*errfile.Record, len(s.records))
	copy(out, s.records)

	return out
}

func (s *recordingSink) lines() []string {
	var out []string

	for _, r := range s.all() {
		out = append(out, strings.TrimSuffix(errfile.FormatRecord(r), "\n"))
	}

	return out
}

func install(t *testing.T) *recordingSink {
	t.Helper()
	errfile.ResetForTests()
	s := &recordingSink{}
	errfile.InstallSinkForTests("server", s)
	t.Cleanup(errfile.ResetForTests)

	return s
}

// ── loghook (§4.5, §8 N6) ──────────────────────────────────────────────────────────────────────

func TestLogHookReportsErrorfWithCallerWhere(t *testing.T) {
	s := install(t)
	InstallLogHook()
	InstallLogHook() // idempotent
	log.Errorf(nil, "[accounts.GetAccounts] failed to get accounts, because %s", "database is locked")
	log.Infof(nil, "[accounts.GetAccounts] info is not a fault")
	log.Warnf(nil, "[cron_job.doRun] job \"x\" is already running")
	recs := s.all()

	if len(recs) != 2 {
		t.Fatalf("records = %v", s.lines())
	}

	if !strings.HasPrefix(recs[0].Where, "pkg/errfile/server/server_test.go:") {
		t.Errorf("where must point at the caller of log.Errorf, got %q", recs[0].Where)
	}

	if recs[0].Level != errfile.LevelError || recs[0].Doing != "[accounts.GetAccounts] failed to get accounts" || recs[0].Error != "logged: database is locked" {
		t.Errorf("record = %q", s.lines()[0])
	}

	if recs[1].Level != errfile.LevelWarn || recs[1].Error != "logged: [cron_job.doRun] job \"x\" is already running" {
		t.Errorf("warn record = %q", s.lines()[1])
	}

	for _, fr := range recs[0].Stack {
		if strings.Contains(fr, "pkg/log/") || strings.Contains(fr, "logrus") {
			t.Errorf("hook frame kept: %v", recs[0].Stack)
		}
	}
}

func TestLogHookSystemErrorIsExpectedAndRequestIdKept(t *testing.T) {
	s := install(t)
	InstallLogHook()
	log.ErrorfWithExtra(nil, "stack", "System Error! because %s", "boom")

	if len(s.all()) != 0 {
		t.Errorf("System Error! must be EXPECTED (nothing written): %v", s.lines())
	}

	c := &fakeCtx{Context: context.Background(), id: "req-42"}
	log.Errorf(c, "[x.Y] failed, because %s", "z")
	lines := s.lines()

	if len(lines) != 1 || !strings.Contains(lines[0], "{request_id=req-42}") {
		t.Errorf("lines = %v", lines)
	}
}

type fakeCtx struct {
	context.Context
	id string
}

func (c *fakeCtx) ClientIP() string        { return "127.0.0.1" }
func (c *fakeCtx) GetContextId() string    { return c.id }
func (c *fakeCtx) GetClientLocale() string { return "" }

// ── ingest (§9) ────────────────────────────────────────────────────────────────────────────────

func engine(check PeerCheck) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()

	if check == nil {
		r.POST("/error-report", Handler())
	} else {
		r.POST("/error-report", HandlerWithPeerCheck(check))
	}

	return r
}

func post(r *gin.Engine, remote, contentType, body string, headers ...string) *httptest.ResponseRecorder {
	req := httptest.NewRequest("POST", "/error-report", strings.NewReader(body))
	req.RemoteAddr = remote
	req.Header.Set("Content-Type", contentType)

	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}

	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	return w
}

const oneEvent = `{"app":"web","events":[{"ts":"2026-09-21T16:04:11.902Z","level":"ERROR","where":"src/stores/account.ts","doing":"loading the account list","error":"AxiosError: Network Error","stack":"Error: x\n    at load (src/stores/account.ts:12:3)","data":{"edition":"desktop","account_name":"Checking","token":"t"}}]}`

func TestIngestNonLoopbackIs204WithNoWrite(t *testing.T) {
	s := install(t)
	r := engine(nil)

	for _, remote := range []string{"192.168.1.20:5555", "[2001:db8::1]:5555", "10.0.0.2:1"} {
		w := post(r, remote, "application/json", oneEvent, "X-Forwarded-For", "127.0.0.1")

		if w.Code != http.StatusNoContent || w.Body.Len() != 0 {
			t.Errorf("%s: %d %q", remote, w.Code, w.Body.String())
		}
	}

	if len(s.all()) != 0 {
		t.Errorf("non-loopback wrote %v", s.lines())
	}
}

func TestIngestLoopbackWritesSanitisedAndTagged(t *testing.T) {
	s := install(t)
	r := engine(nil)
	w := post(r, "127.0.0.1:40000", "application/json", oneEvent)

	if w.Code != 204 || w.Body.Len() != 0 || w.Header().Get("Access-Control-Allow-Origin") != "" || w.Header().Get("Set-Cookie") != "" {
		t.Errorf("answer = %d %q %v", w.Code, w.Body.String(), w.Header())
	}

	lines := s.lines()

	if len(lines) != 1 {
		t.Fatalf("lines = %v", lines)
	}

	want := "[2026-09-21T16:04:11.902Z] [ERROR] [web] [src/stores/account.ts] loading the account list — AxiosError: Network Error {account_name=[ledger-field refused] edition=desktop token=[redacted] via=server}\n    at Error: x\n    at load (src/stores/account.ts:12:3)"

	if lines[0] != want {
		t.Errorf("\n got %q\nwant %q", lines[0], want)
	}

	// text/plain (sendBeacon) and a via=vite header
	w = post(r, "[::1]:1", "text/plain;charset=UTF-8", strings.Replace(oneEvent, "account list", "tag list", 1), "X-Forwarded-Host", "localhost:8081")

	if w.Code != 204 || len(s.all()) != 2 || !strings.Contains(s.lines()[1], "via=vite}") {
		t.Errorf("beacon: %d %v", w.Code, s.lines())
	}
}

func TestIngestRejectsBadTypeSizeAndJSON(t *testing.T) {
	s := install(t)
	r := engine(nil)

	if w := post(r, "127.0.0.1:1", "application/x-www-form-urlencoded", oneEvent); w.Code != 204 {
		t.Errorf("bad type: %d", w.Code)
	}

	big := `{"app":"web","events":[{"where":"x","doing":"y","error":"` + strings.Repeat("z", BodyCap) + `"}]}`

	if w := post(r, "127.0.0.1:1", "application/json", big); w.Code != 204 {
		t.Errorf("big body: %d", w.Code)
	}

	if w := post(r, "127.0.0.1:1", "application/json", "{not json"); w.Code != 204 {
		t.Errorf("bad json: %d", w.Code)
	}

	if w := post(r, "127.0.0.1:1", "application/json", `{"app":"evil","events":[{"level":"FATAL","doing":"","where":"","error":"x"}]}`); w.Code != 204 {
		t.Errorf("odd event: %d", w.Code)
	}

	lines := s.lines()

	if len(lines) != 1 || !strings.Contains(lines[0], "[ERROR] [web] [(browser)] an unspecified browser operation — x {via=server}") {
		t.Errorf("only the odd event survives, normalised: %v", lines)
	}
}

func TestIngestEventCapAndRateLimit(t *testing.T) {
	s := install(t)
	r := engine(nil)
	var events []string

	for i := 0; i < EventsPerRequest+10; i++ {
		events = append(events, `{"where":"src/a.ts","doing":"d`+strings.Repeat("x", i)+`","error":"e"}`)
	}

	w := post(r, "127.0.0.1:1", "application/json", `{"app":"web","events":[`+strings.Join(events, ",")+`]}`)

	if w.Code != 204 || len(s.all()) != EventsPerRequest {
		t.Fatalf("event cap: %d written (want %d)", len(s.all()), EventsPerRequest)
	}

	// the same client keeps sending: the per-client budget stops it at RatePerClient
	for i := 0; i < 10; i++ {
		// a different `where` per request, so the burst folder does not fold them
		batch := strings.ReplaceAll(strings.Join(events[:EventsPerRequest], ","), "src/a.ts", "src/b"+strconv.Itoa(i)+".ts")
		post(r, "127.0.0.1:1", "application/json", `{"app":"web","events":[`+batch+`]}`)
	}

	if n := len(s.all()); n != RatePerClient {
		t.Errorf("rate limit: %d written (want %d)", n, RatePerClient)
	}
}

func TestIngestDropSummaryIsOneWarn(t *testing.T) {
	s := install(t)
	lim := newLimiter(time.Now)
	lim.drop(3, "server")
	lim.drop(4, "server")
	lim.writeSummary("server")
	lim.writeSummary("server")
	lines := s.lines()

	if len(lines) != 1 || !strings.Contains(lines[0], "[WARN] [server] [pkg/errfile/server/ingest.go] receiving browser error reports — logged: dropped 7 browser reports over the rate limit {via=server}") {
		t.Errorf("lines = %v", lines)
	}
}

func TestIngestUsesPeerCheckNotClientIP(t *testing.T) {
	s := install(t)
	r := engine(func(c *gin.Context) bool { return c.Request.RemoteAddr == "@" })

	if err := r.SetTrustedProxies([]string{"0.0.0.0/0"}); err != nil {
		t.Fatal(err)
	}

	post(r, "127.0.0.1:1", "application/json", oneEvent)

	if len(s.all()) != 0 {
		t.Errorf("the plane's peer check must decide, not the address")
	}

	post(r, "@", "application/json", oneEvent)

	if len(s.all()) != 1 {
		t.Errorf("a private unix socket peer must be accepted")
	}
}

package machine

import (
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/mayswind/ezbookkeeping/pkg/errfile"
)

// errfile_test.go — the error-file nets of pm/error_err.mdx §8 N8 and N9 and the canary of §15.3

type efSink struct{ records []*errfile.Record }

func (s *efSink) Write(r *errfile.Record) { s.records = append(s.records, r) }
func (s *efSink) Flush()                  {}

func efInstall(t *testing.T) *efSink {
	t.Helper()
	errfile.ResetForTests()
	s := &efSink{}
	errfile.InstallSinkForTests("server", s)
	t.Cleanup(errfile.ResetForTests)

	return s
}

const efEvent = `{"app":"web","events":[{"ts":"2026-09-21T16:04:11.902Z","level":"ERROR","where":"src/stores/account.ts","doing":"loading the account list","error":"TypeError: x is undefined"}]}`

func efPost(r *gin.Engine, remote string) *httptest.ResponseRecorder {
	req := httptest.NewRequest("POST", "/error-report", strings.NewReader(efEvent))
	req.RemoteAddr = remote
	req.Host = "127.0.0.1:8080"
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", "http://127.0.0.1:8080") // a browser: the machine gates would refuse this
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	return w
}

// N9: /error-report is mounted by Mount, outside /machine/v1 (no key, browser headers allowed) and
// outside /api/v1 (no JWT); loopback writes, non-loopback answers 204 and writes nothing.
func TestErrorReportRouteMountedOutsideTheGates(t *testing.T) {
	jrIsolate(t)
	s := efInstall(t)
	r := gin.New()
	cfg := jrGateConfig()
	Mount(r, cfg)

	mounted := false

	for _, ri := range r.Routes() {
		if ri.Method == "POST" && ri.Path == "/error-report" {
			mounted = true
		}

		if strings.HasPrefix(ri.Path, BasePath) && strings.Contains(ri.Path, "error-report") {
			t.Errorf("/error-report must not live under %s", BasePath)
		}
	}

	if !mounted {
		t.Fatal("POST /error-report is not mounted")
	}

	// no key, browser Origin header, non-loopback peer with a forged X-Forwarded-For: 204, no write
	req := httptest.NewRequest("POST", "/error-report", strings.NewReader(efEvent))
	req.RemoteAddr = "192.168.1.20:5555"
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Forwarded-For", "127.0.0.1")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != 204 || w.Body.Len() != 0 || len(s.records) != 0 {
		t.Errorf("non-loopback: %d %q records=%d", w.Code, w.Body.String(), len(s.records))
	}

	// loopback, still no key: 204 and one record tagged via=server
	if w := efPost(r, "127.0.0.1:40000"); w.Code != 204 || w.Body.Len() != 0 {
		t.Errorf("loopback: %d %q", w.Code, w.Body.String())
	}

	if len(s.records) != 1 {
		t.Fatalf("records = %d", len(s.records))
	}

	line := errfile.FormatRecord(s.records[0])

	if !strings.Contains(line, "[ERROR] [web] [src/stores/account.ts] loading the account list — TypeError: x is undefined {via=server}") {
		t.Errorf("line = %q", line)
	}

	// the plane's own routes are still gated: the same browser-style request to /machine/v1 is a 404
	req = httptest.NewRequest("GET", BasePath+"/ping", nil)
	req.RemoteAddr = "127.0.0.1:1"
	req.Host = "127.0.0.1:8080"
	req.Header.Set("Origin", "http://127.0.0.1:8080")
	w = httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != 404 {
		t.Errorf("machine gates must still refuse a browser: %d", w.Code)
	}
}

// N8: a handler error that is internal / upstream_error is Caught with the request id; a gate
// refusal or a validation answer is Expected (nothing written); a panic inside wrap() is Recovered.
func TestWrapReportsOnlyRealFaults(t *testing.T) {
	jrArm(t, false, false)
	s := efInstall(t)
	r := jrGateEngine(t,
		RouteDef{Method: "GET", Path: "/ef-internal", NoUser: true, Handler: func(*Ctx) (any, error) {
			return nil, NewFail(CodeInternal, "read ~/T/ezbookkeeping/error.err for the server-side detail", "the step failed")
		}},
		RouteDef{Method: "GET", Path: "/ef-invalid", NoUser: true, Handler: func(*Ctx) (any, error) { return nil, Invalid("fix it", "bad input") }},
		RouteDef{Method: "GET", Path: "/ef-panic", NoUser: true, Handler: func(*Ctx) (any, error) { panic("handler exploded") }},
	)

	if w := jrDo(r, jrReq{path: BasePath + "/ef-invalid"}); w.Code != 400 || len(s.records) != 0 {
		t.Errorf("invalid_input must be Expected: %d records=%d", w.Code, len(s.records))
	}

	if w := jrDo(r, jrReq{path: BasePath + "/ef-internal"}); w.Code != 500 || len(s.records) != 1 {
		t.Fatalf("internal must be Caught: %d records=%d", w.Code, len(s.records))
	}

	line := errfile.FormatRecord(s.records[0])

	if !strings.Contains(line, "handling GET /ef-internal — *machine.Fail: internal: the step failed") || !strings.Contains(line, "request_id=") || !strings.Contains(line, "code=internal") {
		t.Errorf("internal line = %q", line)
	}

	w := jrDo(r, jrReq{path: BasePath + "/ef-panic"})

	if w.Code != 500 || jrErrCode(t, w) != CodeInternal || !strings.Contains(w.Body.String(), "error.err") || strings.Contains(w.Body.String(), "ezbookkeeping.log") {
		t.Errorf("panic answer = %d %s", w.Code, w.Body)
	}

	if len(s.records) != 2 || !strings.Contains(s.records[1].Error, "panic: handler exploded") || s.records[1].Doing != "handling GET /ef-panic" {
		t.Errorf("panic record: %+v", s.records)
	}
}

// §15.3: the canary route exists only under EZBK_ERROR_FILE_CANARY=1, is never published, and
// panics inside wrap() so one ERROR line reaches the file.
func TestCanaryRouteOnlyUnderEnv(t *testing.T) {
	jrIsolate(t)
	jrArm(t, false, false)
	r := gin.New()
	cfg := jrGateConfig()
	Mount(r, cfg)

	for _, ri := range r.Routes() {
		if strings.Contains(ri.Path, "__canary") {
			t.Fatalf("the canary is mounted without the env var: %s", ri.Path)
		}
	}

	t.Setenv(EnvCanary, "1")
	s := efInstall(t)
	r = gin.New()
	Mount(r, cfg)
	w := jrDo(r, jrReq{method: "POST", path: BasePath + "/__canary"})

	if w.Code != 500 || jrErrCode(t, w) != CodeInternal {
		t.Errorf("canary answer = %d %s", w.Code, w.Body)
	}

	if len(s.records) != 1 || !strings.Contains(s.records[0].Error, "errfile canary") || s.records[0].App != "server" {
		t.Errorf("canary record: %+v", s.records)
	}

	routes, _ := projectCapabilities(cfg)

	for _, cr := range routes {
		if strings.Contains(cr.Path, "__canary") {
			t.Errorf("the canary must never be published in /capabilities")
		}
	}
}

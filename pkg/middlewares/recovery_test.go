package middlewares

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/mayswind/ezbookkeeping/pkg/core"
	"github.com/mayswind/ezbookkeeping/pkg/errfile"
)

type recordingSink struct{ records []*errfile.Record }

func (s *recordingSink) Write(r *errfile.Record) { s.records = append(s.records, r) }
func (s *recordingSink) Flush()                  {}

// pm/error_err.mdx §8 N7: a panic in a handler is one ERROR line with the route template and the
// request id, and the client still gets upstream's system-error answer.
func TestRecoveryReportsPanicToErrorFile(t *testing.T) {
	errfile.ResetForTests()
	t.Cleanup(errfile.ResetForTests)
	sink := &recordingSink{}
	errfile.InstallSinkForTests("server", sink)

	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(func(c *gin.Context) { Recovery(core.WrapWebContext(c, nil)) })
	r.GET("/api/v1/accounts/:id", func(c *gin.Context) {
		var m map[string]int
		m["boom"] = 1
	})

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("GET", "/api/v1/accounts/42?token=secret", nil))

	if w.Code != http.StatusOK && w.Code != http.StatusInternalServerError {
		t.Errorf("status = %d", w.Code)
	}

	if len(sink.records) != 1 {
		t.Fatalf("records = %d", len(sink.records))
	}

	rec := sink.records[0]
	line := errfile.FormatRecord(rec)

	if rec.Doing != "handling GET /api/v1/accounts/:id" || strings.Contains(line, "token=secret") || strings.Contains(line, "/accounts/42") {
		t.Errorf("the route template, never the raw URL: %q", line)
	}

	if !strings.Contains(rec.Error, "assignment to entry in nil map") || !strings.HasPrefix(rec.Stack[0], "TestRecoveryReportsPanicToErrorFile.func") {
		t.Errorf("panic site must be the first frame: %v / %q", rec.Stack, rec.Error)
	}
}

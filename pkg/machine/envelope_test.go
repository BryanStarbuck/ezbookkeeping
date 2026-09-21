package machine

import (
	"errors"
	"fmt"
	"net/http"
	"testing"

	"github.com/mayswind/ezbookkeeping/pkg/errs"
)

// envelope_test.go — apis.mdx §7.1–§7.2, §21 "Envelope"

func TestEnvelopeNineCodes(t *testing.T) {
	if len(AllCodes) != 9 {
		t.Fatalf("the vocabulary is exactly nine codes, got %d", len(AllCodes))
	}

	want := map[string]int{
		CodeUnauthorized: 401, CodeForbidden: 403, CodeNotFound: 404, CodeInvalidInput: 400, CodeConflict: 409,
		CodeWriteDisabled: 403, CodeNotReady: 503, CodeUpstreamError: 502, CodeInternal: 500,
	}

	seen := map[string]bool{}

	for _, c := range AllCodes {
		if seen[c] {
			t.Errorf("duplicate code %s", c)
		}

		seen[c] = true

		if got := (&Fail{Code: c}).HTTPStatus(); got != want[c] {
			t.Errorf("%s → HTTP %d, want %d", c, got, want[c])
		}
	}

	if (&Fail{Code: "tenth_code"}).HTTPStatus() != http.StatusInternalServerError {
		t.Error("an unknown code must answer 500")
	}
}

// the MCP's twelve codes are a strict superset of these nine (mcp.mdx §12.3)
func TestEnvelopeMCPSuperset(t *testing.T) {
	mcp := map[string]bool{"confirm_required": true, "too_many_changes": true, "wrong_server": true}

	for _, c := range AllCodes {
		mcp[c] = true
	}

	if len(mcp) != 12 {
		t.Fatalf("mcp vocabulary would be %d codes, want 12", len(mcp))
	}
}

func TestEnvelopeUpstreamMapping(t *testing.T) {
	cases := []struct {
		err  *errs.Error
		code string
	}{
		{errs.ErrRepeatedRequest, CodeConflict},
		{errs.ErrIPForbidden, CodeForbidden},
		{errs.ErrAccountNotFound, CodeNotFound},
		{errs.ErrTransactionNotFound, CodeNotFound},
		{errs.ErrIncompleteOrIncorrectSubmission, CodeInvalidInput},
		{errs.ErrOperationFailed, CodeUpstreamError},
		{errs.ErrDatabaseOperationFailed, CodeUpstreamError},
	}

	for _, c := range cases {
		f := Upstream(c.err)

		if f.Code != c.code {
			t.Errorf("%s (%d) → %s, want %s", c.err.Message, c.err.Code(), f.Code, c.code)
		}

		if f.Hint == "" {
			t.Errorf("%s maps without a hint (R6)", c.err.Message)
		}

		if f.UpstreamCode != c.err.Code() {
			t.Errorf("%s: upstreamCode %d, want %d", c.err.Message, f.UpstreamCode, c.err.Code())
		}
	}

	// apis.mdx §7.2: a feature restriction maps to forbidden
	if f := Upstream(errs.ErrNotPermittedToPerformThisAction); f.Code != CodeForbidden {
		t.Errorf("ErrNotPermittedToPerformThisAction maps to %s, spec says forbidden", f.Code)
	}

	// a server-side failure never echoes upstream's internal message
	if f := Upstream(errs.ErrDatabaseOperationFailed); f.Message != "the server could not complete the operation" {
		t.Errorf("upstream_error message = %q", f.Message)
	}
}

func TestEnvelopeToFail(t *testing.T) {
	if toFail(nil) != nil {
		t.Error("toFail(nil) must be nil")
	}

	if UpErr(nil) != nil {
		t.Error("UpErr(nil) must be a true nil error")
	}

	var e *errs.Error

	if err := UpErr(e); err != nil {
		t.Error("UpErr of a typed nil must be a true nil error")
	}

	f := toFail(fmt.Errorf("SELECT * FROM secret_table WHERE password='x'"))

	if f.Code != CodeInternal || f.Message != "internal error" || f.Hint == "" {
		t.Errorf("a plain error must become a generic internal Fail, got %+v", f)
	}

	wrapped := fmt.Errorf("context: %w", Invalid("fix it", "bad %s", "thing"))

	if got := toFail(wrapped); got.Code != CodeInvalidInput || got.Message != "bad thing" {
		t.Errorf("a wrapped Fail must survive, got %+v", got)
	}

	up := fmt.Errorf("context: %w", errs.ErrAccountNotFound)

	if got := toFail(up); got.Code != CodeNotFound {
		t.Errorf("a wrapped upstream error must map, got %+v", got)
	}
}

func TestEnvelopeFailHelpers(t *testing.T) {
	for _, f := range []*Fail{Invalid("h", "m"), NotFound("h", "m"), Conflict("h", "m"), NewFail(CodeNotReady, "h", "m %d", 1)} {
		if f.Hint != "h" || f.Message == "" {
			t.Errorf("%+v", f)
		}
	}

	f := Conflict("h", "m").WithDetails(map[string]any{"n": 1})

	if f.Details.(map[string]any)["n"] != 1 {
		t.Error("details lost")
	}

	var target *Fail

	if !errors.As(error(f), &target) || target.Error() != "conflict: m" {
		t.Errorf("Error() = %q", target.Error())
	}
}

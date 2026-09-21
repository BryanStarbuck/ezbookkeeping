package machine

import (
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/mayswind/ezbookkeeping/pkg/errs"
)

// The nine error codes of the machine plane (apis.mdx §7.2). A tenth is a spec change.
const (
	CodeUnauthorized  = "unauthorized"
	CodeForbidden     = "forbidden"
	CodeNotFound      = "not_found"
	CodeInvalidInput  = "invalid_input"
	CodeConflict      = "conflict"
	CodeWriteDisabled = "write_disabled"
	CodeNotReady      = "not_ready"
	CodeUpstreamError = "upstream_error"
	CodeInternal      = "internal"
)

// AllCodes lists the closed vocabulary, in spec order
var AllCodes = []string{CodeUnauthorized, CodeForbidden, CodeNotFound, CodeInvalidInput, CodeConflict, CodeWriteDisabled, CodeNotReady, CodeUpstreamError, CodeInternal}

var httpStatusForCode = map[string]int{
	CodeUnauthorized:  http.StatusUnauthorized,
	CodeForbidden:     http.StatusForbidden,
	CodeNotFound:      http.StatusNotFound,
	CodeInvalidInput:  http.StatusBadRequest,
	CodeConflict:      http.StatusConflict,
	CodeWriteDisabled: http.StatusForbidden,
	CodeNotReady:      http.StatusServiceUnavailable,
	CodeUpstreamError: http.StatusBadGateway,
	CodeInternal:      http.StatusInternalServerError,
}

// Fail is a machine-plane error. Every Fail names a fix in Hint (R6).
type Fail struct {
	Code         string `json:"code"`
	Message      string `json:"message,omitempty"`
	Hint         string `json:"hint,omitempty"`
	Details      any    `json:"details,omitempty"`
	UpstreamCode int32  `json:"upstreamCode,omitempty"`
}

func (f *Fail) Error() string {
	return f.Code + ": " + f.Message
}

// HTTPStatus returns the status this code is answered with
func (f *Fail) HTTPStatus() int {
	if s, ok := httpStatusForCode[f.Code]; ok {
		return s
	}

	return http.StatusInternalServerError
}

// NewFail builds a Fail
func NewFail(code, hint, format string, args ...any) *Fail {
	return &Fail{Code: code, Message: fmt.Sprintf(format, args...), Hint: hint}
}

// WithDetails attaches structured details (never a stack, a path outside the roots, or SQL)
func (f *Fail) WithDetails(d any) *Fail {
	f.Details = d
	return f
}

// Invalid is shorthand for an invalid_input Fail
func Invalid(hint, format string, args ...any) *Fail {
	return NewFail(CodeInvalidInput, hint, format, args...)
}

// NotFound is shorthand for a not_found Fail
func NotFound(hint, format string, args ...any) *Fail {
	return NewFail(CodeNotFound, hint, format, args...)
}

// Conflict is shorthand for a conflict Fail
func Conflict(hint, format string, args ...any) *Fail {
	return NewFail(CodeConflict, hint, format, args...)
}

// Upstream maps an upstream pkg/errs error to a machine-plane Fail. The mapping is one table, not a
// guess per handler (apis.mdx §7.2).
func Upstream(e *errs.Error) *Fail {
	if e == nil {
		return nil
	}

	msg := e.Message
	lower := strings.ToLower(msg)
	f := &Fail{Message: msg, UpstreamCode: e.Code()}

	switch {
	case e == errs.ErrRepeatedRequest:
		f.Code, f.Hint = CodeConflict, "the same submission arrived twice; retry with a new idempotency_key if it was intended"
	case e == errs.ErrIPForbidden:
		f.Code, f.Hint = CodeForbidden, "the server's IP allowlist refused this call"
	case strings.Contains(lower, "not found") || strings.Contains(lower, "not exist"):
		f.Code, f.Hint = CodeNotFound, "list the entities first to find a valid id"
	case e == errs.ErrNotPermittedToPerformThisAction || strings.Contains(lower, "not permitted") || strings.Contains(lower, "not allowed") || strings.Contains(lower, "restrict") || strings.Contains(lower, "not enabled") || strings.Contains(lower, "disabled"):
		f.Code, f.Hint = CodeForbidden, "this action is switched off for the bound user or in conf/ezbookkeeping.ini"
	case e.HttpStatusCode == http.StatusUnauthorized:
		f.Code, f.Hint = CodeForbidden, "the upstream handler refused the bound user"
	case e.HttpStatusCode == http.StatusForbidden:
		f.Code, f.Hint = CodeForbidden, "this action is switched off for the bound user or in conf/ezbookkeeping.ini"
	case e.HttpStatusCode == http.StatusNotFound:
		f.Code, f.Hint = CodeNotFound, "list the entities first to find a valid id"
	case e.HttpStatusCode >= 400 && e.HttpStatusCode < 500:
		f.Code, f.Hint = CodeInvalidInput, "check the arguments against GET /machine/v1/capabilities"
	default:
		f.Code, f.Hint = CodeUpstreamError, "read ~/T/ezbookkeeping/error.err for the server-side detail"
		f.Message = "the server could not complete the operation"
	}

	return f
}

// toFail normalises any handler error into a Fail
func toFail(err error) *Fail {
	if err == nil {
		return nil
	}

	var f *Fail

	if errors.As(err, &f) {
		return f
	}

	var ue *errs.Error

	if errors.As(err, &ue) {
		return Upstream(ue)
	}

	return &Fail{Code: CodeInternal, Message: "internal error", Hint: "read ~/T/ezbookkeeping/error.err for the server-side detail"}
}

// UpErr converts an upstream *errs.Error into an error value that is nil when the pointer is nil.
// (A typed nil *errs.Error stored in an error interface is not nil; this avoids that trap.)
func UpErr(e *errs.Error) error {
	if e == nil {
		return nil
	}

	return Upstream(e)
}

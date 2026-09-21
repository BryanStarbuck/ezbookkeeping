// GENERATED from pkg/errfile — run scripts/sync-errfile.sh
package errfile

import (
	"errors"
	"fmt"
	"io/fs"
	"strings"
	"syscall"
	"testing"
	"time"
	"unicode/utf8"
)

// ── format (§3.2) ──────────────────────────────────────────────────────────────────────────────

func TestFormatCannotForgeHeader(t *testing.T) {
	s := newSink(t, "server")
	Caught("doing\nthings", errors.New("bad\n[ERROR] [server] forged"), F("k", "v\r\nx"))
	lines := s.lines()

	if len(lines) != 1 {
		t.Fatalf("lines = %v", lines)
	}

	if strings.Count(lines[0], "\n") != len(s.all()[0].Stack) {
		t.Errorf("a field forged a line: %q", lines[0])
	}

	if !strings.Contains(lines[0], "doing things — *errors.errorString: bad [ERROR] [server] forged {k=v  x}") {
		t.Errorf("line = %q", lines[0])
	}
}

func TestFormatCauseChainAndCodes(t *testing.T) {
	s := newSink(t, "server")
	inner := &fs.PathError{Op: "open", Path: "/tmp/x/plan.json", Err: syscall.EACCES}
	err := fmt.Errorf("writing the plan: %w", inner)
	Caught("applying the plan", err, F("route", "statements.apply"), F("account_name", "Checking"), F("token", "t"))
	line := s.lines()[0]

	want := "applying the plan — *fmt.wrapError: writing the plan: open /tmp/x/plan.json: permission denied {route=statements.apply account_name=[ledger-field refused] token=[redacted]} | cause: *fs.PathError: open /tmp/x/plan.json: permission denied (op=open) | cause: syscall.Errno: permission denied (errno=EACCES)"

	if !strings.Contains(line, want) {
		t.Errorf("line = %q\nwant contains %q", line, want)
	}
}

func TestFormatJoinShowsThreeMembers(t *testing.T) {
	s := newSink(t, "server")
	err := errors.Join(errors.New("a"), errors.New("b"), errors.New("c"), errors.New("d"))
	Caught("joining", err)
	line := s.lines()[0]

	if strings.Count(line, "| cause:") != 3 || strings.Contains(line, "cause: *errors.errorString: d") {
		t.Errorf("line = %q", line)
	}
}

func TestFormatRecordCapKeepsHeader(t *testing.T) {
	r := &Record{TS: time.Now(), Level: LevelError, App: "server", Where: "pkg/x.go:1", Doing: "d", Error: "Error: m"}

	for i := 0; i < 12; i++ {
		r.Stack = append(r.Stack, strings.Repeat("f", 900)+" (pkg/x.go:1)")
	}

	out := FormatRecord(r)

	if n := utf8.RuneCountInString(out); n > RecordCap+1 {
		t.Errorf("record not capped: %d", n)
	}

	if !strings.HasPrefix(out, FormatHeader(r)) {
		t.Errorf("header was cut")
	}

	r.Stack = nil
	r.Error = strings.Repeat("x", 9000)
	out = FormatRecord(r)

	if n := utf8.RuneCountInString(out); n > RecordCap+40 || !strings.Contains(out, " chars cut)… ") {
		t.Errorf("oversized header not capped in the middle: %d", n)
	}
}

func TestDescribeCodesFromMethodsAndFields(t *testing.T) {
	line := Describe(&codedErr{msg: "invalid file format", code: 104001, HttpStatusCode: 400})

	if line != "*errfile.codedErr: invalid file format (code=104001 status=400)" {
		t.Errorf("Describe = %q", line)
	}

	if Describe(nil) != "" {
		t.Errorf("Describe(nil) must be empty")
	}

	cyc := &testErr{msg: "a"}
	cyc.cause = cyc

	if got := Describe(cyc); strings.Count(got, "cause:") != 0 {
		t.Errorf("a self-cause must not loop: %q", got)
	}
}

type codedErr struct {
	msg            string
	code           int32
	HttpStatusCode int
}

func (e *codedErr) Error() string { return e.msg }
func (e *codedErr) Code() int32   { return e.code }

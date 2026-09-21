package errfile

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

// ── expected (R6) ──────────────────────────────────────────────────────────────────────────────

func TestExpectedWritesNothingUnlessVerbose(t *testing.T) {
	s := newSink(t, "server")
	Expected("probing", errors.New("absent"))

	if len(s.all()) != 0 {
		t.Errorf("Expected wrote %v", s.lines())
	}

	t.Setenv(EnvVerbose, "1")
	Expected("probing", errors.New("absent"))

	if lines := s.lines(); len(lines) != 1 || !strings.Contains(lines[0], "[EXPECTED] [server]") {
		t.Errorf("verbose Expected: %v", lines)
	}
}

// ── totality (R11) ─────────────────────────────────────────────────────────────────────────────

type panickyErr struct{}

func (panickyErr) Error() string { panic("Error() panics") }

func TestTotality(t *testing.T) {
	s := newSink(t, "server")
	Caught("a hostile error", panickyErr{})

	if lines := s.lines(); len(lines) != 1 || !strings.Contains(lines[0], "[Error() panicked: Error() panics]") {
		t.Errorf("hostile Error(): %v", lines)
	}

	m := map[string]any{}
	m["self"] = m
	Caught("circular data", errors.New("x"), F("m", m))

	if lines := s.lines(); len(lines) != 2 || !strings.Contains(lines[1], "{m=[object]}") {
		t.Errorf("circular data: %v", lines)
	}

	s.panics = true
	lastResortUsed.Store(false)
	Caught("panicking sink", errors.New("y"))
	s.panics = false

	if !lastResortUsed.Load() {
		t.Errorf("a panicking sink must fall back to stderr once")
	}

	// re-entrancy: a sink that reports must not loop
	ResetForTests()
	loop := &reentrantSink{}
	InstallSinkForTests("server", loop)
	Caught("re-entrant", errors.New("z"))

	if loop.calls > inFlightCap+1 {
		t.Errorf("re-entrant sink looped %d times", loop.calls)
	}
}

type reentrantSink struct{ calls int }

func (s *reentrantSink) Write(r *Record) {
	s.calls++
	Caught("reporting from the sink", fmt.Errorf("again %d", s.calls))
}

func (s *reentrantSink) Flush() {}

package errfile

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// ── recovered / Go (§4.4) ──────────────────────────────────────────────────────────────────────

func TestGoWritesPanicSiteFirstAndProcessSurvives(t *testing.T) {
	s := newSink(t, "server")
	done := make(chan struct{})
	Go("crashing on purpose", func() {
		defer close(done)
		explode()
	})
	<-done
	deadline := time.Now().Add(time.Second)

	for len(s.all()) == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}

	recs := s.all()

	if len(recs) != 1 {
		t.Fatalf("records = %v", s.lines())
	}

	if !strings.HasPrefix(recs[0].Stack[0], "explode ("+here()+":") {
		t.Errorf("panic site must be the first frame: %v", recs[0].Stack)
	}

	if !strings.Contains(recs[0].Error, "runtime error: index out of range") || recs[0].Doing != "crashing on purpose" {
		t.Errorf("line = %q", s.lines()[0])
	}
}

func explode() {
	var a []int
	_ = a[3]
}

func TestRecoveredWithNonErrorValue(t *testing.T) {
	s := newSink(t, "server")

	func() {
		defer RecoverNet("running the job")()
		panic("job blew up")
	}()

	if lines := s.lines(); len(lines) != 1 || !strings.Contains(lines[0], "running the job — panic: job blew up") {
		t.Errorf("lines = %v", lines)
	}
}

// ── fatal flushes ──────────────────────────────────────────────────────────────────────────────

func TestFatalFlushes(t *testing.T) {
	s := newSink(t, "app-cli")
	Fatal("running ezbookkeeping", errors.New("cannot open database"), F("command", "database"))

	if s.flushes != 1 || len(s.all()) != 1 || s.all()[0].Level != LevelFatal {
		t.Errorf("flushes=%d recs=%v", s.flushes, s.lines())
	}
}

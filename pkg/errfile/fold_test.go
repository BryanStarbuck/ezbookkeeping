package errfile

import (
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"
)

// ── fold (R10) ─────────────────────────────────────────────────────────────────────────────────

func TestFold500IntoOnePlusSummary(t *testing.T) {
	s := newSink(t, "server")
	now := time.Now()
	state.folder.now = func() time.Time { return now }
	t.Cleanup(func() { state.folder.now = time.Now })

	for i := 0; i < 500; i++ {
		Caught("loading the list", fmt.Errorf("id %d failed", i))
	}

	if got := len(s.all()); got != 1 {
		t.Fatalf("500 identical faults wrote %d lines before the window closed", got)
	}

	now = now.Add(61 * time.Second)
	Caught("loading the list", fmt.Errorf("id %d failed", 999))
	lines := s.lines()

	if len(lines) != 3 {
		t.Fatalf("expected first + summary + next, got %d: %v", len(lines), lines)
	}

	if !strings.Contains(lines[1], "×499 more in the previous 60s (first at ") || !strings.Contains(lines[1], "): *errors.errorString: id 0 failed") {
		t.Errorf("summary = %q", lines[1])
	}
}

func TestFoldKeyIgnoresLineAndStack(t *testing.T) {
	s := newSink(t, "server")
	report(&Submission{Level: LevelError, Doing: "d", Err: errors.New("m"), Where: "pkg/x.go:10", NoDedupe: true}, 1)
	report(&Submission{Level: LevelError, Doing: "d", Err: errors.New("m"), Where: "pkg/x.go:20", NoDedupe: true}, 1)

	if len(s.all()) != 1 {
		t.Errorf("different lines of one file must fold: %d", len(s.all()))
	}
}

func TestFoldTimerWritesSummary(t *testing.T) {
	s := newSink(t, "server")
	f := newFolder(10, func(sum foldSummary) {
		s.Write(&Record{Level: LevelError, Error: SummaryText(sum.count, sum.window, "x", "h")})
	})

	if !f.admit("k", 20*time.Millisecond, foldContext{}) || f.admit("k", 20*time.Millisecond, foldContext{}) {
		t.Fatal("first admitted, second folded")
	}

	deadline := time.Now().Add(2 * time.Second)

	for len(s.all()) == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}

	if len(s.all()) != 1 || !strings.Contains(s.all()[0].Error, "×1 more") {
		t.Errorf("timer summary missing: %v", s.lines())
	}
}

func TestFoldKeyCapEvictsAndWritesOwedSummaries(t *testing.T) {
	var summaries []foldSummary
	f := newFolder(3, func(sum foldSummary) { summaries = append(summaries, sum) })
	f.admit("a", time.Minute, foldContext{})
	f.admit("a", time.Minute, foldContext{}) // a owes 1

	for _, k := range []string{"b", "c", "d"} {
		f.admit(k, time.Minute, foldContext{})
	}

	if f.size() != 3 {
		t.Errorf("size = %d", f.size())
	}

	if len(summaries) != 1 || summaries[0].count != 1 {
		t.Errorf("evicted key must write its owed summary: %+v", summaries)
	}

	f.admit("b", time.Minute, foldContext{})
	f.flushAll()

	if len(summaries) != 2 || f.size() != 0 {
		t.Errorf("flushAll: %+v size=%d", summaries, f.size())
	}
}

func TestTransientNetworkIsWarnWithLongWindow(t *testing.T) {
	s := newSink(t, "ezbk")
	err := &net.OpError{Op: "dial", Net: "tcp", Err: &os.SyscallError{Syscall: "connect", Err: syscall.ECONNREFUSED}}
	Caught("probing the server", err)
	recs := s.all()

	if len(recs) != 1 || recs[0].Level != LevelWarn {
		t.Fatalf("recs = %v", s.lines())
	}

	if !strings.Contains(recs[0].Cause, "(errno=ECONNREFUSED)") || !strings.Contains(recs[0].Error, "(op=dial)") {
		t.Errorf("line = %q", s.lines()[0])
	}

	e := state.folder.table[state.folder.order.Front().Value.(*foldEntry).key]

	if e.window != TransientFoldWindow {
		t.Errorf("window = %v", e.window)
	}

	Caught("resolving", &net.DNSError{Err: "no such host", Name: "rates.example", IsNotFound: true})

	if recs := s.all(); recs[len(recs)-1].Level != LevelWarn || !strings.Contains(recs[len(recs)-1].Error, "code=ENOTFOUND") {
		t.Errorf("dns miss: %v", s.lines())
	}
}

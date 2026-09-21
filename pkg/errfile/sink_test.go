package errfile

import (
	"fmt"
	"runtime"
	"strings"
	"sync"
	"testing"
)

// recordingSink captures records in memory.
type recordingSink struct {
	mu      sync.Mutex
	records []*Record
	flushes int
	panics  bool
}

func (s *recordingSink) Write(r *Record) {
	if s.panics {
		panic("sink exploded")
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	s.records = append(s.records, r)
}

func (s *recordingSink) Flush() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.flushes++
}

func (s *recordingSink) all() []*Record {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*Record, len(s.records))
	copy(out, s.records)

	return out
}

func (s *recordingSink) lines() []string {
	var out []string

	for _, r := range s.all() {
		out = append(out, strings.TrimSuffix(FormatRecord(r), "\n"))
	}

	return out
}

// here is the repo-relative path of the calling test file, so the same tests pass in the vendored
// copy under cli/internal/errfile.
func here() string {
	_, file, _, _ := runtime.Caller(1)

	return relPath(file)
}

func newSink(t *testing.T, app string) *recordingSink {
	t.Helper()
	ResetForTests()
	s := &recordingSink{}
	InstallSinkForTests(app, s)
	t.Cleanup(ResetForTests)

	return s
}

// ── pre-install queue (§4.2) ───────────────────────────────────────────────────────────────────

func TestPreInstallQueueDrainsAndCountsOverflow(t *testing.T) {
	ResetForTests()
	t.Cleanup(ResetForTests)

	for i := 0; i < PreInstallCap+5; i++ {
		Caught("booting", fmt.Errorf("early %d", i), F("i", i))
	}

	// each is a distinct message but the same normalised key: the folder keeps one
	if len(state.queue) != 1 {
		t.Fatalf("queue = %d", len(state.queue))
	}

	for i := 0; i < PreInstallCap+5; i++ {
		Caught("booting "+fmt.Sprint(i), fmt.Errorf("early"))
	}

	if len(state.queue) != PreInstallCap || state.overflow != 6 {
		t.Fatalf("queue = %d overflow = %d", len(state.queue), state.overflow)
	}

	s := &recordingSink{}
	InstallSinkForTests("app-cli", s)
	recs := s.all()

	if len(recs) != PreInstallCap+1 {
		t.Fatalf("drained %d", len(recs))
	}

	if recs[0].App != "app-cli" || !strings.Contains(recs[len(recs)-1].Error, "6 records were dropped") {
		t.Errorf("last = %q app = %q", recs[len(recs)-1].Error, recs[0].App)
	}
}

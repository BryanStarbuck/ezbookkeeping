// GENERATED from pkg/errfile — run scripts/sync-errfile.sh
package errfile

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

// ── dedupe (R4) ────────────────────────────────────────────────────────────────────────────────

func TestDedupeOnceThroughWrapsAndNets(t *testing.T) {
	s := newSink(t, "server")
	base := errors.New("disk on fire")
	Caught("writing the plan", base)
	wrapped := fmt.Errorf("applying: %w", base)
	Caught("applying", wrapped)
	twice := fmt.Errorf("handling the route: %w", wrapped)
	Caught("handling POST /x", twice)
	Recovered("recovering", twice)

	if got := len(s.all()); got != 1 {
		t.Errorf("one error, wrapped twice, seen by a net: %d lines (want 1)", got)
	}

	if !IsReported(twice) || !IsReported(base) {
		t.Errorf("IsReported must walk the chain")
	}
}

func TestDedupeExpiresAfterTTL(t *testing.T) {
	s := newSink(t, "server")
	now := time.Now()
	state.reported.now = func() time.Time { return now }
	state.folder.now = func() time.Time { return now }
	t.Cleanup(func() { state.reported.now = time.Now; state.folder.now = time.Now })

	sentinel := errors.New("operation failed")
	Caught("a", sentinel)
	Caught("b", sentinel) // a different site, the same value, inside the TTL: deduped
	now = now.Add(6 * time.Second)
	Caught("c", sentinel) // after the TTL: a new line

	if got := len(s.all()); got != 2 {
		t.Errorf("same sentinel 6 s later must be a new line: %d", got)
	}
}

func TestDedupeBounded(t *testing.T) {
	newSink(t, "server")

	for i := 0; i < DedupeEntries*2; i++ {
		Caught("filling", fmt.Errorf("e%d", i))
	}

	if n := len(state.reported.seen); n > DedupeEntries {
		t.Errorf("reported-set grew to %d", n)
	}
}

func TestDedupeUncomparableErrorDoesNotPanic(t *testing.T) {
	s := newSink(t, "server")
	Caught("a", sliceErr{"x"})
	Caught("a", sliceErr{"x"})

	if len(s.all()) != 1 { // folded, not deduped
		t.Errorf("lines = %d", len(s.all()))
	}
}

type sliceErr []string

func (e sliceErr) Error() string { return strings.Join(e, ",") }

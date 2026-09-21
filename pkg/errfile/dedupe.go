package errfile

import (
	"reflect"
	"sync"
	"time"
)

// The reported-set (R4-Go). Errors have no identity that is safe to mark forever —
// errs.ErrOperationFailed is one shared sentinel thousands of failures return — so a comparable
// error's DYNAMIC VALUE is remembered in a bounded map with a short TTL, and IsReported walks the
// Unwrap chain: an error reported at the site that knew what it was doing, wrapped with %w twice
// and finally seen by the recovery middleware, is one line.

// DedupeTTL is how long a reported value is remembered.
const DedupeTTL = 5 * time.Second

// DedupeEntries caps the reported-set.
const DedupeEntries = 1024

type reportedSet struct {
	mu   sync.Mutex
	seen map[any]time.Time
	now  func() time.Time
}

func newReportedSet() *reportedSet {
	return &reportedSet{seen: map[any]time.Time{}, now: time.Now}
}

// comparable reports whether an error's dynamic value can be a map key without panicking.
func comparable(err error) bool {
	if err == nil {
		return false
	}

	t := reflect.TypeOf(err)

	return t != nil && t.Comparable()
}

func (s *reportedSet) mark(err error) {
	if !comparable(err) {
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	now := s.now()

	if len(s.seen) >= DedupeEntries {
		s.evictLocked(now)
	}

	s.seen[err] = now.Add(DedupeTTL)
}

// evictLocked drops expired entries, then — if still full — the entries closest to expiry.
func (s *reportedSet) evictLocked(now time.Time) {
	for k, until := range s.seen {
		if !until.After(now) {
			delete(s.seen, k)
		}
	}

	for len(s.seen) >= DedupeEntries {
		var oldestKey any
		var oldest time.Time
		first := true

		for k, until := range s.seen {
			if first || until.Before(oldest) {
				oldestKey, oldest, first = k, until, false
			}
		}

		delete(s.seen, oldestKey)
	}
}

func (s *reportedSet) has(err error) bool {
	if !comparable(err) {
		return false
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	until, ok := s.seen[err]

	if !ok {
		return false
	}

	if !until.After(s.now()) {
		delete(s.seen, err)
		return false
	}

	return true
}

func (s *reportedSet) reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.seen = map[any]time.Time{}
}

// markReported remembers the top value of an error for DedupeTTL.
func markReported(err error) {
	defer func() { _ = recover() }()
	state.reported.mark(err)
}

// IsReported reports whether err, or anything it wraps (Unwrap() error / Unwrap() []error, up to
// CauseDepth links), was written in the last DedupeTTL.
func IsReported(err error) (reported bool) {
	defer func() {
		if r := recover(); r != nil {
			reported = false
		}
	}()

	if err == nil {
		return false
	}

	seen := map[uintptr]bool{}

	return isReportedWalk(err, seen, 0)
}

func isReportedWalk(err error, seen map[uintptr]bool, depth int) bool {
	if err == nil || depth > CauseDepth || isNilPointer(err) || !remember(seen, err) {
		return false
	}

	if state.reported.has(err) {
		return true
	}

	for _, link := range nextLinks(err) {
		if isReportedWalk(link, seen, depth+1) {
			return true
		}
	}

	return false
}

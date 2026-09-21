package errfile

import (
	"container/list"
	"regexp"
	"strconv"
	"sync"
	"time"
	"unicode/utf8"
)

// Burst folding (R10, §11.1). The first occurrence of a key is written through. Later ones inside
// the window are counted, and ONE summary line is written when the window closes, when the key is
// evicted, or at exit (flushAll). The table is capped, least-recently-seen first, and a key
// evicted while it still owes a summary writes that summary first.

// FoldWindow is the default fold window.
const FoldWindow = 60 * time.Second

// TransientFoldWindow is the fold window for transient network faults (§11.2).
const TransientFoldWindow = 10 * time.Minute

// FoldMaxKeys caps the fold table.
const FoldMaxKeys = 1000

// FoldKeyMessageCap is how much of the normalised message goes into the key.
const FoldKeyMessageCap = 300

// The normalisation steps, in order (vectors "normalise"): uuid, path, hex, digits.
var (
	normUUID   = regexp.MustCompile(`[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}`)
	normPath   = regexp.MustCompile(`(?:/[A-Za-z0-9_.\-]+){2,}`)
	normHex    = regexp.MustCompile(`\b[0-9a-fA-F]{8,}\b`)
	normDigits = regexp.MustCompile(`[0-9]+`)
)

// NormalizeMessage makes `id 123` and `id 456` fold together: UUIDs → <uuid>, absolute paths →
// <path>, hex runs of 8+ → <hex>, digits → #, then cut to FoldKeyMessageCap characters.
func NormalizeMessage(message string) string {
	if message == "" {
		return ""
	}

	out := normUUID.ReplaceAllString(message, "<uuid>")
	out = normPath.ReplaceAllString(out, "<path>")
	out = normHex.ReplaceAllString(out, "<hex>")
	out = normDigits.ReplaceAllString(out, "#")

	if utf8.RuneCountInString(out) > FoldKeyMessageCap {
		out = string([]rune(out)[:FoldKeyMessageCap])
	}

	return out
}

// foldContext is what a summary line needs to be written later.
type foldContext struct {
	level    Level
	app      string
	where    string
	doing    string
	headline string
	noEcho   bool
}

// foldSummary is one owed summary.
type foldSummary struct {
	count   int
	window  time.Duration
	firstAt time.Time
	ctx     foldContext
}

type foldEntry struct {
	key     string
	firstAt time.Time
	window  time.Duration
	count   int
	ctx     foldContext
	timer   *time.Timer
	elem    *list.Element
}

// folder is the burst folder. Its emit callback runs OUTSIDE the lock.
type folder struct {
	mu      sync.Mutex
	maxKeys int
	table   map[string]*foldEntry
	order   *list.List // front = least recently seen
	emit    func(foldSummary)
	now     func() time.Time
}

func newFolder(maxKeys int, emit func(foldSummary)) *folder {
	return &folder{maxKeys: maxKeys, table: map[string]*foldEntry{}, order: list.New(), emit: emit, now: time.Now}
}

// admit reports whether this occurrence is written (true) or folded into a burst (false).
func (f *folder) admit(key string, window time.Duration, ctx foldContext) bool {
	var owed []foldSummary
	f.mu.Lock()
	now := f.now()
	e := f.table[key]

	if e != nil && now.Sub(e.firstAt) < e.window {
		e.count++
		e.ctx = ctx
		f.order.MoveToBack(e.elem)

		if e.timer == nil {
			remaining := e.window - now.Sub(e.firstAt)

			if remaining < time.Millisecond {
				remaining = time.Millisecond
			}

			e.timer = time.AfterFunc(remaining, func() { f.closeWindow(key) })
		}

		f.mu.Unlock()

		return false
	}

	if e != nil {
		owed = append(owed, f.removeLocked(e)...)
	}

	e = &foldEntry{key: key, firstAt: now, window: window, ctx: ctx}
	e.elem = f.order.PushBack(e)
	f.table[key] = e

	for f.order.Len() > f.maxKeys {
		oldest := f.order.Front().Value.(*foldEntry)
		owed = append(owed, f.removeLocked(oldest)...)
	}

	f.mu.Unlock()
	f.emitAll(owed)

	return true
}

// removeLocked drops an entry from the table and returns its summary, if it owes one.
func (f *folder) removeLocked(e *foldEntry) []foldSummary {
	if e.timer != nil {
		e.timer.Stop()
		e.timer = nil
	}

	delete(f.table, e.key)
	f.order.Remove(e.elem)

	if e.count > 0 {
		s := foldSummary{count: e.count, window: e.window, firstAt: e.firstAt, ctx: e.ctx}
		e.count = 0

		return []foldSummary{s}
	}

	return nil
}

func (f *folder) closeWindow(key string) {
	f.mu.Lock()
	e := f.table[key]
	var owed []foldSummary

	if e != nil {
		e.timer = nil
		owed = f.removeLocked(e)
	}

	f.mu.Unlock()
	f.emitAll(owed)
}

// flushAll writes every owed summary now (exit, fatal).
func (f *folder) flushAll() {
	f.mu.Lock()
	var owed []foldSummary

	for f.order.Len() > 0 {
		e := f.order.Front().Value.(*foldEntry)
		owed = append(owed, f.removeLocked(e)...)
	}

	f.mu.Unlock()
	f.emitAll(owed)
}

// reset drops every entry without writing (test seam).
func (f *folder) reset() {
	f.mu.Lock()
	defer f.mu.Unlock()

	for _, e := range f.table {
		if e.timer != nil {
			e.timer.Stop()
		}
	}

	f.table = map[string]*foldEntry{}
	f.order.Init()
}

func (f *folder) size() int {
	f.mu.Lock()
	defer f.mu.Unlock()

	return len(f.table)
}

func (f *folder) emitAll(owed []foldSummary) {
	for _, s := range owed {
		func() {
			defer func() { _ = recover() }() // the summary is worth strictly less than the line it counts (R11)
			f.emit(s)
		}()
	}
}

// SummaryText is the error text of a folded summary line (§3.2):
// "×37 more in the previous 60s (first at 16:04:11): AxiosError: Network Error".
func SummaryText(count int, window time.Duration, firstAtClock string, headline string) string {
	w := ""

	if window > time.Minute && window%time.Minute == 0 {
		w = strconv.Itoa(int(window/time.Minute)) + "m"
	} else {
		w = strconv.Itoa(int((window + time.Second/2) / time.Second)) + "s"
	}

	text := "×" + strconv.Itoa(count) + " more in the previous " + w + " (first at " + firstAtClock + ")"

	if headline != "" {
		text += ": " + headline
	}

	return text
}

// clockTime is HH:MM:SS in UTC, like the timestamps.
func clockTime(t time.Time) string {
	return t.UTC().Format("15:04:05")
}

// GENERATED from pkg/errfile — run scripts/sync-errfile.sh
package errfile

import (
	"fmt"
	"os"
	"sync/atomic"
	"time"
)

// Level is a record's severity.
type Level string

// The levels (§3.2). EXPECTED appears only under EZBK_ERROR_FILE_VERBOSE=1.
const (
	LevelWarn     Level = "WARN"
	LevelError    Level = "ERROR"
	LevelFatal    Level = "FATAL"
	LevelExpected Level = "EXPECTED"
)

// EnvVerbose switches EXPECTED records on.
const EnvVerbose = "EZBK_ERROR_FILE_VERBOSE"

// EnvEcho switches the stderr echo on outside development.
const EnvEcho = "EZBK_ERROR_FILE_ECHO"

// Record is one fault, already described and redacted. It is plain data: the ingest route builds
// one from a browser's JSON and the logrus hook builds one from an entry.
type Record struct {
	TS    time.Time
	Level Level
	// App is the runtime (§3.3). Empty until a sink stamps it.
	App string
	// Where is the repo-relative source position (R14).
	Where string
	// Doing is the gerund phrase.
	Doing string
	// Error is the headline "Type: message (k=v …)"; "" for a WARN with no error.
	Error string
	// Cause is the " | cause: …" chain, "" when there is none.
	Cause string
	// Stack holds the trimmed frames, "fn (file:line)" each.
	Stack []string
	// Data holds the allowlisted, redacted pairs.
	Data []KV
	// noEcho marks records that must never be echoed to stderr (the logrus hook's, a browser's).
	noEcho bool
}

// Submission is the lower-level input of a report. The call-site API (Caught, Warn, …) fills one
// in; the logrus hook and the panic nets use it directly through Submit.
type Submission struct {
	Level  Level
	Doing  string
	Err    error
	Fields []Field
	// Type and Message describe a fault that has no error value (a logged message). They are used
	// when Err is nil.
	Type    string
	Message string
	// Where overrides the runtime.Caller position when set.
	Where string
	// Skip is how many extra caller frames to drop before choosing `where`.
	Skip int
	// SkipPrefixes are function-name prefixes never chosen as `where` and dropped from the stack
	// (the hook passes logrus and pkg/log).
	SkipPrefixes []string
	// NoEcho keeps the record off stderr even in development.
	NoEcho bool
	// AfterPanic captures the stack from the panic site (Recovered).
	AfterPanic bool
	// NoDedupe skips the reported-set (records with no error identity to remember).
	NoDedupe bool
}

// inFlight is the re-entrancy guard: reports in progress across all goroutines. Anything above the
// cap is dropped, so a sink that reports, or an Error() that reports, can never loop (R11).
var inFlight atomic.Int32

const inFlightCap = 64

var lastResortUsed atomic.Bool

// lastResort is the one place left to say the library itself failed: stderr, once.
func lastResort(what string, r any) {
	if lastResortUsed.Swap(true) {
		return
	}

	defer func() { _ = recover() }()
	fmt.Fprintf(os.Stderr, "[errfile] %s: %v\n", what, r)
}

// Submit runs the report path of §4.2 for one fault. Every step is total.
func Submit(s Submission) {
	if inFlight.Add(1) > inFlightCap {
		inFlight.Add(-1)
		return
	}

	defer inFlight.Add(-1)
	defer func() {
		if r := recover(); r != nil {
			lastResort("could not report an error", r)
		}
	}()

	report(&s, 2)
}

func report(s *Submission, skip int) {
	if s.Level == LevelExpected && !verbose() {
		return
	}

	err := s.Err

	if err != nil && !s.NoDedupe {
		if IsReported(err) {
			return
		}

		markReported(err)
	}

	var d described

	if err != nil {
		d = describeError(err)
	} else {
		d.typ = s.Type
		d.message = redactText(s.Message)
		d.headline = Headline(d.typ, d.message, nil)
	}

	level := s.Level
	window := FoldWindow

	if level == LevelError && err != nil && isTransientNetwork(err) {
		level = LevelWarn
		window = TransientFoldWindow
	}

	box := current()
	app := ""

	if box != nil {
		app = box.app
	}

	frames := captureFrames(skip+s.Skip, s.SkipPrefixes, s.AfterPanic)
	where := s.Where

	if where == "" {
		where = whereFrom(frames)
	}

	doing := stripControlChars(s.Doing)
	ctx := foldContext{level: level, app: app, where: where, doing: doing, headline: d.headline, noEcho: s.NoEcho}

	if level != LevelFatal {
		key := app + "\x01" + whereFile(where) + "\x01" + doing + "\x01" + d.typ + "\x01" + NormalizeMessage(d.message)

		if !state.folder.admit(key, window, ctx) {
			return
		}
	}

	stack := make([]string, 0, len(frames))

	for _, f := range frames {
		stack = append(stack, f.String())
	}

	deliver(&Record{
		TS:     time.Now(),
		Level:  level,
		App:    app,
		Where:  where,
		Doing:  doing,
		Error:  d.headline,
		Cause:  d.cause,
		Stack:  stack,
		Data:   redactFields(s.Fields),
		noEcho: s.NoEcho,
	})
}

// WriteRecord writes an already-built record (the ingest route). It is folded like any other
// record, but not de-duplicated: there is no error value left to remember. It is never echoed.
func WriteRecord(r *Record) {
	if r == nil {
		return
	}

	if inFlight.Add(1) > inFlightCap {
		inFlight.Add(-1)
		return
	}

	defer inFlight.Add(-1)
	defer func() {
		if rec := recover(); rec != nil {
			lastResort("could not write a record", rec)
		}
	}()

	rec := *r
	rec.noEcho = true

	if rec.TS.IsZero() {
		rec.TS = time.Now()
	}

	if rec.Level == "" {
		rec.Level = LevelError
	}

	if rec.Level != LevelFatal {
		key := rec.App + "\x01" + rec.Where + "\x01" + rec.Doing + "\x01" + NormalizeMessage(rec.Error)
		ctx := foldContext{level: rec.Level, app: rec.App, where: rec.Where, doing: rec.Doing, headline: rec.Error, noEcho: true}

		if !state.folder.admit(key, FoldWindow, ctx) {
			return
		}
	}

	deliver(&rec)
}

func emitSummary(s foldSummary) {
	deliver(&Record{
		TS:     time.Now(),
		Level:  s.ctx.level,
		App:    s.ctx.app,
		Where:  s.ctx.where,
		Doing:  s.ctx.doing,
		Error:  SummaryText(s.count, s.window, clockTime(s.firstAt), s.ctx.headline),
		noEcho: s.ctx.noEcho,
	})
}

func verbose() bool {
	return os.Getenv(EnvVerbose) == "1"
}

// GENERATED from pkg/errfile — run scripts/sync-errfile.sh
package errfile

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Caught writes an ERROR: what was being done, where (from runtime.Caller), and the error with its
// cause chain and a trimmed stack. `doing` is a gerund phrase: "reconciling the account".
func Caught(doing string, err error, fields ...Field) {
	Submit(Submission{Level: LevelError, Doing: doing, Err: err, Fields: fields, Skip: 1})
}

// Warn writes a WARN. A gate refusal that is unusual, a retry that will be tried again, a discarded
// cleanup error. `err` may be nil for a standing condition.
func Warn(doing string, err error, fields ...Field) {
	Submit(Submission{Level: LevelWarn, Doing: doing, Err: err, Fields: fields, Skip: 1})
}

// Expected records a DECISION: this failure is part of normal operation (an optional file that is
// absent, a probe, a parse of user input turned into a validation answer). Nothing is written
// unless EZBK_ERROR_FILE_VERBOSE=1, in which case an EXPECTED line appears.
func Expected(doing string, err error) {
	if !verbose() {
		return
	}

	Submit(Submission{Level: LevelExpected, Doing: doing, Err: err, Skip: 1})
}

// Fatal writes a FATAL record and flushes synchronously, so it is on disk before the host exits.
// It never exits: the host prints its own message and chooses its own exit code. When the host
// died before it could Install (a crash at boot, §18 use case 7), Fatal installs the default
// writer itself, tagged with the binary's name, so the pre-install queue and this record land on
// disk instead of dying with the process.
func Fatal(doing string, err error, fields ...Field) {
	Submit(Submission{Level: LevelFatal, Doing: doing, Err: err, Fields: fields, Skip: 1})

	if current() == nil {
		Install(Options{App: binaryName()})
	}

	Flush()
}

func binaryName() string {
	if len(os.Args) == 0 || os.Args[0] == "" {
		return "?"
	}

	name := filepath.Base(os.Args[0])

	return strings.TrimSuffix(name, filepath.Ext(name))
}

// Recovered is called with the value of recover(). It turns the value into an error (an error as
// is; anything else as "panic: <value>"), captures the goroutine's stack from the panic site, and
// writes an ERROR. It never re-panics; the caller decides that.
func Recovered(doing string, rec any, fields ...Field) {
	s := Submission{Level: LevelError, Doing: doing, Fields: fields, Skip: 1, AfterPanic: true}

	if err, ok := rec.(error); ok && err != nil {
		s.Err = err
	} else {
		s.Type = "panic"
		s.Message = safeSprint(rec)
		s.NoDedupe = true
	}

	Submit(s)
}

func safeSprint(v any) (out string) {
	defer func() {
		if r := recover(); r != nil {
			out = "[unprintable panic value]"
		}
	}()

	return fmt.Sprint(v)
}

// Go runs fn on a new goroutine under a recover net: a panic is written with its stack and the
// goroutine ends, instead of taking the process down with no record. It is the one-line
// replacement for `go func() { … }()`.
func Go(doing string, fn func()) {
	go func() {
		defer RecoverNet(doing)()
		fn()
	}()
}

// RecoverNet is the same net for a goroutine whose body is a named method, or that must stay a
// plain `go` for another reason: `defer errfile.RecoverNet("removing rotated log files")()`. The
// returned function recovers, reports, and does not re-panic.
func RecoverNet(doing string) func() {
	return func() {
		if r := recover(); r != nil {
			Recovered(doing, r)
		}
	}
}

// Package server holds the two errfile files that need a dependency — the gin ingest handler for
// POST /error-report and the logrus hook over pkg/log — and is therefore NOT vendored into the CLI.
package server

import (
	"strings"
	"sync"

	"github.com/sirupsen/logrus"

	"github.com/mayswind/ezbookkeeping/pkg/errfile"
	"github.com/mayswind/ezbookkeeping/pkg/log"
)

// logrus and pkg/log frames are never `where` and never in the stack of a hook record.
var hookSkipPrefixes = []string{
	"github.com/sirupsen/logrus",
	"github.com/mayswind/ezbookkeeping/pkg/log.",
}

// logFieldRequestId mirrors pkg/log's unexported field name.
const logFieldRequestId = "REQUEST_ID"

// hook turns every WARN/ERROR/FATAL/PANIC entry of upstream's loggers into a record (§4.5, §8 N6),
// so about 1,480 existing log.Errorf / log.Warnf sites report with zero call-site edits.
type hook struct{}

// Levels is the set of entries the hook sees.
func (hook) Levels() []logrus.Level {
	return []logrus.Level{logrus.WarnLevel, logrus.ErrorLevel, logrus.FatalLevel, logrus.PanicLevel}
}

// Fire runs synchronously under logrus's mutex, so it does exactly what Write does: a buffer append.
// It never returns an error — a failure here must not change what upstream logs.
func (hook) Fire(entry *logrus.Entry) error {
	defer func() { _ = recover() }()

	if entry == nil {
		return nil
	}

	message := entry.Message
	level := errfile.LevelError

	switch entry.Level {
	case logrus.WarnLevel:
		level = errfile.LevelWarn
	case logrus.FatalLevel, logrus.PanicLevel:
		level = errfile.LevelFatal
	}

	// The recovery net (§7 G7) has already written the real record for a panic; upstream's own
	// "System Error!" line for the same panic is only worth keeping under VERBOSE.
	if strings.HasPrefix(message, "System Error!") {
		level = errfile.LevelExpected
	}

	doing, detail := splitBecause(message)
	var fields []errfile.Field

	if id, ok := entry.Data[logFieldRequestId]; ok {
		if s, ok := id.(string); ok && s != "" {
			fields = append(fields, errfile.F("request_id", s))
		}
	}

	errfile.Submit(errfile.Submission{
		Level:        level,
		Doing:        doing,
		Type:         "logged",
		Message:      detail,
		Fields:       fields,
		Where:        errfile.CallerWhere(hookSkipPrefixes...),
		SkipPrefixes: hookSkipPrefixes,
		NoEcho:       true, // upstream already printed it (§11.5)
		NoDedupe:     true, // there is no error value to remember
	})

	// logrus exits right after a Fatal/Panic entry's hooks have run: the record must be on disk first (R9)
	if level == errfile.LevelFatal {
		errfile.Flush()
	}

	return nil
}

// splitBecause separates upstream's "[file.Func] what failed, because <err>" idiom into the doing
// (the static part, which keeps the fold key stable) and the detail (the variable part). A message
// without the idiom is both.
func splitBecause(message string) (doing, detail string) {
	if i := strings.LastIndex(message, ", because "); i > 0 {
		return message[:i], message[i+len(", because "):]
	}

	if i := strings.LastIndex(message, " because "); i > 0 {
		return message[:i], message[i+len(" because "):]
	}

	return message, message
}

var installOnce sync.Once

// InstallLogHook attaches the hook to upstream's boot, cli and default loggers, once.
func InstallLogHook() {
	installOnce.Do(func() { log.AddHook(hook{}) })
}

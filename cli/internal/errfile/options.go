// GENERATED from pkg/errfile — run scripts/sync-errfile.sh
package errfile

import (
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Options configure Install.
type Options struct {
	// App is the runtime tag (§3.3): "server", "app-cli", "ezbk".
	App string
	// Command names the subcommand, for the record: "server run", "database update".
	Command string
	// File overrides the resolved path (§3.1). Default: EZBK_ERROR_FILE, else the path table.
	File string
	// HandleSignals flushes on SIGINT, SIGTERM and SIGHUP, then re-raises the signal so the process
	// ends exactly as it would have. Only for a host with no signal handling of its own.
	HandleSignals bool
	// Development echoes every record to stderr (§11.5). EZBK_ERROR_FILE_ECHO=1 does the same.
	Development bool
	// StatementsRoot is the private statements directory (§12.7). EZBK_STATEMENTS_DIR is read too.
	StatementsRoot string
}

var installMu sync.Mutex
var signalsOnce sync.Once

// Install resolves the path, creates the writer, sets the sink and drains the pre-install queue.
// It returns a func that flushes, for hosts that want to call it on their own exit path. It is
// idempotent: a second call with the same App is a no-op; a second call with a different App is a
// programming error and is reported once as a WARN to the file itself.
func Install(o Options) (flush func()) {
	defer func() {
		if r := recover(); r != nil {
			lastResort("could not install the error file", r)
			flush = Flush
		}
	}()

	installMu.Lock()
	defer installMu.Unlock()

	if box := current(); box != nil {
		if box.app != o.App && o.App != "" {
			Submit(Submission{
				Level:   LevelWarn,
				Doing:   "installing the error file a second time",
				Type:    "logged",
				Message: "Install called with App " + o.App + " after " + box.app + "; the first install is kept",
				Fields:  []Field{F("command", o.Command)},
				Where:   ownWhere(),
			})
		}

		return Flush
	}

	root := o.StatementsRoot

	if root == "" {
		root = os.Getenv(EnvStatementsDir)
	}

	if root != "" {
		SetStatementsRoot(root)
	}

	file := strings.TrimSpace(o.File)

	if file == "" {
		file = DefaultPath()
	}

	writer := NewRollingFileWriter(WriterOptions{Path: file})
	setSink(&installed{
		sink:    writer,
		app:     o.App,
		command: o.Command,
		file:    file,
		echo:    o.Development || os.Getenv(EnvEcho) == "1",
	})

	if o.HandleSignals {
		signalsOnce.Do(installSignalFlush)
	}

	return Flush
}

// InstalledFile returns the file the process writes, or "" before Install.
func InstalledFile() string {
	if box := current(); box != nil {
		return box.file
	}

	return ""
}

// installSignalFlush flushes on a terminating signal, then restores the default disposition and
// re-raises it, so the process still dies of the signal — it never calls os.Exit and never
// swallows the signal.
func installSignalFlush() {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)

	go func() {
		sig := <-ch
		Flush()
		signal.Reset(sig)

		if s, ok := sig.(syscall.Signal); ok {
			_ = raise(s)
		}

		// If the re-raise did not end the process (a host handler swallowed it), give the host a
		// moment and then stop handling: we never own the exit.
		time.Sleep(200 * time.Millisecond)
	}()
}

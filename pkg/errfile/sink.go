package errfile

import (
	"os"
	"sync"
	"sync/atomic"
	"time"
)

// Sink is where records go once a host has booted. The Go sink wraps a RollingFileWriter; tests
// install a recording sink.
type Sink interface {
	Write(r *Record)
	Flush()
}

// PreInstallCap bounds the records held before any sink is installed (a package can fault in
// init(), or before initializeSystem has read the configuration).
const PreInstallCap = 200

// installed is the sink slot's content.
type installed struct {
	sink    Sink
	app     string
	command string
	file    string
	echo    bool
}

// libState is the process-global state of the library.
type libState struct {
	box      atomic.Pointer[installed]
	queueMu  sync.Mutex
	queue    []*Record
	overflow int
	reported *reportedSet
	folder   *folder
}

var state = &libState{reported: newReportedSet(), folder: newFolder(FoldMaxKeys, nil)}

func init() {
	state.folder.emit = emitSummary
}

func current() *installed {
	return state.box.Load()
}

// deliver hands a record to the sink, or to the pre-install queue.
func deliver(r *Record) {
	box := current()

	if box == nil {
		state.queueMu.Lock()

		if len(state.queue) < PreInstallCap {
			state.queue = append(state.queue, r)
		} else {
			state.overflow++
		}

		state.queueMu.Unlock()

		return
	}

	if r.App == "" {
		r.App = box.app
	}

	func() {
		defer func() {
			if rec := recover(); rec != nil {
				lastResort("the error sink panicked", rec)
			}
		}()
		box.sink.Write(r)
	}()

	if box.echo && !r.noEcho {
		echo(r)
	}
}

func echo(r *Record) {
	defer func() { _ = recover() }()
	_, _ = os.Stderr.WriteString(FormatRecord(r))
}

// setSink installs a sink and drains the pre-install queue into it.
func setSink(box *installed) {
	state.box.Store(box)

	if box == nil {
		return
	}

	state.queueMu.Lock()
	queued := state.queue
	overflow := state.overflow
	state.queue = nil
	state.overflow = 0
	state.queueMu.Unlock()

	for _, r := range queued {
		deliver(r)
	}

	if overflow > 0 {
		deliver(&Record{
			TS:    time.Now(),
			Level: LevelWarn,
			App:   box.app,
			Where: "pkg/errfile/sink.go",
			Doing: "installing the error file",
			Error: "logged: " + itoa(overflow) + " records were dropped before the error file was installed",
		})
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}

	neg := n < 0

	if neg {
		n = -n
	}

	var buf [20]byte
	i := len(buf)

	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}

	if neg {
		i--
		buf[i] = '-'
	}

	return string(buf[i:])
}

// Flush writes every owed fold summary, then flushes the sink synchronously. It is the barrier
// Fatal, the signal handler and the main() nets use.
func Flush() {
	defer func() {
		if r := recover(); r != nil {
			lastResort("could not flush the error file", r)
		}
	}()

	state.folder.flushAll()

	if box := current(); box != nil {
		box.sink.Flush()
	}
}

// ResetForTests forgets the sink, the queue, the fold table and the reported-set. Tests only.
func ResetForTests() {
	state.folder.reset()
	state.reported.reset()
	state.box.Store(nil)
	state.queueMu.Lock()
	state.queue = nil
	state.overflow = 0
	state.queueMu.Unlock()
	lastResortUsed.Store(false)
}

// InstallSinkForTests installs an arbitrary sink under an app name, draining the pre-install
// queue. Tests only; production code calls Install.
func InstallSinkForTests(app string, sink Sink) {
	setSink(&installed{sink: sink, app: app})
}

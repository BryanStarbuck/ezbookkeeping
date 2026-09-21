package errfile

import (
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"
)

// RollingFileWriter is a port of ~/BGit/all/app/code/packages/logging/src/rolling-file-writer.ts
// (§4.3), with files at mode 0600 and the directory at 0700.
//
// WRITES ARE BUFFERED AND ASYNCHRONOUS. Write appends to an in-memory buffer and returns without
// touching the filesystem. One 5 ms timer drains the buffer with ONE append call per batch, so a
// batch is one O_APPEND write and lines from different processes never interleave mid-line. The
// buffer is drained synchronously when it reaches 256 KiB (a runaway loop must not grow memory)
// and by Flush, which Fatal, the exit path and the signal handler call. A line is at risk for at
// most 5 ms, and only against a death that runs no Go at all (SIGKILL, power cut).
//
// Several processes may append to the same file (the server, ezbk and the MCP). Before a roll the
// REAL on-disk size is checked: a process that finds the file already rolled by another resets its
// cached size instead of rolling a second time.
type RollingFileWriter struct {
	path       string
	maxBytes   int64
	maxBackups int
	bufferCap  int
	fallback   func([]byte)

	mu      sync.Mutex // guards pending, timer, scheduled
	pending []byte
	timer   *time.Timer

	ioMu  sync.Mutex // sequences drains: one batch on disk before the next
	size  int64      // cached size of the active file
	ready bool
}

// Writer defaults (§4.3).
const (
	DefaultMaxBytes   = 5 * 1024 * 1024
	DefaultMaxBackups = 5
	FlushInterval     = 5 * time.Millisecond
	MaxBufferedBytes  = 256 * 1024
	fileMode          = 0o600
	dirMode           = 0o700
)

// WriterOptions configure a RollingFileWriter.
type WriterOptions struct {
	// Path is the absolute path of the active file.
	Path string
	// MaxBytes rolls the file once it reaches this many bytes. Default 5 MiB.
	MaxBytes int64
	// MaxBackups is how many rotated files (<file>.1 … <file>.N) to keep. Default 5.
	MaxBackups int
	// Fallback receives a batch that could not be written. Default: stderr.
	Fallback func([]byte)
}

// NewRollingFileWriter builds a writer. Nothing touches the filesystem until the first drain.
func NewRollingFileWriter(o WriterOptions) *RollingFileWriter {
	w := &RollingFileWriter{path: o.Path, maxBytes: o.MaxBytes, maxBackups: o.MaxBackups, fallback: o.Fallback}

	if w.maxBytes < 1024 {
		if o.MaxBytes == 0 {
			w.maxBytes = DefaultMaxBytes
		} else {
			w.maxBytes = 1024
		}
	}

	if o.MaxBackups == 0 {
		w.maxBackups = DefaultMaxBackups
	}

	if w.maxBackups < 0 {
		w.maxBackups = 0
	}

	w.bufferCap = MaxBufferedBytes

	if int64(w.bufferCap) > w.maxBytes {
		w.bufferCap = int(w.maxBytes)
	}

	if w.fallback == nil {
		w.fallback = func(b []byte) { _, _ = os.Stderr.Write(b) }
	}

	return w
}

// Path is the active file.
func (w *RollingFileWriter) Path() string {
	return w.path
}

// WriteLine buffers one formatted line. No filesystem work, except the overflow drain. Never panics.
func (w *RollingFileWriter) WriteLine(line string) {
	defer func() { _ = recover() }()

	w.mu.Lock()
	w.pending = append(w.pending, line...)

	if len(line) == 0 || line[len(line)-1] != '\n' {
		w.pending = append(w.pending, '\n')
	}

	overflow := len(w.pending) >= w.bufferCap

	if !overflow && w.timer == nil {
		w.timer = time.AfterFunc(FlushInterval, w.drain)
	}

	w.mu.Unlock()

	if overflow {
		w.Flush()
	}
}

// Write implements Sink over a formatted record.
func (w *RollingFileWriter) Write(r *Record) {
	w.WriteLine(FormatRecord(r))
}

// Flush is the synchronous barrier: everything buffered is on disk (or on stderr) when it returns.
func (w *RollingFileWriter) Flush() {
	defer func() { _ = recover() }()
	w.drain()
}

// drain takes the buffer and writes it as one batch, sequenced after any drain in flight.
func (w *RollingFileWriter) drain() {
	w.ioMu.Lock()
	defer w.ioMu.Unlock()

	w.mu.Lock()

	if w.timer != nil {
		w.timer.Stop()
		w.timer = nil
	}

	batch := w.pending
	w.pending = nil
	w.mu.Unlock()

	if len(batch) == 0 {
		return
	}

	w.emitBatch(batch)
}

// emitBatch runs under ioMu.
func (w *RollingFileWriter) emitBatch(batch []byte) {
	if !w.ready && !w.prepare() {
		w.fail(batch)
		return
	}

	if w.size+int64(len(batch)) > w.maxBytes {
		w.roll(int64(len(batch)))
	}

	f, err := os.OpenFile(w.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, fileMode)

	if err != nil {
		w.fail(batch)
		return
	}

	_, err = f.Write(batch)
	cerr := f.Close()

	if err != nil || cerr != nil {
		w.fail(batch)
		return
	}

	w.size += int64(len(batch))
}

// prepare creates the directory and seeds the cached size. First use, or after a failure.
func (w *RollingFileWriter) prepare() bool {
	if err := os.MkdirAll(filepath.Dir(w.path), dirMode); err != nil {
		return false
	}

	if st, err := os.Stat(w.path); err == nil {
		w.size = st.Size()
	} else {
		w.size = 0
	}

	w.ready = true

	return true
}

// fail sends a batch to the fallback; the next batch retries the mkdir.
func (w *RollingFileWriter) fail(batch []byte) {
	w.ready = false

	defer func() { _ = recover() }()
	w.fallback(batch)
}

// roll rotates <file> → <file>.1 → … → <file>.N. Synchronous and rare: a rename chain within one
// filesystem is ~1–2 ms. Another process may have rolled already, so the real size is read first.
func (w *RollingFileWriter) roll(incoming int64) {
	real := int64(0)

	if st, err := os.Stat(w.path); err == nil {
		real = st.Size()
	}

	if real < w.size {
		// Someone else rolled (or truncated) the file. Adopt the real size; roll only if needed.
		w.size = real

		if w.size+incoming <= w.maxBytes {
			return
		}
	}

	if w.maxBackups == 0 {
		_ = os.Remove(w.path)
		w.size = 0

		return
	}

	_ = os.Remove(w.path + "." + strconv.Itoa(w.maxBackups))

	for i := w.maxBackups - 1; i >= 1; i-- {
		from := w.path + "." + strconv.Itoa(i)

		if _, err := os.Stat(from); err == nil {
			_ = os.Rename(from, w.path+"."+strconv.Itoa(i+1))
		}
	}

	if _, err := os.Stat(w.path); err == nil {
		if err := os.Rename(w.path, w.path+".1"); err != nil {
			_ = os.Remove(w.path)
		}
	}

	w.size = 0
}

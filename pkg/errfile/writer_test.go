package errfile

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestWriterNoSyncFsOnHappyPath(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "sub", "error.err")
	w := NewRollingFileWriter(WriterOptions{Path: file})

	for i := 0; i < 1000; i++ {
		w.WriteLine("line " + strconv.Itoa(i))
	}

	// 1,000 writes and neither the directory nor the file exists yet: nothing touched the disk.
	if _, err := os.Stat(filepath.Dir(file)); err == nil {
		t.Fatalf("the writer touched the filesystem on the write path")
	}

	w.Flush()
	data, err := os.ReadFile(file)

	if err != nil {
		t.Fatal(err)
	}

	if lines := strings.Split(strings.TrimSuffix(string(data), "\n"), "\n"); len(lines) != 1000 || lines[999] != "line 999" {
		t.Errorf("lines = %d last = %q", len(lines), lines[len(lines)-1])
	}

	st, _ := os.Stat(file)

	if st.Mode().Perm() != 0o600 {
		t.Errorf("file mode = %v", st.Mode().Perm())
	}

	dst, _ := os.Stat(filepath.Dir(file))

	if dst.Mode().Perm() != 0o700 {
		t.Errorf("dir mode = %v", dst.Mode().Perm())
	}
}

func TestWriterDrainsAsyncWithin5ms(t *testing.T) {
	file := filepath.Join(t.TempDir(), "error.err")
	w := NewRollingFileWriter(WriterOptions{Path: file})
	w.WriteLine("hello")
	deadline := time.Now().Add(2 * time.Second)

	for time.Now().Before(deadline) {
		if data, err := os.ReadFile(file); err == nil && string(data) == "hello\n" {
			return
		}

		time.Sleep(time.Millisecond)
	}

	t.Fatal("the async drain never wrote the line")
}

func TestWriterOverflowDrainsSynchronously(t *testing.T) {
	file := filepath.Join(t.TempDir(), "error.err")
	w := NewRollingFileWriter(WriterOptions{Path: file})
	line := strings.Repeat("x", 1024)

	for i := 0; i < MaxBufferedBytes/1024; i++ {
		w.WriteLine(line)
	}

	st, err := os.Stat(file)

	if err != nil || st.Size() < MaxBufferedBytes {
		t.Fatalf("a burst over the cap must be on disk synchronously: %v %v", err, st)
	}
}

func TestWriterRotatesAtCap(t *testing.T) {
	file := filepath.Join(t.TempDir(), "error.err")
	w := NewRollingFileWriter(WriterOptions{Path: file, MaxBytes: 4096, MaxBackups: 2})
	line := strings.Repeat("y", 1000)

	for i := 0; i < 20; i++ {
		w.WriteLine(line)
		w.Flush()
	}

	for _, name := range []string{file, file + ".1", file + ".2"} {
		st, err := os.Stat(name)

		if err != nil {
			t.Errorf("%s missing", name)
			continue
		}

		if st.Size() > 4096 {
			t.Errorf("%s is %d bytes", name, st.Size())
		}
	}

	if _, err := os.Stat(file + ".3"); err == nil {
		t.Errorf("too many backups kept")
	}
}

func TestWriterFailingPathFallsBackAndNeverPanics(t *testing.T) {
	var fallback bytes.Buffer
	bad := filepath.Join(t.TempDir(), "file-not-dir")

	if err := os.WriteFile(bad, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	w := NewRollingFileWriter(WriterOptions{Path: filepath.Join(bad, "error.err"), Fallback: func(b []byte) { fallback.Write(b) }})
	w.WriteLine("lost")
	w.Flush()

	if fallback.String() != "lost\n" {
		t.Errorf("fallback = %q", fallback.String())
	}

	// a fallback that itself panics is contained
	w2 := NewRollingFileWriter(WriterOptions{Path: filepath.Join(bad, "error.err"), Fallback: func([]byte) { panic("no") }})
	w2.WriteLine("lost")
	w2.Flush()
}

// ── multi-process (§4.3): two processes appending concurrently never interleave; one rotates ──

const helperEnv = "EZBK_ERRFILE_TEST_HELPER"

// TestHelperProcess is the body of the child processes below. It is not a real test.
func TestHelperProcess(t *testing.T) {
	mode := os.Getenv(helperEnv)

	if mode == "" {
		return
	}

	file := os.Getenv("EZBK_ERROR_FILE")

	switch mode {
	case "append":
		tag := os.Getenv("EZBK_ERRFILE_TAG")
		w := NewRollingFileWriter(WriterOptions{Path: file, MaxBytes: 64 * 1024})

		for i := 0; i < 400; i++ {
			w.WriteLine(fmt.Sprintf("%s|%04d|%s|END", tag, i, strings.Repeat(tag, 200)))

			if i%50 == 0 {
				time.Sleep(time.Millisecond)
			}
		}

		w.Flush()
		os.Exit(0)
	case "crash":
		Install(Options{App: "app-cli", File: file})
		Caught("first", errors.New("one"))
		Caught("second", errors.New("two"))
		Caught("third", errors.New("three"))
		// the main() net (G8): FATAL, flushed, then the host exits
		Fatal("running ezbookkeeping", errors.New("cannot open database"), F("command", "database"))
		os.Exit(3)
	case "panic":
		Install(Options{App: "app-cli", File: file})
		Caught("before the panic", errors.New("one"))

		func() {
			defer func() {
				if r := recover(); r != nil {
					Recovered("running the helper", r)
					Flush()
					os.Exit(4)
				}
			}()
			panic("main goroutine panic")
		}()
	}

	os.Exit(0)
}

func runHelper(t *testing.T, mode, file string, extra ...string) *exec.Cmd {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestHelperProcess$")
	cmd.Env = append(os.Environ(), helperEnv+"="+mode, "EZBK_ERROR_FILE="+file)
	cmd.Env = append(cmd.Env, extra...)

	return cmd
}

func TestMultiProcessAppendNeverInterleaves(t *testing.T) {
	file := filepath.Join(t.TempDir(), "error.err")
	a := runHelper(t, "append", file, "EZBK_ERRFILE_TAG=a")
	b := runHelper(t, "append", file, "EZBK_ERRFILE_TAG=b")

	if err := a.Start(); err != nil {
		t.Fatal(err)
	}

	if err := b.Start(); err != nil {
		t.Fatal(err)
	}

	if err := a.Wait(); err != nil {
		t.Fatal(err)
	}

	if err := b.Wait(); err != nil {
		t.Fatal(err)
	}

	total := 0
	rotated := 0

	for _, name := range []string{file, file + ".1", file + ".2", file + ".3", file + ".4", file + ".5"} {
		data, err := os.ReadFile(name)

		if err != nil {
			continue
		}

		if name != file {
			rotated++
		}

		for _, line := range strings.Split(strings.TrimSuffix(string(data), "\n"), "\n") {
			parts := strings.Split(line, "|")

			if len(parts) != 4 || parts[3] != "END" || parts[2] != strings.Repeat(parts[0], 200) {
				t.Fatalf("interleaved or torn line in %s: %.80q", name, line)
			}

			total++
		}
	}

	if total != 800 {
		t.Errorf("lines = %d (want 800)", total)
	}

	if rotated == 0 {
		t.Errorf("expected at least one rotation with a 64 KiB cap")
	}
}

// ── crash tests (§15.2) ────────────────────────────────────────────────────────────────────────

func TestCrashFatalRecordsSurvive(t *testing.T) {
	file := filepath.Join(t.TempDir(), "error.err")
	cmd := runHelper(t, "crash", file)
	err := cmd.Run()
	var exit *exec.ExitError

	if !errors.As(err, &exit) || exit.ExitCode() != 3 {
		t.Fatalf("child exit: %v", err)
	}

	data, rerr := os.ReadFile(file)

	if rerr != nil {
		t.Fatal(rerr)
	}

	text := string(data)

	for _, want := range []string{"[ERROR] [app-cli] [" + here() + ":", "first — *errors.errorString: one", "second —", "third —", "[FATAL] [app-cli] [" + here() + ":", "running ezbookkeeping — *errors.errorString: cannot open database {command=database}"} {
		if !strings.Contains(text, want) {
			t.Errorf("missing %q in:\n%s", want, text)
		}
	}
}

func TestCrashPanicRecordSurvives(t *testing.T) {
	file := filepath.Join(t.TempDir(), "error.err")
	cmd := runHelper(t, "panic", file)
	err := cmd.Run()
	var exit *exec.ExitError

	if !errors.As(err, &exit) || exit.ExitCode() != 4 {
		t.Fatalf("child exit: %v", err)
	}

	data, _ := os.ReadFile(file)
	text := string(data)

	if !strings.Contains(text, "before the panic —") || !strings.Contains(text, "running the helper — panic: main goroutine panic") || !strings.Contains(text, "    at TestHelperProcess.func") {
		t.Errorf("text = %s", text)
	}
}

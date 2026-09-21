// GENERATED from pkg/errfile — run scripts/sync-errfile.sh
package errfile

import (
	"errors"
	"io/fs"
	"os"
	"strings"
	"syscall"
	"testing"
)

// ── install ────────────────────────────────────────────────────────────────────────────────────

func TestInstallIsIdempotentAndWarnsOnDifferentApp(t *testing.T) {
	ResetForTests()
	t.Cleanup(ResetForTests)
	dir := t.TempDir()
	file := dir + "/error.err"
	flush := Install(Options{App: "server", File: file})
	Install(Options{App: "server", File: file})
	Install(Options{App: "ezbk", File: file})
	Caught("testing install", errors.New("x"))
	flush()
	data, err := os.ReadFile(file)

	if err != nil {
		t.Fatal(err)
	}

	if st, _ := os.Stat(file); st.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v", st.Mode().Perm())
	}

	text := string(data)

	warnAt := "[WARN] [server] [" + strings.Replace(here(), "options_test.go", "options.go", 1) + ":"

	if !strings.Contains(text, warnAt) || !strings.Contains(text, "] installing the error file a second time — logged: Install called with App ezbk after server") {
		t.Errorf("no warn for the second app: %q", text)
	}

	if !strings.Contains(text, "[ERROR] [server] ["+here()+":") || InstalledFile() != file {
		t.Errorf("file = %q", text)
	}
}

func TestInstallLearnsStatementsRoot(t *testing.T) {
	ResetForTests()
	t.Cleanup(func() { ResetForTests(); SetStatementsRoot("") })
	s := &recordingSink{}
	t.Setenv(EnvStatementsDir, "/home/operator/private/bank_statements")
	flush := Install(Options{App: "server", File: t.TempDir() + "/e.err"})
	state.box.Store(&installed{sink: s, app: "server"})
	Caught("opening the statement", &fs.PathError{Op: "open", Path: "/home/operator/private/bank_statements/2026/x.ofx", Err: syscall.ENOENT})
	flush()

	if lines := s.lines(); len(lines) != 1 || !strings.Contains(lines[0], "open <statements>/…/x.ofx: no such file or directory") || strings.Contains(lines[0], "private") {
		t.Errorf("lines = %v", lines)
	}
}

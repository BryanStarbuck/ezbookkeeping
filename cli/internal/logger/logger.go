// Package logger writes cli.info and cli.err under ~/T/_ezbookkeeping (cli.mdx §16). File only for
// INFO; WARN/ERROR also reach stderr through the caller. It never logs the key, an amount, a
// comment or an account name, and it can never crash the CLI.
package logger

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/BryanStarbuck/ezbookkeeping/cli/internal/errfile"
)

const rotateBytes = 8 << 20
const generations = 5

// StateDir is ~/T/_ezbookkeeping (EZBK_STATE_DIR overrides)
func StateDir() string {
	if d := strings.TrimSpace(os.Getenv("EZBK_STATE_DIR")); d != "" {
		return d
	}

	home, err := os.UserHomeDir()

	if err != nil {
		errfile.Expected("finding the home directory for the cli state dir", err)
		return filepath.Join(os.TempDir(), "_ezbookkeeping")
	}

	return filepath.Join(home, "T", "_ezbookkeeping")
}

func write(file, level, format string, args ...any) {
	defer errfile.RecoverNet("writing the cli log line")()

	dir := StateDir()

	if err := os.MkdirAll(dir, 0o700); err != nil {
		errfile.Caught("creating the cli state directory", err)
		return
	}

	path := filepath.Join(dir, file)
	rotate(path)

	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)

	if err != nil {
		errfile.Caught("opening the cli log file", err, errfile.F("file", file))
		return
	}

	defer f.Close()

	line := fmt.Sprintf(format, args...)
	line = strings.ReplaceAll(line, "\n", " ")
	_, _ = fmt.Fprintf(f, "[%s] [%s] %s\n", time.Now().Format(time.RFC3339), level, line)
}

func rotate(path string) {
	info, err := os.Stat(path)

	if err != nil || info.Size() < rotateBytes {
		errfile.Expected("checking the cli log file size before rotating", err)
		return
	}

	for i := generations - 1; i >= 1; i-- {
		_ = os.Rename(fmt.Sprintf("%s.%d", path, i), fmt.Sprintf("%s.%d", path, i+1))
	}

	_ = os.Rename(path, path+".1")
}

// Info logs to cli.info only
func Info(format string, args ...any) { write("cli.info", "INFO", format, args...) }

// APICall logs one machine-plane call: route, duration, outcome — never arguments' values
func APICall(method, path string, took time.Duration, outcome string) {
	write("cli.info", "API_CALL", "%s %s took=%dms outcome=%s", method, path, took.Milliseconds(), outcome)
}

// Warn logs to cli.err
func Warn(format string, args ...any) { write("cli.err", "WARN", format, args...) }

// Error logs to cli.err
func Error(format string, args ...any) { write("cli.err", "ERROR", format, args...) }

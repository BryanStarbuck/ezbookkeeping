package errfile

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

// EnvFile is the full-path override for the error file (§3.1).
const EnvFile = "EZBK_ERROR_FILE"

// fileName is the active file's basename; rotated copies are error.err.1 … error.err.5.
const fileName = "error.err"

// ResolvePath applies the path table of §3.1 to an explicit environment, in this order:
//
//  1. EZBK_ERROR_FILE, when set — even under tests;
//  2. under tests (R13): $TMPDIR/ezbookkeeping_test_<uid>/error.err;
//  3. $HOME/T/ezbookkeeping/error.err;
//  4. no home directory: $TMPDIR/ezbookkeeping_<uid>/error.err.
//
// `lookup` returns an environment variable and whether it is set; `isTest` is testing.Testing();
// `uid` is the numeric user id. It is exported so the vectors test can drive it with a synthetic
// environment.
func ResolvePath(lookup func(string) (string, bool), isTest bool, uid int) string {
	if v, ok := lookup(EnvFile); ok && v != "" {
		return v
	}

	tmp, ok := lookup("TMPDIR")

	if !ok || tmp == "" {
		tmp = os.TempDir()
	}

	if isTest {
		return filepath.Join(tmp, "ezbookkeeping_test_"+strconv.Itoa(uid), fileName)
	}

	if home, ok := lookup("HOME"); ok && home != "" {
		return filepath.Join(home, "T", "ezbookkeeping", fileName)
	}

	return filepath.Join(tmp, "ezbookkeeping_"+strconv.Itoa(uid), fileName)
}

// DefaultPath resolves the error file for this process from the real environment.
func DefaultPath() string {
	return ResolvePath(lookupEnv, testing.Testing(), os.Getuid())
}

func lookupEnv(name string) (string, bool) {
	v, ok := os.LookupEnv(name)

	if name == "HOME" && (!ok || v == "") {
		// Windows and a stripped environment: ask the OS.
		if home, err := os.UserHomeDir(); err == nil && home != "" {
			return home, true
		}
	}

	return v, ok
}

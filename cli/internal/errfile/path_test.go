// GENERATED from pkg/errfile — run scripts/sync-errfile.sh
package errfile

import (
	"strings"
	"testing"
)

func TestDefaultPathUnderTestsNeverTheRealFile(t *testing.T) {
	p := DefaultPath()

	if strings.Contains(p, "/T/ezbookkeeping/") || !strings.Contains(p, "ezbookkeeping_test_") {
		t.Errorf("test path = %q", p)
	}

	t.Setenv(EnvFile, "/x/y/error.err")

	if DefaultPath() != "/x/y/error.err" {
		t.Errorf("override ignored")
	}
}

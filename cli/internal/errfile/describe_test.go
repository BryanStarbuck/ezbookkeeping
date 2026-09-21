// GENERATED from pkg/errfile — run scripts/sync-errfile.sh
package errfile

import (
	"errors"
	"strings"
	"testing"
)

// ── where (R14) ────────────────────────────────────────────────────────────────────────────────

func TestWhereIsRepoRelativeFileAndLine(t *testing.T) {
	s := newSink(t, "server")
	Caught("testing where", errors.New("boom"))
	recs := s.all()

	if len(recs) != 1 {
		t.Fatalf("records = %d", len(recs))
	}

	if !strings.HasPrefix(recs[0].Where, here()+":") {
		t.Errorf("where = %q", recs[0].Where)
	}

	if !strings.Contains(recs[0].Error, "*errors.errorString: boom") {
		t.Errorf("error = %q", recs[0].Error)
	}

	if len(recs[0].Stack) == 0 || !strings.HasPrefix(recs[0].Stack[0], "TestWhereIsRepoRelativeFileAndLine ("+here()+":") {
		t.Errorf("stack = %v", recs[0].Stack)
	}

	for _, fr := range recs[0].Stack {
		if strings.Contains(fr, "errfile/errfile.go") || strings.Contains(fr, "errfile/record.go") || strings.HasPrefix(fr, "runtime.") {
			t.Errorf("own frame kept: %q", fr)
		}
	}
}

func TestRelPath(t *testing.T) {
	cases := map[string]string{
		"/Users/op/BGit/Bryan_git/ezbookkeeping/pkg/machine/mount.go":       "pkg/machine/mount.go",
		"/Users/op/BGit/Bryan_git/ezbookkeeping/cli/internal/app/main.go":   "cli/internal/app/main.go",
		"/Users/op/BGit/Bryan_git/ezbookkeeping/ezbookkeeping.go":           "ezbookkeeping.go",
		"github.com/mayswind/ezbookkeeping/pkg/services/accounts.go":        "pkg/services/accounts.go",
		"github.com/BryanStarbuck/ezbookkeeping/cli/internal/commands/x.go": "cli/internal/commands/x.go",
		"/home/op/go/pkg/mod/github.com/gin-gonic/gin@v1.10.0/context.go":   "github.com/gin-gonic/gin@v1.10.0/context.go",
		"/opt/homebrew/Cellar/go/1.27.1/libexec/src/runtime/panic.go":       "/opt/homebrew/Cellar/go/1.27.1/libexec/src/runtime/panic.go",
		"/srv/other-name/pkg/x.go":                                          "pkg/x.go",
		`C:\Users\op\src\ezbookkeeping\pkg\x.go`:                            "pkg/x.go",
	}

	for in, want := range cases {
		if got := relPath(in); got != want {
			t.Errorf("relPath(%q) = %q want %q", in, got, want)
		}
	}
}

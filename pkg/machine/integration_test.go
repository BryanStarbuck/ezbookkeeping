package machine

import (
	"path/filepath"
	"sync"
	"testing"

	"github.com/mayswind/ezbookkeeping/pkg/errs"
)

// apis.mdx §5.4 rule 2: simultaneous mints converge on the key the file ends up holding
func TestIntegConcurrentMintsConverge(t *testing.T) {
	for round := 0; round < 10; round++ {
		dir := t.TempDir()
		t.Setenv("EZBK_CREDENTIALS_FILE", filepath.Join(dir, "creds.json"))
		t.Setenv("EZBK_STATE_DIR", dir)

		var wg sync.WaitGroup
		keys := make([]string, 4)
		errsOut := make([]error, 4)

		for i := range keys {
			wg.Add(1)

			go func(i int) {
				defer wg.Done()
				keys[i], errsOut[i] = MintIntoCredentials("test", false)
			}(i)
		}

		wg.Wait()

		creds, err := ReadCredentials()

		if err != nil {
			t.Fatalf("read: %v", err)
		}

		for i, k := range keys {
			if errsOut[i] != nil {
				t.Fatalf("mint %d: %v", i, errsOut[i])
			}

			if k != creds.APIKey {
				t.Fatalf("round %d: mint %d returned a key that is not in the file", round, i)
			}
		}
	}
}

func TestIntegNotPermittedIsForbidden(t *testing.T) {
	if f := Upstream(errs.ErrNotPermittedToPerformThisAction); f.Code != CodeForbidden {
		t.Fatalf("got %s", f.Code)
	}
}

func TestIntegFallbackLookupUnknownRun(t *testing.T) {
	t.Setenv("EZBK_STATE_DIR", t.TempDir())

	ids, found, err := integRunFallbackLookup(&Ctx{Uid: 1}, "run_20260101T000000Z_deadbeef")

	if err != nil || found || len(ids) != 0 {
		t.Fatalf("unknown run: ids=%v found=%v err=%v", ids, found, err)
	}
}

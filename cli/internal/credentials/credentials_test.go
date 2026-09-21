package credentials

import (
	"path/filepath"
	"sync"
	"testing"
)

// concurrent mints converge on the key the file ends up holding (apis.mdx §5.4 rule 2)
func TestMintConcurrentConverges(t *testing.T) {
	for round := 0; round < 10; round++ {
		dir := t.TempDir()
		t.Setenv("EZBK_CREDENTIALS_FILE", filepath.Join(dir, "creds.json"))
		t.Setenv("EZBK_STATE_DIR", dir)

		keys := make([]string, 4)
		errs := make([]error, 4)
		var wg sync.WaitGroup

		for i := range keys {
			wg.Add(1)

			go func(i int) {
				defer wg.Done()
				keys[i], errs[i] = Mint("test", false)
			}(i)
		}

		wg.Wait()

		for i := range keys {
			if errs[i] != nil {
				t.Fatal(errs[i])
			}

			if keys[i] != keys[0] {
				t.Fatalf("round %d: mints disagree", round)
			}
		}
	}
}

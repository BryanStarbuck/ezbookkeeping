package errfile

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// vendor_drift_test.go — cli/internal/errfile is a byte-identical copy of this directory (§4.1),
// each .go file prefixed with one header line. This test fails when any file differs after the
// header, in either direction, and checks that the copy builds and passes inside the cli module.
// It is the one file here that is NOT vendored (scripts/sync-errfile.sh skips it).

const vendorHeader = "// GENERATED from pkg/errfile — run scripts/sync-errfile.sh\n"

func repoRoot(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()

	if err != nil {
		t.Fatal(err)
	}

	root := filepath.Dir(filepath.Dir(wd)) // pkg/errfile → repo root

	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Skipf("not running from pkg/errfile inside the repo: %v", err)
	}

	return root
}

func listVendorable(t *testing.T, dir string, withHeader bool) map[string][]byte {
	t.Helper()
	out := map[string][]byte{}
	entries, err := os.ReadDir(dir)

	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}

	for _, e := range entries {
		name := e.Name()

		if e.IsDir() || !strings.HasSuffix(name, ".go") || name == "vendor_drift_test.go" {
			continue
		}

		data, err := os.ReadFile(filepath.Join(dir, name))

		if err != nil {
			t.Fatal(err)
		}

		if withHeader {
			if !bytes.HasPrefix(data, []byte(vendorHeader)) {
				t.Errorf("%s: missing the GENERATED header", filepath.Join(dir, name))
			}

			data = bytes.TrimPrefix(data, []byte(vendorHeader))
		}

		out[name] = data
	}

	vectors, err := filepath.Glob(filepath.Join(dir, "testdata", "*.json"))

	if err == nil {
		for _, v := range vectors {
			data, err := os.ReadFile(v)

			if err == nil {
				out["testdata/"+filepath.Base(v)] = data
			}
		}
	}

	return out
}

func TestVendorCopyMatchesByteForByte(t *testing.T) {
	root := repoRoot(t)
	src := listVendorable(t, filepath.Join(root, "pkg", "errfile"), false)
	dst := listVendorable(t, filepath.Join(root, "cli", "internal", "errfile"), true)
	var names []string

	for n := range src {
		names = append(names, n)
	}

	for n := range dst {
		if _, ok := src[n]; !ok {
			names = append(names, n)
		}
	}

	sort.Strings(names)
	drift := false

	for _, n := range names {
		s, inSrc := src[n]
		d, inDst := dst[n]

		switch {
		case !inDst:
			t.Errorf("cli/internal/errfile/%s is missing", n)
			drift = true
		case !inSrc:
			t.Errorf("cli/internal/errfile/%s has no source in pkg/errfile", n)
			drift = true
		case !bytes.Equal(s, d):
			t.Errorf("cli/internal/errfile/%s differs from pkg/errfile/%s after the header", n, n)
			drift = true
		}
	}

	if drift {
		t.Log("run scripts/sync-errfile.sh to regenerate the copy")
	}
}

func TestVendorCopyBuildsAndPassesInCliModule(t *testing.T) {
	if testing.Short() {
		t.Skip("short mode")
	}

	root := repoRoot(t)
	cli := filepath.Join(root, "cli")
	goBin, err := exec.LookPath("go")

	if err != nil {
		t.Skip("no go binary on PATH")
	}

	for _, args := range [][]string{{"build", "./..."}, {"test", "-count=1", "./internal/errfile/..."}} {
		cmd := exec.Command(goBin, args...)
		cmd.Dir = cli
		out, err := cmd.CombinedOutput()

		if err != nil {
			t.Fatalf("cd cli && go %s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}
}

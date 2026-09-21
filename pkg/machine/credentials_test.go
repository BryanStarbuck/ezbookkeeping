package machine

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// credentials_test.go — apis.mdx §5.2–§5.5, §20, §21 "Credentials"

type jrCredVectors struct {
	WellFormed []struct {
		Key         string `json:"key"`
		Fingerprint string `json:"fingerprint"`
	} `json:"wellFormed"`
	Malformed []struct {
		Key string `json:"key"`
		Why string `json:"why"`
	} `json:"malformed"`
	CredentialsFile map[string]json.RawMessage `json:"credentialsFile"`
}

func jrLoadVectors(t *testing.T) *jrCredVectors {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "credentials_vectors.json"))

	if err != nil {
		t.Fatal(err)
	}

	var v jrCredVectors

	if err := json.Unmarshal(data, &v); err != nil {
		t.Fatal(err)
	}

	return &v
}

func jrCredsPath(t *testing.T) string {
	t.Helper()
	p, err := CredentialsPath()

	if err != nil {
		t.Fatal(err)
	}

	return p
}

func jrWriteFile(t *testing.T, path string, data []byte, mode os.FileMode) {
	t.Helper()

	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(path, data, mode); err != nil {
		t.Fatal(err)
	}

	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
}

func TestCredentialsMintShape(t *testing.T) {
	seen := map[string]bool{}
	shape := regexp.MustCompile(`^[0-9a-f]{64}$`)

	for i := 0; i < 64; i++ {
		k, err := MintKey()

		if err != nil {
			t.Fatal(err)
		}

		if !shape.MatchString(k) || !IsWellFormedKey(k) {
			t.Fatalf("minted key %q is not 64 lowercase hex", k)
		}

		if seen[k] {
			t.Fatal("two mints produced the same key")
		}

		seen[k] = true
	}
}

func TestCredentialsVectors(t *testing.T) {
	v := jrLoadVectors(t)

	if len(v.WellFormed) == 0 || len(v.Malformed) == 0 {
		t.Fatal("vectors file is empty")
	}

	for _, w := range v.WellFormed {
		if !IsWellFormedKey(w.Key) {
			t.Errorf("%q should be well formed", w.Key)
		}

		if got := Fingerprint(w.Key); got != w.Fingerprint {
			t.Errorf("Fingerprint(%s…) = %s, want %s", w.Key[:4], got, w.Fingerprint)
		}

		if strings.Contains(Fingerprint(w.Key), w.Key[4:12]) {
			t.Errorf("fingerprint carries more than the first four characters of the key")
		}
	}

	for _, m := range v.Malformed {
		if IsWellFormedKey(m.Key) {
			t.Errorf("%q (%s) must be refused", m.Key, m.Why)
		}
	}

	if Fingerprint("") != "none" {
		t.Error("the fingerprint of no key is \"none\"")
	}
}

func TestCredentialsMintCreatesPrivateFile(t *testing.T) {
	jrIsolate(t)
	path := jrCredsPath(t)

	key, err := MintIntoCredentials("ezbk", false)

	if err != nil {
		t.Fatal(err)
	}

	if !IsWellFormedKey(key) {
		t.Fatalf("minted %q", key)
	}

	info, err := os.Stat(path)

	if err != nil {
		t.Fatal(err)
	}

	if info.Mode().Perm() != 0o600 {
		t.Fatalf("credentials file mode = %o, want 600", info.Mode().Perm())
	}

	dirInfo, _ := os.Stat(filepath.Dir(path))

	if dirInfo.Mode().Perm()&0o077 != 0 {
		t.Fatalf("credentials dir mode = %o, want 700", dirInfo.Mode().Perm())
	}

	creds, err := ReadCredentials()

	if err != nil || creds.APIKey != key || creds.CreatedBy != "ezbk" || creds.Created == "" {
		t.Fatalf("read back %+v err=%v", creds, err)
	}

	entries, _ := os.ReadDir(filepath.Dir(path))

	for _, e := range entries {
		if strings.Contains(e.Name(), ".tmp-") {
			t.Fatalf("temp file left behind: %s", e.Name())
		}
	}
}

func TestCredentialsMergePreservesOtherProducts(t *testing.T) {
	jrIsolate(t)
	v := jrLoadVectors(t)
	path := jrCredsPath(t)
	before, _ := json.Marshal(v.CredentialsFile)
	jrWriteFile(t, path, before, 0o600)

	key, err := MintIntoCredentials("server", false)

	if err != nil {
		t.Fatal(err)
	}

	data, _ := os.ReadFile(path)
	var after map[string]json.RawMessage

	if err := json.Unmarshal(data, &after); err != nil {
		t.Fatal(err)
	}

	for product, raw := range v.CredentialsFile {
		if product == credentialsProduct {
			continue
		}

		if !jrJSONEqual(t, raw, after[product]) {
			t.Errorf("product %q changed:\nbefore %s\nafter  %s", product, raw, after[product])
		}
	}

	creds, err := ReadCredentials()

	if err != nil {
		t.Fatal(err)
	}

	// our own subtree keeps the keys the mint does not own
	if creds.APIKey != key || creds.Username != "operator" || creds.Timezone != "America/Los_Angeles" || creds.StatementsRoot != "/tmp/synthetic_statements" || creds.Label != "synthetic-host" {
		t.Fatalf("own subtree after mint = %+v", creds)
	}
}

func jrJSONEqual(t *testing.T, a, b json.RawMessage) bool {
	t.Helper()
	var x, y any

	if err := json.Unmarshal(a, &x); err != nil {
		return false
	}

	if err := json.Unmarshal(b, &y); err != nil {
		return false
	}

	xa, _ := json.Marshal(x)
	ya, _ := json.Marshal(y)

	return string(xa) == string(ya)
}

func TestCredentialsTooPermissiveRefused(t *testing.T) {
	jrIsolate(t)
	path := jrCredsPath(t)
	jrWriteFile(t, path, []byte(`{"ezbookkeeping":{"machine":{"api_key":"`+strings.Repeat("0", 64)+`"}}}`), 0o644)

	if _, err := ReadCredentials(); !errors.Is(err, ErrCredentialsTooPermissive) {
		t.Fatalf("0644 must be refused, got %v", err)
	}

	if _, err := MintIntoCredentials("server", true); err == nil {
		t.Fatal("minting into a 0644 file must be refused")
	}

	if _, _, err := ResolveKey("server", true); err == nil {
		t.Fatal("resolving from a 0644 file must be refused")
	}

	if err := os.Chmod(path, 0o640); err != nil {
		t.Fatal(err)
	}

	if _, err := ReadCredentials(); !errors.Is(err, ErrCredentialsTooPermissive) {
		t.Fatalf("0640 must be refused, got %v", err)
	}
}

func TestCredentialsSymlinkRefused(t *testing.T) {
	dir := jrIsolate(t)
	path := jrCredsPath(t)
	real := filepath.Join(dir, "elsewhere.json")
	jrWriteFile(t, real, []byte(`{}`), 0o600)

	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}

	if err := os.Symlink(real, path); err != nil {
		t.Fatal(err)
	}

	if _, err := ReadCredentials(); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("read through a symlink must be refused, got %v", err)
	}

	if _, err := MintIntoCredentials("server", true); err == nil {
		t.Fatal("write through a symlink must be refused")
	}

	// the target was never written
	if data, _ := os.ReadFile(real); string(data) != "{}" {
		t.Fatalf("symlink target was modified: %s", data)
	}

	// and the write path refuses even when called directly
	if err := writeCredentialsAtomic(path, map[string]json.RawMessage{}); err == nil {
		t.Fatal("writeCredentialsAtomic must refuse a symlinked destination")
	}
}

func TestCredentialsCompareAndSet(t *testing.T) {
	jrIsolate(t)

	first, err := MintIntoCredentials("server", false)

	if err != nil {
		t.Fatal(err)
	}

	// a second minter that is not forcing yields to the key already there
	second, err := MintIntoCredentials("ezbk", false)

	if err != nil || second != first {
		t.Fatalf("non-forced mint replaced the existing key (err=%v)", err)
	}

	creds, _ := ReadCredentials()

	if creds.CreatedBy != "server" {
		t.Fatalf("created_by changed to %q on a yielded mint", creds.CreatedBy)
	}

	// rotation (force) replaces it
	third, err := MintIntoCredentials("ezbk", true)

	if err != nil || third == first || !IsWellFormedKey(third) {
		t.Fatalf("forced mint: key unchanged or malformed (err=%v)", err)
	}
}

func TestCredentialsMalformedStoredKeyNotReused(t *testing.T) {
	jrIsolate(t)
	path := jrCredsPath(t)
	jrWriteFile(t, path, []byte(`{"ezbookkeeping":{"machine":{"api_key":"NOT-A-KEY"}}}`), 0o600)

	if _, _, err := ResolveKey("server", true); err == nil || !strings.Contains(err.Error(), "rotate") {
		t.Fatalf("a malformed stored key must be refused with the rotate hint, got %v", err)
	}
}

func TestCredentialsResolutionOrder(t *testing.T) {
	dir := jrIsolate(t)
	v := jrLoadVectors(t)
	envKey := v.WellFormed[0].Key
	fileKey := v.WellFormed[1].Key
	storedKey := v.WellFormed[2].Key

	jrWriteFile(t, jrCredsPath(t), []byte(`{"ezbookkeeping":{"machine":{"api_key":"`+storedKey+`"}}}`), 0o600)
	keyFile := filepath.Join(dir, "docker-secret")
	jrWriteFile(t, keyFile, []byte(fileKey+"\n"), 0o600)

	t.Setenv("EZBK_API_KEY", envKey)
	t.Setenv("EZBK_API_KEY_FILE", keyFile)

	if k, src, err := ResolveKey("server", true); err != nil || k != envKey || src != "EZBK_API_KEY" {
		t.Fatalf("step 1: %s %s %v", Fingerprint(k), src, err)
	}

	t.Setenv("EZBK_API_KEY", "")

	if k, src, err := ResolveKey("server", true); err != nil || k != fileKey || src != "EZBK_API_KEY_FILE" {
		t.Fatalf("step 2: %s %s %v", Fingerprint(k), src, err)
	}

	t.Setenv("EZBK_API_KEY_FILE", "")

	if k, _, err := ResolveKey("server", true); err != nil || k != storedKey {
		t.Fatalf("step 3: %s %v", Fingerprint(k), err)
	}

	// a malformed override is refused, never trimmed or lowercased
	t.Setenv("EZBK_API_KEY", strings.ToUpper(envKey))

	if _, _, err := ResolveKey("server", true); err == nil {
		t.Fatal("an uppercase EZBK_API_KEY must be refused")
	}
}

func TestCredentialsNoMintWhenNotAllowed(t *testing.T) {
	jrIsolate(t)

	k, _, err := ResolveKey("ezbk", false)

	if err != nil || k != "" {
		t.Fatalf("got %q %v", k, err)
	}

	if _, err := os.Stat(jrCredsPath(t)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("a resolve that may not mint created the credentials file")
	}

	// the server's resolve mints, and a later resolve reuses that key unchanged
	minted, _, err := ResolveKey("server", true)

	if err != nil || !IsWellFormedKey(minted) {
		t.Fatalf("server mint: %v", err)
	}

	again, _, err := ResolveKey("server", true)

	if err != nil || again != minted {
		t.Fatal("the server must reuse a pre-existing key unchanged")
	}
}

// §5.2: minting is crypto/rand only; math/rand appears nowhere in the plane or the CLI
func TestCredentialsNoMathRand(t *testing.T) {
	for _, root := range []string{".", filepath.Join("..", "..", "cli")} {
		_ = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
			if err != nil || info.IsDir() || !strings.HasSuffix(path, ".go") {
				return nil
			}

			data, rerr := os.ReadFile(path)

			if rerr != nil {
				return nil
			}

			// the needle is assembled so this file does not match itself
			needle := `"math/` + `rand`

			if strings.Contains(string(data), needle+`"`) || strings.Contains(string(data), needle+`/v2"`) {
				t.Errorf("%s imports math/rand", path)
			}

			return nil
		})
	}

	src, err := os.ReadFile("credentials.go")

	if err != nil || !strings.Contains(string(src), `"crypto/rand"`) {
		t.Fatal("credentials.go must mint with crypto/rand")
	}
}

// §20: no 64-hex literal (a key-shaped secret) in the plane's or the CLI's non-test sources
func TestCredentialsNoKeyLiteralInTree(t *testing.T) {
	hex64 := regexp.MustCompile(`[0-9a-fA-F]{64}`)

	for _, root := range []string{".", filepath.Join("..", "..", "cli")} {
		_ = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
			if err != nil {
				return nil
			}

			if info.IsDir() && info.Name() == "testdata" {
				return filepath.SkipDir
			}

			if info.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}

			data, rerr := os.ReadFile(path)

			if rerr == nil && hex64.Match(data) {
				t.Errorf("%s contains a 64-hex literal", path)
			}

			return nil
		})
	}
}

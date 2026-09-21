// Package credentials reads, mints and fingerprints the API secret key in
// ~/.credentials/ezbookkeeping.json. It follows exactly the rules of the server's copy
// (pkg/machine/credentials.go): 32 bytes from crypto/rand as 64 hex, 0600, owner-checked, symlinks
// refused, merge-writes that preserve every other product's keys, compare-and-set minting.
// Spec: apis.mdx §5, cli.mdx §4.
package credentials

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"
)

const product = "ezbookkeeping"

var keyPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

// ErrTooPermissive is returned when the file is readable by group or others
var ErrTooPermissive = errors.New("credentials file is too permissive")

// Credentials is this app's subtree of the credentials file
type Credentials struct {
	APIKey         string
	Created        string
	CreatedBy      string
	Label          string
	Username       string
	StatementsRoot string
	Timezone       string
}

// Path returns the credentials file path (EZBK_CREDENTIALS_FILE overrides it)
func Path() (string, error) {
	if p := strings.TrimSpace(os.Getenv("EZBK_CREDENTIALS_FILE")); p != "" {
		return p, nil
	}

	home, err := os.UserHomeDir()

	if err != nil {
		return "", err
	}

	return filepath.Join(home, ".credentials", "ezbookkeeping.json"), nil
}

// MintKey returns 32 bytes from crypto/rand as 64 lowercase hex
func MintKey() (string, error) {
	buf := make([]byte, 32)

	if _, err := io.ReadFull(rand.Reader, buf); err != nil {
		return "", fmt.Errorf("crypto/rand failed: %w", err)
	}

	return hex.EncodeToString(buf), nil
}

// IsWellFormed reports whether a key has the only accepted shape
func IsWellFormed(key string) bool {
	return keyPattern.MatchString(key)
}

// Fingerprint is the only representation of a key that may be printed or logged
func Fingerprint(key string) string {
	if len(key) < 4 {
		return "none"
	}

	sum := sha256.Sum256([]byte(key))

	return key[:4] + "…/sha256:" + hex.EncodeToString(sum[:])[:4]
}

// FileInfo describes the credentials file for `ezbk key show` and `ezbk doctor`
type FileInfo struct {
	Path    string
	Exists  bool
	Mode    os.FileMode
	OwnerOK bool
	Problem string
}

// Inspect checks the file without failing on problems, for diagnostics
func Inspect() FileInfo {
	path, err := Path()
	info := FileInfo{Path: path}

	if err != nil {
		info.Problem = err.Error()
		return info
	}

	exists, cerr := check(path)
	info.Exists = exists

	if st, err := os.Lstat(path); err == nil {
		info.Mode = st.Mode().Perm()

		if sys, ok := st.Sys().(*syscall.Stat_t); ok {
			info.OwnerOK = int(sys.Uid) == os.Getuid()
		}
	}

	if cerr != nil {
		info.Problem = cerr.Error()
	}

	return info
}

func check(path string) (bool, error) {
	info, err := os.Lstat(path)

	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	} else if err != nil {
		return false, err
	}

	if info.Mode()&os.ModeSymlink != 0 {
		return true, fmt.Errorf("credentials file %s is a symlink; refusing to follow it", path)
	}

	if !info.Mode().IsRegular() {
		return true, fmt.Errorf("credentials file %s is not a regular file", path)
	}

	if info.Mode().Perm()&0o077 != 0 {
		return true, fmt.Errorf("%w: %s is mode %o (fix: chmod 600 %s)", ErrTooPermissive, path, info.Mode().Perm(), path)
	}

	if st, ok := info.Sys().(*syscall.Stat_t); ok && int(st.Uid) != os.Getuid() {
		return true, fmt.Errorf("credentials file %s is owned by uid %d, not the current user", path, st.Uid)
	}

	return true, nil
}

func readRaw(path string) (map[string]json.RawMessage, bool, error) {
	exists, err := check(path)

	if err != nil || !exists {
		return map[string]json.RawMessage{}, exists, err
	}

	data, err := os.ReadFile(path)

	if err != nil {
		return nil, true, err
	}

	raw := map[string]json.RawMessage{}

	if strings.TrimSpace(string(data)) == "" {
		return raw, true, nil
	}

	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, true, fmt.Errorf("credentials file %s is not a JSON object: %w", path, err)
	}

	return raw, true, nil
}

// Read returns this app's subtree; a missing file is not an error
func Read() (*Credentials, error) {
	path, err := Path()

	if err != nil {
		return nil, err
	}

	raw, _, err := readRaw(path)

	if err != nil {
		return nil, err
	}

	creds := &Credentials{}
	sub := raw[product]

	if len(sub) == 0 {
		return creds, nil
	}

	var p struct {
		Machine struct {
			APIKey    string `json:"api_key"`
			Created   string `json:"created"`
			CreatedBy string `json:"created_by"`
			Label     string `json:"label"`
			Username  string `json:"username"`
		} `json:"machine"`
		Statements struct {
			Root string `json:"root"`
		} `json:"statements"`
		Timezone string `json:"timezone"`
	}

	if err := json.Unmarshal(sub, &p); err != nil {
		return nil, fmt.Errorf("the %q subtree of %s is malformed: %w", product, path, err)
	}

	creds.APIKey = p.Machine.APIKey
	creds.Created = p.Machine.Created
	creds.CreatedBy = p.Machine.CreatedBy
	creds.Label = p.Machine.Label
	creds.Username = p.Machine.Username
	creds.StatementsRoot = p.Statements.Root
	creds.Timezone = p.Timezone

	return creds, nil
}

// Resolve returns the key and where it came from: EZBK_API_KEY, EZBK_API_KEY_FILE, the file. It
// never mints; an empty key with a nil error means "no key anywhere" (cli.mdx §4.5 R6).
func Resolve() (key string, source string, err error) {
	if v := strings.TrimSpace(os.Getenv("EZBK_API_KEY")); v != "" {
		if !IsWellFormed(v) {
			return "", "EZBK_API_KEY", errors.New("EZBK_API_KEY is not 64 lowercase hex characters")
		}

		return v, "EZBK_API_KEY", nil
	}

	if p := strings.TrimSpace(os.Getenv("EZBK_API_KEY_FILE")); p != "" {
		data, err := os.ReadFile(p)

		if err != nil {
			return "", "EZBK_API_KEY_FILE", err
		}

		v := strings.TrimSpace(string(data))

		if !IsWellFormed(v) {
			return "", "EZBK_API_KEY_FILE", fmt.Errorf("%s does not hold 64 lowercase hex characters", p)
		}

		return v, "EZBK_API_KEY_FILE", nil
	}

	path, err := Path()

	if err != nil {
		return "", "", err
	}

	creds, err := Read()

	if err != nil {
		return "", path, err
	}

	if creds.APIKey == "" {
		return "", path, nil
	}

	if !IsWellFormed(creds.APIKey) {
		return "", path, fmt.Errorf("the api_key in %s is malformed; rotate it with: ezbk key rotate --yes", path)
	}

	return creds.APIKey, path, nil
}

// Mint writes a new key, merging into the file. Without force it is a compare-and-set: an existing
// well-formed key is kept and returned.
func Mint(createdBy string, force bool) (string, error) {
	path, err := Path()

	if err != nil {
		return "", err
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", err
	}

	// the same lock file the server takes, so a CLI mint and a server mint converge
	lock, err := os.OpenFile(path+".lock", os.O_RDWR|os.O_CREATE, 0o600)

	if err != nil {
		return "", err
	}

	defer lock.Close()

	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		return "", err
	}

	defer func() { _ = syscall.Flock(int(lock.Fd()), syscall.LOCK_UN) }()

	raw, _, err := readRaw(path)

	if err != nil {
		return "", err
	}

	prod := map[string]json.RawMessage{}

	if sub := raw[product]; len(sub) > 0 {
		if err := json.Unmarshal(sub, &prod); err != nil {
			return "", fmt.Errorf("the %q subtree of %s is not an object: %w", product, path, err)
		}
	}

	machine := map[string]any{}

	if m := prod["machine"]; len(m) > 0 {
		if err := json.Unmarshal(m, &machine); err != nil {
			return "", fmt.Errorf("the machine subtree of %s is not an object: %w", path, err)
		}
	}

	if existing, ok := machine["api_key"].(string); ok && IsWellFormed(existing) && !force {
		return existing, nil
	}

	key, err := MintKey()

	if err != nil {
		return "", err
	}

	machine["api_key"] = key
	machine["created"] = time.Now().UTC().Format(time.RFC3339)
	machine["created_by"] = createdBy

	if _, ok := machine["label"]; !ok {
		if h, err := os.Hostname(); err == nil && h != "" {
			machine["label"] = h
		}
	}

	mj, err := json.Marshal(machine)

	if err != nil {
		return "", err
	}

	prod["machine"] = mj
	pj, err := json.Marshal(prod)

	if err != nil {
		return "", err
	}

	raw[product] = pj

	if err := writeAtomic(path, raw); err != nil {
		return "", err
	}

	creds, err := Read()

	if err != nil {
		return "", err
	}

	return creds.APIKey, nil
}

func writeAtomic(path string, raw map[string]json.RawMessage) error {
	data, err := json.MarshalIndent(raw, "", "  ")

	if err != nil {
		return err
	}

	data = append(data, '\n')

	if info, err := os.Lstat(path); err == nil && info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("credentials file %s is a symlink; refusing to write through it", path)
	}

	suffix, err := MintKey()

	if err != nil {
		return err
	}

	tmp := filepath.Join(filepath.Dir(path), "."+filepath.Base(path)+".tmp-"+suffix[:12])
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)

	if err != nil {
		return err
	}

	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return err
	}

	if err := f.Sync(); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return err
	}

	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return err
	}

	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}

	return nil
}

// Package machine implements this fork's machine-plane API: a loopback-only HTTP surface at
// /machine/v1, authenticated by an API secret key the server mints for itself into
// ~/.credentials/ezbookkeeping.json. The CLI (`ezbk`) and the MCP server are its two clients.
//
// Spec: pm/apis.mdx. Upstream never imports this package; the whole delta to upstream's files is
// machine.Arm() and machine.Mount() in cmd/webserver.go.
package machine

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
	"sync"
	"time"
)

// credentialsProduct is the top-level key this app owns inside the credentials file
const credentialsProduct = "ezbookkeeping"

// keyPattern is the only accepted shape of an API secret key: 32 bytes, lowercase hex
var keyPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

// Credentials holds what this app keeps in its subtree of the credentials file
type Credentials struct {
	APIKey         string
	Created        string
	CreatedBy      string
	Label          string
	Username       string
	StatementsRoot string
	Timezone       string
}

type credentialsMachine struct {
	APIKey    string `json:"api_key,omitempty"`
	Created   string `json:"created,omitempty"`
	CreatedBy string `json:"created_by,omitempty"`
	Label     string `json:"label,omitempty"`
	Username  string `json:"username,omitempty"`
}

// ErrCredentialsTooPermissive is returned when the credentials file is readable by others
var ErrCredentialsTooPermissive = errors.New("credentials file is too permissive")

// CredentialsPath returns the credentials file path (EZBK_CREDENTIALS_FILE overrides it)
func CredentialsPath() (string, error) {
	if p := strings.TrimSpace(os.Getenv("EZBK_CREDENTIALS_FILE")); p != "" {
		return p, nil
	}

	home, err := os.UserHomeDir()

	if err != nil {
		return "", err
	}

	return filepath.Join(home, ".credentials", "ezbookkeeping.json"), nil
}

// MintKey returns a new API secret key: 32 bytes from crypto/rand, rendered as 64 lowercase hex
func MintKey() (string, error) {
	buf := make([]byte, 32)

	if _, err := io.ReadFull(rand.Reader, buf); err != nil {
		return "", fmt.Errorf("crypto/rand failed: %w", err)
	}

	return hex.EncodeToString(buf), nil
}

// IsWellFormedKey reports whether the key has the only accepted shape
func IsWellFormedKey(key string) bool {
	return keyPattern.MatchString(key)
}

// Fingerprint returns the only representation of a key that may be logged or shown
func Fingerprint(key string) string {
	if len(key) < 4 {
		return "none"
	}

	sum := sha256.Sum256([]byte(key))

	return key[:4] + "…/sha256:" + hex.EncodeToString(sum[:])[:4]
}

// checkCredentialsFile refuses symlinks, non-regular files, loose modes and foreign owners
func checkCredentialsFile(path string) (exists bool, err error) {
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
		return true, fmt.Errorf("%w: %s is mode %o (fix: chmod 600 %s)", ErrCredentialsTooPermissive, path, info.Mode().Perm(), path)
	}

	if uid, ok := fileOwnerUid(info); ok && uid != os.Getuid() {
		return true, fmt.Errorf("credentials file %s is owned by uid %d, not the current user", path, uid)
	}

	return true, nil
}

// readCredentialsRaw reads the whole credentials file as top-level raw JSON objects
func readCredentialsRaw(path string) (map[string]json.RawMessage, bool, error) {
	exists, err := checkCredentialsFile(path)

	if err != nil || !exists {
		return map[string]json.RawMessage{}, exists, err
	}

	data, err := os.ReadFile(path)

	if err != nil {
		return nil, true, err
	}

	raw := map[string]json.RawMessage{}

	if len(strings.TrimSpace(string(data))) == 0 {
		return raw, true, nil
	}

	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, true, fmt.Errorf("credentials file %s is not a JSON object: %w", path, err)
	}

	return raw, true, nil
}

// ReadCredentials reads this app's subtree of the credentials file. A missing file is not an error.
func ReadCredentials() (*Credentials, error) {
	path, err := CredentialsPath()

	if err != nil {
		return nil, err
	}

	raw, _, err := readCredentialsRaw(path)

	if err != nil {
		return nil, err
	}

	return parseProductSubtree(raw[credentialsProduct])
}

func parseProductSubtree(sub json.RawMessage) (*Credentials, error) {
	creds := &Credentials{}

	if len(sub) == 0 {
		return creds, nil
	}

	var product struct {
		Machine    credentialsMachine `json:"machine"`
		Statements struct {
			Root string `json:"root"`
		} `json:"statements"`
		Timezone string `json:"timezone"`
	}

	if err := json.Unmarshal(sub, &product); err != nil {
		return nil, fmt.Errorf("the %q subtree of the credentials file is malformed: %w", credentialsProduct, err)
	}

	creds.APIKey = product.Machine.APIKey
	creds.Created = product.Machine.Created
	creds.CreatedBy = product.Machine.CreatedBy
	creds.Label = product.Machine.Label
	creds.Username = product.Machine.Username
	creds.StatementsRoot = product.Statements.Root
	creds.Timezone = product.Timezone

	return creds, nil
}

// ResolveKey returns the API secret key and where it came from, minting one into the credentials
// file when allowed and none exists. Resolution order: EZBK_API_KEY, EZBK_API_KEY_FILE, the
// credentials file, mint (apis.mdx §5.5).
func ResolveKey(createdBy string, allowMint bool) (key string, source string, err error) {
	if v := strings.TrimSpace(os.Getenv("EZBK_API_KEY")); v != "" {
		if !IsWellFormedKey(v) {
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

		if !IsWellFormedKey(v) {
			return "", "EZBK_API_KEY_FILE", fmt.Errorf("%s does not hold 64 lowercase hex characters", p)
		}

		return v, "EZBK_API_KEY_FILE", nil
	}

	path, err := CredentialsPath()

	if err != nil {
		return "", "", err
	}

	creds, err := ReadCredentials()

	if err != nil {
		return "", path, err
	}

	if creds.APIKey != "" {
		if !IsWellFormedKey(creds.APIKey) {
			return "", path, fmt.Errorf("the api_key in %s is malformed; rotate it with: ezbk key rotate --yes", path)
		}

		return creds.APIKey, path, nil
	}

	if !allowMint {
		return "", path, nil
	}

	key, err = MintIntoCredentials(createdBy, false)

	return key, path, err
}

// MintIntoCredentials writes a new key into the credentials file, merging into it so every key the
// app does not own survives. Unless force is set it is a compare-and-set: if a key appeared
// meanwhile, that key is kept and returned (apis.mdx §5.4).
// credMintMu serialises mints inside one process (flock is per open file description)
var credMintMu sync.Mutex

func MintIntoCredentials(createdBy string, force bool) (string, error) {
	path, err := CredentialsPath()

	if err != nil {
		return "", err
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", err
	}

	unlock, err := lockCredentials(path)

	if err != nil {
		return "", err
	}

	defer unlock()

	raw, _, err := readCredentialsRaw(path)

	if err != nil {
		return "", err
	}

	product := map[string]json.RawMessage{}

	if sub := raw[credentialsProduct]; len(sub) > 0 {
		if err := json.Unmarshal(sub, &product); err != nil {
			return "", fmt.Errorf("the %q subtree of %s is not an object: %w", credentialsProduct, path, err)
		}
	}

	machine := map[string]any{}

	if m := product["machine"]; len(m) > 0 {
		if err := json.Unmarshal(m, &machine); err != nil {
			return "", fmt.Errorf("the machine subtree of %s is not an object: %w", path, err)
		}
	}

	if existing, ok := machine["api_key"].(string); ok && existing != "" && !force {
		if IsWellFormedKey(existing) {
			return existing, nil
		}
	}

	key, err := MintKey()

	if err != nil {
		return "", err
	}

	label, _ := os.Hostname()
	machine["api_key"] = key
	machine["created"] = time.Now().UTC().Format(time.RFC3339)
	machine["created_by"] = createdBy

	if _, ok := machine["label"]; !ok && label != "" {
		machine["label"] = label
	}

	machineJSON, err := json.Marshal(machine)

	if err != nil {
		return "", err
	}

	product["machine"] = machineJSON
	productJSON, err := json.Marshal(product)

	if err != nil {
		return "", err
	}

	raw[credentialsProduct] = productJSON

	if err := writeCredentialsAtomic(path, raw); err != nil {
		return "", err
	}

	creds, err := ReadCredentials()

	if err != nil {
		return "", err
	}

	return creds.APIKey, nil
}

// writeCredentialsAtomic writes via an O_EXCL temp file created at 0600, fsync and rename, and
// refuses a symlinked destination
func writeCredentialsAtomic(path string, raw map[string]json.RawMessage) error {
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

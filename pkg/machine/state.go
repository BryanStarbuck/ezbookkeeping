package machine

import (
	"crypto/sha256"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"github.com/mayswind/ezbookkeeping/pkg/errfile"
)

// Limits (apis.mdx §18)
const (
	DefaultLimit      = 200
	MaxLimit          = 5000
	MaxBodyBytes      = 16 << 20
	DefaultMaxChanges = 200
	ConfirmTTL        = 10 * time.Minute
)

// planeState is what Arm() resolved. It is swapped atomically; handlers read it once per request.
type planeState struct {
	keyDigest   [32]byte
	fingerprint string
	keySource   string
	allowWrite  bool
	allowAdmin  bool
	armedAt     time.Time
	credsPath   string
}

var stateHolder atomic.Pointer[planeState]

// socketListenerIsPrivate is set by Arm() when the server listens on a unix socket owned by us at 0600
var socketListenerIsPrivate bool

func currentState() *planeState {
	return stateHolder.Load()
}

// envBool reads a boolean switch from the server's environment
func envBool(name string) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(name))) {
	case "1", "true", "yes", "on":
		return true
	}

	return false
}

// StateDir is where the plane's audit file lives (~/T/_ezbookkeeping, EZBK_STATE_DIR overrides)
func StateDir() string {
	if d := strings.TrimSpace(os.Getenv("EZBK_STATE_DIR")); d != "" {
		return d
	}

	home, err := os.UserHomeDir()

	if err != nil {
		errfile.Warn("resolving the home directory for the machine state dir", err)
		return filepath.Join(os.TempDir(), "_ezbookkeeping")
	}

	return filepath.Join(home, "T", "_ezbookkeeping")
}

func newPlaneState(key, source, credsPath string) *planeState {
	return &planeState{
		keyDigest:   sha256.Sum256([]byte(key)),
		fingerprint: Fingerprint(key),
		keySource:   source,
		allowWrite:  envBool("EZBK_MACHINE_ALLOW_WRITE"),
		allowAdmin:  envBool("EZBK_MACHINE_ALLOW_ADMIN"),
		armedAt:     time.Now(),
		credsPath:   credsPath,
	}
}

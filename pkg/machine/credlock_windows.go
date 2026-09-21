//go:build windows

package machine

import "os"

// lockCredentials on Windows serialises mints within this process only
func lockCredentials(path string) (func(), error) {
	credMintMu.Lock()

	return credMintMu.Unlock, nil
}

// fileOwnerUid has no meaning on Windows (no POSIX owner)
func fileOwnerUid(info os.FileInfo) (int, bool) {
	return 0, false
}

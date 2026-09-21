//go:build !windows

package machine

import (
	"os"
	"syscall"
)

// lockCredentials takes an exclusive advisory lock beside the credentials file so concurrent
// mints (server bring-up, CLI, a second server) converge on one key (apis.mdx §5.4 rule 2)
func lockCredentials(path string) (func(), error) {
	credMintMu.Lock()

	f, err := os.OpenFile(path+".lock", os.O_RDWR|os.O_CREATE, 0o600)

	if err != nil {
		credMintMu.Unlock()
		return nil, err
	}

	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		_ = f.Close()
		credMintMu.Unlock()
		return nil, err
	}

	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
		credMintMu.Unlock()
	}, nil
}

// fileOwnerUid returns the owning uid of a file, ok=false where the platform has none
func fileOwnerUid(info os.FileInfo) (int, bool) {
	if st, ok := info.Sys().(*syscall.Stat_t); ok {
		return int(st.Uid), true
	}

	return 0, false
}

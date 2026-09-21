//go:build windows

package errfile

import (
	"os"
	"syscall"
)

func raise(s syscall.Signal) error {
	// Windows has no kill(2); after the flush the process ends as the console would have ended it.
	os.Exit(128 + int(s))
	return nil
}

// GENERATED from pkg/errfile — run scripts/sync-errfile.sh
//go:build !windows

package errfile

import "syscall"

func raise(s syscall.Signal) error {
	return syscall.Kill(syscall.Getpid(), s)
}
